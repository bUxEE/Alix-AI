package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ---- fakes ----

type diagFakeQueue struct {
	mu        sync.Mutex
	global    jobs.Status
	scoped    jobs.Status
	err       error
	gotScopes []contracts.Scope
}

func diagStatus(n int) jobs.Status {
	st := jobs.Status{At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	for _, c := range jobs.Classes() {
		st.Classes = append(st.Classes, jobs.ClassStatus{Class: c, Name: c.String(), Workers: 2, MaxDepth: 100, Pending: n, Running: 1})
	}
	st.Classes[0].OldestReadyAgeMs = 1500
	return st
}

func (q *diagFakeQueue) Status(context.Context) (jobs.Status, error) { return q.global, q.err }
func (q *diagFakeQueue) ScopedStatus(_ context.Context, sc contracts.Scope) (jobs.Status, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.gotScopes = append(q.gotScopes, sc)
	return q.scoped, q.err
}

type diagFakeProviders struct{ roles []providers.RoleStatus }

func (p diagFakeProviders) Snapshot() []providers.RoleStatus { return p.roles }
func (p diagFakeProviders) Overall() providers.Health {
	for _, r := range p.roles {
		if r.Configured && r.Status != providers.HealthOK {
			return providers.HealthDegraded
		}
	}
	return providers.HealthOK
}

type diagFakeAudit struct {
	called int
	err    error
}

func (a *diagFakeAudit) Export(_ context.Context, _ contracts.Scope, w io.Writer, _ audit.ExportOptions) (audit.ExportResult, error) {
	a.called++
	if a.err != nil {
		return audit.ExportResult{}, a.err
	}
	_, _ = io.WriteString(w, `{"id":"aud_1"}`+"\n")
	return audit.ExportResult{Events: 1}, nil
}

func diagScope(caps ...string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep", ApplicationID: "app-a", TenantID: "ten-1", WorkspaceID: "ws-1", PrincipalID: "pr-1",
		Roles: []string{"user"}, CapabilityScopes: caps, SessionID: "ses-1",
	}
}

var diagCtx = context.Background()

// ---- the bundle is redacted ----

