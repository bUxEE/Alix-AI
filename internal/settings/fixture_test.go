package settings

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The synthetic application the tests run against.
const (
	tApp     = "app_demo"
	tOther   = "app_other"
	tTenantA = "ten_acme"
	tTenantB = "ten_globex"
	tWsp1    = "wsp_main"
	tWsp2    = "wsp_second"
	tUser1   = "usr_ada"
	tUser2   = "usr_bob"

	capGet    = "items.get"
	capCreate = "items.create"
	capDelete = "items.delete"
	capAdmin  = "admin.purge"
	capGhost  = "items.ghost" // never declared by the package
)

var tNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// settingsScopes is every capability scope of the settings API.
var settingsScopes = []string{
	CapabilityUserRead, CapabilityUserWrite, CapabilityTenantRead, CapabilityTenantWrite,
	CapabilityDeploymentRead, CapabilityDeploymentWrite,
}

// tPackage is an in-memory active package that also states its languages.
type tPackage struct {
	*policy.Package
	langs []string
}

func (p tPackage) SupportedLanguages() []string { return p.langs }

// buildPackage builds the demo package: a read capability, a level-2 capability
// (explicit confirmation), a level-3 one (step-up) and an admin-only one, with the
// host policy granting the member role the first three.
func buildPackage(t testing.TB, appID string) tPackage {
	t.Helper()
	decls := []policy.CapabilityDecl{
		{ID: capGet, Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead, Confirmation: policy.ConfirmNone,
			Fields: []policy.FieldDecl{{Name: "item_id", Required: true}}},
		{ID: capCreate, Audience: policy.AudienceUser, EffectClass: policy.EffectExternal, RiskLevel: contracts.RiskConsequential, Confirmation: policy.ConfirmExplicit,
			Fields: []policy.FieldDecl{{Name: "name", Required: true}, {Name: "quantity", Required: true}}},
		{ID: capDelete, Audience: policy.AudienceUser, EffectClass: policy.EffectPrivileged, RiskLevel: contracts.RiskPrivileged, Confirmation: policy.ConfirmStepUp,
			Fields: []policy.FieldDecl{{Name: "item_id", Required: true}}},
		{ID: capAdmin, Audience: policy.AudienceAdmin, EffectClass: policy.EffectPrivileged, RiskLevel: contracts.RiskPrivileged, Confirmation: policy.ConfirmStepUp},
	}
	hp, err := policy.ParseHostPolicy([]byte(`{
  "policy_id": "default-access",
  "role_grants": [
    { "role": "member", "capabilities": ["items.get", "items.create", "items.delete"] },
    { "role": "visitor", "capabilities": ["items.get"] }
  ],
  "risk_overrides": [],
  "field_projection": []
}`))
	if err != nil {
		t.Fatal(err)
	}
	pp, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: appID, Version: "1.0.0", Status: policy.StatusActive,
		Roles: []string{"member", "visitor", "staff"}, Capabilities: decls, Policies: []*policy.HostPolicy{hp},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tPackage{Package: pp, langs: []string{"en", "it"}}
}

// fakePackages serves one active package per application, or quarantined.
type fakePackages struct {
	mu          sync.Mutex
	pkgs        map[string]Package
	quarantined map[string]bool
	calls       int
}

func (f *fakePackages) Package(_ context.Context, applicationID string) (Package, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	p, ok := f.pkgs[applicationID]
	if !ok || f.quarantined[applicationID] {
		return nil, contracts.New(contracts.ErrQuarantined, "the application has no active package")
	}
	return p, nil
}

func (f *fakePackages) quarantine(app string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quarantined[app] = on
}

// recordingAudit wraps the real log: it counts appends and can be made to fail,
// every append or (failType) the appends of one event type.
type recordingAudit struct {
	audit.Appender
	mu       sync.Mutex
	n        int
	fail     error
	failType string
	events   []audit.NewEvent
}

func (r *recordingAudit) AppendTx(ctx context.Context, tx *store.Tx, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	r.mu.Lock()
	if r.fail != nil && (r.failType == "" || r.failType == string(ev.Type)) {
		err := r.fail
		r.mu.Unlock()
		return audit.Event{}, err
	}
	r.n++
	r.events = append(r.events, ev)
	r.mu.Unlock()
	return r.Appender.AppendTx(ctx, tx, loc, ev)
}

func (r *recordingAudit) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// fixture is a Service over the real store, audit log and policy gateway.
type fixture struct {
	t        *testing.T
	db       *store.DB
	clk      *clock.Fake
	log      *audit.Log
	rec      *recordingAudit
	gw       *policy.Gateway
	pkg      tPackage
	packages *fakePackages
	svc      *Service
	findings *fakeFindings
	provs    *fakeProviders
}

