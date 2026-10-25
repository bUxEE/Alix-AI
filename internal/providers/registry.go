package providers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// DefaultProbeTimeout bounds one provider health probe in Refresh.
const DefaultProbeTimeout = 3 * time.Second

// Registry maps each Role to the provider serving it and records per-role
// health. It is safe for concurrent use.
//
// Callers must handle a missing role: every optional role (router, embedder,
// reranker, voice) can be absent, and the controller can be down. Has reports
// whether a role is configured; Usable also requires it not to be
// unavailable; Snapshot and Degraded describe what the service can no longer
// do and how it must behave instead (master plan section 18).
type Registry struct {
	clk          clock.Clock
	probeTimeout time.Duration

	mu    sync.RWMutex
	roles map[Role]*entry
}

type entry struct {
	provider any
	health   HealthReport
	since    time.Time
}

// RegistryOption configures NewRegistry.
type RegistryOption func(*Registry)

// WithProbeTimeout sets the per-provider timeout used by Refresh.
func WithProbeTimeout(d time.Duration) RegistryOption {
	return func(r *Registry) {
		if d > 0 {
			r.probeTimeout = d
		}
	}
}

// NewRegistry returns an empty registry. A nil clock uses the wall clock.
func NewRegistry(clk clock.Clock, opts ...RegistryOption) *Registry {
	if clk == nil {
		clk = clock.Real{}
	}
	r := &Registry{clk: clk, probeTimeout: DefaultProbeTimeout, roles: make(map[Role]*entry)}
	for _, o := range opts {
		o(r)
	}
	return r
}

// implements reports whether p satisfies the interface the role needs. Voice
// roles are reserved: their interfaces arrive with M4 in
// internal/providers/voice, so any non-nil value is accepted and typed access
// goes through Lookup.
func implements(role Role, p any) bool {
	switch role {
	case RoleController, RoleRouter:
		_, ok := p.(TextGenerator)
		return ok
	case RoleEmbedder:
		_, ok := p.(Embedder)
		return ok
	case RoleReranker:
		_, ok := p.(Reranker)
		return ok
	}
	return role.Reserved()
}

// Register binds p to role. A new registration starts as ok. It fails for an
// unknown role, a nil provider, a provider that does not implement the role's
// interface, and a role that is already registered (conflict).
func (r *Registry) Register(role Role, p any) error {
	if !role.Valid() {
		return contracts.Wrap(contracts.ErrValidationFailed, unknownRoleError(role), "providers: register")
	}
	if p == nil {
		return contracts.Newf(contracts.ErrValidationFailed, "providers: nil provider for role %s", role)
	}
	if !implements(role, p) {
		return contracts.Newf(contracts.ErrValidationFailed, "providers: %T does not implement the %s role", p, role)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.roles[role]; dup {
		return contracts.Newf(contracts.ErrConflict, "providers: role %s is already registered", role)
	}
	r.roles[role] = &entry{provider: p, health: HealthReport{Status: HealthOK}, since: r.clk.Now()}
	return nil
}

// Has reports whether a provider is registered for role. It says nothing
// about health.
func (r *Registry) Has(role Role) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.roles[role]
	return ok
}

// Usable reports whether role is registered and not unavailable (degraded
// still counts: it can serve, with reduced quality).
func (r *Registry) Usable(role Role) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.roles[role]
	return ok && e.health.Status != HealthUnavailable
}

// Provider returns the value registered for role.
func (r *Registry) Provider(role Role) (any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.roles[role]
	if !ok {
		return nil, false
	}
	return e.provider, true
}

func missingRole(role Role) error {
	return contracts.Newf(contracts.ErrUnavailable, "providers: no provider registered for role %s", role).
		WithDetails("role", string(role)).
		WithDetails("effect", role.Effect())
}

// Lookup returns the provider registered for role as T. It fails with
// unavailable when the role is not registered (the caller then follows the
// section 18 behaviour for that role) and with internal when the registered
// value is not a T. It does not consult health: a role marked unavailable may
// have recovered, and the call itself reports a real failure. Check Usable
// first to skip work that needs a role known to be down.
func Lookup[T any](r *Registry, role Role) (T, error) {
	var zero T
	p, ok := r.Provider(role)
	if !ok {
		return zero, missingRole(role)
	}
	t, ok := p.(T)
	if !ok {
		return zero, contracts.Newf(contracts.ErrInternal, "providers: role %s holds %T, not the requested type", role, p)
	}
	return t, nil
}

// TextGenerator returns the provider for a text role (controller or router).
func (r *Registry) TextGenerator(role Role) (TextGenerator, error) {
	if !role.IsText() {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "providers: role %s is not a text role", role)
	}
	return Lookup[TextGenerator](r, role)
}

// Embedder returns the embedding provider.
func (r *Registry) Embedder() (Embedder, error) { return Lookup[Embedder](r, RoleEmbedder) }

// Reranker returns the reranking provider.
func (r *Registry) Reranker() (Reranker, error) { return Lookup[Reranker](r, RoleReranker) }

