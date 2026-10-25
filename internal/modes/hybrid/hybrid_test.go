package hybrid

import (
	"context"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providerreg"

	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestExternalRealizationUsesVerifiedDirectiveOnly(t *testing.T) {
	t.Run("a verified directive is sent as facts only and the verified text is shown", func(t *testing.T) {
		w := newWorld(t)
		d, r := outcomeDirective()
		text := "Done: your appointment with Dr. Bianchi on 14 October 2026 is booked."
		if _, err := verify.Verify(text, d, r, enOpts); err != nil {
			t.Fatal(err)
		}
		w.reply(text)
		got := w.realise(w.turn(d, r))
		if got.Realiser != RealiserExternal || got.Source != verify.SourceModel || got.Text != text {
			t.Fatalf("got %q by %s/%s, want the external text", got.Text, got.Realiser, got.Source)
		}
		if n := len(w.sender.sent()); n != 1 {
			t.Fatalf("sender calls = %d, want 1", n)
		}
		if w.local.Calls() != 0 {
			t.Error("the local model was called for a turn the external provider served")
		}
		if len(got.Transitions) != 0 {
			t.Errorf("unexpected transitions %+v", got.Transitions)
		}
		cats := w.categories()
		want := []string{"dialogue_directive", "action_receipt"}
		if strings.Join(cats, ",") != strings.Join(want, ",") {
			t.Errorf("sent categories %v, want only %v", cats, want)
		}
		sent := w.sentText()
		for _, must := range []string{"Dr. Bianchi", "2026-10-14", "committed"} {
			if !strings.Contains(sent, must) {
				t.Errorf("the envelope lacks %q", must)
			}
		}
		for _, never := range []string{turnID, receiptID, prepID, "appointments.book", "aud_01J"} {
			if strings.Contains(sent, never) {
				t.Errorf("the envelope carries the internal id %q", never)
			}
		}
		rq := w.sender.sent()[0]
		if len(rq.Rich.Tools) != 0 || rq.Features.Any() {
			t.Errorf("the realisation request offered tools %d or native features %+v", len(rq.Rich.Tools), rq.Features)
		}
	})

	t.Run("a directive the verifier refuses is never sent", func(t *testing.T) {
		w := newWorld(t)
		d := resultDirective()
		d.MustState = append(d.MustState, "not_a_fact")
		w.reply("anything")
		_, err := w.h.Realise(ctxb(), w.turn(d, nil))
		if err == nil || contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Fatalf("err = %v, want schema_invalid", err)
		}
		if len(w.sender.sent()) != 0 || len(w.budgets.reserved) != 0 || w.guard.count() != 0 {
			t.Error("a request left, or was reserved or evaluated, for a directive that does not verify")
		}
		if _, err := w.h.Realise(ctxb(), Turn{Scope: w.scope, Options: enOpts}); err == nil {
			t.Error("a turn without a directive was realised")
		}
	})

	t.Run("a tool call in the reply is dropped, never run, and the turn is realised locally", func(t *testing.T) {
		w := newWorld(t)
		d := resultDirective()
		w.ext.On(pfake.Any()).ProposeToolCall("call_1", "appointments.cancel", `{"id":"x"}`).Times(1)
		w.localReply(rendered(t, d, nil))
		got := w.realise(w.turn(d, nil))
		if got.Outcome.Target != modes.TargetLocal || len(got.Outcome.Hops) != 1 || got.Outcome.Hops[0].Condition != modes.CondMalformedOutput {
			t.Fatalf("outcome %+v, want a local hop on malformed_output", got.Outcome)
		}
		if got.Source != verify.SourceModel || got.Realiser != RealiserLocalModel {
			t.Errorf("text by %s/%s, want the verified local text", got.Realiser, got.Source)
		}
	})
}

