package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// transportsSwapDefault gives the test an empty process registry and restores
// the real one afterwards, so no test depends on which transports the binary
// registers from their own files (an init in transport_mcp.go claims "mcp").
// Tests that use it are not parallel.
func transportsSwapDefault(t testing.TB) *transportsRegistry {
	t.Helper()
	old := transportsDefault
	transportsDefault = newTransportsRegistry()
	t.Cleanup(func() { transportsDefault = old })
	return transportsDefault
}

// transportsRecorder is a factory that remembers what it was asked for.
type transportsRecorder struct {
	mu    sync.Mutex
	calls []TransportRequest
}

func (r *transportsRecorder) factory(app *fake.App) TransportFactory {
	return func(_ context.Context, req TransportRequest) (adapters.AppAdapter, error) {
		r.mu.Lock()
		r.calls = append(r.calls, req)
		r.mu.Unlock()
		return fake.NewAdapter(app), nil
	}
}

func (r *transportsRecorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		out = append(out, c.Kind)
	}
	return out
}

func transportsManifest(eps map[string]ap.ManifestEndpointValue) ap.Manifest {
	m := ap.Manifest{Endpoints: map[string]ap.ManifestEndpoint{}}
	for id, v := range eps {
		m.Endpoints[id] = ap.ManifestEndpoint{Value: v}
	}
	return m
}

func transportsEndpointOf(kind string) ap.ManifestEndpointValue {
	switch kind {
	case TransportKindMCP:
		return ap.ManifestMCPEndpoint{Transport: "mcp"}
	case TransportKindOpenAPI:
		return ap.ManifestOpenAPIEndpoint{Transport: "openapi"}
	case TransportKindNative:
		return ap.ManifestNativeEndpoint{Transport: "native"}
	}
	return ap.ManifestEventsEndpoint{Transport: "events"}
}

func transportsOpen(src transportsSource, m ap.Manifest) (adapters.AppAdapter, *transportsPlan, error) {
	return src.build(context.Background(), "demo-library", &apppkg.Package{Manifest: m}, apppkg.Record{ApplicationID: "demo-library", ManifestHash: "h1", State: apppkg.StateActive, Path: "packages/demo-library/h1"})
}

// The transport is chosen by the kind the manifest declares, from the one
// registry, and only that factory is called. Each transport registers itself
// with RegisterTransport (as transport_<kind>.go does from init) and the
// registry is never edited to add one.
func TestTransportRegistryChoosesByDeclaredKind(t *testing.T) {
	transportsSwapDefault(t)
	rec := &transportsRecorder{}
	for _, kind := range transportsOperationKinds {
		RegisterTransport(kind, rec.factory(fake.NewApp(fake.WithApplicationVersion("v-"+kind))))
	}
	dataDir := t.TempDir()
	src := transportsSource{dataDir: dataDir}

	for _, kind := range transportsOperationKinds {
		t.Run(kind, func(t *testing.T) {
			before := len(rec.kinds())
			ad, plan, err := transportsOpen(src, transportsManifest(map[string]ap.ManifestEndpointValue{
				"b-" + kind: transportsEndpointOf(kind),
				"a-" + kind: transportsEndpointOf(kind),
				"events":    transportsEndpointOf("events"),
			}))
			if err != nil {
				t.Fatal(err)
			}
			if plan.Kind != kind || len(plan.Events) != 1 || plan.Events[0] != "events" {
				t.Errorf("plan = %+v", plan)
			}
			// Exactly the factory of the declared kind ran, once, and it got the
			// endpoints of that kind (the events endpoint is not one of them).
			got := rec.kinds()[before:]
			if len(got) != 1 || got[0] != kind {
				t.Fatalf("factories called = %v, want only %s", got, kind)
			}
			req := rec.calls[len(rec.calls)-1]
			if len(req.Endpoints) != 2 || req.Endpoints[0].ID != "a-"+kind || req.Endpoints[1].ID != "b-"+kind {
				t.Errorf("endpoints = %+v, want a- and b- sorted", req.Endpoints)
			}
			if req.ApplicationID != "demo-library" || req.Package == nil || req.Record.ManifestHash != "h1" ||
				req.Dir != filepath.Join(dataDir, "packages", "demo-library", "h1") {
				t.Errorf("request = %+v", req)
			}
			// The adapter is that factory's, wrapped.
			d, err := ad.Describe(context.Background())
			if err != nil || d.ApplicationVersion != "v-"+kind {
				t.Errorf("Describe = %+v, %v: not the %s factory's adapter", d, err, kind)
			}
		})
	}
}

