package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// Components is the set of component versions a generation is built with. A
// changed component is a new generation (master plan 12.3).
type Components = rag.IndexGenerationComponents

// Generation is the status of one index generation, as the schema states it.
type Generation = rag.IndexGeneration

var componentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

const maxVersionLen = 128

func validateComponent(label string, c rag.ComponentVersion) error {
	if !componentNameRE.MatchString(c.Name) {
		return invalidArg("%s component name %q is not a valid component name", label, c.Name)
	}
	return checkID(label+" component version", string(c.Version), maxVersionLen)
}

func validateComponents(c Components) error {
	if err := validateComponent("parser", c.Parser); err != nil {
		return err
	}
	if err := validateComponent("chunker", c.Chunker); err != nil {
		return err
	}
	if c.OCR != nil {
		if err := validateComponent("ocr", *c.OCR); err != nil {
			return err
		}
	}
	if c.Embedder != nil {
		if err := validateComponent("embedder", *c.Embedder); err != nil {
			return err
		}
	}
	return nil
}

// genRow is a row of ragindex_generation.
type genRow struct {
	id         string
	sourceID   string
	state      rag.IndexGenerationState
	components string
	documents  int64
	chunks     int64
	embedded   int64
	createdAt  string
	activated  sql.NullString
	retired    sql.NullString
	errCode    sql.NullString
	errMessage sql.NullString
}

const genCols = `generation_id, source_id, state, components, documents, chunks, embedded_chunks, created_at, activated_at, retired_at, error_code, error_message`

func (g *genRow) scan(sc interface{ Scan(...any) error }) error {
	var state string
	if err := sc.Scan(&g.id, &g.sourceID, &state, &g.components, &g.documents, &g.chunks, &g.embedded,
		&g.createdAt, &g.activated, &g.retired, &g.errCode, &g.errMessage); err != nil {
		return err
	}
	g.state = rag.IndexGenerationState(state)
	return nil
}

func (g genRow) status() (Generation, error) {
	var comps Components
	if err := json.Unmarshal([]byte(g.components), &comps); err != nil {
		return Generation{}, contracts.Wrap(contracts.ErrInternal, err, "index: stored generation components are unreadable")
	}
	out := Generation{
		GenerationID: rag.GenerationID(g.id),
		SourceID:     actions.SourceID(g.sourceID),
		State:        g.state,
		Components:   comps,
		Counts: rag.IndexGenerationCounts{
			Documents:      rag.Count(g.documents),
			Chunks:         rag.Count(g.chunks),
			EmbeddedChunks: rag.Count(g.embedded),
		},
		CreatedAt: common.Timestamp(g.createdAt),
	}
	if g.activated.Valid {
		t := common.Timestamp(g.activated.String)
		out.ActivatedAt = &t
	}
	if g.retired.Valid {
		t := common.Timestamp(g.retired.String)
		out.RetiredAt = &t
	}
	if g.errCode.Valid {
		out.Error = &common.Error{
			Code:      common.ErrorCode(g.errCode.String),
			Message:   g.errMessage.String,
			Retryable: contracts.ErrCode(g.errCode.String).Retryable(),
		}
	}
	return out, nil
}

// loadGeneration reads one generation under the scope predicate. An id that
// does not exist, or belongs to another scope, is errNotFound.
func loadGeneration(ctx context.Context, r store.Reader, scope contracts.Scope, id string) (genRow, error) {
	row, err := store.QueryRowScoped(ctx, r, scope, store.ScopedStmt{
		SQL:       `SELECT ` + genCols + ` FROM ragindex_generation`,
		Where:     `generation_id = ?`,
		WhereArgs: []any{id},
	})
	if err != nil {
		return genRow{}, err
	}
	var g genRow
	switch err := g.scan(row); {
	case errors.Is(err, sql.ErrNoRows):
		return genRow{}, errNotFound("generation")
	case err != nil:
		return genRow{}, err
	}
	return g, nil
}

