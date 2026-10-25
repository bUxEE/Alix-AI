package lifecycle

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/buxee/alix-ai/internal/decision"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// The metric names. Counters end in _total; the histogram is in milliseconds.
const (
	MetricLatencyMS         = "decision_latency_ms"
	MetricRequests          = "decision_requests_total"
	MetricAccepted          = "decision_accepted_total"
	MetricFallback          = "decision_fallback_total"
	MetricControllerAvoided = "decision_controller_calls_avoided_total"
	MetricCacheHits         = "decision_cache_hits_total"
	MetricCacheMisses       = "decision_cache_misses_total"
	MetricLoads             = "decision_loads_total"
	MetricLoadFailures      = "decision_load_failures_total"
	MetricRestarts          = "decision_restarts_total"
	MetricEvictions         = "decision_evictions_total"
	MetricRollbacks         = "decision_rollbacks_total"
	MetricResidentBytes     = "decision_resident_bytes"
)

// LatencyBucketsMS are the upper bounds of the latency histogram buckets.
var LatencyBucketsMS = []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000}

// Labels is the whole label set a metric can carry: provider, model, revision and
// profile, plus a Reason that holds a fixed enum string (a typed fallback reason, an
// unload or rollback reason). There is deliberately no field for a tenant, an
// application, a principal or a session, so a series cannot identify one.
type Labels struct {
	Provider string
	Model    string
	Revision string
	Profile  string
	Reason   string
}

// Registry is the small recording interface the lifecycle writes to. It only RECORDS:
// there is no method that sends, flushes or exports, and nothing in this package opens
// a connection. An operator who wants the numbers elsewhere reads them from the
// implementation (MemoryRegistry.Counter and friends) and exports them explicitly.
type Registry interface {
	// Add adds delta to a counter.
	Add(name string, labels Labels, delta int64)
	// Set sets a gauge.
	Set(name string, labels Labels, value float64)
	// Observe records one value in a histogram.
	Observe(name string, labels Labels, value float64)
}

// Discard is a Registry that records nothing.
type Discard struct{}

// Add implements Registry.
func (Discard) Add(string, Labels, int64) {}

// Set implements Registry.
func (Discard) Set(string, Labels, float64) {}

// Observe implements Registry.
func (Discard) Observe(string, Labels, float64) {}

// Metrics records decision-layer observations through a Registry. The supervisor uses
// it for lifecycle events; the dialogue integration calls ObserveAdvice for every
// Engine.Score outcome and ControllerCallAvoided when an accepted advice let it skip
// the controller.
type Metrics struct {
	reg  Registry
	base Labels
}

// NewMetrics returns a recorder. A nil reg records nothing. base carries the profile
// (and provider, model and revision until a result names its own).
func NewMetrics(reg Registry, base Labels) *Metrics {
	if reg == nil {
		reg = Discard{}
	}
	base.Reason = ""
	return &Metrics{reg: reg, base: base}
}

// ObserveAdvice records one scoring outcome: a request, its latency, accepted or the
// typed fallback reason, and the cache outcome. A request that never reached scoring
// (the layer is disabled, or the candidate set was rejected) records no latency.
func (m *Metrics) ObserveAdvice(adv decision.Advice) {
	res := adv.Result()
	l := m.base
	if res.ProviderID != "" {
		l.Provider = string(res.ProviderID)
	}
	if res.ModelID != nil {
		l.Model = string(*res.ModelID)
	}
	if res.ModelRevision != nil {
		l.Revision = string(*res.ModelRevision)
	}
	m.reg.Add(MetricRequests, l, 1)

	reason, fell := adv.FallbackReason()
	scored := !fell || (reason != gen.FallbackReasonDisabled && reason != gen.FallbackReasonCandidateSetRejected)
	if scored {
		m.reg.Observe(MetricLatencyMS, l, float64(res.LatencyMS))
	}
	if fell {
		lr := l
		lr.Reason = string(reason)
		m.reg.Add(MetricFallback, lr, 1)
	} else {
		m.reg.Add(MetricAccepted, l, 1)
	}
	switch res.CacheStatus {
	case gen.DecisionScoreResultCacheStatusHit:
		m.reg.Add(MetricCacheHits, l, 1)
	case gen.DecisionScoreResultCacheStatusMiss:
		m.reg.Add(MetricCacheMisses, l, 1)
	}
}

// ControllerCallAvoided counts one conversational-controller call the layer saved.
func (m *Metrics) ControllerCallAvoided() {
	m.reg.Add(MetricControllerAvoided, m.base, 1)
}

func (m *Metrics) event(name, reason string, rev Revision) {
	l := m.labelsFor(rev)
	l.Reason = reason
	m.reg.Add(name, l, 1)
}

func (m *Metrics) memory(rev Revision, bytes uint64) {
	m.reg.Set(MetricResidentBytes, m.labelsFor(rev), float64(bytes))
}

