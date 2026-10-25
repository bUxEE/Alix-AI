// Package storetest holds test helpers for packages that use internal/store:
// opening a throwaway database, building valid scopes for cross-tenant
// fixtures and asserting that a module's tables carry the scope columns.
package storetest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Open opens a fresh database in t.TempDir() with the given migration
// registry (nil means store.Default) and closes it when the test ends. mutate
// may adjust the Options (cache sizes, retry settings) before opening.
func Open(t testing.TB, reg *store.Registry, mutate ...func(*store.Options)) *store.DB {
	t.Helper()
	opts := store.Options{
		Path:       filepath.Join(t.TempDir(), "test.db"),
		Migrations: reg,
	}
	for _, m := range mutate {
		m(&opts)
	}
	db, err := store.Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return db
}

// Scope returns a fully populated, valid contracts.Scope for the given
// application, tenant and workspace, for building cross-tenant fixtures.
func Scope(application, tenant, workspace string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep_test",
		ApplicationID:    application,
		TenantID:         tenant,
		WorkspaceID:      workspace,
		PrincipalID:      "usr_test",
		Roles:            []string{"member"},
		CapabilityScopes: []string{"test.read"},
		SessionID:        "ses_test",
	}
}

// AssertScopedTables fails t for every table in db that lacks the
// application_id, tenant_id and workspace_id columns (TEXT, NOT NULL).
// schema_migrations is always exempt; list further tables that legitimately
// hold only deployment-wide data in exempt.
func AssertScopedTables(t testing.TB, db *store.DB, exempt ...string) {
	t.Helper()
	violations, err := store.CheckScopeColumns(context.Background(), db, exempt...)
	if err != nil {
		t.Fatalf("scope lint could not inspect the schema: %v", err)
	}
	for _, v := range violations {
		t.Errorf("table %s breaks the scope rule: %s (every table needs application_id, tenant_id, workspace_id TEXT NOT NULL)", v.Table, v.Problem)
	}
}
