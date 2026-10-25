package conversation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The conversation tests stand a Go transcription in for the TypeScript clinic
// application. These tests hold the stand-in to what the SIGNED PACKAGE says the
// application answers: every reply it sends, for every operation and capability the tests
// use, validates against the result schema the package declares for that operation
// (package/schemas/*.result.schema.json, the same files the application's own tests hold
// the real application to). A stand-in that drifted into saying something the real
// application would not would fail here, before it could make a conversation test pass for
// the wrong reason.

func compileResultSchema(t testing.TB, name string) *jsonschema.Schema {
	t.Helper()
	path, err := filepath.Abs(filepath.Join(clinicPackageDir, "schemas", name))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the package has no schema %s: %v", name, err)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	url := "file:///" + filepath.ToSlash(path)
	if err := c.AddResource(url, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(url)
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return s
}

// schemaFileFor names the package schema that describes a reply: a resolution has one for
// all four entity types, a read its own, and the four operations of an action capability
// one each (validate, prepare, commit and status; the last two share the receipt schema).
func schemaFileFor(r clinicReply) string {
	if r.Operation == adapters.OpRead && r.Fields["entity_type"] != nil {
		return "resolve.result.schema.json"
	}
	switch r.Operation {
	case adapters.OpRead:
		return r.CapabilityID + ".result.schema.json"
	case adapters.OpValidate:
		return r.CapabilityID + ".validation.result.schema.json"
	case adapters.OpPrepare:
		return r.CapabilityID + ".prepared.result.schema.json"
	}
	return r.CapabilityID + ".receipt.result.schema.json"
}

func TestClinicHostRepliesMatchThePackagesOwnSchemas(t *testing.T) {
	w := newWorld(t)
	sc := receptionScope()
	g := w.guard
	read := func(capability string, fields map[string]any) {
		t.Helper()
		if _, err := g.Read(ctxb(), sc, adapters.ReadRequest{CapabilityID: capability, Fields: fields}); err != nil {
			t.Fatalf("read %s: %v", capability, err)
		}
	}

	// Reads, and a refusal of each kind the clinic gives.
	read("availability.list", map[string]any{"date": "2026-10-14", "days": json.Number("2"), "max_results": json.Number("3")})
	read("availability.list", map[string]any{"date": "2020-01-01"}) // validation_failed
	read("availability.list", map[string]any{"doctor": "doc_nobody0001"})
	read("patients.find", map[string]any{"query": "Maria Rossi"})
	read("patients.find", map[string]any{"query": "Zzzzzz"})
	read("patients.find", map[string]any{"query": "x"})
	read("patients.find", map[string]any{"query": "pat_nobody0001"})
	for _, c := range []struct {
		capability string
		fields     map[string]any
	}{
		{"appointments.book", map[string]any{"entity_type": "patient", "mention": "Maria Rossi"}},
		{"appointments.book", map[string]any{"entity_type": "patient", "mention": "Nobody Here"}},
		{"appointments.book", map[string]any{"entity_type": "patient", "mention": "pat_nobody0001"}},
		{"appointments.book", map[string]any{"entity_type": "slot", "mention": "2026-10-14 10:30"}},
		{"appointments.book", map[string]any{"entity_type": "slot", "mention": "the next available one"}},
		{"appointments.book", map[string]any{"entity_type": "slot", "mention": "whenever"}},
		{"availability.list", map[string]any{"entity_type": "doctor", "mention": "Dr. Verdi"}},
		{"appointments.cancel", map[string]any{"entity_type": "appointment", "mention": refMaria}},
		{"appointments.cancel", map[string]any{"entity_type": "appointment", "mention": "Luca Ferri"}},
	} {
		read(c.capability, c.fields)
	}

	// The three consequential capabilities, validate through status, through the coordinator
	// exactly as a conversation does it.
	for _, c := range []struct {
		capability string
		fields     map[string]any
	}{
		{"appointments.book", map[string]any{"patient": pidLuca, "slot": slot14at1030, "reason": "flu shot"}},
		{"appointments.cancel", map[string]any{"appointment": apptMaria, "reason": "patient_request"}},
		{"patients.update_contact", map[string]any{"patient": pidLuca, "email": "luca.ferri@example.test"}},
	} {
		if _, err := g.Validate(ctxb(), sc, adapters.ValidateRequest{CapabilityID: c.capability, Fields: c.fields}); err != nil {
			t.Fatalf("validate %s: %v", c.capability, err)
		}
		p, err := w.co.Prepare(ctxb(), sc, actions.PrepareInput{Session: w.session(sc).Scope, CapabilityID: c.capability, Fields: c.fields})
		if err != nil {
			t.Fatalf("prepare %s: %v", c.capability, err)
		}
		if !p.Confirmation.IsZero() {
			if _, err := w.co.Confirm(ctxb(), sc, actions.ConfirmInput{PreparationID: string(p.Action.PreparationID), Token: p.Confirmation}); err != nil {
				t.Fatalf("confirm %s: %v", c.capability, err)
			}
		}
		rc, err := w.co.Commit(ctxb(), sc, string(p.Action.PreparationID))
		if err != nil || rc.Status != "committed" {
			t.Fatalf("commit %s: %+v, %v", c.capability, rc, err)
		}
		// Status of the key the coordinator committed with.
		if _, err := g.Status(ctxb(), sc, adapters.StatusRequest{CapabilityID: c.capability, IdempotencyKey: p.Action.IdempotencyKey}); err != nil {
			t.Fatalf("status %s: %v", c.capability, err)
		}
		// A repeated commit answers with the original reply.
		if _, err := g.Commit(ctxb(), sc, adapters.CommitRequest{CapabilityID: c.capability, IdempotencyKey: p.Action.IdempotencyKey, Prepared: p.Action}); err != nil {
			t.Fatalf("repeated commit %s: %v", c.capability, err)
		}
	}
	// Refusals of an action: a patient outside the caller's reach, a slot already taken, a window rule.
	for _, c := range []struct {
		capability string
		fields     map[string]any
	}{
		{"appointments.book", map[string]any{"patient": "pat_nobody0001", "slot": slot14at0900}},
		{"appointments.book", map[string]any{"patient": pidLuca, "slot": slot14at1030}}, // taken above
		{"appointments.book", map[string]any{"patient": pidLuca, "slot": "slot_soon000001"}},
		{"appointments.cancel", map[string]any{"appointment": apptMaria}}, // cancelled above
		{"patients.update_contact", map[string]any{"patient": pidLuca}},
		{"patients.update_contact", map[string]any{"patient": pidLuca, "email": "not-an-email"}},
	} {
		if _, err := g.Validate(ctxb(), sc, adapters.ValidateRequest{CapabilityID: c.capability, Fields: c.fields}); err != nil {
			t.Fatalf("validate %s %v: %v", c.capability, c.fields, err)
		}
	}
	// The doctor role and the patient role see their own slice.
	for _, who := range []struct {
		scope contracts.Scope
		query string
	}{{doctorScope(), "Luca"}, {patientScope(), "Rossi"}} {
		if _, err := g.Read(ctxb(), who.scope, adapters.ReadRequest{CapabilityID: "patients.find", Fields: map[string]any{"query": who.query}}); err != nil {
			t.Fatal(err)
		}
	}

	replies := w.host.repliesSnapshot()
	if len(replies) < 40 {
		t.Fatalf("only %d replies were recorded; the test did not exercise the host", len(replies))
	}
	schemas := map[string]*jsonschema.Schema{}
	statuses := map[string]bool{}
	for i, r := range replies {
		file := schemaFileFor(r)
		s, ok := schemas[file]
		if !ok {
			s = compileResultSchema(t, file)
			schemas[file] = s
		}
		inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(r.Body)))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Validate(inst); err != nil {
			t.Errorf("reply %d (%s %s %v) does not match %s: %v\n%s", i, r.Operation, r.CapabilityID, r.Fields, file, err, r.Body)
		}
		var head struct{ Status string }
		_ = json.Unmarshal(r.Body, &head)
		statuses[fmt.Sprintf("%s/%s", r.Operation, head.Status)] = true
	}
	// Both outcomes of the host were checked, for the operations that have both.
	for _, want := range []string{"read/ok", "read/rejected", "validate/ok", "validate/rejected", "prepare/ok", "commit/ok", "status/ok"} {
		if !statuses[want] {
			t.Errorf("the replies never included %s (saw %v)", want, statuses)
		}
	}
}

