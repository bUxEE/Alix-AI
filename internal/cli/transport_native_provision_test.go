package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	nativetransport "github.com/buxee/alix-ai/adapters/native"
	"github.com/buxee/alix-ai/examples/reference-apps/ledger"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/integrationaudit/probes"
	"github.com/buxee/alix-ai/pkg/bridge"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// The ledger reference application (Go, the host bridge of pkg/bridge, native endpoint) is the
// application under test, in-process: its bridge listens on loopback, the transport reaches it
// through the real native adapter configured from the shipped package, and the provisioning
// operations are the ones the ledger declares with HandleManagement in test support mode.

const (
	nativeProvisionLedgerDir = "../../examples/reference-apps/ledger/package"
	nativeProvisionKeyEnv    = "LEDGER_BRIDGE_KEY"
	nativeProvisionKeyID     = "ledger-bridge-1"
	nativeProvisionSuffix    = "0123456789"
)

// nativeProvisionCountingListener counts the connections a bridge accepted. The adapter opens one
// connection per call, so an unchanged count proves that nothing reached the application.
type nativeProvisionCountingListener struct {
	net.Listener
	accepts atomic.Int64
}

func (l *nativeProvisionCountingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepts.Add(1)
	}
	return c, err
}

// nativeProvisionKey makes a throwaway application key for the test and puts it where the adapter
// configuration of the package says AlixAI reads it (env:LEDGER_BRIDGE_KEY).
func nativeProvisionKey(t testing.TB) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	key := []byte(hex.EncodeToString(b))
	t.Setenv(nativeProvisionKeyEnv, string(key))
	return key
}

// nativeProvisionLedgerBridge starts a ledger in test support mode with its bridge.
func nativeProvisionLedgerBridge(t testing.TB) (*ledger.Ledger, *bridge.Server) {
	t.Helper()
	app := ledger.New(ledger.Config{TestSupport: true, Clock: clock.NewFake(time.Date(2026, 10, 3, 9, 30, 0, 0, time.UTC)).Now})
	srv, err := app.NewBridge(ledger.BridgeConfig{Keys: map[string][]byte{nativeProvisionKeyID: nativeProvisionKey(t)}})
	if err != nil {
		t.Fatal(err)
	}
	return app, srv
}

