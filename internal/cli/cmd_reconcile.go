package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Reconciling uncertain outcomes in `alixai serve` (master plan 18 "Commit response uncertain: query
// idempotency/status before retrying"; M5-25, the gap (c) of the M6-04 drill).
//
// A commit whose outcome could not be decided, even by asking the application's Status, ends
// `uncertain`: an uncertain receipt is written and the action is never retried blindly.
// actions.Coordinator.Reconcile asks the application again and never commits, but nothing called it,
// so an uncertain outcome stayed uncertain until somebody reconciled it by hand. This file is the
// caller.
//
// # What runs
//
//   - A loop (a part of the process, started with serve) that, at start and every opsInspectReconcileEvery,
//     queues one job per preparation that has been uncertain for at least that long. Sooner would ask
//     again an application that has just been asked: RecoverAtStartup asks it before serve listens.
//     The job is keyed by the preparation and the period, so a restart inside a period queues nothing
//     a second time. A pass reads at most opsInspectReconcileBatch rows and carries on from where it stopped,
//     so a backlog is worked through and the oldest rows that stay uncertain do not starve the rest.
//   - The job, type actions.reconcile, class 6 (maintenance), on the RAG stack's one queue and one
//     pool (ragStack.shareJobs; there is no second pool), so the resource governor holds it behind
//     every live turn and every higher class. The handler calls Coordinator.Reconcile and nothing
//     else.
//
// # The job and its authority
//
// Reconcile takes a scope, and the scope a job runs under is the one the preparation was made under
// (what Recover uses as well): the sweep reads it from the preparation's own row and the coordinator
// then loads the preparation with it, checks it is the same principal and session (SEC-040) and
// refuses anything else, so a job can reconcile only the preparation it names. The pool accepts
// opsInspectReconcileServiceIdentity for the reconcile job type only, with a payload that is exactly
// {"preparation_id": ...} and a scope equal to the one stored for that preparation. A declared
// identity alone grants nothing (jobs.ServiceAuthorizer).
//
// # What it never does
//
// It never commits and never retries a commit: Reconcile only asks Status. What it records is the
// coordinator's own, with the audit events the coordinator defines (a recorded outcome is a new
// receipt and action.committed from uncertain; proof that the application never applied the commit
// reopens the preparation under the same idempotency key). A preparation that is still undecided
// stays uncertain, and the next period asks again.

const (
	// opsInspectReconcileProviderName is the name of the serve provider. Providers run in name order and this
	// one needs the RAG stack's queue and pool, which "rag" builds.
	opsInspectReconcileProviderName = "rag_reconcile"

	// opsInspectReconcileServiceIdentity is the declared service identity the job runs under, and opsInspectReconcileJobType
	// the job type.
	opsInspectReconcileServiceIdentity = "actions.reconcile"
	opsInspectReconcileJobType         = "actions.reconcile"

	// opsInspectReconcileJobAttempts is how often the queue leases one job before it gives up: an application
	// that cannot be reached is retried, then asked again by the next period's job.
	opsInspectReconcileJobAttempts = 3
	// opsInspectReconcileBatch is the most jobs one pass queues.
	opsInspectReconcileBatch = 100

	// opsInspectReconcileListSQL lists the next uncertain preparations of every tenant, oldest
	// first: last changed at or before ?, after seq ?, at most ?. It repeats the condition of the
	// partial index action_preparations_uncertain literally, which is what lets SQLite use it.
	opsInspectReconcileListSQL = `SELECT seq, id, application_id, tenant_id, workspace_id, principal_id, session_id, scope_json
	   FROM action_preparations WHERE state = 'uncertain' AND updated_at <= ? AND seq > ? ORDER BY seq LIMIT ?`
)

// Test seams, replaced only by tests.
var (
	// opsInspectReconcileEvery is how often serve queues the jobs, and how long an outcome must have been
	// uncertain before it is asked about.
	opsInspectReconcileEvery = 5 * time.Minute
	// opsInspectReconcileOnCompose sees the job serve builds.
	opsInspectReconcileOnCompose func(*opsInspectReconcileStack)
	// opsInspectReconcileOnPass sees the result of every pass of the loop.
	opsInspectReconcileOnPass func(opsInspectReconcilePass, error)
)

func init() { registerServeProvider(opsInspectReconcileProviderName, opsInspectReconcileProvide) }

// opsInspectReconcilePass is what one pass of the loop did.
type opsInspectReconcilePass struct {
	// Examined is how many uncertain preparations were read, Enqueued how many jobs were queued,
	// Existing how many already had a job in this period and Skipped how many rows could not be
	// queued (a stored scope that is not valid, or an enqueue the queue refused).
	Examined, Enqueued, Existing, Skipped int
}

// opsInspectReconcileStack is the reconcile job of the process.
type opsInspectReconcileStack struct {
	db    *store.DB
	clk   clock.Clock
	log   *slog.Logger
	co    *actions.Coordinator
	queue *jobs.Queue
	every time.Duration

	mu     sync.Mutex
	cursor int64 // the seq the next pass reads after
}

// opsInspectReconcileProvide puts the job on the RAG stack's queue and pool and starts the loop that queues
// it. A serveDeps built by hand, with no coordinator, has nothing to reconcile.
func opsInspectReconcileProvide(_ context.Context, d serveDeps) error {
	if d.Actions == nil || d.DB == nil {
		return nil
	}
	rs, ok := gateway.Lookup[*ragStack](d.Services)
	if !ok {
		return contracts.New(contracts.ErrInternal, "cli: the reconcile job runs on the RAG stack's queue and pool, which were not provided")
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	r := &opsInspectReconcileStack{db: d.DB, clk: d.Clock, log: d.Log, co: d.Actions, queue: rs.queue, every: opsInspectReconcileEvery}
	if err := rs.queue.Register(jobs.TypeSpec{Name: opsInspectReconcileJobType, Class: jobs.ClassMaintenance, MaxAttempts: opsInspectReconcileJobAttempts}); err != nil {
		return err
	}
	if err := rs.shareJobs(opsInspectReconcileServiceIdentity, opsInspectReconcileAuthority{db: d.DB}, func(p *jobs.Pool) error {
		return p.Handle(opsInspectReconcileJobType, r.handle)
	}); err != nil {
		return err
	}
	if d.AddService != nil {
		d.AddService(r.start)
	}
	if opsInspectReconcileOnCompose != nil {
		opsInspectReconcileOnCompose(r)
	}
	return nil
}

// start is the service of the process (serveDeps.AddService): it runs a pass at once and after every
// period until ctx ends. The function it returns stops the loop and waits for it, before the store
// closes.
func (r *opsInspectReconcileStack) start(ctx context.Context) (stop func()) {
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(r.every)
		defer tick.Stop()
		for {
			r.runPass(rctx)
			select {
			case <-rctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// runPass is one pass of the loop. A failed pass is logged and the next one goes on.
func (r *opsInspectReconcileStack) runPass(ctx context.Context) {
	res, err := r.pass(ctx)
	if opsInspectReconcileOnPass != nil {
		opsInspectReconcileOnPass(res, err)
	}
	switch {
	case err != nil && ctx.Err() == nil:
		r.log.Warn("actions: queueing the reconciliation of uncertain outcomes failed; the next pass goes on",
			slog.String("code", string(contracts.CodeOf(err))), slog.String("err_text", err.Error()))
	case err == nil && res.Enqueued > 0:
		r.log.Info("actions: reconciliation of uncertain outcomes queued",
			slog.Int("queued", res.Enqueued), slog.Int("already_queued", res.Existing), slog.Int("skipped", res.Skipped))
	}
}

// pass queues one job for each uncertain preparation, oldest first, up to opsInspectReconcileBatch of them. It
// is a service operation across tenants, like Coordinator.Recover and ExpireDue (it reads the
// preparations of every tenant by nature and is never reachable from a request), and it reads the
// scope each preparation was made under, which is the scope its job runs under.
func (r *opsInspectReconcileStack) pass(ctx context.Context) (opsInspectReconcilePass, error) {
	var res opsInspectReconcilePass
	r.mu.Lock()
	after := r.cursor
	r.mu.Unlock()

	now := r.clk.Now()
	rows, err := r.db.QueryContext(ctx, opsInspectReconcileListSQL, now.Add(-r.every).UnixMilli(), after, opsInspectReconcileBatch)
	if err != nil {
		return res, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the uncertain outcomes cannot be listed")
	}
	type candidate struct {
		seq   int64
		id    string
		scope contracts.Scope
	}
	var found []candidate
	var last int64
	for rows.Next() {
		var c candidate
		var app, tenant, ws, principal, session, scopeJSON string
		if err := rows.Scan(&c.seq, &c.id, &app, &tenant, &ws, &principal, &session, &scopeJSON); err != nil {
			_ = rows.Close()
			return res, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the uncertain outcomes cannot be listed")
		}
		last = c.seq
		res.Examined++
		// The coordinator refuses a row whose columns differ from its stored scope: such a row is not
		// queued, it could not be reconciled.
		if err := json.Unmarshal([]byte(scopeJSON), &c.scope); err != nil || c.scope.Validate() != nil ||
			c.scope.ApplicationID != app || c.scope.TenantID != tenant || c.scope.WorkspaceID != ws ||
			c.scope.PrincipalID != principal || c.scope.SessionID != session {
			res.Skipped++
			r.log.Warn("actions: an uncertain preparation has no valid stored scope and is not reconciled", slog.String("preparation_id", c.id))
			continue
		}
		found = append(found, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return res, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the uncertain outcomes cannot be listed")
	}
	_ = rows.Close()

	// The cursor moves on past what was read; a short page means the end of the backlog, and the next
	// pass starts again from the oldest.
	r.mu.Lock()
	if res.Examined < opsInspectReconcileBatch {
		r.cursor = 0
	} else {
		r.cursor = last
	}
	r.mu.Unlock()

	period := now.Truncate(r.every).UnixMilli()
	for _, c := range found {
		payload, _ := json.Marshal(opsInspectReconcilePayload{PreparationID: c.id})
		out, err := r.queue.Enqueue(ctx, c.scope, jobs.EnqueueRequest{
			Type: opsInspectReconcileJobType, Payload: payload, ServiceIdentity: opsInspectReconcileServiceIdentity,
			IdempotencyKey: fmt.Sprintf("reconcile:%s:%d", c.id, period),
		})
		switch {
		case err == nil && out.Duplicate:
			res.Existing++
		case err == nil:
			res.Enqueued++
		case contracts.CodeOf(err) == contracts.ErrResourceExhausted:
			// The class is full: the rest is queued by a later pass.
			return res, err
		case ctx.Err() != nil:
			return res, ctx.Err()
		default:
			res.Skipped++
			r.log.Warn("actions: the reconciliation of an uncertain preparation could not be queued",
				slog.String("preparation_id", c.id), slog.String("code", string(contracts.CodeOf(err))))
		}
	}
	return res, nil
}

// opsInspectReconcilePayload is the payload of the job, and all of it.
type opsInspectReconcilePayload struct {
	PreparationID string `json:"preparation_id"`
}

func opsInspectReconcileDecode(raw json.RawMessage) (opsInspectReconcilePayload, error) {
	var p opsInspectReconcilePayload
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil || p.PreparationID == "" || dec.More() {
		return p, contracts.New(contracts.ErrPermissionDenied, "the reconcile job names no preparation")
	}
	return p, nil
}

// handle is the handler of the job: ask the application again, through the coordinator, and say what
// became of the preparation. It returns nil whenever the preparation is no longer uncertain (the
// coordinator recorded the outcome, or the application proved nothing was applied and the preparation
// was reopened, or someone else resolved it first) and whenever the application still cannot tell
// (the next period asks again). A failure that left it uncertain for another reason (the application
// cannot be reached, the store is busy) is returned, and the pool retries the job a few times.
//
// What happened is read from the preparation's state and not from the error's code: not applied
// comes back as unavailable (retryable, and wrong to retry), a recorded partial outcome comes back
// as an error beside its receipt, and a second reconciliation of a resolved preparation is a
// conflict.
func (r *opsInspectReconcileStack) handle(ctx context.Context, job jobs.Job) error {
	if job.ServiceIdentity != opsInspectReconcileServiceIdentity {
		return jobs.Permanent(contracts.New(contracts.ErrPermissionDenied, "the reconcile job needs its service identity"))
	}
	p, err := opsInspectReconcileDecode(job.Payload)
	if err != nil {
		return jobs.Permanent(err)
	}
	rc, rerr := r.co.Reconcile(ctx, job.Scope, p.PreparationID)
	state, serr := r.stateOf(ctx, job.Scope, p.PreparationID)
	switch {
	case serr != nil && rerr != nil:
		return rerr
	case serr != nil:
		return serr
	case state != string(act.PreparedActionStateUncertain):
		r.log.Info("actions: an uncertain outcome was reconciled", slog.String("preparation_id", p.PreparationID),
			slog.String("state", state), slog.String("receipt_status", string(rc.Status)))
		return nil
	case rerr == nil:
		return nil
	case contracts.CodeOf(rerr) == contracts.ErrUncertainOutcome:
		r.log.Info("actions: an uncertain outcome is still undecided; it is asked about again next period", slog.String("preparation_id", p.PreparationID))
		return nil
	}
	return rerr
}

// stateOf is the state of a preparation of the scope.
func (r *opsInspectReconcileStack) stateOf(ctx context.Context, scope contracts.Scope, id string) (string, error) {
	row, err := store.QueryRowScoped(ctx, r.db, scope, store.ScopedStmt{
		SQL: "SELECT state FROM action_preparations", Where: "id = ?", WhereArgs: []any{id},
	})
	if err != nil {
		return "", err
	}
	var state string
	if err := row.Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", contracts.New(contracts.ErrScopeViolation, "the preparation is not in the job's scope")
		}
		return "", contracts.Wrap(contracts.ErrUnavailable, err, "actions: the preparation cannot be read")
	}
	return state, nil
}

// opsInspectReconcileAuthority is the pool's authority for opsInspectReconcileServiceIdentity: that identity, the
// reconcile job type, a payload that names one preparation and nothing else, and a scope equal to
// the one stored for that preparation. A job that carries the identity but another payload or scope
// is denied, so a job queued for one preparation can never reconcile, or read the state of, another.
type opsInspectReconcileAuthority struct{ db *store.DB }

// AuthorizeService implements jobs.ServiceAuthorizer.
func (a opsInspectReconcileAuthority) AuthorizeService(ctx context.Context, identity string, job jobs.Job) error {
	switch {
	case identity != opsInspectReconcileServiceIdentity:
		return contracts.New(contracts.ErrPermissionDenied, "unknown service identity")
	case job.Type != opsInspectReconcileJobType:
		return contracts.New(contracts.ErrPermissionDenied, "the service identity does not run this job type")
	}
	if err := job.Scope.Validate(); err != nil {
		return contracts.Wrap(contracts.ErrPermissionDenied, err, "the reconcile job has no valid scope")
	}
	p, err := opsInspectReconcileDecode(job.Payload)
	if err != nil {
		return err
	}
	var scopeJSON string
	err = a.db.QueryRowContext(ctx,
		`SELECT scope_json FROM action_preparations WHERE id = ? AND application_id = ? AND tenant_id = ? AND workspace_id = ?`,
		p.PreparationID, job.Scope.ApplicationID, job.Scope.TenantID, job.Scope.WorkspaceID).Scan(&scopeJSON)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return contracts.New(contracts.ErrPermissionDenied, "the preparation is not in the job's scope")
	case err != nil:
		// Could not decide: the pool retries the attempt.
		return contracts.Wrap(contracts.ErrUnavailable, err, "actions: the preparation cannot be read")
	}
	var stored contracts.Scope
	if err := json.Unmarshal([]byte(scopeJSON), &stored); err != nil || !opsInspectReconcileSameScope(stored, job.Scope) {
		return contracts.New(contracts.ErrPermissionDenied, "the reconcile job's scope is not the scope the preparation was made under")
	}
	return nil
}

func opsInspectReconcileSameScope(a, b contracts.Scope) bool {
	return a.DeploymentID == b.DeploymentID && a.ApplicationID == b.ApplicationID && a.TenantID == b.TenantID &&
		a.WorkspaceID == b.WorkspaceID && a.PrincipalID == b.PrincipalID && a.SessionID == b.SessionID &&
		slices.Equal(a.Roles, b.Roles) && slices.Equal(a.CapabilityScopes, b.CapabilityScopes)
}
