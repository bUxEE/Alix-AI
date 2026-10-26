package posturesec

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tests/security/secutil"
	"github.com/buxee/alix-ai/tools/devkeys"
)

const (
	libraryApp = "demo-library"
	otherApp   = "demo-books"
	// validMinimalDir is the committed, signed fixture package of the application demo-library.
	validMinimalDir = "examples/packages/valid-minimal"
)

var capsForTraffic = []string{"catalog.find_title", "reservations.create"}

// ---- stand-ins for the services this suite does not exercise -----------------
//
// The gateway mounts an area only when every service it needs is provided, and fails
// the build of a server whose area is half wired. The areas whose services need a
// model, a RAG index, a voice runtime or an event intake are given recording stand-ins:
// the routes they mount are exactly the real ones (a route depends on which services
// exist, not on what they do), and a request that reaches one is counted.

type recorder struct {
	mu    sync.Mutex
	calls map[string]int
}

func (r *recorder) hit(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[name]++
}

func (r *recorder) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[name]
}

func unavailable(what string) error {
	return contracts.New(contracts.ErrUnavailable, "this suite runs no "+what)
}

type ragSources struct{ rec *recorder }

func (s ragSources) Register(context.Context, contracts.Scope, gateway.RAGSourcesRegistration) (gateway.RAGSourcesRecord, error) {
	s.rec.hit("rag.register")
	return gateway.RAGSourcesRecord{}, unavailable("RAG")
}
func (s ragSources) Reaudit(context.Context, contracts.Scope) (gateway.RAGSourcesReaudit, error) {
	s.rec.hit("rag.reaudit")
	return gateway.RAGSourcesReaudit{}, unavailable("RAG")
}
func (s ragSources) Remove(context.Context, contracts.Scope, string) error {
	s.rec.hit("rag.remove")
	return unavailable("RAG")
}
func (s ragSources) Status(context.Context, contracts.Scope, string) ([]gateway.RAGSourcesRecord, error) {
	s.rec.hit("rag.status")
	return nil, unavailable("RAG")
}

type eventsIntakes struct{}

func (eventsIntakes) EventsHandler(context.Context, string) (http.Handler, error) {
	return nil, unavailable("event intake")
}

type noAdapters struct{}

func (noAdapters) Adapter(context.Context, string) (adapters.AppAdapter, error) {
	return nil, unavailable("application adapter")
}

// panicking stand-ins: an embedded nil interface mounts the routes and panics when called,
// which the gateway turns into a 500 envelope.
type settingsStandIn struct{ gateway.SettingsService }
type opsStandIn struct{ gateway.OpsService }
type voiceStandIn struct{ gateway.VoiceService }
type ragQueriesStandIn struct{ gateway.RAGQueries }
type ragIndexingStandIn struct{ gateway.RAGIndexing }

// ---- the server --------------------------------------------------------------

// rig is the gateway as `alixai serve` composes it, minus the model: the real store,
// audit log, trust store, package installer, application service, capability registry,
// policy gateway, action coordinator, session store, dialogue engine, files, history,
// stream hub and AG-UI runs behind gateway.NewServer with the identity middleware,
// every route area mounted.
type rig struct {
	t       *testing.T
	clk     *clock.Fake
	db      *store.DB
	srv     *gateway.Server
	apps    *gateway.Applications
	rec     *recorder
	dataDir string
	issuers map[string]*identitytest.Issuer
}

type rigOptions struct {
	dev   gateway.DevConfig
	extra map[string]gateway.AreaFunc
	// uiTools is the host's allow-list of UI tools per application, when it registered any.
	uiTools map[string][]string
}