// nativeProvisionFakeBridge is a bridge of pkg/bridge with only the handlers a test gives it, for
// the cases the ledger does not produce (a lost reply, the arguments as they were sent).
func nativeProvisionFakeBridge(t testing.TB, handlers map[string]bridge.ManageHandler) *bridge.Server {
	t.Helper()
	srv, err := bridge.New(bridge.Config{
		ApplicationID: ledger.ApplicationID, EndpointID: ledger.EndpointID, ApplicationVersion: "1.0.0",
		Keys: map[string][]byte{nativeProvisionKeyID: nativeProvisionKey(t)}, HandshakeTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, h := range handlers {
		if err := srv.HandleManagement(name, h); err != nil {
			t.Fatal(err)
		}
	}
	return srv
}

// nativeProvisionWorld is the ledger package audited against a running bridge: the input the audit
// hands its checks, and the raw adapter underneath it.
type nativeProvisionWorld struct {
	pkg *apppkg.Package
	dir string
	in  *integrationaudit.Input
	ln  *nativeProvisionCountingListener
	raw *nativetransport.Adapter
}

// nativeProvisionWorldFor serves srv on loopback, points the package's native endpoint at it and
// builds the adapter through the process's transport registry, exactly as audit run holds it.
// dirOf, when set, names the install directory the transport reads from (a copy of the package);
// edit changes the loaded package before the adapter is built.
func nativeProvisionWorldFor(t testing.TB, srv *bridge.Server, dirOf func(dir string) string, edit ...func(pkg *apppkg.Package)) *nativeProvisionWorld {
	t.Helper()
	dir, err := filepath.Abs(nativeProvisionLedgerDir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := apppkg.Load(context.Background(), apppkg.FromDir(dir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("the ledger package does not load: %+v", res.Findings)
	}
	pkg := res.Package

	l, err := bridge.ListenLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	ln := &nativeProvisionCountingListener{Listener: l}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })

	v, ok := pkg.Manifest.Endpoints[ledger.EndpointID].Value.(ap.ManifestNativeEndpoint)
	if !ok {
		t.Fatalf("the ledger package has no native endpoint %q", ledger.EndpointID)
	}
	v.IPC.Address = ln.Addr().String()
	pkg.Manifest.Endpoints[ledger.EndpointID] = ap.ManifestEndpoint{Value: v}
	for _, e := range edit {
		e(pkg)
	}

	use := dir
	if dirOf != nil {
		use = dirOf(dir)
	}
	dataDir := filepath.Dir(use)
	was := nativeProvisionDataDir
	nativeProvisionDataDir = func() string { return dataDir }
	t.Cleanup(func() { nativeProvisionDataDir = was })

	rec := apppkg.Record{
		ApplicationID: string(pkg.Manifest.Application.ID), ManifestHash: "h1", State: apppkg.StateQuarantined, Path: filepath.Base(use),
	}
	ctx := context.Background()
	ad, _, err := transportsSource{dataDir: dataDir}.build(ctx, rec.ApplicationID, pkg, rec)
	if err != nil {
		t.Fatalf("the adapter of the package: %v", err)
	}
	t.Cleanup(func() { transportsClose(ad) })
	closing, ok := ad.(transportsClosing)
	if !ok {
		t.Fatalf("the transport built %T, want a transportsClosing", ad)
	}
	raw, ok := closing.inner.(*nativetransport.Adapter)
	if !ok {
		t.Fatalf("the transport built %T underneath the wrapper, want the native adapter", closing.inner)
	}

	in := &integrationaudit.Input{
		Package: pkg, Record: rec, CoreVersion: apppkg.DevCoreVersion, Authenticity: apppkg.AuthVerified,
		Adapter: ad, Clock: clock.Real{},
		Scope: contracts.Scope{
			DeploymentID: "dep_audit", ApplicationID: rec.ApplicationID, TenantID: "alixai-audit", WorkspaceID: "alixai-audit",
			PrincipalID: "alixai-audit", Roles: []string{"auditor"}, CapabilityScopes: []string{"audit.inspect"}, SessionID: "audit-test",
		},
	}
	// what the runner asks of the adapter before any check runs
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if d, err := ad.Describe(cctx); err == nil {
		in.Description = &d
	}
	if h, err := ad.Health(cctx); err == nil {
		in.Health = &h
	}
	if i, err := ad.Inspect(cctx, in.Scope, adapters.InspectRequest{}); err == nil {
		in.Inspection = &i
	}
	return &nativeProvisionWorld{pkg: pkg, dir: use, in: in, ln: ln, raw: raw}
}

// nativeProvisionLedgerWorld is the ledger itself behind the transport.
func nativeProvisionLedgerWorld(t testing.TB, dirOf func(dir string) string) (*ledger.Ledger, *nativeProvisionWorld) {
	t.Helper()
	app, srv := nativeProvisionLedgerBridge(t)
	return app, nativeProvisionWorldFor(t, srv, dirOf)
}

func nativeProvisionCtx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func nativeProvisionMustSource(t testing.TB, in *integrationaudit.Input) probes.Provisioner {
	t.Helper()
	p, err := nativeProvisionSource(nativeProvisionCtx(t), in)
	if err != nil {
		t.Fatalf("the provisioning source: %v", err)
	}
	if p == nil {
		t.Fatal("the provisioning source offers no provisioner")
	}
	return p
}

// nativeProvisionRun runs the checks of the given sections (the audit's runtime probes) with the
// options and returns their outcomes by section.
func nativeProvisionRun(t testing.TB, in *integrationaudit.Input, opts probes.Options, sections ...int) map[int]*integrationaudit.Outcome {
	t.Helper()
	if opts.ScratchDir == "" {
		opts.ScratchDir = t.TempDir()
	}
	want := map[int]bool{}
	for _, s := range sections {
		want[s] = true
	}
	out := map[int]*integrationaudit.Outcome{}
	for _, c := range probes.Checks(opts) {
		if !want[c.Section()] {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		o, err := c.Run(ctx, in)
		cancel()
		if err != nil {
			t.Fatalf("section %d (%s) did not run: %v", c.Section(), c.ID(), err)
		}
		out[c.Section()] = o
	}
	return out
}

// nativeProvisionDescribe prints an outcome the way a failing test needs to read it.
func nativeProvisionDescribe(o *integrationaudit.Outcome) string {
	var b strings.Builder
	for _, v := range o.Verdicts {
		fmt.Fprintf(&b, "  verdict %s %s: %s\n", v.CapabilityID, v.Status, v.Reason)
	}
	for _, u := range o.Unverified {
		fmt.Fprintf(&b, "  unverified %s [%s]: %s\n", u.Subject, u.Capability, u.Reason)
	}
	for _, f := range o.Findings {
		fmt.Fprintf(&b, "  finding %s %s [%s]: %s\n", f.Severity, f.CheckID, f.Capability, f.Message)
	}
	covered := append([]string(nil), o.Covered...)
	sort.Strings(covered)
	fmt.Fprintf(&b, "  covered: %v\n", covered)
	return b.String()
}

func nativeProvisionReasons(o *integrationaudit.Outcome) []string {
	var out []string
	for _, v := range o.Verdicts {
		out = append(out, v.Reason)
	}
	for _, u := range o.Unverified {
		out = append(out, u.Reason)
	}
	for _, f := range o.Findings {
		out = append(out, f.Message)
	}
	return out
}

func nativeProvisionHas(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// nativeProvisionTenantOf is a tenant of the ledger suite, with the identifiers the audit would
// generate (tnt_<ref>_<suffix>, ws_<ref>_<suffix>, usr_<ref>_<suffix>, "-" as "_"): an organisation
// of two teams.
func nativeProvisionTenantOf(suffix string) probes.Tenant {
	north, south := "ws_north_a_"+suffix, "ws_south_a_"+suffix
	p := func(ref, ws, role string) probes.Principal {
		return probes.Principal{Ref: ref, ID: "usr_" + strings.ReplaceAll(ref, "-", "_") + "_" + suffix, WorkspaceID: ws, Roles: []string{role}}
	}
	return probes.Tenant{
		ID: "tnt_org_a_" + suffix, Workspaces: []string{north, south},
		Principals: []probes.Principal{
			p("acct-a-north", north, "accountant"), p("mgr-a-north", north, "manager"),
			p("view-a-north", north, "viewer"), p("acct-a-south", south, "accountant"),
		},
	}
}

// nativeProvisionSeedOf seeds the tenant of nativeProvisionTenantOf from the package's seed data
// file. The quote links to the customer listed before it, as the ledger requires.
func nativeProvisionSeedOf(suffix string) probes.Seed {
	north, south := "ws_north_a_"+suffix, "ws_south_a_"+suffix
	return probes.Seed{
		TenantID: "tnt_org_a_" + suffix, DataFile: "tests/seed/ledger.json",
		Records: []probes.Record{
			{Ref: "cust-a-north", EntityType: "customer", WorkspaceID: north},
			{Ref: "cust-a-south", EntityType: "customer", WorkspaceID: south},
			{Ref: "quote-a-north", EntityType: "quote", WorkspaceID: north},
			{Ref: "inv-a-north", EntityType: "invoice", WorkspaceID: north},
			{Ref: "inv-a-south", EntityType: "invoice", WorkspaceID: south},
		},
	}
}

// nativeProvisionGone asks the ledger itself, through the raw adapter, whether a tenant is still
// held: a cleanup of a tenant that is gone answers existed false with nothing remaining.
func nativeProvisionGone(t testing.TB, w *nativeProvisionWorld, id string) {
	t.Helper()
	obj, err := w.raw.Manage(nativeProvisionCtx(t), ledger.EndpointID, "cleanup", map[string]any{"tenant_id": id})
	if err != nil {
		t.Errorf("the application's own answer about %s: %v", id, err)
		return
	}
	if obj["status"] != "ok" || obj["existed"] != false {
		t.Errorf("tenant %s is still held by the application: %v", id, obj)
	}
	if rem, _ := obj["remaining"].(map[string]any); len(rem) == 0 {
		t.Errorf("the application did not state what remains of %s: %v", id, obj)
	} else {
		for kind, n := range rem {
			if n != float64(0) {
				t.Errorf("tenant %s left %v %s behind", id, n, kind)
			}
		}
	}
}

// nativeProvisionSpy records what the provisioners the audit builds were asked.
type nativeProvisionSpy struct {
	mu      sync.Mutex
	created []string
	seeded  map[string]int // tenant -> records the application identified
	cleaned map[string]int // tenant -> cleanups that reported nothing left
}

type nativeProvisionSpied struct {
	probes.Provisioner
	spy *nativeProvisionSpy
}

func (s nativeProvisionSpied) CreateTenant(ctx context.Context, t probes.Tenant) error {
	err := s.Provisioner.CreateTenant(ctx, t)
	if err == nil {
		s.spy.mu.Lock()
		s.spy.created = append(s.spy.created, t.ID)
		s.spy.mu.Unlock()
	}
	return err
}

func (s nativeProvisionSpied) Seed(ctx context.Context, x probes.Seed) (probes.SeedResult, error) {
	res, err := s.Provisioner.Seed(ctx, x)
	if err == nil {
		s.spy.mu.Lock()
		s.spy.seeded[x.TenantID] += len(res.Entities)
		s.spy.mu.Unlock()
	}
	return res, err
}

func (s nativeProvisionSpied) Cleanup(ctx context.Context, id string) (probes.CleanupReport, error) {
	rep, err := s.Provisioner.Cleanup(ctx, id)
	if err == nil && rep.Left() == 0 {
		s.spy.mu.Lock()
		s.spy.cleaned[id]++
		s.spy.mu.Unlock()
	}
	return rep, err
}

// source is a probes.ProvisionerSource that is the native one, watched.
func (s *nativeProvisionSpy) source(ctx context.Context, in *integrationaudit.Input) (probes.Provisioner, error) {
	p, err := nativeProvisionSource(ctx, in)
	if p == nil || err != nil {
		return p, err
	}
	return nativeProvisionSpied{Provisioner: p, spy: s}, nil
}

// ---- 1. the real ledger application ------------------------------------------

// The audit's runtime probes (sections 6, 8 and 10) run against the real ledger application
// through the provisioner this file registers: the disposable organisations of the package's suite
// are created, seeded and removed through the ledger's management operations over the native
// bridge, the probes run through the capability methods, and nothing is left behind (the
// application itself says so). The checks use the default source in the second half, so this
// also proves the init registration.
func TestNativeProvisionerRunsFixturesAgainstTheBridgeApp(t *testing.T) {
	_, w := nativeProvisionLedgerWorld(t, nil)
	spy := &nativeProvisionSpy{seeded: map[string]int{}, cleaned: map[string]int{}}

	sections := []int{integrationaudit.SectionScope, integrationaudit.SectionSafeExecution, integrationaudit.SectionRuntimeSmoke}
	outs := nativeProvisionRun(t, w.in, probes.Options{Source: spy.source}, sections...)

	if len(spy.created) == 0 {
		t.Fatal("the application was never asked to create a disposable tenant")
	}
	for _, id := range spy.created {
		if spy.seeded[id] == 0 {
			t.Errorf("tenant %s was created and never seeded", id)
		}
		if spy.cleaned[id] == 0 {
			t.Errorf("tenant %s was created and its cleanup did not report nothing left", id)
		}
		nativeProvisionGone(t, w, id) // the application's own account, not the audit's
	}

	// what the audit could not probe must not be for want of a disposable tenant
	notProvisioning := []string{"provisioner", "creates a disposable tenant", "create_tenant for", "seed for fixture"}
	for s, o := range outs {
		for _, reason := range nativeProvisionReasons(o) {
			for _, bad := range notProvisioning {
				if strings.Contains(reason, bad) {
					t.Errorf("section %d still says the disposable tenant could not be reached: %q", s, reason)
				}
			}
		}
		for _, f := range o.Findings {
			if f.CheckID == "probes.tenant_left_behind" {
				t.Errorf("section %d: a disposable tenant was left behind: %s", s, f.Message)
			}
		}
		passed := 0
		for _, p := range o.Probes {
			if p.Passed {
				passed++
			}
		}
		if passed == 0 {
			t.Errorf("section %d ran no probe that passed:\n%s", s, nativeProvisionDescribe(o))
		}
	}
	if !nativeProvisionHas(outs[integrationaudit.SectionScope].Covered, "invoices.find") {
		t.Errorf("section 6 did not probe invoices.find:\n%s", nativeProvisionDescribe(outs[integrationaudit.SectionScope]))
	}

	// the default source (the one the init function registered) provisions the same application
	before := w.ln.accepts.Load()
	smoke := nativeProvisionRun(t, w.in, probes.Options{}, integrationaudit.SectionRuntimeSmoke)[integrationaudit.SectionRuntimeSmoke]
	if w.ln.accepts.Load() == before {
		t.Error("the default source never reached the application: the init registration is missing")
	}
	for _, reason := range nativeProvisionReasons(smoke) {
		for _, bad := range notProvisioning {
			if strings.Contains(reason, bad) {
				t.Errorf("the default source did not provision: %q", reason)
			}
		}
	}
}

// ---- 2. only the audit's own tenants -------------------------------------------

// A provisioner acts only on the tenants it asked for. A second provisioner of the same audit (the
// audit builds one for every check) is refused a tenant the first one created, and so is a tenant
// nobody created and the application's own demonstration organisation, and the application is not
// even asked: its refusal would carry another reason, and no connection is accepted. A create the
// application refused (the tenant exists) is not adopted either, so the cleanup that follows can
// never remove somebody else's tenant.
func TestNativeProvisionerRefusesTenantsTheAuditDidNotCreate(t *testing.T) {
	app, w := nativeProvisionLedgerWorld(t, nil)
	if err := app.SeedDemo(); err != nil {
		t.Fatal(err)
	}
	ctx := nativeProvisionCtx(t)

	mine, other := nativeProvisionMustSource(t, w.in), nativeProvisionMustSource(t, w.in)
	tenant := nativeProvisionTenantOf("aaaaaaaaaa")
	if err := mine.CreateTenant(ctx, tenant); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if _, err := mine.Seed(ctx, nativeProvisionSeedOf("aaaaaaaaaa")); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	accepted := w.ln.accepts.Load()
	for name, call := range map[string]func() error{
		"seed of a tenant another provisioner created": func() error {
			_, err := other.Seed(ctx, nativeProvisionSeedOf("aaaaaaaaaa"))
			return err
		},
		"cleanup of a tenant another provisioner created": func() error { _, err := other.Cleanup(ctx, tenant.ID); return err },
		"seed of a tenant nobody created": func() error {
			_, err := mine.Seed(ctx, nativeProvisionSeedOf("bbbbbbbbbb"))
			return err
		},
		"cleanup of a tenant nobody created":                    func() error { _, err := mine.Cleanup(ctx, "tnt_org_a_bbbbbbbbbb"); return err },
		"cleanup of the application's own demonstration tenant": func() error { _, err := mine.Cleanup(ctx, ledger.DemoTenant); return err },
		"seed of the application's own demonstration tenant": func() error {
			s := nativeProvisionSeedOf("aaaaaaaaaa")
			s.TenantID = ledger.DemoTenant
			_, err := mine.Seed(ctx, s)
			return err
		},
	} {
		err := call()
		if contracts.CodeOf(err) != contracts.ErrPermissionDenied || nativeProvisionReasonOf(err) != nativeProvisionReasonNotOurs {
			t.Errorf("%s: %v (code %q, reason %q), want permission_denied by this bridge", name, err, contracts.CodeOf(err), nativeProvisionReasonOf(err))
		}
	}
	if n := w.ln.accepts.Load(); n != accepted {
		t.Errorf("a refused call reached the application: %d connections were accepted for them", n-accepted)
	}

	// a tenant the other provisioner owns, which this one asks to create: the application refuses
	// (it exists), and that tenant stays the other's
	foreign := nativeProvisionTenantOf("cccccccccc")
	if err := other.CreateTenant(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := mine.CreateTenant(ctx, foreign); contracts.CodeOf(err) != contracts.ErrConflict || nativeProvisionReasonOf(err) != nativeProvisionReasonRejected {
		t.Fatalf("a second create of an existing tenant: %v, want the application's conflict", err)
	}
	// and so is the application's own organisation, which a create cannot take over
	demo := nativeProvisionTenantOf("dddddddddd")
	demo.ID = ledger.DemoTenant
	if err := mine.CreateTenant(ctx, demo); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("a create of the demonstration tenant: %v, want conflict", err)
	}
	accepted = w.ln.accepts.Load()
	for _, id := range []string{foreign.ID, ledger.DemoTenant} {
		if _, err := mine.Cleanup(ctx, id); contracts.CodeOf(err) != contracts.ErrPermissionDenied || nativeProvisionReasonOf(err) != nativeProvisionReasonNotOurs {
			t.Errorf("cleanup of %s, whose creation the application refused: %v, want permission_denied by this bridge", id, err)
		}
	}
	if n := w.ln.accepts.Load(); n != accepted {
		t.Error("the cleanup of a tenant this provisioner did not create reached the application")
	}
	if rep, err := other.Cleanup(ctx, foreign.ID); err != nil || !rep.Existed || rep.Left() != 0 {
		t.Errorf("the owner's cleanup of the tenant = %+v, %v: it must still exist and be removed", rep, err)
	}
	// the application still holds its own organisation: it refuses to remove what is not disposable
	obj, err := w.raw.Manage(ctx, ledger.EndpointID, "cleanup", map[string]any{"tenant_id": ledger.DemoTenant})
	if err != nil || obj["status"] != "rejected" {
		t.Errorf("the demonstration tenant is not held any more: %v, %v", obj, err)
	}
	if rep, err := mine.Cleanup(ctx, tenant.ID); err != nil || !rep.Existed || rep.Left() != 0 {
		t.Errorf("the owner's cleanup = %+v, %v", rep, err)
	}
}

// ---- 3. every method can be called again ---------------------------------------

func TestNativeProvisionerCleanupIsIdempotent(t *testing.T) {
	_, w := nativeProvisionLedgerWorld(t, nil)
	ctx := nativeProvisionCtx(t)
	p := nativeProvisionMustSource(t, w.in)
	tenant := nativeProvisionTenantOf(nativeProvisionSuffix)

	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	// a second create of a tenant that exists is a typed conflict and harms nothing
	if err := p.CreateTenant(ctx, tenant); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("a second CreateTenant: %v, want conflict", err)
	}
	seed, err := p.Seed(ctx, nativeProvisionSeedOf(nativeProvisionSuffix))
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	for _, r := range nativeProvisionSeedOf(nativeProvisionSuffix).Records {
		if seed.Entities[r.Ref] == "" {
			t.Errorf("no identifier for %s: %v", r.Ref, seed.Entities)
		}
	}

	first, err := p.Cleanup(ctx, tenant.ID)
	if err != nil || !first.Existed || first.Left() != 0 {
		t.Fatalf("the first cleanup = %+v, %v: want Existed true and nothing left", first, err)
	}
	for i := 2; i <= 3; i++ {
		again, err := p.Cleanup(ctx, tenant.ID)
		if err != nil || again.Existed || again.Left() != 0 {
			t.Errorf("cleanup number %d = %+v, %v: want Existed false and nothing left", i, again, err)
		}
	}
	nativeProvisionGone(t, w, tenant.ID)

	// nothing is seeded into a tenant that is gone, and nothing is sent to try
	accepted := w.ln.accepts.Load()
	if _, err := p.Seed(ctx, nativeProvisionSeedOf(nativeProvisionSuffix)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("Seed after Cleanup: %v, want permission_denied", err)
	}
	if w.ln.accepts.Load() != accepted {
		t.Error("a seed of a removed tenant reached the application")
	}
	// the same identifiers can be created again once the tenant is gone
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Errorf("CreateTenant after Cleanup: %v", err)
	}
	if rep, err := p.Cleanup(ctx, tenant.ID); err != nil || !rep.Existed || rep.Left() != 0 {
		t.Errorf("the last cleanup = %+v, %v", rep, err)
	}
}

// ---- 4. what the bridge sends, and what it will not trust or do ----------------

// nativeProvisionCopyPackage copies the ledger package to <temp>/package and lets edit change a
// file of it (by its package-relative path) on the way. Only the transport reads the copy; the
// package the audit loaded is the untouched original.
func nativeProvisionCopyPackage(t testing.TB, edit func(rel string, b []byte) []byte) func(dir string) string {
	return func(dir string) string {
		t.Helper()
		dst := filepath.Join(t.TempDir(), "package")
		err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			target := filepath.Join(dst, rel)
			if d.IsDir() {
				return os.MkdirAll(target, 0o700)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, edit(filepath.ToSlash(rel), b), 0o600)
		})
		if err != nil {
			t.Fatal(err)
		}
		return dst
	}
}