// Registering is a programming act, and a mistake in it is found when the
// binary starts: a kind that carries no operations, a typo, a nil factory and
// a second factory for a kind all panic.
func TestRegisterTransportRefusesProgrammingErrors(t *testing.T) {
	transportsSwapDefault(t)
	ok := func(context.Context, TransportRequest) (adapters.AppAdapter, error) { return nil, nil }
	RegisterTransport(TransportKindMCP, ok)
	for name, fn := range map[string]func(){
		"events":     func() { RegisterTransport("events", ok) },
		"unknown":    func() { RegisterTransport("grpc", ok) },
		"empty":      func() { RegisterTransport("", ok) },
		"nil":        func() { RegisterTransport(TransportKindOpenAPI, nil) },
		"duplicate":  func() { RegisterTransport(TransportKindMCP, ok) },
		"upper case": func() { RegisterTransport("MCP", ok) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			fn()
		}()
	}
	if got := transportsDefault.kinds(); len(got) != 1 || got[0] != TransportKindMCP {
		t.Errorf("registered = %v", got)
	}
}

func TestTransportsPlanFor(t *testing.T) {
	for name, tc := range map[string]struct {
		eps    map[string]ap.ManifestEndpointValue
		kind   string
		reason string
		events []string
	}{
		"one mcp":              {eps: map[string]ap.ManifestEndpointValue{"x": transportsEndpointOf("mcp")}, kind: "mcp"},
		"mcp and events":       {eps: map[string]ap.ManifestEndpointValue{"x": transportsEndpointOf("mcp"), "e": transportsEndpointOf("events")}, kind: "mcp", events: []string{"e"}},
		"two native":           {eps: map[string]ap.ManifestEndpointValue{"x": transportsEndpointOf("native"), "y": transportsEndpointOf("native")}, kind: "native"},
		"none":                 {eps: map[string]ap.ManifestEndpointValue{}, reason: transportsReasonNoEndpoint},
		"events only":          {eps: map[string]ap.ManifestEndpointValue{"e": transportsEndpointOf("events")}, reason: transportsReasonNoEndpoint, events: []string{"e"}},
		"mcp and openapi":      {eps: map[string]ap.ManifestEndpointValue{"x": transportsEndpointOf("mcp"), "y": transportsEndpointOf("openapi")}, reason: transportsReasonMixedKinds},
		"all three":            {eps: map[string]ap.ManifestEndpointValue{"x": transportsEndpointOf("mcp"), "y": transportsEndpointOf("openapi"), "z": transportsEndpointOf("native")}, reason: transportsReasonMixedKinds},
		"an endpoint of no id": {eps: map[string]ap.ManifestEndpointValue{"x": nil}, reason: transportsReasonNoEndpoint},
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := transportsPlanFor(transportsManifest(tc.eps))
			if tc.reason == "" {
				if err != nil || plan.Kind != tc.kind {
					t.Fatalf("plan = %+v, %v; want kind %s", plan, err, tc.kind)
				}
			} else if reason, _, ok := transportsUnavailableReason(err); !ok || reason != tc.reason || contracts.CodeOf(err) != contracts.ErrUnavailable {
				t.Fatalf("err = %v, want unavailable with reason %s", err, tc.reason)
			}
			if strings.Join(plan.Events, ",") != strings.Join(tc.events, ",") && tc.reason != transportsReasonMixedKinds {
				t.Errorf("events = %v, want %v", plan.Events, tc.events)
			}
		})
	}
}

