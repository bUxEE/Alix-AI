package adapterssec

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"strings"
	"testing"

	mcpfixture "github.com/buxee/alix-ai/adapters/mcp/testdata/fixture"
	openapifixture "github.com/buxee/alix-ai/adapters/openapi/testdata/fixture"

	mcptransport "github.com/buxee/alix-ai/adapters/mcp"
	openapitransport "github.com/buxee/alix-ai/adapters/openapi"
	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

const booksApp = "demo-books"

// booksPackage is valid-minimal as a second application: it registers the same raw
// capability names as demo-library, but its policy grants members the search only.
func booksPackage(t testing.TB, base pkgFiles) pkgFiles {
	t.Helper()
	p := base.clone().
		edit(t, "alixai.yaml", "id: demo-library", "id: "+booksApp).
		edit(t, "rag-sources/loan-policies.yaml", "application_id: demo-library", "application_id: "+booksApp).
		edit(t, "policies/default-access.yaml", "  - catalog.find_title\n  - reservations.create\n- role: visitor", "  - catalog.find_title\n- role: visitor")
	return p
}

// SEC-053: capability ids are namespaced per application and resolved only inside the
// session's application; an operation outside the approved subset is not callable;
// the integration audit reports ambiguous operations.
//
// SEC-053
func TestSecCapabilityIdsNamespacedPerApplication(t *testing.T) {
	base := validMinimal(t)
	base.edit(t, "capabilities/reservations-create.yaml", "pickup_date: pickupDate", "pickup_date: pickup_day")

	t.Run("registry and policy gateway, two real packages with the same raw names", func(t *testing.T) {
		v := verifier(t, libraryApp, booksApp)
		lib := load(t, v, base.clone().signed(t, libraryApp, "1.0.0"))
		books := load(t, v, booksPackage(t, base).signed(t, booksApp, "1.0.0"))
		if !lib.OK() || !books.OK() {
			t.Fatalf("the packages do not load: %v / %v", criticalIDs(lib), criticalIDs(books))
		}
		pkgA, pkgB := activeLibrary{capabilities.FromApppkg(lib.Package)}, activeLibrary{capabilities.FromApppkg(books.Package)}
		clk := clock.NewFake(loadNow)
		reg := capabilities.NewRegistry(nil, capabilities.WithClock(clk))
		for _, p := range []capabilities.Package{pkgA, pkgB} {
			if err := reg.Activate(p); err != nil {
				t.Fatal(err)
			}
		}
		infoA, errA := reg.Info(ctxb(t), libraryApp)
		infoB, errB := reg.Info(ctxb(t), booksApp)
		if errA != nil || errB != nil || strings.Join(infoA.Capabilities, ",") != strings.Join(infoB.Capabilities, ",") || infoA.ContentHash == infoB.ContentHash {
			t.Fatalf("the registry does not hold two entries with the same raw names: %+v / %+v (%v, %v)", infoA, infoB, errA, errB)
		}
		if _, err := reg.Info(ctxb(t), "demo-nothing"); err == nil {
			t.Fatal("the registry resolved an application that was never registered")
		}

		gw := policy.NewGateway(clk)
		scope := func(app string) contracts.Scope {
			s := libraryScope()
			s.ApplicationID = app
			return s
		}
		decide := func(sc contracts.Scope, pkg policy.ActivePackage, bind policy.Binding, id string) policy.Decision {
			return gw.Decide(ctxb(t), policy.Request{
				Scope: sc, Package: pkg, CapabilityID: id, Binding: bind, Phase: policy.PhasePrepare,
				Fields: map[string]any{"title": "x", "pickup_date": "2026-11-13"},
			})
		}
		scA, scB := scope(libraryApp), scope(booksApp)

		// Control: A's own session resolves A's capability.
		if d := decide(scA, pkgA, policy.BindingOf(scA), capReserve); !d.Allowed {
			t.Fatalf("control: the capability is denied inside its own application: %v", d.Err())
		}
		// B registers the same raw name; B's own policy decides in B's session, and it does
		// not grant the member what A's grants.
		if d := decide(scB, pkgB, policy.BindingOf(scB), capReserve); d.Allowed {
			t.Error("B's session was allowed the capability its own policy does not grant (A's grant was used)")
		}
		if d := decide(scB, pkgB, policy.BindingOf(scB), capFind); !d.Allowed {
			t.Errorf("control: B's own capability is denied in B's session: %v", d.Err())
		}
		// A's package presented in B's session, or B's in A's, decides nothing.
		for name, c := range map[string]struct {
			sc  contracts.Scope
			pkg policy.ActivePackage
			bnd policy.Binding
		}{
			"A's package in B's session":       {scB, pkgA, policy.BindingOf(scB)},
			"B's package in A's session":       {scA, pkgB, policy.BindingOf(scA)},
			"A's binding in B's session":       {scB, pkgB, policy.BindingOf(scA)},
			"B's binding on A's package":       {scA, pkgA, policy.BindingOf(scB)},
			"A's package, B's scope, A's bind": {scB, pkgA, policy.BindingOf(scA)},
		} {
			for _, id := range []string{capFind, capReserve} {
				if d := decide(c.sc, c.pkg, c.bnd, id); d.Allowed {
					t.Errorf("%s: %s was allowed across applications", name, id)
				}
			}
		}
		// What B's session may see is B's: exposure is decided against the session's own package.
		ex := gw.Expose(ctxb(t), policy.ExposeRequest{Scope: scB, Package: pkgB, Binding: policy.BindingOf(scB)})
		var seen []string
		for _, d := range ex.Visible {
			seen = append(seen, d.Record.CapabilityID)
		}
		if strings.Join(seen, ",") != capFind {
			t.Errorf("B's session is shown %v, want only %s", seen, capFind)
		}
	})

	t.Run("coordinator, adapters and hosts of two applications", func(t *testing.T) {
		s := newStack(t, "fake", level1, "app-alpha", "app-bravo")
		// B declares the read capability only: it registers the raw name items.get, and not
		// items.create.
		s.pkgs["app-bravo"] = onlyRead(t, "app-bravo")
		scA := scopeFor("app-alpha", tenantA, "user-1", "ses-1")
		scB := scopeFor("app-bravo", tenantA, "user-1", "ses-1")
		hostA, hostB := s.wire("app-alpha").app, s.wire("app-bravo").app
		hostA.Seed(tenantA, ws1, "item_a", "alpha item")
		hostB.Seed(tenantA, ws1, "item_b", "bravo item")

		// Control: each session reads its own application's record, from its own host only.
		oa, err := s.read(scA, "item_a")
		if err != nil || len(oa.Result.Identifiers) != 1 || oa.Result.Identifiers[0].EntityID != "item_a" {
			t.Fatalf("control: A's session cannot read A's record: %v %+v", err, oa.Result)
		}
		ob, err := s.read(scB, "item_b")
		if err != nil || len(ob.Result.Identifiers) != 1 || ob.Result.Identifiers[0].EntityID != "item_b" {
			t.Fatalf("control: B's session cannot read B's record: %v %+v", err, ob.Result)
		}
		if len(hostA.Calls()) != 1 || len(hostB.Calls()) != 1 {
			t.Fatalf("host calls: A %d, B %d, want 1 and 1", len(hostA.Calls()), len(hostB.Calls()))
		}
		// A's record is invisible to B's session: B's host is asked and has no such item.
		if _, err := s.read(scB, "item_a"); err == nil {
			t.Fatal("B's session read A's record")
		}
		if len(hostA.Calls()) != 1 {
			t.Fatal("B's session reached A's host")
		}

		// A's capability, not registered by B, cannot be invoked in B's session: no host is called.
		before := len(hostA.Calls()) + len(hostB.Calls())
		_, err = s.prepare(scB, "from B")
		if err == nil {
			t.Fatal("B's session prepared a capability B does not declare")
		}
		if got := contracts.CodeOf(err); got != contracts.ErrUnknownCapability && got != contracts.ErrPermissionDenied && got != contracts.ErrScopeViolation {
			t.Errorf("error code = %s", got)
		}
		if len(hostA.Calls())+len(hostB.Calls()) != before || hostA.Effects() != 0 || hostB.Effects() != 0 {
			t.Fatal("a host was called for a capability the session's application does not declare")
		}
		// And control: the same capability is prepared and committed in A's session.
		p := s.mustPrepare(scA, "from A")
		if _, err := s.commit(scA, p); err != nil || hostA.Effects() != 1 || hostB.Effects() != 0 {
			t.Fatalf("control: A's own commit: %v (effects A %d, B %d)", err, hostA.Effects(), hostB.Effects())
		}

		// The wiring is wrong: the adapter source answers A's adapter for B's session. The
		// wrapper is bound to A and refuses B's scope; nothing reaches A's host.
		s.mu.Lock()
		s.forcedAdapter = s.guard["app-alpha"]
		s.mu.Unlock()
		callsA := len(hostA.Calls())
		_, err = s.read(scB, "item_a")
		if got := contracts.CodeOf(err); err == nil || got != contracts.ErrScopeViolation {
			t.Fatalf("A's adapter in B's session: error = %v (%s), want scope_violation", err, got)
		}
		if len(hostA.Calls()) != callsA {
			t.Fatal("A's host received a call made in B's session")
		}
		s.mu.Lock()
		s.forcedAdapter = nil
		// The package source answers A's package for B's session: the policy gateway binds
		// the decision to the application of the scope and refuses.
		s.forcedPackage = s.pkgs["app-alpha"]
		s.mu.Unlock()
		callsB := len(hostB.Calls())
		if _, err = s.read(scB, "item_b"); err == nil {
			t.Fatal("A's package decided for B's session")
		}
		if len(hostB.Calls()) != callsB {
			t.Fatal("B's host was called on the strength of A's package")
		}
	})

	t.Run("an operation outside the approved OpenAPI subset", func(t *testing.T) {
		app := fake.NewApp(fake.WithUnmapped(openapifixture.OpExport))
		host := openapifixture.Start(t, app)
		token := "tok_" + randomHex(t)
		host.RequireBearer(token)
		settings, err := openapitransport.ParseAdapterConfig(openapifixture.AdapterConfig, "fixture-api")
		if err != nil {
			t.Fatal(err)
		}
		bind := func(id string) openapitransport.Binding {
			return openapitransport.Binding{Endpoint: "fixture-api", OperationID: id}
		}
		get := bind(openapifixture.OpGet)
		get.Arguments = map[string]string{"item_id": "id"}
		cfg := func(caps ...openapitransport.Capability) openapitransport.Config {
			return openapitransport.Config{
				ApplicationID: demoApp,
				Endpoints: []openapitransport.Endpoint{{
					ID: "fixture-api", BaseURL: host.URL, Spec: openapifixture.Spec, Settings: settings, CredentialRef: "cred:fixture",
				}},
				Capabilities:     caps,
				Dialer:           loopbackDialer(t, host.URL),
				Secrets:          openapitransport.StaticSecrets{"cred:fixture": token},
				MaxResponseBytes: 64 << 10,
			}
		}
		approved := openapitransport.Capability{ID: capRead, Operations: map[adapters.Operation]openapitransport.Binding{adapters.OpRead: get}}
		a, err := openapitransport.New(cfg(approved))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close() })
		w, _ := adapters.Wrap(a, adapters.Options{ApplicationID: demoApp})
		app.Seed(tenantA, ws1, "item_1", "name")
		sc := scopeFor(demoApp, tenantA, "user-1", "ses-1")

		// Control: the approved operation executes.
		if _, err := w.Read(ctxb(t), sc, adapters.ReadRequest{CapabilityID: capRead, Fields: map[string]any{"item_id": "item_1"}}); err != nil {
			t.Fatalf("control: the approved operation does not execute: %v", err)
		}
		requests := len(host.Requests())

		// The document defines /admin/export (exportEverything); nothing maps it. No name for
		// it reaches it, whichever way it is spelled.
		sc2 := sc
		sc2.CapabilityScopes = append(sc2.CapabilityScopes, "admin.export", openapifixture.OpExport, "/admin/export")
		for _, id := range []string{"admin.export", openapifixture.OpExport, "/admin/export", "exportEverything ", "EXPORTEVERYTHING", capCreate} {
			_, err := a.Read(ctxb(t), sc2, adapters.ReadRequest{CapabilityID: id, Fields: map[string]any{}})
			if c := contracts.CodeOf(err); err == nil || (c != contracts.ErrUnknownCapability && c != contracts.ErrValidationFailed) {
				t.Errorf("capability %q: error %v, want unknown_capability", id, err)
			}
		}
		if host.ExportHits() != 0 || len(host.Requests()) != requests {
			t.Fatalf("the unapproved operation or any other was reached: export hits %d, requests %d -> %d", host.ExportHits(), requests, len(host.Requests()))
		}
		// The host announces the operation as one of its own; the adapter reports it as
		// unmapped to the audit and still does not call it.
		d, err := w.Describe(ctxb(t))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, u := range d.Unmapped {
			found = found || u == openapifixture.OpExport
		}
		if !found {
			t.Errorf("the unmapped operation is not reported: %v", d.Unmapped)
		}
		if host.ExportHits() != 0 {
			t.Fatal("describing the application called the unapproved operation")
		}
		// A package that maps an operationId the document does not define is refused when
		// the adapter is built: nothing is checked best-effort.
		for _, id := range []string{"exportEverythin", "getitem", "GetItem", openapifixture.OpGet + " "} {
			bad := openapitransport.Capability{ID: capRead, Operations: map[adapters.Operation]openapitransport.Binding{adapters.OpRead: bind(id)}}
			if b, err := openapitransport.New(cfg(bad)); err == nil {
				_ = b.Close()
				t.Errorf("an adapter was built for the operationId %q, which the document does not define", id)
			}
		}
		// The coordinator never gets that far: a capability the package does not declare is
		// refused by the policy gateway before any adapter is asked.
		s := newStack(t, "openapi", level1)
		w2 := s.wire(demoApp)
		calls := len(w2.app.Calls())
		for _, id := range []string{"admin.export", openapifixture.OpExport} {
			if _, err := s.co.Read(ctxb(t), scopeFor(demoApp, tenantA, "user-1", "ses-1"), readInput(id)); err == nil {
				t.Errorf("the coordinator served the undeclared capability %q", id)
			}
		}
		if len(w2.app.Calls()) != calls || w2.rest.ExportHits() != 0 {
			t.Fatal("an undeclared capability reached the host")
		}
	})

	t.Run("the audit reports ambiguous operations", func(t *testing.T) {
		files := base.clone()
		// ambiguous: differs from a mapped tool only by case, '_', '-' or '.'
		ambiguous := []string{"Commit_Reservation", "commit-reservation", "commit.reservation", "COMMIT_RESERVATION", "Find_Title"}
		// printable but not foldable: merely undocumented
		other := []string{"debug_dump_everything"}
		// not valid tool names: reported by length and hash, never by name
		invalidNames := []string{"commit_reservation​", "commіt_reservation"}

		serve := func(extra ...string) []integrationaudit.KeyedFinding {
			srv := libraryServer(t, exactTools...)
			for _, n := range extra {
				addTool(srv, n, "a host operation")
			}
			h := mcpfixture.StartHTTP(t, srv)
			v := verifier(t, libraryApp) // only for parsing: the audit has its own
			_ = v
			lr := load(t, verifier(t, libraryApp), files.clone().signed(t, libraryApp, "1.0.0"))
			if !lr.OK() {
				t.Fatalf("the package does not load: %v", criticalIDs(lr))
			}
			a, err := mcptransport.New(mcpConfigFromPackage(t, lr.Package, files, h.URL))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = a.Close() })
			w, _ := adapters.Wrap(a, adapters.Options{ApplicationID: libraryApp})
			return runAudit(t, files, w)
		}

		clean := serve()
		for _, id := range []string{"surface.ambiguous_operation", "surface.undocumented_operation", "surface.unreachable_operation", "surface.over_broad_operation"} {
			if got := findingsOf(clean, id); len(got) != 0 {
				t.Errorf("control: an honest application is reported with %s: %v", id, got)
			}
		}
		hostile := serve(append(append(append([]string(nil), ambiguous...), other...), invalidNames...)...)
		amb := findingsOf(hostile, "surface.ambiguous_operation")
		if len(amb) != len(ambiguous) {
			t.Fatalf("%d ambiguous findings, want %d (%v)", len(amb), len(ambiguous), amb)
		}
		for _, f := range amb {
			if f.Severity != contracts.SeverityCritical {
				t.Errorf("an ambiguous operation must be critical: %+v", f.Finding)
			}
		}
		for _, n := range ambiguous {
			if !anyMessageContains(amb, `"`+n+`"`) {
				t.Errorf("no finding names the ambiguous operation %q", n)
			}
		}
		und := findingsOf(hostile, "surface.undocumented_operation")
		if len(und) != len(other)+len(invalidNames) {
			t.Errorf("%d undocumented findings, want %d: %v", len(und), len(other)+len(invalidNames), und)
		}
		for _, f := range und {
			if strings.ContainsAny(f.Message, "​і") {
				t.Errorf("an invalid tool name was printed as the server wrote it: %q", f.Message)
			}
		}
	})
}

