package verify

import (
	"errors"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// bookingFacts are the facts of the appointment the tests talk about. 14 October 2026 is a
// Wednesday.
func bookingFacts() []actions.Fact {
	return []actions.Fact{
		textFact("doctor", "Dr. Rossi"),
		dateFact("slot_date", "2026-10-14"),
		timeFact("slot_start", "10:30"),
		moneyFact("fee", "50.00", "EUR"),
	}
}

func confirmDirective() *conversation.DialogueDirective {
	d := directive(conversation.DialogueDirectiveActRequestConfirmation, bookingFacts(),
		"doctor", "slot_date", "slot_start", "fee")
	prep := actions.PreparationID(prepID)
	d.PreparationID = &prep
	d.Confirmation = actions.ConfirmationRequirement{Mode: actions.ConfirmationRequirementModeExplicit}
	d.PermittedNextActions = []actions.PermittedNextAction{
		{Action: actions.PermittedNextActionActionConfirm},
		{Action: actions.PermittedNextActionActionReject},
		{Action: actions.PermittedNextActionActionModify},
	}
	return d
}

const goodConfirmEN = "Please confirm: Dr. Rossi on Wednesday 14 October 2026 at 10:30, fee 50.00 EUR. Shall I go ahead?"

// check is the common shape of the five failure tests: the text must verify as the positive
// control, and each mutation must fail with the named violation. A verifier that rejects
// everything would pass the mutations, so the control comes first.
type mutation struct {
	name string
	text string
	want []ViolationKind // each of these must be present
}

func runMutations(t *testing.T, d *conversation.DialogueDirective, r *actions.ActionReceipt, opts Options, control string, muts []mutation) {
	t.Helper()
	if res := mustVerify(t, control, d, r, opts); !res.OK {
		t.Fatalf("positive control %q must verify, got %v", control, res.Violations)
	}
	for _, m := range muts {
		t.Run(m.name, func(t *testing.T) {
			res := mustVerify(t, m.text, d, r, opts)
			if res.OK {
				t.Fatalf("%q verified, want violations %v", m.text, m.want)
			}
			for _, w := range m.want {
				if !res.Has(w) {
					t.Errorf("%q: violations %v lack %s", m.text, kinds(res), w)
				}
			}
		})
	}
}

// SEC-011: a required fact left out of the model's text fails verification, so the template
// replaces it.
func TestMissingRequiredFactFails(t *testing.T) {
	d := confirmDirective()
	runMutations(t, d, nil, en, goodConfirmEN, []mutation{
		{"doctor omitted", "Please confirm: Wednesday 14 October 2026 at 10:30, fee 50.00 EUR.", []ViolationKind{ViolationMissingFact}},
		{"date omitted", "Please confirm: Dr. Rossi at 10:30, fee 50.00 EUR.", []ViolationKind{ViolationMissingFact}},
		{"time omitted", "Please confirm: Dr. Rossi on 14 October 2026, fee 50.00 EUR.", []ViolationKind{ViolationMissingFact}},
		{"amount omitted", "Please confirm: Dr. Rossi on 14 October 2026 at 10:30.", []ViolationKind{ViolationMissingFact}},
		{"amount without its currency", "Please confirm: Dr. Rossi on 14 October 2026 at 10:30, fee 50.", []ViolationKind{ViolationMissingFact}},
		{"date without a day", "Please confirm: Dr. Rossi in October at 10:30, fee 50.00 EUR.", []ViolationKind{ViolationMissingFact}},
		{"everything omitted", "Please confirm the appointment.", []ViolationKind{ViolationMissingFact}},
	})

	// The violation names the field that is missing and only that field.
	res := mustVerify(t, "Please confirm: Dr. Rossi at 10:30, fee 50.00 EUR.", d, nil, en)
	if len(res.Violations) != 1 || res.Violations[0].Kind != ViolationMissingFact || res.Violations[0].Field != "slot_date" {
		t.Errorf("violations = %v, want exactly missing_fact slot_date", res.Violations)
	}

	// A fact that is not in must_state may be left out.
	d2 := confirmDirective()
	d2.MustState = []common.FieldName{"doctor"}
	if res := mustVerify(t, "Please confirm the visit with Dr. Rossi.", d2, nil, en); !res.OK {
		t.Errorf("a fact outside must_state was required: %v", res.Violations)
	}

	// The display form satisfies a required fact.
	d3 := directive(conversation.DialogueDirectiveActPresentResult,
		[]actions.Fact{withDisplay(dateFact("slot_date", "2026-10-14"), "mercoledì prossimo")}, "slot_date")
	if res := mustVerify(t, "Il visita è mercoledì prossimo.", d3, nil, it); !res.OK {
		t.Errorf("the display form was not accepted: %v", res.Violations)
	}

	// The wrong value does not satisfy it: the fact is missing and the stated value is invented.
	res = mustVerify(t, "Please confirm: Dr. Rossi on 14 October 2026 at 10:45, fee 50.00 EUR.", d, nil, en)
	if !res.Has(ViolationMissingFact) || !res.Has(ViolationUnknownTime) {
		t.Errorf("wrong time: violations = %v, want missing_fact and unknown_time", kinds(res))
	}

	// must_state may only name facts that exist: otherwise the directive is unusable.
	bad := confirmDirective()
	bad.MustState = append(bad.MustState, "nonexistent")
	if _, err := Verify(goodConfirmEN, bad, nil, en); !errors.Is(err, ErrInvalidDirective) {
		t.Errorf("must_state naming no fact: err = %v, want ErrInvalidDirective", err)
	}
}

// SEC-011: a number the directive does not carry fails verification.
func TestInventedNumberFails(t *testing.T) {
	d := directive(conversation.DialogueDirectiveActPresentResult,
		[]actions.Fact{intFact("cancellation_notice_hours", 24), dateFact("slot_date", "2026-10-14")},
		"cancellation_notice_hours")
	control := "You can cancel free of charge up to 24 hours before the visit on 14 October 2026."
	runMutations(t, d, nil, en, control, []mutation{
		{"other number", "You can cancel free of charge up to 48 hours before the visit on 14 October 2026.", []ViolationKind{ViolationUnknownNumber, ViolationMissingFact}},
		{"extra number", "You can cancel up to 24 hours before, and you have 3 appointments.", []ViolationKind{ViolationUnknownNumber}},
		{"number in words", "You can cancel up to 24 hours before; there are three open slots.", []ViolationKind{ViolationUnknownNumber}},
		{"hyphenated number in words", "You can cancel up to twenty-four hours before, or forty-eight hours for groups.", []ViolationKind{ViolationUnknownNumber}},
		{"italian number in words", "You can cancel up to 24 hours before, in quarantotto slots.", []ViolationKind{ViolationUnknownNumber}},
		{"decimal", "You can cancel up to 24 hours before, a 2.5 hour wait.", []ViolationKind{ViolationUnknownNumber}},
		{"percent", "You can cancel up to 24 hours before for a 20% fee.", []ViolationKind{ViolationUnknownNumber}},
		{"fullwidth digits", "You can cancel up to 24 hours before; or \uff14\uff18 hours for groups.", []ViolationKind{ViolationUnknownNumber}},
		{"digits split by zero-width characters", "You can cancel up to 24 hours before; or 4\u200b8 hours for groups.", []ViolationKind{ViolationUnknownNumber}},
		{"arabic-indic digits", "You can cancel up to 24 hours before; or \u0664\u0668 hours for groups.", []ViolationKind{ViolationUnknownNumber}},
		{"numeral the normaliser cannot read", "You can cancel up to 24 hours before, see clause \u216b.", []ViolationKind{ViolationUnknownNumber}},
		{"negative of an allowed number", "You can cancel up to -24 hours before.", []ViolationKind{ViolationUnknownNumber}},
		{"year that is not the visit's", "You can cancel up to 24 hours before the visit on 14 October 2026, as of 2027.", []ViolationKind{ViolationUnknownNumber}},
		{"number inside a phone-like token", "You can cancel up to 24 hours before; call 555-1234.", []ViolationKind{ViolationUnknownNumber}},
	})

	// Things that are not stated numbers are left alone: list numerals, one/un as pronouns and
	// articles, the year of an allowed date.
	for _, text := range []string{
		"1. You can cancel up to 24 hours before.\n2. The visit is on 14 October 2026.",
		"You can cancel one of them up to 24 hours before.",
		"You can cancel up to 24 hours before, in 2026.",
	} {
		if res := mustVerify(t, text, d, nil, en); !res.OK {
			t.Errorf("%q should verify: %v", text, res.Violations)
		}
	}
	itD := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{intFact("ore", 24)}, "ore")
	if res := mustVerify(t, "Puoi annullare fino a ventiquattro ore prima. Un momento, sei sicuro?", itD, nil, it); !res.OK {
		t.Errorf("italian number words and articles: %v", res.Violations)
	}
	if res := mustVerify(t, "Puoi annullare fino a 24 ore prima, per due appuntamenti.", itD, nil, it); !res.Has(ViolationUnknownNumber) {
		t.Errorf("an invented italian 'due' was not caught: %v", res.Violations)
	}
}

