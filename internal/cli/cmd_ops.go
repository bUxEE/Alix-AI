package cli

import (
	"context"
	"log/slog"
	"sync"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/diag"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/jobs"
	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/metrics"
	"github.com/buxee/alix-ai/internal/resources"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The operations surface of `alixai serve` (master plan 14.2 "Operations", 16.2; M5-05, M3-30).
//
// internal/diag is the service; this file is where it is fed and where its two doors are opened.
//
//   - Host door: gateway.OpsService is provided, so /v1/ops/queue, /v1/ops/providers and
//     /v1/ops/audit/export answer (they answer 503 while none is provided). A host reaches only its
//     own scope's queue counts, the model roles and its own audit events, each behind an exact
//     capability in its context token.
//   - Operator door: with metrics.enabled, a loopback-only listener serves
//     diag.OperatorHandler: Prometheus text at /metrics and the redacted diagnostics bundle at
//     /diagnostics behind the operator credential, the same one the admin interface uses
//     (serveAdminStart). Deployment-wide data (global queue counts, resource figures, the
//     configuration, recent errors) is on that door and never on /v1.
//
// diag does not import internal/resources (module map), so the governor's Stats are adapted to
// diag.ResourceUsage here. The recent warnings and errors come from a ring that wraps the logger's
// handler (opsWrapLogger), built before anything logs.
func init() {
	// Providers run in name order and "ops" runs before "rag", which builds the job queue: the
	// queue is therefore read lazily (opsQueue), when the first status is asked for.
	registerServeProvider("ops", opsServeProvide)
}

// opsWrapLogger returns log with its handler wrapped by a diag.ErrorRing, and the ring. The
// ring keeps the redacted warnings and errors for the diagnostics bundle; records still go to the
// original handler unchanged.
func opsWrapLogger(log *slog.Logger, redactKeys []string) (*slog.Logger, *diag.ErrorRing) {
	ring := diag.NewErrorRing(0, alog.NewRedactor(redactKeys))
	return slog.New(ring.Wrap(log.Handler())), ring
}

// opsResourceUsage adapts the governor's Stats to the plain numbers diag reports.
func opsResourceUsage(gov *resources.Governor) func() diag.ResourceUsage {
	return func() diag.ResourceUsage {
		st := gov.Stats()
		return diag.ResourceUsage{
			MemoryBudgetBytes: st.BudgetBytes,
			RSSBytes:          st.RSSBytes,
			RuntimeBytes:      st.RuntimeBytes,
			ReservedBytes:     st.ReservedBytes,
			InteractiveActive: st.InteractiveActive,
			BackgroundHeld:    st.BackgroundHeld,
			HoldReason:        string(st.HoldReason),
			Admitted:          st.Admitted,
			Held:              st.Held,
			Rejected:          st.Rejected,
			ModelUnloads:      st.Unloads,
			ModelReloads:      st.Reloads,
		}
	}
}

// opsQueue is diag.QueueSource over the queue the RAG stack builds later than this provider runs.
// Until then it answers unavailable, which diag reports as a problem of the bundle, never as an
// empty queue.
type opsQueue struct{ w *resourcesWiring }

func (q opsQueue) get() (*jobs.Queue, error) {
	if q.w != nil {
		if jq := q.w.jobQueue(); jq != nil {
			return jq, nil
		}
	}
	return nil, contracts.New(contracts.ErrUnavailable, "the job queue is not built")
}

func (q opsQueue) Status(ctx context.Context) (jobs.Status, error) {
	jq, err := q.get()
	if err != nil {
		return jobs.Status{}, err
	}
	return jq.Status(ctx)
}

func (q opsQueue) ScopedStatus(ctx context.Context, scope contracts.Scope) (jobs.Status, error) {
	jq, err := q.get()
	if err != nil {
		return jobs.Status{}, err
	}
	return jq.ScopedStatus(ctx, scope)
}

// opsConfigSnapshot is the configuration as the bundle shows it, by section. It holds only
// sections that carry no credential, and the bundle redacts it again (diag.sanitize).
func opsConfigSnapshot(d serveDeps) func() map[string]any {
	return func() map[string]any {
		lcfg := localaiSection
		lcfg.APIKeyRef = ""
		out := map[string]any{
			localaiSectionName:   lcfg,
			inferenceSectionName: inferenceSection,
			"jobs":               ragSourcesSectionsOf(d.Config).jobs,
			"metrics":            resourcesSectionsOf(d.Config).metrics,
			resourcesSectionName: resourcesSectionsOf(d.Config).gov,
		}
		if d.Resources != nil {
			// The classes the governor actually runs with, not the section's.
			out[resourcesSectionName] = d.Resources.Gov.Config()
		}
		return out
	}
}

// opsServeProvide builds diag.Service from what serve has and provides it twice: as
// gateway.OpsService for the /v1/ops area and as *diag.Service for the operator door that
// runServe opens (opsStartMetrics).
func opsServeProvide(_ context.Context, d serveDeps) error {
	reg := d.Config
	if reg == nil {
		reg = config.Default
	}
	src := diag.Sources{
		Clock:  d.Clock,
		Config: opsConfigSnapshot(d),
		Errors: d.ErrorRing,
		// The operator's log.redact_keys hide values in the bundle as they do in the log.
		RedactKeys: reg.Core().Log.RedactKeys,
	}
	if d.Providers != nil {
		src.Providers = d.Providers
	}
	if d.Audit != nil {
		src.Audit = d.Audit
	}
	if d.Resources != nil {
		src.Queue = opsQueue{w: d.Resources}
		src.Resources = opsResourceUsage(d.Resources.Gov)
	}
	svc, err := diag.New(src)
	if err != nil {
		return err
	}
	if err := gateway.Provide[gateway.OpsService](d.Services, svc); err != nil {
		return err
	}
	return gateway.Provide(d.Services, svc)
}

// opsStartMetrics opens the operator door when the metrics section enables it: a listener that
// is loopback or refused, serving diag.OperatorHandler with the admin credential. It returns the
// function that stops the listener and waits for it. With metrics off it opens nothing.
func opsStartMetrics(ctx context.Context, log *slog.Logger, cfg metrics.Config, services *gateway.Services, token string) (stop func(), err error) {
	if !cfg.Enabled {
		return func() {}, nil
	}
	svc, ok := gateway.Lookup[*diag.Service](services)
	if !ok {
		return nil, contracts.New(contracts.ErrInternal, "metrics are enabled but the operations service is not provided")
	}
	h, err := diag.OperatorHandler(svc, token)
	if err != nil {
		return nil, err
	}
	ln, err := metrics.Listen(cfg.Listen)
	if err != nil {
		return nil, err
	}
	denyRelease := lastWireDenyListener(ln) // PT-001: a connector can never be aimed at the metrics listener, even on port 0
	log.Info("metrics listener", slog.String("addr", ln.Addr().String()))
	mctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := metrics.Serve(mctx, ln, h); err != nil {
			log.Error("the metrics listener stopped", slog.String("err_text", err.Error()))
		}
	}()
	return func() {
		cancel()
		wg.Wait()
		denyRelease()
	}, nil
}
