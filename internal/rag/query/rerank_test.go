package query

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

func sprocketMD() string {
	return doc("Sprockets", mdSection("Catalogue",
		"Sprocket one is steel and fits the small drive.",
		"Sprocket two is brass and fits the large drive.",
		"Sprocket three is nylon and fits the quiet drive.",
		"Sprocket four is titanium and fits the racing drive.",
		"Sprocket five is carbon and fits the lightweight drive.",
		"Sprocket six is bronze and fits the vintage drive.",
		"Part PN-9000-X is the sprocket service kit."))
}

// addReranker registers a fake provider in the Reranker role.
func (e *env) addReranker(opts ...fake.Option) *fake.Provider {
	e.t.Helper()
	rr := fake.New(append([]fake.Option{fake.WithModel("test-rerank")}, opts...)...)
	if err := e.reg.Register(providers.RoleReranker, rr); err != nil {
		e.t.Fatal(err)
	}
	return rr
}

// reverse is a reranker that disagrees with the fused order about everything.
func reverseRerank(req providers.RerankRequest) ([]providers.RerankHit, error) {
	hits := make([]providers.RerankHit, len(req.Documents))
	for i := range req.Documents {
		idx := len(req.Documents) - 1 - i
		hits[i] = providers.RerankHit{Index: idx, Score: float64(len(req.Documents)-i) / 10}
	}
	return hits, nil
}

