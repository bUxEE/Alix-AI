package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	openapitransport "github.com/buxee/alix-ai/adapters/openapi"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/rag/providers/docpkg"
	fsprovider "github.com/buxee/alix-ai/internal/rag/providers/fs"
	"github.com/buxee/alix-ai/internal/rag/providers/httpapi"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// The source providers of the RAG composition (M2-18): the filesystem provider and the
// application-document HTTP provider (internal/rag/providers/httpapi, M2-03), for the source
// types local_folder (and its siblings) and application_document_api and http_api. A registered
// source of either kind of API then syncs; before this file a registration of one was refused
// with unavailable ("no provider serves sources of type ..."). The documentation-package provider
// (internal/rag/providers/docpkg, M2-21) is the third: it serves the documentation_package sources,
// whose files ship inside the application's own signed package (see "The package files" below).
//
// # What the provider is given
//
//   - The egress registry. Every connection of the provider goes through internal/egress, and the
//     dialer of an application reads that application's allowlist from a registry: an
//     application with no entry reaches nothing. The registry of serve (ragServeNetwork) holds,
//     for each application, exactly the destinations of network.egress that the RAG source
//     declarations of its active package name in their egress entries, and it learns them when
//     the active package is opened to register or re-bind a source (ragInstallerSource.open), so
//     after a restart they are back before the pool leases its first job. Nothing else in
//     network.egress is reachable through it, and a package whose entry cannot be turned into a
//     destination reaches nothing at all (the allowlist is withdrawn, with a warning).
//   - The credential reference. A source may declare a credential_ref: an opaque "cred:" id the
//     provider resolves for every request and puts in one header for the origin of the source's
//     root. The credential service of this build is the one the OpenAPI transport uses
//     (RegisterOpenAPISecretProvider); ragServeSecrets adapts it to httpapi.SecretProvider, whose
//     Secret type is deliberately its own (adapters/openapi's and this provider's are not
//     interchangeable). With no credential provider a source that names a credential sends nothing.
//   - The endpoint resolver. A source whose root names a manifest endpoint (root.endpoint)
//     instead of a url is resolved to that endpoint's base_url, from the active package's own
//     manifest; an endpoint of another kind, or one the package does not declare, resolves to
//     nothing and the source is refused.
//
// # What stays with ingestion
//
// Both providers believe a complete listing: an API that answers 200 with an empty or
// mis-scoped listing looks like a source whose documents were all deleted. The mass-deletion
// guard of ingestion is what holds such a scan (nothing from it is applied, the source is marked
// unavailable with the reason mass_deletion, and only the operator's ConfirmMassDeletion lets it
// through); it is in the composition (ragCompose), and TestHTTPAPISourceProviderIsRegisteredAndSyncs
// runs a real hold against the HTTP provider.
//
// # The audit
//
// The integration audit asks a provider whether a root can be reached; it never syncs. The
// provider is also registered with it (auditRAGAddPackageProvider), built with no live
// dependency: no egress and no credential, so it opens no connection of its own. It does get the
// endpoint resolver serve uses (ragServeNetwork.endpoint), taught by the package under audit the
// way serve teaches it from the active package: a source whose root is root.endpoint plus
// base_path (facility-maintenance's maintenance-history) is then built by the audit, and one that
// names an endpoint the package does not declare, or declares as another kind, is still refused.
//
// # The package files (M2-21)
//
// A documentation_package source reads the files of the package the application runs, which the
// installer holds in <data dir>/packages/<application>/<manifest hash>. The provider is built here,
// in ragServeSourceProviders, with the network as its package source; what fills it is the serve
// provider "rag_docpkg" (docpkgServeProvide), which runs right after "rag" and takes the installer the
// RAG stack was built with and the data directory, because ragServeProvide does not hand either to
// this file. Until it has run, and in a process that has no installer, every call answers
// unavailable: nothing is read from a package that was not looked up.
//
// The package source of serve (docpkgInstalled) answers with the application's active package only:
// it lists the installed records, takes the one in state active, and has the installer open it,
// which loads the stored bytes again with the full checks (signature, content hash), before the
// provider reads a file of it. The verification is kept for docpkgVerifyEvery per package content,
// not per file, so a scan of a large package does not load it once per item; a package that no longer
// verifies, a revoked key included, is then unreachable at the next look. The audit's package source
// (docpkgAuditResolver) answers with the one package under audit, which is quarantined and not yet
// active, from the record the audit holds.
//
// The version of such a source is the content hash of the package: a registration names it
// (Definition.ManifestHash), the catalogue serves it only while the active package is that one, and
// the provider refuses a registration that names another. A replaced package therefore changes the
// version of every file whose bytes changed, and the re-bind that follows a re-audit rescans against
// the files of the new package.

