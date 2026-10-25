package ingest

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

func hasAudit(types []string, want string) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

// SEC-028: an item deleted while its embedding job waits on the model keeps no
// vector. The deletion is applied (and the item's vectors cleaned up) before
// the job stores its batch, so the job must remove what it stored too late.
func TestEmbeddingsStoredAfterADeletionAreRemoved(t *testing.T) {
	f := newFixture(t)
	corpus(f, 3)
	f.sync()
	vecs := f.vectors()

	var once sync.Once
	fired := false
	hook := func(req providers.EmbedRequest) {
		if !strings.Contains(strings.Join(req.Inputs, "\n"), "doomedtext") {
			return
		}
		once.Do(func() {
			fired = true
			f.remove("doomed.md")
			res, err := f.svc.RequestSync(bg, f.scope, f.srcID, SyncOptions{Key: "while-embedding"})
			if err != nil {
				t.Errorf("RequestSync: %v", err)
				return
			}
			if err := f.svc.syncHandler(bg, res.Job); err != nil {
				t.Errorf("sync: %v", err)
			}
		})
	}
	f.emb.hook.Store(&hook)
	f.write("doomed.md", doc("Doomed", para("doomedtext")))
	f.sync()

	mustEqual(t, "the deletion ran while the model was embedding", fired, true)
	mustEqual(t, "hits", len(f.search("doomedtext")), 0)
	mustEqual(t, "vectors of the deleted document",
		f.countRows(`SELECT count(*) FROM vector_rows WHERE chunk_id IN (SELECT chunk_id FROM ragindex_chunk WHERE text LIKE '%doomedtext%')`), 0)
	mustEqual(t, "vectors", f.vectors(), vecs)
	mustEqual(t, "other documents", len(docHits(f.search("p1a"))), 1)
}

