package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/integrationaudit/probes"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The exported package-to-adapter builder.
//
// `alixai serve` and `alixai audit run` reach an application through transportsSource: the manifest's
// endpoints pick one transport kind (mcp, openapi or native), the factory registered for the kind
// (transport_mcp.go, transport_openapi.go, transport_native.go) turns the verified package into its
// adapter, and the result is wrapped by adapters.Wrap. Everything else that has to reach the application
// the way those two do (the conformance conversation stage of cmd/alixai-conformance, a test rig) used
// to rebuild the MCP half of that by hand and so could drive MCP packages only. BuildPackageAdapter is
// the same code path for any caller: it adds no mapping of its own, so a fourth transport that registers
// itself is reachable from here without a line changed, and a package that is served the way the audit
// saw it is the one that is driven.
//
// What a caller gets is the transport's own adapter (PackageAdapter.Adapter), not the wrapped one: a
// caller that sits between the transport and adapters.Wrap (the conformance stage records exactly what
// the application is asked, which a call the wrapper refuses never is) puts its own wrapper on top.
//
// # The provisioner
//
// The disposable tenants of a package's test suites are created, seeded and removed by the provisioning
// bridge of the transport (transport_*_provision.go), which the integration audit uses through a
// probes.ProvisionerSource. Those sources recognise the adapter by its concrete type, so a caller that
// wrapped the adapter would get none: PackageAdapter.Provisioner asks them on the transport's own
// adapter, in the order of the registered kinds, and answers nil, nil when the package declares no
// provisioning calls or the transport has no bridge, as the audit does.

// BuildRequest names the package to reach.
type BuildRequest struct {
	// ApplicationID is the application the adapter serves.
	ApplicationID string
	// Package is the loaded package. Its manifest's endpoints are what the adapter connects to, so a
	// caller that points a package at an application under test edits them before building.
	Package *apppkg.Package
	// ManifestHash identifies the package to the provisioning bridge, which remembers where it opened
	// it from. Any value that stays the same between BuildPackageAdapter and the provisioner is
	// enough; the package's real hash when the caller has it.
	ManifestHash string
	// Dir is the directory of the package's signed files, adapter_config files included. It is made
	// absolute before it is used.
	Dir string
}

// PackageAdapter is the adapter of one package, built by the transport its manifest declares.
type PackageAdapter struct {
	kind  string
	req   BuildRequest
	rec   apppkg.Record
	built adapters.AppAdapter // what transportsSource.build returned: the wrapped adapter, closing the transport
	raw   adapters.AppAdapter // the transport's own adapter underneath
}

// BuildPackageAdapter builds the adapter of the package the way serve and audit do. The error is an
// unavailable error whose Details["transport_reason"] says why there is no adapter (TransportNoAdapter
// tells a package the build cannot serve from one whose application cannot be reached).
func BuildPackageAdapter(ctx context.Context, req BuildRequest) (*PackageAdapter, error) {
	switch {
	case req.Package == nil:
		return nil, contracts.New(contracts.ErrInternal, "cli: BuildPackageAdapter needs the loaded package")
	case req.ApplicationID == "":
		return nil, contracts.New(contracts.ErrInternal, "cli: BuildPackageAdapter needs the application id")
	case req.Dir == "":
		return nil, contracts.New(contracts.ErrInternal, "cli: BuildPackageAdapter needs the package directory")
	}
	dir, err := filepath.Abs(req.Dir)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, "cli: the package directory cannot be made absolute")
	}
	req.Dir = dir
	rec := apppkg.Record{
		ApplicationID: req.ApplicationID, ManifestHash: req.ManifestHash, State: apppkg.StateQuarantined,
		// transportsSource joins the data directory and the record's path; with no data directory
		// an absolute path is the directory itself.
		Path: filepath.ToSlash(dir),
	}
	built, plan, err := transportsSource{}.build(ctx, req.ApplicationID, req.Package, rec)
	if err != nil {
		return nil, err
	}
	closing, ok := built.(transportsClosing)
	if !ok {
		transportsClose(built)
		return nil, contracts.Newf(contracts.ErrInternal, "cli: the transport built %T, want an adapter that can be closed", built)
	}
	return &PackageAdapter{kind: plan.Kind, req: req, rec: rec, built: built, raw: closing.inner}, nil
}

// TransportNoAdapter reports whether err says that this build has no adapter for the package (its
// manifest declares no operation endpoint, more than one kind, or a kind nobody registered), as
// opposed to a transport that is registered and could not be opened. The answer is the same on every
// call for the same package.
func TransportNoAdapter(err error) bool { return transportsNoAdapter(err) }

// Kind is the transport kind of the package: mcp, openapi or native.
func (a *PackageAdapter) Kind() string { return a.kind }

// Adapter is the transport's own adapter: scope checking, bounds and result validation are
// adapters.Wrap's, which the caller adds with the application id. It is released by Close.
func (a *PackageAdapter) Adapter() adapters.AppAdapter { return a.raw }

// Close releases what the transport holds (connections, sessions). It is safe to call twice.
func (a *PackageAdapter) Close() error {
	if a == nil || a.built == nil {
		return nil
	}
	b := a.built
	a.built = nil
	if c, ok := b.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// transportsBuildProvisionSources are the provisioning bridges of the operation transports, in the order
// of their kinds. Each answers nil, nil for an adapter that is not its own.
var transportsBuildProvisionSources = []probes.ProvisionerSource{mcpProvisionSource, openapiProvisionSource, nativeProvisionSource}

// Provisioner returns the provisioner of the package's disposable tenants, built by the bridge of its
// transport, or nil, nil when the package's test suites name no provisioning calls or the transport has
// no bridge. An error is the bridge's own refusal (a tool that drifted, a call the adapter
// configuration does not approve): the audit stops on it too.
func (a *PackageAdapter) Provisioner(ctx context.Context) (probes.Provisioner, error) {
	if a == nil || a.built == nil {
		return nil, errors.New("cli: the package adapter is closed")
	}
	// The bridges look at the audit's input: the package, its record and the adapter by its concrete
	// type, behind the closing wrapper the registry's build gives it.
	in := &integrationaudit.Input{
		Package: a.req.Package, Record: a.rec,
		Adapter: transportsClosing{AppAdapter: a.raw, inner: a.raw},
	}
	for _, src := range transportsBuildProvisionSources {
		p, err := src(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("the %s provisioning bridge: %w", a.kind, err)
		}
		if p != nil {
			return p, nil
		}
	}
	return nil, nil
}