// A kind nobody registered is said, never skipped: there is no adapter, the
// error names the kind and what this build does register, a transport of
// another kind is never used in its place, and the audit records the
// application as unverified with the same reason, on stderr and in its output.
func TestUnknownKindIsUnavailableNotSkipped(t *testing.T) {
	transportsSwapDefault(t)
	src := transportsSource{dataDir: t.TempDir()}
	mcp := transportsManifest(map[string]ap.ManifestEndpointValue{"x": transportsEndpointOf("mcp")})

	// 1. nothing registered
	ad, plan, err := transportsOpen(src, mcp)
	reason, kind, typed := transportsUnavailableReason(err)
	if ad != nil || err == nil || !typed || reason != transportsReasonNotRegistered || kind != TransportKindMCP || contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("no transport: adapter %v, err %v", ad, err)
	}
	if plan == nil || plan.Kind != TransportKindMCP {
		t.Errorf("plan = %+v: the declared kind is not reported", plan)
	}
	for _, want := range []string{`"mcp"`, "no adapter transport", "registered in this build: none"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not say %q", err, want)
		}
	}
	if !transportsNoAdapter(err) {
		t.Error("a missing transport is not reported as a deterministic no-adapter")
	}

	// 2. another kind registered: not used in its place
	other := &transportsRecorder{}
	RegisterTransport(TransportKindOpenAPI, other.factory(fake.NewApp()))
	RegisterTransport(TransportKindNative, other.factory(fake.NewApp()))
	ad, _, err = transportsOpen(src, mcp)
	if ad != nil || !transportsNoAdapter(err) || !strings.Contains(err.Error(), "registered in this build: native, openapi") {
		t.Fatalf("with only other kinds registered: adapter %v, err %v", ad, err)
	}
	if len(other.kinds()) != 0 {
		t.Fatalf("a factory of another kind was called: %v", other.kinds())
	}

	// 3. the manifest declares nothing to reach, or two kinds: no adapter either
	for name, tc := range map[string]struct {
		eps    map[string]ap.ManifestEndpointValue
		reason string
	}{
		"no operation endpoint": {map[string]ap.ManifestEndpointValue{"e": transportsEndpointOf("events")}, transportsReasonNoEndpoint},
		"mixed kinds":           {map[string]ap.ManifestEndpointValue{"x": transportsEndpointOf("openapi"), "y": transportsEndpointOf("native")}, transportsReasonMixedKinds},
	} {
		ad, _, err := transportsOpen(src, transportsManifest(tc.eps))
		if r, _, ok := transportsUnavailableReason(err); ad != nil || !ok || r != tc.reason {
			t.Errorf("%s: adapter %v, err %v", name, ad, err)
		}
	}
	if len(other.kinds()) != 0 {
		t.Fatalf("a factory was called for a package it cannot serve: %v", other.kinds())
	}

	// 4. a factory that fails is not "no adapter": it is an open failure, with
	// the cause kept out of the message a client could see
	bad := newTransportsRegistry()
	bad.register(TransportKindMCP, func(context.Context, TransportRequest) (adapters.AppAdapter, error) {
		return nil, errors.New("dial tcp 10.0.0.9:443: secret-host unreachable")
	})
	_, _, err = transportsOpen(transportsSource{registry: bad, dataDir: t.TempDir()}, mcp)
	if r, _, ok := transportsUnavailableReason(err); !ok || r != transportsReasonOpenFailed || transportsNoAdapter(err) {
		t.Fatalf("a failing factory: %v", err)
	}
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		t.Fatalf("not a typed error: %v", err)
	}
	if strings.Contains(ce.Message, "secret-host") {
		t.Errorf("the client-visible message %q carries the cause", ce.Message)
	}

	// 5. the audit says it, in its output and on stderr, and runs on
	e, key := transportsOpsEnv(t, "")
	res := e.run("audit", "run", "-signing-key", "audit-signer="+key, "-no-activate", "demo-library")
	var out transportsRunOut
	decode(t, res.stdout, &out)
	if out.Transport.Status != auditTransportUnverified || out.Transport.Kind != TransportKindMCP || out.Transport.Reason != transportsReasonNotRegistered {
		t.Errorf("transport = %+v", out.Transport)
	}
	for _, want := range []string{"no adapter is available", `"mcp"`, "unverified"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr %q does not say %q", res.stderr, want)
		}
	}
	if !transportsHasFinding(out, "surface.not_inspected") {
		t.Errorf("an application with no adapter must raise surface.not_inspected: %+v", out.Findings)
	}
}

// transportsRunOut is runOut plus the members this task adds.
type transportsRunOut struct {
	runOut
	Transport auditTransportOut    `json:"transport"`
	Server    *transportsServerOut `json:"server"`
}

func transportsHasFinding(out transportsRunOut, checkID string) bool {
	for _, f := range out.Findings {
		if f.CheckID == checkID {
			return true
		}
	}
	return false
}

// transportsOpsEnv is a deployment with the publisher and the platform key
// trusted and demo-library installed in quarantine. The audit signer key is
// written by transportsSignerKey.
func transportsOpsEnv(t *testing.T, extraConfig string) (*opsEnv, string) {
	t.Helper()
	e := newOpsEnv(t, extraConfig)
	signerPriv, signerPub, _ := e.genKey("audit-signer")
	e.mustRun("trust", "add-key", "-app", trust.PlatformApplicationID, "-key-id", "audit-signer", "-public-key-file", signerPub)
	e.mustRun("trust", "add-key", "-app", "demo-library", "-key-id", devkeys.KeyID, "-public-key", devkeys.PublicKeyBase64(), "-allow-test-keys")
	e.mustRun("package", "install", filepath.Join(fixtures, "valid-minimal"))
	return e, signerPriv
}

// transportsActivate audits demo-library, has alice accept every warning and
// audits again until the package is active. The deployment has no transport,
// so the warnings are the ones every CLI activation has. A package whose
// warnings were accepted before (one quarantined again) activates on the first
// audit.
func transportsActivate(t *testing.T, e *opsEnv, key string) transportsRunOut {
	t.Helper()
	sign := "audit-signer=" + key
	var first transportsRunOut
	decode(t, e.run("audit", "run", "-signing-key", sign, "-operator", "alice", "demo-library").stdout, &first)
	if first.Activated {
		return first
	}
	for _, f := range first.Findings {
		if f.Severity == "warning" && !f.Accepted {
			e.mustRun("audit", "accept-warning", "-finding", f.Key, "-operator", "alice", "-reason", "reviewed", "demo-library")
		}
	}
	res := e.mustRun("audit", "run", "-signing-key", sign, "-operator", "alice", "demo-library")
	var out transportsRunOut
	decode(t, res.stdout, &out)
	if !out.Activated {
		t.Fatalf("not activated: %+v\n%s", out, res.stderr)
	}
	return out
}

