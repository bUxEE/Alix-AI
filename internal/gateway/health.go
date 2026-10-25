package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Check is one readiness condition beyond the runtime providers: the store
// answers, the model preflight passed. Fn must be quick and must return an
// error whose text is safe to show to an anonymous caller; /readyz reports only
// the check's name, whether it passed and, if not, that text.
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

// checkTimeout bounds one Check.
const checkTimeout = 2 * time.Second

// readiness caches the result of probing the providers and the checks, so a
// flood of unauthenticated /readyz requests costs one probe per TTL and not one
// per request. Concurrent callers wait for the probe that is already running.
type readiness struct {
	clk    clock.Clock
	ttl    time.Duration
	reg    *providers.Registry
	checks []Check

	mu     sync.Mutex
	last   time.Time
	probed bool
	report ReadyReport
}

// RoleReport is one provider role in /readyz. The runtime version and the
// provider name are left out: an anonymous caller gets no inventory.
type RoleReport struct {
	Role       providers.Role   `json:"role"`
	Configured bool             `json:"configured"`
	Status     providers.Health `json:"status"`
	Reason     string           `json:"reason,omitempty"`
	// Effect is the stable degradation code when the role is not ok.
	Effect string `json:"effect,omitempty"`
}

// CheckReport is one Check in /readyz.
type CheckReport struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// ReadyReport is the body of /readyz.
type ReadyReport struct {
	// Status is ok, degraded or unavailable. Ready is true unless it is
	// unavailable.
	Status providers.Health `json:"status"`
	Ready  bool             `json:"ready"`
	Roles  []RoleReport     `json:"roles"`
	Checks []CheckReport    `json:"checks"`
}

func newReadiness(clk clock.Clock, ttl time.Duration, reg *providers.Registry, checks []Check) *readiness {
	return &readiness{clk: clk, ttl: ttl, reg: reg, checks: checks}
}

// Report returns the cached report, probing first when it is older than the
// TTL (or has never been taken).
func (h *readiness) Report(ctx context.Context) ReadyReport {
	// The probe outlives the request that happens to trigger it: one caller
	// hanging up must not leave a failed probe in the cache for everyone.
	ctx = context.WithoutCancel(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.probed && h.clk.Now().Sub(h.last) < h.ttl {
		return h.report
	}
	h.reg.Refresh(ctx)

	rep := ReadyReport{Status: h.reg.Overall(), Roles: []RoleReport{}, Checks: []CheckReport{}}
	for _, s := range h.reg.Snapshot() {
		if !s.Configured && !s.Role.Required() {
			continue // an optional role nobody configured is a choice, not a state
		}
		rep.Roles = append(rep.Roles, RoleReport{
			Role: s.Role, Configured: s.Configured, Status: s.Status,
			Reason: alog.RedactString(s.Reason), Effect: s.Effect,
		})
	}
	for _, c := range h.checks {
		cctx, cancel := context.WithTimeout(ctx, checkTimeout)
		err := c.Fn(cctx)
		cancel()
		cr := CheckReport{Name: c.Name, OK: err == nil}
		if err != nil {
			rep.Status = providers.HealthUnavailable
			cr.Reason = checkReason(err)
		}
		rep.Checks = append(rep.Checks, cr)
	}
	rep.Ready = rep.Status != providers.HealthUnavailable

	h.report, h.last, h.probed = rep, h.clk.Now(), true
	return rep
}

// checkReason is what an anonymous caller may read about a failed check: the
// typed message, redacted, never the wrapped cause.
func checkReason(err error) string {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil && ce.Message != "" {
		return truncate(alog.RedactString(ce.Message), 200)
	}
	return "check failed"
}
