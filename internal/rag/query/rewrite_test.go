package query

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// countingRewriter returns a fixed list and records how it was asked.
type countingRewriter struct {
	mu    sync.Mutex
	calls int
	max   []int
	alts  []string
	err   error
	block func(ctx context.Context)
}

func (c *countingRewriter) Rewrite(ctx context.Context, _ contracts.Scope, _ string, max int) ([]string, error) {
	c.mu.Lock()
	c.calls++
	c.max = append(c.max, max)
	block := c.block
	c.mu.Unlock()
	if block != nil {
		block(ctx)
	}
	return c.alts, c.err
}

func (c *countingRewriter) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func warrantyShortMD() string {
	return doc("Warranty", mdSection("Terms",
		"The warranty covers manufacturing defects for twenty four months.",
		"Replacement parts carry a ninety day warranty of their own.",
		"Accidental damage is not covered at all.",
		"Shipping is free within the country."))
}

// Query rewriting is for low-confidence searches only, and it is bounded three
// ways: at most MaxRewrites alternatives (one full-text pass each), the
// rewriter's own deadline, and no rewriting once half the total budget is spent.
// It never changes the package's query or the scope.
func TestQueryRewriteBudgeted(t *testing.T) {
	sc := scopeOf("ta")
	rewriteOn := DefaultConfig()
	rewriteOn.Rewrite = true
	rewriteOn.ParentExpansion = false
	// "warranties" matches nothing in the index (it does not stem); "defects" matches one chunk.
	lowConfidence := "warranties defects"

	fixture := func(t *testing.T, rw Rewriter) (*env, *spyLex, Deps) {
		e := newEnv(t)
		e.addSource(sc, "Warranty", docSpec{id: "warranty", md: warrantyShortMD()})
		spy := &spyLex{Lexical: e.ix}
		d := e.deps()
		d.Lexical, d.Rewriter = spy, rw
		return e, spy, d
	}

	t.Run("off by default, even for a search that finds nothing", func(t *testing.T) {
		rw := &countingRewriter{alts: []string{"warranty"}}
		e, spy, d := fixture(t, rw)
		res := mustQuery(t, e.pipeline(d, DefaultConfig()), sc, Request{Text: "zzqxv"})
		if rw.callCount() != 0 || spy.searchCount() != 1 || res.Diagnostics.Rewrites != 0 {
			t.Errorf("rewriter called %d times, %d searches", rw.callCount(), spy.searchCount())
		}
	})

	t.Run("a confident search is not rewritten", func(t *testing.T) {
		rw := &countingRewriter{alts: []string{"warranty"}}
		e, spy, d := fixture(t, rw)
		// "warranty" matches two chunks, "parts covered defects" more: three hits is confident.
		cfg := rewriteOn
		cfg.RewriteBelowHits = 2
		res := mustQuery(t, e.pipeline(d, cfg), sc, Request{Text: "warranty covers"})
		if res.Diagnostics.FTSCandidates < 2 {
			t.Fatalf("fixture: only %d full-text hits", res.Diagnostics.FTSCandidates)
		}
		if rw.callCount() != 0 || spy.searchCount() != 1 {
			t.Errorf("rewriter called %d times, %d searches for a confident search", rw.callCount(), spy.searchCount())
		}
	})

	t.Run("a low-confidence search is rewritten without a model", func(t *testing.T) {
		e, spy, d := fixture(t, nil) // the no-model VariantRewriter
		res := mustQuery(t, e.pipeline(d, rewriteOn), sc, Request{Text: lowConfidence})
		if res.Diagnostics.Rewrites != 1 || spy.searchCount() != 2 {
			t.Fatalf("rewrites %d searches %d (%q), want one alternative and so two searches", res.Diagnostics.Rewrites, spy.searchCount(), spy.searchTexts())
		}
		if got := spy.searchTexts()[1]; got != "warranty defect" {
			t.Errorf("alternative %q, want the other inflection of each word", got)
		}
		if !strings.Contains(allText(res.Package), "ninety day warranty") {
			t.Errorf("the passage only the rewrite finds is missing: %q", texts(res.Package))
		}
		if !hasNotice(res.Package, rag.RetrievalNoticeQueryRewritten) {
			t.Errorf("notices %v: evidence a rewrite added must say so", res.Package.Retrieval.Notices)
		}
		if string(res.Package.Query) != lowConfidence {
			t.Errorf("package query %q, want the question as asked", res.Package.Query)
		}
	})

	t.Run("a rewriter that returns too many is cut to MaxRewrites", func(t *testing.T) {
		var alts []string
		for i := 0; i < 10; i++ {
			alts = append(alts, fmt.Sprintf("warranty variant%d", i))
		}
		rw := &countingRewriter{alts: alts}
		e, spy, d := fixture(t, rw)
		cfg := rewriteOn
		cfg.MaxRewrites = 2
		res := mustQuery(t, e.pipeline(d, cfg), sc, Request{Text: lowConfidence})
		if n := spy.searchCount(); n != 3 {
			t.Fatalf("%d searches (%q), want the first pass and at most MaxRewrites=2 more", n, spy.searchTexts())
		}
		if rw.max[0] != 2 || res.Diagnostics.Rewrites != 2 {
			t.Errorf("rewriter asked for %v, diagnostics rewrites %d", rw.max, res.Diagnostics.Rewrites)
		}
	})

	t.Run("a rewriter that is too slow is cut at its deadline", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		cases := map[string]func(ctx context.Context){
			"honours its context": func(ctx context.Context) { <-ctx.Done() },
			"ignores its context": func(ctx context.Context) { <-release },
		}
		for name, block := range cases {
			t.Run(name, func(t *testing.T) {
				rw := &countingRewriter{alts: []string{"warranty"}, block: block}
				e, spy, d := fixture(t, rw)
				cfg := rewriteOn
				cfg.RewriteTimeout = 40 * time.Millisecond
				start := time.Now()
				res := mustQuery(t, e.pipeline(d, cfg), sc, Request{Text: lowConfidence})
				if took := time.Since(start); took > 3*time.Second {
					t.Errorf("held for %s past a 40ms rewrite budget", took)
				}
				if !hasString(res.Diagnostics.Degraded, DegradedRewriteFailed) || res.Diagnostics.Rewrites != 0 || spy.searchCount() != 1 {
					t.Errorf("degraded %v rewrites %d searches %d", res.Diagnostics.Degraded, res.Diagnostics.Rewrites, spy.searchCount())
				}
				if !strings.Contains(allText(res.Package), "defects") {
					t.Errorf("the first pass's result must stand: %q", texts(res.Package))
				}
			})
		}
	})

	t.Run("no rewrite once half the total budget is spent", func(t *testing.T) {
		rw := &countingRewriter{alts: []string{"warranty"}}
		e, spy, d := fixture(t, rw)
		cfg := rewriteOn
		p := e.pipeline(d, cfg)
		p.hook = func(stage string) {
			if stage == "retrieved" {
				e.clk.Advance(cfg.TotalTimeout/2 + time.Millisecond)
			}
		}
		res := mustQuery(t, p, sc, Request{Text: lowConfidence})
		if rw.callCount() != 0 || spy.searchCount() != 1 || res.Diagnostics.Rewrites != 0 {
			t.Errorf("rewriter called %d times, %d searches, after half the budget", rw.callCount(), spy.searchCount())
		}

		// The control: without the clock jump the same query is rewritten.
		rw2 := &countingRewriter{alts: []string{"warranty"}}
		e2, _, d2 := fixture(t, rw2)
		mustQuery(t, e2.pipeline(d2, cfg), sc, Request{Text: lowConfidence})
		if rw2.callCount() != 1 {
			t.Errorf("the control did not rewrite: %d calls", rw2.callCount())
		}
	})

	t.Run("a failing rewriter leaves the first pass standing", func(t *testing.T) {
		rw := &countingRewriter{err: contracts.New(contracts.ErrUnavailable, "router down")}
		e, spy, d := fixture(t, rw)
		res := mustQuery(t, e.pipeline(d, rewriteOn), sc, Request{Text: lowConfidence})
		if !hasString(res.Diagnostics.Degraded, DegradedRewriteFailed) || spy.searchCount() != 1 || !strings.Contains(allText(res.Package), "defects") {
			t.Errorf("degraded %v searches %d items %q", res.Diagnostics.Degraded, spy.searchCount(), texts(res.Package))
		}
	})

	t.Run("an alternative whose search times out is a degradation, not an error", func(t *testing.T) {
		rw := &countingRewriter{alts: []string{"warranty defect"}}
		e, spy, d := fixture(t, rw)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		spy.searchFn = func(ctx context.Context, scope contracts.Scope, q index.Query) (index.Result, error) {
			if strings.Contains(q.Text, "warranty") && spy.searchCount() > 1 {
				<-release
			}
			return e.ix.Search(ctx, scope, q)
		}
		cfg := rewriteOn
		cfg.FTSTimeout = time.Second // the unblocked first pass must beat it even on a slow CI box
		res := mustQuery(t, e.pipeline(d, cfg), sc, Request{Text: lowConfidence})
		if !hasString(res.Diagnostics.Degraded, DegradedRewriteFailed) || len(res.Package.Items) == 0 {
			t.Errorf("degraded %v items %d", res.Diagnostics.Degraded, len(res.Package.Items))
		}
	})

	t.Run("a rewriter's output is data: syntax and size are neutralised", func(t *testing.T) {
		rw := &countingRewriter{alts: []string{
			`" OR tenant_id:tb NEAR(a b) * -text`,
			"warranty" + strings.Repeat(" more", 500),
			"control\x00chars\x07here warranty",
		}}
		e, spy, d := fixture(t, rw)
		res := mustQuery(t, e.pipeline(d, rewriteOn), sc, Request{Text: lowConfidence})
		for _, txt := range spy.searchTexts() {
			if strings.ContainsAny(txt, "\x00\x07") || len([]rune(txt)) > maxRewriteRunes {
				t.Errorf("an unbounded alternative reached the index: %q", txt)
			}
		}
		for _, it := range res.Package.Items {
			if string(it.SourceID) == "" {
				t.Errorf("empty source")
			}
		}
	})

	t.Run("the model rewriter asks the router role for the question alone", func(t *testing.T) {
		e := newEnv(t)
		e.addSource(sc, "Warranty", docSpec{id: "warranty", md: warrantyShortMD()})
		router := fake.New(fake.WithModel("test-router"))
		router.On(fake.ForRole(providers.RoleRouter)).ReplyJSON(map[string]any{"queries": []string{"warranty defect", "warranty parts", "warranty defect"}})
		if err := e.reg.Register(providers.RoleRouter, router); err != nil {
			t.Fatal(err)
		}
		d := e.deps()
		d.Rewriter = ModelRewriter{Registry: e.reg}
		res := mustQuery(t, e.pipeline(d, rewriteOn), sc, Request{Text: lowConfidence})
		if res.Diagnostics.Rewrites != 2 {
			t.Errorf("rewrites %d, want the two distinct alternatives", res.Diagnostics.Rewrites)
		}
		prompts := router.Prompts()
		if len(prompts) != 1 {
			t.Fatalf("%d router calls", len(prompts))
		}
		var all []string
		for _, m := range prompts[0].Chat.Messages {
			all = append(all, m.Content)
		}
		joined := strings.Join(all, "\n")
		if !strings.Contains(joined, lowConfidence) {
			t.Errorf("the router did not see the question: %q", joined)
		}
		for _, leak := range []string{"ninety day", "manufacturing defects", "Replacement parts"} {
			if strings.Contains(joined, leak) {
				t.Errorf("evidence text %q was sent to the rewriting model", leak)
			}
		}
		if prompts[0].Chat.MaxTokens != ModelRewriteMaxToks {
			t.Errorf("the model reply is not bounded: max tokens %d", prompts[0].Chat.MaxTokens)
		}
	})

	t.Run("no router configured is a degradation", func(t *testing.T) {
		e := newEnv(t)
		e.addSource(sc, "Warranty", docSpec{id: "warranty", md: warrantyShortMD()})
		d := e.deps()
		d.Rewriter = ModelRewriter{Registry: e.reg}
		res := mustQuery(t, e.pipeline(d, rewriteOn), sc, Request{Text: lowConfidence})
		if !hasString(res.Diagnostics.Degraded, DegradedRewriteFailed) || len(res.Package.Items) == 0 {
			t.Errorf("degraded %v items %d", res.Diagnostics.Degraded, len(res.Package.Items))
		}
	})
}

