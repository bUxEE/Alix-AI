package adaptersuite

import (
	"context"
	"encoding/json"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// describe calls Describe under the watchdog. The error is the adapter's, or
// the watchdog's when the adapter ignored its context.
func describe(ctx context.Context, s Subject) (adapters.Description, error) {
	r, werr := bounded(ctx, "describe", func() (adapters.Description, error) { return s.Adapter.Describe(ctx) })
	if werr != nil {
		return adapters.Description{}, werr
	}
	return r.v, r.err
}

func negotiate(ctx context.Context, s Subject) (adapters.Negotiated, error) {
	r, werr := bounded(ctx, "negotiate", func() (adapters.Negotiated, error) { return adapters.Negotiate(ctx, s.Adapter) })
	if werr != nil {
		return adapters.Negotiated{}, werr
	}
	return r.v, r.err
}

func checkDescribe(ctx context.Context, s Subject) error {
	d1, err := describe(ctx, s)
	if err != nil {
		return failf("Describe failed: %v", err)
	}
	if err := d1.Validate(); err != nil {
		return failf("Describe returned an invalid description: %v", err)
	}
	found := false
	for _, v := range d1.ContractVersions {
		found = found || v == adapters.ContractVersion
	}
	if !found {
		return failf("Describe does not list contract version %d: %v", adapters.ContractVersion, d1.ContractVersions)
	}
	if !d1.Supports(fake.ReadCapability, adapters.OpRead) {
		return failf("Describe does not list %s for %s", adapters.OpRead, fake.ReadCapability)
	}
	for _, op := range []adapters.Operation{adapters.OpValidate, adapters.OpPrepare, adapters.OpCommit, adapters.OpStatus, adapters.OpCancel} {
		if !d1.Supports(fake.ActionCapability, op) {
			return failf("Describe does not list %s for %s", op, fake.ActionCapability)
		}
	}
	d2, err := describe(ctx, s)
	if err != nil {
		return failf("second Describe failed: %v", err)
	}
	if diff := cmp.Diff(d1, d2); diff != "" {
		return failf("Describe is not stable between calls (-first +second):\n%s", diff)
	}
	if n := s.Fixture.Effects(); n != 0 {
		return failf("Describe changed the application: %d effects", n)
	}
	return nil
}

func checkVersionNegotiation(ctx context.Context, s Subject) error {
	fx := s.Fixture
	defer fx.SetContractVersions(adapters.ContractVersion)

	steps := []struct {
		name     string
		versions []int
		want     int // 0 = must be refused
	}{
		{"the host speaks only the core's version", []int{adapters.ContractVersion}, adapters.ContractVersion},
		{"the host also speaks a newer version", []int{adapters.ContractVersion, adapters.ContractVersion + 1}, adapters.ContractVersion},
		{"the host lists versions out of order", []int{adapters.ContractVersion + 2, adapters.ContractVersion, adapters.ContractVersion + 1}, adapters.ContractVersion},
		{"the host speaks only newer majors", []int{adapters.ContractVersion + 1, adapters.ContractVersion + 2}, 0},
		{"the host speaks only one newer major", []int{adapters.ContractVersion + 1}, 0},
		{"the host is back on the core's version", []int{adapters.ContractVersion}, adapters.ContractVersion},
	}
	for _, st := range steps {
		fx.SetContractVersions(st.versions...)
		n, err := negotiate(ctx, s)
		if st.want == 0 {
			if err == nil {
				return failf("%s: negotiation succeeded with version %d; a host with no shared version must be refused", st.name, n.Version)
			}
			if !contracts.HasCode(err, contracts.ErrValidationFailed) || reasonIn(err) != "version_incompatible" {
				return failf("%s: refused with %v, want validation_failed (version_incompatible)", st.name, err)
			}
			continue
		}
		if err != nil {
			return failf("%s: negotiation failed: %v", st.name, err)
		}
		if n.Version != st.want {
			return failf("%s: negotiated version %d, want %d", st.name, n.Version, st.want)
		}
	}
	return nil
}

func checkDescribeBackwardsCompatibility(ctx context.Context, s Subject) error {
	fx := s.Fixture
	defer fx.SetContractVersions(adapters.ContractVersion)

	// 1. The version 1 members are all there, under their frozen names.
	d, err := describe(ctx, s)
	if err != nil {
		return failf("Describe failed: %v", err)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return failf("Description does not encode: %v", err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	for _, k := range []string{"transport", "contract_versions", "capabilities"} {
		if _, ok := members[k]; !ok {
			return failf("the Description lost its version 1 member %q", k)
		}
	}

	// 2. A host built against a newer revision (unknown members, an extra
	// contract version) is still describable and still negotiates to 1.
	fx.Inject(adapters.OpDescribe, fake.FaultNewerDescribe)
	nd, err := describe(ctx, s)
	if err != nil {
		return failf("a newer host's Describe was refused (%v); unknown optional members must be ignored", err)
	}
	if !nd.Supports(fake.ReadCapability, adapters.OpRead) || !nd.Supports(fake.ActionCapability, adapters.OpCommit) {
		return failf("a newer host's description lost capabilities: %+v", nd.Capabilities)
	}
	fx.Inject(adapters.OpDescribe, fake.FaultNewerDescribe)
	n, err := negotiate(ctx, s)
	if err != nil || n.Version != adapters.ContractVersion {
		return failf("a newer host did not negotiate down to version %d: version %d, %v", adapters.ContractVersion, n.Version, err)
	}

	// 3. An older host that only knows version 1 is fine.
	fx.SetContractVersions(1)
	if _, err := negotiate(ctx, s); err != nil {
		return failf("a version 1 host was refused: %v", err)
	}
	return nil
}

func checkTypedResults(ctx context.Context, s Subject) error {
	sc := scopeA(s)
	want := func(what string, o outcome, op adapters.Operation, capID string) error {
		if o.err != nil {
			return failf("%s: unexpected error: %v", what, o.err)
		}
		if string(o.res.Operation) != string(op) || string(o.res.CapabilityID) != capID {
			return failf("%s: result is for %s %s, want %s %s", what, o.res.Operation, o.res.CapabilityID, op, capID)
		}
		return nil
	}

	// A refusal by the host is a result, not an error.
	o, err := readItem(ctx, s, sc, "item_does_not_exist")
	if err != nil {
		return err
	}
	if err := want("read of an unknown item", o, adapters.OpRead, fake.ReadCapability); err != nil {
		return err
	}
	if o.res.Status != actions.DeterministicResultStatusRejected || issueCode(o.res) != "validation_failed" || issueField(o.res) != "item_id" {
		return failf("read of an unknown item: status %s, issue %s on %q; want rejected, validation_failed on item_id", o.res.Status, issueCode(o.res), issueField(o.res))
	}

	o, err = validateName(ctx, s, sc, map[string]any{"name": "Widget"})
	if err != nil {
		return err
	}
	if err := want("validate", o, adapters.OpValidate, fake.ActionCapability); err != nil {
		return err
	}
	if o.res.Status != actions.DeterministicResultStatusOk || o.res.Freshness == nil || o.res.Preparation != nil {
		return failf("validate of a good name: status %s, freshness %v, preparation %v; want ok with freshness and no preparation", o.res.Status, o.res.Freshness, o.res.Preparation)
	}
	o, err = validateName(ctx, s, sc, map[string]any{"name": ""})
	if err != nil {
		return err
	}
	if o.err != nil || o.res.Status != actions.DeterministicResultStatusRejected || issueCode(o.res) != "validation_failed" || issueField(o.res) != "name" {
		return failf("validate of an empty name: %+v, %v; want rejected, validation_failed on name", o.res.Errors, o.err)
	}
	o, err = validateName(ctx, s, sc, map[string]any{})
	if err != nil {
		return err
	}
	if o.err != nil || o.res.Status != actions.DeterministicResultStatusRejected || issueCode(o.res) != "missing_fields" {
		return failf("validate with no name: %+v, %v; want rejected, missing_fields", o.res.Errors, o.err)
	}

	po, err := prepareName(ctx, s, sc, "Widget")
	if err != nil {
		return err
	}
	if err := want("prepare", po, adapters.OpPrepare, fake.ActionCapability); err != nil {
		return err
	}
	pr := po.res.Preparation
	if po.res.Status != actions.DeterministicResultStatusOk || pr == nil || len(pr.Effects) != 1 || len(pr.DataVersions) == 0 || pr.Summary == "" {
		return failf("prepare: status %s, preparation %+v; want ok with one effect, a data version and a summary", po.res.Status, pr)
	}
	if pr.Effects[0].Operation != actions.EffectOperationCreate || string(pr.Effects[0].EntityType) != fake.ItemEntity {
		return failf("prepare: effect %+v, want a create of %s", pr.Effects[0], fake.ItemEntity)
	}
	p := preparedFrom(po.res)

	co, err := commit(ctx, s, sc, p)
	if err != nil {
		return err
	}
	if err := want("commit", co, adapters.OpCommit, fake.ActionCapability); err != nil {
		return err
	}
	if co.res.Status != actions.DeterministicResultStatusOk || len(co.res.Identifiers) != 1 || co.res.Freshness == nil {
		return failf("commit: status %s, identifiers %d; want ok with the created record", co.res.Status, len(co.res.Identifiers))
	}
	id := string(co.res.Identifiers[0].EntityID)

	so, err := status(ctx, s, sc, p.IdempotencyKey)
	if err != nil {
		return err
	}
	if err := want("status", so, adapters.OpStatus, fake.ActionCapability); err != nil {
		return err
	}
	if so.res.Status != actions.DeterministicResultStatusOk || len(so.res.Identifiers) != 1 || so.res.Identifiers[0] != co.res.Identifiers[0] {
		return failf("status of a committed key: status %s, identifiers %+v; want ok with the commit's record", so.res.Status, so.res.Identifiers)
	}

	ro, err := readItem(ctx, s, sc, id)
	if err != nil {
		return err
	}
	if err := want("read", ro, adapters.OpRead, fake.ReadCapability); err != nil {
		return err
	}
	if ro.res.Status != actions.DeterministicResultStatusOk || len(ro.res.Identifiers) != 1 || len(ro.res.Values) == 0 {
		return failf("read of the created item: status %s, %d identifiers, %d values", ro.res.Status, len(ro.res.Identifiers), len(ro.res.Values))
	}

	xo, err := cancelKey(ctx, s, sc, p.IdempotencyKey)
	if err != nil {
		return err
	}
	if err := want("cancel", xo, adapters.OpCancel, fake.ActionCapability); err != nil {
		return err
	}
	if xo.res.Status != actions.DeterministicResultStatusRejected || issueCode(xo.res) != "conflict" {
		return failf("cancel of an applied action: status %s, issue %s; want rejected, conflict (an applied effect is not reversed)", xo.res.Status, issueCode(xo.res))
	}

	if n := s.Fixture.Effects(); n != 1 {
		return failf("the host applied %d effects for one commit", n)
	}
	return nil
}

// step is one scoped call of the sequence that scope_propagation replays.
type step struct {
	op         adapters.Operation
	capability string
	run        func(contracts.Scope) (outcome, error)
}

func checkScopePropagation(ctx context.Context, s Subject) error {
	scopes := []contracts.Scope{scopeA(s), scopeB(s)}
	// Make the two scopes differ in every level the host sees.
	scopes[1].Roles = []string{"auditor"}

	var seen []adapters.Identity
	for _, sc := range scopes {
		var p actions.PreparedAction
		steps := []step{
			{adapters.OpValidate, fake.ActionCapability, func(sc contracts.Scope) (outcome, error) {
				return validateName(ctx, s, sc, map[string]any{"name": "Widget"})
			}},
			{adapters.OpPrepare, fake.ActionCapability, func(sc contracts.Scope) (outcome, error) {
				o, err := prepareName(ctx, s, sc, "Widget")
				if err == nil && o.err == nil && o.res.Preparation != nil {
					p = preparedFrom(o.res)
				}
				return o, err
			}},
			{adapters.OpCommit, fake.ActionCapability, func(sc contracts.Scope) (outcome, error) { return commit(ctx, s, sc, p) }},
			{adapters.OpStatus, fake.ActionCapability, func(sc contracts.Scope) (outcome, error) { return status(ctx, s, sc, p.IdempotencyKey) }},
			{adapters.OpCancel, fake.ActionCapability, func(sc contracts.Scope) (outcome, error) { return cancelKey(ctx, s, sc, p.IdempotencyKey) }},
			{adapters.OpRead, fake.ReadCapability, func(sc contracts.Scope) (outcome, error) { return readItem(ctx, s, sc, "item_1") }},
		}
		for _, st := range steps {
			before := callCount(s)
			o, err := st.run(sc)
			if err != nil {
				return err
			}
			if o.err != nil {
				return failf("%s for principal %s failed: %v", st.op, sc.PrincipalID, o.err)
			}
			calls := newCalls(s, before)
			if len(calls) != 1 {
				return failf("%s sent %d requests to the host, want exactly 1 (the adapter must not retry, probe or fan out)", st.op, len(calls))
			}
			c := calls[0]
			if c.Operation != st.op || c.CapabilityID != st.capability {
				return failf("%s reached the host as %s for %q", st.op, c.Operation, c.CapabilityID)
			}
			if st.op == adapters.OpCommit || st.op == adapters.OpStatus || st.op == adapters.OpCancel {
				if c.IdempotencyKey != string(p.IdempotencyKey) {
					return failf("%s reached the host with idempotency key %q, want %q", st.op, c.IdempotencyKey, p.IdempotencyKey)
				}
			}
			if diff := cmp.Diff(adapters.IdentityFrom(sc), c.Identity); diff != "" {
				return failf("%s: the host received a different identity than the call carried (-want +got):\n%s", st.op, diff)
			}
			seen = append(seen, c.Identity)
		}

		// Inspect carries the identity too.
		before := callCount(s)
		if _, ierr := inspect(ctx, s, sc, adapters.InspectRequest{}); ierr != nil {
			return failf("inspect failed: %v", ierr)
		}
		calls := newCalls(s, before)
		if len(calls) != 1 || calls[0].Operation != adapters.OpInspect {
			return failf("inspect sent %d requests to the host", len(calls))
		}
		if diff := cmp.Diff(adapters.IdentityFrom(sc), calls[0].Identity); diff != "" {
			return failf("inspect: the host received a different identity (-want +got):\n%s", diff)
		}
	}
	// The second scope's requests must not carry the first scope's identity.
	half := len(seen) / 2
	for i := 0; i < half; i++ {
		if cmp.Equal(seen[i], seen[half+i]) {
			return failf("two principals were presented to the host as the same identity: %+v", seen[i])
		}
	}
	return nil
}

func checkScopeFailClosed(ctx context.Context, s Subject) error {
	good := scopeA(s)
	p, err := prepared(ctx, s, good, "Widget")
	if err != nil {
		return err
	}

	mut := func(f func(*contracts.Scope)) contracts.Scope {
		sc := good.Clone()
		f(&sc)
		return sc
	}
	bad := []struct {
		name  string
		scope contracts.Scope
	}{
		{"the zero scope", contracts.Scope{}},
		{"no tenant", mut(func(sc *contracts.Scope) { sc.TenantID = "" })},
		{"no workspace", mut(func(sc *contracts.Scope) { sc.WorkspaceID = "" })},
		{"no principal", mut(func(sc *contracts.Scope) { sc.PrincipalID = "" })},
		{"no roles", mut(func(sc *contracts.Scope) { sc.Roles = nil })},
		{"no session", mut(func(sc *contracts.Scope) { sc.SessionID = "" })},
		{"a wildcard tenant", mut(func(sc *contracts.Scope) { sc.TenantID = "*" })},
		{"a padded workspace", mut(func(sc *contracts.Scope) { sc.WorkspaceID = " wks_a " })},
		{"another application", mut(func(sc *contracts.Scope) { sc.ApplicationID = "app_somebody_else" })},
		{"a capability the token does not permit", mut(func(sc *contracts.Scope) { sc.CapabilityScopes = []string{"unrelated.capability"} })},
	}
	calls := map[string]func(sc contracts.Scope) (outcome, error){
		"read": func(sc contracts.Scope) (outcome, error) { return readItem(ctx, s, sc, "item_1") },
		"validate": func(sc contracts.Scope) (outcome, error) {
			return validateName(ctx, s, sc, map[string]any{"name": "x"})
		},
		"prepare": func(sc contracts.Scope) (outcome, error) { return prepareName(ctx, s, sc, "x") },
		"commit":  func(sc contracts.Scope) (outcome, error) { return commit(ctx, s, sc, p) },
		"status":  func(sc contracts.Scope) (outcome, error) { return status(ctx, s, sc, p.IdempotencyKey) },
		"cancel":  func(sc contracts.Scope) (outcome, error) { return cancelKey(ctx, s, sc, p.IdempotencyKey) },
	}
	for _, b := range bad {
		for name, call := range calls {
			before := callCount(s)
			o, err := call(b.scope)
			if err != nil {
				return err
			}
			if o.err == nil {
				return failf("%s with %s was accepted (status %s); it must be refused", name, b.name, o.res.Status)
			}
			if c := code(o.err); c != contracts.ErrScopeMissing && c != contracts.ErrScopeViolation {
				return failf("%s with %s: code %s (%v), want scope_missing or scope_violation", name, b.name, c, o.err)
			}
			if !zeroResult(o.res) {
				return failf("%s with %s returned a result alongside the error", name, b.name)
			}
			if n := len(newCalls(s, before)); n != 0 {
				return failf("%s with %s reached the host (%d requests); nothing may be sent without a valid scope", name, b.name, n)
			}
		}
		before := callCount(s)
		_, ierr := inspect(ctx, s, b.scope, adapters.InspectRequest{})
		// Inspect needs no capability, so only the scope levels matter.
		if b.name == "a capability the token does not permit" {
			continue
		}
		if ierr == nil || (code(ierr) != contracts.ErrScopeMissing && code(ierr) != contracts.ErrScopeViolation) {
			return failf("inspect with %s: %v, want scope_missing or scope_violation", b.name, ierr)
		}
		if n := len(newCalls(s, before)); n != 0 {
			return failf("inspect with %s reached the host", b.name)
		}
	}
	return nil
}

func checkTenantIsolation(ctx context.Context, s Subject) error {
	a, b := scopeA(s), scopeB(s)
	sameTenantOtherWorkspace := scopeFor(s, tenantA, "wks_other", principalA)
	id, _, err := committed(ctx, s, a, "Alice's widget")
	if err != nil {
		return err
	}
	o, err := readItem(ctx, s, a, id)
	if err != nil {
		return err
	}
	if o.err != nil || o.res.Status != actions.DeterministicResultStatusOk || len(o.res.Identifiers) != 1 {
		return failf("the owner cannot read its own item: status %s, %v", o.res.Status, o.err)
	}
	for _, other := range []struct {
		name  string
		scope contracts.Scope
	}{{"another tenant", b}, {"another workspace of the same tenant", sameTenantOtherWorkspace}} {
		o, err := readItem(ctx, s, other.scope, id)
		if err != nil {
			return err
		}
		if o.err != nil {
			return failf("a read from %s failed with an error (%v); the host's answer is a rejected result", other.name, o.err)
		}
		if o.res.Status == actions.DeterministicResultStatusOk || len(o.res.Identifiers) != 0 || len(o.res.Values) != 0 {
			return failf("a read from %s saw the item (status %s, %d identifiers): the identity context did not reach the host intact", other.name, o.res.Status, len(o.res.Identifiers))
		}
	}
	return nil
}
