package verify

import (
	"slices"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// A completion word inside the VALUE of a host fact the response states is the host's data, not
// the response saying something was done: the clinic's own prepare result for a cancellation
// carries appointment_status = "cancelled", and the confirmation that asks the user whether to go
// ahead must be able to show it (task M1-28). These tests hold both halves of that rule: the data
// passes, and every way a model could borrow the word to claim completion still does not.

// confirmationOf is a request for confirmation that states the given facts.
func confirmationOf(facts ...actions.Fact) *conversation.DialogueDirective {
	must := make([]string, len(facts))
	for i, f := range facts {
		must[i] = string(f.Field)
	}
	d := directive(conversation.DialogueDirectiveActRequestConfirmation, facts, must...)
	prep := actions.PreparationID(prepID)
	d.PreparationID = &prep
	d.Confirmation = actions.ConfirmationRequirement{Mode: actions.ConfirmationRequirementModeExplicit}
	d.PermittedNextActions = []actions.PermittedNextAction{
		{Action: actions.PermittedNextActionActionConfirm},
		{Action: actions.PermittedNextActionActionReject},
	}
	return d
}

// statedLine is one fact as the template and a model state it: "- <label>: <value>".
func statedLine(opts Options, field, value string) string {
	return "- " + fieldLabel(opts, field) + ": " + value
}

// SEC-011, SEC-047 (the other half of TestCompletionWordInModelTextStillNeedsAReceipt).
func TestHostFactValueWithCompletionWordIsStatedDataNotAClaim(t *testing.T) {
	itOpts := Options{Language: LangIT, FieldLabels: map[string]string{
		"appointment_status": "Stato dell'appuntamento", "doctor": "Medico",
	}}
	customEN := Options{Language: LangEN, FieldLabels: map[string]string{"appointment_status": "Current state"}}
	for _, c := range []struct {
		name  string
		opts  Options
		fact  actions.Fact
		value string // what is stated after the label
	}{
		{"cancelled in the clinic's appointment_status", en, textFact("appointment_status", "cancelled"), "cancelled"},
		{"booked", en, textFact("appointment_status", "booked"), "booked"},
		{"paid", en, textFact("payment_state", "paid"), "paid"},
		{"canceled in the American spelling", en, textFact("appointment_status", "canceled"), "canceled"},
		{"a word of any application, not only the clinic's", en, textFact("payment", "refunded"), "refunded"},
		{"under the application's own label", customEN, textFact("appointment_status", "cancelled"), "cancelled"},
		{"an Italian participle", itOpts, textFact("appointment_status", "annullato"), "annullato"},
		{"an English value in an Italian response", itOpts, textFact("appointment_status", "cancelled"), "cancelled"},
		{"several words, one of them a completion word", en, textFact("appointment_status", "booked and paid"), "booked and paid"},
		{"a completion phrase", en, textFact("setup_state", "all set"), "all set"},
		{"an Italian completion phrase", it, textFact("setup_state", "con successo"), "con successo"},
		{"the host's display form", en, withDisplay(intFact("state_code", 3), "cancelled"), "cancelled"},
	} {
		t.Run(c.name, func(t *testing.T) {
			label := fieldLabel(c.opts, string(c.fact.Field))
			doctor := textFact("doctor", "Dr. Rossi")
			d := confirmationOf(doctor, c.fact)
			line := statedLine(c.opts, string(c.fact.Field), c.value)

			// The deterministic template states the fact exactly so, and it verifies: the
			// confirmation of a cancellation is not an internal error.
			text, err := Render(d, nil, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(text, line) {
				t.Fatalf("the template does not state %q:\n%s", line, text)
			}
			out, err := Degraded(d, nil, c.opts)
			if err != nil {
				t.Fatalf("the template for a confirmation request was refused: %v\n%s", err, text)
			}
			if out.Source != SourceTemplate || out.Text != text {
				t.Fatalf("degraded = %+v", out)
			}
			// Nothing about the word makes the template a completion claim.
			if res := mustVerify(t, text, d, nil, c.opts); !res.OK || res.Has(ViolationCompletionWithoutReceipt) {
				t.Fatalf("the template: %v\n%s", res.Violations, text)
			}

			// A model that states the fact the same way, in its own words around it, passes.
			model := "Please check this before I go on.\n" + statedLine(c.opts, "doctor", "Dr. Rossi") + "\n" + line + "\nShall I go ahead?"
			if res := mustVerify(t, model, d, nil, c.opts); !res.OK {
				t.Fatalf("the model's text %q: %v", model, res.Violations)
			}
			got, err := Realise(model, d, nil, c.opts)
			if err != nil || got.Source != SourceModel || got.Reason != ReasonVerified {
				t.Fatalf("realise = %+v, %v", got, err)
			}
			// The label is part of the exemption for a single word: the same word under a label that
			// is not its fact's is a claim. (A host string of several words may be quoted anywhere, as
			// every authored string always could.)
			other := strings.Replace(model, label+":", "Result:", 1)
			if c.opts.Language == LangEN && !strings.Contains(c.value, " ") {
				if res := mustVerify(t, other, d, nil, c.opts); !res.Has(ViolationCompletionWithoutReceipt) {
					t.Fatalf("%q: a value under a label that is not its fact's passed: %v", other, kinds(res))
				}
			}
		})
	}

	// The same facts in the other acts the template states facts in, and for an outcome that did
	// not complete, where the facts are the details of the request: every template is produced.
	t.Run("every act and every receipt", func(t *testing.T) {
		facts := []actions.Fact{textFact("appointment_status", "cancelled"), textFact("payment_state", "paid")}
		for _, opts := range []Options{en, it} {
			for _, act := range []conversation.DialogueDirectiveAct{
				conversation.DialogueDirectiveActRequestConfirmation, conversation.DialogueDirectiveActRequestStepUp,
				conversation.DialogueDirectiveActPresentResult,
			} {
				d := directive(act, facts, "appointment_status", "payment_state")
				text, err := Render(d, nil, opts)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(text, statedLine(opts, "appointment_status", "cancelled")) {
					t.Errorf("%s/%s: the template does not state the status:\n%s", opts.Language, act, text)
				}
				if _, err := Degraded(d, nil, opts); err != nil {
					t.Errorf("%s/%s: %v", opts.Language, act, err)
				}
			}
			d := outcomeDirective(facts, "appointment_status", "payment_state")
			for _, status := range []actions.ActionReceiptStatus{
				actions.ActionReceiptStatusCommitted, actions.ActionReceiptStatusRejected, actions.ActionReceiptStatusQueued,
				actions.ActionReceiptStatusPartial, actions.ActionReceiptStatusUncertain,
			} {
				r := receipt(status, facts...)
				if _, err := Degraded(d, r, opts); err != nil {
					t.Errorf("%s/%s: %v", opts.Language, status, err)
				}
			}
			if _, err := Degraded(d, nil, opts); err != nil {
				t.Errorf("%s/no receipt: %v", opts.Language, err)
			}
		}
	})
}

// SEC-011: an identifier whose value is a completion word (a status code "cancelled") is the same
// word as a text value: data when stated as its fact, a claim anywhere else. The identifier being
// allowed must not hide the word from the completion check.
func TestCompletionWordIdentifierIsStillAClaim(t *testing.T) {
	doctor, status := textFact("doctor", "Dr. Rossi"), idFact("status", "cancelled")
	d := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{doctor, status})
	text := "Here is what I found:\n" + statedLine(en, "doctor", "Dr. Rossi") + "\n" + statedLine(en, "status", "cancelled")
	if res := mustVerify(t, text, d, nil, en); !res.OK {
		t.Fatalf("positive control %q must verify, got %v", text, res.Violations)
	}
	for _, text := range []string{
		"Your appointment with Dr. Rossi is cancelled.",
		"Here is what I found:\n" + statedLine(en, "doctor", "cancelled"),
	} {
		if res := mustVerify(t, text, d, nil, en); !res.Has(ViolationCompletionWithoutReceipt) {
			t.Errorf("%q: violations = %v, want completion_without_receipt", text, kinds(res))
		}
	}
	// Every template that states the identifier still verifies.
	for _, opts := range []Options{en, it} {
		if _, err := Degraded(d, nil, opts); err != nil {
			t.Errorf("%s: %v", opts.Language, err)
		}
		out := outcomeDirective([]actions.Fact{doctor, status}, "doctor", "status")
		for _, st := range []actions.ActionReceiptStatus{
			actions.ActionReceiptStatusCommitted, actions.ActionReceiptStatusRejected, actions.ActionReceiptStatusQueued,
			actions.ActionReceiptStatusPartial, actions.ActionReceiptStatusUncertain,
		} {
			if _, err := Degraded(out, receipt(st, doctor, status), opts); err != nil {
				t.Errorf("%s/%s: %v", opts.Language, st, err)
			}
		}
	}
}

// SEC-011, SEC-047: the rule above is about a stated host value and nothing else. Whatever a
// model writes that is not that, a completion word still needs a committed receipt.
func TestCompletionWordInModelTextStillNeedsAReceipt(t *testing.T) {
	facts := []actions.Fact{
		textFact("doctor", "Dr. Rossi"),
		textFact("appointment_status", "cancelled"),
		textFact("payment_state", "paid"),
	}
	d := confirmationOf(facts...)
	const control = "Please check these details.\n- Doctor: Dr. Rossi\n- Appointment status: cancelled\n- Payment state: paid\nShall I go ahead?"
	const doctor, status, payment = "- Doctor: Dr. Rossi\n", "- Appointment status: cancelled\n", "- Payment state: paid\n"

	if res := mustVerify(t, control, d, nil, en); !res.OK {
		t.Fatalf("positive control %q must verify, got %v", control, res.Violations)
	}

	// Each text is the control with one change. A verifier that read the word by position alone,
	// by the word alone, or not at all would let it through.
	for _, m := range []struct {
		name string
		text string
		only bool // the claim is the only thing wrong with the text
	}{
		{"free text beside the stated value", control + "\nYour appointment has been cancelled.", true},
		{"free text before the stated value", "Your appointment has been cancelled.\n" + doctor + status + payment, true},
		{"a model's own sentence with the host's word", "Please check these details.\n" + doctor + "The appointment is cancelled.\n" + payment, false},
		{"the word as a paraphrase of the status", "Please check these details.\n" + doctor + "- Appointment status: it is cancelled\n" + payment, false},
		{"the word after the label, in a longer value", "Please check these details.\n" + doctor + "- Appointment status: cancelled by the clinic\n" + payment, false},
		{"the word joined to others by hyphens", "Please check these details.\n" + doctor + "- Appointment status: cancelled-and-confirmed\n" + payment, false},
		{"a second completion word after the value", "Please check these details.\n" + doctor + "- Appointment status: cancelled, and the booking is confirmed\n" + payment, false},
		{"the same line with a word the host did not give", "Please check these details.\n" + doctor + "- Appointment status: booked\n" + payment, false},
		{"the host's value under another fact's label", "Please check these details.\n" + doctor + "- Appointment status: paid\n- Payment state: cancelled\n", false},
		{"the host's value under a label no fact has", "Please check these details.\n" + doctor + status + payment + "- Outcome: cancelled\n", true},
		{"the host's value under the label of another fact", "Please check these details.\n- Doctor: cancelled\n" + status + payment, false},
		{"the host's value with no label at all", "Cancelled.\n" + doctor + status + payment, true},
		{"a completion phrase beside the stated value", control + "\nAll set.", true},
		{"an Italian claim beside the stated value", control + "\nL'appuntamento è stato annullato.", true},
	} {
		t.Run(m.name, func(t *testing.T) {
			res := mustVerify(t, m.text, d, nil, en)
			if res.OK || !res.Has(ViolationCompletionWithoutReceipt) {
				t.Fatalf("%q: violations = %v, want completion_without_receipt", m.text, kinds(res))
			}
			if m.only && !slices.Equal(kinds(res), []string{string(ViolationCompletionWithoutReceipt)}) {
				t.Fatalf("%q: violations = %v, want only completion_without_receipt: the text would pass without the completion rule", m.text, kinds(res))
			}
			// A refused text is replaced by the template, which is always shown.
			out, err := Realise(m.text, d, nil, en)
			if err != nil || out.Source != SourceTemplate || out.Reason != ReasonVerificationFailed {
				t.Fatalf("realise = %+v, %v", out, err)
			}
		})
	}

	t.Run("a fact the directive does not carry", func(t *testing.T) {
		noStatus := confirmationOf(textFact("doctor", "Dr. Rossi"))
		text := "Please check these details.\n- Doctor: Dr. Rossi\n- Appointment status: cancelled\nShall I go ahead?"
		if res := mustVerify(t, text, noStatus, nil, en); !res.Has(ViolationCompletionWithoutReceipt) {
			t.Fatalf("a status the host did not give passed: %v", kinds(res))
		}
	})

	t.Run("a fact the template does not state", func(t *testing.T) {
		// The act is one that states only what must_state names, and must_state names no fact.
		ask := directive(conversation.DialogueDirectiveActAskMissing, facts)
		ask.MissingFields = []common.FieldName{"slot"}
		text := "What time would suit you?\n- Appointment status: cancelled"
		if res := mustVerify(t, text, ask, nil, en); !res.Has(ViolationCompletionWithoutReceipt) {
			t.Fatalf("a fact that is not stated by the template passed as stated data: %v", kinds(res))
		}
	})

	t.Run("a value a receipt carries, not the directive", func(t *testing.T) {
		out := outcomeDirective([]actions.Fact{textFact("doctor", "Dr. Rossi")}, "doctor")
		r := receipt(actions.ActionReceiptStatusRejected, textFact("doctor", "Dr. Rossi"), textFact("appointment_status", "booked"))
		text := "The request was rejected.\n- Doctor: Dr. Rossi\n- Appointment status: booked"
		if res := mustVerify(t, text, out, r, en); !res.Has(ViolationCompletionWithoutReceipt) {
			t.Fatalf("a receipt fact the template does not state passed as stated data: %v", kinds(res))
		}
	})

	t.Run("a phrase that is only partly a value", func(t *testing.T) {
		// "paid all" is the host's value, and "all set" the claim made across its end.
		d := confirmationOf(textFact("note", "paid all"))
		if res := mustVerify(t, "- Note: paid all", d, nil, en); !res.OK {
			t.Fatalf("control: %v", res.Violations)
		}
		if res := mustVerify(t, "- Note: paid all\nset", d, nil, en); !res.Has(ViolationCompletionWithoutReceipt) {
			t.Fatalf("a completion phrase across the end of a value passed: %v", kinds(res))
		}
		if res := mustVerify(t, "All set.\n- Note: paid all", d, nil, en); !res.Has(ViolationCompletionWithoutReceipt) {
			t.Fatalf("a completion phrase before a stated value passed: %v", kinds(res))
		}
	})

	t.Run("a value that is itself a phrase", func(t *testing.T) {
		d := confirmationOf(textFact("setup_state", "all set"))
		if res := mustVerify(t, "- Setup state: all set", d, nil, en); !res.OK {
			t.Fatalf("control: %v", res.Violations)
		}
		for _, text := range []string{
			"- Setup state: all set\nAll set!",
			"You are all set.\n- Setup state: all set",
			"- Setup state: x all set",
		} {
			if res := mustVerify(t, text, d, nil, en); !res.Has(ViolationCompletionWithoutReceipt) {
				t.Errorf("%q: the phrase said outside the stated value passed: %v", text, kinds(res))
			}
		}
	})

	t.Run("a committed receipt still allows the claim", func(t *testing.T) {
		out := outcomeDirective(facts, "doctor", "appointment_status", "payment_state")
		r := receipt(actions.ActionReceiptStatusCommitted, facts...)
		if res := mustVerify(t, control+"\nYour appointment has been cancelled.", out, r, en); !res.OK {
			t.Fatalf("the claim against a committed receipt: %v", res.Violations)
		}
	})

	t.Run("in Italian", func(t *testing.T) {
		opts := Options{Language: LangIT, FieldLabels: map[string]string{
			"appointment_status": "Stato dell'appuntamento", "doctor": "Medico",
		}}
		d := confirmationOf(textFact("doctor", "Dr. Rossi"), textFact("appointment_status", "annullato"))
		const ctl = "Controlla questi dati.\n- Medico: Dr. Rossi\n- Stato dell'appuntamento: annullato\nProcedo?"
		if res := mustVerify(t, ctl, d, nil, opts); !res.OK {
			t.Fatalf("positive control %q must verify, got %v", ctl, res.Violations)
		}
		for _, text := range []string{
			ctl + "\nL'appuntamento è stato annullato.",
			"Appuntamento annullato.\n- Medico: Dr. Rossi\n- Stato dell'appuntamento: annullato",
			"Controlla questi dati.\n- Medico: Dr. Rossi\n- Stato dell'appuntamento: annullato con successo",
			"Controlla questi dati.\n- Medico: Dr. Rossi\n- Stato: annullato",
		} {
			if res := mustVerify(t, text, d, nil, opts); !res.Has(ViolationCompletionWithoutReceipt) {
				t.Errorf("%q: violations = %v, want completion_without_receipt", text, kinds(res))
			}
		}
	})
}
