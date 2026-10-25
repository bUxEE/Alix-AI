package gateway

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ---- the test seam for later areas ----

// testServiceProviders lets the test file of a route area provide the fakes
// its area needs, so the inventory and OpenAPI tests build their server with
// every area wired and a later task never edits a shared helper:
//
//	func init() {
//		registerTestServices("rag", func(t testing.TB, e *apiEnv, s *Services) {
//			if err := Provide[RagService](s, fakeRag{}); err != nil {
//				t.Fatal(err)
//			}
//		})
//	}
//
// Every provider runs in newAPIEnv after the M1-16 services are provided, so a
// type two areas share is provided once, by the first, and a second Provide of
// it fails the test (two owners for one service is a bug).
var testServiceProviders = map[string]func(testing.TB, *apiEnv, *Services){}

func registerTestServices(area string, fn func(testing.TB, *apiEnv, *Services)) {
	if _, dup := testServiceProviders[area]; dup {
		panic("gateway test: services of area " + area + " registered twice")
	}
	testServiceProviders[area] = fn
}

func provideTestServices(t testing.TB, e *apiEnv, s *Services) {
	t.Helper()
	names := make([]string, 0, len(testServiceProviders))
	for n := range testServiceProviders {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		testServiceProviders[n](t, e, s)
	}
}

// mayMountNothing are the areas allowed to have no route: the dev area mounts
// its one route only when dev.spike_endpoint is on. Every other area that
// mounts nothing in the full test server is an area whose fakes were not
// provided, and the area would silently escape the OpenAPI comparison.
var mayMountNothing = map[string]bool{"dev": true}

var v1AreaNames = []string{"v1_actions", "v1_apps", "v1_sessions", "v1_turnevents"}

// ---- the registry ----

type fakeThing interface{ Thing() string }

type thingImpl struct{ name string }

func (t thingImpl) Thing() string { return t.name }

func TestServicesAreKeyedByTheStaticType(t *testing.T) {
	s := NewServices()
	if err := Provide[fakeThing](s, thingImpl{"a"}); err != nil {
		t.Fatal(err)
	}
	got, ok := Lookup[fakeThing](s)
	if !ok || got.Thing() != "a" {
		t.Fatalf("Lookup[fakeThing] = %v, %v", got, ok)
	}
	// The same value under its concrete type is another registration, and the
	// interface the area asks for is not satisfied by it.
	if _, ok := Lookup[thingImpl](s); ok {
		t.Error("a service provided as an interface was found under its concrete type")
	}
	s2 := NewServices()
	if err := Provide(s2, thingImpl{"b"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := Lookup[fakeThing](s2); ok {
		t.Error("a service provided as a concrete type satisfied an interface lookup")
	}
	if _, ok := Lookup[fakeThing](nil); ok {
		t.Error("a nil registry provided a service")
	}
	if got := strings.Join(s.Types(), ","); got != "gateway.fakeThing" {
		t.Errorf("Types = %q", got)
	}
}

func TestServicesRefuseNilDuplicateAndLateRegistration(t *testing.T) {
	s := NewServices()
	var nilThing fakeThing
	var nilCoordinator *actions.Coordinator
	for name, err := range map[string]error{
		"nil interface":      Provide(s, nilThing),
		"nil pointer":        Provide(s, nilCoordinator),
		"typed nil in iface": Provide[ActionService](s, nilCoordinator),
		"nil registry":       Provide[fakeThing](nil, thingImpl{}),
	} {
		if err == nil {
			t.Errorf("%s: Provide succeeded", name)
		}
	}
	if len(s.Types()) != 0 {
		t.Fatalf("a refused Provide registered %v", s.Types())
	}
	if err := Provide[fakeThing](s, thingImpl{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := Provide[fakeThing](s, thingImpl{"b"}); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("a second provider for one type = %v, want %s", err, contracts.ErrConflict)
	}
	if got, _ := Lookup[fakeThing](s); got.Thing() != "a" {
		t.Error("a refused second provider replaced the first")
	}
	// NewServer freezes the registry: a service cannot be swapped in under
	// routes that already captured the old one.
	if _, err := NewServer(Options{Services: s}); err != nil {
		t.Fatal(err)
	}
	if err := Provide[ActionService](s, &actions.Coordinator{}); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("Provide after NewServer = %v, want %s", err, contracts.ErrConflict)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("MustProvide did not panic on a duplicate")
			}
		}()
		MustProvide[fakeThing](s, thingImpl{"c"})
	}()
}