// setState moves a generation from one of the allowed states to another,
// stamping the timestamps the schema requires for the target state. It reports
// a conflict when the generation is not in an allowed state, which also covers
// a concurrent change.
func (ix *Index) setState(ctx context.Context, tx *store.Tx, scope contracts.Scope, id string, to rag.IndexGenerationState, from ...rag.IndexGenerationState) error {
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(from)), ", ")
	args := []any{string(to), ix.now()}
	set := `state = ?, updated_at = ?`
	switch to {
	case rag.IndexGenerationStateActive:
		set += `, activated_at = ?, retired_at = NULL`
		args = append(args, ix.now())
	case rag.IndexGenerationStateSuperseded, rag.IndexGenerationStateInvalidated:
		set += `, retired_at = ?`
		args = append(args, ix.now())
	}
	where := []any{id}
	for _, f := range from {
		where = append(where, string(f))
	}
	res, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       `UPDATE ragindex_generation SET ` + set,
		Args:      args,
		Where:     `generation_id = ? AND state IN (` + marks + `)`,
		WhereArgs: where,
	})
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return contracts.Newf(contracts.ErrConflict, "index: generation %s is not in a state that allows moving to %s", id, to)
	}
	return nil
}

// BeginGeneration starts a new generation of a source in state building. At
// most one build may be in flight per source: starting another while one is
// building or validating is a conflict, so an abandoned build has to be failed
// (FailGeneration) or finished explicitly, never silently accumulated.
func (ix *Index) BeginGeneration(ctx context.Context, scope contracts.Scope, sourceID string, comps Components) (Generation, error) {
	if err := validateScope(scope); err != nil {
		return Generation{}, err
	}
	if err := checkID("source id", sourceID, maxIDLen); err != nil {
		return Generation{}, err
	}
	if err := validateComponents(comps); err != nil {
		return Generation{}, err
	}
	raw, err := json.Marshal(comps)
	if err != nil {
		return Generation{}, contracts.Wrap(contracts.ErrInternal, err, "index: cannot encode components")
	}
	id := ix.ids.New("gen")
	var out Generation
	err = ix.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := ix.ensureScope(ctx, tx, scope); err != nil {
			return err
		}
		var inflight int
		row, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       `SELECT count(*) FROM ragindex_generation`,
			Where:     `source_id = ? AND state IN ('building', 'validating')`,
			WhereArgs: []any{sourceID},
		})
		if err != nil {
			return err
		}
		if err := row.Scan(&inflight); err != nil {
			return err
		}
		if inflight > 0 {
			return contracts.New(contracts.ErrConflict, "index: a generation of this source is already being built; finish or fail it first")
		}
		now := ix.now()
		if _, err := store.InsertScoped(ctx, tx, scope, "ragindex_generation",
			[]string{"generation_id", "source_id", "state", "components", "created_at", "updated_at"},
			id, sourceID, string(rag.IndexGenerationStateBuilding), string(raw), now, now); err != nil {
			return err
		}
		g, err := loadGeneration(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		out, err = g.status()
		return err
	})
	if err != nil {
		return Generation{}, err
	}
	return out, nil
}

// GetGeneration returns one generation's status. An unknown id, or one that
// belongs to another scope, is the same not-found error.
func (ix *Index) GetGeneration(ctx context.Context, scope contracts.Scope, id string) (Generation, error) {
	if err := validateScope(scope); err != nil {
		return Generation{}, err
	}
	var out Generation
	err := ix.db.View(ctx, func(tx *store.Tx) error {
		g, err := loadGeneration(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		out, err = g.status()
		return err
	})
	return out, err
}

// ListGenerations returns every generation of a source in the scope, newest
// first.
func (ix *Index) ListGenerations(ctx context.Context, scope contracts.Scope, sourceID string) ([]Generation, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	var out []Generation
	err := ix.db.View(ctx, func(tx *store.Tx) error {
		rows, err := store.QueryScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       `SELECT ` + genCols + ` FROM ragindex_generation`,
			Where:     `source_id = ?`,
			WhereArgs: []any{sourceID},
			Tail:      `ORDER BY created_at DESC, generation_id DESC`,
		})
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g genRow
			if err := g.scan(rows); err != nil {
				return err
			}
			st, err := g.status()
			if err != nil {
				return err
			}
			out = append(out, st)
		}
		return rows.Err()
	})
	return out, err
}

