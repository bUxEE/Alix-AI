package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

func TestScanLimitsApplyNothing(t *testing.T) {
	f := newFixture(t)
	f.def = f.mkDef(f.srcID, "tenant-1", "ws-1", f.root, func(d *ap.RAGSource) { d.Limits.MaxFiles = 2 })
	f.reg.set(f.def)
	corpus(f, 3)

	// Three files against a limit of two: resource_exhausted, nothing applied.
	j := f.sync()
	mustEqual(t, "job state", j.State, jobs.StateFailed)
	mustEqual(t, "job code", j.ErrorCode, contracts.ErrResourceExhausted)
	mustEqual(t, "attempts (the same scan finds the same excess)", j.Attempts, 1)
	mustEqual(t, "snapshot rows", f.countRows(`SELECT count(*) FROM ragingest_item`), 0)
	mustEqual(t, "versions", f.countRows(`SELECT count(*) FROM ragingest_version`), 0)
	mustEqual(t, "generations", len(f.generations()), 0)
	st := f.status()
	mustEqual(t, "state", st.State, rag.SourceStatusStatePaused)
	mustEqual(t, "reason", st.Reason, ReasonLimitExceeded)
	mustEqual(t, "error code", st.ErrorCode, string(contracts.ErrResourceExhausted))
	if !st.LastSuccess.IsZero() {
		t.Fatalf("a scan that applied nothing is reported as checked: %v", st.LastSuccess)
	}

	// Within the limit again, the source recovers.
	f.remove("doc2.md")
	mustEqual(t, "sync", f.sync().State, jobs.StateSucceeded)
	mustEqual(t, "documents", len(docHits(f.search("alpha"))), 2)
	st = f.status()
	mustEqual(t, "state after recovery", st.State, rag.SourceStatusStateReady)
	mustEqual(t, "available", st.Available, true)

	// Past the limit with data indexed: the old data keeps serving, the new item
	// and everything else the scan found is not applied.
	f.write("late.md", doc("Late", para("lateone")))
	f.write("later.md", doc("Later", para("latetwo")))
	j = f.sync()
	mustEqual(t, "job code with data", j.ErrorCode, contracts.ErrResourceExhausted)
	mustEqual(t, "documents", len(docHits(f.search("alpha"))), 2)
	mustEqual(t, "the new text", len(f.search("lateone")), 0)
	mustEqual(t, "snapshot rows", f.countRows(`SELECT count(*) FROM ragingest_item`), 2)
}

func TestUnreachableRootKeepsEverything(t *testing.T) {
	f := newFixture(t)
	corpus(f, 2)
	f.sync()
	vecs := f.vectors()
	last := f.status().LastSuccess
	f.clk.Advance(time.Hour)

	// The root itself is gone (not an empty directory: gone).
	if err := os.Rename(f.root, f.root+".away"); err != nil {
		t.Fatal(err)
	}
	j := f.sync()
	mustEqual(t, "job state", j.State, jobs.StateFailed)
	mustEqual(t, "job code", j.ErrorCode, contracts.ErrUnavailable)
	if j.Attempts <= 1 {
		t.Fatalf("an unreachable root is retried, attempts = %d", j.Attempts)
	}
	mustEqual(t, "documents", len(docHits(f.search("alpha"))), 2)
	mustEqual(t, "vectors", f.vectors(), vecs)
	mustEqual(t, "tombstones", f.countRows(`SELECT count(*) FROM ragingest_tombstone`), 0)
	st := f.status()
	mustEqual(t, "available", st.Available, false)
	mustEqual(t, "reason", st.Reason, ReasonRootUnreachable)
	if !st.LastSuccess.Equal(last) {
		t.Fatalf("an unreachable source was reported as checked: %v -> %v", last, st.LastSuccess)
	}
	if st.Hold != nil {
		t.Fatal("an unreachable root is not a held deletion")
	}

	// Back again: unchanged, nothing parsed.
	if err := os.Rename(f.root+".away", f.root); err != nil {
		t.Fatal(err)
	}
	parses := f.parser.calls.Load()
	mustEqual(t, "sync", f.sync().State, jobs.StateSucceeded)
	mustEqual(t, "available", f.status().Available, true)
	mustEqual(t, "parses", f.parser.calls.Load(), parses)
}

// scriptedProvider wraps a provider and lets a test rewrite what a scan reports.
type scriptedProvider struct {
	source.Provider
	rewrite func(cs source.ChangeSet, prev []source.Record) source.ChangeSet
}

func (p *scriptedProvider) Discover(ctx context.Context, scope contracts.Scope, def source.Definition, prev []source.Record) (source.ChangeSet, error) {
	cs, err := p.Provider.Discover(ctx, scope, def, prev)
	if err != nil || p.rewrite == nil {
		return cs, err
	}
	return p.rewrite(cs, prev), nil
}

