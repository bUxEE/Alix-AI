package ingest

import (
	"context"

	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The deletion outbox (master plan 19; SEC-028).
//
// A deletion is first written down: the snapshot row goes, the tombstone row
// is added and the item's versions are superseded in ONE transaction, so there
// is no moment at which the item is forgotten but not recorded as deleted. The
// index and the vector index are then told (hidden at once, purged after the
// grace period), and the tombstone row is marked applied. A crash between the
// two leaves an open row, which the next sync, the next removal call and the
// maintenance job finish. Hiding is idempotent.

// applyOutbox applies the open deletions of a source. It takes the write lock.
func (s *Service) applyOutbox(ctx context.Context, scope contracts.Scope, sourceID string) error {
	unlock := s.lock(scope, sourceID)
	defer unlock()
	return s.applyOutboxLocked(ctx, scope, sourceID)
}

// applyOutboxLocked is applyOutbox for a caller that holds the write lock.
func (s *Service) applyOutboxLocked(ctx context.Context, scope contracts.Scope, sourceID string) error {
	src, found, err := loadSource(ctx, s.db, scope, sourceID)
	if err != nil {
		return err
	}
	if found && src.Availability == availRemoved {
		return s.applyRemovalLocked(ctx, scope, sourceID, src.DeletionMode, false)
	}
	for {
		type open struct {
			key, doc string
			at       int64
		}
		rows, err := store.QueryScoped(ctx, s.db, scope, store.ScopedStmt{
			SQL: `SELECT item_key, document_id, at_ns FROM ragingest_tombstone`, Where: `source_id = ? AND applied_ns = 0`, WhereArgs: []any{sourceID},
			Tail: `ORDER BY at_ns, item_key LIMIT 500`,
		})
		if err != nil {
			return err
		}
		var batch []open
		for rows.Next() {
			var o open
			if err := rows.Scan(&o.key, &o.doc, &o.at); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, o)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}
		for _, o := range batch {
			if _, err := s.ix.DeleteDocument(ctx, scope, sourceID, o.doc); err != nil {
				return err
			}
		}
		if err := s.cleanupVectors(ctx, scope, sourceID); err != nil {
			return err
		}
		now := nsOf(s.clk.Now())
		if err := s.db.Tx(ctx, func(tx *store.Tx) error {
			for _, o := range batch {
				if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
					SQL: `UPDATE ragingest_tombstone SET applied_ns = ?`, Args: []any{now},
					Where: `source_id = ? AND item_key = ? AND at_ns = ?`, WhereArgs: []any{sourceID, o.key, o.at},
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
}

// applyRemovalLocked applies the removal of a whole source: everything it holds
// in the index is hidden and its generations withdrawn at once, and with the
// purge behaviour the embeddings are removed too (they cannot be searched
// anyway: no generation of the source is active). Physical removal of the
// index rows follows by compaction after the grace period.
//
// Without force it does nothing when no tombstone is open, so a stale job of a
// removed source does not touch the index again.
func (s *Service) applyRemovalLocked(ctx context.Context, scope contracts.Scope, sourceID, mode string, force bool) error {
	if !force {
		open, err := s.openTombstones(ctx, scope, sourceID)
		if err != nil || open == 0 {
			return err
		}
	}
	if _, err := s.ix.DeleteSource(ctx, scope, sourceID); err != nil {
		return err
	}
	if s.vec != nil && mode != modeDisable {
		if _, err := s.vec.DeleteSource(ctx, scope, sourceID); err != nil {
			return err
		}
	}
	return s.markApplied(ctx, scope, sourceID)
}

func (s *Service) openTombstones(ctx context.Context, scope contracts.Scope, sourceID string) (int, error) {
	q, err := store.QueryRowScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: `SELECT count(*) FROM ragingest_tombstone`, Where: `source_id = ? AND applied_ns = 0`, WhereArgs: []any{sourceID},
	})
	if err != nil {
		return 0, err
	}
	var n int
	return n, q.Scan(&n)
}

func (s *Service) markApplied(ctx context.Context, scope contracts.Scope, sourceID string) error {
	_, err := store.ExecScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: `UPDATE ragingest_tombstone SET applied_ns = ?`, Args: []any{nsOf(s.clk.Now())},
		Where: `source_id = ? AND applied_ns = 0`, WhereArgs: []any{sourceID},
	})
	return err
}

// cleanupVectors removes from the vector index the embeddings a version owes it
// (vec_owed): chunks of a replaced version that its successor no longer has,
// and every chunk of a deleted item. The index's own rows are replaced or
// tombstoned by the index; the vector index does not follow them, and a vector
// is as retained and as deletable as the text it came from (master plan 19).
// A chunk the item's current version also holds, under the same chunk id, is
// left alone: that is the embedding the next search needs.
func (s *Service) cleanupVectors(ctx context.Context, scope contracts.Scope, sourceID string) error {
	for round := 0; round < 200; round++ {
		owed, err := queryVersions(ctx, s.db, scope, `source_id = ? AND vec_owed = 1`, []any{sourceID}, `ORDER BY created_ns, version_id LIMIT 100`)
		if err != nil {
			return err
		}
		if len(owed) == 0 {
			return nil
		}
		for _, v := range owed {
			if s.vec != nil && len(v.ChunkIDs) > 0 {
				keep := map[string]bool{}
				live, err := queryVersions(ctx, s.db, scope, `generation_id = ? AND item_key = ? AND state = 'indexed' AND vec_owed = 0`, []any{v.GenerationID, v.Key}, "")
				if err != nil {
					return err
				}
				for _, l := range live {
					for _, id := range l.ChunkIDs {
						keep[id] = true
					}
				}
				var vanished []string
				for _, id := range v.ChunkIDs {
					if !keep[id] {
						vanished = append(vanished, id)
					}
				}
				for start := 0; start < len(vanished); start += vector.MaxBatch {
					end := min(start+vector.MaxBatch, len(vanished))
					if _, err := s.vec.DeleteChunks(ctx, scope, v.GenerationID, vanished[start:end]); err != nil {
						return err
					}
				}
			}
			if _, err := setVersion(ctx, s.db, scope, v.ID, `vec_owed = 0, chunk_ids = '[]', updated_ns = ?`, []any{nsOf(s.clk.Now())}); err != nil {
				return err
			}
		}
	}
	return nil
}
