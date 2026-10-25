package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	openapitransport "github.com/buxee/alix-ai/adapters/openapi"
	openapifixture "github.com/buxee/alix-ai/adapters/openapi/testdata/fixture"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/integrationaudit/probes"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// The application these tests audit is the OpenAPI test fixture of adapters/openapi
// (fake.App behind HTTP, with the document spec.yaml describes) with the provisioning
// half of a real host put in front of it: the fixture's own provisioning routes only
// record tenant identifiers and have no seed operation, so a small loopback server
// answers /tenants itself, with the semantics the contract asks of an application
// (409 for a tenant that exists, 403 for a tenant it did not create, 404 for a tenant
// that is gone, an item in the fixture application for every seeded record), and
// forwards every other request to the fixture. The capability operations the probes
// call are therefore the fixture's own, through the adapter's own HTTP client.

const (
	openapiProvisionAppID  = "items-app"
	openapiProvisionEP     = "fixture-api"
	openapiProvisionSuffix = "0123456789"
	// openapiProvisionCanary is text an application chooses: it must never reach an
	// error, a log or a report.
	openapiProvisionCanary = "CANARY-TEXT-THE-SERVER-CHOSE"
)

// ---- the application ----------------------------------------------------------

type openapiProvisionSeen struct {
	Route  string
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// openapiProvisionRow is what the application knows of a disposable tenant.
type openapiProvisionRow struct {
	body   map[string]any
	seeded []map[string]any
	items  int
}

type openapiProvisionApp struct {
	host *openapifixture.Host
	URL  string
	Port int

	mu      sync.Mutex
	tenants map[string]*openapiProvisionRow
	seen    []openapiProvisionSeen
	nextID  int
	on      map[string]func(w http.ResponseWriter, r *http.Request, body []byte) bool
	// remaining is what a cleanup reports as left, by kind.
	remaining map[string]int64
	// loseCreateReply applies the next create and then drops the connection.
	loseCreateReply bool
}

func openapiProvisionStartApp(t testing.TB) *openapiProvisionApp {
	t.Helper()
	host := openapifixture.Start(t, fake.NewApp())
	target, err := url.Parse(host.URL)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &openapiProvisionApp{
		host: host, URL: "http://" + ln.Addr().String(), Port: ln.Addr().(*net.TCPAddr).Port,
		tenants: map[string]*openapiProvisionRow{}, on: map[string]func(http.ResponseWriter, *http.Request, []byte) bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tenants", a.wrap("create", a.create))
	mux.HandleFunc("POST /tenants/{tenant_id}/seed", a.wrap("seed", a.seed))
	mux.HandleFunc("DELETE /tenants/{tenant_id}", a.wrap("cleanup", a.cleanup))
	mux.HandleFunc("POST /tenants/{tenant_id}/step-ups", a.wrap("stepup", a.stepUp)) // step-up completion, see transport_stepup_test.go
	mux.Handle("/", httputil.NewSingleHostReverseProxy(target))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return a
}

func (a *openapiProvisionApp) wrap(route string, f func(http.ResponseWriter, *http.Request, []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		a.mu.Lock()
		a.seen = append(a.seen, openapiProvisionSeen{Route: route, Method: r.Method, Path: r.URL.EscapedPath(), Header: r.Header.Clone(), Body: body})
		over := a.on[route]
		a.mu.Unlock()
		if over != nil && over(w, r, body) {
			return
		}
		f(w, r, body)
	}
}

func openapiProvisionWrite(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func openapiProvisionRefuse(w http.ResponseWriter, status int, code string) {
	openapiProvisionWrite(w, status, map[string]any{
		"status": "rejected", "errors": []any{map[string]any{"code": code, "message": openapiProvisionCanary}},
	})
}

func openapiProvisionDrop(w http.ResponseWriter) {
	if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
		_ = conn.Close()
	}
}

func (a *openapiProvisionApp) create(w http.ResponseWriter, _ *http.Request, body []byte) {
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	id, _ := req["tenant_id"].(string)
	a.mu.Lock()
	if _, exists := a.tenants[id]; exists || id == "" {
		a.mu.Unlock()
		openapiProvisionRefuse(w, http.StatusConflict, "conflict")
		return
	}
	a.tenants[id] = &openapiProvisionRow{body: req}
	lose := a.loseCreateReply
	a.loseCreateReply = false
	a.mu.Unlock()
	if lose {
		openapiProvisionDrop(w)
		return
	}
	openapiProvisionWrite(w, http.StatusCreated, map[string]any{"tenant_id": id, "created": true})
}

func (a *openapiProvisionApp) seed(w http.ResponseWriter, r *http.Request, body []byte) {
	id := r.PathValue("tenant_id")
	a.mu.Lock()
	row := a.tenants[id]
	a.mu.Unlock()
	if row == nil {
		openapiProvisionRefuse(w, http.StatusForbidden, "permission_denied")
		return
	}
	var req struct {
		Records []map[string]any `json:"records"`
	}
	_ = json.Unmarshal(body, &req)
	out := make([]any, 0, len(req.Records))
	for _, rec := range req.Records {
		ref, _ := rec["ref"].(string)
		ws, _ := rec["workspace_id"].(string)
		name, _ := rec["name"].(string)
		if name == "" {
			name = "synthetic " + ref
		}
		a.mu.Lock()
		a.nextID++
		item := fmt.Sprintf("itm_%d", a.nextID)
		row.seeded = append(row.seeded, rec)
		row.items++
		a.mu.Unlock()
		a.host.App.Seed(id, ws, item, name)
		out = append(out, map[string]any{"ref": ref, "entity_id": item})
	}
	openapiProvisionWrite(w, http.StatusOK, map[string]any{"tenant_id": id, "records": out})
}

func (a *openapiProvisionApp) cleanup(w http.ResponseWriter, r *http.Request, _ []byte) {
	id := r.PathValue("tenant_id")
	a.mu.Lock()
	_, existed := a.tenants[id]
	remaining := a.remaining
	if existed && len(remaining) == 0 {
		delete(a.tenants, id)
	}
	a.mu.Unlock()
	if !existed {
		openapiProvisionWrite(w, http.StatusNotFound, map[string]any{"existed": false, "remaining": map[string]any{}})
		return
	}
	left := map[string]any{}
	for k, v := range remaining {
		left[k] = v
	}
	openapiProvisionWrite(w, http.StatusOK, map[string]any{"existed": true, "remaining": left})
}

// route installs a function that sees the requests of a provisioning route before the
// application does; when it returns true it has answered.
func (a *openapiProvisionApp) route(name string, f func(w http.ResponseWriter, r *http.Request, body []byte) bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.on[name] = f
}

// answer makes a route answer every request with a status and a body.
func (a *openapiProvisionApp) answer(route string, status int, body any) {
	a.route(route, func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		openapiProvisionWrite(w, status, body)
		return true
	})
}

func (a *openapiProvisionApp) requests(route string) []openapiProvisionSeen {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []openapiProvisionSeen
	for _, s := range a.seen {
		if route == "" || s.Route == route {
			out = append(out, s)
		}
	}
	return out
}

func (a *openapiProvisionApp) tenantIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.tenants))
	for id := range a.tenants {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// remove takes a tenant away behind the audit's back.
func (a *openapiProvisionApp) remove(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tenants, id)
}

func (a *openapiProvisionApp) row(id string) *openapiProvisionRow {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tenants[id]
}

// ---- the document, the adapter configuration and the package -------------------

// openapiProvisionPaths replace the fixture's two provisioning paths with the three
// operations of the contract and the statuses an application can answer with.
const openapiProvisionPaths = `  /tenants:
    post:
      operationId: createTenant
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/Open" }
      responses:
        "201": &open_reply
          description: A JSON object.
          content:
            application/json:
              schema: { $ref: "#/components/schemas/Open" }
        "400": *open_reply
        "403": *open_reply
        "409": *open_reply
        "422": *open_reply
        "429": *open_reply
        "500": *open_reply
        "503": *open_reply
  /tenants/{tenant_id}/seed:
    post:
      operationId: seedTenant
      parameters:
        - name: tenant_id
          in: path
          required: true
          schema: { type: string }
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/Open" }
      responses:
        "200": *open_reply
        "400": *open_reply
        "403": *open_reply
        "404": *open_reply
        "409": *open_reply
        "422": *open_reply
  /tenants/{tenant_id}:
    delete:
      operationId: cleanupTenant
      parameters:
        - name: tenant_id
          in: path
          required: true
          schema: { type: string }
      responses:
        "200": *open_reply
        "403": *open_reply
        "404": *open_reply
        "409": *open_reply
        "500": *open_reply
`

func openapiProvisionSpec(t testing.TB) string {
	t.Helper()
	spec := string(openapifixture.Spec)
	from := strings.Index(spec, "  /tenants:\n")
	to := strings.Index(spec, "\ncomponents:\n")
	if from < 0 || to < from {
		t.Fatal("the fixture's document no longer has the shape these tests edit")
	}
	return spec[:from] + openapiProvisionPaths + spec[to+1:]
}

func openapiProvisionAdapterConfig(provisioning string) string {
	return "server: " + openapiProvisionEP + "\nmanagement:\n  describe: describeApplication\n  health: getHealth\n  inspect: inspectApplication\n  provisioning: " + provisioning + "\n"
}

const openapiProvisionSeedData = `{"records": [
  {"ref": "item-a1", "entity_type": "item", "name": "Alpha one", "workspace": "ws-1", "principal": "member-a1", "tenant_id": "someone-else", "color": "red"},
  {"ref": "item-b1", "name": "Beta one"}
]}`