func newFixture(t *testing.T, mutate ...func(*Deps)) *fixture {
	t.Helper()
	f := &fixture{t: t, clk: clock.NewFake(tNow)}
	f.db = storetest.Open(t, nil)
	var err error
	if f.log, err = audit.New(f.db, audit.Options{Clock: f.clk}); err != nil {
		t.Fatal(err)
	}
	f.rec = &recordingAudit{Appender: f.log}
	sink := audit.NewRecordSink[policy.Record](f.log, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	f.gw = policy.NewGateway(f.clk, policy.WithSink(sink))
	f.pkg = buildPackage(t, tApp)
	f.packages = &fakePackages{pkgs: map[string]Package{tApp: f.pkg, tOther: buildPackage(t, tOther)}, quarantined: map[string]bool{}}
	f.findings = &fakeFindings{byApp: map[string]AuditStatus{}}
	f.provs = &fakeProviders{}
	d := Deps{
		DB: f.db, Clock: f.clk, Audit: f.rec, Policy: f.gw, Packages: f.packages,
		Limits: Limits{
			ApprovedProfiles: []string{"lite", "balanced"}, DefaultProfile: "lite", ApprovedVoices: []string{"it_female_1"},
			MaxHistoryRetentionDays: 365, DefaultHistoryRetentionDays: 90, MaxAttachmentMB: 25,
		},
		Findings: f.findings, Providers: f.provs,
	}
	for _, m := range mutate {
		m(&d)
	}
	if f.svc, err = New(d); err != nil {
		t.Fatal(err)
	}
	return f
}

// scope returns a valid scope: member of tenant, workspace and application given,
// holding the settings scopes and the user capabilities.
func scopeOf(app, tenant, workspace, principal string, caps ...string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_test", ApplicationID: app, TenantID: tenant, WorkspaceID: workspace, PrincipalID: principal,
		Roles: []string{"member"}, CapabilityScopes: caps, SessionID: "ses_" + principal,
	}
}

// ada is a full-permission member of tenant A.
func ada() contracts.Scope {
	return scopeOf(tApp, tTenantA, tWsp1, tUser1, append(append([]string{}, settingsScopes...), capGet, capCreate, capDelete)...)
}

// withScopes returns sc with exactly these capability scopes.
func withScopes(sc contracts.Scope, caps ...string) contracts.Scope {
	sc = sc.Clone()
	sc.CapabilityScopes = caps
	return sc
}

// put writes a document and fails the test on an error.
func (f *fixture) put(sc contracts.Scope, level Level, expected int64, doc string) Record {
	f.t.Helper()
	rec, err := f.svc.Put(context.Background(), sc, level, PutRequest{ExpectedVersion: expected, Settings: json.RawMessage(doc)})
	if err != nil {
		f.t.Fatalf("Put(%s, %s): %v", level, doc, err)
	}
	return rec
}

// get reads a document and fails the test on an error.
func (f *fixture) get(sc contracts.Scope, level Level) Record {
	f.t.Helper()
	rec, err := f.svc.Get(context.Background(), sc, level)
	if err != nil {
		f.t.Fatalf("Get(%s): %v", level, err)
	}
	return rec
}

// tryPut writes and returns the error.
func (f *fixture) tryPut(sc contracts.Scope, level Level, expected int64, doc string) error {
	_, err := f.svc.Put(context.Background(), sc, level, PutRequest{ExpectedVersion: expected, Settings: json.RawMessage(doc)})
	return err
}

// auditEvents returns the settings events of the tenant chain of the scope.
func (f *fixture) settingsEvents() []audit.Event {
	f.t.Helper()
	return f.eventsOfType(string(EventDocumentWritten.Name()))
}

// eventsOfType exports every event of the given type from all chains the tests use.
func (f *fixture) eventsOfType(typ string) []audit.Event {
	f.t.Helper()
	var out []audit.Event
	rows, err := f.db.QueryContext(context.Background(),
		"SELECT id, application_id, tenant_id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, data, prev_hash, hash FROM audit_events WHERE event_type = ? ORDER BY application_id, tenant_id, chain_seq", typ)
	if err != nil {
		f.t.Fatalf("reading the audit events: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e audit.Event
		var at int64
		var data string
		if err := rows.Scan(&e.ID, &e.ApplicationID, &e.TenantID, &e.WorkspaceID, &e.ChainSeq, &e.Type, &at, &e.PrincipalID, &e.SessionID, &data, &e.PrevHash, &e.Hash); err != nil {
			f.t.Fatal(err)
		}
		e.OccurredAt, e.Data = time.UnixMilli(at).UTC(), json.RawMessage(data)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// code returns the contracts code of err, or "" for nil.
func code(err error) contracts.ErrCode {
	if err == nil {
		return ""
	}
	return contracts.CodeOf(err)
}

// wantCode fails unless err carries the code.
func wantCode(t testing.TB, err error, want contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", want)
	}
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Code != want {
		t.Fatalf("error = %v (code %s), want %s", err, code(err), want)
	}
}

// fakeFindings is the integration-audit source of the tests.
type fakeFindings struct {
	byApp map[string]AuditStatus
	err   error
	asked []string
}

func (f *fakeFindings) IntegrationAudit(_ context.Context, app string) (AuditStatus, error) {
	f.asked = append(f.asked, app)
	if f.err != nil {
		return AuditStatus{}, f.err
	}
	if st, ok := f.byApp[app]; ok {
		return st, nil
	}
	return AuditStatus{ApplicationID: app, State: "not_installed"}, nil
}

// fakeProviders is the provider-health source of the tests.
type fakeProviders struct {
	health ProviderHealth
	err    error
}

func (f *fakeProviders) ProviderHealth(context.Context) (ProviderHealth, error) {
	return f.health, f.err
}

// contains reports whether s contains every needle.
func containsAll(s string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(s, n) {
			return false
		}
	}
	return true
}

// asErr is errors.As for the typed error.
func asErr(err error, target **contracts.Error) bool { return errors.As(err, target) }

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }
