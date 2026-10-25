package upgrade

import (
	"context"
	"errors"
	"fmt"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The embedded vector index (internal/rag/vector, the flat provider of ADR 0011)
// is derived data like a full-text index: every row of it was computed from a
// chunk of text that is kept elsewhere. It is not an FTS5 table, so FTSIndexes
// does not find it; VectorIndex puts it under the same isolate, rebuild and
// quarantine rules, and Config wires it into ExtraDerived.
//
// What differs is the rebuild. An FTS5 index is rebuilt by SQLite from its
// content table. A vector is the output of the embedding model, so rebuilding
// the index means embedding the chunks again, which needs the embedder and runs
// as a queued job in the serving process, not inside an upgrade. VectorIndex
// therefore isolates (drops what it holds, so nothing is served from damaged
// bytes) and then, unless the composition root supplied a VectorRebuild, reports
// ErrReembedRequired: the index stays in the quarantine, retrieval keeps saying
// it has reduced semantic recall, and whoever finishes the re-embedding releases
// the record with Quarantine.Clear. A rebuild that only queued work and returned
// would let the record go while the index is still empty, which is the silent
// "found nothing" the quarantine exists to prevent.

// VectorIndexName is the name the vector index has in the quarantine. It is the
// table that holds the vectors; retrieval consults Quarantine.Has with it.
const VectorIndexName = "vector_rows"

// ErrReembedRequired is returned by a vector index's Rebuild when the vectors can
// only be recreated by embedding the chunks again and no VectorRebuild is
// configured. It is not damage and not a failure of the repair: the index stays
// quarantined and is reported in DerivedReport.Pending.
var ErrReembedRequired = errors.New("derived index can only be rebuilt by embedding its chunks again")

// VectorRebuild recreates the vectors of the database by embedding every chunk
// again. It must not return before the index holds all of them: the upgrade
// checks the index and releases it from the quarantine as soon as it returns.
type VectorRebuild func(ctx context.Context, db *store.DB) error

// VectorDerived is the Config.ExtraDerived function for the vector index. A nil
// rebuild leaves the index quarantined until something else re-embeds it.
func VectorDerived(rebuild VectorRebuild) func(db *store.DB) []DerivedIndex {
	return func(db *store.DB) []DerivedIndex { return []DerivedIndex{vectorIndex{db: db, rebuild: rebuild}} }
}

type vectorIndex struct {
	db      *store.DB
	rebuild VectorRebuild
}

func (vectorIndex) Name() string { return VectorIndexName }

// present reports whether the vector tables exist. A database that never had the
// vector module has no vector index: nothing to check, isolate or rebuild.
func (v vectorIndex) present(ctx context.Context) (bool, error) {
	var n int
	err := v.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('vector_generations', 'vector_rows')`).Scan(&n)
	return n == 2, err
}

// The consistency checks of the flat provider's tables (migration 0001 of module
// ragvector). Each returns how many rows break one rule.
var vectorChecks = []struct{ rule, query string }{
	{"a vector belongs to no generation", `SELECT count(*) FROM vector_rows r WHERE NOT EXISTS (
		SELECT 1 FROM vector_generations g WHERE g.application_id = r.application_id AND g.tenant_id = r.tenant_id
		AND g.workspace_id = r.workspace_id AND g.generation_id = r.generation_id)`},
	{"a vector is not as long as its generation's dimension, or has no norm", `SELECT count(*) FROM vector_rows r
		JOIN vector_generations g ON g.application_id = r.application_id AND g.tenant_id = r.tenant_id
		AND g.workspace_id = r.workspace_id AND g.generation_id = r.generation_id
		WHERE typeof(r.code) <> 'blob' OR length(r.code) <> g.dim OR typeof(r.inv_norm) <> 'real' OR r.inv_norm <= 0`},
	{"a generation's vector count is not the number of its vectors", `SELECT count(*) FROM vector_generations g WHERE g.state = 'live' AND g.vectors <> (
		SELECT count(*) FROM vector_rows r WHERE r.application_id = g.application_id AND r.tenant_id = g.tenant_id
		AND r.workspace_id = g.workspace_id AND r.generation_id = g.generation_id)`},
}

// Check compares the vectors with what their generations say about them. A read
// that fails on the database's own pages is damage; a busy or cancelled one is
// not evidence of it.
func (v vectorIndex) Check(ctx context.Context) error {
	damaged := func(err error) error {
		if ctx.Err() != nil || store.IsBusy(err) {
			return err
		}
		return fmt.Errorf("%w: %s: %v", ErrDamaged, VectorIndexName, err)
	}
	ok, err := v.present(ctx)
	if err != nil {
		return damaged(err)
	}
	if !ok {
		return nil
	}
	for _, c := range vectorChecks {
		var n int
		if err := v.db.QueryRowContext(ctx, c.query).Scan(&n); err != nil {
			return damaged(err)
		}
		if n > 0 {
			return fmt.Errorf("%w: %s: %s (%d)", ErrDamaged, VectorIndexName, c.rule, n)
		}
	}
	return nil
}

// Isolate drops every vector and zeroes the generations' counts, bumping each
// generation's revision so a resident cache and a reuse can never hand out what
// was dropped. The generations themselves stay: they say which source, model and
// dimension a rebuild has to reproduce.
func (v vectorIndex) Isolate(ctx context.Context) error {
	ok, err := v.present(ctx)
	if err != nil || !ok {
		return err
	}
	return v.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM vector_rows`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE vector_generations SET vectors = 0, rev = rev + 1`)
		return err
	})
}

// Rebuild re-embeds through the configured VectorRebuild, or says that it cannot
// (ErrReembedRequired). Nothing is rebuilt in a database that has no vector
// tables.
func (v vectorIndex) Rebuild(ctx context.Context) error {
	ok, err := v.present(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if v.rebuild == nil {
		return contracts.Wrap(contracts.ErrUnavailable, ErrReembedRequired,
			"the vectors are rebuilt by embedding the chunks again, which needs the embedder")
	}
	return v.rebuild(ctx, v.db)
}
