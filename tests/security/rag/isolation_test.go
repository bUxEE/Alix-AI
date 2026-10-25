package ragsec

import (
	"slices"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// What each tenant (and the second workspace of tenant A) holds that nobody else
// may see. The two refund policies are look-alikes: same title, same headings,
// same reference identifier; they differ only in these strings.
//
// A "marker" is the identifier a question can be built around, so a probe may
// legitimately type it; a "text" is a sentence or date that no question types, so
// finding it in a prompt can only mean the document text itself went there.
const (
	markerA   = "ALPHA-EXCLUSIVE-4471"
	markerB   = "BRAVO-EXCLUSIVE-9902"
	markerWS2 = "WS2-EXCLUSIVE-5512"
)

var (
	textsA   = []string{"The partner discount code is " + markerA, "31 March 2027", "Refunds are available within 14 days of purchase"}
	textsB   = []string{"The partner discount code is " + markerB, "30 June 2027", "Refunds are available within 30 days of purchase"}
	textsWS2 = []string{"The second workspace code is " + markerWS2, "Refunds are available within 21 days of purchase"}
)

// isolationWorld is two tenants of one application with look-alike corpora, and a
// second workspace of tenant A.
type isolationWorld struct {
	*world
	scopeA, scopeB, scopeA2 contracts.Scope
	srcA, srcB, srcA2       *src
}

func newIsolationWorld(t *testing.T, o ...worldOpt) *isolationWorld {
	t.Helper()
	w := newWorld(t, o...)
	iw := &isolationWorld{
		world:   w,
		scopeA:  scopeFor("tenant-a", "ws-1", "usr-alice"),
		scopeB:  scopeFor("tenant-b", "ws-1", "usr-bob"),
		scopeA2: scopeFor("tenant-a", "ws-2", "usr-alice"),
	}
	iw.srcA = w.addSource(iw.scopeA, "library-a")
	iw.srcA.loadAs("refunds.md", "refund_a.md")
	iw.srcA.loadFixtures(legitCorpus...)
	iw.srcB = w.addSource(iw.scopeB, "library-b")
	iw.srcB.loadAs("refunds.md", "refund_b.md") // the same file name, the same document title
	iw.srcB.loadFixtures(legitCorpus...)
	iw.srcA2 = w.addSource(iw.scopeA2, "library-a-ws2")
	iw.srcA2.writeString("refunds.md", "# Refund policy\n\n## Refunds for event tickets\n\n"+
		textsWS2[1]+". Quote reference POLICY-REF-2026-014 when contacting the box office. "+textsWS2[0]+".\n")
	for _, s := range []*src{iw.srcA, iw.srcB, iw.srcA2} {
		w.syncOK(s)
		if st := w.status(s); st.Counts.Indexed == 0 {
			t.Fatalf("nothing was indexed for %s\n%s", s.name, w.dumpJobs())
		}
	}
	return iw
}

// returned is every string a client could have been shown for the question: the
// evidence items, the answer and its citations, as JSON. It leaves out the
// question itself, which a probe may type on purpose.
func (a asked) returned(t testing.TB) string {
	t.Helper()
	b := a.everything(t)
	if q := string(a.Package.Query); q != "" {
		b = strings.ReplaceAll(b, q, "")
	}
	return b
}

// requireNone fails when any of the strings is in text.
func requireNone(t testing.TB, what, text string, forbidden []string) {
	t.Helper()
	if n, found := containsAny(text, forbidden...); found {
		t.Errorf("%s holds %q, which belongs to someone else", what, n)
	}
}

func sameScope(a, b contracts.Scope) bool {
	return a.DeploymentID == b.DeploymentID && a.ApplicationID == b.ApplicationID && a.TenantID == b.TenantID &&
		a.WorkspaceID == b.WorkspaceID && a.PrincipalID == b.PrincipalID && a.SessionID == b.SessionID &&
		slices.Equal(a.Roles, b.Roles) && slices.Equal(a.CapabilityScopes, b.CapabilityScopes)
}

// promptsIn are the captured calls made under exactly this tenant and workspace.
func (w *world) promptsIn(scope contracts.Scope) []fake.Prompt {
	var out []fake.Prompt
	for _, p := range w.fp.Prompts() {
		if p.Scope.TenantID == scope.TenantID && p.Scope.WorkspaceID == scope.WorkspaceID {
			out = append(out, p)
		}
	}
	return out
}

func anyPromptContains(ps []fake.Prompt, sub string) bool {
	for _, p := range ps {
		if p.Contains(sub) {
			return true
		}
	}
	return false
}

// requireNoForeignText fails when a call made under the scope's tenant and
// workspace sent any of the strings that belong to someone else.
func (w *world) requireNoForeignText(scope contracts.Scope, foreign ...[]string) {
	w.t.Helper()
	calls := w.promptsIn(scope)
	if len(calls) == 0 {
		w.t.Errorf("no call was made under %s/%s: the check has nothing to look at", scope.TenantID, scope.WorkspaceID)
	}
	for _, p := range calls {
		for _, list := range foreign {
			for _, f := range list {
				if p.Contains(f) {
					w.t.Errorf("call #%d (%s) under %s/%s sent %q, which belongs to someone else", p.Seq, p.Op, scope.TenantID, scope.WorkspaceID, f)
				}
			}
		}
	}
}

// SEC-018: content planted in one tenant's corpus (look-alike documents, same
// titles, same identifiers) is returned to another tenant. Two tenants hold
// near-identical documents that differ in a marker string; the other tenant's
// marker never appears in a result, a citation or a captured prompt, in FTS-only
// and in hybrid mode, with the catalog doing its job and with the catalog
// deliberately over-granting (so that what holds is the index and the vector
// store, not the catalog).
func TestSecRAGCrossTenantZeroLeak(t *testing.T) {
	modes := []struct {
		name string
		opts []worldOpt
		mode query.Mode
	}{
		{"hybrid", nil, query.ModeHybrid},
		{"fts_only", []worldOpt{ftsOnly()}, query.ModeFTSOnly},
		{"hybrid with every model role", []worldOpt{withModelRoles()}, query.ModeHybrid},
		{"hybrid with an over-granting catalog", []worldOpt{overGranting()}, query.ModeHybrid},
		{"fts_only with an over-granting catalog", []worldOpt{overGranting(), ftsOnly()}, query.ModeFTSOnly},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			iw := newIsolationWorld(t, m.opts...)
			// A model that states every sentence it is given: whatever reaches it can
			// reach an answer, so only isolation upstream of it keeps text out.
			iw.model.setScript(obedient())

			everyoneElseA := slices.Concat(textsB, textsWS2, []string{markerB, markerWS2})
			everyoneElseB := slices.Concat(textsA, textsWS2, []string{markerA, markerWS2})
			type probe struct {
				name      string
				scope     contracts.Scope
				question  string
				sources   []string
				own       string   // the caller's own text the question is built to find; "" when nothing may be found
				forbidden []string // strings of someone else
				foreign   []string // ids of sources the caller may not read
			}
			probes := []probe{
				{"A: look-alike policy by title and shared identifier", iw.scopeA, "refund policy reference POLICY-REF-2026-014 partner discount code event tickets",
					nil, markerA, everyoneElseA, []string{iw.srcB.id, iw.srcA2.id}},
				{"B: look-alike policy by title and shared identifier", iw.scopeB, "refund policy reference POLICY-REF-2026-014 partner discount code event tickets",
					nil, markerB, everyoneElseB, []string{iw.srcA.id, iw.srcA2.id}},
				{"A: exact identifier of the other tenant", iw.scopeA, markerB, nil, "", everyoneElseA, []string{iw.srcB.id}},
				{"B: exact identifier of the other tenant", iw.scopeB, markerA, nil, "", everyoneElseB, []string{iw.srcA.id}},
				{"A: identifier of its own other workspace", iw.scopeA, markerWS2, nil, "", everyoneElseA, []string{iw.srcA2.id}},
				{"A: the other tenant's source named in the request", iw.scopeA, "refund policy partner discount code expires",
					[]string{iw.srcB.id}, "", everyoneElseA, []string{iw.srcB.id}},
				{"A: the other workspace's source named in the request", iw.scopeA, "refund policy reference POLICY-REF-2026-014",
					[]string{iw.srcA2.id}, "", everyoneElseA, []string{iw.srcA2.id}},
				{"A: the wording of the other tenant's policy", iw.scopeA, "partner discount code expires refunds available within days of purchase", nil, markerA, everyoneElseA, []string{iw.srcB.id, iw.srcA2.id}},
			}
			var asks []asked
			for _, p := range probes {
				a := iw.mustAsk(p.scope, p.question, p.sources...)
				asks = append(asks, a)
				what := p.name
				requireNone(t, what+": the evidence, answer and citations", a.returned(t), p.forbidden)
				for _, it := range a.Package.Items {
					if slices.Contains(p.foreign, string(it.SourceID)) {
						t.Errorf("%s: an evidence item comes from source %s, which the caller may not read", what, it.SourceID)
					}
				}
				for _, id := range a.citedSources() {
					if slices.Contains(p.foreign, id) {
						t.Errorf("%s: a citation names source %s, which the caller may not read", what, id)
					}
				}
				if p.own != "" {
					// The positive control: the caller's own look-alike is found, so the
					// absence of the other one is isolation and not an empty result.
					if !strings.Contains(a.evidenceText(), p.own) {
						t.Errorf("%s: the caller's own text %q was not retrieved\n%s", what, p.own, a.evidenceText())
					}
					if _, ok := a.grounded(); !ok {
						t.Errorf("%s: no grounded answer for the caller's own corpus: %+v", what, a.Answer.Answer.Value)
					}
					if a.QDiag.Mode != m.mode {
						t.Errorf("%s: mode = %s (degraded %v), want %s", what, a.QDiag.Mode, a.QDiag.Degraded, m.mode)
					}
					if m.mode == query.ModeFTSOnly {
						if !slices.Contains(a.Package.Retrieval.Notices, rag.RetrievalNoticeReducedSemanticRecall) {
							t.Errorf("%s: an FTS-only package is not labelled reduced_semantic_recall: %v", what, a.Package.Retrieval.Notices)
						}
					} else if a.QDiag.VectorCandidates == 0 {
						t.Errorf("%s: the vector stage returned no candidate, so this run does not exercise vector isolation (degraded %v)", what, a.QDiag.Degraded)
					}
				}
			}

			// The reference identifier is held by every tenant, and each caller sees
			// one holder of it: its own.
			ownSource := map[string]string{"tenant-a": iw.srcA.id, "tenant-b": iw.srcB.id}
			for i, p := range probes[:2] {
				holders := asks[i].sourcesInPackage()
				if !holders[ownSource[p.scope.TenantID]] {
					t.Errorf("%s: the evidence does not come from the caller's own source: %v", p.name, holders)
				}
				for id := range holders {
					if id != ownSource[p.scope.TenantID] {
						t.Errorf("%s: the evidence also comes from source %s", p.name, id)
					}
				}
			}

			// Every prompt of every kind (ingestion embeddings, query embeddings,
			// reranking, rewriting, answering) was sent under the scope of the tenant
			// whose text it carries, and carries no text of anyone else. The markers
			// themselves are left out here only because a probe typed them as the
			// question; the sentences they sit in were never typed.
			iw.requireNoForeignText(iw.scopeA, textsB, textsWS2)
			iw.requireNoForeignText(iw.scopeB, textsA, textsWS2)
			iw.requireNoForeignText(iw.scopeA2, textsA, textsB)
			// Positive controls: the prompts do carry each tenant's own text.
			if !anyPromptContains(iw.promptsIn(iw.scopeA), textsA[0]) {
				t.Error("no prompt under tenant A carries tenant A's own text: the captured record is empty or the corpus was not embedded")
			}
			if !anyPromptContains(iw.promptsIn(iw.scopeB), textsB[0]) {
				t.Error("no prompt under tenant B carries tenant B's own text")
			}
			if !anyPromptContains(iw.promptsIn(iw.scopeA2), textsWS2[0]) {
				t.Error("no prompt under tenant A's second workspace carries its own text")
			}
			// The answer prompts of each caller ran under that caller's scope.
			for i, p := range probes {
				if asks[i].Answer.Diagnostics.Generated {
					found := false
					for _, pr := range iw.answerPrompts() {
						found = found || sameScope(pr.Scope, p.scope)
					}
					if !found {
						t.Errorf("%s: no answer prompt ran under the caller's scope", p.name)
					}
				}
			}

			// Every scope any collaborator saw is one of the three callers', never a
			// blend and never a widened one.
			valid := []contracts.Scope{iw.scopeA, iw.scopeB, iw.scopeA2}
			if len(iw.log.all()) == 0 {
				t.Error("no collaborator call was recorded")
			}
			for _, e := range iw.log.all() {
				ok := false
				for _, v := range valid {
					ok = ok || sameScope(e.Scope, v)
				}
				if !ok {
					t.Errorf("%s was called with scope %+v, which is none of the verified scopes", e.Where, e.Scope)
				}
			}
		})
	}
}