func readInput(id string) (in actions.ReadInput) {
	in.Session = scopeFor(demoApp, tenantA, "user-1", "ses-1")
	in.CapabilityID = id
	in.Fields = map[string]any{"item_id": "item_1"}
	return in
}

// onlyRead is an active package that declares the read capability and nothing else.
func onlyRead(t testing.TB, appID string) policy.ActivePackage {
	t.Helper()
	hp, err := policy.ParseHostPolicy([]byte(`{"policy_id":"default-access","role_grants":[{"role":"member","capabilities":["items.get"]}],"risk_overrides":[],"field_projection":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: appID, Version: "1.0.0", Status: policy.StatusActive,
		Roles: []string{"member", "visitor", "staff"},
		Capabilities: []policy.CapabilityDecl{{ID: capRead, Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead,
			Confirmation: policy.ConfirmNone, Fields: []policy.FieldDecl{{Name: "item_id", Required: true}}}},
		Policies: []*policy.HostPolicy{hp},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func findingsOf(fs []integrationaudit.KeyedFinding, checkID string) []integrationaudit.KeyedFinding {
	var out []integrationaudit.KeyedFinding
	for _, f := range fs {
		if f.CheckID == checkID {
			out = append(out, f)
		}
	}
	return out
}

func anyMessageContains(fs []integrationaudit.KeyedFinding, s string) bool {
	for _, f := range fs {
		if strings.Contains(f.Message, s) {
			return true
		}
	}
	return false
}

// runAudit installs the package into quarantine and runs the production integration
// audit over it with the adapter, returning the findings of the signed report.
func runAudit(t testing.TB, files pkgFiles, ad adapters.AppAdapter) []integrationaudit.KeyedFinding {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewFake(loadNow)
	db := storetest.Open(t, nil)
	ts := trust.NewStore(db, clk, trust.AllowTestOnlyKeys())
	if _, err := ts.RegisterKey(ctx, libraryApp, devkeys.KeyID, devkeys.PublicKey(), "security suite"); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const platformKey = "alixai-audit-signer-sec"
	if _, err := ts.RegisterKey(ctx, trust.PlatformApplicationID, platformKey, pub, "audit report signer"); err != nil {
		t.Fatal(err)
	}
	ver := trust.NewVerifier(ts, clk)
	dataDir := t.TempDir()
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{DB: db, Clock: clk, DataDir: dataDir, Verifier: ver, CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Install(ctx, apppkg.FromFS(files.signed(t, libraryApp, "1.0.0"))); err != nil {
		t.Fatalf("Install: %v", err)
	}
	alog, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	checks := integrationaudit.NewRegistry()
	for _, c := range integrationaudit.Builtins() {
		if err := checks.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	runner, err := integrationaudit.NewRunner(integrationaudit.Config{
		DB: db, Clock: clk, Installer: inst, DataDir: dataDir, Verifier: ver, CoreVersion: apppkg.DevCoreVersion,
		Signing: &trust.SigningKey{KeyID: platformKey, Key: priv}, Log: alog, Checks: checks,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runner.Audit(ctx, integrationaudit.RunRequest{ApplicationID: libraryApp, Adapter: ad})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(run.Signed.Report.Findings) == 0 {
		t.Fatal("the audit report holds no finding at all: the audit did not look")
	}
	return run.Signed.Report.Findings
}