func TestScanFailuresKeepRecordsAndSkippedItemsLeave(t *testing.T) {
	f := newFixture(t)
	f.write("a.md", doc("A", para("aaa")))
	f.write("sub/b.md", doc("B", para("bbb")))
	f.write("c.md", doc("C", para("ccc")))
	f.sync()
	script := &scriptedProvider{Provider: f.prov}
	f.reg.prov = script

	// A directory that cannot be listed: the provider reports it as a failure and
	// keeps the records of what it hides. Nothing is tombstoned for it.
	script.rewrite = func(cs source.ChangeSet, prev []source.Record) source.ChangeSet {
		cs.Failed = append(cs.Failed, source.Failure{Key: "sub", Err: contracts.New(contracts.ErrUnavailable, "listing failed")})
		cs.Counts.Failed = 1
		cs.Next = prev
		cs.Deleted, cs.Counts.Tombstoned = nil, 0
		return cs
	}
	mustEqual(t, "sync", f.sync().State, jobs.StateSucceeded)
	mustEqual(t, "b is still searchable", len(f.search("bbb")), 1)
	mustEqual(t, "snapshot rows", f.countRows(`SELECT count(*) FROM ragingest_item`), 3)
	mustEqual(t, "tombstones", f.countRows(`SELECT count(*) FROM ragingest_tombstone`), 0)
	if st := f.status(); st.State != rag.SourceStatusStateReady {
		t.Fatalf("state = %s", st.State)
	}

	// An item that became a link (or whose bytes no longer match its name) is
	// skipped by the provider and tombstoned with that reason: it must leave
	// retrieval.
	script.rewrite = func(cs source.ChangeSet, prev []source.Record) source.ChangeSet {
		var next []source.Record
		for _, r := range prev {
			if r.Key == "a.md" {
				cs.Deleted = append(cs.Deleted, source.Tombstone{Key: r.Key, Reason: source.ReasonSymlink, Last: r.Version(), At: f.clk.Now()})
				cs.Skipped = append(cs.Skipped, source.Skip{Key: r.Key, Reason: source.ReasonSymlink})
				continue
			}
			next = append(next, r)
		}
		cs.Next = next
		return cs
	}
	mustEqual(t, "sync", f.sync().State, jobs.StateSucceeded)
	mustEqual(t, "a is gone", len(f.search("aaa")), 0)
	mustEqual(t, "b stays", len(f.search("bbb")), 1)
	mustEqual(t, "tombstone reason", f.countRows(`SELECT count(*) FROM ragingest_tombstone WHERE reason = 'symlink' AND applied_ns > 0`), 1)
	mustEqual(t, "skipped in the status", f.status().Counts.Skipped, 1)
}

func TestRealProviderSkipsLinksAndMismatches(t *testing.T) {
	f := newFixture(t)
	f.write("ok.md", doc("Ok", para("okaytext")))
	// A name that claims a PDF over bytes that are text; a type the declaration
	// does not list; a type it lists but the parser has no reader for.
	f.write("fake.pdf", "just some text pretending to be a PDF")
	f.write("table.csv", "a,b\n1,2\n")
	f.def = f.mkDef(f.srcID, "tenant-1", "ws-1", f.root, func(d *ap.RAGSource) {
		d.Formats = append(d.Formats, source.MIMEPDF, source.MIMEJSON)
	})
	f.reg.set(f.def)
	f.write("data.json", `{"a": 1}`)
	outside := filepath.Join(filepath.Dir(f.root), "outside.md")
	if err := os.WriteFile(outside, []byte("OUTSIDE the root"), 0o644); err != nil {
		t.Fatal(err)
	}
	linked := os.Symlink(outside, filepath.Join(f.root, "link.md")) == nil

	mustEqual(t, "sync", f.sync().State, jobs.StateSucceeded)
	mustEqual(t, "indexed documents", len(docHits(f.search("okaytext"))), 1)
	mustEqual(t, "the file behind a link is not read", len(f.search("OUTSIDE")), 0)
	st := f.versionStates()
	mustEqual(t, "indexed versions", st[vIndexed], 1)
	mustEqual(t, "versions skipped for want of a reader", st[vSkipped], 1) // data.json
	info := f.status()
	// fake.pdf (mime_mismatch), table.csv (format_not_accepted) and the link, if
	// the platform allows one, are skipped by the provider.
	want := 2
	if linked {
		want = 3
	}
	if info.Counts.Skipped != want+1 { // plus data.json
		t.Fatalf("skipped = %d, want %d", info.Counts.Skipped, want+1)
	}
	mustEqual(t, "documents parsed", f.parser.calls.Load(), 1)
}