// SEC-018, SEC-021 (end to end): vectors of another tenant's generation are not
// candidates for this tenant even when the caller names the generation, and the
// lexical index does not return the other tenant's chunks for its exact
// identifier. This is the isolation above, read at the stores directly.
func TestSecRAGCrossTenantStoresReturnNoForeignRow(t *testing.T) {
	iw := newIsolationWorld(t)
	genOf := func(s *src) string {
		g, ok, err := iw.ix.ActiveGeneration(bg, s.scope, s.id)
		if err != nil || !ok {
			t.Fatalf("no active generation for %s: %v", s.name, err)
		}
		return string(g.GenerationID)
	}
	genA, genB := genOf(iw.srcA), genOf(iw.srcB)

	// Tenant B's own sentence as the query vector: the nearest vectors in the whole
	// database are B's chunks.
	qv := fake.EmbedVector("The partner discount code is "+markerB+" and it expires on 30 June 2027.", fake.DefaultEmbedDim)
	own, err := iw.flat.Search(bg, iw.scopeB, vector.Query{Vector: qv, Model: embedModel, K: 20, Generations: []string{genB}})
	if err != nil || len(own.Hits) == 0 {
		t.Fatalf("control: tenant B's own vector search returned %d hits: %v", len(own.Hits), err)
	}
	// Tenant A asks with the same vector and names B's generation as well as its own.
	res, err := iw.flat.Search(bg, iw.scopeA, vector.Query{Vector: qv, Model: embedModel, K: 20, Generations: []string{genB, genA}})
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("control: tenant A's own generation returned nothing, so the check below proves nothing")
	}
	foreignChunk := map[string]bool{}
	for _, h := range own.Hits {
		foreignChunk[h.ChunkID] = true
	}
	for _, h := range res.Hits {
		if h.SourceID == iw.srcB.id || h.GenerationID == genB || foreignChunk[h.ChunkID] {
			t.Errorf("a vector of tenant B's source (chunk %s) is a candidate for tenant A", h.ChunkID)
		}
	}
	skipped := false
	for _, s := range res.Skipped {
		skipped = skipped || s.Generation == genB
	}
	if !skipped {
		t.Errorf("tenant B's generation is not reported as skipped: %+v", res.Skipped)
	}

	// The lexical index: B's identifier is found for B, and never B's rows for A,
	// hidden text included.
	if hits := iw.search(iw.scopeB, markerB, true); len(hits) == 0 {
		t.Fatal("control: tenant B cannot find its own identifier")
	}
	for _, include := range []bool{false, true} {
		for _, h := range iw.search(iw.scopeA, markerB, include) {
			if h.SourceID != iw.srcA.id || strings.Contains(h.Text, markerB) {
				t.Errorf("the lexical index returned a row of tenant B to tenant A (hidden included: %v): %q", include, h.Text)
			}
		}
	}
	for _, h := range iw.search(iw.scopeA, "refund policy POLICY-REF-2026-014", false) {
		if h.SourceID != iw.srcA.id {
			t.Errorf("tenant A's search returned a chunk of source %s", h.SourceID)
		}
	}
}

