package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// This file is the composition seam between the application transports and the
// two processes that need an adapter, and between the CLI and a running server.
//
// # One registry, keyed by the declared transport kind
//
// A package's manifest declares its endpoints (alixai.yaml, endpoints:), and
// each endpoint has a transport: mcp, openapi, native or events. The operation
// transports register themselves, each from its own file, with
//
//	func init() { RegisterTransport(TransportKindMCP, openMCP) }   // transport_mcp.go
//
// and neither the registry, cmd_serve.go nor cmd_audit.go is edited to add one.
// `alixai serve` (adapterSource) and `alixai audit run` both build their adapter
// through transportsSource.adapter, so there is one lookup, one call to the
// factory and one adapters.Wrap, and the audit inspects exactly what serve will
// call. A factory gets a TransportRequest: the verified package, its install
// record and directory, and the endpoints of the kind it serves.
//
// # Nothing is skipped silently
//
// An application is served by one operation transport kind. When the manifest
// declares an endpoint kind nobody registered, declares none, or declares more
// than one kind, there is no adapter and the reason is said, never defaulted
// away: serve answers every call for the application with unavailable (and logs
// why, once per package), and the audit records the application and runtime
// targets as unverified, prints why and names it in its output. events endpoints
// carry no operations (the manifest check refuses one that owns an operation)
// and have no adapter in this build; they are reported as declared, not as a
// failure.
//
// # The server's admin interface
//
// A CLI process cannot reach a running server's memory, and the capability
// registry caches an entry for 30 seconds. The server therefore listens on a
// loopback-only admin interface (gateway.AdminServer) and records where in
// <data_dir>/admin-endpoint.json; transportsServer is the
// integrationaudit.CapabilityRegistry of the CLI's runner, so activating a
// package tells the server to refresh and quarantining one tells it to evict, at
// once. With no server running it says so and succeeds (the store is the
// authority; a server started later reads it); with a server that is reachable
// and does not confirm, the command fails.

// The transport kinds that carry application operations: the kinds a manifest
// endpoint can declare that have an adapters.AppAdapter behind them.
const (
	TransportKindMCP     = "mcp"
	TransportKindOpenAPI = "openapi"
	TransportKindNative  = "native"
)

var transportsOperationKinds = []string{TransportKindMCP, TransportKindOpenAPI, TransportKindNative}

// TransportEndpoint is one endpoint a manifest declares, as the transport that
// serves its kind receives it.
type TransportEndpoint struct {
	// ID is the endpoint's key in the manifest's endpoints map.
	ID string
	// Declaration is the typed endpoint: a ap.ManifestMCPEndpoint,
	// ap.ManifestOpenAPIEndpoint or ap.ManifestNativeEndpoint in Value.
	Declaration ap.ManifestEndpoint
}

// TransportRequest is everything a transport factory is given.
type TransportRequest struct {
	ApplicationID string
	// Kind is the transport kind being opened.
	Kind string
	// Endpoints are the endpoints of that kind the manifest declares, by ID.
	// There is at least one; whether a transport accepts several is its own
	// decision (and an error of its own).
	Endpoints []TransportEndpoint
	// Package is the loaded, verified package (its manifest, capabilities and
	// network.egress). It is never nil.
	Package *apppkg.Package
	// Record is the install record: manifest hash, state (quarantined for an
	// audit, active for serve) and the install directory relative to DataDir.
	Record apppkg.Record
	// Dir is the absolute install directory: the package's signed files,
	// adapter_config files included, are below it.
	Dir string
}

// TransportFactory builds the transport adapter of one application. It returns
// the raw adapter: the caller wraps it with adapters.Wrap (scope check, bounds,
// result re-validation), so a factory never does and never needs to. ctx bounds
// building (dialling, a handshake) and must not be kept: the adapter lives as
// long as the package does.
//
// Serve calls a factory once per application at a time and once for each
// package hash that application is served with: a package that is superseded, or
// evicted by the admin interface, is built again on its next use. An audit calls
// it once per run. When the adapter a factory returns implements io.Closer, the
// core closes it when it is dropped (the package is superseded or evicted, the
// service stops, the audit ends); the adapter must tolerate calls still in
// flight when that happens.
type TransportFactory func(ctx context.Context, req TransportRequest) (adapters.AppAdapter, error)

// RegisterTransport adds the factory for a transport kind. It is called from the
// init function of the transport's own file (transport_<kind>.go). It panics on
// a kind that carries no operations (events, or a typo), a nil factory and a
// second factory for the same kind: programming errors found when the binary
// starts, not at the first request.
func RegisterTransport(kind string, factory TransportFactory) {
	transportsDefault.register(kind, factory)
}

