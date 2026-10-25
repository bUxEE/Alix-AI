package verify

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func mustRealise(t *testing.T, text string, d *conversation.DialogueDirective, r *actions.ActionReceipt, opts Options) Outcome {
	t.Helper()
	out, err := Realise(text, d, r, opts)
	if err != nil {
		t.Fatalf("Realise: %v", err)
	}
	return out
}

// requireTemplate checks the outcome is the deterministic template, chosen because the model's
// text failed verification, and that the template itself verifies.
func requireTemplate(t *testing.T, out Outcome, d *conversation.DialogueDirective, r *actions.ActionReceipt, opts Options, want string) {
	t.Helper()
	if out.Source != SourceTemplate || out.Reason != ReasonVerificationFailed || len(out.Violations) == 0 {
		t.Fatalf("outcome = %+v, want the template after a failed verification", out)
	}
	if out.Text != want {
		t.Errorf("template text mismatch\n got: %q\nwant: %q", out.Text, want)
	}
	if res := mustVerify(t, out.Text, d, r, opts); !res.OK {
		t.Errorf("the template does not verify: %v", res.Violations)
	}
}

// SEC-011: when verification fails the turn uses the deterministic template for the same
// directive, and the user gets the correct facts in plainer words.
func TestTemplateFallbackEnglish(t *testing.T) {
	d := confirmDirective()

	// A good model text is kept as written.
	good := mustRealise(t, "  "+goodConfirmEN+"\n", d, nil, en)
	if good.Source != SourceModel || good.Reason != ReasonVerified || good.Text != goodConfirmEN || good.Violations != nil {
		t.Errorf("verified model text: %+v", good)
	}

	// An invented number, an invented claim, and a missing fact each fall back to the template.
	wantConfirm := "Please check these details:\n" +
		"- Doctor: Dr. Rossi\n" +
		"- Slot date: 14 October 2026\n" +
		"- Slot start: 10:30\n" +
		"- Fee: 50.00 EUR\n" +
		"Shall I go ahead? You can confirm, change it or decline."
	for name, text := range map[string]string{
		"invented number": "Please confirm Dr. Rossi on 14 October 2026 at 10:30 for 60.00 EUR.",
		"invented claim":  "Booked! Dr. Rossi, 14 October 2026, 10:30, 50.00 EUR.",
		"missing fact":    "Please confirm the visit with Dr. Rossi on 14 October 2026 at 10:30.",
		"empty":           "",
	} {
		t.Run(name, func(t *testing.T) {
			requireTemplate(t, mustRealise(t, text, d, nil, en), d, nil, en, wantConfirm)
		})
	}

	// The same for an outcome: the template states completion only because the receipt is committed.
	facts := []actions.Fact{idFact("appointment_id", "apt_20991"), dateFact("slot_date", "2026-10-14"), timeFact("slot_start", "10:30")}
	od := outcomeDirective(facts, "appointment_id", "slot_date", "slot_start")
	committed := receipt(actions.ActionReceiptStatusCommitted, facts...)
	requireTemplate(t, mustRealise(t, "Done: apt_77777.", od, committed, en), od, committed, en,
		"This has been completed.\n- Appointment id: apt_20991\n- Slot date: 14 October 2026\n- Slot start: 10:30")

	// The same facts with a receipt that is not committed: the model says it is booked, the
	// template says what is true.
	for status, want := range map[actions.ActionReceiptStatus]string{
		actions.ActionReceiptStatusRejected:  "This was not carried out. No changes were made.",
		actions.ActionReceiptStatusQueued:    "Your request has been accepted and is being processed. It is not finished yet.",
		actions.ActionReceiptStatusPartial:   "Only part of this was carried out.",
		actions.ActionReceiptStatusUncertain: "I couldn't confirm whether this was carried out. Please check in the application before trying again, and do not repeat it blindly.",
	} {
		r := receipt(status, facts...)
		out := mustRealise(t, "Your appointment apt_20991 is booked for 14 October 2026 at 10:30.", od, r, en)
		if out.Source != SourceTemplate || !out.hasViolation(ViolationCompletionWithoutReceipt) {
			t.Errorf("%s: outcome = %+v", status, out)
		}
		if !strings.HasPrefix(out.Text, want) {
			t.Errorf("%s: template = %q, want prefix %q", status, out.Text, want)
		}
		if strings.Contains(strings.ToLower(out.Text), "completed") || strings.Contains(out.Text, "booked") {
			t.Errorf("%s: the template claims completion: %q", status, out.Text)
		}
	}

	// Other acts.
	ask := directive(conversation.DialogueDirectiveActAskMissing, []actions.Fact{textFact("doctor", "Dr. Rossi")})
	ask.MissingFields = []common.FieldName{"slot_start"}
	ask.NextQuestion = &conversation.DialogueDirectiveQuestion{
		Field: "slot_start", AnswerKind: conversation.DialogueDirectiveQuestionAnswerKindChoice,
		Choices: []actions.Choice{
			{ChoiceID: "c1", Label: "Tuesday 14 October, 10:30", Value: actions.CanonicalValue{Value: actions.DatetimeValue{Value: "2026-10-14T08:30:00Z", Zone: ptr("Europe/Rome")}}},
			{ChoiceID: "c2", Label: "Tuesday 14 October, 11:00", Description: ptr("later"), Value: actions.CanonicalValue{Value: actions.DatetimeValue{Value: "2026-10-14T09:00:00Z", Zone: ptr("Europe/Rome")}}},
		},
	}
	requireTemplate(t, mustRealise(t, "I found 7 slots, pick one.", ask, nil, en), ask, nil, en,
		"I need a bit more information to continue.\nPlease choose Slot start:\n- Tuesday 14 October, 10:30\n- Tuesday 14 October, 11:00 (later)")

	refuse := directive(conversation.DialogueDirectiveActRefuse, nil)
	code := common.ErrorCode("permission_denied")
	refuse.ReasonCode = &code
	requireTemplate(t, mustRealise(t, "Sure, I made an exception for you at 12:00.", refuse, nil, en), refuse, nil, en,
		"I'm not able to do that.\nYou don't have permission to do that.")

	noEv := directive(conversation.DialogueDirectiveActReportNoEvidence, nil)
	rc := common.ErrorCode("no_supported_evidence")
	noEv.ReasonCode = &rc
	requireTemplate(t, mustRealise(t, "The policy says 30 days.", noEv, nil, en), noEv, nil, en,
		"I couldn't find anything in the available sources that answers this.")

	// A present_result with a source cites it by marker, with the page the source carries.
	pr := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{intFact("cancellation_notice_hours", 24)}, "cancellation_notice_hours")
	src := source(srcA, "Clinic cancellation policy", "Appointments can be cancelled free of charge up to 24 hours before the start.")
	src.Locator = &conversation.CitationLocator{Page: ptr(int64(3)), Section: ptr("Late cancellations")}
	pr.Sources = []conversation.Citation{src}
	pr.PermittedNextActions = []actions.PermittedNextAction{{Action: actions.PermittedNextActionActionCancel, Label: ptr("Cancel booking")}}
	requireTemplate(t, mustRealise(t, "It is 48 hours [2].", pr, nil, en), pr, nil, en,
		"Here is what I found:\n- Cancellation notice hours: 24\n"+
			"- Appointments can be cancelled free of charge up to 24 hours before the start. [1]\n"+
			"Sources:\n[1] Clinic cancellation policy (page 3, Late cancellations)\n"+
			"You can also: Cancel booking.")
}

