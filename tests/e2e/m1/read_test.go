package m1

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// factsEqualApplication holds the facts a directive states to the facts the application itself
// states for the same read: same fields, same values, nothing added and nothing missing. This
// is the exit gate's reading of "natural response with exact deterministic facts" for a read: the
// numbers, dates and names come from the application, never from the model.
func factsEqualApplication(t tb, what string, got []act.Fact, want appResult) {
	t.Helper()
	g := factMap(got)
	w := map[string]string{}
	for _, v := range want.Values {
		if v.Display != "" {
			w[v.Field] = v.Display
		} else {
			w[v.Field] = anyString(v.Value.Value)
		}
	}
	if len(g) != len(w) {
		t.Errorf("%s: the response carries %d facts, the application states %d:\n got  %v\n want %v", what, len(g), len(w), g, w)
	}
	for k, v := range w {
		if g[k] != v {
			t.Errorf("%s: fact %s is %q, the application states %q", what, k, g[k], v)
		}
	}
}

func anyString(v any) string { return fmt.Sprint(v) }

// A read workflow completes in natural multi-turn text: a receptionist asks what is free, looks a
// patient up, and asks again in Italian; a patient looking for someone else's record is told what
// the application lets them see. Every read goes through the model's proposal, the policy gateway
// and the MCP adapter to the application, its facts are the application's own, and nothing is
// prepared, committed or written (master plan 21, M1 exit gate: "natural multi-turn text can
// complete read and write workflows").
func TestM1ReadWorkflowE2E(t *testing.T) {
	dep := sharedDeployment(t)
	w := seedWorld(t, dep.clinic, "read")
	r := newRig(t, dep, withTemplateOnly())
	slots := w.slots(t, w.Reception)
	if len(slots) != 3 {
		t.Fatalf("the application lists %d free slots, want the 3 that were seeded", len(slots))
	}
	date := slots[0].Date

	rc := r.convo(t, w, w.Reception)
	opened := rc.open()
	if opened.Status != http.StatusOK || !strings.Contains(string(opened.Body), `"status":"active"`) {
		t.Fatalf("open the session: %s", opened)
	}
	before := dep.clinic.stats(t)

	t.Run("the free appointments, as the application lists them", func(t *testing.T) {
		r.proposal(w.Tenant, "availability.list", "task", map[string]any{"date": date})
		tr := rc.say("Which appointments are free on "+date+"?", "en")
		if !tr.ran() || tr.act() != "present_result" || tr.finish() != "completed" || tr.code() != "" {
			t.Fatalf("%s", tr)
		}
		if tr.prepared() != nil || tr.receipt() != nil || tr.Body.Confirmation != nil {
			t.Fatalf("a read prepared or committed something: %s", tr)
		}
		d, _ := tr.directive()
		factsEqualApplication(t, "availability.list", d.Facts, parseResult(t, w.callAs(t, w.Reception, "availability_list", map[string]any{"date": date})))
		mustState := map[string]bool{}
		for _, name := range d.MustState {
			mustState[string(name)] = true
		}
		checked := 0
		for _, f := range d.Facts {
			if !mustState[string(f.Field)] {
				continue
			}
			if c, ok := stated(f, tr.text()); c {
				checked++
				if !ok {
					t.Errorf("the response does not state %s = %q:\n%s", f.Field, factValue(f), tr.text())
				}
			}
		}
		if checked < 5 {
			t.Errorf("only %d of the facts the response must state could be looked for in it", checked)
		}
		for _, s := range slots {
			if !strings.Contains(tr.text(), s.Display) {
				t.Errorf("the response does not list the slot at %s the application lists:\n%s", s.Display, tr.text())
			}
		}
		rec, ok := r.turnRecordOf(w.Tenant, tr.Body.TurnID)
		if !ok || rec.Act != "present_result" || rec.Response != "template" || rec.ModelCalls != 1 {
			t.Errorf("the audit record of the turn = %+v (found %v)", rec, ok)
		}
	})

	t.Run("a patient is looked up by the application's own search", func(t *testing.T) {
		r.proposal(w.Tenant, "patients.find", "task", map[string]any{"query": "Gatefield"})
		tr := rc.say("Find the patient Gatefield", "en")
		if !tr.ran() || tr.act() != "present_result" || tr.code() != "" {
			t.Fatalf("%s", tr)
		}
		d, _ := tr.directive()
		factsEqualApplication(t, "patients.find", d.Facts, parseResult(t, w.callAs(t, w.Reception, "patients_find", map[string]any{"query": "Gatefield"})))
		if got := factMap(d.Facts)["patient_count"]; got != "1" {
			t.Errorf("patient_count = %q, want the one patient that was seeded", got)
		}
		if !strings.Contains(tr.text(), "Alma Gatefield") {
			t.Errorf("the response does not name the patient:\n%s", tr.text())
		}
	})

	t.Run("the same question in Italian", func(t *testing.T) {
		r.proposal(w.Tenant, "availability.list", "task", map[string]any{"date": date})
		tr := rc.say("Quali visite sono libere il "+date+"?", "it")
		if !tr.ran() || tr.act() != "present_result" || tr.code() != "" {
			t.Fatalf("%s", tr)
		}
		d, _ := tr.directive()
		factsEqualApplication(t, "availability.list (it)", d.Facts, parseResult(t, w.callAs(t, w.Reception, "availability_list", map[string]any{"date": date})))
		rec, ok := r.turnRecordOf(w.Tenant, tr.Body.TurnID)
		if !ok || rec.Language != "it" {
			t.Errorf("the audit record of the turn = %+v (found %v), want an Italian turn", rec, ok)
		}
	})

	t.Run("a patient sees only what the application lets a patient see", func(t *testing.T) {
		pc := r.convo(t, w, w.Patient)
		pc.open()
		// The same search the receptionist made finds nobody: record-level visibility is the
		// application's, and the response states what the application answered.
		r.proposal(w.Tenant, "patients.find", "task", map[string]any{"query": "Gatefield"})
		tr := pc.say("Find the patient Gatefield", "en")
		if !tr.ran() || tr.code() != "" {
			t.Fatalf("%s", tr)
		}
		d, _ := tr.directive()
		want := parseResult(t, w.callAs(t, w.Patient, "patients_find", map[string]any{"query": "Gatefield"}))
		factsEqualApplication(t, "patients.find as a patient", d.Facts, want)
		if strings.Contains(tr.text(), "Alma") || strings.Contains(tr.text(), "Gatefield") {
			t.Errorf("a patient was shown another patient's record:\n%s", tr.text())
		}
	})

	after := dep.clinic.stats(t)
	if n := after.prepares() - before.prepares(); n != 0 {
		t.Errorf("a read workflow made the application prepare %d actions", n)
	}
	if n := after.commits() - before.commits(); n != 0 {
		t.Errorf("a read workflow made the application commit %d actions", n)
	}
	if n := after.totalsSince(before, "appointments"); n != 0 {
		t.Errorf("the application holds %d new appointments after reads", n)
	}
	if after.Calls["availability_list"]-before.Calls["availability_list"] < 4 { // 2 by the service, 2 by this test
		t.Errorf("the application was not asked to list availability: %s", after.callsSince(before))
	}
	if got := r.recs.denials("availability.list") + r.recs.denials("patients.find"); got != 0 {
		t.Errorf("the policy gateway denied %d reads that are allowed", got)
	}
	if n := r.verifyChain(w.Tenant); n < 8 {
		t.Errorf("the tenant's audit chain has %d events after four turns", n)
	}
}
