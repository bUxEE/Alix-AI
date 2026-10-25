package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	decisiongen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// The tests of the decision area (D-09) run the real handlers and the real identity
// middleware over a scripted DecisionService: the engine's lifecycle counters are not
// the subject here, the surface is.

// decisionTestFake is the scripted DecisionService of one test environment.
type decisionTestFake struct {
	mu        sync.Mutex
	operators map[string]bool // principal ids the deployment lists as operators
	floor     float64
	status    map[string]decisiongen.DecisionStatus // by tenant
	cfg       map[string]DecisionProfileConfig      // by profile
	seen      []contracts.Scope
	sets      int
	authErr   error
}

func (f *decisionTestFake) record(sc contracts.Scope) {
	f.seen = append(f.seen, sc)
}

func (f *decisionTestFake) Status(_ context.Context, sc contracts.Scope) (decisiongen.DecisionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(sc)
	st, ok := f.status[sc.TenantID]
	if !ok {
		st = decisionTestStatus(0, 0)
	}
	return st, nil
}

func (f *decisionTestFake) AuthorizeOperator(_ context.Context, sc contracts.Scope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.authErr != nil {
		return f.authErr
	}
	// The deployment's own list, by principal: never the roles or capability scopes of the token.
	if f.operators[sc.PrincipalID] {
		return nil
	}
	return contracts.New(contracts.ErrPermissionDenied, "not an operator")
}

func (f *decisionTestFake) PlatformFloor() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.floor
}

func (f *decisionTestFake) Config(_ context.Context, sc contracts.Scope) ([]DecisionProfileConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(sc)
	out := make([]DecisionProfileConfig, 0, len(f.cfg))
	for _, p := range f.cfg {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Profile < out[j].Profile })
	return out, nil
}

func (f *decisionTestFake) SetConfig(_ context.Context, sc contracts.Scope, up DecisionConfigUpdate) (DecisionProfileConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(sc)
	cur, ok := f.cfg[up.Profile]
	if !ok {
		return DecisionProfileConfig{}, contracts.New(contracts.ErrValidationFailed, "the profile is not approved")
	}
	f.sets++
	cur.Mode = up.Mode
	if up.MinConfidence != nil {
		cur.MinConfidence = *up.MinConfidence
	}
	f.cfg[up.Profile] = cur
	return cur, nil
}

func (f *decisionTestFake) setCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sets
}

func (f *decisionTestFake) profile(name string) DecisionProfileConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg[name]
}

func (f *decisionTestFake) scopes() []contracts.Scope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]contracts.Scope(nil), f.seen...)
}

func decisionTestStatus(accepted int64, lowConfidence int64) decisiongen.DecisionStatus {
	st := decisiongen.DecisionStatus{
		Mode: decisiongen.DecisionStatusModeAdvisory, ProviderID: "localai-decisions",
		AcceptedCount: accepted, RejectedCount: lowConfidence, FallbackCounts: []decisiongen.DecisionStatusFallbackCount{},
		Since: "2026-01-01T00:00:00Z", GeneratedAt: "2026-01-01T00:05:00Z",
	}
	mid, rev := common.Identifier("clm-v0.1-8b"), actions.OpaqueVersion("rev-1")
	st.ModelID, st.ModelRevision = &mid, &rev
	if lowConfidence > 0 {
		st.FallbackCounts = append(st.FallbackCounts, decisiongen.DecisionStatusFallbackCount{Reason: decisiongen.FallbackReasonLowConfidence, Count: lowConfidence})
	}
	return st
}

var decisionTestFakeSets sync.Map // *apiEnv -> *decisionTestFake

func decisionFakeOf(e *apiEnv) *decisionTestFake {
	v, _ := decisionTestFakeSets.Load(e)
	return v.(*decisionTestFake)
}

