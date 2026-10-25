package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/settings"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// History retention in `alixai serve` (master plan 19, M3-21).
//
// internal/history owns the one stored retention policy (per application and per tenant, the
// shorter of the two applies, no policy deletes nothing) and the job that applies it. This file
// is where the process runs it:
//
//   - the job runs on the RAG stack's one queue and one pool (cmd_rag.go, ragStack.shareJobs):
//     history.New gets the stack's queue and the deployment id, the pool accepts the identity
//     history.ServiceIdentity for the type history.retention only, and svc.Handle attaches the
//     handler. There is no second pool. The job is class 6, so the resource governor holds it
//     behind every live turn and every higher class;
//   - a loop schedules it at start and every history.Config.RetentionInterval (24 h by default).
//     History queues at most one job per workspace per period (its idempotency key names the
//     period), so a restart inside a period queues nothing a second time, and a workspace with
//     no policy or nothing past it gets no job;
//   - the retention preference of the settings API is stored in history and nowhere else
//     (historyRetentionSettings, below).
//
// The retention policy has no HTTP surface of its own: master plan 14.2 puts "configure history
// retention" under Settings.

// historyRetentionProviderName is the name the history serve provider is registered under.
// Providers run in name order and this one needs the RAG stack's queue and pool, which the "rag"
// provider builds, and is needed by the "settings" provider, so the name sorts between them.
const historyRetentionProviderName = "rag_history"

// Test seams, replaced only by tests.
var (
	// historyRetentionConfig is the history service's configuration; a zero field takes the default.
	historyRetentionConfig history.Config
	// historyRetentionOnCompose sees the history service serve builds.
	historyRetentionOnCompose func(*history.Service)
	// historyRetentionOnPass sees the result of every scheduling pass.
	historyRetentionOnPass func(history.ScheduleResult, error)
)

// historyRetentionEvery is how often serve schedules the retention job.
func historyRetentionEvery(c history.Config) time.Duration {
	if c.RetentionInterval > 0 {
		return c.RetentionInterval
	}
	return history.DefaultRetentionInterval
}

// historyRetentionAuthority is the pool's authority for history.ServiceIdentity: that identity,
// the retention job type, and a complete scope. The scope names the workspace the job works in;
// what is deleted is decided by the policy the handler re-reads when it runs. A declared
// identity alone grants nothing (jobs.ServiceAuthorizer).
type historyRetentionAuthority struct{}

// AuthorizeService implements jobs.ServiceAuthorizer.
func (historyRetentionAuthority) AuthorizeService(_ context.Context, identity string, job jobs.Job) error {
	switch {
	case identity != history.ServiceIdentity:
		return contracts.New(contracts.ErrPermissionDenied, "unknown service identity")
	case job.Type != history.JobRetention:
		return contracts.New(contracts.ErrPermissionDenied, "the service identity does not run this job type")
	}
	if err := job.Scope.Validate(); err != nil {
		return contracts.Wrap(contracts.ErrPermissionDenied, err, "the retention job has no valid scope")
	}
	return nil
}

// historyRetentionOperator is history's Operator hook: the application-wide retention policy is
// an operator action, and its only caller is the settings service, which has already required
// the caller's token to carry settings.deployment.write for it. The hook asks for the same
// capability again, so no other caller of history.Service can set it without that token.
func historyRetentionOperator(_ context.Context, scope contracts.Scope, action history.Action) error {
	if action == history.ActionApplicationRetention && scope.Permits(settings.CapabilityDeploymentWrite) {
		return nil
	}
	return contracts.New(contracts.ErrPermissionDenied, "the application retention policy is changed through the deployment settings")
}

// historyRetentionSettings is settings.RetentionStore over the history service: the one place the
// retention policy is stored. The settings service has authorised every call (the token's
// settings.tenant.write or settings.deployment.write for a write, settings.user.read for a
// read), so history is not asked for its own history.retention capability: this adapter adds it
// to the scope it passes, for the retention calls only. The deployment level maps to the
// application-wide policy, whose Operator hook is historyRetentionOperator, and the tenant level
// to the tenant's own policy, which history refuses to set above the application's.
type historyRetentionSettings struct{ svc *history.Service }

func (historyRetentionSettings) scoped(scope contracts.Scope) contracts.Scope {
	scope = scope.Clone()
	scope.CapabilityScopes = append(scope.CapabilityScopes, history.CapRetention)
	return scope
}

// Retention implements settings.RetentionStore.
func (a historyRetentionSettings) Retention(ctx context.Context, scope contracts.Scope) (settings.Retention, error) {
	p, err := a.svc.RetentionPolicy(ctx, a.scoped(scope))
	if err != nil {
		return settings.Retention{}, err
	}
	out := settings.Retention{EffectiveDays: p.EffectiveDays, Source: p.Source}
	if p.Application != nil {
		out.Application = p.Application.Days
	}
	if p.Tenant != nil {
		out.Tenant = p.Tenant.Days
	}
	return out, nil
}

// SetRetention implements settings.RetentionStore: the policy is written in the settings document's
// transaction, so the two commit together or not at all.
func (a historyRetentionSettings) SetRetention(ctx context.Context, tx *store.Tx, scope contracts.Scope, level settings.Level, days int) (err error) {
	switch level {
	case settings.LevelTenant:
		_, err = a.svc.SetRetentionTx(ctx, tx, a.scoped(scope), days)
	case settings.LevelDeployment:
		_, err = a.svc.SetApplicationRetentionTx(ctx, tx, scope, days)
	default:
		err = contracts.New(contracts.ErrValidationFailed, "history: only the tenant and deployment retention is stored by history")
	}
	return err
}

// historyRetentionShare puts the retention job on the RAG stack's pool and starts the loop that
// schedules it. The loop is a part of the process: it runs once everything is built and stops,
// and is waited for, before the store closes.
func historyRetentionShare(d serveDeps, st *ragStack, svc *history.Service, every time.Duration) error {
	if err := st.shareJobs(history.ServiceIdentity, historyRetentionAuthority{}, svc.Handle); err != nil {
		return err
	}
	if d.AddService != nil {
		d.AddService(func(ctx context.Context) func() { return historyRetentionRun(ctx, svc, every, d.Log) })
	}
	return nil
}

// historyRetentionRun schedules the retention job at once and every `every` until ctx ends. The
// returned function stops the loop and waits for it.
func historyRetentionRun(ctx context.Context, svc *history.Service, every time.Duration, log *slog.Logger) (stop func()) {
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			historyRetentionSchedule(rctx, svc, log)
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

// historyRetentionSchedule is one pass of the loop. A failed pass is logged and the next one
// goes on; a full queue stops a pass with resource_exhausted, which is the same.
func historyRetentionSchedule(ctx context.Context, svc *history.Service, log *slog.Logger) (history.ScheduleResult, error) {
	res, err := svc.ScheduleRetention(ctx)
	if historyRetentionOnPass != nil {
		historyRetentionOnPass(res, err)
	}
	switch {
	case err != nil && ctx.Err() == nil:
		log.Warn("history: scheduling the retention jobs failed; the next pass goes on",
			slog.String("code", string(contracts.CodeOf(err))), slog.String("err_text", err.Error()))
	case err == nil && res.Enqueued > 0:
		log.Info("history: retention jobs queued", slog.Int("workspaces", res.Workspaces), slog.Int("queued", res.Enqueued), slog.Int("already_queued", res.Existing))
	}
	return res, err
}
