package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// openRaw opens a file with a plain read-only pool, outside Open, so a test
// can inspect a backup without migrating it.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	p, err := openPool(DefaultConfig().dsn(path, roleReader), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func TestBackupVacuumInto(t *testing.T) {
	db := openTest(t, nil)
	ctx := context.Background()
	sc := testScope("app", "tenantA", "w1")
	for i := 0; i < 100; i++ {
		insertNote(t, db, sc, fmt.Sprintf("n%03d", i), fmt.Sprintf("title %d", i), "espresso body "+fmt.Sprint(i))
	}
	dest := filepath.Join(t.TempDir(), "nested", "backup.db")

	if err := db.Backup(ctx, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// Writes after the backup must not appear in it.
	insertNote(t, db, sc, "late", "late", "late arrival")
	if n := count(t, db, `SELECT count(*) FROM notes`); n != 101 {
		t.Fatalf("live rows = %d, want 101", n)
	}

	// The copy is one self-contained file: no partial, no WAL needed.
	if _, err := os.Stat(dest + ".partial"); !os.IsNotExist(err) {
		t.Errorf("partial file left behind: %v", err)
	}
	if err := IntegrityCheckFile(ctx, dest); err != nil {
		t.Fatalf("backup fails its integrity check: %v", err)
	}
	raw := openRaw(t, dest)
	var rows, hits, lateRows, migrations int
	for q, dst := range map[string]*int{
		`SELECT count(*) FROM notes`:                                      &rows,
		`SELECT count(*) FROM notes_fts WHERE notes_fts MATCH 'espresso'`: &hits,
		`SELECT count(*) FROM notes WHERE id = 'late'`:                    &lateRows,
		`SELECT count(*) FROM schema_migrations`:                          &migrations,
	} {
		if err := raw.QueryRowContext(ctx, q).Scan(dst); err != nil {
			t.Fatalf("%s on the backup: %v", q, err)
		}
	}
	if rows != 100 || hits != 100 {
		t.Errorf("backup has %d notes and %d FTS hits, want 100 and 100", rows, hits)
	}
	if lateRows != 0 {
		t.Error("a write made after the backup appears in it")
	}
	if migrations != 1 {
		t.Errorf("backup has %d schema_migrations rows, want 1", migrations)
	}

	// The copy restores: Open applies nothing and finds the data.
	raw.Close()
	restored, err := Open(ctx, Options{Path: dest, Migrations: notesRegistry(t)})
	if err != nil {
		t.Fatalf("opening the backup as a store: %v", err)
	}
	if n := count(t, restored, `SELECT count(*) FROM notes`); n != 100 {
		t.Errorf("restored rows = %d, want 100", n)
	}
	restored.Close()

	if runtime.GOOS != "windows" {
		if st, err := os.Stat(dest); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("backup mode = %v (%v), want 0600", st.Mode().Perm(), err)
		}
	}

	t.Run("never overwrites", func(t *testing.T) {
		before, err := os.ReadFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		err = db.Backup(ctx, dest)
		if got := contracts.CodeOf(err); got != contracts.ErrConflict {
			t.Errorf("code = %s, want %s (%v)", got, contracts.ErrConflict, err)
		}
		after, _ := os.ReadFile(dest)
		if !bytes.Equal(before, after) {
			t.Error("an existing backup was modified")
		}
	})

	t.Run("refuses the live file", func(t *testing.T) {
		if err := db.Backup(ctx, db.Path()); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("backup onto the live database: %v", err)
		}
		if err := db.IntegrityCheck(ctx); err != nil {
			t.Errorf("live database damaged: %v", err)
		}
	})

	t.Run("cancelled context leaves nothing", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		d := filepath.Join(t.TempDir(), "cancelled.db")
		if err := db.Backup(cctx, d); err == nil {
			t.Error("Backup succeeded with a cancelled context")
		}
		for _, f := range []string{d, d + ".partial"} {
			if _, err := os.Stat(f); !os.IsNotExist(err) {
				t.Errorf("%s left behind after a failed backup", f)
			}
		}
	})

	t.Run("consistent while writers run", func(t *testing.T) {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		var inserted int
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if err := db.Tx(ctx, func(tx *Tx) error {
					_, err := InsertScoped(ctx, tx, sc, "notes", []string{"id", "title", "body"},
						fmt.Sprintf("w%05d", i), "writer", "concurrent espresso")
					return err
				}); err != nil {
					t.Errorf("writer: %v", err)
					return
				}
				inserted = i + 1
			}
		}()
		time.Sleep(50 * time.Millisecond)
		d := filepath.Join(t.TempDir(), "live.db")
		err := db.Backup(ctx, d)
		close(stop)
		wg.Wait()
		if err != nil {
			t.Fatalf("Backup during writes: %v", err)
		}
		if err := IntegrityCheckFile(ctx, d); err != nil {
			t.Errorf("backup taken during writes is inconsistent: %v", err)
		}
		r := openRaw(t, d)
		var n, fts int
		if err := r.QueryRowContext(ctx, `SELECT count(*) FROM notes`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if err := r.QueryRowContext(ctx, `SELECT count(*) FROM notes_fts WHERE notes_fts MATCH 'espresso'`).Scan(&fts); err != nil {
			t.Fatal(err)
		}
		if n < 101 || n > 101+inserted || fts != n-101+100 {
			t.Errorf("backup has %d notes / %d FTS hits with %d concurrent inserts: not a point-in-time copy", n, fts, inserted)
		}
	})
}