// ---- a cancellation reaches its confirmation ----

// The clinic's own prepare result for appointments.cancel carries the effect change
// appointment_status = "cancelled" (examples/reference-apps/clinic-booking, domain/
// capabilities.ts), and the engine's confirmation template states every fact of the directive.
// internal/verify used to read the word "cancelled" in that template as a completion claim,
// refuse the template for the confirmation REQUEST (no receipt exists yet) and end the turn in an
// internal error, so a cancellation could never be confirmed (found while writing these tests,
// reported with task M3-16, fixed by M1-28). The verifier now treats a completion word that is
// the value of a host fact the template states as that fact as data. This test holds the whole
// path: the confirmation is shown, nothing is committed before the user confirms, and then the
// appointment is cancelled and its slot released against a committed receipt.
func TestConvCancelConfirmationRenders(t *testing.T) {
	for _, c := range []struct {
		name, lang, say string
	}{
		{"en", "en", "Cancel appointment APT-7K3Q9X"},
		{"it", "it", "Annulla l'appuntamento APT-7K3Q9X"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			sc := receptionScope()
			w.propose("appointments.cancel", "task", map[string]any{"appointment": refMaria})

			res := w.say(sc, c.lang, c.say)

			// The confirmation request is produced: the act, the preparation, the secret the
			// client presents, and the template that states what is about to be done.
			wantAct(t, res, "request_confirmation")
			if res.Code != "" {
				t.Fatalf("code = %q", res.Code)
			}
			if res.Prepared == nil || res.Confirmation.IsZero() {
				t.Fatalf("no preparation or no confirmation token: %+v", res.Prepared)
			}
			if res.Response.Source != verify.SourceTemplate {
				t.Fatalf("response = %+v", res.Response)
			}
			mustContain(t, res.Response.Text, refMaria, "the confirmation")
			// The host's own word is shown as the host's data, in the language's own template.
			mustContain(t, res.Response.Text, ": cancelled\n", "the confirmation")
			if status, ok := factValue(res, "appointment_status"); !ok || status != "cancelled" {
				t.Fatalf("the directive has no appointment_status = cancelled: %q, %v", status, ok)
			}

			// Nothing is committed before the user confirms: the appointment stands, its slot is
			// still taken, and the one preparation is only prepared.
			w.noCommits()
			if got := w.host.appointment(apptMaria).status; got != "booked" {
				t.Fatalf("the appointment is %s before the confirmation", got)
			}
			if got := w.host.slot("slot_booked0001").status; got != "booked" {
				t.Fatalf("the slot is %s before the confirmation", got)
			}
			if res.Receipt != nil {
				t.Fatalf("receipt = %+v", res.Receipt)
			}
			if states := w.prepStates(); len(states) != 1 || states[0] != "prepared" {
				t.Fatalf("preparations = %v", states)
			}

			// The user confirms: one commit, a committed receipt, the appointment cancelled and
			// its slot released.
			out := w.confirm(sc, res)

			wantAct(t, out, "report_outcome")
			if out.Receipt == nil || out.Receipt.Status != act.ActionReceiptStatusCommitted {
				t.Fatalf("receipt = %+v", out.Receipt)
			}
			if w.host.count(adapters.OpCommit) != 1 || w.host.writes() != 1 {
				t.Fatalf("commits = %d, effects = %d, want exactly one", w.host.count(adapters.OpCommit), w.host.writes())
			}
			if got := w.host.appointment(apptMaria).status; got != "cancelled" {
				t.Fatalf("the appointment is %s after the confirmation", got)
			}
			if got := w.host.slot("slot_booked0001").status; got != "free" {
				t.Fatalf("the slot is %s after the confirmation, want it released", got)
			}
			mustContain(t, out.Response.Text, refMaria, "the outcome")
		})
	}
}
