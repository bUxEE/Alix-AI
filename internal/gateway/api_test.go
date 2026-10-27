package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// The tests of the public API (M1-16) run the real stack behind the real
// handlers: a throwaway store, the real audit log, policy gateway, action
// coordinator, session store and dialogue engine, identity verification with
// a freshly generated issuer key, and the in-memory host application
// (internal/adapters/fake) behind adapters.Wrap. Only the model is scripted
// (the deterministic provider fake). Every request mints a fresh single-use
// context token, so a test that passes cannot be passing on a reused jti.

// ---- the synthetic application ----

const (
	apiApp    = identitytest.DefaultApplicationID // "app_demo"
	apiTenant = identitytest.DefaultTenant        // "ten_acme"
	apiOther  = "ten_globex"
	apiWsp    = identitytest.DefaultWorkspace // "wsp_main"
	apiWsp2   = "wsp_second"
	apiSess   = identitytest.DefaultSession // "ses_test_01"
	apiDep    = identitytest.DefaultDeployment

	capGet    = fake.ReadCapability   // items.get
	capCreate = fake.ActionCapability // items.create
	capAdmin  = "admin.purge"

	sayCreate = "create an item named widget with quantity 3"
)

var apiNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// testPkg is an application package built in memory: the policy view beside
// the typed view. It satisfies capabilities.Package without a signed archive.
type testPkg struct {
	*policy.Package
	hash      string
	langs     []string
	specs     map[string]ap.Capability
	contracts map[[2]string]ap.ConversationContract
	terms     []ap.TerminologyTerm
}

func (p *testPkg) ContentHash() string                  { return p.hash }
func (p *testPkg) SupportedLanguages() []string         { return p.langs }
func (p *testPkg) Terminology() []ap.TerminologyTerm    { return p.terms }
func (p *testPkg) Spec(id string) (ap.Capability, bool) { s, ok := p.specs[id]; return s, ok }
func (p *testPkg) ContractFor(c, l string) (ap.ConversationContract, bool) {
	x, ok := p.contracts[[2]string{c, l}]
	return x, ok
}

var _ capabilities.Package = (*testPkg)(nil)

// level says how items.create is declared: the effect class fixes the platform
// risk floor, which fixes the confirmation it needs.
type level struct {
	effect  policy.EffectClass
	risk    contracts.RiskLevel
	confirm policy.ConfirmationMode
	typed   ap.CapabilityConfirmationMode
}

var (
	level2 = level{policy.EffectExternal, contracts.RiskConsequential, policy.ConfirmExplicit, ap.CapabilityConfirmationModeExplicit}
	level3 = level{policy.EffectPrivileged, contracts.RiskPrivileged, policy.ConfirmStepUp, ap.CapabilityConfirmationModeStepUp}
)

func i64(n int64) *int64     { return &n }
func f64(n float64) *float64 { return &n }
func boolp(b bool) *bool     { return &b }

func capID(s string) ap.CapabilityID { return ap.CapabilityID(s) }

func buildPackage(t testing.TB, create level) *testPkg {
	t.Helper()
	specs := map[string]ap.Capability{
		capGet: {
			ID: capID(capGet), Audience: ap.CapabilityAudienceUser, Title: "Get an item",
			Purpose: "Look up one item by its ID and report its name. Use it when the user asks about an item.",
			Fields: []ap.CapabilityField{
				{Name: "item_id", Type: ap.CapabilityFieldTypeString, Required: true, Description: "The item to look up."},
			},
			EntityTypes: map[string]ap.CapabilityEntityType{},
		},
		capCreate: {
			ID: capID(capCreate), Audience: ap.CapabilityAudienceUser, Title: "Create an item",
			Purpose: "Create a new item with a name and a quantity. The item is prepared first and created only after the user confirms.",
			Fields: []ap.CapabilityField{
				{Name: "name", Type: ap.CapabilityFieldTypeString, Required: true, Description: "The item name.", MinLength: i64(1), MaxLength: i64(64)},
				{Name: "quantity", Type: ap.CapabilityFieldTypeInteger, Required: true, Description: "How many.", Minimum: f64(1), Maximum: f64(99)},
			},
			EntityTypes: map[string]ap.CapabilityEntityType{},
		},
		capAdmin: {
			ID: capID(capAdmin), Audience: ap.CapabilityAudienceAdmin, Title: "Purge everything",
			Purpose: "Operators only: delete every item.", EntityTypes: map[string]ap.CapabilityEntityType{},
		},
	}
	decls := []policy.CapabilityDecl{
		{ID: capGet, Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead,
			Confirmation: policy.ConfirmNone, Fields: []policy.FieldDecl{{Name: "item_id", Required: true}}},
		{ID: capCreate, Audience: policy.AudienceUser, EffectClass: create.effect, RiskLevel: create.risk,
			Confirmation: create.confirm, Fields: []policy.FieldDecl{{Name: "name", Required: true}, {Name: "quantity", Required: true}}},
		{ID: capAdmin, Audience: policy.AudienceAdmin, EffectClass: policy.EffectPrivileged, RiskLevel: contracts.RiskPrivileged, Confirmation: policy.ConfirmStepUp},
	}
	cr := specs[capCreate]
	cr.EffectClass, cr.RiskLevel = ap.CapabilityEffectClass(create.effect), ap.RiskLevel(create.risk)
	cr.Confirmation = ap.CapabilityConfirmation{Mode: create.typed}
	specs[capCreate] = cr

	p := &testPkg{
		hash:  "0000000000000000000000000000000000000000000000000000000000000abc",
		langs: []string{"en", "it"}, specs: specs, contracts: map[[2]string]ap.ConversationContract{},
	}
	p.terms = []ap.TerminologyTerm{
		{ID: "create", Labels: map[string]ap.TerminologyLabel{
			"en": {Preferred: "create", Synonyms: []ap.Label{"add", "new"}}, "it": {Preferred: "crea", Synonyms: []ap.Label{"aggiungi", "nuovo"}}}},
		{ID: "item", Labels: map[string]ap.TerminologyLabel{
			"en": {Preferred: "item", Synonyms: []ap.Label{"thing", "article"}}, "it": {Preferred: "articolo", Synonyms: []ap.Label{"oggetto"}}}},
		{ID: "lookup", Labels: map[string]ap.TerminologyLabel{
			"en": {Preferred: "get", Synonyms: []ap.Label{"find", "show", "lookup"}}, "it": {Preferred: "trova", Synonyms: []ap.Label{"mostra", "cerca"}}}},
		{ID: "name", Labels: map[string]ap.TerminologyLabel{"en": {Preferred: "name"}, "it": {Preferred: "nome"}}},
		{ID: "quantity", Labels: map[string]ap.TerminologyLabel{"en": {Preferred: "quantity"}, "it": {Preferred: "quantità"}}},
	}
	sort.Slice(p.terms, func(i, j int) bool { return p.terms[i].ID < p.terms[j].ID })
	prompts := func(items ...[2]string) []ap.ConversationContractFieldPrompt {
		var out []ap.ConversationContractFieldPrompt
		for _, it := range items {
			out = append(out, ap.ConversationContractFieldPrompt{Field: common.FieldName(it[0]), Label: ap.Label(it[1]), Prompt: ap.Template("What is the " + it[1] + "?")})
		}
		return out
	}
	for _, l := range []string{"en", "it"} {
		p.contracts[[2]string{capGet, l}] = ap.ConversationContract{CapabilityID: capID(capGet), Language: ap.LanguageTag(l),
			Terminology: []ap.Slug{"lookup", "item"}, FieldPrompts: prompts([2]string{"item_id", "Item code"})}
		p.contracts[[2]string{capCreate, l}] = ap.ConversationContract{CapabilityID: capID(capCreate), Language: ap.LanguageTag(l),
			Terminology:  []ap.Slug{"create", "item", "quantity", "name"},
			FieldPrompts: prompts([2]string{"name", "Item name"}, [2]string{"quantity", "Quantity"})}
	}
	hp, err := policy.ParseHostPolicy([]byte(`{
  "policy_id": "default-access",
  "role_grants": [
    { "role": "member", "capabilities": ["items.get", "items.create"] },
    { "role": "visitor", "capabilities": ["items.get"] }
  ],
  "risk_overrides": [],
  "field_projection": []
}`))
	if err != nil {
		t.Fatal(err)
	}
	pp, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: apiApp, Version: "1.0.0", Status: policy.StatusActive,
		Roles: []string{"member", "visitor", "staff"}, Capabilities: decls, Policies: []*policy.HostPolicy{hp},
	})
	if err != nil {
		t.Fatalf("NewPackage: %v", err)
	}
	p.Package = pp
	return p
}