func openapiProvisionPtr[T any](v T) *T { return &v }

// openapiProvisionOpts vary the package a test builds.
type openapiProvisionOpts struct {
	// provisioning is the management.provisioning list of the adapter configuration.
	provisioning string
	// calls edits the provisioning calls of the suite.
	calls func(p *ap.PackageTestsProvisioning)
	// noSuites builds a package with no test suite.
	noSuites bool
	// baseURL overrides the endpoint's base URL (the application's front by default).
	baseURL string
}

type openapiProvisionWorld struct {
	app     *openapiProvisionApp
	pkg     *apppkg.Package
	dataDir string
	rec     apppkg.Record
	ad      adapters.AppAdapter
	in      *integrationaudit.Input
}

func openapiProvisionCall(op string) ap.AdapterCall {
	return ap.AdapterCall{Endpoint: openapiProvisionEP, OpenAPIOperationID: openapiProvisionPtr(ap.OperationName(op))}
}

func openapiProvisionBinding(op string) *ap.CapabilityOperationBinding {
	return &ap.CapabilityOperationBinding{Endpoint: openapiProvisionEP, OpenAPIOperationID: openapiProvisionPtr(ap.OperationName(op))}
}

func openapiProvisionFailures(codes ...string) []ap.CapabilityFailureState {
	out := make([]ap.CapabilityFailureState, 0, len(codes))
	for _, c := range codes {
		out = append(out, ap.CapabilityFailureState{Code: ap.ErrorCode(c), Description: "documented"})
	}
	return out
}

// openapiProvisionCapabilities are the two capabilities of the fixture application:
// items.get reads one item and items.create creates one (validate, prepare, commit, status).
func openapiProvisionCapabilities() map[string]ap.Capability {
	get := openapiProvisionBinding(openapifixture.OpGet)
	get.Arguments = map[string]ap.OperationName{"item": "id"}
	return map[string]ap.Capability{
		"items.get": {
			ID: "items.get", Audience: ap.CapabilityAudienceUser, ScopeLevel: ap.CapabilityScopeLevelWorkspace,
			Fields: []ap.CapabilityField{{
				Name: "item", Type: ap.CapabilityFieldTypeEntityRef, Required: true, EntityType: openapiProvisionPtr(ap.Slug("item")), Description: "the item",
			}},
			EffectClass: ap.CapabilityEffectClassNone, RiskLevel: 0,
			Operations:    ap.CapabilityOperations{Read: get},
			Confirmation:  ap.CapabilityConfirmation{Mode: ap.CapabilityConfirmationModeNone},
			FailureStates: openapiProvisionFailures("scope_violation", "permission_denied", "missing_fields", "validation_failed", "unavailable"),
		},
		"items.create": {
			ID: "items.create", Audience: ap.CapabilityAudienceUser, ScopeLevel: ap.CapabilityScopeLevelWorkspace,
			Fields: []ap.CapabilityField{{
				Name: "name", Type: ap.CapabilityFieldTypeString, Required: true, Description: "the name", MaxLength: openapiProvisionPtr(int64(64)),
			}},
			EffectClass: ap.CapabilityEffectClassReversible, RiskLevel: 1,
			Operations: ap.CapabilityOperations{
				Validate: openapiProvisionBinding(openapifixture.OpValidate), Prepare: openapiProvisionBinding(openapifixture.OpPrepare),
				Commit: openapiProvisionBinding(openapifixture.OpCommit), Status: openapiProvisionBinding(openapifixture.OpStatus),
			},
			Confirmation: ap.CapabilityConfirmation{Mode: ap.CapabilityConfirmationModeExplicit},
			PreparedAction: &ap.CapabilityPreparedAction{
				TTLSeconds: 300, InvalidatedBy: []ap.CapabilityPreparedActionInvalidatedByItem{ap.CapabilityPreparedActionInvalidatedByItemRecordRevision},
			},
			Idempotency: &ap.CapabilityIdempotency{
				KeySource: ap.CapabilityIdempotencyKeySourcePreparedActionID, WindowSeconds: 86400, OnReplay: ap.CapabilityIdempotencyOnReplayReturnOriginalReceipt,
			},
			Examples: []ap.CapabilityExample{{
				ID: "en-create", Language: "en", TestOnly: true, Utterance: "Create an item", ExpectedFields: ap.ExampleFields{"name": "A synthetic item"},
			}},
			FailureStates: openapiProvisionFailures("scope_violation", "permission_denied", "missing_fields", "validation_failed", "stale", "expired",
				"conflict", "unavailable", "uncertain_outcome"),
		},
	}
}

func openapiProvisionPrincipal(ref, tenant, workspace string) ap.PackageTestsPrincipal {
	var p ap.PackageTestsPrincipal
	doc := fmt.Sprintf(`{"ref":%q,"tenant":%q,"workspace":%q,"kind":"human","roles":["member"]}`, ref, tenant, workspace)
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		panic(err)
	}
	return p
}

// openapiProvisionSuite is the test suite of the fixture application: two tenants, one
// with two workspaces, a principal and an item in each, and the probes the loader
// requires (one that expects allow and one that expects deny, the seven safe-execution
// kinds, a text and an action path).
func openapiProvisionSuite(calls func(*ap.PackageTestsProvisioning)) ap.PackageTests {
	allow := func(id, capID, principal, target string) ap.PackageTestsScopeProbe {
		p := ap.PackageTestsScopeProbe{
			ID: ap.Slug(id), Dimension: ap.PackageTestsScopeProbeDimensionRecord, Operation: ap.PackageTestsScopeProbeOperationRead,
			CapabilityID: openapiProvisionPtr(ap.CapabilityID(capID)), Principal: ap.Slug(principal), Expect: ap.PackageTestsScopeProbeExpectAllow,
		}
		if target != "" {
			p.TargetRecord = openapiProvisionPtr(ap.Slug(target))
		}
		return p
	}
	deny := func(id string, dim ap.PackageTestsScopeProbeDimension, principal, target string) ap.PackageTestsScopeProbe {
		return ap.PackageTestsScopeProbe{
			ID: ap.Slug(id), Dimension: dim, Operation: ap.PackageTestsScopeProbeOperationRead, CapabilityID: openapiProvisionPtr(ap.CapabilityID("items.get")),
			Principal: ap.Slug(principal), TargetRecord: openapiProvisionPtr(ap.Slug(target)), Expect: ap.PackageTestsScopeProbeExpectDeny,
			ErrorCode: openapiProvisionPtr(ap.ErrorCode("validation_failed")),
		}
	}
	safety := func(id string, kind ap.PackageTestsSafetyProbeKind, out ap.PackageTestsSafetyExpectOutcome, code string) ap.PackageTestsSafetyProbe {
		p := ap.PackageTestsSafetyProbe{
			ID: ap.Slug(id), CapabilityID: "items.create", Principal: "member-a1", Kind: kind, Expect: ap.PackageTestsSafetyExpect{Outcome: out},
		}
		if code != "" {
			p.Expect.ErrorCode = openapiProvisionPtr(ap.ErrorCode(code))
		}
		return p
	}
	prov := ap.PackageTestsProvisioning{
		CreateTenant: openapiProvisionCall("createTenant"), Seed: openapiProvisionCall("seedTenant"), Cleanup: openapiProvisionCall("cleanupTenant"),
		SeedData: openapiProvisionPtr("tests/seed/items.json"),
	}
	if calls != nil {
		calls(&prov)
	}
	return ap.PackageTests{
		Suite: "items", Synthetic: true,
		Fixtures: ap.PackageTestsFixtures{
			Provisioning: prov,
			Tenants: []ap.PackageTestsFixturesTenantsItem{
				{Ref: "org-a", Workspaces: []ap.Slug{"ws-1", "ws-2"}},
				{Ref: "org-b", Workspaces: []ap.Slug{"ws-1"}},
			},
			Principals: []ap.PackageTestsPrincipal{
				openapiProvisionPrincipal("member-a1", "org-a", "ws-1"),
				openapiProvisionPrincipal("member-a2", "org-a", "ws-2"),
				openapiProvisionPrincipal("member-b1", "org-b", "ws-1"),
			},
			Records: []ap.PackageTestsRecord{
				{Ref: "item-a1", EntityType: "item", Tenant: "org-a", Workspace: "ws-1"},
				{Ref: "item-a2", EntityType: "item", Tenant: "org-a", Workspace: "ws-2"},
				{Ref: "item-b1", EntityType: "item", Tenant: "org-b", Workspace: "ws-1"},
			},
		},
		ScopeProbes: []ap.PackageTestsScopeProbe{
			allow("member-reads-own-item", "items.get", "member-a1", "item-a1"),
			deny("member-cannot-read-other-workspace", ap.PackageTestsScopeProbeDimensionWorkspace, "member-a1", "item-a2"),
			deny("member-cannot-read-other-tenant", ap.PackageTestsScopeProbeDimensionTenant, "member-a1", "item-b1"),
		},
		SafetyProbes: []ap.PackageTestsSafetyProbe{
			safety("create-confirmed", ap.PackageTestsSafetyProbeKindConfirmedCommit, ap.PackageTestsSafetyExpectOutcomeCommittedOnce, ""),
			safety("create-unconfirmed", ap.PackageTestsSafetyProbeKindUnconfirmedCommit, ap.PackageTestsSafetyExpectOutcomeRejectedNoEffect, "confirmation_required"),
			safety("create-replayed", ap.PackageTestsSafetyProbeKindReplayedCommit, ap.PackageTestsSafetyExpectOutcomeOriginalReceiptReturned, ""),
			safety("create-expired", ap.PackageTestsSafetyProbeKindExpiredPreparation, ap.PackageTestsSafetyExpectOutcomeRejectedNoEffect, "expired"),
			safety("create-stale", ap.PackageTestsSafetyProbeKindStaleRecord, ap.PackageTestsSafetyExpectOutcomeRejectedNoEffect, "stale"),
			safety("create-cancelled", ap.PackageTestsSafetyProbeKindCancelledPreparation, ap.PackageTestsSafetyExpectOutcomeRejectedNoEffect, "conflict"),
			safety("create-uncertain", ap.PackageTestsSafetyProbeKindUncertainCommitRecovery, ap.PackageTestsSafetyExpectOutcomeRecoveredViaStatus, ""),
		},
		Smoke: ap.PackageTestsSmoke{VerifyCleanup: true, Paths: []ap.PackageTestsSmokePath{
			{ID: "smoke-text", Path: ap.PackageTestsSmokePathPathText, Principal: "member-a1", CapabilityID: openapiProvisionPtr(ap.CapabilityID("items.get")),
				Utterance: "Show my item", Expect: ap.PackageTestsSmokePathExpect{AuditEvents: []ap.DottedID{"policy.decision"}}},
			{ID: "smoke-action", Path: ap.PackageTestsSmokePathPathAction, Principal: "member-a1", CapabilityID: openapiProvisionPtr(ap.CapabilityID("items.create")),
				Utterance: "Create an item", Expect: ap.PackageTestsSmokePathExpect{AuditEvents: []ap.DottedID{"action.prepared", "action.confirmed", "action.committed"}}},
		}},
	}
}

