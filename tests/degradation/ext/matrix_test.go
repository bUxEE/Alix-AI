package ext

import (
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerreg"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// One test per row of the failure and fallback table of the External Provider Mode additive plan, section 13. Every
// test has a healthy control in the same world shape, so a failure that cannot happen cannot pass.

// healthy runs the control: with nothing failing the booking is proposed by the external provider, validated, and
// prepared for the user's confirmation, never committed.
func healthy(t *testing.T, w *world) {
	t.Helper()
	w.proposeBook()
	res := w.say(receptionist(), sayBook)
	wantAct(t, res, "request_confirmation")
	if res.Prepared == nil || w.snd.sends() != 1 {
		t.Fatalf("control: prepared %v sends %d: the healthy flow must reach the provider and prepare, or the failures below prove nothing", res.Prepared, w.snd.sends())
	}
	if o := w.lastOutcome(); !o.Served() || o.Target != modes.TargetExternal {
		t.Fatalf("control: outcome %+v", o)
	}
	if _, _, commits := w.counter.counts(); commits != 0 {
		t.Fatalf("control: %d commits before any confirmation", commits)
	}
}

// ---- Provider unavailable ----
// Required: use an approved local fallback, queue safe work or report unavailable; never pretend completion.

func TestExtProviderUnavailable(t *testing.T) {
	t.Run("control", func(t *testing.T) { healthy(t, newWorld(t)) })

	t.Run("no fallback named: reported unavailable, nothing completed, no hidden fallback", func(t *testing.T) {
		w := newWorld(t)
		w.ext.On(offersAnyTool()).Fail(pfake.FailUnavailable)
		res := w.say(receptionist(), sayBook)

		w.failedWith(res, contracts.ErrUnavailable)
		wantAct(t, res, "report_failure")
		if res.Response.Source != verify.SourceTemplate {
			t.Fatalf("response %+v: a clear failure is the core's own words, not model text", res.Response)
		}
		w.nothingExecuted(res)
		o := w.lastOutcome()
		if o.Kind != modes.KindUnavailable || o.Target != modes.TargetNone || len(o.Hops) != 0 || !o.AIActionsDisabled {
			t.Fatalf("outcome %+v", o)
		}
		if w.local.Calls() != 0 {
			t.Fatalf("the local runtime was called %d times although no fallback is named", w.local.Calls())
		}
		if w.snd.sends() != 1 {
			t.Fatalf("sends = %d, want exactly one attempt", w.snd.sends())
		}
	})

	t.Run("the approved local fallback serves, and the hop is recorded", func(t *testing.T) {
		w := newWorld(t, withLocalFallback(modes.CondUnavailable))
		w.ext.On(offersAnyTool()).Fail(pfake.FailUnavailable)
		w.localProposeBook()
		res := w.say(receptionist(), sayBook)

		wantAct(t, res, "request_confirmation")
		if res.Prepared == nil {
			t.Fatal("the local fallback did not carry the request to a prepared action")
		}
		o := w.lastOutcome()
		if !o.Served() || o.Target != modes.TargetLocal || len(o.Hops) != 1 || o.Hops[0].To != modes.LocalHop || o.Hops[0].Condition != modes.CondUnavailable {
			t.Fatalf("outcome %+v: a fallback must be taken only as named, and recorded", o)
		}
		if _, _, commits := w.counter.counts(); commits != 0 {
			t.Fatalf("%d commits: a fallback turn still waits for the user's confirmation", commits)
		}
	})

	t.Run("a fallback is not taken for a condition policy did not name", func(t *testing.T) {
		w := newWorld(t, withLocalFallback(modes.CondTimeout))
		w.ext.On(offersAnyTool()).Fail(pfake.FailUnavailable)
		w.localProposeBook()
		res := w.say(receptionist(), sayBook)

		w.failedWith(res, contracts.ErrUnavailable)
		if w.local.Calls() != 0 {
			t.Fatal("the local runtime served a condition policy did not name")
		}
		w.nothingExecuted(res)
	})
}

// ---- Network lost during request ----
// Required: cancel or mark uncertain; do not duplicate an application commit.

func TestExtNetworkLostDuringRequest(t *testing.T) {
	// Every fallback policy could name is named: none of them may apply to an outcome nobody knows.
	w := newWorld(t, withLocalFallback(modes.CondUnavailable, modes.CondTimeout, modes.CondRateLimited, modes.CondMalformedOutput))
	w.localProposeBook()
	w.proposeBook()
	w.snd.loseConnection(connectionReset) // the provider processed the request; the reply never arrived

	res := w.say(receptionist(), sayBook)

	w.failedWith(res, contracts.ErrUncertainOutcome)
	wantAct(t, res, "report_failure")
	w.nothingExecuted(res)
	o := w.lastOutcome()
	if o.Kind != modes.KindUncertain || !o.DataSent || len(o.Hops) != 0 || o.Target != modes.TargetNone || !o.AIActionsDisabled {
		t.Fatalf("outcome %+v: want uncertain_outcome, data sent, no hop", o)
	}
	if w.snd.sends() != 1 {
		t.Fatalf("sends = %d: a request that may have been processed is never sent again", w.snd.sends())
	}
	if w.local.Calls() != 0 {
		t.Fatal("the unknown outcome was handed to the local runtime")
	}
	if a, ok := w.budget.lastSettled(); !ok || a.Outcome != pv.UsageRecordOutcomeUncertain {
		t.Fatalf("settlement %+v: the spend of an unknown outcome is recorded as uncertain, at the reserved bound", a)
	}

	// The network is back. The user asks again: the booking is prepared, confirmed and committed exactly once,
	// and confirming the same preparation again commits nothing more.
	w.snd.loseConnection(nil)
	w.proposeBook()
	prep := w.say(receptionist(), sayBook)
	wantAct(t, prep, "request_confirmation")
	if prep.Prepared == nil {
		t.Fatal("nothing was prepared after the network came back")
	}
	done := w.confirm(receptionist(), prep)
	if done.Receipt == nil || done.Receipt.Status != act.ActionReceiptStatusCommitted {
		t.Fatalf("receipt %+v code %q", done.Receipt, done.Code)
	}
	if _, _, commits := w.counter.counts(); commits != 1 {
		t.Fatalf("commits = %d, want exactly 1", commits)
	}
	again, err := w.eng.Turn(ctxb(), receptionist(), confirmTurn(prep))
	if err == nil && again.Receipt != nil && again.Receipt.Status == act.ActionReceiptStatusCommitted && again.Code == "" {
		t.Fatalf("a second confirmation produced another committed receipt: %+v", again.Receipt)
	}
	if _, _, commits := w.counter.counts(); commits != 1 {
		t.Fatalf("commits = %d after a repeated confirmation, want 1: a commit was duplicated", commits)
	}
}

// ---- Provider timeout ----
// Required: return a typed timeout and preserve the local goal state.

func TestExtProviderTimeout(t *testing.T) {
	t.Run("a timed-out response is a typed timeout, the answer is the core's, the goal state is untouched", func(t *testing.T) {
		w := newWorld(t, withModelReplies())
		w.ext.On(offersAnyTool()).ProposeToolCall("call_1", "availability_list", `{"date":"2026-10-14"}`).Times(1)
		w.ext.On(pfake.Not(offersAnyTool())).Fail(pfake.FailTimeout)
		res := w.say(receptionist(), "Which appointments are free on 2026-10-14?")

		// The turn is answered from the host's facts in the core's own words; no model text stands in for them.
		wantAct(t, res, "present_result")
		if res.Code != "" || res.Response.Source != verify.SourceTemplate || !strings.Contains(res.Response.Text, "2026-10-14 09:00") {
			t.Fatalf("code %q response %+v", res.Code, res.Response)
		}
		o := w.lastOutcome()
		if o.Kind != modes.KindTimeout || o.Reason != "provider_timeout" || o.Served() || len(o.Hops) != 0 {
			t.Fatalf("outcome %+v, want a typed timeout", o)
		}
		if contracts.CodeOf(o.Err()) != contracts.ErrTimeout {
			t.Fatalf("error code %q, want timeout", contracts.CodeOf(o.Err()))
		}
		if w.snd.sends() != 2 {
			t.Fatalf("sends = %d, want 2: the proposal and one attempt at the response, no retry", w.snd.sends())
		}
		g := w.goal()
		if g == nil || g.CapabilityID != "availability.list" || g.Slots["date"].Value != "2026-10-14" {
			t.Fatalf("goal %+v: the local goal state must be preserved", g)
		}
		if _, _, commits := w.counter.counts(); commits != 0 {
			t.Fatalf("%d commits", commits)
		}
	})

	t.Run("a timed-out proposal fails the turn, typed, and the goal in progress survives", func(t *testing.T) {
		w := newWorld(t)
		// The first turn leaves a goal behind: the provider proposes the booking with one field missing.
		w.ext.On(offersAnyTool()).ProposeToolCall("call_1", "appointments_book", `{"patient":"Luca Ferri"}`).Times(1)
		wantAct(t, w.say(receptionist(), sayBook), "ask_missing")
		before := w.goal()
		if before == nil || before.CapabilityID != "appointments.book" || len(before.Slots) == 0 {
			t.Fatalf("control: goal %+v, want the booking goal with its first slot", before)
		}

		w.ext.FailNext(pfake.FailTimeout)
		res := w.say(receptionist(), "at 2026-10-14 10:30")

		// A proposal can drive a prepare and a confirmation, so a timeout after the request left is reported as
		// the router reports any request that may have been processed: uncertain, with the timeout as its reason.
		w.failedWith(res, contracts.ErrUncertainOutcome)
		wantAct(t, res, "report_failure")
		w.nothingExecuted(res)
		if o := w.lastOutcome(); o.Kind != modes.KindUncertain || o.Reason != "timeout_after_send" || !o.AIActionsDisabled {
			t.Fatalf("outcome %+v", o)
		}
		after := w.goal()
		if after == nil || after.CapabilityID != before.CapabilityID || len(after.Slots) != len(before.Slots) {
			t.Fatalf("goal %+v after the timeout, was %+v: the local goal state must survive", after, before)
		}
		if w.snd.sends() != 2 {
			t.Fatalf("sends = %d, want 2 (one per request, no retry of the timed-out one)", w.snd.sends())
		}

		// The provider is back; the same goal continues.
		w.proposeBook()
		next := w.say(receptionist(), "at 2026-10-14 10:30")
		wantAct(t, next, "request_confirmation")
		if next.Prepared == nil {
			t.Fatal("the preserved goal did not continue")
		}
	})
}

// ---- Provider rate limit ----
// Required: apply bounded retry/backoff and budget policy.

func TestExtProviderRateLimit(t *testing.T) {
	t.Run("control", func(t *testing.T) { healthy(t, newWorld(t)) })

	t.Run("the provider's 429 is typed, carries when to retry, and is not retried inside the request", func(t *testing.T) {
		w := newWorld(t)
		w.ext.FailNext(pfake.FailRateLimited)
		res := w.say(receptionist(), sayBook)

		w.failedWith(res, contracts.ErrRateLimited)
		w.nothingExecuted(res)
		o := w.lastOutcome()
		if o.Kind != modes.KindRateLimited || o.RetryAfter != pfake.DefaultRetryAfter {
			t.Fatalf("outcome %+v, want rate_limited with the provider's retry-after", o)
		}
		if w.snd.sends() != 1 {
			t.Fatalf("sends = %d: the backoff belongs to the caller, the request is bounded to one attempt", w.snd.sends())
		}
		if w.local.Calls() != 0 {
			t.Fatal("a rate limit moved the request to another provider unasked")
		}
		if a, ok := w.budget.lastSettled(); !ok || a.Outcome != pv.UsageRecordOutcomeFailed {
			t.Fatalf("settlement %+v: a refused request is not billed", a)
		}
	})

	t.Run("a repeated limit never turns into a retry storm", func(t *testing.T) {
		w := newWorld(t)
		for i := 0; i < 3; i++ {
			w.ext.FailNext(pfake.FailRateLimited)
			w.failedWith(w.say(receptionist(), sayBook), contracts.ErrRateLimited)
		}
		if w.snd.sends() != 3 {
			t.Fatalf("sends = %d for 3 requests, want 3", w.snd.sends())
		}
	})

	t.Run("the budget's own rate limit stops the call before it is sent", func(t *testing.T) {
		w := newWorld(t)
		w.budget.setReserveErr(contracts.New(contracts.ErrRateLimited, "providerbudget: rate limited").
			WithDetails("reason", "rate_limited").WithDetails("retry_after_ms", int64(1500)))
		res := w.say(receptionist(), sayBook)

		w.failedWith(res, contracts.ErrRateLimited)
		if o := w.lastOutcome(); o.Kind != modes.KindRateLimited || o.RetryAfter != 1500*time.Millisecond || o.DataSent {
			t.Fatalf("outcome %+v", o)
		}
		if w.snd.sends() != 0 {
			t.Fatal("a call was sent after the budget refused it")
		}
	})

	t.Run("a named local fallback serves a limited request", func(t *testing.T) {
		w := newWorld(t, withLocalFallback(modes.CondRateLimited))
		w.ext.FailNext(pfake.FailRateLimited)
		w.localProposeBook()
		res := w.say(receptionist(), sayBook)

		wantAct(t, res, "request_confirmation")
		o := w.lastOutcome()
		if !o.Served() || o.Target != modes.TargetLocal || len(o.Hops) != 1 || o.Hops[0].Condition != modes.CondRateLimited {
			t.Fatalf("outcome %+v", o)
		}
	})
}

// ---- Provider returns malformed tool call ----
// Required: reject and use deterministic clarification or fallback.

func TestExtMalformedToolCall(t *testing.T) {
	t.Run("arguments of the wrong shape are rejected and the user is asked", func(t *testing.T) {
		w := newWorld(t)
		w.ext.On(offersAnyTool()).ProposeToolCall("call_1", "appointments_book", `{"patient":{"nested":["x"]},"slot":["a",{"b":1}]}`).Times(1)
		res := w.say(receptionist(), sayBook)

		wantAct(t, res, "ask_missing")
		w.nothingExecuted(res)
	})

	t.Run("arguments that are not JSON fail the turn closed", func(t *testing.T) {
		w := newWorld(t)
		w.ext.On(offersAnyTool()).ProposeToolCall("call_1", "appointments_book", `{"patient": `).Times(1)
		res := w.say(receptionist(), sayBook)

		if !res.Failed() {
			t.Fatalf("act %s: a malformed call must not pass", res.Directive.Act)
		}
		w.nothingExecuted(res)
	})

	t.Run("a tool nobody offered is refused, never run", func(t *testing.T) {
		w := newWorld(t)
		w.ext.On(offersAnyTool()).ProposeToolCall("call_1", "patients_export_all", `{"everything":true}`).Times(1)
		res := w.say(receptionist(), sayBook)

		if res.Code != contracts.ErrUnknownCapability && res.Code != contracts.ErrSchemaInvalid {
			t.Fatalf("code %q (act %s), want a refusal", res.Code, res.Directive.Act)
		}
		w.nothingExecuted(res)
	})

	t.Run("a provider that misbehaves is rejected, typed, and not retried", func(t *testing.T) {
		w := newWorld(t)
		w.ext.On(offersAnyTool()).Fail(pfake.FailMalformedToolCall)
		res := w.say(receptionist(), sayBook)

		w.failedWith(res, contracts.ErrSchemaInvalid)
		w.nothingExecuted(res)
		if o := w.lastOutcome(); o.Kind != modes.KindMalformedOutput || o.Target != modes.TargetNone || len(o.Hops) != 0 {
			t.Fatalf("outcome %+v", o)
		}
		if w.snd.sends() != 1 || w.local.Calls() != 0 {
			t.Fatalf("sends %d local calls %d: malformed output is neither retried nor handed on unasked", w.snd.sends(), w.local.Calls())
		}
	})

	t.Run("a named local fallback replaces the rejected answer", func(t *testing.T) {
		w := newWorld(t, withLocalFallback(modes.CondMalformedOutput))
		w.ext.On(offersAnyTool()).Fail(pfake.FailMalformedToolCall)
		w.localProposeBook()
		res := w.say(receptionist(), sayBook)

		wantAct(t, res, "request_confirmation")
		if o := w.lastOutcome(); !o.Served() || o.Target != modes.TargetLocal || len(o.Hops) != 1 || o.Hops[0].Condition != modes.CondMalformedOutput {
			t.Fatalf("outcome %+v", o)
		}
		if _, _, commits := w.counter.counts(); commits != 0 {
			t.Fatalf("%d commits", commits)
		}
	})
}

// ---- Provider returns unsupported modality ----
// Required: select another qualified provider or report unsupported.

func TestExtUnsupportedModality(t *testing.T) {
	lacks := reasonErr(contracts.ErrValidationFailed, providerreg.ReasonCapability)

	t.Run("another qualified provider is selected", func(t *testing.T) {
		w := newWorld(t, withSecondProfile())
		w.prof.setCheckErr(profA, lacks)
		w.proposeBook()
		res := w.say(receptionist(), sayBook)

		wantAct(t, res, "request_confirmation")
		if got := w.snd.sentTo(); len(got) != 1 || got[0] != profB {
			t.Fatalf("sent to %v, want only the second qualified profile %s", got, profB)
		}
		if o := w.lastOutcome(); !o.Served() || o.Profile != profB {
			t.Fatalf("outcome %+v", o)
		}
	})

	t.Run("no qualified provider: reported unsupported, nothing sent", func(t *testing.T) {
		w := newWorld(t, withSecondProfile())
		w.prof.setCheckErr(profA, lacks)
		w.prof.setCheckErr(profB, lacks)
		res := w.say(receptionist(), sayBook)

		// The engine's guard refuses the prompt before anything is built for a provider.
		w.failedWith(res, contracts.ErrPermissionDenied)
		w.nothingExecuted(res)
		if w.snd.sends() != 0 || w.budget.reservations() != 0 || w.local.Calls() != 0 {
			t.Fatalf("sends %d reservations %d local calls %d: nothing may be spent on a request no provider can take",
				w.snd.sends(), w.budget.reservations(), w.local.Calls())
		}
		// The router names the row: unsupported_modality, validation_failed, nothing run.
		o, err, sent, localRuns := w.routeDirect(false)
		if o.Kind != modes.KindUnsupportedModality || modes.OutcomeOf(err) != modes.KindUnsupportedModality || contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Fatalf("outcome %+v err %v", o, err)
		}
		if sent != 0 || localRuns != 0 || o.DataSent || len(o.Hops) != 0 {
			t.Fatalf("sent %d local runs %d outcome %+v", sent, localRuns, o)
		}
	})
}