// SEC-083: Master plan 16.2: "Logs redact tokens, secrets and configured sensitive
// fields", and the diagnostics bundle is what an operator posts to an issue
// tracker. A secret under a sensitive key, a secret embedded in free text (a
// bearer token, a JWT, URL userinfo, a PEM key, key=value), a secret in the
// recent errors, in a check's message, in a provider's reason and in a failing
// source's error never reaches the bundle; ordinary values do.
func TestDiagnosticsRedacted(t *testing.T) {
	secrets := []string{
		"sk-live-ABC123SECRET", "hunter2pass", "cs-SECRETVALUE", "p@ssw0rd-LITERAL", "CUSTOM-SENSITIVE-42",
		"topsecretbearer123", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig12345", "pa55word", "MIIEvQIBADANBgkqhkiG9w0B",
		"abc123reason", "xyzqueuesecret", "dbcheckpass", "sk-123-in-group", "cookie-value-77", "b64-private-material",
	}
	ring := NewErrorRing(10, nil)
	logger := slog.New(ring.Wrap(slog.NewJSONHandler(io.Discard, nil)))
	logger.Error("provider call failed",
		"error", errors.New("POST https://user:pa55word@api.example/v1 failed: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig12345 rejected"),
		slog.Group("req", slog.String("api_key", "sk-123-in-group"), slog.String("path", "/v1/turns")),
		"cookie", "cookie-value-77",
		"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig12345",
		"tok_in", 12,
	)
	logger.With("private_key", "b64-private-material").Warn("key rotated", "attempt", 3)

	q := &diagFakeQueue{global: diagStatus(2), err: nil}
	svc, err := New(Sources{
		Clock: clock.NewFake(time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)),
		Queue: q,
		Providers: diagFakeProviders{roles: []providers.RoleStatus{
			{Role: providers.RoleController, Configured: true, Provider: "localai", Status: providers.HealthDegraded, Reason: "upstream said token=abc123reason", Version: "3.1"},
		}},
		Resources: func() ResourceUsage { return ResourceUsage{MemoryBudgetBytes: 4 << 30, RSSBytes: 1 << 30} },
		Config: func() map[string]any {
			return map[string]any{
				"server":   map[string]any{"listen": "127.0.0.1:8080", "max_body_bytes": 65536, "big": int64(9007199254740993)},
				"localai":  map[string]any{"endpoint": "http://127.0.0.1:8081", "api_key": "sk-live-ABC123SECRET"},
				"database": map[string]any{"dsn": "postgres://admin:hunter2pass@db.internal:5432/app"},
				"identity": map[string]any{"issuer": map[string]any{"client_secret": "cs-SECRETVALUE", "name": "demo"}},
				"ops":      map[string]any{"password": "p@ssw0rd-LITERAL"},
				"custom":   map[string]any{"internal_id": "CUSTOM-SENSITIVE-42", "label": "visible-label"},
				"list": []any{
					map[string]any{"Authorization": "Bearer abcdef.ghijkl"},
					"note: Authorization: Bearer topsecretbearer123",
				},
				"tls": map[string]any{"cert": "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0B\n-----END PRIVATE KEY-----"},
			}
		},
		Checks: []Check{
			{Name: "store", Fn: func(context.Context) error { return errors.New("dial failed: password=dbcheckpass for db") }},
			{Name: "model", Fn: func(context.Context) error { return nil }},
		},
		Errors:     ring,
		RedactKeys: []string{"internal_id"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A failing source: its error text is redacted like everything else.
	q.err = errors.New("select failed: secret=xyzqueuesecret")

	raw, err := svc.MarshalBundle(diagCtx)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("the bundle leaks %q:\n%s", s, out)
		}
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("nothing was redacted:\n%s", out)
	}

	// Positive controls: redaction is not deletion. Ordinary values, the
	// structure, the exact numbers and the failing source are all still there.
	for _, keep := range []string{
		"127.0.0.1:8080", "65536", "9007199254740993", "http://127.0.0.1:8081", "visible-label", `"name": "demo"`,
		"provider call failed", "key rotated", "/v1/turns", `"tok_in": 12`, "db.internal:5432", "localai", "3.1",
		`"format": "alixai.diagnostics/v1"`, "2026-05-06T07:08:09Z", "memory_budget_bytes", "queue: select failed",
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("the bundle lost %q:\n%s", keep, out)
		}
	}

	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("the bundle is not JSON: %v", err)
	}
	if b.Redactions < 8 {
		t.Errorf("Redactions = %d, want a count of what was replaced", b.Redactions)
	}
	if len(b.RecentErrors) != 2 || b.RecentErrors[0].Level != "ERROR" || b.RecentErrors[1].Level != "WARN" {
		t.Errorf("recent errors = %+v", b.RecentErrors)
	}
	if len(b.Health.Checks) != 2 || b.Health.Checks[0].Name != "model" || !b.Health.Checks[0].OK || b.Health.Checks[1].OK {
		t.Errorf("checks = %+v", b.Health.Checks)
	}
	if b.Health.Status != "degraded" {
		t.Errorf("health status = %q", b.Health.Status)
	}
	if b.Queue != nil {
		t.Errorf("a queue that failed is reported as data: %+v", b.Queue)
	}
}

