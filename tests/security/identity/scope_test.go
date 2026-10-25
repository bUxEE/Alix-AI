package identitysec

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// scopeLevelSpec describes one level of the scope as a request could try to
// name it: the member names an SDK or a client might use for it, the headers a
// proxy or a "convenient" client might send, the value an attacker would put
// there and the value the token really carries.
type scopeLevelSpec struct {
	level    string
	names    []string
	headers  []string
	attacker string
	// own is the value the token carries; the control requests use it.
	own func(sub string) string
}

var scopeLevels = []scopeLevelSpec{
	{
		level:    "application",
		names:    []string{"application_id", "applicationId", "app_id", "application"},
		headers:  []string{"X-Application-Id", "X-Alixai-Application-Id", "X-App-Id"},
		attacker: "app_attacker",
		own:      func(string) string { return identitytest.DefaultApplicationID },
	},
	{
		level:    "tenant",
		names:    []string{"tenant_id", "tenantId", "tenant", "org_id"},
		headers:  []string{"X-Tenant-Id", "X-Alixai-Tenant-Id", "X-Tenant", "X-Org-Id"},
		attacker: "ten_attacker",
		own:      func(string) string { return identitytest.DefaultTenant },
	},
	{
		level:    "workspace",
		names:    []string{"workspace_id", "workspaceId", "workspace"},
		headers:  []string{"X-Workspace-Id", "X-Alixai-Workspace-Id", "X-Workspace"},
		attacker: "wsp_attacker",
		own:      func(string) string { return identitytest.DefaultWorkspace },
	},
	{
		level:    "principal",
		names:    []string{"principal_id", "principalId", "sub", "user_id", "userId", "subject_id", "user"},
		headers:  []string{"X-Principal-Id", "X-User-Id", "X-Alixai-Principal-Id", "X-Subject"},
		attacker: "usr_attacker",
		own:      func(sub string) string { return sub },
	},
	{
		level:    "session",
		names:    []string{"session_id", "sessionId", "session"},
		headers:  []string{"X-Session-Id", "X-Alixai-Session-Id"},
		attacker: "ses_attacker",
		own:      func(string) string { return identitytest.DefaultSession },
	},
	{
		level:    "deployment",
		names:    []string{"deployment_id", "deploymentId", "deployment"},
		headers:  []string{"X-Deployment-Id", "X-Alixai-Deployment-Id"},
		attacker: "dep_attacker",
		own:      func(string) string { return identitytest.DefaultDeployment },
	},
	{
		// Roles and capability scopes are levels of the scope too (master plan
		// 10): a request that could name them could grant itself anything.
		level:    "roles_and_capabilities",
		names:    []string{"roles", "role", "capability_scopes", "capabilityScopes", "scopes", "permissions"},
		headers:  []string{"X-Roles", "X-Capability-Scopes", "X-Scopes"},
		attacker: "admin,settings.deployment.write,settings.tenant.write",
		own:      func(string) string { return "member" },
	},
}

// attackerValues are the strings that must never reach the scope, a row or a
// response, whichever level they were sent for.
func attackerValues() []string {
	var out []string
	for _, l := range scopeLevels {
		out = append(out, l.attacker)
	}
	return out
}

// outcome is what a request is required to do.
type outcome int

const (
	// mustAccept: the request is served, with the token's scope.
	mustAccept outcome = iota
	// mustRefuseBeforeService: the request is refused with a 4xx and the
	// settings service is never called.
	mustRefuseBeforeService
	// mayRefuse: the request is served with the token's scope or refused;
	// either is safe, a different scope is not.
	mayRefuse
)

// expected is the Scope a token minted by r.token(sub) verifies to.
func (r *rig) expected(sub string) contracts.Scope {
	sc := identitytest.ExpectedScope()
	sc.PrincipalID = sub
	sc.CapabilityScopes = userScopes()
	return sc
}