// SEC-011, SEC-002: an identifier the directive does not carry fails verification, including one
// the model echoes from elsewhere (a session ID, a URL, an e-mail address).
func TestInventedIdentifierFails(t *testing.T) {
	d := outcomeDirective([]actions.Fact{idFact("appointment_id", "apt_20991"), timeFact("slot_start", "10:30")},
		"appointment_id", "slot_start")
	r := receipt(actions.ActionReceiptStatusCommitted, idFact("appointment_id", "apt_20991"))
	control := "Done: your appointment apt_20991 is at 10:30."
	runMutations(t, d, r, en, control, []mutation{
		{"invented id", "Done: your appointment apt_20992 is at 10:30, reference apt_20991.", []ViolationKind{ViolationUnknownIdentifier}},
		{"stated id replaced", "Done: your appointment apt_99999 is at 10:30.", []ViolationKind{ViolationUnknownIdentifier, ViolationMissingFact}},
		{"different case", "Done: your appointment APT_20991 is at 10:30.", []ViolationKind{ViolationUnknownIdentifier, ViolationMissingFact}},
		{"echoed session id", "Done: apt_20991 at 10:30. Session ses_01J0000000000000000000000A.", []ViolationKind{ViolationUnknownIdentifier}},
		{"echoed tenant-style id", "Done: apt_20991 at 10:30 (tenant acme-7f3a).", []ViolationKind{ViolationUnknownIdentifier}},
		{"credential-looking token", "Done: apt_20991 at 10:30. Key sk-live-9a8b7c6d5e4f.", []ViolationKind{ViolationUnknownIdentifier}},
		{"letters-only random token", "Done: apt_20991 at 10:30. Reference: UbgFXUxjcxDQdzzlvsWgCw.", []ViolationKind{ViolationUnknownIdentifier}},
		{"url", "Done: apt_20991 at 10:30. See https://evil.example/x?apt_20991.", []ViolationKind{ViolationUnknownIdentifier}},
		{"email", "Done: apt_20991 at 10:30. Write to ceo@example.com.", []ViolationKind{ViolationUnknownIdentifier}},
		{"id hidden by zero-width space", "Done: apt_2\u200b0992 at 10:30, apt_20991.", []ViolationKind{ViolationUnknownIdentifier}},
		{"id with a suffix", "Done: apt_20991b at 10:30.", []ViolationKind{ViolationUnknownIdentifier, ViolationMissingFact}},
	})

	// Identifiers the directive and receipt do carry may be stated: the turn and receipt IDs
	// in the directive, the audit reference in the receipt, the capability.
	for _, id := range []string{turnID, receiptID, "aud_01J0000000000000000000000A", "appointments.book"} {
		if res := mustVerify(t, "Done: apt_20991 at 10:30 ("+id+").", d, r, en); !res.OK {
			t.Errorf("identifier %s carried by the directive/receipt was rejected: %v", id, res.Violations)
		}
	}

	// An identifier written with a zero-width character is read as the same identifier, but the
	// text is not shown: what a reader sees is not what was verified.
	if res := mustVerify(t, "Done: apt_2\u200b0991 at 10:30.", d, r, en); res.OK || !res.Has(ViolationInvisibleCharacters) {
		t.Errorf("the same identifier with an invisible character: %v", res.Violations)
	}

	// The violation does not carry the offending text: results are logged and the model may
	// have echoed a secret.
	res := mustVerify(t, "Done: apt_20991 at 10:30. Key sk-live-9a8b7c6d5e4f.", d, r, en)
	for _, v := range res.Violations {
		if s := v.String(); strings.Contains(s, "sk-live") || strings.Contains(v.Got, "sk-live") || v.Fingerprint == "" {
			t.Errorf("violation %q leaks the offending text or has no fingerprint", s)
		}
	}
}

