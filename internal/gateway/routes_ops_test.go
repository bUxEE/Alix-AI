package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/diag"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The tests of the operations area (M5-05) run the real diag.Service and the real
// audit log behind the real handlers and the real identity middleware. Only the
// two read-outs that need other modules (the job queue, the provider registry)
// are scripted.

// opsTestQueue answers with counts that are visibly the caller's own and records
// the scopes it was asked for.
type opsTestQueue struct {
	mu     sync.Mutex
	scopes []contracts.Scope
}

func opsTestStatus(pending int) jobs.Status {
	st := jobs.Status{At: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	for _, c := range jobs.Classes() {
		st.Classes = append(st.Classes, jobs.ClassStatus{Class: c, Name: c.String(), Workers: 2, MaxDepth: 50, Pending: pending})
	}
	return st
}

func (q *opsTestQueue) Status(context.Context) (jobs.Status, error) {
	return opsTestStatus(9999), nil // the deployment-wide figure: a /v1 caller must never see it
}

func (q *opsTestQueue) ScopedStatus(_ context.Context, sc contracts.Scope) (jobs.Status, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.scopes = append(q.scopes, sc)
	return opsTestStatus(len(sc.TenantID)), nil
}

type opsTestProviders struct{}

func (opsTestProviders) Snapshot() []providers.RoleStatus {
	return []providers.RoleStatus{
		{Role: providers.RoleController, Configured: true, Provider: "fake", Status: providers.HealthOK, Version: "1.0"},
		{Role: providers.RoleEmbedder, Configured: true, Provider: "fake", Status: providers.HealthDegraded, Reason: "slow; api_key=opsleakedvalue", Effect: providers.EffectReducedSemanticRecall, Behavior: "FTS only"},
	}
}
func (opsTestProviders) Overall() providers.Health { return providers.HealthDegraded }

var opsTestQueues sync.Map // *apiEnv -> *opsTestQueue

func init() {
	registerTestServices("v1_ops", func(t testing.TB, e *apiEnv, s *Services) {
		q := &opsTestQueue{}
		opsTestQueues.Store(e, q)
		svc, err := diag.New(diag.Sources{Clock: e.clk, Queue: q, Providers: opsTestProviders{}, Audit: e.alog})
		if err != nil {
			t.Fatal(err)
		}
		if err := Provide[OpsService](s, svc); err != nil {
			t.Fatal(err)
		}
	})
}

type opsRoute struct {
	method, path, capability string
}

var opsRoutes = []opsRoute{
	{http.MethodGet, "/v1/ops/queue", diag.CapabilityQueueRead},
	{http.MethodGet, "/v1/ops/providers", diag.CapabilityProvidersRead},
	{http.MethodPost, "/v1/ops/audit/export", audit.CapabilityExport},
}

// Every operation needs a verified token and the exact capability, and a token
// that has it gets an answer.
func TestOpsRoutesNeedATokenAndTheExactCapability(t *testing.T) {
	e := newAPIEnv(t)
	for _, rt := range opsRoutes {
		name := rt.method + " " + rt.path
		if rec := e.callAs("", rt.method, rt.path, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d, want 401", name, rec.Code)
		}
		// The default token has the application's own capabilities and no ops one.
		for _, caps := range [][]string{defaultCaps, {"OPS.QUEUE.READ"}, {"ops.queue.write"}, otherOpsCaps(rt.capability)} {
			rec := e.call(rt.method, rt.path, nil, withScopes(caps...))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s with %v: %d, want 403 (%s)", name, caps, rec.Code, rec.Body.String())
				continue
			}
			if env := mustEnvelope(t, rec); env.Code != string(contracts.ErrPermissionDenied) {
				t.Errorf("%s with %v: code %q, want permission_denied", name, caps, env.Code)
			}
		}
		rec := e.call(rt.method, rt.path, nil, withScopes(rt.capability))
		if rec.Code != http.StatusOK {
			t.Errorf("%s with %s: %d, want 200 (%s)", name, rt.capability, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q", name, rec.Header().Get("Cache-Control"))
		}
		if rec := e.call(otherMethod(rt.method), rt.path, nil, withScopes(rt.capability)); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s with the wrong method: %d, want 405", name, rec.Code)
		}
	}
}

