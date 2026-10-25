package identitysec

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/settings"
	"github.com/buxee/alix-ai/pkg/contracts"
	settingsgen "github.com/buxee/alix-ai/pkg/contracts/gen/settings"
)

// The rig of SEC-035 is the production request path of the public API with the
// least that can be wired around it:
//
//	httptest request -> gateway.Server -> identity.Middleware(verifier)
//	  -> the settings route area of the gateway -> recordingSettings -> the real
//	     settings.Service -> the real store (settings_documents) and audit log
//
// The settings area is the one route area whose only requirement is its own
// service (gateway.Need), so nothing else has to be provided: the gateway
// refuses a half-wired area, and an area with no service mounts nothing. The
// recording decorator sees the exact Scope the handler passes to the service,
// and the store holds what the service created.

// settingsCall is one call that reached the settings service.
type settingsCall struct {
	method string
	scope  contracts.Scope
	level  settings.Level
}

// recordingSettings records the scope of every call and delegates to the real
// service.
type recordingSettings struct {
	inner gateway.SettingsService

	mu    sync.Mutex
	calls []settingsCall
}

func (r *recordingSettings) record(method string, scope contracts.Scope, level settings.Level) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, settingsCall{method: method, scope: scope.Clone(), level: level})
}

func (r *recordingSettings) take() []settingsCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

func (r *recordingSettings) Get(ctx context.Context, scope contracts.Scope, level settings.Level) (settings.Record, error) {
	r.record("Get", scope, level)
	return r.inner.Get(ctx, scope, level)
}

func (r *recordingSettings) Put(ctx context.Context, scope contracts.Scope, level settings.Level, req settings.PutRequest) (settings.Record, error) {
	r.record("Put", scope, level)
	return r.inner.Put(ctx, scope, level, req)
}

func (r *recordingSettings) Effective(ctx context.Context, scope contracts.Scope) (settingsgen.EffectiveSettings, error) {
	r.record("Effective", scope, "")
	return r.inner.Effective(ctx, scope)
}

func (r *recordingSettings) Profiles(ctx context.Context, scope contracts.Scope) (settings.ProfileList, error) {
	r.record("Profiles", scope, "")
	return r.inner.Profiles(ctx, scope)
}

func (r *recordingSettings) Capabilities(ctx context.Context, scope contracts.Scope) (settings.CapabilityView, error) {
	r.record("Capabilities", scope, "")
	return r.inner.Capabilities(ctx, scope)
}

func (r *recordingSettings) IntegrationAudit(ctx context.Context, scope contracts.Scope) (settings.AuditStatus, error) {
	r.record("IntegrationAudit", scope, "")
	return r.inner.IntegrationAudit(ctx, scope)
}

func (r *recordingSettings) ProviderHealth(ctx context.Context, scope contracts.Scope) (settings.ProviderHealth, error) {
	r.record("ProviderHealth", scope, "")
	return r.inner.ProviderHealth(ctx, scope)
}

// activePackage is the in-memory application package the settings service finds
// for the demo application: a policy package that also states its languages.
type activePackage struct {
	*policy.Package
}

func (activePackage) SupportedLanguages() []string { return []string{"en", "it"} }

type packageSource struct{ pkg settings.Package }

func (p packageSource) Package(_ context.Context, applicationID string) (settings.Package, error) {
	if p.pkg.ApplicationID() != applicationID {
		return nil, contracts.New(contracts.ErrQuarantined, "the application has no active package")
	}
	return p.pkg, nil
}

// rig is one gateway server in front of one recording settings service. Several
// rigs share one environment (clock, store, issuer key, verifier), so the same
// token verifies on each.
type rig struct {
	env     *identitytest.Env
	handler http.Handler
	rec     *recordingSettings
}

