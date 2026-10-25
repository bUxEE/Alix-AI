package conversation

import (
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The conversation tests of master plan 22.3, one per behaviour, all against the clinic
// reference application (see doc.go). Each one scripts only what a model would PROPOSE;
// what the engine does with it, what the application is asked, what the policy gateway
// allows and what the user is told are the things under test, and each test asserts them
// from the engine's result, the application's own record of what it received and did, and
// the coordinator's and the session's stored state, never from the script it wrote.

// The utterances the clinic's own signed conversation contracts give as examples (the
// contract examples are test data, never runtime policy, master plan 9.2). Maria Rossi is
// the contract's own name; the clinic stand-in has two of her, so it is Luca Ferri who is
// booked when no ambiguity is wanted.
const (
	sayFreeEN = "Which appointments are free on 2026-10-14?"
	sayFreeIT = "Quali visite sono libere il 2026-10-14?"
	sayBookEN = "Book Luca Ferri for 2026-10-14 10:30"
	sayBookIT = "Prenota Luca Ferri il 2026-10-14 alle 10:30"
)

// slotIDs of the clinic's free slots, by when they start.
const (
	slot14at0900 = "slot_free00001"
	slot14at1030 = "slot_free00002"
	slot14at1130 = "slot_free00003"
	slot15at0900 = "slot_free00004"
	slot15at1400 = "slot_free00005"
)

func s(v ...string) []string { return v }

// bookLuca proposes and runs the one-turn booking of Luca Ferri at 2026-10-14 10:30 and
// asserts it stopped at a confirmation request.
func (w *world) bookLuca(sc contracts.Scope, lang string) dialogue.Result {
	w.t.Helper()
	w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"})
	text := sayBookEN
	if lang == "it" {
		text = sayBookIT
	}
	res := w.say(sc, lang, text)
	wantAct(w.t, res, "request_confirmation")
	if res.Prepared == nil || res.Confirmation.IsZero() {
		w.t.Fatalf("no preparation or no confirmation token: %+v", res.Prepared)
	}
	return res
}

// lastCall returns the most recent request of one operation on one capability.
func (w *world) lastCall(op adapters.Operation, capabilityID string) clinicCall {
	w.t.Helper()
	calls := w.host.callsSnapshot()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Operation == op && calls[i].CapabilityID == capabilityID {
			return calls[i]
		}
	}
	w.t.Fatalf("the host never saw a %s of %s", op, capabilityID)
	return clinicCall{}
}

// resolverCalls counts the resolution reads the host answered for one entity type.
func (w *world) resolverCalls(entityType string) int {
	n := 0
	for _, c := range w.host.callsSnapshot() {
		if c.Operation == adapters.OpRead && c.Fields["entity_type"] == entityType {
			n++
		}
	}
	return n
}

// ---- direct commands ----

