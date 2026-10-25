package history

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	// JobRetention applies the retention policy to one workspace: it deletes the
	// transcript turns past it. It is class 6, maintenance, so it yields to
	// everything interactive.
	JobRetention = "history.retention"
	// ServiceIdentity is the identity the retention job declares. The job does not
	// run as a user (a revoked user's token must not be able to stop retention), so
	// the composition root's ServiceAuthorizer must accept it, and the handler
	// refuses a job that does not declare it.
	ServiceIdentity = "history.retention"

	maxRetentionAttempts = 3

	// The service principal of the retention job's scope.
	servicePrincipal = "svc_history_retention"
	serviceSession   = "ses_history_retention"
	serviceRole      = "service"
)

// JobSpec is the job type this package registers on the queue (New does it when
// a queue is given).
func JobSpec() jobs.TypeSpec {
	return jobs.TypeSpec{Name: JobRetention, Class: jobs.ClassMaintenance, MaxAttempts: maxRetentionAttempts}
}

// Handle attaches the retention handler to the pool.
func (s *Service) Handle(pool *jobs.Pool) error {
	if pool == nil {
		return contracts.New(contracts.ErrValidationFailed, "history: no job pool to attach the retention handler to")
	}
	return pool.Handle(JobRetention, s.retentionHandler)
}

// retentionHandler runs one retention job. Jobs are delivered at least once and
// the work is idempotent: it deletes what is past the policy in force now.
func (s *Service) retentionHandler(ctx context.Context, job jobs.Job) error {
	// The job deletes across a whole workspace, so it runs only as the declared
	// service: a job enqueued under a user's own scope is not this job's business.
	if job.ServiceIdentity != ServiceIdentity {
		return jobs.Permanent(contracts.New(contracts.ErrPermissionDenied, "history: the retention job runs only under its service identity"))
	}
	if len(bytes.TrimSpace(job.Payload)) > 0 && !bytes.Equal(bytes.TrimSpace(job.Payload), []byte("null")) {
		var p struct{}
		dec := json.NewDecoder(bytes.NewReader(job.Payload))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return jobs.Permanent(contracts.Wrap(contracts.ErrValidationFailed, err, "history: the retention job payload is malformed"))
		}
	}
	if err := job.Scope.Validate(); err != nil {
		return jobs.Permanent(err)
	}
	_, err := s.applyRetention(ctx, job.Scope)
	return err
}

// serviceScope is the scope the retention job of one workspace runs under.
func (s *Service) serviceScope(application, tenant, workspace string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     s.dep,
		ApplicationID:    application,
		TenantID:         tenant,
		WorkspaceID:      workspace,
		PrincipalID:      servicePrincipal,
		Roles:            []string{serviceRole},
		CapabilityScopes: []string{CapCleanup},
		SessionID:        serviceSession,
	}
}

// ScheduleResult says what a ScheduleRetention pass did.
type ScheduleResult struct {
	// Workspaces is how many workspaces holding transcript turns were looked at.
	Workspaces int
	// Enqueued is how many retention jobs were queued and Existing how many were
	// already queued for the current period.
	Enqueued int
	Existing int
}

// ScheduleRetention queues a retention job for every workspace that holds
// transcript turns past the retention in force for its tenant. A workspace with
// no policy, or with nothing that old, gets no job, so an idle deployment queues
// nothing. At most one job per workspace is queued per Config.RetentionInterval
// (the idempotency key names the period), so calling it more often is harmless.
// The jobs run under ServiceIdentity.
//
// It reads across every tenant, which is the nature of a maintenance pass, and so
// is for the composition root and never for a request. It needs the queue and
// deployment id of Deps; without them it is unavailable. When the queue is full
// the pass stops with resource_exhausted and the next pass continues.
func (s *Service) ScheduleRetention(ctx context.Context) (ScheduleResult, error) {
	var res ScheduleResult
	if s.q == nil {
		return res, contracts.New(contracts.ErrUnavailable, "history: scheduled retention needs a job queue")
	}
	period := s.now().Truncate(s.cfg.RetentionInterval).Format("20060102T150405Z")
	policies := map[[2]string]RetentionPolicy{}
	var after [3]string
	for {
		ws, err := s.workspacesAfter(ctx, after)
		if err != nil {
			return res, err
		}
		for _, w := range ws {
			res.Workspaces++
			after = w
			key := [2]string{w[0], w[1]}
			pol, ok := policies[key]
			if !ok {
				if pol, err = s.loadRetention(ctx, s.db, w[0], w[1]); err != nil {
					return res, err
				}
				policies[key] = pol
			}
			if pol.EffectiveDays == 0 {
				continue
			}
			scope := s.serviceScope(w[0], w[1], w[2])
			due, err := s.countOlder(ctx, scope, s.retentionCutoff(pol.EffectiveDays))
			if err != nil {
				return res, err
			}
			if due == 0 {
				continue
			}
			r, err := s.q.Enqueue(ctx, scope, jobs.EnqueueRequest{
				Type: JobRetention, IdempotencyKey: "retention:" + period, ServiceIdentity: ServiceIdentity,
			})
			if err != nil {
				return res, err
			}
			if r.Duplicate {
				res.Existing++
			} else {
				res.Enqueued++
			}
		}
		if len(ws) < s.cfg.SweepBatch {
			return res, nil
		}
	}
}

// workspacesAfter reads the next batch of distinct (application, tenant,
// workspace) triples that hold transcript turns, in key order, after the given
// one. It is the one deployment-wide read of this package.
func (s *Service) workspacesAfter(ctx context.Context, after [3]string) ([][3]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT application_id, tenant_id, workspace_id FROM transcript_turns
 WHERE (application_id, tenant_id, workspace_id) > (?, ?, ?)
 ORDER BY application_id, tenant_id, workspace_id LIMIT ?`,
		after[0], after[1], after[2], s.cfg.SweepBatch)
	if err != nil {
		return nil, wrapDB(err)
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var w [3]string
		if err := rows.Scan(&w[0], &w[1], &w[2]); err != nil {
			return nil, wrapDB(err)
		}
		out = append(out, w)
	}
	return out, wrapDB(rows.Err())
}
