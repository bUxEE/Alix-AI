package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// maxCompactPasses bounds one maintenance job: compaction works in budgeted
// passes, and a backlog larger than this is finished by the next job.
const maxCompactPasses = 50

// RequestMaintenance queues a maintenance pass for the scope (class 6, so it
// yields to everything else): it finishes deletions that were recorded and not
// yet applied, compacts what is already hidden (tombstoned rows past the grace
// period, retired generations, abandoned builds), removes the working rows and
// the embeddings of generations the index no longer holds, and sweeps vectors
// that belong to no generation that can serve. One request per scope and hour
// is kept.
func (s *Service) RequestMaintenance(ctx context.Context, scope contracts.Scope, runAt time.Time) (jobs.EnqueueResult, error) {
	return s.requestMaintenanceAs(ctx, scope, "", runAt)
}

func (s *Service) requestMaintenanceAs(ctx context.Context, scope contracts.Scope, serviceIdentity string, runAt time.Time) (jobs.EnqueueResult, error) {
	if err := scope.Validate(); err != nil {
		return jobs.EnqueueResult{}, err
	}
	at := runAt
	if at.IsZero() {
		at = s.clk.Now()
	}
	key := fmt.Sprintf("maint:%s:%s:%s:%d", scope.ApplicationID, scope.TenantID, scope.WorkspaceID, at.Unix()/3600)
	return s.enqueueFresh(ctx, scope, serviceIdentity, JobMaintain, maintainPayload{}, key, runAt)
}

// DefaultMaintenanceInterval is how often the composition root calls
// ScheduleMaintenance: the requests of one hour coalesce into one job per scope.
const DefaultMaintenanceInterval = time.Hour

// The service principal of a scheduled maintenance job's scope.
const (
	maintPrincipal  = "svc_rag_maintenance"
	maintSession    = "ses_rag_maintenance"
	maintRole       = "service"
	maintCapability = "rag.maintenance"
)

// MaintenanceSchedule says what a ScheduleMaintenance pass did.
type MaintenanceSchedule struct {
	// Scopes is how many scopes holding a source were looked at.
	Scopes int
	// Enqueued is how many maintenance jobs were queued and Existing how many
	// were already queued for the current hour.
	Enqueued int
	Existing int
}

// ScheduleMaintenance queues a maintenance pass (RequestMaintenance) for every
// scope that holds a source, removed ones included, so that what a deletion
// hid, what a rebuild superseded and what a failed build left behind is purged
// with nothing but the passing of time: deleted text does not wait for a source
// removal to leave the database. A request coalesces into the job of its scope
// and hour that is still waiting, so calling it more often is harmless. The
// jobs run under a service principal of the deployment and declare
// serviceIdentity, which the pool's ServiceAuthorizer must accept for
// JobMaintain ("" runs them under the service scope itself).
//
// It reads across every scope, which is the nature of a maintenance pass, so it
// is for the composition root and never for a request. When the queue is full
// the pass stops with resource_exhausted and the next pass continues.
func (s *Service) ScheduleMaintenance(ctx context.Context, deploymentID, serviceIdentity string) (MaintenanceSchedule, error) {
	var res MaintenanceSchedule
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT application_id, tenant_id, workspace_id FROM ragingest_source ORDER BY application_id, tenant_id, workspace_id`)
	if err != nil {
		return res, err
	}
	var scopes [][3]string
	for rows.Next() {
		var k [3]string
		if err := rows.Scan(&k[0], &k[1], &k[2]); err != nil {
			rows.Close()
			return res, err
		}
		scopes = append(scopes, k)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, err
	}
	rows.Close()
	for _, k := range scopes {
		res.Scopes++
		scope := contracts.Scope{
			DeploymentID: deploymentID, ApplicationID: k[0], TenantID: k[1], WorkspaceID: k[2],
			PrincipalID: maintPrincipal, Roles: []string{maintRole}, CapabilityScopes: []string{maintCapability}, SessionID: maintSession,
		}
		out, err := s.requestMaintenanceAs(ctx, scope, serviceIdentity, time.Time{})
		if err != nil {
			return res, err
		}
		if out.Duplicate {
			res.Existing++
		} else {
			res.Enqueued++
		}
	}
	return res, nil
}

// requestMaintenance is RequestMaintenance for a caller that has already done
// its work: the pass is housekeeping, so a failure to queue it is logged and
// not returned.
func (s *Service) requestMaintenance(ctx context.Context, scope contracts.Scope, runAt time.Time) {
	if _, err := s.RequestMaintenance(ctx, scope, runAt); err != nil {
		s.log.Warn("maintenance could not be queued", "err", err.Error())
	}
}

func (s *Service) maintainHandler(ctx context.Context, job jobs.Job) error {
	var p maintainPayload
	if err := decodePayload(job.Payload, &p); err != nil {
		return err
	}
	return s.maintain(ctx, job.Scope)
}

func (s *Service) maintain(ctx context.Context, scope contracts.Scope) error {
	// 1. Deletions recorded and not yet applied.
	open, err := s.openTombstoneSources(ctx, scope)
	if err != nil {
		return err
	}
	for _, id := range open {
		if err := s.applyOutbox(ctx, scope, id); err != nil {
			return err
		}
	}
	// 2. Physical removal of what is hidden, in this scope only.
	for pass := 0; pass < maxCompactPasses; pass++ {
		st, err := s.ix.CompactScope(ctx, scope, index.CompactOptions{Grace: s.cfg.CompactGrace})
		if err != nil {
			return err
		}
		if !st.More {
			break
		}
	}
	// 3. Working rows and vectors of generations the index no longer holds.
	if err := s.sweepGenerations(ctx, scope); err != nil {
		return err
	}
	if err := s.sweepOrphanVectors(ctx, scope); err != nil {
		return err
	}
	// 4. Tombstone rows past their purge window.
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM ragingest_tombstone WHERE application_id = ? AND tenant_id = ? AND workspace_id = ?
 AND applied_ns > 0 AND purge_after_ns > 0 AND purge_after_ns <= ?`,
		scope.ApplicationID, scope.TenantID, scope.WorkspaceID, nsOf(s.clk.Now()))
	return err
}

