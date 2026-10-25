package vector

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// quota counts vectors against the workspace and deployment ceilings inside the
// writing transaction, so two writers cannot both slip under the limit.
type quota struct {
	workspace, deployment       int
	maxWorkspace, maxDeployment int
}

func (f *Flat) loadQuota(ctx context.Context, tx *store.Tx, scope contracts.Scope) (*quota, error) {
	q := &quota{maxWorkspace: f.cfg.MaxVectorsPerWorkspace, maxDeployment: f.cfg.MaxVectorsPerDeployment}
	row, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
		SQL: "SELECT COALESCE(SUM(vectors), 0) FROM vector_generations",
	})
	if err != nil {
		return nil, err
	}
	if err := row.Scan(&q.workspace); err != nil {
		return nil, err
	}
	// The one deliberately unscoped statement of this package: the deployment
	// ceiling is a property of the whole database, and only a count leaves it.
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(vectors), 0) FROM vector_generations").Scan(&q.deployment); err != nil {
		return nil, err
	}
	return q, nil
}

// admit accounts for one new vector, or refuses with resource_exhausted.
func (q *quota) admit() error {
	if q.workspace >= q.maxWorkspace {
		return contracts.Newf(contracts.ErrResourceExhausted,
			"vector index: the workspace already holds its limit of %d vectors; remove a source or a superseded generation, or raise rag_vector.max_vectors_per_workspace (ADR 0011 gives the supported ceiling)", q.maxWorkspace)
	}
	if q.deployment >= q.maxDeployment {
		return contracts.New(contracts.ErrResourceExhausted,
			"vector index: the deployment has reached its configured vector limit; raise rag_vector.max_vectors_per_deployment or move to a profile with an approximate index (ADR 0011)")
	}
	q.workspace++
	q.deployment++
	return nil
}

// ensureGeneration returns the generation, creating it when absent. An
// existing generation must agree on source, model and dimension.
func (f *Flat) ensureGeneration(ctx context.Context, tx *store.Tx, scope contracts.Scope, generation, source, model string, dim int) error {
	g, ok, err := loadGen(ctx, tx, scope, generation)
	if err != nil {
		return err
	}
	if ok {
		switch {
		case g.dropping:
			return contracts.Newf(contracts.ErrConflict, "vector index: generation %q is being deleted", generation)
		case g.source != source:
			return contracts.Newf(contracts.ErrConflict, "vector index: generation %q belongs to another source", generation)
		case g.model != model:
			return contracts.Newf(contracts.ErrConflict, "vector index: generation %q was built with model %q, not %q", generation, g.model, model)
		case g.dim != dim:
			return contracts.Newf(contracts.ErrConflict, "vector index: generation %q holds %d-dimensional vectors, not %d", generation, g.dim, dim)
		}
		return nil
	}
	_, err = store.InsertScoped(ctx, tx, scope, "vector_generations",
		[]string{"generation_id", "source_id", "model_id", "dim", "vectors", "rev", "created_ms"},
		generation, source, model, dim, 0, 0, f.clk.Now().UnixMilli())
	return err
}

// bump records a write to a generation: its count moves by delta and its rev
// changes, so no cached copy of the generation can be used again.
func bump(ctx context.Context, tx *store.Tx, scope contracts.Scope, generation string, delta int) error {
	_, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL:       "UPDATE vector_generations SET vectors = vectors + ?, rev = rev + 1",
		Args:      []any{delta},
		Where:     "generation_id = ?",
		WhereArgs: []any{generation},
	})
	return err
}

// rowWriter writes vector_rows of one generation, updating in place and
// inserting what is missing.
type rowWriter struct {
	upd, ins *sql.Stmt
	scope    []any
	gen      string
}

func newRowWriter(ctx context.Context, tx *store.Tx, scope contracts.Scope, generation string) (*rowWriter, error) {
	pred, sargs, err := store.ScopedWhere(scope)
	if err != nil {
		return nil, err
	}
	upd, err := tx.PrepareContext(ctx,
		"UPDATE vector_rows SET input_key = ?, inv_norm = ?, code = ? WHERE "+pred+" AND generation_id = ? AND chunk_id = ?")
	if err != nil {
		return nil, err
	}
	ins, err := tx.PrepareContext(ctx,
		"INSERT INTO vector_rows (application_id, tenant_id, workspace_id, generation_id, chunk_id, input_key, inv_norm, code) VALUES (?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		upd.Close()
		return nil, err
	}
	return &rowWriter{upd: upd, ins: ins, scope: sargs, gen: generation}, nil
}

