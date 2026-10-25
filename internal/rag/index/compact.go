package index

import (
	"context"
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// CompactOptions bounds one compaction pass. Compaction is background
// maintenance: it works in short transactions so the single writer connection
// is never held for long, and it stops at a row budget so it cannot starve
// interactive work (master plan 3 rule 12); call it again while More is true.
type CompactOptions struct {
	// Grace is how long a tombstone stays before its rows are physically
	// removed. Zero removes every tombstone at once.
	Grace time.Duration
	// KeepSuperseded is how many superseded generations per source are kept for
	// rollback (default 1; negative keeps none).
	KeepSuperseded int
	// RetireGrace is how long a failed, invalidated or surplus superseded
	// generation is kept after it stopped serving, for diagnosis (default 0).
	RetireGrace time.Duration
	// StaleBuild fails builds that are building or validating and have taken no
	// write for longer than this, so an abandoned build neither blocks the source
	// nor lingers (default 24h; negative disables). A build is judged by its last
	// activity, never by its age: a long one that is still progressing stays.
	StaleBuild time.Duration
	// MaxRows is the budget of chunk rows removed per call (default 20000).
	MaxRows int
	// BatchRows is the chunk rows removed per transaction (default 2000).
	BatchRows int
}

func (o CompactOptions) withDefaults() CompactOptions {
	if o.KeepSuperseded == 0 {
		o.KeepSuperseded = 1
	}
	if o.KeepSuperseded < 0 {
		o.KeepSuperseded = 0
	}
	if o.StaleBuild == 0 {
		o.StaleBuild = 24 * time.Hour
	}
	if o.MaxRows <= 0 {
		o.MaxRows = 20_000
	}
	if o.BatchRows <= 0 {
		o.BatchRows = 2_000
	}
	if o.BatchRows > o.MaxRows {
		o.BatchRows = o.MaxRows
	}
	return o
}

// CompactStats is what a pass removed.
type CompactStats struct {
	// Chunks, Parents and Documents are the rows physically removed.
	Chunks    int
	Parents   int
	Documents int
	// Generations is the generation rows removed.
	Generations int
	// Abandoned is the stale builds marked failed.
	Abandoned int
	// More is true when the row budget ran out before the work did.
	More bool
}

// maintenanceScope builds the scope compaction uses to reuse the scoped
// helpers for one (application, tenant, workspace). Compaction is not a
// request path: it enumerates every scope that holds index data, and acts on
// one scope at a time through the same predicates as everything else.
func maintenanceScope(app, tenant, ws string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "ragindex-maintenance",
		ApplicationID:    app,
		TenantID:         tenant,
		WorkspaceID:      ws,
		PrincipalID:      "svc_ragindex_compact",
		Roles:            []string{"service"},
		CapabilityScopes: []string{"ragindex.compact"},
		SessionID:        "ragindex-compact",
	}
}

