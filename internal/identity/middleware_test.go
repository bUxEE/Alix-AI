package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// probe is the handler behind the middleware: it records what it was given.
type probe struct {
	calls   int
	scope   contracts.Scope
	scopeOK bool
	header  http.Header
	verif   *identity.Verified
}

func (p *probe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.calls++
	p.scope, p.scopeOK = identity.ScopeFromContext(r.Context())
	p.verif, _ = identity.VerifiedFromContext(r.Context())
	p.header = r.Header.Clone()
	w.WriteHeader(http.StatusNoContent)
}

func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func newReq(method, target string, hdr http.Header, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range hdr {
		r.Header[k] = v
	}
	return r
}

// errorBody decodes the typed error envelope of a refusal.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder) contracts.Error {
	t.Helper()
	var e contracts.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("response is not an error envelope: %v\n%s", err, rec.Body.String())
	}
	return e
}

func TestMiddlewareProducesVerifiedScope(t *testing.T) {
	env := identitytest.NewEnv(t)
	p := &probe{}
	h := identity.Middleware(env.Verifier)(p)

	token := env.Issuer.Token()
	rec := do(h, newReq("POST", "/v1/sessions", bearer(token), `{}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !p.scopeOK {
		t.Fatal("handler saw no scope")
	}
	if diff := cmp.Diff(identitytest.ExpectedScope(), p.scope); diff != "" {
		t.Fatalf("scope (-want +got):\n%s", diff)
	}
	if p.verif == nil || p.verif.TokenID == "" || p.verif.KeyID != env.Issuer.KeyID {
		t.Fatalf("verified metadata missing: %+v", p.verif)
	}
	// The bearer credential goes no further than the middleware.
	if got := p.header.Get("Authorization"); got != "" {
		t.Fatalf("downstream handler still sees the Authorization header: %q", got)
	}
}

// SEC-035: scope comes from the verified token and from nothing else.
func TestMiddlewareScopeFromTokenOnly(t *testing.T) {
	env := identitytest.NewEnv(t)
	p := &probe{}
	h := identity.Middleware(env.Verifier)(p)

	hdr := bearer(env.Issuer.Token())
	hdr.Set("X-Tenant-ID", "ten_evil")
	hdr.Set("X-Workspace-ID", "wsp_evil")
	hdr.Set("X-Principal-ID", "usr_evil")
	hdr.Set("X-Application-ID", "app_evil")
	hdr.Set("X-Session-ID", "ses_evil")
	hdr.Set("X-Deployment-ID", "dep_evil")
	hdr.Set("Cookie", "tenant_id=ten_evil")
	q := url.Values{"tenant_id": {"ten_evil"}, "workspace_id": {"wsp_evil"}, "principal_id": {"usr_evil"}}
	body := `{"tenant_id":"ten_evil","workspace_id":"wsp_evil","principal_id":"usr_evil","deployment_id":"dep_evil","roles":["root"]}`
	req := newReq("POST", "/v1/sessions?"+q.Encode(), hdr, body)
	req.Header.Set("Content-Type", "application/json")

	if rec := do(h, req); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if diff := cmp.Diff(identitytest.ExpectedScope(), p.scope); diff != "" {
		t.Fatalf("a request value reached the scope (-want +got):\n%s", diff)
	}
}

func TestMiddlewareRefusals(t *testing.T) {
	env := identitytest.NewEnv(t)
	other := identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: "app_other", Clock: env.Clock})
	if err := env.Keys.Register(other.Key()); err != nil {
		t.Fatal(err)
	}
	p := &probe{}
	h := identity.Middleware(env.Verifier)(p)

	replayed := env.Issuer.Token()
	if rec := do(h, newReq("GET", "/v1/x", bearer(replayed), "")); rec.Code != http.StatusNoContent {
		t.Fatalf("first use refused: %d %s", rec.Code, rec.Body)
	}
	p.calls = 0

	expired := env.Issuer.Draft().Times(-6*minute, -6*minute, -minute).Sign()
	tampered := withPayload(t, env.Issuer.Token(), func(m map[string]any) { m[identity.ClaimTenant] = "ten_other" })

	tokenInQuery := env.Issuer.Token()
	tokenInCookie := env.Issuer.Token()
	tokenInBody := env.Issuer.Token()

	cases := []struct {
		name   string
		req    *http.Request
		status int
		code   contracts.ErrCode
		reason identity.Reason
	}{
		{"no header", newReq("GET", "/v1/x", nil, ""), 401, contracts.ErrUnauthenticated, identity.ReasonNoCredential},
		{"token in query only", newReq("GET", "/v1/x?access_token="+tokenInQuery+"&token="+tokenInQuery, nil, ""), 401, contracts.ErrUnauthenticated, identity.ReasonNoCredential},
		{"token in cookie only", newReq("GET", "/v1/x", http.Header{"Cookie": {"token=" + tokenInCookie}}, ""), 401, contracts.ErrUnauthenticated, identity.ReasonNoCredential},
		{"token in body only", newReq("POST", "/v1/x", nil, `{"token":"`+tokenInBody+`"}`), 401, contracts.ErrUnauthenticated, identity.ReasonNoCredential},
		{"basic scheme", newReq("GET", "/v1/x", http.Header{"Authorization": {"Basic dXNlcjpwYXNz"}}, ""), 401, contracts.ErrUnauthenticated, identity.ReasonMalformed},
		{"bare token", newReq("GET", "/v1/x", http.Header{"Authorization": {env.Issuer.Token()}}, ""), 401, contracts.ErrUnauthenticated, identity.ReasonMalformed},
		{"two headers", newReq("GET", "/v1/x", http.Header{"Authorization": {"Bearer " + env.Issuer.Token(), "Bearer " + env.Issuer.Token()}}, ""), 401, contracts.ErrUnauthenticated, identity.ReasonMalformed},
		{"empty bearer", newReq("GET", "/v1/x", http.Header{"Authorization": {"Bearer "}}, ""), 401, contracts.ErrUnauthenticated, identity.ReasonMalformed},
		{"garbage", newReq("GET", "/v1/x", bearer("not-a-token"), ""), 401, contracts.ErrUnauthenticated, identity.ReasonMalformed},
		{"bad signature", newReq("GET", "/v1/x", bearer(tampered), ""), 401, contracts.ErrUnauthenticated, identity.ReasonBadSignature},
		{"expired", newReq("GET", "/v1/x", bearer(expired), ""), 401, contracts.ErrExpired, identity.ReasonExpired},
		{"replayed", newReq("GET", "/v1/x", bearer(replayed), ""), 401, contracts.ErrReplayed, identity.ReasonReplayed},
		{"missing workspace", newReq("GET", "/v1/x", bearer(env.Issuer.Token(func(d *identitytest.Draft) { d.Drop(identity.ClaimWorkspace) })), ""), 401, contracts.ErrScopeMissing, ""},
		{"wildcard tenant", newReq("GET", "/v1/x", bearer(env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimTenant, "*") })), ""), 403, contracts.ErrScopeViolation, ""},
		{"wrong audience", newReq("GET", "/v1/x", bearer(env.Issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimAudience, "dep_other") })), ""), 401, contracts.ErrUnauthenticated, identity.ReasonWrongAudience},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(h, c.req)
			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.status, rec.Body)
			}
			e := errorBody(t, rec)
			if e.Code != c.code {
				t.Fatalf("code = %s, want %s", e.Code, c.code)
			}
			if got, _ := e.Details[identity.DetailReason].(string); c.reason != "" && got != string(c.reason) {
				t.Fatalf("reason = %q, want %q", got, c.reason)
			}
			if e.Retryable {
				t.Fatal("an identity refusal must not be retryable")
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q", ct)
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("refusals must not be cached")
			}
			if c.status == 401 && rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("401 without a WWW-Authenticate challenge")
			}
		})
	}
	if p.calls != 0 {
		t.Fatalf("the handler ran %d times for refused requests", p.calls)
	}
}

func TestMiddlewareScopeMissingNamesLevels(t *testing.T) {
	env := identitytest.NewEnv(t)
	h := identity.Middleware(env.Verifier)(&probe{})
	token := env.Issuer.Token(func(d *identitytest.Draft) { d.Drop(identity.ClaimWorkspace) })
	rec := do(h, newReq("GET", "/v1/x", bearer(token), ""))
	e := errorBody(t, rec)
	levels, _ := e.Details["levels"].([]any)
	if rec.Code != 401 || e.Code != contracts.ErrScopeMissing || len(levels) != 1 || levels[0] != "workspace" {
		t.Fatalf("response = %d %+v", rec.Code, e)
	}
}

// Replay-cache and key-store faults are 503 and never acceptance.
func TestMiddlewareUnavailableFailsClosed(t *testing.T) {
	env := identitytest.NewEnv(t)
	v, err := identity.NewVerifier(identity.Options{
		DeploymentID: identitytest.DefaultDeployment,
		Keys:         env.Keys,
		Replay:       failingReplay{},
		Clock:        env.Clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := &probe{}
	rec := do(identity.Middleware(v)(p), newReq("GET", "/v1/x", bearer(env.Issuer.Token()), ""))
	if rec.Code != http.StatusServiceUnavailable || p.calls != 0 {
		t.Fatalf("status = %d, handler calls = %d", rec.Code, p.calls)
	}
	e := errorBody(t, rec)
	if e.Code != contracts.ErrUnavailable || !e.Retryable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("envelope = %+v, Retry-After = %q", e, rec.Header().Get("Retry-After"))
	}
	if strings.Contains(rec.Body.String(), errBroken.Error()) {
		t.Fatal("the internal cause leaked into the response")
	}
}

// Any Authenticator plugs into the middleware: the local IPC transport will
// produce a Verified from peer authentication the same way.
type fakeAuth struct {
	v   *identity.Verified
	err error
}

func (f fakeAuth) Authenticate(context.Context, string) (*identity.Verified, error) {
	return f.v, f.err
}

func TestMiddlewareWorksWithAnyAuthenticator(t *testing.T) {
	scope, err := identity.NewScope("dep_main", identity.ContextClaims{
		ApplicationID: "app_native", TenantID: "ten_a", WorkspaceID: "wsp_a", PrincipalID: "usr_a",
		Roles: []string{"member"}, CapabilityScopes: []string{"a.read"}, SessionID: "ses_a",
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("accepts", func(t *testing.T) {
		p := &probe{}
		h := identity.Middleware(fakeAuth{v: &identity.Verified{Scope: scope}})(p)
		if rec := do(h, newReq("GET", "/v1/x", bearer("peer-credential"), "")); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d", rec.Code)
		}
		if diff := cmp.Diff(scope, p.scope); diff != "" {
			t.Fatalf("(-want +got):\n%s", diff)
		}
	})
	t.Run("an authenticator returning an invalid scope is refused", func(t *testing.T) {
		p := &probe{}
		h := identity.Middleware(fakeAuth{v: &identity.Verified{}})(p)
		rec := do(h, newReq("GET", "/v1/x", bearer("x"), ""))
		if rec.Code != http.StatusUnauthorized || p.calls != 0 {
			t.Fatalf("status = %d, handler calls = %d", rec.Code, p.calls)
		}
	})
	t.Run("an authenticator returning nothing is refused", func(t *testing.T) {
		p := &probe{}
		h := identity.Middleware(fakeAuth{})(p)
		rec := do(h, newReq("GET", "/v1/x", bearer("x"), ""))
		if rec.Code != http.StatusUnauthorized || p.calls != 0 {
			t.Fatalf("status = %d, handler calls = %d", rec.Code, p.calls)
		}
	})
	t.Run("an untyped error is an internal refusal without its text", func(t *testing.T) {
		p := &probe{}
		h := identity.Middleware(fakeAuth{err: errors.New("database password is hunter2")})(p)
		rec := do(h, newReq("GET", "/v1/x", bearer("x"), ""))
		if rec.Code != http.StatusInternalServerError || p.calls != 0 {
			t.Fatalf("status = %d, handler calls = %d", rec.Code, p.calls)
		}
		if e := errorBody(t, rec); e.Code != contracts.ErrInternal || strings.Contains(rec.Body.String(), "hunter2") {
			t.Fatalf("envelope = %+v body = %s", e, rec.Body)
		}
	})
}

func TestBearerToken(t *testing.T) {
	long := strings.Repeat("a", identity.MaxTokenSize+1)
	cases := []struct {
		name   string
		header http.Header
		want   string
		reason identity.Reason
	}{
		{"plain", http.Header{"Authorization": {"Bearer abc.def.ghi"}}, "abc.def.ghi", ""},
		{"scheme is case-insensitive", http.Header{"Authorization": {"bEaReR abc.def.ghi"}}, "abc.def.ghi", ""},
		{"none", http.Header{}, "", identity.ReasonNoCredential},
		{"two headers", http.Header{"Authorization": {"Bearer a", "Bearer b"}}, "", identity.ReasonMalformed},
		{"wrong scheme", http.Header{"Authorization": {"Token abc"}}, "", identity.ReasonMalformed},
		{"no token", http.Header{"Authorization": {"Bearer"}}, "", identity.ReasonMalformed},
		{"empty token", http.Header{"Authorization": {"Bearer "}}, "", identity.ReasonMalformed},
		{"inner space", http.Header{"Authorization": {"Bearer a b"}}, "", identity.ReasonMalformed},
		{"tab", http.Header{"Authorization": {"Bearer a\tb"}}, "", identity.ReasonMalformed},
		{"too long", http.Header{"Authorization": {"Bearer " + long}}, "", identity.ReasonMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := identity.BearerToken(c.header)
			if c.reason == "" {
				if err != nil || got != c.want {
					t.Fatalf("BearerToken = %q, %v; want %q", got, err, c.want)
				}
				return
			}
			wantReject(t, err, contracts.ErrUnauthenticated, c.reason)
		})
	}
}

// Sanity for the clock used by the middleware tests: a token minted now is
// valid for its whole default lifetime.
func TestDefaultTokenLifetimeIsShort(t *testing.T) {
	if identitytest.DefaultLifetime >= identity.MaxLifetime {
		t.Fatalf("default test lifetime %s is not below the ceiling %s", identitytest.DefaultLifetime, identity.MaxLifetime)
	}
	if identity.MaxLifetime != 15*time.Minute {
		t.Fatalf("MaxLifetime = %s, ADR 0013 fixes it at 15 minutes", identity.MaxLifetime)
	}
}
