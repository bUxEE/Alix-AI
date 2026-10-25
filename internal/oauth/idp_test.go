package oauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
)

// idp is a mock identity provider on a plain-HTTP loopback httptest server. It is the only "provider" the tests
// talk to: nothing here contacts a real one. Every value it issues is a TESTONLY canary.
type idp struct {
	t   *testing.T
	srv *httptest.Server
	clk clock.Clock

	mu      sync.Mutex
	codes   map[string]*codeRec
	access  map[string]string // access value -> grant
	refresh map[string]string // refresh value -> grant
	grants  map[string]*grantRec
	seq     int
	reqs    []reqRec
	revoked []string // hints of the revocation calls
	lastQ   url.Values

	// behaviour knobs
	rotate      bool
	account     string // the account that answers the next authorization
	deny        bool
	oidc        bool
	idNonce     string // overrides the nonce in the ID token ("" = the real one)
	idAud       string
	idIss       string
	idExp       int64
	omitID      bool
	extraScope  string // a scope granted in addition to the requested ones
	expiresIn   int
	delay       time.Duration
	hold        chan struct{} // when set, a refresh request waits until it is closed
	down        bool
	clientID    string
	clientAuth  string // "", "post" or "basic"
	secret      string
	refreshHits atomic.Int32
	tokenHits   atomic.Int32
}

type codeRec struct {
	challenge, redirect, nonce, scope, account, client string
	used                                               bool
}

type grantRec struct {
	account string
	revoked bool
}

type reqRec struct{ path, rawQuery, method, remote string }