// ---- External egress denied ----
// Required: keep data local and use the local provider if available.

func TestExtEgressDenied(t *testing.T) {
	t.Run("denied by policy: the data stays local, typed, and nothing is sent or reserved", func(t *testing.T) {
		w := newWorld(t, withoutEgressPolicy())
		w.proposeBook()
		res := w.say(receptionist(), sayBook)

		w.failedWith(res, contracts.ErrPermissionDenied)
		w.nothingExecuted(res)
		if w.ext.Calls() != 0 || w.snd.sends() != 0 || w.budget.reservations() != 0 {
			t.Fatalf("provider calls %d sends %d reservations %d after a denial", w.ext.Calls(), w.snd.sends(), w.budget.reservations())
		}
		if w.local.Calls() != 0 {
			t.Fatal("a denial moved the request to the local runtime unasked")
		}
	})

	t.Run("the global disable keeps everything local and the local provider serves", func(t *testing.T) {
		w := newWorld(t)
		healthy(t, w) // control: the same world reaches the external provider
		w2 := newWorld(t)
		if err := w2.eg.EngageKillSwitch(ctxb(), dataegress.Operator{DeploymentID: receptionist().DeploymentID, PrincipalID: "opr_root"}, "test"); err != nil {
			t.Fatalf("kill switch: %v", err)
		}
		w2.localProposeBook()
		res := w2.say(receptionist(), sayBook)

		wantAct(t, res, "request_confirmation")
		if res.Prepared == nil {
			t.Fatal("the local provider did not serve")
		}
		if w2.ext.Calls() != 0 || w2.snd.sends() != 0 || w2.budget.reservations() != 0 {
			t.Fatalf("provider calls %d sends %d reservations %d under the global disable", w2.ext.Calls(), w2.snd.sends(), w2.budget.reservations())
		}
		if w2.local.Calls() == 0 {
			t.Fatal("the local runtime never ran")
		}
	})

	t.Run("a switch to Local mode does the same", func(t *testing.T) {
		w := newWorld(t)
		if err := w.modes.Set(modes.LevelTenant, receptionist(), modes.ModeLocal); err != nil {
			t.Fatal(err)
		}
		w.localProposeBook()
		res := w.say(receptionist(), sayBook)

		wantAct(t, res, "request_confirmation")
		if w.ext.Calls() != 0 || w.snd.sends() != 0 {
			t.Fatal("the external provider was contacted in Local mode")
		}
	})
}