type transportsRegistry struct {
	mu     sync.RWMutex
	byKind map[string]TransportFactory
}

func newTransportsRegistry() *transportsRegistry {
	return &transportsRegistry{byKind: map[string]TransportFactory{}}
}

// transportsDefault is the registry of this process, the one serve and audit
// consult. Tests swap it (transportsSwapDefault).
var transportsDefault = newTransportsRegistry()

func (r *transportsRegistry) register(kind string, factory TransportFactory) {
	known := false
	for _, k := range transportsOperationKinds {
		known = known || k == kind
	}
	switch {
	case kind == "events":
		panic(`cli: no adapter transport serves "events" endpoints: they carry no operations`)
	case !known:
		panic(fmt.Sprintf("cli: %q is not an operation transport kind (want %s)", kind, strings.Join(transportsOperationKinds, ", ")))
	case factory == nil:
		panic("cli: the " + kind + " transport has no factory")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byKind[kind]; dup {
		panic("cli: the " + kind + " transport is registered twice")
	}
	r.byKind[kind] = factory
}

func (r *transportsRegistry) lookup(kind string) (TransportFactory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.byKind[kind]
	return f, ok
}

// kinds lists the registered kinds, sorted.
func (r *transportsRegistry) kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byKind))
	for k := range r.byKind {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The reasons there is no adapter for a package, in Details["transport_reason"]
// of the unavailable error.
const (
	// transportsReasonNotRegistered: the manifest declares a kind and this
	// binary has no transport for it.
	transportsReasonNotRegistered = "not_registered"
	// transportsReasonNoEndpoint: the manifest declares no mcp, openapi or
	// native endpoint.
	transportsReasonNoEndpoint = "no_operation_endpoint"
	// transportsReasonMixedKinds: the manifest declares operation endpoints of
	// more than one kind.
	transportsReasonMixedKinds = "mixed_kinds"
	// transportsReasonOpenFailed: the transport is registered and could not be
	// opened (the application is unreachable, its adapter config is refused).
	transportsReasonOpenFailed = "open_failed"
	// transportsReasonPackage: the installed package could not be opened.
	transportsReasonPackage = "package_unreadable"
)

// transportsUnavailableReason reports why err says there is no adapter, when it
// is one of this file's errors.
func transportsUnavailableReason(err error) (reason, kind string, ok bool) {
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce == nil || ce.Code != contracts.ErrUnavailable {
		return "", "", false
	}
	reason, ok = ce.Details["transport_reason"].(string)
	kind, _ = ce.Details["transport_kind"].(string)
	return reason, kind, ok
}

// transportsNoAdapter reports whether err is a deterministic "this package has
// no adapter in this build" (as opposed to one that failed to open): the
// answer is the same on every call for the same package.
func transportsNoAdapter(err error) bool {
	switch reason, _, _ := transportsUnavailableReason(err); reason {
	case transportsReasonNotRegistered, transportsReasonNoEndpoint, transportsReasonMixedKinds:
		return true
	}
	return false
}

func transportsUnavailable(reason, kind string, cause error, format string, a ...any) *contracts.Error {
	e := contracts.Wrap(contracts.ErrUnavailable, cause, fmt.Sprintf(format, a...)).WithDetails("transport_reason", reason)
	if kind != "" {
		e.WithDetails("transport_kind", kind)
	}
	return e
}

// transportsPlan is what a manifest declares for the adapter.
type transportsPlan struct {
	// Kind is the one operation transport kind the manifest declares.
	Kind string
	// Endpoints are its endpoints of that kind, sorted by ID.
	Endpoints []TransportEndpoint
	// Events are the IDs of the declared events endpoints, which no adapter of
	// this build consumes.
	Events []string
}

// transportsPlanFor reads the endpoints a manifest declares and picks the
// transport kind. It is pure: nothing is looked up or opened.
func transportsPlanFor(m ap.Manifest) (transportsPlan, error) {
	ids := make([]string, 0, len(m.Endpoints))
	for id := range m.Endpoints {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	byKind := map[string][]TransportEndpoint{}
	var plan transportsPlan
	for _, id := range ids {
		ep := m.Endpoints[id]
		switch ep.Value.(type) {
		case ap.ManifestMCPEndpoint:
			byKind[TransportKindMCP] = append(byKind[TransportKindMCP], TransportEndpoint{ID: id, Declaration: ep})
		case ap.ManifestOpenAPIEndpoint:
			byKind[TransportKindOpenAPI] = append(byKind[TransportKindOpenAPI], TransportEndpoint{ID: id, Declaration: ep})
		case ap.ManifestNativeEndpoint:
			byKind[TransportKindNative] = append(byKind[TransportKindNative], TransportEndpoint{ID: id, Declaration: ep})
		case ap.ManifestEventsEndpoint:
			plan.Events = append(plan.Events, id)
		default:
			return transportsPlan{}, transportsUnavailable(transportsReasonNoEndpoint, "", nil,
				"the manifest's endpoint %q declares no known transport", id)
		}
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	switch len(kinds) {
	case 0:
		return plan, transportsUnavailable(transportsReasonNoEndpoint, "", nil,
			"the manifest declares no mcp, openapi or native endpoint, so the application has no operations to reach")
	case 1:
		plan.Kind, plan.Endpoints = kinds[0], byKind[kinds[0]]
		return plan, nil
	}
	return plan, transportsUnavailable(transportsReasonMixedKinds, "", nil,
		"the manifest declares operation endpoints of more than one transport (%s): this build reaches an application through one transport kind",
		strings.Join(kinds, ", "))
}

// transportsSource turns an installed package into the adapter that reaches its
// application. Serve and audit both hold one and call adapter: that is what
// makes "one registry for both" true by construction.
type transportsSource struct {
	// registry is the registry consulted; nil means the process's own.
	registry  *transportsRegistry
	installer *apppkg.Installer
	dataDir   string
}

func (s transportsSource) reg() *transportsRegistry {
	if s.registry != nil {
		return s.registry
	}
	return transportsDefault
}

// adapter opens the package with the given manifest hash through the installer
// (which re-verifies it), picks the transport kind it declares, builds the
// adapter with the factory registered for that kind and wraps it with
// adapters.Wrap. The returned plan is set whenever the package could be read,
// also when there is no adapter. Every failure is an unavailable error whose
// Details["transport_reason"] says which (transportsUnavailableReason).
func (s transportsSource) adapter(ctx context.Context, applicationID, manifestHash string) (adapters.AppAdapter, *transportsPlan, error) {
	pkg, rec, err := s.installer.Open(ctx, applicationID, manifestHash)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, err
		}
		return nil, nil, transportsUnavailable(transportsReasonPackage, "", err, "the installed package could not be opened to build its adapter")
	}
	return s.build(ctx, applicationID, pkg, rec)
}

// build is adapter for a package that is already open: it reads the declared
// endpoints, picks the factory by kind, calls it and wraps what it returns.
func (s transportsSource) build(ctx context.Context, applicationID string, pkg *apppkg.Package, rec apppkg.Record) (adapters.AppAdapter, *transportsPlan, error) {
	plan, err := transportsPlanFor(pkg.Manifest)
	if err != nil {
		return nil, &plan, err
	}
	factory, ok := s.reg().lookup(plan.Kind)
	if !ok {
		have := s.reg().kinds()
		if len(have) == 0 {
			have = []string{"none"}
		}
		return nil, &plan, transportsUnavailable(transportsReasonNotRegistered, plan.Kind, nil,
			"no adapter transport is registered for the declared endpoint kind %q (registered in this build: %s)", plan.Kind, strings.Join(have, ", "))
	}
	inner, err := factory(ctx, TransportRequest{
		ApplicationID: applicationID, Kind: plan.Kind, Endpoints: plan.Endpoints, Package: pkg, Record: rec,
		Dir: filepath.Join(s.dataDir, filepath.FromSlash(rec.Path)),
	})
	if err == nil && inner == nil {
		err = errors.New("the transport returned no adapter")
	}
	if err != nil {
		return nil, &plan, transportsUnavailable(transportsReasonOpenFailed, plan.Kind, err, "the %s adapter of the application could not be opened", plan.Kind)
	}
	ad, err := adapters.Wrap(inner, adapters.Options{ApplicationID: applicationID})
	if err != nil {
		transportsClose(inner)
		return nil, &plan, err
	}
	return transportsClosing{AppAdapter: ad, inner: inner}, &plan, nil
}

// transportsClosing is the wrapped adapter of a package that can also release
// what the transport underneath holds: Close closes the transport when it
// implements io.Closer. The wrapped adapter is what every call goes through; the
// transport itself is never reachable without it.
type transportsClosing struct {
	adapters.AppAdapter
	inner adapters.AppAdapter
}

// Close implements io.Closer.
func (c transportsClosing) Close() error {
	if cl, ok := c.inner.(io.Closer); ok {
		return cl.Close()
	}
	return nil
}

// transportsClose closes an adapter built by transportsSource when it can be
// closed. A nil adapter, or one that cannot, is not an error.
func transportsClose(ad adapters.AppAdapter) {
	if c, ok := ad.(io.Closer); ok {
		_ = c.Close()
	}
}

// ---- the running server -----------------------------------------------------

// transportsServerOut is what the commands print about the running server.
type transportsServerOut struct {
	// Status is one of: refreshed, evicted (the server confirmed), no_server
	// (none is running for this data directory: nothing to tell), failed (one is
	// reachable, or its endpoint record cannot be used, and it did not confirm).
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

const (
	transportsServerRefreshed = "refreshed"
	transportsServerEvicted   = "evicted"
	transportsServerNone      = "no_server"
	transportsServerFailed    = "failed"
)

// transportsServer is the integrationaudit.CapabilityRegistry of a CLI process:
// it tells the server that is running on the same data directory, through its
// admin interface, to refresh or evict an application. What happened is kept for
// the command to print and to fail on.
type transportsServer struct {
	dataDir string

	mu   sync.Mutex
	out  *transportsServerOut
	fail error
}

func newTransportsServer(dataDir string) *transportsServer {
	return &transportsServer{dataDir: dataDir}
}

// Refresh implements integrationaudit.CapabilityRegistry.
func (n *transportsServer) Refresh(ctx context.Context, applicationID string) error {
	return n.tell(ctx, applicationID, transportsServerRefreshed, func(c *gateway.AdminClient) error {
		return c.Refresh(ctx, applicationID)
	})
}

// Deactivate implements integrationaudit.CapabilityRegistry. The interface has
// no error to return; the outcome is read with result.
func (n *transportsServer) Deactivate(applicationID string) {
	_ = n.tell(context.Background(), applicationID, transportsServerEvicted, func(c *gateway.AdminClient) error {
		return c.Deactivate(context.Background(), applicationID)
	})
}

func (n *transportsServer) tell(_ context.Context, app, okStatus string, call func(*gateway.AdminClient) error) error {
	set := func(status, detail string, err error) error {
		n.mu.Lock()
		n.out, n.fail = &transportsServerOut{Status: status, Detail: detail}, err
		n.mu.Unlock()
		return err
	}
	ep, err := gateway.ReadAdminEndpoint(n.dataDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return set(transportsServerNone, "no running server records an admin endpoint in this data directory; a server started later reads the store, and one that is running without it refreshes within 30 seconds", nil)
	case err != nil:
		// A file is there and cannot be used: a server may be running that this
		// process cannot reach.
		return set(transportsServerFailed, err.Error(), fmt.Errorf("a running server may not have been told about %s: %w", app, err))
	}
	client, err := gateway.NewAdminClient(ep)
	if err != nil {
		return set(transportsServerFailed, err.Error(), fmt.Errorf("a running server may not have been told about %s: %w", app, err))
	}
	switch err := call(client); {
	case err == nil:
		return set(okStatus, "", nil)
	case errors.Is(err, gateway.ErrAdminUnreachable):
		return set(transportsServerNone, "the admin endpoint file is stale: the server that wrote it is not running; a server started later reads the store", nil)
	default:
		return set(transportsServerFailed, err.Error(), fmt.Errorf("the running server did not confirm for %s: %w", app, err))
	}
}

// transportsNotifyRefresh is the one line a command that changes what a package
// source reports (an install, a key revocation) adds to tell a running server to
// reload an application: it asks, prints what came of it on stderr and returns
// the outcome and the error a reachable server that did not confirm gives. With
// no server running the error is nil.
func transportsNotifyRefresh(ctx context.Context, env *Env, dataDir, applicationID string) (*transportsServerOut, error) {
	n := newTransportsServer(dataDir)
	err := n.Refresh(ctx, applicationID)
	out, _ := n.result()
	if out != nil {
		fmt.Fprintf(env.Stderr, "alixai: note: %s\n", out.note(applicationID))
	}
	return out, err
}

// result is what the last call found, or nil when no call was made, and the
// error when a server that should have been told was not.
func (n *transportsServer) result() (*transportsServerOut, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.out, n.fail
}

// note renders the outcome as the line a command prints on stderr.
func (o transportsServerOut) note(app string) string {
	switch o.Status {
	case transportsServerRefreshed:
		return fmt.Sprintf("the running server reloaded the capability registry for %s", app)
	case transportsServerEvicted:
		return fmt.Sprintf("the running server evicted %s from its capability registry; it serves nothing for it now", app)
	case transportsServerNone:
		return "no server was told: " + o.Detail
	}
	return "the running server was NOT confirmed to have refreshed " + app + ": " + o.Detail
}
