package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/settings"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The tests of the settings area (M3-07) run the real settings service over the
// same throwaway store, audit log and policy gateway as the rest of the API, behind
// the real handlers and the real identity middleware. Only the two read-outs that
// need other modules (integration-audit findings, provider health) are scripted.

// settingsTestFakes are the scripted read-outs of one test environment.
type settingsTestFakes struct {
	mu        sync.Mutex
	status    settings.AuditStatus
	health    settings.ProviderHealth
	findErr   error
	healthErr error
}

func (f *settingsTestFakes) IntegrationAudit(_ context.Context, app string) (settings.AuditStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.findErr != nil {
		return settings.AuditStatus{}, f.findErr
	}
	st := f.status
	st.ApplicationID = app
	return st, nil
}

func (f *settingsTestFakes) ProviderHealth(context.Context) (settings.ProviderHealth, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.health, f.healthErr
}

var settingsTestFakeSets sync.Map // *apiEnv -> *settingsTestFakes

func settingsFakesOf(e *apiEnv) *settingsTestFakes {
	v, _ := settingsTestFakeSets.Load(e)
	return v.(*settingsTestFakes)
}

// settingsTestPackages gives the settings service the package the application
// service serves, the way the composition root does.
type settingsTestPackages struct{ apps ApplicationService }

func (p settingsTestPackages) Package(ctx context.Context, applicationID string) (settings.Package, error) {
	pkg, err := p.apps.Package(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	return pkg, nil
}

func init() {
	registerTestServices("v1_settings", func(t testing.TB, e *apiEnv, s *Services) {
		gw, ok := Lookup[*policy.Gateway](s)
		if !ok {
			t.Fatal("the settings tests need the policy gateway")
		}
		fakes := &settingsTestFakes{status: settings.AuditStatus{State: "active", Audited: true, Outcome: "clear", Findings: []settings.AuditFinding{}},
			health: settings.ProviderHealth{Status: "ok", Roles: []settings.ProviderRole{{Role: "controller", Configured: true, Status: "ok"}}}}
		settingsTestFakeSets.Store(e, fakes)
		svc, err := settings.New(settings.Deps{
			DB: e.db, Clock: e.clk, Audit: e.alog, Policy: gw, Packages: settingsTestPackages{apps: e.apps},
			Limits: settings.Limits{ApprovedProfiles: []string{"lite", "balanced"}, DefaultProfile: "lite",
				MaxHistoryRetentionDays: 365, DefaultHistoryRetentionDays: 90, MaxAttachmentMB: 25},
			Findings: fakes, Providers: fakes,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := Provide[SettingsService](s, svc); err != nil {
			t.Fatal(err)
		}
	})
}

// settingsAllScopes is everything a settings administrator's token carries: every
// settings scope and the application capabilities of the default token.
var settingsAllScopes = append([]string{
	settings.CapabilityUserRead, settings.CapabilityUserWrite, settings.CapabilityTenantRead, settings.CapabilityTenantWrite,
	settings.CapabilityDeploymentRead, settings.CapabilityDeploymentWrite,
}, defaultCaps...)

// settingsCall sends a request with a token that carries every settings scope.
func settingsCall(e *apiEnv, method, path string, body any, mods ...claims) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.call(method, path, body, append([]claims{withScopes(settingsAllScopes...)}, mods...)...)
}

func settingsPut(e *apiEnv, level string, expected int, doc string, mods ...claims) *httptest.ResponseRecorder {
	e.t.Helper()
	return settingsCall(e, http.MethodPut, "/v1/settings/"+level, fmt.Sprintf(`{"expected_version":%d,"settings":%s}`, expected, doc), mods...)
}

func settingsRows(e *apiEnv) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM settings_documents`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func settingsEvents(e *apiEnv) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM audit_events WHERE event_type = 'settings.document_written'`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// ---- the schemas of the area, validated with a compiler that knows every area ----

var (
	settingsSpecOnce sync.Once
	settingsSpecComp *jsonschema.Compiler
	settingsSpecErr  error
)

// settingsValidateSpec validates raw against a component of the OpenAPI document.
// The shared helper (validateSpec) knows only the schema areas of M1; this one
// registers every schema under schemas/ so a component that references the
// settings schemas compiles.
func settingsValidateSpec(t testing.TB, name string, raw []byte) {
	t.Helper()
	settingsSpecOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		settingsSpecErr = filepath.WalkDir(apiSchemaRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "examples" || d.Name() == "api" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".schema.json") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
			if err != nil {
				return err
			}
			return c.AddResource(doc.(map[string]any)["$id"].(string), doc)
		})
		if settingsSpecErr != nil {
			return
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(mergedComponents(t)))
		if err != nil {
			settingsSpecErr = err
			return
		}
		settingsSpecErr = c.AddResource(specResourceID, doc)
		settingsSpecComp = c
	})
	if settingsSpecErr != nil {
		t.Fatalf("the OpenAPI components cannot be compiled: %v", settingsSpecErr)
	}
	s, err := settingsSpecComp.Compile(specResourceID + "#/components/schemas/" + name)
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

