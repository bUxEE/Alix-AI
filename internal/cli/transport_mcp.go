package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	mcptransport "github.com/buxee/alix-ai/adapters/mcp"
	openapitransport "github.com/buxee/alix-ai/adapters/openapi"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/egress"
	ailog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// The MCP transport of master plan 9.3, registered for the endpoint kind `mcp`
// so that `alixai serve` and `alixai audit run` reach the application through the
// same adapter (transports.go). This file is the composition root's half of it:
// adapters/mcp knows nothing of packages, and here a verified package becomes its
// Config.
//
// # What a package becomes
//
//   - Endpoints. Every mcp endpoint of the manifest is one server, reached over
//     streamable HTTP at the endpoint's url. A package can never name a command,
//     so no process is ever started for one (adapters/mcp can speak stdio to a
//     host that wires it in code, not here).
//   - The mapping. The operations of every capability (read, validate, prepare,
//     commit, status) and the resolve operation of every entity type become the
//     tools they name, with the operation's timeout and argument renames. This is
//     the only set of tools the adapter will ever call.
//   - The adapter configuration. The file the endpoint's adapter_config names is
//     read from the install directory (it is signed with the package and was
//     verified when the package was opened) and parsed strictly; its tools.allow
//     must list every tool the capabilities map.
//   - The egress allowlist. Outbound access is denied by default (master plan
//     16.1). The destinations are the endpoints' own host, port and scheme: for a
//     host that is not loopback only when network.egress lists it (the package
//     audit requires the entry for a loopback endpoint too), and for a loopback
//     host always, because the manifest itself names it. The platform's own
//     ports are refused by the dialer even so (egress_platform_deny.go, PT-001).
//     Nothing else in network.egress (the entries of RAG sources, say) is
//     reachable through this adapter.
//   - contract_sha256, when the endpoint declares it, is the pin of the approved
//     tool list. A listing that hashes differently refuses the mapped tools.
//   - credential_ref is resolved by the credential provider this build has
//     (RegisterOpenAPISecretProvider; the same service OpenAPI and RAG api use).
//     An endpoint that declares a reference and finds no provider is refused when
//     the adapter is built (fail closed, same rule as OpenAPI). The resolved value
//     is sent as Authorization: <scheme> <value> on streamable HTTP / SSE (header
//     and scheme from the adapter configuration, defaults Authorization / Bearer).
//     Endpoints without credential_ref send no auth header. Stdio is not used for
//     packages.
func init() { RegisterTransport(TransportKindMCP, mcpTransportOpen) }

// mcpTransportMaxConfigBytes bounds an adapter configuration file.
const mcpTransportMaxConfigBytes = 1 << 20

// mcpTransportSurfaceRecheck is how often the adapters this transport builds list
// the server tools again to check them against the pins; 0 is the adapter default
// and a negative value checks before every call. Tests set it.
var mcpTransportSurfaceRecheck time.Duration

// mcpTransportDriftObserver is called when a running MCP adapter finds that a
// tool changed after it was approved.
type mcpTransportDriftObserver func(applicationID string, d mcptransport.Drift)

type mcpTransportObserverEntry struct {
	id int
	fn mcpTransportDriftObserver
}

var mcpTransportObservers struct {
	mu   sync.Mutex
	next int
	fns  []mcpTransportObserverEntry
}

// RegisterMCPDriftObserver registers a function that is called, once per drifted
// tool, when an MCP adapter built by this transport finds that a mapped tool
// changed after it was approved (the re-audit flag of master plan 4.5; the
// adapter already refuses the tool's calls). It is for the re-audit triggers
// (M5-10, run by serve in cmd_reaudit.go) and is called from the goroutine of the
// call that found the drift, so it must not block and must not wait for anything
// that goroutine may hold (the watcher's own adapter finds drift while the watcher
// holds the lock of the application). The function it returns removes the
// observer again: serve removes its own when it stops, so a process that runs
// serve more than once (a test) keeps no observer of a stopped one.
func RegisterMCPDriftObserver(fn func(applicationID string, d mcptransport.Drift)) (remove func()) {
	if fn == nil {
		return func() {}
	}
	mcpTransportObservers.mu.Lock()
	defer mcpTransportObservers.mu.Unlock()
	mcpTransportObservers.next++
	id := mcpTransportObservers.next
	mcpTransportObservers.fns = append(mcpTransportObservers.fns, mcpTransportObserverEntry{id: id, fn: fn})
	return func() {
		mcpTransportObservers.mu.Lock()
		defer mcpTransportObservers.mu.Unlock()
		for i, e := range mcpTransportObservers.fns {
			if e.id == id {
				mcpTransportObservers.fns = append(mcpTransportObservers.fns[:i:i], mcpTransportObservers.fns[i+1:]...)
				return
			}
		}
	}
}

