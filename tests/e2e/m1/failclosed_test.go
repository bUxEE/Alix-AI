package m1

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// prepareBooking has the principal's conversation prepare the booking of the world's patient into
// the slot, in one turn, and returns the turn with the confirmation the client holds. The turn must
// have asked for the confirmation: nothing is committed by it.
func prepareBooking(t tb, r *rig, w *world, c *convo, slot slotInfo) (tr *turn, preparationID, secret string) {
	t.Helper()
	r.proposal(w.Tenant, "appointments.book", "task", map[string]any{"patient": w.PatientName, "slot": slot.Display})
	tr = c.say("Please book Alma Gatefield "+slot.Display, "en")
	if !tr.ran() || tr.act() != "request_confirmation" || tr.prepared() == nil {
		t.Fatalf("the booking was not prepared: %s", tr)
	}
	preparationID, secret = tr.confirmation()
	if preparationID == "" || secret == "" {
		t.Fatalf("the prepared booking came with no confirmation: %s", tr)
	}
	return tr, preparationID, secret
}

// attemptRow runs one attempt of the matrix and holds it to the exit gate's rule: it yields its
// ErrCode, and the application is never asked to commit. When the attempt must not reach the
// application at all, the application is not asked anything.
func attemptRow(t *testing.T, dep *deployment, want contracts.ErrCode, mustNotTouchApp bool, attempt func() (contracts.ErrCode, string)) {
	t.Helper()
	before := dep.clinic.stats(t)
	got, what := attempt()
	after := dep.clinic.stats(t)
	if got != want {
		t.Errorf("the attempt ended with %q, want %q: %s", got, want, what)
	}
	if n := after.commits() - before.commits(); n != 0 {
		t.Errorf("the application was asked to commit %d times: %s", n, after.callsSince(before))
	}
	if n := after.totalsSince(before, "appointments"); n != 0 {
		t.Errorf("the application holds %d new appointments", n)
	}
	if mustNotTouchApp && appCalls(after) != appCalls(before) {
		t.Errorf("the application was asked something for a request that had to be refused before it: %s", after.callsSince(before))
	}
}