// newEnv builds the shared environment and the real settings service over it.
func newEnv(t *testing.T) (*identitytest.Env, *settings.Service) {
	t.Helper()
	env := identitytest.NewEnv(t)
	alog, err := audit.New(env.DB, audit.Options{Clock: env.Clock})
	if err != nil {
		t.Fatal(err)
	}
	sink := audit.NewRecordSink[policy.Record](alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	gw := policy.NewGateway(env.Clock, policy.WithSink(sink))
	pp, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: identitytest.DefaultApplicationID, Version: "1.0.0", Status: policy.StatusActive,
		Roles: []string{"member"},
		Capabilities: []policy.CapabilityDecl{
			{ID: "items.get", Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead,
				Confirmation: policy.ConfirmNone, Fields: []policy.FieldDecl{{Name: "item_id", Required: true}}},
		},
	})
	if err != nil {
		t.Fatalf("policy.NewPackage: %v", err)
	}
	svc, err := settings.New(settings.Deps{
		DB: env.DB, Clock: env.Clock, Audit: alog, Policy: gw,
		Packages: packageSource{pkg: activePackage{pp}},
		Limits:   settings.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	return env, svc
}

// newRig builds a gateway server over the settings service. auth is the /v1
// authentication middleware the server mounts: identity.Middleware(verifier)
// for the production path, or a deliberately leaky wrapper for the controls.
func newRig(t *testing.T, env *identitytest.Env, svc gateway.SettingsService, auth func(http.Handler) http.Handler) *rig {
	t.Helper()
	rec := &recordingSettings{inner: svc}
	services := gateway.NewServices()
	if err := gateway.Provide[gateway.SettingsService](services, rec); err != nil {
		t.Fatal(err)
	}
	srv, err := gateway.NewServer(gateway.Options{Clock: env.Clock, Services: services, V1Auth: auth})
	if err != nil {
		t.Fatalf("gateway.NewServer: %v", err)
	}
	if un := srv.Unmounted(); len(un) == 0 {
		t.Fatal("expected every area except the settings area to be unmounted; the rig is wired wider than intended")
	}
	if got := srv.AreaRoutes("v1_settings"); len(got) == 0 {
		t.Fatal("the settings area mounted no route")
	}
	return &rig{env: env, handler: srv.Handler(), rec: rec}
}

// caseSeq numbers the cases of the whole package, so no two rigs share a principal.
var caseSeq atomic.Int64

// sub returns a principal id unique to one case of the table, so a PUT with
// expected_version 0 always creates its own document.
func (r *rig) sub() string { return fmt.Sprintf("usr_case_%03d", caseSeq.Add(1)) }

// userScopes is what a token needs to read and write user settings.
func userScopes() []string {
	return []string{settings.CapabilityUserRead, settings.CapabilityUserWrite}
}

// token mints a fresh single-use token for principal sub with the settings
// scopes; mods bend it further.
func (r *rig) token(sub string, mods ...func(*identitytest.Draft)) string {
	all := append([]func(*identitytest.Draft){
		func(d *identitytest.Draft) {
			d.Set(identity.ClaimSubject, sub)
			d.Set(identity.ClaimCapabilityScopes, userScopes())
		},
	}, mods...)
	return r.env.Issuer.Token(all...)
}

// request is one HTTP request to the rig.
type request struct {
	method  string
	target  string
	body    string
	headers map[string]string
}

// do sends req with token and returns the response.
func (r *rig) do(t *testing.T, token string, req request) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if req.body != "" {
		body = strings.NewReader(req.body)
	}
	hr := httptest.NewRequest(req.method, req.target, body)
	if req.body != "" {
		hr.Header.Set("Content-Type", "application/json")
	}
	for k, v := range req.headers {
		hr.Header.Set(k, v)
	}
	if token != "" {
		hr.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, hr)
	return rec
}

// documentRow is one row of settings_documents.
type documentRow struct {
	application, tenant, workspace, level, subject string
}

// rows returns every settings document in the store.
func (r *rig) rows(t *testing.T) []documentRow {
	t.Helper()
	q, err := r.env.DB.QueryContext(testCtx(t),
		`SELECT application_id, tenant_id, workspace_id, level, subject_id FROM settings_documents ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	var out []documentRow
	for q.Next() {
		var d documentRow
		if err := q.Scan(&d.application, &d.tenant, &d.workspace, &d.level, &d.subject); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	if err := q.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// leakyAuth is the planted violation of the controls: the production identity
// middleware followed by a layer that lets the request override the tenant of
// the verified scope, from a header or from the query string. It is what a
// careless "convenience" in a gateway would look like.
func leakyAuth(auth func(http.Handler) http.Handler, tenantFrom func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sc, err := identity.RequireScope(r.Context())
			if err == nil {
				if v := tenantFrom(r); v != "" {
					sc.TenantID = v
					if ctx, werr := identity.WithScope(r.Context(), sc); werr == nil {
						r = r.WithContext(ctx)
					}
				}
			}
			next.ServeHTTP(w, r)
		}))
	}
}