// ActiveGeneration returns the generation currently serving a source, or
// ok=false when none does.
func (ix *Index) ActiveGeneration(ctx context.Context, scope contracts.Scope, sourceID string) (g Generation, ok bool, err error) {
	if err := validateScope(scope); err != nil {
		return Generation{}, false, err
	}
	err = ix.db.View(ctx, func(tx *store.Tx) error {
		row, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       `SELECT ` + genCols + ` FROM ragindex_generation`,
			Where:     `source_id = ? AND state = 'active'`,
			WhereArgs: []any{sourceID},
		})
		if err != nil {
			return err
		}
		var r genRow
		switch err := r.scan(row); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		g, err = r.status()
		ok = err == nil
		return err
	})
	return g, ok, err
}

// SetEmbeddedChunks records how many of the generation's chunks have an
// embedding. Embeddings are queued work and trail the full-text index.
func (ix *Index) SetEmbeddedChunks(ctx context.Context, scope contracts.Scope, generationID string, n int) error {
	if err := validateScope(scope); err != nil {
		return err
	}
	if n < 0 {
		return invalidArg("embedded chunk count %d is negative", n)
	}
	return ix.db.Tx(ctx, func(tx *store.Tx) error {
		g, err := loadGeneration(ctx, tx, scope, generationID)
		if err != nil {
			return err
		}
		if int64(n) > g.chunks {
			return invalidArg("embedded chunk count %d exceeds the generation's %d chunks", n, g.chunks)
		}
		_, err = store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       `UPDATE ragindex_generation SET embedded_chunks = ?, updated_at = ?`,
			Args:      []any{n, ix.now()},
			Where:     `generation_id = ?`,
			WhereArgs: []any{generationID},
		})
		return err
	})
}

// CountDocuments reports how many of the given documents a generation holds
// (not tombstoned). A generation the scope does not hold holds none.
func (ix *Index) CountDocuments(ctx context.Context, scope contracts.Scope, generationID string, documentIDs []string) (int, error) {
	if err := validateScope(scope); err != nil {
		return 0, err
	}
	if len(documentIDs) == 0 {
		return 0, nil
	}
	list, err := json.Marshal(documentIDs)
	if err != nil {
		return 0, contracts.Wrap(contracts.ErrInternal, err, "index: cannot encode the document ids")
	}
	var n int
	err = ix.db.View(ctx, func(tx *store.Tx) error {
		row, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       `SELECT count(*) FROM ragindex_document`,
			Where:     `generation_id = ? AND tombstoned_at IS NULL AND document_id IN (SELECT value FROM json_each(?))`,
			WhereArgs: []any{generationID, string(list)},
		})
		if err != nil {
			return err
		}
		return row.Scan(&n)
	})
	return n, err
}

// FailGeneration marks a generation that could not be built or did not
// validate as failed. It never served and never will; compaction removes it.
func (ix *Index) FailGeneration(ctx context.Context, scope contracts.Scope, generationID string, cause error) error {
	if err := validateScope(scope); err != nil {
		return err
	}
	code, msg := causeOf(cause)
	return ix.db.Tx(ctx, func(tx *store.Tx) error {
		if err := ix.setState(ctx, tx, scope, generationID, rag.IndexGenerationStateFailed,
			rag.IndexGenerationStateBuilding, rag.IndexGenerationStateValidating); err != nil {
			return err
		}
		return setError(ctx, tx, scope, generationID, code, msg)
	})
}

