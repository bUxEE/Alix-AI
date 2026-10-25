package capabilities

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// syntheticApp returns a package of n user capabilities in two languages, the
// size of a large application: each capability has a title, a purpose and three
// terms with a handful of synonyms in English and Italian. Words are made of the
// capability number, so a request names exactly one capability.
func syntheticApp(tb testing.TB, n int) (*testPkg, contracts.Scope) {
	tb.Helper()
	def := pkgDef{app: "bench-app", langs: []string{"en", "it"}, roles: []string{"member"}, grants: map[string][]string{}}
	var ids []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("area%02d.action%02d", i%10, i)
		ids = append(ids, id)
		var terms []string
		for k := 0; k < 3; k++ {
			tid := fmt.Sprintf("t%d_%d", i, k)
			terms = append(terms, tid)
			def.terms = append(def.terms, termDef{tid, map[string][]string{
				"en": {fmt.Sprintf("word%dalpha%d", i, k), fmt.Sprintf("word%dbeta%d", i, k), fmt.Sprintf("word%dgamma%d", i, k)},
				"it": {fmt.Sprintf("parola%dalfa%d", i, k), fmt.Sprintf("parola%dbeta%d", i, k)},
			}})
		}
		def.caps = append(def.caps, capDef{
			id: id, title: fmt.Sprintf("Operation number %d of the application", i), purpose: "Does one specific thing for the user and reports the result.",
			fields: []string{"query", "date"}, terms: map[string][]string{"en": terms, "it": terms},
		})
	}
	def.grants["member"] = ids
	scope := scopeFor("bench-app", "member", ids)
	return def.build(tb), scope
}

func benchRouter(tb testing.TB, p Package, opts ...RouterOption) *Router {
	tb.Helper()
	reg := NewRegistry(nil)
	if err := reg.Activate(p); err != nil {
		tb.Fatal(err)
	}
	rt, err := NewRouter(reg, policy.NewGateway(clock.NewFake(t0)), opts...)
	if err != nil {
		tb.Fatal(err)
	}
	return rt
}

func BenchmarkRoute(b *testing.B) {
	for _, n := range []int{4, 25, 100} {
		pkg, scope := syntheticApp(b, n)
		rt := benchRouter(b, pkg)
		mid := n / 2
		req := reqFor(scope, fmt.Sprintf("please do word%dalpha0 and word%dbeta1 for me", mid, mid), inLanguage("en"))
		b.Run(fmt.Sprintf("rules/%dcaps", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				res, err := rt.Route(context.Background(), req)
				if err != nil || len(res.Candidates) == 0 {
					b.Fatalf("%v %v", res.Outcome, err)
				}
			}
		})
	}

	for _, n := range []int{4, 25, 100} {
		pkg, scope := syntheticApp(b, n)
		gw := policy.NewGateway(clock.NewFake(t0))
		b.Run(fmt.Sprintf("expose_only/%dcaps", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if e := gw.Expose(context.Background(), policy.ExposeRequest{Scope: scope, Package: pkg, Binding: policy.BindingOf(scope)}); len(e.Visible) != n {
					b.Fatal(len(e.Visible))
				}
			}
		})
	}

	pkg, scope := syntheticApp(b, 25)
	rt := benchRouter(b, pkg)
	b.Run("rules/25caps/out_of_scope", func(b *testing.B) {
		req := reqFor(scope, "what is the capital of australia and who wrote hamlet", inLanguage("en"))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if res, err := rt.Route(context.Background(), req); err != nil || res.Outcome != OutcomeOutOfScope {
				b.Fatalf("%v %v", res.Outcome, err)
			}
		}
	})
	b.Run("rules+fake embedder/25caps (descriptors cached, query embedded)", func(b *testing.B) {
		emb := fake.New(fake.WithEmbedDim(384))
		rtE := benchRouter(b, pkg, WithEmbedder(emb), WithConfig(Config{MinSimilarity: 0.3}))
		req := reqFor(scope, "something with no keyword at all", inLanguage("en"))
		if _, err := rtE.Route(context.Background(), req); err != nil { // warm the descriptor cache
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := rtE.Route(context.Background(), req); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("registry compile/25caps", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := compile(pkg); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestIndexFootprint measures what the compiled index and the descriptor-vector
// cache cost in memory, with a bound so a change that bloats them is noticed.
// Run with -v for the figures recorded in docs/efficiency/M1-08.md.
func TestIndexFootprint(t *testing.T) {
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	for _, n := range []int{4, 25, 100} {
		pkg, scope := syntheticApp(t, n)
		before := heap()
		ix, err := compile(pkg)
		if err != nil {
			t.Fatal(err)
		}
		after := heap()
		// A signed delta: garbage from an earlier test can be reclaimed between the
		// two readings, which would wrap an unsigned difference.
		delta := int64(after) - int64(before)
		perCap := float64(delta) / float64(n)

		// Fill the vector cache for every capability at 384 dimensions.
		emb := fake.New(fake.WithEmbedDim(384))
		rt := benchRouter(t, pkg, WithEmbedder(emb), WithConfig(Config{MinSimilarity: 0.3}))
		if _, err := rt.Route(context.Background(), reqFor(scope, "nothing matches this", inLanguage("en"))); err != nil {
			t.Fatal(err)
		}
		vecBytes := 0
		rt.reg.mu.RLock()
		for _, vs := range rt.reg.apps["bench-app"].vecs {
			for _, v := range vs.vecs {
				vecBytes += len(v) * 4
			}
		}
		rt.reg.mu.RUnlock()
		t.Logf("%3d capabilities: compiled index %6.1f KiB (%.0f B per capability); descriptor vectors at 384 dimensions %6.1f KiB",
			n, float64(delta)/1024, perCap, float64(vecBytes)/1024)
		switch {
		case delta <= 0:
			t.Logf("%d capabilities: the heap shrank during the measurement (%d bytes); not measurable this run", n, delta)
		case delta > int64(n)*32*1024:
			t.Errorf("the index of %d capabilities takes %d bytes, over the 32 KiB per capability bound", n, delta)
		}
		runtime.KeepAlive(ix)
	}
}
