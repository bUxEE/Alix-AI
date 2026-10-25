package source

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

func ptr[T any](v T) *T { return &v }

func baseDecl() ap.RAGSource {
	return ap.RAGSource{
		ID:    "handbook",
		Type:  ap.RAGSourceTypeLocalFolder,
		Title: ptr(ap.Label("Handbook")),
		Owner: ap.RAGSourceOwner{Name: "Branch managers"},
		Scope: ap.RAGSourceScope{
			ApplicationID: "demo-library",
			Tenant:        ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest},
			Workspace:     ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest},
		},
		Root:     ap.RAGSourceRoot{Value: ap.RAGSourceRootFolder{Kind: "folder", Path: "/srv/handbooks"}},
		Include:  []ap.GlobPattern{"**/*.md"},
		Exclude:  []ap.GlobPattern{"**/draft-*"},
		Formats:  []ap.MimeType{"text/markdown"},
		Limits:   ap.RAGSourceLimits{MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 30, MaxFiles: 1000},
		Sync:     ap.RAGSourceSync{Method: ap.RAGSourceSyncMethodPoll, IntervalSeconds: ptr(int64(3600))},
		Deletion: ap.RAGSourceDeletion{PurgeAfterHours: 24, OnSourceRemoved: ap.RAGSourceDeletionOnSourceRemovedDisable},
	}
}

func baseBinding() Binding {
	return Binding{SourceID: ids.New(IDPrefix), TenantID: "tenant-1", WorkspaceID: "ws-1"}
}

func wantCode(t *testing.T, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil || contracts.CodeOf(err) != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func TestFromDeclaration(t *testing.T) {
	d, err := FromDeclaration(baseDecl(), baseBinding())
	if err != nil {
		t.Fatal(err)
	}
	if d.PackageSourceID != "handbook" || d.Title != "Handbook" || d.ApplicationID != "demo-library" ||
		d.Root.Kind != RootFolder || d.Root.Path != "/srv/handbooks" ||
		d.Tenant.ID != "tenant-1" || d.Workspace.ID != "ws-1" ||
		d.Limits.MaxFileBytes != 1<<20 || d.Limits.MaxFiles != 1000 ||
		d.Deletion.PurgeAfter != 24*time.Hour || d.Deletion.OnSourceRemoved != ap.RAGSourceDeletionOnSourceRemovedDisable ||
		d.Sync.Interval != time.Hour || len(d.Include) != 1 || len(d.Exclude) != 1 || !d.AcceptsFormat("text/markdown") || d.AcceptsFormat("application/pdf") {
		t.Errorf("definition = %+v", d)
	}
}

func TestFromDeclarationFailsClosed(t *testing.T) {
	tests := map[string]func(*ap.RAGSource, *Binding){
		"no source id":           func(_ *ap.RAGSource, b *Binding) { b.SourceID = "" },
		"wrong id prefix":        func(_ *ap.RAGSource, b *Binding) { b.SourceID = ids.New("ses") },
		"malformed id":           func(_ *ap.RAGSource, b *Binding) { b.SourceID = "src_nope" },
		"unknown type":           func(d *ap.RAGSource, _ *Binding) { d.Type = "crawl_everything" },
		"no include rule":        func(d *ap.RAGSource, _ *Binding) { d.Include = nil },
		"no formats":             func(d *ap.RAGSource, _ *Binding) { d.Formats = nil },
		"zero file limit":        func(d *ap.RAGSource, _ *Binding) { d.Limits.MaxFileBytes = 0 },
		"zero total limit":       func(d *ap.RAGSource, _ *Binding) { d.Limits.MaxTotalBytes = 0 },
		"zero file count":        func(d *ap.RAGSource, _ *Binding) { d.Limits.MaxFiles = 0 },
		"bad deletion behaviour": func(d *ap.RAGSource, _ *Binding) { d.Deletion.OnSourceRemoved = "shred" },
		"bad sync method":        func(d *ap.RAGSource, _ *Binding) { d.Sync.Method = "always" },
		"no application":         func(d *ap.RAGSource, _ *Binding) { d.Scope.ApplicationID = "" },
		"request tenant needs a registered tenant": func(_ *ap.RAGSource, b *Binding) {
			b.TenantID = ""
		},
		"request workspace needs a registered workspace": func(_ *ap.RAGSource, b *Binding) {
			b.WorkspaceID = ""
		},
		"fixed tenant without an id": func(d *ap.RAGSource, _ *Binding) {
			d.Scope.Tenant = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeFixed}
		},
		"fixed tenant registered for another": func(d *ap.RAGSource, _ *Binding) {
			d.Scope.Tenant = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeFixed, ID: ptr(common.Identifier("tenant-9"))}
		},
		"shared tenant with a registered tenant": func(d *ap.RAGSource, _ *Binding) {
			d.Scope.Tenant = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeShared}
			d.Scope.Workspace = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeShared}
		},
		"shared tenant with a bound workspace": func(d *ap.RAGSource, b *Binding) {
			d.Scope.Tenant = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeShared}
			b.TenantID = ""
		},
		"unknown binding mode": func(d *ap.RAGSource, _ *Binding) {
			d.Scope.Tenant = ap.RAGSourceScopeBinding{Mode: "everyone"}
		},
		"no root": func(d *ap.RAGSource, _ *Binding) { d.Root = ap.RAGSourceRoot{} },
		"folder root with no path": func(d *ap.RAGSource, _ *Binding) {
			d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootFolder{Kind: "folder"}}
		},
		"mount root without a resolver": func(d *ap.RAGSource, _ *Binding) {
			d.Type = ap.RAGSourceTypeMountedVolume
			d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootMount{Kind: "mount", MountID: "nas-1"}}
		},
		"mount that is not registered": func(d *ap.RAGSource, b *Binding) {
			d.Type = ap.RAGSourceTypeMountedVolume
			d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootMount{Kind: "mount", MountID: "nas-1"}}
			b.Mounts = func(string) (string, bool) { return "", false }
		},
		"local folder with a mount root": func(d *ap.RAGSource, b *Binding) {
			d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootMount{Kind: "mount", MountID: "nas-1"}}
			b.Mounts = func(string) (string, bool) { return "/mnt/nas", true }
		},
		"mounted volume with a folder root": func(d *ap.RAGSource, _ *Binding) { d.Type = ap.RAGSourceTypeMountedVolume },
		"api type with a folder root":       func(d *ap.RAGSource, _ *Binding) { d.Type = ap.RAGSourceTypeHTTPAPI },
		"mount sub-path that climbs": func(d *ap.RAGSource, b *Binding) {
			d.Type = ap.RAGSourceTypeMountedVolume
			p := ap.RelativePath("a/../../b")
			d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootMount{Kind: "mount", MountID: "nas-1", Path: &p}}
			b.Mounts = func(string) (string, bool) { return "/mnt/nas", true }
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			d, b := baseDecl(), baseBinding()
			mutate(&d, &b)
			if _, err := FromDeclaration(d, b); err == nil {
				t.Fatal("definition accepted")
			} else if c := contracts.CodeOf(err); c != contracts.ErrValidationFailed {
				t.Fatalf("code = %s (%v), want validation_failed", c, err)
			}
		})
	}
}

