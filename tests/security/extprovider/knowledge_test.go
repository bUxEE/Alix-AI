package extprovidersec

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/pkg/contracts"
	actgen "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

const sayQuestion = "What are the opening hours of the clinic building?"

// ---- the RAG side: a retriever and a catalogue that answer what the test sets ----

type fakeRetriever struct {
	mu  sync.Mutex
	pkg rag.EvidencePackage
}

func (f *fakeRetriever) Query(context.Context, contracts.Scope, query.Request) (query.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return query.Result{Package: f.pkg}, nil
}

func (f *fakeRetriever) set(p rag.EvidencePackage) { f.mu.Lock(); f.pkg = p; f.mu.Unlock() }

type fakeCatalog struct {
	mu     sync.Mutex
	states map[string]query.SourceState
}

func (c *fakeCatalog) Sources(_ context.Context, sc contracts.Scope, ids []string) (map[string]query.SourceState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]query.SourceState{}
	for _, id := range ids {
		if st, ok := c.states[sc.TenantID+"/"+id]; ok {
			out[id] = st
		}
	}
	return out, nil
}

func (c *fakeCatalog) grant(sc contracts.Scope, pkg rag.EvidencePackage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, it := range pkg.Items {
		c.states[sc.TenantID+"/"+string(it.SourceID)] = query.SourceState{
			DisplayName: it.DisplayName, Authorised: true, Availability: it.Availability, Staleness: it.Staleness,
		}
	}
}

// enableKnowledge gives the engine the external-mode Knowledge over a scripted retriever.
func (w *world) enableKnowledge() {
	w.t.Helper()
	w.retr = &fakeRetriever{}
	w.catalog = &fakeCatalog{states: map[string]query.SourceState{}}
	acfg := answer.DefaultConfig()
	localAns, err := answer.New(answer.Deps{Generator: w.local, Catalog: w.catalog, Clock: w.clk}, acfg)
	if err != nil {
		w.t.Fatal(err)
	}
	extAns, err := answer.New(answer.Deps{Generator: w.rt.ExternalGenerator(), Catalog: w.catalog, Clock: w.clk, Resolver: localAns.Resolver()}, acfg)
	if err != nil {
		w.t.Fatal(err)
	}
	if w.know, err = w.rt.NewKnowledge(w.retr, localAns, extAns); err != nil {
		w.t.Fatal(err)
	}
	w.eng.SetKnowledge(w.know)
}

// newSource registers the policy of a RAG source at the source level and returns its id.
func (w *world) newSource(sc contracts.Scope, mode pv.EgressMode, class pv.DataClassification) pv.SourceID {
	w.t.Helper()
	id := pv.SourceID(ids.New("src"))
	cls := class
	w.putPolicyWith(sc, pv.EgressLevelSource, id, func(in *dataegress.PolicyInput) {
		in.SourceMode, in.SourceClass = mode, &cls
		in.AllowedClasses = []pv.EgressEligibleClass{pv.EgressEligibleClass(class)}
	})
	return id
}

// evItem describes one evidence item of a test package.
type evItem struct {
	source string
	text   string
	id     pv.SourceID
	egress rag.SourceEgress
	class  rag.SourceClassification
}

// pkgFor builds the package the query pipeline would build for the scope.
func pkgFor(sc contracts.Scope, question string, items ...evItem) rag.EvidencePackage {
	out := make([]rag.EvidenceItem, len(items))
	for i, s := range items {
		eg, cl := s.egress, s.class
		sec := "Section " + string(rune('A'+i))
		loc := conversation.CitationLocator{Section: &sec}
		out[i] = rag.EvidenceItem{
			EvidenceID:   rag.EvidenceID("e" + string(rune('1'+i))),
			Rank:         int64(i + 1),
			ChunkID:      common.Identifier("chk_" + string(rune('1'+i))),
			GenerationID: rag.GenerationID(ids.New("gen")),
			SourceID:     actgen.SourceID(s.id),
			DisplayName:  s.source,
			Version:      actgen.OpaqueVersion("v1"),
			Locator:      &loc,
			Availability: conversation.CitationAvailabilityAvailable,
			Staleness:    conversation.CitationStalenessCurrent,
			OpenRef:      conversation.CitationOpenRef{Value: conversation.CitationOpenRefSource{Kind: "source"}},
			Text:         s.text, EstToks: rag.EstToks((len(s.text) + 2) / 3),
			SourceEgress: &eg, SourceClassification: &cl,
		}
	}
	return rag.EvidencePackage{
		PackageID: rag.EvidencePackageID(ids.New("evp")),
		Scope: rag.EvidencePackageScope{
			ApplicationID: common.Identifier(sc.ApplicationID), TenantID: common.Identifier(sc.TenantID),
			WorkspaceID: common.Identifier(sc.WorkspaceID), PrincipalID: common.Identifier(sc.PrincipalID),
		},
		Query:     rag.SearchRequestQuery(question),
		CreatedAt: common.Timestamp("2026-10-12T09:00:00.000Z"),
		Items:     out,
		Budget:    rag.EvidencePackageBudget{MaxItems: 8, MaxToks: 3000, EstToks: 100},
		Retrieval: rag.EvidencePackageRetrieval{Methods: []rag.RetrievalMethod{rag.RetrievalMethodFts}, Notices: rag.RetrievalNotices{}},
	}
}

func (w *world) ask(sc contracts.Scope, pkg rag.EvidencePackage) (rag.AnswerResult, error) {
	w.t.Helper()
	w.retr.set(pkg)
	w.catalog.grant(sc, pkg)
	return w.know.Answer(ctxb(), sc, rag.AnswerRequest{Question: rag.SearchRequestQuery(sayQuestion)})
}

type claimJSON struct {
	Text string   `json:"text"`
	IDs  []string `json:"evidence_ids"`
}

// answerWith scripts the external provider's reply to the answer prompt.
func (w *world) answerWith(claims ...claimJSON) {
	w.t.Helper()
	if claims == nil {
		claims = []claimJSON{}
	}
	w.ext.On(anyPrompt()).ReplyJSON(map[string]any{"claims": claims})
}

func grounded(w *world, r rag.AnswerResult) rag.GroundedAnswer {
	w.t.Helper()
	g, ok := r.Value.(rag.GroundedAnswer)
	if !ok {
		w.t.Fatalf("outcome is %T %+v, want a grounded answer", r.Value, r.Value)
	}
	return g
}

// evidenceRestricted fails unless the outcome is the typed result "the selected provider cannot access the evidence".
func evidenceRestricted(w *world, r rag.AnswerResult) {
	w.t.Helper()
	ns, ok := r.Value.(rag.NoSupportedEvidence)
	if !ok || ns.Reason != rag.NoSupportedEvidenceReasonEgressRestricted {
		w.t.Fatalf("outcome = %T %+v, want no_supported_evidence with reason egress_restricted", r.Value, r.Value)
	}
}

// envelopeJSON is the whole filtered request the provider may receive, as the wire would carry it.
func envelopeJSON(w *world, env *pv.RequestEnvelope) string {
	w.t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		w.t.Fatal(err)
	}
	return string(b)
}
