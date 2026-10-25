package ragsec

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

const qLoanPeriod = "How long may members borrow a book?"

// polled declares a source that is rescanned every hour: the only kind whose
// indexed version can be called current, and only for as long as its last
// successful scan is recent.
func polled(d *ap.RAGSource) {
	hour := int64(3600)
	d.Sync = ap.RAGSourceSync{Method: ap.RAGSourceSyncMethodPoll, IntervalSeconds: &hour}
}

// citationStates are the availability and staleness each citation of the answer
// carries.
func (a asked) citationStates(t testing.TB) (avail []conversation.CitationAvailability, stale []conversation.CitationStaleness) {
	t.Helper()
	ga, ok := a.grounded()
	if !ok {
		t.Fatalf("the answer is not grounded: %+v", a.Answer.Answer.Value)
	}
	if len(ga.Citations) == 0 {
		t.Fatal("the answer has no citation")
	}
	for _, c := range ga.Citations {
		avail = append(avail, c.Availability)
		stale = append(stale, c.Staleness)
	}
	return avail, stale
}

// requireCitations fails unless every citation carries exactly these states.
func (a asked) requireCitations(t testing.TB, what string, availability conversation.CitationAvailability, staleness conversation.CitationStaleness) {
	t.Helper()
	avail, stale := a.citationStates(t)
	for i := range avail {
		if avail[i] != availability || stale[i] != staleness {
			t.Errorf("%s: citation %d is %s / %s, want %s / %s", what, i, avail[i], stale[i], availability, staleness)
		}
	}
}

// citedVersions are the versions (content hashes) the citations name.
func (a asked) citedVersions(t testing.TB) []string {
	t.Helper()
	ga, ok := a.grounded()
	if !ok {
		t.Fatalf("the answer is not grounded: %+v", a.Answer.Answer.Value)
	}
	var out []string
	for _, c := range ga.Citations {
		out = append(out, string(c.Version))
	}
	return out
}