// fakeApps is the ApplicationService of the tests that do not install a real
// package: it serves one in-memory package, or none (quarantined).
type fakeApps struct {
	mu          sync.Mutex
	pkg         *testPkg
	quarantined bool
}

func (f *fakeApps) Package(_ context.Context, applicationID string) (capabilities.Package, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.quarantined || f.pkg == nil || f.pkg.ApplicationID() != applicationID {
		return nil, errNotActive()
	}
	return f.pkg, nil
}

func (f *fakeApps) Records(_ context.Context, applicationID string) ([]apppkg.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := apppkg.StateActive
	if f.quarantined {
		state = apppkg.StateQuarantined
	}
	return []apppkg.Record{{ApplicationID: applicationID, ManifestHash: f.pkg.hash, PackageVersion: "1.0.0", State: state,
		InstalledAt: apiNow, StatusChangedAt: apiNow}}, nil
}

func (f *fakeApps) Register(context.Context, contracts.Scope, []byte) (*apppkg.Installed, error) {
	return nil, contracts.New(contracts.ErrUnavailable, "the in-memory applications do not register packages")
}

func (f *fakeApps) quarantine(on bool) {
	f.mu.Lock()
	f.quarantined = on
	f.mu.Unlock()
}

// ---- the environment ----

type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type apiEnv struct {
	t      testing.TB
	clk    *clock.Fake
	db     *store.DB
	alog   *audit.Log
	host   *fake.App
	model  *pfake.Provider
	issuer *identitytest.Issuer
	keys   *identity.KeyRegistry
	apps   ApplicationService
	fapps  *fakeApps
	real   *Applications
	co     *actions.Coordinator
	eng    *dialogue.Engine
	sess   *sessions.Store
	svc    *Services
	srv    *Server
	logs   *syncLog
	pkg    *testPkg
	appID  string
	// dataDir is where a real installer writes packages.
	dataDir string
}

type apiOpts struct {
	create    level
	real      bool
	appID     string
	turnAudit func(audit.Appender) audit.Appender
	caps      []string
}

type apiOpt func(*apiOpts)

func withLevel(l level) apiOpt { return func(o *apiOpts) { o.create = l } }

// withRealApps installs packages for real (the apppkg installer with the
// published TEST-ONLY publisher key trusted) instead of serving an in-memory one.
func withRealApps(appID string) apiOpt {
	return func(o *apiOpts) { o.real, o.appID = true, appID }
}

// withTurnAudit wraps the appender only the dialogue engine writes its turn
// events to.
func withTurnAudit(f func(audit.Appender) audit.Appender) apiOpt {
	return func(o *apiOpts) { o.turnAudit = f }
}

// devTrust trusts the published TEST-ONLY publisher key for every application,
// like a developer's trust store.
type devTrust struct{}

func (devTrust) LookupKey(_ context.Context, app, id string) (trust.Key, error) {
	if id != devkeys.KeyID {
		return trust.Key{}, trust.ErrUnknownKey
	}
	return trust.Key{ApplicationID: app, KeyID: devkeys.KeyID, PublicKey: devkeys.PublicKey(), Status: trust.KeyActive}, nil
}

var defaultCaps = []string{capGet, capCreate}

