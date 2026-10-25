package extprovidersec

import (
	"errors"
	"testing"

	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/modes"
)

// engage engages the global external-disable switch of the world's deployment.
func (w *world) engage() {
	w.t.Helper()
	if err := w.eg.EngageKillSwitch(ctxb(), w.operator(), "test_emergency"); err != nil {
		w.t.Fatalf("EngageKillSwitch: %v", err)
	}
}

// noEgress fails unless nothing was sent, reserved or contacted on the external side.
func (w *world) noEgress(why string) {
	w.t.Helper()
	if w.ext.Calls() != 0 || w.snd.sends() != 0 || w.budget.reservations() != 0 {
		w.t.Fatalf("%s: provider calls %d sends %d reservations %d, want none", why, w.ext.Calls(), w.snd.sends(), w.budget.reservations())
	}
}

// SEC-126
func TestSecExtGlobalKillSwitchStopsAllEgress(t *testing.T) {
	t.Run("control: the same world reaches the external provider while the switch is released", func(t *testing.T) {
		healthy(t, newWorld(t))
	})

	t.Run("an engaged switch keeps the turn local: nothing is sent, reserved or billed", func(t *testing.T) {
		w := newWorld(t)
		w.engage()
		w.proposeBook()
		w.localProposeBook()
		res := w.say(receptionist(), sayBook)

		wantAct(t, res, "request_confirmation")
		if res.Prepared == nil || w.local.Calls() == 0 {
			t.Fatalf("prepared %v local calls %d: the local runtime must carry the turn", res.Prepared, w.local.Calls())
		}
		w.noEgress("engaged switch")
		if _, _, commits := w.counter.counts(); commits != 0 {
			t.Fatalf("%d commits", commits)
		}
	})

	t.Run("the router refuses a call below the dialogue engine too", func(t *testing.T) {
		w := newWorld(t)
		_, _, sent, _ := w.routeDirect(receptionist(), false)
		if sent != 1 {
			t.Fatalf("control: sent = %d, want 1 before the switch", sent)
		}
		w.engage()
		o, _, sent, _ := w.routeDirect(receptionist(), false)
		if sent != 0 || o.Target == modes.TargetExternal || o.DataSent {
			t.Fatalf("sent %d outcome %+v: the router sent under the switch", sent, o)
		}
	})

	t.Run("a request the guard released earlier is stopped when the switch is engaged during the router's profile gate", func(t *testing.T) {
		w := newWorld(t)
		w.localProposeBook()
		w.proposeBook()
		// The switch is engaged while the router runs the profile gate: the guard released the request earlier. The
		// router reads the switch again after that gate and the guard reads it again before it builds the envelope;
		// either alone stops the request (mutations M1a and M1b), both removed do not.
		fired := false
		w.reg.hook(func() {
			if w.reg.checks == 2 {
				fired = true
				w.engage()
			}
		})
		res := w.say(receptionist(), sayBook)

		if !fired {
			t.Fatal("the switch was never engaged mid-request, so the check proves nothing")
		}
		w.noEgress("switch engaged between the gates and the send")
		o := w.lastOutcome()
		if o.Target == modes.TargetExternal || o.DataSent {
			t.Fatalf("outcome %+v: the pending request was sent after the switch was engaged", o)
		}
		if res.Receipt != nil {
			t.Fatalf("receipt %+v", res.Receipt)
		}
	})

	t.Run("a switch that cannot be read counts as engaged", func(t *testing.T) {
		w := newWorld(t)
		w.ks.unreadable(errors.New("store unavailable"))
		w.localProposeBook()
		w.proposeBook()
		res := w.say(receptionist(), sayBook)
		wantAct(t, res, "request_confirmation")
		w.noEgress("unreadable switch")
		if w.ks.readCount() == 0 {
			t.Fatal("the switch was never read")
		}
	})

	t.Run("the switch is persistent: another service over the same store sees it, and only a release ends it", func(t *testing.T) {
		w := newWorld(t)
		w.engage()
		other, err := dataegress.New(dataegress.Deps{DB: w.db, Clock: w.clk, Audit: w.alog, IDs: ids.NewGenerator(w.clk)})
		if err != nil {
			t.Fatal(err)
		}
		st, err := other.KillSwitch(ctxb(), receptionist().DeploymentID)
		if err != nil || !st.Engaged {
			t.Fatalf("state %+v err %v: a restart must not clear the switch", st, err)
		}
		if err := w.eg.ReleaseKillSwitch(ctxb(), w.operator()); err != nil {
			t.Fatal(err)
		}
		healthy(t, w) // released: the policy chain, not the release, decides, and it is enabled
	})
}

// SEC-129
//
// The router reads the kill switch before the profile gate, the guard reads it before it builds the envelope, and
// the router reads it a last time after the budget reservation, immediately before the send. A switch engaged in
// the window between the reservation and the send stops that one request too: nothing is sent, the reservation is
// given back (nothing is charged) and the turn is served locally.
func TestSecKillSwitchIsReReadAfterReservation(t *testing.T) {
	w := newWorld(t)
	w.localProposeBook()
	w.proposeBook()
	fired := false
	w.budget.after = func() { fired = true; w.engage() }
	res := w.say(receptionist(), sayBook)

	if !fired {
		t.Fatal("the switch was never engaged after the reservation, so the check proves nothing")
	}
	if w.snd.sends() != 0 || w.ext.Calls() != 0 {
		t.Fatalf("sends %d provider calls %d: a request was sent after the switch was engaged", w.snd.sends(), w.ext.Calls())
	}
	o := w.lastOutcome()
	if o.DataSent || o.Target == modes.TargetExternal {
		t.Fatalf("outcome %+v: the request counts as sent", o)
	}
	wantAct(t, res, "request_confirmation")
	if res.Prepared == nil || w.local.Calls() == 0 {
		t.Fatalf("prepared %v local calls %d: the local runtime must carry the turn", res.Prepared, w.local.Calls())
	}
	if res.Receipt != nil {
		t.Fatalf("receipt %+v", res.Receipt)
	}
}