// SEC-011, SEC-047: no completion claim without a committed receipt.
func TestCompletionClaimWithoutReceiptFails(t *testing.T) {
	facts := []actions.Fact{dateFact("slot_date", "2026-10-14"), timeFact("slot_start", "10:30")}
	d := outcomeDirective(facts, "slot_date", "slot_start")
	committed := receipt(actions.ActionReceiptStatusCommitted, facts...)
	claim := "Your appointment is booked for 14 October 2026 at 10:30."

	// Positive control: the same text verifies against a committed receipt.
	if res := mustVerify(t, claim, d, committed, en); !res.OK {
		t.Fatalf("a committed receipt must allow the claim: %v", res.Violations)
	}
	if res := mustVerify(t, "Il tuo appuntamento è stato prenotato il 14 ottobre 2026 alle 10:30.", d, committed, it); !res.OK {
		t.Fatalf("italian claim with a committed receipt: %v", res.Violations)
	}

	for _, status := range []actions.ActionReceiptStatus{
		actions.ActionReceiptStatusRejected, actions.ActionReceiptStatusQueued,
		actions.ActionReceiptStatusPartial, actions.ActionReceiptStatusUncertain,
	} {
		t.Run("status "+string(status), func(t *testing.T) {
			r := receipt(status, facts...)
			if res := mustVerify(t, claim, d, r, en); !res.Has(ViolationCompletionWithoutReceipt) {
				t.Errorf("claim against a %s receipt: violations = %v", status, kinds(res))
			}
		})
	}
	t.Run("no receipt at all", func(t *testing.T) {
		res := mustVerify(t, claim, d, nil, en)
		if !res.Has(ViolationCompletionWithoutReceipt) || !res.Has(ViolationReceiptMissing) {
			t.Errorf("violations = %v, want completion_without_receipt and receipt_missing", kinds(res))
		}
	})

	// The directive must say it is about this receipt.
	other := receipt(actions.ActionReceiptStatusCommitted, facts...)
	other.ReceiptID = "rcpt_01J0000000000000000000000Z"
	if _, err := Verify(claim, d, other, en); !errors.Is(err, ErrReceiptMismatch) {
		t.Errorf("a receipt the directive does not name: err = %v, want ErrReceiptMismatch", err)
	}
	noID := outcomeDirective(facts, "slot_date", "slot_start")
	noID.ReceiptID = nil
	if _, err := Verify(claim, noID, committed, en); !errors.Is(err, ErrReceiptMismatch) {
		t.Errorf("a receipt for a directive that names none: err = %v, want ErrReceiptMismatch", err)
	}

	// Other acts: a claim with no receipt is invented, in either language, in many words.
	confirm := confirmDirective()
	for _, text := range []string{
		goodConfirmEN + " Your appointment is booked.",
		"Done! Dr. Rossi, 14 October 2026, 10:30, 50.00 EUR.",
		"All set: Dr. Rossi on 14 October 2026 at 10:30, 50.00 EUR.",
		"I have successfully scheduled it: Dr. Rossi, 14 October 2026, 10:30, 50.00 EUR.",
		"Your payment was refunded: Dr. Rossi, 14 October 2026, 10:30, 50.00 EUR.",
		"Appuntamento prenotato: Dr. Rossi, 14 ottobre 2026, 10:30, 50,00 EUR.",
		"Tutto fatto: Dr. Rossi, 14 ottobre 2026, 10:30, 50,00 EUR.",
		"Ho annullato l'appuntamento con Dr. Rossi il 14 ottobre 2026 alle 10:30, 50,00 EUR.",
		"Prenotazione confermata con successo: Dr. Rossi, 14 ottobre 2026, 10:30, 50,00 EUR.",
	} {
		lang := en
		if strings.Contains(text, "ottobre") {
			lang = it
		}
		if res := mustVerify(t, text, confirm, nil, lang); !res.Has(ViolationCompletionWithoutReceipt) {
			t.Errorf("%q: violations = %v, want completion_without_receipt", text, kinds(res))
		}
	}

	// Negated mentions and requests are not claims.
	for _, text := range []string{
		"Please confirm and I will book it: Dr. Rossi on 14 October 2026 at 10:30, 50.00 EUR.",
		"Nothing was booked yet. Dr. Rossi, 14 October 2026, 10:30, 50.00 EUR. Shall I go ahead?",
		"It has not been booked: Dr. Rossi, 14 October 2026, 10:30, 50.00 EUR.",
		"No changes were saved. Dr. Rossi, 14 October 2026, 10:30, 50.00 EUR.",
	} {
		if res := mustVerify(t, text, confirm, nil, en); res.Has(ViolationCompletionWithoutReceipt) {
			t.Errorf("%q is not a completion claim: %v", text, res.Violations)
		}
	}
	if res := mustVerify(t, "Non è stato prenotato: Dr. Rossi, 14 ottobre 2026, 10:30, 50,00 EUR. Procedo?", confirm, nil, it); res.Has(ViolationCompletionWithoutReceipt) {
		t.Errorf("a negated italian claim was rejected: %v", res.Violations)
	}
}

