package agui

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func catalog() []CapabilityHint {
	return []CapabilityHint{
		{ID: "appointments.cancel", Title: "Cancel an appointment", Examples: []ExampleHint{
			{Language: "en", Utterance: "Cancel my appointment on Friday"}, {Language: "it", Utterance: "Annulla il mio appuntamento di venerdi"}}},
		{ID: "appointments.reschedule", Title: "Reschedule", Examples: []ExampleHint{{Language: "en-GB", Utterance: "Move my appointment to Monday"}}},
		{ID: "appointments.list", Title: "List appointments"},
		// A capability the directive does not name: the caller may not use it, and nothing may reveal that it exists.
		{ID: "billing.refund", Title: "Refund a payment", Examples: []ExampleHint{{Language: "en", Utterance: "Refund the last payment"}}},
	}
}

func suggestDirective(t testing.TB, mutate ...func(d map[string]any)) *conversation.DialogueDirective {
	t.Helper()
	ev := tev(t, "directive", 0, func(doc map[string]any) {
		d := doc["directive"].(map[string]any)
		d["act"] = "present_result"
		d["next_question"] = nil
		delete(d, "next_question")
		d["missing_fields"] = []any{}
		d["permitted_next_actions"] = []any{
			map[string]any{"action": "invoke_capability", "capability_id": "appointments.cancel", "label": "Cancel"},
			map[string]any{"action": "invoke_capability", "capability_id": "appointments.reschedule"},
			map[string]any{"action": "invoke_capability", "capability_id": "appointments.list"},
			map[string]any{"action": "cancel"},
			map[string]any{"action": "invoke_capability", "capability_id": "appointments.not_in_the_catalog"},
		}
		d["allowed_suggestions"] = []any{
			map[string]any{"choice_id": "c1", "label": "Tomorrow at 9", "value": map[string]any{"kind": "text", "value": "tomorrow 9"}},
		}
		for _, m := range mutate {
			m(d)
		}
	})
	d := ev.Value.(conversation.DirectiveEvent).Directive
	return &d
}

