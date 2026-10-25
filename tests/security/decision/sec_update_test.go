package decisionsec

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/decision/lifecycle"
	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// SEC-110
func TestSecDecisionUpdateRollbackRestoresPreviousRevision(t *testing.T) {
	passGate := lifecycle.PromotionGateFunc(func(context.Context, lifecycle.Instance) error { return nil })

	// served runs one request and asserts which revision answered and which is reported.
	wantServing := func(t *testing.T, r *lcRig, rev string) {
		t.Helper()
		if got := r.serve(); got != rev {
			t.Fatalf("the answer came from %q, want %q", got, rev)
		}
		if got := string(r.sup.Manifest().ModelRevision); got != rev {
			t.Fatalf("the manifest says %q, want %q", got, rev)
		}
	}

	t.Run("control: a good update is staged, switches and keeps the previous revision resident for the probation", func(t *testing.T) {
		r := newLCRig(t, nil)
		wantServing(t, r, "rev1")
		next, src := r.next("rev2")

		rep, err := r.sup.Update(bg, next, src, passGate)

		if err != nil || rep.Status != lifecycle.UpdateProbation {
			t.Fatalf("report %+v, err %v", rep, err)
		}
		wantServing(t, r, "rev2")
		if st := r.sup.State(); !st.Probation || st.ResidentBytes != 16*gib {
			t.Fatalf("state %+v: want a probation with both revisions resident", st)
		}
	})

	t.Run("a staged revision that fails to load, to be healthy, to pass its gate or to be itself leaves the previous one serving", func(t *testing.T) {
		for _, c := range []struct {
			name    string
			arrange func(*lcRig)
			gate    lifecycle.PromotionGate
			reason  string
		}{
			{"load error", func(r *lcRig) { r.ld.loadErr["rev2"] = errors.New("cannot map the weights") }, passGate, "load"},
			{"unhealthy after load", func(r *lcRig) { r.ld.healthErr["rev2"] = errors.New("not ready") }, passGate, "health_probe"},
			{"promotion gate refuses", nil, lifecycle.PromotionGateFunc(func(context.Context, lifecycle.Instance) error { return errors.New("calibration regressed") }), "gate"},
			{"another model answers to the revision's name", func(r *lcRig) {
				m := manifestOf("rev2", revFiles("rev2"))
				m.ModelID = "impostor"
				r.ld.identity["rev2"] = m
			}, passGate, "load"},
			{"another revision answers to the revision's name", func(r *lcRig) {
				r.ld.identity["rev2"] = manifestOf("rev-evil", revFiles("rev-evil"))
			}, passGate, "load"},
		} {
			r := newLCRig(t, nil)
			wantServing(t, r, "rev1")
			if c.arrange != nil {
				c.arrange(r)
			}
			next, src := r.next("rev2")

			rep, err := r.sup.Update(bg, next, src, c.gate)

			if err == nil || rep.Status != lifecycle.UpdateRolledBack || rep.Reason != c.reason {
				t.Fatalf("%s: report %+v, err %v", c.name, rep, err)
			}
			wantServing(t, r, "rev1")
			if in := r.ld.last("rev2"); in != nil && in.closes() != 1 {
				t.Fatalf("%s: the discarded staged instance was closed %d times, want 1", c.name, in.closes())
			}
			if r.gate.resident() != 8*gib {
				t.Fatalf("%s: resident bytes = %d, want only the serving revision", c.name, r.gate.resident())
			}
			if st := r.sup.State(); st.Probation {
				t.Fatalf("%s: the supervisor is in probation after a rolled back update", c.name)
			}
			if got := r.met.Counter(lifecycle.MetricRollbacks, lifecycle.Labels{Provider: "fake-scorer", Model: "fake-model", Revision: "rev2", Profile: "sec", Reason: c.reason}); got != 1 {
				t.Fatalf("%s: rollback counter = %d, want 1", c.name, got)
			}
		}
	})

	t.Run("a staged copy that does not fit is rejected: nothing is evicted and the previous revision serves", func(t *testing.T) {
		r := newLCRig(t, nil)
		wantServing(t, r, "rev1")
		r.gate.budget = 12 * gib // room for one revision, not two
		next, src := r.next("rev2")

		rep, err := r.sup.Update(bg, next, src, passGate)

		if err == nil || rep.Status != lifecycle.UpdateRejected || rep.Reason != "resource_gate" || !contracts.HasCode(err, contracts.ErrResourceExhausted) {
			t.Fatalf("report %+v, err %v", rep, err)
		}
		wantServing(t, r, "rev1")
		if r.gate.evicted != 0 || !r.gate.protected["controller"] || !r.gate.protected["retrieval"] || !r.gate.protected["speech"] {
			t.Fatalf("a protected worker was touched: evicted=%d protected=%v", r.gate.evicted, r.gate.protected)
		}
		if r.ld.loads("rev2") != 0 {
			t.Fatal("a revision the gate did not admit was loaded")
		}
	})

	t.Run("a gate breach during the probation rolls back to the previous instance, which never reloads", func(t *testing.T) {
		r := newLCRig(t, nil)
		wantServing(t, r, "rev1")
		before := r.ld.last("rev1")
		next, src := r.next("rev2")
		if _, err := r.sup.Update(bg, next, src, passGate); err != nil {
			t.Fatal(err)
		}
		wantServing(t, r, "rev2")
		bad := r.ld.last("rev2")

		if err := r.sup.Rollback(bg); err != nil {
			t.Fatal(err)
		}

		wantServing(t, r, "rev1")
		if r.ld.last("rev1") != before || r.ld.loads("rev1") != 1 {
			t.Fatalf("the previous revision was reloaded (%d loads) instead of resumed", r.ld.loads("rev1"))
		}
		if bad.closes() != 1 {
			t.Fatalf("the rolled back revision was closed %d times, want 1", bad.closes())
		}
		if r.gate.resident() != 8*gib || r.sup.State().Probation {
			t.Fatalf("resident %d, state %+v", r.gate.resident(), r.sup.State())
		}
		if err := r.sup.Rollback(bg); !contracts.HasCode(err, contracts.ErrConflict) {
			t.Fatalf("a second rollback: err = %v, want a conflict", err)
		}
	})

	t.Run("a new revision that goes unhealthy during the probation is rolled back by the periodic probe", func(t *testing.T) {
		r := newLCRig(t, nil)
		wantServing(t, r, "rev1")
		next, src := r.next("rev2")
		if _, err := r.sup.Update(bg, next, src, passGate); err != nil {
			t.Fatal(err)
		}
		r.ld.last("rev2").setHealth(errors.New("segfault"))

		events := r.sup.Tick(bg)

		if len(events) != 1 || events[0] != "rolled_back" {
			t.Fatalf("tick events = %v", events)
		}
		wantServing(t, r, "rev1")
	})

	t.Run("a probation that ends well promotes: the previous revision is released, and a rollback is then refused", func(t *testing.T) {
		r := newLCRig(t, nil)
		wantServing(t, r, "rev1")
		next, src := r.next("rev2")
		if _, err := r.sup.Update(bg, next, src, passGate); err != nil {
			t.Fatal(err)
		}
		r.clk.Advance(11 * time.Minute)
		if events := r.sup.Tick(bg); len(events) != 1 || events[0] != "promoted" {
			t.Fatalf("tick events = %v", events)
		}
		wantServing(t, r, "rev2")
		if r.gate.resident() != 8*gib {
			t.Fatalf("resident %d after the promotion", r.gate.resident())
		}
		if err := r.sup.Rollback(bg); !contracts.HasCode(err, contracts.ErrConflict) {
			t.Fatalf("rollback after promotion: err = %v, want a conflict", err)
		}
	})

	t.Run("updates do not stack: during an update's probation, for the same revision, or after a stop", func(t *testing.T) {
		r := newLCRig(t, nil)
		wantServing(t, r, "rev1")
		same, src := r.next("rev1")
		if rep, err := r.sup.Update(bg, same, src, passGate); err == nil || rep.Status != lifecycle.UpdateRejected {
			t.Fatalf("an update to the serving revision: report %+v, err %v", rep, err)
		}
		next, src := r.next("rev2")
		if _, err := r.sup.Update(bg, next, src, passGate); err != nil {
			t.Fatal(err)
		}
		third, src3 := r.next("rev3")
		if _, err := r.sup.Update(bg, third, src3, passGate); !contracts.HasCode(err, contracts.ErrConflict) {
			t.Fatalf("an update during a probation: err = %v, want a conflict", err)
		}
		wantServing(t, r, "rev2")
		r.sup.Stop(bg)
		if _, err := r.sup.ScoreDecision(bg, scopeOf("ten1"), lcRequest(r.clk)); !contracts.HasCode(err, contracts.ErrUnavailable) {
			t.Fatalf("a stopped supervisor: err = %v, want unavailable", err)
		}
		if r.gate.resident() != 0 {
			t.Fatalf("resident %d after Stop", r.gate.resident())
		}
	})

	t.Run("an update interrupted while its files are read changes nothing", func(t *testing.T) {
		r := newLCRig(t, nil)
		wantServing(t, r, "rev1")
		next, src := r.next("rev2")
		ctx, cancel := context.WithCancel(bg)
		cancel()

		rep, err := r.sup.Update(ctx, next, src, passGate)

		if err == nil || rep.Status != lifecycle.UpdateRejected {
			t.Fatalf("report %+v, err %v", rep, err)
		}
		wantServing(t, r, "rev1")
		if r.ld.loads("rev2") != 0 || r.gate.resident() != 8*gib {
			t.Fatalf("loads %d, resident %d", r.ld.loads("rev2"), r.gate.resident())
		}
	})

	t.Run("recovery: a crashed scorer is unavailable while it backs off, then restarts by itself", func(t *testing.T) {
		r := newLCRig(t, nil)
		wantServing(t, r, "rev1")
		r.ld.last("rev1").setHealth(errors.New("the process died"))
		if events := r.sup.Tick(bg); len(events) != 1 || events[0] != "probe_failed" {
			t.Fatalf("tick events = %v", events)
		}

		if _, err := r.sup.ScoreDecision(bg, scopeOf("ten1"), lcRequest(r.clk)); !contracts.HasCode(err, contracts.ErrUnavailable) {
			t.Fatalf("during the backoff: err = %v, want unavailable", err)
		}
		if st := r.sup.State(); st.Ready || st.Loadable || st.Phase != "backoff" {
			t.Fatalf("state %+v", st)
		}
		r.clk.Advance(2 * time.Second)
		wantServing(t, r, "rev1")
		if r.ld.loads("rev1") != 2 {
			t.Fatalf("loads of rev1 = %d, want 2 (the restart)", r.ld.loads("rev1"))
		}
	})

	t.Run("recovery: a scorer that will not start is a typed fallback of the engine and the deterministic path continues", func(t *testing.T) {
		r := newLCRig(t, nil)
		r.ld.loadErr["rev1"] = errors.New("out of memory")
		rg := newRig(t)
		eng, err := decision.New(decision.Deps{Clock: rg.clk, Provider: r.sup, Resolver: rg.cat, Audit: rg.aud, Config: rg.cfg})
		if err != nil {
			t.Fatal(err)
		}
		adv, err := eng.Score(bg, scopeOf("ten1"), rg.request("ten1"))
		if err != nil {
			t.Fatal(err)
		}
		wantFallback(t, adv, gen.FallbackReasonUnavailable)
	})

	t.Run("after a rollback the engine follows the revision that serves, and a cached answer of the bad revision is not reused", func(t *testing.T) {
		r := newLCRig(t, nil)
		rg := newRig(t, func(c *decision.Config) {
			c.Calibration = []decision.Calibration{
				{ModelID: "fake-model", ModelRevision: "rev1", ContractVersion: "cv1", ECE: 0.02, MaxECE: 0.05},
				{ModelID: "fake-model", ModelRevision: "rev2", ContractVersion: "cv1", ECE: 0.02, MaxECE: 0.05},
			}
		})
		eng, err := decision.New(decision.Deps{Clock: rg.clk, Provider: r.sup, Resolver: rg.cat, Audit: rg.aud, Config: rg.cfg})
		if err != nil {
			t.Fatal(err)
		}
		rev := func() string {
			adv, err := eng.Score(bg, scopeOf("ten1"), rg.request("ten1"))
			if err != nil {
				t.Fatal(err)
			}
			wantAccepted(t, adv)
			return string(*adv.Result().ModelRevision)
		}
		if got := rev(); got != "rev1" {
			t.Fatalf("revision %q", got)
		}
		next, src := r.next("rev2")
		if _, err := r.sup.Update(bg, next, src, passGate); err != nil {
			t.Fatal(err)
		}
		if got := rev(); got != "rev2" {
			t.Fatalf("revision %q after the update", got)
		}
		if err := r.sup.Rollback(bg); err != nil {
			t.Fatal(err)
		}
		if got := rev(); got != "rev1" {
			t.Fatalf("revision %q after the rollback: a cached answer of the rolled back revision was served", got)
		}
	})
}
