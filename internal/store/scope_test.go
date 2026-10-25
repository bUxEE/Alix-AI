package store

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestScopedQueryRequiresScope(t *testing.T) {
	db := openTest(t, nil)
	ctx := context.Background()
	good := testScope("app", "tenantA", "w1")
	insertNote(t, db, good, "n1", "keep", "me")

	without := func(mut func(*contracts.Scope)) contracts.Scope {
		s := good.Clone()
		mut(&s)
		return s
	}
	cases := []struct {
		name  string
		scope contracts.Scope
		code  contracts.ErrCode
		level string
	}{
		{"zero scope", contracts.Scope{}, contracts.ErrScopeMissing, "application"},
		{"no application", without(func(s *contracts.Scope) { s.ApplicationID = "" }), contracts.ErrScopeMissing, "application"},
		{"no tenant", without(func(s *contracts.Scope) { s.TenantID = "" }), contracts.ErrScopeMissing, "tenant"},
		{"no workspace", without(func(s *contracts.Scope) { s.WorkspaceID = "" }), contracts.ErrScopeMissing, "workspace"},
		{"blank tenant", without(func(s *contracts.Scope) { s.TenantID = "   " }), contracts.ErrScopeMissing, "tenant"},
		{"no principal", without(func(s *contracts.Scope) { s.PrincipalID = "" }), contracts.ErrScopeMissing, "principal"},
		{"no session", without(func(s *contracts.Scope) { s.SessionID = "" }), contracts.ErrScopeMissing, "session"},
		{"no roles", without(func(s *contracts.Scope) { s.Roles = nil }), contracts.ErrScopeMissing, "roles"},
		{"wildcard tenant", without(func(s *contracts.Scope) { s.TenantID = "*" }), contracts.ErrScopeViolation, "tenant"},
		{"wildcard workspace", without(func(s *contracts.Scope) { s.WorkspaceID = "w*" }), contracts.ErrScopeViolation, "workspace"},
		{"padded application", without(func(s *contracts.Scope) { s.ApplicationID = " app" }), contracts.ErrScopeViolation, "application"},
	}
	stmt := ScopedStmt{SQL: `SELECT id FROM notes`}
	del := ScopedStmt{SQL: `DELETE FROM notes`}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check := func(op string, err error) {
				t.Helper()
				if err == nil {
					t.Fatalf("%s ran with an invalid scope", op)
				}
				if got := contracts.CodeOf(err); got != c.code {
					t.Errorf("%s: code = %s, want %s (%v)", op, got, c.code, err)
				}
				if !strings.Contains(err.Error(), c.level) {
					t.Errorf("%s: error %q does not name level %q", op, err, c.level)
				}
			}
			_, _, err := ScopedWhere(c.scope)
			check("ScopedWhere", err)
			_, _, err = ScopedWhereAs(c.scope, "n")
			check("ScopedWhereAs", err)
			rows, err := QueryScoped(ctx, db, c.scope, stmt)
			if rows != nil {
				rows.Close()
			}
			check("QueryScoped", err)
			row, err := QueryRowScoped(ctx, db, c.scope, stmt)
			if row != nil {
				t.Error("QueryRowScoped returned a row for an invalid scope")
			}
			check("QueryRowScoped", err)
			_, err = ExecScoped(ctx, db, c.scope, del)
			check("ExecScoped", err)
			_, err = InsertScoped(ctx, db, c.scope, "notes", []string{"id", "title", "body"}, "x", "x", "x")
			check("InsertScoped", err)
		})
	}
	// None of the refused statements touched the table.
	if n := count(t, db, `SELECT count(*) FROM notes`); n != 1 {
		t.Errorf("notes has %d rows, want the single original: a refused statement still ran", n)
	}

	// A valid scope builds exactly the documented predicate.
	pred, args, err := ScopedWhere(good)
	if err != nil {
		t.Fatal(err)
	}
	if want := "application_id = ? AND tenant_id = ? AND workspace_id = ?"; pred != want {
		t.Errorf("predicate = %q, want %q", pred, want)
	}
	if !reflect.DeepEqual(args, []any{"app", "tenantA", "w1"}) {
		t.Errorf("args = %v", args)
	}
	if pred, _, _ := ScopedWhereAs(good, "n"); !strings.HasPrefix(pred, "n.application_id = ? AND n.tenant_id") {
		t.Errorf("aliased predicate = %q", pred)
	}
}