// ---- Provider account budget exhausted ----
// Required: stop external calls and show budget state.

func TestExtBudgetExhausted(t *testing.T) {
	t.Run("control", func(t *testing.T) { healthy(t, newWorld(t)) })

	t.Run("an exhausted budget stops every call, and the state says so", func(t *testing.T) {
		w := newWorld(t, withBudget(1)) // one micro-unit a day: below any call's upper bound
		for i := 0; i < 3; i++ {
			w.proposeBook()
			res := w.say(receptionist(), sayBook)
			w.failedWith(res, contracts.ErrResourceExhausted)
			w.nothingExecuted(res)
			if o := w.lastOutcome(); o.Kind != modes.KindBudgetExhausted || o.DataSent || !o.AIActionsDisabled {
				t.Fatalf("outcome %+v", o)
			}
		}
		if w.snd.sends() != 0 || w.ext.Calls() != 0 {
			t.Fatalf("sends %d provider calls %d: external calls must stop", w.snd.sends(), w.ext.Calls())
		}
		if w.local.Calls() != 0 {
			t.Fatal("an exhausted budget moved the request to the local runtime unasked")
		}
		states, err := w.budget.States(ctxb(), receptionist(), egProvider, pv.ChannelInteractive)
		if err != nil || len(states) == 0 {
			t.Fatalf("states %v err %v: the budget state must be readable", states, err)
		}
		rep, err := w.rt.Router().LocalityReport(ctxb(), receptionist())
		if err != nil {
			t.Fatal(err)
		}
		if e, _ := rep.Entry("controller"); e.Available && e.Locality == modes.LocalityRemote && !e.LocalUsable {
			t.Fatalf("entry %+v: the report must not present the exhausted provider as ready", e)
		}
	})

	t.Run("the account's own quota is the same row", func(t *testing.T) {
		w := newWorld(t)
		w.budget.setReserveErr(contracts.New(contracts.ErrResourceExhausted, "providerbudget: exhausted").WithDetails("reason", "budget_exhausted"))
		w.failedWith(w.say(receptionist(), sayBook), contracts.ErrResourceExhausted)
		if w.snd.sends() != 0 {
			t.Fatal("a call was sent")
		}
	})

	t.Run("a named local fallback serves while the budget is exhausted", func(t *testing.T) {
		w := newWorld(t, withBudget(1), withLocalFallback(modes.CondBudgetExhausted))
		w.localProposeBook()
		res := w.say(receptionist(), sayBook)

		wantAct(t, res, "request_confirmation")
		o := w.lastOutcome()
		if !o.Served() || o.Target != modes.TargetLocal || len(o.Hops) != 1 || o.Hops[0].Condition != modes.CondBudgetExhausted || w.snd.sends() != 0 {
			t.Fatalf("outcome %+v sends %d", o, w.snd.sends())
		}
	})
}

