package decisionsec

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/decision"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

func traceOf(t testing.TB, e recordedEvent) string {
	t.Helper()
	const key = `"trace_id":"`
	i := strings.Index(e.JSON, key)
	if i < 0 {
		t.Fatalf("event %s has no trace id: %s", e.Type, e.JSON)
	}
	rest := e.JSON[i+len(key):]
	return rest[:strings.Index(rest, `"`)]
}

// SEC-109
func TestSecDecisionAuditEventsForInvocationAcceptanceFallbackAndFinalAction(t *testing.T) {
	sc := scopeOf("ten1")
	wantTypes := func(t *testing.T, r *rig, want ...string) {
		t.Helper()
		if got := r.aud.types(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("audit events = %v, want %v", got, want)
		}
	}

	t.Run("an accepted and resolved decision leaves invoked, accepted and final_action under one trace id", func(t *testing.T) {
		r := newRig(t)
		adv := r.score(sc, r.request("ten1"))
		wantAccepted(t, adv)
		r.resolve(sc, adv)

		wantTypes(t, r, "decision.invoked", "decision.accepted", "decision.final_action")
		ev := r.aud.all()
		for _, e := range ev {
			if traceOf(t, e) != adv.TraceID() || e.Tenant != "ten1" {
				t.Fatalf("event %s: trace %s tenant %s, want %s / ten1", e.Type, traceOf(t, e), e.Tenant, adv.TraceID())
			}
		}
		mustContain(t, ev[0].JSON, `"candidates":3`, "decision.invoked")
		mustContain(t, ev[0].JSON, `"model_revision":"rev1"`, "decision.invoked")
		mustContain(t, ev[1].JSON, `"candidate_id":"c1"`, "decision.accepted")
		mustContain(t, ev[1].JSON, `"confidence_permille":950`, "decision.accepted")
		mustContain(t, ev[2].JSON, `"action":"selection_resolved"`, "decision.final_action")
		if adv.Result().AuditRef == "" {
			t.Fatal("the accepted advice carries no audit reference")
		}
		rep, err := r.aud.log.VerifyChain(bg, audit.ChainKey{ApplicationID: "app-sec", TenantID: "ten1"}, audit.VerifyOptions{})
		if err != nil || !rep.OK() || rep.Events != 3 {
			t.Fatalf("chain: events=%d ok=%v err=%v", rep.Events, rep.OK(), err)
		}
	})

	t.Run("a fallback after the scorer was reached records the invocation, the fallback and the routing", func(t *testing.T) {
		r := newRig(t)
		r.sc.Script(pickStep("c1", 0.5))
		adv := r.score(sc, r.request("ten1"))
		r.resolve(sc, adv)

		wantTypes(t, r, "decision.invoked", "decision.fallback", "decision.final_action")
		fb := r.aud.ofType("decision.fallback")[0].JSON
		mustContain(t, fb, `"reason":"low_confidence"`, "decision.fallback")
		mustContain(t, fb, `"invoked":true`, "decision.fallback")
		mustContain(t, r.aud.ofType("decision.final_action")[0].JSON, `"action":"fallback_routed"`, "decision.final_action")
		if adv.Result().AuditRef == "" {
			t.Fatal("the fallback advice carries no audit reference")
		}
	})

	t.Run("a rejected candidate set records a fallback that says the scorer was never reached", func(t *testing.T) {
		r := newRig(t)
		adv := r.score(sc, r.request("ten1", "c1", "d1"))
		r.resolve(sc, adv)

		wantTypes(t, r, "decision.fallback", "decision.final_action")
		fb := r.aud.ofType("decision.fallback")[0].JSON
		mustContain(t, fb, `"invoked":false`, "decision.fallback")
		mustContain(t, fb, `"reject_code":"cross_tenant"`, "decision.fallback")
	})

	t.Run("a refused selection records invoked, accepted and a refusal", func(t *testing.T) {
		r := newRig(t)
		adv := r.score(sc, r.request("ten1"))
		r.cat.revoke("c1")
		r.resolve(sc, adv)
		wantTypes(t, r, "decision.invoked", "decision.accepted", "decision.final_action")
		mustContain(t, r.aud.ofType("decision.final_action")[0].JSON, `"action":"selection_refused"`, "decision.final_action")
	})

	t.Run("a layer that is off records its fallback too", func(t *testing.T) {
		r := newRig(t, func(c *decision.Config) { c.Enabled = false })
		r.resolve(sc, r.score(sc, r.request("ten1")))
		wantTypes(t, r, "decision.fallback", "decision.final_action")
		mustContain(t, r.aud.ofType("decision.fallback")[0].JSON, `"reason":"disabled"`, "decision.fallback")
	})

	t.Run("no event holds a label, a candidate text, a state value or a user utterance", func(t *testing.T) {
		r := newRig(t)
		req := r.request("ten1")
		secret := "AUDIT-CANARY-TEXT-52c1"
		req.CandidateSet.Candidates[0].Text = &secret
		req.CandidateSet.Candidates[1].Label = "AUDIT-CANARY-LABEL-77aa"
		req.BoundedState["user_text"] = "AUDIT-CANARY-UTTERANCE-0c9f"
		r.resolve(sc, r.score(sc, req))
		if len(r.aud.all()) != 3 {
			t.Fatalf("audit events = %v", r.aud.types())
		}
		all := auditText(r.aud)
		for _, c := range []string{"AUDIT-CANARY-TEXT-52c1", "AUDIT-CANARY-LABEL-77aa", "AUDIT-CANARY-UTTERANCE-0c9f"} {
			mustNotContain(t, all, c, "the audit events")
		}
		mustContain(t, strings.Join(r.sc.requests(), ""), "AUDIT-CANARY-UTTERANCE-0c9f", "the scorer request (the control: the canary was in the request)")

		w := newWorld(t, withScript(pickStep("availability.list", 0.97)))
		w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"})
		w.say(recep(), injectedUtterance)
		if len(w.aud.all()) < 3 {
			t.Fatalf("dialogue audit events = %v", w.aud.types())
		}
		mustNotContain(t, auditText(w.aud), "Ignore previous instructions", "the dialogue layer's audit events")
		mustContain(t, strings.Join(w.scorer.requests(), ""), "Ignore previous instructions", "the scorer request (the control)")
	})

	t.Run("the dialogue layer audits a scored turn from the invocation to the final action, in the real log", func(t *testing.T) {
		w := newWorld(t, withScript(pickStep("availability.list", 0.97)))
		w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"})
		res := w.say(recep(), sayBook)
		if res.Trace.Decision != "selected" {
			t.Fatalf("decision = %q", res.Trace.Decision)
		}
		if got := strings.Join(w.aud.types(), ","); got != "decision.invoked,decision.accepted,decision.final_action" {
			t.Fatalf("audit events = %s", got)
		}
		sc := recep()
		rep, err := w.alog.VerifyChain(bg, audit.ChainKey{ApplicationID: sc.ApplicationID, TenantID: sc.TenantID}, audit.VerifyOptions{})
		if err != nil || !rep.OK() || rep.Events < 3 {
			t.Fatalf("chain: events=%d ok=%v err=%v", rep.Events, rep.OK(), err)
		}
	})

	t.Run("when the invocation cannot be recorded the scorer is not called", func(t *testing.T) {
		r := newRig(t)
		r.aud.failOn("decision.invoked")
		adv := r.score(sc, r.request("ten1"))
		wantFallback(t, adv, gen.FallbackReasonUnavailable)
		if r.sc.received() != 0 {
			t.Fatalf("the scorer was called %d times with no record of the call", r.sc.received())
		}
	})

	t.Run("when the acceptance cannot be recorded the proposal is not used", func(t *testing.T) {
		r := newRig(t)
		r.aud.failOn("decision.accepted")
		adv := r.score(sc, r.request("ten1"))
		wantFallback(t, adv, gen.FallbackReasonUnavailable)
		res := r.resolve(sc, adv)
		if len(res.Selected()) != 0 {
			t.Fatalf("selected %+v", res.Selected())
		}
	})

	t.Run("when the final action cannot be recorded the resolved selection is withheld", func(t *testing.T) {
		r := newRig(t)
		adv := r.score(sc, r.request("ten1"))
		wantAccepted(t, adv)
		r.aud.failOn("decision.final_action")
		res := r.resolve(sc, adv)
		if reason, ok := res.Fallback(); !ok || reason != gen.FallbackReasonUnavailable || len(res.Selected()) != 0 {
			t.Fatalf("selected %+v, fallback %q", res.Selected(), reason)
		}
	})

	t.Run("when the log is down altogether nothing is scored and nothing is selected", func(t *testing.T) {
		r := newRig(t)
		r.aud.failOn("*")
		adv := r.score(sc, r.request("ten1"))
		wantFallback(t, adv, gen.FallbackReasonUnavailable)
		if r.sc.received() != 0 || len(r.resolve(sc, adv).Selected()) != 0 {
			t.Fatal("work was done with no audit log")
		}
	})

	t.Run("a cache hit is audited like a call", func(t *testing.T) {
		r := newRig(t)
		r.score(sc, r.request("ten1"))
		r.score(sc, r.request("ten1"))
		inv := r.aud.ofType("decision.invoked")
		if len(inv) != 2 || !strings.Contains(inv[1].JSON, `"cache_status":"hit"`) || r.sc.received() != 1 {
			t.Fatalf("invoked events = %+v, scorer requests = %d", inv, r.sc.received())
		}
	})
}