func TestScopedQueriesIsolateTenants(t *testing.T) {
	db := openTest(t, nil)
	ctx := context.Background()
	a1 := testScope("app", "tenantA", "w1")
	a2 := testScope("app", "tenantA", "w2")
	b1 := testScope("app", "tenantB", "w1")
	other := testScope("other-app", "tenantA", "w1")
	for _, f := range []struct {
		sc contracts.Scope
		id string
	}{{a1, "a1-x"}, {a1, "a1-y"}, {a2, "a2-x"}, {b1, "b1-x"}, {other, "o-x"}} {
		insertNote(t, db, f.sc, f.id, "t "+f.id, "body")
	}

	list := func(sc contracts.Scope, extra ScopedStmt) []string {
		t.Helper()
		extra.SQL = `SELECT id FROM notes`
		rows, err := QueryScoped(ctx, db, sc, extra)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		sort.Strings(out)
		return out
	}
	for _, c := range []struct {
		name string
		sc   contracts.Scope
		want []string
	}{
		{"tenant A workspace 1", a1, []string{"a1-x", "a1-y"}},
		{"tenant A workspace 2", a2, []string{"a2-x"}},
		{"tenant B", b1, []string{"b1-x"}},
		{"other application", other, []string{"o-x"}},
	} {
		if got := list(c.sc, ScopedStmt{}); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}

	// An extra condition narrows, never widens: OR inside Where cannot escape
	// the scope because it is parenthesised.
	got := list(a1, ScopedStmt{Where: `id = ? OR 1 = 1`, WhereArgs: []any{"zzz"}})
	if !reflect.DeepEqual(got, []string{"a1-x", "a1-y"}) {
		t.Errorf("OR in Where escaped the scope: %v", got)
	}
	if got := list(a1, ScopedStmt{Where: `id LIKE ?`, WhereArgs: []any{"%-y"}, Tail: `LIMIT ?`, TailArgs: []any{5}}); !reflect.DeepEqual(got, []string{"a1-y"}) {
		t.Errorf("where+tail = %v", got)
	}

	// Single-row lookups, update and delete are scoped too: the same id in
	// another tenant is invisible and untouched.
	row, err := QueryRowScoped(ctx, db, b1, ScopedStmt{SQL: `SELECT id FROM notes`, Where: `id = ?`, WhereArgs: []any{"a1-x"}})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := row.Scan(&id); err == nil {
		t.Errorf("tenant B read tenant A's row %q", id)
	}
	res, err := ExecScoped(ctx, db, b1, ScopedStmt{
		SQL: `UPDATE notes SET title = ?`, Args: []any{"hijacked"},
		Where: `id = ?`, WhereArgs: []any{"a1-x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Errorf("tenant B updated %d of tenant A's rows", n)
	}
	res, err = ExecScoped(ctx, db, b1, ScopedStmt{SQL: `DELETE FROM notes`, Where: `id = ?`, WhereArgs: []any{"a1-x"}})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Errorf("tenant B deleted %d of tenant A's rows", n)
	}
	if n := count(t, db, `SELECT count(*) FROM notes WHERE id = 'a1-x' AND title = 't a1-x'`); n != 1 {
		t.Errorf("tenant A's row was changed by tenant B")
	}
	res, err = ExecScoped(ctx, db, a1, ScopedStmt{SQL: `DELETE FROM notes`, Where: `id = ?`, WhereArgs: []any{"a1-x"}})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("owner delete affected %d rows, want 1", n)
	}
}

