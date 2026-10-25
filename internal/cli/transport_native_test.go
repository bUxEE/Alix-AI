package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	nativetransport "github.com/buxee/alix-ai/adapters/native"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/pkg/bridge"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// nativeTransportClinicDir is the reference application's real, signed package.
// Its capabilities are what the native transport maps here: the package is
// rewritten in memory to reach the same application over a native endpoint, so
// that the mapping is tested against what a package really contains.
const nativeTransportClinicDir = "../../examples/reference-apps/clinic-booking/package"

const (
	nativeTransportApp    = "clinic-booking"
	nativeTransportEP     = "clinic-bridge"
	nativeTransportKeyEnv = "NATIVE_TRANSPORT_TEST_KEY"
	nativeTransportSecret = "native-transport-test-key-0123456789-abcdef"
	nativeTransportCfgRel = "schemas/adapters/clinic-native.yaml"
	nativeTransportCfgDoc = "server: " + nativeTransportEP + "\nkey:\n  id: k1\n  ref: env:" + nativeTransportKeyEnv + "\nmanagement:\n  provisioning: [create_tenant]\n"
)

// nativeTransportOkResult is a well-formed DeterministicResult of a successful
// read.
const nativeTransportOkResult = `{"status":"ok","identifiers":[],"values":[{"field":"name","value":{"kind":"text","value":"Rossi"}}],"errors":[],"allowed_next_actions":[],"freshness":{"as_of":"2026-10-09T08:00:00Z","version":"v1"}}`