const transportsAcceptConfig = "integration_audit:\n  accept_operators: [alice]\n"

// The audit and serve consult ONE registry: the factory registered for the kind
// a manifest declares builds the adapter the audit inspects (the package is
// quarantined then) and, later, the adapter the running service calls (the
// package is active), and the service's own wiring asks the process registry,
// not one of its own.
func TestServeAndAuditUseTheSameRegistry(t *testing.T) {
	transportsSwapDefault(t)
	rec := &transportsRecorder{}
	RegisterTransport(TransportKindMCP, rec.factory(fake.NewApp(fake.WithApplicationVersion("v-shared"))))

	e, key := transportsOpsEnv(t, transportsAcceptConfig)

	// the audit: the factory's adapter is inspected, not "no adapter"
	res := e.run("audit", "run", "-signing-key", "audit-signer="+key, "-no-activate", "demo-library")
	var out transportsRunOut
	decode(t, res.stdout, &out)
	if out.Transport.Status != auditTransportInUse || out.Transport.Kind != TransportKindMCP || out.Transport.Reason != "" {
		t.Fatalf("transport = %+v\n%s", out.Transport, res.stderr)
	}
	if transportsHasFinding(out, "surface.not_inspected") || strings.Contains(res.stderr, "no adapter is available") {
		t.Errorf("the audit found no adapter although one is registered: %s", res.stderr)
	}
	if got := rec.kinds(); len(got) != 1 || rec.calls[0].Record.State != apppkg.StateQuarantined || rec.calls[0].Record.ManifestHash != out.ManifestHash {
		t.Fatalf("the audit's factory calls: %v", got)
	}

	// the service: production wiring over the same data directory, package
	// active (the audit above found problems in a fake host, so the state is set
	// the way the existing adapter tests do)
	ctx := context.Background()
	w, db := transportsWire(t, e)
	if _, err := db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rep_cli_01' WHERE application_id = 'demo-library'`); err != nil {
		t.Fatal(err)
	}
	src, ok := gateway.Lookup[actions.AdapterSource](w.services)
	if !ok {
		t.Fatal("serve does not provide an adapter source")
	}
	as, ok := src.(*adapterSource)
	if !ok || as.transports.registry != nil || as.transports.installer == nil {
		t.Fatalf("serve's adapter source = %#v: it must ask the process registry (registry nil)", src)
	}
	ad, err := src.Adapter(ctx, "demo-library")
	if err != nil {
		t.Fatal(err)
	}
	if d, err := ad.Describe(ctx); err != nil || d.ApplicationVersion != "v-shared" {
		t.Fatalf("serve's adapter is not the registered factory's: %+v, %v", d, err)
	}
	if got := rec.kinds(); len(got) != 2 || rec.calls[1].Record.State != apppkg.StateActive || rec.calls[1].Record.ManifestHash != rec.calls[0].Record.ManifestHash {
		t.Fatalf("after serve: factory calls %v, want one for the audit and one for serve, about the same package", got)
	}
}

