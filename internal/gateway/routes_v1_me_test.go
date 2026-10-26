package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

type meTestFake struct {
	mu       sync.Mutex
	calls    []string
	catalog  pv.ProviderCatalog
	account  pv.PrincipalLlm
	credStat PrincipalCredentialStatus
	lastKey  string
	lastRole pv.PrincipalLlmRole
	deny     bool
}

func (f *meTestFake) Catalog(_ context.Context, _ contracts.Scope) (pv.ProviderCatalog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Catalog")
	return f.catalog, nil
}

func (f *meTestFake) Get(_ context.Context, scope contracts.Scope) (pv.PrincipalLlm, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Get:"+scope.PrincipalID)
	a := f.account
	a.PrincipalID = common.Identifier(scope.PrincipalID)
	a.ApplicationID = common.Identifier(scope.ApplicationID)
	return a, nil
}

func (f *meTestFake) PutRole(_ context.Context, scope contracts.Scope, role pv.PrincipalLlmRole, in PrincipalLLMBindingInput) (pv.PrincipalLlm, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "PutRole")
	f.lastRole = role
	if f.deny {
		return pv.PrincipalLlm{}, contracts.New(contracts.ErrValidationFailed, "not in catalog").WithDetails("reason", "provider_not_in_catalog")
	}
	model := in.ModelID
	if model == "" {
		model = "gpt-test"
	}
	a := f.account
	a.PrincipalID = common.Identifier(scope.PrincipalID)
	a.ApplicationID = common.Identifier(scope.ApplicationID)
	a.Bindings = []pv.PrincipalLlmBinding{{
		Role:               role,
		ProfileID:          in.ProfileID,
		ProviderID:         "openai",
		ModelID:            model,
		AccountStatus:      pv.PrincipalLlmBindingAccountStatusMissing,
		BillingNoticeAcked: false,
		UpdatedAt:          common.Timestamp(time.Now().UTC().Format(time.RFC3339)),
	}}
	a.UpdatedAt = a.Bindings[0].UpdatedAt
	f.account = a
	return a, nil
}

func (f *meTestFake) DeleteRole(_ context.Context, scope contracts.Scope, role pv.PrincipalLlmRole) (pv.PrincipalLlm, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "DeleteRole:"+string(role))
	a := f.account
	a.PrincipalID = common.Identifier(scope.PrincipalID)
	a.ApplicationID = common.Identifier(scope.ApplicationID)
	a.Bindings = nil
	f.account = a
	return a, nil
}

func (f *meTestFake) SubmitCredential(_ context.Context, scope contracts.Scope, provider pv.ProviderID, in PrincipalCredentialInput) (PrincipalCredentialStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "SubmitCredential:"+scope.PrincipalID)
	f.lastKey = in.Key.Reveal()
	f.credStat = PrincipalCredentialStatus{ProviderID: string(provider), Status: "active"}
	return f.credStat, nil
}

func (f *meTestFake) DeleteCredential(_ context.Context, _ contracts.Scope, provider pv.ProviderID) (PrincipalCredentialStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "DeleteCredential")
	return PrincipalCredentialStatus{ProviderID: string(provider), Status: "revoked"}, nil
}

func (f *meTestFake) AcknowledgeBillingNotice(_ context.Context, _ contracts.Scope, provider pv.ProviderID, in PrincipalBillingAckInput) (PrincipalBillingAckResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Ack")
	return PrincipalBillingAckResult{ProviderID: string(provider), NoticeVersion: in.NoticeVersion, AcknowledgedAt: time.Now().UTC()}, nil
}

func (f *meTestFake) OAuthStart(_ context.Context, _ contracts.Scope, req PrincipalOAuthStart) (PrincipalOAuthStarted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "OAuthStart:"+string(req.Provider))
	return PrincipalOAuthStarted{
		AuthorizationURL: "https://example.test/auth",
		RedirectURI:      req.RedirectURI,
		ExpiresAt:        time.Now().UTC().Add(time.Minute),
	}, nil
}

var meFakeSets sync.Map

func init() {
	registerTestServices("v1_me", func(t testing.TB, e *apiEnv, s *Services) {
		f := &meTestFake{
			catalog:  pv.ProviderCatalog{Entries: []pv.ProviderCatalogEntry{}},
			account:  pv.PrincipalLlm{Bindings: []pv.PrincipalLlmBinding{}, UpdatedAt: common.Timestamp(time.Now().UTC().Format(time.RFC3339))},
			credStat: PrincipalCredentialStatus{ProviderID: "openai", Status: "missing"},
		}
		meFakeSets.Store(e, f)
		if err := Provide[PrincipalLLMService](s, f); err != nil {
			t.Fatal(err)
		}
	})
}

