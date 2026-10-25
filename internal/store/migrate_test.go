package store

import (
	"context"
	"embed"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/buxee/alix-ai/pkg/contracts"
)

//go:embed testdata/embedmod/migrations/*.sql
var embeddedMigrations embed.FS

type appliedRow struct {
	Module  string
	Version int
	Name    string
}

// appliedOrder returns schema_migrations in the order the migrations ran.
func appliedOrder(t *testing.T, db *DB) []appliedRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT module, version, name FROM schema_migrations ORDER BY applied_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []appliedRow
	for rows.Next() {
		var r appliedRow
		if err := rows.Scan(&r.Module, &r.Version, &r.Name); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func tableExists(t *testing.T, db *DB, name string) bool {
	t.Helper()
	return count(t, db, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name) == 1
}

func TestMigrationsPerModuleOrdered(t *testing.T) {
	// Registered zeta first, alpha second: the opposite of alphabetical order.
	// alpha's table references zeta's, so the cross-module foreign key can only
	// be created if modules run in registration order, not name order.
	reg := NewRegistry()
	mustRegister(t, reg, "zeta", map[string]string{
		"0001_base.sql": `CREATE TABLE zeta_things (id INTEGER PRIMARY KEY, label TEXT NOT NULL);`,
		"0002_more.sql": `ALTER TABLE zeta_things ADD COLUMN note TEXT;`,
	})
	mustRegister(t, reg, "alpha", map[string]string{
		"0001_link.sql":  `CREATE TABLE alpha_links (id INTEGER PRIMARY KEY, thing INTEGER NOT NULL REFERENCES zeta_things(id));`,
		"0002_index.sql": `CREATE INDEX alpha_links_thing ON alpha_links (thing);`,
	})
	clk := newTickClock()
	opts := Options{Path: filepath.Join(t.TempDir(), "m.db"), Migrations: reg, Clock: clk}
	db, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := []appliedRow{
		{"zeta", 1, "0001_base.sql"},
		{"zeta", 2, "0002_more.sql"},
		{"alpha", 1, "0001_link.sql"},
		{"alpha", 2, "0002_index.sql"},
	}
	if got := appliedOrder(t, db); !reflect.DeepEqual(got, want) {
		t.Errorf("migrations ran in order\n got %v\nwant %v", got, want)
	}
	// The column added by zeta/0002 exists, so version order held inside a module.
	if _, err := db.ExecContext(context.Background(), `INSERT INTO zeta_things (label, note) VALUES ('x', 'y')`); err != nil {
		t.Errorf("zeta 0002 did not apply: %v", err)
	}
	// The recorded checksum is the SHA-256 of the file.
	var sum string
	if err := db.QueryRowContext(context.Background(),
		`SELECT checksum FROM schema_migrations WHERE module = 'alpha' AND version = 2`).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if want := checksum([]byte(`CREATE INDEX alpha_links_thing ON alpha_links (thing);`)); sum != want || len(sum) != 64 {
		t.Errorf("recorded checksum %q, want %q", sum, want)
	}
	stamps := count(t, db, `SELECT count(DISTINCT applied_at) FROM schema_migrations`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-opening is idempotent: nothing is applied again, nothing is re-stamped.
	db2, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if got := appliedOrder(t, db2); !reflect.DeepEqual(got, want) {
		t.Errorf("after reopen: %v", got)
	}
	if got := count(t, db2, `SELECT count(DISTINCT applied_at) FROM schema_migrations`); got != stamps || stamps != 4 {
		t.Errorf("distinct applied_at = %d (was %d), want 4 unchanged", got, stamps)
	}

	// A later release adds 0003 to one module and a whole new module; only
	// those run, after everything already applied.
	reg3 := NewRegistry()
	mustRegister(t, reg3, "zeta", map[string]string{
		"0001_base.sql": `CREATE TABLE zeta_things (id INTEGER PRIMARY KEY, label TEXT NOT NULL);`,
		"0002_more.sql": `ALTER TABLE zeta_things ADD COLUMN note TEXT;`,
		"0003_flag.sql": `ALTER TABLE zeta_things ADD COLUMN flag INTEGER NOT NULL DEFAULT 0;`,
	})
	mustRegister(t, reg3, "alpha", map[string]string{
		"0001_link.sql":  `CREATE TABLE alpha_links (id INTEGER PRIMARY KEY, thing INTEGER NOT NULL REFERENCES zeta_things(id));`,
		"0002_index.sql": `CREATE INDEX alpha_links_thing ON alpha_links (thing);`,
	})
	mustRegister(t, reg3, "mid", map[string]string{"0001_init.sql": `CREATE TABLE mid_t (id INTEGER PRIMARY KEY);`})
	db2.Close()
	opts.Migrations = reg3
	db3, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer db3.Close()
	got := appliedOrder(t, db3)
	if len(got) != 6 || got[4] != (appliedRow{"zeta", 3, "0003_flag.sql"}) || got[5] != (appliedRow{"mid", 1, "0001_init.sql"}) {
		t.Errorf("upgrade did not apply exactly the new migrations in order: %v", got)
	}
}

func TestMigrationChecksumMismatchRefuses(t *testing.T) {
	v1 := map[string]string{
		"0001_init.sql": `CREATE TABLE widgets (id INTEGER PRIMARY KEY, name TEXT NOT NULL);`,
		"0002_more.sql": `CREATE TABLE gadgets (id INTEGER PRIMARY KEY);`,
	}
	reg := NewRegistry()
	mustRegister(t, reg, "shop", v1)
	opts := Options{Path: filepath.Join(t.TempDir(), "c.db"), Migrations: reg}
	db, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	before := appliedOrder(t, db)
	db.Close()

	// Someone edits an applied migration (a harmless-looking comment is enough)
	// and ships a new migration and a new module in the same release.
	edited := map[string]string{
		"0001_init.sql": v1["0001_init.sql"] + "\n-- tweaked after release",
		"0002_more.sql": v1["0002_more.sql"],
		"0003_new.sql":  `CREATE TABLE should_not_exist (id INTEGER PRIMARY KEY);`,
	}
	reg2 := NewRegistry()
	mustRegister(t, reg2, "audit", map[string]string{"0001_init.sql": `CREATE TABLE audit_should_not_exist (id INTEGER PRIMARY KEY);`})
	mustRegister(t, reg2, "shop", edited)
	opts.Migrations = reg2

	db2, err := Open(context.Background(), opts)
	if err == nil {
		db2.Close()
		t.Fatal("Open accepted a migration whose checksum changed")
	}
	if got := contracts.CodeOf(err); got != contracts.ErrInternal {
		t.Errorf("code = %s, want %s", got, contracts.ErrInternal)
	}
	for _, frag := range []string{"checksum mismatch", "shop/0001_init.sql", "refusing to start"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error %q does not mention %q", err, frag)
		}
	}
	var ce *contracts.Error
	if !asError(err, &ce) || ce.Details["module"] != "shop" || ce.Details["version"] != 1 {
		t.Errorf("error details do not name module and version: %+v", ce)
	}

	// Nothing was applied, including the unrelated new module that registered
	// before the drifted one: the check runs before any migration.
	chk, err := Open(context.Background(), Options{Path: opts.Path, Migrations: reg})
	if err != nil {
		t.Fatalf("original registry no longer opens the database: %v", err)
	}
	defer chk.Close()
	if got := appliedOrder(t, chk); !reflect.DeepEqual(got, before) {
		t.Errorf("schema_migrations changed by the refused start: %v (was %v)", got, before)
	}
	for _, name := range []string{"should_not_exist", "audit_should_not_exist"} {
		if tableExists(t, chk, name) {
			t.Errorf("table %s was created by a start that should have been refused", name)
		}
	}

}

func TestChecksumIgnoresLineEndings(t *testing.T) {
	lf := "CREATE TABLE a (\n  id INTEGER PRIMARY KEY\n);\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	if checksum([]byte(lf)) != checksum([]byte(crlf)) {
		t.Error("CRLF and LF copies of one migration must have the same checksum")
	}
	if checksum([]byte(lf)) == checksum([]byte(lf+"-- edit\n")) {
		t.Error("an edit must change the checksum")
	}
}