// The bridge sends the audit's own identifiers and the seed data file's members in the audit's
// order, and nothing the audit owns can be set by the file: a record's workspace and principal are
// the generated identifiers and the logical references of the file never reach the application.
// Numbers keep their value, and the application answers as it likes: a reply for another record
// than the ones asked for is refused.
func TestNativeProvisionerSendsTheSeedDataWithTheAuditsIdentifiers(t *testing.T) {
	var mu sync.Mutex
	var created, seeded []map[string]any
	var answer func(args map[string]any) map[string]any
	srv := nativeProvisionFakeBridge(t, map[string]bridge.ManageHandler{
		"create_tenant": func(_ context.Context, args map[string]any) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			created = append(created, args)
			return map[string]any{"status": "ok", "tenant_id": args["tenant_id"]}, nil
		},
		"seed": func(_ context.Context, args map[string]any) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			seeded = append(seeded, args)
			return answer(args), nil
		},
		"cleanup": func(_ context.Context, args map[string]any) (any, error) {
			return map[string]any{"status": "ok", "tenant_id": args["tenant_id"], "existed": true, "remaining": map[string]any{"records": 0}}, nil
		},
	})
	seedFile := `{"records":[
	  {"ref":"cust-a-north","entity_type":"customer","workspace":"north-a","workspace_id":"hijack","tenant_id":"hijack","name":"Acme Srl","currency":"EUR","principal":"acct-a-north"},
	  {"ref":"inv-a-north","entity_type":"invoice","workspace":"north-a","customer":"cust-a-north","amount_minor":125050,"paid_minor":0,"number":"INV-0001"}]}`
	w := nativeProvisionWorldFor(t, srv, nativeProvisionCopyPackage(t, func(rel string, b []byte) []byte {
		if rel == "tests/seed/ledger.json" {
			return []byte(seedFile)
		}
		return b
	}))
	ctx := nativeProvisionCtx(t)
	p := nativeProvisionMustSource(t, w.in)

	tenant := nativeProvisionTenantOf(nativeProvisionSuffix)
	tenant.Principals[1].Service = true
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	north, south := "ws_north_a_"+nativeProvisionSuffix, "ws_south_a_"+nativeProvisionSuffix
	wantCreate := map[string]any{
		"tenant_id":  tenant.ID,
		"workspaces": []any{map[string]any{"workspace_id": north}, map[string]any{"workspace_id": south}},
		"principals": []any{
			map[string]any{"principal_id": "usr_acct_a_north_" + nativeProvisionSuffix, "workspace_id": north, "roles": []any{"accountant"}},
			map[string]any{"principal_id": "usr_mgr_a_north_" + nativeProvisionSuffix, "workspace_id": north, "roles": []any{"manager"}, "service": true},
			map[string]any{"principal_id": "usr_view_a_north_" + nativeProvisionSuffix, "workspace_id": north, "roles": []any{"viewer"}},
			map[string]any{"principal_id": "usr_acct_a_south_" + nativeProvisionSuffix, "workspace_id": south, "roles": []any{"accountant"}},
		},
	}
	if len(created) != 1 {
		t.Fatalf("%d creates", len(created))
	}
	if diff := cmp.Diff(wantCreate, created[0]); diff != "" {
		t.Errorf("create_tenant arguments (-want +got):\n%s", diff)
	}

	seed := nativeProvisionSeedOf(nativeProvisionSuffix)
	seed.Records = []probes.Record{seed.Records[0], seed.Records[3]} // cust-a-north, inv-a-north
	answer = func(args map[string]any) map[string]any {
		var recs []any
		for _, r := range args["records"].([]any) {
			ref := r.(map[string]any)["ref"]
			recs = append(recs, map[string]any{"ref": ref, "entity_id": "ent_" + fmt.Sprint(ref)})
		}
		return map[string]any{"status": "ok", "tenant_id": args["tenant_id"], "records": recs}
	}
	res, err := p.Seed(ctx, seed)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(map[string]string{"cust-a-north": "ent_cust-a-north", "inv-a-north": "ent_inv-a-north"}, res.Entities); diff != "" {
		t.Errorf("entities (-want +got):\n%s", diff)
	}
	wantRecords := []any{
		map[string]any{
			"ref": "cust-a-north", "entity_type": "customer", "workspace_id": north, "principal_id": "usr_acct_a_north_" + nativeProvisionSuffix,
			"name": "Acme Srl", "currency": "EUR",
		},
		map[string]any{
			"ref": "inv-a-north", "entity_type": "invoice", "workspace_id": north, "customer": "cust-a-north",
			"amount_minor": float64(125050), "paid_minor": float64(0), "number": "INV-0001",
		},
	}
	if len(seeded) != 1 || seeded[0]["tenant_id"] != tenant.ID {
		t.Fatalf("seed calls: %v", seeded)
	}
	if diff := cmp.Diff(wantRecords, seeded[0]["records"]); diff != "" {
		t.Errorf("records sent, in the audit's order (-want +got):\n%s", diff)
	}

	// a reply that identifies a record nobody asked for, or leaves one out, is not a seed
	for name, wrong := range map[string]func(args map[string]any) map[string]any{
		"a record that was not asked for": func(args map[string]any) map[string]any {
			return map[string]any{"status": "ok", "tenant_id": args["tenant_id"], "records": []any{map[string]any{"ref": "stranger", "entity_id": "e1"}}}
		},
		"a record left out": func(args map[string]any) map[string]any {
			return map[string]any{"status": "ok", "tenant_id": args["tenant_id"], "records": []any{map[string]any{"ref": "cust-a-north", "entity_id": "e1"}}}
		},
		"another tenant": func(map[string]any) map[string]any {
			return map[string]any{"status": "ok", "tenant_id": "tnt_other", "records": []any{}}
		},
	} {
		answer = wrong
		if _, err := p.Seed(ctx, seed); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("a seed reply with %s: %v, want schema_invalid", name, err)
		}
	}
}

