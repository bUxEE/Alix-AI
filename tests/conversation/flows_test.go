package conversation

import (
	"testing"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/sessions"
)

// ---- relative references ----

// "The second one" and "that patient" are resolved from the state by rules, never by the
// model: an ordinal indexes the records the last result presented, in the order the
// application listed them; a demonstrative is the one record of the field's type the state
// holds. A reference the state cannot resolve to exactly one answer is not guessed.
func TestConvRelativeReference(t *testing.T) {
	t.Run("an ordinal picks the record the application listed in that place", func(t *testing.T) {
		for _, c := range []struct {
			lang, list, listDate, say, mention, wantSlot string
		}{
			{"en", sayFreeEN, "2026-10-14", "Book the second one for Luca Ferri", "the second one", slot14at1030},
			{"it", "Quali visite sono libere il 2026-10-15?", "2026-10-15", "Prenota il secondo per Luca Ferri", "il secondo", slot15at1400},
		} {
			t.Run(c.lang, func(t *testing.T) {
				w := newWorld(t)
				sc := receptionScope()
				w.propose("availability.list", "task", map[string]any{"date": c.listDate})
				listed := w.say(sc, c.lang, c.list)
				wantAct(t, listed, "present_result")

				// The model writes the user's words into the slot. They are a reference, so
				// the engine overrides whatever the model made of them with the record.
				w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri", "slot": c.mention})
				res := w.say(sc, c.lang, c.say)

				wantAct(t, res, "request_confirmation")
				if got := w.lastCall(adapters.OpPrepare, "appointments.book").Fields["slot"]; got != c.wantSlot {
					t.Fatalf("prepared slot = %v, want %s (the second slot the application listed)", got, c.wantSlot)
				}
				if n := w.resolverCalls("slot"); n != 0 {
					t.Fatalf("the application resolved %d slot mentions; a reference is resolved from the state", n)
				}
				if !hasTransition(res, dialogue.TransitionReference) {
					t.Fatalf("transitions = %v", res.Trace.Transitions)
				}
				g := w.goal(sc)
				if s := g.Slots["slot"]; s.Value != c.wantSlot || s.Origin != sessions.OriginEntityResolution {
					t.Fatalf("slot = %+v, want the listed record as a resolved entity", s)
				}
				// The record keeps the data version the listing relied on, so a later change
				// to it invalidates it like any other resolved entity.
				var found bool
				for _, e := range g.Entities {
					found = found || (e.Field == "slot" && e.EntityID == c.wantSlot && e.Version != "")
				}
				if !found {
					t.Fatalf("entities = %+v", g.Entities)
				}
			})
		}
	})

	t.Run("a demonstrative is the one record the state holds", func(t *testing.T) {
		for _, c := range []struct{ lang, say string }{
			{"en", "Book that patient for 2026-10-14 10:30"},
			{"it", "Prenota quel paziente per il 2026-10-14 alle 10:30"},
		} {
			t.Run(c.lang, func(t *testing.T) {
				w := newWorld(t)
				sc := receptionScope()
				w.propose("patients.find", "task", map[string]any{"query": "Luca Ferri"})
				found := w.say(sc, c.lang, "Find the patient Luca Ferri")
				wantAct(t, found, "present_result")

				w.propose("appointments.book", "task", map[string]any{"patient": "that patient", "slot": "2026-10-14 10:30"})
				res := w.say(sc, c.lang, c.say)

				wantAct(t, res, "request_confirmation")
				if got := w.lastCall(adapters.OpPrepare, "appointments.book").Fields["patient"]; got != pidLuca {
					t.Fatalf("prepared for %v, want the patient the search found", got)
				}
				if n := w.resolverCalls("patient"); n != 0 {
					t.Fatalf("the application resolved %d patient mentions", n)
				}
			})
		}
	})

	t.Run("a reference the state cannot answer is not guessed", func(t *testing.T) {
		for _, c := range []struct{ name, say, mention string }{
			{"an ordinal past the end of the list", "Book the fifth one for Luca Ferri", "the fifth one"},
			{"a demonstrative after a list of several", "Book that slot for Luca Ferri", "that slot"},
		} {
			t.Run(c.name, func(t *testing.T) {
				w := newWorld(t)
				sc := receptionScope()
				w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"}) // three slots
				w.say(sc, "en", sayFreeEN)

				w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri", "slot": c.mention})
				res := w.say(sc, "en", c.say)

				wantAct(t, res, "ask_invalid")
				if w.host.countFor(adapters.OpPrepare, "appointments.book") != 0 {
					t.Fatal("a booking was prepared from a reference nothing could resolve")
				}
				// The application was asked about the user's own words, and said they match
				// nothing; the engine invented no record.
				slotCalls := 0
				for _, call := range w.host.callsSnapshot() {
					if call.Fields["entity_type"] == "slot" {
						slotCalls++
						if call.Fields["mention"] != c.mention {
							t.Fatalf("the application was asked about %v, want the user's words %q", call.Fields["mention"], c.mention)
						}
					}
				}
				if slotCalls != 1 {
					t.Fatalf("slot resolutions = %d, want 1", slotCalls)
				}
			})
		}
	})
}

