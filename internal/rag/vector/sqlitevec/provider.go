// Package sqlitevec is candidate B of ADR 0011: a vector.VectorIndexProvider
// over sqlite-vec's vec0 virtual table, through the glue in internal/store/vec.
//
// It is complete (it passes the same contract suite as the flat provider) and
// measured, but it is not the default and nothing in the service links it:
// importing it pulls in sqlite-vec for every connection of the process (see
// internal/store/vec). A composition root that selects it calls vec.Register on
// the migration registry, opens the store, then New. ADR 0011 records why the
// flat scan is the default and what would change that.
//
// Differences from the flat provider that matter to an operator:
//
//   - one vector dimension, vec.Dim (384), fixed in the table definition;
//   - the query is quantised to int8 as well (vec0 compares int8 with int8),
//     where the flat scan keeps the query in float32;
//   - the scan runs inside the C extension (transpiled to Go), measured in
//     docs/efficiency/M2-07.md;
//   - vec0's shadow tables carry no scope columns (vec.ShadowTables), and a
//     whole-generation delete is one vec0 delete per vector.
package sqlitevec

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sort"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/vec"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Name is Info.Name of this provider.
const Name = "sqlitevec"

// Options is everything New needs besides the database.
type Options struct {
	// Config supplies the ceilings (CacheMiB is ignored: there is no resident
	// copy). A zero Config means the defaults.
	Config vector.Config
	Clock  clock.Clock
	Logger *slog.Logger
}

// Provider is the sqlite-vec provider. It is safe for concurrent use.
type Provider struct {
	db  *store.DB
	cfg vector.Config
	clk clock.Clock
	log *slog.Logger
}

var _ vector.VectorIndexProvider = (*Provider)(nil)