// SEC-113
//
// A scorer is untrusted code. When it panics, Engine.Score recovers, appends a decision.fallback (typed reason
// unavailable, invoked=true) after the decision.invoked, and returns a fallback advice: the trail never ends at
// decision.invoked. The panic value is scorer-controlled text and never reaches the audit log.
func TestSecPanickingScorerLeavesATerminalAuditEvent(t *testing.T) {
	const canary = "PANIC-CANARY-77b2-not-for-the-audit-log"
	sc := scopeOf("ten1")

	t.Run("control: without the crash the same request ends in decision.accepted", func(t *testing.T) {
		r := newRig(t)
		if adv := r.score(sc, r.request("ten1")); !adv.Accepted() {
			t.Fatal("the control request was not accepted")
		}
		if got := strings.Join(r.aud.types(), ","); got != "decision.invoked,decision.accepted" {
			t.Fatalf("control trail = %s", got)
		}
	})

	t.Run("a crashed scorer ends the trail in a typed fallback", func(t *testing.T) {
		r := newRig(t)
		r.sc.crash = true
		var adv decision.Advice
		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("Engine.Score let a scorer panic escape: %v", v)
				}
			}()
			adv = r.score(sc, r.request("ten1"))
		}()
		if got := strings.Join(r.aud.types(), ","); got != "decision.invoked,decision.fallback" {
			t.Fatalf("the invocation has no terminal event: %v", r.aud.types())
		}
		if reason, ok := adv.FallbackReason(); !ok || reason != gen.FallbackReasonUnavailable || adv.Accepted() || len(adv.Suggestions()) != 0 {
			t.Fatalf("a crashed scorer did not produce a typed fallback: reason %q ok=%v accepted=%v", reason, ok, adv.Accepted())
		}
		fb := r.aud.ofType("decision.fallback")[0]
		if !strings.Contains(fb.JSON, `"invoked":true`) || !strings.Contains(fb.JSON, `"reason":"unavailable"`) || traceOf(t, fb) != adv.TraceID() {
			t.Errorf("fallback event = %s (trace %s)", fb.JSON, adv.TraceID())
		}
		if strings.Contains(fb.JSON, "crashed") {
			t.Errorf("the panic text reached the audit log: %s", fb.JSON)
		}
	})

	t.Run("the panic value never reaches the audit log, whatever it is", func(t *testing.T) {
		r := newRig(t)
		r.sc.crashWith = canary
		r.score(sc, r.request("ten1"))
		for _, e := range r.aud.all() {
			if strings.Contains(e.JSON, canary) {
				t.Fatalf("the panic value reached %s: %s", e.Type, e.JSON)
			}
		}
	})

	t.Run("the layer keeps working and the crash is not cached", func(t *testing.T) {
		r := newRig(t)
		r.sc.crash = true
		req := r.request("ten1")
		r.score(sc, req)
		if adv := r.score(sc, req); !adv.Accepted() {
			t.Fatal("the request after a crash was not accepted: the crash was cached or the engine is wedged")
		}
		if got := strings.Join(r.aud.types(), ","); got != "decision.invoked,decision.fallback,decision.invoked,decision.accepted" {
			t.Fatalf("trail = %s", got)
		}
	})
}