// nativeTransportMethods lists every native_method the package maps, sorted.
func nativeTransportMethods(pkg *apppkg.Package) []string {
	set := map[string]bool{}
	add := func(b *ap.CapabilityOperationBinding) {
		if b != nil && b.NativeMethod != nil {
			set[string(*b.NativeMethod)] = true
		}
	}
	for _, c := range pkg.Capabilities {
		add(c.Operations.Read)
		add(c.Operations.Validate)
		add(c.Operations.Prepare)
		add(c.Operations.Commit)
		add(c.Operations.Status)
		for _, et := range c.EntityTypes {
			add(et.Resolve)
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// nativeTransportHost is a Go bridge that declares every method the package maps
// and answers each with a well-formed result, and remembers what it was sent.
type nativeTransportHost struct {
	Network string
	Address string
	srv     *bridge.Server
	mu      sync.Mutex
	calls   []bridge.Call
}

func (h *nativeTransportHost) seen() []bridge.Call {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bridge.Call(nil), h.calls...)
}

func nativeTransportStartHost(t testing.TB, pkg *apppkg.Package, network string) *nativeTransportHost {
	t.Helper()
	h := &nativeTransportHost{Network: network}
	srv, err := bridge.New(bridge.Config{
		ApplicationID: nativeTransportApp, EndpointID: nativeTransportEP, Keys: map[string][]byte{"k1": []byte(nativeTransportSecret)},
		ApplicationVersion: "1.0.0", HandshakeTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range nativeTransportMethods(pkg) {
		if err := srv.Handle(m, func(_ context.Context, c bridge.Call) (bridge.Reply, error) {
			h.mu.Lock()
			h.calls = append(h.calls, c)
			h.mu.Unlock()
			return bridge.Reply{Result: json.RawMessage(nativeTransportOkResult), IdempotencyKey: c.IdempotencyKey}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.HandleManagement("create_tenant", func(context.Context, map[string]any) (any, error) {
		return map[string]any{"tenant_id": "ten_disposable"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if network == nativetransport.NetworkUnix {
		dir, err := os.MkdirTemp("", "nt")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		h.Address = filepath.Join(dir, "run", "b.sock")
	} else {
		h.Address = "127.0.0.1:0"
	}
	var ln net.Listener
	if network == nativetransport.NetworkUnix {
		ln, err = bridge.ListenUnix(h.Address)
	} else {
		ln, err = bridge.ListenLoopback(0)
	}
	if err != nil {
		t.Fatal(err)
	}
	h.srv, h.Address = srv, ln.Addr().String()
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	return h
}

// nativeTransportInstall loads the clinic package, rewrites it to reach address
// over a native endpoint of the given IPC kind and returns it with a fresh
// install directory that holds the adapter configuration given (the default
// when it is empty).
func nativeTransportInstall(t testing.TB, kind ap.ManifestNativeIPCKind, address, adapterConfig string) (*apppkg.Package, string) {
	t.Helper()
	src, err := filepath.Abs(nativeTransportClinicDir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := apppkg.Load(context.Background(), apppkg.FromDir(src), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("the clinic package does not load: %+v", res.Findings)
	}
	pkg := res.Package

	rewrite := func(b *ap.CapabilityOperationBinding) *ap.CapabilityOperationBinding {
		if b == nil || b.MCPTool == nil {
			return b
		}
		nb := *b
		nb.Endpoint = nativeTransportEP
		m := *b.MCPTool
		nb.NativeMethod, nb.MCPTool = &m, nil
		return &nb
	}
	for id, c := range pkg.Capabilities {
		c.Operations.Read = rewrite(c.Operations.Read)
		c.Operations.Validate = rewrite(c.Operations.Validate)
		c.Operations.Prepare = rewrite(c.Operations.Prepare)
		c.Operations.Commit = rewrite(c.Operations.Commit)
		c.Operations.Status = rewrite(c.Operations.Status)
		for typ, et := range c.EntityTypes {
			et.Resolve = rewrite(et.Resolve)
			c.EntityTypes[typ] = et
		}
		pkg.Capabilities[id] = c
	}
	pkg.Manifest.Endpoints = map[string]ap.ManifestEndpoint{
		nativeTransportEP: {Value: ap.ManifestNativeEndpoint{
			Transport: "native", Bridge: ap.ManifestNativeEndpointBridgeGo, IPC: ap.ManifestNativeIPC{Kind: kind, Address: address},
			AdapterConfig: ap.SchemaRef(nativeTransportCfgRel),
		}},
	}
	if adapterConfig == "" {
		adapterConfig = nativeTransportCfgDoc
	}
	dir := filepath.Join(t.TempDir(), "package")
	p := filepath.Join(dir, filepath.FromSlash(nativeTransportCfgRel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(adapterConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return pkg, dir
}

// nativeTransportBuild builds the adapter of a package through the registry, the
// way serve and audit do (the transport registers itself from its own init).
func nativeTransportBuild(pkg *apppkg.Package, dir string) (adapters.AppAdapter, error) {
	src := transportsSource{dataDir: filepath.Dir(dir)}
	ad, _, err := src.build(context.Background(), nativeTransportApp, pkg,
		apppkg.Record{ApplicationID: nativeTransportApp, ManifestHash: "h1", State: apppkg.StateQuarantined, Path: filepath.Base(dir)})
	return ad, err
}

func nativeTransportScope(pkg *apppkg.Package) contracts.Scope {
	return contracts.Scope{DeploymentID: "dep_t", ApplicationID: nativeTransportApp, TenantID: "tnt_demo", WorkspaceID: "site_north",
		PrincipalID: "usr_reception_site_north", Roles: []string{"receptionist"}, CapabilityScopes: pkg.SortedCapabilityIDs(), SessionID: "ses_t"}
}

func TestNativeTransportIsRegisteredForTheNativeKind(t *testing.T) {
	if _, ok := transportsDefault.lookup(TransportKindNative); !ok {
		t.Fatalf("no transport is registered for %q (registered: %v)", TransportKindNative, transportsDefault.kinds())
	}
}

// The package becomes the adapter's mapping: the methods its capabilities name,
// with the operations' timeouts and renames, the endpoint's IPC and the key it
// refers to; the egress allowlist is the declared loopback port and nothing else.
func TestNativeTransportMapsThePackage(t *testing.T) {
	t.Setenv(nativeTransportKeyEnv, nativeTransportSecret)
	pkg, dir := nativeTransportInstall(t, ap.ManifestNativeIPCKindLoopbackTCP, "127.0.0.1:7001", "")
	plan, err := transportsPlanFor(pkg.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != TransportKindNative {
		t.Fatalf("plan = %+v", plan)
	}
	cfg, err := nativeTransportConfig(context.Background(),
		TransportRequest{ApplicationID: nativeTransportApp, Kind: TransportKindNative, Package: pkg, Dir: dir, Endpoints: plan.Endpoints}, nativeTransportLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Endpoints) != 1 {
		t.Fatalf("endpoints = %+v", cfg.Endpoints)
	}
	ep := cfg.Endpoints[0]
	if ep.ID != nativeTransportEP || ep.Network != nativetransport.NetworkTCP || ep.Address != "127.0.0.1:7001" || ep.KeyID != "k1" || !ep.Key.IsSet() ||
		len(ep.Provisioning) != 1 || ep.Provisioning[0] != "create_tenant" || ep.PeerCheck != nil {
		t.Errorf("endpoint = %+v", ep)
	}
	if strings.Contains(fmt.Sprintf("%+v", cfg), nativeTransportSecret) {
		t.Error("the key is printed with the configuration")
	}
	if cfg.Dialer == nil || cfg.ApplicationID != nativeTransportApp {
		t.Errorf("config = %+v", cfg)
	}
	if conn, err := cfg.Dialer.DialContext(context.Background(), "tcp", "127.0.0.1:7002"); err == nil {
		_ = conn.Close()
		t.Error("the dialer reaches a loopback port the package does not declare")
	}

	byID := map[string]nativetransport.Capability{}
	for _, c := range cfg.Capabilities {
		byID[c.ID] = c
	}
	book := byID["appointments.book"]
	if got := book.Operations[adapters.OpCommit]; got.Method != "appointments_book_commit" || got.Endpoint != nativeTransportEP || got.Timeout != 15*time.Second {
		t.Errorf("appointments.book commit = %+v", got)
	}
	if got := book.Operations[adapters.OpStatus]; got.Method != "appointments_book_status" || got.Timeout != 5*time.Second {
		t.Errorf("appointments.book status = %+v", got)
	}
	if _, has := book.Operations[adapters.OpRead]; has {
		t.Error("appointments.book maps a read, which the package does not")
	}
	if got := book.Resolve["patient"]; got.Method != "resolve_patient" {
		t.Errorf("the patient resolver of appointments.book = %+v", got)
	}
	for _, c := range cfg.Capabilities {
		if _, cancel := c.Operations[adapters.OpCancel]; cancel {
			t.Errorf("%s maps a cancel, which no package can", c.ID)
		}
	}
	if len(cfg.Capabilities) != len(pkg.Capabilities) {
		t.Errorf("%d capabilities mapped, the package declares %d", len(cfg.Capabilities), len(pkg.Capabilities))
	}
}

// The adapter the registry builds is the one serve and the audit call, over a
// Unix socket and over loopback TCP: what a package declares is what the audit
// finds, every call carries the identity and goes to the method the package maps
// (a resolution to the resolver of its entity type) and to nothing else, and the
// core closing the adapter closes the transport.
func TestNativeTransportServesThroughTheRegistry(t *testing.T) {
	t.Setenv(nativeTransportKeyEnv, nativeTransportSecret)
	for _, tc := range []struct {
		network string
		kind    ap.ManifestNativeIPCKind
	}{
		{nativetransport.NetworkUnix, ap.ManifestNativeIPCKindUnixSocket},
		{nativetransport.NetworkTCP, ap.ManifestNativeIPCKindLoopbackTCP},
	} {
		t.Run(tc.network, func(t *testing.T) {
			pkg, _ := nativeTransportInstall(t, tc.kind, "127.0.0.1:1", "")
			host := nativeTransportStartHost(t, pkg, tc.network)
			pkg, dir := nativeTransportInstall(t, tc.kind, host.Address, "")
			ad, err := nativeTransportBuild(pkg, dir)
			if err != nil {
				t.Fatal(err)
			}
			closed := false
			t.Cleanup(func() {
				if !closed {
					transportsClose(ad)
				}
			})
			ctx, scope := context.Background(), nativeTransportScope(pkg)

			d, err := ad.Describe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if d.Transport != "native" || len(d.Unmapped) != 0 || len(d.Capabilities) != len(pkg.Capabilities) {
				t.Errorf("description = %+v", d)
			}
			if h, err := ad.Health(ctx); err != nil || h.Status != adapters.HealthOK {
				t.Errorf("health = %+v, %v", h, err)
			}

			if _, err := ad.Read(ctx, scope, adapters.ReadRequest{CapabilityID: "patients.find", Fields: map[string]any{"query": "Rossi"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := ad.Read(ctx, scope, adapters.ReadRequest{CapabilityID: "appointments.book", Fields: map[string]any{"entity_type": "patient", "mention": "Rossi"}}); err != nil {
				t.Fatal(err)
			}
			key := act.IdempotencyKey("idem_clinic_test_key_0001")
			prepared := act.PreparedAction{
				PreparationID: "prep_1", CapabilityID: "appointments.book", State: act.PreparedActionStateCommitting, Summary: "Book", Effects: []act.Effect{}, RiskLevel: 1,
				DataVersions: []act.DataVersion{}, PreparedAt: "2026-10-09T08:00:00Z", ExpiresAt: "2026-10-09T08:10:00Z",
				InvalidationConditions: []act.PreparedActionInvalidationConditionsItem{act.PreparedActionInvalidationConditionsItemDataVersionChanged},
				Confirmation:           act.ConfirmationRequirement{Mode: act.ConfirmationRequirementModeNone, Bindings: []act.ConfirmationRequirementBindingsItem{}},
				IdempotencyKey:         key,
			}
			if _, err := ad.Commit(ctx, scope, adapters.CommitRequest{CapabilityID: "appointments.book", IdempotencyKey: key, Prepared: prepared}); err != nil {
				t.Fatal(err)
			}
			if _, err := ad.Status(ctx, scope, adapters.StatusRequest{CapabilityID: "appointments.book", IdempotencyKey: key}); err != nil {
				t.Fatal(err)
			}
			// Anything the package does not map is refused before it is sent.
			before := len(host.seen())
			if _, err := ad.Read(ctx, scope, adapters.ReadRequest{CapabilityID: "patients.find", Fields: map[string]any{"entity_type": "spaceship", "mention": "x"}}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Errorf("an entity type the capability does not resolve: %v", err)
			}
			if _, err := ad.Cancel(ctx, scope, adapters.CancelRequest{CapabilityID: "appointments.book", IdempotencyKey: key}); contracts.CodeOf(err) != contracts.ErrUnknownCapability {
				t.Errorf("a cancel, which no package maps: %v", err)
			}
			if len(host.seen()) != before {
				t.Error("a call the package does not map reached the host")
			}

			calls := host.seen()
			wantMethods := []string{"patients_find", "resolve_patient", "appointments_book_commit", "appointments_book_status"}
			var got []string
			for _, c := range calls {
				got = append(got, c.Method)
				if c.Identity.TenantID != scope.TenantID || c.Identity.PrincipalID != scope.PrincipalID || c.Identity.ApplicationID != nativeTransportApp {
					t.Errorf("%s carried the identity %+v", c.Method, c.Identity)
				}
			}
			if diff := cmp.Diff(wantMethods, got); diff != "" {
				t.Errorf("methods called (-want +got):\n%s", diff)
			}
			if string(calls[2].IdempotencyKey) != string(key) || calls[2].Prepared == nil || calls[1].Fields["mention"] != "Rossi" {
				t.Errorf("the commit was sent %+v, the resolver %+v", calls[2], calls[1].Fields)
			}

			// The core closes the adapter when it drops the package.
			closed = true
			transportsClose(ad)
			if _, err := ad.Read(ctx, scope, adapters.ReadRequest{CapabilityID: "patients.find", Fields: map[string]any{"query": "x"}}); contracts.CodeOf(err) != contracts.ErrUnavailable {
				t.Errorf("a call after the core closed the adapter: %v, want unavailable", err)
			}
		})
	}
}

// A package whose files or key the transport cannot trust, or whose endpoint it
// cannot reach safely, is a build error: the application is unavailable
// (open_failed) and nothing is called, never a weaker adapter, and the error does
// not repeat the key.
func TestNativeTransportRefusesWhatItCannotTrust(t *testing.T) {
	good := nativeTransportCfgDoc
	for _, tc := range []struct {
		name   string
		kind   ap.ManifestNativeIPCKind
		addr   string
		config string
		env    string
		mutate func(pkg *apppkg.Package)
	}{
		{name: "a named pipe", kind: ap.ManifestNativeIPCKindNamedPipe, addr: `\\.\pipe\clinic`},
		{name: "a key that is not set", env: "unset"},
		{name: "a key that is too short", env: "short"},
		{name: "a literal key in the configuration", config: "key:\n  id: k1\n  ref: " + nativeTransportSecret + "\n"},
		{name: "no key in the configuration", config: "server: " + nativeTransportEP + "\n"},
		{name: "an unknown member", config: good + "token: x\n"},
		{name: "a configuration for another server", config: "server: another\nkey:\n  id: k1\n  ref: env:" + nativeTransportKeyEnv + "\n"},
		{name: "a socket that is not absolute", kind: ap.ManifestNativeIPCKindUnixSocket, addr: "run/b.sock"},
		{name: "a malformed loopback address", addr: "127.0.0.1"},
		{name: "a method the capability does not name", mutate: func(p *apppkg.Package) {
			c := p.Capabilities["patients.find"]
			b := *c.Operations.Read
			b.NativeMethod = nil
			c.Operations.Read = &b
			p.Capabilities["patients.find"] = c
		}},
		{name: "a binding of another endpoint", mutate: func(p *apppkg.Package) {
			c := p.Capabilities["patients.find"]
			b := *c.Operations.Read
			b.Endpoint = "elsewhere"
			c.Operations.Read = &b
			p.Capabilities["patients.find"] = c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			switch tc.env {
			case "unset":
				t.Setenv(nativeTransportKeyEnv, "")
				_ = os.Unsetenv(nativeTransportKeyEnv)
			case "short":
				t.Setenv(nativeTransportKeyEnv, "short")
			default:
				t.Setenv(nativeTransportKeyEnv, nativeTransportSecret)
			}
			kind, addr := tc.kind, tc.addr
			if kind == "" {
				kind = ap.ManifestNativeIPCKindLoopbackTCP
			}
			if addr == "" {
				addr = "127.0.0.1:7001"
			}
			pkg, dir := nativeTransportInstall(t, kind, addr, tc.config)
			if tc.mutate != nil {
				tc.mutate(pkg)
			}
			ad, err := nativeTransportBuild(pkg, dir)
			if err == nil {
				transportsClose(ad)
				t.Fatal("the adapter was built")
			}
			if reason, _, ok := transportsUnavailableReason(err); !ok || reason != transportsReasonOpenFailed {
				t.Errorf("error = %v, want unavailable (open_failed)", err)
			}
			if strings.Contains(err.Error(), nativeTransportSecret) {
				t.Errorf("the error repeats the key: %v", err)
			}
		})
	}

	t.Run("a missing adapter configuration file", func(t *testing.T) {
		t.Setenv(nativeTransportKeyEnv, nativeTransportSecret)
		pkg, dir := nativeTransportInstall(t, ap.ManifestNativeIPCKindLoopbackTCP, "127.0.0.1:7001", "")
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(nativeTransportCfgRel))); err != nil {
			t.Fatal(err)
		}
		if _, err := nativeTransportBuild(pkg, dir); err == nil {
			t.Fatal("the adapter was built")
		}
	})

	t.Run("a configuration path that leaves the package", func(t *testing.T) {
		t.Setenv(nativeTransportKeyEnv, nativeTransportSecret)
		pkg, dir := nativeTransportInstall(t, ap.ManifestNativeIPCKindLoopbackTCP, "127.0.0.1:7001", "")
		v := pkg.Manifest.Endpoints[nativeTransportEP].Value.(ap.ManifestNativeEndpoint)
		v.AdapterConfig = "../../secret.yaml"
		pkg.Manifest.Endpoints[nativeTransportEP] = ap.ManifestEndpoint{Value: v}
		if _, err := nativeTransportBuild(pkg, dir); err == nil {
			t.Fatal("the adapter was built")
		}
	})
}

// A package that approves provisioning operations lets the audit reach them
// through Manage, and only those.
func TestNativeTransportExposesProvisioning(t *testing.T) {
	t.Setenv(nativeTransportKeyEnv, nativeTransportSecret)
	pkg, _ := nativeTransportInstall(t, ap.ManifestNativeIPCKindLoopbackTCP, "127.0.0.1:1", "")
	host := nativeTransportStartHost(t, pkg, nativetransport.NetworkTCP)
	pkg, dir := nativeTransportInstall(t, ap.ManifestNativeIPCKindLoopbackTCP, host.Address, "")
	inner, err := nativeTransportOpen(context.Background(), nativeTransportRequest(t, pkg, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { transportsClose(inner) })
	m, ok := inner.(interface {
		Manage(ctx context.Context, endpointID, operation string, args map[string]any) (map[string]any, error)
	})
	if !ok {
		t.Fatal("the transport exposes no management methods")
	}
	out, err := m.Manage(context.Background(), nativeTransportEP, "create_tenant", map[string]any{"name": "t"})
	if err != nil || out["tenant_id"] != "ten_disposable" {
		t.Fatalf("create_tenant: %v, %v", out, err)
	}
	if _, err := m.Manage(context.Background(), nativeTransportEP, "patients_find", nil); err == nil {
		t.Error("a capability method was reachable as a management operation")
	}
}

func nativeTransportRequest(t testing.TB, pkg *apppkg.Package, dir string) TransportRequest {
	t.Helper()
	plan, err := transportsPlanFor(pkg.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	return TransportRequest{ApplicationID: nativeTransportApp, Kind: TransportKindNative, Package: pkg, Dir: dir, Endpoints: plan.Endpoints}
}
