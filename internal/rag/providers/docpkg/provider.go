// Package docpkg is the source provider for documentation that ships inside the
// application's own signed package (master plan 12.1, 12.2, 19): it implements
// source.Provider for the source type documentation_package, whose declared root
// is a package-relative folder below documentation/ (root kind "package").
//
// # What it reads
//
// The files of the package the application is running: the signed, active
// package whose install directory a Resolver names. The provider asks the
// resolver for the application's package on every call, so a package that was
// replaced is followed at once, and it reads nothing else. Item keys are the
// paths below the declared folder, which is what the declaration's include and
// exclude rules are written against.
//
// # Confinement
//
// The package is signed content: what an audit reviewed and an operator
// activated is what is served, and nothing outside it is ever reachable. Three
// layers deny, each on its own:
//
//  1. The declared folder is hostile input, though the loader checks it too. It
//     must be a clean relative path made of plain names, at least one level
//     below "documentation/" (rootFolder); "..", an absolute or drive path, a
//     backslash, the manifest, the signatures, the capabilities and every other
//     place of the package are refused before anything is read.
//  2. The package's install directory is the root of the filesystem provider's
//     confinement (internal/rag/providers/fs), with the folder as its sub-root:
//     every read goes through an os.Root, a link or reparse point is never
//     followed, a key is a clean relative slash path, and the canonical path of
//     an item must be the key.
//  3. The provider only ever calls the read half of the filesystem provider.
//     There is no write, rename or delete anywhere in this package.
//
// # Contract
//
// Listing, versioning (the SHA-256 of the content), Stat, Open, size and format
// limits, tombstones and the fail-closed rules are those of the filesystem
// provider, because they are its: an item is gone only when the package was
// reached and the item's place was inspected and found empty; a package that
// cannot be reached (none active, the install directory missing, the declared
// folder gone) is contracts.ErrUnavailable and Health unavailable, never an
// empty result and never a tombstone.
//
// # Versions
//
// A definition that names the package it was built from (Definition.ManifestHash)
// is served only from that package: when the application's active package has
// another content hash the answer is contracts.ErrUnavailable until the
// registration has been rebuilt from the new package, so a declaration's rules
// are never applied to another package's files. A definition with no hash (the
// integration audit builds one straight from a declaration) is served from
// whatever package the resolver names for the application, which is the package
// under audit.
package docpkg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/internal/clock"
	fsprovider "github.com/buxee/alix-ai/internal/rag/providers/fs"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// docsPrefix is the folder every documentation_package root lies below (the
// loader's own rule, apppkg checks the same).
const docsPrefix = "documentation/"

// Package is one installed package, as far as the provider reads it.
type Package struct {
	// ApplicationID is the application the package belongs to. The provider
	// refuses a package of another application than the source's.
	ApplicationID string
	// ManifestHash is the content address of the package (apppkg.Package's
	// ManifestHash): the version a registration is pinned to.
	ManifestHash string
	// Dir is the absolute host path of the directory that holds exactly the
	// files of the package, the way the installer wrote them.
	Dir string
}

// Resolver finds the package an application is running. It must return only a
// package that is signed, verified and active for the application, and it
// answers with an error (never a stale or another application's package) when
// there is none: the provider treats every failure as an unreachable root.
type Resolver interface {
	Package(ctx context.Context, applicationID string) (Package, error)
}

// ResolverFunc adapts a function to Resolver.
type ResolverFunc func(ctx context.Context, applicationID string) (Package, error)

// Package calls f.
func (f ResolverFunc) Package(ctx context.Context, applicationID string) (Package, error) {
	return f(ctx, applicationID)
}

// Config configures a Provider.
type Config struct {
	// Clock supplies the time stamped on tombstones and probes. Nil means
	// clock.Real.
	Clock clock.Clock
	// Packages finds the application's package. Nil means no package is ever
	// reachable: every call answers unavailable.
	Packages Resolver
}

