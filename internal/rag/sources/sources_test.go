package sources

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var testStart = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func scopeOf(app, tenant, workspace string) contracts.Scope {
	s := storetest.Scope(app, tenant, workspace)
	s.PrincipalID, s.SessionID = "usr-registrar", "ses_registrar"
	return s
}

func regOf(scope contracts.Scope, slug, hash string) Registration {
	return Registration{
		Scope: scope, SourceID: ids.New(source.IDPrefix), PackageSourceID: slug, PackageVersion: "1.0.0", ManifestHash: hash,
		BoundTenantID: scope.TenantID, BoundWorkspaceID: scope.WorkspaceID,
	}
}

// ownMigrations is a migration registry holding this module only, so the tests
// see exactly its tables (the packages this one imports register theirs with
// store.Default).
func ownMigrations(t testing.TB) *store.Registry {
	t.Helper()
	reg := store.NewRegistry()
	if err := reg.Register(Module, Migrations()); err != nil {
		t.Fatal(err)
	}
	return reg
}

func openAt(t *testing.T, path string, clk clock.Clock) (*store.DB, *Registry) {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: path, Migrations: ownMigrations(t)})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	r, err := New(db, clk)
	if err != nil {
		t.Fatal(err)
	}
	return db, r
}

func codeOf(t *testing.T, err error, want contracts.ErrCode) {
	t.Helper()
	if got := contracts.CodeOf(err); err == nil || got != want {
		t.Fatalf("error = %v (code %q), want code %q", err, got, want)
	}
}

// A registration is still there after the store is closed and opened again, with
// everything a rebuild needs: the declaration it came from, the package content
// it was bound to, the tenant and workspace the definition names (empty for a
// shared level), the scope it was registered under, its removal state and its
// times.
func TestRegistrySurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	clk := clock.NewFake(testStart)

	db, r := openAt(t, path, clk)
	scope := scopeOf("demo-library", "t1", "w1")
	request := regOf(scope, "handbook", "hash-1")
	shared := regOf(scope, "loan-policies", "hash-1")
	shared.BoundTenantID, shared.BoundWorkspaceID = "", ""
	removing := regOf(scope, "old-docs", "hash-1")
	for _, reg := range []Registration{request, shared, removing} {
		if err := r.Put(ctx, reg); err != nil {
			t.Fatalf("Put %s: %v", reg.PackageSourceID, err)
		}
	}
	clk.Advance(time.Minute)
	if ok, err := r.MarkRemoving(ctx, scope, removing.SourceID); err != nil || !ok {
		t.Fatalf("MarkRemoving: %v, %v", ok, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The process starts again over the same file.
	clk.Advance(time.Hour)
	db2, r2 := openAt(t, path, clk)
	t.Cleanup(func() { _ = db2.Close() })
	all, err := r2.LoadAll(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("LoadAll after a restart = %d registrations, %v; want 3", len(all), err)
	}
	byID := map[string]Registration{}
	for _, reg := range all {
		byID[reg.SourceID] = reg
	}
	for _, want := range []Registration{request, shared} {
		got, ok := byID[want.SourceID]
		if !ok {
			t.Fatalf("%s did not survive the restart", want.PackageSourceID)
		}
		want.CreatedAt, want.UpdatedAt = testStart, testStart
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%s after the restart (-want +got):\n%s", want.PackageSourceID, diff)
		}
	}
	gone := byID[removing.SourceID]
	if !gone.Removing || !gone.UpdatedAt.Equal(testStart.Add(time.Minute)) {
		t.Errorf("a source whose removal began is read back as %+v; it must stay removing so it is never served and the removal can be finished", gone)
	}

	// The scoped reads agree with the whole-deployment one.
	got, ok, err := r2.Get(ctx, scope, request.SourceID)
	if err != nil || !ok || got.PackageSourceID != "handbook" || got.ManifestHash != "hash-1" {
		t.Errorf("Get after the restart = %+v, %v, %v", got, ok, err)
	}
	if list, err := r2.List(ctx, scope); err != nil || len(list) != 3 {
		t.Errorf("List after the restart = %d, %v", len(list), err)
	}

	// The end of a removal is a delete, and it too is remembered.
	if ok, err := r2.Delete(ctx, scope, removing.SourceID); err != nil || !ok {
		t.Fatalf("Delete: %v, %v", ok, err)
	}
	if ok, err := r2.Delete(ctx, scope, removing.SourceID); err != nil || ok {
		t.Errorf("deleting twice = %v, %v; the second must report there was nothing", ok, err)
	}
	if all, _ := r2.LoadAll(ctx); len(all) != 2 {
		t.Errorf("%d registrations after the delete, want 2", len(all))
	}
}

