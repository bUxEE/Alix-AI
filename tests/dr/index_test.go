package dr

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/upgrade"
	"github.com/buxee/alix-ai/tools/devkeys"
)

const (
	// transcriptTurns is how many transcript turns the drill gives the history index: enough for a
	// rebuild to take a measurable time, small enough to keep the drill quick.
	transcriptTurns = 20000
	// indexMarker is upper case in the content table and lower case in the FTS index, so its
	// lower-case bytes occur only in the derived index: damaging them damages the index and nothing
	// else. (internal/upgrade/derived_test.go uses the same device.)
	indexMarker = "QQQDRILLMARKERZ"
)

type ftsFinding struct {
	Damaged bool
	Hits    int
}

// ftsState checks the history index of the data directory and asks it for the marker, in-process.
func ftsState(t *testing.T, d *deployment, dbPath string) ftsFinding {
	t.Helper()
	ctx := context.Background()
	db := openDB(t, d.cfgPath, dbPath)
	defer db.Close()
	idx, err := upgrade.FTSIndexes(ctx, db)
	if err != nil {
		t.Fatalf("list the full-text indexes: %v", err)
	}
	var f ftsFinding
	found := false
	for _, ix := range idx {
		if ix.Name() != "history_fts" {
			continue
		}
		found = true
		switch err := ix.Check(ctx); {
		case err == nil:
		case errors.Is(err, upgrade.ErrDamaged):
			f.Damaged = true
		default:
			t.Fatalf("the check of history_fts could not be made: %v", err)
		}
	}
	if !found {
		t.Fatal("the database has no history_fts index")
	}
	row := db.QueryRowContext(ctx, `SELECT count(*) FROM history_fts WHERE history_fts MATCH ?`, `"`+strings.ToLower(indexMarker)+`"`)
	if err := row.Scan(&f.Hits); err != nil {
		t.Fatalf("search the history index: %v", err)
	}
	return f
}

