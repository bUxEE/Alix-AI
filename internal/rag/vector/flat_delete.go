package vector

import (
	"context"
	"database/sql"
	"errors"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// dropBatch is how many rows one delete transaction removes. The store has ONE
// writer connection; deleting 100,000 vectors in a single transaction held it
// for about two seconds on the dev box (docs/efficiency/M2-07.md), which stalls
// every other writer of the process (sessions, the job queue). A batch of this
// size holds it for tens of milliseconds.
const dropBatch = 2048

// DeleteGeneration implements VectorIndexProvider.
//
// Deletion is two-phase so that it is both immediate and bounded: one short
// transaction marks the generation 'dropping', after which it no longer exists
// for search, stats, reuse or upserts; then its rows go in batches of dropBatch.
// If the process dies or ctx ends between the phases the generation stays
// hidden and a repeat of the call (or ResumeDrops) finishes the job; nothing of
// it can be served in the meantime.
func (f *Flat) DeleteGeneration(ctx context.Context, scope contracts.Scope, generation string) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if err := CheckID("generation", generation); err != nil {
		return 0, err
	}
	return f.drop(ctx, scope, "generation_id = ?", generation)
}

// DeleteSource implements VectorIndexProvider.
func (f *Flat) DeleteSource(ctx context.Context, scope contracts.Scope, sourceID string) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if err := CheckID("source id", sourceID); err != nil {
		return 0, err
	}
	return f.drop(ctx, scope, "source_id = ?", sourceID)
}

// DeleteScope implements VectorIndexProvider.
func (f *Flat) DeleteScope(ctx context.Context, scope contracts.Scope) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	n, err := f.drop(ctx, scope, "")
	f.cache.dropScope(scope.ApplicationID, scope.TenantID, scope.WorkspaceID)
	return n, err
}

// drop deletes the scope's generations matching where (all when empty). It
// returns how many vectors those generations held.
func (f *Flat) drop(ctx context.Context, scope contracts.Scope, where string, args ...any) (int, error) {
	var (
		gens    []string
		removed int
	)
	// Phase one: hide.
	err := f.db.Tx(ctx, func(tx *store.Tx) error {
		gens, removed = gens[:0], 0
		rows, err := store.QueryScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: "SELECT generation_id, vectors FROM vector_generations", Where: where, WhereArgs: args,
		})
		if err != nil {
			return err
		}
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
		if len(gens) == 0 {
			return nil
		}
		_, err = store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: "UPDATE vector_generations SET state = 'dropping', rev = rev + 1", Where: where, WhereArgs: args,
		})
		return err
	})
	if err != nil {
		return 0, err
	}
	for _, g := range gens {
		f.cache.drop(cacheKey{scope.ApplicationID, scope.TenantID, scope.WorkspaceID, g})
	}
	// Phase two: remove, a bounded batch per transaction.
	for _, g := range gens {
		if err := f.dropRows(ctx, scope.ApplicationID, scope.TenantID, scope.WorkspaceID, g); err != nil {
			return 0, err
		}
	}
	return removed, nil
}

// dropRows deletes the rows of one generation in batches and, with the last
// batch, the generation row. The scope arrives as its three levels because the
// janitor (ResumeDrops) has no principal; both callers take them from a
// validated scope or from the generation's own row.
func (f *Flat) dropRows(ctx context.Context, application, tenant, workspace, generation string) error {
	const rowsPred = "application_id = ? AND tenant_id = ? AND workspace_id = ? AND generation_id = ?"
	for {
		last := false
		err := f.db.Tx(ctx, func(tx *store.Tx) error {
			// The batch is a primary-key range: find the chunk id that ends it,
			// then delete everything up to it. (A "chunk_id IN (SELECT ... LIMIT)"
			// delete re-scans the whole remaining range for every batch and made
			// a 100,000-vector delete three times slower than one transaction.)
			var upTo string
			err := tx.QueryRowContext(ctx,
				"SELECT chunk_id FROM vector_rows WHERE "+rowsPred+" ORDER BY chunk_id LIMIT 1 OFFSET ?",
				application, tenant, workspace, generation, dropBatch-1).Scan(&upTo)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				// Fewer than a batch left: this is the last one, and the
				// generation row goes with it.
				last = true
				if _, err := tx.ExecContext(ctx, "DELETE FROM vector_rows WHERE "+rowsPred,
					application, tenant, workspace, generation); err != nil {
					return err
				}
				_, err = tx.ExecContext(ctx, "DELETE FROM vector_generations WHERE "+rowsPred,
					application, tenant, workspace, generation)
				return err
			case err != nil:
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM vector_rows WHERE "+rowsPred+" AND chunk_id <= ?",
				application, tenant, workspace, generation, upTo); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx,
				"UPDATE vector_generations SET vectors = MAX(vectors - ?, 0) WHERE "+rowsPred, dropBatch, application, tenant, workspace, generation)
			return err
		})
		if err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

// ResumeDrops finishes every deletion that was interrupted (a crash, a
// cancelled context) and returns how many generations it completed. It works
// across all scopes, so it is for the process's own maintenance, never for a
// request that carries a tenant's input. The composition root calls it at
// startup and from its maintenance job; ingestion's tombstone propagation may
// also simply repeat the Delete call it already made.
func (f *Flat) ResumeDrops(ctx context.Context) (int, error) {
	type target struct{ app, tenant, ws, gen string }
	var todo []target
	err := f.db.View(ctx, func(tx *store.Tx) error {
		todo = todo[:0]
		rows, err := tx.QueryContext(ctx, "SELECT application_id, tenant_id, workspace_id, generation_id FROM vector_generations WHERE state = 'dropping'")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t target
			if err := rows.Scan(&t.app, &t.tenant, &t.ws, &t.gen); err != nil {
				return err
			}
			todo = append(todo, t)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	for i, t := range todo {
		if err := f.dropRows(ctx, t.app, t.tenant, t.ws, t.gen); err != nil {
			return i, err
		}
	}
	return len(todo), nil
}