// Compact physically removes what is already invisible: tombstoned chunks,
// parents and documents past their grace, generations that failed, were
// invalidated or are surplus superseded ones, and abandoned builds. Removing a
// chunk also removes its full-text entry and its identifiers (triggers), so the
// index shrinks. Nothing visible to a query changes: tombstoned rows and
// non-active generations were already excluded. It works through every scope
// and is a deployment-level pass; the maintenance of one scope is CompactScope.
func (ix *Index) Compact(ctx context.Context, opts CompactOptions) (CompactStats, error) {
	opts = opts.withDefaults()
	var st CompactStats
	now := ix.clk.Now()

	type scopeKey struct{ app, tenant, ws string }
	var scopes []scopeKey
	if err := ix.db.View(ctx, func(tx *store.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT application_id, tenant_id, workspace_id FROM ragindex_scope ORDER BY scope_no`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k scopeKey
			if err := rows.Scan(&k.app, &k.tenant, &k.ws); err != nil {
				return err
			}
			scopes = append(scopes, k)
		}
		return rows.Err()
	}); err != nil {
		return st, err
	}

	for _, k := range scopes {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		scope := maintenanceScope(k.app, k.tenant, k.ws)
		if err := scope.Validate(); err != nil {
			// A stored scope that no longer validates is left alone, not
			// deleted blindly.
			continue
		}
		if err := ix.compactScope(ctx, scope, now, opts, &st); err != nil {
			return st, err
		}
		if st.More {
			break
		}
	}
	return st, ix.mergeSegments(ctx, st)
}

// CompactScope is Compact for one (application, tenant, workspace) only: the
// maintenance of one scope neither reads nor changes another scope's rows or
// builds.
func (ix *Index) CompactScope(ctx context.Context, scope contracts.Scope, opts CompactOptions) (CompactStats, error) {
	if err := validateScope(scope); err != nil {
		return CompactStats{}, err
	}
	opts = opts.withDefaults()
	var st CompactStats
	if err := ix.compactScope(ctx, scope, ix.clk.Now(), opts, &st); err != nil {
		return st, err
	}
	return st, ix.mergeSegments(ctx, st)
}

// mergeSegments lets FTS5 fold the segments the deletes of a pass left behind,
// a bounded amount per pass; a full rewrite is Optimize.
func (ix *Index) mergeSegments(ctx context.Context, st CompactStats) error {
	if st.Chunks == 0 {
		return nil
	}
	if _, err := ix.db.ExecContext(ctx, `INSERT INTO ragindex_fts (ragindex_fts, rank) VALUES ('merge', 500)`); err != nil {
		return fmt.Errorf("index: merging full-text segments: %w", err)
	}
	return nil
}

// Optimize rewrites the full-text index into one segment. It is expensive
// (it rewrites everything); run it after large deletions, off-peak.
func (ix *Index) Optimize(ctx context.Context) error {
	if _, err := ix.db.ExecContext(ctx, `INSERT INTO ragindex_fts (ragindex_fts) VALUES ('optimize')`); err != nil {
		return fmt.Errorf("index: optimizing the full-text index: %w", err)
	}
	return nil
}

func (ix *Index) compactScope(ctx context.Context, scope contracts.Scope, now time.Time, opts CompactOptions, st *CompactStats) error {
	budget := func() int { return opts.MaxRows - st.Chunks }
	tombCutoff := formatTime(now.Add(-opts.Grace))

	// 1. Abandoned builds: silent for the stale period.
	if opts.StaleBuild > 0 {
		var n int64
		err := ix.db.Tx(ctx, func(tx *store.Tx) error {
			res, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
				SQL:       `UPDATE ragindex_generation SET state = 'failed', error_code = 'timeout', error_message = 'abandoned build'`,
				Where:     `state IN ('building', 'validating') AND COALESCE(updated_at, created_at) <= ?`,
				WhereArgs: []any{formatTime(now.Add(-opts.StaleBuild))},
			})
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			return err
		})
		if err != nil {
			return err
		}
		st.Abandoned += int(n)
	}

	// 2. Tombstoned chunks, in bounded batches.
	for budget() > 0 {
		batch := min(opts.BatchRows, budget())
		n, err := ix.deleteChunkBatch(ctx, scope, `tombstoned_at IS NOT NULL AND tombstoned_at <= ?`, []any{tombCutoff}, batch)
		if err != nil {
			return err
		}
		st.Chunks += n
		if n < batch {
			break
		}
	}
	if budget() <= 0 {
		st.More = true
		return nil
	}
	for _, table := range []string{"ragindex_parent", "ragindex_document"} {
		n, err := ix.deleteWhere(ctx, scope, table, `tombstoned_at IS NOT NULL AND tombstoned_at <= ?`, []any{tombCutoff})
		if err != nil {
			return err
		}
		if table == "ragindex_parent" {
			st.Parents += n
		} else {
			st.Documents += n
		}
	}

	// 3. Generations that no longer need to exist.
	gens, err := ix.removableGenerations(ctx, scope, now, opts)
	if err != nil {
		return err
	}
	for _, g := range gens {
		for budget() > 0 {
			batch := min(opts.BatchRows, budget())
			n, err := ix.deleteChunkBatch(ctx, scope, `source_id = ? AND generation_id = ?`, []any{g.sourceID, g.id}, batch)
			if err != nil {
				return err
			}
			st.Chunks += n
			if n < batch {
				break
			}
		}
		if budget() <= 0 {
			// Maybe finished exactly at the budget; the next pass will know.
			remaining, err := ix.countChunks(ctx, scope, g)
			if err != nil {
				return err
			}
			if remaining > 0 {
				st.More = true
				return nil
			}
		}
		n, err := ix.deleteWhere(ctx, scope, "ragindex_parent", `generation_id = ?`, []any{g.id})
		if err != nil {
			return err
		}
		st.Parents += n
		n, err = ix.deleteWhere(ctx, scope, "ragindex_document", `generation_id = ?`, []any{g.id})
		if err != nil {
			return err
		}
		st.Documents += n
		n, err = ix.deleteWhere(ctx, scope, "ragindex_generation", `generation_id = ? AND state IN ('failed', 'invalidated', 'superseded')`, []any{g.id})
		if err != nil {
			return err
		}
		st.Generations += n
	}
	return nil
}

type removable struct{ id, sourceID string }

// removableGenerations lists the generations of a scope that can go: failed
// and invalidated ones past the retire grace, and superseded ones beyond the
// number kept per source.
func (ix *Index) removableGenerations(ctx context.Context, scope contracts.Scope, now time.Time, opts CompactOptions) ([]removable, error) {
	cutoff := formatTime(now.Add(-opts.RetireGrace))
	var out []removable
	err := ix.db.View(ctx, func(tx *store.Tx) error {
		rows, err := store.QueryScoped(ctx, tx, scope, store.ScopedStmt{
			SQL:   `SELECT generation_id, source_id, state, COALESCE(retired_at, created_at) FROM ragindex_generation`,
			Where: `state IN ('failed', 'invalidated', 'superseded')`,
			Tail:  `ORDER BY source_id, COALESCE(retired_at, created_at) DESC, generation_id DESC`,
		})
		if err != nil {
			return err
		}
		defer rows.Close()
		keptSuperseded := map[string]int{}
		for rows.Next() {
			var id, src, state, when string
			if err := rows.Scan(&id, &src, &state, &when); err != nil {
				return err
			}
			if state == "superseded" && keptSuperseded[src] < opts.KeepSuperseded {
				keptSuperseded[src]++
				continue
			}
			if when <= cutoff {
				out = append(out, removable{id: id, sourceID: src})
			}
		}
		return rows.Err()
	})
	return out, err
}

func (ix *Index) countChunks(ctx context.Context, scope contracts.Scope, g removable) (int, error) {
	var n int
	err := ix.db.View(ctx, func(tx *store.Tx) error {
		row, err := store.QueryRowScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: `SELECT count(*) FROM ragindex_chunk`, Where: `source_id = ? AND generation_id = ?`, WhereArgs: []any{g.sourceID, g.id},
		})
		if err != nil {
			return err
		}
		return row.Scan(&n)
	})
	return n, err
}

// deleteChunkBatch deletes up to limit chunk rows of the scope matching cond.
// The delete triggers remove each row's full-text entry and identifiers in the
// same transaction.
func (ix *Index) deleteChunkBatch(ctx context.Context, scope contracts.Scope, cond string, args []any, limit int) (int, error) {
	pred, pargs, err := store.ScopedWhere(scope)
	if err != nil {
		return 0, err
	}
	sub := `rid IN (SELECT rid FROM ragindex_chunk WHERE ` + pred + ` AND ` + cond + ` LIMIT ?)`
	subArgs := append(append(append([]any(nil), pargs...), args...), limit)
	var n int64
	err = ix.db.Tx(ctx, func(tx *store.Tx) error {
		res, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: `DELETE FROM ragindex_chunk`, Where: sub, WhereArgs: subArgs,
		})
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}

func (ix *Index) deleteWhere(ctx context.Context, scope contracts.Scope, table, cond string, args []any) (int, error) {
	var n int64
	err := ix.db.Tx(ctx, func(tx *store.Tx) error {
		res, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: `DELETE FROM ` + table, Where: cond, WhereArgs: args,
		})
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}
