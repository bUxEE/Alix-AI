package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	nativetransport "github.com/buxee/alix-ai/adapters/native"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/egress"
	ailog "github.com/buxee/alix-ai/internal/log"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// The native SDK transport of master plan 9.3, registered for the endpoint kind
// `native` so that `alixai serve` and `alixai audit run` reach the application
// through the same adapter (transports.go). This file is the composition root's
// half of it: adapters/native knows nothing of packages, and here a verified
// package becomes its Config.
//
// # What a package becomes
//
//   - Endpoints. Every native endpoint of the manifest is one bridge, reached over
//     the local IPC it declares: a Unix socket, or loopback TCP. A named pipe is
//     declared by the manifest schema and not supported by this build: the
//     application is then unavailable (open_failed) and the audit says why, rather
//     than the declaration being mapped to something else.
//   - The mapping. The operations of every capability (read, validate, prepare,
//     commit, status) and the resolve operation of every entity type become the
//     native_method they name, with the operation's timeout and argument renames.
//     This is the only set of methods the adapter will ever call, besides the
//     protocol's own (describe, health, inspect) and the provisioning operations
//     the adapter configuration approves, which no capability maps.
//   - The adapter configuration. The file the endpoint's adapter_config names is
//     read from the install directory (it is signed with the package and was
//     verified when the package was opened) and parsed strictly. It names the key
//     id and a reference to the application key (env:NAME or file:/path, owner-only),
//     never the key: a package is signed, distributed and read by people. A
//     reference that does not resolve refuses the application (open_failed); the
//     adapter never connects without the key, because the key is what
//     authenticates the bridge to AlixAI and AlixAI to the bridge (SEC-038).
//     Other reference forms (the credential service of milestone X) have no
//     resolver in this build.
//   - The egress allowlist. Outbound access is denied by default (master plan
//     16.1). The manifest allows a native endpoint only a Unix socket, a named pipe
//     or a loopback port, so network.egress has nothing to say about it: the
//     allowlist is the declared loopback host and port of each TCP endpoint, which
//     the dialer refuses unless it is listed, and nothing else. A Unix socket is
//     not network egress and is guarded by its file permissions instead.
func init() { RegisterTransport(TransportKindNative, nativeTransportOpen) }

// Bounds on the files an endpoint names.
const nativeTransportMaxConfigBytes = 1 << 20

// nativeTransportLogger is the logger of the adapters this transport builds: JSON
// on stderr, through the redacting handler.
var nativeTransportLogger = func() *slog.Logger { return ailog.New(config.LogConfig{Level: "info"}) }

func nativeTransportOpen(ctx context.Context, req TransportRequest) (adapters.AppAdapter, error) {
	cfg, err := nativeTransportConfig(ctx, req, nativeTransportLogger())
	if err != nil {
		return nil, err
	}
	return nativetransport.New(cfg)
}