func (w *rowWriter) close() { w.upd.Close(); w.ins.Close() }

// put stores one vector; replaced reports an update of an existing row. admit
// is called before an insert and may refuse it.
func (w *rowWriter) put(ctx context.Context, chunk string, key InputKey, inv float32, code []byte, admit func() error) (replaced bool, err error) {
	args := make([]any, 0, 8)
	args = append(args, key[:], float64(inv), code)
	args = append(args, w.scope...)
	args = append(args, w.gen, chunk)
	res, err := w.upd.ExecContext(ctx, args...)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, err
	} else if n > 0 {
		return true, nil
	}
	if err := admit(); err != nil {
		return false, err
	}
	_, err = w.ins.ExecContext(ctx, w.scope[0], w.scope[1], w.scope[2], w.gen, chunk, key[:], float64(inv), code)
	return false, err
}

// Upsert implements VectorIndexProvider.
func (f *Flat) Upsert(ctx context.Context, scope contracts.Scope, b UpsertBatch) (UpsertResult, error) {
	if err := scope.Validate(); err != nil {
		return UpsertResult{}, err
	}
	if err := b.Validate(); err != nil {
		return UpsertResult{}, err
	}
	// Quantise before taking the writer: it is pure CPU.
	type coded struct {
		code []byte
		inv  float32
	}
	cs := make([]coded, len(b.Items))
	for i, it := range b.Items {
		code, inv, err := Quantise(it.Vector)
		if err != nil {
			return UpsertResult{}, err
		}
		cs[i] = coded{code, inv}
	}
	dim := len(b.Items[0].Vector)

	var res UpsertResult
	err := f.db.Tx(ctx, func(tx *store.Tx) error {
		res = UpsertResult{} // Tx may run fn again after a busy error
		if err := f.ensureGeneration(ctx, tx, scope, b.Generation, b.SourceID, b.Model, dim); err != nil {
			return err
		}
		q, err := f.loadQuota(ctx, tx, scope)
		if err != nil {
			return err
		}
		w, err := newRowWriter(ctx, tx, scope, b.Generation)
		if err != nil {
			return err
		}
		defer w.close()
		for i, it := range b.Items {
			replaced, err := w.put(ctx, it.ChunkID, it.Key, cs[i].inv, cs[i].code, q.admit)
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
		return UpsertResult{}, err
	}
	return res, nil
}

// Reuse implements VectorIndexProvider.
func (f *Flat) Reuse(ctx context.Context, scope contracts.Scope, r ReuseRequest) (ReuseResult, error) {
	if err := scope.Validate(); err != nil {
		return ReuseResult{}, err
	}
	if err := r.Validate(); err != nil {
		return ReuseResult{}, err
	}
	var out ReuseResult
	err := f.db.Tx(ctx, func(tx *store.Tx) error {
		out = ReuseResult{}
		target, targetExists, err := loadGen(ctx, tx, scope, r.Generation)
		if err != nil {
			return err
		}
		if targetExists && (target.dropping || target.source != r.SourceID || target.model != r.Model) {
			return contracts.Newf(contracts.ErrConflict, "vector index: generation %q is being deleted or belongs to another source or model", r.Generation)
		}

		// Where a reusable vector may come from: another live generation of the
		// SAME scope that was built with the same model. The scope predicate comes
		// first in every statement here, so there is no way to land on another
		// scope's row, and the row must also carry the very same chunk id and the
		// very same input key (which hashes scope, model and the exact text).
		pred, sargs, err := store.ScopedWhere(scope)
		if err != nil {
			return err
		}
		var sources []string
		grows, err := tx.QueryContext(ctx,
			"SELECT generation_id FROM vector_generations WHERE "+pred+" AND model_id = ? AND state = 'live' AND generation_id <> ?",
			append(append([]any(nil), sargs...), r.Model, r.Generation)...)
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
		find, err := tx.PrepareContext(ctx,
			"SELECT inv_norm, code FROM vector_rows WHERE "+pred+" AND generation_id = ? AND chunk_id = ? AND input_key = ?")
		if err != nil {
			return err
		}
		defer find.Close()
		cur, err := tx.PrepareContext(ctx, "SELECT input_key FROM vector_rows WHERE "+pred+" AND generation_id = ? AND chunk_id = ?")
		if err != nil {
			return err
		}
		defer cur.Close()

		var (
			q      *quota
			w      *rowWriter
			wrote  int
			insert int
		)
		for _, ref := range r.Refs {
			// Already present with this very key (an earlier attempt, or an
			// unchanged re-run): nothing to copy.
			var have []byte
			switch err := cur.QueryRowContext(ctx, append(append([]any(nil), sargs...), r.Generation, ref.ChunkID)...).Scan(&have); {
			case err == nil && bytes.Equal(have, ref.Key[:]):
				out.Reused = append(out.Reused, ref.ChunkID)
				continue
			case err != nil && !errors.Is(err, sql.ErrNoRows):
				return err
			}

			var (
				inv   float64
				code  []byte
				found bool
			)
			for _, g := range sources {
				err := find.QueryRowContext(ctx, append(append([]any(nil), sargs...), g, ref.ChunkID, ref.Key[:])...).Scan(&inv, &code)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				found = true
				break
			}
			if !found {
				continue
			}
			dim := len(code)
			if targetExists && target.dim != dim {
				continue
			}
			if !targetExists {
				if err := f.ensureGeneration(ctx, tx, scope, r.Generation, r.SourceID, r.Model, dim); err != nil {
					return err
				}
				target, targetExists = genInfo{source: r.SourceID, model: r.Model, dim: dim}, true
			}
			if q == nil {
				if q, err = f.loadQuota(ctx, tx, scope); err != nil {
					return err
				}
				if w, err = newRowWriter(ctx, tx, scope, r.Generation); err != nil {
					return err
				}
				defer w.close()
			}
			replaced, err := w.put(ctx, ref.ChunkID, ref.Key, float32(inv), code, q.admit)
			if err != nil {
				return err
			}
			wrote++
			if !replaced {
				insert++
			}
			out.Reused = append(out.Reused, ref.ChunkID)
		}
		if wrote > 0 {
			return bump(ctx, tx, scope, r.Generation, insert)
		}
		return nil
	})
	if err != nil {
		return ReuseResult{}, err
	}
	return out, nil
}

// DeleteChunks implements VectorIndexProvider.
func (f *Flat) DeleteChunks(ctx context.Context, scope contracts.Scope, generation string, chunkIDs []string) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if err := CheckChunkIDs(generation, chunkIDs); err != nil {
		return 0, err
	}
	var removed int
	err := f.db.Tx(ctx, func(tx *store.Tx) error {
		removed = 0
		pred, sargs, err := store.ScopedWhere(scope)
		if err != nil {
			return err
		}
		del, err := tx.PrepareContext(ctx, "DELETE FROM vector_rows WHERE "+pred+" AND generation_id = ? AND chunk_id = ?")
		if err != nil {
			return err
		}
		defer del.Close()
		for _, id := range Dedupe(chunkIDs) {
			res, err := del.ExecContext(ctx, append(append([]any(nil), sargs...), generation, id)...)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			removed += int(n)
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

// Stats implements VectorIndexProvider.
func (f *Flat) Stats(ctx context.Context, scope contracts.Scope) (Stats, error) {
	if err := scope.Validate(); err != nil {
		return Stats{}, err
	}
	st := Stats{Info: f.Info()}
	err := f.db.View(ctx, func(tx *store.Tx) error {
		st.Vectors, st.Generations = 0, st.Generations[:0]
		rows, err := store.QueryScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:   "SELECT generation_id, source_id, model_id, dim, vectors FROM vector_generations",
			Where: "state = 'live'",
			Tail:  "ORDER BY generation_id",
		})
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g GenerationStats
			if err := rows.Scan(&g.Generation, &g.SourceID, &g.Model, &g.Dim, &g.Vectors); err != nil {
				return err
			}
			st.Generations = append(st.Generations, g)
			st.Vectors += g.Vectors
		}
		return rows.Err()
	})
	if err != nil {
		return Stats{}, err
	}
	return st, nil
}
