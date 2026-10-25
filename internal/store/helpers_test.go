package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// notesMigration is a realistic module: a scoped table, a scope index, and an
// external-content FTS5 table kept in step by triggers (the pattern the RAG
// module will use).
const notesMigration = `
CREATE TABLE notes (
    id             TEXT PRIMARY KEY,
    application_id TEXT NOT NULL,
    tenant_id      TEXT NOT NULL,
    workspace_id   TEXT NOT NULL,
    title          TEXT NOT NULL,
    body           TEXT NOT NULL
);
CREATE INDEX notes_scope ON notes (application_id, tenant_id, workspace_id, id);

CREATE VIRTUAL TABLE notes_fts USING fts5 (
    title, body,
    application_id UNINDEXED, tenant_id UNINDEXED, workspace_id UNINDEXED,
    content = 'notes', content_rowid = 'rowid',
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER notes_ai AFTER INSERT ON notes BEGIN
    INSERT INTO notes_fts (rowid, title, body, application_id, tenant_id, workspace_id)
    VALUES (new.rowid, new.title, new.body, new.application_id, new.tenant_id, new.workspace_id);
END;
CREATE TRIGGER notes_ad AFTER DELETE ON notes BEGIN
    INSERT INTO notes_fts (notes_fts, rowid, title, body, application_id, tenant_id, workspace_id)
    VALUES ('delete', old.rowid, old.title, old.body, old.application_id, old.tenant_id, old.workspace_id);
END;
CREATE TRIGGER notes_au AFTER UPDATE ON notes BEGIN
    INSERT INTO notes_fts (notes_fts, rowid, title, body, application_id, tenant_id, workspace_id)
    VALUES ('delete', old.rowid, old.title, old.body, old.application_id, old.tenant_id, old.workspace_id);
    INSERT INTO notes_fts (rowid, title, body, application_id, tenant_id, workspace_id)
    VALUES (new.rowid, new.title, new.body, new.application_id, new.tenant_id, new.workspace_id);
END;
`

// migFS builds an fs.FS holding migrations/<name> files.
func migFS(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for name, body := range files {
		m[MigrationsDir+"/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}

// mustRegister adds module to reg or fails the test.
func mustRegister(t *testing.T, reg *Registry, module string, files map[string]string) {
	t.Helper()
	if err := reg.Register(module, migFS(files)); err != nil {
		t.Fatalf("Register(%q): %v", module, err)
	}
}

// notesRegistry returns a registry holding just the notes module.
func notesRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	mustRegister(t, reg, "notes", map[string]string{"0001_init.sql": notesMigration})
	return reg
}

// openTest opens a database in t.TempDir() and closes it when the test ends.
// The TempDir is created before the Close cleanup is registered, so on
// Windows the file is released before the directory is removed.
func openTest(t *testing.T, reg *Registry, mutate ...func(*Options)) *DB {
	t.Helper()
	if reg == nil {
		reg = notesRegistry(t)
	}
	opts := Options{
		Path:       filepath.Join(t.TempDir(), "alixai.db"),
		Migrations: reg,
		Clock:      clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)),
	}
	for _, m := range mutate {
		m(&opts)
	}
	db, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

func testScope(app, tenant, workspace string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep_test",
		ApplicationID:    app,
		TenantID:         tenant,
		WorkspaceID:      workspace,
		PrincipalID:      "usr_test",
		Roles:            []string{"member"},
		CapabilityScopes: []string{"notes.read"},
		SessionID:        "ses_test",
	}
}

// tickClock advances one second every time it is read, so applied_at values
// record the order in which migrations ran.
type tickClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTickClock() *tickClock { return &tickClock{t: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)} }

func (c *tickClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

// insertNote adds a note through the scoped helper.
func insertNote(t *testing.T, db *DB, sc contracts.Scope, id, title, body string) {
	t.Helper()
	if _, err := InsertScoped(context.Background(), db, sc, "notes", []string{"id", "title", "body"}, id, title, body); err != nil {
		t.Fatalf("InsertScoped(%s): %v", id, err)
	}
}

func count(t *testing.T, q Reader, query string, args ...any) int {
	t.Helper()
	var n int
	if err := q.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