func TestSensitiveSourceStaysLocalInHybrid(t *testing.T) {
	const secretClaim = "The door code of the pharmacy store is 4417."
	const openClaim = "Cancellations need twenty four hours notice."

	// run realises a directive citing one source of the given mode and returns the world and result.
	run := func(t *testing.T, mode pv.EgressMode, class pv.DataClassification, register bool, claim string) (*world, Realised) {
		w := newWorld(t)
		id := pv.SourceID(ids.New("src")) // well formed, never registered: no policy is local_only
		if register {
			id = w.newSource(mode, class)
		}
		d := resultDirective(citation(id, "Clinic handbook", claim))
		w.reply(rendered(t, d, nil)).Usage(100, 20)
		w.localReply(rendered(t, d, nil))
		return w, w.realise(w.turn(d, nil))
	}

	t.Run("control: an allow_excerpt source is sent as an excerpt", func(t *testing.T) {
		w, got := run(t, pv.EgressModeAllowExcerpt, pv.DataClassificationInternal, true, openClaim)
		if got.Realiser != RealiserExternal || len(w.sender.sent()) != 1 {
			t.Fatalf("realiser %s with %d sends, want the external provider", got.Realiser, len(w.sender.sent()))
		}
		if !strings.Contains(w.sentText(), openClaim) {
			t.Error("the claims of an allow_excerpt source were not sent")
		}
		if !contains(w.categories(), "rag_excerpt") {
			t.Errorf("categories %v lack the excerpt", w.categories())
		}
	})

	t.Run("an allow_metadata source sends its name and never its claims", func(t *testing.T) {
		w, got := run(t, pv.EgressModeAllowMetadata, pv.DataClassificationInternal, true, secretClaim)
		if got.Realiser != RealiserExternal {
			t.Fatalf("realiser %s, want external", got.Realiser)
		}
		sent := w.sentText()
		if strings.Contains(sent, secretClaim) || strings.Contains(sent, "4417") {
			t.Error("the claims of a metadata-only source left the deployment")
		}
		if !strings.Contains(sent, "Clinic handbook") || !contains(w.categories(), "rag_metadata") {
			t.Errorf("the source name was not sent as metadata: %v", w.categories())
		}
	})

	for _, tc := range []struct {
		name     string
		mode     pv.EgressMode
		register bool
	}{
		{"local_only source", pv.EgressModeLocalOnly, true},
		{"denied source", pv.EgressModeDeny, true},
		{"source with no policy", "", false},
	} {
		t.Run(tc.name+" is realised locally and nothing is sent", func(t *testing.T) {
			w, got := run(t, tc.mode, pv.DataClassificationInternal, tc.register, secretClaim)
			if n := len(w.sender.sent()); n != 0 || w.ext.Calls() != 0 {
				t.Fatalf("sender calls = %d, provider calls = %d, want none", n, w.ext.Calls())
			}
			if strings.Contains(w.sentText(), "4417") || len(w.budgets.reserved) != 0 {
				t.Error("source content left, or a reservation was held for a request that was refused")
			}
			if got.Outcome.Target != modes.TargetLocal || got.Outcome.DataSent {
				t.Errorf("outcome %+v, want local with no data sent", got.Outcome)
			}
			if len(got.Outcome.Hops) != 1 || got.Outcome.Hops[0].Condition != modes.CondEgressDenied {
				t.Errorf("hops %+v, want one local hop on egress_denied", got.Outcome.Hops)
			}
			if got.Realiser != RealiserLocalModel || got.Source != verify.SourceModel {
				t.Errorf("text by %s/%s, want the verified local text", got.Realiser, got.Source)
			}
			if tr := w.auditedTransitions(); len(tr) != 1 || tr[0].Kind != TransitionLocalFallback || tr[0].Condition != "egress_denied" {
				t.Errorf("audited transitions %+v, want one local_fallback on egress_denied", tr)
			}
		})
	}

	t.Run("without the policy naming egress_denied the turn shows the template and still sends nothing", func(t *testing.T) {
		pol := modes.StaticPolicy{modes.RoleRealisation: {Profiles: []pv.ProfileID{testProfile}}}
		w := newWorld(t, withPolicy(pol))
		id := w.newSource(pv.EgressModeLocalOnly, pv.DataClassificationInternal)
		d := resultDirective(citation(id, "Clinic handbook", secretClaim))
		got := w.realise(w.turn(d, nil))
		if len(w.sender.sent()) != 0 || w.local.Calls() != 0 {
			t.Error("a request was sent, or the local model used where no fallback is named")
		}
		if got.Outcome.Kind != modes.KindEgressDenied || got.Realiser != RealiserTemplate || got.Text == "" {
			t.Errorf("outcome %s realiser %s text %q, want egress_denied and the template", got.Outcome.Kind, got.Realiser, got.Text)
		}
	})
}