func newIDP(t *testing.T, clk clock.Clock) *idp {
	p := &idp{t: t, clk: clk, codes: map[string]*codeRec{}, access: map[string]string{}, refresh: map[string]string{},
		grants: map[string]*grantRec{}, account: "acct_A", expiresIn: 3600, clientID: "client-alix", rotate: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", p.authorize)
	mux.HandleFunc("/token", p.token)
	mux.HandleFunc("/revoke", p.revoke)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) url() string { return p.srv.URL }

func (p *idp) hostPort() (string, int) {
	u, _ := url.Parse(p.srv.URL)
	port, _ := strconv.Atoi(u.Port())
	return u.Hostname(), port
}

func (p *idp) record(r *http.Request) {
	p.mu.Lock()
	p.reqs = append(p.reqs, reqRec{path: r.URL.Path, rawQuery: r.URL.RawQuery, method: r.Method, remote: r.RemoteAddr})
	p.mu.Unlock()
}

// requests returns how many requests reached a path.
func (p *idp) requests(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.reqs {
		if r.path == path {
			n++
		}
	}
	return n
}

// conns returns how many distinct client connections the requests to a path came on.
func (p *idp) conns(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, r := range p.reqs {
		if r.path == path {
			seen[r.remote] = true
		}
	}
	return len(seen)
}

// grantOf returns the grant an access value was issued for.
func (p *idp) grantOf(access string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.access[access]
}

func (p *idp) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

func (p *idp) next(prefix string) string {
	p.seq++
	return fmt.Sprintf("TESTONLY-%s-%d", prefix, p.seq)
}

// authorize plays the user's browser consent: it records what the engine asked for and answers with the
// redirect the real provider would.
func (p *idp) authorize(w http.ResponseWriter, r *http.Request) {
	p.record(r)
	q := r.URL.Query()
	p.mu.Lock()
	p.lastQ = q
	p.mu.Unlock()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("state") == "" || q.Get("redirect_uri") == "" || q.Get("client_id") != p.clientID {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	loc, _ := url.Parse(q.Get("redirect_uri"))
	out := loc.Query()
	out.Set("state", q.Get("state"))
	if p.oidc {
		out.Set("iss", p.srv.URL)
	}
	if p.deny {
		out.Set("error", "access_denied")
	} else {
		p.mu.Lock()
		code := p.next("code")
		p.codes[code] = &codeRec{challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), nonce: q.Get("nonce"),
			scope: q.Get("scope"), account: p.account, client: q.Get("client_id")}
		p.mu.Unlock()
		out.Set("code", code)
	}
	loc.RawQuery = out.Encode()
	http.Redirect(w, r, loc.String(), http.StatusFound)
}

func (p *idp) clientOK(r *http.Request) bool {
	switch p.clientAuth {
	case "post":
		return r.PostForm.Get("client_id") == p.clientID && r.PostForm.Get("client_secret") == p.secret
	case "basic":
		u, pw, ok := r.BasicAuth()
		return ok && u == p.clientID && pw == p.secret && r.PostForm.Get("client_secret") == ""
	}
	return r.PostForm.Get("client_id") == p.clientID && r.PostForm.Get("client_secret") == ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthErr(w http.ResponseWriter, code string) {
	// the description is deliberately a canary: the engine must never copy provider text into an error
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": "TESTONLY-provider-text-" + code})
}

func (p *idp) token(w http.ResponseWriter, r *http.Request) {
	p.record(r)
	p.tokenHits.Add(1)
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		http.Error(w, "token endpoint takes a POST form", http.StatusBadRequest)
		return
	}
	_ = r.ParseForm()
	if p.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if !p.clientOK(r) {
		oauthErr(w, "invalid_client")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		p.mu.Lock()
		c := p.codes[r.PostForm.Get("code")]
		if c == nil || c.used || c.redirect != r.PostForm.Get("redirect_uri") || c.client != p.clientID {
			p.mu.Unlock()
			oauthErr(w, "invalid_grant")
			return
		}
		c.used = true
		sum := challengeOf(r.PostForm.Get("code_verifier"))
		if sum != c.challenge {
			p.mu.Unlock()
			oauthErr(w, "invalid_grant")
			return
		}
		grant := p.next("grant")
		p.grants[grant] = &grantRec{account: c.account}
		resp := p.issueLocked(grant, c.scope, c.nonce)
		p.mu.Unlock()
		writeJSON(w, http.StatusOK, resp)
	case "refresh_token":
		p.refreshHits.Add(1)
		if p.delay > 0 {
			time.Sleep(p.delay)
		}
		if h := p.hold; h != nil {
			select {
			case <-h:
			case <-time.After(10 * time.Second):
			}
		}
		p.mu.Lock()
		rv := r.PostForm.Get("refresh_token")
		grant, ok := p.refresh[rv]
		if !ok || p.grants[grant].revoked {
			p.mu.Unlock()
			oauthErr(w, "invalid_grant")
			return
		}
		if p.rotate {
			delete(p.refresh, rv) // the old refresh value is retired
		}
		resp := p.issueLocked(grant, "", "")
		if !p.rotate {
			delete(resp, "refresh_token")
		}
		p.mu.Unlock()
		writeJSON(w, http.StatusOK, resp)
	default:
		oauthErr(w, "unsupported_grant_type")
	}
}

func challengeOf(v string) string {
	return ChallengeS256(credentialsSecret(v))
}

// issueLocked mints a token response for a grant. scope "" reuses the grant's last scope.
func (p *idp) issueLocked(grant, scope, nonce string) map[string]any {
	acc, ref := p.next("access"), p.next("refresh")
	p.access[acc] = grant
	p.refresh[ref] = grant
	g := p.grants[grant]
	resp := map[string]any{"access_token": acc, "token_type": "Bearer", "expires_in": p.expiresIn, "refresh_token": ref,
		"account_id": g.account, "org_id": "org_1"}
	if scope != "" {
		if p.extraScope != "" {
			scope += " " + p.extraScope
		}
		resp["scope"] = scope
	}
	if p.oidc && nonce != "" && !p.omitID {
		resp["id_token"] = p.idToken(g.account, nonce)
	}
	return resp
}

func (p *idp) idToken(account, nonce string) string {
	if p.idNonce != "" {
		nonce = p.idNonce
	}
	aud, iss := p.clientID, p.srv.URL
	if p.idAud != "" {
		aud = p.idAud
	}
	if p.idIss != "" {
		iss = p.idIss
	}
	exp := p.clk.Now().Add(time.Hour).Unix()
	if p.idExp != 0 {
		exp = p.idExp
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	return enc(map[string]string{"alg": "none"}) + "." +
		enc(map[string]any{"iss": iss, "sub": account, "aud": aud, "exp": exp, "nonce": nonce}) + ".sig"
}

func (p *idp) revoke(w http.ResponseWriter, r *http.Request) {
	p.record(r)
	if r.Method != http.MethodPost || r.URL.RawQuery != "" {
		http.Error(w, "revocation takes a POST form", http.StatusBadRequest)
		return
	}
	_ = r.ParseForm()
	if p.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if !p.clientOK(r) {
		oauthErr(w, "invalid_client")
		return
	}
	tok, hint := r.PostForm.Get("token"), r.PostForm.Get("token_type_hint")
	p.mu.Lock()
	p.revoked = append(p.revoked, hint)
	if g, ok := p.refresh[tok]; ok {
		p.grants[g].revoked = true
	} else if g, ok := p.access[tok]; ok {
		p.grants[g].revoked = true
	}
	p.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// revokeAllGrants simulates the user revoking the app in the provider's account settings.
func (p *idp) revokeAllGrants() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, g := range p.grants {
		g.revoked = true
	}
}

func (p *idp) revokedHints() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.revoked...)
}

func (p *idp) authQuery() url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastQ
}

// browse plays the browser: it opens the authorization URL (without following the redirect) and returns the
// callback the gateway would receive.
func browse(t *testing.T, authURL, redirectURI string) CallbackRequest {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(authURL)
	if err != nil {
		t.Fatalf("browser: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorization endpoint answered %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(loc.String(), redirectURI) {
		t.Fatalf("the provider redirected to %q, not the bound redirect", loc.Host)
	}
	return CallbackFromQuery(redirectURI, loc.Query())
}