// damageIndexBytes flips one letter of every copy of the indexed marker in a database file, which is
// what a bad sector or a stray write in a derived structure looks like: the b-tree stays valid, the
// index no longer matches its source.
func damageIndexBytes(t *testing.T, path string) int {
	t.Helper()
	for _, f := range []string{path + "-wal", path + "-shm"} {
		if fileExists(f) {
			t.Fatalf("%s is still there: damage written to the main file would sit behind the WAL", f)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	needle := []byte(strings.ToLower(indexMarker))
	n := bytes.Count(b, needle)
	if n == 0 {
		t.Fatal("the lower-case marker is not in the database file: there is nothing to damage")
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
	return n
}

// TestDRCorruptIndexRebuild damages the history index in the database file under a deployment that
// holds a receipt and an audit trail, and checks master plan 18, "SQLite/index corruption: isolate
// damage, restore metadata backup or rebuild derived index" and 19 ("derived embeddings and indexes can
// be rebuilt"): the damage is detected, the index is rebuilt from the transcript it is derived from
// by the operator's own command, and nothing that cannot be rebuilt (receipts, the audit chains, the
// transcript) is touched.
func TestDRCorruptIndexRebuild(t *testing.T) {
	e := sharedEnv(t)
	d := e.newDeployment(t)
	w := seedWorld(t, e.clinic, "index", 1)

	// A deployment with something that cannot be rebuilt in it: a committed booking, through the
	// real service.
	c := d.openCore()
	prep := c.prepareBooking(w, 0)
	c.close()
	srv := d.startServe()
	if status, body, err := srv.call("POST", "/v1/actions/"+prep.ID+"/confirm", w.hostToken(t, prep.SessionID), map[string]any{"token": prep.Secret}); err != nil || status != 200 {
		t.Fatalf("book through the service = %d, %v: %s", status, err, body)
	}
	srv.kill()

	// The transcript the index is derived from.
	c = d.openCore()
	c.seedTranscript(w, transcriptTurns, indexMarker)
	c.close()
	before := takeSnapshot(t, d.cfgPath, d.dbPath())
	if err := before.chainsOK(); err != nil {
		t.Fatal(err)
	}
	if len(before.Receipts) != 1 || before.Turns != transcriptTurns {
		t.Fatalf("setup: %s", before)
	}
	if f := ftsState(t, d, d.dbPath()); f.Damaged || f.Hits != 1 {
		t.Fatalf("before the damage the index is %+v, want healthy with one hit", f)
	}

	// ---- the damage ----
	flipped := damageIndexBytes(t, d.dbPath())
	f := ftsState(t, d, d.dbPath())
	if !f.Damaged {
		t.Fatalf("the integrity check of the damaged index passed: %+v", f)
	}
	if f.Hits != 0 {
		t.Fatalf("the damaged index still answers the marker (%d hits): a search would return wrong rows", f.Hits)
	}
	t.Logf("flipped %d copies of the marker in the index; integrity-check fails, search finds %d", flipped, f.Hits)

	// What the operator's own tools say. `upgrade status` reads the quarantine; `upgrade recover`
	// finishes an interrupted upgrade and rebuilds what is already isolated: neither looks for fresh
	// damage (it is found by `upgrade apply`, which checks every derived index first). Recorded as
	// observed; production.md says the same.
	status := d.must("upgrade", "status")
	recoverRes := d.must("upgrade", "recover")
	recordObservation("corrupt index", "`upgrade status` on the damaged database: %s; `upgrade recover`: %s",
		oneLine(status.Stdout), oneLine(recoverRes.Stdout))
	t.Logf("upgrade status: %s | upgrade recover: %s", oneLine(status.Stdout), oneLine(recoverRes.Stdout))
	if g := ftsState(t, d, d.dbPath()); !g.Damaged {
		t.Logf("note: `upgrade recover` repaired the index by itself; production.md must say so")
	}

	// The rebuild itself, measured on a copy of the damaged file (the real repair is timed below).
	rebuildMs := timeIndexRebuild(t, d)
	recordTiming("index rebuild (FTS5 'rebuild' of history_fts)", rebuildMs, "%d transcript turns, in-process on a copy of the damaged database", transcriptTurns)

	// ---- the repair, with the real command ----
	var applied struct {
		Outcome string   `json:"outcome"`
		Damaged []string `json:"derived_damaged"`
		Rebuilt []string `json:"derived_rebuilt"`
		Backup  string   `json:"backup_id"`
	}
	r := d.must("upgrade", "apply", "-manifest", e.releaseManifest, "-allow-dev-key",
		"-pubkey", devkeys.KeyID+"="+filepath.Join(e.moduleRoot, "tools", "devkeys", "test-only-publisher.pub"))
	if err := r.decode(&applied); err != nil {
		t.Fatal(err)
	}
	recordTiming("upgrade apply that repairs the damaged index", r.Dur, "verify the signed release, rebuild the damaged index, signed pre-upgrade backup, migrations, probe; %d transcript turns", transcriptTurns)
	if applied.Outcome != "applied" || len(applied.Damaged) != 1 || applied.Damaged[0] != "history_fts" ||
		len(applied.Rebuilt) != 1 || applied.Rebuilt[0] != "history_fts" {
		t.Fatalf("upgrade apply = %+v, want applied with history_fts damaged and rebuilt\n%s", applied, r)
	}

	// ---- afterwards ----
	if f := ftsState(t, d, d.dbPath()); f.Damaged || f.Hits != 1 {
		t.Fatalf("after the repair the index is %+v, want healthy and finding the marker once", f)
	}
	after := takeSnapshot(t, d.cfgPath, d.dbPath())
	if err := after.chainsOK(); err != nil {
		t.Fatal(err)
	}
	if err := sameReceipts(before.Receipts, after.Receipts); err != nil {
		t.Fatalf("the repair changed the receipts: %v", err)
	}
	if after.Turns != before.Turns || after.TurnsDigest != before.TurnsDigest {
		t.Fatalf("the repair changed the transcript: %d turns became %d", before.Turns, after.Turns)
	}
	for _, b := range before.Chains {
		a, ok := after.chain(b.Chain)
		if !ok || a.Events < b.Events {
			t.Fatalf("audit chain %s went from %d events to %+v", b.Chain, b.Events, a)
		}
	}
	if err := d.must("upgrade", "status").decode(&map[string]any{}); err != nil {
		t.Fatal(err)
	}

	// The service starts on the repaired database and still answers for the receipt.
	srv2 := d.startServe()
	if status, body, err := srv2.call("GET", "/healthz", "", nil); err != nil || status != 200 {
		t.Fatalf("/healthz after the repair = %d, %v: %s", status, err, body)
	}
	if status, body := srv2.receipt(t, w, prep.SessionID, prep.ID); status != 200 || !strings.Contains(string(body), `"status":"committed"`) {
		t.Fatalf("the receipt after the repair = %d %s", status, body)
	}
	srv2.kill()
	recordObservation("corrupt index", "after `upgrade apply`: history_fts was reported damaged and rebuilt, the marker is found again; receipts, transcript and audit chains are unchanged (%s)", after.Summary())
}

// timeIndexRebuild copies the damaged database file to a scratch directory and rebuilds its history
// index there, returning how long the rebuild took.
func timeIndexRebuild(t *testing.T, d *deployment) time.Duration {
	t.Helper()
	scratch := filepath.Join(t.TempDir(), "alixai.db")
	b, err := os.ReadFile(d.dbPath())
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, scratch, b)
	ctx := context.Background()
	db := openDB(t, d.cfgPath, scratch)
	defer db.Close()
	idx, err := upgrade.FTSIndexes(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, ix := range idx {
		if ix.Name() != "history_fts" {
			continue
		}
		begin := time.Now()
		if err := ix.Rebuild(ctx); err != nil {
			t.Fatalf("rebuild history_fts: %v", err)
		}
		took := time.Since(begin)
		if err := ix.Check(ctx); err != nil {
			t.Fatalf("the rebuilt index does not pass its check: %v", err)
		}
		return took
	}
	t.Fatal("no history_fts to rebuild")
	return 0
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