// SEC-011: a citation to a source the directive does not list fails verification.
func TestCitationNotInEvidenceFails(t *testing.T) {
	d := directive(conversation.DialogueDirectiveActPresentResult,
		[]actions.Fact{intFact("cancellation_notice_hours", 24)}, "cancellation_notice_hours")
	policy := source(srcA, "Clinic cancellation policy", "Appointments can be cancelled free of charge up to 24 hours before the start.")
	policy.Locator = &conversation.CitationLocator{Page: ptr(int64(3))}
	removed := source(srcB, "Old tariff sheet", "Fees were different.")
	removed.Availability = conversation.CitationAvailabilityRemoved
	d.Sources = []conversation.Citation{policy, removed}

	control := "Cancellation is free up to 24 hours before the visit [1]."
	runMutations(t, d, nil, en, control, []mutation{
		{"index past the end", "Cancellation is free up to 24 hours before the visit [3].", []ViolationKind{ViolationCitationNotInEvidence}},
		{"index zero", "Cancellation is free up to 24 hours before the visit [0].", []ViolationKind{ViolationCitationNotInEvidence}},
		{"removed source by index", "Cancellation is free up to 24 hours before the visit [2].", []ViolationKind{ViolationCitationNotInEvidence}},
		{"removed source by id", "Cancellation is free up to 24 hours before the visit [" + srcB + "].", []ViolationKind{ViolationCitationNotInEvidence}},
		{"unlisted source id", "Cancellation is free up to 24 hours before the visit [src_01J0000000000000000000000Z].", []ViolationKind{ViolationCitationNotInEvidence}},
		{"unlisted bare id", "Cancellation is free up to 24 hours before the visit, see src_01J0000000000000000000000Z.", []ViolationKind{ViolationCitationNotInEvidence}},
		{"unlisted name", "Cancellation is free up to 24 hours before the visit (source: Employee handbook).", []ViolationKind{ViolationCitationNotInEvidence}},
		{"unlisted name on a line", "Cancellation is free up to 24 hours before the visit.\nSource: Employee handbook", []ViolationKind{ViolationCitationNotInEvidence}},
		{"listed and unlisted names", "Cancellation is free up to 24 hours before the visit [source: Clinic cancellation policy; Employee handbook].", []ViolationKind{ViolationCitationNotInEvidence}},
		{"italian marker", "La cancellazione è gratuita fino a 24 ore prima (fonte: Manuale interno).", []ViolationKind{ViolationCitationNotInEvidence}},
	})

	// Citations to listed sources verify, in every marker form, with a locator the source carries.
	for _, text := range []string{
		"Cancellation is free up to 24 hours before the visit [1].",
		"Cancellation is free up to 24 hours before the visit [" + srcA + "].",
		"Cancellation is free up to 24 hours before the visit, see " + srcA + ".",
		"Cancellation is free up to 24 hours before the visit (source: Clinic cancellation policy).",
		"Cancellation is free up to 24 hours before the visit [source: Clinic cancellation policy, page 3].",
		"Cancellation is free up to 24 hours before the visit.\nSource: Clinic cancellation policy",
		"Cancellation is free up to 24 hours before the visit (fonte: clinic cancellation policy).",
	} {
		if res := mustVerify(t, text, d, nil, en); !res.OK {
			t.Errorf("%q should verify: %v", text, res.Violations)
		}
	}
	// ...but a page the source does not carry is an invented number.
	if res := mustVerify(t, "Cancellation is free up to 24 hours before the visit [source: Clinic cancellation policy, page 9].", d, nil, en); !res.Has(ViolationUnknownNumber) {
		t.Errorf("an invented page number was not caught: %v", res.Violations)
	}

	// No sources at all: any citation is absent from the evidence.
	none := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{intFact("h", 24)}, "h")
	if res := mustVerify(t, "Free up to 24 hours before [1].", none, nil, en); !res.Has(ViolationCitationNotInEvidence) {
		t.Errorf("citation with no sources: %v", res.Violations)
	}
}

func ptr[T any](v T) *T { return &v }