func init() {
	registerTestServices("v1_decision", func(t testing.TB, e *apiEnv, s *Services) {
		f := &decisionTestFake{
			operators: map[string]bool{},
			floor:     0.8,
			status: map[string]decisiongen.DecisionStatus{
				apiTenant:   decisionTestStatus(7, 3),
				"ten_other": decisionTestStatus(900, 100),
			},
			cfg: map[string]DecisionProfileConfig{
				"lite":     {Profile: "lite", Mode: decisiongen.DecisionStatusModeOff, MinConfidence: 0.9},
				"balanced": {Profile: "balanced", Mode: decisiongen.DecisionStatusModeShadow, MinConfidence: 0.85},
			},
		}
		decisionTestFakeSets.Store(e, f)
		if err := Provide[DecisionService](s, f); err != nil {
			t.Fatal(err)
		}
	})
}

// decisionStatusScopes is what a token needs to read the status.
var decisionStatusScopes = append([]string{DecisionCapabilityStatusRead}, defaultCaps...)

func decisionStatusCall(e *apiEnv, mods ...claims) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.call(http.MethodGet, "/v1/decision/status", nil, append([]claims{withScopes(decisionStatusScopes...)}, mods...)...)
}

// decisionOperator makes the default principal an operator of the deployment.
func decisionOperator(e *apiEnv) {
	f := decisionFakeOf(e)
	f.mu.Lock()
	f.operators[e.scopeOf().PrincipalID] = true
	f.mu.Unlock()
}

// decisionPowerful is everything a host application could assert about a token that
// is not an operator of the deployment's configuration.
func decisionPowerful() []claims {
	return []claims{
		withRoles("admin", "owner", "operator", "root"),
		withScopes(append([]string{"decision.config.write", "decision.config.read", "admin", "ops.queue.read", "settings.deployment.write"}, decisionStatusScopes...)...),
	}
}

func decisionSchemaProps(t testing.TB) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(apiSchemaRoot, "decision", "decision-status.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for k := range doc.Properties {
		out[k] = true
	}
	return out
}

// ---- the status ----

// The status is aggregate state and nothing an integrator could mistake for an
// authorisation: exactly the D-02 schema's members, no score, threshold, gate,
// candidate, key or permission anywhere in it, and a read-only route.
func TestDecisionStatusHasNoAuthorityFields(t *testing.T) {
	e := newAPIEnv(t)
	rec := decisionStatusCall(e)
	wantStatus(t, rec, http.StatusOK)
	settingsValidateSpec(t, "DecisionStatus", rec.Body.Bytes())

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	props := decisionSchemaProps(t)
	forbidden := []string{"score", "threshold", "confidence", "authoris", "authoriz", "permit", "allow", "grant", "approv", "candidate",
		"gate", "key", "secret", "tenant", "scope", "principal", "session", "commit", "decision_id", "trace", "selected"}
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				for _, w := range forbidden {
					if strings.Contains(strings.ToLower(k), w) {
						t.Errorf("%s%s: a status member that looks like authority or an internal (%q)", path, k, w)
					}
				}
				walk(c, path+k+".")
			}
		case []any:
			for _, c := range x {
				walk(c, path)
			}
		}
	}
	walk(body, "")
	for k := range body {
		if !props[k] {
			t.Errorf("member %q is not in the D-02 status schema", k)
		}
	}
	// The generated type has exactly the schema's members: nothing can be added to the wire by a service.
	rt := reflect.TypeOf(decisiongen.DecisionStatus{})
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if !props[name] {
			t.Errorf("DecisionStatus.%s is not in the schema", rt.Field(i).Name)
		}
	}
	if len(props) != rt.NumField() {
		t.Errorf("the schema has %d members, the type %d", len(props), rt.NumField())
	}
	var typed decisiongen.DecisionStatus
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&typed); err != nil {
		t.Fatal(err)
	}
	if typed.RejectedCount != 3 || typed.AcceptedCount != 7 || len(typed.FallbackCounts) != 1 {
		t.Errorf("status = %+v", typed)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}

	// Read-only: nothing writes, resets or approves through it.
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if rec := e.call(m, "/v1/decision/status", `{}`, withScopes(decisionStatusScopes...)); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /v1/decision/status = %d, want 405", m, rec.Code)
		}
	}
	// The capability is exact: no role, and no other scope, stands in for it.
	rec = e.call(http.MethodGet, "/v1/decision/status", nil, withRoles("admin", "operator"), withScopes(append([]string{"decision.status.write", "decision.status"}, defaultCaps...)...))
	env := wantError(t, rec, http.StatusForbidden, contracts.ErrPermissionDenied)
	if env.Details["capability"] != DecisionCapabilityStatusRead {
		t.Errorf("the refusal names %v", env.Details["capability"])
	}
	wantUnauthorized(t, e.callAs("", http.MethodGet, "/v1/decision/status", nil))
}