// A registration belongs to the application, tenant and workspace it was made
// for. Another tenant or workspace reads, changes and removes nothing of it, and
// is answered exactly as for a source that does not exist; the same slug may be
// registered once per scope.
func TestRegistryIsScopedByTenantAndWorkspace(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t, ownMigrations(t))
	storetest.AssertScopedTables(t, db)
	r, err := New(db, clock.NewFake(testStart))
	if err != nil {
		t.Fatal(err)
	}

	a := scopeOf("demo-library", "t1", "w1")
	otherTenant := scopeOf("demo-library", "t2", "w1")
	otherWorkspace := scopeOf("demo-library", "t1", "w2")
	otherApp := scopeOf("another-app", "t1", "w1")
	regs := map[string]Registration{}
	for name, scope := range map[string]contracts.Scope{"a": a, "tenant": otherTenant, "workspace": otherWorkspace, "app": otherApp} {
		reg := regOf(scope, "handbook", "hash-1")
		if err := r.Put(ctx, reg); err != nil {
			t.Fatalf("the same slug in scope %s: %v", name, err)
		}
		regs[name] = reg
	}

	for name, scope := range map[string]contracts.Scope{"tenant": otherTenant, "workspace": otherWorkspace, "app": otherApp} {
		if _, ok, err := r.Get(ctx, scope, regs["a"].SourceID); err != nil || ok {
			t.Errorf("scope %s read the source of scope a: %v, %v", name, ok, err)
		}
		if moved, err := r.Rebind(ctx, scope, regs["a"].SourceID, "hash-1", "hash-2", "2.0.0"); err != nil || moved {
			t.Errorf("scope %s re-bound the source of scope a: %v, %v", name, moved, err)
		}
		if ok, err := r.MarkRemoving(ctx, scope, regs["a"].SourceID); err != nil || ok {
			t.Errorf("scope %s began the removal of the source of scope a: %v, %v", name, ok, err)
		}
		if ok, err := r.Delete(ctx, scope, regs["a"].SourceID); err != nil || ok {
			t.Errorf("scope %s deleted the source of scope a: %v, %v", name, ok, err)
		}
		list, err := r.List(ctx, scope)
		if err != nil || len(list) != 1 || list[0].SourceID != regs[name].SourceID {
			t.Errorf("List of scope %s = %+v, %v; it must hold its own source only", name, list, err)
		}
	}
	got, ok, err := r.Get(ctx, a, regs["a"].SourceID)
	if err != nil || !ok || got.ManifestHash != "hash-1" || got.Removing {
		t.Errorf("the source of scope a was changed by another scope: %+v, %v, %v", got, ok, err)
	}

	// The registering scope is what keys a row: writing a source under a scope
	// that says another application's tenant is writing there, not here.
	if all, err := r.LoadAll(ctx); err != nil || len(all) != 4 {
		t.Errorf("LoadAll = %d, %v", len(all), err)
	}

	// An incomplete scope reads and writes nothing.
	for name, bad := range map[string]contracts.Scope{
		"no tenant":    {DeploymentID: "d", ApplicationID: "demo-library", WorkspaceID: "w1", PrincipalID: "p", Roles: []string{"r"}, CapabilityScopes: []string{"c"}, SessionID: "s"},
		"a wildcard":   func() contracts.Scope { s := a; s.TenantID = "*"; return s }(),
		"a zero scope": {},
	} {
		if _, _, err := r.Get(ctx, bad, regs["a"].SourceID); err == nil {
			t.Errorf("%s: Get accepted the scope", name)
		}
		if _, err := r.List(ctx, bad); err == nil {
			t.Errorf("%s: List accepted the scope", name)
		}
		if _, err := r.Delete(ctx, bad, regs["a"].SourceID); err == nil {
			t.Errorf("%s: Delete accepted the scope", name)
		}
		reg := regOf(a, "x", "h")
		reg.Scope = bad
		if err := r.Put(ctx, reg); err == nil {
			t.Errorf("%s: Put accepted the scope", name)
		}
	}
}