func openapiProvisionPolicy(t testing.TB) *policy.HostPolicy {
	t.Helper()
	hp, err := policy.ParseHostPolicy([]byte(`{
  "policy_id": "default-access",
  "role_grants": [ { "role": "member", "capabilities": ["items.get", "items.create"] } ],
  "risk_overrides": [],
  "field_projection": []
}`))
	if err != nil {
		t.Fatal(err)
	}
	return hp
}

// openapiProvisionNewWorld installs a package for the fixture application in a fresh
// directory and builds the audit's input for it: the adapter the transport registry
// builds (the wrapped OpenAPI adapter, exactly as audit run holds it) and what the runner
// asks of it first.
func openapiProvisionNewWorld(t testing.TB, o openapiProvisionOpts) *openapiProvisionWorld {
	t.Helper()
	app := openapiProvisionStartApp(t)
	baseURL := app.URL
	if o.baseURL != "" {
		baseURL = o.baseURL
	}

	m := ap.Manifest{}
	m.Application.ID = openapiProvisionAppID
	m.Application.Version = "1.0.0"
	m.Package.Version = "1.0.0"
	m.Tenancy.Roles = map[string]ap.ManifestTenancyRolesValue{"member": {}}
	m.Endpoints = map[string]ap.ManifestEndpoint{
		openapiProvisionEP: {Value: ap.ManifestOpenAPIEndpoint{
			Transport: "openapi", BaseURL: ap.HTTPURL(baseURL), Spec: "schemas/items-openapi.yaml", AdapterConfig: "schemas/adapters/items-openapi.yaml",
		}},
	}
	pkg := &apppkg.Package{Manifest: m, Capabilities: openapiProvisionCapabilities(), Policies: []*policy.HostPolicy{openapiProvisionPolicy(t)}}
	if !o.noSuites {
		pkg.Tests = []ap.PackageTests{openapiProvisionSuite(o.calls)}
	}

	provisioning := o.provisioning
	if provisioning == "" {
		provisioning = "[createTenant, seedTenant, cleanupTenant]"
	}
	spec := openapiProvisionSpec(t)
	if strings.Contains(provisioning, openapiProvisionApproveOp) {
		spec = transportStepUpWithApproval(t, spec)
	}
	dataDir := t.TempDir()
	dir := filepath.Join(dataDir, "package")
	for rel, content := range map[string]string{
		"schemas/items-openapi.yaml":          spec,
		"schemas/adapters/items-openapi.yaml": openapiProvisionAdapterConfig(provisioning),
		"tests/seed/items.json":               openapiProvisionSeedData,
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rec := apppkg.Record{ApplicationID: openapiProvisionAppID, ManifestHash: "h1", State: apppkg.StateQuarantined, Path: filepath.Base(dir)}
	ctx := context.Background()
	ad, _, err := transportsSource{dataDir: dataDir}.build(ctx, rec.ApplicationID, pkg, rec)
	if err != nil {
		t.Fatalf("the adapter of the package: %v", err)
	}
	t.Cleanup(func() { transportsClose(ad) })

	in := &integrationaudit.Input{
		Package: pkg, Record: rec, CoreVersion: apppkg.DevCoreVersion, Authenticity: apppkg.AuthVerified, Adapter: ad, Clock: clock.Real{},
		Scope: contracts.Scope{
			DeploymentID: "dep_audit", ApplicationID: rec.ApplicationID, TenantID: "alixai-audit", WorkspaceID: "alixai-audit",
			PrincipalID: "alixai-audit", Roles: []string{"auditor"}, CapabilityScopes: []string{"audit.inspect"}, SessionID: "audit-test",
		},
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if d, err := ad.Describe(cctx); err == nil {
		in.Description = &d
	}
	if h, err := ad.Health(cctx); err == nil {
		in.Health = &h
	}
	if i, err := ad.Inspect(cctx, in.Scope, adapters.InspectRequest{}); err == nil {
		in.Inspection = &i
	}
	return &openapiProvisionWorld{app: app, pkg: pkg, dataDir: dataDir, rec: rec, ad: ad, in: in}
}

func openapiProvisionCtx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// provisioner builds the provisioner of the world's audit, the way the probes ask for it.
func (w *openapiProvisionWorld) provisioner(t testing.TB) probes.Provisioner {
	t.Helper()
	p, err := openapiProvisionSource(openapiProvisionCtx(t), w.in)
	if err != nil {
		t.Fatalf("the provisioning source: %v", err)
	}
	if p == nil {
		t.Fatal("the provisioning source offers no provisioner")
	}
	return p
}

// openapiProvisionRun runs the checks of the given sections (the audit's runtime probes)
// with the options and returns their outcomes by section.
func openapiProvisionRun(t testing.TB, in *integrationaudit.Input, opts probes.Options, sections ...int) map[int]*integrationaudit.Outcome {
	t.Helper()
	if opts.ScratchDir == "" {
		opts.ScratchDir = t.TempDir()
	}
	want := map[int]bool{}
	for _, s := range sections {
		want[s] = true
	}
	out := map[int]*integrationaudit.Outcome{}
	for _, c := range probes.Checks(opts) {
		if !want[c.Section()] {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		o, err := c.Run(ctx, in)
		cancel()
		if err != nil {
			t.Fatalf("section %d (%s) did not run: %v", c.Section(), c.ID(), err)
		}
		out[c.Section()] = o
	}
	return out
}

func openapiProvisionDescribe(o *integrationaudit.Outcome) string {
	var b strings.Builder
	for _, v := range o.Verdicts {
		fmt.Fprintf(&b, "  verdict %s %s: %s\n", v.CapabilityID, v.Status, v.Reason)
	}
	for _, u := range o.Unverified {
		fmt.Fprintf(&b, "  unverified %s [%s]: %s\n", u.Subject, u.Capability, u.Reason)
	}
	for _, f := range o.Findings {
		fmt.Fprintf(&b, "  finding %s %s [%s]: %s\n", f.Severity, f.CheckID, f.Capability, f.Message)
	}
	for _, p := range o.Probes {
		fmt.Fprintf(&b, "  probe passed=%v %s %s: expected %s, observed %s\n", p.Passed, p.Capability, p.Subject, p.Expected, p.Observed)
	}
	covered := append([]string(nil), o.Covered...)
	sort.Strings(covered)
	fmt.Fprintf(&b, "  covered: %v\n", covered)
	return b.String()
}

func openapiProvisionReasons(o *integrationaudit.Outcome) []string {
	var out []string
	for _, v := range o.Verdicts {
		out = append(out, v.Reason)
	}
	for _, u := range o.Unverified {
		out = append(out, u.Reason)
	}
	for _, f := range o.Findings {
		out = append(out, f.Message)
	}
	return out
}

func openapiProvisionHas(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// openapiProvisionTenantOf is a tenant of the suite, with the identifiers the audit
// would generate (probes: tnt_<ref>_<suffix>, ws_<ref>_<suffix>, usr_<ref>_<suffix>,
// "-" as "_").
func openapiProvisionTenantOf(suffix string) probes.Tenant {
	ws := "ws_ws_1_" + suffix
	return probes.Tenant{
		ID:         "tnt_org_a_" + suffix,
		Workspaces: []string{ws, "ws_ws_2_" + suffix},
		Principals: []probes.Principal{
			{Ref: "member-a1", ID: "usr_member_a1_" + suffix, WorkspaceID: ws, Roles: []string{"member"}},
			{Ref: "member-a2", ID: "usr_member_a2_" + suffix, WorkspaceID: "ws_ws_2_" + suffix, Roles: []string{"member"}, Service: true},
		},
	}
}

func openapiProvisionSeedOf(suffix string) probes.Seed {
	ws := "ws_ws_1_" + suffix
	return probes.Seed{
		TenantID: "tnt_org_a_" + suffix, DataFile: "tests/seed/items.json",
		Records: []probes.Record{
			{Ref: "item-a1", EntityType: "item", WorkspaceID: ws},
			{Ref: "item-b1", EntityType: "item", WorkspaceID: ws},
		},
	}
}

// ---- 1. the audit's runtime probes run for real --------------------------------

// The audit's runtime probes (sections 6, 8 and 10) run against the OpenAPI test fixture
// through the provisioner this file registers: the disposable tenants of the package's
// suite are created, seeded and removed through the three provisioning operations, the
// probes call the capabilities through the adapter's own HTTP client, and no capability is
// recorded as not probed for want of a disposable tenant. The checks use the default
// source, so this also proves the init registration.
func TestOpenAPIProvisionerRunsFixturesAgainstTheFixtureApp(t *testing.T) {
	w := openapiProvisionNewWorld(t, openapiProvisionOpts{})

	outs := openapiProvisionRun(t, w.in, probes.Options{}, integrationaudit.SectionScope, integrationaudit.SectionSafeExecution, integrationaudit.SectionRuntimeSmoke)

	creates, seeds, cleanups := w.app.requests("create"), w.app.requests("seed"), w.app.requests("cleanup")
	if len(creates) == 0 || len(seeds) == 0 || len(cleanups) < len(creates) {
		t.Fatalf("the application was asked to create %d tenants, seed %d and clean up %d: a tenant was never provisioned or was left behind",
			len(creates), len(seeds), len(cleanups))
	}
	if left := w.app.tenantIDs(); len(left) != 0 {
		t.Errorf("the application still holds the disposable tenants %v", left)
	}
	for _, s := range w.app.requests("") {
		if s.Header.Get("X-AlixAI-Identity") != "" || s.Header.Get("Authorization") != "" {
			t.Errorf("a provisioning call carried an identity or a credential: %s %s %v", s.Method, s.Path, s.Header)
		}
		if !strings.HasPrefix(s.Path, "/tenants") {
			t.Errorf("a provisioning call went to %s", s.Path)
		}
	}
	// the probes reached the fixture application's capability operations for real
	if reads := len(w.app.host.Requests()); reads == 0 {
		t.Error("no capability operation reached the fixture application")
	}
	var commits int
	for _, c := range w.app.host.App.Calls() {
		if c.Operation == adapters.OpCommit {
			commits++
		}
	}
	if commits == 0 {
		t.Error("no commit reached the fixture application: the safe-execution probes did not run")
	}
	if len(w.app.host.App.Items("", "")) != 0 {
		t.Error("an item was created outside any tenant")
	}

	// what the audit could not probe must not be for want of a disposable tenant
	notProvisioning := []string{"provisioner", "creates a disposable tenant", "create_tenant for", "seed for fixture"}
	for s, o := range outs {
		for _, reason := range openapiProvisionReasons(o) {
			for _, bad := range notProvisioning {
				if strings.Contains(reason, bad) {
					t.Errorf("section %d still says the disposable tenant could not be reached: %q", s, reason)
				}
			}
		}
		for _, f := range o.Findings {
			if f.CheckID == "probes.tenant_left_behind" {
				t.Errorf("section %d: a disposable tenant was left behind: %s", s, f.Message)
			}
		}
		passed := 0
		for _, p := range o.Probes {
			if p.Passed {
				passed++
			}
		}
		if passed == 0 {
			t.Errorf("section %d ran no probe that passed:\n%s", s, openapiProvisionDescribe(o))
		}
	}
	for section, want := range map[int][]string{
		integrationaudit.SectionScope:         {"items.get"},
		integrationaudit.SectionSafeExecution: {"items.create"},
		integrationaudit.SectionRuntimeSmoke:  {"items.get", "items.create"},
	} {
		for _, id := range want {
			if !openapiProvisionHas(outs[section].Covered, id) {
				t.Errorf("section %d did not probe %s:\n%s", section, id, openapiProvisionDescribe(outs[section]))
			}
		}
	}
}

// ---- 2. only the audit's own tenants -------------------------------------------

// A provisioner acts only on the tenants it asked for. A second provisioner of the same
// audit (the audit builds one for every check) is refused a tenant the first one
// created, and so is a tenant nobody created, and the application is not even asked: an
// application that does not guard its own tenants is never given the chance. A create
// the application refused (the tenant exists) is not adopted either, so the cleanup that
// follows a failed create can never remove somebody else's tenant.
func TestOpenAPIProvisionerRefusesTenantsTheAuditDidNotCreate(t *testing.T) {
	w := openapiProvisionNewWorld(t, openapiProvisionOpts{})
	ctx := openapiProvisionCtx(t)

	mine, other := w.provisioner(t), w.provisioner(t)
	tenant := openapiProvisionTenantOf("aaaaaaaaaa")
	if err := mine.CreateTenant(ctx, tenant); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if _, err := mine.Seed(ctx, openapiProvisionSeedOf("aaaaaaaaaa")); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	// the application holds a tenant that is not the audit's
	w.app.mu.Lock()
	w.app.tenants["tnt_demo"] = &openapiProvisionRow{}
	w.app.mu.Unlock()

	sent := func() int { return len(w.app.requests("")) }
	before := sent()
	stranger := openapiProvisionSeedOf("aaaaaaaaaa")
	for name, call := range map[string]func() error{
		"seed of a tenant another provisioner created": func() error { _, err := other.Seed(ctx, stranger); return err },
		"cleanup of a tenant another provisioner created": func() error {
			_, err := other.Cleanup(ctx, tenant.ID)
			return err
		},
		"seed of a tenant nobody created": func() error {
			_, err := mine.Seed(ctx, openapiProvisionSeedOf("bbbbbbbbbb"))
			return err
		},
		"cleanup of a tenant nobody created": func() error {
			_, err := mine.Cleanup(ctx, "tnt_org_a_bbbbbbbbbb")
			return err
		},
		"cleanup of the application's own tenant": func() error {
			_, err := mine.Cleanup(ctx, "tnt_demo")
			return err
		},
	} {
		err := call()
		if contracts.CodeOf(err) != contracts.ErrPermissionDenied || openapiProvisionReasonOf(err) != openapiProvisionReasonNotOurs {
			t.Errorf("%s: %v (code %q, reason %q), want permission_denied", name, err, contracts.CodeOf(err), openapiProvisionReasonOf(err))
		}
	}
	if sent() != before {
		t.Errorf("a refused call reached the application: %d requests before, %d after", before, sent())
	}
	if ids := w.app.tenantIDs(); fmt.Sprint(ids) != fmt.Sprint([]string{"tnt_demo", tenant.ID}) {
		t.Errorf("the application's tenants are %v", ids)
	}

	// a tenant the other provisioner owns, which this one asks to create: the application
	// refuses (it exists), and that tenant stays the other's
	foreign := openapiProvisionTenantOf("cccccccccc")
	if err := other.CreateTenant(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := mine.CreateTenant(ctx, foreign); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("a second create of an existing tenant: %v, want conflict", err)
	}
	before = sent()
	if _, err := mine.Cleanup(ctx, foreign.ID); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("cleanup of a tenant whose creation the application refused: %v, want permission_denied", err)
	}
	if sent() != before {
		t.Error("the cleanup of a tenant this provisioner did not create reached the application")
	}
	if rep, err := other.Cleanup(ctx, foreign.ID); err != nil || !rep.Existed || rep.Left() != 0 {
		t.Errorf("the owner's cleanup of the tenant = %+v, %v: it must still exist and be removed", rep, err)
	}
	if rep, err := mine.Cleanup(ctx, tenant.ID); err != nil || !rep.Existed || rep.Left() != 0 {
		t.Errorf("the owner's cleanup = %+v, %v", rep, err)
	}
	if ids := w.app.tenantIDs(); fmt.Sprint(ids) != fmt.Sprint([]string{"tnt_demo"}) {
		t.Errorf("the application's tenants are %v, want only its own", ids)
	}
}

// ---- 3. every method can be called again ---------------------------------------

// A cleanup of a tenant that is gone is not an error: the application answers 404 and
// the report says Existed false with nothing remaining. That holds for a tenant that was
// removed behind the audit's back, for one the audit just removed, and for as many repeats
// as the audit makes; every one of them reaches the application. The same identifiers can
// be created again once the tenant is gone, and nothing is seeded into a tenant that is.
func TestOpenAPIProvisionerCleanupOfAGoneTenantIsNotAnError(t *testing.T) {
	w := openapiProvisionNewWorld(t, openapiProvisionOpts{})
	ctx := openapiProvisionCtx(t)
	p := w.provisioner(t)

	tenant := openapiProvisionTenantOf(openapiProvisionSuffix)
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	// a second create of a tenant that exists is a typed conflict that sends nothing
	creates := len(w.app.requests("create"))
	if err := p.CreateTenant(ctx, tenant); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("a second CreateTenant: %v, want conflict", err)
	}
	if len(w.app.requests("create")) != creates {
		t.Error("a second create of a tenant that exists reached the application")
	}

	// removed out of band: the first cleanup finds a 404
	w.app.remove(tenant.ID)
	cleanups := len(w.app.requests("cleanup"))
	rep, err := p.Cleanup(ctx, tenant.ID)
	if err != nil || rep.Existed || rep.Left() != 0 || rep.Remaining == nil {
		t.Fatalf("cleanup of a tenant that was removed out of band = %+v, %v: want Existed false and nothing left", rep, err)
	}
	if len(w.app.requests("cleanup")) != cleanups+1 {
		t.Error("the cleanup of a gone tenant did not reach the application")
	}
	if _, err := p.Seed(ctx, openapiProvisionSeedOf(openapiProvisionSuffix)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("Seed after the tenant was gone: %v, want permission_denied", err)
	}

	// created again, seeded, removed by the audit, and cleaned up twice more
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatalf("CreateTenant after the tenant was gone: %v", err)
	}
	seed, err := p.Seed(ctx, openapiProvisionSeedOf(openapiProvisionSuffix))
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	for _, r := range openapiProvisionSeedOf(openapiProvisionSuffix).Records {
		if seed.Entities[r.Ref] == "" {
			t.Errorf("no identifier for %s: %v", r.Ref, seed.Entities)
		}
	}
	first, err := p.Cleanup(ctx, tenant.ID)
	if err != nil || !first.Existed || first.Left() != 0 {
		t.Fatalf("the first cleanup = %+v, %v: want Existed true and nothing left", first, err)
	}
	for i := 2; i <= 3; i++ {
		before := len(w.app.requests("cleanup"))
		again, err := p.Cleanup(ctx, tenant.ID)
		if err != nil || again.Existed || again.Left() != 0 {
			t.Errorf("cleanup number %d = %+v, %v: want Existed false and nothing left", i, again, err)
		}
		if len(w.app.requests("cleanup")) != before+1 {
			t.Errorf("cleanup number %d did not reach the application", i)
		}
	}
	if _, err := p.Seed(ctx, openapiProvisionSeedOf(openapiProvisionSuffix)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("Seed after Cleanup: %v, want permission_denied", err)
	}
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Errorf("CreateTenant after Cleanup: %v", err)
	}
	if rep, err := p.Cleanup(ctx, tenant.ID); err != nil || !rep.Existed || rep.Left() != 0 {
		t.Errorf("the last cleanup = %+v, %v", rep, err)
	}
	if ids := w.app.tenantIDs(); len(ids) != 0 {
		t.Errorf("the application still holds %v", ids)
	}
}

