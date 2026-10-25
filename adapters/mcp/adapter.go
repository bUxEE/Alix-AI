package mcp

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// capRoutes is the mapping of one capability.
type capRoutes struct {
	ops     map[adapters.Operation]*route
	resolve map[string]*route
}

// Adapter is an adapters.AppAdapter over one or more MCP servers. It is the
// raw transport: production wiring wraps it with adapters.Wrap, which checks the
// scope and the request, bounds time and concurrency and re-validates every
// result. It is safe for concurrent use and implements io.Closer.
//
// See the package comment for what it will and will not do.
type Adapter struct {
	cfg  Config
	eps  []*endpointConn
	byID map[string]*endpointConn
	caps map[string]*capRoutes

	mu     sync.Mutex
	closed bool

	driftMu sync.Mutex
	drifts  []Drift
	reaudit atomic.Bool
}

var (
	_ adapters.AppAdapter = (*Adapter)(nil)
	_ io.Closer           = (*Adapter)(nil)
)

// New builds the adapter. It opens nothing: sessions are opened by the first
// call that needs one and again after one breaks, so an application that is down
// when AlixAI starts is reached as soon as it is up. A configuration it refuses
// is a validation_failed error.
func New(cfg Config) (*Adapter, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Dialer == nil {
		cfg.Dialer = egress.NewDialer(nil) // nothing is allowed
	}
	a := &Adapter{cfg: cfg, byID: map[string]*endpointConn{}, caps: map[string]*capRoutes{}}

	for _, ep := range cfg.Endpoints {
		e := &endpointConn{
			a:           a,
			cfg:         ep,
			mapped:      map[string]bool{},
			managed:     map[string]bool{cfg.DescribeTool: true, cfg.HealthTool: true, cfg.InspectTool: true},
			connSlot:    make(chan struct{}, 1),
			refreshSlot: make(chan struct{}, 1),
			pins:        map[string]string{},
			drifts:      map[string]Drift{},
		}
		if ep.Allow != nil {
			e.approved = map[string]bool{}
			for _, t := range ep.Allow {
				e.approved[t] = true
			}
		}
		for k, v := range cfg.Pins {
			if id, tool, _ := strings.Cut(k, "/"); id == ep.ID {
				e.pins[tool] = v
			}
		}
		e.client = mcp.NewClient(&mcp.Implementation{Name: cfg.ClientName, Version: cfg.ClientVersion}, &mcp.ClientOptions{
			// The SDK logs URLs and server text; this adapter logs only what it
			// chooses to.
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			// No roots, sampling or elicitation: the adapter answers questions
			// the application asks about its own tools, nothing else.
			Capabilities:           &mcp.ClientCapabilities{},
			MultiRoundTrip:         &mcp.MultiRoundTripOptions{Disabled: true},
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { e.markStale() },
		})
		if ep.URL != "" {
			e.http = a.httpClient(e)
		}
		a.byID[ep.ID] = e
		a.eps = append(a.eps, e)
	}
	sort.Slice(a.eps, func(i, j int) bool { return a.eps[i].cfg.ID < a.eps[j].cfg.ID })

	mk := func(b Binding) *route {
		ep := a.byID[b.Endpoint]
		ep.mapped[b.Tool] = true
		rt := &route{ep: ep, tool: b.Tool, timeout: b.Timeout}
		if len(b.Arguments) > 0 {
			rt.args = make(map[string]string, len(b.Arguments))
			for k, v := range b.Arguments {
				rt.args[k] = v
			}
		}
		return rt
	}
	for _, cp := range cfg.Capabilities {
		cr := &capRoutes{ops: map[adapters.Operation]*route{}, resolve: map[string]*route{}}
		for op, b := range cp.Operations {
			cr.ops[op] = mk(b)
		}
		for typ, b := range cp.Resolve {
			cr.resolve[typ] = mk(b)
		}
		a.caps[cp.ID] = cr
	}
	return a, nil
}

// Close ends every session now (and every stdio process), failing the calls still
// in flight. It is idempotent. Calls after Close are unavailable.
func (a *Adapter) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	a.mu.Unlock()
	for _, e := range a.eps {
		e.close()
	}
	return nil
}

// Drifts returns the drift found so far, oldest first.
func (a *Adapter) Drifts() []Drift {
	a.driftMu.Lock()
	defer a.driftMu.Unlock()
	return append([]Drift(nil), a.drifts...)
}

// ReauditRequired is the re-audit flag: true once any tool has drifted. It stays
// true for the life of the adapter; calls to the drifted tools stay refused.
func (a *Adapter) ReauditRequired() bool { return a.reaudit.Load() }

// Pins returns the tool hashes the adapter holds, keyed "endpoint/tool": the pins
// it was given and the ones it took when a tool was first listed. They are what
// an audit records so a later adapter can be given them back (Config.Pins).
func (a *Adapter) Pins() map[string]string {
	out := map[string]string{}
	for _, e := range a.eps {
		for tool, h := range e.snapshotPins() {
			out[e.cfg.ID+"/"+tool] = h
		}
	}
	return out
}