func TestLocalControllerHandlesPrivacySensitiveTask(t *testing.T) {
	t.Run("a confidential turn is realised locally and never reaches the guard", func(t *testing.T) {
		w := newWorld(t)
		d := resultDirective()
		w.reply(rendered(t, d, nil))
		w.localReply(rendered(t, d, nil))
		tn := w.turn(d, nil)
		tn.Class = pv.DataClassificationConfidential
		got := w.realise(tn)
		if w.guard.count() != 0 || len(w.sender.sent()) != 0 || w.ext.Calls() != 0 {
			t.Fatalf("guard calls %d, sends %d: a confidential turn went toward the provider", w.guard.count(), len(w.sender.sent()))
		}
		if got.Outcome.Reason != "hybrid_sensitive_local" || got.Outcome.Target != modes.TargetLocal || got.Realiser != RealiserLocalModel {
			t.Errorf("outcome %+v realiser %s, want hybrid_sensitive_local served by the local model", got.Outcome, got.Realiser)
		}
		// Control: the same turn at the configured class goes out.
		got = w.realise(w.turn(d, nil))
		if got.Realiser != RealiserExternal || len(w.sender.sent()) != 1 {
			t.Errorf("control: realiser %s with %d sends, want the external provider", got.Realiser, len(w.sender.sent()))
		}
	})

	t.Run("an unclassified deployment keeps every turn local", func(t *testing.T) {
		w := newWorld(t, withFactClass(""))
		d := resultDirective()
		w.reply(rendered(t, d, nil))
		got := w.realise(w.turn(d, nil))
		if len(w.sender.sent()) != 0 || got.Outcome.Reason != "hybrid_sensitive_local" {
			t.Errorf("sends %d reason %s, want a local turn", len(w.sender.sent()), got.Outcome.Reason)
		}
		if got.Text == "" || got.Realiser != RealiserTemplate {
			t.Errorf("realiser %s text %q, want the template when the local model has no reply", got.Realiser, got.Text)
		}
	})

	t.Run("the controller, embeddings, speech and routing roles are served locally whatever the policy lists", func(t *testing.T) {
		w := newWorld(t)
		pol := w.policy
		pol[modes.RoleSTT] = modes.RolePolicy{Profiles: []pv.ProfileID{testProfile}}
		pol[modes.RoleTTS] = modes.RolePolicy{Profiles: []pv.ProfileID{testProfile}}
		pinnedReason := map[modes.Role]string{
			modes.RoleController: "no_external_route", modes.RoleSTT: "no_external_route", modes.RoleTTS: "no_external_route",
			modes.RoleEmbedder: "role_local_only", modes.RoleReranker: "role_local_only", modes.RoleVAD: "role_local_only",
			modes.RoleDecisionScorer: "decision_scorer_local_only",
		}
		for _, role := range []modes.Role{modes.RoleController, modes.RoleEmbedder, modes.RoleSTT, modes.RoleTTS, modes.RoleDecisionScorer, modes.RoleReranker, modes.RoleVAD} {
			w.locals[role] = true
			ran := ""
			req := modes.Request{
				Scope: w.scope, Role: role, Requirement: providerreg.Requirement{DataClass: pv.DataClassificationInternal},
				Estimate: func(pv.ProviderProfile) (providerbudget.Units, pv.MicroUnits, error) {
					return providerbudget.Units{}, 1, nil
				},
			}
			out, err := w.h.Router().Execute(ctxb(), req, modes.Executors{
				Local: func(context.Context) error { ran = "local"; return nil },
				External: func(context.Context, modes.ExternalCall) (modes.ExternalResult, error) {
					ran = "external"
					return modes.ExternalResult{}, nil
				},
			})
			if err != nil || out.Target != modes.TargetLocal || ran != "local" {
				t.Errorf("role %s: ran %q outcome %+v err %v, want local", role, ran, out, err)
			}
			if want := pinnedReason[role]; out.Reason != want {
				t.Errorf("role %s: reason %q, want %q", role, out.Reason, want)
			}
		}
		if w.guard.count() != 0 || len(w.sender.sent()) != 0 {
			t.Error("a local role reached the guard or the provider")
		}
	})

	t.Run("outside Hybrid mode realisation is local, External included", func(t *testing.T) {
		for _, m := range []modes.Mode{modes.ModeLocal, modes.ModeExternal} {
			w := newWorld(t, withMode(m))
			d := resultDirective()
			w.reply(rendered(t, d, nil))
			got := w.realise(w.turn(d, nil))
			if len(w.sender.sent()) != 0 || got.Outcome.Target != modes.TargetLocal {
				t.Errorf("mode %s: sends %d target %s, want local", m, len(w.sender.sent()), got.Outcome.Target)
			}
		}
	})
}

func TestOutageFallsBackToLocalAndIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail pfake.FailKind
		cond modes.Condition
	}{
		{"unavailable", pfake.FailUnavailable, modes.CondUnavailable},
		{"timeout", pfake.FailTimeout, modes.CondTimeout},
		{"rate limited", pfake.FailRateLimited, modes.CondRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			d := resultDirective()
			w.ext.FailNext(tc.fail)
			w.localReply(rendered(t, d, nil))
			got := w.realise(w.turn(d, nil))
			if !got.Outcome.Served() || got.Outcome.Target != modes.TargetLocal {
				t.Fatalf("outcome %+v, want served locally", got.Outcome)
			}
			if len(got.Outcome.Hops) != 1 || got.Outcome.Hops[0].To != modes.LocalHop || got.Outcome.Hops[0].From != string(testProfile) || got.Outcome.Hops[0].Condition != tc.cond {
				t.Errorf("hops %+v, want %s -> local on %s", got.Outcome.Hops, testProfile, tc.cond)
			}
			if got.Realiser != RealiserLocalModel || got.Source != verify.SourceModel {
				t.Errorf("text by %s/%s, want the verified local text", got.Realiser, got.Source)
			}
			want := Transition{Kind: TransitionLocalFallback, Reason: "fallback_" + string(tc.cond), From: string(testProfile), To: "local", Condition: string(tc.cond), Outcome: "served", Mode: "hybrid", Act: "present_result", DataSent: true}
			if len(got.Transitions) != 1 || !sameTransition(got.Transitions[0], want) {
				t.Errorf("transitions %+v, want %+v", got.Transitions, want)
			}
			if a := w.auditedTransitions(); len(a) != 1 || !sameTransition(a[0], want) {
				t.Errorf("audited %+v, want %+v", a, want)
			}
			if got.RecordFailed {
				t.Error("the transition was not recorded")
			}
		})
	}

	t.Run("the local model failing too leaves the template, recorded", func(t *testing.T) {
		w := newWorld(t)
		d := resultDirective()
		w.ext.FailNext(pfake.FailUnavailable)
		w.local.FailNext(pfake.FailUnavailable)
		got := w.realise(w.turn(d, nil))
		if got.Realiser != RealiserTemplate || got.Text != mustTemplate(t, d, nil) {
			t.Fatalf("realiser %s text %q, want the template", got.Realiser, got.Text)
		}
		tr := w.auditedTransitions()
		if len(tr) != 2 || tr[0].Kind != TransitionLocalFallback || tr[1].Kind != TransitionTemplateFallback || tr[1].From != "local" {
			t.Errorf("audited %+v, want a local hop then a template fallback", tr)
		}
	})

	t.Run("no named fallback: the turn shows the template and never the local model", func(t *testing.T) {
		pol := modes.StaticPolicy{modes.RoleRealisation: {Profiles: []pv.ProfileID{testProfile}}}
		w := newWorld(t, withPolicy(pol))
		d := resultDirective()
		w.ext.FailNext(pfake.FailUnavailable)
		w.localReply(rendered(t, d, nil))
		got := w.realise(w.turn(d, nil))
		if w.local.Calls() != 0 || got.Outcome.Kind != modes.KindUnavailable || len(got.Outcome.Hops) != 0 {
			t.Fatalf("local calls %d outcome %+v, want unavailable with no hop", w.local.Calls(), got.Outcome)
		}
		if got.Realiser != RealiserTemplate || got.Text != mustTemplate(t, d, nil) {
			t.Errorf("realiser %s, want the template", got.Realiser)
		}
		if a := w.auditedTransitions(); len(a) != 1 || a[0].Kind != TransitionTemplateFallback || a[0].Outcome != "unavailable" {
			t.Errorf("audited %+v, want one template fallback on unavailable", a)
		}
	})

	t.Run("an exhausted budget ends the external route with the template, not another provider", func(t *testing.T) {
		w := newWorld(t)
		w.budgets.refuse = contracts.New(contracts.ErrResourceExhausted, "budget").WithDetails("reason", "hard_stop")
		d := resultDirective()
		w.localReply(rendered(t, d, nil))
		got := w.realise(w.turn(d, nil))
		if got.Outcome.Kind != modes.KindBudgetExhausted || w.local.Calls() != 0 || len(w.sender.sent()) != 0 {
			t.Fatalf("outcome %s, local calls %d, sends %d, want budget_exhausted and no call", got.Outcome.Kind, w.local.Calls(), len(w.sender.sent()))
		}
		if got.Realiser != RealiserTemplate || got.Text == "" {
			t.Errorf("realiser %s text %q, want the template", got.Realiser, got.Text)
		}
	})
}