// Weekdays are claims too: a weekday that does not belong to the stated date is wrong.
func TestWeekdayMustMatchTheDate(t *testing.T) {
	d := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{dateFact("slot_date", "2026-10-14")}, "slot_date")
	runMutations(t, d, nil, en, "The visit is on Wednesday 14 October 2026.", []mutation{
		{"wrong weekday", "The visit is on Tuesday 14 October 2026.", []ViolationKind{ViolationWeekdayMismatch}},
		{"wrong weekday without a year", "The visit is on Thursday, 14 October.", []ViolationKind{ViolationWeekdayMismatch}},
		{"wrong weekday after the article", "The visit is on Tuesday the 14th of October.", []ViolationKind{ViolationWeekdayMismatch}},
		{"weekday of no allowed date", "The visit is on Friday, 14 October 2026 or Monday.", []ViolationKind{ViolationWeekdayMismatch}},
		{"wrong abbreviated weekday", "The visit is on Mon 14 October 2026.", []ViolationKind{ViolationWeekdayMismatch}},
		{"wrong abbreviated weekday with a dot", "The visit is on Fri., 14 Oct 2026.", []ViolationKind{ViolationWeekdayMismatch}},
	})
	if res := mustVerify(t, "La visita è mercoledì 14 ottobre.", d, nil, it); !res.OK {
		t.Errorf("italian weekday: %v", res.Violations)
	}
	if res := mustVerify(t, "La visita è martedì 14 ottobre.", d, nil, it); !res.Has(ViolationWeekdayMismatch) {
		t.Errorf("wrong italian weekday: %v", res.Violations)
	}
	if res := mustVerify(t, "The visit is on Wed 14 October 2026. La visita è mer 14 ottobre 2026.", d, nil, it); !res.OK {
		t.Errorf("abbreviated weekdays: %v", res.Violations)
	}
	if res := mustVerify(t, "La visita è lun 14 ottobre 2026.", d, nil, it); !res.Has(ViolationWeekdayMismatch) {
		t.Errorf("wrong abbreviated italian weekday: %v", res.Violations)
	}
	// A host display form that pairs a weekday with the date is the host's wording and stands.
	hd := directive(conversation.DialogueDirectiveActPresentResult,
		[]actions.Fact{withDisplay(dateFact("slot_date", "2026-10-14"), "Tuesday 14 October 2026")}, "slot_date")
	if res := mustVerify(t, "The visit is on Tuesday 14 October 2026.", hd, nil, en); !res.OK {
		t.Errorf("the host's own display form was rejected: %v", res.Violations)
	}
}

// Money: amount and currency are checked together.
func TestMoneyAmountAndCurrency(t *testing.T) {
	d := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{moneyFact("fee", "12.50", "EUR")}, "fee")
	runMutations(t, d, nil, en, "The fee is €12.50.", []mutation{
		{"wrong currency symbol", "The fee is $12.50.", []ViolationKind{ViolationCurrencyMismatch, ViolationMissingFact}},
		{"wrong currency code", "The fee is 12.50 GBP.", []ViolationKind{ViolationCurrencyMismatch}},
		{"wrong amount", "The fee is €13.50.", []ViolationKind{ViolationUnknownAmount, ViolationMissingFact}},
		{"extra amount", "The fee is €12.50, plus €2 booking.", []ViolationKind{ViolationUnknownAmount}},
		{"amount in words and the wrong currency", "The fee is twelve dollars.", []ViolationKind{ViolationUnknownAmount}},
	})
	for _, text := range []string{
		"The fee is 12.50 EUR.", "The fee is EUR 12.50.", "The fee is 12.50 euros.", "The fee is 12.5 euro.",
		"The fee is €12.50, which is 12.50 euro.",
	} {
		if res := mustVerify(t, text, d, nil, en); !res.OK {
			t.Errorf("%q should verify: %v", text, res.Violations)
		}
	}
	if res := mustVerify(t, "Il costo è di 12,50 €.", d, nil, it); !res.OK {
		t.Errorf("italian amount: %v", res.Violations)
	}
	if res := mustVerify(t, "Il costo è di 12,50\u00a0€.", d, nil, it); !res.OK {
		t.Errorf("italian amount with a no-break space: %v", res.Violations)
	}
	if res := mustVerify(t, "Il costo è di 12.50 €.", d, nil, it); !res.OK {
		t.Errorf("lenient italian decimal point: %v", res.Violations)
	}
}

