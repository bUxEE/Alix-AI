package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// The tests of the providers and oauth areas (X-13) run the real handlers and the real identity middleware over
// a scripted ProvidersService: the credential service, the OAuth engine and the budget ledger are not the
// subject here, the surface is. Nothing here reaches a network or a real provider.

const providersTestSecret = "sk-live-TOPSECRET-0123456789abcdef"

// providersTestFake is the scripted ProvidersService (and ProvidersOAuthFlowService) of one test environment.
type providersTestFake struct {
	mu        sync.Mutex
	operators map[string]ProvidersOperatorLevel // principal ids the deployment lists as operators
	authErr   error
	calls     []string // the service methods called besides AuthorizeOperator
	scopes    []contracts.Scope
	keys      []string // the revealed values the service received
	keyErr    error
	pending   map[string]contracts.Scope // OAuth states by value
	flows     map[string]providersTestFlow
	modes     map[string]pv.DeploymentMode
	audit     []providersTestAudit
	kill      ProvidersKillSwitch
	killBy    []string
	acks      []string
}

type providersTestAudit struct {
	Principal, Tenant string
	Level             ProvidersModeLevel
	Previous, Mode    pv.DeploymentMode
}

func (f *providersTestFake) enter(method string, sc contracts.Scope) {
	f.calls = append(f.calls, method)
	f.scopes = append(f.scopes, sc)
}

func (f *providersTestFake) AuthorizeOperator(_ context.Context, sc contracts.Scope, level ProvidersOperatorLevel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.authErr != nil {
		return f.authErr
	}
	// The deployment's own list, by principal: never the roles or capability scopes of the token.
	have, ok := f.operators[sc.PrincipalID]
	switch {
	case !ok:
	case have == ProvidersOperatorDeployment:
		return nil
	case level == ProvidersOperatorTenant:
		return nil
	}
	return contracts.New(contracts.ErrPermissionDenied, "not an operator")
}

func providersLoad[T any](t testing.TB, rel string) T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(apiSchemaRoot, "providers", "examples", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *providersTestFake) Connections(_ context.Context, sc contracts.Scope) ([]pv.ConnectionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("Connections", sc)
	return []pv.ConnectionStatus{providersLoad[pv.ConnectionStatus](providersT, "connection-status/valid/connected-api-key.json")}, nil
}

// providersT is the test the fixtures are loaded for (set by providersEnv).
var providersT testing.TB

func (f *providersTestFake) OAuthStart(_ context.Context, sc contracts.Scope, req ProvidersOAuthStart) (ProvidersOAuthStarted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("OAuthStart", sc)
	state := "st-" + strings.Repeat("a", 20)
	f.pending[state] = sc
	out := ProvidersOAuthStarted{
		AuthorizationURL: "https://idp.example.test/authorize?client_id=alixai&code_challenge=CH&code_challenge_method=S256&state=" + state + "&nonce=N",
		RedirectURI:      req.RedirectURI,
		ExpiresAt:        time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC),
		Consent: ProvidersOAuthConsent{
			ProviderName: "Example", Scopes: []ProvidersOAuthScope{{Name: "model.request", Description: "Send model requests", DataAccess: "prompts"}},
			DataAccess: []string{"prompts you send"},
		},
	}
	if req.Presentation == ProvidersOAuthPresentationPopup {
		id := "oaf_01J0000000000000000000000T"
		f.flows[id] = providersTestFlow{scope: sc, provider: req.Provider, status: "pending", updated: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
		out.FlowID = id
		out.PollIntervalMS = 1000
	}
	return out, nil
}

type providersTestFlow struct {
	scope    contracts.Scope
	provider pv.ProviderID
	status   string
	updated  time.Time
}

func (f *providersTestFake) OAuthFlowStatus(_ context.Context, sc contracts.Scope, flowID string) (ProvidersOAuthFlowStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("OAuthFlowStatus", sc)
	fl, ok := f.flows[flowID]
	if !ok || fl.scope.PrincipalID != sc.PrincipalID || fl.scope.TenantID != sc.TenantID || fl.scope.ApplicationID != sc.ApplicationID || fl.scope.WorkspaceID != sc.WorkspaceID {
		return ProvidersOAuthFlowStatus{}, contracts.New(contracts.ErrValidationFailed, "no such flow").WithDetails("reason", "not_found")
	}
	return ProvidersOAuthFlowStatus{FlowID: flowID, Provider: fl.provider, Status: fl.status, UpdatedAt: fl.updated}, nil
}

func (f *providersTestFake) OAuthFlowCancel(_ context.Context, sc contracts.Scope, flowID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("OAuthFlowCancel", sc)
	fl, ok := f.flows[flowID]
	if !ok || fl.scope.PrincipalID != sc.PrincipalID || fl.scope.TenantID != sc.TenantID || fl.scope.ApplicationID != sc.ApplicationID || fl.scope.WorkspaceID != sc.WorkspaceID {
		return contracts.New(contracts.ErrValidationFailed, "no such flow").WithDetails("reason", "not_found")
	}
	if fl.status == "pending" {
		fl.status = "cancelled"
		fl.updated = time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
		f.flows[flowID] = fl
	}
	return nil
}

func (f *providersTestFake) OAuthCallback(_ context.Context, sc contracts.Scope, cb ProvidersOAuthCallback) (pv.ConnectionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("OAuthCallback", sc)
	state := cb.Query.Get("state")
	owner, ok := f.pending[state]
	delete(f.pending, state) // spent by the first call, whatever its outcome
	if !ok || owner.PrincipalID != sc.PrincipalID || owner.TenantID != sc.TenantID {
		// A real engine's message names no state and no code; this one deliberately quotes both, to prove the
		// gateway drops whatever the service says.
		return pv.ConnectionStatus{}, contracts.New(contracts.ErrValidationFailed, "state "+state+" code "+cb.Query.Get("code")+" does not match")
	}
	return providersLoad[pv.ConnectionStatus](providersT, "connection-status/valid/connected-oauth.json"), nil
}

