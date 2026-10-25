package capabilities

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// conceptVectors is a scripted stand-in for an embedding model: it places a text
// on four axes by the concepts it mentions, so the tests control exactly which
// capability is "semantically close" to which request. It is not a model and
// proves nothing about embedding quality; it proves what the router does with
// similarities it is handed.
func conceptVectors(req providers.EmbedRequest) ([][]float32, error) {
	out := make([][]float32, len(req.Inputs))
	for i, in := range req.Inputs {
		s := strings.ToLower(in)
		v := []float32{0, 0, 0, 0.1} // a small constant so no vector has zero length
		for axis, words := range [][]string{
			{"pickup", "aside", "reserve", "put by"},
			{"due date", "extend", "borrowed", "longer"},
			{"catalogue", "search", "look for"},
		} {
			for _, w := range words {
				if strings.Contains(s, w) {
					v[axis] = 1
					break
				}
			}
		}
		out[i] = v
	}
	return out, nil
}

func newConceptEmbedder() *fake.Provider { return fake.New(fake.WithEmbedFunc(conceptVectors)) }

func embedCalls(p *fake.Provider, purpose providers.EmbedPurpose) int {
	n := 0
	for _, pr := range p.Prompts() {
		if pr.Op == fake.OpEmbed && pr.Embed.Purpose == purpose {
			n++
		}
	}
	return n
}

// TestEmbeddingFillsWhatRulesMiss: when no rule fires, the embedder's similarity
// can put a visible capability on the shortlist, and says so in the signal.
func TestEmbeddingFillsWhatRulesMiss(t *testing.T) {
	emb := newConceptEmbedder()
	rt, _ := newRouter(t, []RouterOption{WithEmbedder(emb)}, demoPackage(t))
	member := memberScope()

	text := "could you put by a copy for me" // no term, no title word
	if rules := mustRoute(t, mustRouter(t, nil), reqFor(member, text)); rules.Outcome != OutcomeOutOfScope {
		t.Fatalf("the rules alone should find nothing in %q, got %v", text, rules.IDs())
	}
	res := mustRoute(t, rt, reqFor(member, text))
	if res.Embedding != EmbeddingUsed || !equalStrings(res.IDs(), []string{capReserve}) {
		t.Fatalf("got %s %v embedding=%s, want [%s] by embedding", res.Outcome, res.IDs(), res.Embedding, capReserve)
	}
	s, ok := res.Candidates[0].signal(KindEmbedding)
	if !ok || s.Similarity < 0.9 || s.Weight <= 0 || s.Weight > embedWeight {
		t.Fatalf("embedding signal = %+v", s)
	}
}

// TestEmbeddingAddsToAmbiguousRules: when two capabilities tie on the rules, the
// embedding similarity breaks the tie, and the rule evidence is kept.
func TestEmbeddingAddsToAmbiguousRules(t *testing.T) {
	emb := newConceptEmbedder()
	rt, _ := newRouter(t, []RouterOption{WithEmbedder(emb)}, demoPackage(t))
	// "book" is carried by find_title and reservations.create alike. Without an
	// embedder find_title wins the tie by ID; the request also says "put by".
	text := "put by a book"
	base := mustRoute(t, mustRouter(t, nil), reqFor(memberScope(), text, inLanguage("en")))
	if len(base.Candidates) != 2 || base.Candidates[0].CapabilityID != capFind {
		t.Fatalf("rules alone: %v", base.IDs())
	}
	res := mustRoute(t, rt, reqFor(memberScope(), text, inLanguage("en")))
	if res.Embedding != EmbeddingUsed || res.Candidates[0].CapabilityID != capReserve {
		t.Fatalf("with embedding: %v (%s), want %s first", res.IDs(), res.Embedding, capReserve)
	}
	top := res.Candidates[0]
	if _, ok := top.signal(KindTerminology); !ok {
		t.Errorf("the rule evidence was dropped: %+v", top.Signals)
	}
}