func newAPIEnv(t testing.TB, opts ...apiOpt) *apiEnv {
	t.Helper()
	o := apiOpts{create: level2, appID: apiApp, caps: defaultCaps}
	for _, f := range opts {
		f(&o)
	}
	e := &apiEnv{t: t, clk: clock.NewFake(apiNow), logs: &syncLog{}, appID: o.appID}
	e.db = storetest.Open(t, nil)
	var err error
	if e.alog, err = audit.New(e.db, audit.Options{Clock: e.clk}); err != nil {
		t.Fatal(err)
	}
	sink := audit.NewRecordSink[policy.Record](e.alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	gw := policy.NewGateway(e.clk, policy.WithSink(sink))
	e.host = fake.NewApp(fake.WithClock(e.clk))
	guard, err := adapters.Wrap(fake.NewAdapter(e.host), adapters.Options{ApplicationID: apiApp})
	if err != nil {
		t.Fatal(err)
	}
	adapterSource := actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil })

	var registry *capabilities.Registry
	if o.real {
		e.dataDir = t.TempDir()
		inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{
			DB: e.db, Clock: e.clk, DataDir: e.dataDir, Verifier: trust.NewVerifier(devTrust{}, e.clk), CoreVersion: apppkg.DevCoreVersion,
		})
		if err != nil {
			t.Fatal(err)
		}
		e.real, err = NewApplications(inst, apppkg.Options{Verifier: trust.NewVerifier(devTrust{}, e.clk), CoreVersion: apppkg.DevCoreVersion}, e.clk, 0)
		if err != nil {
			t.Fatal(err)
		}
		e.apps = e.real
		registry = capabilities.NewRegistry(e.real.Source(), capabilities.WithClock(e.clk))
	} else {
		e.pkg = buildPackage(t, o.create)
		e.fapps = &fakeApps{pkg: e.pkg}
		e.apps = e.fapps
		registry = capabilities.NewRegistry(nil, capabilities.WithClock(e.clk))
		if err := registry.Activate(e.pkg); err != nil {
			t.Fatalf("Activate: %v", err)
		}
	}
	e.co, err = actions.New(actions.Deps{
		DB: e.db, Policy: gw, Audit: e.alog, Clock: e.clk, Adapters: adapterSource,
		Packages: actions.PackageSourceFunc(func(ctx context.Context, app string) (policy.ActivePackage, error) {
			p, err := e.apps.Package(ctx, app)
			if err != nil {
				return nil, err
			}
			return p, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.sess, err = sessions.New(e.db, e.clk, sessions.Config{}); err != nil {
		t.Fatal(err)
	}
	e.model = pfake.New(pfake.WithClock(e.clk))
	budget := providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute}
	sel, err := dialogue.LocalSelector(e.model, budget)
	if err != nil {
		t.Fatal(err)
	}
	var turnAudit audit.Appender = e.alog
	if o.turnAudit != nil {
		turnAudit = o.turnAudit(e.alog)
	}
	e.eng, err = dialogue.New(dialogue.Deps{
		Sessions: e.sess, Registry: registry, Packages: e.apps, Policy: gw, Actions: e.co, Adapters: adapterSource,
		Providers: sel, Audit: turnAudit, Clock: e.clk,
		Decision: dialogue.NoopDecisionStage{}, Guard: dialogue.LocalOnlyGuard{},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The services of the three /v1 areas, provided by type exactly the way the
	// composition root provides them. An area added later puts its own fakes in
	// through registerTestServices (services_test.go), so this stays one block.
	e.svc = NewServices()
	for _, perr := range []error{
		Provide(e.svc, e.sess),
		Provide(e.svc, e.eng),
		Provide[ActionService](e.svc, e.co),
		Provide[ApplicationService](e.svc, e.apps),
		Provide(e.svc, gw),
		Provide[actions.AdapterSource](e.svc, adapterSource),
		Provide[audit.Appender](e.svc, e.alog),
	} {
		if perr != nil {
			t.Fatal(perr)
		}
	}
	provideTestServices(t, e, e.svc)

	e.keys = identity.NewKeyRegistry(nil)
	e.issuer = identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: o.appID, Clock: e.clk})
	if err := e.keys.Register(e.issuer.Key()); err != nil {
		t.Fatal(err)
	}
	verifier, err := identity.NewVerifier(identity.Options{
		DeploymentID: apiDep, Keys: e.keys, Replay: identity.NewSQLiteReplayCache(e.db, e.clk, nil), Clock: e.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := providers.NewRegistry(e.clk)
	if err := reg.Register(providers.RoleController, e.model); err != nil {
		t.Fatal(err)
	}
	e.srv, err = NewServer(Options{
		Clock: e.clk, Logger: alog.NewWithWriter(config.LogConfig{Level: "debug"}, e.logs), Providers: reg,
		V1Auth: identity.Middleware(verifier), Services: e.svc,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// ---- requests ----

// claims adjusts the context a token is minted with.
type claims = func(*identitytest.Draft)

func withTenant(id string) claims {
	return func(d *identitytest.Draft) { d.Set(identity.ClaimTenant, id) }
}
func withWorkspace(id string) claims {
	return func(d *identitytest.Draft) { d.Set(identity.ClaimWorkspace, id) }
}
func withPrincipal(id string) claims {
	return func(d *identitytest.Draft) { d.Set(identity.ClaimSubject, id) }
}
func withSession(id string) claims {
	return func(d *identitytest.Draft) { d.Set(identity.ClaimSession, id) }
}
func withRoles(r ...string) claims {
	return func(d *identitytest.Draft) { d.Set(identity.ClaimRoles, r) }
}
func withScopes(c ...string) claims {
	return func(d *identitytest.Draft) { d.Set(identity.ClaimCapabilityScopes, c) }
}

// token mints a fresh token (a new jti) for the default context.
func (e *apiEnv) token(mods ...claims) string {
	base := []func(*identitytest.Draft){withScopes(defaultCaps...)}
	for _, m := range mods {
		base = append(base, m)
	}
	return e.issuer.Token(base...)
}

// call sends one request with a fresh token. body is marshalled to JSON when it
// is not a string or []byte.
func (e *apiEnv) call(method, path string, body any, mods ...claims) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.callAs(e.token(mods...), method, path, body)
}

func (e *apiEnv) callAs(token, method, path string, body any, headers ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case string:
		rd, ct = strings.NewReader(b), "application/json"
	case []byte:
		rd, ct = bytes.NewReader(b), "application/json"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd, ct = bytes.NewReader(raw), "application/json"
	}
	req := httptest.NewRequest(method, path, rd)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *apiEnv) callRaw(token, method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func sessionsPath(id string) string { return "/v1/sessions/" + id }

// v1AreaRoutes are the routes the V1 areas of this API mount and no others: a
// later task's area has its own tests, and a check that posts a body of this
// API's shape to every route would not be fair to a route of its own.
func (e *apiEnv) v1AreaRoutes() []string {
	e.t.Helper()
	var out []string
	for _, area := range []string{"v1_actions", "v1_apps", "v1_sessions"} {
		out = append(out, e.srv.AreaRoutes(area)...)
	}
	sort.Strings(out)
	return out
}

// turn posts one turn and returns the response.
func (e *apiEnv) turn(in TurnRequest, mods ...claims) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.call(http.MethodPost, sessionsPath(apiSess)+"/turns", in, mods...)
}

func wantStatus(t testing.TB, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d: %s", rec.Code, want, rec.Body.String())
	}
}

// wantError asserts the response is the error envelope with the code and status.
func wantError(t testing.TB, rec *httptest.ResponseRecorder, status int, code contracts.ErrCode) contracts.Error {
	t.Helper()
	wantStatus(t, rec, status)
	var env contracts.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("not an error envelope: %v: %s", err, rec.Body.String())
	}
	validateAgainst(t, "urn:alixai:schema:common:error:v1", rec.Body.Bytes())
	if env.Code != code {
		t.Fatalf("error code = %s, want %s: %s", env.Code, code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("error Content-Type = %q", ct)
	}
	return env
}

// wantUnauthorized asserts the identity middleware's refusal: 401 with a
// credential challenge and one of its codes.
func wantUnauthorized(t testing.TB, rec *httptest.ResponseRecorder) contracts.Error {
	t.Helper()
	wantStatus(t, rec, http.StatusUnauthorized)
	var env contracts.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("not an error envelope: %v: %s", err, rec.Body.String())
	}
	validateAgainst(t, "urn:alixai:schema:common:error:v1", rec.Body.Bytes())
	switch env.Code {
	case contracts.ErrUnauthenticated, contracts.ErrExpired, contracts.ErrReplayed, contracts.ErrScopeMissing:
	default:
		t.Fatalf("a 401 with code %s: %s", env.Code, rec.Body.String())
	}
	return env
}

func decodeBody[T any](t testing.TB, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decoding %T: %v: %s", v, err, rec.Body.String())
	}
	return v
}

// ---- scripting the model ----

// apiProposal builds a model output for a TaskProposal.
func apiProposal(capability, intent string, fields map[string]any) string {
	m := map[string]any{
		"capability_id": nil, "fields": map[string]any{}, "entity_refs": []any{}, "intent": intent,
		"uncertainty": map[string]any{"overall": 0.9},
	}
	if capability != "" {
		m["capability_id"] = capability
	}
	if fields != nil {
		m["fields"] = fields
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (e *apiEnv) onProposal(reply string) *pfake.Rule {
	return e.model.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(reply)
}

func (e *apiEnv) onResponse(text string) *pfake.Rule {
	return e.model.On(pfake.Constrained(providers.ConstraintNone)).Reply(text)
}

// prepareCreate runs the turn that prepares items.create for widget and returns
// the JSON response, its events and the confirmation grant.
func (e *apiEnv) prepareCreate(mods ...claims) (TurnBody, *httptest.ResponseRecorder) {
	e.t.Helper()
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	e.onResponse("Please confirm: create an item named widget.")
	rec := e.turn(TurnRequest{Text: sayCreate, Language: "en"}, mods...)
	wantStatus(e.t, rec, http.StatusOK)
	body := decodeBody[TurnBody](e.t, rec)
	if body.Confirmation == nil || body.Confirmation.PreparationID == "" || body.Confirmation.Token == "" {
		e.t.Fatalf("the turn prepared no confirmable action: %s", rec.Body.String())
	}
	return body, rec
}

func (e *apiEnv) hostCalls(op adapters.Operation) int {
	n := 0
	for _, c := range e.host.Calls() {
		if c.Operation == op {
			n++
		}
	}
	return n
}

func (e *apiEnv) sessionRows() int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// scopeOf is the verified scope of the default token, for calling the
// coordinator directly in assertions.
func (e *apiEnv) scopeOf(mods ...claims) contracts.Scope {
	sc := identitytest.ExpectedScope()
	sc.ApplicationID = e.appID
	sc.CapabilityScopes = append([]string(nil), defaultCaps...)
	for _, m := range mods {
		d := &identitytest.Draft{Claims: map[string]any{}}
		m(d)
		if v, ok := d.Claims[identity.ClaimTenant].(string); ok {
			sc.TenantID = v
		}
		if v, ok := d.Claims[identity.ClaimWorkspace].(string); ok {
			sc.WorkspaceID = v
		}
		if v, ok := d.Claims[identity.ClaimSubject].(string); ok {
			sc.PrincipalID = v
		}
		if v, ok := d.Claims[identity.ClaimSession].(string); ok {
			sc.SessionID = v
		}
	}
	return sc
}

// ---- schema validation of what the API produces ----

var apiSchemaRoot = filepath.Join("..", "..", "schemas")

const schemaTurnEvent = "urn:alixai:schema:conversation:turn-event:v1"

var (
	apiSchemaOnce sync.Once
	apiSchemaComp *jsonschema.Compiler
	apiSchemaErr  error
)

func apiSchemas(t testing.TB) *jsonschema.Compiler {
	t.Helper()
	apiSchemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		for _, area := range []string{"common", "actions", "conversation", "memory", "analyst"} {
			files, err := filepath.Glob(filepath.Join(apiSchemaRoot, area, "*.schema.json"))
			if err != nil || len(files) == 0 {
				apiSchemaErr = errors.Join(err, errors.New("no schemas in "+area))
				return
			}
			for _, f := range files {
				raw, err := os.ReadFile(f)
				if err != nil {
					apiSchemaErr = err
					return
				}
				doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
				if err != nil {
					apiSchemaErr = err
					return
				}
				if err := c.AddResource(doc.(map[string]any)["$id"].(string), doc); err != nil {
					apiSchemaErr = err
					return
				}
			}
		}
		apiSchemaComp = c
	})
	if apiSchemaErr != nil || apiSchemaComp == nil {
		t.Fatalf("schemas unavailable: %v", apiSchemaErr)
	}
	return apiSchemaComp
}

func validateAgainst(t testing.TB, schemaID string, raw []byte) {
	t.Helper()
	s, err := apiSchemas(t).Compile(schemaID)
	if err != nil {
		t.Fatalf("compile %s: %v", schemaID, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("%s rejects %s: %v", schemaID, raw, err)
	}
}

// checkTurnEvents asserts the structural rules every turn's events satisfy:
// each is a valid TurnEvent (the JSON schema, which also holds the cross-field
// rules the generated types cannot express), seq runs from 0 without gaps, one
// turn_id throughout, the last event is done, a failed done follows an error
// event, and the confirmation secret appears in none of them.
func checkTurnEvents(t testing.TB, raws []json.RawMessage) []map[string]any {
	t.Helper()
	if len(raws) == 0 {
		t.Fatal("no events")
	}
	out := make([]map[string]any, len(raws))
	turnID := ""
	sawError := false
	for i, raw := range raws {
		validateAgainst(t, schemaTurnEvent, raw)
		var ev conversationEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatal(err)
		}
		// The generated type decodes strictly: an unknown type or member is an error.
		var typed conversation.TurnEvent
		if err := json.Unmarshal(raw, &typed); err != nil {
			t.Fatalf("event %d does not decode as a TurnEvent: %v: %s", i, err, raw)
		}
		if strings.Contains(string(raw), "cfm_") {
			t.Fatalf("a confirmation secret is in event %d: %s", i, raw)
		}
		if ev.Seq != int64(i) {
			t.Fatalf("event %d has seq %d: the counter must run from 0 without gaps", i, ev.Seq)
		}
		if turnID == "" {
			turnID = ev.TurnID
		}
		if ev.TurnID != turnID || turnID == "" {
			t.Fatalf("event %d has turn_id %q, want %q throughout", i, ev.TurnID, turnID)
		}
		if ev.Type == "error" {
			sawError = true
		}
		if ev.Type == "done" && i != len(raws)-1 {
			t.Fatalf("done is event %d of %d: it must be last", i, len(raws))
		}
		if i == len(raws)-1 {
			if ev.Type != "done" {
				t.Fatalf("the last event is %q, want done", ev.Type)
			}
			if ev.FinishReason == "failed" && !sawError {
				t.Fatal("a failed done with no error event before it")
			}
			if ev.FinishReason != "failed" && sawError {
				t.Fatalf("an error event but finish_reason %q", ev.FinishReason)
			}
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out[i] = m
	}
	return out
}

// conversationEvent reads the envelope members every TurnEvent has.
type conversationEvent struct {
	Type         string `json:"type"`
	TurnID       string `json:"turn_id"`
	Seq          int64  `json:"seq"`
	FinishReason string `json:"finish_reason"`
}

func eventTypes(evs []map[string]any) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i], _ = ev["type"].(string)
	}
	return out
}

