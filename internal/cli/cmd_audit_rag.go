package cli

// What the RAG checks of the integration audit look at of the deployment (M2-20).
//
// internal/integrationaudit/rag registers four checks (sources, citations, scope, smoke) from
// its own init, and cmd imports it through audit_checks_rag.go. Those checks know nothing of the
// store the durable source registry lives in, of the operator's mounts or of the providers the
// deployment runs; they ask a DeploymentSource (rag.SetDeploymentSource) and, with none, fall
// back to a folder provider, no mounts and no way to list registrations: a mount root, and the
// question "which sources does the deployment hold that the package does not declare?", are
// then recorded as unverified for ever. This file is the composition root's answer.
//
// # Per-audit install and remove, not one init-time source
//
// The source is installed for one audit and removed when it ends: runAuditRun calls
// auditRAGInstall right after the stack is open and defers the function it returns. The other
// shape the rag package offers (one source, installed once from init, reading a variable that
// openStack sets) would put a process-wide mutable variable between two commands that each open
// their own stack, and a source reading it after that stack was closed would hold a dead store.
// Here the source is a closure over exactly the stack of the audit that installed it, it is
// consulted only by that audit, and rag.SetDeploymentSource's remove function takes it out again,
// so sources cannot pile up (TestDeploymentSourceDoesNotPileUpAcrossAudits). The cost is the
// assumption that one process runs one audit at a time, which is what the command does: the rag
// package consults sources in install order and the first that answers wins, so two audits of
// different stacks running side by side in one process would see each other's source. An
// in-process caller that wants concurrent audits passes rag.Options.Deployment instead.
//
// # What the deployment shows
//
//   - Providers: the filesystem provider, built the way `alixai serve` builds it (cmd_rag.go),
//     plus whatever auditRAGAddProvider or auditRAGAddPackageProvider registered. The
//     application-document HTTP provider (M2-18) is added through the second one, because it
//     resolves a root that names a manifest endpoint from the package under audit, exactly as
//     serve resolves it from the active package (cmd_rag_providers.go): a provider built without
//     the package would refuse such a root as validation_failed, a critical finding the
//     application did not earn. The documentation-package provider (M2-21) is added by
//     auditRAGDeployment itself, after those, because it needs what only the audit's input
//     holds: the record of the package under audit, whose install directory is where the
//     package's files are. The audit probes a documentation_package source against the very
//     bytes the package was verified from, whatever its state (it is quarantined until the
//     audit passes); with no record it reaches nothing, and says so as unavailable.
//   - Mounts: "rag.mounts" as a source.MountResolver. A deployment that lists no mounts has no
//     mounts, and the audit says so (a package that declares a mount root is reported unreachable)
//     instead of leaving it as unknown, which is only right for a deployment the audit cannot read.
//   - Registrations: rag.FromRegistry over the durable source registry (internal/rag/sources) in
//     the audit's own store. A registry that cannot be read is an error the check records as
//     unverified; it is never "no registrations".
//
// The audit does not exercise the catalogue serve builds (ragSources in cmd_rag.go): the probes
// package cannot import it. Its lab probes the index, the query pipeline, the answer step and the
// resolver beneath it, which is by design.

import (
	"context"
	"maps"
	"sync"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	ragaudit "github.com/buxee/alix-ai/internal/integrationaudit/rag"
	fsprovider "github.com/buxee/alix-ai/internal/rag/providers/fs"
	"github.com/buxee/alix-ai/internal/rag/source"
	ragsources "github.com/buxee/alix-ai/internal/rag/sources"
)

var (
	auditRAGMu        sync.Mutex
	auditRAGProviders []func(clock.Clock, *apppkg.Package) source.Provider
)

// auditRAGAddProvider adds a source provider to the ones the audit's RAG checks see, next to the
// filesystem provider. A composition file that registers a provider with serve calls it from an
// init function with a constructor that builds the same provider without any live dependency: the
// audit only asks a provider whether a root can be reached, it never syncs. A provider that needs
// the package under audit (the HTTP one, cmd_rag_providers.go) is added with
// auditRAGAddPackageProvider instead.
func auditRAGAddProvider(build func(clock.Clock) source.Provider) {
	if build == nil {
		return
	}
	auditRAGAddPackageProvider(func(clk clock.Clock, _ *apppkg.Package) source.Provider { return build(clk) })
}