func (o Outcome) hasViolation(k ViolationKind) bool {
	for _, v := range o.Violations {
		if v.Kind == k {
			return true
		}
	}
	return false
}

// SEC-011: the same in Italian.
func TestTemplateFallbackItalian(t *testing.T) {
	d := confirmDirective()
	goodIT := "Conferma: Dr. Rossi il 14 ottobre 2026 alle 10:30, costo 50,00 EUR. Procedo?"
	good := mustRealise(t, goodIT, d, nil, it)
	if good.Source != SourceModel || good.Text != goodIT {
		t.Errorf("verified italian text: %+v", good)
	}

	wantConfirm := "Controlla questi dettagli:\n" +
		"- Doctor: Dr. Rossi\n" +
		"- Slot date: 14 ottobre 2026\n" +
		"- Slot start: 10:30\n" +
		"- Fee: 50,00 EUR\n" +
		"Procedo? Puoi confermare, cambiarla oppure rifiutare."
	for name, text := range map[string]string{
		"invented number": "Conferma: Dr. Rossi il 14 ottobre 2026 alle 10:30, costo 60,00 EUR.",
		"invented claim":  "Prenotato! Dr. Rossi, 14 ottobre 2026, 10:30, 50,00 EUR.",
		"missing fact":    "Conferma la visita con Dr. Rossi il 14 ottobre 2026 alle 10:30.",
		"wrong separator": "Conferma: Dr. Rossi il 14 ottobre 2026 alle 10:30, costo 50.000 EUR.",
	} {
		t.Run(name, func(t *testing.T) {
			requireTemplate(t, mustRealise(t, text, d, nil, it), d, nil, it, wantConfirm)
		})
	}

	// Field labels from the application's terminology replace the field names.
	labelled := Options{Language: LangIT, FieldLabels: map[string]string{
		"doctor": "Medico", "slot_date": "Data", "slot_start": "Ora", "fee": "Costo",
	}}
	out := mustRealise(t, "Conferma 3 visite.", d, nil, labelled)
	wantLabelled := "Controlla questi dettagli:\n- Medico: Dr. Rossi\n- Data: 14 ottobre 2026\n- Ora: 10:30\n- Costo: 50,00 EUR\nProcedo? Puoi confermare, cambiarla oppure rifiutare."
	requireTemplate(t, out, d, nil, labelled, wantLabelled)

	facts := []actions.Fact{idFact("appointment_id", "apt_20991"), dateFact("slot_date", "2026-10-14"), timeFact("slot_start", "10:30")}
	od := outcomeDirective(facts, "appointment_id", "slot_date", "slot_start")
	committed := receipt(actions.ActionReceiptStatusCommitted, facts...)
	requireTemplate(t, mustRealise(t, "Fatto: apt_77777.", od, committed, it), od, committed, it,
		"Operazione completata.\n- Appointment id: apt_20991\n- Slot date: 14 ottobre 2026\n- Slot start: 10:30")

	for status, want := range map[actions.ActionReceiptStatus]string{
		actions.ActionReceiptStatusRejected:  "L'operazione non è stata portata a termine. Nessuna modifica è stata apportata.",
		actions.ActionReceiptStatusQueued:    "La richiesta è stata accettata ed è in elaborazione. Non è ancora terminata.",
		actions.ActionReceiptStatusPartial:   "Solo una parte dell'operazione è stata portata a termine.",
		actions.ActionReceiptStatusUncertain: "Non posso verificare se l'operazione è stata portata a termine. Controlla nell'applicazione prima di riprovare e non ripeterla alla cieca.",
	} {
		r := receipt(status, facts...)
		out := mustRealise(t, "Il tuo appuntamento apt_20991 è stato prenotato il 14 ottobre 2026 alle 10:30.", od, r, it)
		if out.Source != SourceTemplate || !out.hasViolation(ViolationCompletionWithoutReceipt) {
			t.Errorf("%s: outcome = %+v", status, out)
		}
		if !strings.HasPrefix(out.Text, want) {
			t.Errorf("%s: template = %q, want prefix %q", status, out.Text, want)
		}
	}

	refuse := directive(conversation.DialogueDirectiveActRefuse, nil)
	code := common.ErrorCode("permission_denied")
	refuse.ReasonCode = &code
	requireTemplate(t, mustRealise(t, "Certo, faccio un'eccezione alle 12:00.", refuse, nil, it), refuse, nil, it,
		"Non posso farlo.\nNon hai l'autorizzazione per farlo.")

	help := directive(conversation.DialogueDirectiveActOfferHelp, nil)
	help.AllowedSuggestions = []actions.Choice{
		{ChoiceID: "s1", Label: "Prenota una visita", Value: actions.CanonicalValue{Value: actions.IdentifierValue{Value: "appointments.book"}}},
		{ChoiceID: "s2", Label: "Annulla una visita", Value: actions.CanonicalValue{Value: actions.IdentifierValue{Value: "appointments.cancel"}}},
	}
	requireTemplate(t, mustRealise(t, "Posso fare 100 cose.", help, nil, it), help, nil, it,
		"Posso aiutarti con queste attività:\n- Prenota una visita\n- Annulla una visita")

	cancel := directive(conversation.DialogueDirectiveActAcknowledgeCancel, nil)
	requireTemplate(t, mustRealise(t, "Ho cancellato tutto.", cancel, nil, it), cancel, nil, it,
		"Va bene, ho interrotto la richiesta. Nessuna modifica è stata apportata.")
}