func meFakeOf(e *apiEnv) *meTestFake {
	v, _ := meFakeSets.Load(e)
	return v.(*meTestFake)
}

func TestMeRoutesMatchInventory(t *testing.T) {
	e := newAPIEnv(t)
	have := map[string]bool{}
	for _, r := range e.srv.AreaRoutes("v1_me") {
		have[r] = true
	}
	for _, r := range []string{
		"GET /v1/providers/catalog",
		"GET /v1/me/llm",
		"PUT /v1/me/llm/{role}",
		"DELETE /v1/me/llm/{role}",
		"PUT /v1/me/providers/{provider}/credential",
		"DELETE /v1/me/providers/{provider}/credential",
		"POST /v1/me/providers/{provider}/billing-notice/ack",
		"POST /v1/me/oauth/{provider}/start",
	} {
		if !have[r] {
			t.Errorf("missing route %s (have %v)", r, e.srv.AreaRoutes("v1_me"))
		}
	}
}

func TestMeCatalogAndLLMSelfOnly(t *testing.T) {
	e := newAPIEnv(t)
	_ = meFakeOf(e)

	wantStatus(t, e.call(http.MethodGet, "/v1/providers/catalog", nil), http.StatusOK)
	wantError(t, e.call(http.MethodGet, "/v1/providers/catalog?x=1", nil), http.StatusBadRequest, contracts.ErrValidationFailed)

	rec := e.call(http.MethodGet, "/v1/me/llm", nil)
	wantStatus(t, rec, http.StatusOK)
	var got pv.PrincipalLlm
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if string(got.PrincipalID) != e.scopeOf().PrincipalID {
		t.Fatalf("principal = %s, want token principal", got.PrincipalID)
	}

	profile := pv.ProfileID("pprf_01J0000000000000000000000A")
	put := e.call(http.MethodPut, "/v1/me/llm/conversational_controller", map[string]any{"profile_id": string(profile)})
	wantStatus(t, put, http.StatusOK)
	wantStatus(t, e.call(http.MethodPut, "/v1/me/llm/tts", map[string]any{"profile_id": string(profile), "voice": "alloy"}), http.StatusOK)
	wantError(t, e.call(http.MethodPut, "/v1/me/llm/vision", map[string]any{"profile_id": string(profile)}), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantStatus(t, e.call(http.MethodDelete, "/v1/me/llm/conversational_controller", nil), http.StatusOK)
}

func TestMeCredentialWriteOnlyAndValidation(t *testing.T) {
	e := newAPIEnv(t)
	f := meFakeOf(e)
	secret := "sk-test-never-echo"
	rec := e.call(http.MethodPut, "/v1/me/providers/openai/credential", map[string]any{"key": secret})
	wantStatus(t, rec, http.StatusOK)
	if f.lastKey != secret {
		t.Fatalf("service did not receive key")
	}
	if rec.Body.String() != "" && containsSecret(rec.Body.String(), secret) {
		t.Fatalf("response echoed secret: %s", rec.Body.String())
	}
	wantError(t, e.call(http.MethodPut, "/v1/me/providers/Bad_Provider/credential", map[string]any{"key": secret}), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantStatus(t, e.call(http.MethodDelete, "/v1/me/providers/openai/credential", nil), http.StatusOK)

	ack := e.call(http.MethodPost, "/v1/me/providers/openai/billing-notice/ack", map[string]any{
		"notice_version": "2026-01",
		"consumer_subscription_is_not_api_access": true,
		"separate_api_billing_understood":         true,
	})
	wantStatus(t, ack, http.StatusOK)

	start := e.call(http.MethodPost, "/v1/me/oauth/openai/start", map[string]any{
		"capabilities": []string{"chat"}, "redirect_uri": "http://127.0.0.1:5555/cb",
	})
	wantStatus(t, start, http.StatusOK)
}

func TestMeRoutesUnavailableWithoutService(t *testing.T) {
	e := newAPIEnv(t)
	h := &meHandler{apps: e.apps, svc: nil}
	scope := e.scopeOf()
	ctx, err := identity.WithScope(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.catalog(rec, httptest.NewRequest(http.MethodGet, "/v1/providers/catalog", nil).WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("catalog = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
}

func containsSecret(body, secret string) bool {
	return len(secret) > 0 && (len(body) >= len(secret)) && (stringIndex(body, secret) >= 0)
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
