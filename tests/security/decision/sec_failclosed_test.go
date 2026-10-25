package decisionsec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/adapters/decision/clm"
	"github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/decision/fakescorer"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/pkg/contracts"
	actgen "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// SEC-106
func TestSecDecisionFailClosedOnTimeoutCrashMismatchAndLowConfidence(t *testing.T) {
	sc := scopeOf("ten1")
	mutate := func(f func(*gen.DecisionScoreResult)) fakescorer.Step { return fakescorer.Step{Mutate: f} }
	model := func(id string) func(*gen.DecisionScoreResult) {
		return func(r *gen.DecisionScoreResult) { m := common.Identifier(id); r.ModelID = &m }
	}
	revision := func(rev string) func(*gen.DecisionScoreResult) {
		return func(r *gen.DecisionScoreResult) { v := actgen.OpaqueVersion(rev); r.ModelRevision = &v }
	}

	t.Run("control: the healthy path is accepted and resolves", func(t *testing.T) {
		r := newRig(t)
		adv := r.score(sc, r.request("ten1"))
		wantAccepted(t, adv)
		if len(r.resolve(sc, adv).Selected()) != 1 {
			t.Fatal("control: the accepted advice did not resolve")
		}
	})

	engineCases := []struct {
		name string
		step fakescorer.Step
		mods []func(*decision.Config)
		want gen.FallbackReason
	}{
		{"timeout", fakescorer.Step{Fail: fakescorer.FailTimeout}, nil, gen.FallbackReasonTimeout},
		{"hung scorer", fakescorer.Step{Hang: true}, []func(*decision.Config){func(c *decision.Config) { c.Timeout = 30 * time.Millisecond }}, gen.FallbackReasonTimeout},
		{"answer after the deadline", fakescorer.Step{Latency: 2 * time.Second}, nil, gen.FallbackReasonTimeout},
		{"crash: sidecar down", fakescorer.Step{Fail: fakescorer.FailUnavailable}, nil, gen.FallbackReasonUnavailable},
		{"malformed output", fakescorer.Step{Fail: fakescorer.FailMalformed}, nil, gen.FallbackReasonMalformedOutput},
		{"cannot load", fakescorer.Step{Fail: fakescorer.FailResourceExhausted}, nil, gen.FallbackReasonResourceGate},
		{"another model answers", mutate(model("impostor")), nil, gen.FallbackReasonModelMismatch},
		{"another revision answers", mutate(revision("rev-evil")), nil, gen.FallbackReasonModelMismatch},
		{"another provider answers", mutate(func(r *gen.DecisionScoreResult) { r.ProviderID = "impostor" }), nil, gen.FallbackReasonModelMismatch},
		{"another contract version", mutate(func(r *gen.DecisionScoreResult) { r.ContractVersion = "cv-other" }), nil, gen.FallbackReasonModelMismatch},
		{"the operator pinned another revision", fakescorer.Step{}, []func(*decision.Config){func(c *decision.Config) { c.PinnedRevision = "rev0" }}, gen.FallbackReasonModelMismatch},
		{"the operator pinned another model", fakescorer.Step{}, []func(*decision.Config){func(c *decision.Config) { c.PinnedModelID = "model-0" }}, gen.FallbackReasonModelMismatch},
		{"low confidence", pickStep("c1", 0.84), nil, gen.FallbackReasonLowConfidence},
		{"confidence is NaN", pickStep("c1", math.NaN()), nil, gen.FallbackReasonMalformedOutput},
		{"confidence above one", pickStep("c1", 1.5), nil, gen.FallbackReasonMalformedOutput},
		{"no calibration for this contract version", fakescorer.Step{}, []func(*decision.Config){func(c *decision.Config) { c.Calibration = nil }}, gen.FallbackReasonCalibrationFailed},
		{"calibration error above the bound", fakescorer.Step{}, []func(*decision.Config){func(c *decision.Config) { c.Calibration[0].ECE = 0.4 }}, gen.FallbackReasonCalibrationFailed},
		{"out of distribution", mutate(func(r *gen.DecisionScoreResult) { r.OodStatus = gen.DecisionScoreResultOodStatusOutOfDistribution }), nil, gen.FallbackReasonOodUnsupportedShape},
		{"distribution not evaluated", mutate(func(r *gen.DecisionScoreResult) { r.OodStatus = gen.DecisionScoreResultOodStatusNotEvaluated }), nil, gen.FallbackReasonOodUnsupportedShape},
		{"unknown disposition", mutate(func(r *gen.DecisionScoreResult) { r.Disposition = "approved" }), nil, gen.FallbackReasonMalformedOutput},
		{"accepted but with a fallback reason", mutate(func(r *gen.DecisionScoreResult) { f := gen.FallbackReasonDisabled; r.FallbackReason = &f }), nil, gen.FallbackReasonMalformedOutput},
		{"declined but with a selection", mutate(func(r *gen.DecisionScoreResult) {
			f := gen.FallbackReasonLowConfidence
			r.Disposition, r.FallbackReason = gen.DecisionScoreResultDispositionFallback, &f
		}), nil, gen.FallbackReasonMalformedOutput},
		{"declined with an invalid reason", mutate(func(r *gen.DecisionScoreResult) {
			f := gen.FallbackReason("because")
			r.Disposition, r.FallbackReason, r.Selections = gen.DecisionScoreResultDispositionFallback, &f, nil
		}), nil, gen.FallbackReasonMalformedOutput},
		{"wrong decision type echoed", mutate(func(r *gen.DecisionScoreResult) { r.DecisionType = gen.DecisionTypeActionSelection }), nil, gen.FallbackReasonMalformedOutput},
		{"wrong schema version echoed", mutate(func(r *gen.DecisionScoreResult) { r.SchemaVersion = 2 }), nil, gen.FallbackReasonMalformedOutput},
	}
	for _, c := range engineCases {
		t.Run("engine: "+c.name, func(t *testing.T) {
			r := newRig(t, c.mods...)
			r.sc.Script(c.step)
			adv := r.score(sc, r.request("ten1"))
			wantFallback(t, adv, c.want)
			res := r.resolve(sc, adv)
			if _, ok := res.Fallback(); !ok || len(res.Selected()) != 0 || len(r.cat.resolveCalls()) != 0 {
				t.Fatalf("a failed scoring left a usable selection: %+v, resolver calls %v", res.Selected(), r.cat.resolveCalls())
			}
			if r.aud.count("decision.fallback") != 1 || r.aud.count("decision.accepted") != 0 {
				t.Fatalf("audit = %v", r.aud.types())
			}
		})
	}

	t.Run("engine: a second selection below the confidence floor sinks the whole proposal", func(t *testing.T) {
		r := newRig(t, func(c *decision.Config) {})
		req := r.request("ten1")
		req.CandidateSet.MaxSelections = 2
		r.sc.Script(fakescorer.Step{Picks: []fakescorer.Pick{{ID: "c1", Score: 0.9, Confidence: 0.99}, {ID: "c2", Score: 0.8, Confidence: 0.5}}})
		wantFallback(t, r.score(sc, req), gen.FallbackReasonLowConfidence)
	})

	t.Run("engine: the confidence floor is exact: at the floor accepted, one step below refused", func(t *testing.T) {
		r := newRig(t)
		r.sc.Script(pickStep("c1", r.cfg.MinConfidence))
		wantAccepted(t, r.score(sc, r.request("ten1")))
		r = newRig(t)
		r.sc.Script(pickStep("c1", r.cfg.MinConfidence-0.001))
		wantFallback(t, r.score(sc, r.request("ten1")), gen.FallbackReasonLowConfidence)
	})

	t.Run("engine: layer off, no scorer, an expired deadline and a plain error all fall back and never reach a scorer or succeed", func(t *testing.T) {
		r := newRig(t, func(c *decision.Config) { c.Enabled = false })
		wantFallback(t, r.score(sc, r.request("ten1")), gen.FallbackReasonDisabled)
		if r.sc.received() != 0 {
			t.Fatal("a disabled layer called the scorer")
		}

		r = newRig(t)
		req := r.request("ten1")
		req.Deadline = ts(r.clk.Now().Add(-time.Second))
		wantFallback(t, r.score(sc, req), gen.FallbackReasonTimeout)
		if r.sc.received() != 0 {
			t.Fatal("a request past its deadline was sent to the scorer")
		}

		// Nil provider: no scorer registered.
		var nilProv decision.DecisionModelProvider
		eng, err := decision.New(decision.Deps{Clock: r.clk, Provider: nilProv, Resolver: r.cat, Audit: r.aud, Config: r.cfg})
		if err != nil {
			t.Fatal(err)
		}
		adv, err := eng.Score(bg, sc, r.request("ten1"))
		if err != nil {
			t.Fatal(err)
		}
		wantFallback(t, adv, gen.FallbackReasonUnavailable)

		// An error nobody classified is unavailable, never success.
		plain := &errProvider{err: errors.New("segfault in the sidecar")}
		eng, err = decision.New(decision.Deps{Clock: r.clk, Provider: plain, Resolver: r.cat, Audit: r.aud, Config: r.cfg})
		if err != nil {
			t.Fatal(err)
		}
		adv, err = eng.Score(bg, sc, r.request("ten1"))
		if err != nil {
			t.Fatal(err)
		}
		wantFallback(t, adv, gen.FallbackReasonUnavailable)

		// A provider that wraps the unsupported-runtime sentinel is reported as such.
		plain.err = fmt.Errorf("backend lacks the route: %w", decision.ErrUnsupportedRuntime)
		adv, _ = eng.Score(bg, sc, r.request("ten1"))
		wantFallback(t, adv, gen.FallbackReasonUnsupportedRuntime)
	})

	t.Run("engine: a verified scope is required: no scope, no scoring", func(t *testing.T) {
		r := newRig(t)
		if _, err := r.eng.Score(bg, scopeOf(""), r.request("ten1")); err == nil {
			t.Fatal("a request with no tenant was scored")
		}
		if r.sc.received() != 0 {
			t.Fatal("the scorer was reached with no scope")
		}
	})

	// The dialogue world: every failure gives the controller's outcome, exactly as with no layer at all.
	bookFields := map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"}
	baseline := newWorld(t, withoutLayer())
	baseline.propose("appointments.book", "task", bookFields)
	baseRes := baseline.say(recep(), sayBook)
	baseShape := baseline.shapeOf(recep(), baseRes)
	if baseRes.Prepared == nil {
		t.Fatalf("control: the controller-only run did not prepare the booking: %+v", baseShape)
	}

	worldCases := []struct {
		name   string
		opts   []worldOpt
		crash  bool
		reason dialogue.FallbackReason
	}{
		{"no scorer in the router role", []worldOpt{withNoScorer()}, false, dialogue.FallbackUnavailable},
		{"the router role holds a text model", []worldOpt{withTextRouter()}, false, dialogue.FallbackUnsupportedRuntime},
		{"the scorer is down", []worldOpt{withScript(fakescorer.Step{Fail: fakescorer.FailUnavailable})}, false, dialogue.FallbackUnavailable},
		{"the scorer times out", []worldOpt{withScript(fakescorer.Step{Fail: fakescorer.FailTimeout})}, false, dialogue.FallbackTimeout},
		{"the scorer hangs", []worldOpt{withScript(fakescorer.Step{Hang: true}), withCore(func(c *decision.Config) { c.Timeout = 40 * time.Millisecond })}, false, dialogue.FallbackTimeout},
		{"the scorer returns garbage", []worldOpt{withScript(fakescorer.Step{Fail: fakescorer.FailMalformed})}, false, dialogue.FallbackMalformedOutput},
		{"the scorer cannot load", []worldOpt{withScript(fakescorer.Step{Fail: fakescorer.FailResourceExhausted})}, false, dialogue.FallbackResourceGate},
		{"a different model answers", []worldOpt{withScript(mutate(model("impostor")))}, false, dialogue.FallbackModelMismatch},
		{"a different revision answers", []worldOpt{withScript(mutate(revision("rev-evil")))}, false, dialogue.FallbackModelMismatch},
		{"the answer is not confident", []worldOpt{withScript(pickStep("appointments.book", 0.40))}, false, dialogue.FallbackLowConfidence},
		{"the scorer process crashes (panic)", []worldOpt{withScript(pickStep("appointments.book", 0.99))}, true, dialogue.FallbackUnavailable},
		{"the layer is switched off", []worldOpt{withCore(func(c *decision.Config) { c.Enabled = false })}, false, dialogue.FallbackDisabled},
	}
	for _, c := range worldCases {
		t.Run("dialogue: "+c.name, func(t *testing.T) {
			w := newWorld(t, c.opts...)
			if c.crash {
				w.scorer.crash = true
			}
			w.propose("appointments.book", "task", bookFields)

			res := w.say(recep(), sayBook)

			if res.Trace.Decision != string(c.reason) {
				t.Fatalf("decision = %q, want %q", res.Trace.Decision, c.reason)
			}
			if d := cmp.Diff(baseShape, w.shapeOf(recep(), res)); d != "" {
				t.Fatalf("the failure changed the outcome:\n%s", d)
			}
			if got := proposalCaps(t, res); got != 3 {
				t.Fatalf("the controller was offered %d capabilities, want the full shortlist of 3", got)
			}
			if res.Receipt != nil {
				t.Fatalf("a receipt appeared: %+v", res.Receipt)
			}
			w.noCommits()
		})
	}

	t.Run("a hostile CLM sidecar behind the real adapter: every misbehaviour is a typed fallback with no selection", func(t *testing.T) {
		const models = `{"data":[{"id":"clm-latest","revision":"rev1"}]}`
		const health = `{"cache":{"entries":0,"hit_rate":0}}`
		rank := func(candidate string, prob float64) string {
			return fmt.Sprintf(`{"model":"clm-latest","ranked":[{"rank":1,"candidate":%q,"prob":%v},{"rank":2,"candidate":"Option 1","prob":0.01}]}`, candidate, prob)
		}
		type sidecar func(w http.ResponseWriter, rq *http.Request)
		serve := func(models, rankBody string) sidecar {
			return func(w http.ResponseWriter, rq *http.Request) {
				switch rq.URL.Path {
				case "/v1/models":
					fmt.Fprint(w, models)
				case "/health":
					fmt.Fprint(w, health)
				case "/v1/rank":
					fmt.Fprint(w, rankBody)
				default:
					http.NotFound(w, rq)
				}
			}
		}
		crash := func(w http.ResponseWriter, rq *http.Request) {
			if rq.URL.Path == "/v1/models" {
				fmt.Fprint(w, models)
				return
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close() // the process died with the request in flight
			}
		}
		hang := func(w http.ResponseWriter, rq *http.Request) {
			if rq.URL.Path == "/v1/models" {
				fmt.Fprint(w, models)
				return
			}
			select {
			case <-rq.Context().Done():
			case <-time.After(3 * time.Second):
			}
		}
		cases := []struct {
			name string
			h    sidecar
			want []gen.FallbackReason // any of
		}{
			{"control: a healthy sidecar", serve(models, rank("Option 2", 0.97)), nil},
			{"the revision was swapped", serve(`{"data":[{"id":"clm-latest","revision":"rev2"}]}`, rank("Option 2", 0.97)), []gen.FallbackReason{gen.FallbackReasonModelMismatch}},
			{"the model is another one", serve(`{"data":[{"id":"other","revision":"rev1"}]}`, rank("Option 2", 0.97)), []gen.FallbackReason{gen.FallbackReasonModelMismatch}},
			{"the process died mid request", crash, []gen.FallbackReason{gen.FallbackReasonUnavailable}},
			{"the process hangs", hang, []gen.FallbackReason{gen.FallbackReasonTimeout}},
			{"the answer is not confident", serve(models, rank("Option 2", 0.40)), []gen.FallbackReason{gen.FallbackReasonLowConfidence}},
			{"the answer names a candidate that was not offered", serve(models, rank("Option 9", 0.99)), []gen.FallbackReason{gen.FallbackReasonMalformedOutput}},
			{"the answer is not JSON", serve(models, `<html>502</html>`), []gen.FallbackReason{gen.FallbackReasonMalformedOutput}},
			{"the answer is far too big", serve(models, strings.Repeat("A", 3<<20)), []gen.FallbackReason{gen.FallbackReasonMalformedOutput, gen.FallbackReasonUnavailable, gen.FallbackReasonResourceGate}},
		}
		for _, c := range cases {
			srv := httptest.NewServer(http.HandlerFunc(c.h))
			if host, _, err := net.SplitHostPort(srv.Listener.Addr().String()); err != nil || host != "127.0.0.1" {
				srv.Close()
				t.Fatalf("%s: the stand-in sidecar is not on loopback: %v", c.name, srv.Listener.Addr())
			}
			p, err := clm.New(clm.Config{Endpoint: srv.URL, Model: "clm-latest", ModelRevision: "rev1", RequestTimeout: 150 * time.Millisecond})
			if err != nil {
				srv.Close()
				t.Fatal(err)
			}
			r := newRig(t, func(cfg *decision.Config) {
				cfg.Calibration = []decision.Calibration{{ModelID: string(p.Manifest().ModelID), ModelRevision: "rev1", ContractVersion: "cv1", ECE: 0.01, MaxECE: 0.05}}
			})
			eng, err := decision.New(decision.Deps{Clock: r.clk, Provider: p, Resolver: r.cat, Audit: r.aud, Config: r.cfg})
			if err != nil {
				srv.Close()
				t.Fatal(err)
			}
			req := r.request("ten1")
			for i := range req.CandidateSet.Candidates {
				req.CandidateSet.Candidates[i].Label = fmt.Sprintf("Option %d", i+1)
			}
			// The adapter takes real time; the engine clock is fake, so give the request a far deadline.
			req.Deadline = ts(r.clk.Now().Add(10 * time.Second))
			adv, err := eng.Score(bg, sc, req)
			srv.Close()
			if err != nil {
				t.Fatal(err)
			}
			if c.want == nil {
				if !adv.Accepted() {
					reason, _ := adv.FallbackReason()
					t.Fatalf("%s: fallback %q", c.name, reason)
				}
				continue
			}
			reason, ok := adv.FallbackReason()
			t.Logf("sidecar %s -> %q", c.name, reason)
			found := false
			for _, w := range c.want {
				found = found || reason == w
			}
			if adv.Accepted() || !ok || !found || len(adv.Suggestions()) != 0 {
				t.Errorf("%s: accepted=%v reason=%q want one of %v", c.name, adv.Accepted(), reason, c.want)
			}
		}
	})
}

type errProvider struct{ err error }

func (e *errProvider) Manifest() gen.ScorerManifest { return fakescorer.DefaultManifest() }
func (e *errProvider) ScoreDecision(_ context.Context, _ contracts.Scope, _ gen.DecisionRequest) (gen.DecisionScoreResult, error) {
	return gen.DecisionScoreResult{}, e.err
}