func TestSuggestionsAreDeterministicAndNeedNoModel(t *testing.T) {
	d := suggestDirective(t)
	want := []Suggestion{
		{Title: "Tomorrow at 9", Message: "Tomorrow at 9"},
		{Title: "Cancel an appointment", Message: "Cancel my appointment on Friday", CapabilityID: "appointments.cancel"},
		{Title: "Reschedule", Message: "Move my appointment to Monday", CapabilityID: "appointments.reschedule"},
		{Title: "List appointments", Message: "List appointments", CapabilityID: "appointments.list"},
	}

	t.Run("the host's choices first, then the capabilities the directive permits, with their examples", func(t *testing.T) {
		got := Suggest(d, "en", catalog(), 0)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("the same inputs always give the same list, whatever the order of the catalog", func(t *testing.T) {
		cat := catalog()
		for i := 0; i < 20; i++ {
			// Rotate and reverse the catalog: the order of the list comes from the directive, not from the catalog.
			cat = append(cat[1:], cat[0])
			if i%2 == 1 {
				for l, r := 0, len(cat)-1; l < r; l, r = l+1, r-1 {
					cat[l], cat[r] = cat[r], cat[l]
				}
			}
			if got := Suggest(d, "en", cat, 0); !reflect.DeepEqual(got, want) {
				t.Fatalf("round %d: %+v", i, got)
			}
		}
	})
	t.Run("the language of the turn picks the example", func(t *testing.T) {
		got := Suggest(d, "it-IT", catalog(), 0)
		if got[1].Message != "Annulla il mio appuntamento di venerdi" {
			t.Fatalf("%+v", got[1])
		}
		// No example in the language: the first example of the package; none at all: the title.
		got = Suggest(d, "fr", catalog(), 0)
		if got[1].Message != "Cancel my appointment on Friday" || got[3].Message != "List appointments" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a capability the directive does not permit is never suggested, and its existence is not revealed", func(t *testing.T) {
		for _, s := range Suggest(d, "en", catalog(), MaxSuggestionsCeiling) {
			if strings.Contains(strings.ToLower(s.Title+s.Message), "refund") || s.CapabilityID == "billing.refund" {
				t.Fatalf("an unpermitted capability was suggested: %+v", s)
			}
		}
		// Even when the directive names it, a capability that is not in the caller's catalog is not suggested.
		only := Suggest(d, "en", []CapabilityHint{catalog()[0]}, 0)
		if len(only) != 2 {
			t.Fatalf("%+v", only)
		}
	})
	t.Run("bounded and de-duplicated", func(t *testing.T) {
		if got := Suggest(d, "en", catalog(), 2); len(got) != 2 {
			t.Fatalf("%d suggestions with a limit of 2", len(got))
		}
		if got := Suggest(d, "en", catalog(), 1000); len(got) > MaxSuggestionsCeiling {
			t.Fatalf("%d suggestions", len(got))
		}
		dup := suggestDirective(t, func(dd map[string]any) {
			dd["allowed_suggestions"] = []any{
				map[string]any{"choice_id": "a", "label": "Same", "value": map[string]any{"kind": "text", "value": "1"}},
				map[string]any{"choice_id": "b", "label": "Same", "value": map[string]any{"kind": "text", "value": "2"}},
			}
		})
		if got := Suggest(dup, "en", nil, 0); len(got) != 1 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("text is one clean line", func(t *testing.T) {
		long := suggestDirective(t, func(dd map[string]any) {
			dd["allowed_suggestions"] = []any{map[string]any{"choice_id": "a", "label": "Line one\nline two\x00\t" + strings.Repeat("x", 500), "value": map[string]any{"kind": "text", "value": "1"}}}
		})
		got := Suggest(long, "en", nil, 0)
		if len(got) != 1 || strings.ContainsAny(got[0].Message, "\n\x00\t") || len([]rune(got[0].Message)) > suggestionMaxRunes {
			t.Fatalf("%q", got[0].Message)
		}
	})
	t.Run("a confirmation, a step-up, a refusal and a failure leave the user with the one thing asked of them", func(t *testing.T) {
		for _, act := range []string{"request_confirmation", "request_step_up", "refuse", "report_failure", "report_no_evidence"} {
			if got := Suggest(suggestDirective(t, func(dd map[string]any) { dd["act"] = act }), "en", catalog(), 0); got != nil {
				t.Errorf("%s: %+v", act, got)
			}
		}
		if Suggest(nil, "en", catalog(), 0) != nil {
			t.Error("a nil directive has suggestions")
		}
	})

	// There is nothing in the computation that could call a model: the package imports no provider, adapter, retrieval
	// or network package, and the run that sends the suggestions is the same pure mapping.
	t.Run("the package has no path to a model", TestTheMappingHasNoPathToAModel)
	t.Run("the run sends them as a custom event, the same every time", TestRunSendsSuggestions)
}

func TestRunSendsSuggestions(t *testing.T) {
	rc := newRC()
	rc.Catalog = catalog()
	rc.MaxSuggestions = 3
	events := seqed(t,
		func(s int64) conversation.TurnEvent {
			return tev(t, "directive", s, func(doc map[string]any) {
				d := doc["directive"].(map[string]any)
				d["act"] = "present_result"
				delete(d, "next_question")
				d["missing_fields"] = []any{}
				d["permitted_next_actions"] = []any{map[string]any{"action": "invoke_capability", "capability_id": "appointments.cancel"}}
			})
		},
		func(s int64) conversation.TurnEvent { return token(t, s, "Here you go.") },
		func(s int64) conversation.TurnEvent { return done(t, s, "completed", "") },
	)
	em := runAll(t, rc, events)
	validateAll(t, em)
	var got struct {
		Suggestions []Suggestion `json:"suggestions"`
	}
	idx := -1
	for i, e := range em {
		if c, ok := e.Event.(CustomEvent); ok && c.Name == SuggestionsName {
			idx = i
			if err := json.Unmarshal(mustJSON(t, c.Value), &got); err != nil {
				t.Fatal(err)
			}
		}
	}
	if idx < 0 || len(got.Suggestions) != 1 || got.Suggestions[0].CapabilityID != "appointments.cancel" {
		t.Fatalf("suggestions %+v in %v", got, types(em))
	}
	if _, ok := em[len(em)-1].Event.(RunFinishedEvent); !ok || idx != len(em)-2 {
		t.Fatalf("the suggestions are not the last thing before the run finishes: %v", types(em))
	}
	// Without a catalog there are only the host's own suggestions; with none of those, no event at all.
	rc.Catalog = nil
	for _, e := range runAll(t, rc, events) {
		if c, ok := e.Event.(CustomEvent); ok && c.Name == SuggestionsName {
			t.Fatalf("suggestions without anything to suggest: %+v", c)
		}
	}
	// And the same run twice gives the same bytes.
	a, b := runAll(t, rc, events), runAll(t, rc, events)
	for i := range a {
		if string(a[i].Data) != string(b[i].Data) {
			t.Fatalf("event %d differs", i)
		}
	}
	_ = fmt.Sprint
}

// ---- state ----

// applyPatch applies add, replace and remove operations to a decoded JSON document (the subset of RFC 6902 the mapper
// produces), failing the way a client's patch library does when a path does not exist.
func applyPatch(doc any, ops []PatchOp) (any, error) {
	for _, op := range ops {
		parts := strings.Split(strings.TrimPrefix(op.Path, "/"), "/")
		for i := range parts {
			parts[i] = strings.ReplaceAll(strings.ReplaceAll(parts[i], "~1", "/"), "~0", "~")
		}
		var set func(cur any, depth int) (any, error)
		set = func(cur any, depth int) (any, error) {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: %q is not an object", op.Op, strings.Join(parts[:depth], "/"))
			}
			key := parts[depth]
			if depth < len(parts)-1 {
				child, exists := m[key]
				if !exists {
					return nil, fmt.Errorf("%s %s: %q does not exist", op.Op, op.Path, key)
				}
				nc, err := set(child, depth+1)
				if err != nil {
					return nil, err
				}
				m[key] = nc
				return m, nil
			}
			_, exists := m[key]
			switch op.Op {
			case "add":
				m[key] = jsonRoundTrip(op.Value)
			case "replace":
				if !exists {
					return nil, fmt.Errorf("replace %s: the member does not exist", op.Path)
				}
				m[key] = jsonRoundTrip(op.Value)
			case "remove":
				if !exists {
					return nil, fmt.Errorf("remove %s: the member does not exist", op.Path)
				}
				delete(m, key)
			default:
				return nil, fmt.Errorf("unsupported op %s", op.Op)
			}
			return m, nil
		}
		var err error
		if doc, err = set(doc, 0); err != nil {
			return nil, err
		}
	}
	return doc, nil
}

func jsonRoundTrip(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// canaries are values that must never reach the client state: a slot value, an entity id, the roles and capabilities
// the token allowed, a result's label.
const (
	canarySlot  = "CANARY-SLOT-VALUE-4711"
	canaryRole  = "CANARY-ROLE-admin"
	canaryEnt   = "CANARY-ENTITY-ID-9"
	canaryLabel = "CANARY-RESULT-LABEL"
)

func canaryState() sessions.State {
	return sessions.State{
		Goal: &sessions.Goal{
			CapabilityID: "appointments.book", Phase: "open",
			Slots: map[string]sessions.Slot{
				"doctor":     {Value: canarySlot, Origin: sessions.OriginUserTurn},
				"patient_id": {Value: canaryEnt, Origin: sessions.OriginEntityResolution},
			},
			Pending:  &sessions.Clarification{Field: "slot_start", Kind: sessions.ClarifyChoice, Choices: nil},
			Entities: []sessions.ResolvedEntity{{Field: "patient_id", EntityType: "patient", EntityID: canaryEnt, Version: "7"}},
		},
		Last:        &sessions.DirectiveSummary{MissingFields: []string{"slot_start"}},
		Results:     []sessions.ResultSummary{{CapabilityID: "appointments.list", Items: []sessions.ResultItem{{EntityID: canaryEnt, Label: canaryLabel}}}},
		AllowedNext: []string{"appointments.cancel"},
		Perms:       &sessions.Permissions{Roles: []string{canaryRole}, Capabilities: []string{"billing.refund"}},
		Suspended:   []*sessions.Goal{{CapabilityID: "billing.refund", Slots: map[string]sessions.Slot{"amount": {Value: canarySlot}}}},
		Capsules:    []sessions.Capsule{{CapabilityID: "x", Facts: map[string]string{"k": canarySlot}}},
	}
}

// TestSEC_ClientStateIsAnAllowListProjection: the client-safe state holds field names and origins, never values, ids,
// roles, results or other goals, in a snapshot or in any delta.
func TestSEC_ClientStateIsAnAllowListProjection(t *testing.T) {
	st := canaryState()
	before := ProjectState(st)
	st.Goal.Phase = "prepared"
	st.Goal.PreparedID = testPrep
	st.Goal.Slots["slot_start"] = sessions.Slot{Value: canarySlot, Origin: sessions.OriginChoice}
	after := ProjectState(st)

	snap, err := InitialState(StateSnapshotMode, before)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := StateChange(before, after)
	if err != nil || delta == nil {
		t.Fatalf("%v %v", delta, err)
	}
	for _, ev := range []Event{snap, delta} {
		data, err := Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{canarySlot, canaryRole, canaryEnt, canaryLabel, "billing.refund", "appointments.cancel"} {
			if strings.Contains(string(data), canary) {
				t.Fatalf("%s leaked into %s", canary, data)
			}
		}
		if err := vendored(t).ValidateEvent(data); err != nil {
			t.Fatal(err)
		}
	}
	data := string(mustJSON(t, after))
	for _, want := range []string{`"doctor"`, `"patient_id"`, `"slot_start"`, `"origin":"choice"`, testPrep, `"kind":"confirmation"`} {
		if !strings.Contains(data, want) {
			t.Errorf("the projection lacks %s: %s", want, data)
		}
	}
	if (ProjectState(sessions.State{}) != ClientState{}) {
		t.Fatal("a session with no goal projects to something")
	}
}

func TestStateChangeIsAValidMinimalPatch(t *testing.T) {
	a := ProjectState(goalState("open"))
	b := ProjectState(goalState("prepared", func(g *sessions.Goal) {
		g.PreparedID = testPrep
		g.Slots["slot_start"] = sessions.Slot{Value: "x", Origin: sessions.OriginChoice}
	}))
	if ev, err := StateChange(a, a); ev != nil || err != nil {
		t.Fatalf("no change gave %v %v", ev, err)
	}
	for name, pair := range map[string][2]ClientState{
		"open to prepared": {a, b}, "prepared to open": {b, a}, "none to goal": {{}, a}, "goal to none": {a, {}},
	} {
		ev, err := StateChange(pair[0], pair[1])
		if err != nil || ev == nil {
			t.Fatalf("%s: %v %v", name, ev, err)
		}
		d0, _ := pair[0].document()
		got, err := applyPatch(jsonRoundTrip(d0), ev.(StateDeltaEvent).Delta)
		if err != nil {
			t.Fatalf("%s: the patch does not apply: %v", name, err)
		}
		d1, _ := pair[1].document()
		if !reflect.DeepEqual(got, jsonRoundTrip(d1)) {
			t.Fatalf("%s: %s, want %s", name, mustJSON(t, got), mustJSON(t, d1))
		}
		for _, op := range ev.(StateDeltaEvent).Delta {
			if !strings.HasPrefix(op.Path, "/alixai") {
				t.Fatalf("%s: a patch outside /alixai: %+v", name, op)
			}
		}
	}
}