// ---- 4. what is sent -------------------------------------------------------------

// The bridge sends the audit's own identifiers and the seed data file's members through
// the document's parameters and body, and nothing the audit owns can be set by the file: a
// record's workspace and principal are the generated identifiers, and the logical
// references of the file never reach the application.
func TestOpenAPIProvisionerSendsTheAuditsIdentifiersAsTheDocumentDescribesThem(t *testing.T) {
	w := openapiProvisionNewWorld(t, openapiProvisionOpts{})
	ctx := openapiProvisionCtx(t)
	p := w.provisioner(t)
	tenant := openapiProvisionTenantOf(openapiProvisionSuffix)

	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	create := w.app.requests("create")[0]
	if create.Method != http.MethodPost || create.Path != "/tenants" {
		t.Errorf("create_tenant sent %s %s", create.Method, create.Path)
	}
	var body struct {
		TenantID   string `json:"tenant_id"`
		Workspaces []struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspaces"`
		Principals []struct {
			PrincipalID string   `json:"principal_id"`
			WorkspaceID string   `json:"workspace_id"`
			Roles       []string `json:"roles"`
			Service     *bool    `json:"service"`
		} `json:"principals"`
	}
	if err := json.Unmarshal(create.Body, &body); err != nil {
		t.Fatalf("the create body is not JSON: %v", err)
	}
	if body.TenantID != tenant.ID || len(body.Workspaces) != 2 || body.Workspaces[0].WorkspaceID != tenant.Workspaces[0] ||
		len(body.Principals) != 2 || body.Principals[0].PrincipalID != "usr_member_a1_"+openapiProvisionSuffix || body.Principals[0].Service != nil ||
		body.Principals[1].Service == nil || !*body.Principals[1].Service || fmt.Sprint(body.Principals[1].Roles) != "[member]" {
		t.Errorf("the create body does not carry the tenant the audit generated: %s", create.Body)
	}

	if _, err := p.Seed(ctx, openapiProvisionSeedOf(openapiProvisionSuffix)); err != nil {
		t.Fatal(err)
	}
	seed := w.app.requests("seed")[0]
	if seed.Method != http.MethodPost || seed.Path != "/tenants/"+tenant.ID+"/seed" {
		t.Errorf("seed sent %s %s: the tenant belongs in the path the document declares", seed.Method, seed.Path)
	}
	var sent struct {
		TenantID string           `json:"tenant_id"`
		Records  []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(seed.Body, &sent); err != nil {
		t.Fatalf("the seed body is not JSON: %v", err)
	}
	if sent.TenantID != "" {
		t.Errorf("the tenant is a path parameter, and also a member of the body: %s", seed.Body)
	}
	ws := "ws_ws_1_" + openapiProvisionSuffix
	a1 := map[string]any{
		"ref": "item-a1", "entity_type": "item", "workspace_id": ws, "principal_id": "usr_member_a1_" + openapiProvisionSuffix,
		"name": "Alpha one", "color": "red",
	}
	b1 := map[string]any{"ref": "item-b1", "entity_type": "item", "workspace_id": ws, "name": "Beta one"}
	if len(sent.Records) != 2 || fmt.Sprint(sent.Records[0]) != fmt.Sprint(a1) || fmt.Sprint(sent.Records[1]) != fmt.Sprint(b1) {
		t.Errorf("the seed records are\n  %v\nwant\n  %v\n  %v", sent.Records, a1, b1)
	}
	for _, leaked := range []string{"someone-else", "ws-1", "member-a1\""} {
		if strings.Contains(string(seed.Body), leaked) {
			t.Errorf("the seed body carries %q, which the audit owns or which is a logical reference: %s", leaked, seed.Body)
		}
	}
	// the application's own items are in its collections for the audit's generated scope
	if got := w.app.host.App.Items(tenant.ID, ws); len(got) != 2 {
		t.Errorf("the application holds %v for the tenant's workspace, want the two seeded items", got)
	}

	rep, err := p.Cleanup(ctx, tenant.ID)
	if err != nil || !rep.Existed {
		t.Fatalf("Cleanup: %+v, %v", rep, err)
	}
	cleanup := w.app.requests("cleanup")[0]
	if cleanup.Method != http.MethodDelete || cleanup.Path != "/tenants/"+tenant.ID || len(cleanup.Body) != 0 {
		t.Errorf("cleanup sent %s %s with a body of %d bytes", cleanup.Method, cleanup.Path, len(cleanup.Body))
	}
	for _, s := range w.app.requests("") {
		if s.Header.Get("X-AlixAI-Identity") != "" || s.Header.Get("Idempotency-Key") != "" {
			t.Errorf("a provisioning call carried an identity or an idempotency key: %s %s", s.Method, s.Path)
		}
	}
}

// A seed that cannot be built fails closed before anything is sent.
func TestOpenAPIProvisionerFailsASeedClosedBeforeSendingAnything(t *testing.T) {
	w := openapiProvisionNewWorld(t, openapiProvisionOpts{})
	ctx := openapiProvisionCtx(t)
	p := w.provisioner(t)
	tenant := openapiProvisionTenantOf(openapiProvisionSuffix)
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		full := filepath.Join(w.dataDir, "package", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("tests/seed/dup.json", `{"records":[{"ref":"item-a1"},{"ref":"item-a1"}]}`)
	write("tests/seed/type.json", `{"records":[{"ref":"item-a1","entity_type":"patient"}]}`)
	write("tests/seed/ghost.json", `{"records":[{"ref":"item-a1","principal":"nobody"}]}`)
	write("tests/seed/noref.json", `{"records":[{"name":"x"}]}`)
	write("tests/seed/two.json", `{"records":[]} {"records":[]}`)
	write("tests/seed/array.json", `[1,2]`)
	write("tests/seed/huge.json", `{"records":[{"ref":"item-a1","name":"`+strings.Repeat("x", openapiProvisionMaxSeedFileBytes)+`"}]}`)

	good := openapiProvisionSeedOf(openapiProvisionSuffix)
	with := func(mod func(s *probes.Seed)) probes.Seed {
		s := good
		s.Records = append([]probes.Record(nil), good.Records...)
		mod(&s)
		return s
	}
	cases := map[string]probes.Seed{
		"a ref listed twice":                   with(func(s *probes.Seed) { s.DataFile = "tests/seed/dup.json" }),
		"another entity type than the suite":   with(func(s *probes.Seed) { s.DataFile = "tests/seed/type.json" }),
		"a principal the tenant does not have": with(func(s *probes.Seed) { s.DataFile = "tests/seed/ghost.json" }),
		"an entry without a ref":               with(func(s *probes.Seed) { s.DataFile = "tests/seed/noref.json" }),
		"two documents":                        with(func(s *probes.Seed) { s.DataFile = "tests/seed/two.json" }),
		"not an object":                        with(func(s *probes.Seed) { s.DataFile = "tests/seed/array.json" }),
		"a file over the bound":                with(func(s *probes.Seed) { s.DataFile = "tests/seed/huge.json" }),
		"a missing file":                       with(func(s *probes.Seed) { s.DataFile = "tests/seed/none.json" }),
		"a path out of tests/":                 with(func(s *probes.Seed) { s.DataFile = "schemas/items-openapi.yaml" }),
		"a path that climbs":                   with(func(s *probes.Seed) { s.DataFile = "tests/../schemas/items-openapi.yaml" }),
		"an absolute path":                     with(func(s *probes.Seed) { s.DataFile = "/etc/passwd" }),
		"no records":                           with(func(s *probes.Seed) { s.Records = nil }),
		"a record twice":                       with(func(s *probes.Seed) { s.Records = append(s.Records, s.Records[0]) }),
		"a workspace the tenant lacks":         with(func(s *probes.Seed) { s.Records[0].WorkspaceID = "ws_other_" + openapiProvisionSuffix }),
		"an unusable ref":                      with(func(s *probes.Seed) { s.Records[0].Ref = "../x" }),
		"no entity type":                       with(func(s *probes.Seed) { s.Records[0].EntityType = "" }),
	}
	for name, s := range cases {
		_, err := p.Seed(ctx, s)
		if contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: %v, want validation_failed", name, err)
		}
	}
	if n := len(w.app.requests("seed")); n != 0 {
		t.Errorf("%d seeds reached the application although every one was refused first", n)
	}
	// and the tenant is still the audit's to clean up
	if rep, err := p.Cleanup(ctx, tenant.ID); err != nil || !rep.Existed {
		t.Errorf("Cleanup = %+v, %v", rep, err)
	}

	// the same for a tenant the audit generated badly
	for name, bad := range map[string]probes.Tenant{
		"an identifier with a slash":      {ID: "tnt/x", Workspaces: []string{"ws_a"}},
		"no workspace":                    {ID: "tnt_x"},
		"a workspace twice":               {ID: "tnt_x", Workspaces: []string{"ws_a", "ws_a"}},
		"a principal in no workspace":     {ID: "tnt_x", Workspaces: []string{"ws_a"}, Principals: []probes.Principal{{ID: "usr_a", WorkspaceID: "ws_b", Roles: []string{"member"}}}},
		"a principal twice":               {ID: "tnt_x", Workspaces: []string{"ws_a"}, Principals: []probes.Principal{{ID: "usr_a", WorkspaceID: "ws_a"}, {ID: "usr_a", WorkspaceID: "ws_a"}}},
		"a role that is not an id":        {ID: "tnt_x", Workspaces: []string{"ws_a"}, Principals: []probes.Principal{{ID: "usr_a", WorkspaceID: "ws_a", Roles: []string{"a b"}}}},
		"a principal reference twice":     {ID: "tnt_x", Workspaces: []string{"ws_a"}, Principals: []probes.Principal{{Ref: "a", ID: "usr_a", WorkspaceID: "ws_a"}, {Ref: "a", ID: "usr_b", WorkspaceID: "ws_a"}}},
		"an unusable principal reference": {ID: "tnt_x", Workspaces: []string{"ws_a"}, Principals: []probes.Principal{{Ref: "../a", ID: "usr_a", WorkspaceID: "ws_a"}}},
	} {
		if err := p.CreateTenant(ctx, bad); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: %v, want validation_failed", name, err)
		}
	}
	if n := len(w.app.requests("create")); n != 1 {
		t.Errorf("%d creates reached the application, want only the one good one", n)
	}
}

