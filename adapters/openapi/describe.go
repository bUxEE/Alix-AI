package openapi

import (
	"context"
	"encoding/json"
	"net"
	"sort"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Describe implements adapters.AppAdapter.
//
// The description is the adapter's own finding, not the application's claim. The
// contract versions and the version label come from the endpoint's describe
// operation (when it has one); the operations of each capability are those the
// package maps; Unmapped is every operation the OpenAPI documents define that no
// capability and no management role maps, plus what the application itself says
// it exposes that the adapter does not know. None of those is callable.
// Nothing the application says about its own capabilities is believed, so an
// application cannot make the adapter list an operation the package does not
// map.
func (a *Adapter) Describe(ctx context.Context) (adapters.Description, error) {
	var (
		versions     []int
		haveVersions bool
		appVersion   string
		hostUnmapped = map[string]bool{}
	)
	for _, ep := range a.order {
		rt := a.mgmt[ep.cfg.ID].describe
		if rt == nil {
			continue
		}
		body, err := a.document(ctx, rt, callArgs{})
		if err != nil {
			return adapters.Description{}, err
		}
		host, err := adapters.ParseDescription(Transport, body)
		if err != nil {
			return adapters.Description{}, err
		}
		if !haveVersions {
			versions, haveVersions = host.ContractVersions, true
		} else {
			versions = intersect(versions, host.ContractVersions)
		}
		if appVersion == "" {
			appVersion = host.ApplicationVersion
		}
		for _, u := range host.Unmapped {
			hostUnmapped[unmappedName(u)] = true
		}
	}
	if !haveVersions {
		versions = []int{adapters.ContractVersion}
	}
	if len(versions) == 0 {
		return adapters.Description{}, contracts.New(contracts.ErrValidationFailed, "the application's endpoints share no contract version").
			WithDetails("reason", "version_incompatible")
	}

	d := adapters.Description{Transport: Transport, ContractVersions: versions, ApplicationVersion: appVersion, Capabilities: []adapters.CapabilityOps{}}
	ids := make([]string, 0, len(a.caps))
	for id := range a.caps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		var ops []adapters.Operation
		for _, op := range adapters.ResultOperations() {
			if a.caps[id].ops[op] != nil {
				ops = append(ops, op)
			}
		}
		if len(ops) > 0 {
			d.Capabilities = append(d.Capabilities, adapters.CapabilityOps{CapabilityID: id, Operations: ops})
		}
	}

	known := map[string]bool{}
	unmapped := map[string]bool{}
	for _, ep := range a.order {
		for id := range ep.ops {
			known[unmappedName(id)] = true
			if !ep.approved[id] {
				unmapped[unmappedName(id)] = true
			}
		}
		for _, anon := range ep.anonymous {
			unmapped[unmappedName(anon)] = true
		}
	}
	for u := range hostUnmapped {
		if !known[u] {
			unmapped[u] = true
		}
	}
	for n := range unmapped {
		d.Unmapped = append(d.Unmapped, n)
	}
	sort.Strings(d.Unmapped)
	if len(d.Unmapped) > 1024 {
		d.Unmapped = d.Unmapped[:1024]
	}
	return d, nil
}

func intersect(a, b []int) []int {
	var out []int
	for _, x := range a {
		for _, y := range b {
			if x == y {
				out = append(out, x)
				break
			}
		}
	}
	return out
}

// Health implements adapters.AppAdapter: the health operation of every
// endpoint, the worst state being the application's. An endpoint whose
// configuration names no health operation is probed only for reachability (a
// connection to its host and port, which is no request and reaches no
// operation): all that is known of it is that it accepts connections, which is
// reported as degraded with a reason that says so, never as ok.
func (a *Adapter) Health(ctx context.Context) (adapters.HealthReport, error) {
	out := adapters.HealthReport{Status: adapters.HealthOK}
	rank := map[adapters.Health]int{adapters.HealthOK: 0, adapters.HealthDegraded: 1, adapters.HealthUnavailable: 2}
	for _, ep := range a.order {
		h, err := a.endpointHealth(ctx, ep)
		if err != nil {
			return adapters.HealthReport{}, err
		}
		if rank[h.Status] > rank[out.Status] {
			out.Status, out.Reason = h.Status, h.Reason
		}
		if out.ApplicationVersion == "" {
			out.ApplicationVersion = h.ApplicationVersion
		}
	}
	return out, nil
}