// ---- Provider terms/configuration stale ----
// Required: quarantine the provider profile pending review.

func TestExtTermsStale(t *testing.T) {
	t.Run("control", func(t *testing.T) { healthy(t, newWorld(t)) })

	w := newWorld(t, withSecondProfile(), withLocalFallback(modes.CondUnavailable))
	w.prof.setCheckErr(profA, reasonErr(contracts.ErrQuarantined, providerreg.ReasonTermsChanged))
	w.proposeBook()
	w.localProposeBook()

	res := w.say(receptionist(), sayBook)

	// The engine's guard refuses the prompt; the refusal is typed and nothing is built for any provider.
	w.failedWith(res, contracts.ErrPermissionDenied)
	w.nothingExecuted(res)
	if !w.prof.isQuarantined(profA) {
		t.Fatal("the profile with stale terms was not quarantined")
	}
	if w.snd.sends() != 0 || w.budget.reservations() != 0 || w.local.Calls() != 0 {
		t.Fatalf("sends %d reservations %d local calls %d: stale terms never trigger a silent alternate provider",
			w.snd.sends(), w.budget.reservations(), w.local.Calls())
	}
	// Pending review: the quarantine holds on the next request.
	w.failedWith(w.say(receptionist(), sayBook), contracts.ErrPermissionDenied)
	if w.snd.sends() != 0 {
		t.Fatal("a quarantined profile was used")
	}
	// The router names the row: stale_profile, quarantined, no alternate provider and no local run.
	o, err, sent, localRuns := w.routeDirect(true)
	if o.Kind != modes.KindStaleProfile || modes.OutcomeOf(err) != modes.KindStaleProfile || contracts.CodeOf(err) != contracts.ErrQuarantined {
		t.Fatalf("outcome %+v err %v", o, err)
	}
	if sent != 0 || localRuns != 0 || o.DataSent || len(o.Hops) != 0 || o.Reason != string(providerreg.ReasonTermsChanged) {
		t.Fatalf("sent %d local runs %d outcome %+v", sent, localRuns, o)
	}
}

