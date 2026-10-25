package m1

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// committedOnce holds a confirmation to having committed exactly one appointment in the application.
func committedOnce(t tb, dep *deployment, before clinicStats, what string) clinicStats {
	t.Helper()
	after := dep.clinic.stats(t)
	if n := after.commits() - before.commits(); n != 1 {
		t.Errorf("%s: the application was asked to commit %d times, want once: %s", what, n, after.callsSince(before))
	}
	if n := after.totalsSince(before, "appointments"); n != 1 {
		t.Errorf("%s: the application holds %d new appointments, want one", what, n)
	}
	return after
}

// noCommit holds an attempt to not having made the application commit or change anything.
func noCommit(t tb, dep *deployment, before clinicStats, what string) {
	t.Helper()
	after := dep.clinic.stats(t)
	if n := after.commits() - before.commits(); n != 0 {
		t.Errorf("%s: the application was asked to commit %d times: %s", what, n, after.callsSince(before))
	}
	if n := after.totalsSince(before, "appointments"); n != 0 {
		t.Errorf("%s: the application holds %d new appointments", what, n)
	}
}

// failedTurn holds a turn to a refusal that is typed, final and committed nothing.
func failedTurn(t tb, tr *turn, codes ...contracts.ErrCode) {
	t.Helper()
	if !tr.ran() || tr.receipt() != nil || tr.finish() != "failed" {
		t.Fatalf("expected a failed turn with no receipt: %s", tr)
	}
	for _, c := range codes {
		if tr.code() == c {
			return
		}
	}
	t.Fatalf("the turn failed with %q, want one of %v: %s", tr.code(), codes, tr)
}