// A status that contradicts itself is an internal error, never a success-looking
// result: the rejected count is the sum of the fallback counts, every reason once.
func TestDecisionStatusMustBeConsistent(t *testing.T) {
	e := newAPIEnv(t)
	f := decisionFakeOf(e)
	bad := map[string]decisiongen.DecisionStatus{}
	st := decisionTestStatus(1, 3)
	st.RejectedCount = 4
	bad["sum"] = st
	st = decisionTestStatus(1, 3)
	st.FallbackCounts = append(st.FallbackCounts, decisiongen.DecisionStatusFallbackCount{Reason: decisiongen.FallbackReasonLowConfidence, Count: 0})
	bad["duplicate"] = st
	st = decisionTestStatus(1, 0)
	st.AcceptedCount = -1
	bad["negative"] = st
	st = decisionTestStatus(1, 0)
	st.ProviderID = ""
	bad["provider"] = st
	for name, s := range bad {
		f.mu.Lock()
		f.status[apiTenant] = s
		f.mu.Unlock()
		wantError(t, decisionStatusCall(e), http.StatusInternalServerError, contracts.ErrInternal)
		_ = name
	}
}

// ---- the configuration ----

func decisionPut(e *apiEnv, body any, mods ...claims) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.call(http.MethodPut, "/v1/decision/config", body, mods...)
}