// A direct command names everything it needs: the engine asks nothing, does it as far as
// the level of the action allows, and says exactly what happened. A read is answered; a
// consequential action is PREPARED and stops at a confirmation that only the client can
// give (master plan 3 rules 7 and 8).
func TestConvDirectCommand(t *testing.T) {
	t.Run("a read is answered from the application's own facts", func(t *testing.T) {
		for _, c := range []struct{ lang, say string }{{"en", sayFreeEN}, {"it", sayFreeIT}} {
			t.Run(c.lang, func(t *testing.T) {
				w := newWorld(t)
				w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"})

				res := w.say(receptionScope(), c.lang, c.say)

				wantAct(t, res, "present_result")
				if res.Code != "" {
					t.Fatalf("code = %q", res.Code)
				}
				if got, _ := factValue(res, "slot_count"); got != "3" {
					t.Fatalf("slot_count = %q, want the host's 3", got)
				}
				if got, _ := factValue(res, "slot_1_starts_at"); got != "2026-10-14 09:00" {
					t.Fatalf("slot_1_starts_at = %q", got)
				}
				// The question the user asked is the read the application got, with the
				// one value the user gave and nothing else.
				if got := w.lastCall(adapters.OpRead, "availability.list").Fields; len(got) != 1 || got["date"] != "2026-10-14" {
					t.Fatalf("availability.list was asked with %v, want only the date", got)
				}
				// A level 0 read changes nothing and prepares nothing.
				w.noWrites()
				if w.proposals() != 1 || res.Response.Source != verify.SourceTemplate {
					t.Fatalf("proposals = %d, response = %+v", w.proposals(), res.Response)
				}
				// The record the user can refer to next is the host's, in the order it was listed.
				if st := w.session(receptionScope()).State; st.Goal == nil || st.Goal.CapabilityID != "availability.list" {
					t.Fatalf("goal = %+v", st.Goal)
				}
			})
		}
	})

	t.Run("a booking is prepared in one turn and committed only by the client's confirmation", func(t *testing.T) {
		for _, lang := range []string{"en", "it"} {
			t.Run(lang, func(t *testing.T) {
				w := newWorld(t)
				sc := receptionScope()

				res := w.bookLuca(sc, lang)

				// Everything was named, so nothing was asked; the application prepared and
				// has changed nothing.
				if len(res.Directive.MissingFields) != 0 || res.Directive.NextQuestion != nil {
					t.Fatalf("a complete command was asked a question: %+v", res.Directive)
				}
				if res.Directive.Confirmation.Mode != act.ConfirmationRequirementModeExplicit {
					t.Fatalf("confirmation = %+v, want explicit", res.Directive.Confirmation)
				}
				if got, want := w.lastCall(adapters.OpPrepare, "appointments.book").Fields, (map[string]any{"patient": pidLuca, "slot": slot14at1030}); !sameFields(got, want) {
					t.Fatalf("the application was asked to prepare %v, want the resolved records %v", got, want)
				}
				if w.host.writes() != 0 || w.host.count(adapters.OpCommit) != 0 || w.host.slot(slot14at1030).status != "free" {
					t.Fatal("something was committed before the user confirmed")
				}
				if res.Response.Source != verify.SourceTemplate || !strings.Contains(res.Response.Text, "2026-10-14 10:30") {
					t.Fatalf("response = %+v", res.Response)
				}
				if states := w.prepStates(); len(states) != 1 || states[0] != "prepared" {
					t.Fatalf("preparations = %v", states)
				}

				out := w.confirm(sc, res)

				wantAct(t, out, "report_outcome")
				if out.Receipt == nil || out.Receipt.Status != act.ActionReceiptStatusCommitted {
					t.Fatalf("receipt = %+v", out.Receipt)
				}
				if w.host.writes() != 1 || w.host.count(adapters.OpCommit) != 1 {
					t.Fatalf("effects = %d, commits = %d, want exactly one", w.host.writes(), w.host.count(adapters.OpCommit))
				}
				if got := w.host.slot(slot14at1030); got.status != "booked" {
					t.Fatalf("the slot is %s", got.status)
				}
				if booked := w.host.appointmentsOf(pidLuca); len(booked) != 2 { // the clinic seeds one for Luca
					t.Fatalf("Luca's appointments = %+v", booked)
				}
				if ref, _ := factValue(out, "appointment_reference"); !strings.HasPrefix(ref, "APT-") {
					t.Fatalf("appointment_reference = %q", ref)
				}

				// The confirmation is single-use: presenting it again changes nothing.
				again, err := w.eng.Turn(ctxb(), sc, dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: string(res.Prepared.PreparationID), Token: res.Confirmation}})
				if err != nil {
					t.Fatalf("a repeated confirmation is a refused turn, not an error: %v", err)
				}
				if again.Receipt != nil && again.Receipt.ReceiptID != out.Receipt.ReceiptID {
					t.Fatalf("a repeated confirmation produced another receipt")
				}
				if w.host.writes() != 1 {
					t.Fatalf("a repeated confirmation applied a second effect (%d)", w.host.writes())
				}
			})
		}
	})

	t.Run("typed agreement is not a confirmation", func(t *testing.T) {
		w := newWorld(t)
		sc := receptionScope()
		res := w.bookLuca(sc, "en")

		again := w.say(sc, "en", "yes")

		wantAct(t, again, "request_confirmation")
		if !again.Confirmation.IsZero() {
			t.Fatal("a typed yes was answered with a fresh confirmation secret")
		}
		if again.Prepared == nil || again.Prepared.PreparationID != res.Prepared.PreparationID {
			t.Fatal("the pending request was not shown again")
		}
		w.noCommits()
		if w.proposals() != 1 {
			t.Fatalf("a typed yes reached the model (%d proposals)", w.proposals())
		}
	})
}

