package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

func records(n int) []source.Record {
	out := make([]source.Record, n)
	for i := range out {
		out[i] = source.Record{Key: fmt.Sprintf("f%03d.md", i), Hash: fmt.Sprintf("sha256:%064d", i)}
	}
	return out
}

func gone(recs []source.Record, reason source.Reason) []source.Tombstone {
	out := make([]source.Tombstone, len(recs))
	for i, r := range recs {
		out[i] = source.Tombstone{Key: r.Key, Reason: reason, Last: r.Version()}
	}
	return out
}

func TestEvaluateGuard(t *testing.T) {
	cfg := DefaultConfig().Guard // 50 percent, 1000 items, from 10 items
	cfg.MaxDeleted = 20
	still := func(recs []source.Record) source.ChangeSet {
		return source.ChangeSet{Next: recs, Counts: source.Counts{Discovered: len(recs)}}
	}
	cases := []struct {
		name string
		prev []source.Record
		cs   source.ChangeSet
		hold bool
		why  string
	}{
		{name: "nothing deleted", prev: records(5), cs: still(records(5))},
		{name: "a small source that lost one file", prev: records(5), cs: func() source.ChangeSet {
			c := still(records(4))
			c.Deleted = gone(records(5)[4:], source.ReasonDeleted)
			return c
		}()},
		{name: "a small source that lost most of its files but not all", prev: records(5), cs: func() source.ChangeSet {
			c := still(records(1))
			c.Deleted = gone(records(5)[1:], source.ReasonDeleted)
			return c
		}()},
		{name: "an empty root where there were three files", prev: records(3), cs: func() source.ChangeSet {
			c := source.ChangeSet{}
			c.Deleted = gone(records(3), source.ReasonDeleted)
			return c
		}(), hold: true, why: WhyEmptyRoot},
		{name: "an empty root where there was one file", prev: records(1), cs: func() source.ChangeSet {
			c := source.ChangeSet{}
			c.Deleted = gone(records(1), source.ReasonDeleted)
			return c
		}(), hold: true, why: WhyEmptyRoot},
		{name: "an empty root that was empty before", prev: nil, cs: source.ChangeSet{}},
		{name: "a root that is empty because directories could not be listed", prev: records(3), cs: func() source.ChangeSet {
			// The provider keeps the records of what it could not look at, so Next
			// is not empty and nothing is deleted.
			c := still(records(3))
			c.Failed = []source.Failure{{Key: "sub", Err: contracts.New(contracts.ErrUnavailable, "listing failed")}}
			return c
		}()},
		{name: "half of twenty", prev: records(20), cs: func() source.ChangeSet {
			c := still(records(10))
			c.Deleted = gone(records(20)[10:], source.ReasonDeleted)
			return c
		}()},
		{name: "more than half of twenty", prev: records(20), cs: func() source.ChangeSet {
			c := still(records(9))
			c.Deleted = gone(records(20)[9:], source.ReasonDeleted)
			return c
		}(), hold: true, why: WhyMaxFraction},
		{name: "more than the count limit of a big source", prev: records(200), cs: func() source.ChangeSet {
			c := still(records(170))
			c.Deleted = gone(records(200)[170:], source.ReasonDeleted)
			return c
		}(), hold: true, why: WhyMaxDeleted},
		{name: "the count limit exactly", prev: records(200), cs: func() source.ChangeSet {
			c := still(records(180))
			c.Deleted = gone(records(200)[180:], source.ReasonDeleted)
			return c
		}()},
		{name: "items no longer admitted are rules, not an unmounted share", prev: records(30), cs: func() source.ChangeSet {
			c := source.ChangeSet{}
			c.Deleted = gone(records(30), source.ReasonExcluded)
			return c
		}()},
		{name: "items that became links", prev: records(30), cs: func() source.ChangeSet {
			c := still(nil)
			c.Counts.Skipped = 30
			c.Deleted = gone(records(30), source.ReasonSymlink)
			return c
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := evaluateGuard(cfg, c.prev, c.cs)
			if v.Hold != c.hold || v.Why != c.why {
				t.Fatalf("hold=%v why=%q, want hold=%v why=%q (%+v)", v.Hold, v.Why, c.hold, c.why, v)
			}
			if v.Hold && (v.Digest == "" || v.Deleted != len(c.cs.Deleted) || v.Previous != len(c.prev)) {
				t.Fatalf("verdict = %+v", v)
			}
		})
	}

	t.Run("the digest names exactly the deletion set", func(t *testing.T) {
		a := guardDigest(10, gone(records(6), source.ReasonDeleted))
		mustEqual(t, "same set", guardDigest(10, gone(records(6), source.ReasonDeleted)), a)
		if guardDigest(10, gone(records(7), source.ReasonDeleted)) == a {
			t.Error("a larger set has the digest of a smaller one")
		}
		if guardDigest(11, gone(records(6), source.ReasonDeleted)) == a {
			t.Error("another snapshot size has the same digest")
		}
		changed := gone(records(6), source.ReasonDeleted)
		changed[2].Last.Hash = "sha256:" + fmt.Sprintf("%064d", 99)
		if guardDigest(10, changed) == a {
			t.Error("a changed version has the same digest")
		}
	})
}