func init() {
	auditRAGAddPackageProvider(func(clk clock.Clock, pkg *apppkg.Package) source.Provider {
		p, err := httpapi.New(httpapi.Config{Clock: clk, Endpoints: auditRAGEndpoints(pkg)})
		if err != nil {
			return nil
		}
		return p
	})
	registerServeProvider(docpkgServeProviderName, docpkgServeProvide)
}

// auditRAGEndpoints is the endpoint resolver of the audit's HTTP provider: the one serve uses,
// taught by the package under audit. With no package it resolves nothing.
func auditRAGEndpoints(pkg *apppkg.Package) httpapi.EndpointResolver {
	if pkg == nil {
		return nil
	}
	net := ragServeNewNetwork(nil)
	net.learn(string(pkg.Manifest.Application.ID), pkg)
	return net.endpoint
}

// ragServeSourceProviders is the providers serve registers: the filesystem provider, the
// application-document HTTP provider, over the network the stack learns from active packages, and
// the documentation-package provider, over the package source the network is given (docpkgBind).
//
// dataDir is the data directory of the process: a folder source may not point at it, nor at the directory of the
// executable (ragFolderProtected).
func ragServeSourceProviders(clk clock.Clock, log *slog.Logger, net *ragServeNetwork, dataDir string) ([]source.Provider, error) {
	api, err := httpapi.New(httpapi.Config{
		Clock:     clk,
		Egress:    net.egress,
		Secrets:   ragServeSecrets{provider: openapiTransportSecretProvider},
		Endpoints: net.endpoint,
		Logger:    log,
	})
	if err != nil {
		return nil, fmt.Errorf("rag: the HTTP API source provider: %w", err)
	}
	pkgs := docpkg.New(docpkg.Config{Clock: clk, Packages: docpkg.ResolverFunc(net.docpkgPackage)})
	return []source.Provider{fsprovider.New(fsprovider.Config{Clock: clk, Protected: ragFolderProtected(dataDir)}), api, pkgs}, nil
}

// ragFolderProtected are the directories of AlixAI itself that a folder source may not be, lie inside or contain:
// the data directory (the store, the keys, the external-provider secrets) and the directory of the executable.
func ragFolderProtected(dataDir string) []string {
	out := []string{dataDir}
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Dir(exe))
	}
	return out
}

// ragServeNetwork is what serve knows of the network the RAG sources of an application may use:
// its egress allowlist and the base URLs of its manifest endpoints, both read from the active
// package, and, for the sources shipped inside a package, where the installed packages are. It is
// safe for concurrent use.
type ragServeNetwork struct {
	egress *egress.Registry
	log    *slog.Logger

	mu        sync.RWMutex
	endpoints map[string]map[string]string // application -> endpoint id -> base URL
	// packages is the package source of the documentation-package provider, nil until
	// docpkgBind gives it one.
	packages docpkg.Resolver
}

func ragServeNewNetwork(log *slog.Logger) *ragServeNetwork {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ragServeNetwork{egress: egress.NewRegistry(platformDenyOption()), log: log, endpoints: map[string]map[string]string{}}
}

// ragServeAPISource says whether a declaration is one of the HTTP provider's types.
func ragServeAPISource(d ap.RAGSource) bool {
	return d.Type == ap.RAGSourceTypeApplicationDocumentAPI || d.Type == ap.RAGSourceTypeHTTPAPI
}

// learn replaces what the network holds for the application with what the package declares: the
// destinations the entries of its API sources name, and its OpenAPI endpoints. It is called with
// the package that is active, so what a replaced package allowed is gone with it.
func (n *ragServeNetwork) learn(applicationID string, pkg *apppkg.Package) {
	if n == nil || pkg == nil {
		return
	}
	var dests []egress.Destination
	seen := map[string]bool{}
	names := make([]string, 0, len(pkg.Sources))
	for name, d := range pkg.Sources {
		if ragServeAPISource(d) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		for _, id := range pkg.Sources[name].Egress.Entries {
			e, declared := pkg.Manifest.Network.Egress[string(id)]
			if !declared || seen[string(id)] {
				continue // not declared: not allowed
			}
			seen[string(id)] = true
			ports := make([]int, len(e.Ports))
			for i, p := range e.Ports {
				ports[i] = int(p)
			}
			dests = append(dests, egress.Destination{Host: strings.ToLower(string(e.Host)), Ports: ports, Scheme: string(e.Scheme)})
		}
	}
	allow, err := egress.NewAllowlist(dests...)
	if err != nil {
		// One entry that cannot be an allowlist entry withdraws the whole list: the sources
		// reach nothing until the package is fixed. The log carries the application, never a host.
		n.log.Warn("rag: the egress entries of the sources of an application cannot be used; its API sources reach nothing",
			slog.String("application", applicationID))
		n.egress.Revoke(applicationID)
	} else {
		_ = n.egress.Set(applicationID, allow)
	}

	eps := map[string]string{}
	for id, ep := range pkg.Manifest.Endpoints {
		if v, ok := ep.Value.(ap.ManifestOpenAPIEndpoint); ok {
			eps[id] = string(v.BaseURL)
		}
	}
	n.mu.Lock()
	n.endpoints[applicationID] = eps
	n.mu.Unlock()
}