// Degraded mode: no model text at all; the template is the response.
func TestDegradedModeRendersTheTemplate(t *testing.T) {
	d := confirmDirective()
	for _, opts := range []Options{en, it} {
		out, err := Degraded(d, nil, opts)
		if err != nil {
			t.Fatal(err)
		}
		if out.Source != SourceTemplate || out.Reason != ReasonDegraded || out.Violations != nil || out.Text == "" {
			t.Errorf("%s: %+v", opts.Language, out)
		}
		again, _ := Degraded(d, nil, opts)
		if again.Text != out.Text {
			t.Errorf("%s: the template is not deterministic", opts.Language)
		}
		if res := mustVerify(t, out.Text, d, nil, opts); !res.OK {
			t.Errorf("%s: %v", opts.Language, res.Violations)
		}
	}
	if _, err := Degraded(d, nil, Options{Language: "fr"}); err == nil {
		t.Error("degraded mode in an unsupported language must be an error")
	}
	if _, err := Degraded(nil, nil, en); err == nil {
		t.Error("degraded mode with no directive must be an error")
	}
}

// Every example directive of the schemas, in both languages and against every receipt example,
// renders to a template that passes the verifier. This is what catches a template that states
// something the verifier would reject (a false positive in the checks or a wrong template).
func TestEveryExampleDirectiveRendersAndVerifies(t *testing.T) {
	directives := exampleDirectives(t)
	receipts := exampleReceipts(t)
	for name, d := range directives {
		for _, opts := range []Options{en, it} {
			var rs []*actions.ActionReceipt
			if d.Act == conversation.DialogueDirectiveActReportOutcome {
				for _, r := range receipts {
					rs = append(rs, r)
				}
				rs = append(rs, nil)
			} else {
				rs = []*actions.ActionReceipt{nil}
			}
			for _, r := range rs {
				label := name + "/" + string(opts.Language)
				if r != nil {
					label += "/" + string(r.Status)
				}
				text, err := Render(d, r, opts)
				if err != nil {
					t.Errorf("%s: Render: %v", label, err)
					continue
				}
				if strings.TrimSpace(text) == "" {
					t.Errorf("%s: empty template", label)
				}
				res, err := Verify(text, d, r, opts)
				// With no receipt only a model's text is distrusted; the template says "unconfirmed".
				onlyReceiptMissing := r == nil && len(res.Violations) == 1 && res.Violations[0].Kind == ViolationReceiptMissing
				if err != nil || !res.OK && !onlyReceiptMissing {
					t.Errorf("%s: the template does not verify (%v, %v):\n%s", label, err, res.Violations, text)
				}
				if again, _ := Render(d, r, opts); again != text {
					t.Errorf("%s: Render is not deterministic", label)
				}
			}
		}
	}
}

