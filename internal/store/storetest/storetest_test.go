package storetest_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
)

func registry(t *testing.T, sql string) *store.Registry {
	t.Helper()
	reg := store.NewRegistry()
	fsys := fstest.MapFS{"migrations/0001_init.sql": &fstest.MapFile{Data: []byte(sql)}}
	if err := reg.Register("demo", fsys); err != nil {
		t.Fatal(err)
	}
	return reg
}

// recorder captures failures instead of failing the real test.
type recorder struct {
	testing.TB
	errs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, strings.TrimSpace(format))
}

func TestAssertScopedTablesPassesCompliantSchema(t *testing.T) {
	db := storetest.Open(t, registry(t, `
		CREATE TABLE things (
			id TEXT PRIMARY KEY,
			application_id TEXT NOT NULL, tenant_id TEXT NOT NULL, workspace_id TEXT NOT NULL
		);`))
	storetest.AssertScopedTables(t, db)
}

func TestAssertScopedTablesFailsViolations(t *testing.T) {
	db := storetest.Open(t, registry(t, `CREATE TABLE loose (id TEXT PRIMARY KEY);`))
	rec := &recorder{TB: t}
	storetest.AssertScopedTables(rec, db)
	if len(rec.errs) != 3 {
		t.Errorf("want one failure per missing scope column (3), got %d: %v", len(rec.errs), rec.errs)
	}
	rec = &recorder{TB: t}
	storetest.AssertScopedTables(rec, db, "loose")
	if len(rec.errs) != 0 {
		t.Errorf("an exempted table must not fail: %v", rec.errs)
	}
}

func TestScopeFixtureIsValid(t *testing.T) {
	if err := storetest.Scope("app", "tenantA", "ws1").Validate(); err != nil {
		t.Fatal(err)
	}
}