func TestCleanAlternatives(t *testing.T) {
	got := cleanAlternatives("What is the Warranty", []string{
		"  what   IS the warranty ", // the question again
		"warranty terms",
		"Warranty  Terms", // duplicate of the one above
		"",
		"bad\xffutf8",
		"tab\tand\nnewline words",
		strings.Repeat("x", 500),
		"fourth", "fifth",
	}, 4)
	want := []string{"warranty terms", "tab and newline words", strings.Repeat("x", maxRewriteRunes), "fourth"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestVariantRewriter(t *testing.T) {
	cases := map[string]string{
		"warranties defects":  "warranty defect",
		"boxes watches":       "box watch",
		"bus glass":           "bus glasses", // short words are kept; the endings follow rules, not grammar
		"order PN-4471-B now": "orders PN-4471-B now",
		"cat":                 "", // too short and identical: nothing to propose
	}
	for in, want := range cases {
		got, err := VariantRewriter{}.Rewrite(context.Background(), contracts.Scope{}, in, 2)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case want == "" && len(got) != 0:
			t.Errorf("%q: got %q, want none", in, got)
		case want != "" && (len(got) != 1 || got[0] != want):
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (VariantRewriter{}).Rewrite(ctx, contracts.Scope{}, "warranty", 2); err == nil {
		t.Error("a cancelled context was ignored")
	}
}