// Whatever cannot be sent safely fails before anything is sent.
func TestNativeProvisionerFailsASeedClosedBeforeSendingAnything(t *testing.T) {
	_, w := nativeProvisionLedgerWorld(t, nil)
	ctx := nativeProvisionCtx(t)
	p := nativeProvisionMustSource(t, w.in)
	tenant := nativeProvisionTenantOf(nativeProvisionSuffix)
	if err := p.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	record := func(mod func(*probes.Seed)) probes.Seed {
		s := nativeProvisionSeedOf(nativeProvisionSuffix)
		mod(&s)
		return s
	}
	for name, seed := range map[string]probes.Seed{
		"a path that climbs out of the package":    record(func(s *probes.Seed) { s.DataFile = "tests/../../package.json" }),
		"a path outside tests/":                    record(func(s *probes.Seed) { s.DataFile = "alixai.yaml" }),
		"a file that is not there":                 record(func(s *probes.Seed) { s.DataFile = "tests/seed/none.json" }),
		"a workspace of another tenant":            record(func(s *probes.Seed) { s.Records[0].WorkspaceID = "ws_north_a_ffffffffff" }),
		"a record listed twice":                    record(func(s *probes.Seed) { s.Records = append(s.Records, s.Records[0]) }),
		"an entity type the data file contradicts": record(func(s *probes.Seed) { s.Records[0].EntityType = "invoice" }),
		"no records":                               record(func(s *probes.Seed) { s.Records = nil }),
	} {
		accepted := w.ln.accepts.Load()
		_, err := p.Seed(ctx, seed)
		if contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: %v, want validation_failed", name, err)
		}
		if w.ln.accepts.Load() != accepted {
			t.Errorf("%s: the seed reached the application", name)
		}
	}
}