func findEvent(evs []map[string]any, typ string) map[string]any {
	for _, ev := range evs {
		if ev["type"] == typ {
			return ev
		}
	}
	return nil
}

// sseFrame is one server-sent event.
type sseFrame struct {
	ID    string
	Event string
	Data  string
}

func parseSSE(t testing.TB, body string) []sseFrame {
	t.Helper()
	var frames []sseFrame
	for _, block := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var f sseFrame
		for _, line := range strings.Split(block, "\n") {
			k, v, ok := strings.Cut(line, ": ")
			if !ok {
				t.Fatalf("malformed SSE line %q in %q", line, block)
			}
			switch k {
			case "id":
				f.ID = v
			case "event":
				f.Event = v
			case "data":
				if f.Data != "" {
					t.Fatalf("a frame with two data lines: %q", block)
				}
				f.Data = v
			default:
				t.Fatalf("unexpected SSE field %q", k)
			}
		}
		frames = append(frames, f)
	}
	return frames
}

var confirmTokenRE = regexp.MustCompile(`^cfm_[A-Za-z0-9_-]{43}$`)

// rawEvents returns the events of a JSON turn response.
func rawEvents(t testing.TB, body TurnBody) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, len(body.Events))
	for i, ev := range body.Events {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = b
	}
	return out
}