func sameFields(got, want map[string]any) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			return false
		}
	}
	return true
}

// ---- missing details ----

// A request that lacks a required field is answered with a question for exactly that
// field, from the capability's own contract, before the application is bothered; the next
// turn supplies it and the goal goes on where it was (the clinic package's own
// conversation_cases book-asks-for-the-slot and prenota-chiede-il-paziente).
func TestConvMissingDetails(t *testing.T) {
	t.Run("en: the slot is missing", func(t *testing.T) {
		w := newWorld(t)
		sc := receptionScope()
		w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri"})

		ask := w.say(sc, "en", "Please book Luca Ferri")

		wantAct(t, ask, "ask_missing")
		if got := ask.Directive.MissingFields; len(got) != 1 || got[0] != "slot" {
			t.Fatalf("missing = %v, want [slot]", got)
		}
		if q := ask.Directive.NextQuestion; q == nil || q.Field != "slot" {
			t.Fatalf("next question = %+v", q)
		}
		// Nothing was resolved, validated or prepared: the application was not asked.
		if n := w.host.totalCalls(); n != 0 {
			t.Fatalf("the application saw %d calls before the request was complete", n)
		}
		if g := w.goal(sc); g == nil || g.Pending == nil || g.Pending.Field != "slot" || g.Slots["patient"].Value != "Luca Ferri" {
			t.Fatalf("goal = %+v, want the patient kept and the slot pending", g)
		}

		// The answer is just the missing value; the goal keeps the rest.
		w.propose("appointments.book", "task", map[string]any{"slot": "The next available one"})
		done := w.say(sc, "en", "The next available one")

		wantAct(t, done, "request_confirmation")
		// "The next available one" is decided by the application, not by the model: the
		// earliest bookable slot of the site.
		if got := w.lastCall(adapters.OpPrepare, "appointments.book").Fields; got["slot"] != slot14at0900 || got["patient"] != pidLuca {
			t.Fatalf("prepared with %v", got)
		}
		if w.proposals() != 2 {
			t.Fatalf("proposals = %d, want one per turn", w.proposals())
		}
	})

	t.Run("it: the patient is missing", func(t *testing.T) {
		w := newWorld(t)
		sc := receptionScope()
		w.propose("appointments.book", "task", map[string]any{"slot": "primo orario libero"})

		ask := w.say(sc, "it", "Prenota il primo orario libero")

		wantAct(t, ask, "ask_missing")
		if got := ask.Directive.MissingFields; len(got) != 1 || got[0] != "patient" {
			t.Fatalf("missing = %v, want [patient]", got)
		}
		// The question is in the user's language and in the application's own words
		// (the field label of the Italian conversation contract).
		mustContain(t, ask.Response.Text, "Paziente", "the Italian question")
		if ask.Language != verify.LangIT {
			t.Fatalf("language = %s", ask.Language)
		}

		w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri"})
		done := w.say(sc, "it", "Luca Ferri")

		wantAct(t, done, "request_confirmation")
		if got := w.lastCall(adapters.OpPrepare, "appointments.book").Fields; got["slot"] != slot14at0900 || got["patient"] != pidLuca {
			t.Fatalf("prepared with %v", got)
		}
	})

	t.Run("a read that needs a value asks for it too", func(t *testing.T) {
		w := newWorld(t)
		sc := receptionScope()
		w.propose("patients.find", "task", map[string]any{})

		ask := w.say(sc, "it", "Cerca un paziente")

		wantAct(t, ask, "ask_missing")
		if got := ask.Directive.MissingFields; len(got) != 1 || got[0] != "query" {
			t.Fatalf("missing = %v, want [query]", got)
		}
		if w.host.totalCalls() != 0 {
			t.Fatal("the application was asked a read with no query")
		}
	})

	t.Run("a value the application rejects is asked again, not guessed", func(t *testing.T) {
		w := newWorld(t)
		sc := receptionScope()
		w.propose("appointments.book", "task", map[string]any{"patient": "Nobody Here", "slot": "2026-10-14 10:30"})

		res := w.say(sc, "en", "Book Nobody Here for 2026-10-14 10:30")

		wantAct(t, res, "ask_invalid")
		if w.host.countFor(adapters.OpPrepare, "appointments.book") != 0 {
			t.Fatal("a booking was prepared for a patient nobody matches")
		}
		if g := w.goal(sc); g.Pending == nil || g.Pending.Field != "patient" {
			t.Fatalf("the question that stands is %+v, want the patient", g.Pending)
		}
	})
}