// ---- the inventory ----

// The settings area is exactly these eleven routes, and none of them is, or sits
// under, a surface the API must never have (SEC-092, SEC-067): no runtime
// administration, no model management, no agent, tool or MCP registration, no
// provider, credential, egress or budget route.
func TestSettingsRoutesAreTheDocumentedOnes(t *testing.T) {
	e := newAPIEnv(t)
	got := e.srv.AreaRoutes("v1_settings")
	sort.Strings(got)
	want := []string{
		"GET /v1/settings/capabilities", "GET /v1/settings/deployment", "GET /v1/settings/effective", "GET /v1/settings/integration-audit",
		"GET /v1/settings/profiles", "GET /v1/settings/providers", "GET /v1/settings/tenant", "GET /v1/settings/user",
		"PUT /v1/settings/deployment", "PUT /v1/settings/tenant", "PUT /v1/settings/user",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("settings routes:\n got %v\nwant %v", got, want)
	}
	banned := regexp.MustCompile(`(?i)(^|/)(admin|localai|runtime|backend|backends|gallery|webui|models?|apply|agents?|tools?|mcp|plugins?|servers?|credentials?|secrets?|tokens?|keys?|egress|external|hybrid|budgets?|quotas?|decision|scorer|clm|roles?|scopes?|permissions?|grants?|confirm(ation)?s?|oauth)(/|$)`)
	for _, route := range got {
		if banned.MatchString(route) {
			t.Errorf("%s is on the deny-list of surfaces a setting may never reach", route)
		}
		// Nothing takes a tenant, workspace, principal or application from the path.
		if strings.Contains(route, "{") {
			t.Errorf("%s takes an identifier from the path: scope comes from the token alone", route)
		}
	}
	// Only GET and PUT: nothing creates, deletes or patches a document by a path of the caller's choosing.
	for _, route := range got {
		if m, _, _ := strings.Cut(route, " "); m != http.MethodGet && m != http.MethodPut {
			t.Errorf("%s: only GET and PUT exist", route)
		}
	}
	// A path that is not one of them is the envelope's 404, whoever asks.
	for _, p := range []string{"/v1/settings", "/v1/settings/admin", "/v1/settings/localai", "/v1/settings/user/ten_globex", "/v1/settings/external", "/v1/settings/providers/openai"} {
		wantError(t, settingsCall(e, http.MethodGet, p, nil), http.StatusNotFound, contracts.ErrValidationFailed)
	}
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		for _, p := range []string{"/v1/settings/user", "/v1/settings/effective"} {
			rec := settingsCall(e, m, p, `{}`)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", m, p, rec.Code)
			}
		}
	}
}

// A server built without the settings service does not mount the area, and says
// which service it lacked: the composition root (internal/cli/cmd_settings.go)
// refuses to serve while an area is unmounted, so a deployment can never run
// half of the API.
func TestSettingsAreaNeedsItsService(t *testing.T) {
	e := newAPIEnv(t)
	// Every service the environment provides except the settings one, copied by type
	// so the test keeps working as later areas add their own.
	svc := NewServices()
	for typ, v := range e.svc.byType {
		if typ == typeOf[SettingsService]() {
			continue
		}
		svc.byType[typ] = v
	}
	srv, err := NewServer(Options{Services: svc})
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.AreaRoutes("v1_settings"); len(got) != 0 {
		t.Errorf("the area mounted %v without its service", got)
	}
	if got := strings.Join(srv.Unmounted()["v1_settings"], ","); got != "gateway.SettingsService" {
		t.Errorf("unmounted: v1_settings is missing %q, want gateway.SettingsService", got)
	}
	// The other areas are unaffected.
	if len(srv.AreaRoutes("v1_apps")) == 0 || len(srv.AreaRoutes("v1_sessions")) == 0 {
		t.Error("a missing settings service unmounted another area")
	}
}

func settingsMustLookup[T any](t testing.TB, s *Services) T {
	t.Helper()
	v, ok := Lookup[T](s)
	if !ok {
		t.Fatalf("%T is not provided", v)
	}
	return v
}

