package oauth

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestOriginAllowed(t *testing.T) {
	allow := []string{"https://app.example.com", "http://127.0.0.1:5173"}
	if !OriginAllowed("https://app.example.com", allow) {
		t.Fatal("exact origin should pass")
	}
	if OriginAllowed("https://app.example.com/", allow) {
		t.Fatal("trailing slash must fail")
	}
	if OriginAllowed("https://evil.example.com", allow) {
		t.Fatal("other origin must fail")
	}
	if OriginAllowed("https://app.example.com", nil) {
		t.Fatal("empty allowlist refuses")
	}
	if OriginAllowed("*", allow) {
		t.Fatal("* must never pass")
	}
}

func TestCallbackPageNoSecrets(t *testing.T) {
	body := RenderCallbackPage(CallbackPageInput{
		FlowID: "oaf_01J0000000000000000000000A", Status: "completed",
		OpenerOrigin: "https://app.example.com", Nonce: "n0nce",
	})
	s := string(body)
	for _, bad := range []string{"access_token", "refresh_token", "Bearer"} {
		if strings.Contains(s, bad) {
			t.Fatalf("callback page leaked %q: %s", bad, s)
		}
	}
	if !strings.Contains(CallbackCSP("n0nce"), "nonce-n0nce") {
		t.Fatal("CSP nonce missing")
	}
	if !strings.Contains(s, "alixai.oauth.result") {
		t.Fatalf("missing postMessage type: %s", s)
	}
	if !strings.Contains(s, "https://app.example.com") {
		t.Fatal("opener origin missing from script")
	}
	if strings.Contains(s, `,"*"`) || strings.Contains(s, `,'*'`) {
		t.Fatal("must never postMessage to *")
	}
	if !strings.Contains(s, "You can close this window") {
		t.Fatal("missing close text")
	}
}

func TestLoopbackBinds127Only(t *testing.T) {
	g := newRig(t)
	s, redirect, err := g.eng.BindLoopback("/oauth/callback")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}
	if u.Hostname() != "127.0.0.1" {
		t.Fatalf("host %q", u.Hostname())
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		t.Fatal("not loopback")
	}
}

