package sessions

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// SlotOrigin says where a collected slot value came from (master plan 11.3).
type SlotOrigin string

// Origins of a slot.
const (
	// OriginUserTurn: the model proposed it from the user's turn and it passed
	// deterministic validation.
	OriginUserTurn SlotOrigin = "user_turn"
	// OriginCorrection: the user corrected an earlier value.
	OriginCorrection SlotOrigin = "correction"
	// OriginEntityResolution: the host application resolved a mention to this ID.
	OriginEntityResolution SlotOrigin = "entity_resolution"
	// OriginChoice: the user selected one of the choices the core offered.
	OriginChoice SlotOrigin = "choice"
	// OriginDefault: the capability's declared default filled it; nobody said it.
	// A default never replaces a value a person supplied.
	OriginDefault SlotOrigin = "default"
)

// Valid reports whether o is one of the origins above.
func (o SlotOrigin) Valid() bool {
	switch o {
	case OriginUserTurn, OriginCorrection, OriginEntityResolution, OriginChoice, OriginDefault:
		return true
	}
	return false
}

// Slot is one collected field value. Value is a JSON scalar or a flat list of
// scalars (json.Number for numbers).
type Slot struct {
	Value  any        `json:"value"`
	Origin SlotOrigin `json:"origin"`
	// Type is the capability field type the value was validated against (for
	// example "string", "number", "date", "entity_ref"); empty when the writer did
	// not say. The value is checked against the package's field spec by the
	// dialogue engine, not here.
	Type string `json:"type,omitempty"`
	// TurnID is the turn that set the value.
	TurnID string `json:"turn_id,omitempty"`
}

// ClarificationKind is the kind of answer a pending clarification expects.
type ClarificationKind string

// Kinds of clarification, as the directive's question answer kinds.
const (
	ClarifyText   ClarificationKind = "text"
	ClarifyChoice ClarificationKind = "choice"
	ClarifyEntity ClarificationKind = "entity"
)