// The ring redacts when it records, so a raw value never sits in memory waiting
// for a bundle, and it stays bounded.
func TestErrorRingIsBoundedAndRedactsOnInsert(t *testing.T) {
	ring := NewErrorRing(3, nil)
	h := ring.Wrap(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	logger := slog.New(h)
	logger.Info("not kept")
	logger.Debug("not kept either")
	for i := 0; i < 5; i++ {
		logger.Warn("w", "n", i, "password", "plain-pass")
	}
	logger.Error(strings.Repeat("x", 2000), "blob", strings.Repeat("y", 2000))
	got := ring.Recent()
	if len(got) != 3 {
		t.Fatalf("ring kept %d entries, want 3", len(got))
	}
	if got[0].Attrs["n"] != int64(3) || got[1].Attrs["n"] != int64(4) {
		t.Errorf("order or content wrong: %+v", got)
	}
	for _, e := range got {
		if v, ok := e.Attrs["password"]; ok && v != "[REDACTED]" {
			t.Errorf("a sensitive attribute was stored raw: %+v", e)
		}
	}
	last := got[2]
	if len(last.Message) > maxEntryMessage+3 || len(last.Attrs["blob"].(string)) > maxEntryValue+3 {
		t.Errorf("an entry is not bounded: message %d, blob %d", len(last.Message), len(last.Attrs["blob"].(string)))
	}
	if NewErrorRing(0, nil) == nil || len(NewErrorRing(1<<20, nil).buf) != MaxRecentErrors {
		t.Error("the ring size is not clamped")
	}
}

// A source that is broken is the very time a bundle is wanted: it reports what
// it can and names what it could not read.
func TestBundleSurvivesBrokenSources(t *testing.T) {
	svc, err := New(Sources{
		Checks: []Check{{Name: "panics", Fn: func(context.Context) error { panic("boom") }}, {Name: "nofn"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Bundle(diagCtx)
	if err != nil {
		t.Fatal(err)
	}
	if b.Health.Status != "unknown" || len(b.Problems) < 4 || len(b.Health.Checks) != 2 || b.Health.Checks[0].OK || b.Health.Checks[1].OK {
		t.Errorf("bundle of an empty service: %+v", b)
	}
	if b.Versions.Build.GoVersion == "" {
		t.Error("the bundle carries no version")
	}
}

// ---- the host-facing operations ----

// A host token reaches the queue and the provider status only with the exact
// capability, a complete scope and the deployment's own rule; the queue it reads
// is the scoped one, never the deployment-wide counts.
func TestHostOperationsNeedTheExactCapability(t *testing.T) {
	q := &diagFakeQueue{global: diagStatus(99), scoped: diagStatus(1)}
	deny := false
	svc, err := New(Sources{
		Queue: q,
		Providers: diagFakeProviders{roles: []providers.RoleStatus{
			{Role: providers.RoleController, Configured: true, Status: providers.HealthOK, Reason: "api_key=leakedvalue1"},
		}},
		Authorize: func(context.Context, contracts.Scope, string) error {
			if deny {
				return errors.New("operators only")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, caps := range [][]string{{"sessions.create"}, {"OPS.QUEUE.READ"}, {"ops.providers.read"}} {
		if _, err := svc.Queue(diagCtx, diagScope(caps...)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("Queue with %v: code %v, want permission_denied", caps, contracts.CodeOf(err))
		}
	}
	if _, err := svc.Providers(diagCtx, diagScope("ops.queue.read")); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("Providers with the queue capability: code %v, want permission_denied", contracts.CodeOf(err))
	}
	// A wildcard is not a capability: the scope itself is refused (scope_violation).
	for _, caps := range [][]string{{"ops.*"}, {"*"}} {
		if _, err := svc.Queue(diagCtx, diagScope(caps...)); err == nil {
			t.Errorf("Queue accepted the wildcard %v", caps)
		}
	}
	incomplete := diagScope(CapabilityQueueRead)
	incomplete.TenantID = ""
	if _, err := svc.Queue(diagCtx, incomplete); contracts.CodeOf(err) != contracts.ErrScopeMissing {
		t.Errorf("Queue with no tenant: code %v, want scope_missing", contracts.CodeOf(err))
	}
	if _, err := svc.Queue(diagCtx, contracts.Scope{}); err == nil {
		t.Error("Queue accepted the zero scope")
	}
	if len(q.gotScopes) != 0 {
		t.Errorf("the queue was read %d times for refused callers", len(q.gotScopes))
	}

	sc := diagScope(CapabilityQueueRead)
	rep, err := svc.Queue(diagCtx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.gotScopes) != 1 || q.gotScopes[0].TenantID != "ten-1" || q.gotScopes[0].ApplicationID != "app-a" {
		t.Errorf("the queue was read for %+v", q.gotScopes)
	}
	if len(rep.Classes) != 6 || rep.Classes[0].Pending != 1 || rep.Classes[0].Class != "auth_commit" || rep.Classes[0].Priority != 1 {
		t.Errorf("queue report = %+v", rep)
	}

	prov, err := svc.Providers(diagCtx, diagScope(CapabilityProvidersRead))
	if err != nil {
		t.Fatal(err)
	}
	if prov.Status != "ok" || len(prov.Roles) != 1 || strings.Contains(prov.Roles[0].Reason, "leakedvalue1") {
		t.Errorf("provider report = %+v", prov)
	}

	deny = true
	if _, err := svc.Queue(diagCtx, sc); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("an Authorize refusal gave code %v, want permission_denied", contracts.CodeOf(err))
	}

	// A queue that cannot be read is unavailable, with no database text in the error.
	deny = false
	q.err = errors.New("sqlite: disk I/O error at /var/lib/alixai/db")
	_, err = svc.Queue(diagCtx, sc)
	if contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("a failing queue gave code %v, want unavailable", contracts.CodeOf(err))
	}

	// Without a source the operation says so; it never answers with empty data.
	bare, _ := New(Sources{})
	if _, err := bare.Queue(diagCtx, sc); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("no queue: code %v, want unavailable", contracts.CodeOf(err))
	}
	if _, err := bare.Providers(diagCtx, diagScope(CapabilityProvidersRead)); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("no providers: code %v, want unavailable", contracts.CodeOf(err))
	}
	var sink bytes.Buffer
	if _, err := bare.AuditExport(diagCtx, sc, &sink, audit.ExportOptions{}); contracts.CodeOf(err) != contracts.ErrUnavailable || sink.Len() != 0 {
		t.Errorf("no audit log: code %v, wrote %d bytes", contracts.CodeOf(err), sink.Len())
	}
}

// The audit export is internal/audit's decision: diag passes the scope and the
// writer through and adds nothing, so the capability check and the record of the
// export stay where they are.
func TestAuditExportDelegatesToTheAuditLog(t *testing.T) {
	a := &diagFakeAudit{}
	svc, _ := New(Sources{Audit: a})
	var buf bytes.Buffer
	res, err := svc.AuditExport(diagCtx, diagScope(audit.CapabilityExport), &buf, audit.ExportOptions{})
	if err != nil || res.Events != 1 || a.called != 1 || !strings.Contains(buf.String(), "aud_1") {
		t.Fatalf("export: res=%+v err=%v called=%d out=%q", res, err, a.called, buf.String())
	}
	a.err = contracts.New(contracts.ErrPermissionDenied, "no")
	if _, err := svc.AuditExport(diagCtx, diagScope(), io.Discard, audit.ExportOptions{}); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("a refusal of the audit log changed to %v", contracts.CodeOf(err))
	}
}

// ---- the operator door ----

func diagOperatorRequest(path, remote, token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = remote
	r.Host = "127.0.0.1:9464"
	if token != "" {
		r.Header.Set(OperatorTokenHeader, token)
	}
	return r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9464}))
}