func (a *Adapter) raiseDrift(d Drift) {
	a.driftMu.Lock()
	a.drifts = append(a.drifts, d)
	a.driftMu.Unlock()
	a.reaudit.Store(true)
	a.cfg.Logger.Warn("an MCP tool changed since it was approved; its calls are refused until the application is audited again",
		slog.String("application_id", a.cfg.ApplicationID), slog.String("endpoint", d.Endpoint), slog.String("tool", d.Tool),
		slog.String("drift", string(d.Kind)), slog.String("pinned", short(d.Pinned)), slog.String("observed", short(d.Observed)))
	if a.cfg.OnDrift != nil {
		a.cfg.OnDrift(d)
	}
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// ---- results ----------------------------------------------------------------

func (a *Adapter) opRoute(capID string, op adapters.Operation) (*route, error) {
	if cp := a.caps[capID]; cp != nil {
		if rt := cp.ops[op]; rt != nil {
			return rt, nil
		}
	}
	return nil, typed(contracts.ErrUnknownCapability, "the package maps no tool for this operation of the capability", ReasonOperationNotMapped)
}

// readRoute picks the tool of a Read. A Read whose fields carry the reserved
// argument entity_type is the core's entity resolution (master plan 9.3: the
// adapter contract has no operation of its own for it): it goes to the tool the
// capability declares as the resolver of that type and to no other. The fields,
// with entity_type, mention and candidate_id, are sent as they are.
func (a *Adapter) readRoute(capID string, fields map[string]any) (*route, error) {
	v, reserved := fields["entity_type"]
	if !reserved {
		return a.opRoute(capID, adapters.OpRead)
	}
	typ, _ := v.(string)
	cp := a.caps[capID]
	if cp == nil {
		return nil, typed(contracts.ErrUnknownCapability, "the package maps no tool for this operation of the capability", ReasonOperationNotMapped)
	}
	rt := cp.resolve[typ]
	if typ == "" || rt == nil {
		return nil, typed(contracts.ErrValidationFailed, "the capability declares no resolver for this entity type", ReasonUnknownEntityType)
	}
	return rt, nil
}

// renameFields applies the binding's argument renames. Two fields that end up
// under one name are refused rather than merged.
func renameFields(fields map[string]any, rename map[string]string) (map[string]any, error) {
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		if to, ok := rename[k]; ok {
			k = to
		}
		if _, dup := out[k]; dup {
			return nil, contracts.New(contracts.ErrValidationFailed, "adapter request: two fields map to one tool argument")
		}
		out[k] = v
	}
	return out, nil
}

// do sends one result operation and normalises the reply.
func (a *Adapter) do(ctx context.Context, scope contracts.Scope, op adapters.Operation, capID string, rt *route,
	fields map[string]any, withFields bool, extra map[string]any, key string) (actions.DeterministicResult, error) {
	args := map[string]any{"identity": adapters.IdentityFrom(scope)}
	if withFields {
		f, err := renameFields(fields, rt.args)
		if err != nil {
			return actions.DeterministicResult{}, err
		}
		args["fields"] = f
	}
	for k, v := range extra {
		args[k] = v
	}
	rep, err := a.call(ctx, rt, args, op == adapters.OpCommit)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return adapters.Normalise(rep.structured, adapters.Expect{
		Operation:            op,
		CapabilityID:         capID,
		IdempotencyKey:       key,
		EchoedIdempotencyKey: rep.echoedKey,
		MaxBytes:             int(a.cfg.MaxResponseBytes),
	})
}

// Read implements adapters.AppAdapter.
func (a *Adapter) Read(ctx context.Context, scope contracts.Scope, req adapters.ReadRequest) (actions.DeterministicResult, error) {
	rt, err := a.readRoute(req.CapabilityID, req.Fields)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return a.do(ctx, scope, adapters.OpRead, req.CapabilityID, rt, req.Fields, true, nil, "")
}

// Validate implements adapters.AppAdapter.
func (a *Adapter) Validate(ctx context.Context, scope contracts.Scope, req adapters.ValidateRequest) (actions.DeterministicResult, error) {
	rt, err := a.opRoute(req.CapabilityID, adapters.OpValidate)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return a.do(ctx, scope, adapters.OpValidate, req.CapabilityID, rt, req.Fields, true, nil, "")
}

// Prepare implements adapters.AppAdapter.
func (a *Adapter) Prepare(ctx context.Context, scope contracts.Scope, req adapters.PrepareRequest) (actions.DeterministicResult, error) {
	rt, err := a.opRoute(req.CapabilityID, adapters.OpPrepare)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return a.do(ctx, scope, adapters.OpPrepare, req.CapabilityID, rt, req.Fields, true, nil, "")
}

// Commit implements adapters.AppAdapter.
func (a *Adapter) Commit(ctx context.Context, scope contracts.Scope, req adapters.CommitRequest) (actions.DeterministicResult, error) {
	rt, err := a.opRoute(req.CapabilityID, adapters.OpCommit)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return a.do(ctx, scope, adapters.OpCommit, req.CapabilityID, rt, nil, false,
		map[string]any{"idempotency_key": string(req.IdempotencyKey), "prepared": req.Prepared}, string(req.IdempotencyKey))
}

// Status implements adapters.AppAdapter.
func (a *Adapter) Status(ctx context.Context, scope contracts.Scope, req adapters.StatusRequest) (actions.DeterministicResult, error) {
	rt, err := a.opRoute(req.CapabilityID, adapters.OpStatus)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return a.do(ctx, scope, adapters.OpStatus, req.CapabilityID, rt, nil, false,
		map[string]any{"idempotency_key": string(req.IdempotencyKey)}, string(req.IdempotencyKey))
}

// Cancel implements adapters.AppAdapter. A package has no cancel operation, so
// for one the answer is unknown_capability: the application is not asked to
// withdraw anything, and the caller learns the outcome of a prepared action from
// Status and its expiry.
func (a *Adapter) Cancel(ctx context.Context, scope contracts.Scope, req adapters.CancelRequest) (actions.DeterministicResult, error) {
	rt, err := a.opRoute(req.CapabilityID, adapters.OpCancel)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return a.do(ctx, scope, adapters.OpCancel, req.CapabilityID, rt, nil, false,
		map[string]any{"idempotency_key": string(req.IdempotencyKey)}, string(req.IdempotencyKey))
}