// A datetime is compared in the zone the host displays it in, never in UTC wording.
func TestDatetimeIsComparedInItsZone(t *testing.T) {
	// 08:30 UTC is 10:30 in Rome in October (CEST, UTC+2).
	d := directive(conversation.DialogueDirectiveActPresentResult,
		[]actions.Fact{datetimeFact("slot", "2026-10-14T08:30:00Z", "Europe/Rome")}, "slot")
	runMutations(t, d, nil, en, "Your slot is 14 October 2026 at 10:30.", []mutation{
		{"utc wording stated as local time", "Your slot is 14 October 2026 at 08:30.", []ViolationKind{ViolationUnknownTime, ViolationMissingFact}},
		{"wrong day", "Your slot is 15 October 2026 at 10:30.", []ViolationKind{ViolationUnknownDate, ViolationMissingFact}},
		{"time only", "Your slot is at 10:30.", []ViolationKind{ViolationMissingFact}},
		{"date only", "Your slot is 14 October 2026.", []ViolationKind{ViolationMissingFact}},
	})
	for _, text := range []string{
		"Your slot is 2026-10-14T08:30:00Z.", "Your slot is 2026-10-14T10:30:00+02:00.",
		"Your slot is 14 October 2026, 10:30.", "Your slot is 10:30 on 14 October 2026.",
		"Your slot is 10:30 am on Wednesday, 14 October 2026.",
	} {
		if res := mustVerify(t, text, d, nil, en); !res.OK {
			t.Errorf("%q should verify: %v", text, res.Violations)
		}
	}
	if res := mustVerify(t, "Il tuo appuntamento è il 14 ottobre 2026 alle 10:30.", d, nil, it); !res.OK {
		t.Errorf("italian: %v", res.Violations)
	}

	// A local date can differ from the UTC date: 23:30 UTC on 14 October is 01:30 on the 15th in
	// Rome.
	late := directive(conversation.DialogueDirectiveActPresentResult,
		[]actions.Fact{datetimeFact("slot", "2026-10-14T23:30:00Z", "Europe/Rome")}, "slot")
	if res := mustVerify(t, "Your slot is 15 October 2026 at 01:30.", late, nil, en); !res.OK {
		t.Errorf("local date after midnight: %v", res.Violations)
	}
	if res := mustVerify(t, "Your slot is 14 October 2026 at 23:30.", late, nil, en); res.OK {
		t.Errorf("the UTC date and time were accepted as local")
	}

	// A datetime with no zone is UTC, and says so.
	utc := directive(conversation.DialogueDirectiveActPresentResult,
		[]actions.Fact{datetimeFact("slot", "2026-10-14T08:30:00Z", "")}, "slot")
	if res := mustVerify(t, "Your slot is 14 October 2026 at 08:30.", utc, nil, en); !res.OK {
		t.Errorf("zoneless datetime: %v", res.Violations)
	}
	if res := mustVerify(t, "Your slot is 14 October 2026 at 10:30.", utc, nil, en); res.OK {
		t.Errorf("a zoneless datetime was accepted in another time")
	}

	// A zone that cannot be loaded is a broken directive, never silently read as UTC.
	for _, zone := range []string{"Mars/Olympus", "Local", ""} {
		bad := directive(conversation.DialogueDirectiveActPresentResult,
			[]actions.Fact{datetimeFact("slot", "2026-10-14T08:30:00Z", "x")}, "slot")
		z := zone
		bad.Facts[0].Value.Value = actions.DatetimeValue{Value: "2026-10-14T08:30:00Z", Zone: &z}
		if _, err := Verify("14 October 2026 at 08:30", bad, nil, en); !errors.Is(err, ErrInvalidDirective) {
			t.Errorf("zone %q: err = %v, want ErrInvalidDirective", zone, err)
		}
		if _, err := Render(bad, nil, en); !errors.Is(err, ErrInvalidDirective) {
			t.Errorf("zone %q: Render err = %v, want ErrInvalidDirective", zone, err)
		}
	}
}

func TestBooleanFactNeedsAnAffirmationOrItsDisplay(t *testing.T) {
	d := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{boolFact("refundable", true)}, "refundable")
	if res := mustVerify(t, "Yes, it is refundable.", d, nil, en); !res.OK {
		t.Errorf("affirmation: %v", res.Violations)
	}
	if res := mustVerify(t, "Sì, è rimborsabile.", d, nil, it); !res.OK {
		t.Errorf("italian affirmation: %v", res.Violations)
	}
	if res := mustVerify(t, "It is refundable, probably.", d, nil, en); !res.Has(ViolationMissingFact) {
		t.Errorf("no affirmation: %v", res.Violations)
	}
	no := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{boolFact("refundable", false)}, "refundable")
	if res := mustVerify(t, "No, it is not refundable.", no, nil, en); !res.OK {
		t.Errorf("denial: %v", res.Violations)
	}
	disp := directive(conversation.DialogueDirectiveActPresentResult,
		[]actions.Fact{withDisplay(boolFact("refundable", true), "fully refundable")}, "refundable")
	if res := mustVerify(t, "The fee is fully refundable.", disp, nil, en); !res.OK {
		t.Errorf("display form: %v", res.Violations)
	}
}

func TestIdentifierWithNumericValueAllowsTheBareNumber(t *testing.T) {
	d := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{idFact("order", "20991")}, "order")
	if res := mustVerify(t, "Your order is 20991.", d, nil, en); !res.OK {
		t.Errorf("numeric identifier: %v", res.Violations)
	}
	if res := mustVerify(t, "Your order is 20992.", d, nil, en); res.OK {
		t.Errorf("a different number was accepted")
	}
}

// Everything the core and the host authored for the turn may be stated: labels, choices,
// suggestions, messages, source names. Their numbers and times are not invented.
func TestAuthoredStringsMayBeEchoed(t *testing.T) {
	d := directive(conversation.DialogueDirectiveActAskMissing, []actions.Fact{textFact("clinic", "Clinic at Via Roma 12")})
	d.MissingFields = []common.FieldName{"slot_start"}
	d.NextQuestion = &conversation.DialogueDirectiveQuestion{
		Field:      "slot_start",
		AnswerKind: conversation.DialogueDirectiveQuestionAnswerKindChoice,
		Choices: []actions.Choice{
			{ChoiceID: "c1", Label: "Tuesday 14 October, 10:30", Value: actions.CanonicalValue{Value: actions.DatetimeValue{Value: "2026-10-14T08:30:00Z"}}},
			{ChoiceID: "c2", Label: "Tuesday 14 October, 11:00", Value: actions.CanonicalValue{Value: actions.DatetimeValue{Value: "2026-10-14T09:00:00Z"}}},
		},
	}
	text := "Dr. Rossi has two slots at Via Roma 12: Tuesday 14 October, 10:30 or Tuesday 14 October, 11:00. Which one?"
	// "two" is an invented number even though the choices exist...
	if res := mustVerify(t, text, d, nil, en); !res.Has(ViolationUnknownNumber) {
		t.Errorf("the count of choices is a number the directive does not carry: %v", res.Violations)
	}
	// ...but the labels themselves, the text fact's address and the choice IDs may be echoed.
	text = "Which slot at Via Roma 12: Tuesday 14 October, 10:30 or Tuesday 14 October, 11:00 (c1 or c2)?"
	if res := mustVerify(t, text, d, nil, en); !res.OK {
		t.Errorf("echoing authored strings: %v", res.Violations)
	}
	// A time that is in neither choice is invented.
	if res := mustVerify(t, "Which slot: Tuesday 14 October, 10:30 or 11:30?", d, nil, en); !res.Has(ViolationUnknownTime) {
		t.Errorf("a third time: %v", res.Violations)
	}
}