// Provider is the documentation-package source provider. It is stateless and
// safe for concurrent use.
type Provider struct {
	fs    *fsprovider.Provider
	pkgs  Resolver
	paths source.Paths

	// folderRule checks the declared folder. It is rootFolder; a test replaces
	// it to show that the rule, and not something else, is what keeps a
	// declaration inside documentation/.
	folderRule func(string) (string, error)
}

var _ source.Provider = (*Provider)(nil)

// New returns a Provider.
func New(cfg Config) *Provider {
	return &Provider{
		fs:         fsprovider.New(fsprovider.Config{Clock: cfg.Clock}),
		pkgs:       cfg.Packages,
		paths:      source.HostPaths(),
		folderRule: rootFolder,
	}
}

// Kinds implements source.Provider.
func (p *Provider) Kinds() []ap.RAGSourceType {
	return []ap.RAGSourceType{ap.RAGSourceTypeDocumentationPackage}
}

// rootFolder returns the declared package-relative folder in the form the
// provider reads it, or an error that wraps source.ErrEscapesRoot. The folder
// must be a clean relative path of plain names that starts with documentation/
// and has at least one more level, so that the package's manifest, signatures,
// capabilities and every other file are out of reach of a source declaration.
func rootFolder(declared string) (string, error) {
	folder := strings.TrimSuffix(declared, "/")
	if err := source.HostPaths().ValidKey(folder); err != nil {
		return "", err
	}
	if !strings.HasPrefix(folder, docsPrefix) || len(folder) == len(docsPrefix) {
		return "", fmt.Errorf("%w: a package source's folder lies below documentation/", source.ErrEscapesRoot)
	}
	return folder, nil
}

// bind checks what every call needs and returns the definition the filesystem
// provider reads: the same source, rooted at the declared folder of the
// application's package. The error is typed; contracts.ErrUnavailable means the
// package could not be reached, and nothing else does.
func (p *Provider) bind(ctx context.Context, scope contracts.Scope, def source.Definition) (source.Definition, error) {
	if err := ctx.Err(); err != nil {
		return source.Definition{}, contracts.Wrap(contracts.ErrTimeout, err, "the operation was interrupted")
	}
	if err := def.CheckScope(scope); err != nil {
		return source.Definition{}, err
	}
	if def.Type != ap.RAGSourceTypeDocumentationPackage || def.Root.Kind != source.RootPackage {
		return source.Definition{}, contracts.Newf(contracts.ErrValidationFailed,
			"the package provider serves documentation_package sources with a package root, not %q with a %q root", string(def.Type), string(def.Root.Kind))
	}
	decl, ok := def.Decl.Root.Value.(ap.RAGSourceRootPackage)
	if !ok {
		return source.Definition{}, contracts.New(contracts.ErrValidationFailed, "the source declares no package root")
	}
	folder, err := p.folderRule(decl.Path)
	if err != nil {
		return source.Definition{}, err
	}

	if p.pkgs == nil {
		return source.Definition{}, contracts.New(contracts.ErrUnavailable, "no package source is configured")
	}
	pkg, err := p.pkgs.Package(ctx, def.ApplicationID)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return source.Definition{}, contracts.Wrap(contracts.ErrTimeout, cerr, "the operation was interrupted")
		}
		// The resolver's error is not carried on: it may name a host path, and an error of this
		// provider carries none. Its code says what kind of failure it was.
		return source.Definition{}, contracts.New(contracts.ErrUnavailable, "the application's package cannot be reached").
			WithDetails("resolver_code", string(contracts.CodeOf(err)))
	}
	switch {
	case pkg.ApplicationID != def.ApplicationID:
		// Never read another application's files, whatever the resolver meant.
		return source.Definition{}, contracts.New(contracts.ErrScopeViolation, "the package belongs to another application")
	case def.ManifestHash != "" && def.ManifestHash != pkg.ManifestHash:
		return source.Definition{}, contracts.New(contracts.ErrUnavailable,
			"the source was registered for another version of the package than the active one")
	}
	dir, err := p.paths.Normalize(pkg.Dir)
	if err != nil || p.paths.IsVolumeRoot(dir) {
		return source.Definition{}, contracts.New(contracts.ErrUnavailable, "the package directory is not usable")
	}

	read := def
	read.Root = source.Root{Kind: source.RootMount, Path: dir, SubPath: folder}
	return read, nil
}

