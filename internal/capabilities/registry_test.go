package capabilities

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// fakeSource is a Source over in-memory packages. Only the packages whose hash
// is the "active" one of their application are reported as active.
type fakeSource struct {
	mu          sync.Mutex
	active      map[string]string
	pkgs        map[[2]string]Package
	err         error
	activeCalls int
	openCalls   int
}

func newFakeSource() *fakeSource {
	return &fakeSource{active: map[string]string{}, pkgs: map[[2]string]Package{}}
}

func (s *fakeSource) put(p Package, active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pkgs[[2]string{p.ApplicationID(), p.ContentHash()}] = p
	if active {
		s.active[p.ApplicationID()] = p.ContentHash()
	}
}

func (s *fakeSource) deactivate(app string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, app)
}

func (s *fakeSource) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *fakeSource) calls() (active, open int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeCalls, s.openCalls
}

func (s *fakeSource) ActiveHash(_ context.Context, app string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeCalls++
	if s.err != nil {
		return "", false, s.err
	}
	h, ok := s.active[app]
	return h, ok, nil
}

func (s *fakeSource) Open(_ context.Context, app, hash string) (Package, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openCalls++
	p, ok := s.pkgs[[2]string{app, hash}]
	if !ok {
		return nil, errors.New("no such package")
	}
	return p, nil
}

func versioned(t testing.TB, hash string, mod func(*pkgDef)) *testPkg {
	t.Helper()
	d := demoDef()
	d.hash = hash
	if mod != nil {
		mod(&d)
	}
	return d.build(t)
}

// TestRegistryHoldsOnlyActivePackages (SEC-060): a package that is not active
// registers nothing, so it exposes nothing.
// SEC-060
func TestRegistryHoldsOnlyActivePackages(t *testing.T) {
	good := demoPackage(t)
	for _, tc := range []struct {
		name string
		pkg  Package
		code contracts.ErrCode
	}{
		{"quarantined", good.withStatus(policy.StatusQuarantined), contracts.ErrUnavailable},
		{"an unknown status", good.withStatus("rejected"), contracts.ErrUnavailable},
		{"no status at all", good.withStatus(""), contracts.ErrUnavailable},
		{"a nil package", nil, contracts.ErrInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry(nil)
			err := reg.Activate(tc.pkg)
			if !contracts.HasCode(err, tc.code) {
				t.Fatalf("err = %v, want %q", err, tc.code)
			}
			if len(reg.Applications()) != 0 {
				t.Fatalf("a refused package is registered: %v", reg.Applications())
			}
		})
	}

	t.Run("a capability the typed contract lacks fails the whole package", func(t *testing.T) {
		p := demoPackage(t)
		delete(p.specs, capExtend)
		if err := NewRegistry(nil).Activate(p); !contracts.HasCode(err, contracts.ErrSchemaInvalid) {
			t.Fatalf("err = %v, want schema_invalid", err)
		}
	})
	t.Run("a typed spec that disagrees with its ID fails the package", func(t *testing.T) {
		p := demoPackage(t)
		s := p.specs[capExtend]
		s.ID = capReserve
		p.specs[capExtend] = s
		if err := NewRegistry(nil).Activate(p); !contracts.HasCode(err, contracts.ErrSchemaInvalid) {
			t.Fatalf("err = %v, want schema_invalid", err)
		}
	})
}

func TestRegistryInfoListsUserCapabilitiesOfTheActivePackage(t *testing.T) {
	reg := NewRegistry(nil)
	if err := reg.Activate(demoPackage(t)); err != nil {
		t.Fatal(err)
	}
	info, err := reg.Info(context.Background(), appLib)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{capFind, capExtend, capDelete, capReserve}
	// sorted: catalog.find_title, loans.extend, members.delete_account, reservations.create
	if !equalStrings(info.Capabilities, want) {
		t.Errorf("Capabilities = %v, want %v (the admin capability is not a user capability)", info.Capabilities, want)
	}
	if info.ApplicationID != appLib || info.PackageVersion != "1.0.0" || !equalStrings(info.Languages, []string{"en", "it"}) {
		t.Errorf("Info = %+v", info)
	}
	if _, err := reg.Info(context.Background(), "nobody"); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Errorf("unknown application: err = %v", err)
	}
	reg.Deactivate(appLib)
	if _, err := reg.Info(context.Background(), appLib); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Errorf("after Deactivate: err = %v", err)
	}
}