func TestEmptyAndOversizeTextFail(t *testing.T) {
	d := confirmDirective()
	for _, text := range []string{"", "   \n\t "} {
		if res := mustVerify(t, text, d, nil, en); res.OK || !res.Has(ViolationEmptyText) {
			t.Errorf("empty text %q: %v", text, res)
		}
	}
	if res := mustVerify(t, strings.Repeat("a ", MaxTextBytes), d, nil, en); res.OK || !res.Has(ViolationTextTooLong) {
		t.Errorf("oversize text: %v", res)
	}
}

func TestUnusableInputsAreErrorsNeverVerdicts(t *testing.T) {
	d := confirmDirective()
	if _, err := Verify(goodConfirmEN, nil, nil, en); !errors.Is(err, ErrInvalidDirective) || contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
		t.Errorf("nil directive: %v", err)
	}
	for _, l := range []Language{"", "fr", "EN"} {
		_, err := Verify(goodConfirmEN, d, nil, Options{Language: l})
		if !errors.Is(err, ErrUnsupportedLanguage) || contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("language %q: err = %v", l, err)
		}
	}
	bad := confirmDirective()
	bad.Act = "shout"
	if _, err := Verify(goodConfirmEN, bad, nil, en); !errors.Is(err, ErrInvalidDirective) {
		t.Errorf("unknown act: %v", err)
	}
	// Facts of no known kind or with a malformed value are not usable.
	for name, f := range map[string]actions.Fact{
		"no value":       {Field: "x"},
		"no field":       {Value: actions.CanonicalValue{Value: actions.TextValue{Value: "x"}}},
		"bad decimal":    decFact("x", "1,5"),
		"bad date":       dateFact("x", "2026-02-30"),
		"bad time":       timeFact("x", "25:00"),
		"bad instant":    datetimeFact("x", "yesterday", ""),
		"bad money":      moneyFact("x", "ten", "EUR"),
		"bad currency":   moneyFact("x", "10", "euro"),
		"empty id":       idFact("x", ""),
		"lower currency": moneyFact("x", "10", "eur"),
	} {
		dd := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{f})
		if _, err := Verify("x", dd, nil, en); !errors.Is(err, ErrInvalidDirective) {
			t.Errorf("%s: err = %v, want ErrInvalidDirective", name, err)
		}
	}
	// A pointer variant is read like the value.
	pv := directive(conversation.DialogueDirectiveActPresentResult, nil)
	pv.Facts = []actions.Fact{{Field: "n", Value: actions.CanonicalValue{Value: &actions.IntegerValue{Value: 7}}}}
	pv.MustState = []common.FieldName{"n"}
	if res := mustVerify(t, "It is 7.", pv, nil, en); !res.OK {
		t.Errorf("pointer variant: %v", res.Violations)
	}
	var nilInt *actions.IntegerValue
	pv.Facts[0].Value.Value = nilInt
	if _, err := Verify("It is 7.", pv, nil, en); !errors.Is(err, ErrInvalidDirective) {
		t.Errorf("nil pointer variant: %v", err)
	}
	// An error is never accompanied by an OK result.
	if res, err := Verify(goodConfirmEN, nil, nil, en); err == nil || res.OK {
		t.Errorf("error with OK result: %v %v", res, err)
	}
	// A zero Result is not OK.
	if (Result{}).OK {
		t.Error("the zero Result is OK")
	}
}

func TestParseLanguage(t *testing.T) {
	for tag, want := range map[string]Language{"en": LangEN, "EN-gb": LangEN, "it_IT": LangIT, " it ": LangIT, "it-IT.UTF-8": LangIT} {
		if got, err := ParseLanguage(tag); err != nil || got != want {
			t.Errorf("ParseLanguage(%q) = %q, %v; want %q", tag, got, err, want)
		}
	}
	for _, tag := range []string{"", "fr", "de-DE", "e"} {
		if _, err := ParseLanguage(tag); !errors.Is(err, ErrUnsupportedLanguage) {
			t.Errorf("ParseLanguage(%q): err = %v", tag, err)
		}
	}
}

// SEC-011: text that displays differently from what was read does not verify.
func TestInvisibleCharactersAreRejected(t *testing.T) {
	d := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{timeFact("t", "10:30")}, "t")
	if res := mustVerify(t, "It is at 10:30.", d, nil, en); !res.OK {
		t.Fatalf("control: %v", res.Violations)
	}
	for name, text := range map[string]string{
		"zero-width space":       "It is at 10\u200b:30.",
		"zero-width joiner":      "It is at 10:30.\u200d",
		"right-to-left override": "It is at \u202e03:01 ta si ti",
		"left-to-right mark":     "\u200eIt is at 10:30.",
		"byte order mark":        "\ufeffIt is at 10:30.",
		"soft hyphen":            "It is at 10:30 to\u00adday.",
		"bidi isolate":           "It is at \u202610:30\u2069.",
		"control character":      "It is at 10:30.\x01",
		"null":                   "It is at 10:30.\x00",
	} {
		if res := mustVerify(t, text, d, nil, en); res.OK || !res.Has(ViolationInvisibleCharacters) {
			t.Errorf("%s: %q verified or lacks invisible_characters: %v", name, text, res.Violations)
		}
	}
	// Line breaks, tabs, no-break spaces and accents are ordinary text.
	if res := mustVerify(t, "It is at\t10:30.\r\nSì,\u00a0alle 10:30.", d, nil, it); !res.OK {
		t.Errorf("ordinary whitespace and accents: %v", res.Violations)
	}
}

