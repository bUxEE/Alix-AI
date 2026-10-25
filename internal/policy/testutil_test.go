package policy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var t0 = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)

// Capability IDs of the synthetic demo-library package used by the tests.
const (
	capFind     = "catalog.find_title"
	capReserve  = "reservations.create"
	capExtend   = "loans.extend"
	capDelete   = "members.delete_account"
	capReindex  = "admin.reindex"
	tenantA     = "tenant-a"
	tenantB     = "tenant-b"
	workspace1  = "ws-1"
	workspace2  = "ws-2"
	appID       = "demo-library"
	sentinelVal = "SENTINEL-FIELD-VALUE-7731"
)

// scopeWith returns a valid scope for member@tenant-a/ws-1 holding a token for
// the given capabilities, adjusted by mods.
func scopeWith(capScopes []string, mods ...func(*contracts.Scope)) contracts.Scope {
	s := contracts.Scope{
		DeploymentID:     "dep-1",
		ApplicationID:    appID,
		TenantID:         tenantA,
		WorkspaceID:      workspace1,
		PrincipalID:      "user-1",
		Roles:            []string{"member"},
		CapabilityScopes: capScopes,
		SessionID:        "ses-1",
	}
	for _, m := range mods {
		m(&s)
	}
	return s
}

func allUserCaps() []string { return []string{capFind, capReserve, capExtend, capDelete} }

func withRoles(r ...string) func(*contracts.Scope) {
	return func(s *contracts.Scope) { s.Roles = r }
}

const demoPolicyJSON = `{
  "policy_id": "default-access",
  "role_grants": [
    { "role": "member",  "capabilities": ["catalog.find_title", "reservations.create", "loans.extend"] },
    { "role": "visitor", "capabilities": ["catalog.find_title"] },
    { "role": "staff",   "capabilities": ["catalog.find_title", "reservations.create", "loans.extend", "members.delete_account", "admin.reindex"] }
  ],
  "risk_overrides": [],
  "field_projection": []
}`

func demoCaps() []CapabilityDecl {
	return []CapabilityDecl{
		{ID: capFind, Audience: AudienceUser, EffectClass: EffectNone, RiskLevel: 0, Confirmation: ConfirmNone,
			Fields: []FieldDecl{{Name: "query", Required: true}, {Name: "available_only"}}},
		{ID: capReserve, Audience: AudienceUser, EffectClass: EffectExternal, RiskLevel: 2, Confirmation: ConfirmExplicit,
			Fields: []FieldDecl{{Name: "title", Required: true}, {Name: "pickup_date", Required: true}, {Name: "notes", Sensitive: true}}},
		{ID: capExtend, Audience: AudienceUser, EffectClass: EffectReversible, RiskLevel: 1, Confirmation: ConfirmNone,
			Fields: []FieldDecl{{Name: "loan", Required: true}, {Name: "weeks"}}},
		{ID: capDelete, Audience: AudienceUser, EffectClass: EffectPrivileged, RiskLevel: 3, Confirmation: ConfirmStepUp,
			Fields: []FieldDecl{{Name: "member", Required: true}}},
		{ID: capReindex, Audience: AudienceAdmin, EffectClass: EffectNone, RiskLevel: 0, Confirmation: ConfirmNone},
	}
}

func demoRoles() []string { return []string{"member", "visitor", "staff", "junior_member"} }

func mustParsePolicy(t testing.TB, doc string) *HostPolicy {
	t.Helper()
	h, err := ParseHostPolicy([]byte(doc))
	if err != nil {
		t.Fatalf("ParseHostPolicy: %v\n%s", err, doc)
	}
	return h
}

// demoPackage builds an active demo-library package with the given policies
// (default: the demo policy).
func demoPackage(t testing.TB, docs ...string) *Package {
	t.Helper()
	if len(docs) == 0 {
		docs = []string{demoPolicyJSON}
	}
	var pols []*HostPolicy
	for _, d := range docs {
		pols = append(pols, mustParsePolicy(t, d))
	}
	p, err := NewPackage(PackageSpec{
		ApplicationID: appID,
		Version:       "1.0.0",
		Status:        StatusActive,
		Roles:         demoRoles(),
		Capabilities:  demoCaps(),
		Policies:      pols,
	})
	if err != nil {
		t.Fatalf("NewPackage: %v", err)
	}
	return p
}

func newGateway(opts ...Option) (*Gateway, *clock.Fake) {
	clk := clock.NewFake(t0)
	return NewGateway(clk, opts...), clk
}

// recorder is a Sink that keeps every record it is given.
type recorder struct {
	mu   sync.Mutex
	recs []Record
	fail bool
}

func (r *recorder) Emit(_ context.Context, rec Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return context.DeadlineExceeded
	}
	r.recs = append(r.recs, rec)
	return nil
}

func (r *recorder) all() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Record(nil), r.recs...)
}

// decide is Decide for the standard request shape.
func decide(g *Gateway, pkg ActivePackage, scope contracts.Scope, capID string, phase Phase, mods ...func(*Request)) Decision {
	req := Request{
		Scope:        scope,
		Package:      pkg,
		CapabilityID: capID,
		Binding:      BindingOf(scope),
		Phase:        phase,
	}
	for _, m := range mods {
		if m != nil {
			m(&req)
		}
	}
	return g.Decide(context.Background(), req)
}

func visibleIDs(e Exposure) []string {
	ids := []string{}
	for _, d := range e.Visible {
		ids = append(ids, d.Record.CapabilityID)
	}
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- schema helpers ----

var schemaRoot = filepath.Join("..", "..", "schemas")

// schemaCompiler registers every schema under schemas/{common,application-package,policy}.
func schemaCompiler(t testing.TB) *jsonschema.Compiler {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	for _, area := range []string{"common", "application-package", "policy"} {
		files, err := filepath.Glob(filepath.Join(schemaRoot, area, "*.schema.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no schemas in %s (%v)", area, err)
		}
		for _, f := range files {
			raw, err := os.Open(f)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := jsonschema.UnmarshalJSON(raw)
			raw.Close()
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			id := doc.(map[string]any)["$id"].(string)
			if err := c.AddResource(id, doc); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
		}
	}
	return c
}

func compileSchema(t testing.TB, id string) *jsonschema.Schema {
	t.Helper()
	s, err := schemaCompiler(t).Compile(id)
	if err != nil {
		t.Fatalf("compile %s: %v", id, err)
	}
	return s
}

func readJSONFile(t testing.TB, path string) (raw []byte, instance any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return raw, inst
}

func instanceOf(t testing.TB, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return inst
}