// Clarification is the question the core asked and the answer it waits for.
// Choices are the host's, relayed verbatim: selecting one sends its choice ID,
// never free text.
type Clarification struct {
	Field   string            `json:"field"`
	Kind    ClarificationKind `json:"kind"`
	Choices []act.Choice      `json:"choices,omitempty"`
	// EntityType and Version are set when the choices are candidate records the
	// host resolved a mention to: the entity type, and the data version the
	// resolution relied on.
	EntityType string `json:"entity_type,omitempty"`
	Version    string `json:"version,omitempty"`
	// AskedAt is when the question was first stored; Save sets it.
	AskedAt time.Time `json:"asked_at,omitzero"`
	// ExpiresAt is when the question lapses. Zero means it lives as long as its
	// goal; with Config.PendingTTL set, Save stamps it the first time the question
	// is stored. An answer that comes later is not applied to a stale question.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// ResolvedEntity is an entity the host application resolved for a field: its
// stable ID and the data version the resolution relied on.
type ResolvedEntity struct {
	Field      string `json:"field"`
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Version    string `json:"version,omitempty"`
}

// SuspendReason says why a goal is not the active one.
type SuspendReason string

// Reasons a goal is suspended.
const (
	// SuspendedTopicSwitch: the user moved to another request and may come back.
	SuspendedTopicSwitch SuspendReason = "topic_switch"
	// SuspendedNested: the goal is the parent of the active goal and continues when
	// that goal completes.
	SuspendedNested SuspendReason = "nested"
)

// Goal is what the session is currently trying to do.
type Goal struct {
	// ID identifies the goal inside its session; Save assigns one to a goal that
	// has none. Suspending, resuming and nesting address goals by it.
	ID string `json:"id,omitempty"`
	// ParentID is the suspended goal a nested goal returns to when it completes.
	ParentID string `json:"parent_id,omitempty"`
	// CapabilityID is the capability the goal concerns.
	CapabilityID string `json:"capability_id"`
	// Phase is capabilities.PhaseOpen while the goal is being filled in, then
	// PhasePrepared, PhaseSuccess or PhaseFailure.
	Phase string `json:"phase"`
	// Slots are the collected field values by field name.
	Slots map[string]Slot `json:"slots"`
	// Pending is the clarification the core is waiting for, if any.
	Pending *Clarification `json:"pending,omitempty"`
	// PreparedID is the preparation (internal/actions) that awaits a confirmation.
	// It is a reference: the preparation, its expiry and its state live there.
	PreparedID string `json:"prepared_id,omitempty"`
	// PreparedExpiresAt is the preparation's own expiry, copied so the session can
	// stop offering a confirmation that can no longer be given. Zero means the
	// writer did not record it; the coordinator's expiry is authoritative either way.
	PreparedExpiresAt time.Time `json:"prepared_expires_at,omitzero"`
	// Confirmed is true once the confirmation of PreparedID was accepted. If the
	// commit then proved not to have been applied, the same confirmation is not
	// asked for again; the commit is retried.
	Confirmed bool `json:"confirmed,omitempty"`
	// Entities are the resolved entities behind slots of type entity_ref.
	Entities  []ResolvedEntity `json:"entities,omitempty"`
	StartedAt time.Time        `json:"started_at"`
	UpdatedAt time.Time        `json:"updated_at"`
	// ExpiresAt is when an idle goal is discarded; each turn moves it. A suspended
	// goal has its own, set when it is suspended.
	ExpiresAt time.Time `json:"expires_at"`
	// SuspendedAt and SuspendReason are set while the goal is on the suspended
	// stack.
	SuspendedAt   time.Time     `json:"suspended_at,omitzero"`
	SuspendReason SuspendReason `json:"suspend_reason,omitempty"`
}

// Active returns the goal as the capability router reads it.
func (g *Goal) Active() *capabilities.ActiveGoal {
	if g == nil {
		return nil
	}
	return &capabilities.ActiveGoal{CapabilityID: g.CapabilityID, Phase: g.Phase}
}

// SlotValues returns the slot values by field name (a copy).
func (g *Goal) SlotValues() map[string]any {
	out := make(map[string]any, len(g.Slots))
	for k, s := range g.Slots {
		out[k] = s.Value
	}
	return out
}

// Question summarises the question of the last directive.
type Question struct {
	Field      string `json:"field"`
	AnswerKind string `json:"answer_kind"`
	// Choices are the choice IDs and labels offered (host text; untrusted).
	Choices []ChoiceSummary `json:"choices,omitempty"`
}

// ChoiceSummary is one offered choice, without its value.
type ChoiceSummary struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// DirectiveSummary is what the next turn needs to know of the last directive:
// the act, what was missing, the question that was asked. It holds no fact, no
// receipt and no identifier a response may not already state.
type DirectiveSummary struct {
	TurnID        string            `json:"turn_id"`
	Act           string            `json:"act"`
	CapabilityID  string            `json:"capability_id,omitempty"`
	MissingFields []string          `json:"missing_fields,omitempty"`
	InvalidFields []string          `json:"invalid_fields,omitempty"`
	Question      *Question         `json:"question,omitempty"`
	ReasonCode    contracts.ErrCode `json:"reason_code,omitempty"`
}

// ResultKind is what produced a deterministic result.
type ResultKind string

// Kinds of deterministic result.
const (
	ResultRead     ResultKind = "read"
	ResultValidate ResultKind = "validate"
	ResultPrepare  ResultKind = "prepare"
	ResultCommit   ResultKind = "commit"
)

// ResultItem is one record a deterministic result presented, in the order it was
// presented, so "the second one" can be resolved from state. Label is host text:
// untrusted, clipped, and never an instruction.
type ResultItem struct {
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Version    string `json:"version,omitempty"`
	Label      string `json:"label,omitempty"`
}

// ResultSummary is what a later turn may need of a deterministic result: which
// capability produced it, how it ended, and the records it presented. It is a
// reference, not a copy: no field value of a host record is stored.
type ResultSummary struct {
	TurnID       string            `json:"turn_id,omitempty"`
	CapabilityID string            `json:"capability_id"`
	Kind         ResultKind        `json:"kind"`
	Code         contracts.ErrCode `json:"code,omitempty"`
	// DataVersion is the host's data version the result was read at.
	DataVersion string       `json:"data_version,omitempty"`
	Items       []ResultItem `json:"items,omitempty"`
	// ReceiptID is the action receipt of a commit result.
	ReceiptID string    `json:"receipt_id,omitempty"`
	At        time.Time `json:"at"`
}

// Outcome is how a goal ended.
type Outcome string

// Outcomes of a completed goal.
const (
	OutcomeSuccess   Outcome = "success"
	OutcomeFailure   Outcome = "failure"
	OutcomeCancelled Outcome = "cancelled"
)

// Valid reports whether o is one of the outcomes above.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeSuccess, OutcomeFailure, OutcomeCancelled:
		return true
	}
	return false
}