// ---- Local fallback unavailable ----
// Required: keep application UI, deterministic APIs and local search working; disable AI action execution.

func TestExtLocalFallbackUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(w *world)
	}{
		{"the local runtime is not usable", func(w *world) { w.localUp.set(false) }},
		{"the local runtime is usable but fails", func(w *world) { w.local.On(pfake.Any()).Fail(pfake.FailUnavailable) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, withLocalFallback(modes.CondUnavailable))
			w.ext.On(offersAnyTool()).Fail(pfake.FailUnavailable)
			tc.setup(w)
			res := w.say(receptionist(), sayBook)

			w.failedWith(res, contracts.ErrDegraded)
			wantAct(t, res, "report_failure")
			w.nothingExecuted(res)
			o := w.lastOutcome()
			if o.Kind != modes.KindNoFallback || !o.AIActionsDisabled || o.Served() {
				t.Fatalf("outcome %+v, want no_fallback with AI actions disabled", o)
			}
			if len(o.Hops) != 1 || o.Hops[0].To != modes.LocalHop {
				t.Fatalf("hops %+v: the attempted fallback must be on record", o.Hops)
			}
			// The rest of the application is untouched.
			w.hostStillWorks()
		})
	}
}

// ---- An external provider outage cannot prevent ordinary host-application work ----

