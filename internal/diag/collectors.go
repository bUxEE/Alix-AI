package diag

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/internal/metrics"
	"github.com/buxee/alix-ai/internal/providers"
)

// The label values below come from closed sets (the six priority classes, the
// five job states, the model roles and three health values), never from a
// tenant, a principal or a request, so a scrape reveals no one's data and the
// number of series is fixed.

// queueCollector exports the deployment-wide queue: jobs per class and state,
// the configured limits and how long the oldest ready job has waited (a large
// value on a class with free workers means starvation).
func queueCollector(src QueueSource) metrics.Collector {
	return metrics.CollectorFunc("queue", func(ctx context.Context) ([]metrics.Family, error) {
		st, err := src.Status(ctx)
		if err != nil {
			return nil, err
		}
		jobsF := metrics.Family{Name: "alixai_queue_jobs", Type: metrics.Gauge,
			Help: "Jobs per priority class and state; finished states count the jobs still retained."}
		workers := metrics.Family{Name: "alixai_queue_class_workers", Type: metrics.Gauge, Help: "Configured workers per priority class."}
		depth := metrics.Family{Name: "alixai_queue_class_max_depth", Type: metrics.Gauge, Help: "Configured maximum pending depth per priority class."}
		age := metrics.Family{Name: "alixai_queue_oldest_ready_age_seconds", Type: metrics.Gauge,
			Help: "How long the longest-waiting ready job of the class has been eligible to run."}
		for _, c := range st.Classes {
			cl := metrics.Label{Name: "class", Value: c.Name}
			for _, s := range []struct {
				state string
				n     int
			}{{"pending", c.Pending}, {"running", c.Running}, {"succeeded", c.Succeeded}, {"failed", c.Failed}, {"cancelled", c.Cancelled}} {
				jobsF.Samples = append(jobsF.Samples, metrics.Sample{Labels: []metrics.Label{cl, {Name: "state", Value: s.state}}, Value: float64(s.n)})
			}
			workers.Samples = append(workers.Samples, metrics.Sample{Labels: []metrics.Label{cl}, Value: float64(c.Workers)})
			depth.Samples = append(depth.Samples, metrics.Sample{Labels: []metrics.Label{cl}, Value: float64(c.MaxDepth)})
			age.Samples = append(age.Samples, metrics.Sample{Labels: []metrics.Label{cl}, Value: (time.Duration(c.OldestReadyAgeMs) * time.Millisecond).Seconds()})
		}
		return []metrics.Family{jobsF, workers, depth, age}, nil
	})
}

// providerCollector exports the model roles: whether each is configured, its
// health (one series per health value, 1 for the current one) and which runtime
// and version serve it.
func providerCollector(src ProviderSource) metrics.Collector {
	return metrics.CollectorFunc("providers", func(context.Context) ([]metrics.Family, error) {
		configured := metrics.Family{Name: "alixai_provider_configured", Type: metrics.Gauge, Help: "1 when a provider is registered for the role."}
		status := metrics.Family{Name: "alixai_provider_status", Type: metrics.Gauge, Help: "1 for the current health of the role (ok, degraded or unavailable), 0 for the others."}
		info := metrics.Family{Name: "alixai_provider_info", Type: metrics.Gauge, Help: "The runtime and version that serve a configured role; the value is always 1."}
		for _, st := range src.Snapshot() {
			role := metrics.Label{Name: "role", Value: string(st.Role)}
			configured.Samples = append(configured.Samples, metrics.Sample{Labels: []metrics.Label{role}, Value: metrics.B2F(st.Configured)})
			for _, h := range []providers.Health{providers.HealthOK, providers.HealthDegraded, providers.HealthUnavailable} {
				status.Samples = append(status.Samples, metrics.Sample{
					Labels: []metrics.Label{role, {Name: "status", Value: string(h)}}, Value: metrics.B2F(st.Status == h)})
			}
			if st.Configured {
				info.Samples = append(info.Samples, metrics.Sample{
					Labels: []metrics.Label{role, {Name: "provider", Value: st.Provider}, {Name: "version", Value: st.Version}}, Value: 1})
			}
		}
		return []metrics.Family{configured, status, info}, nil
	})
}

// resourceCollector exports the resource governor's gauges and counters.
func resourceCollector(read func() ResourceUsage) metrics.Collector {
	return metrics.CollectorFunc("resources", func(context.Context) ([]metrics.Family, error) {
		u := read()
		one := func(name, help string, t metrics.Type, v float64) metrics.Family {
			return metrics.Family{Name: name, Help: help, Type: t, Samples: []metrics.Sample{{Value: v}}}
		}
		return []metrics.Family{
			one("alixai_resource_memory_budget_bytes", "Memory the governor may commit.", metrics.Gauge, float64(u.MemoryBudgetBytes)),
			one("alixai_resource_rss_bytes", "Resident memory of the service as last read.", metrics.Gauge, float64(u.RSSBytes)),
			one("alixai_resource_runtime_bytes", "Resident memory of the model runtime as last read.", metrics.Gauge, float64(u.RuntimeBytes)),
			one("alixai_resource_reserved_bytes", "Memory held by admitted work.", metrics.Gauge, float64(u.ReservedBytes)),
			one("alixai_resource_interactive_active", "Interactive requests in flight.", metrics.Gauge, float64(u.InteractiveActive)),
			one("alixai_resource_background_held", "1 while background work yields to interactive work or pressure.", metrics.Gauge, metrics.B2F(u.BackgroundHeld)),
			one("alixai_resource_admitted_total", "Requests the governor admitted.", metrics.Counter, float64(u.Admitted)),
			one("alixai_resource_held_total", "Requests the governor held.", metrics.Counter, float64(u.Held)),
			one("alixai_resource_rejected_total", "Requests the governor rejected.", metrics.Counter, float64(u.Rejected)),
			one("alixai_resource_model_unloads_total", "Idle models unloaded.", metrics.Counter, float64(u.ModelUnloads)),
			one("alixai_resource_model_reloads_total", "Models loaded again after the governor unloaded them (each cost a cold load).", metrics.Counter, float64(u.ModelReloads)),
		}, nil
	})
}
