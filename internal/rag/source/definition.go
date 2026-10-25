package source

import (
	"fmt"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// IDPrefix is the prefix of the core-minted ID of a registered source
// ("src_<ULID>"). The package-level name of a source is its slug, which is
// not unique across applications.
const IDPrefix = "src"

// RootKind says how a host-path source finds its root.
type RootKind string

const (
	// RootFolder is an absolute path on the AlixAI host, declared in the
	// package.
	RootFolder RootKind = "folder"
	// RootMount is a mount the operator registered (HDD, SSD or NAS path
	// visible to the host) plus an optional sub-path. The package never
	// carries the host path of a mount.
	RootMount RootKind = "mount"
	// RootAPI and RootPackage are the other two declared root kinds. A host
	// path provider refuses them; their providers read the declaration
	// (Definition.Decl).
	RootAPI     RootKind = "api"
	RootPackage RootKind = "package"
)

// Root is the resolved root of a source.
type Root struct {
	Kind RootKind
	// Path is the absolute host path of the folder, or of the mount for
	// RootMount. Empty for api and package roots.
	Path string
	// MountID and SubPath are set for RootMount: Path is the operator's host
	// path of the mount and SubPath (slash-separated, relative, possibly empty)
	// is the declared position inside it. A provider confines every read to
	// Path joined with SubPath, and the sub-path itself may not leave the
	// mount.
	MountID string
	SubPath string
}

// Limits are the size limits of a source (schema: limits). All three are
// always set; a zero value is rejected when the Definition is built.
type Limits struct {
	// MaxFileBytes is the largest single item accepted.
	MaxFileBytes int64
	// MaxTotalBytes is the most data the source may hold.
	MaxTotalBytes int64
	// MaxFiles is the most items the source may hold.
	MaxFiles int64
}

// Deletion is the deletion behaviour of a source (schema: deletion).
type Deletion struct {
	// PurgeAfter is how long a tombstoned item's derived data survives before
	// it is physically purged. Zero purges immediately.
	PurgeAfter time.Duration
	// OnSourceRemoved is what happens to derived data when the source
	// declaration is removed or disabled.
	OnSourceRemoved ap.RAGSourceDeletionOnSourceRemoved
}

// ScopeBinding is how the definition is bound to a tenant or a workspace.
type ScopeBinding struct {
	// Mode is request, fixed or shared (schema: scopeBinding).
	Mode ap.RAGSourceScopeBindingMode
	// ID is the tenant or workspace this registration serves: the one the
	// package fixes, or the one the operator registered a request-bound source
	// for. Empty for shared.
	ID string
}

// Sync is the synchronisation policy of a source (schema: sync).
type Sync struct {
	Method   ap.RAGSourceSyncMethod
	Interval time.Duration
}

// Definition is a registered source: the declaration from the signed package
// plus the facts only registration knows (the core's source ID, the tenant and
// workspace it serves, the host path of a mount). A provider receives one on
// every call and trusts nothing else about the source.
//
// A Definition is a value; providers and callers treat it as read-only.
type Definition struct {
	// SourceID is the core-minted src_ ID of this registration.
	SourceID string
	// PackageSourceID is the slug the package declares the source under.
	PackageSourceID string
	// PackageVersion is the version of the package the declaration came from,
	// empty when the definition was not built from a loaded package.
	PackageVersion string
	// ManifestHash is the content address of that package, when known.
	ManifestHash string

	Type          ap.RAGSourceType
	Title         string
	ApplicationID string
	Tenant        ScopeBinding
	Workspace     ScopeBinding

	Root    Root
	Include []string
	Exclude []string
	// Formats are the accepted MIME types, as spelled by the MIME constants.
	Formats  []string
	Limits   Limits
	Deletion Deletion
	Sync     Sync

	// Decl is the declaration the definition was built from, unchanged. The
	// providers for API and package roots read the members this type does not
	// repeat (the endpoint or URL, query scope, credential reference, egress
	// entries, citation resolver).
	Decl ap.RAGSource
}

// MountResolver maps the ID of an operator-registered mount to its absolute
// host path. It reports false for a mount that is not registered.
type MountResolver func(mountID string) (hostPath string, ok bool)

// Binding is what registration adds to a declaration.
type Binding struct {
	// SourceID is the core-minted src_ ID. Required.
	SourceID string
	// TenantID and WorkspaceID say which tenant and workspace this
	// registration serves. They are required for a request-bound level and
	// must be empty for a shared one; a fixed level takes its ID from the
	// declaration.
	TenantID    string
	WorkspaceID string
	// Mounts resolves a mount root. Required when the declaration uses one.
	Mounts MountResolver
}

// FromPackage builds the Definition of the source the package declares under
// packageSourceID. It does not look at the package's activation state; a
// registration layer decides whether a quarantined package may serve, and the
// integration audit needs to probe sources before activation.
func FromPackage(pkg *apppkg.Package, packageSourceID string, b Binding) (Definition, error) {
	if pkg == nil {
		return Definition{}, contracts.New(contracts.ErrValidationFailed, "no package")
	}
	decl, ok := pkg.Sources[packageSourceID]
	if !ok {
		return Definition{}, contracts.Newf(contracts.ErrValidationFailed, "package declares no source %q", packageSourceID)
	}
	if string(decl.Scope.ApplicationID) != pkg.ApplicationID() {
		return Definition{}, contracts.Newf(contracts.ErrScopeViolation,
			"source %q is declared for application %q, not %q", packageSourceID, string(decl.Scope.ApplicationID), pkg.ApplicationID())
	}
	d, err := FromDeclaration(decl, b)
	if err != nil {
		return Definition{}, err
	}
	d.PackageVersion = pkg.PackageVersion()
	d.ManifestHash = pkg.ManifestHash
	return d, nil
}

// FromDeclaration builds a Definition from one rag-source declaration. It
// re-checks what a provider relies on instead of assuming the loader ran:
// a src_ ID, a valid source type with a root kind that fits it, at least one
// include rule, at least one format, all three limits positive, a tenant and
// workspace binding that is complete, and a registered mount for a mount
// root. Anything missing is contracts.ErrValidationFailed; it never fills a
// default.
func FromDeclaration(decl ap.RAGSource, b Binding) (Definition, error) {
	bad := func(format string, args ...any) (Definition, error) {
		return Definition{}, contracts.Newf(contracts.ErrValidationFailed, "source %q: "+format, append([]any{string(decl.ID)}, args...)...)
	}
	if !ids.Valid(b.SourceID, IDPrefix) {
		return bad("registration needs a %s_ source ID", IDPrefix)
	}
	if !decl.Type.Valid() {
		return bad("unknown source type %q", string(decl.Type))
	}
	if decl.Scope.ApplicationID == "" {
		return bad("no application in scope")
	}
	if len(decl.Include) == 0 {
		return bad("no include rule: a source with no include rule matches nothing")
	}
	if len(decl.Formats) == 0 {
		return bad("no accepted formats")
	}
	l := decl.Limits
	if l.MaxFileBytes < 1 || l.MaxTotalBytes < 1 || l.MaxFiles < 1 {
		return bad("size limits must all be positive")
	}
	if !decl.Deletion.OnSourceRemoved.Valid() || decl.Deletion.PurgeAfterHours < 0 {
		return bad("invalid deletion behaviour")
	}
	if !decl.Sync.Method.Valid() {
		return bad("invalid sync method")
	}

	tenant, err := bind("tenant", decl.Scope.Tenant, b.TenantID)
	if err != nil {
		return bad("%v", err)
	}
	workspace, err := bind("workspace", decl.Scope.Workspace, b.WorkspaceID)
	if err != nil {
		return bad("%v", err)
	}
	if tenant.Mode == ap.RAGSourceScopeBindingModeShared && workspace.Mode != ap.RAGSourceScopeBindingModeShared {
		return bad("a shared tenant binding needs a shared workspace binding")
	}

	root, err := resolveRoot(decl, b.Mounts)
	if err != nil {
		return bad("%v", err)
	}
	if !rootFitsType(decl.Type, root.Kind) {
		return bad("a %s source cannot have a %s root", string(decl.Type), root.Kind)
	}

	d := Definition{
		SourceID:        b.SourceID,
		PackageSourceID: string(decl.ID),
		Type:            decl.Type,
		ApplicationID:   string(decl.Scope.ApplicationID),
		Tenant:          tenant,
		Workspace:       workspace,
		Root:            root,
		Include:         stringsOf(decl.Include),
		Exclude:         stringsOf(decl.Exclude),
		Formats:         stringsOf(decl.Formats),
		Limits:          Limits{MaxFileBytes: l.MaxFileBytes, MaxTotalBytes: l.MaxTotalBytes, MaxFiles: l.MaxFiles},
		Deletion: Deletion{
			PurgeAfter:      time.Duration(decl.Deletion.PurgeAfterHours) * time.Hour,
			OnSourceRemoved: decl.Deletion.OnSourceRemoved,
		},
		Sync: Sync{Method: decl.Sync.Method},
		Decl: decl,
	}
	if decl.Title != nil {
		d.Title = string(*decl.Title)
	}
	if decl.Sync.IntervalSeconds != nil {
		d.Sync.Interval = time.Duration(*decl.Sync.IntervalSeconds) * time.Second
	}
	return d, nil
}

func stringsOf[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

func bind(level string, decl ap.RAGSourceScopeBinding, registered string) (ScopeBinding, error) {
	switch decl.Mode {
	case ap.RAGSourceScopeBindingModeFixed:
		if decl.ID == nil || *decl.ID == "" {
			return ScopeBinding{}, fmt.Errorf("%s binding is fixed but names no ID", level)
		}
		if registered != "" && registered != string(*decl.ID) {
			return ScopeBinding{}, fmt.Errorf("%s binding is fixed to another ID than the registration's", level)
		}
		return ScopeBinding{Mode: decl.Mode, ID: string(*decl.ID)}, nil
	case ap.RAGSourceScopeBindingModeRequest:
		if registered == "" {
			return ScopeBinding{}, fmt.Errorf("%s binding is request-bound: the registration must name the %s it serves", level, level)
		}
		return ScopeBinding{Mode: decl.Mode, ID: registered}, nil
	case ap.RAGSourceScopeBindingModeShared:
		if registered != "" {
			return ScopeBinding{}, fmt.Errorf("%s binding is shared: the registration must not name a %s", level, level)
		}
		return ScopeBinding{Mode: decl.Mode}, nil
	}
	return ScopeBinding{}, fmt.Errorf("unknown %s binding mode %q", level, string(decl.Mode))
}

func resolveRoot(decl ap.RAGSource, mounts MountResolver) (Root, error) {
	switch r := decl.Root.Value.(type) {
	case ap.RAGSourceRootFolder:
		if r.Path == "" {
			return Root{}, fmt.Errorf("folder root has no path")
		}
		return Root{Kind: RootFolder, Path: r.Path}, nil
	case ap.RAGSourceRootMount:
		if mounts == nil {
			return Root{}, fmt.Errorf("mount root %q needs a mount resolver", string(r.MountID))
		}
		host, ok := mounts(string(r.MountID))
		if !ok || host == "" {
			return Root{}, fmt.Errorf("mount %q is not registered", string(r.MountID))
		}
		sub := ""
		if r.Path != nil {
			sub = strings.Trim(string(*r.Path), "/")
		}
		if sub != "" {
			if err := (Paths{}).ValidKey(sub); err != nil {
				return Root{}, fmt.Errorf("mount sub-path is not a plain relative path")
			}
		}
		return Root{Kind: RootMount, Path: host, MountID: string(r.MountID), SubPath: sub}, nil
	case ap.RAGSourceRootAPI:
		return Root{Kind: RootAPI}, nil
	case ap.RAGSourceRootPackage:
		return Root{Kind: RootPackage}, nil
	}
	return Root{}, fmt.Errorf("no root")
}

func rootFitsType(t ap.RAGSourceType, k RootKind) bool {
	switch t {
	case ap.RAGSourceTypeLocalFolder:
		return k == RootFolder
	case ap.RAGSourceTypeMountedVolume:
		return k == RootMount
	case ap.RAGSourceTypeExportedDataset:
		return k == RootFolder || k == RootMount
	case ap.RAGSourceTypeApplicationDocumentAPI, ap.RAGSourceTypeHTTPAPI:
		return k == RootAPI
	case ap.RAGSourceTypeDocumentationPackage:
		return k == RootPackage
	}
	return false
}

// CheckScope reports whether the verified scope may use this source: the scope
// must be complete, name the application the source was declared for, and
// match the tenant and workspace the registration serves (a shared level
// admits any). It returns contracts.ErrScopeViolation otherwise. Every
// provider call starts with it.
func (d Definition) CheckScope(scope contracts.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if scope.ApplicationID != d.ApplicationID {
		return contracts.New(contracts.ErrScopeViolation, "the source belongs to another application")
	}
	if d.Tenant.Mode != ap.RAGSourceScopeBindingModeShared && scope.TenantID != d.Tenant.ID {
		return contracts.New(contracts.ErrScopeViolation, "the source belongs to another tenant")
	}
	if d.Workspace.Mode != ap.RAGSourceScopeBindingModeShared && scope.WorkspaceID != d.Workspace.ID {
		return contracts.New(contracts.ErrScopeViolation, "the source belongs to another workspace")
	}
	return nil
}

// AcceptsFormat reports whether the declaration lists the MIME type.
func (d Definition) AcceptsFormat(mime string) bool {
	for _, f := range d.Formats {
		if f == mime {
			return true
		}
	}
	return false
}