// A registration is bound to the content hash of the package it was made from,
// and the binding moves only from the value the caller read: of two re-audits
// that both read hash A, the one that moves it to B wins and the other, which
// would have moved it to C, changes nothing. It survives a restart, and a source
// whose removal began or that is gone cannot be re-bound.
func TestRegistryBindsToPackageContentHash(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bind.db")
	clk := clock.NewFake(testStart)
	db, r := openAt(t, path, clk)
	scope := scopeOf("demo-library", "t1", "w1")

	for name, reg := range map[string]Registration{
		"no content hash":   regOf(scope, "handbook", ""),
		"no declaration":    regOf(scope, "", "hash-A"),
		"not a source id":   func() Registration { x := regOf(scope, "handbook", "hash-A"); x.SourceID = "docs"; return x }(),
		"a padded hash":     regOf(scope, "handbook", " hash-A"),
		"a control in hash": regOf(scope, "handbook", "hash\x00A"),
	} {
		if err := r.Put(ctx, reg); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: Put = %v, want validation_failed", name, err)
		}
	}
	if all, _ := r.LoadAll(ctx); len(all) != 0 {
		t.Fatalf("a refused registration was stored: %+v", all)
	}

	reg := regOf(scope, "handbook", "hash-A")
	if err := r.Put(ctx, reg); err != nil {
		t.Fatal(err)
	}
	hashOf := func() (string, string) {
		got, ok, err := r.Get(ctx, scope, reg.SourceID)
		if err != nil || !ok {
			t.Fatalf("Get: %v, %v", ok, err)
		}
		return got.ManifestHash, got.PackageVersion
	}
	if h, v := hashOf(); h != "hash-A" || v != "1.0.0" {
		t.Fatalf("bound to %s %s, want hash-A 1.0.0", h, v)
	}

	clk.Advance(time.Minute)
	if moved, err := r.Rebind(ctx, scope, reg.SourceID, "hash-A", "hash-B", "2.0.0"); err != nil || !moved {
		t.Fatalf("Rebind A to B = %v, %v", moved, err)
	}
	if moved, err := r.Rebind(ctx, scope, reg.SourceID, "hash-A", "hash-C", "3.0.0"); err != nil || moved {
		t.Fatalf("a re-audit that read hash A moved a registration already bound to B: %v, %v", moved, err)
	}
	if h, v := hashOf(); h != "hash-B" || v != "2.0.0" {
		t.Fatalf("bound to %s %s after the lost race, want hash-B 2.0.0", h, v)
	}
	for name, args := range map[string][3]string{
		"an empty origin":      {"", "hash-D", "1"},
		"an empty destination": {"hash-B", "", "1"},
		"a padded version":     {"hash-B", "hash-D", " 1"},
	} {
		if _, err := r.Rebind(ctx, scope, reg.SourceID, args[0], args[1], args[2]); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Registering again under the same id re-binds it too (the registration of a
	// definition rebuilt from the active package), keeping the first time.
	again := reg
	again.ManifestHash, again.PackageVersion = "hash-E", "5.0.0"
	if err := r.Put(ctx, again); err != nil {
		t.Fatal(err)
	}
	if h, _ := hashOf(); h != "hash-E" {
		t.Errorf("registering again left the registration bound to %s", h)
	}
	if err := r.Put(ctx, Registration{
		Scope: scope, SourceID: ids.New(source.IDPrefix), PackageSourceID: "handbook", ManifestHash: "hash-E",
	}); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("a second source id for one declaration of one scope = %v, want conflict", err)
	}

	// The binding is what a restarted process reads.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, r2 := openAt(t, path, clk)
	t.Cleanup(func() { _ = db2.Close() })
	all, err := r2.LoadAll(ctx)
	if err != nil || len(all) != 1 || all[0].ManifestHash != "hash-E" || all[0].PackageVersion != "5.0.0" || !all[0].CreatedAt.Equal(testStart) {
		t.Fatalf("after the restart: %+v, %v (bound to hash-E, created at the first registration)", all, err)
	}

	// A removal that began ends every change of the binding, and registering the
	// source again.
	if ok, err := r2.MarkRemoving(ctx, scope, reg.SourceID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if moved, err := r2.Rebind(ctx, scope, reg.SourceID, "hash-E", "hash-F", "6.0.0"); err != nil || moved {
		t.Errorf("a source being removed was re-bound: %v, %v", moved, err)
	}
	codeOf(t, r2.Put(ctx, again), contracts.ErrConflict)
	if ok, err := r2.Delete(ctx, scope, reg.SourceID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if moved, err := r2.Rebind(ctx, scope, reg.SourceID, "hash-E", "hash-F", "6.0.0"); err != nil || moved {
		t.Errorf("a removed source was re-bound: %v, %v", moved, err)
	}
	// With the removal finished the declaration can be registered again, as a new source.
	if err := r2.Put(ctx, regOf(scope, "handbook", "hash-F")); err != nil {
		t.Errorf("registering the declaration again after its removal: %v", err)
	}
}

// A row whose stored scope is not the scope of the row is not served: it would
// carry the work of one tenant under the scope of another.
func TestRegistryRefusesARowWhoseScopeDisagrees(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t, ownMigrations(t))
	r, err := New(db, clock.NewFake(testStart))
	if err != nil {
		t.Fatal(err)
	}
	scope := scopeOf("demo-library", "t1", "w1")
	reg := regOf(scope, "handbook", "hash-1")
	if err := r.Put(ctx, reg); err != nil {
		t.Fatal(err)
	}
	for name, stored := range map[string]string{
		"another tenant":    `{"deployment_id":"dep_test","application_id":"demo-library","tenant_id":"t9","workspace_id":"w1","principal_id":"p","roles":["r"],"capability_scopes":["c"],"session_id":"s"}`,
		"not a scope":       `[1]`,
		"an incomplete one": `{"application_id":"demo-library","tenant_id":"t1","workspace_id":"w1"}`,
	} {
		if _, err := db.ExecContext(ctx, `UPDATE ragsources_registration SET scope_json = ?`, stored); err != nil {
			t.Fatal(err)
		}
		if _, err := r.LoadAll(ctx); err == nil || !strings.Contains(err.Error(), "unreadable") {
			t.Errorf("%s: LoadAll = %v", name, err)
		}
		if _, _, err := r.Get(ctx, scope, reg.SourceID); err == nil {
			t.Errorf("%s: Get returned a row whose scope disagrees", name)
		}
	}
}

func TestRegistryNeedsAStore(t *testing.T) {
	if _, err := New(nil, nil); contracts.CodeOf(err) != contracts.ErrInternal {
		t.Errorf("New(nil) = %v", err)
	}
	if !IsNotFound(errNotFound()) || IsNotFound(contracts.New(contracts.ErrValidationFailed, "x")) || IsNotFound(nil) {
		t.Error("IsNotFound does not recognise exactly the not-found answer")
	}
}