// ---- multiple details in one turn ----

// Several values in one message are all taken, each validated and resolved by the
// application, in a single model call, and the goal goes straight to the confirmation.
func TestConvMultipleDetailsOneTurn(t *testing.T) {
	for _, c := range []struct{ lang, say, reason string }{
		{"en", "Book Luca Ferri for 2026-10-14 10:30, it is for a flu shot", "flu shot"},
		{"it", "Prenota Luca Ferri il 2026-10-14 alle 10:30 per un controllo", "controllo"},
	} {
		t.Run(c.lang, func(t *testing.T) {
			w := newWorld(t)
			sc := receptionScope()
			w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30", "reason": c.reason})

			res := w.say(sc, c.lang, c.say)

			wantAct(t, res, "request_confirmation")
			if w.proposals() != 1 {
				t.Fatalf("the model was called %d times for one message", w.proposals())
			}
			// Both entities were resolved by the application, never by the model.
			if w.resolverCalls("patient") != 1 || w.resolverCalls("slot") != 1 {
				t.Fatalf("resolver calls: patient %d, slot %d", w.resolverCalls("patient"), w.resolverCalls("slot"))
			}
			// The free text the user gave reaches the application exactly as a value, next to
			// the two records the application itself resolved.
			want := map[string]any{"patient": pidLuca, "slot": slot14at1030, "reason": c.reason}
			if got := w.lastCall(adapters.OpPrepare, "appointments.book").Fields; !sameFields(got, want) {
				t.Fatalf("prepared with %v, want %v", got, want)
			}
			// Each value says where it came from: the user's words, or the host's resolution.
			g := w.goal(sc)
			if g.Slots["reason"].Origin != sessions.OriginUserTurn || g.Slots["patient"].Origin != sessions.OriginEntityResolution || g.Slots["slot"].Origin != sessions.OriginEntityResolution {
				t.Fatalf("slots = %+v", g.Slots)
			}
			if len(g.Entities) != 2 {
				t.Fatalf("resolved entities = %+v, want the patient and the slot with their data versions", g.Entities)
			}
			for _, e := range g.Entities {
				if e.Version == "" {
					t.Fatalf("entity %+v has no data version", e)
				}
			}
			w.noCommits()

			out := w.confirm(sc, res)
			wantAct(t, out, "report_outcome")
			var booked clinicAppt
			for _, a := range w.host.appointmentsOf(pidLuca) {
				if a.slotID == slot14at1030 {
					booked = a
				}
			}
			if booked.reason != c.reason {
				t.Fatalf("the application recorded the reason %q, want %q", booked.reason, c.reason)
			}
		})
	}
}