// endpoint is httpapi.EndpointResolver: the base URL of a manifest endpoint of the application's
// active package, as far as it has been read.
func (n *ragServeNetwork) endpoint(applicationID, endpointID string) (string, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	u, ok := n.endpoints[applicationID][endpointID]
	return u, ok
}

// ragServeSecrets is httpapi.SecretProvider over the credential provider of this build (the one
// the OpenAPI transport resolves its references with). Its Secret type is the HTTP provider's own:
// the value is converted, never shared, and an error of the credential provider is passed as it
// is (the provider that calls this never forwards the text of one).
type ragServeSecrets struct {
	provider func() openapitransport.SecretProvider
}

var _ httpapi.SecretProvider = ragServeSecrets{}

// Secret implements httpapi.SecretProvider.
func (s ragServeSecrets) Secret(ctx context.Context, ref string) (httpapi.Secret, error) {
	var p openapitransport.SecretProvider
	if s.provider != nil {
		p = s.provider()
	}
	if p == nil {
		return "", httpapi.ErrNoSecret
	}
	sec, err := p.Secret(ctx, ref)
	switch {
	case errors.Is(err, openapitransport.ErrNoSecret):
		return "", httpapi.ErrNoSecret
	case err != nil:
		return "", err
	}
	return httpapi.Secret(sec.Reveal()), nil
}

// ---- the package files of a documentation_package source (M2-21) -------------------------

// docpkgServeProviderName is the serve provider that gives the documentation-package provider its
// package source. Providers run in name order, so it runs right after "rag", whose stack it needs.
const docpkgServeProviderName = "rag_docpkg"

// docpkgVerifyEvery is how long the verification of an installed package's stored bytes is trusted,
// per package content. It is a variable so a test can move it.
var docpkgVerifyEvery = 30 * time.Second

// docpkgBind gives the documentation-package provider the place it finds installed packages. It
// replaces what was bound before.
func (n *ragServeNetwork) docpkgBind(r docpkg.Resolver) {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.packages = r
	n.mu.Unlock()
}

// docpkgPackage is docpkg.ResolverFunc over what docpkgBind gave: with nothing bound it is an
// outage, never an empty package.
func (n *ragServeNetwork) docpkgPackage(ctx context.Context, applicationID string) (docpkg.Package, error) {
	n.mu.RLock()
	r := n.packages
	n.mu.RUnlock()
	if r == nil {
		return docpkg.Package{}, contracts.New(contracts.ErrUnavailable, "the installed packages are not bound to the RAG sources")
	}
	return r.Package(ctx, applicationID)
}

// docpkgServeProvide binds the documentation-package provider of the RAG stack to the installer the
// stack was built with and to the data directory the installer writes below. It provides no service
// of its own. A process whose RAG stack is not there has no RAG source to serve and nothing to bind;
// a stack whose package source is not the installer's cannot serve a package's files, and serve
// refuses to start rather than answer unavailable for ever.
func docpkgServeProvide(_ context.Context, d serveDeps) error {
	st, ok := gateway.Lookup[*ragStack](d.Services)
	if !ok || st == nil {
		return nil
	}
	src, ok := st.input.Reaudit.(ragInstallerSource)
	if !ok || src.inst == nil || src.net == nil {
		return errors.New("rag: the RAG stack reads packages through something other than the installer; documentation_package sources cannot be served")
	}
	dir, err := filepath.Abs(d.DataDir)
	if err != nil {
		return fmt.Errorf("rag: the data directory of the installed packages: %w", err)
	}
	src.net.docpkgBind(&docpkgInstalled{inst: src.inst, dataDir: dir, clk: d.Clock})
	return nil
}

// docpkgInstalled is docpkg.Resolver over the installer: the application's active package, verified.
type docpkgInstalled struct {
	inst    *apppkg.Installer
	dataDir string // absolute
	clk     clock.Clock

	mu       sync.Mutex
	verified map[string]docpkgVerified // application -> the package content last verified
}

type docpkgVerified struct {
	hash string
	at   time.Time
}

var _ docpkg.Resolver = (*docpkgInstalled)(nil)

