package ragsec

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/query"
)

// TestHarnessIngestsRetrievesAndAnswers is the control of the whole suite: the
// stack that every attack runs against does ingest, retrieve and cite ordinary
// documents. A security test that passes against a stack that cannot do this
// proves nothing.
func TestHarnessIngestsRetrievesAndAnswers(t *testing.T) {
	w := newWorld(t)
	sc := scopeFor("tenant-a", "ws-1", "usr-alice")
	s := w.addSource(sc, "library")
	s.loadFixtures(legitCorpus...)
	w.syncOK(s)

	if st := w.status(s); st.Counts.Indexed != len(legitCorpus) {
		t.Fatalf("indexed %d documents, want %d\n%s", st.Counts.Indexed, len(legitCorpus), w.dumpJobs())
	}
	a := w.mustAsk(sc, "How long may members borrow a book?")
	if a.QDiag.Mode != query.ModeHybrid {
		t.Errorf("mode = %s (degraded: %v), want hybrid", a.QDiag.Mode, a.QDiag.Degraded)
	}
	ga, ok := a.grounded()
	if !ok {
		t.Fatalf("not a grounded answer: %+v\n%s", a.Answer.Answer.Value, a.evidenceText())
	}
	if !strings.Contains(strings.Join(a.claimTexts(), " "), "21 days") {
		t.Errorf("claims = %q, want one stating 21 days", a.claimTexts())
	}
	if len(ga.Citations) == 0 {
		t.Error("no citation")
	}
}
