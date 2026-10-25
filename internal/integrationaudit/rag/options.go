package rag

import (
	"context"
	"slices"
	"sync"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/rag/answer"
	fsprovider "github.com/buxee/alix-ai/internal/rag/providers/fs"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/rag/source"
	ragsources "github.com/buxee/alix-ai/internal/rag/sources"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	ragc "github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// Retriever is the retrieval the probes call: *query.Pipeline in production. It is an
// interface only so that a test can put a fault between the probes and the pipeline and
// prove that a probe notices it.
type Retriever interface {
	Query(ctx context.Context, scope contracts.Scope, req query.Request) (query.Result, error)
}

// Answerer is the grounded answer step the probes call: *answer.Answerer in production.
type Answerer interface {
	Answer(ctx context.Context, scope contracts.Scope, pkg ragc.EvidencePackage) (answer.Result, error)
}

// Resolver resolves the open/preview reference of a citation: *answer.Resolver in
// production.
type Resolver interface {
	Resolve(ctx context.Context, scope contracts.Scope, fileID actions.FileID) (answer.Resolution, error)
}

// Hooks put a wrapper around the production objects the probes call. They exist for the
// tests of this package, which must show that every probe fails when what it guards is
// broken, and are nil in production.
type Hooks struct {
	Retrieve func(Retriever) Retriever
	Answer   func(Answerer) Answerer
	Resolve  func(Resolver) Resolver
}

// Registration is one RAG source the deployment holds for an application, as the
// deployment's registry records it: the declaration it was built from and the tenant and
// workspace its definition was bound to. It holds no identifier minted by the core and no
// path.
type Registration struct {
	// PackageSourceID is the slug the package declares the source under.
	PackageSourceID string
	// PackageVersion and ManifestHash say which package the registration was last
	// bound to. The audit does not require them to be the package under audit: a
	// re-audit of a new version finds the registrations of the version it replaces.
	PackageVersion string
	ManifestHash   string
	// BoundTenantID and BoundWorkspaceID are what the registration's definition names
	// for the two levels: empty for a shared level.
	BoundTenantID    string
	BoundWorkspaceID string
}

// Deployment is what the audit may look at of the deployment's RAG side. The audit never
// scans for it: whatever the deployment does not supply is recorded as unverified.
type Deployment struct {
	// Providers are the source providers the deployment runs. A source whose type no
	// provider serves cannot be reached from this deployment, and the audit says it
	// could not look.
	Providers []source.Provider
	// Mounts resolves the operator's mounts by the id a package declares. Nil means
	// the deployment's mounts are not known to the audit, so a mount root is recorded
	// as unverified, not as unreachable.
	Mounts source.MountResolver
	// Registrations lists the sources the deployment holds for an application. Nil
	// means the audit cannot list them, so undeclared sources are recorded as
	// unverified, never as absent.
	Registrations func(ctx context.Context, applicationID string) ([]Registration, error)
}

// DeploymentSource returns the Deployment for an audit, or nil when none is available.
type DeploymentSource func(ctx context.Context, in *integrationaudit.Input) (*Deployment, error)

type sourceEntry struct{ fn DeploymentSource }

var (
	sourceMu       sync.RWMutex
	deploymentSrcs []*sourceEntry
)

// SetDeploymentSource adds a source to the ones the checks registered with the audit
// consult. The first source that returns a Deployment wins. It is for the composition
// root, which holds the store the registry lives in and the operator's mounts; nothing
// in this package knows either. A nil source is ignored.
//
// Sources only accumulate, so a composition root that installs one for each audit it runs
// must call the function this returns when the audit is over (or install one source, once,
// that reads what the audit command sets): a source left behind is consulted by every
// later audit of the process.
func SetDeploymentSource(s DeploymentSource) (remove func()) {
	if s == nil {
		return func() {}
	}
	e := &sourceEntry{fn: s}
	sourceMu.Lock()
	deploymentSrcs = append(deploymentSrcs, e)
	sourceMu.Unlock()
	return func() {
		sourceMu.Lock()
		defer sourceMu.Unlock()
		deploymentSrcs = slices.DeleteFunc(deploymentSrcs, func(x *sourceEntry) bool { return x == e })
	}
}

// FromRegistry lists the registrations of the durable registry (internal/rag/sources)
// that belong to an application. It is the production Registrations function: the
// composition root builds the registry over its store and passes the result in a
// Deployment.
func FromRegistry(reg *ragsources.Registry) func(ctx context.Context, applicationID string) ([]Registration, error) {
	return func(ctx context.Context, applicationID string) ([]Registration, error) {
		all, err := reg.LoadAll(ctx)
		if err != nil {
			return nil, err
		}
		var out []Registration
		for _, r := range all {
			if r.Scope.ApplicationID != applicationID || r.Removing {
				continue
			}
			out = append(out, Registration{
				PackageSourceID: r.PackageSourceID, PackageVersion: r.PackageVersion, ManifestHash: r.ManifestHash,
				BoundTenantID: r.BoundTenantID, BoundWorkspaceID: r.BoundWorkspaceID,
			})
		}
		return out, nil
	}
}

// Options configures the checks Checks builds.
type Options struct {
	// Deployment returns the Deployment of an audit. Nil means the sources registered
	// with SetDeploymentSource and then DefaultDeployment.
	Deployment DeploymentSource
	// ScratchDir is the directory in which the probes that run the core's own index
	// keep their throwaway store. Empty means a fresh directory under the system's
	// temporary directory; either way it is removed afterwards.
	ScratchDir string
	// MaxSweep bounds the derived cross-scope retrievals of the scope check. Zero
	// means DefaultMaxSweep.
	MaxSweep int
	// Hooks are for tests.
	Hooks Hooks
}

// DefaultMaxSweep is how many derived cross-scope retrievals the scope check makes at
// most, on top of the probes the package declares.
const DefaultMaxSweep = 200

func (o Options) maxSweep() int {
	if o.MaxSweep > 0 {
		return o.MaxSweep
	}
	return DefaultMaxSweep
}

// DefaultDeployment is what the audit looks at when no composition root supplies a
// Deployment: the folder provider, so that a folder root can be reached and a mount root
// is recorded as unverified; no mounts; and no way to list registrations.
func DefaultDeployment(in *integrationaudit.Input) *Deployment {
	cfg := fsprovider.Config{}
	if in != nil {
		cfg.Clock = in.Clock
	}
	return &Deployment{Providers: []source.Provider{fsprovider.New(cfg)}}
}

// deployment returns the Deployment of an audit.
func (o Options) deployment(ctx context.Context, in *integrationaudit.Input) (*Deployment, error) {
	if o.Deployment != nil {
		d, err := o.Deployment(ctx, in)
		if err != nil || d != nil {
			return d, err
		}
		return DefaultDeployment(in), nil
	}
	sourceMu.RLock()
	srcs := append([]*sourceEntry(nil), deploymentSrcs...)
	sourceMu.RUnlock()
	for _, s := range srcs {
		d, err := s.fn(ctx, in)
		if err != nil {
			return nil, err
		}
		if d != nil {
			return d, nil
		}
	}
	return DefaultDeployment(in), nil
}

// providerFor returns the provider that serves a source type, or nil.
func (d *Deployment) providerFor(t ap.RAGSourceType) source.Provider {
	for _, p := range d.Providers {
		if p != nil && slices.Contains(p.Kinds(), t) {
			return p
		}
	}
	return nil
}