func newRig(t *testing.T, opts ...func(*rigOptions)) *rig {
	t.Helper()
	var o rigOptions
	for _, f := range opts {
		f(&o)
	}
	r := &rig{t: t, clk: clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)), rec: &recorder{}, issuers: map[string]*identitytest.Issuer{}}
	r.dataDir = t.TempDir()
	r.db = storetest.Open(t, nil)
	ctx := context.Background()

	ts := trust.NewStore(r.db, r.clk, trust.AllowTestOnlyKeys())
	for _, app := range []string{libraryApp, otherApp} {
		if _, err := ts.RegisterKey(ctx, app, devkeys.KeyID, devkeys.PublicKey(), "security suite"); err != nil {
			t.Fatal(err)
		}
	}
	verifier := trust.NewVerifier(ts, r.clk)
	alog, err := audit.New(r.db, audit.Options{Clock: r.clk})
	if err != nil {
		t.Fatal(err)
	}
	gw := policy.NewGateway(r.clk, policy.WithSink(audit.NewRecordSink[policy.Record](alog, audit.EventPolicyDecision, func(rec policy.Record) (audit.Locator, error) {
		s := rec.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})))
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{DB: r.db, Clock: r.clk, DataDir: r.dataDir, Verifier: verifier, CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatal(err)
	}
	if r.apps, err = gateway.NewApplications(inst, apppkg.Options{Verifier: verifier, CoreVersion: apppkg.DevCoreVersion}, r.clk, 0); err != nil {
		t.Fatal(err)
	}
	registry := capabilities.NewRegistry(r.apps.Source(), capabilities.WithClock(r.clk))
	co, err := actions.New(actions.Deps{
		DB: r.db, Policy: gw, Audit: alog, Clock: r.clk,
		Packages: actions.PackageSourceFunc(r.apps.ActivePackage), Adapters: noAdapters{},
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.New(r.db, r.clk, sessions.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sel, err := dialogue.LocalSelector(pfake.New(), providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := dialogue.New(dialogue.Deps{
		Sessions: sess, Registry: registry, Packages: r.apps, Policy: gw, Actions: co, Adapters: noAdapters{},
		Providers: sel, Audit: alog, Clock: r.clk, Decision: dialogue.NoopDecisionStage{}, Guard: dialogue.LocalOnlyGuard{},
		UITools: uiRegistry(o.uiTools),
	})
	if err != nil {
		t.Fatal(err)
	}
	filesSvc, err := files.New(files.Options{DB: r.db, Clock: r.clk, DataDir: r.dataDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = filesSvc.Close() })
	historySvc, err := history.New(history.Deps{DB: r.db, Transcripts: sess, Audit: alog, Clock: r.clk})
	if err != nil {
		t.Fatal(err)
	}

	services := gateway.NewServices()
	for _, perr := range []error{
		gateway.Provide[gateway.FilesService](services, filesSvc),
		gateway.Provide[gateway.AttachModelFiles](services, filesSvc),
		gateway.Provide(services, historySvc),
		gateway.Provide(services, sess),
		gateway.Provide(services, engine),
		gateway.Provide[gateway.ActionService](services, co),
		gateway.Provide[gateway.ApplicationService](services, r.apps),
		gateway.Provide[gateway.RAGSources](services, ragSources{rec: r.rec}),
		gateway.Provide[gateway.RAGQueries](services, ragQueriesStandIn{}),
		gateway.Provide[gateway.RAGIndexing](services, ragIndexingStandIn{}),
		gateway.Provide[gateway.EventsIntakes](services, eventsIntakes{}),
		gateway.Provide(services, gateway.NewEventsFeed(gateway.EventsFeedConfig{Clock: r.clk})),
		gateway.Provide(services, gw),
		gateway.Provide[actions.AdapterSource](services, noAdapters{}),
		gateway.Provide[audit.Appender](services, alog),
		gateway.Provide(services, stream.NewHub(stream.Config{Clock: r.clk})),
		gateway.Provide[gateway.SettingsService](services, settingsStandIn{}),
		gateway.Provide[gateway.OpsService](services, opsStandIn{}),
		gateway.Provide[gateway.VoiceService](services, voiceStandIn{}),
		gateway.Provide(services, gateway.NewAGUIRuns(r.clk)),
	} {
		if perr != nil {
			t.Fatalf("provide a service: %v", perr)
		}
	}

	if ui := uiRegistry(o.uiTools); ui != nil {
		if perr := gateway.Provide[dialogue.UIToolRegistry](services, ui); perr != nil {
			t.Fatal(perr)
		}
	}

	keys := identity.NewKeyRegistry(nil)
	for _, app := range []string{libraryApp, otherApp} {
		is := identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: app, Clock: r.clk})
		if err := keys.Register(is.Key()); err != nil {
			t.Fatal(err)
		}
		r.issuers[app] = is
	}
	v, err := identity.NewVerifier(identity.Options{
		DeploymentID: identitytest.DefaultDeployment, Keys: keys, Replay: identity.NewSQLiteReplayCache(r.db, r.clk, nil), Clock: r.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := providers.NewRegistry(r.clk)
	r.srv, err = gateway.NewServer(gateway.Options{
		Clock: r.clk, Providers: reg, V1Auth: identity.Middleware(v), Services: services, Dev: o.dev, ExtraAreas: o.extra,
	})
	if err != nil {
		t.Fatalf("gateway.NewServer: %v", err)
	}
	return r
}

// requireEveryAreaMounted fails when a registered area mounted nothing (its services
// were not provided, so its routes would silently be missing from every inventory).
func (r *rig) requireEveryAreaMounted() {
	r.t.Helper()
	if un := r.srv.Unmounted(); len(un) != 0 {
		r.t.Fatalf("route areas left unmounted: %v", un)
	}
	for _, name := range gateway.Areas() {
		if name == "dev" {
			continue // mounts one route only when dev.spike_endpoint is on
		}
		if len(r.srv.AreaRoutes(name)) == 0 {
			r.t.Fatalf("the route area %s mounted nothing", name)
		}
	}
}

// token mints a fresh single-use context token for an application, with the capability
// scopes given.
func (r *rig) token(app string, scopes ...string) string {
	r.t.Helper()
	is := r.issuers[app]
	if is == nil {
		r.t.Fatalf("no issuer for %s", app)
	}
	return is.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimCapabilityScopes, scopes) })
}

