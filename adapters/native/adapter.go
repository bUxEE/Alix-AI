package native

import (
	"context"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

type endpoint struct {
	cfg Endpoint
	// mapped are the methods a capability maps on this endpoint, and the
	// provisioning operations: the ones the package knows about.
	mapped map[string]bool
}

// route is one mapped operation: the bridge, the method, its timeout and its
// argument renames.
type route struct {
	ep      *endpoint
	method  string
	timeout time.Duration
	args    map[string]string
}

type capRoutes struct {
	ops     map[adapters.Operation]*route
	resolve map[string]*route
}

// Adapter is an adapters.AppAdapter over one or more native bridges. It is the
// raw transport: production wiring wraps it with adapters.Wrap, which checks the
// scope and the request, bounds time and concurrency and re-validates every
// result. It is safe for concurrent use and implements io.Closer.
//
// See the package comment for what it will and will not do.
type Adapter struct {
	cfg   Config
	eps   map[string]*endpoint
	order []*endpoint
	caps  map[string]*capRoutes

	closeCtx  context.Context
	closeFn   context.CancelFunc
	closeOnce sync.Once
}

var (
	_ adapters.AppAdapter = (*Adapter)(nil)
	_ io.Closer           = (*Adapter)(nil)
)

// New builds the adapter. It checks the configuration and opens no connection: the
// first call does, so an application that is down when AlixAI starts is reached as
// soon as it is up. A configuration it refuses is a validation_failed error.
func New(cfg Config) (*Adapter, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Dialer == nil {
		cfg.Dialer = egress.NewDialer(nil) // nothing is allowed
	}
	a := &Adapter{cfg: cfg, eps: map[string]*endpoint{}, caps: map[string]*capRoutes{}}
	a.closeCtx, a.closeFn = context.WithCancel(context.Background())
	for _, ec := range cfg.Endpoints {
		ec.Provisioning = append([]string(nil), ec.Provisioning...)
		ep := &endpoint{cfg: ec, mapped: map[string]bool{}}
		for _, p := range ec.Provisioning {
			ep.mapped[p] = true
		}
		a.eps[ec.ID] = ep
		a.order = append(a.order, ep)
	}
	sort.Slice(a.order, func(i, j int) bool { return a.order[i].cfg.ID < a.order[j].cfg.ID })

	mk := func(b Binding) *route {
		ep := a.eps[b.Endpoint]
		ep.mapped[b.Method] = true
		rt := &route{ep: ep, method: b.Method, timeout: b.Timeout}
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

// Close ends every call in flight. It is idempotent. Calls after Close are
// unavailable.
func (a *Adapter) Close() error {
	a.closeOnce.Do(a.closeFn)
	return nil
}

func (a *Adapter) opRoute(capID string, op adapters.Operation) (*route, error) {
	if cp := a.caps[capID]; cp != nil {
		if rt := cp.ops[op]; rt != nil {
			return rt, nil
		}
	}
	return nil, typed(contracts.ErrUnknownCapability, "the package maps no method for this operation of the capability", ReasonOperationNotMapped)
}

// readRoute picks the method of a Read. A Read whose fields carry the reserved
// argument entity_type is the core's entity resolution (the adapter contract has
// no operation of its own for it): it goes to the method the capability declares
// as the resolver of that type and to no other, with its fields unchanged. An
// entity type the capability does not declare is validation_failed and nothing is
// sent.
func (a *Adapter) readRoute(capID string, fields map[string]any) (*route, error) {
	v, reserved := fields["entity_type"]
	if !reserved {
		return a.opRoute(capID, adapters.OpRead)
	}
	typ, _ := v.(string)
	cp := a.caps[capID]
	if cp == nil {
		return nil, typed(contracts.ErrUnknownCapability, "the package maps no method for this operation of the capability", ReasonOperationNotMapped)
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
			return nil, contracts.New(contracts.ErrValidationFailed, "adapter request: two fields map to one method argument")
		}
		out[k] = v
	}
	return out, nil
}

// withFields is the call of read, validate and prepare.
func (a *Adapter) withFields(ctx context.Context, scope contracts.Scope, op adapters.Operation, capID string, rt *route, fields map[string]any) (actions.DeterministicResult, error) {
	f, err := renameFields(fields, rt.args)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return a.result(ctx, op, capID, rt, map[string]any{"identity": adapters.IdentityFrom(scope), "fields": f}, "")
}

// Read implements adapters.AppAdapter.
func (a *Adapter) Read(ctx context.Context, scope contracts.Scope, req adapters.ReadRequest) (actions.DeterministicResult, error) {
	rt, err := a.readRoute(req.CapabilityID, req.Fields)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return a.withFields(ctx, scope, adapters.OpRead, req.CapabilityID, rt, req.Fields)
}

// Validate implements adapters.AppAdapter.
func (a *Adapter) Validate(ctx context.Context, scope contracts.Scope, req adapters.ValidateRequest) (actions.DeterministicResult, error) {
	rt, err := a.opRoute(req.CapabilityID, adapters.OpValidate)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return a.withFields(ctx, scope, adapters.OpValidate, req.CapabilityID, rt, req.Fields)
}

// Prepare implements adapters.AppAdapter.
func (a *Adapter) Prepare(ctx context.Context, scope contracts.Scope, req adapters.PrepareRequest) (actions.DeterministicResult, error) {
	rt, err := a.opRoute(req.CapabilityID, adapters.OpPrepare)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return a.withFields(ctx, scope, adapters.OpPrepare, req.CapabilityID, rt, req.Fields)
}

// Commit implements adapters.AppAdapter. The prepared action and the idempotency
// key travel as members of the request.
func (a *Adapter) Commit(ctx context.Context, scope contracts.Scope, req adapters.CommitRequest) (actions.DeterministicResult, error) {
	rt, err := a.opRoute(req.CapabilityID, adapters.OpCommit)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	params := map[string]any{"identity": adapters.IdentityFrom(scope), "idempotency_key": string(req.IdempotencyKey), "prepared": req.Prepared}
	return a.result(ctx, adapters.OpCommit, req.CapabilityID, rt, params, string(req.IdempotencyKey))
}

// Status implements adapters.AppAdapter.
func (a *Adapter) Status(ctx context.Context, scope contracts.Scope, req adapters.StatusRequest) (actions.DeterministicResult, error) {
	rt, err := a.opRoute(req.CapabilityID, adapters.OpStatus)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	params := map[string]any{"identity": adapters.IdentityFrom(scope), "idempotency_key": string(req.IdempotencyKey)}
	return a.result(ctx, adapters.OpStatus, req.CapabilityID, rt, params, string(req.IdempotencyKey))
}

// Cancel implements adapters.AppAdapter. A package has no cancel operation, so
// for one the answer is unknown_capability: the application is not asked to
// withdraw anything, and the caller learns the outcome of a prepared action from
// Status and its expiry. A host that wires the adapter in code can map one.
func (a *Adapter) Cancel(ctx context.Context, scope contracts.Scope, req adapters.CancelRequest) (actions.DeterministicResult, error) {
	rt, err := a.opRoute(req.CapabilityID, adapters.OpCancel)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	params := map[string]any{"identity": adapters.IdentityFrom(scope), "idempotency_key": string(req.IdempotencyKey)}
	return a.result(ctx, adapters.OpCancel, req.CapabilityID, rt, params, string(req.IdempotencyKey))
}