// The fail-closed matrix of the M1 exit gate (master plan 3 rule 14, 21): every way a request can
// be unauthorised, unknown, malformed, from another tenant, carried by an expired token or aimed at
// a quarantined application ends in its typed ErrCode, and the clinic application, whose own
// counters are read before and after each attempt, is never asked to commit. A row that prepares a
// valid booking first, attacks it, and then lets its owner confirm shows the counter counts: the
// owner's confirmation is the one commit of the row, so a zero above is not a counter that sees
// nothing.
func TestM1FailClosedMatrix(t *testing.T) {
	dep := sharedDeployment(t)
	w := seedWorld(t, dep.clinic, "fc", withSlots(5))
	r := newRig(t, dep, withTemplateOnly())
	slots := w.slots(t, w.Reception)
	if len(slots) != 5 {
		t.Fatalf("the application lists %d slots, want 5", len(slots))
	}
	start := dep.clinic.stats(t)
	controls := 0 // the commits that the rows themselves make, each of them by the rightful owner

	refused := func(t *testing.T, tr *turn, wantAct, code string) {
		t.Helper()
		if !tr.ran() || tr.act() != wantAct || tr.finish() != "failed" || string(tr.code()) != code {
			t.Fatalf("%s", tr)
		}
		if tr.prepared() != nil || tr.receipt() != nil || tr.Body.Confirmation != nil {
			t.Fatalf("a refused request prepared or committed something: %s", tr)
		}
	}

	t.Run("unauthorised capability: the policy denies a role the package does not grant it", func(t *testing.T) {
		// The host's token carries the booking permission although the clinic's doctors do not
		// hold it; the policy gateway decides on the package's policy, not on the token alone.
		dc := r.convo(t, w, w.Doctor)
		dc.open()
		all := []string{"availability.list", "patients.find", "appointments.book", "appointments.cancel", "patients.update_contact"}
		denied := r.recs.denials("appointments.book")
		attemptRow(t, dep, contracts.ErrPermissionDenied, true, func() (contracts.ErrCode, string) {
			// A model that has been talked into it proposes the booking anyway.
			r.proposal(w.Tenant, "appointments.book", "task", map[string]any{"patient": w.PatientName, "slot": slots[0].Display})
			tr := readTurn(t, r.call(w.tokenScoped(t, w.Doctor, dc.session, 900, all), http.MethodPost, dc.turnsPath(),
				map[string]any{"text": "Please book Alma Gatefield " + slots[0].Display, "language": "en"}))
			refused(t, tr, "refuse", string(contracts.ErrPermissionDenied))
			return tr.code(), tr.String()
		})
		if r.recs.denials("appointments.book") != denied+1 {
			t.Errorf("the denial is not on the policy record")
		}
		// the model was never offered the capability
		last := r.model.Prompts()
		if p := last[len(last)-1]; p.Contains("appointments.book") || !p.Contains("availability.list") {
			t.Errorf("the doctor's prompt offered %v", p.Texts())
		}
	})

	t.Run("unauthorised capability: a token that does not grant it", func(t *testing.T) {
		rc := r.convo(t, w, w.Reception)
		rc.open()
		attemptRow(t, dep, contracts.ErrScopeViolation, true, func() (contracts.ErrCode, string) {
			r.proposal(w.Tenant, "appointments.book", "task", map[string]any{"patient": w.PatientName, "slot": slots[0].Display})
			tr := readTurn(t, r.call(w.tokenScoped(t, w.Reception, rc.session, 900, []string{"availability.list", "patients.find"}),
				http.MethodPost, rc.turnsPath(), map[string]any{"text": "Please book Alma Gatefield " + slots[0].Display, "language": "en"}))
			refused(t, tr, "refuse", string(contracts.ErrScopeViolation))
			return tr.code(), tr.String()
		})
	})

	t.Run("unknown capability", func(t *testing.T) {
		rc := r.convo(t, w, w.Reception)
		rc.open()
		for _, id := range []string{"appointments.delete", "system.shell", "clinic-booking.admin.export_everything"} {
			attemptRow(t, dep, contracts.ErrUnknownCapability, true, func() (contracts.ErrCode, string) {
				r.proposal(w.Tenant, id, "task", map[string]any{"patient": w.PatientName})
				tr := rc.say("Please book Alma Gatefield", "en")
				refused(t, tr, "refuse", string(contracts.ErrUnknownCapability))
				return tr.code(), tr.String()
			})
		}
	})

	t.Run("malformed proposal", func(t *testing.T) {
		rc := r.convo(t, w, w.Reception)
		rc.open()
		for name, out := range map[string]string{
			"prose instead of a proposal": "Sure, I will book it!",
			"an empty output":             ``,
			"not an object":               `[]`,
			"fields that are not an object": `{"capability_id":"appointments.book","fields":"nope","entity_refs":[],"intent":"task",` +
				`"uncertainty":{"overall":0.9}}`,
			"authority the model does not have": `{"capability_id":"appointments.book","fields":{"patient":"Alma Gatefield","slot":"the next available one"},` +
				`"entity_refs":[],"intent":"task","uncertainty":{"overall":0.9},"confirmed":true,"commit":true}`,
			"an intent that is not one": `{"capability_id":"appointments.book","fields":{"patient":"Alma Gatefield"},"entity_refs":[],` +
				`"intent":"commit_now","uncertainty":{"overall":0.9}}`,
		} {
			t.Run(name, func(t *testing.T) {
				attemptRow(t, dep, contracts.ErrSchemaInvalid, true, func() (contracts.ErrCode, string) {
					r.rawProposal(w.Tenant, name, out)
					tr := rc.say("Please book Alma Gatefield", "en")
					refused(t, tr, "report_failure", string(contracts.ErrSchemaInvalid))
					return tr.code(), tr.String()
				})
			})
		}
	})

	t.Run("malformed values inside a valid proposal", func(t *testing.T) {
		rc := r.convo(t, w, w.Reception)
		rc.open()
		attemptRow(t, dep, "", true, func() (contracts.ErrCode, string) {
			r.rawProposal(w.Tenant, "values of the wrong type", `{"capability_id":"appointments.book","fields":{"patient":{"id":"x"},"slot":42},`+
				`"entity_refs":[],"intent":"task","uncertainty":{"overall":0.9}}`)
			tr := rc.say("Please book Alma Gatefield", "en")
			if !tr.ran() || tr.act() != "ask_invalid" || tr.finish() != "awaiting_input" || tr.prepared() != nil {
				t.Fatalf("%s", tr)
			}
			d, _ := tr.directive()
			if len(d.InvalidFields) != 2 || d.InvalidFields[0].Code != "validation_failed" {
				t.Errorf("invalid fields = %+v, want both values refused as validation_failed", d.InvalidFields)
			}
			return "", ""
		})
	})

	t.Run("names the model invents are dropped, never forwarded to the application", func(t *testing.T) {
		rc := r.convo(t, w, w.Reception)
		rc.open()
		// The proposal carries a tenant and an idempotency key of the model's own making. The
		// capability declares neither, so neither is kept: the preparation is made in the caller's
		// scope, with a key the core minted.
		r.rawProposal(w.Tenant, "invented fields", `{"capability_id":"appointments.book","fields":{"patient":"Alma Gatefield","slot":"`+slots[4].Display+
			`","tenant_id":"tnt_somebody_else","idempotency_key":"idem_chosen_by_the_model"},"entity_refs":[],"intent":"task","uncertainty":{"overall":0.9}}`)
		tr := rc.say("Please book Alma Gatefield "+slots[4].Display, "en")
		if !tr.ran() || tr.act() != "request_confirmation" || tr.prepared() == nil {
			t.Fatalf("%s", tr)
		}
		prep := tr.prepared()
		if key := string(prep.IdempotencyKey); strings.Contains(key, "chosen_by_the_model") || !strings.HasPrefix(key, "idem_") {
			t.Errorf("the idempotency key is %q, want one the core minted", key)
		}
		pid, _ := tr.confirmation()
		if rep := rc.preparation(pid); rep.Status != http.StatusOK {
			t.Errorf("the caller cannot see its own preparation: %s", rep)
		}
		if rej := rc.call(http.MethodPost, "/v1/actions/"+pid+"/reject", nil); rej.Status != http.StatusOK {
			t.Errorf("reject the preparation: %s", rej)
		}
	})

	t.Run("wrong tenant, wrong site, wrong principal", func(t *testing.T) {
		other := seedWorld(t, dep.clinic, "fcother")
		rc := r.convo(t, w, w.Reception)
		rc.open()
		_, pid, secret := prepareBooking(t, r, w, rc, slots[1])

		attackers := map[string]*convo{
			"another clinic group's receptionist":              r.convo(t, other, other.Reception),
			"the same group's receptionist of another site":    r.convo(t, w, w.ReceptionB),
			"another principal of the same site (the patient)": r.convo(t, w, w.Patient),
		}
		for name, a := range attackers {
			attemptRow(t, dep, contracts.ErrScopeViolation, true, func() (contracts.ErrCode, string) {
				a.session = rc.session // the same session id: nothing may be inherited from it
				got := a.confirmAction(pid, secret)
				if got.code() != contracts.ErrScopeViolation {
					return got.code(), name + ": " + got.String()
				}
				for _, probe := range []apiReply{
					a.preparation(pid),
					a.call(http.MethodPost, "/v1/actions/"+pid+"/reject", nil),
					a.call(http.MethodGet, "/v1/actions/"+pid+"/receipt", nil),
				} {
					if probe.code() != contracts.ErrScopeViolation {
						return probe.code(), name + ": " + probe.String()
					}
				}
				return got.code(), name
			})
		}
		// a preparation of another scope is indistinguishable from one that does not exist
		foreign := attackers["another clinic group's receptionist"]
		ghost := foreign.preparation("prep_01M000000000000000000000AA")
		real := foreign.preparation(pid)
		if ghost.Status != real.Status || ghost.code() != real.code() || msg(ghost) != msg(real) {
			t.Errorf("another tenant's preparation answers differently from one that does not exist:\n %s\n %s", real, ghost)
		}
		// the same confirmation carried by a turn of another tenant's conversation
		before := dep.clinic.stats(t)
		tr := foreign.confirm(pid, secret)
		if !tr.ran() || tr.receipt() != nil || tr.finish() != "failed" || tr.code() == "" {
			t.Errorf("another tenant's turn confirmed something: %s", tr)
		}
		if dep.clinic.stats(t).commits() != before.commits() {
			t.Errorf("another tenant's turn made the application commit")
		}

		// none of it spent or harmed the preparation: its owner confirms, once, and it commits once
		if st := rc.preparation(pid); !strings.Contains(string(st.Body), `"state":"prepared"`) {
			t.Fatalf("the attacks changed the preparation: %s", st)
		}
		before = dep.clinic.stats(t)
		ok := rc.confirm(pid, secret)
		if !ok.ran() || ok.receipt() == nil || ok.receipt().Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("the owner's confirmation: %s", ok)
		}
		after := dep.clinic.stats(t)
		if after.commits()-before.commits() != 1 {
			t.Errorf("the owner's confirmation made %d commits, want 1: %s", after.commits()-before.commits(), after.callsSince(before))
		}
		controls++
	})

	t.Run("expired token", func(t *testing.T) {
		rc := r.convo(t, w, w.Reception)
		rc.open()
		_, pid, secret := prepareBooking(t, r, w, rc, slots[2])
		short := w.tokenScoped(t, w.Reception, rc.session, 60, nil)
		r.clk.Advance(2 * time.Minute) // the token's minute is over; the preparation's ten are not
		attemptRow(t, dep, contracts.ErrExpired, true, func() (contracts.ErrCode, string) {
			got := r.call(short, http.MethodPost, "/v1/actions/"+pid+"/confirm", map[string]any{"token": secret})
			return got.code(), got.String()
		})
		attemptRow(t, dep, contracts.ErrExpired, true, func() (contracts.ErrCode, string) {
			got := r.call(w.tokenScoped(t, w.Reception, rc.session, 60, nil), http.MethodPost, rc.turnsPath(), map[string]any{"text": "hello"})
			_ = got
			return got.code(), got.String()
		})
		// a token that was already used, and an expired one, are different refusals
		used := w.token(t, w.Reception, rc.session)
		if first := r.call(used, http.MethodGet, "/v1/sessions/"+rc.session, nil); first.Status != http.StatusOK {
			t.Fatalf("the first use of a token: %s", first)
		}
		attemptRow(t, dep, contracts.ErrReplayed, true, func() (contracts.ErrCode, string) {
			got := r.call(used, http.MethodPost, "/v1/actions/"+pid+"/confirm", map[string]any{"token": secret})
			return got.code(), got.String()
		})
		// a request with no token at all
		attemptRow(t, dep, contracts.ErrUnauthenticated, true, func() (contracts.ErrCode, string) {
			got := r.call("", http.MethodPost, "/v1/actions/"+pid+"/confirm", map[string]any{"token": secret})
			return got.code(), got.String()
		})

		// the preparation survived all of it and its owner, with a fresh token, commits it once
		before := dep.clinic.stats(t)
		ok := rc.confirm(pid, secret)
		if !ok.ran() || ok.receipt() == nil || ok.receipt().Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("the owner's confirmation with a fresh token: %s", ok)
		}
		if after := dep.clinic.stats(t); after.commits()-before.commits() != 1 {
			t.Errorf("the owner's confirmation made %d commits, want 1", after.commits()-before.commits())
		}
		controls++
	})

	t.Run("quarantined application", func(t *testing.T) {
		qdep := newInstalledDeployment(t).with(t)
		qw := seedWorld(t, qdep.clinic, "fcq")
		qr := newRig(t, qdep, withTemplateOnly())
		qc := qr.convo(t, qw, qw.Reception)
		// the package is installed and has never passed its audit: nothing is served for it
		for name, call := range map[string]func() apiReply{
			"open a session": qc.open,
			"say something": func() apiReply {
				return qc.call(http.MethodPost, qc.turnsPath(), map[string]any{"text": "Please book Alma Gatefield"})
			},
			"confirm":           func() apiReply { return qc.confirmAction("prep_01M000000000000000000000AA", "cfm_x") },
			"inspect":           func() apiReply { return qc.call(http.MethodGet, "/v1/actions/prep_01M000000000000000000000AA", nil) },
			"the pending state": qc.pending,
		} {
			attemptRow(t, qdep, contracts.ErrQuarantined, true, func() (contracts.ErrCode, string) {
				got := call()
				return got.code(), name + ": " + got.String()
			})
		}
		if n := appCalls(qdep.clinic.stats(t)); n != 0 {
			t.Errorf("the application was asked %d things on behalf of a quarantined package", n)
		}
		if n := qr.model.Calls(); n != 0 {
			t.Errorf("the model was called %d times for a quarantined application", n)
		}
	})

	// the control that gives every zero above its meaning: a well-formed booking by the right
	// principal commits exactly once, and the counter sees it
	t.Run("control: the right request commits once", func(t *testing.T) {
		rc := r.convo(t, w, w.Reception)
		rc.open()
		_, pid, secret := prepareBooking(t, r, w, rc, slots[3])
		before := dep.clinic.stats(t)
		ok := rc.confirm(pid, secret)
		if !ok.ran() || ok.receipt() == nil || ok.receipt().Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("the confirmation: %s", ok)
		}
		if after := dep.clinic.stats(t); after.commits()-before.commits() != 1 {
			t.Errorf("a confirmed booking made %d commits, want 1", after.commits()-before.commits())
		}
		controls++
	})

	end := dep.clinic.stats(t)
	if n := end.commits() - start.commits(); n != controls {
		t.Errorf("the application was asked to commit %d times in the whole matrix, want %d (one per owner's confirmation): %s",
			n, controls, end.callsSince(start))
	}
	if n := end.totalsSince(start, "appointments"); n != controls {
		t.Errorf("the application holds %d new appointments, want %d", n, controls)
	}
	if !subsequence(eventTypes(r.chainEvents(w.Tenant)), []string{"policy.decision", "action.prepared", "action.committed"}) {
		t.Errorf("the audit chain does not show the decisions and the lifecycle: %v", eventTypes(r.chainEvents(w.Tenant)))
	}
	r.verifyChain(w.Tenant)
}

// msg is the message of an error envelope.
func msg(a apiReply) string {
	var e contracts.Error
	_ = json.Unmarshal(a.Body, &e)
	return e.Message
}
