package oauth

import (
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// RFC 7636 Appendix B test vector.
func TestPKCEChallengeS256(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := ChallengeS256(credentials.Secret(verifier)); got != want {
		t.Fatalf("challenge = %q, want %q", got, want)
	}
	v1, err := newVerifier()
	if err != nil {
		t.Fatal(err)
	}
	v2, _ := newVerifier()
	if len(v1) != 43 || v1 == v2 {
		t.Fatalf("verifiers must be 43 characters and fresh: %d, equal=%v", len(v1), v1 == v2)
	}
	g := newRig(t)
	a := g.begin(g.req("chat"), registeredRedirect)
	if q := mustQuery(t, a.URL); q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || strings.Contains(a.URL, "code_verifier") {
		t.Fatalf("authorization URL must carry an S256 challenge and never the verifier: %s", a.URL)
	}
}

func TestStateOneShotExpiringAndConstantTimeCompared(t *testing.T) {
	g := newRig(t)
	a := g.begin(g.req("chat"), registeredRedirect)
	state := stateOf(t, a.URL)
	if len(state) < 43 {
		t.Fatalf("state is too short: %d", len(state))
	}
	if b := stateOf(t, g.begin(g.req("chat"), registeredRedirect).URL); b == state {
		t.Fatal("states repeat")
	}
	// A state that differs in its last character only is unknown, and does not spend the real one.
	forged := state[:len(state)-1] + flip(state[len(state)-1])
	cb := browse(t, a.URL, registeredRedirect)
	bad := cb
	bad.State = forged
	_, err := g.eng.Complete(bg, g.scope, bad)
	mustReason(t, err, contracts.ErrValidationFailed, ReasonStateUnknown)
	// The real one works once.
	if _, err := g.eng.Complete(bg, g.scope, cb); err != nil {
		t.Fatalf("first use: %v", err)
	}
	_, err = g.eng.Complete(bg, g.scope, cb)
	mustReason(t, err, contracts.ErrReplayed, ReasonReplayed)

	// A flow expires after the state TTL.
	a2 := g.begin(g.req("chat"), registeredRedirect)
	cb2 := browse(t, a2.URL, registeredRedirect)
	g.clk.Advance(DefaultStateTTL + time.Second)
	_, err = g.eng.Complete(bg, g.scope, cb2)
	mustReason(t, err, contracts.ErrExpired, ReasonStateExpired)

	// Empty and absurd states are refused outright.
	for _, s := range []string{"", strings.Repeat("a", 300)} {
		_, err := g.eng.Complete(bg, g.scope, CallbackRequest{State: s, Code: "x", RedirectURI: registeredRedirect})
		mustReason(t, err, contracts.ErrValidationFailed, ReasonStateUnknown)
	}
	// The comparison of a state is constant-time: the source says so (timing cannot be asserted in a test).
	src, _ := os.ReadFile("pkce.go")
	if !strings.Contains(string(src), "subtle.ConstantTimeCompare") {
		t.Fatal("equalConst must use crypto/subtle")
	}
	eng, _ := os.ReadFile("engine.go")
	if !strings.Contains(string(eng), "equalConst(f.state, state)") {
		t.Fatal("the state must be compared with equalConst")
	}
}

func flip(b byte) string {
	if b == 'A' {
		return "B"
	}
	return "A"
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestRedirectURIExactBinding(t *testing.T) {
	g := newRig(t)
	c, _ := g.eng.Prepare(bg, g.scope, g.req("chat"))
	begin := func(redirect string) error {
		_, err := g.eng.Begin(bg, g.scope, BeginRequest{PrepareRequest: g.req("chat"), ConsentDigest: c.Digest, RedirectURI: redirect})
		return err
	}
	if err := begin(registeredRedirect); err != nil {
		t.Fatalf("the registered redirect: %v", err)
	}
	for _, bad := range []string{
		"", registeredRedirect + "/", registeredRedirect + "?x=1", registeredRedirect + "/x", registeredRedirect + "#f",
		strings.Replace(registeredRedirect, "https", "http", 1), strings.Replace(registeredRedirect, "alix.", "ALIX.", 1),
		"https://alix.example.test/oauth/callbac", "https://alix.example.test/oauth", "https://evil.test/oauth/callback",
		"https://alix.example.test.evil.test/oauth/callback", "https://user@alix.example.test/oauth/callback",
		"javascript:alert(1)", "https://alix.example.test:8443/oauth/callback",
	} {
		mustReason(t, begin(bad), contracts.ErrValidationFailed, ReasonRedirectRejected)
	}
	// The callback must arrive on the redirect the flow was bound to.
	a := g.begin(g.req("chat"), registeredRedirect)
	cb := browse(t, a.URL, registeredRedirect)
	cb.RedirectURI = registeredRedirect + "/"
	_, err := g.eng.Complete(bg, g.scope, cb)
	mustReason(t, err, contracts.ErrValidationFailed, ReasonRedirectRejected)
	// ...and it is spent: the right redirect cannot be retried.
	cb.RedirectURI = registeredRedirect
	_, err = g.eng.Complete(bg, g.scope, cb)
	mustReason(t, err, contracts.ErrReplayed, ReasonReplayed)
	if g.idp.requests("/token") != 0 {
		t.Fatal("the token endpoint was contacted for a callback on the wrong redirect")
	}
}

func TestLoopbackRedirectRandomPortAccepted(t *testing.T) {
	g := newRig(t)
	// Random ports from the operating system, as RFC 8252 7.3 has the app choose.
	for i := 0; i < 3; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		redirect := LoopbackRedirectURI(port, "")
		a := g.begin(g.req("chat"), redirect)
		conn, err := g.eng.Complete(bg, g.scope, browse(t, a.URL, redirect))
		if err != nil {
			t.Fatalf("port %d: %v", port, err)
		}
		if conn.Ref.Status != providers.CredentialRefStatusActive {
			t.Fatalf("status %s", conn.Ref.Status)
		}
	}
	if err := g.eng.prov["mockidp"].checkRedirect("http://[::1]:51234/oauth/callback"); err != nil {
		t.Fatalf("IPv6 loopback: %v", err)
	}
	for _, bad := range []string{
		"http://localhost:51234/oauth/callback", "http://0.0.0.0:51234/oauth/callback", "http://192.168.1.5:51234/oauth/callback",
		"http://127.0.0.2:51234/oauth/callback", "http://127.0.0.1/oauth/callback", "http://127.0.0.1:0/oauth/callback",
		"http://127.0.0.1:65536/oauth/callback", "http://127.0.0.1:051234/oauth/callback", "http://127.0.0.1:51234/other",
		"http://127.0.0.1:51234/oauth/callback/", "http://127.0.0.1:51234/oauth/callback?a=b", "https://127.0.0.1:51234/oauth/callback",
		"http://u@127.0.0.1:51234/oauth/callback", "http://127.0.0.1:51234@evil.test/oauth/callback",
		"http://127.0.0.1:51234/oauth/callback#x", "http://example.com:51234/oauth/callback",
	} {
		if err := g.eng.prov["mockidp"].checkRedirect(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	// A provider that does not allow loopback refuses it.
	g2 := newRig(t, func(_ *rig, c *ProviderConfig) { c.Loopback = false })
	if err := g2.eng.prov["mockidp"].checkRedirect(LoopbackRedirectURI(50000, "")); err == nil {
		t.Fatal("loopback accepted by a provider that registered none")
	}
}

func TestNonceValidatedWhenOIDC(t *testing.T) {
	setup := func(t *testing.T, mut func(*idp)) (*rig, error) {
		g := newRig(t, func(g *rig, c *ProviderConfig) {
			c.OIDC, c.Issuer = true, g.idp.url()
			c.Capabilities["chat"] = []string{"scope.chat"}
			g.idp.oidc = true
			if mut != nil {
				mut(g.idp)
			}
		})
		a := g.begin(g.req("chat"), registeredRedirect)
		q := mustQuery(t, a.URL)
		if q.Get("nonce") == "" || !strings.Contains(a.URL, "openid") {
			t.Fatalf("an OIDC flow sends a nonce and the openid scope: %s", a.URL)
		}
		_, err := g.eng.Complete(bg, g.scope, browse(t, a.URL, registeredRedirect))
		return g, err
	}
	t.Run("valid", func(t *testing.T) {
		g, err := setup(t, nil)
		if err != nil {
			t.Fatal(err)
		}
		refs, _ := g.svc.List(bg, g.scope, "")
		if len(refs) != 1 || refs[0].Account.AccountID == nil || *refs[0].Account.AccountID != "acct_A" {
			t.Fatalf("the account must come from the ID token subject: %+v", refs)
		}
	})
	for name, tc := range map[string]struct {
		mut    func(*idp)
		reason Reason
	}{
		"wrong nonce":    {func(p *idp) { p.idNonce = "TESTONLY-other-nonce" }, ReasonNonce},
		"no id token":    {func(p *idp) { p.omitID = true }, ReasonIDToken},
		"wrong audience": {func(p *idp) { p.idAud = "someone-else" }, ReasonIDToken},
		"wrong issuer":   {func(p *idp) { p.idIss = "http://evil.test" }, ReasonIssuerMismatch},
		"expired":        {func(p *idp) { p.idExp = tStart.Add(-time.Hour).Unix() }, ReasonIDToken},
	} {
		t.Run(name, func(t *testing.T) {
			g, err := setup(t, tc.mut)
			mustReason(t, err, contracts.ErrUnauthenticated, tc.reason)
			if refs, _ := g.svc.List(bg, g.scope, ""); len(refs) != 0 {
				t.Fatal("a connection was stored for a rejected ID token")
			}
			if len(g.idp.revokedHints()) == 0 {
				t.Fatal("tokens issued for a failed flow must be revoked at the provider")
			}
		})
	}
	// A provider that is OIDC needs an issuer and the openid scope in its config.
	bad := testProvider(newRig(t).idp)
	bad.OIDC = true
	if _, err := bad.normalized(); err == nil {
		t.Fatal("an OIDC config without an issuer was accepted")
	}
}

func TestCallbackReplayRejected(t *testing.T) {
	g := newRig(t)
	a := g.begin(g.req("chat"), registeredRedirect)
	cb := browse(t, a.URL, registeredRedirect)
	if _, err := g.eng.Complete(bg, g.scope, cb); err != nil {
		t.Fatal(err)
	}
	hits := g.idp.requests("/token")
	for i := 0; i < 3; i++ {
		_, err := g.eng.Complete(bg, g.scope, cb)
		mustReason(t, err, contracts.ErrReplayed, ReasonReplayed)
	}
	if g.idp.requests("/token") != hits {
		t.Fatal("a replayed callback reached the token endpoint")
	}
	if refs, _ := g.svc.List(bg, g.scope, ""); len(refs) != 1 {
		t.Fatalf("a replay created %d references", len(refs))
	}
	// A callback with an error is spent too, and never exchanges anything.
	g.idp.deny = true
	a2 := g.begin(g.req("chat"), registeredRedirect)
	cb2 := browse(t, a2.URL, registeredRedirect)
	_, err := g.eng.Complete(bg, g.scope, cb2)
	mustReason(t, err, contracts.ErrPermissionDenied, ReasonAccessDenied)
	_, err = g.eng.Complete(bg, g.scope, cb2)
	mustReason(t, err, contracts.ErrReplayed, ReasonReplayed)
	if g.idp.requests("/token") != hits {
		t.Fatal("a declined authorization reached the token endpoint")
	}
	// Another tenant cannot complete this tenant's flow.
	g.idp.deny = false
	a3 := g.begin(g.req("chat"), registeredRedirect)
	_, err = g.eng.Complete(bg, scopeOf(tApp, tTenant2), browse(t, a3.URL, registeredRedirect))
	mustReason(t, err, contracts.ErrScopeViolation, ReasonScopeMismatch)
}

func TestScopesMinimalAndListedBeforeConsent(t *testing.T) {
	g := newRig(t)
	c, err := g.eng.Prepare(bg, g.scope, g.req("chat"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Scopes) != 1 || c.Scopes[0].Name != "scope.chat" || c.Scopes[0].Description == "" || c.Scopes[0].DataAccess == "" {
		t.Fatalf("consent lists exactly the scopes of the capability with their text: %+v", c.Scopes)
	}
	if c.ProviderName != "Mock IdP" || len(c.DataAccess) == 0 || c.Digest == "" {
		t.Fatalf("consent must name the provider and the data access: %+v", c)
	}
	if g.idp.total() != 0 {
		t.Fatal("Prepare must not touch the network")
	}
	// A union of capabilities is deduplicated; nothing is added.
	c2, _ := g.eng.Prepare(bg, g.scope, PrepareRequest{ProviderID: g.cfg.ID, Capabilities: []string{"chat", "chat_files", "files"}})
	if names(c2.Scopes) != "scope.chat scope.files" {
		t.Fatalf("scopes = %s", names(c2.Scopes))
	}
	// An unknown capability is refused (never widened to a default).
	_, err = g.eng.Prepare(bg, g.scope, PrepareRequest{ProviderID: g.cfg.ID, Capabilities: []string{"admin"}})
	mustReason(t, err, contracts.ErrUnknownCapability, ReasonUnknownCapab)
	_, err = g.eng.Prepare(bg, g.scope, PrepareRequest{ProviderID: g.cfg.ID})
	mustReason(t, err, contracts.ErrValidationFailed, ReasonBadRequest)
	// The URL is issued only for the consent that was prepared.
	_, err = g.eng.Begin(bg, g.scope, BeginRequest{PrepareRequest: g.req("chat"), ConsentDigest: c2.Digest, RedirectURI: registeredRedirect})
	mustReason(t, err, contracts.ErrValidationFailed, ReasonConsentMismatch)
	_, err = g.eng.Begin(bg, g.scope, BeginRequest{PrepareRequest: g.req("chat"), RedirectURI: registeredRedirect})
	mustReason(t, err, contracts.ErrValidationFailed, ReasonConsentMismatch)
	a, err := g.eng.Begin(bg, g.scope, BeginRequest{PrepareRequest: g.req("chat"), ConsentDigest: c.Digest, RedirectURI: registeredRedirect})
	if err != nil {
		t.Fatal(err)
	}
	if got := mustQuery(t, a.URL).Get("scope"); got != "scope.chat" {
		t.Fatalf("requested scope = %q", got)
	}
	conn, err := g.eng.Complete(bg, g.scope, browse(t, a.URL, registeredRedirect))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(conn.Ref.Binding.Scopes, " ") != "scope.chat" {
		t.Fatalf("the reference is permitted %v", conn.Ref.Binding.Scopes)
	}
	// A call needing a scope that was not consented to is refused by the credential service.
	_, err = g.eng.Token(bg, g.scope, TokenRequest{Ref: conn.Ref.RefID, Scopes: []string{"scope.files"}})
	if credentials.ReasonOf(err) != credentials.ReasonScopeNotPermitted {
		t.Fatalf("want scope_not_permitted, got %v", err)
	}
	// A provider that grants more than was asked is quarantined, never silently accepted.
	g.idp.extraScope = "scope.admin"
	a2 := g.begin(g.req("chat"), registeredRedirect)
	conn2, err := g.eng.Complete(bg, g.scope, browse(t, a2.URL, registeredRedirect))
	if err != nil {
		t.Fatal(err)
	}
	if conn2.Ref.Status != providers.CredentialRefStatusQuarantined || strings.Join(conn2.Ref.Binding.Scopes, " ") != "scope.chat" {
		t.Fatalf("over-granted connection: %s %v", conn2.Ref.Status, conn2.Ref.Binding.Scopes)
	}
}

// A provider that grants more than was asked, on a connection that then cannot be quarantined, leaves nothing
// usable behind: the reference is revoked, the grant is revoked at the provider and the flow fails.
func TestOverGrantedConnectionThatCannotBeQuarantinedIsRevoked(t *testing.T) {
	g := newRig(t)
	g.idp.extraScope = "scope.admin"
	g.spy.flagErr = contracts.New(contracts.ErrUnavailable, "credentials: the change could not be recorded")
	a := g.begin(g.req("chat"), registeredRedirect)
	conn, err := g.eng.Complete(bg, g.scope, browse(t, a.URL, registeredRedirect))
	if err == nil {
		t.Fatalf("an over-granted connection that could not be quarantined was accepted: %s", conn.Ref.Status)
	}
	refs, err := g.svc.List(bg, g.scope, g.cfg.ID)
	if err != nil || len(refs) != 1 || refs[0].Status != providers.CredentialRefStatusRevoked {
		t.Fatalf("the over-granted reference is left as %+v (%v)", refs, err)
	}
	if len(g.idp.revokedHints()) == 0 {
		t.Fatal("the over-granted grant was left at the provider")
	}
}

// The same over-granted connection when the credential service cannot record any change (so neither the quarantine
// nor the revocation can be written) and the provider has no revocation endpoint: the stored tokens are gone all the
// same, so the over-scoped grant is never left usable.
func TestOverGrantedConnectionIsUnusableWhenNoChangeCanBeRecorded(t *testing.T) {
	g := newRig(t, func(_ *rig, c *ProviderConfig) { c.RevocationEndpoint = "" })
	g.idp.extraScope = "scope.admin"
	g.spy.Credentials = breakAfterAdd{Credentials: g.svc, aud: g.aud}
	a := g.begin(g.req("chat"), registeredRedirect)
	if conn, err := g.eng.Complete(bg, g.scope, browse(t, a.URL, registeredRedirect)); err == nil {
		t.Fatalf("an over-granted connection that could not be quarantined was accepted: %s", conn.Ref.Status)
	}
	refs, err := g.svc.List(bg, g.scope, g.cfg.ID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("references %+v (%v)", refs, err)
	}
	for _, use := range []credentials.Use{
		{Provider: g.cfg.ID, Purpose: "inference", Scopes: []string{"scope.chat"}},
		{Provider: g.cfg.ID, Purpose: "oauth.refresh", Refresh: true},
	} {
		if v, err := g.svc.Resolve(bg, g.scope, string(refs[0].RefID), use); err == nil || !v.Empty() {
			t.Fatalf("the over-granted connection still releases a token (refresh %v)", use.Refresh)
		}
	}
}

// A token endpoint that answers with a redirect is a failed exchange: the form (code, PKCE verifier, client secret)
// is never replayed to the other origin, even one on the allowlist.
func TestTokenCallFollowsNoRedirect(t *testing.T) {
	var followed atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/token", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	g := newRig(t, func(_ *rig, c *ProviderConfig) { c.TokenEndpoint = redirector.URL + "/token" })
	var dests []egress.Destination
	for _, base := range []string{g.idp.srv.URL, redirector.URL, other.URL} {
		u, _ := url.Parse(base)
		port, _ := strconv.Atoi(u.Port())
		dests = append(dests, egress.Destination{Host: u.Hostname(), Ports: []int{port}, Scheme: "http"})
	}
	al, err := egress.NewAllowlist(dests...)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.eg.Set(tApp, al); err != nil {
		t.Fatal(err)
	}
	a := g.begin(g.req("chat"), registeredRedirect)
	if _, err := g.eng.Complete(bg, g.scope, browse(t, a.URL, registeredRedirect)); err == nil || followed.Load() != 0 {
		t.Fatalf("the token request followed a redirect: %v, %d requests at the other origin", err, followed.Load())
	}
}

// Token calls of one application share one HTTP client: a second call reuses the idle connection of the first
// instead of opening a new one and leaving the old one open until it times out.
func TestTokenCallsReuseTheConnectionOfTheApplication(t *testing.T) {
	g := newRig(t)
	conn := g.connect("chat")
	if _, err := g.eng.Token(bg, g.scope, TokenRequest{Ref: conn.Ref.RefID, ForceRefresh: true}); err != nil {
		t.Fatal(err)
	}
	if n, c := g.idp.requests("/token"), g.idp.conns("/token"); n != 2 || c != 1 {
		t.Fatalf("%d token requests on %d connections, want 2 on 1", n, c)
	}
}

func names(s []ScopeConsent) string {
	var out []string
	for _, c := range s {
		out = append(out, c.Name)
	}
	return strings.Join(out, " ")
}

func TestTokensStoredOnlyViaCredentialService(t *testing.T) {
	g := newRig(t)
	conn := g.connect("chat")
	if g.spy.count("AddOAuth") != 1 {
		t.Fatalf("AddOAuth calls = %d", g.spy.count("AddOAuth"))
	}
	// The values are held by the credential service, which releases them.
	acc, err := g.svc.Resolve(bg, g.scope, string(conn.Ref.RefID), credentials.Use{Provider: "mockidp", Purpose: "test"})
	if err != nil || !strings.HasPrefix(acc.Reveal(), "TESTONLY-access-") {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := g.svc.Resolve(bg, g.scope, string(conn.Ref.RefID), credentials.Use{Provider: "mockidp", Purpose: "test", Refresh: true}); err != nil {
		t.Fatalf("refresh value not held by the credential service: %v", err)
	}
	// Nothing the engine returns carries a value.
	out := asJSON(t, conn) + asJSON(t, g.begin(g.req("chat"), registeredRedirect))
	if strings.Contains(out, "TESTONLY-access") || strings.Contains(out, "TESTONLY-refresh") {
		t.Fatal("a token reached a returned struct")
	}
	// A refresh stores through Rotate, nowhere else.
	g.clk.Advance(time.Hour - 30*time.Second)
	if _, err := g.token(conn.Ref.RefID); err != nil {
		t.Fatal(err)
	}
	if g.spy.count("Rotate") != 1 {
		t.Fatalf("Rotate calls = %d", g.spy.count("Rotate"))
	}
	// The package has no way to persist anything itself: no filesystem, database or store imports.
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch p {
			case "os", "io/ioutil", "io/fs", "database/sql", "os/exec", "plugin",
				"github.com/buxee/alix-ai/internal/store", "github.com/buxee/alix-ai/internal/audit":
				t.Errorf("%s imports %s: tokens may be stored only through internal/credentials", f, p)
			}
		}
	}
}

func TestRefreshRotationSingleFlight(t *testing.T) {
	g := newRig(t)
	conn := g.connect("chat")
	first, _ := g.token(conn.Ref.RefID)
	g.idp.delay = 80 * time.Millisecond
	g.clk.Advance(time.Hour - 10*time.Second) // inside the refresh skew
	const n = 24
	var wg sync.WaitGroup
	got := make([]credentials.Secret, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = g.token(conn.Ref.RefID)
		}()
	}
	wg.Wait()
	if h := g.idp.refreshHits.Load(); h != 1 {
		t.Fatalf("refresh requests = %d, want exactly 1", h)
	}
	for i := range got {
		if errs[i] != nil || got[i] != got[0] || got[i] == first || got[i].Empty() {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
	}
	// The provider retired the old refresh value; the new one was stored, so the next cycle works.
	g.idp.delay = 0
	g.clk.Advance(time.Hour - 10*time.Second)
	second, err := g.token(conn.Ref.RefID)
	if err != nil || second == got[0] {
		t.Fatalf("second refresh: %v", err)
	}
	if h := g.idp.refreshHits.Load(); h != 2 {
		t.Fatalf("refresh requests = %d, want 2", h)
	}
	// Away from expiry nothing is refreshed.
	if _, err := g.token(conn.Ref.RefID); err != nil || g.idp.refreshHits.Load() != 2 {
		t.Fatalf("an unexpired access value was refreshed: %v", err)
	}
	// A provider that does not rotate keeps the refresh value held.
	g.idp.rotate = false
	g.clk.Advance(time.Hour - 10*time.Second)
	if _, err := g.token(conn.Ref.RefID); err != nil {
		t.Fatal(err)
	}
	g.clk.Advance(time.Hour - 10*time.Second)
	if _, err := g.token(conn.Ref.RefID); err != nil {
		t.Fatalf("refresh after a non-rotating refresh: %v", err)
	}
}

// A refresh that was in flight while the connection was re-authorized does not overwrite the tokens of the new
// grant: the refresh, from reading the refresh value to storing the answer, and the storing of a re-authorization
// take turns on the reference.
func TestRefreshInFlightKeepsTheTokensOfAReauthorization(t *testing.T) {
	g := newRig(t)
	conn := g.connect("chat")
	ref := conn.Ref.RefID
	first, err := g.token(ref)
	if err != nil {
		t.Fatal(err)
	}
	oldGrant := g.idp.grantOf(first.Reveal())

	release := make(chan struct{})
	g.idp.hold = release
	refreshed := make(chan error, 1)
	go func() {
		_, err := g.eng.Token(bg, g.scope, TokenRequest{Ref: ref, ForceRefresh: true})
		refreshed <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for g.idp.refreshHits.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the refresh never reached the provider")
		}
		time.Sleep(time.Millisecond)
	}

	// The user re-authorizes while the refresh waits for the provider's answer.
	a := g.begin(PrepareRequest{ProviderID: g.cfg.ID, Capabilities: []string{"chat"}, Reauthorize: ref}, registeredRedirect)
	cb := browse(t, a.URL, registeredRedirect)
	reauthed := make(chan error, 1)
	go func() {
		_, err := g.eng.Complete(bg, g.scope, cb)
		reauthed <- err
	}()
	// The provider answers the refresh once the re-authorization is stored, or once it has waited a while for
	// its turn (it may not store while the refresh holds the reference).
	var reauthErr error
	done := false
	select {
	case reauthErr = <-reauthed:
		done = true
	case <-time.After(time.Second):
	}
	close(release)
	if err := <-refreshed; err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !done {
		reauthErr = <-reauthed
	}
	if reauthErr != nil {
		t.Fatalf("re-authorization: %v", reauthErr)
	}
	now, err := g.token(ref)
	if err != nil {
		t.Fatal(err)
	}
	if g.idp.grantOf(now.Reveal()) == oldGrant {
		t.Fatal("the refresh that was in flight overwrote the tokens of the re-authorization")
	}
}

func TestRevocationDetectedAndFailsClosed(t *testing.T) {
	g := newRig(t)
	conn := g.connect("chat")
	ref := conn.Ref.RefID
	// The user revokes the app at the provider; the next refresh detects it.
	g.idp.revokeAllGrants()
	g.clk.Advance(time.Hour - 10*time.Second)
	_, err := g.token(ref)
	mustReason(t, err, contracts.ErrUnauthenticated, ReasonReauthRequired)
	cur, _ := g.svc.Get(bg, g.scope, ref)
	if cur.Status != providers.CredentialRefStatusReauthorizationRequired {
		t.Fatalf("status = %s", cur.Status)
	}
	hits := g.idp.total()
	for i := 0; i < 3; i++ { // fails closed, without asking the provider again
		_, err = g.token(ref)
		if credentials.ReasonOf(err) != credentials.ReasonReauthRequired {
			t.Fatalf("later call: %v", err)
		}
	}
	if g.idp.total() != hits {
		t.Fatal("a flagged connection kept calling the provider")
	}
	if strings.Contains(g.logbuf.String(), "TESTONLY-provider-text") {
		t.Fatal("provider text reached the logs")
	}

	// An outage is not a revocation: nothing is flagged, and a still-valid access value keeps working.
	g2 := newRig(t)
	c2 := g2.connect("chat")
	g2.idp.down = true
	g2.clk.Advance(time.Hour - 10*time.Second)
	if _, err := g2.token(c2.Ref.RefID); err != nil {
		t.Fatalf("a valid access value must survive a provider outage: %v", err)
	}
	g2.clk.Advance(time.Minute) // now expired and the provider is still down
	_, err = g2.token(c2.Ref.RefID)
	if err == nil {
		t.Fatal("an expired credential was released")
	}
	if cur, _ := g2.svc.Get(bg, g2.scope, c2.Ref.RefID); cur.Status == providers.CredentialRefStatusReauthorizationRequired || cur.Status == providers.CredentialRefStatusInvalid {
		t.Fatalf("an outage flagged the connection: %s", cur.Status)
	}

	// Disconnect revokes at the provider and locally, even when the provider is down.
	g3 := newRig(t)
	c3 := g3.connect("chat")
	res, err := g3.eng.Disconnect(bg, g3.scope, c3.Ref.RefID)
	if err != nil || !res.ProviderRevoked || res.Ref.Status != providers.CredentialRefStatusRevoked {
		t.Fatalf("disconnect: %+v %v", res, err)
	}
	if h := g3.idp.revokedHints(); len(h) != 2 || h[0] != "refresh_token" || h[1] != "access_token" {
		t.Fatalf("revocation calls = %v", h)
	}
	if _, err := g3.token(c3.Ref.RefID); credentials.ReasonOf(err) != credentials.ReasonRevoked {
		t.Fatalf("a disconnected connection is usable: %v", err)
	}
	g4 := newRig(t)
	c4 := g4.connect("chat")
	g4.idp.down = true
	res, err = g4.eng.Disconnect(bg, g4.scope, c4.Ref.RefID)
	if err != nil || res.ProviderRevoked || res.Ref.Status != providers.CredentialRefStatusRevoked {
		t.Fatalf("disconnect with the provider down: %+v %v", res, err)
	}

	// Tenant removal / security reset: every connection of the tenant, none of another's.
	g5 := newRig(t)
	a := g5.connect("chat")
	b := g5.connect("files")
	other := scopeOf(tApp, tTenant2)
	g5.allow(tApp)
	ao := g5.begin2(other, g5.req("chat"))
	oc, err := g5.eng.Complete(bg, other, browse(t, ao.URL, registeredRedirect))
	if err != nil {
		t.Fatal(err)
	}
	n, err := g5.eng.DisconnectTenant(bg, g5.scope)
	if err != nil || n != 2 {
		t.Fatalf("DisconnectTenant = %d, %v", n, err)
	}
	for _, r := range []providers.CredentialRefID{a.Ref.RefID, b.Ref.RefID} {
		if _, err := g5.token(r); credentials.ReasonOf(err) != credentials.ReasonRevoked {
			t.Fatalf("connection survived the tenant reset: %v", err)
		}
	}
	if _, err := g5.eng.Token(bg, other, TokenRequest{Ref: oc.Ref.RefID}); err != nil {
		t.Fatalf("another tenant was disconnected: %v", err)
	}
}

func (g *rig) begin2(scope contracts.Scope, req PrepareRequest) Authorization {
	g.t.Helper()
	c, err := g.eng.Prepare(bg, scope, req)
	if err != nil {
		g.t.Fatal(err)
	}
	a, err := g.eng.Begin(bg, scope, BeginRequest{PrepareRequest: req, ConsentDigest: c.Digest, RedirectURI: registeredRedirect})
	if err != nil {
		g.t.Fatal(err)
	}
	return a
}

func TestAccountSwitchRequiresReconsent(t *testing.T) {
	g := newRig(t)
	conn := g.connect("chat")
	ref := conn.Ref.RefID
	before, _ := g.token(ref)

	// Re-authorization shows the known account and forces the consent screen.
	req := PrepareRequest{ProviderID: g.cfg.ID, Capabilities: []string{"chat"}, Reauthorize: ref}
	c, err := g.eng.Prepare(bg, g.scope, req)
	if err != nil || !c.Reauthorization || c.Account.AccountID == nil || *c.Account.AccountID != "acct_A" {
		t.Fatalf("consent: %+v %v", c, err)
	}
	a := g.begin(req, registeredRedirect)
	if p := mustQuery(t, a.URL).Get("prompt"); p != "consent" {
		t.Fatalf("prompt = %q", p)
	}
	if _, err := g.eng.Complete(bg, g.scope, browse(t, a.URL, registeredRedirect)); err != nil {
		t.Fatalf("same-account re-authorization: %v", err)
	}

	// A different account answering a plain re-authorization is refused, and nothing changes.
	g.idp.account = "acct_B"
	a = g.begin(req, registeredRedirect)
	_, err = g.eng.Complete(bg, g.scope, browse(t, a.URL, registeredRedirect))
	mustReason(t, err, contracts.ErrScopeViolation, ReasonAccountMismatch)
	cur, _ := g.svc.Get(bg, g.scope, ref)
	if cur.Account.AccountID == nil || *cur.Account.AccountID != "acct_A" {
		t.Fatalf("the account changed without a switch: %+v", cur.Account)
	}
	if hints := g.idp.revokedHints(); len(hints) == 0 {
		t.Fatal("the grant of the refused account must be revoked at the provider")
	}

	// A switch is its own request: it needs an existing connection and forces account selection and consent.
	_, err = g.eng.Prepare(bg, g.scope, PrepareRequest{ProviderID: g.cfg.ID, Capabilities: []string{"chat"}, SwitchAccount: true})
	mustReason(t, err, contracts.ErrValidationFailed, ReasonBadRequest)
	sreq := PrepareRequest{ProviderID: g.cfg.ID, Capabilities: []string{"chat"}, Reauthorize: ref, SwitchAccount: true}
	sc, _ := g.eng.Prepare(bg, g.scope, sreq)
	if sc.Digest == c.Digest || !sc.SwitchAccount {
		t.Fatal("a switch is a different consent")
	}
	// The consent for a plain re-authorization cannot be used to start a switch.
	_, err = g.eng.Begin(bg, g.scope, BeginRequest{PrepareRequest: sreq, ConsentDigest: c.Digest, RedirectURI: registeredRedirect})
	mustReason(t, err, contracts.ErrValidationFailed, ReasonConsentMismatch)
	revokedBefore := len(g.idp.revokedHints())
	a = g.begin(sreq, registeredRedirect)
	if p := mustQuery(t, a.URL).Get("prompt"); p != "select_account consent" {
		t.Fatalf("prompt = %q", p)
	}
	sw, err := g.eng.Complete(bg, g.scope, browse(t, a.URL, registeredRedirect))
	if err != nil || !sw.Switched || sw.Ref.RefID != ref {
		t.Fatalf("switch: %+v %v", sw, err)
	}
	if sw.Ref.Account.AccountID == nil || *sw.Ref.Account.AccountID != "acct_B" {
		t.Fatalf("account after the switch: %+v", sw.Ref.Account)
	}
	if len(g.idp.revokedHints()) <= revokedBefore {
		t.Fatal("the previous account's tokens must be revoked at the provider after a switch")
	}
	after, err := g.token(ref)
	if err != nil || after == before {
		t.Fatalf("the new account's token must replace the old: %v", err)
	}
}

func TestTokenNeverInURLsLogsOrPrompts(t *testing.T) {
	g := newRig(t, func(g *rig, c *ProviderConfig) { g.idp.oidc = true; c.OIDC, c.Issuer = true, g.idp.url() })
	seen := []string{}
	a := g.begin(g.req("chat"), registeredRedirect)
	seen = append(seen, a.URL, asJSON(t, a))
	cb := browse(t, a.URL, registeredRedirect)
	conn, err := g.eng.Complete(bg, g.scope, cb)
	if err != nil {
		t.Fatal(err)
	}
	acc, _ := g.token(conn.Ref.RefID)
	g.clk.Advance(time.Hour - 10*time.Second)
	acc2, _ := g.token(conn.Ref.RefID)
	canaries := []string{acc.Reveal(), acc2.Reveal(), "TESTONLY-refresh", "TESTONLY-access", cb.Code}
	// provoke every error path as well
	g.idp.revokeAllGrants()
	g.clk.Advance(time.Hour - 10*time.Second)
	_, e1 := g.token(conn.Ref.RefID)
	_, e2 := g.eng.Complete(bg, g.scope, cb)
	_, e3 := g.eng.Disconnect(bg, g.scope, conn.Ref.RefID)
	for _, e := range []error{e1, e2, e3} {
		if e != nil {
			seen = append(seen, e.Error(), asJSON(t, e))
		}
	}
	seen = append(seen, asJSON(t, conn), g.logbuf.String())
	for _, s := range seen {
		for _, c := range canaries {
			if c != "" && strings.Contains(s, c) {
				t.Fatalf("a token or code appeared in a URL, error, log line or returned struct")
			}
		}
	}
	// The provider never saw a token in a query string: token and revocation calls are POST forms.
	g.idp.mu.Lock()
	for _, r := range g.idp.reqs {
		if r.path != "/authorize" && r.rawQuery != "" {
			t.Errorf("%s was called with a query string", r.path)
		}
	}
	g.idp.mu.Unlock()
	// The log shows references, never provider text.
	if strings.Contains(g.logbuf.String(), "TESTONLY-provider-text") || strings.Contains(g.logbuf.String(), "code_verifier") {
		t.Fatal("provider text or the verifier reached the log")
	}
}

func TestMockIdentityProviderEndToEnd(t *testing.T) {
	t.Run("public client with PKCE", func(t *testing.T) {
		g := newRig(t)
		conn := g.connect("chat_files")
		if conn.Ref.Status != providers.CredentialRefStatusActive || conn.Ref.Kind != providers.CredentialKindOauth ||
			conn.Ref.Account.AccountID == nil || *conn.Ref.Account.AccountID != "acct_A" ||
			conn.Ref.Account.OrganizationID == nil || *conn.Ref.Account.OrganizationID != "org_1" {
			t.Fatalf("connection: %+v", conn.Ref)
		}
		if conn.Ref.Times.AuthorizedAt == nil || conn.ConsentedAt.IsZero() {
			t.Fatal("consent time not recorded")
		}
		tok, err := g.eng.Token(bg, g.scope, TokenRequest{Ref: conn.Ref.RefID, Scopes: []string{"scope.chat", "scope.files"}, Purpose: "inference"})
		if err != nil || tok.Empty() {
			t.Fatalf("token: %v", err)
		}
		if q := g.idp.authQuery(); q.Get("state") == "" || q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "client-alix" {
			t.Fatalf("authorization request: %v", q)
		}
		// Egress: with the provider off the application's allowlist, nothing is reachable.
		g.eg.Revoke(tApp)
		a := g.begin(g.req("chat"), registeredRedirect)
		cb := browse(t, a.URL, registeredRedirect)
		_, err = g.eng.Complete(bg, g.scope, cb)
		mustReason(t, err, contracts.ErrPermissionDenied, ReasonEgressDenied)
	})
	for _, method := range []string{"post", "basic"} {
		t.Run("confidential client "+method, func(t *testing.T) {
			g := newRig(t)
			g.idp.clientAuth, g.idp.secret = method, "TESTONLY-client-secret-0001"
			sec, err := g.svc.AddAPIKey(bg, g.scope, credentials.AddAPIKeyRequest{ProviderID: "mockidp", Label: "client secret", Key: credentials.Secret(g.idp.secret)})
			if err != nil {
				t.Fatal(err)
			}
			cfg := g.cfg
			cfg.ClientSecretRef, cfg.ClientAuth = string(sec.RefID), ClientAuth(method)
			eng, err := New(Config{Credentials: g.spy, Egress: g.eg, Providers: []ProviderConfig{cfg}, Clock: g.clk})
			if err != nil {
				t.Fatal(err)
			}
			g.eng = eng
			conn := g.connect("chat")
			g.clk.Advance(time.Hour - 10*time.Second)
			if _, err := g.token(conn.Ref.RefID); err != nil {
				t.Fatalf("refresh with client authentication: %v", err)
			}
			if _, err := g.eng.Disconnect(bg, g.scope, conn.Ref.RefID); err != nil {
				t.Fatal(err)
			}
			if len(g.idp.revokedHints()) != 2 {
				t.Fatalf("revocation with client authentication: %v", g.idp.revokedHints())
			}
		})
	}
}

func TestUnsupportedProviderReportsManualFallback(t *testing.T) {
	g := newRig(t, func(_ *rig, c *ProviderConfig) {})
	off := ProviderConfig{ID: "keyonly", DisplayName: "Key Only", OAuthSupported: false}
	disabled := g.cfg
	disabled.ID, disabled.Disabled = "operatoroff", true
	eng, err := New(Config{Credentials: g.spy, Egress: g.eg, Providers: []ProviderConfig{g.cfg, off, disabled}, Clock: g.clk})
	if err != nil {
		t.Fatal(err)
	}
	if eng.Mode("mockidp") != ModeOAuth {
		t.Fatal("a supported provider connects by OAuth")
	}
	for _, id := range []providers.ProviderID{"keyonly", "operatoroff", "openai", "anthropic", "never-heard-of"} {
		if eng.Mode(id) != ModeManualAPIKey {
			t.Errorf("%s: mode %s", id, eng.Mode(id))
		}
		_, err := eng.Prepare(bg, g.scope, PrepareRequest{ProviderID: id, Capabilities: []string{"chat"}})
		if !IsManualFallback(err) {
			t.Errorf("%s: Prepare = %v, want the manual fallback", id, err)
		}
		_, err = eng.Begin(bg, g.scope, BeginRequest{PrepareRequest: PrepareRequest{ProviderID: id, Capabilities: []string{"chat"}}, RedirectURI: registeredRedirect})
		if !IsManualFallback(err) {
			t.Errorf("%s: Begin = %v, want the manual fallback", id, err)
		}
		var ce *contracts.Error
		if !asContracts(err, &ce) || ce.Details["fallback"] != "api_key" {
			t.Errorf("%s: the error must name the api_key fallback: %v", id, err)
		}
	}
	if g.idp.total() != 0 {
		t.Fatal("a provider without OAuth caused a network call")
	}
	// The recorded support: key-based providers have none, Google offers OAuth, each with a dated source.
	for id, want := range map[providers.ProviderID]bool{"openai": false, "anthropic": false, "gemini": true} {
		s, ok := SupportFor(id)
		if !ok || s.OAuth != want || s.Source == "" || !strings.HasPrefix(s.Checked, "2026-") {
			t.Errorf("support for %s: %+v", id, s)
		}
	}
	if _, ok := SupportFor("meta"); ok {
		t.Error("an unverified provider must not be recorded")
	}
}

func TestProviderConfigValidation(t *testing.T) {
	g := newRig(t)
	mut := func(f func(*ProviderConfig)) error {
		c := testProvider(g.idp)
		f(&c)
		_, err := c.normalized()
		return err
	}
	for name, f := range map[string]func(*ProviderConfig){
		"no client":            func(c *ProviderConfig) { c.ClientID = "" },
		"plain http endpoint":  func(c *ProviderConfig) { c.TokenEndpoint = "http://idp.example.test/token" },
		"scope not listed":     func(c *ProviderConfig) { c.Capabilities["chat"] = []string{"scope.unlisted"} },
		"reserved extra param": func(c *ProviderConfig) { c.ExtraAuthParams = map[string]string{"redirect_uri": "x"} },
		"secret without auth":  func(c *ProviderConfig) { c.ClientAuth = ClientAuthPost },
		"public with secret":   func(c *ProviderConfig) { c.ClientSecretRef = "cred_X"; c.ClientAuth = ClientAuthNone },
		"http registered":      func(c *ProviderConfig) { c.RedirectURIs = []string{"http://alix.example.test/cb"} },
		"no redirect at all":   func(c *ProviderConfig) { c.RedirectURIs, c.Loopback = nil, false },
	} {
		if err := mut(f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := mut(func(c *ProviderConfig) {}); err != nil {
		t.Fatalf("the test provider must be valid: %v", err)
	}
}