// The principal a seed data file names is found by the reference the suite gives it
// (probes.Principal.Ref), never by how the audit happens to spell the identifier: an
// identifier of any shape resolves, and a reference no principal has does not, whatever an
// identifier looks like.
func TestOpenAPIProvisionerFindsAPrincipalByItsReferenceNotItsIdentifier(t *testing.T) {
	w := openapiProvisionNewWorld(t, openapiProvisionOpts{})
	ctx := openapiProvisionCtx(t)
	p := w.provisioner(t)
	tenant := openapiProvisionTenantOf(openapiProvisionSuffix)
	tenant.Principals[0].ID = "someone-spelled-another-way"
	tenant.Principals[1].ID = "usr_member_a1_" + openapiProvisionSuffix // looks like the first principal's old identifier
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Seed(ctx, openapiProvisionSeedOf(openapiProvisionSuffix)); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	var sent struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(w.app.requests("seed")[0].Body, &sent); err != nil {
		t.Fatal(err)
	}
	if got := sent.Records[0]["principal_id"]; got != "someone-spelled-another-way" {
		t.Errorf("the record's principal_id is %v, want the identifier of the principal whose reference is member-a1", got)
	}

	// no principal of the tenant has the reference the file names
	other := openapiProvisionTenantOf("eeeeeeeeee")
	other.Principals = other.Principals[1:]
	if err := p.CreateTenant(ctx, other); err != nil {
		t.Fatal(err)
	}
	before := len(w.app.requests("seed"))
	if _, err := p.Seed(ctx, openapiProvisionSeedOf("eeeeeeeeee")); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("a seed whose principal reference no principal has: %v, want validation_failed", err)
	}
	if len(w.app.requests("seed")) != before {
		t.Error("that seed reached the application")
	}
}

