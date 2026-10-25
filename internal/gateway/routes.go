package gateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Deps is what a route area may use. Areas receive interfaces and plain
// values only; the composition root (internal/cli) decides what stands behind
// them.
type Deps struct {
	Logger    *slog.Logger
	Clock     clock.Clock
	IDs       *ids.Generator
	Providers *providers.Registry
	Config    Config
	Dev       DevConfig
	// Budget caps what one model call may cost (master plan 17.2). Areas that
	// call a provider wrap it with providers.WithBudget(gen, d.Budget).
	Budget providers.Budget
	// Services is how an area obtains the application services it needs, by
	// type: d.Needs(), Need[T] and Want[T]. Nil is an empty registry.
	Services *Services

	ready *readiness

	// area is the area being registered; unmounted and errs collect what Needs
	// found out about it. NewServer reads them after every area has run.
	area      string
	unmounted map[string][]string
	errs      []error
}

// AreaFunc registers one area's routes. It may register nothing: the dev area
// does exactly that unless dev.spike_endpoint is on, and an area whose services
// were not provided mounts nothing (d.Needs, services.go). It takes the
// services it uses from d.Services by type and has no way to return an error:
// a half wired area is reported through d and fails NewServer.
type AreaFunc func(rt *Router, d *Deps)

var (
	areasMu sync.Mutex
	areas   = map[string]AreaFunc{}

	areaNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// RegisterArea adds a route area. Every routes_<area>.go calls it from init():
//
//	func init() { RegisterArea("sessions", registerSessions) }
//
// so a new area is a new file and never an edit of an existing one. It panics
// on a bad or duplicate name, which is a programming error found when the
// binary first starts.
func RegisterArea(name string, fn AreaFunc) {
	if !areaNameRE.MatchString(name) {
		panic(fmt.Sprintf("gateway: invalid route area name %q", name))
	}
	if fn == nil {
		panic(fmt.Sprintf("gateway: route area %q has no function", name))
	}
	areasMu.Lock()
	defer areasMu.Unlock()
	if _, dup := areas[name]; dup {
		panic(fmt.Sprintf("gateway: route area %q registered twice", name))
	}
	areas[name] = fn
}

// Areas returns the registered area names, sorted.
func Areas() []string {
	areasMu.Lock()
	defer areasMu.Unlock()
	names := make([]string, 0, len(areas))
	for n := range areas {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Router collects the routes of one Server.
type Router struct {
	mux    *http.ServeMux
	routes []string
	v1auth func(http.Handler) http.Handler
	// area is the area being registered and byArea the patterns each area
	// mounted, so a test can tell an area that mounted nothing.
	area   string
	byArea map[string][]string
	// attachLimits are the request body limits the areas declared for their routes
	// (attachSetRouteLimit).
	attachLimits attachLimitSet
}

var routeMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true,
}

// Handle registers h for a "METHOD /path" pattern. A pattern without a method
// is refused (it would answer every method), and so is anything under /v1
// (use V1) or the root pattern, which the gateway keeps for its own 404 and
// 405 answers. It panics on a malformed or conflicting pattern.
func (rt *Router) Handle(pattern string, h http.Handler) {
	method, path := splitPattern(pattern)
	if path == "/v1" || strings.HasPrefix(path, "/v1/") {
		panic(fmt.Sprintf("gateway: %q must be registered through Router.V1", pattern))
	}
	rt.add(method, path, h)
}

// HandleFunc is Handle for a function.
func (rt *Router) HandleFunc(pattern string, f func(http.ResponseWriter, *http.Request)) {
	rt.Handle(pattern, http.HandlerFunc(f))
}

func splitPattern(pattern string) (method, path string) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok || !routeMethods[method] || !strings.HasPrefix(path, "/") || path == "/" || strings.ContainsAny(path, " \t") {
		panic(fmt.Sprintf("gateway: route pattern %q must be \"METHOD /path\" with GET, POST, PUT, PATCH or DELETE", pattern))
	}
	return method, path
}

func (rt *Router) add(method, path string, h http.Handler) {
	pattern := method + " " + path
	rt.mux.Handle(pattern, h)
	rt.routes = append(rt.routes, pattern)
	if rt.byArea == nil {
		rt.byArea = map[string][]string{}
	}
	rt.byArea[rt.area] = append(rt.byArea[rt.area], pattern)
}

// V1 returns the router for the /v1 API. Its handlers run behind
// Options.V1Auth, and refuse every request while that is nil: a /v1 route
// without an identity check never serves anything (fail closed).
func (rt *Router) V1() V1Router { return V1Router{rt: rt} }

// V1Router registers routes below /v1.
type V1Router struct{ rt *Router }

// Handle registers h for "METHOD /path", served as "METHOD /v1/path".
func (v V1Router) Handle(pattern string, h http.Handler) {
	method, path := splitPattern(pattern)
	auth := v.rt.v1auth
	if auth == nil {
		auth = denyV1
	}
	v.rt.add(method, "/v1"+path, auth(h))
}

// HandleFunc is Handle for a function.
func (v V1Router) HandleFunc(pattern string, f func(http.ResponseWriter, *http.Request)) {
	v.Handle(pattern, http.HandlerFunc(f))
}

// denyV1 is the V1 wrapper used while no identity middleware is configured.
func denyV1(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "the /v1 API is not enabled: no identity verification is configured"))
	})
}

// Routes returns every registered "METHOD /path" pattern, sorted. The route
// inventory tests (SEC-092) read it.
func (rt *Router) Routes() []string {
	out := append([]string(nil), rt.routes...)
	sort.Strings(out)
	return out
}

// notFoundOrMethodNotAllowed answers any request no pattern claimed: 405 with
// an Allow header when the path exists under another method, 404 otherwise.
// Both are the error envelope, not the mux's plain text.
func (rt *Router) notFoundOrMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	var allow []string
	for m := range routeMethods {
		if m == r.Method {
			continue
		}
		probe := r.Clone(r.Context())
		probe.Method = m
		if _, pattern := rt.mux.Handler(probe); pattern != "" && pattern != "/" {
			allow = append(allow, m)
			if m == http.MethodGet {
				allow = append(allow, http.MethodHead)
			}
		}
	}
	if len(allow) > 0 {
		sort.Strings(allow)
		w.Header().Set("Allow", strings.Join(allow, ", "))
		WriteErrorStatus(w, r, http.StatusMethodNotAllowed, contracts.New(contracts.ErrValidationFailed, "method not allowed"))
		return
	}
	WriteErrorStatus(w, r, http.StatusNotFound, contracts.New(contracts.ErrValidationFailed, "no such route"))
}