// The configuration is for the deployment's operators and for nobody a token can
// merely claim to be: roles and capability scopes of every kind are refused, before
// the body is read, and nothing changes.
func TestDecisionConfigRequiresOperatorRole(t *testing.T) {
	e := newAPIEnv(t)
	f := decisionFakeOf(e)
	before := f.profile("lite")
	put := map[string]any{"profile": "lite", "mode": "advisory", "min_confidence": 0.95}

	// No operator configured: nobody passes, whatever the token asserts.
	for _, mods := range [][]claims{nil, decisionPowerful()} {
		wantError(t, e.call(http.MethodGet, "/v1/decision/config", nil, mods...), http.StatusForbidden, contracts.ErrPermissionDenied)
		wantError(t, decisionPut(e, put, mods...), http.StatusForbidden, contracts.ErrPermissionDenied)
		// Before the body is read: garbage is the same refusal, not a validation error.
		wantError(t, decisionPut(e, `{not json`, mods...), http.StatusForbidden, contracts.ErrPermissionDenied)
		wantError(t, decisionPut(e, map[string]any{"profile": "lite", "mode": "off", "tenant_id": "ten_other"}, mods...), http.StatusForbidden, contracts.ErrPermissionDenied)
	}
	// The M3 settings token, with every settings scope, is not an operator either.
	wantError(t, settingsCall(e, http.MethodPut, "/v1/decision/config", put), http.StatusForbidden, contracts.ErrPermissionDenied)

	// An authorizer that fails without a decision denies; it never lets a request through.
	f.mu.Lock()
	f.authErr = errors.New("operator list unreadable")
	f.mu.Unlock()
	decisionOperator(e)
	wantError(t, e.call(http.MethodGet, "/v1/decision/config", nil), http.StatusForbidden, contracts.ErrPermissionDenied)
	f.mu.Lock()
	f.authErr = nil
	f.mu.Unlock()

	if f.setCount() != 0 || f.profile("lite") != before {
		t.Fatalf("a refused request changed the configuration: %+v", f.profile("lite"))
	}

	// The operator of the deployment's own list passes, with a token that claims nothing.
	rec := e.call(http.MethodGet, "/v1/decision/config", nil)
	wantStatus(t, rec, http.StatusOK)
	settingsValidateSpec(t, "DecisionConfig", rec.Body.Bytes())
	cfg := decodeBody[DecisionConfigView](t, rec)
	if cfg.PlatformFloor != 0.8 || len(cfg.Profiles) != 2 || cfg.Profiles[0].Profile != "balanced" {
		t.Errorf("config = %+v", cfg)
	}
	rec = decisionPut(e, put)
	wantStatus(t, rec, http.StatusOK)
	settingsValidateSpec(t, "DecisionConfigResult", rec.Body.Bytes())
	res := decodeBody[DecisionConfigResult](t, rec)
	if res.Profile != (DecisionProfileConfig{Profile: "lite", Mode: decisiongen.DecisionStatusModeAdvisory, MinConfidence: 0.95}) {
		t.Errorf("result = %+v", res)
	}
	if got := f.profile("lite"); got != res.Profile {
		t.Errorf("stored %+v, answered %+v", got, res.Profile)
	}
	// Leaving the threshold out keeps it; off disables the profile.
	rec = decisionPut(e, map[string]any{"profile": "lite", "mode": "off"})
	wantStatus(t, rec, http.StatusOK)
	if got := decodeBody[DecisionConfigResult](t, rec).Profile; got.Mode != decisiongen.DecisionStatusModeOff || got.MinConfidence != 0.95 {
		t.Errorf("after off: %+v", got)
	}
	// The body is closed and typed: an identity, an unknown mode or profile, no body.
	for name, body := range map[string]any{
		"tenant":        map[string]any{"profile": "lite", "mode": "off", "tenant_id": "ten_other"},
		"unknown mode":  map[string]any{"profile": "lite", "mode": "authoritative"},
		"no mode":       map[string]any{"profile": "lite"},
		"no profile":    map[string]any{"mode": "off"},
		"bad profile":   map[string]any{"profile": "../lite", "mode": "off"},
		"null mode":     `{"profile":"lite","mode":null}`,
		"approved by":   map[string]any{"profile": "lite", "mode": "advisory", "approved": true},
		"not approved":  map[string]any{"profile": "huge", "mode": "advisory"},
		"threshold str": `{"profile":"lite","mode":"off","min_confidence":"0.9"}`,
	} {
		rec := decisionPut(e, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d: %s", name, rec.Code, rec.Body.String())
		}
	}
	wantError(t, e.call(http.MethodPost, "/v1/decision/config", `{}`), http.StatusMethodNotAllowed, contracts.ErrValidationFailed)
	wantError(t, e.call(http.MethodGet, "/v1/decision/config?tenant_id=ten_other", nil), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantUnauthorized(t, e.callAs("", http.MethodPut, "/v1/decision/config", put))
}

// A threshold is never accepted below the platform floor: the larger of the
// deployment's floor and the hard floor, which no configuration lowers.
func TestThresholdCannotBeSetBelowPlatformFloor(t *testing.T) {
	e := newAPIEnv(t)
	decisionOperator(e)
	f := decisionFakeOf(e)
	before := f.profile("balanced")

	for _, below := range []float64{0.79, 0.5, 0.1, 0, -1} {
		rec := decisionPut(e, map[string]any{"profile": "balanced", "mode": "advisory", "min_confidence": below})
		env := wantError(t, rec, http.StatusBadRequest, contracts.ErrValidationFailed)
		if env.Details["reason"] != "below_platform_floor" {
			t.Errorf("%v: details = %v", below, env.Details)
		}
	}
	wantError(t, decisionPut(e, map[string]any{"profile": "balanced", "mode": "advisory", "min_confidence": 1.01}), http.StatusBadRequest, contracts.ErrValidationFailed)
	if f.setCount() != 0 || f.profile("balanced") != before {
		t.Fatalf("a refused threshold changed the configuration: %+v", f.profile("balanced"))
	}
	// Exactly the floor and above are accepted.
	for _, ok := range []float64{0.8, 0.93, 1} {
		wantStatus(t, decisionPut(e, map[string]any{"profile": "balanced", "mode": "advisory", "min_confidence": ok}), http.StatusOK)
		if got := f.profile("balanced").MinConfidence; got != ok {
			t.Errorf("stored %v, want %v", got, ok)
		}
	}
	// A deployment that configures a floor under the hard floor is still held to the hard floor.
	f.mu.Lock()
	f.floor = 0.2
	f.mu.Unlock()
	wantError(t, decisionPut(e, map[string]any{"profile": "balanced", "mode": "advisory", "min_confidence": 0.49}), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantStatus(t, decisionPut(e, map[string]any{"profile": "balanced", "mode": "advisory", "min_confidence": 0.5}), http.StatusOK)
	rec := e.call(http.MethodGet, "/v1/decision/config", nil)
	wantStatus(t, rec, http.StatusOK)
	if got := decodeBody[DecisionConfigView](t, rec).PlatformFloor; got != decisionHardFloor {
		t.Errorf("platform_floor = %v, want the hard floor %v", got, decisionHardFloor)
	}
	// A floor that cannot be trusted refuses every write (fail closed).
	for _, bad := range []float64{0, math.NaN(), 1.5} {
		f.mu.Lock()
		f.floor = bad
		f.mu.Unlock()
		wantError(t, decisionPut(e, map[string]any{"profile": "balanced", "mode": "advisory", "min_confidence": 0.99}), http.StatusInternalServerError, contracts.ErrInternal)
	}
}

// The settings API can not change the decision layer: no route, no member, and the
// documents that try are refused whole, with nothing stored and the configuration
// and the status untouched.
func TestSettingsCannotChangeDecisionConfig(t *testing.T) {
	e := newAPIEnv(t)
	f := decisionFakeOf(e)
	cfgBefore := map[string]DecisionProfileConfig{"lite": f.profile("lite"), "balanced": f.profile("balanced")}
	stBefore := decisionStatusCall(e).Body.String()

	docs := map[string]string{
		"decision":       `{"decision":{"enabled":true}}`,
		"decision mode":  `{"decision":{"mode":"advisory","min_confidence":0.1}}`,
		"decision_layer": `{"decision_layer":{"profile":"lite","mode":"advisory"}}`,
		"scorer":         `{"scorer":{"model":"clm-v0.1-8b"}}`,
		"threshold":      `{"min_confidence":0.1}`,
	}
	for _, level := range []string{"user", "tenant", "deployment"} {
		for name, doc := range docs {
			rec := settingsPut(e, level, 0, doc)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s/%s: status %d: %s", level, name, rec.Code, rec.Body.String())
			}
		}
	}
	if settingsRows(e) != 0 || settingsEvents(e) != 0 {
		t.Errorf("refused documents left %d rows and %d events", settingsRows(e), settingsEvents(e))
	}
	// What settings do offer says nothing about the layer.
	wantStatus(t, settingsPut(e, "deployment", 0, `{"capabilities":{"disabled":["items.create"]}}`), http.StatusOK)
	for _, p := range []string{"/v1/settings/effective", "/v1/settings/deployment", "/v1/settings/providers", "/v1/settings/capabilities"} {
		rec := settingsCall(e, http.MethodGet, p, nil)
		wantStatus(t, rec, http.StatusOK)
		if s := strings.ToLower(rec.Body.String()); strings.Contains(s, "decision") || strings.Contains(s, "min_confidence") {
			t.Errorf("%s mentions the decision layer: %s", p, rec.Body.String())
		}
	}
	for _, r := range e.srv.AreaRoutes("v1_settings") {
		if strings.Contains(r, "decision") {
			t.Errorf("settings route %s reaches the decision layer", r)
		}
	}
	// A settings administrator is not an operator: the configuration route stays shut to that token.
	wantError(t, settingsCall(e, http.MethodPut, "/v1/decision/config", map[string]any{"profile": "lite", "mode": "advisory"}), http.StatusForbidden, contracts.ErrPermissionDenied)
	wantError(t, settingsCall(e, http.MethodGet, "/v1/decision/config", nil), http.StatusForbidden, contracts.ErrPermissionDenied)

	for name, before := range cfgBefore {
		if got := f.profile(name); got != before {
			t.Errorf("profile %s changed: %+v -> %+v", name, before, got)
		}
	}
	if f.setCount() != 0 {
		t.Errorf("%d configuration writes", f.setCount())
	}
	if after := decisionStatusCall(e).Body.String(); after != stBefore {
		t.Errorf("the status moved:\n%s\n%s", stBefore, after)
	}
}

// ---- tenant isolation ----

// The scope of every call is the verified token's alone: each tenant reads its own
// counters, and a tenant, workspace, principal or application named in a query is
// refused rather than honoured or ignored.
func TestDecisionStatusIsTenantScoped(t *testing.T) {
	e := newAPIEnv(t)
	f := decisionFakeOf(e)

	var acme, other decisiongen.DecisionStatus
	rec := decisionStatusCall(e)
	wantStatus(t, rec, http.StatusOK)
	acme = decodeBody[decisiongen.DecisionStatus](t, rec)
	rec = decisionStatusCall(e, withTenant("ten_other"))
	wantStatus(t, rec, http.StatusOK)
	other = decodeBody[decisiongen.DecisionStatus](t, rec)
	if acme.AcceptedCount != 7 || acme.RejectedCount != 3 || other.AcceptedCount != 900 || other.RejectedCount != 100 {
		t.Fatalf("counters crossed tenants: %+v / %+v", acme, other)
	}
	// A tenant the service has never counted reads zero, not another tenant's numbers.
	rec = decisionStatusCall(e, withTenant("ten_unknown"))
	wantStatus(t, rec, http.StatusOK)
	if got := decodeBody[decisiongen.DecisionStatus](t, rec); got.AcceptedCount != 0 || got.RejectedCount != 0 {
		t.Errorf("an unknown tenant reads %+v", got)
	}

	n := len(f.scopes())
	for _, q := range []string{"?tenant_id=ten_other", "?tenant=ten_other", "?workspace_id=wsp_x", "?principal_id=usr_x", "?application_id=app_x", "?x="} {
		rec := e.call(http.MethodGet, "/v1/decision/status"+q, nil, withScopes(decisionStatusScopes...))
		wantError(t, rec, http.StatusBadRequest, contracts.ErrValidationFailed)
	}
	if len(f.scopes()) != n {
		t.Error("the service was asked about a request that named an identity")
	}
	// The service saw exactly the three verified scopes.
	seen := f.scopes()
	wantTenants := []string{apiTenant, "ten_other", "ten_unknown"}
	if len(seen) != len(wantTenants) {
		t.Fatalf("service calls = %d", len(seen))
	}
	for i, sc := range seen {
		if sc.TenantID != wantTenants[i] || sc.ApplicationID != e.appID {
			t.Errorf("call %d scope = %+v", i, sc)
		}
	}
	// Nothing of another tenant is in the answer.
	if body := decisionStatusCall(e).Body.String(); strings.Contains(body, "ten_") {
		t.Errorf("the status names a tenant: %s", body)
	}
}

// ---- the inventory ----

func TestDecisionRoutesAreTheDocumentedOnes(t *testing.T) {
	e := newAPIEnv(t)
	got := e.srv.AreaRoutes("v1_decision")
	sort.Strings(got)
	want := []string{"GET /v1/decision/config", "GET /v1/decision/status", "PUT /v1/decision/config"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("decision routes:\n got %v\nwant %v", got, want)
	}
	for _, r := range got {
		if strings.Contains(r, "{") {
			t.Errorf("%s takes an identifier from the path: scope comes from the token alone", r)
		}
	}
}