// TestEmbeddingSkippedWhenRulesAreDecisive: a request the rules settle never
// costs an embedding call.
func TestEmbeddingSkippedWhenRulesAreDecisive(t *testing.T) {
	emb := newConceptEmbedder()
	rt, _ := newRouter(t, []RouterOption{WithEmbedder(emb)}, demoPackage(t))
	res := mustRoute(t, rt, reqFor(memberScope(), "I want to renew my loan", inLanguage("en")))
	if res.Embedding != EmbeddingSkipped || !res.Decisive || res.Candidates[0].CapabilityID != capExtend {
		t.Fatalf("got %v embedding=%s decisive=%v", res.IDs(), res.Embedding, res.Decisive)
	}
	if emb.Calls() != 0 {
		t.Fatalf("the embedder was called %d times for a request the rules settled", emb.Calls())
	}
	// A continuation of the active goal is decisive too: a slot answer is not
	// sent to an embedder.
	res = mustRoute(t, rt, reqFor(memberScope(), "next friday", withGoal(capReserve, PhaseOpen)))
	if res.Embedding != EmbeddingSkipped || emb.Calls() != 0 {
		t.Fatalf("goal continuation: embedding=%s calls=%d", res.Embedding, emb.Calls())
	}
}

// TestEmbeddingNeverSeesInvisibleCapabilities: only the visible capabilities'
// descriptors reach the embedder (which may be an external provider), and an
// embedder that prefers a hidden capability cannot expose it.
func TestEmbeddingNeverSeesInvisibleCapabilities(t *testing.T) {
	// Every request, whatever it says, is "closest" to the reservation axis.
	emb := fake.New(fake.WithEmbedFunc(func(req providers.EmbedRequest) ([][]float32, error) {
		out := make([][]float32, len(req.Inputs))
		for i, in := range req.Inputs {
			v := []float32{0, 0, 0, 0.1}
			if strings.Contains(strings.ToLower(in), "reserve") || req.Purpose == providers.EmbedQuery {
				v[0] = 1
			}
			out[i] = v
		}
		return out, nil
	}))
	rt, _ := newRouter(t, []RouterOption{WithEmbedder(emb)}, demoPackage(t))
	visitor := scopeFor(appLib, "visitor", nil)

	res := mustRoute(t, rt, reqFor(visitor, "something unrelated entirely"))
	if res.has(capReserve) || res.has(capExtend) || res.has(capDelete) || res.has(capReindex) {
		t.Fatalf("the embedder exposed a hidden capability: %v", res.IDs())
	}
	var texts []string
	for _, p := range emb.Prompts() {
		texts = append(texts, p.Texts()...)
	}
	all := strings.Join(texts, "\n")
	for _, hidden := range []string{"Reserve a title", "Extend a loan", "Delete a membership", "Reindex the catalogue", "erase the member"} {
		if strings.Contains(all, hidden) {
			t.Errorf("the embedder was sent %q, text of a capability the caller cannot see", hidden)
		}
	}
	if !strings.Contains(all, "Find a title") {
		t.Errorf("the visible capability's descriptor was not embedded: %q", all)
	}
	if strings.Contains(all, "zzzexample") {
		t.Errorf("a test-only example reached the embedder: %q", all)
	}
}

// TestEmbeddingCachesDescriptors: capability descriptors are embedded once per
// package, language and model, so a turn costs one embedding call.
func TestEmbeddingCachesDescriptors(t *testing.T) {
	emb := newConceptEmbedder()
	rt, _ := newRouter(t, []RouterOption{WithEmbedder(emb)}, demoPackage(t))
	member := memberScope()
	text := "could you put by a copy for me"

	for i := 0; i < 3; i++ {
		mustRoute(t, rt, reqFor(member, text))
	}
	if q, d := embedCalls(emb, providers.EmbedQuery), embedCalls(emb, providers.EmbedDocument); q != 3 || d != 1 {
		t.Fatalf("query calls = %d, document calls = %d, want 3 and 1", q, d)
	}
	// A different caller sees a different set: only what is missing is embedded.
	mustRoute(t, rt, reqFor(scopeFor(appLib, "visitor", nil), text))
	if d := embedCalls(emb, providers.EmbedDocument); d != 1 {
		t.Fatalf("document calls = %d after a caller who sees a subset, want still 1", d)
	}
	// A new language has its own descriptors.
	mustRoute(t, rt, reqFor(member, text, inLanguage("it")))
	if d := embedCalls(emb, providers.EmbedDocument); d != 2 {
		t.Fatalf("document calls = %d after another language, want 2", d)
	}
}