func (s *Service) openTombstoneSources(ctx context.Context, scope contracts.Scope) ([]string, error) {
	rows, err := store.QueryScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: `SELECT DISTINCT source_id FROM ragingest_tombstone`, Where: `applied_ns = 0`, Tail: `ORDER BY source_id`,
	})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// sweepGenerations forgets the generations the index has compacted away: their
// versions, embedding batches and generation records, after their vectors are
// dropped.
func (s *Service) sweepGenerations(ctx context.Context, scope contracts.Scope) error {
	rows, err := store.QueryScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: `SELECT generation_id, source_id FROM ragingest_generation`, Tail: `ORDER BY created_ns`,
	})
	if err != nil {
		return err
	}
	type gen struct{ id, src string }
	var gens []gen
	for rows.Next() {
		var g gen
		if err := rows.Scan(&g.id, &g.src); err != nil {
			rows.Close()
			return err
		}
		gens = append(gens, g)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, g := range gens {
		if _, err := s.ix.GetGeneration(ctx, scope, g.id); err == nil {
			continue
		} else if !isNotFound(err) {
			return err
		}
		if err := s.dropGenerationVectors(ctx, scope, g.id); err != nil {
			return err
		}
		if err := s.db.Tx(ctx, func(tx *store.Tx) error {
			if err := supersedeVersions(ctx, tx, scope, `generation_id = ?`, []any{g.id}, "generation_compacted", s.clk.Now()); err != nil {
				return err
			}
			for _, table := range []string{"ragingest_version", "ragingest_generation"} {
				if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
					SQL: `DELETE FROM ` + table, Where: `generation_id = ?`, WhereArgs: []any{g.id},
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// dropResumer is a vector index that can finish the deletions a crash or a
// cancelled context interrupted (vector.Flat.ResumeDrops).
type dropResumer interface {
	ResumeDrops(ctx context.Context) (int, error)
}

// sweepOrphanVectors removes vector generations that no index generation can
// serve: ones the index failed, withdrew or no longer knows. A batch that was
// stored while its generation was being withdrawn leaves exactly such a row.
// It first finishes interrupted deletions: a generation hidden for deletion is
// left out of Stats, so nothing below would ever see it, and its rows would
// keep counting against the vector ceilings. ResumeDrops works across every
// scope and only removes what an earlier deletion already hid, so its failure
// (possibly in another scope) is logged and does not stop this scope's pass.
func (s *Service) sweepOrphanVectors(ctx context.Context, scope contracts.Scope) error {
	if s.vec == nil {
		return nil
	}
	if r, ok := s.vec.(dropResumer); ok {
		if _, err := r.ResumeDrops(ctx); err != nil {
			s.log.Warn("interrupted vector deletions could not be finished", "err", err.Error())
		}
	}
	st, err := s.vec.Stats(ctx, scope)
	if err != nil {
		return err
	}
	for _, g := range st.Generations {
		ig, err := s.ix.GetGeneration(ctx, scope, g.Generation)
		switch {
		case err != nil && !isNotFound(err):
			return err
		case err == nil && ig.State != rag.IndexGenerationStateFailed && ig.State != rag.IndexGenerationStateInvalidated:
			continue
		}
		if _, err := s.vec.DeleteGeneration(ctx, scope, g.Generation); err != nil {
			return err
		}
	}
	return nil
}
