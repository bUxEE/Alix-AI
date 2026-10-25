package decision

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/decision/fakescorer"
	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

const scorerSecCanary = "PANIC-CANARY-5d1e-do-not-log"

// scorerSecPanicker is a provider whose scorer crashes on its first calls, then answers.
type scorerSecPanicker struct {
	*fakescorer.Scorer
	panics int
	value  any
}

func (p *scorerSecPanicker) ScoreDecision(ctx context.Context, scope contracts.Scope, req gen.DecisionRequest) (gen.DecisionScoreResult, error) {
	if p.panics > 0 {
		p.panics--
		panic(p.value)
	}
	return p.Scorer.ScoreDecision(ctx, scope, req)
}

func scorerSecEnv(t *testing.T, panics int, value any) (*env, *scorerSecPanicker) {
	t.Helper()
	e := newEnv(t)
	p := &scorerSecPanicker{Scorer: e.sc, panics: panics, value: value}
	eng, err := New(Deps{Clock: e.clk, Provider: p, Resolver: e.res, Audit: e.rec, Config: e.cfg})
	if err != nil {
		t.Fatal(err)
	}
	e.eng = eng
	return e, p
}

// A scorer is untrusted code: whatever it panics with, Score returns a typed fallback and the
// trail is decision.invoked followed by decision.fallback, never invoked alone.
func TestScorePanicIsRecoveredAndAudited(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"string", scorerSecCanary},
		{"error", contracts.New(contracts.ErrInternal, scorerSecCanary)},
		{"struct", struct{ S string }{scorerSecCanary}},
		{"nil", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := scorerSecEnv(t, 1, tc.value)
			var adv Advice
			func() {
				defer func() {
					if v := recover(); v != nil {
						t.Fatalf("Score let a scorer panic escape: %v", v)
					}
				}()
				adv = e.score(e.request("ten1"))
			}()
			wantFallback(t, adv, gen.FallbackReasonUnavailable)
			if got := strings.Join(e.rec.types(), ","); got != "decision.invoked,decision.fallback" {
				t.Fatalf("trail = %s, want it to end in decision.fallback", got)
			}
			if adv.Result().AuditRef == "" {
				t.Error("the fallback advice carries no audit ref")
			}
			for _, ev := range e.rec.events {
				b, _ := json.Marshal(ev.Data)
				if strings.Contains(string(b), scorerSecCanary) {
					t.Fatalf("the panic value reached the audit trail: %s", b)
				}
			}
			var fb FallbackAudit
			raw, _ := json.Marshal(e.rec.events[1].Data)
			if err := json.Unmarshal(raw, &fb); err != nil {
				t.Fatal(err)
			}
			if !fb.Invoked || fb.Reason != string(gen.FallbackReasonUnavailable) || fb.TraceID != adv.TraceID() {
				t.Errorf("fallback audit = %+v", fb)
			}
		})
	}

	t.Run("the layer keeps working and a crash is not cached", func(t *testing.T) {
		e, p := scorerSecEnv(t, 1, scorerSecCanary)
		req := e.request("ten1")
		wantFallback(t, e.score(req), gen.FallbackReasonUnavailable)
		adv := e.score(req) // same request: a cached crash would repeat the fallback
		if !adv.Accepted() || p.Scorer.Calls() != 1 {
			t.Fatalf("second call: accepted=%v scorer calls=%d", adv.Accepted(), p.Scorer.Calls())
		}
		if got := strings.Join(e.rec.types(), ","); got != "decision.invoked,decision.fallback,decision.invoked,decision.accepted" {
			t.Fatalf("trail = %s", got)
		}
	})
}
