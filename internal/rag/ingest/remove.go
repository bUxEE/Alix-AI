package ingest

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Deletion events carry counts and ids, never paths or text.
type sourceRemovedEvent struct {
	SourceID string `json:"source_id"`
	Items    int    `json:"items"`
	Mode     string `json:"mode"`
}

type tenantRemovedEvent struct {
	Sources int `json:"sources"`
	Items   int `json:"items"`
}

// RemoveSource deletes a source (master plan 19): every item it held gets a
// deletion tombstone, the source is marked removed, and what it holds in the
// index is hidden at once and its embeddings removed (with the purge
// behaviour of its declaration; with disable they stay inert, never searched,
// because no generation of the source is active any more). Physical removal of
// the index rows follows by compaction after the grace period. The deletion is
// recorded in the audit log in the same transaction that records the
// tombstones: if it cannot be recorded, nothing is deleted.
//
// A source that this scope never scanned has nothing to delete and is left
// alone (the caller's registry stops it being scanned). It is idempotent, and a crash after the record and before the hiding is
// finished by the next sync of the source, the maintenance job or a repeat of
// the call. Stage jobs that are still queued for the source end without effect.
func (s *Service) RemoveSource(ctx context.Context, scope contracts.Scope, sourceID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !ids.Valid(sourceID, source.IDPrefix) {
		return contracts.New(contracts.ErrValidationFailed, "ingest: not a source id")
	}
	unlockSync := s.syncLock(scope, sourceID)
	defer unlockSync()
	unlock := s.lock(scope, sourceID)
	defer unlock()

	now := s.clk.Now()
	var items int
	var mode string
	if _, err := s.updateSource(ctx, scope, sourceID, false, func(tx *store.Tx, r *sourceRow) (bool, error) {
		items, mode = 0, r.DeletionMode
		if r.Availability == availRemoved {
			return false, nil
		}
		recs, err := s.loadSnapshot(ctx, tx, scope, sourceID)
		if err != nil {
			return false, err
		}
		items = len(recs)
		purgeAfter := time.Time{}
		if r.DeletionMode != modeDisable {
			purgeAfter = now.Add(r.PurgeAfter)
		}
		if err := tombstoneAll(ctx, tx, scope, sourceID, recs, ReasonRemovedSource, now, purgeAfter); err != nil {
			return false, err
		}
		if err := dropSourceRows(ctx, tx, scope, sourceID); err != nil {
			return false, err
		}
		r.Availability, r.Reason, r.ErrorCode, r.Message = availRemoved, ReasonRemoved, "", "the source was removed"
		r.Since, r.LiveItems, r.SnapshotSeq = now, 0, r.SnapshotSeq+1
		r.HoldDigest, r.HoldDeleted, r.HoldPrevious, r.HoldWhy, r.HoldAt = "", 0, 0, "", time.Time{}
		r.ConfirmDigest, r.ConfirmBy, r.ConfirmUntil = "", "", time.Time{}
		loc, err := audit.ForScope(scope)
		if err != nil {
			return false, err
		}
		_, err = s.aud.AppendTx(ctx, tx, loc, audit.NewEvent{
			Type: audit.EventDeletionSource, Data: sourceRemovedEvent{SourceID: sourceID, Items: items, Mode: r.DeletionMode},
		})
		return err == nil, err
	}); err != nil {
		if isGone(err) {
			// Nothing was ever scanned in this scope under this id: there is
			// nothing derived to delete, and a scope never sees another's source.
			return nil
		}
		return err
	}
	if err := s.applyRemovalLocked(ctx, scope, sourceID, mode, true); err != nil {
		return err
	}
	s.requestMaintenance(ctx, scope, now.Add(s.cfg.CompactGrace))
	return nil
}

// ReasonRemovedSource is the tombstone reason of an item deleted with its source.
const ReasonRemovedSource = string(source.ReasonSourceRemoved)

// ReasonTenantRemoved is the tombstone reason of an item deleted with its tenant.
const ReasonTenantRemoved = "tenant_deleted"

// tombstoneAll writes deletion tombstones for records and removes the
// snapshot rows.
func tombstoneAll(ctx context.Context, tx *store.Tx, scope contracts.Scope, sourceID string, recs []source.Record, reason string, at, purgeAfter time.Time) error {
	for _, r := range recs {
		if _, err := store.InsertScoped(ctx, tx, scope, "ragingest_tombstone",
			[]string{"source_id", "item_key", "document_id", "reason", "last_hash", "at_ns", "purge_after_ns", "applied_ns"},
			sourceID, r.Key, documentID(sourceID, r.Key), reason, r.Hash, nsOf(at), nsOf(purgeAfter), 0); err != nil {
			return err
		}
	}
	_, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL: `DELETE FROM ragingest_item`, Where: `source_id = ?`, WhereArgs: []any{sourceID},
	})
	return err
}