func (f *providersTestFake) Disconnect(_ context.Context, sc contracts.Scope, _ pv.ProviderID) (pv.ConnectionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("Disconnect", sc)
	return providersLoad[pv.ConnectionStatus](providersT, "connection-status/valid/not-connected.json"), nil
}

func (f *providersTestFake) SubmitCredential(_ context.Context, sc contracts.Scope, _ pv.ProviderID, in ProvidersCredentialInput) (pv.ConnectionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("SubmitCredential", sc)
	f.keys = append(f.keys, in.Key.Reveal())
	if f.keyErr != nil {
		return pv.ConnectionStatus{}, f.keyErr
	}
	return providersLoad[pv.ConnectionStatus](providersT, "connection-status/valid/connected-api-key.json"), nil
}

func (f *providersTestFake) AcknowledgeBillingNotice(_ context.Context, sc contracts.Scope, _ pv.ProviderID, in ProvidersBillingAckInput) (pv.BillingNoticeAck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("AcknowledgeBillingNotice", sc)
	ack := providersLoad[pv.BillingNoticeAck](providersT, "billing-notice-ack/valid/tenant-scope.json")
	// From the verified scope, never from the body.
	ack.AcknowledgedBy = common.Identifier(sc.PrincipalID)
	ack.NoticeVersion = in.NoticeVersion
	f.acks = append(f.acks, sc.PrincipalID)
	return ack, nil
}

func (f *providersTestFake) Disclosure(_ context.Context, sc contracts.Scope, _ pv.ProviderID, in ProvidersDisclosureInput) (pv.EgressDisclosure, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("Disclosure", sc)
	return providersLoad[pv.EgressDisclosure](providersT, "egress-disclosure/valid/external-with-excerpts.json"), nil
}

func (f *providersTestFake) modeKey(level ProvidersModeLevel, sc contracts.Scope) string {
	switch level {
	case ProvidersModeTenant:
		return "t/" + sc.ApplicationID + "/" + sc.TenantID
	case ProvidersModeApplication:
		return "a/" + sc.ApplicationID
	}
	return "d"
}

func (f *providersTestFake) view(sc contracts.Scope) ProvidersModeView {
	v := ProvidersModeView{Effective: pv.DeploymentModeLocal, Configured: pv.DeploymentModeLocal, DecidedBy: "default", Levels: map[ProvidersModeLevel]pv.DeploymentMode{}}
	for _, l := range []ProvidersModeLevel{ProvidersModeDeployment, ProvidersModeApplication, ProvidersModeTenant} {
		if m, ok := f.modes[f.modeKey(l, sc)]; ok {
			v.Levels[l] = m
			v.Effective, v.Configured, v.DecidedBy = m, m, string(l)
		}
	}
	return v
}

func (f *providersTestFake) Mode(_ context.Context, sc contracts.Scope) (ProvidersModeView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("Mode", sc)
	return f.view(sc), nil
}

func (f *providersTestFake) SetMode(_ context.Context, sc contracts.Scope, level ProvidersModeLevel, mode pv.DeploymentMode) (ProvidersModeView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("SetMode", sc)
	k := f.modeKey(level, sc)
	prev, ok := f.modes[k]
	if !ok {
		prev = pv.DeploymentModeLocal
	}
	f.modes[k] = mode
	f.audit = append(f.audit, providersTestAudit{Principal: sc.PrincipalID, Tenant: sc.TenantID, Level: level, Previous: prev, Mode: mode})
	return f.view(sc), nil
}

func (f *providersTestFake) EgressPolicy(_ context.Context, sc contracts.Scope, level pv.EgressLevel, _ pv.SourceID) (pv.EgressPolicy, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("EgressPolicy", sc)
	if level == pv.EgressLevelTenant {
		return pv.EgressPolicy{}, false, nil
	}
	return providersLoad[pv.EgressPolicy](providersT, "egress-policy/valid/deployment-public-only.json"), true, nil
}

func (f *providersTestFake) PutEgressPolicy(_ context.Context, sc contracts.Scope, _ ProvidersEgressPolicyInput) (pv.EgressPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("PutEgressPolicy", sc)
	return providersLoad[pv.EgressPolicy](providersT, "egress-policy/valid/deployment-public-only.json"), nil
}

func (f *providersTestFake) Budgets(_ context.Context, sc contracts.Scope) (ProvidersBudgetsView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("Budgets", sc)
	return ProvidersBudgetsView{
		Budgets: []pv.Budget{providersLoad[pv.Budget](providersT, "budget/valid/tenant-interactive.json")},
		States:  []pv.BudgetState{providersLoad[pv.BudgetState](providersT, "budget-state/valid/ok.json")},
	}, nil
}

func (f *providersTestFake) PutBudget(_ context.Context, sc contracts.Scope, b pv.Budget) (pv.Budget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("PutBudget", sc)
	return b, nil
}

func (f *providersTestFake) Usage(_ context.Context, sc contracts.Scope, q ProvidersUsageQuery) (ProvidersUsageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("Usage", sc)
	if q.CSV {
		return ProvidersUsageResult{ContentType: "text/csv; charset=utf-8", Body: []byte("usage_id,provider\n")}, nil
	}
	return ProvidersUsageResult{ContentType: "application/json", Body: []byte(`{"rows":[],"totals":[]}`)}, nil
}

func (f *providersTestFake) KillSwitch(_ context.Context, sc contracts.Scope) (ProvidersKillSwitch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("KillSwitch", sc)
	return f.kill, nil
}