// unmount simulates a NAS share or a removable drive that is not mounted: the
// mount point is a reachable, empty directory.
func (f *fixture) unmount() {
	f.t.Helper()
	if err := os.Rename(f.root, f.root+".unmounted"); err != nil {
		f.t.Fatal(err)
	}
	if err := os.MkdirAll(f.root, 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) remount() {
	f.t.Helper()
	if err := os.Remove(f.root); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Rename(f.root+".unmounted", f.root); err != nil {
		f.t.Fatal(err)
	}
}

func TestMassDeletionGuardHoldsAnUnmountedRoot(t *testing.T) {
	f := newFixture(t)
	corpus(f, 3)
	f.sync()
	vecs, chunks := f.vectors(), f.activeGen().Counts.Chunks
	base := f.counters()
	last := f.status().LastSuccess
	f.clk.Advance(time.Minute)

	f.unmount()
	j := f.sync()

	// The scan is refused as a whole, with a typed error that is not retried.
	mustEqual(t, "job state", j.State, jobs.StateFailed)
	mustEqual(t, "job code", j.ErrorCode, contracts.ErrUnavailable)
	mustEqual(t, "attempts", j.Attempts, 1)
	if !IsMassDeletion(massDeletionError(guardVerdict{Hold: true, Deleted: 3, Previous: 3, Why: WhyEmptyRoot, Digest: "d"})) ||
		IsMassDeletion(contracts.New(contracts.ErrUnavailable, "the root is unreachable")) {
		t.Fatal("IsMassDeletion does not tell the refusal from other unavailable errors")
	}
	if !contains(j.ErrorMessage, "held") {
		t.Fatalf("the job's error does not say the scan was held: %q", j.ErrorMessage)
	}
	// Nothing was applied: the index, the vectors, the snapshot and the versions
	// are as they were, and no tombstone exists.
	mustEqual(t, "documents searchable", len(docHits(f.search("alpha"))), 3)
	mustEqual(t, "chunks", f.activeGen().Counts.Chunks, chunks)
	mustEqual(t, "vectors", f.vectors(), vecs)
	mustEqual(t, "snapshot rows", f.countRows(`SELECT count(*) FROM ragingest_item`), 3)
	mustEqual(t, "tombstones", f.countRows(`SELECT count(*) FROM ragingest_tombstone`), 0)
	mustEqual(t, "indexed versions", f.versionStates()[vIndexed], 3)
	if f.counters() != base {
		t.Fatalf("a held scan cost work: %+v -> %+v", base, f.counters())
	}

	// The source is unavailable, and an operator can see why.
	st := f.status()
	mustEqual(t, "available", st.Available, false)
	mustEqual(t, "reason", st.Reason, ReasonMassDeletion)
	mustEqual(t, "error code", st.ErrorCode, string(contracts.ErrUnavailable))
	if !st.LastSuccess.Equal(last) {
		t.Fatalf("a refused scan advanced the last success: %v -> %v", last, st.LastSuccess)
	}
	if st.Hold == nil || st.Hold.Why != WhyEmptyRoot || st.Hold.Deleted != 3 || st.Hold.Previous != 3 || st.Hold.Confirmed || st.Hold.Digest == "" {
		t.Fatalf("hold = %+v", st.Hold)
	}
	mustEqual(t, "held events", countOf(f.auditTypes(), "ingest.mass_deletion_held"), 1)

	// A second scan of the same tree is the same refusal and is not audited again.
	f.sync()
	mustEqual(t, "held events after a repeat", countOf(f.auditTypes(), "ingest.mass_deletion_held"), 1)
	digest := f.status().Hold.Digest

	// Confirming needs an operator, the digest that was reviewed, and a source
	// that has something held.
	_, err := f.svc.ConfirmMassDeletion(bg, f.scope, f.srcID, digest)
	codeOf(t, err, contracts.ErrPermissionDenied)
	f.oper.allow(true)
	_, err = f.svc.ConfirmMassDeletion(bg, f.scope, f.srcID, "0000")
	codeOf(t, err, contracts.ErrConflict)
	if f.status().Hold.Confirmed {
		t.Fatal("a refused confirmation was recorded")
	}
	if len(f.oper.asked) == 0 || f.oper.asked[0] != ActionConfirmMassDeletion {
		t.Fatalf("the operator hook was asked %v", f.oper.asked)
	}

	// The confirmed deletion is applied by the next scan, once.
	res, err := f.svc.ConfirmMassDeletion(bg, f.scope, f.srcID, digest)
	if err != nil {
		t.Fatalf("ConfirmMassDeletion: %v", err)
	}
	if h := f.status().Hold; h == nil || !h.Confirmed || h.ConfirmedBy != "usr-1" {
		t.Fatalf("hold after the confirmation = %+v", h)
	}
	f.drain()
	mustEqual(t, "confirmed sync", f.job(res.Job.ID).State, jobs.StateSucceeded)
	mustEqual(t, "documents searchable", len(f.search("alpha")), 0)
	mustEqual(t, "vectors", f.vectors(), 0)
	mustEqual(t, "tombstones", f.countRows(`SELECT count(*) FROM ragingest_tombstone WHERE reason = 'deleted' AND applied_ns > 0`), 3)
	st = f.status()
	mustEqual(t, "available", st.Available, true)
	if st.Hold != nil {
		t.Fatalf("the hold outlived the deletion: %+v", st.Hold)
	}
	types := f.auditTypes()
	for _, want := range []string{"ingest.mass_deletion_held", "ingest.mass_deletion_confirmed", "ingest.mass_deletion_applied"} {
		mustEqual(t, want, countOf(types, want), 1)
	}

	// A confirmation is single use: the same deletion of the same files is held
	// again.
	corpus(f, 3)
	f.sync()
	mustEqual(t, "documents back", len(docHits(f.search("alpha"))), 3)
	f.emptyTheRoot(t)
	f.sync()
	mustEqual(t, "documents while held", len(docHits(f.search("alpha"))), 3)
	h := f.status().Hold
	if h == nil || h.Confirmed || h.Digest != digest {
		t.Fatalf("the same deletion was not held again: %+v (first digest %s)", h, digest)
	}
}

// emptyTheRoot deletes the fixture's three corpus files, as a share that came
// back empty would look.
func (f *fixture) emptyTheRoot(t *testing.T) {
	t.Helper()
	for i := 0; i < 3; i++ {
		f.remove(fmt.Sprintf("doc%d.md", i))
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestMassDeletionGuardClearsWhenTheSourceReturns(t *testing.T) {
	f := newFixture(t)
	corpus(f, 4)
	f.sync()
	base := f.counters()
	f.unmount()
	f.sync()
	mustEqual(t, "available while unmounted", f.status().Available, false)
	mustEqual(t, "documents searchable while unmounted", len(docHits(f.search("alpha"))), 4)

	f.remount()
	j := f.sync()
	mustEqual(t, "sync after the remount", j.State, jobs.StateSucceeded)
	st := f.status()
	mustEqual(t, "available", st.Available, true)
	if st.Hold != nil || st.Reason != "" || st.State != rag.SourceStatusStateReady {
		t.Fatalf("the source did not recover: %+v", st)
	}
	mustEqual(t, "documents searchable", len(docHits(f.search("alpha"))), 4)
	if f.counters() != base {
		t.Fatalf("the remount cost work: %+v -> %+v", base, f.counters())
	}
	mustEqual(t, "tombstones", f.countRows(`SELECT count(*) FROM ragingest_tombstone`), 0)
}

func TestMassDeletionConfirmationIsBoundToTheSetAndExpires(t *testing.T) {
	f := newFixture(t)
	f.oper.allow(true)
	corpus(f, 12)
	f.sync()

	// Eight of twelve files go: more than half of a source of at least ten.
	for i := 0; i < 8; i++ {
		f.remove(fmt.Sprintf("doc%d.md", i))
	}
	f.sync()
	h := f.status().Hold
	if h == nil || h.Why != WhyMaxFraction || h.Deleted != 8 || h.Previous != 12 {
		t.Fatalf("hold = %+v", h)
	}
	d1 := h.Digest
	if _, err := f.svc.ConfirmMassDeletion(bg, f.scope, f.srcID, d1); err != nil {
		t.Fatal(err)
	}
	f.drain() // the confirmation queued a sync: it applies exactly this deletion
	mustEqual(t, "documents after the confirmed deletion", len(docHits(f.search("alpha"))), 4)

	// The same eight files vanish again and are held and confirmed again; but
	// before the scan that would apply it runs, one of them comes back, so the
	// deletion is another set. The confirmation does not carry over to it.
	corpus(f, 12)
	f.sync()
	mustEqual(t, "documents restored", len(docHits(f.search("alpha"))), 12)
	for i := 0; i < 8; i++ {
		f.remove(fmt.Sprintf("doc%d.md", i))
	}
	f.syncUntilHeld()
	d2 := f.status().Hold.Digest
	if _, err := f.svc.ConfirmMassDeletion(bg, f.scope, f.srcID, d2); err != nil {
		t.Fatal(err)
	}
	f.writeAt("doc0.md", doc("Title 0", para("p0a")), oldTime.Add(2*time.Hour))
	f.drain()
	h = f.status().Hold
	if h == nil || h.Deleted != 7 || h.Digest == d2 || h.Confirmed {
		t.Fatalf("a confirmation for another deletion set let the scan through: %+v", h)
	}
	mustEqual(t, "documents still searchable", len(docHits(f.search("alpha"))), 12)

	// The confirmation of the current set expires.
	d3 := h.Digest
	if _, err := f.svc.ConfirmMassDeletion(bg, f.scope, f.srcID, d3); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(f.cfg.withDefaults().ConfirmTTL + time.Minute)
	// A queued sync that runs after the expiry finds no confirmation.
	f.drain()
	if h := f.status().Hold; h == nil || h.Digest != d3 || h.Confirmed {
		t.Fatalf("an expired confirmation was honoured: %+v", h)
	}
	mustEqual(t, "documents searchable after the expiry", len(docHits(f.search("alpha"))), 12)
}

// syncUntilHeld syncs once and expects the guard to hold the scan.
func (f *fixture) syncUntilHeld() {
	f.t.Helper()
	j := f.sync()
	if j.State != jobs.StateFailed || j.ErrorCode != contracts.ErrUnavailable {
		f.t.Fatalf("the scan was not held: %s %s", j.State, j.ErrorCode)
	}
}

func TestMassDeletionGuardAppliesAnOrdinaryDeletion(t *testing.T) {
	f := newFixture(t)
	corpus(f, 12)
	f.sync()
	// Five of twelve: under the fraction, not the whole source: applied without
	// anybody's confirmation.
	for i := 0; i < 5; i++ {
		f.remove(fmt.Sprintf("doc%d.md", i))
	}
	j := f.sync()
	mustEqual(t, "sync", j.State, jobs.StateSucceeded)
	mustEqual(t, "documents", len(docHits(f.search("alpha"))), 7)
	if f.status().Hold != nil {
		t.Fatal("an ordinary deletion was held")
	}
}

func TestGuardRefusesWithoutAnAuditTrail(t *testing.T) {
	// Fail closed: when the refusal cannot be recorded the scan is still not
	// applied.
	f := newFixture(t)
	corpus(f, 3)
	f.sync()
	f.svc.aud = failingAudit{}
	f.unmount()
	j := f.sync()
	mustEqual(t, "job state", j.State, jobs.StateFailed)
	mustEqual(t, "documents searchable", len(docHits(f.search("alpha"))), 3)
	mustEqual(t, "tombstones", f.countRows(`SELECT count(*) FROM ragingest_tombstone`), 0)
}

// failingAudit is an audit log that cannot append.
type failingAudit struct{}

func (failingAudit) AppendAt(context.Context, audit.Locator, audit.NewEvent) (audit.Event, error) {
	return audit.Event{}, errors.New("the audit log is unavailable")
}

func (failingAudit) AppendTx(context.Context, *store.Tx, audit.Locator, audit.NewEvent) (audit.Event, error) {
	return audit.Event{}, errors.New("the audit log is unavailable")
}