// ---- ambiguous entities ----

// A name that matches several records is never resolved by the model: the application
// lists its candidates, the user chooses by a structured choice, and only a candidate the
// application offered is accepted.
func TestConvAmbiguousEntity(t *testing.T) {
	for _, c := range []struct{ lang, say string }{
		{"en", "Book Maria Rossi for 2026-10-14 10:30"},
		{"it", "Prenota Maria Rossi il 2026-10-14 alle 10:30"},
	} {
		t.Run(c.lang, func(t *testing.T) {
			w := newWorld(t)
			sc := receptionScope()
			// The model is sure it is the first Maria Rossi and says so. The application does
			// not take its word for it.
			w.propose("appointments.book", "task", map[string]any{"patient": "Maria Rossi", "slot": "2026-10-14 10:30"},
				map[string]any{"entity_type": "patient", "mention": "Maria Rossi", "field": "patient", "candidate_id": pidMaria})

			ask := w.say(sc, c.lang, c.say)

			wantAct(t, ask, "disambiguate")
			q := ask.Directive.NextQuestion
			if q == nil || q.Field != "patient" || len(q.Choices) != 2 {
				t.Fatalf("question = %+v, want the patient with two choices", q)
			}
			// The candidates are the application's, with its labels, in its order.
			if string(q.Choices[0].ChoiceID) != pidMaria || string(q.Choices[1].ChoiceID) != pidMaria2 {
				t.Fatalf("choices = %+v", q.Choices)
			}
			if q.Choices[0].Label != "Maria Rossi (born 1984-03-12)" || q.Choices[1].Label != "Maria Rossi (born 1991-07-30)" {
				t.Fatalf("labels = %q, %q", q.Choices[0].Label, q.Choices[1].Label)
			}
			mustContain(t, ask.Response.Text, "1991-07-30", "the response lists the host's labels")
			// Nothing was prepared on a guess, and the hint changed nothing.
			if w.host.countFor(adapters.OpPrepare, "appointments.book") != 0 {
				t.Fatal("a booking was prepared before the user chose")
			}
			hint := w.lastCall(adapters.OpRead, "appointments.book").Fields
			if hint["mention"] != "Maria Rossi" {
				t.Fatalf("the host was asked %v", hint)
			}

			// A choice nobody offered is refused and the question stands.
			bad := w.choose(sc, pidLuca)
			wantAct(t, bad, "ask_invalid")
			if q := bad.Directive.NextQuestion; q == nil || len(q.Choices) != 2 {
				t.Fatalf("the question did not stand: %+v", bad.Directive.NextQuestion)
			}
			if w.host.countFor(adapters.OpPrepare, "appointments.book") != 0 {
				t.Fatal("a patient nobody offered was accepted")
			}

			// A choice from the list is the record; no model reads it.
			before := w.proposals()
			chosen := w.choose(sc, pidMaria2)

			wantAct(t, chosen, "request_confirmation")
			if w.proposals() != before {
				t.Fatal("a structured choice was read by the model")
			}
			if got := w.lastCall(adapters.OpPrepare, "appointments.book").Fields["patient"]; got != pidMaria2 {
				t.Fatalf("prepared for %v, want the chosen %s", got, pidMaria2)
			}
			g := w.goal(sc)
			if g.Slots["patient"].Origin != sessions.OriginEntityResolution {
				t.Fatalf("patient slot = %+v", g.Slots["patient"])
			}
			if out := w.confirm(sc, chosen); out.Receipt == nil || w.host.appointmentsOf(pidMaria2)[0].slotID != slot14at1030 {
				t.Fatalf("the booking is not for the second Maria Rossi: %+v", w.host.appointmentsOf(pidMaria2))
			}
			if n := len(w.host.appointmentsOf(pidMaria)); n != 1 { // only the seeded one
				t.Fatalf("the first Maria Rossi has %d appointments, want the seeded one only", n)
			}
		})
	}

	t.Run("the same name is not ambiguous when the caller can only see one", func(t *testing.T) {
		// The patient Maria Rossi saying "me" reaches only herself: the application's
		// record-level visibility, not the model, makes the name unique.
		w := newWorld(t)
		sc := patientScope()
		w.propose("appointments.book", "task", map[string]any{"patient": "me", "slot": "2026-10-14 10:30"})

		res := w.say(sc, "en", "Please book me the 2026-10-14 10:30 appointment")

		wantAct(t, res, "request_confirmation")
		if got := w.lastCall(adapters.OpPrepare, "appointments.book").Fields["patient"]; got != pidMaria {
			t.Fatalf("prepared for %v, want her own record", got)
		}
	})
}