// TestRegistryLoadsTheActivePackageFromItsSource: only the package the source
// reports as active is opened, a changed active package replaces the old entry,
// and anything that makes the registry unsure drops the entry.
func TestRegistryLoadsTheActivePackageFromItsSource(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(t0)
	src := newFakeSource()
	v1 := versioned(t, "1111", nil)
	v2 := versioned(t, "2222", func(d *pkgDef) { d.caps = d.caps[:2] }) // v2 drops the other capabilities
	quarantined := versioned(t, "3333", nil).withStatus(policy.StatusQuarantined)
	src.put(v1, true)
	src.put(v2, false)
	reg := NewRegistry(src, WithClock(clk), WithMaxAge(time.Minute))

	// First use loads the active package.
	info, err := reg.Info(ctx, appLib)
	if err != nil || info.ContentHash != "1111" || len(info.Capabilities) != 4 {
		t.Fatalf("Info = %+v, %v", info, err)
	}
	if a, o := src.calls(); a != 1 || o != 1 {
		t.Fatalf("source calls = %d active, %d open", a, o)
	}

	// Within MaxAge the source is not asked again.
	clk.Advance(30 * time.Second)
	if _, err := reg.Info(ctx, appLib); err != nil {
		t.Fatal(err)
	}
	if a, _ := src.calls(); a != 1 {
		t.Fatalf("the source was asked again within MaxAge (%d calls)", a)
	}

	// After MaxAge it is asked, and an unchanged package is not reopened.
	clk.Advance(31 * time.Second)
	if _, err := reg.Info(ctx, appLib); err != nil {
		t.Fatal(err)
	}
	if a, o := src.calls(); a != 2 || o != 1 {
		t.Fatalf("after MaxAge: %d active calls, %d opens, want 2 and 1", a, o)
	}

	// The active package changes: the next re-check swaps the entry whole.
	src.put(v2, true)
	clk.Advance(2 * time.Minute)
	info, err = reg.Info(ctx, appLib)
	if err != nil || info.ContentHash != "2222" || len(info.Capabilities) != 2 {
		t.Fatalf("after the swap: %+v, %v", info, err)
	}

	// An explicit Refresh does not wait for MaxAge.
	src.put(v1, true)
	if err := reg.Refresh(ctx, appLib); err != nil {
		t.Fatal(err)
	}
	if info, _ = reg.Info(ctx, appLib); info.ContentHash != "1111" {
		t.Fatalf("Refresh did not pick up the newly active package: %+v", info)
	}

	// The source opens a package that is not active: refused, entry dropped.
	src.put(quarantined, true)
	if err := reg.Refresh(ctx, appLib); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Fatalf("Refresh of a quarantined package: %v", err)
	}
	if got := reg.Applications(); len(got) != 0 {
		t.Fatalf("entry kept after a refused package: %v", got)
	}

	// No active package any more: nothing is served, and the absence is not
	// re-queried on every request.
	src.put(v1, true)
	if err := reg.Refresh(ctx, appLib); err != nil {
		t.Fatal(err)
	}
	src.deactivate(appLib)
	clk.Advance(2 * time.Minute)
	before, _ := src.calls()
	for i := 0; i < 5; i++ {
		if _, err := reg.Info(ctx, appLib); !contracts.HasCode(err, contracts.ErrUnavailable) {
			t.Fatalf("with no active package: %v", err)
		}
	}
	if after, _ := src.calls(); after-before != 1 {
		t.Fatalf("a quarantined-only application cost %d source reads for 5 requests, want 1", after-before)
	}
	// ...until the activation owner says it changed.
	src.put(v1, true)
	if err := reg.Refresh(ctx, appLib); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Info(ctx, appLib); err != nil {
		t.Fatalf("after re-activation: %v", err)
	}

	// A source that fails is "unknown", never "still active".
	src.fail(errors.New("database is locked"))
	clk.Advance(2 * time.Minute)
	if _, err := reg.Info(ctx, appLib); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Fatalf("with a failing source: %v", err)
	}
	if got := reg.Applications(); len(got) != 0 {
		t.Fatalf("a stale entry survived a failing source: %v", got)
	}
	src.fail(nil)
	if _, err := reg.Info(ctx, appLib); err != nil {
		t.Fatalf("after the source recovered: %v", err)
	}
}

func TestRegistryRefusesAPackageOfAnotherApplication(t *testing.T) {
	src := newFakeSource()
	d := demoDef()
	d.app = "someone-else"
	d.hash = "9999"
	src.put(d.build(t), false)
	src.mu.Lock()
	src.active[appLib] = "9999" // the source answers for demo-library with someone else's package
	src.pkgs[[2]string{appLib, "9999"}] = d.build(t)
	src.mu.Unlock()
	reg := NewRegistry(src)
	if err := reg.Refresh(context.Background(), appLib); !contracts.HasCode(err, contracts.ErrScopeViolation) {
		t.Fatalf("err = %v, want scope_violation", err)
	}
	if len(reg.Applications()) != 0 {
		t.Fatal("a package of another application was registered under this one")
	}
}