// ---- the areas ----

// An area is a file that calls RegisterArea and fetches its services by type;
// the server mounts it when they are provided and not otherwise. Nothing here
// goes through Options.ExtraAreas.
func TestAreasSelfRegisterThroughServiceRegistry(t *testing.T) {
	registered := map[string]bool{}
	for _, name := range Areas() {
		registered[name] = true
	}
	for _, name := range v1AreaNames {
		if !registered[name] {
			t.Errorf("area %s did not register itself with RegisterArea (have %v)", name, Areas())
		}
	}

	// Provided through the registry, the three areas mount, and only through it.
	e := newAPIEnv(t)
	for _, name := range v1AreaNames {
		if len(e.srv.AreaRoutes(name)) == 0 {
			t.Errorf("area %s mounted no route although its services were provided", name)
		}
	}
	if un := e.srv.Unmounted(); len(un) != 0 {
		t.Errorf("areas left unmounted with every service provided: %v", un)
	}

	// Not provided: nothing mounts, the server is built, and the areas say what
	// they were missing.
	bare, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	un := bare.Unmounted()
	for _, name := range v1AreaNames {
		if len(bare.AreaRoutes(name)) != 0 {
			t.Errorf("area %s mounted %v with no services", name, bare.AreaRoutes(name))
		}
		if len(un[name]) == 0 {
			t.Errorf("area %s is not reported unmounted (Unmounted = %v)", name, un)
		}
	}
	if !strings.Contains(strings.Join(un["v1_actions"], ","), "gateway.ActionService") {
		t.Errorf("v1_actions is missing %v, want it to name gateway.ActionService", un["v1_actions"])
	}
	for _, r := range bare.Routes() {
		if strings.HasPrefix(r, "GET /healthz") || strings.HasPrefix(r, "GET /readyz") {
			continue
		}
		t.Errorf("a server with no services mounted %q", r)
	}

	// Half wired is refused, never mounted: the handler would hold a service it
	// does not have.
	half := NewServices()
	if err := Provide[ActionService](half, e.co); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(Options{Services: half}); err == nil || !strings.Contains(err.Error(), "half wired") {
		t.Fatalf("NewServer with some of an area's services = %v, want a half wired refusal", err)
	}

	// Only what is provided as ActionService reaches a request: the
	// coordinator provided under its own type satisfies nothing, so the cross
	// tenant sweeps on it cannot be mounted by mistake.
	wide := NewServices()
	if err := Provide(wide, e.co); err != nil { // T = *actions.Coordinator
		t.Fatal(err)
	}
	if err := Provide[ApplicationService](wide, e.apps); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(Options{Services: wide}); err == nil || !strings.Contains(err.Error(), "gateway.ActionService") {
		t.Fatalf("NewServer with the coordinator under its concrete type = %v, want a refusal naming gateway.ActionService", err)
	}

	// A new area follows the same three steps with nothing else to edit.
	svc := NewServices()
	if err := Provide[fakeThing](svc, thingImpl{"from the registry"}); err != nil {
		t.Fatal(err)
	}
	ad := func(rt *Router, d *Deps) {
		n := d.Needs()
		thing := Need[fakeThing](n)
		note, haveNote := Want[ApplicationService](n)
		if !n.Ready() {
			return
		}
		rt.HandleFunc("GET /things", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"thing": thing.Thing(), "optional_service": haveNote && note != nil})
		})
	}
	h := newHarness(t, func(o *Options) { o.Services = svc; o.ExtraAreas = map[string]AreaFunc{"things": ad} })
	rec := h.do(http.MethodGet, "/things", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"from the registry"`) || !strings.Contains(rec.Body.String(), `"optional_service":false`) {
		t.Fatalf("GET /things = %d %s", rec.Code, rec.Body.String())
	}
	// ...and an optional service is never a requirement.
	missing := newHarness(t, func(o *Options) { o.ExtraAreas = map[string]AreaFunc{"things": ad} })
	if rec := missing.do(http.MethodGet, "/things", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /things with no thing service = %d, want 404", rec.Code)
	}
	if _, ok := missing.srv.Unmounted()["things"]; !ok {
		t.Errorf("an area missing its service is not reported unmounted: %v", missing.srv.Unmounted())
	}
}

// m1v1Routes are the routes M1-16 mounts. The inventory pins them as a subset:
// the OpenAPI test is the exact two-way comparison, and a later area adds its
// routes without editing this list.
var m1v1Routes = []string{
	"GET /v1/actions/{preparation_id}",
	"GET /v1/actions/{preparation_id}/receipt",
	"GET /v1/applications/{application_id}",
	"GET /v1/capabilities",
	"GET /v1/health",
	"GET /v1/sessions/{session_id}",
	"GET /v1/sessions/{session_id}/pending",
	"POST /v1/actions/{preparation_id}/confirm",
	"POST /v1/actions/{preparation_id}/reject",
	"POST /v1/applications",
	"POST /v1/sessions",
	"POST /v1/sessions/{session_id}/turns",
	"DELETE /v1/sessions/{session_id}",
}

// The inventory of a fully wired server: the probes outside /v1 are exactly
// healthz and readyz, the routes of M1-16 are all there, and every registered
// area mounted something. (Before M1-22 this was pinned as an exact
// [dev health v1] list with the /v1 routes injected through ExtraAreas, where
// the OpenAPI test could not see them; see TestRoutesMatchOpenAPISpecWithAllAreas.)
func TestRouteInventoryIncludesV1Areas(t *testing.T) {
	e := newAPIEnv(t)
	have := map[string]bool{}
	var outside []string
	for _, r := range e.srv.Routes() {
		have[r] = true
		if _, path, _ := strings.Cut(r, " "); !strings.HasPrefix(path, "/v1/") {
			outside = append(outside, r)
		}
	}
	for _, want := range m1v1Routes {
		if !have[want] {
			t.Errorf("route %q is missing from the server (routes: %v)", want, e.srv.Routes())
		}
	}
	if got := strings.Join(outside, ","); got != "GET /healthz,GET /readyz" {
		t.Errorf("routes outside /v1 = %q, want only the probes (the spike endpoint is off)", got)
	}
	assertEveryAreaMounted(t, e.srv)
}

// assertEveryAreaMounted fails for a registered area that mounted nothing.
func assertEveryAreaMounted(t *testing.T, srv *Server) {
	t.Helper()
	for _, name := range Areas() {
		if mayMountNothing[name] {
			continue
		}
		if len(srv.AreaRoutes(name)) == 0 {
			t.Errorf("area %s mounted no route: its services were not provided to this server (unmounted: %v)", name, srv.Unmounted()[name])
		}
	}
	if un := srv.Unmounted(); len(un) != 0 {
		t.Errorf("areas left unmounted: %v", un)
	}
}

// SEC-092, SEC-054: the OpenAPI comparison runs over the server with EVERY
// registered area wired through the service registry. An area added later whose
// fakes are missing mounts nothing, and this test says so instead of letting it
// escape the comparison; the comparison itself is assertRoutesMatchSpec.
func TestRoutesMatchOpenAPISpecWithAllAreas(t *testing.T) {
	e := newAPIEnv(t)
	assertEveryAreaMounted(t, e.srv)
	assertRoutesMatchSpec(t, e)

	// A route outside the registered areas, mounted through ExtraAreas, is not
	// part of this comparison: ExtraAreas is for the ad hoc routes of a test and
	// the product registers its areas (see Options.ExtraAreas).
	srv, err := NewServer(Options{Services: e.svc, ExtraAreas: map[string]AreaFunc{"adhoc": func(rt *Router, _ *Deps) {
		rt.HandleFunc("GET /adhoc", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, nil) })
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range Areas() {
		if name == "adhoc" {
			t.Fatal("an ExtraAreas area appears among the registered areas")
		}
	}
	if len(srv.AreaRoutes("adhoc")) != 1 {
		t.Errorf("the ad hoc area mounted %v", srv.AreaRoutes("adhoc"))
	}
}

// The error a half wired area returns carries a typed code, not a bare string.
func TestHalfWiredAreaIsATypedError(t *testing.T) {
	s := NewServices()
	e := newAPIEnv(t)
	if err := Provide[ApplicationService](s, e.apps); err != nil {
		t.Fatal(err)
	}
	_, err := NewServer(Options{Services: s})
	if err == nil {
		t.Fatal("NewServer built a server with half of an area's services")
	}
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		t.Fatalf("error %T does not join the per-area errors", err)
	}
	for _, one := range joined.Unwrap() {
		if contracts.CodeOf(one) != contracts.ErrValidationFailed {
			t.Errorf("%v has code %s, want %s", one, contracts.CodeOf(one), contracts.ErrValidationFailed)
		}
	}
}