// SEC-028: a deleted source, a removed document or a deleted tenant is invisible
// at once (index, vectors) and stays invisible.
func TestSourceDeletionPropagates(t *testing.T) {
	t.Run("an item that left the source", func(t *testing.T) {
		f := newFixture(t)
		corpus(f, 3)
		f.sync()
		hits := f.search("p1a")
		mustEqual(t, "hits before", len(hits), 1)
		ref := index.RefOf(hits[0])
		vecs := f.vectors()

		f.remove("doc1.md")
		mustEqual(t, "sync", f.sync().ErrorCode, contracts.ErrCode(""))

		mustEqual(t, "hits after", len(f.search("p1a")), 0)
		mustEqual(t, "other documents", len(docHits(f.search("alpha"))), 2)
		mustEqual(t, "vectors", f.vectors(), vecs-3)
		vd, err := f.ix.Recheck(bg, f.scope, []index.Ref{ref}, false)
		if err != nil || vd[0].Visible || vd[0].Reason != index.ReasonGone {
			t.Fatalf("the chunk is still retrievable: %+v %v", vd, err)
		}
		mustEqual(t, "applied tombstones", f.countRows(`SELECT count(*) FROM ragingest_tombstone WHERE source_id = ? AND reason = 'deleted' AND applied_ns > 0`, f.srcID), 1)
		mustEqual(t, "snapshot rows", f.countRows(`SELECT count(*) FROM ragingest_item WHERE source_id = ?`, f.srcID), 2)
		mustEqual(t, "vectors owed", f.countRows(`SELECT count(*) FROM ragingest_version WHERE vec_owed = 1`), 0)
		mustEqual(t, "tombstoned in the status", f.status().Counts.Tombstoned, 1)
	})

	t.Run("a recorded deletion that was not applied yet is finished", func(t *testing.T) {
		f := newFixture(t)
		corpus(f, 2)
		f.sync()
		f.remove("doc0.md")

		// Run the first half of a sync by hand and stop where a crash would: the
		// deletion is written down, nothing is hidden yet.
		prev, err := f.svc.loadSnapshot(bg, f.db, f.scope, f.srcID)
		if err != nil {
			t.Fatal(err)
		}
		cs, err := f.prov.Discover(bg, f.scope, f.def, prev)
		if err != nil {
			t.Fatal(err)
		}
		src, found, err := loadSource(bg, f.db, f.scope, f.srcID)
		if err != nil || !found {
			t.Fatal(err)
		}
		pm := map[string]source.Record{}
		for _, r := range prev {
			pm[r.Key] = r
		}
		target, err := f.svc.planGeneration(bg, f.scope, "", f.srcID)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.svc.persistScan(bg, &scanApply{scope: f.scope, def: f.def, fp: fingerprintOf(f.def), prev: pm, cs: cs, src: src, found: found, target: target, now: f.clk.Now()}); err != nil {
			t.Fatal(err)
		}
		mustEqual(t, "open tombstones", f.countRows(`SELECT count(*) FROM ragingest_tombstone WHERE applied_ns = 0`), 1)
		mustEqual(t, "snapshot rows", f.countRows(`SELECT count(*) FROM ragingest_item`), 1)
		if len(f.search("p0a")) == 0 {
			t.Fatal("the chunk was hidden before the deletion was applied; the window this test needs does not exist")
		}

		// The next sync (or a maintenance pass) finds the open row and finishes.
		f.sync()
		mustEqual(t, "hits", len(f.search("p0a")), 0)
		mustEqual(t, "open tombstones after", f.countRows(`SELECT count(*) FROM ragingest_tombstone WHERE applied_ns = 0`), 0)
	})

	t.Run("a removed source", func(t *testing.T) {
		f := newFixture(t)
		corpus(f, 3)
		f.sync()
		mustEqual(t, "vectors before", f.vectors() > 0, true)
		parses := f.parser.calls.Load()

		// A sync that is still queued when the source is removed ends without effect.
		f.requestSync()
		if err := f.svc.RemoveSource(bg, f.scope, f.srcID); err != nil {
			t.Fatalf("RemoveSource: %v", err)
		}
		f.drain()

		mustEqual(t, "hits", len(f.search("alpha")), 0)
		mustEqual(t, "vectors", f.vectors(), 0)
		mustEqual(t, "live chunks", f.countRows(`SELECT count(*) FROM ragindex_chunk WHERE tombstoned_at IS NULL`), 0)
		mustEqual(t, "tombstones", f.countRows(`SELECT count(*) FROM ragingest_tombstone WHERE reason = 'source_removed' AND applied_ns > 0`), 3)
		mustEqual(t, "open tombstones", f.countRows(`SELECT count(*) FROM ragingest_tombstone WHERE applied_ns = 0`), 0)
		mustEqual(t, "snapshot rows", f.countRows(`SELECT count(*) FROM ragingest_item`), 0)
		mustEqual(t, "working rows", f.countRows(`SELECT count(*) FROM ragingest_version`)+f.countRows(`SELECT count(*) FROM ragingest_embed`), 0)
		if !hasAudit(f.auditTypes(), "deletion.source") {
			t.Fatalf("the deletion was not audited: %v", f.auditTypes())
		}
		st := f.status()
		mustEqual(t, "state", st.State, rag.SourceStatusStateRemoving)
		mustEqual(t, "available", st.Available, false)
		mustEqual(t, "reason", st.Reason, ReasonRemoved)
		if f.parser.calls.Load() != parses {
			t.Fatal("a removed source was parsed")
		}
		for _, g := range f.generations() {
			mustEqual(t, "generation state", g.State, rag.IndexGenerationStateInvalidated)
		}

		// A sync of a removed source does nothing, and removing twice is a no-op.
		if j := f.sync(); j.ErrorCode != "" {
			t.Fatalf("a sync of a removed source failed: %s", j.ErrorCode)
		}
		mustEqual(t, "hits after a late sync", len(f.search("alpha")), 0)
		if err := f.svc.RemoveSource(bg, f.scope, f.srcID); err != nil {
			t.Fatalf("second RemoveSource: %v", err)
		}
		mustEqual(t, "deletion events", countOf(f.auditTypes(), "deletion.source"), 1)

		// After the grace period the maintenance pass physically removes the rows.
		f.clk.Advance(f.cfg.withDefaults().CompactGrace + time.Hour)
		f.drain()
		mustEqual(t, "chunk rows", f.countRows(`SELECT count(*) FROM ragindex_chunk`), 0)
	})

	t.Run("a removed source with the disable behaviour keeps its embeddings inert", func(t *testing.T) {
		f := newFixture(t)
		f.def = f.mkDef(f.srcID, "tenant-1", "ws-1", f.root, func(d *ap.RAGSource) {
			d.Deletion.OnSourceRemoved = ap.RAGSourceDeletionOnSourceRemovedDisable
		})
		f.reg.set(f.def)
		corpus(f, 2)
		f.sync()
		vecs := f.vectors()
		if err := f.svc.RemoveSource(bg, f.scope, f.srcID); err != nil {
			t.Fatal(err)
		}
		mustEqual(t, "hits", len(f.search("alpha")), 0)
		mustEqual(t, "vectors kept", f.vectors(), vecs)
		mustEqual(t, "state", f.status().State, rag.SourceStatusStateDisabled)
		mustEqual(t, "purge window", f.countRows(`SELECT count(*) FROM ragingest_tombstone WHERE purge_after_ns <> 0`), 0)
	})

	t.Run("a source of another scope is not visible", func(t *testing.T) {
		f := newFixture(t)
		corpus(f, 1)
		f.sync()
		other := testScope("ws-9")
		if _, err := f.svc.Status(bg, other, f.srcID); err == nil {
			t.Fatal("another workspace read the status of the source")
		}
		if err := f.svc.RemoveSource(bg, other, f.srcID); err != nil {
			t.Fatalf("RemoveSource in another scope: %v", err)
		}
		mustEqual(t, "the source is untouched", len(docHits(f.search("alpha"))), 1)
		mustEqual(t, "state", f.status().State, rag.SourceStatusStateReady)
	})

	t.Run("a deleted tenant", func(t *testing.T) {
		f := newFixture(t)
		corpus(f, 2)
		w2 := f.addSource("tenant-1", "ws-2", "wsb", 2)
		t2 := f.addSource("tenant-2", "ws-1", "ten", 2)
		f.sync()
		f.syncOther(w2)
		f.syncOther(t2)
		for _, c := range []struct {
			name  string
			scope contracts.Scope
		}{{"ws-1", f.scope}, {"ws-2", w2.scope}, {"tenant-2", t2.scope}} {
			if len(f.searchIn(c.scope, "alpha")) == 0 || f.vectorsIn(c.scope) == 0 {
				t.Fatalf("%s holds nothing before the deletion", c.name)
			}
		}

		// An operator action: without the authorisation nothing happens.
		codeOf(t, f.svc.RemoveTenant(bg, f.scope), contracts.ErrPermissionDenied)
		mustEqual(t, "hits after a denied deletion", len(f.search("alpha")) > 0, true)
		f.oper.allow(true)
		if err := f.svc.RemoveTenant(bg, f.scope); err != nil {
			t.Fatalf("RemoveTenant: %v", err)
		}

		for _, sc := range []contracts.Scope{f.scope, w2.scope} {
			mustEqual(t, "hits of "+sc.WorkspaceID, len(f.searchIn(sc, "alpha")), 0)
			mustEqual(t, "vectors of "+sc.WorkspaceID, f.vectorsIn(sc), 0)
		}
		if len(f.searchIn(t2.scope, "alpha")) == 0 || f.vectorsIn(t2.scope) == 0 {
			t.Fatal("another tenant lost its data")
		}
		mustEqual(t, "tombstones", f.countRows(`SELECT count(*) FROM ragingest_tombstone WHERE reason = 'tenant_deleted' AND applied_ns > 0 AND tenant_id = 'tenant-1'`), 4)
		mustEqual(t, "tenant 2 tombstones", f.countRows(`SELECT count(*) FROM ragingest_tombstone WHERE tenant_id = 'tenant-2'`), 0)
		if !hasAudit(f.auditTypes(), "deletion.tenant") {
			t.Fatalf("the tenant deletion was not audited: %v", f.auditTypes())
		}
		st := f.status()
		mustEqual(t, "state", st.State, rag.SourceStatusStateRemoving)
		mustEqual(t, "reason", st.Reason, ReasonTenantDeleted)
		mustEqual(t, "workspace 2 state", f.statusOf(w2).Reason, ReasonTenantDeleted)
		mustEqual(t, "tenant 2 state", f.statusOf(t2).State, rag.SourceStatusStateReady)

		// Nothing comes back: a sync of the deleted tenant's source does nothing.
		parses := f.parser.calls.Load()
		f.sync()
		mustEqual(t, "hits after a late sync", len(f.search("alpha")), 0)
		mustEqual(t, "parses", f.parser.calls.Load(), parses)

		// The physical purge follows in every workspace of the tenant, from the
		// maintenance the removal itself queued.
		f.clk.Advance(f.cfg.withDefaults().CompactGrace + time.Hour)
		f.drain()
		mustEqual(t, "index rows of the deleted tenant", f.countRows(`SELECT count(*) FROM ragindex_chunk WHERE tenant_id = 'tenant-1'`), 0)
		if f.countRows(`SELECT count(*) FROM ragindex_chunk WHERE tenant_id = 'tenant-2'`) == 0 {
			t.Fatal("another tenant's index rows were purged")
		}
	})
}

func countOf(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}