// Replay and stale-confirmation tests of the M1 exit gate (master plan 21): a confirmation is used
// once, whatever carries it; a confirmation made against data that has since changed, or that
// someone else has taken, or that has outlived its time, commits nothing; and in every case the
// application's own commit counter and its records say so.
// SEC-039
func TestM1ReplayAndStaleConfirmation(t *testing.T) {
	dep := sharedDeployment(t)
	r := newRig(t, dep, withTemplateOnly())

	t.Run("a confirmation is used once", func(t *testing.T) {
		w := seedWorld(t, dep.clinic, "rp1", withSlots(3))
		slots := w.slots(t, w.Reception)
		rc := r.convo(t, w, w.Reception)
		rc.open()
		_, pid, secret := prepareBooking(t, r, w, rc, slots[0])
		before := dep.clinic.stats(t)
		first := rc.confirm(pid, secret)
		if !first.ran() || first.receipt() == nil || first.receipt().Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("the confirmation: %s", first)
		}
		receipt := first.receipt()
		after := committedOnce(t, dep, before, "the confirmation")

		// the same confirmation again, by the action route (a client that retries after a lost reply)
		again := rc.confirmAction(pid, secret)
		if again.Status != http.StatusConflict || again.code() != contracts.ErrReplayed {
			t.Errorf("the confirmation presented again: %s", again)
		}
		// and again, as a turn
		failedTurn(t, rc.confirm(pid, secret), contracts.ErrReplayed, contracts.ErrConflict)
		if n := dep.clinic.stats(t).commits() - after.commits(); n != 0 {
			t.Errorf("a repeated confirmation made the application commit %d more times", n)
		}
		noExtra := dep.clinic.stats(t)
		if noExtra.totalsSince(before, "appointments") != 1 || noExtra.totalsSince(before, "notifications") != 1 {
			t.Errorf("a repeated confirmation changed the application's records: %s", noExtra.callsSince(before))
		}
		// the receipt is the one outcome, and it is still there
		rep := rc.call(http.MethodGet, "/v1/actions/"+pid+"/receipt", nil)
		var stored act.ActionReceipt
		if rep.Status != http.StatusOK || json.Unmarshal(rep.Body, &stored) != nil || stored.ReceiptID != receipt.ReceiptID || stored.Status != act.ActionReceiptStatusCommitted {
			t.Errorf("the receipt after the replays: %s", rep)
		}
		// the audit log holds one commit of this preparation
		commits := 0
		for _, e := range r.chainEvents(w.Tenant) {
			if e.Type == "action.committed" && e.Data["preparation_id"] == pid {
				commits++
			}
		}
		if commits != 1 {
			t.Errorf("the audit log records %d commits of the preparation, want 1", commits)
		}
	})

	t.Run("a confirmation nobody was given is refused, and the real one still works", func(t *testing.T) {
		w := seedWorld(t, dep.clinic, "rp2", withSlots(3))
		slots := w.slots(t, w.Reception)
		rc := r.convo(t, w, w.Reception)
		rc.open()
		_, pid, secret := prepareBooking(t, r, w, rc, slots[0])
		before := dep.clinic.stats(t)
		guess := "cfm_" + strings.Repeat("A", len(secret)-len("cfm_"))
		got := rc.confirmAction(pid, guess)
		if got.Status < 400 || got.code() == "" {
			t.Errorf("a made-up confirmation was answered: %s", got)
		}
		noCommit(t, dep, before, "a made-up confirmation")
		// the same secret from another session of the same person is not the right confirmation either
		other := r.convo(t, w, w.Reception)
		other.open()
		got = other.confirmAction(pid, secret)
		if got.Status < 400 || got.code() == "" {
			t.Errorf("the confirmation presented from another session was answered: %s", got)
		}
		noCommit(t, dep, before, "a confirmation from another session")
		ok := rc.confirm(pid, secret)
		if !ok.ran() || ok.receipt() == nil {
			t.Fatalf("the real confirmation after the refused ones: %s", ok)
		}
		committedOnce(t, dep, before, "the real confirmation")
	})

	t.Run("a record that changed after the preparation makes it stale", func(t *testing.T) {
		w := seedWorld(t, dep.clinic, "st1", withSlots(3))
		slots := w.slots(t, w.Reception)
		rc := r.convo(t, w, w.Reception)
		rc.open()
		first, pid, secret := prepareBooking(t, r, w, rc, slots[0])
		oldVersion := ""
		for _, dv := range first.prepared().DataVersions {
			if string(dv.EntityID) == slots[0].ID {
				oldVersion = string(dv.Version)
			}
		}
		// the application's record of the slot moves: someone else touched it
		dep.clinic.mustTool(t, "test_change_record", map[string]any{"tenant_id": w.Tenant, "entity_type": "slot", "entity_id": slots[0].ID})
		before := dep.clinic.stats(t)
		stale := rc.confirm(pid, secret)
		failedTurn(t, stale, contracts.ErrStale)
		noCommit(t, dep, before, "the stale confirmation")
		if st := rc.preparation(pid); !strings.Contains(string(st.Body), `"state":"invalidated"`) {
			t.Errorf("the stale preparation: %s", st)
		}
		// it is not revived by presenting it again
		failedTurn(t, rc.confirm(pid, secret), contracts.ErrStale, contracts.ErrConflict, contracts.ErrReplayed, contracts.ErrExpired)
		noCommit(t, dep, before, "the stale confirmation presented again")

		// the way out is a new preparation, against what the application holds now
		r.proposal(w.Tenant, "appointments.book", "task", map[string]any{"patient": w.PatientName, "slot": slots[0].Display})
		fresh := rc.say("Please book Alma Gatefield "+slots[0].Display, "en")
		if !fresh.ran() || fresh.act() != "request_confirmation" {
			t.Fatalf("the booking was not prepared again: %s", fresh)
		}
		pid2, secret2 := fresh.confirmation()
		if pid2 == pid {
			t.Errorf("the stale preparation was reused")
		}
		newVersion := ""
		for _, dv := range fresh.prepared().DataVersions {
			if string(dv.EntityID) == slots[0].ID {
				newVersion = string(dv.Version)
			}
		}
		if newVersion == "" || newVersion == oldVersion {
			t.Errorf("the new preparation relies on version %q of the slot, the stale one on %q", newVersion, oldVersion)
		}
		ok := rc.confirm(pid2, secret2)
		if !ok.ran() || ok.receipt() == nil || ok.receipt().Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("the new confirmation: %s", ok)
		}
		committedOnce(t, dep, before, "the new confirmation")
	})

	t.Run("a slot someone else booked in the meantime is not booked twice", func(t *testing.T) {
		w := seedWorld(t, dep.clinic, "st2", withSlots(3))
		slots := w.slots(t, w.Reception)
		rc := r.convo(t, w, w.Reception)
		rc.open()
		_, pid, secret := prepareBooking(t, r, w, rc, slots[1])

		// the patient books the same slot for themselves, through the same service
		pc := r.convo(t, w, w.Patient)
		pc.open()
		r.proposal(w.Tenant, "appointments.book", "task", map[string]any{"patient": "me", "slot": slots[1].Display})
		pp := pc.say("Please book me "+slots[1].Display, "en")
		if !pp.ran() || pp.act() != "request_confirmation" {
			t.Fatalf("the patient's booking was not prepared: %s", pp)
		}
		ppid, psecret := pp.confirmation()
		before := dep.clinic.stats(t)
		pok := pc.confirm(ppid, psecret)
		if !pok.ran() || pok.receipt() == nil || pok.receipt().Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("the patient's booking: %s", pok)
		}
		afterPatient := committedOnce(t, dep, before, "the patient's booking")

		// the receptionist's preparation of the same slot can no longer be confirmed
		lost := rc.confirm(pid, secret)
		failedTurn(t, lost, contracts.ErrStale, contracts.ErrConflict)
		if n := dep.clinic.stats(t).commits() - afterPatient.commits(); n != 0 {
			t.Errorf("the lost booking made the application commit %d more times", n)
		}
		if n := dep.clinic.stats(t).totalsSince(before, "appointments"); n != 1 {
			t.Errorf("the slot was booked %d times", n)
		}
		if free := w.slots(t, w.Reception); len(free) != 2 {
			t.Errorf("the application lists %d free slots, want the two nobody booked", len(free))
		}
	})

	t.Run("a preparation that outlived its time commits nothing", func(t *testing.T) {
		w := seedWorld(t, dep.clinic, "ex1", withSlots(3))
		slots := w.slots(t, w.Reception)
		rc := r.convo(t, w, w.Reception)
		rc.open()
		_, pid, secret := prepareBooking(t, r, w, rc, slots[2])
		before := dep.clinic.stats(t)
		r.advance(11 * time.Minute) // the booking preparation lives ten
		failedTurn(t, rc.confirm(pid, secret), contracts.ErrExpired)
		noCommit(t, dep, before, "the expired confirmation")
		// The refusal is enforced on every confirmation; the stored state follows when the service's
		// sweep next runs (alixai serve runs it every thirty seconds).
		if st := r.preparationState(pid); st != "prepared" && st != "expired" {
			t.Errorf("the state of the lapsed preparation is %q", st)
		}
		rep, err := r.co.ExpireDue(context.Background())
		if err != nil || rep.Expired < 1 {
			t.Errorf("the expiry sweep = %+v, %v, want the lapsed preparation expired", rep, err)
		}
		if st := r.preparationState(pid); st != "expired" {
			t.Errorf("the state of the lapsed preparation after the sweep is %q, want expired", st)
		}
		// the action route says the same
		got := rc.confirmAction(pid, secret)
		if got.Status < 400 {
			t.Errorf("an expired preparation was confirmed through the action route: %s", got)
		}
		noCommit(t, dep, before, "the expired confirmation through the action route")
	})

	t.Run("a preparation the user declined, or whose session ended, commits nothing", func(t *testing.T) {
		w := seedWorld(t, dep.clinic, "dc1", withSlots(3))
		slots := w.slots(t, w.Reception)
		before := dep.clinic.stats(t)

		declined := r.convo(t, w, w.Reception)
		declined.open()
		_, pid, secret := prepareBooking(t, r, w, declined, slots[0])
		if rej := declined.call(http.MethodPost, "/v1/actions/"+pid+"/reject", nil); rej.Status != http.StatusOK || !strings.Contains(string(rej.Body), `"state":"rejected"`) {
			t.Fatalf("decline the preparation: %s", rej)
		}
		got := declined.confirmAction(pid, secret)
		if got.Status < 400 || got.code() == "" {
			t.Errorf("a declined preparation was confirmed: %s", got)
		}
		failedTurn(t, declined.confirm(pid, secret), contracts.ErrConflict, contracts.ErrExpired, contracts.ErrReplayed, contracts.ErrStale, contracts.ErrValidationFailed)

		ended := r.convo(t, w, w.Reception)
		ended.open()
		_, pid2, secret2 := prepareBooking(t, r, w, ended, slots[1])
		if end := ended.call(http.MethodDelete, "/v1/sessions/"+ended.session, nil); end.Status != http.StatusOK {
			t.Fatalf("end the session: %s", end)
		}
		got = ended.confirmAction(pid2, secret2)
		if got.Status < 400 {
			t.Errorf("a preparation of an ended session was confirmed: %s", got)
		}
		if st := r.preparationState(pid2); st != "invalidated" {
			t.Errorf("the preparation of the ended session is %q, want invalidated", st)
		}
		if st := r.preparationState(pid); st != "rejected" {
			t.Errorf("the declined preparation is %q, want rejected", st)
		}
		noCommit(t, dep, before, "declined and abandoned preparations")
	})
}