// The adapter configuration and the seed data file are read from where the transport
// opened the package, whatever the configuration's data directory says; the
// configuration's data directory is the fallback for an adapter the transport did not
// open, and a package that is nowhere to be found is an error that is the transport's
// own, with nothing sent.
func TestOpenAPIProvisionSourceFindsThePackageWhereTheTransportOpenedIt(t *testing.T) {
	w := openapiProvisionNewWorld(t, openapiProvisionOpts{})
	ctx := openapiProvisionCtx(t)
	setDataDir := func(dir string) {
		was := openapiProvisionDataDir
		openapiProvisionDataDir = func() string { return dir }
		t.Cleanup(func() { openapiProvisionDataDir = was })
	}
	key := openapiProvisionPackageKey{w.rec.ApplicationID, w.rec.ManifestHash}
	if _, ok := openapiProvisionInstallDirs.Load(key); !ok {
		t.Fatal("the transport did not record where it opened the package")
	}

	// recorded: a data directory that says something else is not consulted
	setDataDir(filepath.Join(w.dataDir, "elsewhere"))
	if p, err := openapiProvisionSource(ctx, w.in); p == nil || err != nil {
		t.Fatalf("with the install directory recorded: %v, %v", p, err)
	}

	// not recorded: the configuration's data directory joined with the record's path
	openapiProvisionInstallDirs.Delete(key)
	if p, err := openapiProvisionSource(ctx, w.in); p != nil || contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("with no install directory known: %v, %v, want an unavailable error", p, err)
	}
	setDataDir(w.dataDir)
	p, err := openapiProvisionSource(ctx, w.in)
	if p == nil || err != nil {
		t.Fatalf("with the configuration's data directory as the fallback: %v, %v", p, err)
	}
	tenant := openapiProvisionTenantOf(openapiProvisionSuffix)
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Seed(ctx, openapiProvisionSeedOf(openapiProvisionSuffix)); err != nil {
		t.Errorf("a seed through the fallback directory: %v", err)
	}
	if _, err := p.Cleanup(ctx, tenant.ID); err != nil {
		t.Fatal(err)
	}

	// recorded, but the directory is gone: the fallback
	openapiProvisionInstallDirs.Store(key, filepath.Join(w.dataDir, "gone"))
	if p, err := openapiProvisionSource(ctx, w.in); p == nil || err != nil {
		t.Errorf("with a recorded directory that is gone: %v, %v", p, err)
	}
}

// ---- 5. what the application says -----------------------------------------------

// A create whose reply was lost may still have created the tenant: it is the audit's to
// clean up, and not yet its to seed. A create the application refused is not.
func TestOpenAPIProvisionerOwnsATenantWhoseCreateReplyWasLost(t *testing.T) {
	w := openapiProvisionNewWorld(t, openapiProvisionOpts{})
	ctx := openapiProvisionCtx(t)
	p := w.provisioner(t)
	tenant := openapiProvisionTenantOf(openapiProvisionSuffix)

	w.app.mu.Lock()
	w.app.loseCreateReply = true
	w.app.mu.Unlock()
	if err := p.CreateTenant(ctx, tenant); err == nil {
		t.Fatal("a create whose reply was lost succeeded")
	}
	if ids := w.app.tenantIDs(); len(ids) != 1 {
		t.Fatalf("the application did not apply the request: %v", ids)
	}
	if _, err := p.Seed(ctx, openapiProvisionSeedOf(openapiProvisionSuffix)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("a seed into a tenant whose creation is unconfirmed: %v, want permission_denied", err)
	}
	if n := len(w.app.requests("seed")); n != 0 {
		t.Errorf("%d seeds reached the application", n)
	}
	rep, err := p.Cleanup(ctx, tenant.ID)
	if err != nil || !rep.Existed || rep.Left() != 0 {
		t.Errorf("the cleanup of a tenant whose creation is unconfirmed = %+v, %v: want it removed", rep, err)
	}
	if ids := w.app.tenantIDs(); len(ids) != 0 {
		t.Errorf("the tenant was left behind: %v", ids)
	}
}