func (f *providersTestFake) SetKillSwitch(_ context.Context, sc contracts.Scope, engage bool, reason string) (ProvidersKillSwitch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enter("SetKillSwitch", sc)
	if engage && reason == "" {
		reason = "operator_emergency"
	}
	f.kill = ProvidersKillSwitch{Engaged: engage, Reason: reason, ChangedBy: sc.PrincipalID}
	f.killBy = append(f.killBy, sc.PrincipalID)
	return f.kill, nil
}

func (f *providersTestFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

var providersFakeSets sync.Map // *apiEnv -> *providersTestFake

func providersFakeOf(e *apiEnv) *providersTestFake {
	v, _ := providersFakeSets.Load(e)
	return v.(*providersTestFake)
}

func init() {
	registerTestServices("v1_providers", func(t testing.TB, e *apiEnv, s *Services) {
		f := &providersTestFake{
			operators: map[string]ProvidersOperatorLevel{},
			pending:   map[string]contracts.Scope{},
			flows:     map[string]providersTestFlow{},
			modes:     map[string]pv.DeploymentMode{},
		}
		providersFakeSets.Store(e, f)
		if err := Provide[ProvidersService](s, f); err != nil {
			t.Fatal(err)
		}
		if err := Provide[ProvidersOAuthFlowService](s, f); err != nil {
			t.Fatal(err)
		}
	})
}

func providersEnv(t *testing.T) (*apiEnv, *providersTestFake) {
	t.Helper()
	providersT = t
	e := newAPIEnv(t)
	return e, providersFakeOf(e)
}

// providersOperator makes the default principal an operator at the level.
func providersOperator(e *apiEnv, level ProvidersOperatorLevel) {
	f := providersFakeOf(e)
	f.mu.Lock()
	f.operators[e.scopeOf().PrincipalID] = level
	f.mu.Unlock()
}

// providersPowerful is everything a host application could assert about a token that is not an operator of the
// deployment's configuration.
func providersPowerful() []claims {
	return []claims{
		withRoles("admin", "owner", "operator", "root", "tenant_admin", "deployment_operator"),
		withScopes(append([]string{"providers.admin", "admin", "dataegress.application.write", "dataegress.tenant.write", "dataegress.approve", "settings.deployment.write"}, defaultCaps...)...),
	}
}

type providersRoute struct {
	name, method, path string
	body               any
	level              ProvidersOperatorLevel
}

func providersRoutes() []providersRoute {
	budget := providersLoad[map[string]any](providersT, "budget/valid/tenant-interactive.json")
	return []providersRoute{
		{"list", http.MethodGet, "/v1/providers", nil, ProvidersOperatorTenant},
		{"credential", http.MethodPut, "/v1/providers/openai/credential", map[string]any{"key": providersTestSecret}, ProvidersOperatorTenant},
		{"disconnect", http.MethodDelete, "/v1/providers/openai/connection", nil, ProvidersOperatorTenant},
		{"ack", http.MethodPost, "/v1/providers/openai/billing-notice/ack", map[string]any{"notice_version": "2026-01", "consumer_subscription_is_not_api_access": true, "separate_api_billing_understood": true}, ProvidersOperatorTenant},
		{"disclosure", http.MethodGet, "/v1/providers/openai/disclosure", nil, ProvidersOperatorTenant},
		{"mode get", http.MethodGet, "/v1/providers/mode", nil, ProvidersOperatorTenant},
		{"mode put", http.MethodPut, "/v1/providers/mode", map[string]any{"level": "tenant", "mode": "hybrid"}, ProvidersOperatorTenant},
		{"policy get", http.MethodGet, "/v1/providers/egress-policy?level=tenant", nil, ProvidersOperatorTenant},
		{"policy put", http.MethodPut, "/v1/providers/egress-policy", map[string]any{"level": "tenant", "expected_revision": 0, "enabled": false, "source_mode": "local_only"}, ProvidersOperatorTenant},
		{"budgets get", http.MethodGet, "/v1/providers/budgets", nil, ProvidersOperatorTenant},
		{"budgets put", http.MethodPut, "/v1/providers/budgets", budget, ProvidersOperatorTenant},
		{"usage", http.MethodGet, "/v1/providers/usage/export", nil, ProvidersOperatorTenant},
		{"kill get", http.MethodGet, "/v1/providers/kill-switch", nil, ProvidersOperatorDeployment},
		{"kill put", http.MethodPut, "/v1/providers/kill-switch", map[string]any{"engaged": true}, ProvidersOperatorDeployment},
		{"oauth start", http.MethodPost, "/v1/oauth/openai/start", map[string]any{"capabilities": []string{"chat"}, "redirect_uri": "http://127.0.0.1:5555/cb"}, ProvidersOperatorTenant},
		{"oauth callback", http.MethodPost, "/v1/oauth/callback", map[string]any{"redirect_uri": "http://127.0.0.1:5555/cb", "query": "code=c&state=s"}, ProvidersOperatorTenant},
	}
}

// Every route is for the deployment's operators and for nobody a token can merely claim to be: roles and
// capability scopes of every kind are refused, before the body is read, and the service does nothing. A tenant
// administrator cannot reach the deployment-level routes.
func TestProviderRoutesRequireAdminRole(t *testing.T) {
	e, f := providersEnv(t)
	for _, rt := range providersRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			// A token that merely claims every role, with a garbage body: refused before the body is read.
			rec := e.callRaw(e.token(providersPowerful()...), rt.method, rt.path, "application/json", []byte(`{not json`))
			wantError(t, rec, http.StatusForbidden, contracts.ErrPermissionDenied)
			rec = e.call(rt.method, rt.path, rt.body, providersPowerful()...)
			wantError(t, rec, http.StatusForbidden, contracts.ErrPermissionDenied)
			wantUnauthorized(t, e.callAs("", rt.method, rt.path, rt.body))
		})
	}
	if n := f.callCount(); n != 0 {
		t.Fatalf("the service was called %d times for callers that are not operators: %v", n, f.calls)
	}

	// An error that is not a typed decision denies.
	providersOperator(e, ProvidersOperatorDeployment)
	f.mu.Lock()
	f.authErr = errors.New("backend down")
	f.mu.Unlock()
	wantError(t, e.call(http.MethodGet, "/v1/providers", nil), http.StatusForbidden, contracts.ErrPermissionDenied)
	f.mu.Lock()
	f.authErr = nil
	f.mu.Unlock()

	// A tenant administrator reaches the tenant routes and not the deployment ones.
	providersOperator(e, ProvidersOperatorTenant)
	for _, rt := range providersRoutes() {
		rec := e.call(rt.method, rt.path, rt.body)
		switch rt.level {
		case ProvidersOperatorDeployment:
			wantError(t, rec, http.StatusForbidden, contracts.ErrPermissionDenied)
		default:
			if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized || rec.Code >= 500 {
				t.Errorf("%s %s as a tenant administrator = %d: %s", rt.method, rt.path, rec.Code, rec.Body.String())
			}
		}
	}
	// A deployment operator reaches everything.
	providersOperator(e, ProvidersOperatorDeployment)
	wantStatus(t, e.call(http.MethodGet, "/v1/providers/kill-switch", nil), http.StatusOK)
	wantStatus(t, e.call(http.MethodGet, "/v1/providers", nil), http.StatusOK)
}