// do sends one request through the server's full handler.
func (r *rig) do(token, method, path, contentType string, body []byte, headers ...string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	r.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// packArchive packs the committed valid-minimal package, signature included, the way
// `tar czf x.tar.gz -C dir .` does.
func packArchive(t testing.TB) []byte {
	t.Helper()
	root := filepath.Join(secutil.RepoRoot(t), filepath.FromSlash(validMinimalDir))
	var names []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		names = append(names, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(n)))
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: "./" + n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// drain reads and discards a body.
func drain(rc io.ReadCloser) { _, _ = io.Copy(io.Discard, rc); _ = rc.Close() }

// param is the value a path parameter takes in a probe: the session of the test tokens for
// the session and thread, anything else otherwise.
func param(name string) string {
	switch name {
	case "session_id", "thread_id":
		return identitytest.DefaultSession
	}
	return "x_1"
}

// substitute fills the path parameters of a route with a plausible value.
func substitute(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] != '{' {
			b.WriteByte(path[i])
			continue
		}
		j := strings.IndexByte(path[i:], '}')
		if j < 0 {
			b.WriteString(path[i:])
			break
		}
		b.WriteString(param(path[i+1 : i+j]))
		i += j
	}
	return b.String()
}

// uiRegistry is the host's allow-list of UI tools, or nil when it registered none.
func uiRegistry(byApplication map[string][]string) dialogue.UIToolRegistry {
	if len(byApplication) == 0 {
		return nil
	}
	return dialogue.StaticUITools(byApplication)
}

// registerAndActivate registers the package of demo-library through the API and gives it
// the state the integration audit gives a package it has cleared.
func (r *rig) registerAndActivate() {
	r.t.Helper()
	rec := r.do(r.token(libraryApp, gateway.CapabilityRegisterApplication), http.MethodPost, "/v1/applications", "application/gzip", packArchive(r.t))
	if rec.Code != http.StatusCreated {
		r.t.Fatalf("registration = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := r.db.ExecContext(context.Background(),
		`UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rep_sec_posture' WHERE application_id = ? AND status = 'quarantined'`, libraryApp); err != nil {
		r.t.Fatal(err)
	}
}