// tidy returns err without the host's own words. The filesystem provider wraps an outage with its
// cause, and an operating-system error spells the absolute path of the install directory; an error
// of a source provider carries no host path, so an unavailable error is rebuilt from its code, its
// message and its details alone. Every other error is returned as it is.
func tidy(err error) error {
	var ce *contracts.Error
	if err == nil || !errors.As(err, &ce) || ce.Code != contracts.ErrUnavailable || ce.Unwrap() == nil {
		return err
	}
	return tidyTyped(ce)
}

func tidyTyped(ce *contracts.Error) *contracts.Error {
	if ce == nil || ce.Code != contracts.ErrUnavailable || ce.Unwrap() == nil {
		return ce
	}
	out := contracts.New(ce.Code, ce.Message)
	for k, v := range ce.Details {
		out.WithDetails(k, v)
	}
	return out
}

// tidyChanges is tidy for a scan: the typed errors of the items that failed to read are tidied too.
func tidyChanges(cs source.ChangeSet, err error) (source.ChangeSet, error) {
	for i := range cs.Failed {
		cs.Failed[i].Err = tidyTyped(cs.Failed[i].Err)
	}
	return cs, tidy(err)
}

// Probe implements source.Provider.
func (p *Provider) Probe(ctx context.Context, scope contracts.Scope, def source.Definition) (source.Health, error) {
	read, err := p.bind(ctx, scope, def)
	if err != nil {
		var ce *contracts.Error
		if errors.As(err, &ce) && ce.Code == contracts.ErrUnavailable {
			return source.Health{Availability: source.Unavailable, Err: ce}, nil
		}
		return source.Health{}, err
	}
	h, err := p.fs.Probe(ctx, scope, read)
	h.Err = tidyTyped(h.Err)
	return h, tidy(err)
}

// Discover implements source.Provider.
func (p *Provider) Discover(ctx context.Context, scope contracts.Scope, def source.Definition, prev []source.Record) (source.ChangeSet, error) {
	read, err := p.bind(ctx, scope, def)
	if err != nil {
		return source.ChangeSet{}, err
	}
	return tidyChanges(p.fs.Discover(ctx, scope, read, prev))
}

// DetectDeletions implements source.Provider.
func (p *Provider) DetectDeletions(ctx context.Context, scope contracts.Scope, def source.Definition, prev []source.Record) (source.ChangeSet, error) {
	read, err := p.bind(ctx, scope, def)
	if err != nil {
		return source.ChangeSet{}, err
	}
	return tidyChanges(p.fs.DetectDeletions(ctx, scope, read, prev))
}

// Stat implements source.Provider.
func (p *Provider) Stat(ctx context.Context, scope contracts.Scope, def source.Definition, key string) (source.ItemInfo, error) {
	read, err := p.bind(ctx, scope, def)
	if err != nil {
		return source.ItemInfo{}, err
	}
	info, err := p.fs.Stat(ctx, scope, read, key)
	return info, tidy(err)
}

// FetchVersion implements source.Provider.
func (p *Provider) FetchVersion(ctx context.Context, scope contracts.Scope, def source.Definition, key string) (source.Version, error) {
	read, err := p.bind(ctx, scope, def)
	if err != nil {
		return source.Version{}, err
	}
	v, err := p.fs.FetchVersion(ctx, scope, read, key)
	return v, tidy(err)
}

// Open implements source.Provider.
func (p *Provider) Open(ctx context.Context, scope contracts.Scope, def source.Definition, key string) (source.Content, error) {
	read, err := p.bind(ctx, scope, def)
	if err != nil {
		return source.Content{}, err
	}
	c, err := p.fs.Open(ctx, scope, read, key)
	return c, tidy(err)
}