// ---- corrections ----

// A value that replaces one already given is a correction: only that slot changes, the
// preparation built on the old value is voided at the coordinator (so its confirmation
// cannot commit it), and the goal is prepared again with the corrected value.
func TestConvCorrection(t *testing.T) {
	for _, c := range []struct{ lang, say string }{
		{"en", "No, make it 2026-10-15 09:00"},
		{"it", "No, invece il 2026-10-15 alle 09:00"},
	} {
		t.Run(c.lang, func(t *testing.T) {
			w := newWorld(t)
			sc := receptionScope()
			first := w.bookLuca(sc, c.lang)
			before := w.goal(sc)

			w.clk.Advance(time.Minute)
			w.propose("appointments.book", "correction", map[string]any{"slot": "2026-10-15 09:00"})
			second := w.say(sc, c.lang, c.say)

			wantAct(t, second, "request_confirmation")
			if !hasTransition(second, dialogue.TransitionCorrected) {
				t.Fatalf("transitions = %v", second.Trace.Transitions)
			}
			g := w.goal(sc)
			if g.ID != before.ID {
				t.Fatal("the correction started another goal")
			}
			if s := g.Slots["slot"]; s.Value != slot15at0900 || s.Origin != sessions.OriginEntityResolution {
				t.Fatalf("slot = %+v, want the new slot resolved by the host", s)
			}
			if p := g.Slots["patient"]; p.Value != pidLuca || p.TurnID != before.Slots["patient"].TurnID {
				t.Fatalf("patient = %+v, want it untouched", p)
			}
			// The application is asked to prepare again, with the corrected value and the kept one.
			if got := w.lastCall(adapters.OpPrepare, "appointments.book").Fields; !sameFields(got, map[string]any{"patient": pidLuca, "slot": slot15at0900}) {
				t.Fatalf("re-prepared with %v", got)
			}
			// The old preparation is dead at the coordinator and its token commits nothing.
			if states := w.prepStates(); len(states) != 2 || states[0] != "invalidated" || states[1] != "prepared" {
				t.Fatalf("preparation states = %v", states)
			}
			if _, err := w.co.Confirm(ctxb(), sc, actions.ConfirmInput{PreparationID: string(first.Prepared.PreparationID), Token: first.Confirmation}); err == nil {
				t.Fatal("the coordinator confirmed a preparation built on a corrected value")
			}
			old, err := w.eng.Turn(ctxb(), sc, dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: string(first.Prepared.PreparationID), Token: first.Confirmation}})
			if err != nil {
				t.Fatal(err)
			}
			if old.Code != contracts.ErrConflict || old.Receipt != nil {
				t.Fatalf("old confirmation: code = %q, receipt = %+v", old.Code, old.Receipt)
			}
			w.noCommits()

			// The booking that results is the corrected one, and only it.
			out := w.confirm(sc, second)
			wantAct(t, out, "report_outcome")
			if w.host.writes() != 1 || w.host.slot(slot15at0900).status != "booked" || w.host.slot(slot14at1030).status != "free" {
				t.Fatalf("effects = %d, 10-15 09:00 is %s, 10-14 10:30 is %s", w.host.writes(), w.host.slot(slot15at0900).status, w.host.slot(slot14at1030).status)
			}
		})
	}
}