// ---- the required tests ----

// SEC-025: a session cannot be created, resumed or ended without a verified
// context token, and the session id is the one the token carries.
func TestSessionCreateRequiresToken(t *testing.T) {
	e := newAPIEnv(t)

	for name, tc := range map[string]struct {
		token   string
		path    string
		headers []string
	}{
		"no credential":      {token: ""},
		"malformed":          {token: "not.a.token"},
		"unknown signer":     {token: identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: apiApp, Clock: e.clk, KeyID: "other-key"}).Token()},
		"token in the query": {token: "", path: "/v1/sessions?access_token=" + e.token()},
		"wrong audience":     {token: e.issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimAudience, "someone-else") })},
		"expired":            {token: e.issuer.Token(func(d *identitytest.Draft) { d.Times(-20*time.Minute, -20*time.Minute, -10*time.Minute) })},
		"basic scheme":       {token: "", headers: []string{"Authorization", "Basic " + e.token()}},
	} {
		path := tc.path
		if path == "" {
			path = "/v1/sessions"
		}
		rec := e.callAs(tc.token, http.MethodPost, path, nil, tc.headers...)
		wantUnauthorized(t, rec)
		if rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: no WWW-Authenticate challenge", name)
		}
		if n := e.sessionRows(); n != 0 {
			t.Fatalf("%s: a session row was created without a verified token (%d rows)", name, n)
		}
	}

	// A valid token creates the session the token carries, and nothing else.
	rec := e.call(http.MethodPost, "/v1/sessions", nil)
	wantStatus(t, rec, http.StatusOK)
	validateSpec(t, "Session", rec.Body.Bytes())
	body := decodeBody[SessionBody](t, rec)
	if body.SessionID != apiSess || body.Status != "active" || body.TurnCount != 0 || body.Pending != nil {
		t.Fatalf("session = %+v", body)
	}
	if e.sessionRows() != 1 {
		t.Fatalf("session rows = %d", e.sessionRows())
	}
	// The stored session is keyed by the id the token carried, under the scope
	// the token asserted.
	got, err := e.sess.Open(context.Background(), e.scopeOf())
	if err != nil || got.ID != apiSess || got.Scope.TenantID != apiTenant || got.Scope.PrincipalID != identitytest.DefaultPrincipal {
		t.Fatalf("stored session = %+v, %v", got.Scope, err)
	}
	// The deployment level of the scope is configuration and never leaves.
	for _, leak := range []string{apiDep, "deployment", apiTenant, apiWsp, identitytest.DefaultPrincipal} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the session response carries %q: %s", leak, rec.Body.String())
		}
	}

	// A later token for the same session resumes it...
	rec = e.call(http.MethodPost, "/v1/sessions", nil)
	wantStatus(t, rec, http.StatusOK)
	// ...but only as the same authenticated context (identity.CheckContinuity).
	env := wantError(t, e.call(http.MethodPost, "/v1/sessions", nil, withPrincipal("usr_mallory")), http.StatusForbidden, contracts.ErrScopeViolation)
	if levels, _ := env.Details["levels"].([]any); len(levels) != 1 || levels[0] != "principal" {
		t.Errorf("continuity details = %v, want the principal level", env.Details)
	}

	// The same token twice is the middleware's `replayed`, not a second session.
	tok := e.token(withSession("ses_other_01"))
	wantStatus(t, e.callAs(tok, http.MethodPost, "/v1/sessions", nil), http.StatusOK)
	rec = e.callAs(tok, http.MethodPost, "/v1/sessions", nil)
	env = wantError(t, rec, http.StatusUnauthorized, contracts.ErrReplayed)
	if env.Details["reason"] != "replayed" {
		t.Errorf("replay details = %v", env.Details)
	}
}

// Without an identity middleware the /v1 routes refuse everything, and the
// handler is never reached.
func TestV1RefusesWithoutIdentityMiddleware(t *testing.T) {
	e := newAPIEnv(t)
	srv, err := NewServer(Options{Clock: e.clk, Services: e.svc}) // no V1Auth
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range srv.Routes() {
		method, path, _ := strings.Cut(route, " ")
		if !strings.HasPrefix(path, "/v1/") {
			continue
		}
		path = strings.NewReplacer("{session_id}", apiSess, "{preparation_id}", "prep_x", "{application_id}", apiApp).Replace(path)
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e.token())
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with no identity middleware = %d, want 503", route, rec.Code)
		}
	}
	if e.sessionRows() != 0 {
		t.Fatal("a handler ran without an identity middleware")
	}
}

