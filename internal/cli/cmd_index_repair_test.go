package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/upgrade"
)

// The marker is stored in upper case in the content table; the FTS index holds it lower-cased, so
// its lower-case bytes occur only in the index. Damaging them damages the derived index and nothing
// else (the device internal/upgrade's tests use).
const (
	opsInspectMarkerContent = "QQQUICKBROWNFOXMARKERZ"
	opsInspectMarkerIndexed = "brownfoxmarkerz"
)

// opsInspectNotesMigration is an external-content FTS5 index kept in step by triggers, the pattern
// internal/rag and internal/history use.
const opsInspectNotesMigration = `
CREATE TABLE notes (
    id             TEXT PRIMARY KEY,
    application_id TEXT NOT NULL,
    tenant_id      TEXT NOT NULL,
    workspace_id   TEXT NOT NULL,
    body           TEXT NOT NULL
);

CREATE VIRTUAL TABLE notes_fts USING fts5 (
    body,
    application_id UNINDEXED, tenant_id UNINDEXED, workspace_id UNINDEXED,
    content = 'notes', content_rowid = 'rowid',
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER notes_ai AFTER INSERT ON notes BEGIN
    INSERT INTO notes_fts (rowid, body, application_id, tenant_id, workspace_id)
    VALUES (new.rowid, new.body, new.application_id, new.tenant_id, new.workspace_id);
END;
`