func TestMigrationDowngradeRefused(t *testing.T) {
	newer := NewRegistry()
	mustRegister(t, newer, "shop", map[string]string{
		"0001_init.sql": `CREATE TABLE widgets (id INTEGER PRIMARY KEY);`,
		"0002_next.sql": `CREATE TABLE gadgets (id INTEGER PRIMARY KEY);`,
	})
	path := filepath.Join(t.TempDir(), "d.db")
	db, err := Open(context.Background(), Options{Path: path, Migrations: newer})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	older := NewRegistry()
	mustRegister(t, older, "shop", map[string]string{"0001_init.sql": `CREATE TABLE widgets (id INTEGER PRIMARY KEY);`})
	if db, err := Open(context.Background(), Options{Path: path, Migrations: older}); err == nil {
		db.Close()
		t.Fatal("an older binary opened a database migrated by a newer one")
	} else if got := contracts.CodeOf(err); got != contracts.ErrInternal || !strings.Contains(err.Error(), "shop version 0002") {
		t.Errorf("downgrade error = %s: %v", got, err)
	}

	// A binary that does not register the module at all leaves its tables alone.
	none := NewRegistry()
	mustRegister(t, none, "other", map[string]string{"0001_init.sql": `CREATE TABLE other_t (id INTEGER PRIMARY KEY);`})
	db, err = Open(context.Background(), Options{Path: path, Migrations: none})
	if err != nil {
		t.Fatalf("unregistered module blocked startup: %v", err)
	}
	db.Close()
}