// ---- cancel and restart ----

// "Never mind" drops the goal and everything built on it: the preparation is rejected at
// the coordinator so its confirmation can never commit it, the question is forgotten and
// nothing reaches the application. "Start over" begins the same goal again from nothing.
func TestConvCancelRestart(t *testing.T) {
	t.Run("cancel drops a pending confirmation", func(t *testing.T) {
		for _, c := range []struct{ lang, say string }{{"en", "never mind"}, {"it", "annulla"}} {
			t.Run(c.lang, func(t *testing.T) {
				w := newWorld(t)
				sc := receptionScope()
				prep := w.bookLuca(sc, c.lang)
				proposals := w.proposals()

				res := w.say(sc, c.lang, c.say)

				wantAct(t, res, "acknowledge_cancel")
				if w.proposals() != proposals {
					t.Fatal("an exact cancel phrase reached the model")
				}
				if !hasTransition(res, dialogue.TransitionCancelled) {
					t.Fatalf("transitions = %v", res.Trace.Transitions)
				}
				if states := w.prepStates(); len(states) != 1 || states[0] != "rejected" {
					t.Fatalf("preparation states = %v, want the one rejected", states)
				}
				st := w.session(sc).State
				if st.Goal != nil || len(st.Suspended) != 0 {
					t.Fatalf("the goal outlived its cancellation: %+v", st.Goal)
				}
				// The capsule remembers THAT it was cancelled and nothing of what it held.
				if len(st.Capsules) != 1 || st.Capsules[0].Outcome != sessions.OutcomeCancelled || st.Capsules[0].CapabilityID != "appointments.book" {
					t.Fatalf("capsules = %+v", st.Capsules)
				}
				if c := st.Capsules[0]; len(c.Facts) != 0 || c.ReceiptID != "" {
					t.Fatalf("the capsule of a cancelled goal kept a value or a receipt: %+v", c)
				}
				// The confirmation that was on screen is dead.
				old, err := w.eng.Turn(ctxb(), sc, dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: string(prep.Prepared.PreparationID), Token: prep.Confirmation}})
				if err != nil {
					t.Fatal(err)
				}
				if old.Receipt != nil || old.Code == "" {
					t.Fatalf("a cancelled preparation was confirmed: code %q, receipt %+v", old.Code, old.Receipt)
				}
				w.noCommits()
			})
		}
	})

	t.Run("after a cancel the user can ask again from nothing", func(t *testing.T) {
		w := newWorld(t)
		sc := receptionScope()
		w.bookLuca(sc, "en")
		w.say(sc, "en", "never mind")

		fresh := w.bookLuca(sc, "en")

		out := w.confirm(sc, fresh)
		wantAct(t, out, "report_outcome")
		if w.host.writes() != 1 {
			t.Fatalf("effects = %d", w.host.writes())
		}
	})

	t.Run("the client can decline a pending confirmation too", func(t *testing.T) {
		w := newWorld(t)
		sc := receptionScope()
		prep := w.bookLuca(sc, "en")
		proposals := w.proposals()

		res := w.turn(sc, dialogue.TurnInput{Reject: string(prep.Prepared.PreparationID)})

		wantAct(t, res, "acknowledge_cancel")
		if w.proposals() != proposals {
			t.Fatal("a client rejection reached the model")
		}
		if states := w.prepStates(); len(states) != 1 || states[0] != "rejected" {
			t.Fatalf("preparation states = %v", states)
		}
		w.noCommits()
	})

	t.Run("start over begins the goal again from nothing", func(t *testing.T) {
		for _, c := range []struct{ lang, say string }{{"en", "start over"}, {"it", "ricomincia"}} {
			t.Run(c.lang, func(t *testing.T) {
				w := newWorld(t)
				sc := receptionScope()
				prep := w.bookLuca(sc, c.lang)
				before := w.goal(sc)
				proposals := w.proposals()

				res := w.say(sc, c.lang, c.say)

				// The same request, with nothing collected: the engine asks for what it needs.
				wantAct(t, res, "ask_missing")
				if got := res.Directive.MissingFields; len(got) != 2 || got[0] != "patient" || got[1] != "slot" {
					t.Fatalf("missing = %v, want [patient slot]", got)
				}
				if w.proposals() != proposals {
					t.Fatal("an exact restart phrase reached the model")
				}
				g := w.goal(sc)
				if g.CapabilityID != "appointments.book" || g.ID == before.ID || len(g.Slots) != 0 || len(g.Entities) != 0 || g.PreparedID != "" {
					t.Fatalf("goal after restart = %+v", g)
				}
				if !hasTransition(res, dialogue.TransitionRestarted) {
					t.Fatalf("transitions = %v", res.Trace.Transitions)
				}
				// The preparation built on the old values is void.
				if states := w.prepStates(); len(states) != 1 || states[0] != "invalidated" {
					t.Fatalf("preparation states = %v", states)
				}
				old, err := w.eng.Turn(ctxb(), sc, dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: string(prep.Prepared.PreparationID), Token: prep.Confirmation}})
				if err != nil {
					t.Fatal(err)
				}
				if old.Receipt != nil {
					t.Fatal("a restarted goal's old preparation was confirmed")
				}
				w.noCommits()

				// And the goal goes on from the beginning.
				w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"})
				again := w.say(sc, c.lang, "Luca Ferri, 2026-10-14 10:30")
				wantAct(t, again, "request_confirmation")
			})
		}
	})
}