// exercise sends req with a fresh token for sub and returns what is wrong with
// the outcome: the empty slice is a request that was scoped by the token only.
// It is the checker of the whole test, and the controls run it against a
// deliberately leaky rig to prove it can see a violation.
func (r *rig) exercise(t *testing.T, sub string, req request, want outcome) (violations []string, rec *httptest.ResponseRecorder) {
	t.Helper()
	before := r.rows(t)
	r.rec.take() // anything recorded so far belongs to an earlier request
	rec = r.do(t, r.token(sub), req)
	calls := r.rec.take()
	after := r.rows(t)
	fail := func(format string, args ...any) {
		violations = append(violations, fmt.Sprintf(format, args...))
	}

	// 1. The scope that reached the service is the token's, level by level.
	want0 := r.expected(sub)
	for i, c := range calls {
		if diff := cmp.Diff(want0, c.scope); diff != "" {
			fail("call %d (%s) reached the service with a scope that differs from the token's (-token +service):\n%s", i, c.method, diff)
		}
	}

	// 2. What was created lies in the token's tenant, workspace and principal,
	// and nothing of the attacker's is in the store.
	newRows := after[len(before):]
	for _, row := range newRows {
		own := documentRow{
			application: want0.ApplicationID, tenant: want0.TenantID, workspace: want0.WorkspaceID,
			level: "user", subject: sub,
		}
		if row != own {
			fail("a settings document was created outside the token's scope: %+v, want %+v", row, own)
		}
	}

	// 3. Nothing the request carried is echoed back as scope.
	for _, a := range attackerValues() {
		if strings.Contains(rec.Body.String(), a) {
			fail("the response carries the attacker's value %q: %s", a, rec.Body.String())
		}
	}

	// 4. The outcome the channel is required to have.
	switch want {
	case mustAccept:
		if rec.Code != http.StatusOK {
			fail("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(calls) != 1 {
			fail("the service was called %d times, want 1", len(calls))
		}
		if req.method == http.MethodPut && len(newRows) != 1 {
			fail("a PUT created %d documents, want 1", len(newRows))
		}
	case mustRefuseBeforeService:
		if rec.Code < 400 || rec.Code >= 500 {
			fail("status = %d, want a 4xx refusal: %s", rec.Code, rec.Body.String())
		}
		if len(calls) != 0 {
			fail("the service was called %d times for a request that must be refused first", len(calls))
		}
		if len(newRows) != 0 {
			fail("a refused request created %d documents", len(newRows))
		}
	case mayRefuse:
		if rec.Code != http.StatusOK && (rec.Code < 400 || rec.Code >= 500) {
			fail("status = %d, want 200 or a 4xx: %s", rec.Code, rec.Body.String())
		}
		if rec.Code != http.StatusOK && len(newRows) != 0 {
			fail("a refused request created %d documents", len(newRows))
		}
	}
	return violations, rec
}

const validPut = `{"expected_version":0,"settings":{"language":"en"}}`

// channel builds the request that carries the values of one level through one
// channel.
type channel struct {
	name string
	want outcome
	// build returns the request. value is what the request says the level is.
	build func(l scopeLevelSpec, value string) request
}

func queryOf(l scopeLevelSpec, value string) string {
	v := url.Values{}
	for _, n := range l.names {
		v.Set(n, value)
	}
	return v.Encode()
}

func headersOf(l scopeLevelSpec, value string) map[string]string {
	h := map[string]string{}
	for _, n := range l.headers {
		h[n] = value
	}
	return h
}

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func channels(t testing.TB) []channel {
	return []channel{
		{
			name: "GET query string",
			want: mustAccept,
			build: func(l scopeLevelSpec, v string) request {
				return request{method: http.MethodGet, target: "/v1/settings/user?" + queryOf(l, v)}
			},
		},
		{
			name: "GET headers",
			want: mustAccept,
			build: func(l scopeLevelSpec, v string) request {
				return request{method: http.MethodGet, target: "/v1/settings/user", headers: headersOf(l, v)}
			},
		},
		{
			name: "GET JSON body with SDK-style members",
			want: mustAccept,
			build: func(l scopeLevelSpec, v string) request {
				flat, nested := map[string]any{}, map[string]any{}
				for _, n := range l.names {
					flat[n], nested[n] = v, v
				}
				flat["scope"] = nested
				flat["context"] = nested
				return request{method: http.MethodGet, target: "/v1/settings/user", body: mustJSON(t, flat)}
			},
		},
		{
			name: "GET query, headers and body together",
			want: mustAccept,
			build: func(l scopeLevelSpec, v string) request {
				flat := map[string]any{}
				for _, n := range l.names {
					flat[n] = v
				}
				return request{method: http.MethodGet, target: "/v1/settings/user?" + queryOf(l, v), headers: headersOf(l, v), body: mustJSON(t, flat)}
			},
		},
		{
			name: "PUT query string and headers, valid body (creates a document)",
			want: mustAccept,
			build: func(l scopeLevelSpec, v string) request {
				return request{method: http.MethodPut, target: "/v1/settings/user?" + queryOf(l, v), headers: headersOf(l, v), body: validPut}
			},
		},
		{
			name: "PUT top-level body member",
			want: mustRefuseBeforeService,
			build: func(l scopeLevelSpec, v string) request {
				body := map[string]any{"expected_version": 0, "settings": map[string]any{"language": "en"}}
				for _, n := range l.names {
					body[n] = v
				}
				return request{method: http.MethodPut, target: "/v1/settings/user", body: mustJSON(t, body)}
			},
		},
		{
			name: "PUT body scope object",
			want: mustRefuseBeforeService,
			build: func(l scopeLevelSpec, v string) request {
				nested := map[string]any{}
				for _, n := range l.names {
					nested[n] = v
				}
				body := map[string]any{"expected_version": 0, "settings": map[string]any{"language": "en"}, "scope": nested}
				return request{method: http.MethodPut, target: "/v1/settings/user", body: mustJSON(t, body)}
			},
		},
		{
			name: "PUT member inside the settings document",
			want: mayRefuse,
			build: func(l scopeLevelSpec, v string) request {
				doc := map[string]any{"language": "en"}
				for _, n := range l.names {
					doc[n] = v
				}
				body := map[string]any{"expected_version": 0, "settings": doc}
				return request{method: http.MethodPut, target: "/v1/settings/user", body: mustJSON(t, body)}
			},
		},
	}
}

// SEC-035
//
// The scope of a request is the verified token's, and nothing else: not the
// body, the query string, a header or an SDK-style member, whatever level they
// name. A token that lacks a level is refused with scope_missing, never given a
// default tenant or a wildcard workspace.
func TestSecScopeComesOnlyFromToken(t *testing.T) {
	env, svc := newEnv(t)
	good := newRig(t, env, svc, identity.Middleware(env.Verifier))

	// Control first: a plain request is served with the token's scope, so every
	// refusal below is caused by what the table adds, not by the rig.
	t.Run("control: a clean request is served with the token's scope", func(t *testing.T) {
		for _, req := range []request{
			{method: http.MethodGet, target: "/v1/settings/user"},
			{method: http.MethodPut, target: "/v1/settings/user", body: validPut},
		} {
			if v, rec := good.exercise(t, good.sub(), req, mustAccept); len(v) != 0 {
				t.Errorf("%s %s: %s\n%s", req.method, req.target, strings.Join(v, "\n"), rec.Body.String())
			}
		}
	})

	chans := channels(t)
	t.Run("a different value in any channel never reaches the scope", func(t *testing.T) {
		for _, l := range scopeLevels {
			for _, ch := range chans {
				t.Run(l.level+"/"+ch.name, func(t *testing.T) {
					sub := good.sub()
					if v, _ := good.exercise(t, sub, ch.build(l, l.attacker), ch.want); len(v) != 0 {
						t.Error(strings.Join(v, "\n"))
					}
				})
			}
		}
	})

	t.Run("control: the same requests with matching values are accepted the same way", func(t *testing.T) {
		for _, l := range scopeLevels {
			for _, ch := range chans {
				t.Run(l.level+"/"+ch.name, func(t *testing.T) {
					sub := good.sub()
					if v, _ := good.exercise(t, sub, ch.build(l, l.own(sub)), ch.want); len(v) != 0 {
						t.Error(strings.Join(v, "\n"))
					}
				})
			}
		}
	})

	t.Run("every level at once, in every channel", func(t *testing.T) {
		for _, ch := range chans {
			t.Run(ch.name, func(t *testing.T) {
				sub := good.sub()
				all := scopeLevelSpec{attacker: "ten_attacker"}
				for _, l := range scopeLevels {
					all.names = append(all.names, l.names...)
					all.headers = append(all.headers, l.headers...)
				}
				if v, _ := good.exercise(t, sub, ch.build(all, "ten_attacker"), ch.want); len(v) != 0 {
					t.Error(strings.Join(v, "\n"))
				}
			})
		}
	})

	t.Run("the credential is read from the Authorization header only", func(t *testing.T) {
		before := len(good.rows(t))
		good.rec.take()
		sub := good.sub()
		for name, req := range map[string]request{
			"no credential, every scope channel filled": {
				method:  http.MethodGet,
				target:  "/v1/settings/user?tenant_id=ten_attacker&workspace_id=wsp_attacker&principal_id=usr_attacker&application_id=app_demo",
				headers: map[string]string{"X-Tenant-Id": "ten_attacker", "X-Workspace-Id": "wsp_attacker", "X-Principal-Id": "usr_attacker", "X-Application-Id": "app_demo"},
				body:    `{"tenant_id":"ten_attacker"}`,
			},
			"a valid token in the query string": {
				method: http.MethodGet,
				target: "/v1/settings/user?access_token=" + url.QueryEscape(good.token(sub)) + "&token=" + url.QueryEscape(good.token(sub)),
			},
			"a valid token in a cookie": {
				method:  http.MethodGet,
				target:  "/v1/settings/user",
				headers: map[string]string{"Cookie": "token=" + good.token(sub)},
			},
		} {
			rec := good.do(t, "", req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want 401: %s", name, rec.Code, rec.Body.String())
			}
			if got := errorCode(t, rec); got != contracts.ErrUnauthenticated {
				t.Errorf("%s: code = %q, want %q", name, got, contracts.ErrUnauthenticated)
			}
		}
		if calls := good.rec.take(); len(calls) != 0 {
			t.Errorf("the service was called %d times without a verified token", len(calls))
		}
		if after := len(good.rows(t)); after != before {
			t.Errorf("rows %d -> %d: an unauthenticated request changed the store", before, after)
		}
	})

	t.Run("a token carrying a deployment level or any extra claim is refused", func(t *testing.T) {
		for name, mod := range map[string]func(*identitytest.Draft){
			"deployment_id claim": func(d *identitytest.Draft) { d.Set("deployment_id", "dep_attacker") },
			"tenant claim":        func(d *identitytest.Draft) { d.Set("tenant", "ten_attacker") },
			"scope claim":         func(d *identitytest.Draft) { d.Set("scope", map[string]any{"tenant_id": "ten_attacker"}) },
		} {
			good.rec.take()
			rec := good.do(t, good.token(good.sub(), mod), request{method: http.MethodGet, target: "/v1/settings/user"})
			if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != contracts.ErrUnauthenticated {
				t.Errorf("%s: status %d code %q, want 401 unauthenticated: %s", name, rec.Code, errorCode(t, rec), rec.Body.String())
			}
			if identity.Reason(errorReason(t, rec)) != identity.ReasonBadClaims {
				t.Errorf("%s: reason = %q, want %q", name, errorReason(t, rec), identity.ReasonBadClaims)
			}
			if calls := good.rec.take(); len(calls) != 0 {
				t.Errorf("%s: the service was called", name)
			}
		}
	})

	t.Run("a token that lacks a level is scope_missing; nothing is defaulted", func(t *testing.T) {
		var missing []levelCase
		for _, lv := range []struct{ level, claim string }{
			{"application", identity.ClaimApplication},
			{"tenant", identity.ClaimTenant},
			{"workspace", identity.ClaimWorkspace},
			{"principal", identity.ClaimSubject},
			{"session", identity.ClaimSession},
		} {
			claim := lv.claim
			missing = append(missing,
				levelCase{lv.level + ": claim absent", lv.level, func(d *identitytest.Draft) { d.Drop(claim) }},
				levelCase{lv.level + ": empty string", lv.level, func(d *identitytest.Draft) { d.Set(claim, "") }},
			)
			if lv.level != "application" {
				// A blank application_id is covered by TestSecScopeBlankApplicationIsScopeMissing
				// (a known defect: the verifier answers unauthenticated, not scope_missing).
				missing = append(missing, levelCase{lv.level + ": blank", lv.level, func(d *identitytest.Draft) { d.Set(claim, "   ") }})
			}
		}
		for _, lv := range []struct{ level, claim string }{
			{"roles", identity.ClaimRoles},
			{"capability_scopes", identity.ClaimCapabilityScopes},
		} {
			claim := lv.claim
			missing = append(missing,
				levelCase{lv.level + ": claim absent", lv.level, func(d *identitytest.Draft) { d.Drop(claim) }},
				levelCase{lv.level + ": empty list", lv.level, func(d *identitytest.Draft) { d.Set(claim, []string{}) }},
				levelCase{lv.level + ": blank entry", lv.level, func(d *identitytest.Draft) { d.Set(claim, []string{""}) }},
			)
		}

		for _, m := range missing {
			t.Run(m.name, func(t *testing.T) {
				good.rec.take()
				before := len(good.rows(t))
				sub := good.sub()
				// A token with the level missing has to override whatever the
				// helper put there, so the modification is applied last.
				rec := good.do(t, good.token(sub, m.mod), request{method: http.MethodGet, target: "/v1/settings/user"})
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("status = %d, want 401: %s", rec.Code, rec.Body.String())
				}
				if got := errorCode(t, rec); got != contracts.ErrScopeMissing {
					t.Errorf("code = %q, want %q: %s", got, contracts.ErrScopeMissing, rec.Body.String())
				}
				if got := errorLevels(t, rec); !cmp.Equal(got, []string{m.level}) {
					t.Errorf("levels = %v, want exactly [%s]", got, m.level)
				}
				if calls := good.rec.take(); len(calls) != 0 {
					t.Errorf("the service was called %d times with an incomplete scope", len(calls))
				}
				// The write path refuses it too, and creates nothing.
				rec = good.do(t, good.token(sub, m.mod), request{method: http.MethodPut, target: "/v1/settings/user", body: validPut})
				if got := errorCode(t, rec); got != contracts.ErrScopeMissing {
					t.Errorf("PUT code = %q, want %q", got, contracts.ErrScopeMissing)
				}
				if after := len(good.rows(t)); after != before {
					t.Errorf("rows %d -> %d: a token without %s created a document (a default level?)", before, after, m.level)
				}
				// Control: the same request with every level present is served.
				if v, _ := good.exercise(t, sub, request{method: http.MethodGet, target: "/v1/settings/user"}, mustAccept); len(v) != 0 {
					t.Errorf("control request with a complete token: %s", strings.Join(v, "\n"))
				}
			})
		}
	})

	t.Run("a wildcard or malformed level is scope_violation, never a wildcard grant", func(t *testing.T) {
		long := strings.Repeat("x", contracts.MaxScopeValueLen+1)
		cases := []levelCase{
			{"workspace *", "workspace", func(d *identitytest.Draft) { d.Set(identity.ClaimWorkspace, "*") }},
			{"tenant *", "tenant", func(d *identitytest.Draft) { d.Set(identity.ClaimTenant, "*") }},
			{"principal *", "principal", func(d *identitytest.Draft) { d.Set(identity.ClaimSubject, "*") }},
			{"session *", "session", func(d *identitytest.Draft) { d.Set(identity.ClaimSession, "*") }},
			{"workspace padded", "workspace", func(d *identitytest.Draft) { d.Set(identity.ClaimWorkspace, " wsp_main") }},
			{"tenant control character", "tenant", func(d *identitytest.Draft) { d.Set(identity.ClaimTenant, "ten\u0000acme") }},
			{"workspace over-long", "workspace", func(d *identitytest.Draft) { d.Set(identity.ClaimWorkspace, long) }},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				good.rec.take()
				before := len(good.rows(t))
				rec := good.do(t, good.token(good.sub(), c.mod), request{method: http.MethodPut, target: "/v1/settings/user", body: validPut})
				if rec.Code != http.StatusForbidden {
					t.Errorf("status = %d, want 403: %s", rec.Code, rec.Body.String())
				}
				if got := errorCode(t, rec); got != contracts.ErrScopeViolation {
					t.Errorf("code = %q, want %q", got, contracts.ErrScopeViolation)
				}
				if got := errorLevels(t, rec); !cmp.Equal(got, []string{c.level}) {
					t.Errorf("levels = %v, want [%s]", got, c.level)
				}
				if calls := good.rec.take(); len(calls) != 0 {
					t.Errorf("the service was called %d times", len(calls))
				}
				if after := len(good.rows(t)); after != before {
					t.Errorf("rows %d -> %d: a malformed level created a document", before, after)
				}
			})
		}
		// An application the signing key is not registered for is refused as a
		// credential problem (the key speaks for one application only).
		rec := good.do(t, good.token(good.sub(), func(d *identitytest.Draft) { d.Set(identity.ClaimApplication, "*") }),
			request{method: http.MethodGet, target: "/v1/settings/user"})
		if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != contracts.ErrUnauthenticated {
			t.Errorf("application *: status %d code %q, want 401 unauthenticated", rec.Code, errorCode(t, rec))
		}
	})

	t.Run("the constructor of every scope has no default for any level", func(t *testing.T) {
		_, err := identity.NewScope(identitytest.DefaultDeployment, identity.ContextClaims{})
		if got := contracts.CodeOf(err); got != contracts.ErrScopeMissing {
			t.Fatalf("an empty context gave %v, want scope_missing", err)
		}
		var ce *contracts.Error
		if !errors.As(err, &ce) {
			t.Fatalf("not a typed error: %v", err)
		}
		levels, _ := ce.Details["levels"].([]string)
		want := []string{"application", "tenant", "workspace", "principal", "roles", "capability_scopes", "session"}
		if !cmp.Equal(levels, want) {
			t.Fatalf("missing levels = %v, want every level but the deployment: %v", levels, want)
		}
	})

	// The controls that make the checker itself trustworthy: the same checker
	// run against a deliberately leaky gateway must see the violation, and must
	// stay quiet on a clean request through the same gateway.
	t.Run("controls: the checker flags a gateway that lets headers or the query string pick the tenant", func(t *testing.T) {
		cenv, csvc := newEnv(t)
		verifier := identity.Middleware(cenv.Verifier)
		fromHeader := newRig(t, cenv, csvc, leakyAuth(verifier, func(r *http.Request) string { return r.Header.Get("X-Tenant-Id") }))
		fromQuery := newRig(t, cenv, csvc, leakyAuth(verifier, func(r *http.Request) string { return r.URL.Query().Get("tenant_id") }))
		tenant := scopeLevels[1]

		type probe struct {
			rig  *rig
			name string
			req  request
		}
		for _, p := range []probe{
			{fromHeader, "header read", channels(t)[1].buildFor(tenant, tenant.attacker)},
			{fromHeader, "header write", channels(t)[4].buildFor(tenant, tenant.attacker)},
			{fromQuery, "query read", channels(t)[0].buildFor(tenant, tenant.attacker)},
			{fromQuery, "query write", channels(t)[4].buildFor(tenant, tenant.attacker)},
		} {
			v, _ := p.rig.exercise(t, p.rig.sub(), p.req, mustAccept)
			if len(v) == 0 {
				t.Errorf("%s: the checker did not notice a gateway that scopes by the request", p.name)
			}
			if p.req.method == http.MethodPut && !containsAny(v, "created outside the token's scope") {
				t.Errorf("%s: the checker did not notice the document created in the attacker's tenant: %v", p.name, v)
			}
		}
		// A clean request through the leaky gateways is not flagged.
		for name, rg := range map[string]*rig{"header": fromHeader, "query": fromQuery} {
			if v, _ := rg.exercise(t, rg.sub(), request{method: http.MethodGet, target: "/v1/settings/user"}, mustAccept); len(v) != 0 {
				t.Errorf("%s: the checker flags a request that carries no tenant value: %v", name, v)
			}
		}
	})
}