// TestRegistryKeepsApplicationsApart: the same capability ID in two applications
// is two capabilities, found only through the application of the Scope (SEC-053).
// SEC-053
func TestRegistryKeepsApplicationsApart(t *testing.T) {
	a := demoDef()
	b := demoDef()
	b.app = "demo-library-b"
	b.hash = "bbbb"
	// In B, reservations.create is about something else entirely.
	b.caps = append([]capDef(nil), b.caps...)
	b.caps[1].title = "Hold a seat" // no word of the title is a word of the request below
	b.terms = append([]termDef(nil), b.terms...)
	for i := range b.terms {
		if b.terms[i].id == "reservation" {
			b.terms[i].labels = map[string][]string{"en": {"table"}, "it": {"tavolo"}}
		}
	}
	rt, reg := newRouter(t, nil, a.build(t), b.build(t))
	if got := reg.Applications(); !equalStrings(got, []string{appLib, "demo-library-b"}) {
		t.Fatalf("Applications = %v", got)
	}

	ra := mustRoute(t, rt, reqFor(scopeFor(appLib, "member", nil), "a reservation please", inLanguage("en")))
	rb := mustRoute(t, rt, reqFor(scopeFor("demo-library-b", "member", nil), "a reservation please", inLanguage("en")))
	if !ra.has(capReserve) {
		t.Fatalf("app A: %v", ra.IDs())
	}
	if rb.has(capReserve) {
		t.Fatalf("app B routed %q through app A's terminology: %v", "reservation", rb.IDs())
	}
	rb = mustRoute(t, rt, reqFor(scopeFor("demo-library-b", "member", nil), "a table please", inLanguage("en")))
	if !rb.has(capReserve) {
		t.Fatalf("app B: %v", rb.IDs())
	}
	if ra2 := mustRoute(t, rt, reqFor(scopeFor(appLib, "member", nil), "a table please", inLanguage("en"))); ra2.has(capReserve) {
		t.Fatalf("app A routed through app B's terminology: %v", ra2.IDs())
	}
}

// TestRegistryIsSafeForConcurrentRefreshAndRoute: readers never see a half-built
// entry while the active package is being swapped. (Run with -race on Linux CI.)
func TestRegistryIsSafeForConcurrentRefreshAndRoute(t *testing.T) {
	src := newFakeSource()
	v1 := versioned(t, "1111", nil)
	v2 := versioned(t, "2222", nil)
	src.put(v1, true)
	src.put(v2, false)
	reg := NewRegistry(src, WithMaxAge(time.Nanosecond))
	rt, err := NewRouter(reg, policy.NewGateway(nil))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				res, err := rt.Route(context.Background(), reqFor(memberScope(), "renew my loan", inLanguage("en")))
				if err != nil {
					t.Errorf("Route: %v", err)
					return
				}
				if len(res.Candidates) == 0 || res.Candidates[0].CapabilityID != capExtend {
					t.Errorf("shortlist %v during a swap", res.IDs())
					return
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		src.put([]*testPkg{v1, v2}[i%2], true)
		if err := reg.Refresh(context.Background(), appLib); err != nil {
			t.Errorf("Refresh: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// flippablePkg is a package whose reported status can change after activation,
// as a package opened from the store could if it were re-read.
type flippablePkg struct {
	*testPkg
	status atomic.Value
}

func (f *flippablePkg) Status() policy.PackageStatus { return f.status.Load().(policy.PackageStatus) }

// TestRegistryStopsServingAPackageThatIsNoLongerActive: the status is checked on
// every lookup, not only when the package is activated.
func TestRegistryStopsServingAPackageThatIsNoLongerActive(t *testing.T) {
	f := &flippablePkg{testPkg: demoPackage(t)}
	f.status.Store(policy.StatusActive)
	rt, reg := newRouter(t, nil, f)
	if res := mustRoute(t, rt, reqFor(memberScope(), "renew my loan", inLanguage("en"))); !res.has(capExtend) {
		t.Fatalf("shortlist = %v", res.IDs())
	}
	f.status.Store(policy.StatusQuarantined)
	if _, err := rt.Route(context.Background(), reqFor(memberScope(), "renew my loan", inLanguage("en"))); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Fatalf("err = %v, want unavailable once the package is not active", err)
	}
	if got := reg.Applications(); len(got) != 0 {
		t.Fatalf("the entry stayed registered: %v", got)
	}
}