func TestMigrationFailureRollsBackThatMigrationOnly(t *testing.T) {
	reg := NewRegistry()
	mustRegister(t, reg, "shop", map[string]string{
		"0001_init.sql": `CREATE TABLE widgets (id INTEGER PRIMARY KEY);`,
		// The first statement succeeds, the second fails: both must vanish.
		"0002_broken.sql": `CREATE TABLE half_done (id INTEGER PRIMARY KEY); INSERT INTO no_such_table VALUES (1);`,
		"0003_later.sql":  `CREATE TABLE never_reached (id INTEGER PRIMARY KEY);`,
	})
	path := filepath.Join(t.TempDir(), "f.db")
	if db, err := Open(context.Background(), Options{Path: path, Migrations: reg}); err == nil {
		db.Close()
		t.Fatal("Open succeeded with a failing migration")
	} else {
		if got := contracts.CodeOf(err); got != contracts.ErrInternal {
			t.Errorf("code = %s, want internal", got)
		}
		if !strings.Contains(err.Error(), "shop/0002_broken.sql") {
			t.Errorf("error does not name the failing migration: %v", err)
		}
	}

	fixed := NewRegistry()
	mustRegister(t, fixed, "shop", map[string]string{"0001_init.sql": `CREATE TABLE widgets (id INTEGER PRIMARY KEY);`})
	db, err := Open(context.Background(), Options{Path: path, Migrations: fixed})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := appliedOrder(t, db); len(got) != 1 || got[0].Version != 1 {
		t.Errorf("recorded migrations = %v, want only 0001", got)
	}
	for _, name := range []string{"half_done", "never_reached"} {
		if tableExists(t, db, name) {
			t.Errorf("table %s exists: the failed migration was not atomic", name)
		}
	}
}

func TestRegisterValidation(t *testing.T) {
	one := map[string]string{"0001_init.sql": `SELECT 1;`}
	cases := []struct {
		name   string
		module string
		fsys   fs.FS
	}{
		{"empty module name", "", migFS(one)},
		{"uppercase module", "Shop", migFS(one)},
		{"module with dash", "my-mod", migFS(one)},
		{"nil fs", "shop", nil},
		{"no migrations dir", "shop", fstest.MapFS{"other/0001_init.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}}},
		{"empty dir", "shop", fstest.MapFS{"migrations/.keep": &fstest.MapFile{Data: nil}}},
		{"bad file name", "shop", migFS(map[string]string{"1_init.sql": "SELECT 1;"})},
		{"upper case name", "shop", migFS(map[string]string{"0001_Init.sql": "SELECT 1;"})},
		{"not sql", "shop", migFS(map[string]string{"0001_init.txt": "SELECT 1;"})},
		{"subdirectory", "shop", fstest.MapFS{"migrations/sub/0001_init.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}}},
		{"gap", "shop", migFS(map[string]string{"0001_a.sql": "SELECT 1;", "0003_c.sql": "SELECT 1;"})},
		{"starts at 2", "shop", migFS(map[string]string{"0002_a.sql": "SELECT 1;"})},
		{"version zero", "shop", migFS(map[string]string{"0000_a.sql": "SELECT 1;"})},
		{"duplicate version", "shop", migFS(map[string]string{"0001_a.sql": "SELECT 1;", "0001_b.sql": "SELECT 2;"})},
		{"empty file", "shop", migFS(map[string]string{"0001_a.sql": "  \n"})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := NewRegistry().Register(c.module, c.fsys); err == nil {
				t.Error("Register accepted it")
			}
		})
	}

	reg := NewRegistry()
	mustRegister(t, reg, "shop", one)
	if err := reg.Register("shop", migFS(one)); err == nil {
		t.Error("registering a module twice was accepted")
	}
}

func TestRegisterAcceptsEmbedFS(t *testing.T) {
	// The brief's signature takes an embed.FS; fs.FS accepts it unchanged. The
	// embedded files live under testdata/embedmod, so root the FS there.
	sub, err := fs.Sub(embeddedMigrations, "testdata/embedmod")
	if err != nil {
		t.Fatal(err)
	}
	saved := Default
	Default = NewRegistry()
	t.Cleanup(func() { Default = saved })

	RegisterMigrations("embedmod", sub)
	if !Default.has("embedmod") {
		t.Fatal("RegisterMigrations did not add to Default")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("registering the same module twice must panic like config.Register")
			}
		}()
		RegisterMigrations("embedmod", sub)
	}()

	// Open with no explicit registry applies Default.
	db, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "e.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if !tableExists(t, db, "embedded_items") {
		t.Error("embedded migration was not applied through Default")
	}
}

func asError(err error, target **contracts.Error) bool {
	for err != nil {
		if e, ok := err.(*contracts.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