func mcpTransportNotify(applicationID string, d mcptransport.Drift) {
	mcpTransportObservers.mu.Lock()
	fns := append([]mcpTransportObserverEntry(nil), mcpTransportObservers.fns...)
	mcpTransportObservers.mu.Unlock()
	for _, e := range fns {
		e.fn(applicationID, d)
	}
}

// mcpTransportDrifts returns the drift the MCP adapter underneath ad has found so far, oldest
// first, or nil when ad is not an adapter this transport built. Drift is sticky for the life of
// the instance, so what it reports is about THAT instance only: the re-audit watcher asks the
// instance that serves the active package now and never one that was dropped, or an operator who
// reactivates a package would see it quarantined again by the drift of its predecessor.
func mcpTransportDrifts(ad adapters.AppAdapter) []mcptransport.Drift {
	c, ok := ad.(transportsClosing)
	if !ok {
		return nil
	}
	d, ok := c.inner.(interface{ Drifts() []mcptransport.Drift })
	if !ok {
		return nil
	}
	return d.Drifts()
}

// mcpTransportLogger is the logger of the adapters this transport builds: JSON on
// stderr, through the redacting handler.
var mcpTransportLogger = func() *slog.Logger { return ailog.New(config.LogConfig{Level: "info"}) }

func mcpTransportOpen(ctx context.Context, req TransportRequest) (adapters.AppAdapter, error) {
	log := mcpTransportLogger()
	cfg, err := mcpTransportConfig(ctx, req, log)
	if err != nil {
		return nil, err
	}
	return mcptransport.New(cfg)
}