// SEC-017: a stale, deleted or unavailable source is presented as current, or an
// unreachable source is reported as checked. Sources that changed after indexing,
// whose last successful scan is old, whose root became unreachable, whose files
// were deleted or that were removed are labelled stale or unavailable in the
// citation, or are gone from the evidence; and a source about which the catalog
// says nothing is labelled unavailable and unknown, never available and current.
func TestSecRAGStaleSourceNotPresentedAsCurrent(t *testing.T) {
	sc := scopeFor("tenant-a", "ws-1", "usr-alice")
	newLibrary := func(t *testing.T, mutate ...func(*ap.RAGSource)) (*world, *src) {
		w := newWorld(t, ftsOnly())
		s := w.addSource(sc, "library", mutate...)
		s.loadFixtures(legitCorpus...)
		w.syncOK(s)
		return w, s
	}
	const oldText, newText = "21 days", "28 days"

	t.Run("a source that changed, then outlived its schedule, is labelled stale", func(t *testing.T) {
		w, s := newLibrary(t, polled)

		// Control: right after a successful scan the source is current.
		a := w.mustAsk(sc, qLoanPeriod)
		a.requireCitations(t, "just after the scan", conversation.CitationAvailabilityAvailable, conversation.CitationStalenessCurrent)
		if !strings.Contains(strings.Join(a.claimTexts(), " "), oldText) {
			t.Fatalf("control: the answer does not state the indexed fact: %q", a.claimTexts())
		}
		firstVersion := a.citedVersions(t)

		// The file changes on disk and nobody rescans; two scan intervals pass.
		s.edit("loans.md", strings.ReplaceAll(string(fixture(t, "loans.md")), oldText, newText))
		w.clk.Advance(3 * time.Hour)
		a = w.mustAsk(sc, qLoanPeriod)
		a.requireCitations(t, "after the schedule was missed", conversation.CitationAvailabilityAvailable, conversation.CitationStalenessStale)
		if got := strings.Join(a.claimTexts(), " "); !strings.Contains(got, oldText) || strings.Contains(got, newText) {
			t.Errorf("before the rescan the answer is built from the indexed version: %q", got)
		}

		// The rescan replaces the old text: the new fact is current, the old one is
		// gone, and the citation names the new version.
		w.syncOK(s)
		a = w.mustAsk(sc, qLoanPeriod)
		a.requireCitations(t, "after the rescan", conversation.CitationAvailabilityAvailable, conversation.CitationStalenessCurrent)
		got := strings.Join(a.claimTexts(), " ")
		if !strings.Contains(got, newText) || strings.Contains(got, oldText) {
			t.Errorf("after the rescan the answer is %q, want the new fact only", got)
		}
		if strings.Contains(a.evidenceText(), oldText) {
			t.Errorf("the superseded text is still in the evidence:\n%s", a.evidenceText())
		}
		if v := a.citedVersions(t); len(v) == 0 || v[0] == firstVersion[0] {
			t.Errorf("the citation still names the superseded version %v (now %v)", firstVersion, v)
		}
	})

	t.Run("a package built while current is labelled stale when the answer is written later", func(t *testing.T) {
		w, _ := newLibrary(t, polled)
		w.model.setScript(faithful())
		res, err := w.pipe.Query(bg, sc, query.Request{Text: qLoanPeriod})
		if err != nil || len(res.Package.Items) == 0 {
			t.Fatalf("Query: %d items, %v", len(res.Package.Items), err)
		}
		for _, it := range res.Package.Items {
			if it.Staleness != conversation.CitationStalenessCurrent || it.Availability != conversation.CitationAvailabilityAvailable {
				t.Fatalf("control: the package item is %s / %s, want current / available", it.Availability, it.Staleness)
			}
		}
		w.clk.Advance(3 * time.Hour) // the schedule is missed between retrieval and the final recheck
		ans, err := w.ans.Answer(bg, sc, res.Package)
		if err != nil {
			t.Fatalf("Answer: %v", err)
		}
		asked{Answer: ans}.requireCitations(t, "the more cautious of the package's state and the catalog's", conversation.CitationAvailabilityAvailable, conversation.CitationStalenessStale)
	})

	t.Run("a source with no schedule is never labelled current", func(t *testing.T) {
		w, _ := newLibrary(t) // manual sync: nothing says when it will be looked at again
		a := w.mustAsk(sc, qLoanPeriod)
		avail, stale := a.citationStates(t)
		for i := range avail {
			if stale[i] == conversation.CitationStalenessCurrent {
				t.Errorf("citation %d of a manually synced source is labelled current", i)
			}
			if avail[i] != conversation.CitationAvailabilityAvailable {
				t.Errorf("citation %d: availability %s after a successful scan", i, avail[i])
			}
		}
	})

	t.Run("an unreachable root is reported as unavailable, never as checked", func(t *testing.T) {
		w, s := newLibrary(t, polled)
		before := w.status(s)
		if !before.Available || before.LastSuccess.IsZero() {
			t.Fatalf("control: %+v", before)
		}

		gone := s.root + ".gone"
		if err := os.Rename(s.root, gone); err != nil {
			t.Fatal(err)
		}
		w.clk.Advance(30 * time.Minute)
		w.sync(s) // the scan cannot reach the root

		st := w.status(s)
		if st.Available || st.Reason != ingest.ReasonRootUnreachable {
			t.Errorf("status = available %v, reason %q, want unavailable / %s", st.Available, st.Reason, ingest.ReasonRootUnreachable)
		}
		if !st.LastSuccess.Equal(before.LastSuccess) {
			t.Errorf("last success moved from %s to %s: a scan that reached nothing was recorded as a check", before.LastSuccess, st.LastSuccess)
		}
		if !st.LastAttempt.After(before.LastAttempt) {
			t.Errorf("the attempt was not recorded: %s -> %s", before.LastAttempt, st.LastAttempt)
		}
		if st.Counts.Indexed != before.Counts.Indexed {
			t.Errorf("indexed %d -> %d: an unreachable root must not delete what was indexed", before.Counts.Indexed, st.Counts.Indexed)
		}
		a := w.mustAsk(sc, qLoanPeriod)
		avail, stale := a.citationStates(t)
		for i := range avail {
			if avail[i] != conversation.CitationAvailabilityUnavailable {
				t.Errorf("citation %d is %s while the source cannot be reached", i, avail[i])
			}
			if stale[i] == conversation.CitationStalenessCurrent {
				t.Errorf("citation %d is labelled current while the source cannot be reached", i)
			}
		}
		if len(a.Package.Items) == 0 || a.Package.Items[0].Availability != conversation.CitationAvailabilityUnavailable {
			t.Errorf("the package item is not labelled unavailable: %+v", a.Package.Items)
		}

		// A remount ends it.
		if err := os.Rename(gone, s.root); err != nil {
			t.Fatal(err)
		}
		w.clk.Advance(time.Minute)
		w.syncOK(s)
		after := w.status(s)
		if !after.Available || !after.LastSuccess.After(before.LastSuccess) {
			t.Errorf("after the remount: %+v", after)
		}
		w.mustAsk(sc, qLoanPeriod).requireCitations(t, "after the remount", conversation.CitationAvailabilityAvailable, conversation.CitationStalenessCurrent)
	})

	t.Run("a deleted file stops being presented", func(t *testing.T) {
		w, s := newLibrary(t, polled)
		const hours = "09:00 to 18:00"
		a := w.mustAsk(sc, qWeekdayHours)
		if !strings.Contains(a.evidenceText(), hours) {
			t.Fatalf("control: the opening hours are not retrieved: %q", a.evidenceText())
		}
		if err := os.Remove(s.root + "/hours.md"); err != nil {
			t.Fatal(err)
		}
		w.clk.Advance(time.Minute)
		w.syncOK(s)
		a = w.mustAsk(sc, qWeekdayHours)
		if strings.Contains(a.returned(t), hours) {
			t.Errorf("the deleted file's text is still presented: %q", a.returned(t))
		}
		if ns, ok := a.noEvidence(); !ok || ns.Reason != rag.NoSupportedEvidenceReasonNoMatch {
			t.Errorf("after the deletion: %+v, want no_supported_evidence (no_match)", a.Answer.Answer.Value)
		}
	})

	t.Run("a removed source is gone from the evidence", func(t *testing.T) {
		w, s := newLibrary(t, polled)
		w.mustAsk(sc, qLoanPeriod).requireCitations(t, "control", conversation.CitationAvailabilityAvailable, conversation.CitationStalenessCurrent)
		if err := w.svc.RemoveSource(bg, sc, s.id); err != nil {
			t.Fatalf("RemoveSource: %v", err)
		}
		a := w.mustAsk(sc, qLoanPeriod)
		if len(a.Package.Items) != 0 || strings.Contains(a.returned(t), oldText) {
			t.Errorf("a removed source's text is still in the evidence: %q", a.evidenceText())
		}
		if _, ok := a.noEvidence(); !ok {
			t.Errorf("a removed source still answers: %+v", a.Answer.Answer.Value)
		}
	})

	t.Run("a catalog that says nothing about a source reads as unavailable and unknown", func(t *testing.T) {
		w, s := newLibrary(t, polled)
		w.cat.force(s, func(query.SourceState) query.SourceState {
			return query.SourceState{DisplayName: "library", Authorised: true} // no availability, no staleness
		})
		a := w.mustAsk(sc, qLoanPeriod)
		a.requireCitations(t, "an unstated state", conversation.CitationAvailabilityUnavailable, conversation.CitationStalenessUnknown)
	})

	t.Run("a catalog that cannot answer is an error, never an empty or a checked result", func(t *testing.T) {
		w, _ := newLibrary(t, polled)
		w.model.setScript(faithful())
		boom := contracts.New(contracts.ErrUnavailable, "the source registry is down")

		// Before retrieval: the query fails closed.
		w.cat.fail(boom)
		a := w.ask(sc, qLoanPeriod)
		if a.QErr == nil || contracts.CodeOf(a.QErr) != contracts.ErrUnavailable || len(a.Package.Items) != 0 {
			t.Errorf("Query with a failing catalog: %d items, %v, want an unavailable error", len(a.Package.Items), a.QErr)
		}
		w.cat.fail(nil)

		// Between retrieval and the final recheck: the answer fails closed.
		res, err := w.pipe.Query(bg, sc, query.Request{Text: qLoanPeriod})
		if err != nil || len(res.Package.Items) == 0 {
			t.Fatalf("control: Query = %d items, %v", len(res.Package.Items), err)
		}
		w.cat.fail(boom)
		ans, err := w.ans.Answer(bg, sc, res.Package)
		if err == nil || contracts.CodeOf(err) != contracts.ErrUnavailable || ans.Answer.Value != nil {
			t.Errorf("Answer with a failing catalog: %+v, %v, want an unavailable error and no answer", ans.Answer.Value, err)
		}
	})
}