// InvalidateGeneration withdraws a generation: it is never retrievable again
// (an operator action, or a detected problem). Invalidating the active
// generation leaves the source without a serving one until a rebuild
// activates a replacement.
func (ix *Index) InvalidateGeneration(ctx context.Context, scope contracts.Scope, generationID string, cause error) error {
	if err := validateScope(scope); err != nil {
		return err
	}
	code, msg := causeOf(cause)
	return ix.db.Tx(ctx, func(tx *store.Tx) error {
		sc, found, err := lookupScope(ctx, tx, scope)
		if err != nil {
			return err
		}
		if !found {
			return errNotFound("generation")
		}
		if err := ix.setState(ctx, tx, scope, generationID, rag.IndexGenerationStateInvalidated,
			rag.IndexGenerationStateBuilding, rag.IndexGenerationStateValidating,
			rag.IndexGenerationStateActive, rag.IndexGenerationStateSuperseded); err != nil {
			return err
		}
		if cause != nil {
			if err := setError(ctx, tx, scope, generationID, code, msg); err != nil {
				return err
			}
		}
		return bumpEpoch(ctx, tx, scope, sc)
	})
}

func causeOf(cause error) (code, msg string) {
	if cause == nil {
		return string(contracts.ErrInternal), "withdrawn"
	}
	var ce *contracts.Error
	if errors.As(cause, &ce) {
		return string(ce.Code), ce.Message
	}
	return string(contracts.ErrInternal), "index generation withdrawn"
}

func setError(ctx context.Context, tx *store.Tx, scope contracts.Scope, id, code, msg string) error {
	if len(msg) > 512 {
		msg = msg[:512]
	}
	_, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       `UPDATE ragindex_generation SET error_code = ?, error_message = ?`,
		Args:      []any{code, msg},
		Where:     `generation_id = ?`,
		WhereArgs: []any{id},
	})
	return err
}

// ActivateGeneration makes a validated generation the one that serves its
// source, in a single transaction: the previous active generation (if any)
// becomes superseded, the new one becomes active, and the scope's epoch moves.
// Readers see the old generation or the new one, never both and never neither;
// a crash before the commit leaves the old one serving.
//
// Only a generation in state validating can be activated, or a superseded one
// (a rollback to the previous generation, still retained). A generation that
// is building has not been validated and cannot be activated.
func (ix *Index) ActivateGeneration(ctx context.Context, scope contracts.Scope, generationID string) error {
	if err := validateScope(scope); err != nil {
		return err
	}
	return ix.db.Tx(ctx, func(tx *store.Tx) error {
		sc, found, err := lookupScope(ctx, tx, scope)
		if err != nil {
			return err
		}
		if !found {
			return errNotFound("generation")
		}
		g, err := loadGeneration(ctx, tx, scope, generationID)
		if err != nil {
			return err
		}
		if g.state != rag.IndexGenerationStateValidating && g.state != rag.IndexGenerationStateSuperseded {
			return contracts.Newf(contracts.ErrConflict, "index: generation %s is %s; only a validated generation can be activated", generationID, g.state)
		}
		// Supersede first: the partial unique index allows one active
		// generation per source at any instant.
		if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       `UPDATE ragindex_generation SET state = 'superseded', retired_at = ?`,
			Args:      []any{ix.now()},
			Where:     `source_id = ? AND state = 'active' AND generation_id <> ?`,
			WhereArgs: []any{g.sourceID, generationID},
		}); err != nil {
			return err
		}
		if ix.activateHook != nil {
			if err := ix.activateHook("superseded"); err != nil {
				return err
			}
		}
		if err := ix.setState(ctx, tx, scope, generationID, rag.IndexGenerationStateActive,
			rag.IndexGenerationStateValidating, rag.IndexGenerationStateSuperseded); err != nil {
			return err
		}
		if ix.activateHook != nil {
			if err := ix.activateHook("activated"); err != nil {
				return err
			}
		}
		return bumpEpoch(ctx, tx, scope, sc)
	})
}