// The events of a turn are TurnEvents (schemas/conversation/turn-event), as
// server-sent events or as a JSON document, in a consistent order.
func TestTurnStreamsTypedEvents(t *testing.T) {
	e := newAPIEnv(t)
	hs := httptest.NewServer(e.srv.Handler()) // plain-HTTP loopback
	defer hs.Close()

	post := func(token string, in TurnRequest, accept string) *http.Response {
		t.Helper()
		b, _ := json.Marshal(in)
		req, err := http.NewRequest(http.MethodPost, hs.URL+sessionsPath(apiSess)+"/turns", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", accept)
		resp, err := hs.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	stream := func(in TurnRequest) (*http.Response, []sseFrame, []map[string]any) {
		t.Helper()
		resp := post(e.token(), in, "text/event-stream")
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %s", resp.StatusCode, raw)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("Content-Type = %q, want text/event-stream", ct)
		}
		frames := parseSSE(t, string(raw))
		raws := make([]json.RawMessage, len(frames))
		for i, f := range frames {
			raws[i] = json.RawMessage(f.Data)
		}
		evs := checkTurnEvents(t, raws)
		for i, f := range frames {
			if f.ID != itoa(i) || f.Event != evs[i]["type"] {
				t.Fatalf("frame %d = id %q event %q, want id %d event %v", i, f.ID, f.Event, i, evs[i]["type"])
			}
		}
		return resp, frames, evs
	}

	// 1. A write: prepared, awaiting the user's confirmation.
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	e.onResponse("Please confirm: create an item named widget.")
	resp, _, evs := stream(TurnRequest{Text: sayCreate, Language: "en"})
	types := eventTypes(evs)
	for _, want := range []string{"progress", "directive", "prepared_action", "block", "token", "done"} {
		if findEvent(evs, want) == nil {
			t.Errorf("no %s event in %v", want, types)
		}
	}
	tok := resp.Header.Get(HeaderConfirmationToken)
	if !confirmTokenRE.MatchString(tok) {
		t.Fatalf("%s = %q, want the confirmation secret", HeaderConfirmationToken, tok)
	}
	done := findEvent(evs, "done")
	prepared := findEvent(evs, "prepared_action")["prepared_action"].(map[string]any)
	if done["finish_reason"] != "awaiting_confirmation" || done["preparation_id"] != prepared["preparation_id"] {
		t.Errorf("done = %v, prepared = %v", done, prepared["preparation_id"])
	}
	panel := findEvent(evs, "block")["block"].(map[string]any)
	if panel["type"] != "confirmation_panel" || panel["preparation_id"] != prepared["preparation_id"] {
		t.Errorf("block = %v", panel)
	}
	if dir := findEvent(evs, "directive")["directive"].(map[string]any); dir["act"] != "request_confirmation" {
		t.Errorf("directive act = %v", dir["act"])
	}
	// Prepared, not committed.
	if e.hostCalls(adapters.OpCommit) != 0 || e.host.Effects() != 0 {
		t.Fatal("a turn committed an action")
	}

	// 2. The same confirmation as a structured input commits it, and the
	// receipt event comes before the text that reports it.
	e.model.Reset()
	e.onResponse("Done. The item widget was created.")
	_, _, evs = stream(TurnRequest{Confirmation: &TurnConfirmation{PreparationID: done["preparation_id"].(string), Token: tok}})
	types = eventTypes(evs)
	iReceipt, iToken := indexOf(types, "receipt"), indexOf(types, "token")
	if iReceipt < 0 || iToken < 0 || iReceipt > iToken {
		t.Fatalf("event order = %v: the receipt must come before the text", types)
	}
	if rc := findEvent(evs, "receipt")["receipt"].(map[string]any); rc["status"] != "committed" {
		t.Errorf("receipt = %v", rc)
	}
	if d := findEvent(evs, "done"); d["finish_reason"] != "completed" {
		t.Errorf("done = %v", d)
	}
	if e.hostCalls(adapters.OpCommit) != 1 || e.host.Effects() != 1 {
		t.Fatalf("host commits = %d, effects = %d, want one of each", e.hostCalls(adapters.OpCommit), e.host.Effects())
	}

	// 3. A refusal is an error event before a failed done, never a quiet success.
	e.model.Reset()
	e.onProposal(apiProposal("", "out_of_scope", nil))
	_, _, evs = stream(TurnRequest{Text: "what is the weather like in Rome tomorrow", Language: "en"})
	errEv := findEvent(evs, "error")
	if errEv == nil || findEvent(evs, "done")["finish_reason"] != "failed" {
		t.Fatalf("a refused turn = %v", eventTypes(evs))
	}
	if code := errEv["error"].(map[string]any)["code"]; code != "unknown_capability" {
		t.Errorf("error code = %v", code)
	}

	// 4. As a JSON document the same turn carries the same events, and the
	// secret in a member of its own.
	e.model.Reset()
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "gadget", "quantity": 2}))
	e.onResponse("Please confirm: create an item named gadget.")
	rec := e.turn(TurnRequest{Text: "create an item named gadget with quantity 2", Language: "en"})
	wantStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	validateSpec(t, "TurnResponse", rec.Body.Bytes())
	body := decodeBody[TurnBody](t, rec)
	if body.Confirmation == nil || !confirmTokenRE.MatchString(body.Confirmation.Token) {
		t.Fatalf("confirmation = %+v", body.Confirmation)
	}
	evs = checkTurnEvents(t, rawEvents(t, body))
	if findEvent(evs, "prepared_action") == nil || rec.Header().Get(HeaderConfirmationToken) != "" {
		t.Errorf("events = %v, header = %q", eventTypes(evs), rec.Header().Get(HeaderConfirmationToken))
	}
	if body.TurnID != evs[0]["turn_id"] {
		t.Errorf("turn_id = %q, events say %v", body.TurnID, evs[0]["turn_id"])
	}

	// 5. A turn that could not be run at all is an error response, never a
	// stream, even when a stream was asked for.
	resp = post(e.token(), TurnRequest{Text: "hello", Language: "xx-invalid"}, "text/event-stream")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("an unrunnable turn = %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), raw)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func indexOf(list []string, want string) int {
	for i, s := range list {
		if s == want {
			return i
		}
	}
	return -1
}

// ---- the OpenAPI document ----

const apiSpecRoot = "../../schemas/api"

// specAreas are the areas the root must list: the planned areas of the public
// API, implemented or not. A later task fills its fragment and never the root.
var specAreas = []string{"sessions", "actions", "apps", "rag", "voice", "settings", "history", "files", "ops", "providers", "oauth", "decision"}

type specOp struct {
	method, path, id string
	op               map[string]any
	area             string
}

func (o specOp) route() string { return o.method + " " + o.path }

// loadYAMLFile parses a YAML (or JSON) file into generic values. An empty
// file is nil.
func loadYAMLFile(t testing.TB, file string) any {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	var doc any
	if strings.HasSuffix(file, ".json") {
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		return doc
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	return doc
}

func asMap(t testing.TB, v any, what string) map[string]any {
	t.Helper()
	if v == nil {
		return map[string]any{}
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want a mapping", what, v)
	}
	return m
}

// loadSpec reads the root and every fragment it lists and returns the merged
// operations. It fails on anything that makes the document unmergeable.
func loadSpec(t testing.TB) (root map[string]any, ops []specOp) {
	t.Helper()
	root = asMap(t, loadYAMLFile(t, filepath.Join(apiSpecRoot, "openapi.yaml")), "the root")
	if v, _ := root["openapi"].(string); !strings.HasPrefix(v, "3.1.") {
		t.Fatalf("openapi = %q, want 3.1.x", v)
	}
	seenPath := map[string]string{}
	add := func(area string, paths map[string]any) {
		for p, item := range paths {
			if !strings.HasPrefix(p, "/") {
				t.Errorf("%s: path %q does not start with /", area, p)
			}
			if prev, dup := seenPath[p]; dup {
				t.Errorf("path %s is defined in both %s and %s", p, prev, area)
			}
			seenPath[p] = area
			for key, op := range asMap(t, item, area+" "+p) {
				switch key {
				case "get", "post", "put", "patch", "delete":
					o := asMap(t, op, area+" "+key+" "+p)
					id, _ := o["operationId"].(string)
					ops = append(ops, specOp{method: strings.ToUpper(key), path: p, id: id, op: o, area: area})
				case "parameters", "summary", "description", "servers":
				default:
					t.Errorf("%s %s: unexpected path item member %q", area, p, key)
				}
			}
		}
	}
	add("root", asMap(t, root["paths"], "root paths"))

	areas := asMap(t, root["x-alixai-areas"], "x-alixai-areas")
	listed := map[string]bool{}
	for name, v := range areas {
		ref, _ := asMap(t, v, "area "+name)["$ref"].(string)
		if ref != "paths/"+name+".yaml" {
			t.Errorf("area %s references %q, want paths/%s.yaml", name, ref, name)
			continue
		}
		listed[name+".yaml"] = true
		frag := asMap(t, loadYAMLFile(t, filepath.Join(apiSpecRoot, ref)), "fragment "+name)
		for k := range frag {
			if k != "paths" && k != "components" {
				t.Errorf("fragment %s has member %q: a fragment has `paths` and `components` only", name, k)
			}
		}
		add(name, asMap(t, frag["paths"], name+" paths"))
	}
	for _, a := range specAreas {
		if _, ok := areas[a]; !ok {
			t.Errorf("the root does not list the %s area", a)
		}
	}
	entries, err := os.ReadDir(filepath.Join(apiSpecRoot, "paths"))
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if !listed[en.Name()] {
			t.Errorf("paths/%s is not listed in x-alixai-areas: a fragment the root does not reference is never served", en.Name())
		}
	}
	return root, ops
}