func TestScopedStmtGuards(t *testing.T) {
	sc := testScope("app", "t", "w")
	bad := []struct {
		name string
		s    ScopedStmt
	}{
		{"empty sql", ScopedStmt{}},
		{"where in sql", ScopedStmt{SQL: `SELECT id FROM notes WHERE id = 1`}},
		{"lowercase where in sql", ScopedStmt{SQL: `select id from notes where id = 1`}},
		{"bad alias", ScopedStmt{SQL: `SELECT 1 FROM notes`, Alias: "n; DROP TABLE notes"}},
	}
	for _, c := range bad {
		if _, _, err := c.s.Build(sc); err == nil {
			t.Errorf("%s: Build accepted it", c.name)
		} else if contracts.CodeOf(err) != contracts.ErrInternal {
			t.Errorf("%s: code = %s, want internal", c.name, contracts.CodeOf(err))
		}
	}
	db := openTest(t, nil)
	ctx := context.Background()
	for name, f := range map[string]func() error{
		"bad table": func() error {
			_, err := InsertScoped(ctx, db, sc, "notes; DROP TABLE notes", []string{"id"}, "x")
			return err
		},
		"bad column": func() error {
			_, err := InsertScoped(ctx, db, sc, "notes", []string{"id) VALUES (1); --"}, "x")
			return err
		},
		"arity":           func() error { _, err := InsertScoped(ctx, db, sc, "notes", []string{"id", "title"}, "x"); return err },
		"override tenant": func() error { _, err := InsertScoped(ctx, db, sc, "notes", []string{"tenant_id"}, "evil"); return err },
		"override case": func() error {
			_, err := InsertScoped(ctx, db, sc, "notes", []string{"Workspace_ID"}, "evil")
			return err
		},
	} {
		if err := f(); err == nil {
			t.Errorf("InsertScoped %s: accepted", name)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM notes`); n != 0 {
		t.Errorf("a refused insert still wrote %d rows", n)
	}
}

func TestScopeLint(t *testing.T) {
	// A compliant schema (notes, notes_fts and its shadow tables) passes.
	good := openTest(t, nil)
	if v, err := CheckScopeColumns(context.Background(), good); err != nil || len(v) != 0 {
		t.Fatalf("compliant schema reported violations %v (err %v)", v, err)
	}

	// Every kind of violation is reported, and exemptions work.
	reg := NewRegistry()
	mustRegister(t, reg, "bad", map[string]string{"0001_init.sql": `
		CREATE TABLE no_scope (id TEXT PRIMARY KEY, name TEXT NOT NULL);
		CREATE TABLE partial (id TEXT PRIMARY KEY, application_id TEXT NOT NULL, tenant_id TEXT NOT NULL);
		CREATE TABLE nullable_ws (id TEXT PRIMARY KEY, application_id TEXT NOT NULL, tenant_id TEXT NOT NULL, workspace_id TEXT);
		CREATE TABLE int_tenant (id TEXT PRIMARY KEY, application_id TEXT NOT NULL, tenant_id INTEGER NOT NULL, workspace_id TEXT NOT NULL);
		CREATE TABLE ok_table (id TEXT PRIMARY KEY, application_id TEXT NOT NULL, tenant_id TEXT NOT NULL, workspace_id TEXT NOT NULL);
		CREATE TABLE deployment_flags (k TEXT PRIMARY KEY, v TEXT);
		CREATE VIRTUAL TABLE fts_no_scope USING fts5 (body);
		CREATE VIRTUAL TABLE fts_ok USING fts5 (body, application_id UNINDEXED, tenant_id UNINDEXED, workspace_id UNINDEXED);
		CREATE VIEW some_view AS SELECT id FROM no_scope;`})
	db := openTest(t, reg)
	got, err := CheckScopeColumns(context.Background(), db, "deployment_flags")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, v := range got {
		lines = append(lines, v.String())
	}
	want := []string{
		"fts_no_scope: missing column application_id",
		"fts_no_scope: missing column tenant_id",
		"fts_no_scope: missing column workspace_id",
		"int_tenant: column tenant_id must be TEXT, is \"INTEGER\"",
		"no_scope: missing column application_id",
		"no_scope: missing column tenant_id",
		"no_scope: missing column workspace_id",
		"nullable_ws: column workspace_id must be NOT NULL",
		"partial: missing column workspace_id",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("violations\n got %q\nwant %q", lines, want)
	}
	// schema_migrations is exempt by default, ok_table and fts_ok comply, and
	// the view and FTS shadow tables are skipped: none of them appear above.
}