func TestFromDeclarationBindingsAndMounts(t *testing.T) {
	d := baseDecl()
	d.Type = ap.RAGSourceTypeMountedVolume
	sub := ap.RelativePath("handbooks/2026")
	d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootMount{Kind: "mount", MountID: "nas-1", Path: &sub}}
	d.Scope.Tenant = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeFixed, ID: ptr(common.Identifier("tenant-7"))}
	b := baseBinding()
	b.TenantID = ""
	b.Mounts = func(id string) (string, bool) { return "/mnt/" + id, id == "nas-1" }
	got, err := FromDeclaration(d, b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != (Root{Kind: RootMount, Path: "/mnt/nas-1", MountID: "nas-1", SubPath: "handbooks/2026"}) || got.Tenant != (ScopeBinding{Mode: ap.RAGSourceScopeBindingModeFixed, ID: "tenant-7"}) {
		t.Errorf("definition = %+v", got)
	}

	shared := baseDecl()
	shared.Scope.Tenant = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeShared}
	shared.Scope.Workspace = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeShared}
	got, err = FromDeclaration(shared, Binding{SourceID: ids.New(IDPrefix)})
	if err != nil || got.Tenant.ID != "" || got.Workspace.ID != "" {
		t.Errorf("shared definition = %+v, %v", got, err)
	}

	api := baseDecl()
	api.Type = ap.RAGSourceTypeHTTPAPI
	ep := ap.Slug("library-mcp")
	api.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootAPI{Kind: "api", Endpoint: &ep}}
	got, err = FromDeclaration(api, baseBinding())
	if err != nil || got.Root.Kind != RootAPI || got.Decl.ID != "handbook" {
		t.Errorf("api definition = %+v, %v", got, err)
	}
}