// opsInspectNotesDB creates <dir>/alixai.db with the notes module, 30 notes and the marker note, and
// closes it.
func opsInspectNotesDB(t *testing.T, dir string) string {
	t.Helper()
	reg := store.NewRegistry()
	err := reg.Register("notes", fstest.MapFS{store.MigrationsDir + "/0001_init.sql": &fstest.MapFile{Data: []byte(opsInspectNotesMigration)}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, store.DefaultFileName)
	db, err := store.Open(context.Background(), store.Options{Path: path, Migrations: reg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for i := 1; i <= 30; i++ {
		if _, err := db.ExecContext(context.Background(), `INSERT INTO notes (id, application_id, tenant_id, workspace_id, body) VALUES (?, 'a', 't', 'w', ?)`,
			fmt.Sprintf("n%02d", i), fmt.Sprintf("ALPHA BRAVO CHARLIE ENTRY%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO notes (id, application_id, tenant_id, workspace_id, body) VALUES ('n99', 'a', 't', 'w', ?)`,
		opsInspectMarkerContent+" ZEBRA"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// opsInspectDamageIndex flips a letter of every copy of the indexed marker in the closed database file.
func opsInspectDamageIndex(t *testing.T, path string) {
	t.Helper()
	for _, f := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("%s is still there: the damage would sit behind the WAL", f)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	needle := []byte(opsInspectMarkerIndexed)
	if bytes.Count(b, needle) == 0 {
		t.Fatal("the lower-case marker is not in the file: there is nothing to damage")
	}
	for off := 0; ; {
		i := bytes.Index(b[off:], needle)
		if i < 0 {
			break
		}
		b[off+i+len(needle)-1] ^= 0x01
		off += i + len(needle)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// opsInspectNotesState checks the index and asks it for the marker, on a fresh handle.
func opsInspectNotesState(t *testing.T, path string) (damaged bool, hits int) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: path, Migrations: store.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	idx, err := upgrade.FTSIndexes(ctx, db)
	if err != nil || len(idx) != 1 || idx[0].Name() != "notes_fts" {
		t.Fatalf("indexes = %v, %v", idx, err)
	}
	damaged = idx[0].Check(ctx) != nil
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM notes_fts WHERE notes_fts MATCH ?`, `"`+strings.ToLower(opsInspectMarkerContent)+`"`).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	return damaged, hits
}

var opsInspectTestTime = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

type opsInspectRepairReport struct {
	Index   string `json:"index"`
	Outcome string `json:"outcome"`
	Damaged bool   `json:"damaged"`
	Resumed bool   `json:"resumed"`
}

func opsInspectRunRepair(t *testing.T, dir string, name string) (code int, rep opsInspectRepairReport, stderr string) {
	t.Helper()
	code, out, errOut := opsInspectRun(t, "index", "repair", "-data-dir", dir, "-index", name)
	if strings.TrimSpace(out) != "" {
		opsInspectDecode(t, out, &rep)
	}
	return code, rep, errOut
}

// A damaged derived index is isolated, rebuilt from its source, checked and released by one command
// outside an upgrade, and only that index's record is touched: another index that is still isolated
// stays isolated.
func TestIndexRepairRebuildsADamagedIndex(t *testing.T) {
	dir := t.TempDir()
	path := opsInspectNotesDB(t, dir)
	q := upgrade.NewQuarantine(dir, clock.Real{})
	// Another index that an upgrade isolated and nothing has rebuilt: it is empty, and its record is
	// the only thing that tells retrieval so. Repairing the notes index must not release it.
	if err := q.Add(upgrade.VectorIndexName, "isolated by an earlier upgrade"); err != nil {
		t.Fatal(err)
	}

	// Healthy: nothing is done and nothing is isolated.
	if code, rep, errOut := opsInspectRunRepair(t, dir, "notes_fts"); code != ExitOK || rep.Outcome != "healthy" || rep.Damaged || q.Has("notes_fts") {
		t.Fatalf("a healthy index: %d %+v (%s), isolated %v", code, rep, errOut, q.Has("notes_fts"))
	}

	opsInspectDamageIndex(t, path)
	if damaged, hits := opsInspectNotesState(t, path); !damaged || hits != 0 {
		t.Fatalf("the damaged index: damaged %v, %d hits (the integrity check must fail and a search must not return wrong rows)", damaged, hits)
	}
	// An unknown index is a usage error that names the indexes there are.
	if code, _, errOut := opsInspectRunRepair(t, dir, "no_such_index"); code != ExitUsage || !strings.Contains(errOut, "notes_fts") || !strings.Contains(errOut, upgrade.VectorIndexName) {
		t.Errorf("an unknown index = %d (%s)", code, errOut)
	}
	if code, _, _ := opsInspectRun(t, "index", "repair", "-data-dir", dir); code != ExitUsage {
		t.Errorf("no -index = %d, want a usage error", code)
	}

	code, rep, errOut := opsInspectRunRepair(t, dir, "notes_fts")
	if code != ExitOK || rep.Outcome != "rebuilt" || !rep.Damaged || rep.Index != "notes_fts" {
		t.Fatalf("repair = %d %+v (%s), want rebuilt after finding it damaged", code, rep, errOut)
	}
	if damaged, hits := opsInspectNotesState(t, path); damaged || hits != 1 {
		t.Errorf("after the repair: damaged %v, %d hits, want healthy and finding the marker once", damaged, hits)
	}
	if q.Has("notes_fts") {
		t.Error("the repaired index is still isolated")
	}
	if !q.Has(upgrade.VectorIndexName) {
		t.Fatal("repairing one index released the record of another that is still isolated: retrieval would answer from an empty index without saying so")
	}
	// The content the index is derived from is untouched.
	db, err := store.Open(context.Background(), store.Options{Path: path, Migrations: store.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	var notes int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM notes`).Scan(&notes); err != nil || notes != 31 {
		t.Errorf("%d notes after the repair, %v", notes, err)
	}
	_ = db.Close()

	// A repair that was interrupted after the record was written (the index is isolated, maybe empty)
	// is finished the same way, and says so.
	if err := q.Add("notes_fts", "interrupted"); err != nil {
		t.Fatal(err)
	}
	if code, rep, errOut := opsInspectRunRepair(t, dir, "notes_fts"); code != ExitOK || rep.Outcome != "rebuilt" || !rep.Resumed || rep.Damaged || q.Has("notes_fts") {
		t.Errorf("resuming an isolated index: %d %+v (%s), isolated %v", code, rep, errOut, q.Has("notes_fts"))
	}
	if !q.Has(upgrade.VectorIndexName) {
		t.Error("finishing the notes repair released the vector record")
	}
}

// When the rebuild cannot succeed (the source is gone) the command fails, says so, and leaves the
// index isolated so nothing serves from it.
func TestIndexRepairKeepsTheRecordWhenTheRebuildFails(t *testing.T) {
	dir := t.TempDir()
	path := opsInspectNotesDB(t, dir)
	opsInspectDamageIndex(t, path)
	db, err := store.Open(context.Background(), store.Options{Path: path, Migrations: store.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `DROP TRIGGER notes_ai`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `DROP TABLE notes`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	code, rep, errOut := opsInspectRunRepair(t, dir, "notes_fts")
	if code != ExitError || rep.Outcome != "failed" || !strings.Contains(errOut, "restore the last backup") {
		t.Fatalf("repair with no source = %d %+v (%s), want a failure that points at the backup", code, rep, errOut)
	}
	if !upgrade.NewQuarantine(dir, clock.Real{}).Has("notes_fts") {
		t.Error("the index left the quarantine although it could not be rebuilt")
	}
}

type opsInspectVectorState struct{ vectors, generationCount int }

func opsInspectVectors(t *testing.T, path string) opsInspectVectorState {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: path, Migrations: store.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var s opsInspectVectorState
	if err := db.QueryRowContext(context.Background(), `SELECT (SELECT count(*) FROM vector_rows), (SELECT coalesce(sum(vectors), 0) FROM vector_generations)`).Scan(&s.vectors, &s.generationCount); err != nil {
		t.Fatal(err)
	}
	return s
}

// A vector index is the output of the embedding model: a healthy one is left alone, a damaged one
// is isolated (emptied, recorded) and stays isolated, "pending", for the serve process that has the
// embedder to re-embed (M5-24). The command never releases it.
func TestIndexRepairLeavesAVectorIndexIsolatedForTheEmbedder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, store.DefaultFileName)
	db := storetest.Open(t, nil, func(o *store.Options) { o.Path = path })
	flat, err := vector.NewFlat(db, vector.FlatOptions{Clock: clock.NewFake(opsInspectTestTime)})
	if err != nil {
		t.Fatal(err)
	}
	sc := storetest.Scope("app", "tenant", "ws")
	var items []vector.Item
	for i := 0; i < 3; i++ {
		key, err := vector.NewInputKey(sc, "model-4", fmt.Sprintf("chunk %d", i))
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, vector.Item{ChunkID: fmt.Sprintf("chk_%d", i), Key: key, Vector: []float32{1, float32(i), 2, 3}})
	}
	if _, err := flat.Upsert(context.Background(), sc, vector.UpsertBatch{Generation: "gen1", SourceID: "src1", Model: "model-4", Items: items}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	q := upgrade.NewQuarantine(dir, clock.Real{})

	// Healthy: every vector stays.
	if code, rep, errOut := opsInspectRunRepair(t, dir, upgrade.VectorIndexName); code != ExitOK || rep.Outcome != "healthy" || q.Has(upgrade.VectorIndexName) {
		t.Fatalf("a healthy vector index: %d %+v (%s)", code, rep, errOut)
	}
	if s := opsInspectVectors(t, path); s.vectors != 3 || s.generationCount != 3 {
		t.Fatalf("a healthy vector index lost vectors: %+v", s)
	}

	// Damaged: a generation that counts a vector it does not have.
	db = opsInspectDB(t, path)
	if _, err := db.ExecContext(context.Background(), `UPDATE vector_generations SET vectors = vectors + 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	code, rep, errOut := opsInspectRunRepair(t, dir, upgrade.VectorIndexName)
	if code != ExitOK || rep.Outcome != "pending" || !rep.Damaged {
		t.Fatalf("a damaged vector index: %d %+v (%s), want isolated and pending", code, rep, errOut)
	}
	if !q.Has(upgrade.VectorIndexName) {
		t.Fatal("the vector index left the quarantine although nothing re-embedded it: retrieval would find nothing without saying so")
	}
	if s := opsInspectVectors(t, path); s.vectors != 0 || s.generationCount != 0 {
		t.Errorf("an isolated vector index still serves %+v", s)
	}
	// Again: still isolated, still pending, and said to be a resumed repair.
	if code, rep, _ := opsInspectRunRepair(t, dir, upgrade.VectorIndexName); code != ExitOK || rep.Outcome != "pending" || !rep.Resumed || !q.Has(upgrade.VectorIndexName) {
		t.Errorf("a second repair = %d %+v", code, rep)
	}
}