// mcpTransportConfig translates the package of a request into the Config of the
// adapter. It reads one file per endpoint and touches nothing else: no
// connection is made.
func mcpTransportConfig(ctx context.Context, req TransportRequest, log *slog.Logger) (mcptransport.Config, error) {
	if req.Package == nil {
		return mcptransport.Config{}, fmt.Errorf("the request carries no package")
	}
	manifest := req.Package.Manifest
	cfg := mcptransport.Config{
		ApplicationID:  req.ApplicationID,
		Logger:         log,
		ClientName:     "alixai",
		ClientVersion:  version.Get().Version,
		OnDrift:        func(d mcptransport.Drift) { mcpTransportNotify(req.ApplicationID, d) },
		SurfaceRecheck: mcpTransportSurfaceRecheck,
	}

	var dests []egress.Destination
	endpointIDs := map[string]bool{}
	for _, te := range req.Endpoints {
		v, ok := te.Declaration.Value.(ap.ManifestMCPEndpoint)
		if !ok {
			return cfg, fmt.Errorf("endpoint %q is not an mcp endpoint", te.ID)
		}
		endpointIDs[te.ID] = true
		ep := mcptransport.Endpoint{ID: te.ID, URL: string(v.URL)}
		if v.ContractSHA256 != nil {
			ep.ContractSHA256 = strings.ToLower(string(*v.ContractSHA256))
		}
		raw, err := mcpTransportReadConfig(req.Dir, string(v.AdapterConfig))
		if err != nil {
			return cfg, fmt.Errorf("endpoint %q: %w", te.ID, err)
		}
		ac, err := mcptransport.ParseAdapterConfig(raw, te.ID)
		if err != nil {
			return cfg, err
		}
		ep.Allow = ac.Tools.Allow
		if v.CredentialRef != nil {
			ref := string(*v.CredentialRef)
			secrets := openapiTransportSecretProvider()
			if secrets == nil {
				return cfg, fmt.Errorf("endpoint %q declares a credential reference and no credential provider is configured", te.ID)
			}
			header, scheme := mcpTransportAuth(ac)
			ep.AuthHeader, ep.AuthScheme = header, scheme
			ep.Authorization = mcpTransportAuthorization(secrets, ref, scheme)
		}
		cfg.Endpoints = append(cfg.Endpoints, ep)

		d, err := mcpTransportDestination(te.ID, string(v.URL), manifest.Network.Egress)
		if err != nil {
			return cfg, err
		}
		dests = append(dests, d)
	}

	allow, err := egress.NewAllowlist(mcpTransportDedupe(dests)...)
	if err != nil {
		return cfg, err
	}
	cfg.Dialer = egress.NewDialer(allow, platformDenyOption(), egress.WithDenyHook(func(d egress.Denial) {
		log.Warn("an MCP connection was refused by the egress allowlist", slog.String("application_id", req.ApplicationID),
			slog.String("host", d.Host), slog.Int("port", d.Port), slog.String("reason", string(d.Reason)))
	}))

	ids := make([]string, 0, len(req.Package.Capabilities))
	for id := range req.Package.Capabilities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := req.Package.Capabilities[id]
		mc := mcptransport.Capability{ID: id, Operations: map[adapters.Operation]mcptransport.Binding{}, Resolve: map[string]mcptransport.Binding{}}
		for op, b := range map[adapters.Operation]*ap.CapabilityOperationBinding{
			adapters.OpRead: c.Operations.Read, adapters.OpValidate: c.Operations.Validate, adapters.OpPrepare: c.Operations.Prepare,
			adapters.OpCommit: c.Operations.Commit, adapters.OpStatus: c.Operations.Status,
		} {
			if b == nil {
				continue
			}
			mb, err := mcpTransportBinding(id, string(op), *b, endpointIDs)
			if err != nil {
				return cfg, err
			}
			mc.Operations[op] = mb
		}
		for typ, et := range c.EntityTypes {
			if et.Resolve == nil {
				continue
			}
			mb, err := mcpTransportBinding(id, "entity_types."+typ+".resolve", *et.Resolve, endpointIDs)
			if err != nil {
				return cfg, err
			}
			mc.Resolve[typ] = mb
		}
		cfg.Capabilities = append(cfg.Capabilities, mc)
	}
	return cfg, ctx.Err()
}

// mcpTransportAuth returns the header and scheme from an adapter configuration
// (defaults Authorization / Bearer).
func mcpTransportAuth(ac mcptransport.AdapterConfig) (header, scheme string) {
	header, scheme = mcptransport.DefaultAuthHeader, mcptransport.DefaultAuthScheme
	if ac.Auth == nil {
		return header, scheme
	}
	if ac.Auth.Header != "" {
		header = ac.Auth.Header
	}
	if ac.Auth.Scheme != nil {
		scheme = *ac.Auth.Scheme
	}
	return header, scheme
}

// mcpTransportAuthorization builds the per-request Authorization value from the
// shared credential provider (scheme plus secret, or bare secret when scheme is "").
func mcpTransportAuthorization(secrets openapitransport.SecretProvider, ref, scheme string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		sec, err := secrets.Secret(ctx, ref)
		if err != nil {
			return "", err
		}
		v := sec.Reveal()
		if scheme == "" {
			return v, nil
		}
		return scheme + " " + v, nil
	}
}