func otherOpsCaps(own string) []string {
	var out []string
	for _, rt := range opsRoutes {
		if rt.capability != own {
			out = append(out, rt.capability)
		}
	}
	return out
}

func otherMethod(m string) string {
	if m == http.MethodGet {
		return http.MethodPut
	}
	return http.MethodGet
}

// The queue a host reads is its own: the scope of the token reaches the queue,
// and the deployment-wide figure never appears.
func TestOpsQueueIsTheCallersOwnScope(t *testing.T) {
	e := newAPIEnv(t)
	v, _ := opsTestQueues.Load(e)
	q := v.(*opsTestQueue)

	rec := e.call(http.MethodGet, "/v1/ops/queue", nil, withScopes(diag.CapabilityQueueRead), withTenant("tenant-xyz"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	rep := decodeBody[diag.QueueReport](t, rec)
	if len(rep.Classes) != 6 || rep.Classes[0].Class != "auth_commit" || rep.Classes[5].Class != "maintenance" {
		t.Fatalf("classes = %+v", rep.Classes)
	}
	if want := len("tenant-xyz"); rep.Classes[0].Pending != want {
		t.Errorf("pending = %d, want the caller's scoped figure %d", rep.Classes[0].Pending, want)
	}
	if strings.Contains(rec.Body.String(), "9999") || strings.Contains(rec.Body.String(), "tenant-xyz") {
		t.Errorf("the response carries a deployment-wide figure or the scope: %s", rec.Body.String())
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.scopes) != 1 || q.scopes[0].TenantID != "tenant-xyz" || q.scopes[0].ApplicationID != e.appID {
		t.Errorf("the queue was read for %+v", q.scopes)
	}
}

func TestOpsProvidersReportsHealthAndRedactsReasons(t *testing.T) {
	e := newAPIEnv(t)
	rec := e.call(http.MethodGet, "/v1/ops/providers", nil, withScopes(diag.CapabilityProvidersRead))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	rep := decodeBody[diag.ProviderReport](t, rec)
	if rep.Status != "degraded" || len(rep.Roles) != 2 || rep.Roles[1].Effect != providers.EffectReducedSemanticRecall {
		t.Errorf("report = %+v", rep)
	}
	if strings.Contains(rec.Body.String(), "opsleakedvalue") {
		t.Errorf("a provider reason leaked a secret: %s", rec.Body.String())
	}
}

// opsExportLines splits an export into its JSON lines.
func opsExportLines(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(rec.Body.Bytes()))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("export line is not JSON: %v: %q", err, sc.Text())
		}
		out = append(out, m)
	}
	return out
}

// The export is the caller's own events, whole, with the trailer that says so,
// and the export is itself in the log.
func TestOpsAuditExportStreamsOwnEventsWithATrailer(t *testing.T) {
	e := newAPIEnv(t)
	rec := e.call(http.MethodPost, "/v1/ops/audit/export", nil, withScopes(audit.CapabilityExport))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("export: %d %q: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	lines := opsExportLines(t, rec)
	if len(lines) < 2 {
		t.Fatalf("export has %d lines, want the export's own event and the trailer", len(lines))
	}
	trailer := lines[len(lines)-1]
	if trailer["complete"] != true || int(trailer["events"].(float64)) != len(lines)-1 {
		t.Errorf("trailer = %v for %d events", trailer, len(lines)-1)
	}
	found := false
	for _, l := range lines[:len(lines)-1] {
		if l["event_type"] == string(audit.EventAuditExported) || l["type"] == string(audit.EventAuditExported) {
			found = true
		}
	}
	if !found {
		t.Errorf("the export of the audit log is not in the audit log: %v", lines)
	}

	// A narrower request: only the export events, inside a time window that ends in the future.
	body := map[string]any{"types": []string{string(audit.EventAuditExported)}, "since": "2000-01-01T00:00:00Z", "until": "2100-01-01T00:00:00Z"}
	rec = e.call(http.MethodPost, "/v1/ops/audit/export", body, withScopes(audit.CapabilityExport))
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered export: %d %s", rec.Code, rec.Body.String())
	}
	lines = opsExportLines(t, rec)
	if len(lines) < 2 {
		t.Fatalf("filtered export has %d lines", len(lines))
	}
	for _, l := range lines[:len(lines)-1] {
		if l["event_type"] != string(audit.EventAuditExported) && l["type"] != string(audit.EventAuditExported) {
			t.Errorf("a filtered export returned %v", l)
		}
	}

	// Another tenant exports its own, not this one's: the first tenant's events are not in it.
	rec = e.call(http.MethodPost, "/v1/ops/audit/export", nil, withScopes(audit.CapabilityExport), withTenant("other-tenant"))
	if rec.Code != http.StatusOK {
		t.Fatalf("other tenant: %d %s", rec.Code, rec.Body.String())
	}
	for _, l := range opsExportLines(t, rec) {
		if tid, ok := l["tenant_id"].(string); ok && tid != "other-tenant" {
			t.Errorf("an export crossed tenants: %v", l)
		}
	}
}