// The reranker is optional in every direction: off by default (and always off in
// Lite), absent, unavailable, failing or slow, the answer is the fused order and
// the package says nothing is wrong with the evidence, only that it was not
// reranked; when it works it can reorder, but never past an exact identifier,
// and it only ever sees authorised chunk text.
func TestRerankerOptional(t *testing.T) {
	question := "sprocket drive"
	base := DefaultConfig()
	base.ParentExpansion = false
	base.MaxItems = 8
	rerankOn := base
	rerankOn.Rerank = true
	rerankOn.RerankCandidates = 4

	if DefaultConfig().Rerank {
		t.Fatal("the default configuration turns the reranker on: it must be off in Lite")
	}

	baseline := func(t *testing.T, e *env) []string {
		t.Helper()
		res := mustQuery(t, e.pipeline(e.deps(), base), scopeOf("ta"), Request{Text: question})
		if len(res.Package.Items) < 6 {
			t.Fatalf("baseline has %d items", len(res.Package.Items))
		}
		return texts(res.Package)
	}
	fixture := func(t *testing.T, opts ...fake.Option) (*env, *fake.Provider) {
		e := newEnv(t)
		e.addSource(scopeOf("ta"), "Sprockets", docSpec{id: "sprockets", md: sprocketMD()})
		return e, e.addReranker(opts...)
	}

	t.Run("a registered reranker is not called while the switch is off", func(t *testing.T) {
		e, rr := fixture(t, fake.WithRerankFunc(reverseRerank))
		res := mustQuery(t, e.pipeline(e.deps(), base), scopeOf("ta"), Request{Text: question})
		if rr.Calls() != 0 || res.Diagnostics.Reranked {
			t.Errorf("reranker called %d times with Rerank off", rr.Calls())
		}
		for _, m := range res.Package.Retrieval.Methods {
			if m == rag.RetrievalMethodRerank {
				t.Errorf("methods %v claim a rerank that did not happen", res.Package.Retrieval.Methods)
			}
		}
		if len(res.Diagnostics.Degraded) != 0 {
			t.Errorf("degraded %v: switching a stage off is not a degradation", res.Diagnostics.Degraded)
		}
	})

	t.Run("no reranker role: fused order, labelled in the diagnostics", func(t *testing.T) {
		e := newEnv(t)
		e.addSource(scopeOf("ta"), "Sprockets", docSpec{id: "sprockets", md: sprocketMD()})
		want := baseline(t, e)
		res := mustQuery(t, e.pipeline(e.deps(), rerankOn), scopeOf("ta"), Request{Text: question})
		if !reflect.DeepEqual(texts(res.Package), want) {
			t.Errorf("order changed without a reranker:\n got %q\nwant %q", texts(res.Package), want)
		}
		if !hasString(res.Diagnostics.Degraded, DegradedNoReranker) || res.Diagnostics.Reranked {
			t.Errorf("degraded %v reranked %v", res.Diagnostics.Degraded, res.Diagnostics.Reranked)
		}
	})

	t.Run("unavailable reranker is not called", func(t *testing.T) {
		e, rr := fixture(t, fake.WithRerankFunc(reverseRerank))
		want := baseline(t, e)
		if err := e.reg.SetHealth(providers.RoleReranker, providers.HealthUnavailable, "down"); err != nil {
			t.Fatal(err)
		}
		res := mustQuery(t, e.pipeline(e.deps(), rerankOn), scopeOf("ta"), Request{Text: question})
		if rr.Calls() != 0 || !reflect.DeepEqual(texts(res.Package), want) || !hasString(res.Diagnostics.Degraded, DegradedNoReranker) {
			t.Errorf("calls %d degraded %v order kept %v", rr.Calls(), res.Diagnostics.Degraded, reflect.DeepEqual(texts(res.Package), want))
		}
	})

	t.Run("failing reranker: fused order", func(t *testing.T) {
		e, rr := fixture(t)
		want := baseline(t, e)
		rr.FailNext(fake.FailUnavailable)
		res := mustQuery(t, e.pipeline(e.deps(), rerankOn), scopeOf("ta"), Request{Text: question})
		if rr.Calls() != 1 {
			t.Fatalf("reranker called %d times, want the one failing call", rr.Calls())
		}
		if !reflect.DeepEqual(texts(res.Package), want) || !hasString(res.Diagnostics.Degraded, DegradedRerankFailed) || res.Diagnostics.Reranked {
			t.Errorf("degraded %v reranked %v order kept %v", res.Diagnostics.Degraded, res.Diagnostics.Reranked, reflect.DeepEqual(texts(res.Package), want))
		}
	})

	t.Run("a reranker answering nonsense is ignored", func(t *testing.T) {
		e, _ := fixture(t, fake.WithRerankFunc(func(req providers.RerankRequest) ([]providers.RerankHit, error) {
			return []providers.RerankHit{{Index: 99, Score: 1}}, nil // not one of the documents
		}))
		want := baseline(t, e)
		res := mustQuery(t, e.pipeline(e.deps(), rerankOn), scopeOf("ta"), Request{Text: question})
		if !reflect.DeepEqual(texts(res.Package), want) || !hasString(res.Diagnostics.Degraded, DegradedRerankFailed) {
			t.Errorf("degraded %v order kept %v", res.Diagnostics.Degraded, reflect.DeepEqual(texts(res.Package), want))
		}
	})

	t.Run("a reranker that does not answer in time is cut", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		e, _ := fixture(t, fake.WithRerankFunc(func(req providers.RerankRequest) ([]providers.RerankHit, error) {
			<-release
			return nil, nil
		}))
		want := baseline(t, e)
		cfg := rerankOn
		cfg.RerankTimeout = 40 * time.Millisecond
		start := time.Now()
		res := mustQuery(t, e.pipeline(e.deps(), cfg), scopeOf("ta"), Request{Text: question})
		if took := time.Since(start); took > 3*time.Second {
			t.Errorf("held for %s past a 40ms budget", took)
		}
		if !reflect.DeepEqual(texts(res.Package), want) || !hasString(res.Diagnostics.Degraded, DegradedRerankFailed) {
			t.Errorf("degraded %v order kept %v", res.Diagnostics.Degraded, reflect.DeepEqual(texts(res.Package), want))
		}
	})

	t.Run("a working reranker reorders its short list and nothing else", func(t *testing.T) {
		e, rr := fixture(t, fake.WithRerankFunc(reverseRerank))
		want := baseline(t, e)
		res := mustQuery(t, e.pipeline(e.deps(), rerankOn), scopeOf("ta"), Request{Text: "sprocket drive"})
		got := texts(res.Package)
		// The exact identifier does not take part in this question, so the first
		// four are the baseline's first four, reversed; the rest keep their place.
		wantOrder := append([]string{want[3], want[2], want[1], want[0]}, want[4:]...)
		if !reflect.DeepEqual(got, wantOrder) {
			t.Errorf("order\n got %q\nwant %q", got, wantOrder)
		}
		if !res.Diagnostics.Reranked || !hasMethod(res.Package, rag.RetrievalMethodRerank) {
			t.Errorf("reranked %v methods %v", res.Diagnostics.Reranked, res.Package.Retrieval.Methods)
		}
		prompts := rr.Prompts()
		if len(prompts) != 1 {
			t.Fatalf("%d rerank calls, want 1", len(prompts))
		}
		rq := prompts[0].Rerank
		if rq.Query != "sprocket drive" || !reflect.DeepEqual(rq.Documents, want[:4]) {
			t.Errorf("the reranker saw %q / %q, want the question and the top 4 chunk texts %q", rq.Query, rq.Documents, want[:4])
		}
	})

	t.Run("unscored documents keep their place after the scored ones", func(t *testing.T) {
		e, _ := fixture(t, fake.WithRerankFunc(func(req providers.RerankRequest) ([]providers.RerankHit, error) {
			return []providers.RerankHit{{Index: 2, Score: 0.9}, {Index: 0, Score: 0.5}}, nil
		}))
		want := baseline(t, e)
		res := mustQuery(t, e.pipeline(e.deps(), rerankOn), scopeOf("ta"), Request{Text: question})
		wantOrder := append([]string{want[2], want[0], want[1], want[3]}, want[4:]...)
		if got := texts(res.Package); !reflect.DeepEqual(got, wantOrder) {
			t.Errorf("order\n got %q\nwant %q", got, wantOrder)
		}
	})

	t.Run("an exact identifier stays first whatever the reranker thinks", func(t *testing.T) {
		e, _ := fixture(t, fake.WithRerankFunc(reverseRerank))
		cfg := rerankOn
		cfg.RerankCandidates = 8
		res := mustQuery(t, e.pipeline(e.deps(), cfg), scopeOf("ta"), Request{Text: "sprocket service kit PN-9000-X"})
		if !res.Diagnostics.Reranked {
			t.Fatal("the reranker did not run")
		}
		if len(res.Package.Items) == 0 || !strings.Contains(res.Package.Items[0].Text, "PN-9000-X") {
			t.Errorf("the reranker moved the exact match: %q", texts(res.Package))
		}
	})

	t.Run("the reranker's input is bounded", func(t *testing.T) {
		e, rr := fixture(t)
		cfg := rerankOn
		cfg.RerankCandidates = 3
		mustQuery(t, e.pipeline(e.deps(), cfg), scopeOf("ta"), Request{Text: question})
		prompts := rr.Prompts()
		if len(prompts) != 1 || len(prompts[0].Rerank.Documents) != 3 {
			t.Fatalf("rerank calls %d, documents %v: want one call over 3 documents", len(prompts), prompts)
		}
	})
}

func hasMethod(p rag.EvidencePackage, m rag.RetrievalMethod) bool {
	for _, have := range p.Retrieval.Methods {
		if have == m {
			return true
		}
	}
	return false
}
