package agui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// sampleFile is a resolved attachment of the user's message.
func sampleFile(t testing.TB) conversation.FileRef {
	t.Helper()
	var f conversation.FileRef
	doc := `{"file_id":"fil_01J0000000000000000000000A","name":"scan.png","mime_type":"image/png","size_bytes":2048,
	"owner":"usr1","origin":"upload","permissions":["read"],"previewable":true,"expires_at":"2026-10-10T00:00:00Z"}`
	if err := json.Unmarshal([]byte(doc), &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func clientState(st sessions.State) *ClientState {
	c := ProjectState(st)
	return &c
}

func goalState(phase string, mod ...func(*sessions.Goal)) sessions.State {
	g := &sessions.Goal{
		CapabilityID: "appointments.book", Phase: phase,
		Slots: map[string]sessions.Slot{"doctor": {Value: "Dr. Rossi", Origin: sessions.OriginUserTurn}},
	}
	for _, m := range mod {
		m(g)
	}
	return sessions.State{Goal: g}
}

// scenarios are the turns the gateway publishes, one per behaviour of the mapping.
func scenarios(t testing.TB) []scenario {
	t.Helper()
	var out []scenario
	add := func(name string, mod func(*RunContext), build ...func(seq int64) conversation.TurnEvent) {
		rc := newRC()
		if mod != nil {
			mod(&rc)
		}
		out = append(out, scenario{name: name, rc: rc, events: seqed(t, build...)})
	}
	type B = func(seq int64) conversation.TurnEvent
	ev := func(name string, mutate ...func(map[string]any)) B {
		return func(seq int64) conversation.TurnEvent { return tev(t, name, seq, mutate...) }
	}
	blk := func(example string) B { return func(seq int64) conversation.TurnEvent { return bev(t, example, seq) } }
	tok := func(text string) B { return func(seq int64) conversation.TurnEvent { return token(t, seq, text) } }
	dn := func(reason, prep string) B {
		return func(seq int64) conversation.TurnEvent { return done(t, seq, reason, prep) }
	}
	prog := func(stage, state string, extra map[string]any) B {
		return func(seq int64) conversation.TurnEvent { return progress(t, seq, stage, state, extra) }
	}
	setAct := func(a string) func(map[string]any) {
		return func(d map[string]any) { d["directive"].(map[string]any)["act"] = a }
	}

	add("plain answer", nil,
		prog("interpreting", "completed", nil), prog("generating", "completed", nil),
		ev("directive", setAct("present_result")), tok("Dr. Rossi is free "), tok("on Tuesday."), dn("completed", ""))
	add("clarification", nil,
		ev("directive"), ev("clarification"), tok("Which time suits you?"), dn("awaiting_input", ""))
	add("read with citations", nil,
		prog("retrieving", "completed", nil), ev("directive", setAct("present_result")), ev("citation"), ev("citation"),
		tok("Cancellations are free up to 24 hours before."), dn("completed", ""))
	add("prepare and interrupt", nil,
		prog("preparing", "completed", nil), ev("directive", setAct("request_confirmation")), ev("prepared-action"),
		blk("confirmation-panel"), tok("Please confirm the booking."), dn("awaiting_confirmation", testPrep))
	add("resume approve", func(rc *RunContext) {
		rc.Kind, rc.Resume = RunKindResume, &ResumeRef{PreparationID: testPrep, Approved: true}
	},
		prog("committing", "started", nil), prog("committing", "completed", nil), ev("directive", setAct("report_outcome")),
		ev("receipt"), tok("Your appointment is booked."), dn("completed", ""))
	add("resume reject", func(rc *RunContext) {
		rc.Kind, rc.Resume = RunKindResume, &ResumeRef{PreparationID: testPrep, Approved: false}
	},
		ev("directive", setAct("acknowledge_cancel")), tok("Okay, nothing was booked."), dn("completed", ""))
	add("resume that fails", func(rc *RunContext) {
		rc.Kind, rc.Resume = RunKindResume, &ResumeRef{PreparationID: testPrep, Approved: true}
	},
		ev("error-unavailable"), tok("The booking system is not available."), dn("failed", ""))
	add("refusal", nil,
		ev("error-permission-denied"), tok("You may not do that."), dn("failed", ""))
	add("progress activity", nil,
		prog("queued", "started", map[string]any{"job_id": "job_01J0000000000000000000000A"}),
		prog("queued", "progress", map[string]any{"percent": 40, "job_id": "job_01J0000000000000000000000A"}),
		prog("queued", "progress", map[string]any{"percent": 80, "job_id": "job_01J0000000000000000000000A"}),
		prog("queued", "completed", map[string]any{"job_id": "job_01J0000000000000000000000A"}),
		ev("progress-tool-call"), dn("completed", ""))
	add("blocks", nil,
		ev("directive", setAct("present_result")), blk("markdown"), blk("code"), blk("table"), blk("chart-bar"), blk("diagram"),
		blk("citation-list"), blk("action-card"), blk("clarification-choices"), blk("confirmation-panel"), dn("completed", ""))
	add("step-up action", nil,
		ev("directive", setAct("request_step_up")), ev("prepared-action", func(d map[string]any) {
			d["prepared_action"].(map[string]any)["confirmation"].(map[string]any)["mode"] = "step_up"
		}), tok("Please continue in the application."), dn("completed", ""))
	add("cancelled", nil, ev("directive", setAct("acknowledge_cancel")), tok("Cancelled."), dn("cancelled", ""))
	add("attachments and state", func(rc *RunContext) {
		rc.Attachments = []conversation.FileRef{sampleFile(t)}
		rc.StateBefore = clientState(goalState("open"))
		rc.StateAfter = clientState(goalState("prepared", func(g *sessions.Goal) {
			g.PreparedID = testPrep
			g.Slots["slot_start"] = sessions.Slot{Value: "10:30", Origin: sessions.OriginChoice}
		}))
	},
		ev("directive", setAct("present_result")), tok("Got your file."), dn("completed", ""))
	add("suggestions", func(rc *RunContext) {
		rc.Catalog = catalog()
		rc.MaxSuggestions = 3
	},
		ev("directive", func(d map[string]any) {
			dd := d["directive"].(map[string]any)
			dd["act"] = "present_result"
			delete(dd, "next_question")
			dd["missing_fields"] = []any{}
			dd["permitted_next_actions"] = []any{map[string]any{"action": "invoke_capability", "capability_id": "appointments.cancel"}}
		}), tok("Here you go."), dn("completed", ""))
	add("state patch mode", func(rc *RunContext) {
		rc.StateMode = StatePatchMode
		rc.StateBefore = clientState(sessions.State{})
		rc.StateAfter = clientState(goalState("open"))
	}, ev("directive", setAct("present_result")), tok("Hello."), dn("completed", ""))
	return out
}

func TestTurnEventsMapToTheAGUILifecycle(t *testing.T) {
	for _, sc := range scenarios(t) {
		t.Run(sc.name, func(t *testing.T) {
			em := runAll(t, sc.rc, sc.events)
			validateAll(t, em)
			ty := types(em)
			if ty[0] != RunStarted {
				t.Fatalf("a run opens with RUN_STARTED, got %v", ty)
			}
			last := ty[len(ty)-1]
			if last != RunFinished && last != RunError {
				t.Fatalf("a run ends with RUN_FINISHED or RUN_ERROR, got %v", ty)
			}
			// Every event carries the seq of the TurnEvent it came from, never decreasing: it is the resume cursor.
			prev := int64(0)
			for _, e := range em {
				if e.Seq < prev {
					t.Fatalf("seq goes backwards: %v", ty)
				}
				prev = e.Seq
			}
			if em[0].Event.(RunStartedEvent).ThreadID != testThread || em[0].Event.(RunStartedEvent).RunID != testRun {
				t.Fatalf("RUN_STARTED does not carry the thread and run ids: %+v", em[0].Event)
			}
		})
	}

	// The specific mappings the protocol and the master plan (15.6) name.
	byName := map[string][]Emitted{}
	for _, sc := range scenarios(t) {
		byName[sc.name] = runAll(t, sc.rc, sc.events)
	}
	finish := func(name string) RunFinishedEvent {
		em := byName[name]
		return em[len(em)-1].Event.(RunFinishedEvent)
	}

	t.Run("streamed text", func(t *testing.T) {
		var sb strings.Builder
		var starts, ends int
		for _, e := range byName["plain answer"] {
			switch v := e.Event.(type) {
			case TextMessageStartEvent:
				starts++
				if v.Role != "assistant" {
					t.Fatalf("role %q", v.Role)
				}
			case TextMessageContentEvent:
				sb.WriteString(v.Delta)
			case TextMessageEndEvent:
				ends++
			}
		}
		if sb.String() != "Dr. Rossi is free on Tuesday." || starts != 1 || ends != 1 {
			t.Fatalf("text %q, %d starts, %d ends", sb.String(), starts, ends)
		}
		if o := finish("plain answer").Outcome; o == nil || o.Type != "success" {
			t.Fatalf("outcome %+v", o)
		}
	})
	t.Run("steps bracket the stages", func(t *testing.T) {
		var got []string
		for _, e := range byName["plain answer"] {
			switch v := e.Event.(type) {
			case StepStartedEvent:
				got = append(got, "start:"+v.StepName)
			case StepFinishedEvent:
				got = append(got, "end:"+v.StepName)
			}
		}
		want := []string{"start:interpreting", "end:interpreting", "start:generating", "end:generating"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("steps %v, want %v", got, want)
		}
	})
	t.Run("the directive is custom data", func(t *testing.T) {
		var found bool
		for _, e := range byName["plain answer"] {
			if c, ok := e.Event.(CustomEvent); ok && c.Name == DirectiveName {
				found = true
			}
		}
		if !found {
			t.Fatal("no alixai.directive custom event")
		}
	})
	t.Run("prepare ends the run with an interrupt", func(t *testing.T) {
		o := finish("prepare and interrupt").Outcome
		if o == nil || o.Type != "interrupt" || len(o.Interrupts) != 1 {
			t.Fatalf("outcome %+v", o)
		}
		it := o.Interrupts[0]
		if it.ID != testPrep || it.Reason != "confirmation_required" || it.ToolCallID != ConfirmCallID(testPrep) {
			t.Fatalf("interrupt %+v", it)
		}
		// The confirmation is a UI tool call that carries the exact effects.
		var args string
		var start, end bool
		for _, e := range byName["prepare and interrupt"] {
			switch v := e.Event.(type) {
			case ToolCallStartEvent:
				start = v.ToolCallName == ConfirmToolName && v.ToolCallID == it.ToolCallID
			case ToolCallArgsEvent:
				args = v.Delta
			case ToolCallEndEvent:
				end = true
			}
		}
		if !start || !end {
			t.Fatal("the confirmation tool call is not complete")
		}
		var view struct {
			PreparationID string `json:"preparationId"`
			Effects       []struct {
				Description string `json:"description"`
			} `json:"effects"`
			RiskLevel int `json:"riskLevel"`
		}
		if err := json.Unmarshal([]byte(args), &view); err != nil || view.PreparationID != testPrep || len(view.Effects) != 1 ||
			!strings.Contains(view.Effects[0].Description, "Dr. Rossi") || view.RiskLevel != 2 {
			t.Fatalf("tool call arguments %s (%v)", args, err)
		}
		md, _ := json.Marshal(it.Metadata)
		if !strings.Contains(string(md), "Create an appointment with Dr. Rossi") {
			t.Fatalf("the interrupt does not carry the exact effects: %s", md)
		}
	})
	t.Run("an approved resume answers the confirmation call with the receipt", func(t *testing.T) {
		var res *ToolCallResultEvent
		for _, e := range byName["resume approve"] {
			if v, ok := e.Event.(ToolCallResultEvent); ok {
				res = &v
			}
		}
		if res == nil || res.ToolCallID != ConfirmCallID(testPrep) || !strings.Contains(res.Content, `"status":"committed"`) {
			t.Fatalf("tool result %+v", res)
		}
		if o := finish("resume approve").Outcome; o.Type != "success" {
			t.Fatalf("outcome %+v", o)
		}
	})
	t.Run("a rejected resume resolves the card as rejected", func(t *testing.T) {
		var content string
		for _, e := range byName["resume reject"] {
			if v, ok := e.Event.(ToolCallResultEvent); ok {
				content = v.Content
			}
		}
		if !strings.Contains(content, `"status":"rejected"`) {
			t.Fatalf("tool result %q", content)
		}
	})
	t.Run("a failed resume resolves the card as failed and ends with an error", func(t *testing.T) {
		em := byName["resume that fails"]
		var content string
		for _, e := range em {
			if v, ok := e.Event.(ToolCallResultEvent); ok {
				content = v.Content
			}
		}
		if !strings.Contains(content, `"status":"failed"`) || !strings.Contains(content, "unavailable") {
			t.Fatalf("tool result %q", content)
		}
		if _, ok := em[len(em)-1].Event.(RunErrorEvent); !ok {
			t.Fatalf("the run ends with %v", types(em))
		}
	})
	t.Run("an error is terminal and follows the text that explains it", func(t *testing.T) {
		em := byName["refusal"]
		ty := types(em)
		// The error TurnEvent comes before the explaining text; RUN_ERROR must come after it, and last.
		if ty[len(ty)-1] != RunError || ty[len(ty)-2] != TextMessageEnd {
			t.Fatalf("order %v", ty)
		}
		re := em[len(em)-1].Event.(RunErrorEvent)
		if re.Code != "permission_denied" || !strings.Contains(re.Message, "may not") {
			t.Fatalf("%+v", re)
		}
		for _, e := range em {
			if f, ok := e.Event.(RunFinishedEvent); ok {
				t.Fatalf("a failed run also finished: %+v", f)
			}
		}
	})
	t.Run("citations are one activity that grows by deltas", func(t *testing.T) {
		var snaps, deltas int
		for _, e := range byName["read with citations"] {
			switch v := e.Event.(type) {
			case ActivitySnapshotEvent:
				if v.ActivityType == ActivityCitations {
					snaps++
				}
			case ActivityDeltaEvent:
				if v.ActivityType == ActivityCitations && v.Patch[0].Path == "/citations/-" {
					deltas++
				}
			}
		}
		if snaps != 1 || deltas != 1 {
			t.Fatalf("%d snapshots, %d deltas", snaps, deltas)
		}
	})
	t.Run("progress becomes one activity updated by deltas", func(t *testing.T) {
		var deltas []PatchOp
		var snaps int
		for _, e := range byName["progress activity"] {
			switch v := e.Event.(type) {
			case ActivitySnapshotEvent:
				if v.ActivityType == ActivityProgress {
					snaps++
				}
			case ActivityDeltaEvent:
				deltas = append(deltas, v.Patch...)
			}
		}
		if snaps != 2 { // queued and tool_call
			t.Fatalf("%d progress snapshots", snaps)
		}
		if len(deltas) < 3 || deltas[0].Op != "replace" || deltas[0].Path != "/state" {
			t.Fatalf("deltas %+v", deltas)
		}
	})
	t.Run("blocks: text for markdown and code, activities for the structured ones", func(t *testing.T) {
		var act []string
		var text strings.Builder
		for _, e := range byName["blocks"] {
			switch v := e.Event.(type) {
			case ActivitySnapshotEvent:
				act = append(act, v.ActivityType)
			case TextMessageContentEvent:
				text.WriteString(v.Delta)
			}
		}
		want := []string{"alixai.block.table", "alixai.block.chart", "alixai.block.diagram", "alixai.block.citation_list",
			"alixai.block.action_card", "alixai.block.clarification_choices", "alixai.block.confirmation_panel"}
		if !reflect.DeepEqual(act, want) {
			t.Fatalf("activities %v, want %v", act, want)
		}
		if !strings.Contains(text.String(), "```") {
			t.Fatalf("the code block is not fenced: %q", text.String())
		}
	})
	t.Run("a step-up action hands over to the application and is not an interrupt", func(t *testing.T) {
		var name string
		for _, e := range byName["step-up action"] {
			if v, ok := e.Event.(ToolCallStartEvent); ok {
				name = v.ToolCallName
			}
		}
		if name != ContinueToolName {
			t.Fatalf("tool %q", name)
		}
		if o := finish("step-up action").Outcome; o.Type != "success" {
			t.Fatalf("outcome %+v", o)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		if o := finish("cancelled").Outcome; o.Type != "cancelled" {
			t.Fatalf("outcome %+v", o)
		}
	})
	t.Run("attachments are an activity with previews", func(t *testing.T) {
		var content string
		for _, e := range byName["attachments and state"] {
			if v, ok := e.Event.(ActivitySnapshotEvent); ok && v.ActivityType == ActivityAttachments {
				content = string(mustJSON(t, v.Content))
			}
		}
		if !strings.Contains(content, `"previewable":true`) || !strings.Contains(content, "fil_01J0000000000000000000000A") ||
			strings.Contains(content, "usr1") || strings.Contains(content, "permissions") {
			t.Fatalf("attachments activity %s", content)
		}
	})
	t.Run("state: a snapshot first, then a delta that turns it into the final state", func(t *testing.T) {
		em := byName["attachments and state"]
		var snap StateSnapshotEvent
		var delta StateDeltaEvent
		var sawSnap, sawDelta bool
		for _, e := range em {
			switch v := e.Event.(type) {
			case StateSnapshotEvent:
				snap, sawSnap = v, true
			case StateDeltaEvent:
				delta, sawDelta = v, true
			}
		}
		if !sawSnap || !sawDelta {
			t.Fatalf("snapshot %v, delta %v: %v", sawSnap, sawDelta, types(em))
		}
		before := mustJSON(t, snap.Snapshot)
		var doc any
		if err := json.Unmarshal(before, &doc); err != nil {
			t.Fatal(err)
		}
		got, err := applyPatch(doc, delta.Delta)
		if err != nil {
			t.Fatal(err)
		}
		wantDoc, _ := (*clientState(goalState("prepared", func(g *sessions.Goal) {
			g.PreparedID = testPrep
			g.Slots["slot_start"] = sessions.Slot{Value: "10:30", Origin: sessions.OriginChoice}
		}))).document()
		var want any
		if err := json.Unmarshal(mustJSON(t, wantDoc), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("snapshot + delta = %s\nwant %s", mustJSON(t, got), mustJSON(t, want))
		}
	})
	t.Run("state in patch mode never replaces the client's own state", func(t *testing.T) {
		for _, e := range byName["state patch mode"] {
			if v, ok := e.Event.(StateSnapshotEvent); ok {
				t.Fatalf("a snapshot would replace the client's own members: %+v", v)
			}
			if v, ok := e.Event.(StateDeltaEvent); ok {
				for _, op := range v.Delta {
					if !strings.HasPrefix(op.Path, "/alixai") {
						t.Fatalf("patch outside /alixai: %+v", op)
					}
				}
			}
		}
	})
}

// TestMappingIsDeterministicAndResumable checks the property that makes reconnect work: mapping the buffered TurnEvents
// again gives the same bytes, so a client that has everything up to a cursor is served the events after it.
func TestMappingIsDeterministicAndResumable(t *testing.T) {
	for _, sc := range scenarios(t) {
		t.Run(sc.name, func(t *testing.T) {
			first := runAll(t, sc.rc, sc.events)
			// The caller keeps the RunContext with the turn as JSON and re-maps from it on a reconnect: what comes back
			// from storage must give the same bytes as the context the live run had (the catalog, the state, the files).
			var stored RunContext
			if err := json.Unmarshal(mustJSON(t, sc.rc), &stored); err != nil {
				t.Fatal(err)
			}
			second := runAll(t, stored, sc.events)
			if len(first) != len(second) {
				t.Fatalf("%d events, then %d", len(first), len(second))
			}
			for i := range first {
				if string(first[i].Data) != string(second[i].Data) || first[i].Seq != second[i].Seq {
					t.Fatalf("event %d differs:\n%s\n%s", i, first[i].Data, second[i].Data)
				}
			}
			// A reconnect at every cursor: the client that had events up to seq c is sent the ones after it, and what it
			// holds plus what it is sent is the whole stream, once each.
			for cursor := int64(-1); cursor < int64(len(sc.events)); cursor++ {
				var held, sent []string
				for _, e := range second {
					if e.Seq <= cursor {
						held = append(held, string(e.Data))
					} else {
						sent = append(sent, string(e.Data))
					}
				}
				var all []string
				for _, e := range first {
					all = append(all, string(e.Data))
				}
				if !reflect.DeepEqual(append(held, sent...), all) {
					t.Fatalf("cursor %d: held+sent is not the stream", cursor)
				}
			}
		})
	}
}

func TestAbortedStreamEndsWithARunErrorAndClosesWhatIsOpen(t *testing.T) {
	events := seqed(t,
		func(s int64) conversation.TurnEvent { return progress(t, s, "interpreting", "started", nil) },
		func(s int64) conversation.TurnEvent { return token(t, s, "Dr. Ros") },
	)
	m, err := NewMapper(newRC())
	if err != nil {
		t.Fatal(err)
	}
	var em []Emitted
	for _, ev := range events {
		got, err := m.Map(ev)
		if err != nil {
			t.Fatal(err)
		}
		em = append(em, got...)
	}
	em = append(em, m.Abort(1, "aborted", "the turn was abandoned")...)
	validateAll(t, em)
	if !m.Finished() || len(m.Abort(1, "x", "y")) != 0 {
		t.Fatal("an aborted run stays finished and is not aborted twice")
	}
	if _, err := m.Map(token(t, 2, "more")); err == nil {
		t.Fatal("an event after the end of the run was accepted")
	}
}

func TestMapperRefusesAnEventOfAnotherTurn(t *testing.T) {
	m, _ := NewMapper(newRC())
	if _, err := m.Map(token(t, 0, "a")); err != nil {
		t.Fatal(err)
	}
	other := tev(t, "token", 1, func(d map[string]any) { d["turn_id"] = "trn_01J0000000000000000000000B" })
	if _, err := m.Map(other); err == nil {
		t.Fatal("an event of another turn was mapped into the run")
	}
}

func TestDoneAwaitingConfirmationWithoutTheConfirmationIsARunError(t *testing.T) {
	// The done event promises a confirmation the run never showed: finishing normally would leave the user nothing to
	// answer, so the run fails.
	em := runAll(t, newRC(), seqed(t,
		func(s int64) conversation.TurnEvent { return token(t, s, "Please confirm.") },
		func(s int64) conversation.TurnEvent { return done(t, s, "awaiting_confirmation", testPrep) },
	))
	validateAll(t, em)
	if _, ok := em[len(em)-1].Event.(RunErrorEvent); !ok {
		t.Fatalf("ends with %v", types(em))
	}
}

func TestConnectRunReplaysHistoryAndState(t *testing.T) {
	rc := newRC()
	rc.Kind = RunKindConnect
	rc.StateBefore = clientState(goalState("open"))
	em, err := Connect(rc, []HistoryTurn{
		{TurnID: "trn_1", Role: "user", Text: "hi"}, {TurnID: "trn_1", Role: "assistant", Text: "hello"},
		{TurnID: "trn_2", Role: "system", Text: "ignored"},
	})
	if err != nil {
		t.Fatal(err)
	}
	validateAll(t, em)
	want := []EventType{RunStarted, MessagesSnapshot, StateSnapshot, RunFinished}
	if !reflect.DeepEqual(types(em), want) {
		t.Fatalf("%v", types(em))
	}
	msgs := em[1].Event.(MessagesSnapshotEvent).Messages
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Content != "hello" {
		t.Fatalf("%+v", msgs)
	}
}
