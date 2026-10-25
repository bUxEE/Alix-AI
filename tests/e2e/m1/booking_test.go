package m1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

var (
	reDate = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	reTime = regexp.MustCompile(`\b\d{1,2}:\d{2}\b`)
	reRef  = regexp.MustCompile(`\bAPT-[0-9A-Z]{6}\b`)
	reID   = regexp.MustCompile(`\b[a-z]{2,8}_[0-9a-z]{10,}\b`)
)

// foreignFacts lists what a response states as a date, a time, a reference or an identifier that
// none of the facts carries: the independent reading of "the response contains exactly the facts
// the application gave", whatever the verifier did.
func foreignFacts(response string, facts []act.Fact) []string {
	var have strings.Builder
	for _, f := range facts {
		have.WriteString(factValue(f))
		have.WriteByte(' ')
		b, _ := json.Marshal(f)
		have.Write(b)
		have.WriteByte(' ')
	}
	text := unescape(response)
	var out []string
	for _, re := range []*regexp.Regexp{reDate, reTime, reRef, reID} {
		for _, m := range re.FindAllString(text, -1) {
			if !strings.Contains(have.String(), m) {
				out = append(out, m)
			}
		}
	}
	return out
}

// allStated holds the response to stating every fact the receipt gives that can be looked for.
func allStated(t tb, what, response string, facts []act.Fact) {
	t.Helper()
	checked := 0
	for _, f := range facts {
		c, ok := stated(f, response)
		if !c {
			continue
		}
		checked++
		if !ok {
			t.Errorf("%s does not state %s = %q:\n%s", what, f.Field, factValue(f), response)
		}
	}
	if checked < 5 {
		t.Errorf("%s: only %d facts could be looked for in the response", what, checked)
	}
}

// sameJSON compares two values by their JSON form.
func sameJSON(t tb, what string, a, b any) {
	t.Helper()
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil || string(ja) != string(jb) {
		t.Errorf("%s differ:\n %s\n %s", what, ja, jb)
	}
}

// change finds the value of a named change of an effect.
func change(e act.Effect, field string) (act.Fact, bool) {
	for _, c := range e.Changes {
		if string(c.Field) == field {
			return c, true
		}
	}
	return act.Fact{}, false
}