func TestRealizedTextFailingVerificationFallsBackToTemplate(t *testing.T) {
	d := resultDirective()
	for _, tc := range []struct {
		name string
		text string
	}{
		{"an invented amount", rendered(t, d, nil) + " The late fee is 75.00 EUR."},
		{"a missing fact", "Your appointment is with Dr. Bianchi."},
		{"an invented identifier", rendered(t, d, nil) + " Your booking reference is BK-4471."},
		{"a completion claim without a receipt", rendered(t, d, nil) + " Your appointment has been booked."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.reply(tc.text)
			w.localReply(rendered(t, d, nil))
			got := w.realise(w.turn(d, nil))
			if got.Source != verify.SourceTemplate || got.Reason != verify.ReasonVerificationFailed || got.Realiser != RealiserTemplate {
				t.Fatalf("got %s/%s by %s, want the template after a failed verification", got.Source, got.Reason, got.Realiser)
			}
			if got.Text != mustTemplate(t, d, nil) {
				t.Errorf("text %q is not the template", got.Text)
			}
			if len(got.Violations) == 0 {
				t.Error("no violation was reported")
			}
			if len(w.sender.sent()) != 1 || w.local.Calls() != 0 {
				t.Errorf("sends %d local calls %d: a failed verification must not trigger a second attempt", len(w.sender.sent()), w.local.Calls())
			}
			tr := w.auditedTransitions()
			if len(tr) != 1 || tr[0].Kind != TransitionTemplateFallback || tr[0].Reason != ReasonVerificationFailed || len(tr[0].Violations) == 0 {
				t.Errorf("audited %+v, want one template_fallback verification_failed with violation kinds", tr)
			}
			if strings.Contains(strings.Join(tr[0].Violations, " "), "75") {
				t.Error("the audit record carries the offending text")
			}
			sum, _ := w.h.Usage(w.scope)
			if sum.VerificationFailures != 1 || sum.TemplateFallbacks != 1 {
				t.Errorf("summary %+v, want one verification failure", sum)
			}
		})
	}

	t.Run("control: the rendered text itself is accepted", func(t *testing.T) {
		w := newWorld(t)
		w.reply(rendered(t, d, nil))
		got := w.realise(w.turn(d, nil))
		if got.Source != verify.SourceModel || got.Reason != verify.ReasonVerified || len(got.Transitions) != 0 {
			t.Errorf("got %s/%s with %d transitions, want a verified external text", got.Source, got.Reason, len(got.Transitions))
		}
	})
}

