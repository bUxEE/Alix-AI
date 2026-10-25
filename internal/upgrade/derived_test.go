package upgrade

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/backup"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The marker is stored in upper case in the content table; the FTS index holds
// it lower-cased, so the lower-case bytes occur exactly once in the file: in
// the index. Damaging them damages the derived index and nothing else.
const (
	markerContent = "QQQUICKBROWNFOXMARKERZ"
	markerIndexed = "brownfoxmarkerz"
)

// notesEnv is a database with the notes module: a content table and the FTS
// index derived from it.
type notesEnv struct {
	t    *testing.T
	path string
	dir  string
	clk  *clock.Fake
	db   *store.DB
}

func newNotesEnv(t *testing.T) *notesEnv {
	t.Helper()
	dir := t.TempDir()
	e := &notesEnv{t: t, dir: dir, path: filepath.Join(dir, store.DefaultFileName), clk: clock.NewFake(t0)}
	e.open()
	for i := 1; i <= 30; i++ {
		e.exec(`INSERT INTO notes (id, application_id, tenant_id, workspace_id, body) VALUES (?, 'a', 't', 'w', ?)`,
			fmt.Sprintf("n%02d", i), fmt.Sprintf("ALPHA BRAVO CHARLIE ENTRY%02d", i))
	}
	e.exec(`INSERT INTO notes (id, application_id, tenant_id, workspace_id, body) VALUES ('n99', 'a', 't', 'w', ?)`, markerContent+" ZEBRA")
	t.Cleanup(func() {
		if e.db != nil {
			e.db.Close()
		}
	})
	return e
}

func (e *notesEnv) open() {
	e.t.Helper()
	reg := registry(e.t, map[string]map[string]string{"notes": {"0001_init.sql": notesMigration}}, "notes")
	db, err := store.Open(ctx, store.Options{Path: e.path, Migrations: reg, Clock: e.clk})
	if err != nil {
		e.t.Fatalf("store.Open: %v", err)
	}
	e.db = db
}

func (e *notesEnv) close() {
	e.t.Helper()
	if err := e.db.Close(); err != nil {
		e.t.Fatal(err)
	}
	e.db = nil
}

func (e *notesEnv) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.db.ExecContext(ctx, q, args...); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
}