// nativeTransportConfig translates the package of a request into the Config of
// the adapter. It reads one file per endpoint and the key each file refers to,
// and opens no connection.
func nativeTransportConfig(ctx context.Context, req TransportRequest, log *slog.Logger) (nativetransport.Config, error) {
	if req.Package == nil {
		return nativetransport.Config{}, fmt.Errorf("the request carries no package")
	}
	cfg := nativetransport.Config{ApplicationID: req.ApplicationID, Logger: log}

	var dests []egress.Destination
	endpointIDs := map[string]bool{}
	for _, te := range req.Endpoints {
		v, ok := te.Declaration.Value.(ap.ManifestNativeEndpoint)
		if !ok {
			return cfg, fmt.Errorf("endpoint %q is not a native endpoint", te.ID)
		}
		endpointIDs[te.ID] = true
		raw, err := nativeTransportReadFile(req.Dir, "adapter_config", string(v.AdapterConfig), nativeTransportMaxConfigBytes)
		if err != nil {
			return cfg, fmt.Errorf("endpoint %q: %w", te.ID, err)
		}
		settings, err := nativetransport.ParseAdapterConfig(raw, te.ID)
		if err != nil {
			return cfg, err
		}
		key, err := nativetransport.ResolveKey(settings.Key.Ref)
		if err != nil {
			return cfg, fmt.Errorf("endpoint %q: the application key cannot be read: %w", te.ID, err)
		}
		ep := nativetransport.Endpoint{
			ID: te.ID, Address: v.IPC.Address, KeyID: settings.Key.ID, Key: key, Provisioning: settings.Management.Provisioning,
		}
		switch v.IPC.Kind {
		case ap.ManifestNativeIPCKindUnixSocket:
			ep.Network = nativetransport.NetworkUnix
		case ap.ManifestNativeIPCKindLoopbackTCP:
			ep.Network = nativetransport.NetworkTCP
			host, port, err := nativeTransportLoopback(v.IPC.Address)
			if err != nil {
				return cfg, fmt.Errorf("endpoint %q: %w", te.ID, err)
			}
			dests = append(dests, egress.Destination{Host: host, Ports: []int{port}, Scheme: egress.SchemeHTTP})
		default:
			return cfg, fmt.Errorf("endpoint %q: the IPC kind %q is not supported by this build (use a unix_socket or loopback_tcp)", te.ID, string(v.IPC.Kind))
		}
		if settings.PeerUID != nil {
			want := *settings.PeerUID
			ep.PeerCheck = func(c nativetransport.PeerCreds) error {
				if c.UID != want {
					return fmt.Errorf("the bridge runs as user %d, not the %d the adapter configuration names", c.UID, want)
				}
				return nil
			}
		}
		cfg.Endpoints = append(cfg.Endpoints, ep)
	}

	if len(dests) > 0 {
		allow, err := egress.NewAllowlist(nativeTransportDedupe(dests)...)
		if err != nil {
			return cfg, err
		}
		cfg.Dialer = egress.NewDialer(allow, platformDenyOption(), egress.WithDenyHook(func(d egress.Denial) {
			log.Warn("a native bridge connection was refused by the egress allowlist", slog.String("application_id", req.ApplicationID),
				slog.String("host", d.Host), slog.Int("port", d.Port), slog.String("reason", string(d.Reason)))
		}))
	}

	ids := make([]string, 0, len(req.Package.Capabilities))
	for id := range req.Package.Capabilities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := req.Package.Capabilities[id]
		nc := nativetransport.Capability{ID: id, Operations: map[adapters.Operation]nativetransport.Binding{}, Resolve: map[string]nativetransport.Binding{}}
		for op, b := range map[adapters.Operation]*ap.CapabilityOperationBinding{
			adapters.OpRead: c.Operations.Read, adapters.OpValidate: c.Operations.Validate, adapters.OpPrepare: c.Operations.Prepare,
			adapters.OpCommit: c.Operations.Commit, adapters.OpStatus: c.Operations.Status,
		} {
			if b == nil {
				continue
			}
			nb, err := nativeTransportBinding(id, string(op), *b, endpointIDs)
			if err != nil {
				return cfg, err
			}
			nc.Operations[op] = nb
		}
		for typ, et := range c.EntityTypes {
			if et.Resolve == nil {
				continue
			}
			nb, err := nativeTransportBinding(id, "entity_types."+typ+".resolve", *et.Resolve, endpointIDs)
			if err != nil {
				return cfg, err
			}
			nc.Resolve[typ] = nb
		}
		cfg.Capabilities = append(cfg.Capabilities, nc)
	}
	return cfg, ctx.Err()
}

// nativeTransportBinding translates one operation binding.
func nativeTransportBinding(capability, what string, b ap.CapabilityOperationBinding, endpoints map[string]bool) (nativetransport.Binding, error) {
	if !endpoints[string(b.Endpoint)] {
		return nativetransport.Binding{}, fmt.Errorf("capability %q: %s names the endpoint %q, which is not a native endpoint of the manifest", capability, what, string(b.Endpoint))
	}
	if b.NativeMethod == nil {
		return nativetransport.Binding{}, fmt.Errorf("capability %q: %s names no native_method", capability, what)
	}
	out := nativetransport.Binding{Endpoint: string(b.Endpoint), Method: string(*b.NativeMethod)}
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

// nativeTransportReadFile reads a file of the package from the install directory.
// The path is the manifest's, a package-relative path with no way out of the
// directory.
func nativeTransportReadFile(dir, what, rel string, max int) ([]byte, error) {
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

// nativeTransportLoopback splits the host and port of a loopback_tcp address,
// which the manifest limits to 127.0.0.1, [::1] or localhost with a port. The
// brackets of an IPv6 literal are not part of the allowlist's host.
func nativeTransportLoopback(address string) (host string, port int, err error) {
	h, p, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, fmt.Errorf("the loopback address %q is not host:port", address)
	}
	if port, err = strconv.Atoi(p); err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("the loopback address %q has no usable port", address)
	}
	return strings.ToLower(h), port, nil
}

// nativeTransportDedupe merges destinations of one host, so that two endpoints on
// one host list its ports once.
func nativeTransportDedupe(in []egress.Destination) []egress.Destination {
	merged := map[string]*egress.Destination{}
	var order []string
	for _, d := range in {
		if m, ok := merged[d.Host]; ok {
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
		merged[d.Host] = &c
		order = append(order, d.Host)
	}
	out := make([]egress.Destination, 0, len(order))
	for _, h := range order {
		out = append(out, *merged[h])
	}
	return out
}
