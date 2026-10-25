package degradation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/backup"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/upgrade"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// The marker is upper case in the content table (ragindex_chunk) and lower case in the full-text index, so its
// lower-case bytes occur only in the derived index: damaging them damages the index and nothing else.
const (
	corruptMarker   = "QQQDEGRADEMARKERZ"
	corruptIndexed  = "qqqdegrademarkerz"
	corruptQuestion = "qqqdegrademarkerz"
)

func openAt(t *testing.T, path string, clk clock.Clock) *store.DB {
	t.Helper()
	db, err := store.Open(ctxb(), store.Options{Path: path, Clock: clk})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func tableDump(t *testing.T, db *store.DB, q string) string {
	t.Helper()
	rows, err := db.QueryContext(ctxb(), q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var b strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			if bs, ok := v.([]byte); ok {
				v = string(bs)
			}
			fmt.Fprint(&b, v, "|")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Section 18, "SQLite/index corruption": isolate damage, restore a metadata backup or rebuild the derived index.
func TestDegradeIndexCorruption(t *testing.T) {
	t.Run("a damaged derived index is isolated, labelled, and rebuilt from its source", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, store.DefaultFileName)
		clk := clock.NewFake(t0)
		db := openAt(t, path, clk)
		e := newRagOn(t, db)
		a := ragScope("tenant-a")
		src := e.addSource(a, "Parts", "parts", mdDoc("Parts", "The part code is "+corruptMarker+" and it fits the large drive.", "Sprocket one is steel.", "Sprocket two is brass."))
		// The audit log is not rebuildable: put some in, to show recovery never touches it.
		alog, err := audit.New(db, audit.Options{Clock: clk})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			clk.Advance(time.Second)
			if _, err := alog.Append(ctxb(), a, audit.NewEvent{Type: audit.EventSettingsChanged, Data: map[string]any{"n": i}}); err != nil {
				t.Fatal(err)
			}
		}
		const contentSQL = `SELECT rid, chunk_id, text FROM ragindex_chunk ORDER BY rid`
		wantContent := tableDump(t, db, contentSQL)
		wantAudit := tableDump(t, db, `SELECT seq, hash FROM audit_events ORDER BY seq`)
		if !strings.Contains(wantContent, corruptMarker) || wantAudit == "" {
			t.Fatalf("setup: content has the marker %v, audit %q", strings.Contains(wantContent, corruptMarker), wantAudit)
		}
		found := func(e *ragEnv, p *query.Pipeline) bool {
			res := e.mustQuery(p, a, corruptQuestion)
			return strings.Contains(strings.Join(itemTexts(res.Package), "\n"), corruptMarker)
		}
		// Control: before the damage the marker is found.
		if !found(e, e.pipeline(e.deps(), query.DefaultConfig())) {
			t.Fatal("control: the marker is not found in a healthy index")
		}

		// The damage: close cleanly (so it does not sit in the WAL) and flip a letter of every indexed copy.
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{path + "-wal", path + "-shm"} {
			if _, err := os.Stat(f); err == nil {
				t.Fatalf("%s is still there after Close: the damage would sit in the WAL", f)
			}
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		n := bytes.Count(raw, []byte(corruptIndexed))
		if n == 0 {
			t.Fatal("the lower-case marker is not in the file: nothing to damage")
		}
		for off := 0; ; {
			i := bytes.Index(raw[off:], []byte(corruptIndexed))
			if i < 0 {
				break
			}
			raw[off+i+len(corruptIndexed)-1] ^= 0x01
			off += i + len(corruptIndexed)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}

		db = openAt(t, path, clk)
		e = newRagOn(t, db)
		e.cat.set(a.TenantID, src, query.SourceState{DisplayName: "Parts", Authorised: true,
			Availability: conversation.CitationAvailabilityAvailable, Staleness: conversation.CitationStalenessCurrent})

		// Detect: the damaged index says so; the healthy ones are not accused.
		idx, err := upgrade.FTSIndexes(ctxb(), db)
		if err != nil {
			t.Fatal(err)
		}
		var lexical upgrade.DerivedIndex
		for _, ix := range idx {
			if ix.Name() == query.LexicalIndexName {
				lexical = ix
			}
		}
		if lexical == nil {
			t.Fatalf("the lexical index %q is not among the derived indexes", query.LexicalIndexName)
		}
		if err := lexical.Check(ctxb()); !errors.Is(err, upgrade.ErrDamaged) {
			t.Fatalf("Check of the damaged index = %v, want ErrDamaged", err)
		}

		// Isolate: recorded where a restart finds it, emptied, and retrieval says so instead of answering "no match".
		quarantine := upgrade.NewQuarantine(dir, clk)
		damaged, err := upgrade.IsolateDamaged(ctxb(), quarantine, idx)
		if err != nil || len(damaged) != 1 || damaged[0] != query.LexicalIndexName {
			t.Fatalf("IsolateDamaged = %v, %v, want only %s", damaged, err, query.LexicalIndexName)
		}
		if !upgrade.NewQuarantine(dir, clk).Has(query.LexicalIndexName) {
			t.Fatal("the quarantine record did not survive")
		}
		d := e.deps()
		d.Quarantine = quarantine
		res, qerr := e.query(e.pipeline(d, query.DefaultConfig()), a, corruptQuestion)
		if !has(res.Diagnostics.Isolated, query.LexicalIndexName) {
			t.Fatalf("isolated %v: the query must say the lexical index was not searched (err %v)", res.Diagnostics.Isolated, qerr)
		}
		if qerr == nil && len(res.Package.Items) == 0 && !hasNotice(res.Package, rag.RetrievalNoticeReducedSemanticRecall) {
			t.Fatal("an isolated index produced an empty, unlabelled package: that reads as \"nothing matched\"")
		}
		// Nothing is served from the damaged bytes.
		if got, err := e.ix.Search(ctxb(), a, index.Query{Text: corruptQuestion}); err != nil || len(got.Hits) != 0 {
			t.Fatalf("the isolated index served %d hits (err %v)", len(got.Hits), err)
		}
		// The source of truth and the records that cannot be rebuilt are untouched.
		if got := tableDump(t, db, contentSQL); got != wantContent {
			t.Fatal("the content table changed while its index was isolated")
		}
		if got := tableDump(t, db, `SELECT seq, hash FROM audit_events ORDER BY seq`); got != wantAudit {
			t.Fatal("the audit log changed during recovery")
		}

		// Rebuild: from the content table, checked, released.
		rebuilt, err := upgrade.RebuildQuarantined(ctxb(), quarantine, idx)
		if err != nil || len(rebuilt) != 1 || rebuilt[0] != query.LexicalIndexName {
			t.Fatalf("RebuildQuarantined = %v, %v", rebuilt, err)
		}
		if quarantine.Has(query.LexicalIndexName) {
			t.Fatal("the quarantine record was not cleared after the rebuild")
		}
		d.Quarantine = quarantine
		if !found(e, e.pipeline(d, query.DefaultConfig())) {
			t.Fatal("the marker is not found after the rebuild")
		}
		if got := tableDump(t, db, contentSQL); got != wantContent {
			t.Fatal("the rebuild changed the content it rebuilds from")
		}
		if err := db.IntegrityCheck(ctxb()); err != nil {
			t.Fatalf("integrity check after the rebuild: %v", err)
		}
	})

	t.Run("a damaged database file is replaced by the signed metadata backup", func(t *testing.T) {
		dataDir := t.TempDir()
		path := filepath.Join(dataDir, backup.DatabaseFile)
		clk := clock.NewFake(t0)
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		keys := backup.StaticKeys{Keys: map[string]ed25519.PublicKey{"degrade-test": pub}}
		db := openAt(t, path, clk)
		sc := storetest.Scope("app1", "tenant1", "ws1")
		alog, err := audit.New(db, audit.Options{Clock: clk})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			clk.Advance(time.Second)
			if _, err := alog.Append(ctxb(), sc, audit.NewEvent{Type: audit.EventSettingsChanged, Data: map[string]any{"n": i}}); err != nil {
				t.Fatal(err)
			}
		}
		chain := audit.ChainKey{ApplicationID: "app1", TenantID: "tenant1"}
		before, err := alog.VerifyChain(ctxb(), chain, audit.VerifyOptions{})
		if err != nil {
			t.Fatal(err)
		}
		made, err := backup.Create(ctxb(), db, backup.Options{Dir: t.TempDir(), DataDir: dataDir, Trees: []string{},
			Signer: trust.SigningKey{KeyID: "degrade-test", Key: priv}, Clock: clk, Version: "test"})
		if err != nil {
			t.Fatalf("backup.Create: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		// Structural damage: not a derived index, the file itself.
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i := 4096; i < len(raw); i++ {
			raw[i] = 0xA5
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.IntegrityCheckFile(ctxb(), path); err == nil {
			t.Fatal("the damaged file passes its integrity check: the test did not damage it")
		}

		// The database in place is the damaged one: its key revocations cannot be
		// carried over (a restore refuses by default), and the operator says so.
		if _, err := backup.Restore(ctxb(), backup.RestoreOptions{BackupDir: made.Dir, DataDir: dataDir, Keys: keys, Clock: clk}); err == nil {
			t.Fatal("a restore over a database whose revocations cannot be read went ahead without the operator's say-so")
		}
		res, err := backup.Restore(ctxb(), backup.RestoreOptions{BackupDir: made.Dir, DataDir: dataDir, Keys: keys, Clock: clk, SkipNewerRevocations: true})
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		// The damaged file is kept aside, never deleted; the restored one is whole.
		if res.PreRestoreDir == "" {
			t.Error("the damaged state was not kept")
		} else if _, err := os.Stat(res.PreRestoreDir); err != nil {
			t.Errorf("the pre-restore directory is gone: %v", err)
		}
		if err := store.IntegrityCheckFile(ctxb(), path); err != nil {
			t.Fatalf("the restored database fails its integrity check: %v", err)
		}
		db = openAt(t, path, clk)
		alog, err = audit.New(db, audit.Options{Clock: clk})
		if err != nil {
			t.Fatal(err)
		}
		after, err := alog.VerifyChain(ctxb(), chain, audit.VerifyOptions{})
		if err != nil || after.Events != before.Events || after.HeadHash != before.HeadHash {
			t.Fatalf("audit chain after the restore: %+v err %v, want %d events with head %s", after, err, before.Events, before.HeadHash)
		}
		// A backup whose signature does not verify is refused, not restored over the damage.
		otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
		wrong := backup.StaticKeys{Keys: map[string]ed25519.PublicKey{"degrade-test": otherPub}}
		_, err = backup.Restore(ctxb(), backup.RestoreOptions{BackupDir: made.Dir, DataDir: t.TempDir(), Keys: wrong, Clock: clk})
		if err == nil {
			t.Fatal("a backup signed by another key was restored")
		}
	})
}
