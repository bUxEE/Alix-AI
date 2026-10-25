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

	openapitransport "github.com/buxee/alix-ai/adapters/openapi"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/egress"
	ailog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// The OpenAPI/HTTP transport of master plan 9.3, registered for the endpoint kind
// `openapi` so that `alixai serve` and `alixai audit run` reach the application
// through the same adapter (transports.go). This file is the composition root's
// half of it: adapters/openapi knows nothing of packages, and here a verified
// package becomes its Config.
//
// # What a package becomes
//
//   - Endpoints. Every openapi endpoint of the manifest is one API: its base_url,
//     the OpenAPI document its spec names and the adapter configuration its
//     adapter_config names, both read from the install directory (they are signed
//     with the package and were verified when it was opened) and refused when they
//     do not parse. The document's own `servers` are never used.
//   - The mapping. The operations of every capability (read, validate, prepare,
//     commit, status) and the resolve operation of every entity type become the
//     operationIds they name, with the operation's timeout and argument renames.
//     This is the only set of operations the adapter will ever call, besides the
//     management operations (describe, health, inspect) and the provisioning
//     operations the adapter configuration names, which no capability maps.
//   - The egress allowlist. Outbound access is denied by default (master plan
//     16.1). The destinations are the endpoints' own host, port and scheme: for a
//     host that is not loopback only when network.egress lists it (the package
//     audit requires the entry for a loopback endpoint too), and for a loopback
//     host always, because the manifest itself names it. The platform's own
//     ports are refused by the dialer even so (egress_platform_deny.go, PT-001).
//     Nothing else in network.egress (the entries of RAG sources, say) is
//     reachable through this adapter.
//   - contract_sha256, when the endpoint declares it, is the pin of the approved
//     operation subset (openapi.ContractSHA256). A document whose subset hashes
//     differently is refused when the adapter is built.
//   - credential_ref is resolved by the credential provider this build has
//     (RegisterOpenAPISecretProvider; the credential service of milestone X is the
//     intended one). An endpoint that declares a reference and finds no provider
//     is refused when the adapter is built, not called without a credential:
//     the application is then unavailable (open_failed) and the audit says why.
func init() { RegisterTransport(TransportKindOpenAPI, openapiTransportOpen) }

// Bounds on the files an endpoint names.
const (
	openapiTransportMaxSpecBytes   = 4 << 20
	openapiTransportMaxConfigBytes = 1 << 20
)

var openapiTransportSecrets struct {
	mu sync.RWMutex
	p  openapitransport.SecretProvider
}

// RegisterOpenAPISecretProvider sets the credential provider every OpenAPI
// adapter this transport builds resolves its credential references with. It is
// for the credential service: it replaces a provider registered earlier, and nil
// removes it. Adapters read it when they are built, so it applies to the next
// build of an application.
func RegisterOpenAPISecretProvider(p openapitransport.SecretProvider) {
	openapiTransportSecrets.mu.Lock()
	defer openapiTransportSecrets.mu.Unlock()
	openapiTransportSecrets.p = p
}

func openapiTransportSecretProvider() openapitransport.SecretProvider {
	openapiTransportSecrets.mu.RLock()
	defer openapiTransportSecrets.mu.RUnlock()
	return openapiTransportSecrets.p
}

// openapiTransportLogger is the logger of the adapters this transport builds: JSON
// on stderr, through the redacting handler.
var openapiTransportLogger = func() *slog.Logger { return ailog.New(config.LogConfig{Level: "info"}) }

func openapiTransportOpen(ctx context.Context, req TransportRequest) (adapters.AppAdapter, error) {
	log := openapiTransportLogger()
	cfg, err := openapiTransportConfig(ctx, req, log)
	if err != nil {
		return nil, err
	}
	return openapitransport.New(cfg)
}