// Every error code has a sentence in every language, for both acts that carry a reason.
func TestEveryReasonCodeHasATemplate(t *testing.T) {
	for _, opts := range []Options{en, it} {
		m := messagesFor(opts.Language)
		known := map[contracts.ErrCode]bool{}
		for _, c := range contracts.AllErrCodes() {
			known[c] = true
			if m.reasons[c] == "" {
				t.Errorf("%s: no sentence for error code %s", opts.Language, c)
			}
		}
		for c := range m.reasons {
			if !known[c] {
				t.Errorf("%s: sentence for unknown error code %s", opts.Language, c)
			}
		}
		for _, c := range contracts.AllErrCodes() {
			code := common.ErrorCode(c)
			for _, act := range []conversation.DialogueDirectiveAct{conversation.DialogueDirectiveActRefuse, conversation.DialogueDirectiveActReportFailure} {
				d := directive(act, nil)
				d.ReasonCode = &code
				out, err := Degraded(d, nil, opts)
				if err != nil {
					t.Errorf("%s/%s/%s: %v", opts.Language, act, c, err)
					continue
				}
				if !strings.Contains(out.Text, m.reasons[c]) {
					t.Errorf("%s/%s/%s: template lacks the reason sentence: %q", opts.Language, act, c, out.Text)
				}
			}
		}
	}
	if enMessages.reasons[contracts.ErrPermissionDenied] == itMessages.reasons[contracts.ErrPermissionDenied] {
		t.Error("the italian reasons are not translated")
	}
}