func scopeFor(app, tenant, ws string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-1", ApplicationID: app, TenantID: tenant, WorkspaceID: ws,
		PrincipalID: "user-1", Roles: []string{"reader"}, CapabilityScopes: []string{"rag.search"}, SessionID: "sess-1",
	}
}

func TestCheckScope(t *testing.T) {
	d, err := FromDeclaration(baseDecl(), baseBinding())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CheckScope(scopeFor("demo-library", "tenant-1", "ws-1")); err != nil {
		t.Errorf("matching scope: %v", err)
	}
	wantCode(t, d.CheckScope(contracts.Scope{}), contracts.ErrScopeMissing)
	wantCode(t, d.CheckScope(scopeFor("other-app", "tenant-1", "ws-1")), contracts.ErrScopeViolation)
	wantCode(t, d.CheckScope(scopeFor("demo-library", "tenant-2", "ws-1")), contracts.ErrScopeViolation)
	wantCode(t, d.CheckScope(scopeFor("demo-library", "tenant-1", "ws-2")), contracts.ErrScopeViolation)
	wantCode(t, d.CheckScope(scopeFor("demo-library", "*", "ws-1")), contracts.ErrScopeViolation)

	shared := baseDecl()
	shared.Scope.Tenant = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeShared}
	shared.Scope.Workspace = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeShared}
	sd, err := FromDeclaration(shared, Binding{SourceID: ids.New(IDPrefix)})
	if err != nil {
		t.Fatal(err)
	}
	if err := sd.CheckScope(scopeFor("demo-library", "any-tenant", "any-ws")); err != nil {
		t.Errorf("a shared source admits any tenant and workspace of its application: %v", err)
	}
	wantCode(t, sd.CheckScope(scopeFor("other-app", "any-tenant", "any-ws")), contracts.ErrScopeViolation)
}

func TestSentinelErrors(t *testing.T) {
	wrapped := fmt.Errorf("%w: \"a/b\"", ErrEscapesRoot)
	if !errors.Is(wrapped, ErrEscapesRoot) || contracts.CodeOf(ErrEscapesRoot) != contracts.ErrScopeViolation {
		t.Error("ErrEscapesRoot")
	}
	if !errors.Is(ErrNotFound, ErrNotFound) || contracts.CodeOf(ErrNotFound) != contracts.ErrStale {
		t.Error("ErrNotFound")
	}
	if contracts.CodeOf(ErrNotAdmitted) != contracts.ErrPermissionDenied {
		t.Error("ErrNotAdmitted")
	}
	if strings.Contains(ErrEscapesRoot.Message, "/") {
		t.Error("a sentinel message must not carry a path")
	}
}

// FromPackage works on a package the loader produced: the committed
// valid-minimal fixture declares one documentation source.
func TestFromPackage(t *testing.T) {
	res, err := apppkg.Load(context.Background(), apppkg.FromDir("../../../examples/packages/valid-minimal"), apppkg.Options{CoreVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("fixture did not load: %+v", res.Findings)
	}
	b := Binding{SourceID: ids.New(IDPrefix)}
	d, err := FromPackage(res.Package, "loan-policies", b)
	if err != nil {
		t.Fatal(err)
	}
	if d.PackageSourceID != "loan-policies" || d.ApplicationID != "demo-library" || d.Type != ap.RAGSourceTypeDocumentationPackage ||
		d.Root.Kind != RootPackage || d.Tenant.Mode != ap.RAGSourceScopeBindingModeShared ||
		d.PackageVersion == "" || d.PackageVersion != res.Package.PackageVersion() ||
		d.ManifestHash == "" || d.ManifestHash != res.ManifestHash ||
		!d.AcceptsFormat("text/markdown") || len(d.Exclude) != 1 || d.Deletion.PurgeAfter != 0 {
		t.Errorf("definition = %+v", d)
	}
	if _, err := FromPackage(res.Package, "no-such-source", b); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("an undeclared source: %v", err)
	}
	if _, err := FromPackage(nil, "loan-policies", b); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("no package: %v", err)
	}
	if _, err := FromPackage(res.Package, "loan-policies", Binding{}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("no source ID: %v", err)
	}
	// A shared source must not be registered for a tenant.
	if _, err := FromPackage(res.Package, "loan-policies", Binding{SourceID: b.SourceID, TenantID: "tenant-1"}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("a tenant on a shared source: %v", err)
	}
}