// Package implements docpkg.Resolver. An application with no active package is contracts.ErrQuarantined,
// like ragInstallerSource.open; a package whose stored bytes no longer pass the loader's checks, or
// are no longer the recorded package, is the error of the installer.
func (r *docpkgInstalled) Package(ctx context.Context, applicationID string) (docpkg.Package, error) {
	recs, err := r.inst.List(ctx, applicationID)
	if err != nil {
		return docpkg.Package{}, err
	}
	hash := ""
	for _, rec := range recs {
		if rec.State == apppkg.StateActive {
			hash = rec.ManifestHash
		}
	}
	if hash == "" {
		return docpkg.Package{}, contracts.New(contracts.ErrQuarantined, "the application has no active package")
	}
	if !r.trusted(applicationID, hash) {
		_, rec, err := r.inst.Open(ctx, applicationID, hash)
		if err != nil {
			r.forget(applicationID)
			return docpkg.Package{}, err
		}
		if rec.State != apppkg.StateActive || rec.ManifestHash != hash {
			r.forget(applicationID)
			return docpkg.Package{}, contracts.New(contracts.ErrQuarantined, "the application's package is no longer the active one")
		}
		r.remember(applicationID, hash)
		return docpkgDir(r.dataDir, rec)
	}
	for _, rec := range recs {
		if rec.ManifestHash == hash {
			return docpkgDir(r.dataDir, rec)
		}
	}
	return docpkg.Package{}, contracts.New(contracts.ErrQuarantined, "the application has no active package")
}

func (r *docpkgInstalled) now() time.Time {
	if r.clk == nil {
		return time.Now().UTC()
	}
	return r.clk.Now()
}

// trusted reports whether the content was verified less than docpkgVerifyEvery ago.
func (r *docpkgInstalled) trusted(applicationID, hash string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.verified[applicationID]
	return ok && v.hash == hash && r.now().Sub(v.at) < docpkgVerifyEvery
}

func (r *docpkgInstalled) remember(applicationID, hash string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.verified == nil {
		r.verified = map[string]docpkgVerified{}
	}
	r.verified[applicationID] = docpkgVerified{hash: hash, at: r.now()}
}

func (r *docpkgInstalled) forget(applicationID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.verified, applicationID)
}

// docpkgDir is the package of a record: its install directory below the data directory. The record's
// path is the installer's own (packages/<application>/<hash>); one that is not a plain relative path
// is refused, so a record can never point outside the data directory.
func docpkgDir(dataDir string, rec apppkg.Record) (docpkg.Package, error) {
	if source.HostPaths().ValidKey(rec.Path) != nil || !filepath.IsAbs(dataDir) {
		return docpkg.Package{}, contracts.New(contracts.ErrUnavailable, "the installed package has no usable install directory")
	}
	return docpkg.Package{
		ApplicationID: rec.ApplicationID,
		ManifestHash:  rec.ManifestHash,
		Dir:           filepath.Join(dataDir, filepath.FromSlash(rec.Path)),
	}, nil
}

// docpkgAuditResolver is docpkg.Resolver for the audit: the one package under audit, from the record
// the audit holds, whatever its state (it is quarantined until the audit passes). It answers for that
// package's application only.
type docpkgAuditResolver struct {
	dataDir string // absolute
	pkg     *apppkg.Package
	rec     apppkg.Record
}

var _ docpkg.Resolver = docpkgAuditResolver{}

// Package implements docpkg.Resolver.
func (r docpkgAuditResolver) Package(_ context.Context, applicationID string) (docpkg.Package, error) {
	switch {
	case r.pkg == nil || r.pkg.ManifestHash == "":
		return docpkg.Package{}, contracts.New(contracts.ErrUnavailable, "the audit has no verified package to read the documentation from")
	case applicationID != r.pkg.ApplicationID() || r.rec.ApplicationID != applicationID || r.rec.ManifestHash != r.pkg.ManifestHash:
		return docpkg.Package{}, contracts.New(contracts.ErrUnavailable, "the package under audit is not the one asked for")
	}
	return docpkgDir(r.dataDir, r.rec)
}

// docpkgAuditProvider is the documentation-package provider of one audit of pkg, the installed
// package recorded as rec below dataDir: the audit probes the declared folder of the very bytes the
// package was verified from, and it opens no other package.
func docpkgAuditProvider(clk clock.Clock, dataDir string, pkg *apppkg.Package, rec apppkg.Record) source.Provider {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		dir = "" // not absolute: the resolver's directory is refused by docpkgDir
	}
	return docpkg.New(docpkg.Config{Clock: clk, Packages: docpkgAuditResolver{dataDir: dir, pkg: pkg, rec: rec}})
}
