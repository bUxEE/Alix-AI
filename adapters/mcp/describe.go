package mcp

import (
	"context"
	"errors"
	"sort"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// notOffered reports whether err says that the server does not list the tool.
func notOffered(err error) bool {
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce == nil {
		return false
	}
	r, _ := ce.Details["reason"].(string)
	return ce.Code == contracts.ErrUnknownCapability && r == ReasonOperationNotOffered
}

// relist lists an endpoint's tools now and checks them against the pins.
func (a *Adapter) relist(ctx context.Context, ep *endpointConn) error {
	s, err := ep.acquire(ctx)
	if err != nil {
		return err
	}
	defer s.release()
	return ep.refresh(ctx, s, true)
}

// knownTool reports whether any endpoint maps, manages or approves the tool.
func (a *Adapter) knownTool(name string) bool {
	for _, e := range a.eps {
		if e.mapped[name] || e.managed[name] || e.approved[name] {
			return true
		}
	}
	return false
}

// Describe implements adapters.AppAdapter.
//
// The description is the adapter's own finding, not the application's claim. The
// contract versions and the version label come from the server's describe tool
// (when it has one); the operations of each capability are those the package
// maps whose tools the server lists now, unchanged since they were approved;
// Unmapped is what the server lists that no capability maps and the adapter
// configuration does not approve. An operation whose tool is gone or has drifted
// is left out, which the integration audit reports as unreachable. Nothing the
// application says about its own capabilities is believed, so an application
// cannot make the adapter list an operation the package does not map.
func (a *Adapter) Describe(ctx context.Context) (adapters.Description, error) {
	var (
		versions     []int
		haveVersions bool
		appVersion   string
		hostUnmapped = map[string]bool{}
		unmapped     = map[string]bool{}
	)
	for _, ep := range a.eps {
		if err := a.relist(ctx, ep); err != nil {
			return adapters.Description{}, err
		}
		rep, err := a.call(ctx, &route{ep: ep, tool: a.cfg.DescribeTool}, map[string]any{}, false)
		switch {
		case err == nil:
			host, perr := adapters.ParseDescription(Transport, rep.structured)
			if perr != nil {
				return adapters.Description{}, perr
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
				hostUnmapped[u] = true
			}
		case notOffered(err):
			// A server without a describe tool states no versions; the handshake
			// version label is what it has.
			if appVersion == "" {
				appVersion = ep.applicationVersion()
			}
		default:
			return adapters.Description{}, err
		}
		for _, n := range ep.unmappedNames() {
			unmapped[n] = true
		}
	}
	for u := range hostUnmapped {
		if !a.knownTool(u) {
			unmapped[u] = true
		}
	}
	if !haveVersions {
		versions = []int{adapters.ContractVersion}
	}
	if len(versions) == 0 {
		return adapters.Description{}, contracts.New(contracts.ErrValidationFailed, "the application's servers share no contract version").
			WithDetails("reason", "version_incompatible")
	}

	ids := make([]string, 0, len(a.caps))
	for id := range a.caps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	d := adapters.Description{Transport: Transport, ContractVersions: versions, ApplicationVersion: appVersion, Capabilities: []adapters.CapabilityOps{}}
	for _, id := range ids {
		var ops []adapters.Operation
		for _, op := range adapters.ResultOperations() {
			if rt := a.caps[id].ops[op]; rt != nil {
				if st, _ := rt.ep.state(rt.tool); st == toolOK {
					ops = append(ops, op)
				}
			}
		}
		if len(ops) > 0 {
			d.Capabilities = append(d.Capabilities, adapters.CapabilityOps{CapabilityID: id, Operations: ops})
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

// Health implements adapters.AppAdapter: the server's health tool, or a ping when
// it has none. With several servers the worst state is the application's.
func (a *Adapter) Health(ctx context.Context) (adapters.HealthReport, error) {
	out := adapters.HealthReport{Status: adapters.HealthOK}
	rank := map[adapters.Health]int{adapters.HealthOK: 0, adapters.HealthDegraded: 1, adapters.HealthUnavailable: 2}
	for _, ep := range a.eps {
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

func (a *Adapter) endpointHealth(ctx context.Context, ep *endpointConn) (adapters.HealthReport, error) {
	rep, err := a.call(ctx, &route{ep: ep, tool: a.cfg.HealthTool}, map[string]any{}, false)
	switch {
	case err == nil:
		return adapters.ParseHealth(rep.structured)
	case !notOffered(err):
		return adapters.HealthReport{}, err
	}
	// No health tool: the protocol's own ping says whether the server answers.
	s, err := ep.acquire(ctx)
	if err != nil {
		return adapters.HealthReport{}, err
	}
	defer s.release()
	bound, done := s.bind(ctx)
	defer done()
	if err := s.cs.Ping(bound, nil); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return adapters.HealthReport{}, cerr
		}
		if !isRPC(err) {
			ep.discard(s)
		}
		return adapters.HealthReport{}, a.unavailable(ctx, err, "the application did not answer a ping")
	}
	return adapters.HealthReport{Status: adapters.HealthOK, ApplicationVersion: ep.applicationVersion()}, nil
}

// Inspect implements adapters.AppAdapter: the inspect tool of the first server
// that has one. A server without one exposes nothing for inspection, and every
// aspect is then unsupported, which the audit records as unverified.
func (a *Adapter) Inspect(ctx context.Context, scope contracts.Scope, req adapters.InspectRequest) (adapters.Inspection, error) {
	if err := req.Validate(); err != nil {
		return adapters.Inspection{}, err
	}
	for _, ep := range a.eps {
		args := map[string]any{"identity": adapters.IdentityFrom(scope)}
		if len(req.Aspects) > 0 {
			args["aspects"] = req.Aspects
		}
		rep, err := a.call(ctx, &route{ep: ep, tool: a.cfg.InspectTool}, args, false)
		switch {
		case err == nil:
			return adapters.NormaliseInspection(rep.structured, req, int(a.cfg.MaxResponseBytes))
		case notOffered(err):
			continue
		default:
			return adapters.Inspection{}, err
		}
	}
	return adapters.NormaliseInspection([]byte(`{"reports":[]}`), req, 0)
}