// mcpTransportBinding translates one operation binding.
func mcpTransportBinding(capability, what string, b ap.CapabilityOperationBinding, endpoints map[string]bool) (mcptransport.Binding, error) {
	if !endpoints[string(b.Endpoint)] {
		return mcptransport.Binding{}, fmt.Errorf("capability %q: %s names the endpoint %q, which is not an mcp endpoint of the manifest", capability, what, string(b.Endpoint))
	}
	if b.MCPTool == nil {
		return mcptransport.Binding{}, fmt.Errorf("capability %q: %s names no mcp_tool", capability, what)
	}
	out := mcptransport.Binding{Endpoint: string(b.Endpoint), Tool: string(*b.MCPTool)}
	if b.TimeoutMS != nil {
		out.Timeout = time.Duration(*b.TimeoutMS) * time.Millisecond
	}
	if len(b.Arguments) > 0 {
		out.Arguments = make(map[string]string, len(b.Arguments))
		for from, to := range b.Arguments {
			out.Arguments[from] = string(to)
		}
	}
	return out, nil
}

// relDataPackageDir is the absolute install directory of a package. The
// directory is built from data_dir, and data_dir may be relative (the shipped
// local-machine guide uses ./data): a relative data_dir is relative to the
// working directory of the process, as the database path is, so it is made
// absolute here once, for every transport, before a file is read. An empty
// directory stays an error: it would read from the working directory.
func relDataPackageDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("the package directory is not known")
	}
	if filepath.IsAbs(dir) {
		return dir, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("the package directory %q cannot be made absolute: %w", dir, err)
	}
	return abs, nil
}

// mcpTransportReadConfig reads an endpoint's adapter configuration from the
// install directory. The path is the manifest's, a package-relative path with no
// way out of the directory.
func mcpTransportReadConfig(dir, rel string) ([]byte, error) {
	dir, err := relDataPackageDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w, so adapter_config cannot be read", err)
	}
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return nil, fmt.Errorf("adapter_config %q is not a path inside the package", rel)
	}
	f, err := os.Open(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("adapter_config %q cannot be read: %w", rel, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, mcpTransportMaxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("adapter_config %q cannot be read: %w", rel, err)
	}
	if len(b) > mcpTransportMaxConfigBytes {
		return nil, fmt.Errorf("adapter_config %q is larger than %d bytes", rel, mcpTransportMaxConfigBytes)
	}
	return b, nil
}

// mcpTransportDestination is the egress destination of one endpoint's URL: its
// own host, port and scheme. A host that is not loopback needs the manifest's
// network.egress to list that host, port and scheme; a loopback host is the
// package's own declaration, listed here because the dialer refuses loopback
// unless it is.
func mcpTransportDestination(endpointID, rawURL string, entries map[string]ap.ManifestEgressEntry) (egress.Destination, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return egress.Destination{}, fmt.Errorf("endpoint %q: the url does not parse", endpointID)
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	portText := u.Port()
	if portText == "" {
		portText = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return egress.Destination{}, fmt.Errorf("endpoint %q: the url has no usable port", endpointID)
	}
	d := egress.Destination{Host: host, Ports: []int{port}, Scheme: scheme}
	if mcpTransportLoopback(host) {
		return d, nil
	}
	for _, e := range entries {
		if strings.ToLower(string(e.Host)) != host || string(e.Scheme) != scheme {
			continue
		}
		for _, p := range e.Ports {
			if int(p) == port {
				return d, nil
			}
		}
	}
	return egress.Destination{}, contracts.New(contracts.ErrValidationFailed,
		fmt.Sprintf("endpoint %q: network.egress has no entry for its host, port and scheme, so it cannot be reached", endpointID))
}

func mcpTransportLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// mcpTransportDedupe merges destinations that name the same host and scheme, so
// that two endpoints on one server list its ports once.
func mcpTransportDedupe(in []egress.Destination) []egress.Destination {
	type key struct{ host, scheme string }
	merged := map[key]*egress.Destination{}
	var order []key
	for _, d := range in {
		k := key{d.Host, d.Scheme}
		if m, ok := merged[k]; ok {
			for _, p := range d.Ports {
				found := false
				for _, q := range m.Ports {
					found = found || p == q
				}
				if !found {
					m.Ports = append(m.Ports, p)
				}
			}
			continue
		}
		c := d
		c.Ports = append([]int(nil), d.Ports...)
		merged[k] = &c
		order = append(order, k)
	}
	out := make([]egress.Destination, 0, len(order))
	for _, k := range order {
		out = append(out, *merged[k])
	}
	return out
}