// transportsWire builds the service's wiring (wireV1, the production one) over
// the data directory of an ops deployment, in this process, like `alixai serve`
// does before it listens. The store is a second connection to the same file the
// CLI commands use.
func transportsWire(t *testing.T, e *opsEnv) (*v1Wiring, *store.DB) {
	t.Helper()
	ctx := context.Background()
	dataDir := filepath.Join(e.dir, "data")
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dataDir, store.DefaultFileName)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := clock.Real{}
	w, err := wireV1(ctx, wireInput{
		log: discardLog(), clk: clk, db: db, dataDir: dataDir, providers: providers.NewRegistry(clk),
		off: &inferenceOff{reason: "test", err: errors.New("test")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return w, db
}

// transportsHideEndpoint moves the endpoint record away, as if no server ran,
// and returns the function that puts it back.
func transportsHideEndpoint(t *testing.T, dataDir string) (restore func()) {
	t.Helper()
	from := filepath.Join(dataDir, gateway.AdminEndpointFile)
	to := from + ".hidden"
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Rename(to, from); err != nil {
			t.Fatal(err)
		}
	}
}

func transportsServing(t *testing.T, e *opsEnv, w *v1Wiring) (stop func()) {
	t.Helper()
	stop, err := serveAdminStart(context.Background(), discardLog(), clock.Real{}, filepath.Join(e.dir, "data"), w.admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return stop
}

func transportsListed(w *v1Wiring, app string) bool {
	for _, a := range w.admin.(serveAdminControl).registry.Applications() {
		if a == app {
			return true
		}
	}
	return false
}

// A quarantine must not leave the running server's capability registry serving
// the application for its 30 second cache. The server listens on a loopback
// admin interface and records where in the data directory; `audit quarantine`
// (and an activation) tell it through the runner, and the registry changes at
// once. Without a server the command says so and succeeds; with one that is
// reachable and does not confirm, it fails.
func TestQuarantineRefreshesRunningServer(t *testing.T) {
	auditChecksProbesBuiltinChecksOnly(t) // this test is about what follows an audit, not the runtime probes (M1-18)
	transportsSwapDefault(t)
	e, key := transportsOpsEnv(t, transportsAcceptConfig)
	dataDir := filepath.Join(e.dir, "data")
	ctx := context.Background()

	// the package is activated while no server runs: nothing to tell, and the
	// command says so
	act := transportsActivate(t, e, key)
	if act.Server == nil || act.Server.Status != transportsServerNone {
		t.Fatalf("activation with no server: server = %+v", act.Server)
	}

	// the server starts over the same data directory and loads the application
	w, _ := transportsWire(t, e)
	registry := w.admin.(serveAdminControl).registry
	if err := registry.Refresh(ctx, "demo-library"); err != nil {
		t.Fatal(err)
	}
	if !transportsListed(w, "demo-library") {
		t.Fatal("the registry did not load the active package")
	}
	stop := transportsServing(t, e, w)

	// control: a quarantine nobody tells the server about leaves its registry
	// serving the application (this is the 30 seconds the admin route removes)
	restore := transportsHideEndpoint(t, dataDir)
	res := e.mustRun("audit", "quarantine", "-reason", "control", "-operator", "root", "demo-library")
	var q quarantineOut
	decode(t, res.stdout, &q)
	if q.State != "quarantined" || q.Server == nil || q.Server.Status != transportsServerNone || !strings.Contains(res.stderr, "no server was told") {
		t.Fatalf("a quarantine with no server to tell: %+v\n%s", q, res.stderr)
	}
	if !transportsListed(w, "demo-library") {
		t.Fatal("control failed: the registry dropped the application without being told, so the test proves nothing")
	}
	restore()

	// the package is activated again; this time the server is told, and
	// reloads the package it holds
	act = transportsActivate(t, e, key)
	if act.Server == nil || act.Server.Status != transportsServerRefreshed {
		t.Fatalf("activation with a server running: server = %+v", act.Server)
	}
	if !transportsListed(w, "demo-library") {
		t.Fatal("the registry lost the application on a refresh")
	}

	// the quarantine: the running server evicts the application at once
	res = e.mustRun("audit", "quarantine", "-reason", "re-audit found a critical change", "-operator", "root", "demo-library")
	q = quarantineOut{}
	decode(t, res.stdout, &q)
	if q.Server == nil || q.Server.Status != transportsServerEvicted || !strings.Contains(res.stderr, "evicted demo-library") {
		t.Fatalf("quarantine = %+v\n%s", q, res.stderr)
	}
	if transportsListed(w, "demo-library") {
		t.Fatal("the running server still holds the application after the quarantine returned")
	}
	if _, err := w.admin.(serveAdminControl).apps.Package(ctx, "demo-library"); contracts.CodeOf(err) != contracts.ErrQuarantined {
		t.Errorf("the server's package source still serves it: %v", err)
	}

	// the server's own stop removes the endpoint record; with none, a command
	// says there is no server
	stop()
	if _, err := os.Stat(filepath.Join(dataDir, gateway.AdminEndpointFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the endpoint file is still there after the server stopped: %v", err)
	}
}

// A stale record (the server died and left it) is "no server", and a server
// that is reachable and refuses is a failure of the command, not a success:
// the store is changed either way, the command never claims more than it knows.
func TestQuarantineReportsStaleAndUnconfirmedServers(t *testing.T) {
	auditChecksProbesBuiltinChecksOnly(t) // this test is about what follows an audit, not the runtime probes (M1-18)
	transportsSwapDefault(t)
	e, key := transportsOpsEnv(t, transportsAcceptConfig)
	dataDir := filepath.Join(e.dir, "data")
	transportsActivate(t, e, key)

	// stale: an endpoint nobody listens at
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	_ = ln.Close()
	token := strings.Repeat("a1", 32)
	if err := gateway.WriteAdminEndpoint(dataDir, gateway.AdminEndpoint{URL: dead, Token: token, PID: 1, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	res := e.mustRun("audit", "quarantine", "-reason", "stale record", "demo-library")
	var q quarantineOut
	decode(t, res.stdout, &q)
	if q.Server == nil || q.Server.Status != transportsServerNone || !strings.Contains(q.Server.Detail, "stale") {
		t.Fatalf("a stale record: %+v\n%s", q, res.stderr)
	}

	// unconfirmed: a server that answers and does not accept the credential
	transportsActivate(t, e, key)
	ctl := &transportsRecordingControl{}
	adm, err := gateway.NewAdminServer(gateway.AdminOptions{Control: ctl, Token: strings.Repeat("b2", 32)})
	if err != nil {
		t.Fatal(err)
	}
	aln, err := gateway.AdminListen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = adm.Serve(ctx, aln) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := gateway.WriteAdminEndpoint(dataDir, gateway.AdminEndpoint{URL: "http://" + aln.Addr().String(), Token: token, PID: 1, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	res = e.run("audit", "quarantine", "-reason", "refused", "demo-library")
	if res.code != ExitError || !strings.Contains(res.stderr, "quarantined in the store, but") || !strings.Contains(res.stderr, "did not confirm") {
		t.Fatalf("an unconfirmed quarantine: exit %d\n%s", res.code, res.stderr)
	}
	q = quarantineOut{}
	decode(t, res.stdout, &q)
	if q.State != "quarantined" || q.Server == nil || q.Server.Status != transportsServerFailed {
		t.Errorf("quarantine = %+v: the output must still say what happened", q)
	}
	if len(ctl.calls()) != 0 {
		t.Errorf("the server acted on a request with the wrong credential: %v", ctl.calls())
	}
	if strings.Contains(res.stderr+res.stdout, token) {
		t.Error("the operator credential is in the command's output")
	}
}

type transportsRecordingControl struct {
	mu sync.Mutex
	c  []string
}

func (r *transportsRecordingControl) Refresh(_ context.Context, app string) error {
	r.mu.Lock()
	r.c = append(r.c, "refresh "+app)
	r.mu.Unlock()
	return nil
}

func (r *transportsRecordingControl) Deactivate(app string) {
	r.mu.Lock()
	r.c = append(r.c, "deactivate "+app)
	r.mu.Unlock()
}

func (r *transportsRecordingControl) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.c...)
}

func TestQuarantineUsageAndRefusals(t *testing.T) {
	transportsSwapDefault(t)
	e := newOpsEnv(t, "")
	for _, args := range [][]string{
		{"audit", "quarantine"},
		{"audit", "quarantine", "a", "b"},
		{"audit", "quarantine", "demo-library"},
		{"audit", "quarantine", "-reason", "  ", "demo-library"},
		{"audit", "quarantine", "-reason", "x", "-operator", "has space", "demo-library"},
	} {
		if res := e.run(args...); res.code != ExitUsage {
			t.Errorf("%v: exit %d, want usage", args, res.code)
		}
	}
	// nothing active to quarantine
	if res := e.run("audit", "quarantine", "-reason", "x", "demo-library"); res.code != ExitError || !strings.Contains(res.stderr, "no active package") {
		t.Errorf("exit %d\n%s", res.code, res.stderr)
	}
}

// The notifier reads the endpoint record the way the CLI does, and never uses
// one it should not: a record that names a non-loopback address, one that
// other users can read (POSIX), or one that is not the format is "failed", never
// "no server" and never a call.
func TestTransportsServerRefusesAnUnsafeEndpointRecord(t *testing.T) {
	dir := t.TempDir()
	n := newTransportsServer(dir)
	if err := n.Refresh(context.Background(), "demo-library"); err != nil {
		t.Fatalf("no record: %v", err)
	}
	if out, _ := n.result(); out == nil || out.Status != transportsServerNone {
		t.Fatalf("no record: %+v", out)
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, gateway.AdminEndpointFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"not json":        "{",
		"non-loopback":    `{"format":"alixai.admin-endpoint/v1","url":"http://10.1.2.3:8080","token":"` + strings.Repeat("a", 40) + `","pid":1,"started_at":"2026-10-02T00:00:00Z"}`,
		"unknown member":  `{"format":"alixai.admin-endpoint/v1","url":"http://127.0.0.1:8080","token":"` + strings.Repeat("a", 40) + `","extra":1,"pid":1,"started_at":"2026-10-02T00:00:00Z"}`,
		"short token":     `{"format":"alixai.admin-endpoint/v1","url":"http://127.0.0.1:8080","token":"abc","pid":1,"started_at":"2026-10-02T00:00:00Z"}`,
		"https":           `{"format":"alixai.admin-endpoint/v1","url":"https://127.0.0.1:8080","token":"` + strings.Repeat("a", 40) + `","pid":1,"started_at":"2026-10-02T00:00:00Z"}`,
		"credentials url": `{"format":"alixai.admin-endpoint/v1","url":"http://u:p@127.0.0.1:8080","token":"` + strings.Repeat("a", 40) + `","pid":1,"started_at":"2026-10-02T00:00:00Z"}`,
	} {
		write(body)
		err := n.Refresh(context.Background(), "demo-library")
		out, ferr := n.result()
		if err == nil || ferr == nil || out == nil || out.Status != transportsServerFailed {
			t.Errorf("%s: err %v, outcome %+v", name, err, out)
		}
		if strings.Contains(out.Detail, strings.Repeat("a", 40)) {
			t.Errorf("%s: the detail carries the credential", name)
		}
	}
}

func TestTransportsServerNoteWording(t *testing.T) {
	for status, want := range map[string]string{
		transportsServerRefreshed: "reloaded",
		transportsServerEvicted:   "evicted demo-library",
		transportsServerNone:      "no server was told",
		transportsServerFailed:    "NOT confirmed",
	} {
		if got := (transportsServerOut{Status: status, Detail: "d"}).note("demo-library"); !strings.Contains(got, want) {
			t.Errorf("%s: %q does not contain %q", status, got, want)
		}
	}
}

// `alixai serve` runs the admin interface on a loopback listener of its own,
// records where in the data directory (owner-only), never mentions it on the
// public listener or in the "listening on" line, and removes the record when it
// stops. The client the CLI uses reaches it end to end.
func TestServeRunsTheAdminInterfaceApartFromThePublicAPI(t *testing.T) {
	transportsSwapDefault(t)
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	cfg, dataDir := serveConfig(t, f, "")
	srv := startServe(t, cfg)

	ep, err := gateway.ReadAdminEndpoint(dataDir)
	if err != nil {
		t.Fatalf("serve left no admin endpoint record: %v", err)
	}
	u, err := url.Parse(ep.URL)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := url.Parse(srv.base)
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() || u.Port() == pub.Port() {
		t.Fatalf("the admin interface is at %s and the public API at %s: want a loopback listener of its own", ep.URL, srv.base)
	}
	if strings.Count(srv.stdout.String(), "listening on") != 1 || strings.Contains(srv.stdout.String(), u.Host) || strings.Contains(srv.stdout.String(), ep.Token) {
		t.Errorf("stdout names the admin interface: %q", srv.stdout.String())
	}
	if strings.Contains(srv.stderr.String(), ep.Token) {
		t.Error("the operator credential is in the log")
	}

	// the public listener has no such route, with the credential or without
	for _, path := range []string{gateway.AdminPathRefresh, gateway.AdminPathDeactivate} {
		req, _ := http.NewRequest(http.MethodPost, srv.base+path, strings.NewReader(`{"application_id":"demo-library"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(gateway.AdminTokenHeader, ep.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s on the public listener = %d, want 404", path, resp.StatusCode)
		}
	}

	// the client the CLI uses, against the real server (nothing installed: the
	// application has no active package, which is what refresh then confirms)
	cl, err := gateway.NewAdminClient(ep)
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Refresh(context.Background(), "demo-library"); err != nil {
		t.Fatalf("Refresh against serve: %v", err)
	}
	if err := cl.Deactivate(context.Background(), "demo-library"); err != nil {
		t.Fatalf("Deactivate against serve: %v", err)
	}
	bad := ep
	bad.Token = strings.Repeat("d", 64)
	wrong, _ := gateway.NewAdminClient(bad)
	if err := wrong.Refresh(context.Background(), "demo-library"); contracts.CodeOf(err) != contracts.ErrUnauthenticated {
		t.Fatalf("serve accepted the wrong credential: %v", err)
	}
	if !strings.Contains(srv.stderr.String(), `"admin interface"`) {
		t.Errorf("serve did not log the admin interface:\n%s", srv.stderr.String())
	}
	if !strings.Contains(srv.stderr.String(), "no adapter transport is registered in this build") {
		t.Errorf("serve did not say at the start that no transport is registered:\n%s", srv.stderr.String())
	}

	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, gateway.AdminEndpointFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the endpoint record is still there after serve stopped: %v", err)
	}
	if err := wrong.Refresh(context.Background(), "demo-library"); !errors.Is(err, gateway.ErrAdminUnreachable) {
		t.Errorf("after serve stopped the admin address answers: %v", err)
	}
}

// An admin interface that cannot be set up refuses the start: without it the
// claim that a quarantine takes effect at once would quietly not hold.
func TestServeRefusesToStartWithoutItsAdminInterface(t *testing.T) {
	transportsSwapDefault(t)
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	cfg, dataDir := serveConfig(t, f, "")
	// the place of the record is taken by a directory, which no rename replaces
	if err := os.MkdirAll(filepath.Join(dataDir, gateway.AdminEndpointFile), 0o700); err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	err, stdout, _ := refusedServe(t, cfg)
	if err == nil || !strings.Contains(err.Error(), "operator admin interface") {
		t.Fatalf("serve = %v, want the admin interface refusal", err)
	}
	// a refused start exits at once; it does not wait for a context nobody
	// cancels (the service jobs it had already started)
	if d := time.Since(begin); d > 3*time.Second {
		t.Errorf("serve took %s to refuse", d)
	}
	if strings.Contains(stdout, "listening on") {
		t.Errorf("serve announced it was listening: %q", stdout)
	}
}

// The one-line helper the other commands use: no server is "no server", and
// the note goes to stderr.
func TestTransportsNotifyRefreshSaysWhatCameOfIt(t *testing.T) {
	var errb strings.Builder
	env := &Env{Stderr: &errb}
	dir := t.TempDir()
	out, err := transportsNotifyRefresh(context.Background(), env, dir, "demo-library")
	if err != nil || out == nil || out.Status != transportsServerNone || !strings.Contains(errb.String(), "no server was told") {
		t.Fatalf("no server: %+v, %v, %q", out, err, errb.String())
	}
}

// transportsCloser is a transport that holds something to release.
type transportsCloser struct {
	adapters.AppAdapter
	closed *atomic.Int32
}

func (c transportsCloser) Close() error { c.closed.Add(1); return nil }

// The service builds one adapter per application and package at a time, however
// many first requests arrive together (a transport that opens a connection when
// it is built must not be built twice), and closes what it drops: an adapter
// evicted by the admin interface, one replaced, and all of them at stop.
func TestAdapterSourceBuildsOnceAndClosesWhatItDrops(t *testing.T) {
	reg := transportsSwapDefault(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	dataDir := t.TempDir()
	db := storetest.Open(t, nil)
	verifier := trust.NewVerifier(devTrust{}, clk)
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{DB: db, Clock: clk, DataDir: dataDir, Verifier: verifier, CoreVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	apps, err := gateway.NewApplications(inst, apppkg.Options{Verifier: verifier, CoreVersion: "0.1.0"}, clk, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Install(ctx, apppkg.FromDir("../../examples/packages/valid-minimal")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rep_cli_01' WHERE application_id = 'demo-library'`); err != nil {
		t.Fatal(err)
	}
	src := &adapterSource{
		apps: apps, log: discardLog(), cache: map[string]cachedAdapter{},
		transports: transportsSource{installer: inst, dataDir: dataDir},
	}

	var built, closed atomic.Int32
	reg.register(TransportKindMCP, func(context.Context, TransportRequest) (adapters.AppAdapter, error) {
		built.Add(1)
		time.Sleep(20 * time.Millisecond) // wide enough for every goroutine to arrive before the first build ends
		return transportsCloser{AppAdapter: fake.NewAdapter(fake.NewApp()), closed: &closed}, nil
	})

	const callers = 16
	got := make([]adapters.AppAdapter, callers)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ad, err := src.Adapter(ctx, "demo-library")
			if err != nil {
				t.Error(err)
			}
			got[i] = ad
		}()
	}
	wg.Wait()
	if built.Load() != 1 {
		t.Fatalf("the factory ran %d times for %d concurrent first requests, want once", built.Load(), callers)
	}
	for _, ad := range got {
		if ad != got[0] {
			t.Fatal("concurrent callers were given different adapters")
		}
	}
	if closed.Load() != 0 {
		t.Fatal("an adapter in use was closed")
	}

	// evicted (what the admin interface's deactivate does): closed, and built again on its next use
	src.forget("demo-library")
	if closed.Load() != 1 {
		t.Fatalf("closed = %d after an eviction, want 1", closed.Load())
	}
	if _, err := src.Adapter(ctx, "demo-library"); err != nil || built.Load() != 2 {
		t.Fatalf("after an eviction: built %d, err %v", built.Load(), err)
	}
	// the service stops: whatever it still holds is closed
	src.close()
	if closed.Load() != 2 {
		t.Fatalf("closed = %d after stop, want 2", closed.Load())
	}

	// the wrapped adapter forwards Close to the transport, and a transport that
	// cannot be closed is not an error
	ad, _, err := transportsOpen(transportsSource{dataDir: t.TempDir()}, transportsManifest(map[string]ap.ManifestEndpointValue{"x": transportsEndpointOf("mcp")}))
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := ad.(io.Closer); !ok || c.Close() != nil || closed.Load() != 3 {
		t.Fatalf("Close on a built adapter: closed = %d", closed.Load())
	}
	reg2 := newTransportsRegistry()
	reg2.register(TransportKindMCP, func(context.Context, TransportRequest) (adapters.AppAdapter, error) {
		return fake.NewAdapter(fake.NewApp()), nil
	})
	plain, _, err := transportsOpen(transportsSource{registry: reg2, dataDir: t.TempDir()}, transportsManifest(map[string]ap.ManifestEndpointValue{"x": transportsEndpointOf("mcp")}))
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := plain.(io.Closer); !ok || c.Close() != nil {
		t.Fatal("a transport with nothing to close made Close fail")
	}
	transportsClose(nil)
}