// buildFor is channel.build for use in expressions.
func (c channel) buildFor(l scopeLevelSpec, v string) request { return c.build(l, v) }

func containsAny(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// errorEnvelope decodes the typed error body of a refusal.
func errorEnvelope(t testing.TB, rec *httptest.ResponseRecorder) (code contracts.ErrCode, details map[string]any) {
	t.Helper()
	var env struct {
		Code    contracts.ErrCode `json:"code"`
		Details map[string]any    `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not a typed error: %v\n%s", err, rec.Body.String())
	}
	return env.Code, env.Details
}

func errorCode(t testing.TB, rec *httptest.ResponseRecorder) contracts.ErrCode {
	t.Helper()
	code, _ := errorEnvelope(t, rec)
	return code
}

func errorReason(t testing.TB, rec *httptest.ResponseRecorder) string {
	t.Helper()
	_, d := errorEnvelope(t, rec)
	s, _ := d[identity.DetailReason].(string)
	return s
}

// errorLevels is the sorted-as-sent list of scope levels a refusal names.
func errorLevels(t testing.TB, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	_, d := errorEnvelope(t, rec)
	raw, _ := d["levels"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// levelCase is one way a token can lack, or garble, one level of the scope.
type levelCase struct {
	name  string
	level string
	mod   func(*identitytest.Draft)
}