// switchingEmbedder reports whatever model name it is told to.
type switchingEmbedder struct {
	inner *fake.Provider
	model atomic.Value
}

func (s *switchingEmbedder) Embed(ctx context.Context, sc contracts.Scope, r providers.EmbedRequest) (providers.EmbedResult, error) {
	res, err := s.inner.Embed(ctx, sc, r)
	res.Model = s.model.Load().(string)
	return res, err
}

// TestEmbeddingModelChangeInvalidatesCache: vectors of one model are never
// compared with a query of another.
func TestEmbeddingModelChangeInvalidatesCache(t *testing.T) {
	sw := &switchingEmbedder{inner: newConceptEmbedder()}
	sw.model.Store("model-a")
	rt, _ := newRouter(t, []RouterOption{WithEmbedder(sw)}, demoPackage(t))
	text := "could you put by a copy for me"
	mustRoute(t, rt, reqFor(memberScope(), text))
	mustRoute(t, rt, reqFor(memberScope(), text))
	if d := embedCalls(sw.inner, providers.EmbedDocument); d != 1 {
		t.Fatalf("document calls = %d, want 1 while the model is unchanged", d)
	}
	sw.model.Store("model-b")
	res := mustRoute(t, rt, reqFor(memberScope(), text))
	if d := embedCalls(sw.inner, providers.EmbedDocument); d != 2 || res.Embedding != EmbeddingUsed {
		t.Fatalf("document calls = %d embedding=%s, want the descriptors re-embedded for the new model", d, res.Embedding)
	}
}

// TestEmbeddingInconsistentModelsFailClosedToRules: if the query and the
// descriptors come from different models, the result is the rules-only result.
func TestEmbeddingInconsistentModelsFailClosedToRules(t *testing.T) {
	var n atomic.Int32
	emb := embedderFunc(func(ctx context.Context, sc contracts.Scope, r providers.EmbedRequest) (providers.EmbedResult, error) {
		res, err := newConceptEmbedder().Embed(ctx, sc, r)
		if n.Add(1) == 1 {
			res.Model = "model-a"
		} else {
			res.Model = "model-b"
		}
		return res, err
	})
	rt, _ := newRouter(t, []RouterOption{WithEmbedder(emb)}, demoPackage(t))
	res := mustRoute(t, rt, reqFor(memberScope(), "could you put by a copy for me"))
	if res.Embedding != EmbeddingFailed || res.Outcome != OutcomeOutOfScope {
		t.Fatalf("got %s %v embedding=%s, want the rules-only out_of_scope", res.Outcome, res.IDs(), res.Embedding)
	}
}

// TestEmbedderFromProviderRegistry: the router follows the registry's embedder
// role, using it while healthy.
func TestEmbedderFromProviderRegistry(t *testing.T) {
	reg := providers.NewRegistry(clock.NewFake(t0))
	emb := newConceptEmbedder()
	if err := reg.Register(providers.RoleEmbedder, emb); err != nil {
		t.Fatal(err)
	}
	rt, _ := newRouter(t, []RouterOption{WithProviders(reg)}, demoPackage(t))
	res := mustRoute(t, rt, reqFor(memberScope(), "could you put by a copy for me"))
	if res.Embedding != EmbeddingUsed || !res.has(capReserve) {
		t.Fatalf("got %v embedding=%s", res.IDs(), res.Embedding)
	}
	if err := reg.SetHealth(providers.RoleEmbedder, providers.HealthUnavailable, "down"); err != nil {
		t.Fatal(err)
	}
	res = mustRoute(t, rt, reqFor(memberScope(), "could you put by a copy for me"))
	if res.Embedding != EmbeddingUnavailable || res.Outcome != OutcomeOutOfScope {
		t.Fatalf("after the role went down: %s embedding=%s", res.Outcome, res.Embedding)
	}
}

func mustRouter(t testing.TB, opts []RouterOption) *Router {
	t.Helper()
	rt, _ := newRouter(t, opts, demoPackage(t))
	return rt
}