func TestSizeAndTimeShortcutIsNotTrustedNearTheObservation(t *testing.T) {
	f := newFixture(t)
	// A file written "now" on the fake clock: its modification time is within the
	// provider's racy window of the observation.
	f.writeAt("fresh.md", doc("Fresh", para("version1")), f.clk.Now())
	f.sync()
	mustEqual(t, "parsed", f.parser.calls.Load(), 1)

	// Same size, same modification time, other bytes, one second later: the
	// record is too fresh to trust, so the file is hashed and the change seen.
	f.clk.Advance(time.Second)
	f.writeAt("fresh.md", doc("Fresh", para("version2")), f.clk.Now().Add(-time.Second))
	if err := os.Chtimes(filepath.Join(f.root, "fresh.md"), f.clk.Now().Add(-time.Second), f.clk.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	f.sync()
	mustEqual(t, "parsed after the in-place change", f.parser.calls.Load(), 2)
	mustEqual(t, "the new text", len(f.search("version2")), 1)
	mustEqual(t, "the old text", len(f.search("version1")), 0)

	// Once the record is old enough it is trusted again: nothing is read.
	f.clk.Advance(time.Minute)
	f.sync()
	mustEqual(t, "parsed after a quiet scan", f.parser.calls.Load(), 2)
}

func TestSourceDefinitionChangeTriggersResync(t *testing.T) {
	f := newFixture(t)
	f.write("keep.md", doc("Keep", para("keeptext")))
	f.write("private/secret.md", doc("Secret", para("secrettext")))
	f.sync()
	mustEqual(t, "private document indexed", len(f.search("secrettext")), 1)

	// The same definition again: nothing to do.
	changed, _, err := f.svc.CheckDefinition(bg, f.scope, f.srcID, SyncOptions{})
	if err != nil || changed {
		t.Fatalf("CheckDefinition on an unchanged definition = %v, %v", changed, err)
	}
	before := f.countRows(`SELECT count(*) FROM jobs`)

	// A re-audited package now excludes private/ and admits CSV files.
	f.write("rows.csv", "name,value\nalpha,1\n")
	f.def = f.mkDef(f.srcID, "tenant-1", "ws-1", f.root, func(d *ap.RAGSource) {
		d.Exclude = []ap.GlobPattern{"private/**"}
		d.Formats = append(d.Formats, source.MIMECSV)
	})
	f.reg.set(f.def)
	parses := f.parser.calls.Load()
	changed, res, err := f.svc.CheckDefinition(bg, f.scope, f.srcID, SyncOptions{})
	if err != nil || !changed || res.Job.Type != JobSync {
		t.Fatalf("CheckDefinition = %v, %+v, %v", changed, res, err)
	}
	// Asking again while the sync waits does not queue a second one.
	if again, _, err := f.svc.CheckDefinition(bg, f.scope, f.srcID, SyncOptions{}); err != nil || !again || f.countRows(`SELECT count(*) FROM jobs`) != before+1 {
		t.Fatalf("a second check queued another sync: %v %v", again, err)
	}
	f.drain()

	mustEqual(t, "the excluded document left retrieval", len(f.search("secrettext")), 0)
	mustEqual(t, "the kept document stays", len(f.search("keeptext")), 1)
	mustEqual(t, "tombstone", f.countRows(`SELECT count(*) FROM ragingest_tombstone WHERE reason = 'excluded' AND applied_ns > 0`), 1)
	mustEqual(t, "the new type is picked up", len(f.search("alpha")) > 0, true)
	mustEqual(t, "only the new item was parsed", f.parser.calls.Load(), parses+1)
	fp := fingerprintOf(f.def)
	row, _, err := loadSource(bg, f.db, f.scope, f.srcID)
	if err != nil || row.Fingerprint != fp {
		t.Fatalf("the source row does not carry the new definition: %v", err)
	}
	changed, _, err = f.svc.CheckDefinition(bg, f.scope, f.srcID, SyncOptions{})
	if err != nil || changed {
		t.Fatalf("CheckDefinition after the resync = %v, %v", changed, err)
	}

	// A change that does not decide what is admitted is not a change.
	f.def.Title = "A new title"
	f.reg.set(f.def)
	if changed, _, err := f.svc.CheckDefinition(bg, f.scope, f.srcID, SyncOptions{}); err != nil || changed {
		t.Fatalf("a retitled source needed a rescan: %v %v", changed, err)
	}
}

func TestSyncOfAnotherScopeFails(t *testing.T) {
	f := newFixture(t)
	corpus(f, 1)
	other := testScope("ws-1")
	other.TenantID = "tenant-2"
	res, err := f.svc.RequestSync(bg, other, f.srcID, SyncOptions{Key: "intruder"})
	if err != nil {
		t.Fatal(err)
	}
	f.drain()
	j, err := f.q.Get(bg, other, res.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "job state", j.State, jobs.StateFailed)
	mustEqual(t, "job code", j.ErrorCode, contracts.ErrScopeViolation)
	mustEqual(t, "documents parsed", f.parser.calls.Load(), 0)
	mustEqual(t, "source rows", f.countRows(`SELECT count(*) FROM ragingest_source`), 0)
}

func TestSyncOfAnUnregisteredSourceEndsQuietly(t *testing.T) {
	f := newFixture(t)
	corpus(f, 1)
	f.reg.drop(f.srcID)
	j := f.sync()
	mustEqual(t, "job state", j.State, jobs.StateSucceeded)
	mustEqual(t, "documents parsed", f.parser.calls.Load(), 0)
	mustEqual(t, "source rows", f.countRows(`SELECT count(*) FROM ragingest_source`), 0)
}