// Capsule is the compact record of a completed goal, kept instead of the goal and
// of the turns that led to it. It names what was done and the records involved;
// it carries a field value only when the writer asked for that field by name.
type Capsule struct {
	GoalID       string  `json:"goal_id,omitempty"`
	CapabilityID string  `json:"capability_id"`
	Outcome      Outcome `json:"outcome"`
	// Fields are the names of the fields the goal had collected, sorted.
	Fields []string `json:"fields,omitempty"`
	// Facts are the clipped values of the fields the writer chose to keep.
	Facts    map[string]string `json:"facts,omitempty"`
	Entities []ResolvedEntity  `json:"entities,omitempty"`
	// ReceiptID is the receipt of the committed action, if there was one.
	ReceiptID   string    `json:"receipt_id,omitempty"`
	TurnID      string    `json:"turn_id,omitempty"`
	CompletedAt time.Time `json:"completed_at"`
}

// Permissions is what the last presented token allowed: the roles and the
// capability scopes. They are compared with the next presentation to see whether
// a permission was taken away.
type Permissions struct {
	Roles        []string `json:"roles,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// State is the durable conversation state of a session (master plan 11.3).
//
// Goal is the active goal; Suspended holds the others, most recently suspended
// last. Everything is bounded (see Config) and holds identifiers, names and short
// clipped text, never a host record, a token or a secret.
type State struct {
	Goal *Goal             `json:"goal,omitempty"`
	Last *DirectiveSummary `json:"last,omitempty"`
	// Suspended are the goals set aside by a topic switch or a nested goal.
	Suspended []*Goal `json:"suspended,omitempty"`
	// Results are the last deterministic results, oldest first.
	Results []ResultSummary `json:"results,omitempty"`
	// AllowedNext are the capabilities the application's package permits next, for
	// the phase the active goal is in. The writer derives them (the router reads
	// them from the package's next_actions); they only ever narrow what the policy
	// gateway then decides.
	AllowedNext []string `json:"allowed_next,omitempty"`
	// Capsules are the compact records of completed goals, oldest first.
	Capsules []Capsule `json:"capsules,omitempty"`
	// Perms is what the last presented token allowed.
	Perms *Permissions `json:"perms,omitempty"`
}

// encode marshals the state.
func (s State) encode() ([]byte, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, "sessions: the state cannot be encoded")
	}
	return b, nil
}

// decodeState reads a stored state. Numbers stay json.Number so an integer slot
// survives a round trip exactly.
func decodeState(b []byte) (State, error) {
	var s State
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&s); err != nil {
		return State{}, contracts.New(contracts.ErrInternal, "sessions: stored state is unreadable")
	}
	return s, nil
}

// Session is one conversation session as stored.
type Session struct {
	// ID is the session ID of the verified context.
	ID string
	// Scope is the scope the session was bound to at its first turn. Policy and
	// the action coordinator bind their decisions to its application, tenant and
	// workspace, not to the presented scope.
	Scope     contracts.Scope
	State     State
	Version   int64
	TurnCount int64
	CreatedAt time.Time
	UpdatedAt time.Time

	// seq is the row the session was read from. Save writes only that row, so a
	// Session value cannot be pointed at another tenant's row by editing its scope.
	seq int64
}

// ExpireGoal discards an idle goal that has passed its expiry and returns the
// preparation it referenced, if any, so the caller can invalidate it. Expired
// state is discarded, never reused (overview.md section 5, step 3). A goal that
// had already finished (success or failure) leaves its capsule behind.
func (s *Session) ExpireGoal(now time.Time) (expired bool, preparedID string) {
	g := s.State.Goal
	if g == nil || g.ExpiresAt.IsZero() || now.Before(g.ExpiresAt) {
		return false, ""
	}
	s.State.keepCapsuleOfFinished(g, now, DefaultMaxCapsules)
	s.State.Goal = nil
	s.State.Last = nil
	return true, g.PreparedID
}

// Turn is one stored turn of the recent window.
type Turn struct {
	TurnID string
	N      int64
	// Role is "user" or "assistant".
	Role string
	// Act is the directive act of an assistant turn, empty for a user turn.
	Act       string
	Body      string
	CreatedAt time.Time
}

// TurnRecord is a completed turn to store with the state it produced.
type TurnRecord struct {
	TurnID string
	// User is the user's text; empty for a turn that carried none (a
	// confirmation or a choice selection).
	User string
	// Act and Assistant are the directive act and the verified response text.
	Act       string
	Assistant string
}

// clip shortens s to at most n runes, marking the cut.
func clip(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:n]), " ") + "..."
}