// What the application says is read by status and shape and never believed beyond them:
// the codes of its refusals are the platform's, a reply for another tenant, a record that
// was not asked for, a cleanup that does not say what remains and a status that
// contradicts its body are all refused, and no word the application chose reaches an error.
func TestOpenAPIProvisionerDoesNotTrustWhatTheApplicationSays(t *testing.T) {
	w := openapiProvisionNewWorld(t, openapiProvisionOpts{})
	ctx := openapiProvisionCtx(t)
	p := w.provisioner(t)
	tenant := openapiProvisionTenantOf(openapiProvisionSuffix)

	assertTyped := func(what string, err error, want contracts.ErrCode) {
		t.Helper()
		if contracts.CodeOf(err) != want {
			t.Errorf("%s: %v (code %q), want %s", what, err, contracts.CodeOf(err), want)
		}
		if err != nil && strings.Contains(err.Error(), openapiProvisionCanary) {
			t.Errorf("%s: the error carries what the application said: %v", what, err)
		}
	}

	// the refusal of a create: the application's code when it is one of the platform's,
	// else the code of the status
	for _, c := range []struct {
		status int
		body   any
		want   contracts.ErrCode
	}{
		{http.StatusConflict, map[string]any{}, contracts.ErrConflict},
		{http.StatusForbidden, map[string]any{}, contracts.ErrPermissionDenied},
		{http.StatusBadRequest, map[string]any{}, contracts.ErrValidationFailed},
		{http.StatusUnprocessableEntity, map[string]any{}, contracts.ErrValidationFailed},
		{http.StatusTooManyRequests, map[string]any{}, contracts.ErrRateLimited},
		{http.StatusInternalServerError, map[string]any{}, contracts.ErrUnavailable},
		{http.StatusServiceUnavailable, map[string]any{"errors": []any{map[string]any{"code": "timeout", "message": openapiProvisionCanary}}}, contracts.ErrTimeout},
		{http.StatusUnprocessableEntity, map[string]any{"errors": []any{map[string]any{"code": "an invented code", "message": openapiProvisionCanary}}}, contracts.ErrValidationFailed},
		{http.StatusCreated, map[string]any{"status": "rejected", "errors": []any{map[string]any{"code": "scope_violation"}}}, contracts.ErrScopeViolation},
		{http.StatusCreated, map[string]any{"status": "queued"}, contracts.ErrSchemaInvalid},
		{http.StatusCreated, map[string]any{"tenant_id": "tnt_another"}, contracts.ErrSchemaInvalid},
		{http.StatusCreated, map[string]any{"tenant_id": 7}, contracts.ErrSchemaInvalid},
	} {
		w.app.answer("create", c.status, c.body)
		assertTyped(fmt.Sprintf("create answered %d %v", c.status, c.body), p.CreateTenant(ctx, tenant), c.want)
	}
	// a status the document does not describe is the adapter's own refusal, and not a success
	w.app.answer("create", http.StatusTeapot, map[string]any{})
	if err := p.CreateTenant(ctx, tenant); err == nil {
		t.Error("a status the document does not describe was accepted")
	}
	// a 2xx that is a success whatever else it says, with no tenant named
	w.app.answer("create", http.StatusCreated, map[string]any{"anything": "else", "status": "ok"})
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Errorf("a 201 that names no tenant: %v", err)
	}
	w.app.route("create", nil)
	// the application never held this tenant: ours to clean up (a reply may have been lost)
	// and the cleanup answers 404, which is fine
	if rep, err := p.Cleanup(ctx, tenant.ID); err != nil || rep.Existed || rep.Left() != 0 {
		t.Errorf("cleanup of the tenant = %+v, %v", rep, err)
	}

	// seed replies
	w.app.route("create", nil)
	fresh := openapiProvisionTenantOf("dddddddddd")
	if err := p.CreateTenant(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	s := openapiProvisionSeedOf("dddddddddd")
	rec := func(refs ...string) []any {
		var out []any
		for _, r := range refs {
			out = append(out, map[string]any{"ref": r, "entity_id": "itm_" + r})
		}
		return out
	}
	for name, c := range map[string]struct {
		status int
		body   map[string]any
		want   contracts.ErrCode
	}{
		"a record that was not asked for": {200, map[string]any{"records": rec("item-a1", "item-b1", "item-zz")}, contracts.ErrSchemaInvalid},
		"a record repeated":               {200, map[string]any{"records": rec("item-a1", "item-a1")}, contracts.ErrSchemaInvalid},
		"a record without an answer":      {200, map[string]any{"records": rec("item-a1")}, contracts.ErrSchemaInvalid},
		"no records":                      {200, map[string]any{"tenant_id": fresh.ID}, contracts.ErrSchemaInvalid},
		"an empty identifier":             {200, map[string]any{"records": []any{map[string]any{"ref": "item-a1", "entity_id": ""}, map[string]any{"ref": "item-b1", "entity_id": "x"}}}, contracts.ErrSchemaInvalid},
		"another tenant":                  {200, map[string]any{"tenant_id": "tnt_another", "records": rec("item-a1", "item-b1")}, contracts.ErrSchemaInvalid},
		"records of the wrong type":       {200, map[string]any{"records": "all of them"}, contracts.ErrSchemaInvalid},
		"a refusal":                       {403, map[string]any{"errors": []any{map[string]any{"code": "permission_denied", "message": openapiProvisionCanary}}}, contracts.ErrPermissionDenied},
		"a conflict":                      {409, map[string]any{}, contracts.ErrConflict},
		"a missing tenant":                {404, map[string]any{}, contracts.ErrValidationFailed},
	} {
		w.app.answer("seed", c.status, c.body)
		_, err := p.Seed(ctx, s)
		assertTyped("seed answered with "+name, err, c.want)
	}
	w.app.route("seed", nil)

	// cleanup replies
	for name, c := range map[string]struct {
		status int
		body   map[string]any
		want   contracts.ErrCode
	}{
		"no existed":                 {200, map[string]any{"remaining": map[string]any{}}, contracts.ErrSchemaInvalid},
		"no remaining":               {200, map[string]any{"existed": true}, contracts.ErrSchemaInvalid},
		"a negative count":           {200, map[string]any{"existed": true, "remaining": map[string]any{"items": -1}}, contracts.ErrSchemaInvalid},
		"a count that is no number":  {200, map[string]any{"existed": true, "remaining": map[string]any{"items": "none"}}, contracts.ErrSchemaInvalid},
		"another tenant":             {200, map[string]any{"tenant_id": "tnt_another", "existed": true, "remaining": map[string]any{}}, contracts.ErrSchemaInvalid},
		"a gone tenant that existed": {404, map[string]any{"existed": true, "remaining": map[string]any{}}, contracts.ErrSchemaInvalid},
		"a gone tenant with rows":    {404, map[string]any{"existed": false, "remaining": map[string]any{"items": 3}}, contracts.ErrSchemaInvalid},
		"a refusal":                  {403, map[string]any{}, contracts.ErrPermissionDenied},
		"a conflict":                 {409, map[string]any{}, contracts.ErrConflict},
		"a server error":             {500, map[string]any{}, contracts.ErrUnavailable},
	} {
		w.app.answer("cleanup", c.status, c.body)
		_, err := p.Cleanup(ctx, fresh.ID)
		assertTyped("cleanup answered with "+name, err, c.want)
	}
	// a cleanup that leaves rows says so, and the tenant stays the audit's to clean up
	w.app.answer("cleanup", 200, map[string]any{"existed": true, "remaining": map[string]any{"items": 3, "notes": 0}})
	rep, err := p.Cleanup(ctx, fresh.ID)
	if err != nil || !rep.Existed || rep.Left() != 3 {
		t.Errorf("a cleanup that left three rows = %+v, %v", rep, err)
	}
	w.app.route("cleanup", nil)
	if rep, err := p.Cleanup(ctx, fresh.ID); err != nil || !rep.Existed || rep.Left() != 0 {
		t.Errorf("the final cleanup = %+v, %v", rep, err)
	}
}

// ---- 6. held to the adapter's own checks ------------------------------------------

// Provisioning goes through the adapter's own egress gate and document checks. An
// adapter whose allowlist does not name the application cannot reach it, and nothing
// is sent; a status the document does not describe, a reply that is not JSON and a
// reply the document's schema refuses are errors, as they are for any call.
func TestOpenAPIProvisionerIsHeldToTheAdaptersEgressAndDocument(t *testing.T) {
	t.Run("egress", func(t *testing.T) {
		w := openapiProvisionNewWorld(t, openapiProvisionOpts{})
		plan, err := transportsPlanFor(w.pkg.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := openapiTransportConfig(context.Background(), TransportRequest{
			ApplicationID: openapiProvisionAppID, Kind: TransportKindOpenAPI, Endpoints: plan.Endpoints, Package: w.pkg, Record: w.rec,
			Dir: filepath.Join(w.dataDir, w.rec.Path),
		}, openapiTransportLogger())
		if err != nil {
			t.Fatal(err)
		}
		var denied []string
		cfg.Dialer = egress.NewDialer(nil, egress.WithDenyHook(func(d egress.Denial) { denied = append(denied, string(d.Reason)) }))
		raw, err := openapitransport.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		in := *w.in
		in.Adapter = transportsClosing{AppAdapter: w.ad, inner: raw}
		p, err := openapiProvisionSource(openapiProvisionCtx(t), &in)
		if err != nil || p == nil {
			t.Fatalf("the source: %v, %v", p, err)
		}
		cerr := p.CreateTenant(openapiProvisionCtx(t), openapiProvisionTenantOf(openapiProvisionSuffix))
		if cerr == nil || !egress.IsDenied(cerr) {
			t.Errorf("a create through a dialer that allows nothing: %v, want the egress gate's refusal", cerr)
		}
		if len(denied) == 0 {
			t.Error("the egress gate did not refuse the connection")
		}
		if n := len(w.app.requests("")); n != 0 {
			t.Errorf("%d requests reached an application the egress allowlist does not name", n)
		}
		// the tenant was never sent, so there is nothing of it to clean up
		if _, err := p.Cleanup(openapiProvisionCtx(t), openapiProvisionTenantOf(openapiProvisionSuffix).ID); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("cleanup of a tenant whose create the egress gate refused: %v, want permission_denied", err)
		}
	})

	t.Run("document", func(t *testing.T) {
		w := openapiProvisionNewWorld(t, openapiProvisionOpts{})
		p := w.provisioner(t)
		ctx := openapiProvisionCtx(t)
		tenant := openapiProvisionTenantOf(openapiProvisionSuffix)

		w.app.route("create", func(rw http.ResponseWriter, _ *http.Request, _ []byte) bool {
			rw.Header().Set("Content-Type", "text/plain")
			rw.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(rw, "created, "+openapiProvisionCanary)
			return true
		})
		err := p.CreateTenant(ctx, tenant)
		if contracts.CodeOf(err) != contracts.ErrSchemaInvalid || openapiProvisionReasonOf(err) != openapitransport.ReasonNotJSON {
			t.Errorf("a reply that is not JSON: %v (reason %q)", err, openapiProvisionReasonOf(err))
		}
		if err != nil && strings.Contains(err.Error(), openapiProvisionCanary) {
			t.Errorf("the error carries what the application said: %v", err)
		}
		w.app.route("create", func(rw http.ResponseWriter, _ *http.Request, _ []byte) bool {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(rw, `["not","an","object"]`)
			return true
		})
		if err := p.CreateTenant(ctx, tenant); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("a reply that is a JSON array: %v", err)
		}
		w.app.route("create", func(rw http.ResponseWriter, _ *http.Request, _ []byte) bool {
			rw.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
			rw.WriteHeader(http.StatusTemporaryRedirect)
			return true
		})
		err = p.CreateTenant(ctx, tenant)
		if contracts.CodeOf(err) != contracts.ErrSchemaInvalid || openapiProvisionReasonOf(err) != openapitransport.ReasonRedirect {
			t.Errorf("a redirect: %v (reason %q)", err, openapiProvisionReasonOf(err))
		}
		w.app.route("create", nil)

		// each of those may have been applied by the application, and the tenant is then
		// the audit's to clean up: it is, and the cleanup is the same call as any other
		if rep, err := p.Cleanup(ctx, tenant.ID); err != nil || rep.Existed || rep.Left() != 0 {
			t.Errorf("cleanup after replies the document refused = %+v, %v", rep, err)
		}
	})
}

