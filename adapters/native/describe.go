package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// maxHostMethods bounds how many method names of a bridge the adapter reads.
const maxHostMethods = 4096

// safeName renders a method name a bridge reported so that it is safe to print:
// a valid name as it is, anything else by its length and a hash prefix.
func safeName(s string) string {
	if methodRE.MatchString(s) {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("invalid-name:%d:%s", len(s), hex.EncodeToString(sum[:4]))
}

// Describe implements adapters.AppAdapter.
//
// The description is the adapter's own finding, not the application's claim. The
// contract versions and the version label come from the bridge's describe
// operation; the operations of each capability are those the package maps;
// Unmapped is every method the bridge declares that no capability maps and the
// adapter configuration does not approve, plus what the host itself says it
// exposes that the adapter does not know. None of those is callable. Nothing the
// host says about its own capabilities is believed, so a host cannot make the
// adapter list an operation the package does not map.
func (a *Adapter) Describe(ctx context.Context) (adapters.Description, error) {
	var (
		versions     []int
		haveVersions bool
		appVersion   string
		unmapped     = map[string]bool{}
	)
	for _, ep := range a.order {
		rep, err := a.exchange(ctx, ep, methodDescribe, map[string]any{}, callOpts{})
		if err != nil {
			return adapters.Description{}, err
		}
		host, err := adapters.ParseDescription(Transport, rep.result)
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
		var extra struct {
			Methods []string `json:"methods"`
		}
		_ = json.Unmarshal(rep.result, &extra) // validated above; this member is the bridge's own
		if len(extra.Methods) > maxHostMethods {
			extra.Methods = extra.Methods[:maxHostMethods]
		}
		for _, m := range extra.Methods {
			if !ep.mapped[m] {
				unmapped[safeName(m)] = true
			}
		}
		for _, u := range host.Unmapped {
			unmapped[safeName(u)] = true
		}
	}
	if len(versions) == 0 {
		return adapters.Description{}, contracts.New(contracts.ErrValidationFailed, "the application's bridges share no contract version").
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
		if slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// Health implements adapters.AppAdapter: the health operation of every bridge, the
// worst state being the application's. A bridge that cannot be reached is an
// unavailable error, which the caller reads as down.
func (a *Adapter) Health(ctx context.Context) (adapters.HealthReport, error) {
	out := adapters.HealthReport{Status: adapters.HealthOK}
	rank := map[adapters.Health]int{adapters.HealthOK: 0, adapters.HealthDegraded: 1, adapters.HealthUnavailable: 2}
	for _, ep := range a.order {
		rep, err := a.exchange(ctx, ep, methodHealth, map[string]any{}, callOpts{})
		if err != nil {
			return adapters.HealthReport{}, err
		}
		h, err := adapters.ParseHealth(rep.result)
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

// Inspect implements adapters.AppAdapter: the inspect operation of the first
// bridge that has one. A bridge without one exposes nothing for inspection, and
// every aspect is then unsupported, which the audit records as unverified.
func (a *Adapter) Inspect(ctx context.Context, scope contracts.Scope, req adapters.InspectRequest) (adapters.Inspection, error) {
	if err := req.Validate(); err != nil {
		return adapters.Inspection{}, err
	}
	for _, ep := range a.order {
		params := map[string]any{"identity": adapters.IdentityFrom(scope)}
		if len(req.Aspects) > 0 {
			params["aspects"] = req.Aspects
		}
		rep, err := a.exchange(ctx, ep, methodInspect, params, callOpts{})
		if err != nil {
			if reasonOf(err) == ReasonOperationNotOffered {
				continue
			}
			return adapters.Inspection{}, err
		}
		return adapters.NormaliseInspection(rep.result, req, int(a.cfg.MaxResponseBytes))
	}
	return adapters.NormaliseInspection([]byte(`{"reports":[]}`), req, 0)
}

// Manage calls one of the provisioning operations the adapter configuration of an
// endpoint approves (management.provisioning). They are the operations of the
// integration audit's disposable tenants (fixtures.provisioning: create_tenant,
// seed, cleanup); they are approved but not mapped to any capability, so the
// adapter contract cannot reach them and a model never can. This method is for a
// bridge a composition root builds for the audit.
//
// No identity is sent: a provisioning operation acts on the tenants its arguments
// name. The result must be a JSON object. Nothing is retried.
func (a *Adapter) Manage(ctx context.Context, endpointID, operation string, args map[string]any) (map[string]any, error) {
	ep := a.eps[endpointID]
	if ep == nil || !slices.Contains(ep.cfg.Provisioning, operation) {
		return nil, typed(contracts.ErrUnknownCapability, "the adapter configuration approves no such provisioning operation", ReasonOperationNotMapped)
	}
	if args == nil {
		args = map[string]any{}
	}
	rep, err := a.exchange(ctx, ep, methodManage, map[string]any{"name": operation, "args": args}, callOpts{})
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(rep.result, &obj); err != nil || obj == nil {
		return nil, typed(contracts.ErrSchemaInvalid, "the reply is not a JSON object", ReasonProtocol)
	}
	return obj, nil
}