func (a *Adapter) endpointHealth(ctx context.Context, ep *endpoint) (adapters.HealthReport, error) {
	rt := a.mgmt[ep.cfg.ID].health
	if rt == nil {
		return a.reachability(ctx, ep)
	}
	body, err := a.document(ctx, rt, callArgs{})
	if err != nil {
		return adapters.HealthReport{}, err
	}
	return adapters.ParseHealth(body)
}

// reachability connects to the endpoint's host and port through the egress
// dialer and closes the connection again.
func (a *Adapter) reachability(ctx context.Context, ep *endpoint) (adapters.HealthReport, error) {
	if err := ctx.Err(); err != nil {
		return adapters.HealthReport{}, err
	}
	addr := net.JoinHostPort(ep.base.Hostname(), ep.origin.port)
	conn, err := a.cfg.Dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return adapters.HealthReport{}, cerr
		}
		return adapters.HealthReport{Status: adapters.HealthUnavailable, Reason: "the application did not accept a connection"}, nil
	}
	_ = conn.Close()
	return adapters.HealthReport{Status: adapters.HealthDegraded, Reason: "the package maps no health operation; only reachability was checked"}, nil
}

// Inspect implements adapters.AppAdapter: the inspect operation of the first
// endpoint that has one. An endpoint without one exposes nothing for
// inspection, and every aspect is then unsupported, which the audit records as
// unverified.
func (a *Adapter) Inspect(ctx context.Context, scope contracts.Scope, req adapters.InspectRequest) (adapters.Inspection, error) {
	if err := req.Validate(); err != nil {
		return adapters.Inspection{}, err
	}
	for _, ep := range a.order {
		rt := a.mgmt[ep.cfg.ID].inspect
		if rt == nil {
			continue
		}
		id := adapters.IdentityFrom(scope)
		body, err := a.document(ctx, rt, callArgs{identity: &id, aspects: req.Aspects})
		if err != nil {
			return adapters.Inspection{}, err
		}
		return adapters.NormaliseInspection(body, req, int(a.cfg.MaxResponseBytes))
	}
	return adapters.NormaliseInspection([]byte(`{"reports":[]}`), req, 0)
}

// ManageReply is the answer of a provisioning operation.
type ManageReply struct {
	// Status is the HTTP status, one the document describes for the operation.
	Status int
	// Body is the reply's JSON object, validated against the document.
	Body map[string]any
}

// Manage calls one of the provisioning operations the adapter configuration of
// an endpoint names (management.provisioning). They are the operations of the
// integration audit's disposable tenants (fixtures.provisioning: create_tenant,
// seed, cleanup); they are approved but not mapped to any capability, so the
// adapter contract cannot reach them and a model never can. This method is for a
// bridge a composition root builds for the audit.
//
// args are the members of the call: those whose names the operation declares as
// parameters go there, the rest are members of the JSON request body, which may
// nest. No identity is sent: a provisioning operation acts on the tenants its
// arguments name. The request and the reply are checked against the document like
// any other, and the call goes through the same egress gate and credential. A
// status the document describes comes back as a ManageReply whatever it is (a
// 404 for a tenant that is already gone is the caller's to interpret); a status it
// does not describe, an unusable reply and a failure to reach the application are
// errors. Nothing is retried.
func (a *Adapter) Manage(ctx context.Context, endpointID, operationID string, args map[string]any) (ManageReply, error) {
	mr := a.mgmt[endpointID]
	if mr == nil || mr.provisioning[operationID] == nil {
		return ManageReply{}, typed(contracts.ErrUnknownCapability, "the adapter configuration names no such provisioning operation", ReasonOperationNotMapped)
	}
	rt := mr.provisioning[operationID]
	canon := make(map[string]any, len(args))
	for k, v := range args {
		c, err := canonicalValue(v)
		if err != nil {
			return ManageReply{}, contracts.New(contracts.ErrValidationFailed, "adapter request: an argument cannot be encoded as JSON")
		}
		canon[k] = c
	}
	w, rp, err := a.exchange(ctx, rt, callArgs{fields: canon}, false)
	if err != nil {
		return ManageReply{}, err
	}
	body, err := a.checkedBody(ctx, rt, w, rp)
	if err != nil {
		return ManageReply{}, err
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return ManageReply{}, typed(contracts.ErrSchemaInvalid, "the reply is not a JSON object", ReasonResponseViolatesSpec)
	}
	return ManageReply{Status: rp.status, Body: obj}, nil
}