// A create whose reply is lost may have been applied: the tenant is the audit's to clean up and
// not to seed, and the application is asked to remove it.
func TestNativeProvisionerCleansUpATenantWhoseCreateReplyWasLost(t *testing.T) {
	var mu sync.Mutex
	held := map[string]bool{}
	srv := nativeProvisionFakeBridge(t, map[string]bridge.ManageHandler{
		"create_tenant": func(_ context.Context, args map[string]any) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			held[fmt.Sprint(args["tenant_id"])] = true
			return nil, errors.New("the reply is lost") // applied, and then the host fails
		},
		"seed": func(context.Context, map[string]any) (any, error) { return nil, errors.New("must not be asked") },
		"cleanup": func(_ context.Context, args map[string]any) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			id := fmt.Sprint(args["tenant_id"])
			existed := held[id]
			delete(held, id)
			return map[string]any{"status": "ok", "tenant_id": id, "existed": existed, "remaining": map[string]any{}}, nil
		},
	})
	w := nativeProvisionWorldFor(t, srv, nil)
	ctx := nativeProvisionCtx(t)
	p := nativeProvisionMustSource(t, w.in)
	tenant := nativeProvisionTenantOf(nativeProvisionSuffix)

	if err := p.CreateTenant(ctx, tenant); err == nil {
		t.Fatal("a create whose reply was lost succeeded")
	}
	accepted := w.ln.accepts.Load()
	if _, err := p.Seed(ctx, nativeProvisionSeedOf(nativeProvisionSuffix)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("a seed of a tenant whose creation is not certain: %v, want permission_denied", err)
	}
	if w.ln.accepts.Load() != accepted {
		t.Error("that seed reached the application")
	}
	rep, err := p.Cleanup(ctx, tenant.ID)
	if err != nil || !rep.Existed || rep.Left() != 0 {
		t.Errorf("the cleanup of the possibly created tenant = %+v, %v: want it removed", rep, err)
	}
	if len(held) != 0 {
		t.Errorf("the application still holds %v", held)
	}
}

