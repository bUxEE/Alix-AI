package gateway

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	libraryApp     = "demo-library" // the application of the valid-minimal fixture package
	fixturePackage = "../../examples/packages/valid-minimal"
	fixtureTamper  = "../../examples/packages/broken-tampered"
)

// packArchive builds a .tar.gz of a package directory the way a publisher
// ships one: files at the root of the archive. skip drops files by path prefix.
func packArchive(t testing.TB, dir string, skip ...string) []byte {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, s := range skip {
			if strings.HasPrefix(rel, s) {
				return nil
			}
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// registrar is the claim that lets a token register a package.
func registrar() claims {
	return withScopes(append(append([]string{}, defaultCaps...), CapabilityRegisterApplication)...)
}

func (e *apiEnv) register(archive []byte, mods ...claims) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.callRaw(e.token(append([]claims{registrar()}, mods...)...), http.MethodPost, "/v1/applications", "application/gzip", archive)
}

// activate does what the integration audit (M1-17) will do: it moves a
// quarantined package to active with an audit report. It is the only way the
// state changes, and it is not reachable from any route.
func (e *apiEnv) activate(app, hash string) {
	e.t.Helper()
	res, err := e.db.ExecContext(context.Background(),
		`UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rep_test_01' WHERE application_id = ? AND manifest_hash = ? AND status = 'quarantined'`, app, hash)
	if err != nil {
		e.t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		e.t.Fatalf("activated %d packages", n)
	}
}

// SEC-054, SEC-060: registering an application lands in quarantine and serves
// no traffic: every session, turn, action and capability route refuses, and
// neither the model nor an adapter is reached.
func TestQuarantinedAppRejectsTraffic(t *testing.T) {
	e := newAPIEnv(t, withRealApps(libraryApp))
	archive := packArchive(t, fixturePackage)

	// Registration installs into quarantine, and says so.
	rec := e.register(archive)
	wantStatus(t, rec, http.StatusCreated)
	validateSpec(t, "Registration", rec.Body.Bytes())
	reg := decodeBody[RegistrationBody](t, rec)
	if reg.ApplicationID != libraryApp || !reg.Created || reg.Package.State != "quarantined" || len(reg.Package.ManifestHash) != 64 {
		t.Fatalf("registration = %+v", reg)
	}
	if strings.Contains(rec.Body.String(), "install_path") || strings.Contains(rec.Body.String(), "packages/") {
		t.Errorf("the registration reveals where the package is stored: %s", rec.Body.String())
	}
	// SEC-081: what registration wrote under the data directory is owner-only.
	// The data directory itself is not registration's: here the test made it
	// (t.TempDir, so its mode follows the umask), and the product never
	// tightens an existing one (serve warns about it instead).
	if runtime.GOOS != "windows" {
		written := 0
		err := filepath.WalkDir(e.dataDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == e.dataDir {
				return nil
			}
			written++
			info, err := d.Info()
			if err != nil {
				return err
			}
			want := fs.FileMode(0o600)
			if d.IsDir() {
				want = 0o700
			}
			if info.Mode().Perm() != want {
				t.Errorf("%s has mode %o, want %o", p, info.Mode().Perm(), want)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if written == 0 {
			t.Fatal("registration wrote nothing under the data directory: the mode check saw nothing")
		}
	}
	// It is recorded on the application's audit chain.
	var n int
	if err := e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM audit_events WHERE event_type = 'package.installed' AND application_id = ?`, libraryApp).Scan(&n); err != nil || n != 1 {
		t.Fatalf("package.installed events = %d, %v", n, err)
	}
	// Registering the same content again changes nothing.
	rec = e.register(archive)
	wantStatus(t, rec, http.StatusOK)
	if again := decodeBody[RegistrationBody](t, rec); again.Created || again.Package.ManifestHash != reg.Package.ManifestHash || again.Package.State != "quarantined" {
		t.Fatalf("second registration = %+v", again)
	}

	// Every route of this API that serves traffic refuses with quarantined.
	exempt := map[string]bool{"POST /v1/applications": true, "GET /v1/applications/{application_id}": true, "GET /v1/health": true}
	checked := 0
	for _, route := range e.v1AreaRoutes() {
		method, path, _ := strings.Cut(route, " ")
		if !strings.HasPrefix(path, "/v1/") || exempt[route] {
			continue
		}
		path = strings.NewReplacer("{session_id}", apiSess, "{preparation_id}", "prep_01HZZZZZZZZZZZZZZZZZZZZZZZ").Replace(path)
		var body any
		switch {
		case strings.HasSuffix(path, "/turns"):
			body = TurnRequest{Text: "hello"}
		case strings.HasSuffix(path, "/confirm"):
			body = ConfirmRequest{Token: "cfm_" + strings.Repeat("A", 43)}
		}
		rec := e.call(method, path, body, withScopes("catalog.find_title", "reservations.create"))
		env := wantError(t, rec, http.StatusForbidden, contracts.ErrQuarantined)
		if !strings.Contains(env.Message, "no active package") {
			t.Errorf("%s: message = %q", route, env.Message)
		}
		checked++
	}
	if checked < 8 {
		t.Fatalf("only %d traffic routes were checked", checked)
	}
	if e.model.Calls() != 0 || len(e.host.Calls()) != 0 || e.sessionRows() != 0 {
		t.Fatalf("a quarantined application reached the model (%d calls), an adapter (%d calls) or created sessions (%d)",
			e.model.Calls(), len(e.host.Calls()), e.sessionRows())
	}

	// It can be inspected: quarantined, its package listed, nothing called.
	rec = e.call(http.MethodGet, "/v1/applications/"+libraryApp, nil)
	wantStatus(t, rec, http.StatusOK)
	validateSpec(t, "Application", rec.Body.Bytes())
	app := decodeBody[ApplicationBody](t, rec)
	if app.Status != "quarantined" || len(app.Packages) != 1 || app.Packages[0].State != "quarantined" || app.Health != nil {
		t.Fatalf("application = %+v", app)
	}
	if len(e.host.Calls()) != 0 {
		t.Fatal("inspecting a quarantined application called the adapter")
	}
	rec = e.call(http.MethodGet, "/v1/health", nil)
	wantStatus(t, rec, http.StatusOK)
	validateSpec(t, "Health", rec.Body.Bytes())
	if h := decodeBody[HealthBody](t, rec); h.Application.Status != "quarantined" || h.Application.ApplicationID != libraryApp {
		t.Fatalf("health = %+v", h)
	}

	// Nothing a request can do activates it: activation is the integration
	// audit's, and the database refuses it without an audit report.
	if _, err := e.db.ExecContext(context.Background(), `UPDATE apppkg_packages SET status = 'active' WHERE application_id = ?`, libraryApp); err == nil {
		t.Fatal("a package became active without an audit report")
	}
	wantError(t, e.call(http.MethodPost, "/v1/sessions", nil, withScopes("catalog.find_title")), http.StatusForbidden, contracts.ErrQuarantined)

	// Once the audit has activated it, the same application serves.
	e.activate(libraryApp, reg.Package.ManifestHash)
	scopes := withScopes("catalog.find_title", "reservations.create")
	wantStatus(t, e.call(http.MethodPost, "/v1/sessions", nil, scopes), http.StatusOK)
	rec = e.call(http.MethodGet, "/v1/capabilities", nil, scopes)
	wantStatus(t, rec, http.StatusOK)
	validateSpec(t, "CapabilityList", rec.Body.Bytes())
	caps := decodeBody[CapabilitiesBody](t, rec)
	var ids []string
	for _, c := range caps.Capabilities {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "catalog.find_title,reservations.create" {
		t.Fatalf("capabilities = %v", ids)
	}
	rec = e.call(http.MethodGet, "/v1/applications/"+libraryApp, nil)
	wantStatus(t, rec, http.StatusOK)
	if app := decodeBody[ApplicationBody](t, rec); app.Status != "active" || app.Packages[0].AuditReportID != "rep_test_01" {
		t.Fatalf("application = %+v", app)
	}
}

func TestRegisterApplicationIsGuarded(t *testing.T) {
	e := newAPIEnv(t, withRealApps(libraryApp))
	archive := packArchive(t, fixturePackage)
	installed := func() int {
		recs, err := e.real.Records(context.Background(), libraryApp)
		if err != nil {
			t.Fatal(err)
		}
		return len(recs)
	}

	// The token must carry the registration capability scope.
	wantError(t, e.callRaw(e.token(), http.MethodPost, "/v1/applications", "application/gzip", archive), http.StatusForbidden, contracts.ErrPermissionDenied)
	// Only an archive: not JSON, not a path, not a URL, not an empty body.
	wantError(t, e.callRaw(e.token(registrar()), http.MethodPost, "/v1/applications", "application/json", []byte(`{"path":"/etc"}`)),
		http.StatusUnsupportedMediaType, contracts.ErrValidationFailed)
	wantError(t, e.callRaw(e.token(registrar()), http.MethodPost, "/v1/applications", "application/gzip", nil), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantError(t, e.callRaw(e.token(registrar()), http.MethodPost, "/v1/applications", "application/gzip", []byte("this is not a gzip archive")),
		http.StatusUnprocessableEntity, contracts.ErrValidationFailed)

	// An unsigned package and one changed after signing are refused with
	// their critical findings, and nothing is written.
	unsigned := packArchive(t, fixturePackage, "signatures/")
	env := wantError(t, e.register(unsigned), http.StatusUnprocessableEntity, contracts.ErrValidationFailed)
	if list, _ := env.Details["findings"].([]any); len(list) == 0 {
		t.Errorf("a refused package says nothing of why: %v", env.Details)
	}
	wantError(t, e.register(packArchive(t, fixtureTamper)), http.StatusUnprocessableEntity, contracts.ErrValidationFailed)
	if installed() != 0 {
		t.Fatalf("a refused package was installed (%d)", installed())
	}

	// A token for another application can never install this application's package.
	otherApp := identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: "app_other", Clock: e.clk})
	if err := e.keys.Register(otherApp.Key()); err != nil {
		t.Fatal(err)
	}
	tok := otherApp.Token(registrar())
	wantError(t, e.callRaw(tok, http.MethodPost, "/v1/applications", "application/gzip", archive), http.StatusForbidden, contracts.ErrScopeViolation)
	if installed() != 0 {
		t.Fatal("a package of another application was installed")
	}

	// A signature by a key the operator did not register is refused: the
	// installer trusts the trust store and nothing in the request.
	untrusted, err := NewApplications(e.real.in, apppkg.Options{Verifier: nil, CoreVersion: apppkg.DevCoreVersion}, e.clk, 0)
	if err == nil || untrusted != nil {
		t.Fatal("applications were built without a trust verifier")
	}

	// And an application never inspects another's record.
	wantError(t, e.call(http.MethodGet, "/v1/applications/app_other", nil), http.StatusForbidden, contracts.ErrScopeViolation)
	wantError(t, e.call(http.MethodGet, "/v1/applications/not%20valid", nil), http.StatusBadRequest, contracts.ErrValidationFailed)
	if e.model.Calls() != 0 || len(e.host.Calls()) != 0 {
		t.Fatal("registration reached the model or an adapter")
	}
}

func TestCapabilitiesAreWhatPolicyExposes(t *testing.T) {
	e := newAPIEnv(t)
	rec := e.call(http.MethodGet, "/v1/capabilities", nil)
	wantStatus(t, rec, http.StatusOK)
	caps := decodeBody[CapabilitiesBody](t, rec)
	if len(caps.Capabilities) != 2 || caps.Capabilities[0].ID != capCreate || caps.Capabilities[1].ID != capGet {
		t.Fatalf("capabilities = %+v", caps)
	}
	if c := caps.Capabilities[0]; c.RiskLevel != 2 || !c.RequiresConfirmation || c.RequiresStepUp || strings.Join(c.Fields, ",") != "name,quantity" {
		t.Errorf("items.create = %+v", c)
	}
	// The admin capability is never listed; a visitor sees fewer.
	if strings.Contains(rec.Body.String(), capAdmin) {
		t.Error("an admin capability is listed")
	}
	rec = e.call(http.MethodGet, "/v1/capabilities", nil, withRoles("visitor"))
	wantStatus(t, rec, http.StatusOK)
	if caps := decodeBody[CapabilitiesBody](t, rec); len(caps.Capabilities) != 1 || caps.Capabilities[0].ID != capGet {
		t.Fatalf("a visitor sees %+v", caps)
	}
	// A token that does not permit a capability does not see it.
	rec = e.call(http.MethodGet, "/v1/capabilities", nil, withScopes(capGet))
	wantStatus(t, rec, http.StatusOK)
	if caps := decodeBody[CapabilitiesBody](t, rec); len(caps.Capabilities) != 1 || caps.Capabilities[0].ID != capGet {
		t.Fatalf("a token scoped to one capability sees %+v", caps)
	}
}

func TestHealthEndpoints(t *testing.T) {
	e := newAPIEnv(t)
	wantUnauthorized(t, e.callAs("", http.MethodGet, "/v1/health", nil))
	// The unauthenticated probes match the document too.
	rec0 := e.callAs("", http.MethodGet, "/readyz", nil)
	wantStatus(t, rec0, http.StatusOK)
	validateSpec(t, "ReadyReport", rec0.Body.Bytes())
	rec := e.call(http.MethodGet, "/v1/health", nil)
	wantStatus(t, rec, http.StatusOK)
	validateSpec(t, "Health", rec.Body.Bytes())
	h := decodeBody[HealthBody](t, rec)
	if !h.Ready || h.Application.ApplicationID != apiApp || h.Application.Status != "active" {
		t.Fatalf("health = %+v", h)
	}
	// An application's own health is read for an active application only.
	rec = e.call(http.MethodGet, "/v1/applications/"+apiApp, nil)
	wantStatus(t, rec, http.StatusOK)
	validateSpec(t, "Application", rec.Body.Bytes())
	if app := decodeBody[ApplicationBody](t, rec); app.Health == nil || app.Health.Status != "ok" {
		t.Fatalf("application = %+v", app)
	}
	// An application that is down is reported as such, without the cause.
	e.host.SetAvailable(false)
	rec = e.call(http.MethodGet, "/v1/applications/"+apiApp, nil)
	wantStatus(t, rec, http.StatusOK)
	if app := decodeBody[ApplicationBody](t, rec); app.Health == nil || app.Health.Status != "unavailable" || app.Health.Reason == "" {
		t.Fatalf("application = %+v", app)
	}
	// The model runtime down makes the service not ready, and says so with 503.
	e.model.SetHealth(providers.HealthUnavailable, "the runtime is down")
	e.clk.Advance(DefaultHealthTTL + 1)
	rec = e.call(http.MethodGet, "/v1/health", nil)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("health with the runtime down = %d %s", rec.Code, rec.Body.String())
	}
}