// openapiTransportConfig translates the package of a request into the Config of
// the adapter. It reads two files per endpoint and touches nothing else: no
// connection is made.
func openapiTransportConfig(ctx context.Context, req TransportRequest, log *slog.Logger) (openapitransport.Config, error) {
	if req.Package == nil {
		return openapitransport.Config{}, fmt.Errorf("the request carries no package")
	}
	manifest := req.Package.Manifest
	cfg := openapitransport.Config{
		ApplicationID: req.ApplicationID,
		Logger:        log,
		UserAgent:     "alixai/" + version.Get().Version,
		Secrets:       openapiTransportSecretProvider(),
	}

	var dests []egress.Destination
	endpointIDs := map[string]bool{}
	for _, te := range req.Endpoints {
		v, ok := te.Declaration.Value.(ap.ManifestOpenAPIEndpoint)
		if !ok {
			return cfg, fmt.Errorf("endpoint %q is not an openapi endpoint", te.ID)
		}
		endpointIDs[te.ID] = true
		if strings.ContainsRune(string(v.Spec), '#') {
			return cfg, fmt.Errorf("endpoint %q: spec %q must name a file, not a place in one", te.ID, string(v.Spec))
		}
		spec, err := openapiTransportReadFile(req.Dir, "spec", string(v.Spec), openapiTransportMaxSpecBytes)
		if err != nil {
			return cfg, fmt.Errorf("endpoint %q: %w", te.ID, err)
		}
		raw, err := openapiTransportReadFile(req.Dir, "adapter_config", string(v.AdapterConfig), openapiTransportMaxConfigBytes)
		if err != nil {
			return cfg, fmt.Errorf("endpoint %q: %w", te.ID, err)
		}
		settings, err := openapitransport.ParseAdapterConfig(raw, te.ID)
		if err != nil {
			return cfg, err
		}
		ep := openapitransport.Endpoint{ID: te.ID, BaseURL: string(v.BaseURL), Spec: spec, Settings: settings}
		if v.ContractSHA256 != nil {
			ep.ContractSHA256 = strings.ToLower(string(*v.ContractSHA256))
		}
		if v.CredentialRef != nil {
			ep.CredentialRef = string(*v.CredentialRef)
		}
		cfg.Endpoints = append(cfg.Endpoints, ep)

		d, err := openapiTransportDestination(te.ID, string(v.BaseURL), manifest.Network.Egress)
		if err != nil {
			return cfg, err
		}
		dests = append(dests, d)
	}

	allow, err := egress.NewAllowlist(openapiTransportDedupe(dests)...)
	if err != nil {
		return cfg, err
	}
	cfg.Dialer = egress.NewDialer(allow, platformDenyOption(), egress.WithDenyHook(func(d egress.Denial) {
		log.Warn("an OpenAPI request was refused by the egress allowlist", slog.String("application_id", req.ApplicationID),
			slog.String("host", d.Host), slog.Int("port", d.Port), slog.String("reason", string(d.Reason)))
	}))

	ids := make([]string, 0, len(req.Package.Capabilities))
	for id := range req.Package.Capabilities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := req.Package.Capabilities[id]
		oc := openapitransport.Capability{ID: id, Operations: map[adapters.Operation]openapitransport.Binding{}, Resolve: map[string]openapitransport.Binding{}}
		for op, b := range map[adapters.Operation]*ap.CapabilityOperationBinding{
			adapters.OpRead: c.Operations.Read, adapters.OpValidate: c.Operations.Validate, adapters.OpPrepare: c.Operations.Prepare,
			adapters.OpCommit: c.Operations.Commit, adapters.OpStatus: c.Operations.Status,
		} {
			if b == nil {
				continue
			}
			ob, err := openapiTransportBinding(id, string(op), *b, endpointIDs)
			if err != nil {
				return cfg, err
			}
			oc.Operations[op] = ob
		}
		for typ, et := range c.EntityTypes {
			if et.Resolve == nil {
				continue
			}
			ob, err := openapiTransportBinding(id, "entity_types."+typ+".resolve", *et.Resolve, endpointIDs)
			if err != nil {
				return cfg, err
			}
			oc.Resolve[typ] = ob
		}
		cfg.Capabilities = append(cfg.Capabilities, oc)
	}
	return cfg, ctx.Err()
}

// openapiTransportBinding translates one operation binding.
func openapiTransportBinding(capability, what string, b ap.CapabilityOperationBinding, endpoints map[string]bool) (openapitransport.Binding, error) {
	if !endpoints[string(b.Endpoint)] {
		return openapitransport.Binding{}, fmt.Errorf("capability %q: %s names the endpoint %q, which is not an openapi endpoint of the manifest", capability, what, string(b.Endpoint))
	}
	if b.OpenAPIOperationID == nil {
		return openapitransport.Binding{}, fmt.Errorf("capability %q: %s names no openapi_operation_id", capability, what)
	}
	out := openapitransport.Binding{Endpoint: string(b.Endpoint), OperationID: string(*b.OpenAPIOperationID)}
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

// openapiTransportReadFile reads a file of the package from the install
// directory. The path is the manifest's, a package-relative path with no way out
// of the directory.
func openapiTransportReadFile(dir, what, rel string, max int) ([]byte, error) {
	dir, err := relDataPackageDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w, so %s cannot be read", err, what)
	}
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return nil, fmt.Errorf("%s %q is not a path inside the package", what, rel)
	}
	f, err := os.Open(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("%s %q cannot be read: %w", what, rel, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, fmt.Errorf("%s %q cannot be read: %w", what, rel, err)
	}
	if len(b) > max {
		return nil, fmt.Errorf("%s %q is larger than %d bytes", what, rel, max)
	}
	return b, nil
}

// openapiTransportDestination is the egress destination of one endpoint's base
// URL: its own host, port and scheme. A host that is not loopback needs the
// manifest's network.egress to list that host, port and scheme; a loopback host
// is the package's own declaration, listed here because the dialer refuses
// loopback unless it is.
func openapiTransportDestination(endpointID, rawURL string, entries map[string]ap.ManifestEgressEntry) (egress.Destination, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return egress.Destination{}, fmt.Errorf("endpoint %q: the base_url does not parse", endpointID)
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	portText := u.Port()
	if portText == "" {
		portText = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return egress.Destination{}, fmt.Errorf("endpoint %q: the base_url has no usable port", endpointID)
	}
	d := egress.Destination{Host: host, Ports: []int{port}, Scheme: scheme}
	if openapiTransportLoopback(host) {
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

func openapiTransportLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// openapiTransportDedupe merges destinations that name the same host and scheme,
// so that two endpoints on one server list its ports once.
func openapiTransportDedupe(in []egress.Destination) []egress.Destination {
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