// checkRefs asserts every $ref in a YAML document of the spec resolves: a
// local pointer to a node, or a relative file (YAML or JSON) that exists, with
// the node its pointer names.
func checkRefs(t testing.TB, file string, doc any) {
	t.Helper()
	var walk func(v any, where string)
	walk = func(v any, where string) {
		switch n := v.(type) {
		case map[string]any:
			if ref, ok := n["$ref"].(string); ok {
				if err := resolveRef(file, doc, ref); err != nil {
					t.Errorf("%s: $ref %q at %s: %v", file, ref, where, err)
				}
			}
			for k, c := range n {
				walk(c, where+"/"+k)
			}
		case []any:
			for i, c := range n {
				walk(c, where+"/"+strconv.Itoa(i))
			}
		}
	}
	walk(doc, "")
}

func resolveRef(file string, doc any, ref string) error {
	target, pointer, _ := strings.Cut(ref, "#")
	if target != "" {
		if strings.Contains(target, "://") || strings.HasPrefix(target, "urn:") {
			return errors.New("only relative file references are resolvable")
		}
		other := filepath.Join(filepath.Dir(file), filepath.FromSlash(target))
		raw, err := os.ReadFile(other)
		if err != nil {
			return err
		}
		doc = nil
		if strings.HasSuffix(other, ".json") {
			err = json.Unmarshal(raw, &doc)
		} else {
			err = yaml.Unmarshal(raw, &doc)
		}
		if err != nil {
			return err
		}
	}
	if pointer == "" {
		return nil
	}
	cur := doc
	for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		part = strings.NewReplacer("~1", "/", "~0", "~").Replace(part)
		m, ok := cur.(map[string]any)
		if !ok {
			return errors.New("the pointer leaves the document at " + part)
		}
		if cur, ok = m[part]; !ok {
			return errors.New("no member " + part)
		}
	}
	return nil
}

// SEC-092, SEC-054: every registered route is on the OpenAPI document and every
// operation in it has a route, in both directions; the document is
// well-formed; no forbidden surface exists; nothing is served without an
// identity. The server is built the way production builds it: every registered
// area, with its services provided through Options.Services, and the dev flag
// off. A later task that registers a route keeps this green by filling its
// fragment under schemas/api/paths and by providing its fakes in its own test
// file (registerTestServices); TestRoutesMatchOpenAPISpecWithAllAreas fails if
// an area was left unmounted.
func TestRoutesMatchOpenAPISpec(t *testing.T) { assertRoutesMatchSpec(t, newAPIEnv(t)) }

func assertRoutesMatchSpec(t *testing.T, e *apiEnv) {
	t.Helper()
	if DevSection().SpikeEndpoint {
		t.Fatal("the dev spike endpoint is on in the default configuration")
	}
	root, ops := loadSpec(t)

	// Every $ref of every document of the spec resolves.
	checkRefs(t, filepath.Join(apiSpecRoot, "openapi.yaml"), root)
	entries, _ := os.ReadDir(filepath.Join(apiSpecRoot, "paths"))
	for _, en := range entries {
		f := filepath.Join(apiSpecRoot, "paths", en.Name())
		checkRefs(t, f, loadYAMLFile(t, f))
	}

	inSpec := map[string]specOp{}
	ids := map[string]string{}
	for _, o := range ops {
		if _, dup := inSpec[o.route()]; dup {
			t.Errorf("%s is defined twice", o.route())
		}
		inSpec[o.route()] = o
		if o.id == "" {
			t.Errorf("%s has no operationId", o.route())
		} else if prev, dup := ids[o.id]; dup {
			t.Errorf("operationId %s is on both %s and %s", o.id, prev, o.route())
		}
		ids[o.id] = o.route()
		resp := asMap(t, o.op["responses"], o.route()+" responses")
		if len(resp) == 0 {
			t.Errorf("%s declares no response", o.route())
		}
		if tags, _ := o.op["tags"].([]any); len(tags) == 0 {
			t.Errorf("%s has no tag", o.route())
		}
		if strings.HasPrefix(o.path, "/v1/") {
			// Behind the identity middleware: bearer auth, never overridden to none.
			if sec, present := o.op["security"]; present {
				if list, _ := sec.([]any); len(list) == 0 {
					t.Errorf("%s turns authentication off", o.route())
				}
			}
			if _, ok := resp["401"]; !ok {
				t.Errorf("%s does not declare the 401 of the identity middleware", o.route())
			}
		} else if sec, ok := o.op["security"].([]any); !ok || len(sec) != 0 {
			t.Errorf("%s is outside /v1 and must declare security: [] (an unauthenticated probe)", o.route())
		}
		if strings.HasPrefix(o.path, "/v0/") {
			t.Errorf("%s: a dev route is on the public API", o.route())
		}
	}

	inRoutes := map[string]bool{}
	for _, route := range e.srv.Routes() {
		inRoutes[route] = true
		if _, ok := inSpec[route]; !ok {
			t.Errorf("route %q is registered but is not in the OpenAPI document", route)
		}
	}
	for route := range inSpec {
		if !inRoutes[route] {
			t.Errorf("operation %q is in the OpenAPI document but has no route", route)
		}
	}
	if len(inRoutes) < 15 {
		t.Errorf("only %d routes are registered: %v", len(inRoutes), e.srv.Routes())
	}

	// SEC-092: no route of a forbidden surface exists. The list is anchored to the
	// surface itself: the LocalAI / OpenAI-compatible and runtime-administration
	// paths at the root or directly under /v1, and the words of a CORS proxy, an
	// execution surface, an agent hub or a free-form MCP registration as a path
	// segment. Nested under an AlixAI resource (a provider or model *status*, a
	// source's tools) they are ordinary names and match nothing.
	deny := regexp.MustCompile(`(?i)^[A-Z]+ (/v1)?/(models?|backends?|chat|completions|embeddings|images|audio|tts|apply|gallery|webui|swagger|admin|v0)(/|$)|(^|/)(cors|proxy|exec|shell|agents?|mcp)(/|$)`)
	for route := range inRoutes {
		if deny.MatchString(route) {
			t.Errorf("route %q is on the deny-list of forbidden surfaces", route)
		}
	}

	// No response ever carries a CORS header, a preflight included, on any route.
	for route := range inRoutes {
		method, path, _ := strings.Cut(route, " ")
		path = strings.NewReplacer("{session_id}", apiSess, "{preparation_id}", "prep_x", "{application_id}", apiApp).Replace(path)
		for _, m := range []string{method, http.MethodOptions} {
			rec := e.callAs("", m, path, nil, "Origin", "https://evil.example", "Access-Control-Request-Method", "POST")
			for k := range rec.Header() {
				if strings.HasPrefix(strings.ToLower(k), "access-control-") {
					t.Errorf("%s %s sent %s", m, path, k)
				}
			}
		}
	}
}