// New returns the provider over db, whose migration registry must include
// vec.Register's module. It fails closed when the extension is not loaded or
// the schema is missing, instead of failing on the first query.
func New(ctx context.Context, db *store.DB, opts Options) (*Provider, error) {
	if db == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "sqlitevec: nil store")
	}
	if opts.Config == (vector.Config{}) {
		opts.Config = vector.DefaultConfig()
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "sqlitevec: invalid configuration")
	}
	if _, err := vec.Version(ctx, db); err != nil {
		return nil, err
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM vecb_generations`).Scan(&n); err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, "sqlitevec: the schema is missing; register vec.Register's migrations before opening the store")
	}
	p := &Provider{db: db, cfg: opts.Config, clk: opts.Clock, log: opts.Logger}
	if p.cfg.MaxVectorsPerWorkspace == 0 {
		p.cfg.MaxVectorsPerWorkspace = vector.DefaultMaxVectorsPerWorkspace
	}
	if p.cfg.MaxVectorsPerDeployment == 0 {
		p.cfg.MaxVectorsPerDeployment = vector.DefaultMaxVectorsPerDeployment
	}
	if p.clk == nil {
		p.clk = clock.Real{}
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	return p, nil
}

// Info implements vector.VectorIndexProvider.
func (p *Provider) Info() vector.Info {
	return vector.Info{Name: Name, Exact: true, Metric: "cosine", Storage: "int8", Reuses: true, StableTies: false}
}

type genInfo struct {
	source, model string
	vectors       int
}

func loadGen(ctx context.Context, q store.Reader, scope contracts.Scope, generation string) (genInfo, bool, error) {
	row, err := store.QueryRowScoped(ctx, q, scope, store.ScopedStmt{
		SQL:       "SELECT source_id, model_id, vectors FROM vecb_generations",
		Where:     "generation_id = ?",
		WhereArgs: []any{generation},
	})
	if err != nil {
		return genInfo{}, false, err
	}
	var g genInfo
	switch err := row.Scan(&g.source, &g.model, &g.vectors); {
	case errors.Is(err, sql.ErrNoRows):
		return genInfo{}, false, nil
	case err != nil:
		return genInfo{}, false, err
	}
	return g, true, nil
}

// Search implements vector.VectorIndexProvider.
func (p *Provider) Search(ctx context.Context, scope contracts.Scope, q vector.Query) (vector.SearchResult, error) {
	if err := scope.Validate(); err != nil {
		return vector.SearchResult{}, err
	}
	if err := q.Validate(); err != nil {
		return vector.SearchResult{}, err
	}
	gens := vector.Dedupe(q.Generations)
	if len(gens) == 0 {
		return vector.SearchResult{}, nil
	}
	var code []byte
	if len(q.Vector) == vec.Dim {
		var err error
		if code, _, err = vector.Quantise(q.Vector); err != nil {
			return vector.SearchResult{}, err
		}
	}

	type cand struct {
		hit vector.Hit
	}
	var (
		res   vector.SearchResult
		cands []cand
	)
	err := p.db.View(ctx, func(tx *store.Tx) error {
		res, cands = vector.SearchResult{}, cands[:0]
		for _, g := range gens {
			info, ok, err := loadGen(ctx, tx, scope, g)
			if err != nil {
				return err
			}
			switch {
			case !ok:
				res.Skipped = append(res.Skipped, vector.Skipped{Generation: g, Reason: vector.SkipUnknownGeneration})
				continue
			case info.model != q.Model:
				res.Skipped = append(res.Skipped, vector.Skipped{Generation: g, Reason: vector.SkipModelMismatch})
				continue
			case code == nil:
				res.Skipped = append(res.Skipped, vector.Skipped{Generation: g, Reason: vector.SkipDimMismatch})
				continue
			}
			ns, err := vec.KNN(ctx, tx, scope, g, code, q.K)
			if err != nil {
				return err
			}
			res.Scanned += info.vectors
			for _, n := range ns {
				cands = append(cands, cand{vector.Hit{ChunkID: n.ChunkID, GenerationID: g, SourceID: info.source, Score: float32(1 - n.Distance)}})
			}
		}
		return nil
	})
	if err != nil {
		return vector.SearchResult{}, err
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i].hit, cands[j].hit
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.ChunkID < b.ChunkID
	})
	for _, c := range cands {
		if len(res.Hits) == q.K {
			break
		}
		res.Hits = append(res.Hits, c.hit)
	}
	return res, nil
}

// ---------------------------------------------------------------- writes

type quota struct {
	workspace, deployment       int
	maxWorkspace, maxDeployment int
}

func (p *Provider) loadQuota(ctx context.Context, tx *store.Tx, scope contracts.Scope) (*quota, error) {
	q := &quota{maxWorkspace: p.cfg.MaxVectorsPerWorkspace, maxDeployment: p.cfg.MaxVectorsPerDeployment}
	row, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{SQL: "SELECT COALESCE(SUM(vectors), 0) FROM vecb_generations"})
	if err != nil {
		return nil, err
	}
	if err := row.Scan(&q.workspace); err != nil {
		return nil, err
	}
	// Deliberately unscoped: the deployment ceiling counts the whole database.
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(vectors), 0) FROM vecb_generations").Scan(&q.deployment); err != nil {
		return nil, err
	}
	return q, nil
}

func (q *quota) admit() error {
	if q.workspace >= q.maxWorkspace {
		return contracts.Newf(contracts.ErrResourceExhausted, "vector index: the workspace already holds its limit of %d vectors", q.maxWorkspace)
	}
	if q.deployment >= q.maxDeployment {
		return contracts.New(contracts.ErrResourceExhausted, "vector index: the deployment has reached its configured vector limit")
	}
	q.workspace++
	q.deployment++
	return nil
}

func (p *Provider) ensureGeneration(ctx context.Context, tx *store.Tx, scope contracts.Scope, generation, source, model string) error {
	g, ok, err := loadGen(ctx, tx, scope, generation)
	if err != nil {
		return err
	}
	if ok {
		switch {
		case g.source != source:
			return contracts.Newf(contracts.ErrConflict, "vector index: generation %q belongs to another source", generation)
		case g.model != model:
			return contracts.Newf(contracts.ErrConflict, "vector index: generation %q was built with model %q, not %q", generation, g.model, model)
		}
		return nil
	}
	_, err = store.InsertScoped(ctx, tx, scope, "vecb_generations",
		[]string{"generation_id", "source_id", "model_id", "vectors", "created_ms"},
		generation, source, model, 0, p.clk.Now().UnixMilli())
	return err
}

func bump(ctx context.Context, tx *store.Tx, scope contracts.Scope, generation string, delta int) error {
	_, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       "UPDATE vecb_generations SET vectors = vectors + ?",
		Args:      []any{delta},
		Where:     "generation_id = ?",
		WhereArgs: []any{generation},
	})
	return err
}

// put stores one vector: an existing chunk's vector is replaced (vec0 rows are
// deleted and re-inserted), a new chunk is admitted against the ceilings first.
func put(ctx context.Context, tx *store.Tx, scope contracts.Scope, generation, chunk string, key vector.InputKey, code []byte, q *quota) (replaced bool, err error) {
	row, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       "SELECT vec_rowid FROM vecb_rows",
		Where:     "generation_id = ? AND chunk_id = ?",
		WhereArgs: []any{generation, chunk},
	})
	if err != nil {
		return false, err
	}
	var old int64
	switch err := row.Scan(&old); {
	case err == nil:
		replaced = true
		if _, err := vec.Remove(ctx, tx, scope, old); err != nil {
			return false, err
		}
	case errors.Is(err, sql.ErrNoRows):
		if err := q.admit(); err != nil {
			return false, err
		}
	default:
		return false, err
	}
	id, err := vec.Insert(ctx, tx, scope, generation, chunk, code)
	if err != nil {
		return false, err
	}
	if replaced {
		_, err = store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       "UPDATE vecb_rows SET vec_rowid = ?, input_key = ?",
			Args:      []any{id, key[:]},
			Where:     "generation_id = ? AND chunk_id = ?",
			WhereArgs: []any{generation, chunk},
		})
		return true, err
	}
	_, err = store.InsertScoped(ctx, tx, scope, "vecb_rows",
		[]string{"generation_id", "chunk_id", "vec_rowid", "input_key"}, generation, chunk, id, key[:])
	return false, err
}

// Upsert implements vector.VectorIndexProvider.
func (p *Provider) Upsert(ctx context.Context, scope contracts.Scope, b vector.UpsertBatch) (vector.UpsertResult, error) {
	if err := scope.Validate(); err != nil {
		return vector.UpsertResult{}, err
	}
	if err := b.Validate(); err != nil {
		return vector.UpsertResult{}, err
	}
	if d := len(b.Items[0].Vector); d != vec.Dim {
		return vector.UpsertResult{}, contracts.Newf(contracts.ErrConflict, "sqlitevec: this provider serves %d-dimensional vectors, the batch has %d", vec.Dim, d)
	}
	codes := make([][]byte, len(b.Items))
	for i, it := range b.Items {
		c, _, err := vector.Quantise(it.Vector)
		if err != nil {
			return vector.UpsertResult{}, err
		}
		codes[i] = c
	}
	var res vector.UpsertResult
	err := p.db.Tx(ctx, func(tx *store.Tx) error {
		res = vector.UpsertResult{}
		if err := p.ensureGeneration(ctx, tx, scope, b.Generation, b.SourceID, b.Model); err != nil {
			return err
		}
		q, err := p.loadQuota(ctx, tx, scope)
		if err != nil {
			return err
		}
		for i, it := range b.Items {
			replaced, err := put(ctx, tx, scope, b.Generation, it.ChunkID, it.Key, codes[i], q)
			if err != nil {
				return err
			}
			if replaced {
				res.Replaced++
			} else {
				res.Inserted++
			}
		}
		return bump(ctx, tx, scope, b.Generation, res.Inserted)
	})
	if err != nil {
		return vector.UpsertResult{}, err
	}
	return res, nil
}

// Reuse implements vector.VectorIndexProvider.
func (p *Provider) Reuse(ctx context.Context, scope contracts.Scope, r vector.ReuseRequest) (vector.ReuseResult, error) {
	if err := scope.Validate(); err != nil {
		return vector.ReuseResult{}, err
	}
	if err := r.Validate(); err != nil {
		return vector.ReuseResult{}, err
	}
	var out vector.ReuseResult
	err := p.db.Tx(ctx, func(tx *store.Tx) error {
		out = vector.ReuseResult{}
		if g, ok, err := loadGen(ctx, tx, scope, r.Generation); err != nil {
			return err
		} else if ok && (g.source != r.SourceID || g.model != r.Model) {
			return contracts.Newf(contracts.ErrConflict, "vector index: generation %q belongs to another source or model", r.Generation)
		}
		// Sources: other generations of the SAME scope built with the same model;
		// the vector must sit under the very same chunk id with the very same
		// input key (which hashes scope, model and the exact text).
		var sources []string
		grows, err := store.QueryScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:       "SELECT generation_id FROM vecb_generations",
			Where:     "model_id = ? AND generation_id <> ?",
			WhereArgs: []any{r.Model, r.Generation},
		})
		if err != nil {
			return err
		}
		for grows.Next() {
			var g string
			if err := grows.Scan(&g); err != nil {
				grows.Close()
				return err
			}
			sources = append(sources, g)
		}
		if err := grows.Err(); err != nil {
			grows.Close()
			return err
		}
		grows.Close()
		var (
			q        *quota
			wrote    int
			inserted int
		)
		for _, ref := range r.Refs {
			// Already present with this very key: nothing to copy.
			cur, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
				SQL:       "SELECT input_key FROM vecb_rows",
				Where:     "generation_id = ? AND chunk_id = ?",
				WhereArgs: []any{r.Generation, ref.ChunkID},
			})
			if err != nil {
				return err
			}
			var have []byte
			switch err := cur.Scan(&have); {
			case err == nil && bytes.Equal(have, ref.Key[:]):
				out.Reused = append(out.Reused, ref.ChunkID)
				continue
			case err != nil && !errors.Is(err, sql.ErrNoRows):
				return err
			}
			var (
				rowID int64
				found bool
			)
			for _, g := range sources {
				row, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
					SQL:       "SELECT vec_rowid FROM vecb_rows",
					Where:     "generation_id = ? AND chunk_id = ? AND input_key = ?",
					WhereArgs: []any{g, ref.ChunkID, ref.Key[:]},
				})
				if err != nil {
					return err
				}
				switch err := row.Scan(&rowID); {
				case err == nil:
					found = true
				case errors.Is(err, sql.ErrNoRows):
					continue
				default:
					return err
				}
				break
			}
			if !found {
				continue
			}
			code, err := vec.Code(ctx, tx, scope, rowID)
			if err != nil {
				return err
			}
			if code == nil {
				continue
			}
			if err := p.ensureGeneration(ctx, tx, scope, r.Generation, r.SourceID, r.Model); err != nil {
				return err
			}
			if q == nil {
				if q, err = p.loadQuota(ctx, tx, scope); err != nil {
					return err
				}
			}
			replaced, err := put(ctx, tx, scope, r.Generation, ref.ChunkID, ref.Key, code, q)
			if err != nil {
				return err
			}
			wrote++
			if !replaced {
				inserted++
			}
			out.Reused = append(out.Reused, ref.ChunkID)
		}
		if wrote > 0 {
			return bump(ctx, tx, scope, r.Generation, inserted)
		}
		return nil
	})
	if err != nil {
		return vector.ReuseResult{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------- deletes

// removeChunk deletes one chunk's vector and map row; it reports whether the
// chunk was there.
func removeChunk(ctx context.Context, tx *store.Tx, scope contracts.Scope, generation, chunk string) (bool, error) {
	row, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       "SELECT vec_rowid FROM vecb_rows",
		Where:     "generation_id = ? AND chunk_id = ?",
		WhereArgs: []any{generation, chunk},
	})
	if err != nil {
		return false, err
	}
	var id int64
	switch err := row.Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	if _, err := vec.Remove(ctx, tx, scope, id); err != nil {
		return false, err
	}
	_, err = store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL: "DELETE FROM vecb_rows", Where: "generation_id = ? AND chunk_id = ?", WhereArgs: []any{generation, chunk},
	})
	return err == nil, err
}

// DeleteChunks implements vector.VectorIndexProvider.
func (p *Provider) DeleteChunks(ctx context.Context, scope contracts.Scope, generation string, chunkIDs []string) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if err := vector.CheckChunkIDs(generation, chunkIDs); err != nil {
		return 0, err
	}
	var removed int
	err := p.db.Tx(ctx, func(tx *store.Tx) error {
		removed = 0
		for _, id := range vector.Dedupe(chunkIDs) {
			ok, err := removeChunk(ctx, tx, scope, generation, id)
			if err != nil {
				return err
			}
			if ok {
				removed++
			}
		}
		if removed == 0 {
			return nil
		}
		return bump(ctx, tx, scope, generation, -removed)
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// DeleteGeneration implements vector.VectorIndexProvider.
func (p *Provider) DeleteGeneration(ctx context.Context, scope contracts.Scope, generation string) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if err := vector.CheckID("generation", generation); err != nil {
		return 0, err
	}
	return p.deleteGenerations(ctx, scope, "generation_id = ?", generation)
}

// DeleteSource implements vector.VectorIndexProvider.
func (p *Provider) DeleteSource(ctx context.Context, scope contracts.Scope, sourceID string) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if err := vector.CheckID("source id", sourceID); err != nil {
		return 0, err
	}
	return p.deleteGenerations(ctx, scope, "source_id = ?", sourceID)
}

// DeleteScope implements vector.VectorIndexProvider.
func (p *Provider) DeleteScope(ctx context.Context, scope contracts.Scope) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	return p.deleteGenerations(ctx, scope, "")
}

// deleteGenerations removes the matching generations of the scope, every vec0
// row of them (vec0 does not cascade) and the map rows (which do).
func (p *Provider) deleteGenerations(ctx context.Context, scope contracts.Scope, where string, args ...any) (int, error) {
	var removed int
	err := p.db.Tx(ctx, func(tx *store.Tx) error {
		removed = 0
		rows, err := store.QueryScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: "SELECT generation_id, vectors FROM vecb_generations", Where: where, WhereArgs: args,
		})
		if err != nil {
			return err
		}
		var gens []string
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return err
			}
			gens = append(gens, id)
			removed += n
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, g := range gens {
			ids, err := store.QueryScoped(ctx, tx, scope, store.ScopedStmt{
				SQL: "SELECT vec_rowid FROM vecb_rows", Where: "generation_id = ?", WhereArgs: []any{g},
			})
			if err != nil {
				return err
			}
			var rowIDs []int64
			for ids.Next() {
				var id int64
				if err := ids.Scan(&id); err != nil {
					ids.Close()
					return err
				}
				rowIDs = append(rowIDs, id)
			}
			if err := ids.Err(); err != nil {
				ids.Close()
				return err
			}
			ids.Close()
			for _, id := range rowIDs {
				if _, err := vec.Remove(ctx, tx, scope, id); err != nil {
					return err
				}
			}
		}
		_, err = store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: "DELETE FROM vecb_generations", Where: where, WhereArgs: args,
		})
		return err
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// Stats implements vector.VectorIndexProvider.
func (p *Provider) Stats(ctx context.Context, scope contracts.Scope) (vector.Stats, error) {
	if err := scope.Validate(); err != nil {
		return vector.Stats{}, err
	}
	st := vector.Stats{Info: p.Info()}
	err := p.db.View(ctx, func(tx *store.Tx) error {
		st.Vectors, st.Generations = 0, st.Generations[:0]
		rows, err := store.QueryScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:  "SELECT generation_id, source_id, model_id, vectors FROM vecb_generations",
			Tail: "ORDER BY generation_id",
		})
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			g := vector.GenerationStats{Dim: vec.Dim}
			if err := rows.Scan(&g.Generation, &g.SourceID, &g.Model, &g.Vectors); err != nil {
				return err
			}
			st.Generations = append(st.Generations, g)
			st.Vectors += g.Vectors
		}
		return rows.Err()
	})
	if err != nil {
		return vector.Stats{}, err
	}
	return st, nil
}
