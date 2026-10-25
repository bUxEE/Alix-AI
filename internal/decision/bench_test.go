package decision

import (
	"context"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/decision/fakescorer"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

type benchAppender struct{ recorder }

func (b *benchAppender) reset() { b.events, b.ids = b.events[:0], b.ids[:0] }

func benchEnv(b *testing.B, candidates int, cacheEntries int) (*Engine, *benchAppender, contracts.Scope, gen.DecisionRequest) {
	b.Helper()
	clk := clock.NewFake(t0)
	cfg := testConfig()
	cfg.CacheEntries = cacheEntries
	app := &benchAppender{}
	eng, err := New(Deps{Clock: clk, Provider: fakescorer.New(fakescorer.DefaultManifest(), clk), Resolver: &fakeResolver{}, Audit: app, Config: cfg})
	if err != nil {
		b.Fatal(err)
	}
	e := &env{clk: clk}
	req := e.request("ten1")
	req.CandidateSet.Candidates = nil
	for i := 0; i < candidates; i++ {
		req.CandidateSet.Candidates = append(req.CandidateSet.Candidates, gen.CandidateSetCandidate{CandidateID: common.Identifier("cand" + string(rune('a'+i))), Label: "label"})
	}
	return eng, app, testScope("ten1"), req
}

// BenchmarkGuard is the guard's own cost: structural checks plus the resolver call per
// candidate (a no-op here), for the typical 8-candidate set.
func BenchmarkGuard(b *testing.B) {
	eng, _, scope, req := benchEnv(b, 8, 0)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, code := eng.checkCandidates(ctx, scope, req); code != RejectNone {
			b.Fatal(code)
		}
		if !eng.shapeSupported(req) {
			b.Fatal("shape")
		}
	}
}

// BenchmarkGate is the acceptance gates over a one-selection result.
func BenchmarkGate(b *testing.B) {
	eng, _, scope, req := benchEnv(b, 8, 0)
	ids, _ := eng.checkCandidates(context.Background(), scope, req)
	fs := fakescorer.New(fakescorer.DefaultManifest(), nil)
	res, _ := fs.ScoreDecision(context.Background(), scope, req)
	m := fs.Manifest()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := eng.gate(m, req, ids, 1, res); r != "" {
			b.Fatal(r)
		}
	}
}

// BenchmarkScoreMiss is the whole engine path with the fake scorer (instant) and an
// in-memory audit double: guard, cache key, three audit appends worth of payload
// building, gates. It excludes the real audit log (a SQLite transaction per append).
func BenchmarkScoreMiss(b *testing.B) {
	eng, app, scope, req := benchEnv(b, 8, 0)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		app.reset()
		if adv, _ := eng.Score(ctx, scope, req); !adv.Accepted() {
			b.Fatal("not accepted")
		}
	}
}

// BenchmarkScoreCacheHit is the same path answered from the cache.
func BenchmarkScoreCacheHit(b *testing.B) {
	eng, app, scope, req := benchEnv(b, 8, 16)
	ctx := context.Background()
	if adv, _ := eng.Score(ctx, scope, req); !adv.Accepted() {
		b.Fatal("setup")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		app.reset()
		if adv, _ := eng.Score(ctx, scope, req); !adv.Accepted() {
			b.Fatal("not accepted")
		}
	}
}

// BenchmarkExactRuleRouter is the no-model baseline: normalise the utterance and look
// it up in an exact phrase table (the shape of dialogue's control detectors).
func BenchmarkExactRuleRouter(b *testing.B) {
	table := map[string]string{"yes": "confirm", "confirm": "confirm", "no": "reject", "cancel": "cancel", "help": "help", "annulla": "cancel", "si": "confirm"}
	in := "  Confirm "
	b.ReportAllocs()
	b.ResetTimer()
	var hit int
	for i := 0; i < b.N; i++ {
		if _, ok := table[strings.ToLower(strings.TrimSpace(in))]; ok {
			hit++
		}
	}
	if hit != b.N {
		b.Fatal("miss")
	}
}