// The credential route is the one place a secret enters the system over HTTP: it goes to the service, and the
// value is in no response, no error and no log, whether the call succeeds, is refused or is malformed.
func TestManualCredentialNeverEchoedOrLogged(t *testing.T) {
	e, f := providersEnv(t)
	providersOperator(e, ProvidersOperatorTenant)

	body := map[string]any{"key": providersTestSecret, "label": "prod", "scopes": []string{"model.request"}}
	rec := e.call(http.MethodPut, "/v1/providers/openai/credential", body)
	wantStatus(t, rec, http.StatusOK)
	settingsValidateSpec(t, "ProviderConnectionStatus", rec.Body.Bytes())
	if len(f.keys) != 1 || f.keys[0] != providersTestSecret {
		t.Fatalf("the service did not receive the value: %d keys", len(f.keys))
	}
	for _, leak := range []string{"fingerprint", "credential_ref", "ref_id", "crd_"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the response mentions %q: %s", leak, rec.Body.String())
		}
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}

	// A refusal whose message would quote the key: the client gets a fixed one.
	f.mu.Lock()
	f.keyErr = contracts.New(contracts.ErrValidationFailed, "the provider rejected key "+providersTestSecret)
	f.mu.Unlock()
	rec2 := e.call(http.MethodPut, "/v1/providers/openai/credential", body)
	wantError(t, rec2, http.StatusBadRequest, contracts.ErrValidationFailed)
	// An untyped failure is internal, still with the fixed message.
	f.mu.Lock()
	f.keyErr = errors.New("boom " + providersTestSecret)
	f.mu.Unlock()
	rec3 := e.call(http.MethodPut, "/v1/providers/openai/credential", body)
	wantError(t, rec3, http.StatusInternalServerError, contracts.ErrInternal)

	// Malformed bodies: a closed object, no unknown member, one value, a bounded size, a JSON content type.
	bad := []struct {
		name string
		raw  string
		ct   string
		code int
	}{
		{"unknown member", `{"key":"` + providersTestSecret + `","tenant_id":"ten_other"}`, "application/json", http.StatusBadRequest},
		{"truncated", `{"key":"` + providersTestSecret, "application/json", http.StatusBadRequest},
		{"not a string", `{"key":123}`, "application/json", http.StatusBadRequest},
		{"trailing value", `{"key":"` + providersTestSecret + `"} {"key":"x"}`, "application/json", http.StatusBadRequest},
		{"empty key", `{"key":""}`, "application/json", http.StatusBadRequest},
		{"wrong media type", `key=` + providersTestSecret, "text/plain", http.StatusUnsupportedMediaType},
		{"oversize", `{"key":"` + strings.Repeat("x", 9<<10) + providersTestSecret + `"}`, "application/json", http.StatusRequestEntityTooLarge},
	}
	f.mu.Lock()
	f.keyErr = nil
	f.keys = nil
	f.mu.Unlock()
	recs := []*httptest.ResponseRecorder{rec, rec2, rec3}
	for _, b := range bad {
		r := e.callRaw(e.token(), http.MethodPut, "/v1/providers/openai/credential", b.ct, []byte(b.raw))
		if r.Code != b.code {
			t.Errorf("%s: status %d, want %d: %s", b.name, r.Code, b.code, r.Body.String())
		}
		recs = append(recs, r)
	}
	// A query string on the credential route is refused, so a key cannot travel in a URL.
	r := e.call(http.MethodPut, "/v1/providers/openai/credential?key="+providersTestSecret, body)
	wantError(t, r, http.StatusBadRequest, contracts.ErrValidationFailed)
	recs = append(recs, r)
	if len(f.keys) != 0 {
		t.Errorf("a malformed or refused request reached the service: %d keys", len(f.keys))
	}
	for i, r := range recs {
		if strings.Contains(r.Body.String(), providersTestSecret) {
			t.Errorf("response %d echoes the key: %s", i, r.Body.String())
		}
		if strings.Contains(r.Body.String(), "TOPSECRET") {
			t.Errorf("response %d echoes part of the key", i)
		}
	}
	if logs := e.logs.String(); strings.Contains(logs, "TOPSECRET") || strings.Contains(logs, providersTestSecret) {
		t.Errorf("the key is in the server log:\n%s", logs)
	}

	// The in-transit type prints as a placeholder in every format.
	var s ProvidersSecret
	if err := json.Unmarshal([]byte(`"`+providersTestSecret+`"`), &s); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{
		s.String(), s.GoString(), providersMustJSON(t, s), providersMustJSON(t, ProvidersCredentialInput{Key: s}), providersFormatAll(s),
	} {
		if strings.Contains(out, "TOPSECRET") {
			t.Errorf("the secret type prints its value: %s", out)
		}
	}
}

func providersMustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func providersFormatAll(v any) string {
	var sb strings.Builder
	for _, verb := range []string{"%v", "%+v", "%s", "%#v", "%q", "%x"} {
		sb.WriteString(fmt.Sprintf(verb, v))
		sb.WriteString(fmt.Sprintf(verb, &v))
		sb.WriteString(fmt.Sprintf(verb, struct{ K ProvidersSecret }{v.(ProvidersSecret)}))
	}
	return sb.String()
}

// A callback whose state does not match is refused with a fixed message that quotes neither the state nor the
// code, the state is spent either way (a replay of the right one fails), and another principal cannot complete
// the flow.
func TestOAuthCallbackStateMismatchRejected(t *testing.T) {
	e, f := providersEnv(t)
	providersOperator(e, ProvidersOperatorTenant)
	start := e.call(http.MethodPost, "/v1/oauth/openai/start", map[string]any{"capabilities": []string{"chat"}, "redirect_uri": "http://127.0.0.1:5555/cb"})
	wantStatus(t, start, http.StatusOK)
	var started ProvidersOAuthStarted
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(started.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	cb := func(q string, mods ...claims) *httptest.ResponseRecorder {
		return e.call(http.MethodPost, "/v1/oauth/callback", map[string]any{"redirect_uri": "http://127.0.0.1:5555/cb", "query": q}, mods...)
	}

	rec := cb("code=SECRETCODE123&state=wrong-state-value")
	wantError(t, rec, http.StatusBadRequest, contracts.ErrValidationFailed)
	for _, leak := range []string{"SECRETCODE123", "wrong-state-value", state} {
		if strings.Contains(rec.Body.String(), leak) || strings.Contains(e.logs.String(), leak) {
			t.Errorf("%q leaked into the response or the log", leak)
		}
	}
	// No state at all is refused before the service.
	n := f.callCount()
	wantError(t, cb("code=c"), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantError(t, cb("%zz"), http.StatusBadRequest, contracts.ErrValidationFailed)
	if f.callCount() != n {
		t.Error("a callback without a state reached the service")
	}

	// Another principal of the same tenant cannot complete the flow, and that spends the state.
	f.mu.Lock()
	f.operators["usr_other"] = ProvidersOperatorTenant
	f.mu.Unlock()
	other := cb("code=c&state="+state, withPrincipal("usr_other"))
	wantError(t, other, http.StatusBadRequest, contracts.ErrValidationFailed)
	wantError(t, cb("code=c&state="+state), http.StatusBadRequest, contracts.ErrValidationFailed)

	// A fresh flow completes once and only once.
	start = e.call(http.MethodPost, "/v1/oauth/openai/start", map[string]any{"capabilities": []string{"chat"}, "redirect_uri": "http://127.0.0.1:5555/cb"})
	wantStatus(t, start, http.StatusOK)
	_ = json.Unmarshal(start.Body.Bytes(), &started)
	u, _ = url.Parse(started.AuthorizationURL)
	ok := cb("?code=c&state=" + u.Query().Get("state"))
	wantStatus(t, ok, http.StatusOK)
	settingsValidateSpec(t, "OAuthConnectionStatus", ok.Body.Bytes())
	wantError(t, cb("code=c&state="+u.Query().Get("state")), http.StatusBadRequest, contracts.ErrValidationFailed)
}

// The start response is the authorization URL and the consent: no verifier, secret or token anywhere, and
// nothing the body says can name a scope.
func TestOAuthStartReturnsURLWithoutSecrets(t *testing.T) {
	e, f := providersEnv(t)
	providersOperator(e, ProvidersOperatorTenant)
	rec := e.call(http.MethodPost, "/v1/oauth/openai/start", map[string]any{"capabilities": []string{"chat"}, "redirect_uri": "http://127.0.0.1:5555/cb", "label": "main"})
	wantStatus(t, rec, http.StatusOK)
	settingsValidateSpec(t, "OAuthStarted", rec.Body.Bytes())
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	au, _ := got["authorization_url"].(string)
	u, err := url.Parse(au)
	if err != nil || u.Query().Get("state") == "" || u.Query().Get("code_challenge") == "" || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization_url = %q (%v)", au, err)
	}
	for _, banned := range []string{"verifier", "client_secret", "access_token", "refresh_token", "secret", "password"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), banned) {
			t.Errorf("the start response mentions %q: %s", banned, rec.Body.String())
		}
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	// The scope is the token's: a body that names one is refused, as is a query and a malformed provider.
	wantError(t, e.call(http.MethodPost, "/v1/oauth/openai/start", map[string]any{"capabilities": []string{"chat"}, "redirect_uri": "http://127.0.0.1:5555/cb", "tenant_id": apiOther}), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantError(t, e.call(http.MethodPost, "/v1/oauth/openai/start?tenant_id="+apiOther, map[string]any{"capabilities": []string{"chat"}, "redirect_uri": "http://127.0.0.1:5555/cb"}), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantError(t, e.call(http.MethodPost, "/v1/oauth/OPEN%20AI/start", map[string]any{"capabilities": []string{"chat"}, "redirect_uri": "http://127.0.0.1:5555/cb"}), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantError(t, e.call(http.MethodPost, "/v1/oauth/openai/start", map[string]any{"capabilities": []string{}, "redirect_uri": "http://127.0.0.1:5555/cb"}), http.StatusBadRequest, contracts.ErrValidationFailed)
	for _, sc := range f.scopes {
		if sc.TenantID != apiTenant || sc.PrincipalID != e.scopeOf().PrincipalID {
			t.Errorf("the service saw scope %+v", sc)
		}
	}
}

// The disclosure lists the provider and model that would process the request and the data categories that may
// leave, and is validated against the schema.
func TestDisclosureEndpointListsProviderAndCategories(t *testing.T) {
	e, f := providersEnv(t)
	providersOperator(e, ProvidersOperatorTenant)
	rec := e.call(http.MethodGet, "/v1/providers/openai/disclosure?role=language_realization&source=src_a&source=src_b&estimated_toks_in=1200&max_output_toks=300", nil)
	wantStatus(t, rec, http.StatusOK)
	settingsValidateSpec(t, "EgressDisclosure", rec.Body.Bytes())
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"provider", "data_categories", "data_classes", "rag_excerpts_included", "cost", "on_unavailable", "terms"} {
		if _, ok := d[k]; !ok {
			t.Errorf("the disclosure has no %q: %s", k, rec.Body.String())
		}
	}
	if cats, _ := d["data_categories"].([]any); len(cats) == 0 {
		t.Errorf("no data categories listed: %s", rec.Body.String())
	}
	if f.scopes[len(f.scopes)-1].TenantID != apiTenant {
		t.Error("the disclosure was computed for another scope")
	}
	for _, q := range []string{"role=nonsense", "estimated_toks_in=-1", "max_output_toks=abc", "tenant_id=" + apiOther, "source=" + strings.Repeat("x", 80)} {
		wantError(t, e.call(http.MethodGet, "/v1/providers/openai/disclosure?"+q, nil), http.StatusBadRequest, contracts.ErrValidationFailed)
	}
}

// The kill switch is for deployment operators; engaging and releasing are recorded with the operator, and the
// reason is a fixed code.
func TestKillSwitchRoute(t *testing.T) {
	e, f := providersEnv(t)
	providersOperator(e, ProvidersOperatorTenant)
	wantError(t, e.call(http.MethodPut, "/v1/providers/kill-switch", map[string]any{"engaged": true}), http.StatusForbidden, contracts.ErrPermissionDenied)
	if f.kill.Engaged {
		t.Fatal("a tenant administrator engaged the global switch")
	}

	providersOperator(e, ProvidersOperatorDeployment)
	rec := e.call(http.MethodPut, "/v1/providers/kill-switch", map[string]any{"engaged": true, "reason": "provider_incident"})
	wantStatus(t, rec, http.StatusOK)
	settingsValidateSpec(t, "ProviderKillSwitch", rec.Body.Bytes())
	var ks ProvidersKillSwitch
	_ = json.Unmarshal(rec.Body.Bytes(), &ks)
	if !ks.Engaged || ks.Reason != "provider_incident" || ks.ChangedBy != e.scopeOf().PrincipalID {
		t.Errorf("kill switch = %+v", ks)
	}
	get := e.call(http.MethodGet, "/v1/providers/kill-switch", nil)
	wantStatus(t, get, http.StatusOK)
	if !strings.Contains(get.Body.String(), `"engaged":true`) {
		t.Errorf("the switch reads %s", get.Body.String())
	}
	for _, bad := range []any{
		map[string]any{"reason": "x"},
		map[string]any{"engaged": true, "reason": "Free text, with spaces"},
		map[string]any{"engaged": true, "tenant_id": apiOther},
		map[string]any{"engaged": "yes"},
	} {
		wantError(t, e.call(http.MethodPut, "/v1/providers/kill-switch", bad), http.StatusBadRequest, contracts.ErrValidationFailed)
	}
	wantStatus(t, e.call(http.MethodPut, "/v1/providers/kill-switch", map[string]any{"engaged": false}), http.StatusOK)
	if f.kill.Engaged || len(f.killBy) != 2 {
		t.Errorf("kill switch = %+v, changes by %v", f.kill, f.killBy)
	}
}

// The acknowledgement needs both statements and a notice version; the operator is the token's principal, never
// a member of the body.
func TestBillingNoticeAckRoute(t *testing.T) {
	e, f := providersEnv(t)
	providersOperator(e, ProvidersOperatorTenant)
	path := "/v1/providers/openai/billing-notice/ack"
	ok := map[string]any{"notice_version": "2026-01", "consumer_subscription_is_not_api_access": true, "separate_api_billing_understood": true}
	rec := e.call(http.MethodPost, path, ok)
	wantStatus(t, rec, http.StatusOK)
	settingsValidateSpec(t, "BillingNoticeAck", rec.Body.Bytes())
	var ack pv.BillingNoticeAck
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatal(err)
	}
	if string(ack.AcknowledgedBy) != e.scopeOf().PrincipalID || ack.NoticeVersion != "2026-01" {
		t.Errorf("ack = %+v", ack)
	}
	n := f.callCount()
	for name, body := range map[string]any{
		"first statement missing":  map[string]any{"notice_version": "2026-01", "consumer_subscription_is_not_api_access": false, "separate_api_billing_understood": true},
		"second statement missing": map[string]any{"notice_version": "2026-01", "consumer_subscription_is_not_api_access": true},
		"no version":               map[string]any{"consumer_subscription_is_not_api_access": true, "separate_api_billing_understood": true},
		"names the operator":       map[string]any{"notice_version": "2026-01", "consumer_subscription_is_not_api_access": true, "separate_api_billing_understood": true, "acknowledged_by": "usr_boss"},
	} {
		wantError(t, e.call(http.MethodPost, path, body), http.StatusBadRequest, contracts.ErrValidationFailed)
		_ = name
	}
	if f.callCount() != n {
		t.Error("an incomplete acknowledgement reached the service")
	}
	wantError(t, e.call(http.MethodPost, "/v1/providers/Bad_Provider/billing-notice/ack", ok), http.StatusBadRequest, contracts.ErrValidationFailed)
}

// A mode change is made by an operator of the right level, for the token's own scope only, and is recorded with
// the operator, the level and the previous and new mode; nothing in the body can name another tenant.
func TestModeChangeAuditedAndScoped(t *testing.T) {
	e, f := providersEnv(t)
	providersOperator(e, ProvidersOperatorTenant)

	// A tenant administrator sets the tenant level.
	rec := e.call(http.MethodPut, "/v1/providers/mode", map[string]any{"level": "tenant", "mode": "hybrid"})
	wantStatus(t, rec, http.StatusOK)
	settingsValidateSpec(t, "ProviderMode", rec.Body.Bytes())
	var v ProvidersModeView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Effective != pv.DeploymentModeHybrid || v.Levels[ProvidersModeTenant] != pv.DeploymentModeHybrid {
		t.Errorf("mode = %+v", v)
	}
	// ... and cannot widen it: the application and deployment levels need a deployment operator.
	for _, lvl := range []string{"application", "deployment"} {
		wantError(t, e.call(http.MethodPut, "/v1/providers/mode", map[string]any{"level": lvl, "mode": "external"}), http.StatusForbidden, contracts.ErrPermissionDenied)
	}
	// The body cannot name a tenant, a query is refused, and unknown values are refused.
	for _, bad := range []any{
		map[string]any{"level": "tenant", "mode": "external", "tenant_id": apiOther},
		map[string]any{"level": "workspace", "mode": "external"},
		map[string]any{"level": "tenant", "mode": "everything"},
		map[string]any{"mode": "external"},
	} {
		wantError(t, e.call(http.MethodPut, "/v1/providers/mode", bad), http.StatusBadRequest, contracts.ErrValidationFailed)
	}
	wantError(t, e.call(http.MethodGet, "/v1/providers/mode?tenant_id="+apiOther, nil), http.StatusBadRequest, contracts.ErrValidationFailed)

	if len(f.audit) != 1 {
		t.Fatalf("audit = %+v, want the one successful change", f.audit)
	}
	a := f.audit[0]
	if a.Principal != e.scopeOf().PrincipalID || a.Tenant != apiTenant || a.Level != ProvidersModeTenant || a.Previous != pv.DeploymentModeLocal || a.Mode != pv.DeploymentModeHybrid {
		t.Errorf("audit = %+v", a)
	}

	// A deployment operator sets the application level; the other tenant's mode did not change.
	providersOperator(e, ProvidersOperatorDeployment)
	wantStatus(t, e.call(http.MethodPut, "/v1/providers/mode", map[string]any{"level": "application", "mode": "external"}), http.StatusOK)
	other := e.call(http.MethodGet, "/v1/providers/mode", nil, withTenant(apiOther))
	wantStatus(t, other, http.StatusOK)
	var ov ProvidersModeView
	_ = json.Unmarshal(other.Body.Bytes(), &ov)
	if _, set := ov.Levels[ProvidersModeTenant]; set {
		t.Errorf("another tenant sees the tenant setting of this one: %+v", ov)
	}
	if got := f.scopes[len(f.scopes)-1].TenantID; got != apiOther {
		t.Errorf("the service answered for tenant %q", got)
	}
}

// The other operator routes take their scope from the token, refuse unknown input and pass what is valid.
func TestProviderOperatorRoutesEdgeCases(t *testing.T) {
	e, f := providersEnv(t)
	providersOperator(e, ProvidersOperatorTenant)

	list := e.call(http.MethodGet, "/v1/providers", nil)
	wantStatus(t, list, http.StatusOK)
	settingsValidateSpec(t, "ProviderList", list.Body.Bytes())
	wantError(t, e.call(http.MethodGet, "/v1/providers?tenant_id="+apiOther, nil), http.StatusBadRequest, contracts.ErrValidationFailed)

	dis := e.call(http.MethodDelete, "/v1/providers/openai/connection", nil)
	wantStatus(t, dis, http.StatusOK)
	settingsValidateSpec(t, "ProviderConnectionStatus", dis.Body.Bytes())

	// The policy of a level with none reads as null; a deployment-level write needs a deployment operator.
	none := e.call(http.MethodGet, "/v1/providers/egress-policy?level=tenant", nil)
	wantStatus(t, none, http.StatusOK)
	settingsValidateSpec(t, "EgressPolicyView", none.Body.Bytes())
	if !strings.Contains(none.Body.String(), `"policy":null`) {
		t.Errorf("a level with no policy reads %s", none.Body.String())
	}
	wantError(t, e.call(http.MethodGet, "/v1/providers/egress-policy", nil), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantError(t, e.call(http.MethodGet, "/v1/providers/egress-policy?level=source", nil), http.StatusBadRequest, contracts.ErrValidationFailed)
	put := map[string]any{"level": "deployment", "expected_revision": 0, "enabled": true, "source_mode": "local_only"}
	wantError(t, e.call(http.MethodPut, "/v1/providers/egress-policy", put), http.StatusForbidden, contracts.ErrPermissionDenied)
	providersOperator(e, ProvidersOperatorDeployment)
	wantStatus(t, e.call(http.MethodPut, "/v1/providers/egress-policy", put), http.StatusOK)

	// An allowed model is exactly a provider and a model id: the item is closed (the specification forbids other
	// members), so an extra member, a missing one or a wrong shape is refused before the service sees it.
	listed := func(item any) map[string]any {
		return map[string]any{"level": "deployment", "expected_revision": 0, "enabled": true, "source_mode": "local_only", "allowed_models": []any{item}}
	}
	wantStatus(t, e.call(http.MethodPut, "/v1/providers/egress-policy", listed(map[string]any{"provider_id": "openai", "model_id": "gpt-4.1-mini"})), http.StatusOK)
	for name, item := range map[string]any{
		"an extra member":     map[string]any{"provider_id": "openai", "model_id": "gpt-4.1-mini", "base_url": "https://evil.example"},
		"a missing model id":  map[string]any{"provider_id": "openai"},
		"a missing provider":  map[string]any{"model_id": "gpt-4.1-mini"},
		"a url as a model id": map[string]any{"provider_id": "openai", "model_id": "https://evil.example/m"},
		"not an object":       "openai/gpt-4.1-mini",
	} {
		rec := e.call(http.MethodPut, "/v1/providers/egress-policy", listed(item))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d (%s), want 400", name, rec.Code, rec.Body.String())
		}
	}

	b := e.call(http.MethodGet, "/v1/providers/budgets", nil)
	wantStatus(t, b, http.StatusOK)
	settingsValidateSpec(t, "ProviderBudgets", b.Body.Bytes())

	usage := e.call(http.MethodGet, "/v1/providers/usage/export?format=csv&scope=application&provider=openai&from=2026-01-01T00:00:00Z", nil)
	wantStatus(t, usage, http.StatusOK)
	if ct := usage.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("csv Content-Type = %q", ct)
	}
	wantStatus(t, e.call(http.MethodGet, "/v1/providers/usage/export", nil), http.StatusOK)
	for _, q := range []string{"format=xml", "scope=world", "from=yesterday", "channel=weird", "tenant_id=" + apiOther} {
		wantError(t, e.call(http.MethodGet, "/v1/providers/usage/export?"+q, nil), http.StatusBadRequest, contracts.ErrValidationFailed)
	}
	// A tenant administrator cannot export the whole application.
	providersOperator(e, ProvidersOperatorTenant)
	wantError(t, e.call(http.MethodGet, "/v1/providers/usage/export?scope=application", nil), http.StatusForbidden, contracts.ErrPermissionDenied)
	_ = f
}