// SEC-092: the M0 spike endpoint is dev-only. With the flag off it does not
// exist; with it on it is exactly one route outside /v1 that is not part of the
// public API.
func TestDevSpikeStaysOutsideThePublicAPI(t *testing.T) {
	e := newAPIEnv(t)
	base := e.srv.Routes()
	for _, r := range base {
		if strings.Contains(r, "/v0/") {
			t.Fatalf("the spike route %q is mounted with the dev flag off", r)
		}
	}
	on, err := NewServer(Options{Clock: e.clk, Dev: DevConfig{SpikeEndpoint: true}, Services: e.svc})
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, r := range base {
		have[r] = true
	}
	var extra []string
	for _, r := range on.Routes() {
		if !have[r] {
			extra = append(extra, r)
		}
	}
	if len(extra) != 1 || extra[0] != "POST /v0/dev/propose" {
		t.Fatalf("the dev flag adds %v, want exactly POST /v0/dev/propose", extra)
	}
	_, ops := loadSpec(t)
	for _, o := range ops {
		if strings.HasPrefix(o.path, "/v0/") {
			t.Errorf("%s is on the public API", o.route())
		}
	}
}

// Every /v1 route sits behind the identity middleware: none answers a request
// that carries no token, a malformed one or one signed by a stranger.
func TestEveryV1RouteRequiresIdentity(t *testing.T) {
	e := newAPIEnv(t)
	stranger := identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: apiApp, Clock: e.clk, KeyID: "stranger-key"})
	n := 0
	for _, route := range e.srv.Routes() {
		method, path, _ := strings.Cut(route, " ")
		if !strings.HasPrefix(path, "/v1/") {
			continue
		}
		path = strings.NewReplacer("{session_id}", apiSess, "{preparation_id}", "prep_x", "{application_id}", apiApp).Replace(path)
		for _, tok := range []string{"", "x.y.z", stranger.Token()} {
			wantUnauthorized(t, e.callAs(tok, method, path, "{}"))
		}
		n++
	}
	if n < 10 {
		t.Fatalf("only %d /v1 routes were checked", n)
	}
	if e.sessionRows() != 0 || e.model.Calls() != 0 || len(e.host.Calls()) != 0 {
		t.Fatal("an unauthenticated request reached a handler")
	}
}

// ---- the responses against the document that describes them ----

const specResourceID = "urn:alixai:test:openapi"

var (
	specCompOnce sync.Once
	specComp     *jsonschema.Compiler
	specCompErr  error
)

// mergedComponents returns the components of the root and of every fragment as
// one document, with every reference rewritten to something a JSON Schema
// compiler can resolve: a pointer into the merged document, or the urn $id of
// the schema file it names.
func mergedComponents(t testing.TB) []byte {
	t.Helper()
	root := asMap(t, loadYAMLFile(t, filepath.Join(apiSpecRoot, "openapi.yaml")), "the root")
	merged := map[string]any{}
	add := func(src map[string]any, file string) {
		rewriteRefs(t, src, file)
		for group, v := range asMap(t, src, "components") {
			dst, _ := merged[group].(map[string]any)
			if dst == nil {
				dst = map[string]any{}
				merged[group] = dst
			}
			for name, def := range asMap(t, v, group) {
				if _, dup := dst[name]; dup {
					t.Errorf("components.%s.%s is defined twice", group, name)
				}
				dst[name] = def
			}
		}
	}
	add(asMap(t, root["components"], "root components"), filepath.Join(apiSpecRoot, "openapi.yaml"))
	for name := range asMap(t, root["x-alixai-areas"], "areas") {
		f := filepath.Join(apiSpecRoot, "paths", name+".yaml")
		frag := asMap(t, loadYAMLFile(t, f), name)
		add(asMap(t, frag["components"], name+" components"), f)
	}
	b, err := json.Marshal(map[string]any{"components": merged})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func rewriteRefs(t testing.TB, v any, file string) {
	t.Helper()
	switch n := v.(type) {
	case map[string]any:
		if ref, ok := n["$ref"].(string); ok {
			target, pointer, _ := strings.Cut(ref, "#")
			switch {
			case target == "":
			case strings.HasSuffix(target, "openapi.yaml"):
				n["$ref"] = "#" + pointer
			case strings.HasSuffix(target, ".json"):
				raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), filepath.FromSlash(target)))
				if err != nil {
					t.Fatal(err)
				}
				var doc struct {
					ID string `json:"$id"`
				}
				if err := json.Unmarshal(raw, &doc); err != nil || doc.ID == "" {
					t.Fatalf("%s has no $id: %v", target, err)
				}
				if pointer != "" {
					n["$ref"] = doc.ID + "#" + pointer
				} else {
					n["$ref"] = doc.ID
				}
			}
		}
		for _, c := range n {
			rewriteRefs(t, c, file)
		}
	case []any:
		for _, c := range n {
			rewriteRefs(t, c, file)
		}
	}
}

// validateSpec checks a response body against a named schema of the OpenAPI
// document (its components), so the document and the handlers cannot drift
// apart unnoticed.
func validateSpec(t testing.TB, name string, raw []byte) {
	t.Helper()
	apiSchemas(t) // the schema files the document refers to
	specCompOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		for _, area := range []string{"common", "actions", "conversation", "memory", "analyst"} {
			files, _ := filepath.Glob(filepath.Join(apiSchemaRoot, area, "*.schema.json"))
			for _, f := range files {
				b, err := os.ReadFile(f)
				if err != nil {
					specCompErr = err
					return
				}
				doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
				if err != nil {
					specCompErr = err
					return
				}
				if err := c.AddResource(doc.(map[string]any)["$id"].(string), doc); err != nil {
					specCompErr = err
					return
				}
			}
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(mergedComponents(t)))
		if err != nil {
			specCompErr = err
			return
		}
		if err := c.AddResource(specResourceID, doc); err != nil {
			specCompErr = err
			return
		}
		specComp = c
	})
	if specCompErr != nil || specComp == nil {
		t.Fatalf("the OpenAPI components cannot be compiled: %v", specCompErr)
	}
	s, err := specComp.Compile(specResourceID + "#/components/schemas/" + name)
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: %v: %s", name, err, raw)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("the response does not match the %s schema of the OpenAPI document: %v\n%s", name, err, raw)
	}
}