// ---- documents over HTTP ----

func TestSettingsDocumentRoundTrip(t *testing.T) {
	e := newAPIEnv(t)

	// Nothing stored yet: version 0 and the empty document, at every level.
	for _, level := range []string{"user", "tenant", "deployment"} {
		rec := settingsCall(e, http.MethodGet, "/v1/settings/"+level, nil)
		wantStatus(t, rec, http.StatusOK)
		settingsValidateSpec(t, "Settings"+strings.ToUpper(level[:1])+level[1:]+"Record", rec.Body.Bytes())
		var body struct {
			Version  int64           `json:"version"`
			Settings json.RawMessage `json:"settings"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Version != 0 || string(body.Settings) != "{}" {
			t.Errorf("%s: %s", level, rec.Body.String())
		}
	}

	// Create, read back, update, conflict.
	rec := settingsPut(e, "user", 0, `{"language":"it","voice":{"enabled":true,"speech_rate_pct":120}}`)
	wantStatus(t, rec, http.StatusOK)
	settingsValidateSpec(t, "SettingsUserRecord", rec.Body.Bytes())
	var created settings.Record
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Version != 1 || created.UpdatedBy != "usr_alice" || created.Level != settings.LevelUser || created.UpdatedAt == nil {
		t.Errorf("created = %+v", created)
	}
	if got := settingsCall(e, http.MethodGet, "/v1/settings/user", nil); got.Body.String() != rec.Body.String() {
		t.Errorf("GET differs from the PUT response:\n%s\n%s", got.Body.String(), rec.Body.String())
	}
	e.clk.Advance(1e9)
	rec = settingsPut(e, "user", 1, `{"language":"en"}`)
	wantStatus(t, rec, http.StatusOK)
	if v := decodeBody[settings.Record](t, rec).Version; v != 2 {
		t.Errorf("version = %d", v)
	}
	// A stale writer gets 409 with the current version and changes nothing.
	env := wantError(t, settingsPut(e, "user", 1, `{"language":"it"}`), http.StatusConflict, contracts.ErrConflict)
	if env.Details["current_version"] != float64(2) {
		t.Errorf("details = %v", env.Details)
	}
	if got := decodeBody[settings.Record](t, settingsCall(e, http.MethodGet, "/v1/settings/user", nil)); string(got.Settings) != `{"language":"en"}` {
		t.Errorf("stored = %s", got.Settings)
	}
	// Tenant and deployment documents.
	for level, doc := range map[string]string{
		"tenant":     `{"default_language":"it","history":{"retention_days":30},"capabilities":{"disabled":["items.create"]}}`,
		"deployment": `{"voice":{"allowed":true},"attachments":{"allowed":true,"max_file_mb":5}}`,
	} {
		rec := settingsPut(e, level, 0, doc)
		wantStatus(t, rec, http.StatusOK)
		settingsValidateSpec(t, "Settings"+strings.ToUpper(level[:1])+level[1:]+"Record", rec.Body.Bytes())
	}
	if settingsRows(e) != 3 || settingsEvents(e) != 4 { // three documents, four changes
		t.Errorf("rows = %d, events = %d", settingsRows(e), settingsEvents(e))
	}
}

func TestSettingsPutBodyIsStrict(t *testing.T) {
	e := newAPIEnv(t)
	tok := func() string { return e.token(withScopes(settingsAllScopes...)) }
	send := func(contentType, body string) *httptest.ResponseRecorder {
		return e.callRaw(tok(), http.MethodPut, "/v1/settings/user", contentType, []byte(body))
	}
	for name, tc := range map[string]struct {
		ct, body string
		status   int
		code     contracts.ErrCode
	}{
		"no expected_version":         {"application/json", `{"settings":{}}`, 400, contracts.ErrValidationFailed},
		"no settings":                 {"application/json", `{"expected_version":0}`, 400, contracts.ErrValidationFailed},
		"null settings":               {"application/json", `{"expected_version":0,"settings":null}`, 400, contracts.ErrValidationFailed},
		"a tenant in the body":        {"application/json", `{"expected_version":0,"settings":{},"tenant_id":"ten_globex"}`, 400, contracts.ErrValidationFailed},
		"a scope in the body":         {"application/json", `{"expected_version":0,"settings":{},"scope":{"tenant_id":"ten_globex"}}`, 400, contracts.ErrValidationFailed},
		"a level in the body":         {"application/json", `{"expected_version":0,"settings":{},"level":"deployment"}`, 400, contracts.ErrValidationFailed},
		"a string version":            {"application/json", `{"expected_version":"0","settings":{}}`, 400, contracts.ErrValidationFailed},
		"a float version":             {"application/json", `{"expected_version":0.5,"settings":{}}`, 400, contracts.ErrValidationFailed},
		"a negative version":          {"application/json", `{"expected_version":-1,"settings":{}}`, 400, contracts.ErrValidationFailed},
		"settings as a list":          {"application/json", `{"expected_version":0,"settings":[]}`, 400, contracts.ErrValidationFailed},
		"settings as a string":        {"application/json", `{"expected_version":0,"settings":"language=it"}`, 400, contracts.ErrValidationFailed},
		"a duplicate member inside":   {"application/json", `{"expected_version":0,"settings":{"language":"it","language":"en"}}`, 400, contracts.ErrValidationFailed},
		"trailing data":               {"application/json", `{"expected_version":0,"settings":{}} {}`, 400, contracts.ErrValidationFailed},
		"not JSON":                    {"application/json", `expected_version=0`, 400, contracts.ErrValidationFailed},
		"the wrong content type":      {"text/plain", `{"expected_version":0,"settings":{}}`, 415, contracts.ErrValidationFailed},
		"no content type":             {"", `{"expected_version":0,"settings":{}}`, 415, contracts.ErrValidationFailed},
		"a malformed language":        {"application/json", `{"expected_version":0,"settings":{"language":"IT"}}`, 400, contracts.ErrValidationFailed},
		"a language not supported":    {"application/json", `{"expected_version":0,"settings":{"language":"de"}}`, 400, contracts.ErrValidationFailed},
		"a profile not approved":      {"application/json", `{"expected_version":0,"settings":{"model_profile":"experimental-4g"}}`, 400, contracts.ErrValidationFailed},
		"retention above the ceiling": {"application/json", `{"expected_version":0,"settings":{"history":{"retention_days":3650}}}`, 400, contracts.ErrValidationFailed},
		"an undeclared capability":    {"application/json", `{"expected_version":0,"settings":{"capabilities":{"disabled":["items.ghost"]}}}`, 422, contracts.ErrUnknownCapability},
		"an admin capability":         {"application/json", `{"expected_version":0,"settings":{"capabilities":{"disabled":["admin.purge"]}}}`, 422, contracts.ErrUnknownCapability},
	} {
		rec := send(tc.ct, tc.body)
		if rec.Code != tc.status {
			t.Errorf("%s: status %d, want %d: %s", name, rec.Code, tc.status, rec.Body.String())
			continue
		}
		wantError(t, rec, tc.status, tc.code)
	}
	if settingsRows(e) != 0 || settingsEvents(e) != 0 {
		t.Errorf("refused bodies left %d rows and %d events", settingsRows(e), settingsEvents(e))
	}
	// A body past the gateway's limit is refused before it is read.
	big := `{"expected_version":0,"settings":{"language":"` + strings.Repeat("a", 2<<20) + `"}}`
	if rec := send("application/json", big); rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Errorf("a 2 MiB body = %d", rec.Code)
	}
}

// ---- the guards, over the wire ----

// Master plan 14.2: a client can not use settings to enable an undeclared tool,
// enlarge a scope, bypass confirmation or expose LocalAI administration; and the
// additive plans: external or hybrid mode, data egress, credentials, budgets, the
// decision layer. Each is refused with the envelope, names the rule it ran into,
// stores nothing, records nothing, and never repeats what the client sent.
func TestSettingsGuardsOverHTTP(t *testing.T) {
	e := newAPIEnv(t)
	const secret = "sk-live-SECRET-0123456789"
	forbidden := map[string]string{
		"tool":           `{"capabilities":{"enabled":["items.ghost"]}}`,
		"mcp":            `{"mcp_servers":[{"url":"http://127.0.0.1:9000"}]}`,
		"scope":          `{"capability_scopes":["admin.purge"],"roles":["admin"]}`,
		"tenant":         `{"tenant_id":"ten_globex"}`,
		"confirmation":   `{"confirmation":{"mode":"none"}}`,
		"skip":           `{"skip_confirmation":true}`,
		"runtime":        `{"localai":{"admin":true},"base_url":"http://127.0.0.1:8080"}`,
		"gallery":        `{"gallery":{"enabled":true}}`,
		"external_mode":  `{"mode":"external"}`,
		"hybrid":         `{"external":{"enabled":true,"mode":"hybrid"}}`,
		"egress":         `{"data_egress":{"allow":["restricted"]}}`,
		"credentials":    `{"api_key":"` + secret + `"}`,
		"budget":         `{"budget":{"daily_micro_units":99999999}}`,
		"decision_layer": `{"decision":{"enabled":true}}`,
	}
	guardOf := map[string]string{
		"tool": "tool", "mcp": "tool", "scope": "scope", "tenant": "scope", "confirmation": "confirmation", "skip": "confirmation",
		"runtime": "runtime_admin", "gallery": "runtime_admin", "external_mode": "external_mode", "hybrid": "external_mode",
		"egress": "data_egress", "credentials": "credentials", "budget": "budget", "decision_layer": "decision_layer",
	}
	// What the gateway decides for the default token, before and after.
	exposure := func() string {
		sc := e.scopeOf()
		exp := e.co // keep the coordinator referenced: the package source below is the real one
		_ = exp
		var b strings.Builder
		for _, id := range []string{capGet, capCreate, capAdmin} {
			d := settingsPolicyDecide(e, sc, id)
			fmt.Fprintf(&b, "%s:%t:%s:%d;", id, d.Allowed, d.Code, d.EffectiveRisk)
		}
		return b.String()
	}
	before := exposure()
	for _, level := range []string{"user", "tenant", "deployment"} {
		for name, doc := range forbidden {
			rec := settingsPut(e, level, 0, doc)
			env := wantError(t, rec, http.StatusBadRequest, contracts.ErrValidationFailed)
			if env.Details["guard"] != guardOf[name] {
				t.Errorf("%s/%s: guard %v, want %s", level, name, env.Details["guard"], guardOf[name])
			}
			if strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Body.String(), "127.0.0.1") {
				t.Errorf("%s/%s: the refusal repeats what the client sent: %s", level, name, rec.Body.String())
			}
		}
	}
	if settingsRows(e) != 0 || settingsEvents(e) != 0 {
		t.Errorf("forbidden documents left %d rows and %d events", settingsRows(e), settingsEvents(e))
	}
	if exposure() != before {
		t.Error("the policy gateway's decisions moved")
	}
	// After the strongest legitimate use of settings, the gateway still decides alone.
	wantStatus(t, settingsPut(e, "deployment", 0, `{"capabilities":{"disabled":["items.create"]},"confirmation":{"extra_from_risk_level":0}}`), http.StatusOK)
	wantStatus(t, settingsPut(e, "user", 0, `{"confirmation":{"extra_from_risk_level":3},"capabilities":{"disabled":["items.get"]}}`), http.StatusOK)
	if exposure() != before {
		t.Error("the policy gateway's decisions moved after valid settings were written")
	}
}

func settingsPolicyDecide(e *apiEnv, sc contracts.Scope, id string) policy.Decision {
	gw := settingsMustLookup[*policy.Gateway](e.t, e.svc)
	pkg, err := e.apps.Package(context.Background(), sc.ApplicationID)
	if err != nil {
		e.t.Fatal(err)
	}
	return gw.Decide(context.Background(), policy.Request{Scope: sc, Package: pkg, CapabilityID: id, Binding: policy.BindingOf(sc), Phase: policy.PhaseExpose})
}

// ---- permission and scope, over the wire ----

func TestSettingsPermissionsOverHTTP(t *testing.T) {
	e := newAPIEnv(t)
	userOnly := withScopes(settings.CapabilityUserRead, settings.CapabilityUserWrite, capGet, capCreate)
	read := map[string]string{"user": settings.CapabilityUserRead, "tenant": settings.CapabilityTenantRead, "deployment": settings.CapabilityDeploymentRead}
	write := map[string]string{"user": settings.CapabilityUserWrite, "tenant": settings.CapabilityTenantWrite, "deployment": settings.CapabilityDeploymentWrite}

	for level := range read {
		// The token without the scope of the level is refused, whatever else it holds.
		rec := e.call(http.MethodGet, "/v1/settings/"+level, nil, withScopes(append([]string{write[level]}, defaultCaps...)...))
		env := wantError(t, rec, http.StatusForbidden, contracts.ErrPermissionDenied)
		if env.Details["capability"] != read[level] {
			t.Errorf("%s: the refusal names %v, want %s", level, env.Details["capability"], read[level])
		}
		rec = e.callAs(e.token(withScopes(append([]string{read[level]}, defaultCaps...)...)), http.MethodPut, "/v1/settings/"+level, `{"expected_version":0,"settings":{}}`)
		wantError(t, rec, http.StatusForbidden, contracts.ErrPermissionDenied)
	}
	// A user-only token reads and writes only its own level.
	wantStatus(t, e.call(http.MethodPut, "/v1/settings/user", `{"expected_version":0,"settings":{"language":"it"}}`, userOnly), http.StatusOK)
	for _, level := range []string{"tenant", "deployment"} {
		wantError(t, e.call(http.MethodGet, "/v1/settings/"+level, nil, userOnly), http.StatusForbidden, contracts.ErrPermissionDenied)
		wantError(t, e.call(http.MethodPut, "/v1/settings/"+level, `{"expected_version":0,"settings":{}}`, userOnly), http.StatusForbidden, contracts.ErrPermissionDenied)
	}
	for _, p := range []string{"/v1/settings/integration-audit", "/v1/settings/providers"} {
		wantError(t, e.call(http.MethodGet, p, nil, userOnly), http.StatusForbidden, contracts.ErrPermissionDenied)
	}
	// A token with no settings scope at all (the default one) reads nothing.
	for _, p := range []string{"/v1/settings/user", "/v1/settings/effective", "/v1/settings/profiles", "/v1/settings/capabilities"} {
		wantError(t, e.call(http.MethodGet, p, nil), http.StatusForbidden, contracts.ErrPermissionDenied)
	}
	if settingsRows(e) != 1 {
		t.Errorf("%d rows, want 1 (the user document only)", settingsRows(e))
	}

	// The scope is the token's: another tenant, workspace, principal never reaches it.
	for name, mods := range map[string][]claims{
		"another tenant":    {withTenant(apiOther)},
		"another workspace": {withWorkspace(apiWsp2)},
		"another principal": {withPrincipal("usr_bob")},
	} {
		rec := settingsCall(e, http.MethodGet, "/v1/settings/user", nil, mods...)
		wantStatus(t, rec, http.StatusOK)
		if v := decodeBody[settings.Record](t, rec).Version; v != 0 {
			t.Errorf("%s reads the document of the default principal (version %d)", name, v)
		}
	}
	// Nothing in the query, a header or the path moves the key.
	for _, q := range []string{"?tenant_id=" + apiOther, "?workspace_id=" + apiWsp2, "?principal_id=usr_bob", "?application_id=app_other"} {
		rec := e.callAs(e.token(withScopes(settingsAllScopes...)), http.MethodGet, "/v1/settings/user"+q, nil, "X-Tenant-ID", apiOther, "X-Principal-ID", "usr_bob")
		wantStatus(t, rec, http.StatusOK)
		if v := decodeBody[settings.Record](t, rec).Version; v != 1 {
			t.Errorf("%s: read version %d, want the token's own document (1)", q, v)
		}
	}
	// Tenant and deployment documents are shared by the tenant / application and by no one else.
	wantStatus(t, settingsPut(e, "tenant", 0, `{"default_language":"it"}`), http.StatusOK)
	wantStatus(t, settingsPut(e, "deployment", 0, `{"default_language":"it"}`), http.StatusOK)
	if v := decodeBody[settings.Record](t, settingsCall(e, http.MethodGet, "/v1/settings/tenant", nil, withPrincipal("usr_bob"))).Version; v != 1 {
		t.Errorf("a principal of the same tenant reads version %d of the tenant document", v)
	}
	if v := decodeBody[settings.Record](t, settingsCall(e, http.MethodGet, "/v1/settings/tenant", nil, withTenant(apiOther))).Version; v != 0 {
		t.Errorf("another tenant reads version %d of the tenant document", v)
	}
}

func TestSettingsQuarantinedApplicationServesNoSettings(t *testing.T) {
	e := newAPIEnv(t)
	wantStatus(t, settingsPut(e, "user", 0, `{"language":"it"}`), http.StatusOK)
	settingsFakesOf(e).status = settings.AuditStatus{State: "quarantined", Audited: true, Outcome: "blocked", Counts: settings.AuditCounts{Critical: 1},
		Findings: []settings.AuditFinding{{Key: "0123456789abcdef", CheckID: "audit.x", Severity: "critical", Message: "m", Remediation: "r", Status: settings.FindingOpen}}}
	e.fapps.quarantine(true)
	for _, p := range []string{"/v1/settings/user", "/v1/settings/tenant", "/v1/settings/deployment", "/v1/settings/effective", "/v1/settings/profiles", "/v1/settings/capabilities"} {
		wantError(t, settingsCall(e, http.MethodGet, p, nil), http.StatusForbidden, contracts.ErrQuarantined)
	}
	wantError(t, settingsPut(e, "user", 1, `{"language":"en"}`), http.StatusForbidden, contracts.ErrQuarantined)
	// The findings, which say why, stay readable.
	rec := settingsCall(e, http.MethodGet, "/v1/settings/integration-audit", nil)
	wantStatus(t, rec, http.StatusOK)
	settingsValidateSpec(t, "SettingsAuditStatus", rec.Body.Bytes())
	if st := decodeBody[settings.AuditStatus](t, rec); st.Outcome != "blocked" || len(st.Findings) != 1 || st.Findings[0].Status != settings.FindingOpen {
		t.Errorf("status = %+v", st)
	}
	e.fapps.quarantine(false)
	wantStatus(t, settingsCall(e, http.MethodGet, "/v1/settings/user", nil), http.StatusOK)
}

// ---- the read-outs ----

func TestSettingsReadouts(t *testing.T) {
	e := newAPIEnv(t)
	fakes := settingsFakesOf(e)

	wantStatus(t, settingsPut(e, "deployment", 0, `{"voice":{"allowed":true},"capabilities":{"disabled":["items.create"]},"history":{"retention_days":60}}`), http.StatusOK)
	wantStatus(t, settingsPut(e, "user", 0, `{"voice":{"enabled":true},"model_profile":"balanced","confirmation":{"extra_from_risk_level":0}}`), http.StatusOK)

	t.Run("effective", func(t *testing.T) {
		rec := settingsCall(e, http.MethodGet, "/v1/settings/effective", nil)
		wantStatus(t, rec, http.StatusOK)
		settingsValidateSpec(t, "SettingsEffective", rec.Body.Bytes())
		var eff struct {
			Levels   map[string]int `json:"levels"`
			Profile  string         `json:"model_profile"`
			Voice    map[string]any `json:"voice"`
			History  map[string]any `json:"history"`
			Decided  map[string]any `json:"decided_by"`
			Capsrest map[string]any `json:"capabilities"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &eff)
		if eff.Levels["deployment"] != 1 || eff.Levels["user"] != 1 || eff.Levels["tenant"] != 0 || eff.Profile != "balanced" ||
			eff.Voice["enabled"] != true || eff.History["retention_days"] != float64(60) || eff.Decided["history.retention_days"] != "deployment" {
			t.Errorf("effective = %s", rec.Body.String())
		}
		if got := fmt.Sprint(eff.Capsrest["disabled"]); got != "[items.create]" {
			t.Errorf("disabled = %s", got)
		}
	})

	t.Run("profiles", func(t *testing.T) {
		rec := settingsCall(e, http.MethodGet, "/v1/settings/profiles", nil)
		wantStatus(t, rec, http.StatusOK)
		settingsValidateSpec(t, "SettingsProfileList", rec.Body.Bytes())
		if got := decodeBody[settings.ProfileList](t, rec); fmt.Sprint(got.Profiles) != "[{balanced false true} {lite true false}]" {
			t.Errorf("profiles = %v", got.Profiles)
		}
	})

	t.Run("capabilities", func(t *testing.T) {
		rec := settingsCall(e, http.MethodGet, "/v1/settings/capabilities", nil)
		wantStatus(t, rec, http.StatusOK)
		settingsValidateSpec(t, "SettingsCapabilityView", rec.Body.Bytes())
		view := decodeBody[settings.CapabilityView](t, rec)
		byID := map[string]settings.CapabilityState{}
		for _, c := range view.Capabilities {
			byID[c.ID] = c
		}
		// The default token holds items.get and items.create; admin.purge is admin-only and never listed.
		if len(byID) != 2 || byID[capGet].ID == "" || byID[capCreate].ID == "" {
			t.Fatalf("capabilities = %v", byID)
		}
		if byID[capCreate].Enabled || fmt.Sprint(byID[capCreate].DisabledBy) != "[deployment]" || !byID[capCreate].RequiresConfirmation || byID[capCreate].ExtraConfirmation {
			t.Errorf("items.create = %+v", byID[capCreate])
		}
		if !byID[capGet].Enabled || !byID[capGet].RequiresConfirmation || !byID[capGet].ExtraConfirmation {
			t.Errorf("items.get = %+v (the user asked for confirmation from level 0)", byID[capGet])
		}
		if !view.SettingsAccess.User.Write || !view.SettingsAccess.Deployment.Write {
			t.Errorf("settings_access = %+v", view.SettingsAccess)
		}
	})

	t.Run("integration audit", func(t *testing.T) {
		fakes.mu.Lock()
		fakes.status = settings.AuditStatus{State: "active", PackageVersion: "1.0.0", ManifestHash: strings.Repeat("a", 64), Audited: true, RunID: "run_01", Outcome: "needs_acceptance",
			Counts: settings.AuditCounts{Warnings: 1}, Findings: []settings.AuditFinding{
				{Key: "0123456789abcdef", CheckID: "audit.section_not_run", Severity: "warning", Message: "a section was not run", Remediation: "run it", Status: settings.FindingAccepted},
			}}
		fakes.mu.Unlock()
		rec := settingsCall(e, http.MethodGet, "/v1/settings/integration-audit", nil)
		wantStatus(t, rec, http.StatusOK)
		settingsValidateSpec(t, "SettingsAuditStatus", rec.Body.Bytes())
		if st := decodeBody[settings.AuditStatus](t, rec); st.ApplicationID != apiApp || st.Findings[0].Status != settings.FindingAccepted {
			t.Errorf("status = %+v", st)
		}
		fakes.mu.Lock()
		fakes.findErr = contracts.New(contracts.ErrUnavailable, "the audit store is busy")
		fakes.mu.Unlock()
		wantError(t, settingsCall(e, http.MethodGet, "/v1/settings/integration-audit", nil), http.StatusServiceUnavailable, contracts.ErrUnavailable)
	})

	t.Run("provider health names no runtime", func(t *testing.T) {
		fakes.mu.Lock()
		fakes.health = settings.ProviderHealth{Status: "degraded", Roles: []settings.ProviderRole{
			{Role: "controller", Configured: true, Status: "ok"},
			{Role: "embedder", Configured: false, Status: "unavailable", Effect: "reduced_semantic_recall"},
			{Role: "http://127.0.0.1:8080/models", Configured: true, Status: "LocalAI", Effect: "see /models/available"},
		}}
		fakes.mu.Unlock()
		rec := settingsCall(e, http.MethodGet, "/v1/settings/providers", nil)
		wantStatus(t, rec, http.StatusOK)
		settingsValidateSpec(t, "SettingsProviderHealth", rec.Body.Bytes())
		for _, banned := range []string{"localai", "LocalAI", "127.0.0.1", "http", "models"} {
			if strings.Contains(rec.Body.String(), banned) {
				t.Errorf("provider health leaks %q: %s", banned, rec.Body.String())
			}
		}
	})
}