func TestOperatorHandlerNeedsLoopbackAndTheOperatorCredential(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef-operator"
	svc, err := New(Sources{
		Queue: &diagFakeQueue{global: diagStatus(4)},
		Config: func() map[string]any {
			return map[string]any{"x": map[string]any{"api_key": "operatorleak99", "ok": "fine"}}
		},
		Providers: diagFakeProviders{roles: []providers.RoleStatus{
			{Role: providers.RoleController, Configured: true, Provider: "localai", Status: providers.HealthOK, Version: "1"},
			{Role: providers.RoleRouter, Status: providers.HealthUnavailable},
		}},
		Resources: func() ResourceUsage { return ResourceUsage{RSSBytes: 123, BackgroundHeld: true} },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OperatorHandler(svc, "short"); err == nil {
		t.Error("a short operator credential was accepted")
	}
	if _, err := OperatorHandler(nil, token); err == nil {
		t.Error("a nil service was accepted")
	}
	h, err := OperatorHandler(svc, token)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name, path, remote, token string
		want                      int
	}{
		{"metrics from the machine", "/metrics", "127.0.0.1:5000", "", http.StatusOK},
		{"metrics from a LAN peer", "/metrics", "192.0.2.7:5000", "", http.StatusForbidden},
		{"diagnostics, no credential", "/diagnostics", "127.0.0.1:5000", "", http.StatusForbidden},
		{"diagnostics, a context-token-shaped credential", "/diagnostics", "127.0.0.1:5000", "eyJhbGciOiJFZERTQSJ9.e30.sig", http.StatusForbidden},
		{"diagnostics, a prefix of the credential", "/diagnostics", "127.0.0.1:5000", token[:len(token)-1], http.StatusForbidden},
		{"diagnostics, right credential, LAN peer", "/diagnostics", "192.0.2.7:5000", token, http.StatusForbidden},
		{"diagnostics, right credential, the machine", "/diagnostics", "127.0.0.1:5000", token, http.StatusOK},
		{"an unknown path", "/debug/vars", "127.0.0.1:5000", token, http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, diagOperatorRequest(c.path, c.remote, c.token))
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
		if c.want != http.StatusOK && (strings.Contains(rec.Body.String(), "operatorleak99") || strings.Contains(rec.Body.String(), "alixai_queue")) {
			t.Errorf("%s: a refused request received data", c.name)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, diagOperatorRequest("/diagnostics", "127.0.0.1:5000", token))
	if strings.Contains(rec.Body.String(), "operatorleak99") || !strings.Contains(rec.Body.String(), `"ok": "fine"`) {
		t.Errorf("the served bundle is not the redacted one:\n%s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, diagOperatorRequest("/metrics", "127.0.0.1:5000", ""))
	for _, want := range []string{
		`alixai_queue_jobs{class="auth_commit",state="pending"} 4`,
		`alixai_queue_oldest_ready_age_seconds{class="auth_commit"} 1.5`,
		`alixai_provider_status{role="controller",status="ok"} 1`,
		`alixai_provider_status{role="controller",status="degraded"} 0`,
		`alixai_provider_info{provider="localai",role="controller",version="1"} 1`,
		`alixai_provider_configured{role="router"} 0`,
		"alixai_resource_rss_bytes 123\n",
		"alixai_resource_background_held 1\n",
		`alixai_scrape_collector_up{collector="queue"} 1`,
		"alixai_build_info{",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, rec.Body.String())
		}
	}
	// No tenant, principal, session or application identifier is ever a label.
	for _, banned := range []string{"tenant", "principal", "session", "application_id", "workspace"} {
		if strings.Contains(rec.Body.String(), banned) {
			t.Errorf("the metrics mention %q", banned)
		}
	}
}