// ---- 7. when there is no bridge ----------------------------------------------------

// A package whose provisioning cannot be run leaves the checks exactly as they were: no
// provisioner, or an error that is the OpenAPI transport's own, and every capability
// unverified, none probed, and the application never asked for a tenant.
func TestOpenAPIProvisionSourceLeavesTheChecksUnverifiedWithoutProvisioning(t *testing.T) {
	notApproved := map[string]openapiProvisionOpts{
		"an operation the adapter configuration does not approve": {provisioning: "[createTenant, cleanupTenant]"},
		"no provisioning operations in the adapter configuration": {provisioning: "[]"},
		"a suite that names an mcp tool": {calls: func(p *ap.PackageTestsProvisioning) {
			p.Seed = ap.AdapterCall{Endpoint: openapiProvisionEP, MCPTool: openapiProvisionPtr(ap.OperationName("test_seed"))}
		}},
		"one operation for two roles": {calls: func(p *ap.PackageTestsProvisioning) { p.Seed = p.CreateTenant }},
		"an endpoint the package does not have": {calls: func(p *ap.PackageTestsProvisioning) {
			for _, c := range []*ap.AdapterCall{&p.CreateTenant, &p.Seed, &p.Cleanup} {
				c.Endpoint = "elsewhere"
			}
		}},
		"calls on two endpoints": {calls: func(p *ap.PackageTestsProvisioning) { p.Cleanup.Endpoint = "elsewhere" }},
	}
	for name, o := range notApproved {
		t.Run(name, func(t *testing.T) {
			w := openapiProvisionNewWorld(t, o)
			p, err := openapiProvisionSource(openapiProvisionCtx(t), w.in)
			if p != nil || contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Fatalf("the source = %v, %v, want no provisioner and a validation_failed error", p, err)
			}
			outs := openapiProvisionRun(t, w.in, probes.Options{}, integrationaudit.SectionSafeExecution, integrationaudit.SectionRuntimeSmoke)
			for s, o := range outs {
				if len(o.Covered) != 0 {
					t.Errorf("section %d probed %v without provisioning:\n%s", s, o.Covered, openapiProvisionDescribe(o))
				}
				critical := false
				for _, f := range o.Findings {
					critical = critical || f.Severity == contracts.SeverityCritical
				}
				if !critical {
					t.Errorf("section %d records no critical finding for the consequential capability that was not probed:\n%s", s, openapiProvisionDescribe(o))
				}
			}
			if n := len(w.app.requests("")); n != 0 {
				t.Errorf("%d provisioning calls reached the application", n)
			}
		})
	}

	t.Run("a package with no test suite", func(t *testing.T) {
		w := openapiProvisionNewWorld(t, openapiProvisionOpts{noSuites: true})
		if p, err := openapiProvisionSource(openapiProvisionCtx(t), w.in); p != nil || err != nil {
			t.Errorf("the source = %v, %v, want nothing", p, err)
		}
	})
}

// The source answers for the audits of this transport and of no other: a missing input,
// a double of an adapter, the adapter itself unwrapped, and an adapter of another
// transport all leave the probes to the other sources.
func TestOpenAPIProvisionSourceLeavesOtherAdaptersAlone(t *testing.T) {
	w := openapiProvisionNewWorld(t, openapiProvisionOpts{})
	ctx := openapiProvisionCtx(t)
	for name, in := range map[string]*integrationaudit.Input{
		"no input":   nil,
		"no package": {Adapter: w.ad},
		"no adapter": {Package: w.pkg},
		"an adapter that is not the registry's wrapper": func() *integrationaudit.Input {
			c := *w.in
			c.Adapter = fake.NewAdapter(fake.NewApp())
			return &c
		}(),
		"a wrapper around another adapter": func() *integrationaudit.Input {
			c := *w.in
			other := fake.NewAdapter(fake.NewApp())
			c.Adapter = transportsClosing{AppAdapter: other, inner: other}
			return &c
		}(),
	} {
		if p, err := openapiProvisionSource(ctx, in); p != nil || err != nil {
			t.Errorf("%s: the source = %v, %v, want nothing", name, p, err)
		}
	}
	// a package of another transport kind, with the OpenAPI adapter in hand
	c := *w.in
	pkg := *w.pkg
	pkg.Manifest.Endpoints = map[string]ap.ManifestEndpoint{
		openapiProvisionEP: {Value: ap.ManifestMCPEndpoint{Transport: "mcp", URL: ap.HTTPURL("http://127.0.0.1:1/mcp"), AdapterConfig: "schemas/x.yaml"}},
	}
	c.Package = &pkg
	if p, err := openapiProvisionSource(ctx, &c); p != nil || err != nil {
		t.Errorf("a package of another transport: the source = %v, %v, want nothing", p, err)
	}
}

// ---- 8. the adapter's own call -------------------------------------------------------

// A provisioning call reaches the application only through the operation the adapter
// configuration approves: the fixture's other operations, though the document describes
// them, stay unreachable through the provisioner.
func TestOpenAPIProvisionerReachesOnlyTheApprovedOperations(t *testing.T) {
	w := openapiProvisionNewWorld(t, openapiProvisionOpts{})
	ctx := openapiProvisionCtx(t)
	closing, ok := w.ad.(transportsClosing)
	if !ok {
		t.Fatalf("the audit's adapter is a %T", w.ad)
	}
	raw := closing.inner.(*openapitransport.Adapter)
	setup := len(w.app.requests("")) + len(w.app.host.Requests()) // what the audit's input asked for
	for _, op := range []string{openapifixture.OpExport, openapifixture.OpGet, openapifixture.OpCommit, "createTenant2", ""} {
		if _, err := raw.Manage(ctx, openapiProvisionEP, op, map[string]any{"tenant_id": "tnt_x"}); contracts.CodeOf(err) != contracts.ErrUnknownCapability {
			t.Errorf("Manage(%q) = %v, want unknown_capability", op, err)
		}
	}
	if _, err := raw.Manage(ctx, "elsewhere", "createTenant", nil); contracts.CodeOf(err) != contracts.ErrUnknownCapability {
		t.Errorf("Manage on another endpoint = %v, want unknown_capability", err)
	}
	if n := w.app.host.ExportHits(); n != 0 {
		t.Errorf("the export operation was reached %d times", n)
	}
	if n := len(w.app.requests("")) + len(w.app.host.Requests()) - setup; n != 0 {
		t.Errorf("%d requests reached the application for operations the audit may not call", n)
	}
}

// The provisioning source holds nothing between audits: each call builds a provisioner
// with its own record of the tenants it created, and a tenant one of them created is
// refused to the next. It is also safe for concurrent use.
func TestOpenAPIProvisionerIsSafeForConcurrentUse(t *testing.T) {
	w := openapiProvisionNewWorld(t, openapiProvisionOpts{})
	ctx := openapiProvisionCtx(t)
	p := w.provisioner(t)
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n*3)
	for i := 0; i < n; i++ {
		suffix := fmt.Sprintf("%010d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.CreateTenant(ctx, openapiProvisionTenantOf(suffix)); err != nil {
				errs <- err
				return
			}
			if _, err := p.Seed(ctx, openapiProvisionSeedOf(suffix)); err != nil {
				errs <- err
			}
			if rep, err := p.Cleanup(ctx, openapiProvisionTenantOf(suffix).ID); err != nil || !rep.Existed || rep.Left() != 0 {
				errs <- fmt.Errorf("cleanup = %+v, %v", rep, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if ids := w.app.tenantIDs(); len(ids) != 0 {
		t.Errorf("the application still holds %v", ids)
	}
	if got := len(w.app.requests("create")); got != n {
		t.Errorf("%d creates for %d tenants", got, n)
	}
}