func (m *Metrics) labelsFor(rev Revision) Labels {
	l := m.base
	l.Provider = string(rev.Manifest.ProviderID)
	l.Model = string(rev.Manifest.ModelID)
	l.Revision = string(rev.Manifest.ModelRevision)
	return l
}

// ---- in-memory registry ----------------------------------------------------

// MemoryRegistry is a Registry that keeps everything in process memory. It is pull
// only: callers read it, it never sends anything. It is safe for concurrent use.
type MemoryRegistry struct {
	mu       sync.Mutex
	counters map[string]int64
	gauges   map[string]float64
	hists    map[string]*histogram
}

// NewMemoryRegistry returns an empty registry.
func NewMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{counters: map[string]int64{}, gauges: map[string]float64{}, hists: map[string]*histogram{}}
}

type histogram struct {
	counts []int64 // one per bucket, plus a last overflow slot
	sum    float64
	n      int64
}

func seriesKey(name string, l Labels) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s", name, l.Provider, l.Model, l.Revision, l.Profile, l.Reason)
}

// Add implements Registry.
func (r *MemoryRegistry) Add(name string, l Labels, delta int64) {
	r.mu.Lock()
	r.counters[seriesKey(name, l)] += delta
	r.mu.Unlock()
}

// Set implements Registry.
func (r *MemoryRegistry) Set(name string, l Labels, v float64) {
	r.mu.Lock()
	r.gauges[seriesKey(name, l)] = v
	r.mu.Unlock()
}

// Observe implements Registry. Negative and NaN values are dropped.
func (r *MemoryRegistry) Observe(name string, l Labels, v float64) {
	if math.IsNaN(v) || v < 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := seriesKey(name, l)
	h := r.hists[k]
	if h == nil {
		h = &histogram{counts: make([]int64, len(LatencyBucketsMS)+1)}
		r.hists[k] = h
	}
	i := sort.SearchFloat64s(LatencyBucketsMS, v)
	h.counts[i]++
	h.sum += v
	h.n++
}

// Counter returns one counter series.
func (r *MemoryRegistry) Counter(name string, l Labels) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counters[seriesKey(name, l)]
}

// CounterTotal returns the sum of a counter over all its label sets.
func (r *MemoryRegistry) CounterTotal(name string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	prefix := name + "|"
	for k, v := range r.counters {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			n += v
		}
	}
	return n
}

// CounterByReason returns a counter summed over every label set that has the given
// reason (for example MetricFallback and "timeout").
func (r *MemoryRegistry) CounterByReason(name, reason string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	prefix, suffix := name+"|", "|"+reason
	for k, v := range r.counters {
		if strings.HasPrefix(k, prefix) && strings.HasSuffix(k, suffix) {
			n += v
		}
	}
	return n
}

// Gauge returns one gauge series.
func (r *MemoryRegistry) Gauge(name string, l Labels) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gauges[seriesKey(name, l)]
}

// HistogramSnapshot is a read of one histogram. The quantiles are the upper bound of
// the bucket that holds the rank (the last bound, when the rank is in the overflow).
type HistogramSnapshot struct {
	Count         int64
	Sum           float64
	P50, P95, P99 float64
}

// Histogram returns the histogram summed over all label sets of name.
func (r *MemoryRegistry) Histogram(name string) HistogramSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := make([]int64, len(LatencyBucketsMS)+1)
	var snap HistogramSnapshot
	prefix := name + "|"
	for k, h := range r.hists {
		if len(k) < len(prefix) || k[:len(prefix)] != prefix {
			continue
		}
		for i, c := range h.counts {
			counts[i] += c
		}
		snap.Count += h.n
		snap.Sum += h.sum
	}
	snap.P50 = quantile(counts, snap.Count, 0.50)
	snap.P95 = quantile(counts, snap.Count, 0.95)
	snap.P99 = quantile(counts, snap.Count, 0.99)
	return snap
}

func quantile(counts []int64, n int64, q float64) float64 {
	if n == 0 {
		return 0
	}
	rank := int64(math.Ceil(q * float64(n)))
	var cum int64
	for i, c := range counts {
		cum += c
		if cum >= rank {
			if i >= len(LatencyBucketsMS) {
				return LatencyBucketsMS[len(LatencyBucketsMS)-1]
			}
			return LatencyBucketsMS[i]
		}
	}
	return LatencyBucketsMS[len(LatencyBucketsMS)-1]
}

// AcceptanceRate is accepted / requests over every label set; ok is false before the
// first request.
func (r *MemoryRegistry) AcceptanceRate() (rate float64, ok bool) {
	return ratio(r.CounterTotal(MetricAccepted), r.CounterTotal(MetricRequests))
}

// CacheHitRate is hits / (hits + misses); ok is false before the first cache lookup.
func (r *MemoryRegistry) CacheHitRate() (rate float64, ok bool) {
	h := r.CounterTotal(MetricCacheHits)
	return ratio(h, h+r.CounterTotal(MetricCacheMisses))
}

func ratio(num, den int64) (float64, bool) {
	if den <= 0 {
		return 0, false
	}
	return float64(num) / float64(den), true
}
