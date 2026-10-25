package policyaudit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/policy/policyaudit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	pkgApp    = "demo-library"
	pkgTenant = "tenant-a"
	pkgWs     = "ws-1"
)

var pkgT0 = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)

type auditHarness struct {
	t   *testing.T
	db  *store.DB
	log *audit.Log
	clk *clock.Fake
}

func newAuditHarness(t *testing.T) *auditHarness {
	t.Helper()
	clk := clock.NewFake(pkgT0)
	db := storetest.Open(t, nil)
	l, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	return &auditHarness{t: t, db: db, log: l, clk: clk}
}

type storedEvent struct{ typ, data string }

// events returns the stored events of one chain in order.
func (h *auditHarness) events(application, tenant string) []storedEvent {
	h.t.Helper()
	rows, err := h.db.QueryContext(context.Background(),
		`SELECT event_type, data FROM audit_events WHERE application_id = ? AND tenant_id = ? ORDER BY chain_seq`, application, tenant)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []storedEvent
	for rows.Next() {
		var e storedEvent
		if err := rows.Scan(&e.typ, &e.data); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func (h *auditHarness) verify(application, tenant string) {
	h.t.Helper()
	if rep, err := h.log.VerifyChain(context.Background(), audit.ChainKey{ApplicationID: application, TenantID: tenant}, audit.VerifyOptions{}); err != nil {
		h.t.Fatalf("VerifyChain: %v\n%+v", err, rep.Problems)
	}
}

func libraryPackage(t *testing.T) (*policy.Package, contracts.Scope) {
	t.Helper()
	caps := []policy.CapabilityDecl{
		{ID: "catalog.find_title", Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead, Confirmation: policy.ConfirmNone,
			Fields: []policy.FieldDecl{{Name: "query", Required: true}}},
		{ID: "reservations.create", Audience: policy.AudienceUser, EffectClass: policy.EffectExternal, RiskLevel: contracts.RiskConsequential, Confirmation: policy.ConfirmExplicit,
			Fields: []policy.FieldDecl{{Name: "title", Required: true}}},
		{ID: "admin.reindex", Audience: policy.AudienceAdmin, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead, Confirmation: policy.ConfirmNone},
	}
	hp, err := policy.ParseHostPolicy([]byte(`{"policy_id":"default-access","role_grants":[{"role":"member","capabilities":["catalog.find_title","reservations.create"]}],"risk_overrides":[],"field_projection":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := policy.NewPackage(policy.PackageSpec{ApplicationID: pkgApp, Version: "1.0.0", Status: policy.StatusActive, Roles: []string{"member"}, Capabilities: caps, Policies: []*policy.HostPolicy{hp}})
	if err != nil {
		t.Fatal(err)
	}
	scope := contracts.Scope{
		DeploymentID: "dep-1", ApplicationID: pkgApp, TenantID: pkgTenant, WorkspaceID: pkgWs, PrincipalID: "user-1",
		Roles: []string{"member"}, CapabilityScopes: pkg.CapabilityIDs(), SessionID: "ses-1",
	}
	return pkg, scope
}

func expose(g *policy.Gateway, pkg policy.ActivePackage, s contracts.Scope) policy.Exposure {
	return g.Expose(context.Background(), policy.ExposeRequest{Scope: s, Package: pkg, Binding: policy.BindingOf(s)})
}

func TestEventExposureIsRegistered(t *testing.T) {
	spec, ok := audit.DefaultRegistry.Lookup("policy.exposure")
	if !ok || policyaudit.EventExposure.Name() != "policy.exposure" {
		t.Fatalf("policy.exposure is not registered: %+v", spec)
	}
	if spec.Levels != audit.LevelTenant|audit.LevelApplication || spec.Validate == nil || len(spec.Required) == 0 {
		t.Errorf("spec = %+v", spec)
	}
	if _, ok := audit.DefaultRegistry.Lookup(audit.EventPolicyDecision); !ok {
		t.Error("policy.decision is no longer a core type")
	}
}

func TestSinkStoresEachKindOfRecordUnderItsOwnEventType(t *testing.T) {
	h := newAuditHarness(t)
	pkg, scope := libraryPackage(t)
	g := policy.NewGateway(h.clk, policy.WithSink(policyaudit.NewSink(h.log, nil)))
	ctx := context.Background()

	e := expose(g, pkg, scope)
	if len(e.Visible) != 2 || e.Code != "" {
		t.Fatalf("exposure = %+v", e)
	}
	d := g.Decide(ctx, policy.Request{Scope: scope, Package: pkg, CapabilityID: "reservations.create", Binding: policy.BindingOf(scope), Phase: policy.PhasePrepare, Fields: map[string]any{"title": "SENTINEL-VALUE"}})
	if !d.Allowed {
		t.Fatalf("prepare denied: %+v", d.Reasons)
	}
	expose(g, pkg.WithStatus(policy.StatusQuarantined), scope)

	evs := h.events(pkgApp, pkgTenant)
	var types []string
	for _, ev := range evs {
		types = append(types, ev.typ)
	}
	want := []string{"policy.exposure", "policy.decision", "policy.decision"}
	if diff := cmp.Diff(want, types); diff != "" {
		t.Fatalf("event types (-want +got):\n%s", diff)
	}
	for _, ev := range evs {
		if strings.Contains(ev.data, audit.Redacted) || strings.Contains(ev.data, "SENTINEL-VALUE") {
			t.Errorf("a record was censored or leaked a value:\n%s", ev.data)
		}
	}

	// The stored exposure reads back as the record the gateway wrote: every
	// declared capability, in one event.
	var back policy.Record
	if err := json.Unmarshal([]byte(evs[0].data), &back); err != nil {
		t.Fatal(err)
	}
	if !back.IsExposure() || len(back.Exposure.Outcomes) != 3 || back.Exposure.Considered != 3 || back.Exposure.Parts != 1 {
		t.Fatalf("stored exposure = %+v", back.Exposure)
	}
	for _, v := range e.Visible {
		if v.Record.DecisionID != back.DecisionID {
			t.Errorf("%s: decision ID %s, stored record %s", v.Record.CapabilityID, v.Record.DecisionID, back.DecisionID)
		}
	}
	if !strings.Contains(evs[0].data, `"capability_id":"admin.reindex"`) || !strings.Contains(evs[0].data, `"reason":"admin_capability_not_model_visible"`) {
		t.Errorf("the denial of the admin capability is not in the stored event:\n%s", evs[0].data)
	}
	h.verify(pkgApp, pkgTenant)
}

// The wiring every composition root has today stores all records under
// policy.decision. The exposure record must go through it unchanged: this is
// what makes the gain land without touching internal/cli.
func TestSingleTypeWiringStillStoresTheExposureRecord(t *testing.T) {
	h := newAuditHarness(t)
	pkg, scope := libraryPackage(t)
	sink := audit.NewRecordSink[policy.Record](h.log, audit.EventPolicyDecision, policyaudit.SubjectLocator)
	g := policy.NewGateway(h.clk, policy.WithSink(sink))

	if e := expose(g, pkg, scope); len(e.Visible) != 2 || e.Code != "" {
		t.Fatalf("exposure = %+v", e)
	}
	evs := h.events(pkgApp, pkgTenant)
	if len(evs) != 1 || evs[0].typ != "policy.decision" {
		t.Fatalf("events = %+v, want one policy.decision event for one Expose of three capabilities", evs)
	}
	if !strings.Contains(evs[0].data, `"format":"alixai.policy-exposure/v1"`) || !strings.Contains(evs[0].data, `"outcomes":[`) {
		t.Errorf("the stored event is not the exposure record:\n%s", evs[0].data)
	}
	h.verify(pkgApp, pkgTenant)
}

// The audit log refuses event data above 32 KiB. The largest catalogue a package
// may declare, with the longest IDs and a principal with the most roles, must be
// stored, in as many events as it takes, never refused (which would deny every
// Expose of that application).
func TestWorstCaseExposureIsStoredInBoundedEvents(t *testing.T) {
	h := newAuditHarness(t)
	longID := func(i int) string {
		seg := func(c string) string { return c + strings.Repeat("x", 30) }
		return seg("a") + "." + seg("b") + "." + seg("c") + "." + "d" + strings.Repeat("x", 27) + fmt.Sprintf("%03d", i)
	}
	var caps []policy.CapabilityDecl
	var ids, quoted []string
	for i := 0; i < policy.MaxExposureOutcomes; i++ {
		id := longID(i)
		caps = append(caps, policy.CapabilityDecl{ID: id, Audience: policy.AudienceAdmin, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead, Confirmation: policy.ConfirmNone})
		ids = append(ids, id)
		quoted = append(quoted, `"`+id+`"`)
	}
	hp, err := policy.ParseHostPolicy([]byte(`{"policy_id":"p","role_grants":[{"role":"member","capabilities":[` + strings.Join(quoted, ",") + `]}],"risk_overrides":[],"field_projection":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := policy.NewPackage(policy.PackageSpec{ApplicationID: pkgApp, Version: "1.0.0", Status: policy.StatusActive, Roles: []string{"member"}, Capabilities: caps, Policies: []*policy.HostPolicy{hp}})
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for i := 0; i < 64; i++ {
		roles = append(roles, fmt.Sprintf("r%02d", i)+strings.Repeat("y", 125))
	}
	scope := contracts.Scope{DeploymentID: "dep-1", ApplicationID: pkgApp, TenantID: pkgTenant, WorkspaceID: pkgWs, PrincipalID: "user-1", Roles: roles, CapabilityScopes: ids, SessionID: "ses-1"}

	g := policy.NewGateway(h.clk, policy.WithSink(policyaudit.NewSink(h.log, nil)))
	e := expose(g, pkg, scope)
	if e.Code != "" || e.Considered != policy.MaxExposureOutcomes {
		t.Fatalf("exposure = code %q, considered %d: the audit log refused a record", e.Code, e.Considered)
	}
	evs := h.events(pkgApp, pkgTenant)
	listed := 0
	for i, ev := range evs {
		if ev.typ != "policy.exposure" {
			t.Fatalf("event %d is %s", i, ev.typ)
		}
		if len(ev.data) > audit.DefaultMaxDataBytes {
			t.Errorf("event %d is %d bytes, over the audit limit %d", i, len(ev.data), audit.DefaultMaxDataBytes)
		}
		var r policy.Record
		if err := json.Unmarshal([]byte(ev.data), &r); err != nil {
			t.Fatal(err)
		}
		listed += len(r.Exposure.Outcomes)
		if r.Exposure.Part != i+1 || r.Exposure.Parts != len(evs) {
			t.Errorf("event %d is part %d of %d, want %d of %d", i, r.Exposure.Part, r.Exposure.Parts, i+1, len(evs))
		}
	}
	t.Logf("%d capabilities, 64 roles of 128 characters, IDs of %d characters: %d events, the largest %d bytes", len(ids), len(ids[0]), len(evs), maxLen(evs))
	if listed != policy.MaxExposureOutcomes {
		t.Errorf("the events list %d capabilities, want %d", listed, policy.MaxExposureOutcomes)
	}
	h.verify(pkgApp, pkgTenant)
}

func maxLen(evs []storedEvent) int {
	n := 0
	for _, e := range evs {
		if len(e.data) > n {
			n = len(e.data)
		}
	}
	return n
}

// SEC-087 through the real log: an append that fails leaves nothing exposed.
func TestExposureThatCannotBeAppendedExposesNothing(t *testing.T) {
	h := newAuditHarness(t)
	pkg, scope := libraryPackage(t)
	g := policy.NewGateway(h.clk, policy.WithSink(policyaudit.NewSink(failingAppender{h.log}, nil)))
	e := expose(g, pkg, scope)
	if len(e.Visible) != 0 || e.Code != contracts.ErrUnavailable {
		t.Fatalf("exposure = %+v", e)
	}
	if n := len(h.events(pkgApp, pkgTenant)); n != 0 {
		t.Errorf("%d events stored by a failing appender", n)
	}
}

type failingAppender struct{ audit.Appender }

func (failingAppender) AppendAt(context.Context, audit.Locator, audit.NewEvent) (audit.Event, error) {
	return audit.Event{}, contracts.New(contracts.ErrUnavailable, "disk full")
}

// The registered type validates what it stores, so the log does not have to trust
// its writer to have been the gateway.
func TestRegisteredTypeRefusesAMalformedExposure(t *testing.T) {
	h := newAuditHarness(t)
	loc, err := audit.ForIDs(pkgApp, pkgTenant, pkgWs, "user-1", "ses-1")
	if err != nil {
		t.Fatal(err)
	}
	good := map[string]any{
		"format": policy.ExposureFormat, "decision_id": "pdec_01M3XZ5YE0VF1A3VM6K82CEN2A", "phase": "expose", "outcome": "allow",
		"considered": 1, "part": 1, "parts": 1,
		"outcomes": []any{map[string]any{"capability_id": "catalog.find_title", "outcome": "allow", "risk": 0}},
	}
	mutate := func(f func(m map[string]any)) map[string]any {
		m := map[string]any{}
		for k, v := range good {
			m[k] = v
		}
		f(m)
		return m
	}
	many := make([]any, policy.MaxExposureOutcomes+1)
	for i := range many {
		many[i] = map[string]any{"capability_id": "a.b", "outcome": "allow", "risk": 0}
	}
	if _, err := policyaudit.EventExposure.Append(context.Background(), h.log, loc, policy.Record{}); err == nil {
		t.Error("an empty record was appended")
	}
	for name, data := range map[string]map[string]any{
		"another format":        mutate(func(m map[string]any) { m["format"] = policy.RecordFormat }),
		"not the expose phase":  mutate(func(m map[string]any) { m["phase"] = "commit" }),
		"outcomes missing":      mutate(func(m map[string]any) { delete(m, "outcomes") }),
		"outcomes not a list":   mutate(func(m map[string]any) { m["outcomes"] = "all" }),
		"too many outcomes":     mutate(func(m map[string]any) { m["outcomes"] = many }),
		"an outcome without id": mutate(func(m map[string]any) { m["outcomes"] = []any{map[string]any{"outcome": "allow"}} }),
		"an outcome that is yes": mutate(func(m map[string]any) {
			m["outcomes"] = []any{map[string]any{"capability_id": "a.b", "outcome": "yes"}}
		}),
	} {
		if _, err := h.log.AppendAt(context.Background(), loc, audit.NewEvent{Type: policyaudit.EventExposure.Name(), Data: data}); err == nil {
			t.Errorf("%s: the log accepted it", name)
		}
	}
	if _, err := h.log.AppendAt(context.Background(), loc, audit.NewEvent{Type: policyaudit.EventExposure.Name(), Data: good}); err != nil {
		t.Errorf("a well-formed exposure was refused: %v", err)
	}
	if n := len(h.events(pkgApp, pkgTenant)); n != 1 {
		t.Errorf("%d events stored, want only the well-formed one", n)
	}
}
