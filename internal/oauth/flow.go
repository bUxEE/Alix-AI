package oauth

import (
	"context"
	"net/url"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Flow presentation and callback modes (1.1.0 popup contract).
const (
	PresentationRedirect = "redirect"
	PresentationPopup    = "popup"
	CallbackHost         = "host"
	CallbackLoopback     = "loopback"
	// LoopbackPlaceholder is the redirect_uri callers send when callback is loopback; the engine
	// replaces it with the ephemeral listener URI and accepts it only in that case.
	LoopbackPlaceholder = "http://127.0.0.1/oauth/loopback"
	DefaultPollInterval = 1500 * time.Millisecond
	flowIDPrefix        = "oaf"
)

// FlowStatus is the server-side status of one popup (or CLI) authorization.
type FlowStatus string

// The flow statuses.
const (
	FlowPending   FlowStatus = "pending"
	FlowCompleted FlowStatus = "completed"
	FlowFailed    FlowStatus = "failed"
	FlowExpired   FlowStatus = "expired"
	FlowCancelled FlowStatus = "cancelled"
)

// FlowRecord is what GET /v1/oauth/flows/{flow_id} returns (connection filled by the composition root).
type FlowRecord struct {
	FlowID    string
	Provider  providers.ProviderID
	Status    FlowStatus
	Reason    Reason
	UpdatedAt time.Time
	scope     contracts.Scope
	stateKey  string // hash of state, or loopback bind key when NoState
	expires   time.Time
	opener    string // validated opener_origin; empty for CLI / redirect
	mu        sync.Mutex
	conn      *Connection // set on completed; composition root maps to ConnectionStatus
}

// FlowView is the public view of a flow (no scope internals).
type FlowView struct {
	FlowID     string
	Provider   providers.ProviderID
	Status     FlowStatus
	Reason     Reason
	UpdatedAt  time.Time
	Connection *Connection
}

func (e *Engine) newFlowID() string { return ids.New(flowIDPrefix) }

// registerFlow stores a pending flow record keyed by flow id.
func (e *Engine) registerFlow(f *FlowRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.flows == nil {
		e.flows = map[string]*FlowRecord{}
	}
	e.flows[f.FlowID] = f
}

// FlowGet returns the status of a popup flow. Unknown or foreign-scope ids look like not_found.
func (e *Engine) FlowGet(_ context.Context, scope contracts.Scope, flowID string) (FlowView, error) {
	if err := scope.Validate(); err != nil {
		return FlowView{}, err
	}
	if !ids.Valid(flowID, flowIDPrefix) {
		return FlowView{}, flowNotFound()
	}
	now := e.clk.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.purgeFlowsLocked(now)
	f, ok := e.flows[flowID]
	if !ok || !sameOwner(f.scope, scope) {
		return FlowView{}, flowNotFound()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Status == FlowPending && !now.Before(f.expires) {
		f.Status, f.Reason, f.UpdatedAt = FlowExpired, ReasonStateExpired, now
	}
	return flowView(f), nil
}

// FlowCancel cancels a pending flow. Idempotent for a terminal flow. Unknown/foreign → not_found.
func (e *Engine) FlowCancel(_ context.Context, scope contracts.Scope, flowID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !ids.Valid(flowID, flowIDPrefix) {
		return flowNotFound()
	}
	now := e.clk.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.purgeFlowsLocked(now)
	f, ok := e.flows[flowID]
	if !ok || !sameOwner(f.scope, scope) {
		return flowNotFound()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Status == FlowPending {
		f.Status, f.Reason, f.UpdatedAt = FlowCancelled, ReasonAccessDenied, now
		if f.stateKey != "" {
			delete(e.pending, f.stateKey)
			e.consumed[f.stateKey] = usedState{until: now.Add(e.ttl)}
		}
		if ln, ok := e.loopbacks[f.FlowID]; ok {
			_ = ln.Close()
			delete(e.loopbacks, f.FlowID)
		}
	}
	return nil
}

func flowNotFound() error {
	return contracts.New(contracts.ErrValidationFailed, "oauth: the flow is unknown").WithDetails("reason", "not_found")
}

func flowView(f *FlowRecord) FlowView {
	v := FlowView{FlowID: f.FlowID, Provider: f.Provider, Status: f.Status, Reason: f.Reason, UpdatedAt: f.UpdatedAt}
	if f.Status == FlowCompleted && f.conn != nil {
		c := *f.conn
		v.Connection = &c
	}
	return v
}

func (e *Engine) purgeFlowsLocked(now time.Time) {
	for id, f := range e.flows {
		f.mu.Lock()
		terminal := f.Status != FlowPending
		expired := !now.Before(f.expires.Add(e.ttl))
		if f.Status == FlowPending && !now.Before(f.expires) {
			f.Status, f.Reason, f.UpdatedAt = FlowExpired, ReasonStateExpired, now
		}
		f.mu.Unlock()
		if terminal && expired {
			delete(e.flows, id)
			if ln, ok := e.loopbacks[id]; ok {
				_ = ln.Close()
				delete(e.loopbacks, id)
			}
		}
	}
}

// markFlowCompleted records a successful Complete against the flow that owned the pending state.
func (e *Engine) markFlowCompleted(stateKey string, conn Connection) {
	now := e.clk.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range e.flows {
		f.mu.Lock()
		if f.stateKey == stateKey && f.Status == FlowPending {
			c := conn
			f.Status, f.Reason, f.UpdatedAt, f.conn = FlowCompleted, "", now, &c
		}
		f.mu.Unlock()
	}
}

// markFlowFailed records a failed Complete (or loopback exchange) against the flow.
func (e *Engine) markFlowFailed(stateKey string, reason Reason) {
	now := e.clk.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range e.flows {
		f.mu.Lock()
		if f.stateKey == stateKey && f.Status == FlowPending {
			f.Status, f.Reason, f.UpdatedAt = FlowFailed, reason, now
		}
		f.mu.Unlock()
	}
}

// markFlowByIDFailed updates a flow by id (loopback path when state is absent).
func (e *Engine) markFlowByIDFailed(flowID string, reason Reason) {
	now := e.clk.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if f, ok := e.flows[flowID]; ok {
		f.mu.Lock()
		if f.Status == FlowPending {
			f.Status, f.Reason, f.UpdatedAt = FlowFailed, reason, now
		}
		f.mu.Unlock()
	}
}

// markFlowByIDCompleted updates a flow by id after a successful loopback exchange.
func (e *Engine) markFlowByIDCompleted(flowID string, conn Connection) {
	now := e.clk.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if f, ok := e.flows[flowID]; ok {
		f.mu.Lock()
		if f.Status == FlowPending {
			c := conn
			f.Status, f.Reason, f.UpdatedAt, f.conn = FlowCompleted, "", now, &c
		}
		f.mu.Unlock()
	}
}

// OriginAllowed reports whether origin is on the operator popup_origins allowlist (exact match, never *).
func OriginAllowed(origin string, allowlist []string) bool {
	if origin == "" || len(allowlist) == 0 {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return false
	}
	if u.Path != "" && u.Path != "/" {
		return false
	}
	if u.Path == "/" {
		return false // origins have no path; reject trailing-slash forms
	}
	canon := u.Scheme + "://" + u.Host
	for _, a := range allowlist {
		if a == canon {
			return true
		}
	}
	return false
}