// ---- 5. when there is no bridge ---------------------------------------------------

// A package whose test suites name no native provisioning methods leaves the probes exactly as they
// were before this bridge: no provisioner, every capability not probed, nothing sent to try.
func TestNativeProvisionerLeavesAPackageWithoutProvisioningUnverified(t *testing.T) {
	_, w := nativeProvisionLedgerWorld(t, nil)
	for i := range w.pkg.Tests {
		pv := &w.pkg.Tests[i].Fixtures.Provisioning
		pv.CreateTenant.NativeMethod, pv.Seed.NativeMethod, pv.Cleanup.NativeMethod = nil, nil, nil
	}
	if p, err := nativeProvisionSource(nativeProvisionCtx(t), w.in); p != nil || err != nil {
		t.Fatalf("a package that names no provisioning methods gave a provisioner: %v, %v", p, err)
	}
	accepted := w.ln.accepts.Load()
	sections := []int{integrationaudit.SectionScope, integrationaudit.SectionSafeExecution, integrationaudit.SectionRuntimeSmoke}
	got := nativeProvisionRun(t, w.in, probes.Options{}, sections...)
	none := func(context.Context, *integrationaudit.Input) (probes.Provisioner, error) { return nil, nil }
	was := nativeProvisionRun(t, w.in, probes.Options{Source: none}, sections...)
	if w.ln.accepts.Load() != accepted {
		t.Error("the probes called an application they cannot give a disposable tenant")
	}
	for _, s := range sections {
		if diff := cmp.Diff(nativeProvisionDescribe(was[s]), nativeProvisionDescribe(got[s])); diff != "" {
			t.Errorf("section %d differs from the audit of an application with no provisioner (-before +now):\n%s", s, diff)
		}
		if len(got[s].Covered) != 0 {
			t.Errorf("section %d covered %v without a disposable tenant", s, got[s].Covered)
		}
	}
}