// The templates state completion only for a committed receipt, and show the facts of a request
// that did not complete as the details of the request, never as a result.
func TestTemplatesNeverClaimCompletionWithoutACommittedReceipt(t *testing.T) {
	facts := []actions.Fact{dateFact("slot_date", "2026-10-14")}
	d := outcomeDirective(facts, "slot_date")
	for _, opts := range []Options{en, it} {
		m := messagesFor(opts.Language)
		for _, status := range []actions.ActionReceiptStatus{
			actions.ActionReceiptStatusRejected, actions.ActionReceiptStatusQueued,
			actions.ActionReceiptStatusPartial, actions.ActionReceiptStatusUncertain,
		} {
			r := receipt(status, facts...)
			r.Errors = []actions.Issue{{Code: "partial_failure", Message: "The confirmation email was not sent."}}
			r.FollowUp = &actions.FollowUp{Reference: "mail_771", State: actions.FollowUpStateFailed}
			text, err := Render(d, r, opts)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(text, m.committed) {
				t.Errorf("%s/%s: the template says it was completed: %s", opts.Language, status, text)
			}
			if !strings.Contains(text, m.requested+"\n- Slot date: ") {
				t.Errorf("%s/%s: the facts are not labelled as request details: %s", opts.Language, status, text)
			}
			// The host's own message about the failure is quoted, and does not count as a claim.
			if res := mustVerify(t, text, d, r, opts); !res.OK {
				t.Errorf("%s/%s: %v\n%s", opts.Language, status, res.Violations, text)
			}
			// The same text is a claim when the model says it about a receipt that is not committed:
			// the verifier agrees with the template about what completion means.
			if res := mustVerify(t, m.committed, d, r, opts); !res.Has(ViolationCompletionWithoutReceipt) {
				t.Errorf("%s/%s: the committed sentence %q passed against a %s receipt", opts.Language, status, m.committed, status)
			}
		}
		// With no receipt at all the outcome is stated as unconfirmed.
		text, err := Render(d, nil, opts)
		if err != nil || strings.Contains(text, m.committed) || !strings.Contains(text, m.uncertain) {
			t.Errorf("%s: no receipt: %q, %v", opts.Language, text, err)
		}
		// And only a committed receipt gets the committed sentence.
		text, err = Render(d, receipt(actions.ActionReceiptStatusCommitted, facts...), opts)
		if err != nil || !strings.HasPrefix(text, m.committed) || strings.Contains(text, m.requested) {
			t.Errorf("%s: committed: %q, %v", opts.Language, text, err)
		}
	}
}

// Every canonical kind formats to something its own check reads back, in both languages.
func TestTemplatesRenderEveryCanonicalKind(t *testing.T) {
	facts := []actions.Fact{
		textFact("doctor", "Dr. Rossi (studio 12)"),
		intFact("count", 3),
		decFact("rate", "12.50"),
		boolFact("refundable", true),
		dateFact("slot_date", "2026-10-14"),
		timeFact("slot_start", "10:30"),
		datetimeFact("created", "2026-10-14T08:30:00Z", "Europe/Rome"),
		datetimeFact("created_utc", "2026-10-14T08:30:45Z", ""),
		moneyFact("fee", "1250.50", "EUR"),
		idFact("appointment_id", "apt_20991"),
		idFact("order", "20991"),
		intFact("balance", -5),
		withDisplay(dateFact("alt_date", "2026-10-15"), "domani"),
		textFact("address_line_1", "Via Roma 12"),
	}
	must := []string{"doctor", "count", "rate", "refundable", "slot_date", "slot_start", "created", "created_utc",
		"fee", "appointment_id", "order", "balance", "alt_date", "address_line_1"}
	d := directive(conversation.DialogueDirectiveActPresentResult, facts, must...)
	want := map[Language][]string{
		LangEN: {"Dr. Rossi (studio 12)", "- Count: 3", "- Rate: 12.50", "- Refundable: Yes", "14 October 2026", "10:30",
			"14 October 2026, 10:30", "14 October 2026, 08:30:45 UTC", "1250.50 EUR", "apt_20991", "- Order: 20991", "- Balance: -5", "domani", "Via Roma 12"},
		LangIT: {"Dr. Rossi (studio 12)", "- Count: 3", "- Rate: 12,50", "- Refundable: Sì", "14 ottobre 2026", "10:30",
			"14 ottobre 2026, 10:30", "14 ottobre 2026, 08:30:45 UTC", "1250,50 EUR", "apt_20991", "- Order: 20991", "- Balance: -5", "domani", "Via Roma 12"},
	}
	for _, opts := range []Options{en, it} {
		text, err := Render(d, nil, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range want[opts.Language] {
			if !strings.Contains(text, w) {
				t.Errorf("%s: template lacks %q:\n%s", opts.Language, w, text)
			}
		}
		if res := mustVerify(t, text, d, nil, opts); !res.OK {
			t.Errorf("%s: %v\n%s", opts.Language, res.Violations, text)
		}
	}
}