// auditRAGAddPackageProvider is auditRAGAddProvider for a provider that needs the package under
// audit: the constructor is given it (nil when the audit could not load one), so that what serve
// reads from the active package, such as the base URL of a manifest endpoint, is read from the
// audited package here.
func auditRAGAddPackageProvider(build func(clock.Clock, *apppkg.Package) source.Provider) {
	if build == nil {
		return
	}
	auditRAGMu.Lock()
	defer auditRAGMu.Unlock()
	auditRAGProviders = append(auditRAGProviders, build)
}

// auditRAGProvidersFor builds the providers of one audit that has no package to give them.
func auditRAGProvidersFor(clk clock.Clock) []source.Provider {
	return auditRAGProvidersForPackage(clk, nil)
}

// auditRAGProvidersForPackage builds the providers of one audit of pkg.
func auditRAGProvidersForPackage(clk clock.Clock, pkg *apppkg.Package) []source.Provider {
	return auditRAGProvidersForData(clk, pkg, "")
}

// auditRAGProvidersForData builds the providers of one audit of pkg in a deployment whose data directory is dataDir
// ("" when there is none): its folder provider refuses the same roots serve's does.
func auditRAGProvidersForData(clk clock.Clock, pkg *apppkg.Package, dataDir string) []source.Provider {
	out := []source.Provider{fsprovider.New(fsprovider.Config{Clock: clk, Protected: ragFolderProtected(dataDir)})}
	auditRAGMu.Lock()
	builders := append([]func(clock.Clock, *apppkg.Package) source.Provider(nil), auditRAGProviders...)
	auditRAGMu.Unlock()
	for _, b := range builders {
		if p := b(clk, pkg); p != nil {
			out = append(out, p)
		}
	}
	return out
}

// auditRAGMounts is the operator's mounts as a source.MountResolver. Unlike ragMountsOf (which
// serve passes on, where "none" and "nil" mean the same), it is never nil: a nil resolver tells
// the audit the mounts are unknown, and a deployment whose configuration lists none knows it.
func auditRAGMounts(c ragConfig) source.MountResolver {
	m := maps.Clone(c.Mounts)
	return func(id string) (string, bool) { p, ok := m[id]; return p, ok }
}

// auditRAGDeployment is the deployment of one audit of pkg, the installed package recorded as rec:
// the providers, the mounts the configuration the stack was opened with lists, and the
// registrations of the stack's store.
func auditRAGDeployment(s *stack, cfg ragConfig, pkg *apppkg.Package, rec apppkg.Record) (*ragaudit.Deployment, error) {
	reg, err := ragsources.New(s.db, s.clk)
	if err != nil {
		return nil, err
	}
	return &ragaudit.Deployment{
		Providers:     append(auditRAGProvidersForData(s.clk, pkg, s.dataDir), docpkgAuditProvider(s.clk, s.dataDir, pkg, rec)),
		Mounts:        auditRAGMounts(cfg),
		Registrations: ragaudit.FromRegistry(reg),
	}, nil
}

// auditRAGInstall makes the RAG checks of the audit about to run on s see s's deployment. Call
// it once the stack's configuration is loaded and defer the function it returns, which removes
// the source again (see the file comment).
//
// A deployment that cannot be built is returned as an error from the source, never as nil: nil
// would hand the checks the default deployment and read as an unverified audit that quietly looked
// at less than it can, where an error says why it could not look.
func auditRAGInstall(s *stack) (remove func()) {
	cfg := ragSection // read through the registered pointer, after the stack loaded the configuration
	cfg.Mounts = maps.Clone(cfg.Mounts)
	return ragaudit.SetDeploymentSource(func(_ context.Context, in *integrationaudit.Input) (*ragaudit.Deployment, error) {
		var (
			pkg *apppkg.Package
			rec apppkg.Record
		)
		if in != nil {
			pkg, rec = in.Package, in.Record
		}
		return auditRAGDeployment(s, cfg, pkg, rec)
	})
}