func TestHybridCostAndUsageExposed(t *testing.T) {
	w := newWorld(t)
	d := resultDirective()

	// Turn 1: the provider reports its cost.
	w.reply(rendered(t, d, nil)).Usage(120, 30).Cost(2500, "USD")
	first := w.realise(w.turn(d, nil))
	u := first.Usage
	if u.ExternalCalls != 1 || u.ToksIn != 120 || u.ToksOut != 30 || u.CostMicros != 2500 || u.CostBasis != CostBasisReported || u.Currency != "USD" {
		t.Errorf("turn usage %+v, want 120/30 toks, 2500 USD micro-units reported", u)
	}
	if u.ReservedMicros <= 0 || u.ReservationID == "" {
		t.Errorf("turn usage %+v lacks the reserved upper bound or the reservation id", u)
	}
	if len(w.budgets.reserved) != 1 || len(w.budgets.settled) != 1 {
		t.Fatalf("reservations %d settlements %d, want 1 and 1", len(w.budgets.reserved), len(w.budgets.settled))
	}

	// Turn 2: no reported cost, so the reserved upper bound is the charge and says so.
	w.reply(rendered(t, d, nil)).Usage(100, 20)
	second := w.realise(w.turn(d, nil))
	if second.Usage.CostBasis != CostBasisEstimate || second.Usage.CostMicros != second.Usage.ReservedMicros || second.Usage.CostMicros <= 0 {
		t.Errorf("turn usage %+v, want the estimate as an upper bound", second.Usage)
	}

	// Turn 3: a local turn costs nothing external.
	tn := w.turn(d, nil)
	tn.Class = pv.DataClassificationConfidential
	third := w.realise(tn)
	if third.Usage.ExternalCalls != 0 || third.Usage.CostBasis != CostBasisNone || third.Usage.CostMicros != 0 {
		t.Errorf("a local turn reports usage %+v", third.Usage)
	}

	w.usage.export = providerbudget.UsageExport{Totals: []providerbudget.UsageTotal{{ProviderID: "openai", Channel: pv.ChannelInteractive, Currency: "USD", Records: 2}}}
	rep, err := w.h.Report(ctxb(), w.scope, providerbudget.UsageFilter{ProviderID: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	s := rep.Summary
	if s.Turns != 3 || s.ExternalTurns != 2 || s.ExternalCalls != 2 || s.ToksIn != 220 || s.ToksOut != 50 {
		t.Errorf("summary %+v, want 3 turns, 2 external, 220/50 toks", s)
	}
	if len(s.Spend) != 1 || s.Spend[0].Currency != "USD" || s.Spend[0].ReportedMicros != 2500 || s.Spend[0].EstimatedMicros != second.Usage.CostMicros {
		t.Errorf("spend %+v, want 2500 reported and the second estimate", s.Spend)
	}
	if s.ByOutcome["served"] != 3 {
		t.Errorf("by outcome %+v", s.ByOutcome)
	}
	if rep.Provider == nil || len(rep.Provider.Totals) != 1 || w.usage.filter.ProviderID != "openai" {
		t.Errorf("provider export %+v with filter %+v was not passed through", rep.Provider, w.usage.filter)
	}

	// The totals are per tenant: another tenant sees none of it.
	other := w.scope.Clone()
	other.TenantID = "grp-other-9921"
	if o, err := w.h.Usage(other); err != nil || o.Turns != 0 {
		t.Errorf("another tenant sees %+v (err %v)", o, err)
	}
	if _, err := w.h.Usage(contracts.Scope{}); err == nil {
		t.Error("usage was returned for an invalid scope")
	}
}

// ---- helpers ----

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sameTransition(a, b Transition) bool {
	return a.Kind == b.Kind && a.Reason == b.Reason && a.From == b.From && a.To == b.To && a.Condition == b.Condition &&
		a.Outcome == b.Outcome && a.Mode == b.Mode && a.Act == b.Act && a.DataSent == b.DataSent
}

func mustTemplate(t *testing.T, d *conversation.DialogueDirective, r *actions.ActionReceipt) string {
	t.Helper()
	o, err := verify.Degraded(d, r, enOpts)
	if err != nil {
		t.Fatal(err)
	}
	return o.Text
}

func TestControllerLocalPolicyOnlyStripsTheControllerUnderHybrid(t *testing.T) {
	inner := modes.StaticPolicy{
		modes.RoleController: {Profiles: []pv.ProfileID{testProfile}},
		modes.RoleVision:     {Profiles: []pv.ProfileID{testProfile}},
	}
	for _, tc := range []struct {
		mode           modes.Mode
		wantController int
		wantVision     int
	}{
		{modes.ModeHybrid, 0, 1},
		{modes.ModeExternal, 1, 1},
	} {
		w := newWorld(t, withMode(tc.mode))
		res, err := modes.NewResolver(w.modes, w.eg)
		if err != nil {
			t.Fatal(err)
		}
		pol := ControllerLocalPolicy(inner, res)
		for role, want := range map[modes.Role]int{modes.RoleController: tc.wantController, modes.RoleVision: tc.wantVision} {
			rp, err := pol.RolePolicy(ctxb(), w.scope, role)
			if err != nil || len(rp.Profiles) != want {
				t.Errorf("mode %s role %s: %d profiles (err %v), want %d", tc.mode, role, len(rp.Profiles), err, want)
			}
		}
	}
}

// namedModel is the external provider reporting another model name in its replies.
type namedModel struct {
	providers.MultimodalGenerator
	name string
}

func (n namedModel) Generate(ctx context.Context, scope contracts.Scope, req providers.RichRequest) (providers.RichResult, error) {
	res, err := n.MultimodalGenerator.Generate(ctx, scope, req)
	res.Model = n.name
	return res, err
}

// SEC-128: the model a realisation reply names is compared with the pinned model; another model's text is discarded.
func TestRealisationReplyOfAnotherModelIsDiscarded(t *testing.T) {
	d := resultDirective()
	for _, name := range []string{"gpt-5-pro", ""} {
		w := newWorld(t)
		w.sender.gen = namedModel{MultimodalGenerator: w.ext, name: name}
		w.reply(rendered(t, d, nil))
		w.localReply(rendered(t, d, nil))
		got := w.realise(w.turn(d, nil))
		if got.Source == verify.SourceModel && got.Realiser == RealiserExternal {
			t.Fatalf("model %q: the text of a reply from another model was accepted", name)
		}
		if len(w.sender.sent()) != 1 {
			t.Fatalf("model %q: sends %d", name, len(w.sender.sent()))
		}
	}

	t.Run("control: the pinned model's reply is accepted", func(t *testing.T) {
		w := newWorld(t)
		w.sender.gen = namedModel{MultimodalGenerator: w.ext, name: "gpt-4.1-mini-2025-04-14"}
		w.reply(rendered(t, d, nil))
		got := w.realise(w.turn(d, nil))
		if got.Source != verify.SourceModel || got.Reason != verify.ReasonVerified {
			t.Fatalf("got %s/%s", got.Source, got.Reason)
		}
	})
}
