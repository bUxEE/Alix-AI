package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sort"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Options is everything NewServer needs. Nothing is read from global state, so
// a test builds a server from values alone.
type Options struct {
	// Config is the gateway section; zero fields take the defaults.
	Config Config
	// Dev is the dev section. SpikeEndpoint mounts POST /v0/dev/propose.
	Dev DevConfig
	// Logger receives the access log and errors. Nil discards. It should come
	// from internal/log so values are redacted.
	Logger *slog.Logger
	// Clock times requests and the readiness cache. Nil is the wall clock.
	Clock clock.Clock
	// Providers is the runtime registry. Nil is an empty registry, which
	// makes /readyz report unavailable.
	Providers *providers.Registry
	// Checks are extra readiness conditions (store, model preflight).
	Checks []Check
	// V1Auth wraps every /v1 route: identity.Middleware(auth) from M1-16. Nil
	// makes every /v1 route refuse its requests.
	V1Auth func(http.Handler) http.Handler
	// Budget caps one model call. The zero value is providers.DefaultBudget.
	Budget providers.Budget
	// Services holds the application services the route areas use, by type
	// (see Services). The composition root fills it from what it built; a test
	// fills it with fakes through the same Provide calls. Nil is an empty
	// registry: no area that needs a service is mounted. NewServer freezes it.
	Services *Services
	// ExtraAreas are route areas for this server only, on top of the
	// registered ones: ad hoc routes of a test that are no part of the
	// product (they are invisible to the OpenAPI and inventory tests, which
	// read the registered areas). A real area calls RegisterArea from its
	// routes_<area>.go and takes its services from Options.Services; nothing
	// in production sets this.
	ExtraAreas map[string]AreaFunc
}

// Server is the gateway: a handler plus the listener policy and lifecycle.
type Server struct {
	cfg     Config
	dev     DevConfig
	log     *slog.Logger
	clk     clock.Clock
	ids     *ids.Generator
	tlsCfg  *tls.Config
	policy  ListenPolicy
	handler http.Handler
	router  *Router
	// unmounted maps an area that mounted nothing, because none of the
	// services it needs were provided, to the types it was missing.
	unmounted map[string][]string
}

// NewServer validates the options, loads the TLS identity if one is configured
// and builds the handler. It binds nothing.
func NewServer(opts Options) (*Server, error) {
	cfg := opts.Config.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "gateway: invalid configuration")
	}
	s := &Server{
		cfg: cfg,
		dev: opts.Dev,
		log: opts.Logger,
		clk: opts.Clock,
		policy: ListenPolicy{
			TLS:           cfg.TLS.Enabled(),
			SpikeEndpoint: opts.Dev.SpikeEndpoint,
		},
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.clk == nil {
		s.clk = clock.Real{}
	}
	s.ids = ids.NewGenerator(s.clk)
	if cfg.TLS.Enabled() {
		t, err := cfg.TLS.serverTLS()
		if err != nil {
			return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "gateway: invalid TLS configuration")
		}
		s.tlsCfg = t
	}

	reg := opts.Providers
	if reg == nil {
		reg = providers.NewRegistry(s.clk)
	}
	budget := opts.Budget
	if b := opts.Budget; b.MaxPromptTokens == 0 && b.MaxOutputTokens == 0 && b.MaxBatchItems == 0 && b.Timeout == 0 {
		budget = providers.DefaultBudget()
	}
	services := opts.Services
	if services == nil {
		services = NewServices()
	}
	services.freeze()
	deps := &Deps{
		Logger:    s.log,
		Clock:     s.clk,
		IDs:       s.ids,
		Providers: reg,
		Config:    cfg,
		Dev:       opts.Dev,
		Budget:    budget,
		Services:  services,
		ready:     newReadiness(s.clk, cfg.HealthTTL, reg, opts.Checks),
	}

	rt := &Router{mux: http.NewServeMux(), v1auth: opts.V1Auth}
	apply := func(name string, fn AreaFunc) {
		deps.area, rt.area = name, name
		fn(rt, deps)
	}
	for _, name := range Areas() {
		areasMu.Lock()
		fn := areas[name]
		areasMu.Unlock()
		apply(name, fn)
	}
	extra := make([]string, 0, len(opts.ExtraAreas))
	for name := range opts.ExtraAreas {
		extra = append(extra, name)
	}
	sort.Strings(extra)
	for _, name := range extra {
		apply(name, opts.ExtraAreas[name])
	}
	deps.area, rt.area = "", ""
	if len(deps.errs) > 0 {
		return nil, errors.Join(deps.errs...)
	}
	rt.mux.HandleFunc("/", rt.notFoundOrMethodNotAllowed)

	s.router = rt
	s.unmounted = deps.unmounted
	s.handler = s.wrap(rt.mux)
	return s, nil
}