// A bad request is an ordinary error response: nothing is streamed.
func TestOpsAuditExportRefusesBadRequests(t *testing.T) {
	e := newAPIEnv(t)
	manyTypes := make([]string, opsMaxExportTypes+1)
	for i := range manyTypes {
		manyTypes[i] = "action.committed"
	}
	for _, c := range []struct {
		name   string
		body   any
		status int
	}{
		{"a malformed event type", map[string]any{"types": []string{"Not Valid!"}}, http.StatusBadRequest},
		{"until before since", map[string]any{"since": "2026-02-01T00:00:00Z", "until": "2026-01-01T00:00:00Z"}, http.StatusBadRequest},
		{"too many types", map[string]any{"types": manyTypes}, http.StatusBadRequest},
		{"an unknown member", map[string]any{"scope": "everything"}, http.StatusBadRequest},
		{"a malformed time", map[string]any{"since": "yesterday"}, http.StatusBadRequest},
	} {
		rec := e.call(http.MethodPost, "/v1/ops/audit/export", c.body, withScopes(audit.CapabilityExport))
		if rec.Code != c.status {
			t.Errorf("%s: %d, want %d (%s)", c.name, rec.Code, c.status, rec.Body.String())
			continue
		}
		if env := mustEnvelope(t, rec); env.Code != string(contracts.ErrValidationFailed) {
			t.Errorf("%s: code %q", c.name, env.Code)
		}
	}
	rec := e.callRaw(e.token(withScopes(audit.CapabilityExport)), http.MethodPost, "/v1/ops/audit/export", "text/plain", []byte("types"))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("a text body: %d, want 415", rec.Code)
	}
}

// While no operations service is provided the routes say so (503 unavailable);
// an empty success would pass for a quiet system. The scope is checked first.
func TestOpsRoutesAnswerUnavailableWithoutTheService(t *testing.T) {
	e := newAPIEnv(t)
	h := &opsHandler{apps: e.apps}
	scope := e.scopeOf()
	scope.CapabilityScopes = []string{diag.CapabilityQueueRead, diag.CapabilityProvidersRead, audit.CapabilityExport}
	ctx, err := identity.WithScope(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]http.HandlerFunc{"queue": h.queue, "providers": h.providers, "export": h.auditExport} {
		rec := httptest.NewRecorder()
		fn(rec, httptest.NewRequest(http.MethodGet, "/v1/ops/x", nil).WithContext(ctx))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d, want 503", name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `"classes"`) || strings.Contains(rec.Body.String(), `"roles"`) {
			t.Errorf("%s answered with data: %s", name, rec.Body.String())
		}
	}
	// No verified scope at all: refused before the service is considered.
	rec := httptest.NewRecorder()
	h.queue(rec, httptest.NewRequest(http.MethodGet, "/v1/ops/queue", nil))
	if rec.Code == http.StatusOK || rec.Code == http.StatusServiceUnavailable {
		t.Errorf("a request with no scope got %d", rec.Code)
	}
}
