package gateway

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Services is the typed service registry of one Server: the one way a route
// area obtains what it needs. The composition root (internal/cli) provides the
// services; a test provides fakes through the very same calls; an area asks for
// the types it uses and never reaches a package variable, a central struct or
// another area.
//
//	// the composition root, or a test
//	svc := gateway.NewServices()
//	gateway.Provide[gateway.ActionService](svc, coordinator)
//	srv, err := gateway.NewServer(gateway.Options{Services: svc, ...})
//
//	// routes_<area>.go
//	func registerThings(rt *gateway.Router, d *gateway.Deps) {
//		n := d.Needs()
//		things := gateway.Need[ThingService](n)
//		if !n.Ready() {
//			return
//		}
//		...
//	}
//
// A service is registered under its static type T, never under the dynamic
// type of the value: Provide[ActionService](svc, c) is found by
// Need[ActionService] and by nothing else, which is how a handler is given the
// narrow interface it may use and not the whole object behind it. (Provide(svc,
// c) with a *Coordinator is a different registration; an area asking for
// ActionService then finds nothing and says so, rather than being handed more
// than it should have.)
//
// The registry is frozen when NewServer takes it: a service cannot appear, or
// be replaced, after routes have captured the one they were given.
type Services struct {
	mu     sync.RWMutex
	byType map[reflect.Type]any
	frozen bool
}

// NewServices returns an empty registry.
func NewServices() *Services { return &Services{byType: map[reflect.Type]any{}} }

func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

// isNilValue reports whether v is nil or holds a nil pointer, map, slice,
// channel or function: a registration that would hand an area a service it
// cannot call.
func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

// Provide registers v as the service of type T. It fails (and registers
// nothing) for a nil value, a type that is already provided and a registry
// that NewServer has already frozen: two providers for one type is a wiring
// mistake, never a "last one wins".
func Provide[T any](s *Services, v T) error {
	t := typeOf[T]()
	if s == nil {
		return contracts.Newf(contracts.ErrValidationFailed, "gateway: cannot provide %s to a nil service registry", t)
	}
	if isNilValue(v) {
		return contracts.Newf(contracts.ErrValidationFailed, "gateway: the %s service is nil", t)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frozen {
		return contracts.Newf(contracts.ErrConflict, "gateway: the service registry is frozen; %s was provided too late", t)
	}
	if _, dup := s.byType[t]; dup {
		return contracts.Newf(contracts.ErrConflict, "gateway: the %s service is provided twice", t)
	}
	s.byType[t] = v
	return nil
}

// MustProvide is Provide for wiring that cannot continue without the service:
// it panics on the errors Provide returns, which are programming mistakes.
func MustProvide[T any](s *Services, v T) {
	if err := Provide[T](s, v); err != nil {
		panic(err)
	}
}

// Lookup returns the service provided as type T.
func Lookup[T any](s *Services) (T, bool) {
	var zero T
	if s == nil {
		return zero, false
	}
	s.mu.RLock()
	v, ok := s.byType[typeOf[T]()]
	s.mu.RUnlock()
	if !ok {
		return zero, false
	}
	t, ok := v.(T)
	return t, ok
}

// Types lists the provided service types, sorted: for diagnostics and tests.
func (s *Services) Types() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.byType))
	for t := range s.byType {
		out = append(out, t.String())
	}
	sort.Strings(out)
	return out
}

func (s *Services) freeze() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.frozen = true
	s.mu.Unlock()
}

// Needs collects the services one route area requires while it registers. An
// area calls Need for every service it cannot work without, Want for the ones
// it can, and Ready before it registers any route.
type Needs struct {
	d       *Deps
	missing []string
	found   int
}

// Needs starts the requirements of the area being registered.
func (d *Deps) Needs() *Needs { return &Needs{d: d} }

// Need returns the service provided as type T and records it as required: if
// it is not provided, Ready reports false and the area must register nothing.
func Need[T any](n *Needs) T {
	v, ok := Lookup[T](n.d.Services)
	if ok {
		n.found++
	} else {
		n.missing = append(n.missing, typeOf[T]().String())
	}
	return v
}

// Want returns the service provided as type T when there is one. A service an
// area can do without is never a requirement and never makes it unmounted.
func Want[T any](n *Needs) (T, bool) { return Lookup[T](n.d.Services) }

// Ready reports whether every required service was provided, which is when the
// area may register its routes. The rule is fail closed in both directions:
//
//   - No required service provided: the area is not wired into this server. It
//     mounts nothing (Server.Unmounted names it) and the server is built, so a
//     server that was given no services, like every M0 test, still serves its
//     probes and has no /v1 route at all.
//   - Some but not all provided: that is a wiring mistake, not a choice, and
//     NewServer fails with an error naming the missing types. A handler is
//     never mounted holding a service it does not have.
func (n *Needs) Ready() bool {
	if len(n.missing) == 0 {
		return true
	}
	if n.found == 0 {
		if n.d.unmounted == nil {
			n.d.unmounted = map[string][]string{}
		}
		n.d.unmounted[n.d.area] = n.missing
		return false
	}
	n.d.errs = append(n.d.errs, contracts.Newf(contracts.ErrValidationFailed,
		"gateway: route area %q is half wired: its other services are provided but %s %s not",
		n.d.area, strings.Join(n.missing, ", "), pluralIs(len(n.missing))))
	return false
}

func pluralIs(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// String describes the registry for a log line: the number of services, not
// their values.
func (s *Services) String() string { return fmt.Sprintf("gateway.Services(%d)", len(s.Types())) }
