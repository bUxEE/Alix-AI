package providerreg

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	// JobRevalidate is the maintenance job that runs Revalidate for one application. It is class
	// 6, so it yields to everything interactive, and it carries no payload: the application is
	// the job scope's.
	JobRevalidate = "providerreg.revalidate"
	// ServiceIdentity is the identity the job declares. The job does not run as a user (a revoked
	// user's token must not be able to stop a revalidation), so the composition root's
	// ServiceAuthorizer must accept it for JobRevalidate only, and the handler refuses a job that
	// does not declare it.
	ServiceIdentity = "providerreg.revalidate"

	// DefaultRevalidateInterval is how often ScheduleRevalidation runs when no interval is given.
	// A review goes stale at 00:00 UTC, so a sweep this often quarantines it within the hour even
	// when no request names the profile (Check still quarantines at the first request).
	DefaultRevalidateInterval = time.Hour

	maxRevalidateAttempts = 3

	// The service scope the job of every application runs under: it names the application and
	// reaches no tenant data.
	jobTenant, jobWorkspace = "alixai-providerreg", "alixai-providerreg"
	jobPrincipal            = "svc_providerreg_revalidate"
	jobSession              = "ses_providerreg_revalidate"
	jobRole                 = "service"
	jobCapability           = "providerreg.revalidate"
)

// JobSpec is the job type to register on the queue (class 6, maintenance).
func JobSpec() jobs.TypeSpec {
	return jobs.TypeSpec{Name: JobRevalidate, Class: jobs.ClassMaintenance, MaxAttempts: maxRevalidateAttempts}
}

// Handle attaches the revalidation handler to the pool.
func (r *Registry) Handle(pool *jobs.Pool) error {
	if pool == nil {
		return contracts.New(contracts.ErrValidationFailed, "providerreg: no job pool to attach the revalidation handler to")
	}
	return pool.Handle(JobRevalidate, r.revalidateHandler)
}

// revalidateHandler runs one revalidation job: it quarantines every candidate or qualified
// profile of the job's application whose review has expired. Jobs are delivered at least once
// and the work is idempotent (a quarantined profile is not touched again).
func (r *Registry) revalidateHandler(ctx context.Context, job jobs.Job) error {
	// The job changes lifecycles across an application, so it runs only as the declared service:
	// a job enqueued under a user's own scope is not this job's business.
	if job.ServiceIdentity != ServiceIdentity {
		return jobs.Permanent(contracts.New(contracts.ErrPermissionDenied, "providerreg: the revalidation job runs only under its service identity"))
	}
	if p := bytes.TrimSpace(job.Payload); len(p) > 0 && !bytes.Equal(p, []byte("null")) {
		var none struct{}
		dec := json.NewDecoder(bytes.NewReader(p))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&none); err != nil {
			return jobs.Permanent(contracts.Wrap(contracts.ErrValidationFailed, err, "providerreg: the revalidation job payload is malformed"))
		}
	}
	if err := job.Scope.Validate(); err != nil {
		return jobs.Permanent(err)
	}
	_, err := r.Revalidate(ctx, job.Scope)
	return err
}

// revalidationScope is the scope the revalidation job of one application runs under.
func revalidationScope(deploymentID, application string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     deploymentID,
		ApplicationID:    application,
		TenantID:         jobTenant,
		WorkspaceID:      jobWorkspace,
		PrincipalID:      jobPrincipal,
		Roles:            []string{jobRole},
		CapabilityScopes: []string{jobCapability},
		SessionID:        jobSession,
	}
}

// ScheduleResult says what a ScheduleRevalidation pass did.
type ScheduleResult struct {
	// Applications is how many applications hold a candidate or qualified profile.
	Applications int
	// Enqueued is how many revalidation jobs were queued and Existing how many were already
	// queued for the current period.
	Enqueued int
	Existing int
}

// ScheduleRevalidation queues one revalidation job for every application that holds a candidate
// or qualified profile, so that expired reviews are quarantined without a request arriving
// first. At most one job per application is queued per interval (the idempotency key names the
// period), so calling it more often is harmless; zero takes DefaultRevalidateInterval. An
// application with no live profile gets no job, so a deployment with external mode unused queues
// nothing. The jobs run under ServiceIdentity.
//
// It reads across every application, which is the nature of a maintenance pass, so it is for the
// composition root and never for a request. When the queue is full the pass stops with
// resource_exhausted and the next pass continues.
func (r *Registry) ScheduleRevalidation(ctx context.Context, q *jobs.Queue, deploymentID string, interval time.Duration) (ScheduleResult, error) {
	var res ScheduleResult
	if q == nil {
		return res, contracts.New(contracts.ErrUnavailable, "providerreg: scheduled revalidation needs a job queue")
	}
	if interval <= 0 {
		interval = DefaultRevalidateInterval
	}
	apps, err := r.liveApplications(ctx)
	if err != nil {
		return res, err
	}
	period := r.clk.Now().UTC().Truncate(interval).Format("20060102T150405Z")
	for _, app := range apps {
		res.Applications++
		scope := revalidationScope(deploymentID, app)
		if err := scope.Validate(); err != nil {
			return res, err
		}
		out, err := q.Enqueue(ctx, scope, jobs.EnqueueRequest{
			Type: JobRevalidate, IdempotencyKey: "revalidate:" + period, ServiceIdentity: ServiceIdentity,
		})
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

// RunRevalidationSchedule calls ScheduleRevalidation now and then every interval until ctx ends,
// and returns ctx's error. A failed pass is passed to onError (when not nil) and the schedule
// goes on: the lazy check in Check keeps a stale profile out in the meantime.
func (r *Registry) RunRevalidationSchedule(ctx context.Context, q *jobs.Queue, deploymentID string, interval time.Duration, onError func(error)) error {
	if interval <= 0 {
		interval = DefaultRevalidateInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if _, err := r.ScheduleRevalidation(ctx, q, deploymentID, interval); err != nil && onError != nil && ctx.Err() == nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// LiveApplications lists the applications that hold a candidate or qualified profile: the applications
// a deployment-wide maintenance job (the budget sweep of the composition root) has anything to do for.
func (r *Registry) LiveApplications(ctx context.Context) ([]string, error) {
	return r.liveApplications(ctx)
}

// liveApplications lists the applications that hold a candidate or qualified profile. It is the
// one deployment-wide read of this package.
func (r *Registry) liveApplications(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT DISTINCT application_id FROM providerreg_profiles
 WHERE tenant_id = '' AND workspace_id = '' AND lifecycle IN ('candidate', 'qualified')
 ORDER BY application_id`)
	if err != nil {
		return nil, wrapStore(err, "providerreg: listing applications failed")
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var app string
		if err := rows.Scan(&app); err != nil {
			return nil, wrapStore(err, "providerreg: listing applications failed")
		}
		out = append(out, app)
	}
	return out, wrapStore(rows.Err(), "providerreg: listing applications failed")
}