// Methods the adapter configuration does not approve in management.provisioning are not
// provisioning methods: the source fails with a typed error, the probes record why, and the
// application is not asked.
func TestNativeProvisionerRefusesMethodsTheAdapterConfigurationDoesNotApprove(t *testing.T) {
	_, w := nativeProvisionLedgerWorld(t, nativeProvisionCopyPackage(t, func(rel string, b []byte) []byte {
		if rel == "schemas/adapters/ledger-bridge.yaml" {
			return []byte(strings.Replace(string(b), "    - \"seed\"\n", "", 1))
		}
		return b
	}))
	accepted := w.ln.accepts.Load()
	p, err := nativeProvisionSource(nativeProvisionCtx(t), w.in)
	if p != nil || contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("an unapproved provisioning method: %v, %v, want a validation_failed error", p, err)
	}
	o := nativeProvisionRun(t, w.in, probes.Options{}, integrationaudit.SectionSafeExecution)[integrationaudit.SectionSafeExecution]
	if len(o.Covered) != 0 {
		t.Errorf("probed %v without an approved provisioning method:\n%s", o.Covered, nativeProvisionDescribe(o))
	}
	if n := w.ln.accepts.Load(); n != accepted {
		t.Errorf("%d connections were made to an application whose provisioning is not approved", n-accepted)
	}
}

// An audit of another transport is never touched: the source answers nil, nil and no error, so
// that the MCP and OpenAPI audits keep their own provisioners.
func TestNativeProvisionerStaysSilentForOtherInputs(t *testing.T) {
	ctx := nativeProvisionCtx(t)
	for name, in := range map[string]*integrationaudit.Input{
		"no input":   nil,
		"no package": {},
		"no adapter": {Package: &apppkg.Package{}},
		"an adapter this transport did not build": {Package: &apppkg.Package{}, Adapter: nativeProvisionOtherAdapter{}},
	} {
		if p, err := nativeProvisionSource(ctx, in); p != nil || err != nil {
			t.Errorf("%s: %v, %v, want nil, nil", name, p, err)
		}
	}
}

// nativeProvisionOtherAdapter is an adapter that is not the transport's wrapper.
type nativeProvisionOtherAdapter struct{ adapters.AppAdapter }