func TestIntegrityCheck(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ic.db")
	reg := notesRegistry(t)
	db, err := Open(ctx, Options{Path: path, Migrations: reg})
	if err != nil {
		t.Fatal(err)
	}
	sc := testScope("app", "tenantA", "w1")
	for i := 0; i < 300; i++ {
		insertNote(t, db, sc, fmt.Sprintf("n%04d", i), fmt.Sprintf("title %d", i), "some body text for the index")
	}

	// A healthy database passes, live and as a file.
	if err := db.IntegrityCheck(ctx); err != nil {
		t.Fatalf("healthy database failed: %v", err)
	}
	if err := IntegrityCheckFile(ctx, path); err != nil {
		t.Fatalf("healthy file failed: %v", err)
	}

	// Foreign key violations are findings too. The store's writer enforces
	// foreign keys, so plant the orphan through a plain connection, as a
	// restored or hand-edited file might have one.
	if _, err := db.ExecContext(ctx, `CREATE TABLE fk_parent (id INTEGER PRIMARY KEY); CREATE TABLE fk_child (id INTEGER PRIMARY KEY, p INTEGER REFERENCES fk_parent(id));`); err != nil {
		t.Fatal(err)
	}
	lax, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lax.ExecContext(ctx, `INSERT INTO fk_child (p) VALUES (99)`); err != nil {
		t.Fatalf("planting an orphan row: %v", err)
	}
	err = db.IntegrityCheck(ctx)
	if got := contracts.CodeOf(err); got != contracts.ErrInternal || !strings.Contains(fmt.Sprint(err), "foreign key violation: fk_child") {
		t.Errorf("orphaned foreign key not reported: %v", err)
	}
	if _, err := lax.ExecContext(ctx, `DELETE FROM fk_child`); err != nil {
		t.Fatal(err)
	}
	lax.Close()
	if err := db.IntegrityCheck(ctx); err != nil {
		t.Errorf("database still failing after the orphan was removed: %v", err)
	}

	// Now corrupt the file: fold the WAL in, close everything, and break the
	// page type of the notes_scope index root page. Page 1 is untouched, so the
	// file still opens; only the check can tell.
	var rootPage, pageSize int
	if err := db.QueryRowContext(ctx, `SELECT rootpage FROM sqlite_master WHERE name = 'notes_scope'`).Scan(&rootPage); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if rootPage < 2 {
		t.Fatalf("index root page %d: nothing to corrupt beyond page 1", rootPage)
	}
	if _, err := db.w.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("%s still present after Close; the corruption below would be masked by the WAL", f)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xFF}, int64(rootPage-1)*int64(pageSize)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	err = IntegrityCheckFile(ctx, path)
	if err == nil {
		t.Fatal("IntegrityCheckFile passed a file with a corrupted index page")
	}
	if got := contracts.CodeOf(err); got != contracts.ErrInternal {
		t.Errorf("code = %s, want %s", got, contracts.ErrInternal)
	}
	var ce *contracts.Error
	if !asError(err, &ce) || ce.Details["problems"] == nil {
		t.Errorf("error does not carry the findings: %+v", err)
	}
	t.Logf("corruption reported as: %v", err)

	// And a backup of a database that fails its check is never published.
	// (Open on the corrupted file may itself fail; either way nothing is
	// reported healthy.)
	if db2, err := Open(ctx, Options{Path: path, Migrations: reg}); err == nil {
		defer db2.Close()
		if err := db2.IntegrityCheck(ctx); err == nil {
			t.Error("IntegrityCheck on the reopened corrupted database passed")
		}
		dest := filepath.Join(t.TempDir(), "bad.db")
		if err := db2.Backup(ctx, dest); err == nil {
			t.Error("Backup published a copy of a corrupted database")
		} else if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
			t.Errorf("a failed backup left %s behind", dest)
		}
	}
}

// ViewFile reads a copy no store has open without changing it, and leaves
// nothing beside it; a missing file is not created.
func TestViewFileIsReadOnly(t *testing.T) {
	db := openTest(t, nil)
	ctx := context.Background()
	insertNote(t, db, testScope("app", "tenantA", "w1"), "n1", "title", "body")
	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := db.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	err = ViewFile(ctx, dest, func(q Reader) error {
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM notes`).Scan(&n); err != nil {
			return err
		}
		if _, err := q.QueryContext(ctx, `DELETE FROM notes`); err == nil {
			t.Error("a write went through")
		}
		return nil
	})
	if err != nil || n != 1 {
		t.Fatalf("ViewFile: %d notes, %v", n, err)
	}
	if after, _ := os.ReadFile(dest); !bytes.Equal(before, after) {
		t.Error("the file changed")
	}
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 1 {
		t.Errorf("the directory holds %v, want the database alone", entries)
	}
	absent := filepath.Join(t.TempDir(), "absent.db")
	if err := ViewFile(ctx, absent, func(Reader) error { return nil }); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("missing file: %v", err)
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Errorf("a missing file was created (%v)", err)
	}
}

func TestIntegrityCheckFileMissing(t *testing.T) {
	err := IntegrityCheckFile(context.Background(), filepath.Join(t.TempDir(), "absent.db"))
	if got := contracts.CodeOf(err); got != contracts.ErrUnavailable {
		t.Errorf("missing file: code = %s, want %s (%v)", got, contracts.ErrUnavailable, err)
	}
}