// The write workflow of the exit gate, end to end and in natural text: a booking request with a
// field missing is answered with a clarification; the user's next words supply it; the core
// prepares the action with the application and shows its exact effects; a typed "yes" confirms
// nothing; the structured confirmation commits it through the application's idempotent commit;
// and the response is the verified statement of the receipt's facts. The model only ever proposes
// and phrases: it claims a completion before there is a receipt, then a time the application never
// gave, and both are refused by the verifier in favour of the deterministic template.
func TestM1MultiTurnBookingE2E(t *testing.T) {
	dep := sharedDeployment(t)
	w := seedWorld(t, dep.clinic, "book")
	r := newRig(t, dep) // the model phrases every response; every phrasing is scripted
	slots := w.slots(t, w.Reception)
	rc := r.convo(t, w, w.Reception)
	if opened := rc.open(); opened.Status != http.StatusOK {
		t.Fatalf("open the session: %s", opened)
	}
	before := dep.clinic.stats(t)

	// ---- 1. a field is missing: the clarification ----
	const askText = "Which time would you like?"
	r.proposal(w.Tenant, "appointments.book", "task", map[string]any{"patient": w.PatientName})
	r.phrase(w.Tenant, askText)
	t1 := rc.say("Please book Alma Gatefield", "en")
	if !t1.ran() || t1.act() != "ask_missing" || t1.finish() != "awaiting_input" || t1.code() != "" {
		t.Fatalf("turn 1: %s", t1)
	}
	d1, _ := t1.directive()
	if len(d1.MissingFields) != 1 || d1.MissingFields[0] != "slot" || d1.NextQuestion == nil || d1.NextQuestion.Field != "slot" {
		t.Fatalf("turn 1 asks for %v / %+v, want the slot", d1.MissingFields, d1.NextQuestion)
	}
	if t1.text() != askText {
		t.Errorf("the verified model phrasing was not used: %q", t1.text())
	}
	if rec, ok := r.turnRecordOf(w.Tenant, t1.Body.TurnID); !ok || rec.Response != "model" || rec.Act != "ask_missing" {
		t.Errorf("turn 1 audit record = %+v (found %v)", rec, ok)
	}
	s1 := dep.clinic.stats(t)
	if n := appCalls(s1) - appCalls(before); n != 0 {
		t.Fatalf("the application was asked something before the request was complete: %s", s1.callsSince(before))
	}
	pend := rc.pending()
	if pend.Status != http.StatusOK || !strings.Contains(string(pend.Body), `"kind":"clarification"`) || !strings.Contains(string(pend.Body), `"missing_fields":["slot"]`) {
		t.Errorf("the session's pending state after the question: %s", pend)
	}

	// ---- 2. the user supplies it: the prepared action, over server-sent events ----
	r.proposal(w.Tenant, "appointments.book", "task", map[string]any{"slot": "the next available one"})
	r.phrase(w.Tenant, "All set, I have booked the appointment for you.") // a completion claimed with no receipt
	t2, frames := rc.sayStream("The next available one", "en")
	if !t2.ran() || t2.act() != "request_confirmation" || t2.finish() != "awaiting_confirmation" || t2.code() != "" {
		t.Fatalf("turn 2: %s", t2)
	}
	for i, f := range frames {
		if f.ID != fmt.Sprint(i) {
			t.Fatalf("event %d of the stream has id %q: the sequence skips or repeats", i, f.ID)
		}
	}
	order := map[string]int{}
	for i, f := range frames {
		if _, seen := order[f.Event]; !seen {
			order[f.Event] = i
		}
	}
	if last := frames[len(frames)-1]; last.Event != "done" || order["prepared_action"] > order["token"] || order["directive"] > order["prepared_action"] {
		t.Errorf("the events of turn 2 are not in the order directive, prepared_action, text, done: %v", order)
	}
	prep := t2.prepared()
	pid, secret := t2.confirmation()
	if prep == nil || pid == "" || secret == "" || string(prep.PreparationID) != pid {
		t.Fatalf("turn 2 gave no prepared action with its confirmation: %+v / %q", prep, pid)
	}
	if prep.State != act.PreparedActionStatePrepared || prep.CapabilityID != "appointments.book" || prep.RiskLevel != 2 ||
		prep.Confirmation.Mode != act.ConfirmationRequirementModeExplicit {
		t.Errorf("the prepared action = state %s, capability %s, risk %d, confirmation %s", prep.State, prep.CapabilityID, prep.RiskLevel, prep.Confirmation.Mode)
	}
	bound := map[string]bool{}
	for _, b := range prep.Confirmation.Bindings {
		bound[string(b)] = true
	}
	for _, want := range []string{"preparation_id", "principal", "session", "data_versions"} {
		if !bound[want] {
			t.Errorf("the confirmation is not bound to %s: %v", want, prep.Confirmation.Bindings)
		}
	}
	// the exact effects, against what the application itself holds
	if len(prep.Effects) != 2 {
		t.Fatalf("the prepared action has %d effects: %+v", len(prep.Effects), prep.Effects)
	}
	create, notify := prep.Effects[0], prep.Effects[1]
	if create.Operation != "create" || create.EntityType != "appointment" || create.External {
		t.Errorf("the first effect = %+v, want the creation of an appointment inside the application", create)
	}
	if f, ok := change(create, "patient_id"); !ok || factValue(f) != w.PatientID {
		t.Errorf("the appointment is not for the patient %s: %+v", w.PatientID, create.Changes)
	}
	if f, ok := change(create, "slot_id"); !ok || factValue(f) != slots[0].ID {
		t.Errorf("the appointment is not in the slot %s the user asked for (the next available one): %+v", slots[0].ID, create.Changes)
	}
	if f, ok := change(create, "appointment_starts_at"); !ok || factValue(f) != slots[0].Display || !strings.Contains(mustJSON(f), slots[0].UTC) {
		t.Errorf("the appointment does not start when the application's slot does (%s, %s): %+v", slots[0].Display, slots[0].UTC, create.Changes)
	}
	if notify.Operation != "send" || notify.EntityType != "notification" || !notify.External {
		t.Errorf("the second effect = %+v, want the booking message, which leaves the application", notify)
	}
	versions := map[string]string{}
	for _, dv := range prep.DataVersions {
		versions[string(dv.EntityType)+":"+string(dv.EntityID)] = string(dv.Version)
	}
	if versions["slot:"+slots[0].ID] == "" || versions["patient:"+w.PatientID] == "" {
		t.Errorf("the preparation does not pin the data it relied on: %+v", prep.DataVersions)
	}
	if ttl := mustTime(t, string(prep.ExpiresAt)).Sub(mustTime(t, string(prep.PreparedAt))); ttl != 10*time.Minute {
		t.Errorf("the preparation lives %s, the package says ten minutes", ttl)
	}
	// nothing was written, the response is the template and not the model's false claim
	s2 := dep.clinic.stats(t)
	if s2.commits()-before.commits() != 0 || s2.totalsSince(before, "appointments") != 0 {
		t.Fatalf("something was committed before the user confirmed: %s", s2.callsSince(before))
	}
	if s2.prepares()-before.prepares() != 1 {
		t.Errorf("the application prepared %d actions, want 1: %s", s2.prepares()-before.prepares(), s2.callsSince(before))
	}
	// the patient and the slot were resolved by the application, from the user's words: the model
	// proposed a mention, never a record
	for _, tool := range []string{"resolve_patient", "resolve_slot"} {
		if n := s2.Calls[tool] - before.Calls[tool]; n != 1 {
			t.Errorf("the application was asked %s %d times, want once: %s", tool, n, s2.callsSince(before))
		}
	}
	if strings.Contains(t2.text(), "All set") || strings.Contains(strings.ToLower(t2.text()), "i have booked") {
		t.Errorf("the model's completion claim reached the user:\n%s", t2.text())
	}
	if rec, ok := r.turnRecordOf(w.Tenant, t2.Body.TurnID); !ok || rec.Response != "template" {
		t.Errorf("turn 2 audit record = %+v (found %v), want the template", rec, ok)
	}
	for _, want := range []string{slots[0].Display, "Alma Gatefield", w.DoctorName} {
		if !strings.Contains(unescape(t2.text()), want) {
			t.Errorf("the confirmation does not state %q:\n%s", want, t2.text())
		}
	}
	if extra := foreignFacts(t2.text(), directiveFacts(t2)); len(extra) > 0 {
		t.Errorf("the confirmation states %v, which no fact carries", extra)
	}
	// the preparation is what the API reports for it, and the secret is shown once, to the client
	got := rc.preparation(pid)
	var inspected act.PreparedAction
	if got.Status != http.StatusOK || json.Unmarshal(got.Body, &inspected) != nil {
		t.Fatalf("GET the preparation: %s", got)
	}
	sameJSON(t, "the announced preparation and the one the API reports", prep, inspected)
	pend = rc.pending()
	for what, body := range map[string]string{
		"the events": mustJSON(t2.Body.Events), "the preparation": string(got.Body), "the pending state": string(pend.Body),
		"the service's log": r.logs.String(),
	} {
		if strings.Contains(body, secret) {
			t.Errorf("the confirmation secret is in %s", what)
		}
	}
	if !strings.Contains(string(pend.Body), `"kind":"confirmation"`) {
		t.Errorf("the session's pending state while waiting for the confirmation: %s", pend)
	}

	// ---- 3. a typed "yes" is not a confirmation ----
	r.phrase(w.Tenant, "Shall I go ahead?") // the pending request is shown again
	yes := rc.say("yes", "en")
	if !yes.ran() || yes.receipt() != nil || yes.Body.Confirmation != nil {
		t.Fatalf("a typed yes: %s", yes)
	}
	if again := rc.preparation(pid); !strings.Contains(string(again.Body), `"state":"prepared"`) {
		t.Errorf("the preparation after a typed yes: %s", again)
	}
	if s := dep.clinic.stats(t); s.commits()-before.commits() != 0 {
		t.Fatalf("a typed yes committed: %s", s.callsSince(before))
	}

	// ---- 4. the confirmation: the receipt and the verified outcome ----
	r.phraseFunc(w.Tenant, "an outcome that states exactly the receipt", func(c pfake.Call) (string, error) {
		rc, err := r.co.Receipt(context.Background(), c.Scope, pid)
		if err != nil {
			return "", err
		}
		f := factMap(rc.Facts)
		return fmt.Sprintf("Done: appointment %s is %s for %s with %s on %s at %s.",
			f["appointment_reference"], f["appointment_status"], f["patient_name"], f["doctor_name"], f["appointment_starts_at"], f["site_name"]), nil
	})
	t3 := rc.confirm(pid, secret)
	if !t3.ran() || t3.act() != "report_outcome" || t3.finish() != "completed" || t3.code() != "" {
		t.Fatalf("turn 3: %s", t3)
	}
	receipt := t3.receipt()
	if receipt == nil || receipt.Status != act.ActionReceiptStatusCommitted || string(receipt.PreparationID) != pid || receipt.CapabilityID != "appointments.book" {
		t.Fatalf("the receipt = %+v", receipt)
	}
	var appointmentID string
	for _, rec := range receipt.Records {
		if rec.EntityType == "appointment" {
			appointmentID = string(rec.EntityID)
		}
	}
	rf := factMap(receipt.Facts)
	if appointmentID == "" || rf["appointment_reference"] == "" {
		t.Fatalf("the receipt names no appointment record or reference: %+v", receipt)
	}
	// the application agrees: one commit, one new appointment, found by the reference the receipt
	// states, the same record the receipt names, and the slot is gone from what is free
	s3 := dep.clinic.stats(t)
	if n := s3.commitsOf("appointments_book") - before.commitsOf("appointments_book"); n != 1 || s3.commits()-before.commits() != 1 {
		t.Errorf("the application was asked to commit %d times, want exactly once: %s", s3.commits()-before.commits(), s3.callsSince(before))
	}
	if s3.totalsSince(before, "appointments") != 1 || s3.totalsSince(before, "notifications") != 1 {
		t.Errorf("the application's rows moved by %d appointments and %d notifications, want one of each",
			s3.totalsSince(before, "appointments"), s3.totalsSince(before, "notifications"))
	}
	if found := w.appointmentByReference(t, w.Reception, rf["appointment_reference"]); len(found) != 1 || found[0] != appointmentID {
		t.Errorf("the application finds %v under the reference %s, the receipt names %s", found, rf["appointment_reference"], appointmentID)
	}
	if left := w.slots(t, w.Reception); len(left) != 2 || left[0].ID != slots[1].ID {
		t.Errorf("after the booking the application lists %+v as free, want the two slots that were not booked", left)
	}
	// the response states exactly the receipt's facts
	rfacts := receipt.Facts
	if rf["patient_name"] != "Alma Gatefield" || rf["appointment_starts_at"] != slots[0].Display || rf["doctor_name"] != w.DoctorName {
		t.Errorf("the receipt's facts disagree with the application's data: %v", rf)
	}
	allStated(t, "the outcome", t3.text(), rfacts)
	if extra := foreignFacts(t3.text(), rfacts); len(extra) > 0 {
		t.Errorf("the outcome states %v, which no receipt fact carries:\n%s", extra, t3.text())
	}
	if rec, ok := r.turnRecordOf(w.Tenant, t3.Body.TurnID); !ok || rec.Response != "model" || rec.Receipt != string(receipt.ReceiptID) {
		t.Errorf("turn 3 audit record = %+v (found %v), want the model's verified text and the receipt", rec, ok)
	}
	stored := rc.call(http.MethodGet, "/v1/actions/"+pid+"/receipt", nil)
	var storedReceipt act.ActionReceipt
	if stored.Status != http.StatusOK || json.Unmarshal(stored.Body, &storedReceipt) != nil {
		t.Fatalf("GET the receipt: %s", stored)
	}
	sameJSON(t, "the receipt of the turn and the stored one", receipt, storedReceipt)

	// ---- 5. a second booking, by time, whose outcome the model gets wrong ----
	r.proposal(w.Tenant, "appointments.book", "task", map[string]any{"patient": w.PatientName, "slot": slots[1].Display})
	r.phrase(w.Tenant, "Shall I go ahead?") // leaves out every fact it must state
	t4 := rc.say("Book Alma Gatefield "+slots[1].Display, "en")
	if !t4.ran() || t4.act() != "request_confirmation" {
		t.Fatalf("second booking: %s", t4)
	}
	if rec, ok := r.turnRecordOf(w.Tenant, t4.Body.TurnID); !ok || rec.Response != "template" {
		t.Errorf("a confirmation that omits what the user must see was shown as the model wrote it: %+v", rec)
	}
	pid2, secret2 := t4.confirmation()
	if p2 := t4.prepared(); p2 == nil || p2.Effects[0].EntityType != "appointment" {
		t.Fatalf("second preparation: %+v", p2)
	} else if f, ok := change(p2.Effects[0], "slot_id"); !ok || factValue(f) != slots[1].ID {
		t.Errorf("the second booking is not in the slot the user named by its time: %+v", p2.Effects[0].Changes)
	}
	const lie = "Your appointment is confirmed for 2026-10-04 11:15 with Dr. Nobody at Gate Site A."
	r.phrase(w.Tenant, lie)
	t5 := rc.confirm(pid2, secret2)
	if !t5.ran() || t5.act() != "report_outcome" || t5.receipt() == nil || t5.receipt().Status != act.ActionReceiptStatusCommitted {
		t.Fatalf("second outcome: %s", t5)
	}
	rf2 := factMap(t5.receipt().Facts)
	if strings.Contains(t5.text(), "11:15") || strings.Contains(t5.text(), "Nobody") {
		t.Errorf("the model's invented time and doctor reached the user:\n%s", t5.text())
	}
	if rec, ok := r.turnRecordOf(w.Tenant, t5.Body.TurnID); !ok || rec.Response != "template" {
		t.Errorf("turn 5 audit record = %+v (found %v), want the template", rec, ok)
	}
	allStated(t, "the second outcome", t5.text(), t5.receipt().Facts)
	if extra := foreignFacts(t5.text(), t5.receipt().Facts); len(extra) > 0 {
		t.Errorf("the second outcome states %v, which no receipt fact carries", extra)
	}
	if rf2["appointment_starts_at"] != slots[1].Display || rf2["appointment_reference"] == rf["appointment_reference"] {
		t.Errorf("the second receipt = %v", rf2)
	}
	s5 := dep.clinic.stats(t)
	if s5.commits()-before.commits() != 2 || s5.totalsSince(before, "appointments") != 2 {
		t.Errorf("two bookings made %d commits and %d appointments: %s", s5.commits()-before.commits(), s5.totalsSince(before, "appointments"), s5.callsSince(before))
	}

	// ---- the record of it all ----
	events := eventTypes(r.chainEvents(w.Tenant))
	want := []string{"action.prepared", "action.confirmed", "action.committing", "action.committed"}
	if !subsequence(events, want) {
		t.Errorf("the audit chain lacks the lifecycle %v in order: %v", want, events)
	}
	if n := r.verifyChain(w.Tenant); n != len(events) {
		t.Errorf("the chain verifies %d events, the log holds %d", n, len(events))
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// directiveFacts are the facts of the turn's directive.
func directiveFacts(tr *turn) []act.Fact {
	d, _ := tr.directive()
	return d.Facts
}

// subsequence reports whether want occurs in have in order (not necessarily adjacent).
func subsequence(have, want []string) bool {
	i := 0
	for _, h := range have {
		if i < len(want) && h == want[i] {
			i++
		}
	}
	return i == len(want)
}

func mustTime(t tb, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("%q is not a timestamp: %v", s, err)
	}
	return ts
}