// SetHealth records a role's health. It fails for an unregistered role (a
// missing role is always unavailable; there is nothing to record) and for an
// invalid status. Since changes only when the status changes.
func (r *Registry) SetHealth(role Role, status Health, reason string) error {
	if !status.Valid() {
		return contracts.Newf(contracts.ErrValidationFailed, "providers: invalid health %q", string(status))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.roles[role]
	if !ok {
		return missingRole(role)
	}
	r.setLocked(e, HealthReport{Status: status, Reason: reason, Version: e.health.Version})
	return nil
}

func (r *Registry) setLocked(e *entry, h HealthReport) {
	if e.health.Status != h.Status {
		e.since = r.clk.Now()
	}
	e.health = h
}

// Refresh probes every registered provider that implements AIRuntimeProvider
// and records the outcome. A failed probe, a probe timeout or an invalid
// status is unavailable. Providers are probed outside the registry lock.
func (r *Registry) Refresh(ctx context.Context) {
	type target struct {
		role Role
		p    AIRuntimeProvider
	}
	var targets []target
	r.mu.RLock()
	for _, role := range Roles() {
		if e, ok := r.roles[role]; ok {
			if p, ok := e.provider.(AIRuntimeProvider); ok {
				targets = append(targets, target{role, p})
			}
		}
	}
	r.mu.RUnlock()

	for _, t := range targets {
		pctx, cancel := context.WithTimeout(ctx, r.probeTimeout)
		rep, err := t.p.Health(pctx)
		cancel()
		switch {
		case err != nil:
			rep = HealthReport{Status: HealthUnavailable, Reason: "health probe failed: " + shortError(err)}
		case !rep.Status.Valid():
			rep = HealthReport{Status: HealthUnavailable, Reason: "health probe returned an invalid status"}
		}
		r.mu.Lock()
		if e, ok := r.roles[t.role]; ok {
			r.setLocked(e, rep)
		}
		r.mu.Unlock()
	}
}

func shortError(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}

// RoleStatus describes one role.
type RoleStatus struct {
	Role Role `json:"role"`
	// Configured is whether a provider is registered.
	Configured bool `json:"configured"`
	// Provider is the registered runtime's name when it exposes one.
	Provider string `json:"provider,omitempty"`
	// Status is ok, degraded or unavailable. An unconfigured role is
	// unavailable.
	Status  Health `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Version string `json:"version,omitempty"`
	// Since is when Status last changed (zero for an unconfigured role).
	Since time.Time `json:"since"`
	// Effect and Behavior are set when Status is not ok: the stable
	// degradation code and the section 18 behaviour for the role.
	Effect   string `json:"effect,omitempty"`
	Behavior string `json:"behavior,omitempty"`
}

// Status returns the status of one role.
func (r *Registry) Status(role Role) RoleStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.statusLocked(role)
}

func (r *Registry) statusLocked(role Role) RoleStatus {
	e, ok := r.roles[role]
	if !ok {
		reason := "not configured"
		return RoleStatus{Role: role, Status: HealthUnavailable, Reason: reason, Effect: role.Effect(), Behavior: role.Behavior()}
	}
	s := RoleStatus{
		Role: role, Configured: true, Status: e.health.Status,
		Reason: e.health.Reason, Version: e.health.Version, Since: e.since,
	}
	if n, ok := e.provider.(interface{ Name() string }); ok {
		s.Provider = n.Name()
	}
	if s.Status != HealthOK {
		s.Effect, s.Behavior = role.Effect(), role.Behavior()
	}
	return s
}

// Snapshot returns the status of every known role in canonical order, for
// readiness endpoints and diagnostics. The result is a copy.
func (r *Registry) Snapshot() []RoleStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]RoleStatus, 0, len(roleTable))
	for _, role := range Roles() {
		out = append(out, r.statusLocked(role))
	}
	return out
}

// Degraded returns the roles the service must work around: configured roles
// that are not ok, plus any required role (the controller) that is missing.
// An optional role that was never configured is a choice, not a degradation,
// and is not listed; use Has to decide what to offer.
func (r *Registry) Degraded() []RoleStatus {
	var out []RoleStatus
	for _, s := range r.Snapshot() {
		if s.Status == HealthOK {
			continue
		}
		if s.Configured || s.Role.Required() {
			out = append(out, s)
		}
	}
	return out
}

// Overall summarises the registry: unavailable when the controller is missing
// or unavailable (the service cannot answer), degraded when any other
// configured role is not ok or the controller is degraded, ok otherwise.
func (r *Registry) Overall() Health {
	overall := HealthOK
	for _, s := range r.Snapshot() {
		switch {
		case s.Role.Required() && s.Status == HealthUnavailable:
			return HealthUnavailable
		case s.Status != HealthOK && s.Configured:
			overall = HealthDegraded
		}
	}
	return overall
}

// String lists the configured roles, for logs.
func (r *Registry) String() string {
	out := ""
	for _, s := range r.Snapshot() {
		if !s.Configured {
			continue
		}
		if out != "" {
			out += " "
		}
		out += fmt.Sprintf("%s=%s", s.Role, s.Status)
	}
	return "providers.Registry{" + out + "}"
}