// SEC-018 (negative control): the observation channels of the isolation tests see a leak.
//
// The negative control of the isolation tests: when one tenant really does hold
// both look-alike documents (the leak the tests above rule out is planted by
// registering tenant B's corpus under tenant A's scope), the very observation
// channels they read (the returned evidence, the answer, the captured prompts)
// do show the other policy's text. An isolation test whose probes could not see
// a leak would pass for the wrong reason.
func TestSecRAGLeakDetectorsSeeAPlantedLeak(t *testing.T) {
	w := newWorld(t)
	scopeA := scopeFor("tenant-a", "ws-1", "usr-alice")
	merged := w.addSource(scopeA, "merged")
	merged.loadAs("refunds-a.md", "refund_a.md")
	merged.loadAs("refunds-b.md", "refund_b.md")
	w.syncOK(merged)
	w.model.setScript(obedient())

	a := w.mustAsk(scopeA, "refund policy reference POLICY-REF-2026-014 partner discount code event tickets")
	got := a.returned(t)
	for _, want := range []string{markerA, markerB} {
		if !strings.Contains(got, want) {
			t.Errorf("the returned evidence and answer lack %q although the tenant holds it: the leak check cannot see it", want)
		}
	}
	if !anyPromptContains(w.promptsIn(scopeA), textsB[0]) {
		t.Error("the captured prompts lack a sentence the tenant holds: the prompt check cannot see a leak")
	}
	if !anyPromptContains(w.answerPrompts(), markerB) {
		t.Error("the answer prompt lacks a marker that was in the evidence")
	}
}
