package agui

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/sessions"
)

// StateKey is the one member of the shared AG-UI state that AlixAI owns. The state document of a thread is
// {"alixai": ClientState}; whatever else the client keeps in its own state is not touched (see StateMode).
const StateKey = "alixai"

// StateMode says how the client's shared state is updated.
type StateMode string

const (
	// StateSnapshotMode replaces the state with {"alixai": ...}. It is the mode when the client sent no state, or a
	// state that is not an object: there is nothing of the client's to lose.
	StateSnapshotMode StateMode = "snapshot"
	// StatePatchMode only adds and changes "/alixai" with JSON Patch operations, so the members of a state object the
	// client keeps for itself survive. It is the mode when the client sent an object as its state.
	StatePatchMode StateMode = "patch"
)

// ClientState is the client-safe projection of a session's state: what the user may be shown of where the
// conversation stands. It is an allow-list: a field is here because a client needs it, never because the session has
// it. Slot VALUES, entity ids, results, capsules, permissions and the roles of the token are not here and cannot be
// added by accident, because the projection is built field by field (ProjectState), not by encoding the session.
type ClientState struct {
	// Pending is what the active goal waits for, or null when there is no goal.
	Pending *PendingView `json:"pending"`
}

// PendingView is the pending state of the active goal, like the sessions API's pending object.
type PendingView struct {
	// Kind is confirmation (a prepared action awaits the user), clarification (a question awaits an answer) or goal.
	Kind         string `json:"kind"`
	CapabilityID string `json:"capabilityId"`
	Phase        string `json:"phase"`
	// CollectedFields are the fields the goal has a value for and where each came from. The values stay in the session.
	CollectedFields []FieldView   `json:"collectedFields"`
	MissingFields   []string      `json:"missingFields"`
	Question        *QuestionView `json:"question,omitempty"`
	// PreparationID is the preparation that awaits confirmation; the secret that confirms it is never part of state.
	PreparationID string `json:"preparationId,omitempty"`
	ExpiresAt     string `json:"expiresAt,omitempty"`
}

// FieldView is one collected field: its name and the origin of its value.
type FieldView struct {
	Name   string `json:"name"`
	Origin string `json:"origin"`
}

// QuestionView is the clarification the core asked.
type QuestionView struct {
	Field      string       `json:"field"`
	AnswerKind string       `json:"answerKind"`
	Choices    []ChoiceView `json:"choices,omitempty"`
}

// ChoiceView is one offered choice: an id and a label, never its value.
type ChoiceView struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ProjectState builds the ClientState of a session state. Nothing is copied except the fields named below.
func ProjectState(st sessions.State) ClientState {
	g := st.Goal
	if g == nil {
		return ClientState{}
	}
	p := &PendingView{
		Kind:            "goal",
		CapabilityID:    g.CapabilityID,
		Phase:           g.Phase,
		CollectedFields: make([]FieldView, 0, len(g.Slots)),
		MissingFields:   []string{},
	}
	for name, slot := range g.Slots {
		p.CollectedFields = append(p.CollectedFields, FieldView{Name: name, Origin: string(slot.Origin)})
	}
	sort.Slice(p.CollectedFields, func(i, j int) bool { return p.CollectedFields[i].Name < p.CollectedFields[j].Name })
	if last := st.Last; last != nil {
		p.MissingFields = append(p.MissingFields, last.MissingFields...)
	}
	exp := g.ExpiresAt
	if c := g.Pending; c != nil {
		p.Kind = "clarification"
		q := &QuestionView{Field: c.Field, AnswerKind: string(c.Kind)}
		for _, ch := range c.Choices {
			q.Choices = append(q.Choices, ChoiceView{ID: string(ch.ChoiceID), Label: ch.Label})
		}
		p.Question = q
	}
	if g.PreparedID != "" {
		p.PreparationID = g.PreparedID
		if !g.Confirmed {
			p.Kind = "confirmation"
		}
		if !g.PreparedExpiresAt.IsZero() {
			exp = g.PreparedExpiresAt
		}
	}
	if !exp.IsZero() {
		p.ExpiresAt = exp.UTC().Format(time.RFC3339)
	}
	return ClientState{Pending: p}
}

// document is the shared-state document {"alixai": state} as plain JSON values.
func (s ClientState) document() (map[string]any, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return map[string]any{StateKey: v}, nil
}

// InitialState is the first state event of a run: a STATE_SNAPSHOT of {"alixai": state}, or, in patch mode, a STATE_DELTA
// that adds "/alixai" and leaves the client's own members alone.
func InitialState(mode StateMode, s ClientState) (Event, error) {
	doc, err := s.document()
	if err != nil {
		return nil, err
	}
	if mode == StatePatchMode {
		return NewStateDelta([]PatchOp{{Op: "add", Path: "/" + StateKey, Value: doc[StateKey]}}), nil
	}
	return NewStateSnapshot(doc), nil
}

// StateChange is a STATE_DELTA that turns the state a client holds (before) into after, or nil when nothing changed.
// Both are the AlixAI part only; every path starts with "/alixai". Objects are compared member by member; arrays and
// scalars are replaced whole.
func StateChange(before, after ClientState) (Event, error) {
	b, err := before.document()
	if err != nil {
		return nil, err
	}
	a, err := after.document()
	if err != nil {
		return nil, err
	}
	var ops []PatchOp
	diffValue("/"+StateKey, b[StateKey], a[StateKey], &ops)
	if len(ops) == 0 {
		return nil, nil
	}
	return NewStateDelta(ops), nil
}

func diffValue(path string, before, after any, ops *[]PatchOp) {
	bm, bok := before.(map[string]any)
	am, aok := after.(map[string]any)
	if bok && aok {
		keys := map[string]bool{}
		for k := range bm {
			keys[k] = true
		}
		for k := range am {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			child := path + "/" + escapePointer(k)
			bv, inB := bm[k]
			av, inA := am[k]
			switch {
			case inB && !inA:
				*ops = append(*ops, PatchOp{Op: "remove", Path: child})
			case !inB && inA:
				*ops = append(*ops, PatchOp{Op: "add", Path: child, Value: av})
			default:
				diffValue(child, bv, av, ops)
			}
		}
		return
	}
	if !reflect.DeepEqual(before, after) {
		*ops = append(*ops, PatchOp{Op: "replace", Path: path, Value: after})
	}
}

// escapePointer escapes a member name as a JSON Pointer token (RFC 6901).
func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