func TestExtProviderOutageDoesNotBlockHostWork(t *testing.T) {
	w := newWorld(t, withLocalFallback(modes.CondUnavailable, modes.CondTimeout))
	sc := receptionist()

	// Work in progress before the outage: a booking prepared and waiting for the user's confirmation, and a source
	// the search finds.
	w.proposeBook()
	prep := w.say(sc, sayBook)
	wantAct(t, prep, "request_confirmation")
	search := newSearch(t, w.db)
	search.addSource(sc, "Clinic handbook", "# Clinic handbook\n\n## Opening\n\nThe clinic opens at eight and the reception desk answers the telephone until six.\n")
	if res, err := search.search(sc, "reception telephone"); err != nil || len(res.Package.Items) == 0 {
		t.Fatalf("control: search before the outage: %v items %d", err, len(res.Package.Items))
	}

	// Every provider fails: the external one at every call, the local runtime is down and not usable.
	w.ext.On(pfake.Any()).Fail(pfake.FailUnavailable)
	w.local.On(pfake.Any()).Fail(pfake.FailUnavailable)
	w.localUp.set(false)

	// Only AI action execution is disabled.
	res := w.say(sc, "Book Anna Rossi for 2026-10-15 11:00")
	w.failedWith(res, contracts.ErrDegraded)
	if res.Prepared != nil || res.Receipt != nil {
		t.Fatalf("prepared %v receipt %v: AI action execution must be off", res.Prepared, res.Receipt)
	}
	if o := w.lastOutcome(); o.Kind != modes.KindNoFallback || !o.AIActionsDisabled {
		t.Fatalf("outcome %+v", o)
	}

	// Everything else works, with no provider at all.
	calls := w.ext.Calls() + w.local.Calls()
	w.hostStillWorks()
	if got, err := search.search(sc, "reception telephone"); err != nil || len(got.Package.Items) == 0 {
		t.Fatalf("local search during the outage: %v items %d", err, len(got.Package.Items))
	}
	done := w.confirm(sc, prep) // the structured confirmation of the work in progress needs no model
	if done.Receipt == nil || done.Receipt.Status != act.ActionReceiptStatusCommitted {
		t.Fatalf("receipt %+v code %q: a confirmed action commits with every provider down", done.Receipt, done.Code)
	}
	if _, _, commits := w.counter.counts(); commits != 1 {
		t.Fatalf("commits = %d, want 1", commits)
	}
	if w.ext.Calls()+w.local.Calls() != calls {
		t.Fatal("host work called a model provider")
	}
}

// hostStillWorks runs a governed read through the action coordinator and the host adapter: a deterministic
// application API that needs no model.
func (w *world) hostStillWorks() {
	w.t.Helper()
	sc := receptionist()
	readsBefore, _, _ := w.counter.counts()
	out, err := w.co.Read(ctxb(), sc, actions.ReadInput{Session: sc, CapabilityID: "availability.list", Fields: map[string]any{"date": "2026-10-14"}})
	if err != nil {
		w.t.Fatalf("a governed read failed with the providers down: %v", err)
	}
	if out.Result.Status != act.DeterministicResultStatusOk {
		w.t.Fatalf("read result %+v", out.Result)
	}
	if reads, _, _ := w.counter.counts(); reads != readsBefore+1 {
		w.t.Fatalf("host reads %d -> %d: the read did not reach the host", readsBefore, reads)
	}
}