// Popup start returns flow_id and poll_interval_ms; redirect start does not; opener_origin and
// loopback placeholder are validated at the gateway.
func TestOAuthPopupStartAndFlowRoutes(t *testing.T) {
	e, f := providersEnv(t)
	providersOperator(e, ProvidersOperatorTenant)

	wantError(t, e.call(http.MethodPost, "/v1/oauth/openrouter/start", map[string]any{
		"capabilities": []string{"chat"}, "redirect_uri": "http://127.0.0.1:5555/cb",
		"presentation": "popup",
	}), http.StatusBadRequest, contracts.ErrValidationFailed)

	start := e.call(http.MethodPost, "/v1/oauth/openrouter/start", map[string]any{
		"capabilities": []string{"chat"}, "redirect_uri": ProvidersOAuthLoopbackPlaceholder,
		"presentation": "popup", "callback": "loopback", "opener_origin": "http://127.0.0.1:3000",
	})
	wantStatus(t, start, http.StatusOK)
	settingsValidateSpec(t, "OAuthStarted", start.Body.Bytes())
	var started ProvidersOAuthStarted
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.FlowID == "" || started.PollIntervalMS <= 0 {
		t.Fatalf("popup start missing flow members: %+v", started)
	}

	st := e.call(http.MethodGet, "/v1/oauth/flows/"+started.FlowID, nil)
	wantStatus(t, st, http.StatusOK)
	settingsValidateSpec(t, "OAuthFlowStatus", st.Body.Bytes())

	f.mu.Lock()
	f.operators["usr_other"] = ProvidersOperatorTenant
	f.mu.Unlock()
	wantError(t, e.call(http.MethodGet, "/v1/oauth/flows/"+started.FlowID, nil, withPrincipal("usr_other")), http.StatusNotFound, contracts.ErrValidationFailed)
	wantError(t, e.call(http.MethodGet, "/v1/oauth/flows/oaf_01J0000000000000000000000Z", nil), http.StatusNotFound, contracts.ErrValidationFailed)
	wantError(t, e.call(http.MethodGet, "/v1/oauth/flows/bad", nil), http.StatusBadRequest, contracts.ErrValidationFailed)

	wantStatus(t, e.call(http.MethodDelete, "/v1/oauth/flows/"+started.FlowID, nil), http.StatusNoContent)
	wantStatus(t, e.call(http.MethodDelete, "/v1/oauth/flows/"+started.FlowID, nil), http.StatusNoContent)

	redirect := e.call(http.MethodPost, "/v1/oauth/openai/start", map[string]any{
		"capabilities": []string{"chat"}, "redirect_uri": "http://127.0.0.1:5555/cb",
	})
	wantStatus(t, redirect, http.StatusOK)
	var plain ProvidersOAuthStarted
	_ = json.Unmarshal(redirect.Body.Bytes(), &plain)
	if plain.FlowID != "" || plain.PollIntervalMS != 0 {
		t.Errorf("redirect start unexpectedly set flow members: %+v", plain)
	}
	wantError(t, e.call(http.MethodPost, "/v1/oauth/openai/start", map[string]any{
		"capabilities": []string{"chat"}, "redirect_uri": "http://127.0.0.1:5555/cb",
		"presentation": "redirect", "opener_origin": "http://127.0.0.1:3000",
	}), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantError(t, e.call(http.MethodPost, "/v1/oauth/openai/start", map[string]any{
		"capabilities": []string{"chat"}, "redirect_uri": "http://127.0.0.1:5555/cb",
		"callback": "loopback",
	}), http.StatusBadRequest, contracts.ErrValidationFailed)
}

// Without the popup-flow service the flow routes answer unavailable after the operator gate.
func TestOAuthFlowRoutesUnavailableWithoutFlowService(t *testing.T) {
	e, _ := providersEnv(t)
	providersOperator(e, ProvidersOperatorTenant)
	h := &providersHandler{apps: e.apps, svc: providersFakeOf(e), flows: nil}
	scope := e.scopeOf()
	ctx, err := identity.WithScope(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/oauth/flows/oaf_01J0000000000000000000000A", nil).WithContext(ctx)
	req.SetPathValue("flow_id", "oaf_01J0000000000000000000000A")
	rec := httptest.NewRecorder()
	h.oauthFlowStatus(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
}
