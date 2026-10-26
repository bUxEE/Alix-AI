//go:build !noexternal

package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/oauth"
)

func TestExternalOAuthConfigRejectsUnknownAndStar(t *testing.T) {
	bad := externalOAuthConfig{PopupOrigins: []string{"*"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("star origin must fail")
	}
	bad = externalOAuthConfig{Providers: map[string]externalOAuthProvider{"anthropic": {Enabled: true, ClientID: "x"}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown oauth provider must fail")
	}
	ok := externalOAuthConfig{
		PopupOrigins: []string{"https://app.example.com"},
		Providers:    map[string]externalOAuthProvider{"openrouter": {Enabled: true}},
	}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestExternalCredRefRejectsLiteralKey(t *testing.T) {
	ref := externalCredRefConfig{Provider: "openai", Label: "x", Env: "sk-literal-looking-value"}
	if err := ref.Validate(); err == nil {
		t.Fatal("literal-looking env must fail")
	}
	ref = externalCredRefConfig{Provider: "openai", Label: "x", Env: "ALIXAI_SECRET_OPENAI_KEY"}
	if err := ref.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestProvidersLoginRejectsKeyOnlyProvider(t *testing.T) {
	env := (&Env{
		Stdout: io.Discard, Stderr: io.Discard, Config: config.NewRegistry(),
		LookupEnv: func(string) (string, bool) { return "", false },
	}).withDefaults()
	err := providersLoginRun(context.Background(), env, []string{"anthropic", "-operator", "usr_ops", "-application", "app_demo"})
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("want ErrUsage for key-only provider, got %v", err)
	}
}

func TestOAuthStartPopupAndLoopbackAgainstFakeIdP(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "external")
	a := externalTestAdmin(t, dir, nil)

	var mu sync.Mutex
	var authQuery url.Values
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"atk","refresh_token":"rtk","token_type":"bearer","expires_in":3600,"scope":"openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"}`))
			return
		}
		if strings.Contains(r.URL.Path, "authorize") || r.URL.Path == "/" {
			mu.Lock()
			authQuery = r.URL.Query()
			mu.Unlock()
			http.Error(w, "open browser", http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(idp.Close)

	preset, ok := oauth.Preset("openai")
	if !ok {
		t.Fatal("preset")
	}
	preset.AuthorizationEndpoint = idp.URL + "/authorize"
	preset.TokenEndpoint = idp.URL + "/token"
	preset.OIDC = false // skip ID token for this CLI wiring test
	preset.Issuer = ""
	n, err := preset.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := oauth.New(oauth.Config{
		Credentials: a.creds, Egress: a.oauthEg, Providers: []oauth.ProviderConfig{n},
		PopupOrigins: []string{"https://app.example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a.oauth = eng
	dest, err := egress.DestinationFromURL(idp.URL)
	if err != nil {
		t.Fatal(err)
	}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.oauthEg.Set("app_demo", al); err != nil {
		t.Fatal(err)
	}

	sc := externalTestScope("ten_acme", "usr_ops")
	started, err := a.OAuthStart(context.Background(), sc, gateway.ProvidersOAuthStart{
		Provider: "openai", Capabilities: []string{"inference"},
		RedirectURI:  gateway.ProvidersOAuthLoopbackPlaceholder,
		Presentation: gateway.ProvidersOAuthPresentationPopup,
		Callback:     gateway.ProvidersOAuthCallbackLoopback,
		OpenerOrigin: "https://app.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.FlowID == "" || started.AuthorizationURL == "" || !strings.Contains(started.RedirectURI, "127.0.0.1:") {
		t.Fatalf("started: %+v", started)
	}
	st, err := a.OAuthFlowStatus(context.Background(), sc, started.FlowID)
	if err != nil || st.Status != "pending" {
		t.Fatalf("status: %v %+v", err, st)
	}
	if err := a.OAuthFlowCancel(context.Background(), sc, started.FlowID); err != nil {
		t.Fatal(err)
	}
	st, err = a.OAuthFlowStatus(context.Background(), sc, started.FlowID)
	if err != nil || st.Status != "cancelled" {
		t.Fatalf("cancelled: %v %+v", err, st)
	}

	// Opener not on allowlist → refused.
	_, err = a.OAuthStart(context.Background(), sc, gateway.ProvidersOAuthStart{
		Provider: "openai", Capabilities: []string{"inference"},
		RedirectURI:  gateway.ProvidersOAuthLoopbackPlaceholder,
		Presentation: gateway.ProvidersOAuthPresentationPopup,
		Callback:     gateway.ProvidersOAuthCallbackLoopback,
		OpenerOrigin: "https://evil.example.com",
	})
	if err == nil {
		t.Fatal("evil opener must fail")
	}

	_ = authQuery
}

func TestOpenSystemBrowserInjectable(t *testing.T) {
	prev := openSystemBrowser
	t.Cleanup(func() { openSystemBrowser = prev })
	var seen string
	openSystemBrowser = func(u string) error { seen = u; return nil }
	if err := openSystemBrowser("https://example.com/auth"); err != nil {
		t.Fatal(err)
	}
	if seen != "https://example.com/auth" {
		t.Fatalf("seen %q", seen)
	}
}

func TestParseLoginFor(t *testing.T) {
	app, principal, err := parseLoginFor("app_demo/usr_owner")
	if err != nil || app != "app_demo" || principal != "usr_owner" {
		t.Fatalf("got %q %q %v", app, principal, err)
	}
	if _, _, err := parseLoginFor(""); err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("empty: %v", err)
	}
	if _, _, err := parseLoginFor("no-slash"); err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("no slash: %v", err)
	}
	if _, _, err := parseLoginFor("app/usr/extra"); err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("extra slash: %v", err)
	}
}

func TestEnsureAgentHostIDStableOrFail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "siwc_host_id")
	a := &externalAdmin{hostIDPath: path}
	id1, err := a.ensureAgentHostID()
	if err != nil || id1 == "" {
		t.Fatalf("first: %v %q", err, id1)
	}
	id2, err := a.ensureAgentHostID()
	if err != nil || id2 != id1 {
		t.Fatalf("stable: %v %q want %q", err, id2, id1)
	}
	// Unwritable parent → fail closed (no ephemeral id).
	bad := &externalAdmin{hostIDPath: filepath.Join(dir, "missing-parent", "nested", "id")}
	// Create a file where the directory should be so MkdirAll/write fails on some paths;
	// simpler: point at a path under a file-as-directory.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad.hostIDPath = filepath.Join(blocker, "id")
	if _, err := bad.ensureAgentHostID(); err == nil {
		t.Fatal("unwritable host id path must fail closed")
	}
}