func (e *notesEnv) search(term string) []string {
	e.t.Helper()
	rows, err := e.db.QueryContext(ctx, `SELECT n.id FROM notes_fts f JOIN notes n ON n.rowid = f.rowid WHERE notes_fts MATCH ? ORDER BY n.id`, `"`+term+`"`)
	if err != nil {
		e.t.Fatalf("search %s: %v", term, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatalf("search %s: %v", term, err)
	}
	return out
}

// dump is the content table, the source the index is rebuilt from.
func (e *notesEnv) dump() string {
	e.t.Helper()
	rows, err := e.db.QueryContext(ctx, `SELECT id, body FROM notes ORDER BY id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			e.t.Fatal(err)
		}
		b.WriteString(id + "=" + body + "\n")
	}
	return b.String()
}

func (e *notesEnv) indexes() []DerivedIndex {
	e.t.Helper()
	idx, err := FTSIndexes(ctx, e.db)
	if err != nil {
		e.t.Fatal(err)
	}
	return idx
}

// corrupt closes the database and damages the bytes of the marker term in the
// FTS index, which is what a bad sector or a stray write in a derived structure
// looks like: the b-tree stays valid, the index no longer matches its source.
func (e *notesEnv) corrupt() {
	e.t.Helper()
	e.close()
	for _, f := range []string{e.path + "-wal", e.path + "-shm"} {
		if _, err := os.Stat(f); err == nil {
			e.t.Fatalf("%s is still there after Close: the damage below would sit in the WAL", f)
		}
	}
	b, err := os.ReadFile(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := damageIndexBytes(b); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.path, b, 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.open()
}

// damageIndexBytes flips one letter of every copy of the indexed marker (a term
// lives in each segment that holds its row, and in stale pages). The content
// table has it upper-cased, so only the derived index is hit.
func damageIndexBytes(b []byte) error {
	n := bytes.Count(b, []byte(markerIndexed))
	if n == 0 {
		return errors.New("the lower-case marker is not in the file: nothing to damage")
	}
	for off := 0; ; {
		i := bytes.Index(b[off:], []byte(markerIndexed))
		if i < 0 {
			return nil
		}
		b[off+i+len(markerIndexed)-1] ^= 0x01
		off += i + len(markerIndexed)
	}
}

func TestFTSIndexesAreDiscovered(t *testing.T) {
	e := newNotesEnv(t)
	idx := e.indexes()
	if len(idx) != 1 || idx[0].Name() != "notes_fts" {
		t.Fatalf("indexes = %v, want notes_fts", names(idx))
	}
	if err := idx[0].Check(ctx); err != nil {
		t.Fatalf("a healthy index: %v", err)
	}
	// A contentless index has no source to rebuild from and is not listed.
	e.exec(`CREATE VIRTUAL TABLE bare_fts USING fts5 (x, content = '')`)
	if idx := e.indexes(); len(idx) != 1 {
		t.Errorf("indexes = %v, want only notes_fts", names(idx))
	}
}

func names(idx []DerivedIndex) []string {
	var out []string
	for _, ix := range idx {
		out = append(out, ix.Name())
	}
	return out
}

// SEC-074: a corrupt derived index is isolated: recorded, emptied, and not
// served from; the content it is derived from is untouched.
func TestCorruptIndexIsolated(t *testing.T) {
	e := newNotesEnv(t)
	if got := e.search(strings.ToLower(markerContent)); len(got) != 1 || got[0] != "n99" {
		t.Fatalf("before the damage the marker finds %v", got)
	}
	wantDump := e.dump()
	e.corrupt()

	idx := e.indexes()
	err := idx[0].Check(ctx)
	if !errors.Is(err, ErrDamaged) {
		t.Fatalf("Check on a damaged index = %v, want ErrDamaged", err)
	}
	t.Logf("detected: %v", err)

	q := NewQuarantine(e.dir, e.clk)
	damaged, err := IsolateDamaged(ctx, q, idx)
	if err != nil || len(damaged) != 1 || damaged[0] != "notes_fts" {
		t.Fatalf("IsolateDamaged = %v, %v", damaged, err)
	}
	// Recorded where a restart finds it.
	q2 := NewQuarantine(e.dir, e.clk)
	if !q2.Has("notes_fts") {
		t.Fatal("the quarantine record did not survive")
	}
	if entries, err := q2.List(); err != nil || len(entries) != 1 || entries[0].Reason == "" || entries[0].Since == "" {
		t.Errorf("entries = %+v, %v", entries, err)
	}
	// Nothing is served from the damaged bytes.
	if got := e.search("alpha"); len(got) != 0 {
		t.Errorf("an isolated index still serves %d rows", len(got))
	}
	if got := e.search(strings.ToLower(markerContent)); len(got) != 0 {
		t.Errorf("an isolated index still serves the damaged term: %v", got)
	}
	// The source is untouched.
	if got := e.dump(); got != wantDump {
		t.Errorf("the content table changed while its index was isolated")
	}
	// A healthy index is not touched at all.
	healthy := newNotesEnv(t)
	hq := NewQuarantine(healthy.dir, healthy.clk)
	if d, err := IsolateDamaged(ctx, hq, healthy.indexes()); err != nil || len(d) != 0 {
		t.Errorf("IsolateDamaged on a healthy index = %v, %v", d, err)
	}
	if got := healthy.search("alpha"); len(got) != 30 {
		t.Errorf("a healthy index serves %d rows, want 30", len(got))
	}
	// A name that cannot be recorded is refused rather than isolated unrecorded.
	if err := q.Add("bad name!", "x"); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("Add of a bad name: %v", err)
	}
}

// SEC-074: an isolated index is rebuilt from its source and released, and a
// crash between the two steps is finished from the quarantine.
func TestDerivedIndexRebuild(t *testing.T) {
	e := newNotesEnv(t)
	wantAlpha := e.search("alpha")
	wantMarker := e.search(strings.ToLower(markerContent))
	wantEntry := e.search("entry07")
	wantDump := e.dump()
	if len(wantAlpha) != 30 || len(wantMarker) != 1 || len(wantEntry) != 1 {
		t.Fatalf("setup: %d alpha, %d marker, %d entry07", len(wantAlpha), len(wantMarker), len(wantEntry))
	}
	e.corrupt()

	if _, err := IsolateDamaged(ctx, NewQuarantine(e.dir, e.clk), e.indexes()); err != nil {
		t.Fatal(err)
	}
	// "Crash": a new process, a new Quarantine, the same directory.
	q := NewQuarantine(e.dir, e.clk)
	rebuilt, err := RebuildQuarantined(ctx, q, e.indexes())
	if err != nil || len(rebuilt) != 1 || rebuilt[0] != "notes_fts" {
		t.Fatalf("RebuildQuarantined = %v, %v", rebuilt, err)
	}
	if entries, _ := q.List(); len(entries) != 0 {
		t.Errorf("the quarantine still holds %+v after the rebuild", entries)
	}
	for term, want := range map[string][]string{"alpha": wantAlpha, strings.ToLower(markerContent): wantMarker, "entry07": wantEntry} {
		if got := e.search(term); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("search %q after the rebuild = %v, want %v", term, got, want)
		}
	}
	if got := e.dump(); got != wantDump {
		t.Error("the content table changed")
	}
	if err := e.indexes()[0].Check(ctx); err != nil {
		t.Errorf("the rebuilt index fails its check: %v", err)
	}
	if err := e.db.IntegrityCheck(ctx); err != nil {
		t.Errorf("integrity after the rebuild: %v", err)
	}

	t.Run("RepairDerived does both and is a no-op on a healthy index", func(t *testing.T) {
		e := newNotesEnv(t)
		q := NewQuarantine(e.dir, e.clk)
		if rep, err := RepairDerived(ctx, q, e.indexes()); err != nil || len(rep.Damaged)+len(rep.Rebuilt) != 0 {
			t.Fatalf("on a healthy index: %+v, %v", rep, err)
		}
		e.corrupt()
		rep, err := RepairDerived(ctx, q, e.indexes())
		if err != nil || len(rep.Damaged) != 1 || len(rep.Rebuilt) != 1 {
			t.Fatalf("on a damaged index: %+v, %v", rep, err)
		}
		if got := e.search(strings.ToLower(markerContent)); len(got) != 1 {
			t.Errorf("marker after RepairDerived = %v", got)
		}
	})

	t.Run("a record for an index that no longer exists is cleared", func(t *testing.T) {
		e := newNotesEnv(t)
		q := NewQuarantine(e.dir, e.clk)
		if err := q.Add("dropped_fts", "gone"); err != nil {
			t.Fatal(err)
		}
		if _, err := RebuildQuarantined(ctx, q, e.indexes()); err != nil {
			t.Fatal(err)
		}
		if q.Has("dropped_fts") {
			t.Error("a stale record blocks forever")
		}
	})
}

// SEC-074, plan 18: damage a rebuild cannot reach (the index's own b-tree) is
// reported as structural and left in quarantine, and the way out is the last
// backup: the upgrade refuses to start over it, and a restore brings it back.
func TestStructuralDamageFallsBackToBackup(t *testing.T) {
	f := newFixture(t)
	db := f.openWith(regV1(t))
	if _, err := db.ExecContext(ctx, `INSERT INTO notes (id, application_id, tenant_id, workspace_id, body) VALUES ('n1', 'a', 't', 'w', 'ALPHA BRAVO')`); err != nil {
		t.Fatal(err)
	}
	bk, err := backup.Create(ctx, db, backup.Options{Dir: filepath.Join(t.TempDir(), "bk"), DataDir: f.dataDir, Signer: f.bkSigner, Clock: f.clk})
	if err != nil {
		t.Fatal(err)
	}
	var root, pageSize int
	if err := db.QueryRowContext(ctx, `SELECT rootpage FROM sqlite_master WHERE name = 'notes_fts_data'`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.dataDir, store.DefaultFileName)
	if _, err := os.Stat(path + "-wal"); err == nil {
		t.Fatal("WAL still present")
	}
	fh, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteAt([]byte{0xFF}, int64(root-1)*int64(pageSize)); err != nil {
		t.Fatal(err)
	}
	fh.Close()

	u := f.upgrader(regV2(t, ""))
	res, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet})
	if !errors.Is(err, ErrRefused) || res.Outcome != OutcomeRefused {
		t.Fatalf("Apply over structural damage = %+v, %v", res, err)
	}
	t.Logf("refusal: %v", err)
	if !u.Quarantine().Has("notes_fts") {
		t.Error("the damaged index is not in the quarantine")
	}

	// The last backup is the other branch of the plan.
	if _, err := backup.Restore(ctx, backup.RestoreOptions{BackupDir: bk.Dir, DataDir: f.dataDir, Keys: f.bkKeys(), Clock: f.clk}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := u.Quarantine().Clear("notes_fts"); err != nil {
		t.Fatal(err)
	}
	res, err = u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet})
	if err != nil || res.Outcome != OutcomeApplied {
		t.Fatalf("Apply after the restore = %+v, %v", res, err)
	}
}

// SEC-074: the probe of a staged upgrade rebuilds a damaged derived index and
// the upgrade goes on; receipts and audit are not touched.
func TestUpgradeRebuildsDamagedIndex(t *testing.T) {
	f := newFixture(t)
	events, head := f.auditBefore(t)
	db := f.openWith(regV1(t))
	if _, err := db.ExecContext(ctx, `INSERT INTO notes (id, application_id, tenant_id, workspace_id, body) VALUES ('n1', 'a', 't', 'w', ?)`, markerContent); err != nil {
		t.Fatal(err)
	}
	db.Close()
	path := filepath.Join(f.dataDir, store.DefaultFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := damageIndexBytes(b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	u := f.upgrader(regV2(t, ""))
	res, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet})
	if err != nil || res.Outcome != OutcomeApplied {
		t.Fatalf("Apply = %+v, %v", res, err)
	}
	if len(res.Derived.Rebuilt) != 1 || res.Derived.Rebuilt[0] != "notes_fts" {
		t.Errorf("derived = %+v, want notes_fts rebuilt", res.Derived)
	}
	db = f.openWith(regV2(t, ""))
	defer db.Close()
	if e, h := auditHead(t, db); e != events || h != head {
		t.Errorf("audit chain = %d events head %.12s, want %d events head %.12s", e, h, events, head)
	}
	if n := count(t, db, `SELECT count(*) FROM notes_fts WHERE notes_fts MATCH '"qqquickbrownfoxmarkerz"'`); n != 1 {
		t.Errorf("the rebuilt index finds the marker %d times, want 1", n)
	}
	if err := db.IntegrityCheck(ctx); err != nil {
		t.Errorf("integrity: %v", err)
	}
}