// snake_case identifiers are identifiers; markdown emphasis is not.
func TestSnakeCaseTokensAreIdentifiers(t *testing.T) {
	d := directive(conversation.DialogueDirectiveActPresentResult,
		[]actions.Fact{idFact("ref", "usr_alice"), timeFact("slot_start", "10:30")}, "ref", "slot_start")
	if res := mustVerify(t, "Reference usr_alice, at 10:30, _as agreed_ and *as noted*. The slot_start is set.", d, nil, en); !res.OK {
		t.Errorf("control: %v", res.Violations)
	}
	for _, text := range []string{
		"Reference usr_alice, at 10:30, requested by usr_bob.",
		"Reference usr_alice, at 10:30, tenant_acme.",
		"Reference usr_alice, at 10:30, set by slot_end.",
	} {
		if res := mustVerify(t, text, d, nil, en); !res.Has(ViolationUnknownIdentifier) {
			t.Errorf("%q: %v", text, res.Violations)
		}
	}
}

func TestCompletionWordsBeyondTheCore(t *testing.T) {
	d := confirmDirective()
	for _, text := range []string{
		"Arranged: Dr. Rossi, 14 October 2026, 10:30, 50.00 EUR.",
		"Secured: Dr. Rossi, 14 October 2026, 10:30, 50.00 EUR.",
		"Sistemato: Dr. Rossi, 14 ottobre 2026, 10:30, 50,00 EUR.",
	} {
		opts := en
		if strings.Contains(text, "ottobre") {
			opts = it
		}
		if res := mustVerify(t, text, d, nil, opts); !res.Has(ViolationCompletionWithoutReceipt) {
			t.Errorf("%q: %v", text, res.Violations)
		}
	}
	// A progressive form says it is under way, not done.
	if res := mustVerify(t, "Your request is being processed: Dr. Rossi, 14 October 2026, 10:30, 50.00 EUR.", d, nil, en); res.Has(ViolationCompletionWithoutReceipt) {
		t.Errorf("a progressive form was read as a completion claim: %v", res.Violations)
	}
}

// An identifier that is a bare number (a choice ID "2") must not hide a citation marker.
func TestNumericIdentifiersDoNotHideCitationMarkers(t *testing.T) {
	d := directive(conversation.DialogueDirectiveActAskMissing, nil)
	d.NextQuestion = &conversation.DialogueDirectiveQuestion{
		Field: "slot", AnswerKind: conversation.DialogueDirectiveQuestionAnswerKindChoice,
		Choices: []actions.Choice{
			{ChoiceID: "1", Label: "First", Value: actions.CanonicalValue{Value: actions.TextValue{Value: "a"}}},
			{ChoiceID: "2", Label: "Second", Value: actions.CanonicalValue{Value: actions.TextValue{Value: "b"}}},
		},
	}
	d.Sources = []conversation.Citation{source(srcA, "Clinic policy", "Cancellation is free.")}
	if res := mustVerify(t, "Pick First or Second, as in [1].", d, nil, en); !res.OK {
		t.Errorf("control: %v", res.Violations)
	}
	if res := mustVerify(t, "Pick First or Second, as in [2].", d, nil, en); !res.Has(ViolationCitationNotInEvidence) {
		t.Errorf("citation [2] with one source: %v", res.Violations)
	}
}

// "5k" and "5 mila" are not 5: a scale after a number multiplies it.
func TestScaleWordsMultiplyTheNumber(t *testing.T) {
	d := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{moneyFact("fee", "5.00", "EUR")}, "fee")
	if res := mustVerify(t, "It costs 5 EUR.", d, nil, en); !res.OK {
		t.Fatalf("control: %v", res.Violations)
	}
	for _, c := range []struct {
		text string
		opts Options
	}{
		{"It costs 5k EUR.", en},
		{"It costs 5 million EUR.", en},
		{"Costa 5 mila euro.", it},
		{"Costa 5mila euro.", it},
		{"Costa 5 milioni di euro, cioè 5 EUR.", it},
	} {
		if res := mustVerify(t, c.text, d, nil, c.opts); res.OK {
			t.Errorf("%q verified", c.text)
		}
	}
	big := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{moneyFact("fee", "5000.00", "EUR")}, "fee")
	if res := mustVerify(t, "Costa 5 mila euro.", big, nil, it); !res.OK {
		t.Errorf("5 mila euro is 5000 EUR: %v", res.Violations)
	}
}

// An identifier that is a bare number (a choice ID "3") must not hide that number in a date, a
// time or an amount: it is allowed as the number it is, and nothing more.
func TestNumericIdentifiersDoNotHideNumbers(t *testing.T) {
	d := confirmDirective()
	d.AllowedSuggestions = []actions.Choice{
		{ChoiceID: "3", Label: "Other", Value: actions.CanonicalValue{Value: actions.TextValue{Value: "x"}}},
	}
	if res := mustVerify(t, goodConfirmEN+" Or pick option 3.", d, nil, en); !res.OK {
		t.Errorf("control: %v", res.Violations)
	}
	text := "Please confirm: Dr. Rossi on 14 October 2026 at 10:30, fee 50.00 EUR, or alternatively on 3 October, for 3 EUR, at 3 pm."
	res := mustVerify(t, text, d, nil, en)
	for _, k := range []ViolationKind{ViolationUnknownDate, ViolationUnknownAmount, ViolationUnknownTime} {
		if !res.Has(k) {
			t.Errorf("%q lacks %s: %v", text, k, res.Violations)
		}
	}
}