// ---- topic switch and resume ----

// A request for something else sets the goal aside with everything it collected, voiding
// any preparation (a confirmation must not be answerable while the user is elsewhere);
// "go back" brings it back and asks, or prepares afresh, exactly where it was. A lookup
// while the goal waits for an answer is a side trip, not a switch.
func TestConvTopicSwitchResume(t *testing.T) {
	t.Run("a prepared booking is set aside, its confirmation dies, and resuming prepares it afresh", func(t *testing.T) {
		for _, c := range []struct{ lang, other, back string }{
			{"en", "Which appointments are free on 2026-10-15?", "go back to the booking"},
			{"it", "Quali visite sono libere il 2026-10-15?", "torna alla prenotazione"},
		} {
			t.Run(c.lang, func(t *testing.T) {
				w := newWorld(t)
				sc := receptionScope()
				first := w.bookLuca(sc, c.lang)

				w.propose("availability.list", "task", map[string]any{"date": "2026-10-15"})
				away := w.say(sc, c.lang, c.other)

				wantAct(t, away, "present_result")
				if !hasTransition(away, dialogue.TransitionSuspended) {
					t.Fatalf("transitions = %v", away.Trace.Transitions)
				}
				st := w.session(sc).State
				if st.Goal.CapabilityID != "availability.list" || len(st.Suspended) != 1 || st.Suspended[0].CapabilityID != "appointments.book" {
					t.Fatalf("goal = %s, suspended = %+v", st.Goal.CapabilityID, st.Suspended)
				}
				// What the booking collected is kept; its preparation is not.
				if g := st.Suspended[0]; g.Slots["patient"].Value != pidLuca || g.Slots["slot"].Value != slot14at1030 || g.PreparedID != "" {
					t.Fatalf("suspended goal = %+v", g)
				}
				if states := w.prepStates(); len(states) != 1 || states[0] != "invalidated" {
					t.Fatalf("preparation states = %v, want the booking's voided", states)
				}
				// While the user is elsewhere the old confirmation commits nothing.
				stale, err := w.eng.Turn(ctxb(), sc, dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: string(first.Prepared.PreparationID), Token: first.Confirmation}})
				if err != nil {
					t.Fatal(err)
				}
				if stale.Receipt != nil {
					t.Fatal("a booking was confirmed while the user was looking at something else")
				}
				w.noCommits()

				proposals := w.proposals()
				back := w.say(sc, c.lang, c.back)

				// Resume is a rule, and it prepares the same booking again with a new token.
				wantAct(t, back, "request_confirmation")
				if w.proposals() != proposals {
					t.Fatal("resuming reached the model")
				}
				if !hasTransition(back, dialogue.TransitionResumed) {
					t.Fatalf("transitions = %v", back.Trace.Transitions)
				}
				if back.Prepared == nil || back.Prepared.PreparationID == first.Prepared.PreparationID || back.Confirmation.IsZero() {
					t.Fatal("resuming did not prepare the booking afresh")
				}
				if got := w.lastCall(adapters.OpPrepare, "appointments.book").Fields; !sameFields(got, map[string]any{"patient": pidLuca, "slot": slot14at1030}) {
					t.Fatalf("re-prepared with %v", got)
				}
				out := w.confirm(sc, back)
				wantAct(t, out, "report_outcome")
				if w.host.writes() != 1 || w.host.slot(slot14at1030).status != "booked" {
					t.Fatalf("effects = %d", w.host.writes())
				}
			})
		}
	})

	t.Run("a lookup while the goal waits for an answer is a side trip", func(t *testing.T) {
		w := newWorld(t)
		sc := receptionScope()
		w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri"})
		ask := w.say(sc, "en", "Please book Luca Ferri")
		wantAct(t, ask, "ask_missing")

		w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"})
		look := w.say(sc, "en", sayFreeEN)

		wantAct(t, look, "present_result")
		for _, want := range []string{dialogue.TransitionNested, dialogue.TransitionReturned} {
			if !hasTransition(look, want) {
				t.Fatalf("transitions = %v, want %s", look.Trace.Transitions, want)
			}
		}
		// The booking is the goal again, with its question still pending and nothing lost.
		g := w.goal(sc)
		if g.CapabilityID != "appointments.book" || g.Pending == nil || g.Pending.Field != "slot" || g.Slots["patient"].Value != "Luca Ferri" {
			t.Fatalf("goal = %+v", g)
		}
		if len(w.session(sc).State.Suspended) != 0 {
			t.Fatal("a side trip left a suspended goal")
		}
		// The user can answer the booking's question from the list that was just shown.
		w.propose("appointments.book", "task", map[string]any{"slot": "the first one"})
		done := w.say(sc, "en", "the first one")
		wantAct(t, done, "request_confirmation")
		if got := w.lastCall(adapters.OpPrepare, "appointments.book").Fields["slot"]; got != slot14at0900 {
			t.Fatalf("prepared slot = %v, want the first one listed", got)
		}
	})

	t.Run("a switch to a consequential request keeps the interrupted goal for later", func(t *testing.T) {
		w := newWorld(t)
		sc := receptionScope()
		w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri"})
		w.say(sc, "en", "Please book Luca Ferri")

		w.propose("patients.update_contact", "task", map[string]any{"patient": "Luca Ferri", "email": "luca.ferri@example.test"})
		other := w.say(sc, "en", "Update the contact details of Luca Ferri: the new email is luca.ferri@example.test")

		wantAct(t, other, "report_outcome") // level 1, no separate confirmation: prepared, then committed
		if !hasTransition(other, dialogue.TransitionSuspended) {
			t.Fatalf("transitions = %v", other.Trace.Transitions)
		}
		if got := w.host.patient(pidLuca).email; got != "luca.ferri@example.test" {
			t.Fatalf("the contact change was not applied: %q", got)
		}

		back := w.say(sc, "en", "go back to the booking")

		wantAct(t, back, "ask_missing")
		if got := back.Directive.MissingFields; len(got) != 1 || got[0] != "slot" {
			t.Fatalf("missing = %v, want the question it was waiting on", got)
		}
		if g := w.goal(sc); g.CapabilityID != "appointments.book" || g.Slots["patient"].Value != "Luca Ferri" {
			t.Fatalf("goal = %+v", g)
		}
		w.propose("appointments.book", "task", map[string]any{"slot": "2026-10-14 10:30"})
		done := w.say(sc, "en", "2026-10-14 10:30")
		wantAct(t, done, "request_confirmation")
	})
}