// dropSourceRows removes the pipeline's own working rows of a source: its
// versions, staged parses, embedding batches and generation records. The
// derived data in the index and the vector index is removed by the callers.
func dropSourceRows(ctx context.Context, tx *store.Tx, scope contracts.Scope, sourceID string) error {
	for _, q := range []string{
		`DELETE FROM ragingest_stage WHERE version_id IN (SELECT version_id FROM ragingest_version WHERE source_id = ? AND application_id = ? AND tenant_id = ? AND workspace_id = ?)
 AND application_id = ? AND tenant_id = ? AND workspace_id = ?`,
		`DELETE FROM ragingest_embed WHERE version_id IN (SELECT version_id FROM ragingest_version WHERE source_id = ? AND application_id = ? AND tenant_id = ? AND workspace_id = ?)
 AND application_id = ? AND tenant_id = ? AND workspace_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, sourceID, scope.ApplicationID, scope.TenantID, scope.WorkspaceID,
			scope.ApplicationID, scope.TenantID, scope.WorkspaceID); err != nil {
			return err
		}
	}
	for _, table := range []string{"ragingest_version", "ragingest_generation"} {
		if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: `DELETE FROM ` + table, Where: `source_id = ?`, WhereArgs: []any{sourceID},
		}); err != nil {
			return err
		}
	}
	return nil
}

// RemoveTenant deletes everything ingestion holds for the scope's tenant, in
// every workspace (master plan 19: deleting a tenant produces deletion
// tombstones and invalidates caches before physical compaction). The tenant
// comes from the scope, never from an argument. It is an operator action: the
// OperatorAuthorizer must accept ActionDeleteTenant, and with none configured
// it is denied.
//
// Each of the tenant's sources is marked removed and its items are tombstoned,
// with the deletion audited on the tenant's chain (the chain is never cut), all
// in one transaction. Then the index hides the tenant's data in every
// workspace (and withdraws its generations), and the vector index is purged
// workspace by workspace.
func (s *Service) RemoveTenant(ctx context.Context, scope contracts.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if s.operator == nil {
		return contracts.New(contracts.ErrPermissionDenied, "ingest: no operator authorisation is configured; a tenant cannot be deleted")
	}
	if err := s.operator.AuthorizeOperator(ctx, scope, ActionDeleteTenant); err != nil {
		if c := contracts.CodeOf(err); c == contracts.ErrPermissionDenied || c == contracts.ErrScopeViolation {
			return err
		}
		return contracts.Wrap(contracts.ErrPermissionDenied, err, "ingest: the operator action is not authorised")
	}
	now := s.clk.Now()
	type target struct{ ws, src string }
	var targets []target
	var items int
	if err := s.db.Tx(ctx, func(tx *store.Tx) error {
		targets, items = nil, 0
		rows, err := tx.QueryContext(ctx,
			`SELECT workspace_id, source_id FROM ragingest_source WHERE application_id = ? AND tenant_id = ? ORDER BY workspace_id, source_id`,
			scope.ApplicationID, scope.TenantID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t target
			if err := rows.Scan(&t.ws, &t.src); err != nil {
				rows.Close()
				return err
			}
			targets = append(targets, t)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, t := range targets {
			ws := scope
			ws.WorkspaceID = t.ws
			r, ok, err := loadSource(ctx, tx, ws, t.src)
			if err != nil || !ok {
				return err
			}
			if r.Availability != availRemoved {
				recs, err := s.loadSnapshot(ctx, tx, ws, t.src)
				if err != nil {
					return err
				}
				items += len(recs)
				if err := tombstoneAll(ctx, tx, ws, t.src, recs, ReasonTenantRemoved, now, time.Time{}); err != nil {
					return err
				}
				if err := dropSourceRows(ctx, tx, ws, t.src); err != nil {
					return err
				}
			}
			r.Availability, r.Reason, r.ErrorCode, r.Message = availRemoved, ReasonTenantDeleted, "", "the tenant was deleted"
			r.Since, r.LiveItems, r.SnapshotSeq, r.Updated = now, 0, r.SnapshotSeq+1, now
			r.HoldDigest, r.HoldDeleted, r.HoldPrevious, r.HoldWhy, r.HoldAt = "", 0, 0, "", time.Time{}
			r.ConfirmDigest, r.ConfirmBy, r.ConfirmUntil = "", "", time.Time{}
			if err := writeSource(ctx, tx, ws, r, false); err != nil {
				return err
			}
		}
		loc, err := audit.ForScope(scope)
		if err != nil {
			return err
		}
		_, err = s.aud.AppendTx(ctx, tx, loc, audit.NewEvent{
			Type: audit.EventDeletionTenant, Data: tenantRemovedEvent{Sources: len(targets), Items: items},
		})
		return err
	}); err != nil {
		return err
	}

	if _, err := s.ix.DeleteTenant(ctx, scope); err != nil {
		return err
	}
	if s.vec != nil {
		seen := map[string]bool{}
		for _, t := range targets {
			if seen[t.ws] {
				continue
			}
			seen[t.ws] = true
			ws := scope
			ws.WorkspaceID = t.ws
			if _, err := s.vec.DeleteScope(ctx, ws); err != nil {
				return err
			}
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE ragingest_tombstone SET applied_ns = ? WHERE application_id = ? AND tenant_id = ? AND applied_ns = 0`,
		nsOf(now), scope.ApplicationID, scope.TenantID); err != nil {
		return err
	}
	// Maintenance works on one workspace: each of the tenant's gets its own pass.
	s.requestMaintenance(ctx, scope, now.Add(s.cfg.CompactGrace))
	queued := map[string]bool{scope.WorkspaceID: true}
	for _, t := range targets {
		if queued[t.ws] {
			continue
		}
		queued[t.ws] = true
		ws := scope
		ws.WorkspaceID = t.ws
		s.requestMaintenance(ctx, ws, now.Add(s.cfg.CompactGrace))
	}
	return nil
}
