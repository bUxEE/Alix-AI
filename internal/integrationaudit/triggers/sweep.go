package triggers

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The job of the scheduled comparison (master plan 17.1, class 6, maintenance).
//
// This package does not import internal/jobs (the module map does not list it for
// integrationaudit): the composition root registers the type, queues through an
// Enqueuer it implements, and calls HandleJob from the handler it attaches to the
// pool, the way internal/history does for its retention job.
const (
	// JobType is the job type to register, as class 6 (jobs.ClassMaintenance).
	JobType = "integrationaudit.reaudit_watch"
	// JobClass is the priority class of the job: maintenance, which yields to
	// every interactive class.
	JobClass = 6
	// ServiceIdentity is the identity the job declares. The job does not run as a
	// user (a revoked user's token must not be able to stop a re-audit), so the
	// composition root's service authorizer must accept it for JobType only, and
	// HandleJob refuses a job that does not carry it.
	ServiceIdentity = "integrationaudit.reaudit_watch"
)

// JobPayload is the payload of the job: a reference, never data.
type JobPayload struct {
	ApplicationID string `json:"application_id"`
}

// Enqueuer queues one job of type JobType. The composition root implements it
// over the job queue: scope is the job's initiating scope (JobScope), key its
// idempotency key, and the job declares ServiceIdentity. duplicate is true when
// the key matched a job that already existed. It must return resource_exhausted,
// untouched, when the class is full.
type Enqueuer interface {
	EnqueueWatch(ctx context.Context, scope contracts.Scope, idempotencyKey string, payload json.RawMessage) (duplicate bool, err error)
}

// JobScope is the scope a watch job of an application runs under: valid at every
// level, bound to the application, and the identity of a service rather than of
// a user or a tenant. It reaches no tenant data.
func JobScope(deploymentID, applicationID string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     deploymentID,
		ApplicationID:    applicationID,
		TenantID:         "alixai-audit",
		WorkspaceID:      "alixai-audit",
		PrincipalID:      SystemPrincipal,
		Roles:            []string{"service"},
		CapabilityScopes: []string{"audit.reaudit"},
		SessionID:        "reaudit-triggers",
	}
}

func (w *Watcher) enqueue(ctx context.Context, applicationID, key string) (duplicate bool, err error) {
	if w.cfg.Enqueuer == nil {
		return false, nil
	}
	payload, err := json.Marshal(JobPayload{ApplicationID: applicationID})
	if err != nil {
		return false, contracts.Wrap(contracts.ErrInternal, err, "triggers: cannot encode the job payload")
	}
	scope := JobScope(w.cfg.DeploymentID, applicationID)
	if err := scope.Validate(); err != nil {
		return false, err
	}
	return w.cfg.Enqueuer.EnqueueWatch(ctx, scope, key, payload)
}

// SweepResult says what a Sweep did.
type SweepResult struct {
	// Applications is how many applications are watched.
	Applications int
	// Enqueued is how many watch jobs were queued and Existing how many were
	// already queued for the current period.
	Enqueued int
	Existing int
}

// Sweep queues one watch job for every application that has an active package or
// one the watcher quarantined, at most one per application per SweepPeriod (the
// idempotency key names the period, so calling it more often is harmless). It is
// the comparison "on a schedule" of master plan 4.5: the composition root calls
// it on a timer, and each job runs Check. It reads across every application,
// which is the nature of a maintenance pass, so it is for the composition root and
// never for a request. When the queue is full it stops with resource_exhausted
// and the next pass continues.
func (w *Watcher) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	if w.cfg.Enqueuer == nil || w.cfg.DB == nil {
		return res, contracts.New(contracts.ErrUnavailable, "triggers: a scheduled sweep needs a database and a job queue")
	}
	apps, err := w.watchedApplications(ctx)
	if err != nil {
		return res, err
	}
	period := w.cfg.Clock.Now().UTC().Truncate(w.cfg.SweepPeriod).Format("20060102T150405Z")
	for _, app := range apps {
		res.Applications++
		dup, err := w.enqueue(ctx, app, "watch:"+period)
		if err != nil {
			return res, err
		}
		if dup {
			res.Existing++
		} else {
			res.Enqueued++
		}
	}
	return res, nil
}

// watchedApplications lists the applications with an active package or a package
// the watcher quarantined (they still need a report that postdates it).
func (w *Watcher) watchedApplications(ctx context.Context) ([]string, error) {
	var out []string
	err := w.cfg.DB.View(ctx, func(tx *store.Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx,
			`SELECT DISTINCT application_id FROM apppkg_packages
			  WHERE status = 'active' OR (status = 'quarantined' AND substr(status_reason, 1, ?) = ?)
			  ORDER BY application_id`, len(ReasonPrefix), ReasonPrefix)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var app string
			if err := rows.Scan(&app); err != nil {
				return err
			}
			out = append(out, app)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "triggers: cannot list the applications to watch")
	}
	return out, nil
}

// HandleJob is the body of the job handler. serviceIdentity is the identity the
// job declared and payload its payload. A job that does not declare
// ServiceIdentity, or whose payload is anything but a JobPayload, is refused with
// a code that is not retryable, so it fails at once instead of being retried.
// Errors from Check keep their codes: an unavailable store, or a registry that still
// serves an application that was quarantined (also unavailable), is retried by the
// queue, and the next attempt confirms the stop again before it does anything else.
func (w *Watcher) HandleJob(ctx context.Context, serviceIdentity string, payload json.RawMessage) error {
	if serviceIdentity != ServiceIdentity {
		return contracts.New(contracts.ErrPermissionDenied, "triggers: the re-audit watch runs only under its service identity")
	}
	var p JobPayload
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil || p.ApplicationID == "" || dec.More() {
		return contracts.New(contracts.ErrValidationFailed, "triggers: the re-audit watch payload is not {\"application_id\": ...}")
	}
	_, err := w.Check(ctx, p.ApplicationID)
	return err
}