// A change is on the audit chain and the chain stays intact.
func TestSettingsChangesAreOnTheAuditChain(t *testing.T) {
	e := newAPIEnv(t)
	wantStatus(t, settingsPut(e, "user", 0, `{"language":"it"}`), http.StatusOK)
	wantStatus(t, settingsPut(e, "tenant", 0, `{"default_language":"it"}`), http.StatusOK)
	wantStatus(t, settingsPut(e, "deployment", 0, `{"default_language":"it"}`), http.StatusOK)
	if got := settingsEvents(e); got != 3 {
		t.Fatalf("%d settings events, want 3", got)
	}
	var tenantChain, appChain int
	_ = e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM audit_events WHERE event_type = 'settings.document_written' AND application_id = ? AND tenant_id = ?`, apiApp, apiTenant).Scan(&tenantChain)
	_ = e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM audit_events WHERE event_type = 'settings.document_written' AND application_id = ? AND tenant_id = ''`, apiApp).Scan(&appChain)
	if tenantChain != 2 || appChain != 1 {
		t.Errorf("tenant chain %d, application chain %d, want 2 and 1", tenantChain, appChain)
	}
}

// Identity (ADR 0013): the scope of a settings request is the one the single-use
// context token carries. A token presented twice is replayed, on a read and on a
// write, and a replayed write stores and records nothing.
func TestSettingsTokenIsSingleUse(t *testing.T) {
	e := newAPIEnv(t)
	tok := e.token(withScopes(settingsAllScopes...))
	wantStatus(t, e.callAs(tok, http.MethodGet, "/v1/settings/user", nil), http.StatusOK)
	env := wantUnauthorized(t, e.callAs(tok, http.MethodGet, "/v1/settings/user", nil))
	if env.Code != contracts.ErrReplayed {
		t.Errorf("a replayed read = %s", env.Code)
	}
	tok = e.token(withScopes(settingsAllScopes...))
	wantStatus(t, e.callAs(tok, http.MethodPut, "/v1/settings/user", `{"expected_version":0,"settings":{"language":"it"}}`), http.StatusOK)
	wantUnauthorized(t, e.callAs(tok, http.MethodPut, "/v1/settings/user", `{"expected_version":1,"settings":{"language":"en"}}`))
	if got := decodeBody[settings.Record](t, settingsCall(e, http.MethodGet, "/v1/settings/user", nil)); string(got.Settings) != `{"language":"it"}` || got.Version != 1 {
		t.Errorf("a replayed write changed the document: %+v", got)
	}
	if settingsEvents(e) != 1 {
		t.Errorf("%d events", settingsEvents(e))
	}
}