func TestPopupFlowCancelAndPrincipalIsolation(t *testing.T) {
	g := newRig(t)
	g.scope.PrincipalID = "usr_ops"
	g.eng.PopupOrigins = []string{"https://app.example.com"}

	consent, err := g.eng.Prepare(bg, g.scope, g.req("chat"))
	if err != nil {
		t.Fatal(err)
	}
	au, err := g.eng.Begin(bg, g.scope, BeginRequest{
		PrepareRequest: g.req("chat"), ConsentDigest: consent.Digest, RedirectURI: registeredRedirect,
		Presentation: PresentationPopup, OpenerOrigin: "https://app.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if au.FlowID == "" || au.PollIntervalMS <= 0 {
		t.Fatalf("popup start missing flow fields: %+v", au)
	}
	view, err := g.eng.FlowGet(bg, g.scope, au.FlowID)
	if err != nil || view.Status != FlowPending {
		t.Fatalf("pending: %v %+v", err, view)
	}
	other := g.scope
	other.PrincipalID = "usr_other"
	if _, err := g.eng.FlowGet(bg, other, au.FlowID); err == nil {
		t.Fatal("foreign principal must not see the flow")
	}
	if err := g.eng.FlowCancel(bg, g.scope, au.FlowID); err != nil {
		t.Fatal(err)
	}
	view, err = g.eng.FlowGet(bg, g.scope, au.FlowID)
	if err != nil || view.Status != FlowCancelled {
		t.Fatalf("cancelled: %v %+v", err, view)
	}
	if err := g.eng.FlowCancel(bg, g.scope, au.FlowID); err != nil {
		t.Fatal("cancel must be idempotent:", err)
	}
}

func TestOpenRouterKeyExchange(t *testing.T) {
	g := newRig(t)
	g.scope.PrincipalID = "usr_ops"
	var gotBody string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/keys" {
			raw, _ := io.ReadAll(r.Body)
			gotBody = string(raw)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"key":"sk-or-test-issued-key-0001"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(idp.Close)
	allowURL(t, g, idp.URL)

	p := ProviderConfig{
		ID: "openrouter", OAuthSupported: true,
		AuthorizationEndpoint: idp.URL + "/auth",
		GrantKind:             GrantIssuedKey, AuthStyle: AuthStyleOpenRouter, NoState: true,
		KeyExchangeEndpoint: idp.URL + "/api/v1/auth/keys",
		Loopback:            true, LoopbackPath: "/oauth/callback",
		Capabilities: map[string][]string{"inference": {"api"}},
		Scopes:       []ScopeInfo{{Name: "api", Description: "api", DataAccess: "api"}},
	}
	np := mustNorm(t, p)
	g.eng.prov[np.ID] = np

	lb, redirect, err := g.eng.BindLoopback(np.LoopbackPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Close()

	consent, err := g.eng.Prepare(bg, g.scope, PrepareRequest{ProviderID: np.ID, Capabilities: []string{"inference"}})
	if err != nil {
		t.Fatal(err)
	}
	au, err := g.eng.Begin(bg, g.scope, BeginRequest{
		PrepareRequest: PrepareRequest{ProviderID: np.ID, Capabilities: []string{"inference"}},
		ConsentDigest:  consent.Digest, RedirectURI: redirect,
		CallbackMode: CallbackLoopback, Presentation: PresentationPopup, SkipOpenerCheck: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	lb.Engine, lb.Scope, lb.FlowID, lb.CLIClose = g.eng, g.scope, au.FlowID, true
	g.eng.AttachLoopback(au.FlowID, lb)
	go lb.Serve()

	resp, err := http.Get(redirect + "?code=or-code-1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, page)
	}
	if !strings.Contains(gotBody, `"code":"or-code-1"`) {
		t.Fatalf("exchange body %q page %s", gotBody, page)
	}
	v, err := g.eng.FlowGet(bg, g.scope, au.FlowID)
	if err != nil {
		t.Fatalf("FlowGet: %v", err)
	}
	if v.Status != FlowCompleted {
		t.Fatalf("status=%s reason=%s page=%s body=%s", v.Status, v.Reason, page, gotBody)
	}
	if v.Connection == nil || v.Connection.Ref.Kind != providers.CredentialKindOauthIssuedKey {
		t.Fatalf("want oauth_issued_key: %+v", v.Connection)
	}
	sec, err := g.eng.cr.Resolve(bg, g.scope, string(v.Connection.Ref.RefID), credentials.Use{Provider: np.ID, Purpose: "inference"})
	if err != nil || sec.Reveal() != "sk-or-test-issued-key-0001" {
		t.Fatalf("resolve: %v %q", err, sec.Reveal())
	}
}

func TestSIWCDynamicClientAndResource(t *testing.T) {
	g := newRig(t)
	g.scope.PrincipalID = "usr_ops"
	var tokenBody string
	var nonceHold string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			raw, _ := io.ReadAll(r.Body)
			tokenBody = string(raw)
			w.Header().Set("Content-Type", "application/json")
			idTok := fakeIDToken(t, "https://auth.openai.com", "dynamic_agent_client", nonceHold, g.clk.Now().Add(time.Hour).Unix())
			_, _ = w.Write([]byte(`{"access_token":"atk","refresh_token":"rtk","token_type":"bearer","expires_in":3600,"scope":"openid profile email offline_access resource.invoke chatgpt.tokens.use.direct","id_token":"` + idTok + `","client_id":"oaiapp_issued_1"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(idp.Close)
	allowURL(t, g, idp.URL)

	preset, ok := Preset("openai")
	if !ok {
		t.Fatal("preset")
	}
	preset.AuthorizationEndpoint = idp.URL + "/authorize"
	preset.TokenEndpoint = idp.URL + "/token"
	preset.Issuer = "https://auth.openai.com"
	np := mustNorm(t, preset)
	g.eng.prov[np.ID] = np

	lb, redirect, err := g.eng.BindLoopback(np.LoopbackPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Close()

	consent, err := g.eng.Prepare(bg, g.scope, PrepareRequest{ProviderID: np.ID, Capabilities: []string{"inference"}})
	if err != nil {
		t.Fatal(err)
	}
	au, err := g.eng.Begin(bg, g.scope, BeginRequest{
		PrepareRequest: PrepareRequest{ProviderID: np.ID, Capabilities: []string{"inference"}},
		ConsentDigest:  consent.Digest, RedirectURI: redirect,
		CallbackMode: CallbackLoopback, Presentation: PresentationPopup, SkipOpenerCheck: true,
		ExtAgentHostID: "urn:uuid:11111111-2222-3333-4444-555555555555",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(au.URL, "client_id=dynamic_agent_client") || !strings.Contains(au.URL, "resource=") || !strings.Contains(au.URL, "ext_agent_host_id=") {
		t.Fatalf("auth URL missing SIWC params: %s", au.URL)
	}
	u, _ := url.Parse(au.URL)
	state := u.Query().Get("state")
	nonceHold = u.Query().Get("nonce")

	conn, err := g.eng.Complete(bg, g.scope, CallbackRequest{State: state, Code: "authcode1", RedirectURI: redirect})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tokenBody, "resource=") || !strings.Contains(tokenBody, "code_verifier=") {
		t.Fatalf("token body %q", tokenBody)
	}
	if conn.Ref.RefID == "" {
		t.Fatal("no ref")
	}
}

func TestLoopbackAcceptsOneGETOnly(t *testing.T) {
	g := newRig(t)
	g.scope.PrincipalID = "usr_ops"
	lb, redirect, err := g.eng.BindLoopback("/oauth/callback")
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Close()
	consent, err := g.eng.Prepare(bg, g.scope, g.req("chat"))
	if err != nil {
		t.Fatal(err)
	}
	au, err := g.eng.Begin(bg, g.scope, BeginRequest{
		PrepareRequest: g.req("chat"), ConsentDigest: consent.Digest, RedirectURI: redirect,
		CallbackMode: CallbackLoopback, Presentation: PresentationPopup, SkipOpenerCheck: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	lb.Engine, lb.Scope, lb.FlowID = g.eng, g.scope, au.FlowID
	g.eng.AttachLoopback(au.FlowID, lb)
	go lb.Serve()

	cb := browse(t, au.URL, redirect)
	q := url.Values{"code": {cb.Code}, "state": {cb.State}}
	resp, err := http.Get(redirect + "?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	v, err := g.eng.FlowGet(bg, g.scope, au.FlowID)
	if err != nil || v.Status != FlowCompleted {
		t.Fatalf("first GET: %v %+v", err, v)
	}
	// Second request: listener closed after the first callback (connection refused) or replay fails.
	resp2, err := http.Get(redirect + "?" + q.Encode())
	if err == nil {
		body, _ := io.ReadAll(resp2.Body)
		_ = resp2.Body.Close()
		if resp2.StatusCode == http.StatusOK && strings.Contains(string(body), `"status":"completed"`) {
			t.Fatal("second GET must not report a fresh completed postMessage")
		}
	}
}

func TestLoopbackPlaceholderRejectedWithoutBind(t *testing.T) {
	g := newRig(t)
	consent, err := g.eng.Prepare(bg, g.scope, g.req("chat"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.eng.Begin(bg, g.scope, BeginRequest{
		PrepareRequest: g.req("chat"), ConsentDigest: consent.Digest,
		RedirectURI: LoopbackPlaceholder, CallbackMode: CallbackLoopback, SkipOpenerCheck: true,
		Presentation: PresentationPopup,
	})
	if err == nil || ReasonOf(err) != ReasonBadRequest {
		t.Fatalf("want bad_request for unbound placeholder, got %v", err)
	}
}

func TestLoopbackIgnoresWrongStateKeepsPending(t *testing.T) {
	g := newRig(t)
	g.scope.PrincipalID = "usr_ops"
	lb, redirect, err := g.eng.BindLoopback("/oauth/callback")
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Close()
	consent, err := g.eng.Prepare(bg, g.scope, g.req("chat"))
	if err != nil {
		t.Fatal(err)
	}
	au, err := g.eng.Begin(bg, g.scope, BeginRequest{
		PrepareRequest: g.req("chat"), ConsentDigest: consent.Digest, RedirectURI: redirect,
		CallbackMode: CallbackLoopback, Presentation: PresentationPopup, SkipOpenerCheck: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	lb.Engine, lb.Scope, lb.FlowID = g.eng, g.scope, au.FlowID
	g.eng.AttachLoopback(au.FlowID, lb)
	go lb.Serve()

	bad, err := http.Get(redirect + "?code=x&state=forged-state-not-ours")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(bad.Body)
	_ = bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong state status %d", bad.StatusCode)
	}
	v, err := g.eng.FlowGet(bg, g.scope, au.FlowID)
	if err != nil || v.Status != FlowPending {
		t.Fatalf("flow must stay pending after wrong state: %v %+v", err, v)
	}

	nocode, err := http.Get(redirect + "?state=also-wrong")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(nocode.Body)
	_ = nocode.Body.Close()
	v, err = g.eng.FlowGet(bg, g.scope, au.FlowID)
	if err != nil || v.Status != FlowPending {
		t.Fatalf("flow must stay pending after empty/wrong callback: %v %+v", err, v)
	}

	cb := browse(t, au.URL, redirect)
	q := url.Values{"code": {cb.Code}, "state": {cb.State}}
	ok, err := http.Get(redirect + "?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(ok.Body)
	_ = ok.Body.Close()
	v, err = g.eng.FlowGet(bg, g.scope, au.FlowID)
	if err != nil || v.Status != FlowCompleted {
		t.Fatalf("real callback: %v %+v", err, v)
	}
}

func TestNoStateLoopbackRequiresCode(t *testing.T) {
	g := newRig(t)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/keys" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"key":"sk-or-test-issued-key-0002"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(idp.Close)
	allowURL(t, g, idp.URL)

	p := ProviderConfig{
		ID: "openrouter", OAuthSupported: true,
		AuthorizationEndpoint: idp.URL + "/auth",
		GrantKind:             GrantIssuedKey, AuthStyle: AuthStyleOpenRouter, NoState: true,
		KeyExchangeEndpoint: idp.URL + "/api/v1/auth/keys",
		Loopback:            true, LoopbackPath: "/oauth/callback",
		Capabilities: map[string][]string{"inference": {"api"}},
		Scopes:       []ScopeInfo{{Name: "api", Description: "api", DataAccess: "api"}},
	}
	np := mustNorm(t, p)
	g.eng.prov[np.ID] = np

	lb, redirect, err := g.eng.BindLoopback(np.LoopbackPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Close()
	consent, err := g.eng.Prepare(bg, g.scope, PrepareRequest{ProviderID: np.ID, Capabilities: []string{"inference"}})
	if err != nil {
		t.Fatal(err)
	}
	au, err := g.eng.Begin(bg, g.scope, BeginRequest{
		PrepareRequest: PrepareRequest{ProviderID: np.ID, Capabilities: []string{"inference"}},
		ConsentDigest:  consent.Digest, RedirectURI: redirect,
		CallbackMode: CallbackLoopback, Presentation: PresentationPopup, SkipOpenerCheck: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	lb.Engine, lb.Scope, lb.FlowID, lb.CLIClose = g.eng, g.scope, au.FlowID, true
	g.eng.AttachLoopback(au.FlowID, lb)
	go lb.Serve()

	empty, err := http.Get(redirect)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(empty.Body)
	_ = empty.Body.Close()
	if empty.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty code status %d", empty.StatusCode)
	}
	v, err := g.eng.FlowGet(bg, g.scope, au.FlowID)
	if err != nil || v.Status != FlowPending {
		t.Fatalf("NoState without code must keep pending: %v %+v", err, v)
	}

	ok, err := http.Get(redirect + "?code=or-code-2")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(ok.Body)
	_ = ok.Body.Close()
	v, err = g.eng.FlowGet(bg, g.scope, au.FlowID)
	if err != nil || v.Status != FlowCompleted {
		t.Fatalf("with code: %v %+v", err, v)
	}
}

func TestSIWCBindsOwnerPrincipal(t *testing.T) {
	g := newRig(t)
	g.scope.PrincipalID = "usr_plan_owner"
	var nonceHold string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			idTok := fakeIDToken(t, "https://auth.openai.com", "dynamic_agent_client", nonceHold, g.clk.Now().Add(time.Hour).Unix())
			_, _ = w.Write([]byte(`{"access_token":"atk","refresh_token":"rtk","token_type":"bearer","expires_in":3600,"scope":"openid profile email offline_access resource.invoke chatgpt.tokens.use.direct","id_token":"` + idTok + `","client_id":"oaiapp_issued_1"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(idp.Close)
	allowURL(t, g, idp.URL)

	preset, ok := Preset("openai")
	if !ok {
		t.Fatal("preset")
	}
	preset.AuthorizationEndpoint = idp.URL + "/authorize"
	preset.TokenEndpoint = idp.URL + "/token"
	preset.Issuer = "https://auth.openai.com"
	np := mustNorm(t, preset)
	g.eng.prov[np.ID] = np

	lb, redirect, err := g.eng.BindLoopback(np.LoopbackPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Close()
	consent, err := g.eng.Prepare(bg, g.scope, PrepareRequest{ProviderID: np.ID, Capabilities: []string{"inference"}})
	if err != nil {
		t.Fatal(err)
	}
	au, err := g.eng.Begin(bg, g.scope, BeginRequest{
		PrepareRequest: PrepareRequest{ProviderID: np.ID, Capabilities: []string{"inference"}},
		ConsentDigest:  consent.Digest, RedirectURI: redirect,
		CallbackMode: CallbackLoopback, Presentation: PresentationPopup, SkipOpenerCheck: true,
		ExtAgentHostID: "urn:uuid:11111111-2222-3333-4444-555555555555",
	})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(au.URL)
	nonceHold = u.Query().Get("nonce")
	conn, err := g.eng.Complete(bg, g.scope, CallbackRequest{State: u.Query().Get("state"), Code: "authcode1", RedirectURI: redirect})
	if err != nil {
		t.Fatal(err)
	}
	use := credentials.Use{Provider: np.ID, Purpose: "inference"}
	if _, err := g.eng.cr.Resolve(bg, g.scope, string(conn.Ref.RefID), use); err != nil {
		t.Fatalf("owner: %v", err)
	}
	other := g.scope
	other.PrincipalID = "usr_other"
	_, err = g.eng.cr.Resolve(bg, other, string(conn.Ref.RefID), use)
	if err == nil || credentials.ReasonOf(err) != credentials.ReasonPrincipalMismatch {
		t.Fatalf("want principal mismatch, got %v", err)
	}
}

func allowURL(t *testing.T, g *rig, raw string) {
	t.Helper()
	dest, err := egress.DestinationFromURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	host, p := g.idp.hostPort()
	al, err := egress.NewAllowlist(dest, egress.Destination{Host: host, Ports: []int{p}, Scheme: "http"})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.eg.Set(tApp, al); err != nil {
		t.Fatal(err)
	}
}

func fakeIDToken(t *testing.T, iss, aud, nonce string, exp int64) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]any{"alg": "none"}) + "." +
		enc(map[string]any{"iss": iss, "sub": "user_1", "aud": aud, "exp": exp, "nonce": nonce}) + ".sig"
}

func mustNorm(t *testing.T, p ProviderConfig) ProviderConfig {
	t.Helper()
	n, err := p.normalized()
	if err != nil {
		t.Fatal(err)
	}
	return n
}
