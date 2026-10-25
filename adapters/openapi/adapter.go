package openapi

import (
	"context"
	"io"
	"sort"
	"sync"

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

// mgmtRoutes are the operations the adapter configuration of an endpoint names.
type mgmtRoutes struct {
	describe, health, inspect *route
	provisioning              map[string]*route
}

// Adapter is an adapters.AppAdapter over one or more HTTP APIs described by
// OpenAPI documents. It is the raw transport: production wiring wraps it with
// adapters.Wrap, which checks the scope and the request, bounds time and
// concurrency and re-validates every result. It is safe for concurrent use and
// implements io.Closer.
//
// See the package comment for what it will and will not do.
type Adapter struct {
	cfg   Config
	eps   map[string]*endpoint
	order []*endpoint
	caps  map[string]*capRoutes
	mgmt  map[string]*mgmtRoutes

	closeCtx  context.Context
	closeFn   context.CancelFunc
	closeOnce sync.Once
}

var (
	_ adapters.AppAdapter = (*Adapter)(nil)
	_ io.Closer           = (*Adapter)(nil)
)

// New builds the adapter. It loads and validates every document and checks the
// mapping against them, but opens no connection: the first call does, so an
// application that is down when AlixAI starts is reached as soon as it is up. A
// configuration, document or mapping it refuses is a validation_failed error.
func New(cfg Config) (*Adapter, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Dialer == nil {
		cfg.Dialer = egress.NewDialer(nil) // nothing is allowed
	}
	a := &Adapter{cfg: cfg, eps: map[string]*endpoint{}, caps: map[string]*capRoutes{}, mgmt: map[string]*mgmtRoutes{}}
	a.closeCtx, a.closeFn = context.WithCancel(context.Background())

	for _, ec := range cfg.Endpoints {
		ep, err := newEndpoint(ec)
		if err != nil {
			return nil, err
		}
		ep.client = a.newClient(ep)
		a.eps[ec.ID] = ep
		a.order = append(a.order, ep)
	}
	sort.Slice(a.order, func(i, j int) bool { return a.order[i].cfg.ID < a.order[j].cfg.ID })

	mk := func(where, endpointID, opID string, r role, b *Binding) (*route, error) {
		ep := a.eps[endpointID]
		op := ep.ops[opID]
		if op == nil {
			return nil, cfgErr("%s maps the operationId %q, which the OpenAPI document of endpoint %q does not define", where, truncate(opID, 40), endpointID)
		}
		if err := op.prepare(r); err != nil {
			return nil, err
		}
		if r == roleInspect {
			if p := op.params["aspects"]; p == nil || p.in != "query" {
				return nil, cfgErr("%s: the inspect operation %q declares no query parameter named aspects", where, truncate(opID, 40))
			}
		}
		ep.approved[opID] = true
		rt := &route{op: op, role: r}
		if b != nil {
			rt.timeout = b.Timeout
			if len(b.Arguments) > 0 {
				rt.args = make(map[string]string, len(b.Arguments))
				for k, v := range b.Arguments {
					rt.args[k] = v
				}
			}
		}
		return rt, nil
	}
	for _, cp := range cfg.Capabilities {
		cr := &capRoutes{ops: map[adapters.Operation]*route{}, resolve: map[string]*route{}}
		for op, b := range cp.Operations {
			rt, err := mk("capability "+cp.ID+" "+string(op), b.Endpoint, b.OperationID, roleOf(op), &b)
			if err != nil {
				return nil, err
			}
			cr.ops[op] = rt
		}
		for typ, b := range cp.Resolve {
			rt, err := mk("capability "+cp.ID+" resolver of "+typ, b.Endpoint, b.OperationID, roleResolve, &b)
			if err != nil {
				return nil, err
			}
			cr.resolve[typ] = rt
		}
		a.caps[cp.ID] = cr
	}
	for _, ep := range a.order {
		m := &ep.cfg.Settings.Management
		mr := &mgmtRoutes{provisioning: map[string]*route{}}
		for _, x := range []struct {
			id   string
			r    role
			slot **route
		}{{m.Describe, roleDescribe, &mr.describe}, {m.Health, roleHealth, &mr.health}, {m.Inspect, roleInspect, &mr.inspect}} {
			if x.id == "" {
				continue
			}
			rt, err := mk("the management "+string(x.r)+" operation", ep.cfg.ID, x.id, x.r, nil)
			if err != nil {
				return nil, err
			}
			*x.slot = rt
		}
		for _, id := range m.Provisioning {
			rt, err := mk("a provisioning operation", ep.cfg.ID, id, roleProvision, nil)
			if err != nil {
				return nil, err
			}
			mr.provisioning[id] = rt
		}
		a.mgmt[ep.cfg.ID] = mr
	}
	for _, ep := range a.order {
		if ep.cfg.ContractSHA256 == "" {
			continue
		}
		ids := make([]string, 0, len(ep.approved))
		for id := range ep.approved {
			ids = append(ids, id)
		}
		got, err := ContractSHA256(ep.cfg.Spec, ids)
		if err != nil {
			return nil, cfgErr("endpoint %q: the approved operation subset cannot be hashed: %s", ep.cfg.ID, reasonOf(err))
		}
		if got != ep.cfg.ContractSHA256 {
			return nil, contracts.New(contracts.ErrValidationFailed, "the approved operation subset of an endpoint does not hash as the manifest approved it").
				WithDetails("transport", Transport).WithDetails("reason", ReasonContractMismatch).WithDetails("endpoint", ep.cfg.ID)
		}
	}
	return a, nil
}

// ReasonContractMismatch is the reason of the error New returns when the
// approved operation subset of a document does not hash as contract_sha256 says.
const ReasonContractMismatch = "contract_sha256_mismatch"

// Close ends every call in flight and drops idle connections. It is idempotent.
// Calls after Close are unavailable.
func (a *Adapter) Close() error {
	a.closeOnce.Do(func() {
		a.closeFn()
		for _, ep := range a.order {
			ep.client.CloseIdleConnections()
		}
	})
	return nil
}

func (a *Adapter) opRoute(capID string, op adapters.Operation) (*route, error) {
	if cp := a.caps[capID]; cp != nil {
		if rt := cp.ops[op]; rt != nil {
			return rt, nil
		}
	}
	return nil, typed(contracts.ErrUnknownCapability, "the package maps no operation for this operation of the capability", ReasonOperationNotMapped)
}

// readRoute picks the operation of a Read. A Read whose fields carry the
// reserved argument entity_type is the core's entity resolution (the adapter
// contract has no operation of its own for it): it goes to the operation the
// capability declares as the resolver of that type and to no other. An entity
// type the capability does not declare is validation_failed and nothing is sent.
func (a *Adapter) readRoute(capID string, fields map[string]any) (rt *route, resolver bool, err error) {
	v, reserved := fields["entity_type"]
	if !reserved {
		rt, err = a.opRoute(capID, adapters.OpRead)
		return rt, false, err
	}
	typ, _ := v.(string)
	cp := a.caps[capID]
	if cp == nil {
		return nil, false, typed(contracts.ErrUnknownCapability, "the package maps no operation for this operation of the capability", ReasonOperationNotMapped)
	}
	rt = cp.resolve[typ]
	if typ == "" || rt == nil {
		return nil, false, typed(contracts.ErrValidationFailed, "the capability declares no resolver for this entity type", ReasonUnknownEntityType)
	}
	return rt, true, nil
}

// fieldArgs applies the binding's renames to the fields and canonicalises the
// values. Two fields that end up under one name are refused rather than merged.
func fieldArgs(rt *route, fields map[string]any, identity adapters.Identity, resolver bool) (callArgs, error) {
	canon, err := canonicalFields(fields)
	if err != nil {
		return callArgs{}, err
	}
	out := make(map[string]any, len(canon))
	opt := map[string]bool{}
	for k, v := range canon {
		orig := k
		if to, ok := rt.args[k]; ok {
			k = to
		}
		if _, dup := out[k]; dup {
			return callArgs{}, contracts.New(contracts.ErrValidationFailed, "adapter request: two fields map to one parameter")
		}
		out[k] = v
		if resolver && orig == "entity_type" {
			opt[k] = true
		}
	}
	return callArgs{fields: out, optional: opt, identity: &identity}, nil
}

// Read implements adapters.AppAdapter.
func (a *Adapter) Read(ctx context.Context, scope contracts.Scope, req adapters.ReadRequest) (actions.DeterministicResult, error) {
	rt, resolver, err := a.readRoute(req.CapabilityID, req.Fields)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	args, err := fieldArgs(rt, req.Fields, adapters.IdentityFrom(scope), resolver)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return a.result(ctx, adapters.OpRead, req.CapabilityID, rt, args)
}

// Validate implements adapters.AppAdapter.
func (a *Adapter) Validate(ctx context.Context, scope contracts.Scope, req adapters.ValidateRequest) (actions.DeterministicResult, error) {
	rt, err := a.opRoute(req.CapabilityID, adapters.OpValidate)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	args, err := fieldArgs(rt, req.Fields, adapters.IdentityFrom(scope), false)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return a.result(ctx, adapters.OpValidate, req.CapabilityID, rt, args)
}

// Prepare implements adapters.AppAdapter.
func (a *Adapter) Prepare(ctx context.Context, scope contracts.Scope, req adapters.PrepareRequest) (actions.DeterministicResult, error) {
	rt, err := a.opRoute(req.CapabilityID, adapters.OpPrepare)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	args, err := fieldArgs(rt, req.Fields, adapters.IdentityFrom(scope), false)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	return a.result(ctx, adapters.OpPrepare, req.CapabilityID, rt, args)
}

// Commit implements adapters.AppAdapter. The prepared action travels as the
// request body and the idempotency key in the idempotency header.
func (a *Adapter) Commit(ctx context.Context, scope contracts.Scope, req adapters.CommitRequest) (actions.DeterministicResult, error) {
	rt, err := a.opRoute(req.CapabilityID, adapters.OpCommit)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	id, p := adapters.IdentityFrom(scope), req.Prepared
	return a.result(ctx, adapters.OpCommit, req.CapabilityID, rt, callArgs{key: string(req.IdempotencyKey), prepared: &p, identity: &id})
}

// Status implements adapters.AppAdapter.
func (a *Adapter) Status(ctx context.Context, scope contracts.Scope, req adapters.StatusRequest) (actions.DeterministicResult, error) {
	rt, err := a.opRoute(req.CapabilityID, adapters.OpStatus)
	if err != nil {
		return actions.DeterministicResult{}, err
	}
	id := adapters.IdentityFrom(scope)
	return a.result(ctx, adapters.OpStatus, req.CapabilityID, rt, callArgs{key: string(req.IdempotencyKey), identity: &id})
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
	id := adapters.IdentityFrom(scope)
	return a.result(ctx, adapters.OpCancel, req.CapabilityID, rt, callArgs{key: string(req.IdempotencyKey), identity: &id})
}