// Handler returns the full handler: middleware plus routes. Tests drive it with
// httptest; Serve uses it too.
func (s *Server) Handler() http.Handler { return s.handler }

// Routes returns every mounted "METHOD /path" pattern, sorted.
func (s *Server) Routes() []string { return s.router.Routes() }

// AreaRoutes returns the "METHOD /path" patterns one registered area mounted,
// sorted: empty for an area that mounted nothing.
func (s *Server) AreaRoutes(area string) []string {
	out := append([]string(nil), s.router.byArea[area]...)
	sort.Strings(out)
	return out
}

// Unmounted names the areas that mounted nothing because none of the services
// they need were provided, with the service types each was missing. An area
// that is only partly wired is not here: NewServer refuses it. The composition
// root checks this before it serves, so an area added without its wiring is a
// refused start and not a 404 found in production.
func (s *Server) Unmounted() map[string][]string {
	out := make(map[string][]string, len(s.unmounted))
	for area, missing := range s.unmounted {
		out[area] = append([]string(nil), missing...)
	}
	return out
}

// Policy returns the listener policy this server needs, for Listen.
func (s *Server) Policy() ListenPolicy { return s.policy }

// TLSEnabled reports whether Serve wraps its listener in TLS.
func (s *Server) TLSEnabled() bool { return s.tlsCfg != nil }

// Serve serves on ln until ctx is done, then drains in-flight requests for the
// configured shutdown time. It returns nil after a clean shutdown. A listener
// that breaks the policy (not loopback without TLS, or not loopback with the
// spike endpoint on) is refused before anything is served, whoever bound it.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if err := s.policy.CheckListener(ln); err != nil {
		_ = ln.Close()
		return err
	}
	if s.tlsCfg != nil {
		ln = tls.NewListener(ln, s.tlsCfg)
	}
	// Requests derive from this context, not from ctx: a graceful shutdown lets
	// them finish, and only a forced close cancels them. An event stream lasts
	// as long as its client stays, so it ends as soon as the shutdown begins
	// (drain, see serverStreamContext) and its client resumes from the cursor.
	drain, startDrain := context.WithCancel(context.Background())
	defer startDrain()
	base, cancelBase := context.WithCancel(context.WithValue(context.Background(), serverDrainKey{}, drain))
	defer cancelBase()
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		WriteTimeout:      s.cfg.WriteTimeout,
		IdleTimeout:       s.cfg.IdleTimeout,
		MaxHeaderBytes:    s.cfg.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return base },
	}
	srv.RegisterOnShutdown(startDrain)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		s.log.Warn("graceful shutdown timed out; closing connections")
		cancelBase()
		_ = srv.Close()
	}
	if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// serverDrainKey holds, in the requests of Serve, the context that ends when a
// graceful shutdown begins.
type serverDrainKey struct{}

// serverStreamContext is the context an event stream runs under: the request's,
// ended also when a graceful shutdown of Serve begins. The caller calls stop
// when the stream is over.
func serverStreamContext(r *http.Request) (ctx context.Context, stop func()) {
	drain, ok := r.Context().Value(serverDrainKey{}).(context.Context)
	if !ok {
		return r.Context(), func() {}
	}
	ctx, cancel := context.WithCancel(r.Context())
	stopDrain := context.AfterFunc(drain, cancel)
	return ctx, func() { stopDrain(); cancel() }
}
