package sessions

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Void is a preparation the state no longer references and that the caller must
// invalidate through the action coordinator (outside any database transaction).
// The state methods only report it: they never call the coordinator themselves.
type Void struct {
	PreparedID string
	GoalID     string
	Reason     act.PreparedActionInvalidationConditionsItem
}

// Impact is what a state change did. The state methods return it so the caller
// can invalidate the preparations that lost their goal and tell the user what was
// dropped; Store.Invalidate does that itself.
type Impact struct {
	// Voided are the preparations to invalidate.
	Voided []Void
	// GoalsEnded counts goals discarded (a revoked capability, an idle or suspended
	// expiry, a goal pushed off a full suspended stack, a principal change).
	GoalsEnded int
	// PendingCleared counts clarifications dropped.
	PendingCleared int
	// EntitiesDropped counts resolved entities (and the slots they filled) dropped
	// because the record behind them changed.
	EntitiesDropped int
	// ResultsDropped counts deterministic results (or presented records) dropped.
	ResultsDropped int
	// Lapsed counts prepared-action references dropped because the preparation's
	// own expiry passed; there is nothing to invalidate.
	Lapsed int
	// Resumed is the ID of the parent goal that became active again when a nested
	// goal completed.
	Resumed string
	// Ended is true when the whole session must end (the principal changed).
	Ended bool
}

// Changed reports whether anything happened.
func (i Impact) Changed() bool {
	return len(i.Voided) > 0 || i.GoalsEnded > 0 || i.PendingCleared > 0 || i.EntitiesDropped > 0 ||
		i.ResultsDropped > 0 || i.Lapsed > 0 || i.Resumed != "" || i.Ended
}

// merge adds o to i.
func (i *Impact) merge(o Impact) {
	i.Voided = append(i.Voided, o.Voided...)
	i.GoalsEnded += o.GoalsEnded
	i.PendingCleared += o.PendingCleared
	i.EntitiesDropped += o.EntitiesDropped
	i.ResultsDropped += o.ResultsDropped
	i.Lapsed += o.Lapsed
	if o.Resumed != "" {
		i.Resumed = o.Resumed
	}
	i.Ended = i.Ended || o.Ended
}

// newGoalID returns a goal ID. Goal IDs are opaque references inside one session.
func newGoalID() string { return ids.New("goal") }

// ---- slots ----

// SlotMeta is optional detail recorded with a slot.
type SlotMeta struct {
	// Type is the capability field type the value was validated against.
	Type string
	// TurnID is the turn that set the value.
	TurnID string
}

// SlotChange is what SetSlot did.
type SlotChange struct {
	// Changed is true if the slot now differs from before (a new slot, a new
	// value, or a new origin).
	Changed bool
	// Replaced is true if an existing slot got a different value. A preparation
	// built on the old value is then stale: the caller invalidates it
	// (field_corrected) and clears the reference with ClearPrepared.
	Replaced bool
	// Previous is the slot that was replaced, when there was one.
	Previous Slot
}

// SetSlot records a collected value and where it came from. The origin must be
// one of the known origins. A default fills only a field nobody has set: it never
// replaces a value a person or the host supplied. A replaced value drops the
// entity the old value was resolved to, because the new one is resolved again.
//
// SetSlot does not touch a prepared action: whoever replaces a value under one
// must invalidate it first (the coordinator call is the caller's) and then clear
// the reference with ClearPrepared.
func (g *Goal) SetSlot(name string, value any, origin SlotOrigin, meta ...SlotMeta) (SlotChange, error) {
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 128 {
		return SlotChange{}, contracts.New(contracts.ErrValidationFailed, "sessions: a slot needs a name")
	}
	if !origin.Valid() {
		return SlotChange{}, contracts.Newf(contracts.ErrValidationFailed, "sessions: %q is not a slot origin", string(origin))
	}
	if err := checkSlotValue(value); err != nil {
		return SlotChange{}, err
	}
	var m SlotMeta
	if len(meta) > 0 {
		m = meta[0]
	}
	if g.Slots == nil {
		g.Slots = map[string]Slot{}
	}
	prev, had := g.Slots[name]
	if origin == OriginDefault && had {
		return SlotChange{}, nil
	}
	same := had && sameValue(prev.Value, value)
	if same && prev.Origin == origin && prev.Type == m.Type {
		return SlotChange{}, nil
	}
	g.Slots[name] = Slot{Value: value, Origin: origin, Type: m.Type, TurnID: m.TurnID}
	ch := SlotChange{Changed: true, Replaced: had && !same}
	if ch.Replaced {
		ch.Previous = prev
		g.Entities = dropEntityOf(g.Entities, name)
	}
	return ch, nil
}

// checkSlotValue accepts what a slot may hold: a scalar or a flat, bounded list of
// scalars. A nested object is a host record, which the state never keeps.
func checkSlotValue(v any) error {
	bad := func() error {
		return contracts.New(contracts.ErrValidationFailed, "sessions: a slot holds a scalar or a flat list of scalars")
	}
	scalar := func(x any) bool {
		switch t := x.(type) {
		case string:
			return utf8.RuneCountInString(t) <= maxSlotRunes
		case bool, json.Number, int, int32, int64, uint, uint32, uint64, float32, float64:
			return true
		}
		return false
	}
	switch t := v.(type) {
	case nil:
		return bad()
	case []any:
		if len(t) > maxSlotList {
			return bad()
		}
		for _, x := range t {
			if !scalar(x) {
				return bad()
			}
		}
		return nil
	case []string:
		if len(t) > maxSlotList {
			return bad()
		}
		for _, x := range t {
			if !scalar(x) {
				return bad()
			}
		}
		return nil
	}
	if !scalar(v) {
		return bad()
	}
	return nil
}

// sameValue compares two slot values by their JSON form, so json.Number("3"),
// 3 and 3.0 are one value.
func sameValue(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

func dropEntityOf(in []ResolvedEntity, field string) []ResolvedEntity {
	out := in[:0:0]
	for _, e := range in {
		if e.Field != field {
			out = append(out, e)
		}
	}
	return out
}

// ---- prepared action reference ----

// SetPrepared records the preparation that awaits a confirmation and its expiry,
// and moves the goal to the prepared phase. The preparation itself, its state and
// its authoritative expiry stay in internal/actions.
func (g *Goal) SetPrepared(preparationID string, expiresAt time.Time) {
	g.PreparedID, g.PreparedExpiresAt, g.Confirmed = preparationID, expiresAt, false
	g.Phase = capabilities.PhasePrepared
}

// ClearPrepared drops the reference to the preparation and returns its ID ("" if
// there was none) so the caller can invalidate it. A goal in the prepared phase
// goes back to open.
func (g *Goal) ClearPrepared() string {
	id := g.PreparedID
	g.PreparedID, g.PreparedExpiresAt, g.Confirmed = "", time.Time{}, false
	if g.Phase == capabilities.PhasePrepared {
		g.Phase = capabilities.PhaseOpen
	}
	return id
}

// ---- results and allowed next actions ----

// RecordResult appends a deterministic result. The list keeps the newest
// cfg.MaxResults; each result keeps at most ten presented records and clips their
// labels.
func (s *State) RecordResult(r ResultSummary, cfg Config) {
	cfg = cfg.withDefaults()
	r.Items = clipItems(r.Items)
	s.Results = append(s.Results, r)
	if over := len(s.Results) - cfg.MaxResults; over > 0 {
		s.Results = slices.Clone(s.Results[over:])
	}
}

func clipItems(in []ResultItem) []ResultItem {
	if len(in) > maxResultItems {
		in = in[:maxResultItems]
	}
	out := make([]ResultItem, len(in))
	for i, it := range in {
		it.Label = clip(it.Label, maxLabelRunes)
		out[i] = it
	}
	return out
}

// SetAllowedNext replaces the capabilities the package permits next. Duplicates
// and empty IDs are dropped and the list is bounded.
func (s *State) SetAllowedNext(capabilityIDs []string) {
	s.AllowedNext = boundIDs(capabilityIDs)
}

func boundIDs(in []string) []string {
	var out []string
	for _, id := range in {
		if id == "" || slices.Contains(out, id) {
			continue
		}
		out = append(out, id)
		if len(out) == maxAllowedNext {
			break
		}
	}
	return out
}

// ---- goals: suspend, resume, nest, complete ----

// Find returns the active goal or a suspended one by ID.
func (s *State) Find(goalID string) *Goal {
	if goalID == "" {
		return nil
	}
	if s.Goal != nil && s.Goal.ID == goalID {
		return s.Goal
	}
	for _, g := range s.Suspended {
		if g.ID == goalID {
			return g
		}
	}
	return nil
}

func conflict(msg string) error { return contracts.New(contracts.ErrConflict, msg) }

// Suspend sets the active goal aside. Its slots, resolved entities and pending
// clarification stay, so a later Resume continues where it stopped; its
// preparation does not: a confirmation must not be answerable while the user is
// somewhere else, so the reference is cleared and returned in Impact.Voided for
// the caller to invalidate (goal_changed). The goal gets its own expiry,
// cfg.SuspendedTTL from now. If the suspended stack is full, the goal suspended
// longest ago is discarded.
func (s *State) Suspend(now time.Time, reason SuspendReason, cfg Config) (Impact, error) {
	cfg = cfg.withDefaults()
	if s.Goal == nil {
		return Impact{}, conflict("sessions: there is no active goal to suspend")
	}
	if reason != SuspendedTopicSwitch && reason != SuspendedNested {
		return Impact{}, contracts.Newf(contracts.ErrValidationFailed, "sessions: %q is not a suspend reason", string(reason))
	}
	return s.push(now, reason, cfg), nil
}

// push moves the active goal onto the suspended stack. The caller checked there is one.
func (s *State) push(now time.Time, reason SuspendReason, cfg Config) Impact {
	var imp Impact
	g := s.Goal
	if g.ID == "" {
		g.ID = newGoalID()
	}
	if id := g.ClearPrepared(); id != "" {
		imp.Voided = append(imp.Voided, Void{PreparedID: id, GoalID: g.ID, Reason: act.PreparedActionInvalidationConditionsItemGoalChanged})
	}
	g.SuspendedAt, g.SuspendReason, g.UpdatedAt = now, reason, now
	g.ExpiresAt = now.Add(cfg.SuspendedTTL)
	s.Suspended = append(s.Suspended, g)
	for len(s.Suspended) > cfg.MaxSuspended {
		imp.GoalsEnded++
		s.Suspended = slices.Clone(s.Suspended[1:])
	}
	s.Goal, s.Last = nil, nil
	return imp
}

// Nest makes child the active goal and sets the current goal aside as its parent.
// When the child completes, Complete makes the parent active again. The child
// starts now with an expiry of cfg.GoalTTL.
func (s *State) Nest(child *Goal, now time.Time, cfg Config) (Impact, error) {
	cfg = cfg.withDefaults()
	if s.Goal == nil {
		return Impact{}, conflict("sessions: there is no active goal to nest under")
	}
	if child == nil || child.CapabilityID == "" {
		return Impact{}, contracts.New(contracts.ErrValidationFailed, "sessions: a nested goal needs a capability")
	}
	imp := s.push(now, SuspendedNested, cfg)
	// The parent is the newest suspended goal, which a full stack never evicts.
	child.ParentID = s.Suspended[len(s.Suspended)-1].ID
	s.startGoal(child, now, cfg)
	return imp, nil
}

// startGoal makes g the active goal.
func (s *State) startGoal(g *Goal, now time.Time, cfg Config) {
	if g.ID == "" {
		g.ID = newGoalID()
	}
	if g.Slots == nil {
		g.Slots = map[string]Slot{}
	}
	if g.StartedAt.IsZero() {
		g.StartedAt = now
	}
	g.UpdatedAt, g.ExpiresAt = now, now.Add(cfg.GoalTTL)
	g.SuspendedAt, g.SuspendReason = time.Time{}, ""
	s.Goal, s.Last = g, nil
}

// Resume makes a suspended goal the active one. goalID names it; "" means the
// goal suspended most recently. A goal that is active now is suspended first (a
// topic switch back), and the resumed goal gets a fresh idle expiry.
func (s *State) Resume(goalID string, now time.Time, cfg Config) (Impact, error) {
	cfg = cfg.withDefaults()
	idx := len(s.Suspended) - 1
	if goalID != "" {
		idx = slices.IndexFunc(s.Suspended, func(g *Goal) bool { return g.ID == goalID })
	}
	if idx < 0 || idx >= len(s.Suspended) {
		return Impact{}, conflict("sessions: there is no suspended goal to resume")
	}
	target := s.Suspended[idx]
	s.Suspended = slices.Delete(slices.Clone(s.Suspended), idx, idx+1)
	var imp Impact
	if s.Goal != nil {
		imp = s.push(now, SuspendedTopicSwitch, cfg)
	}
	s.startGoal(target, now, cfg)
	imp.Resumed = target.ID
	return imp, nil
}

// CompleteOptions says how a goal ended.
type CompleteOptions struct {
	// Outcome is how the goal ended. Empty takes it from the goal's phase (success
	// or failure); an open goal needs an explicit outcome, for example cancelled.
	Outcome Outcome
	// TurnID is the turn that completed the goal.
	TurnID string
	// ReceiptID is the receipt of the committed action, if there was one.
	ReceiptID string
	// KeepFields names the fields whose values the capsule may carry (clipped).
	// Every other field is recorded by name only: the caller, who knows which
	// fields are sensitive, decides.
	KeepFields []string
	// AllowedNext are the capabilities the package permits after this goal, for the
	// state that follows; none if empty.
	AllowedNext []string
}

// Complete ends the active goal and keeps a compact capsule of it. The goal, its
// slots and the turns that led to it are not kept. A preparation the goal still
// references is returned in Impact.Voided. If the goal was nested, its parent
// becomes active again (Impact.Resumed).
func (s *State) Complete(now time.Time, opts CompleteOptions, cfg Config) (Capsule, Impact, error) {
	cfg = cfg.withDefaults()
	g := s.Goal
	if g == nil {
		return Capsule{}, Impact{}, conflict("sessions: there is no active goal to complete")
	}
	outcome := opts.Outcome
	if outcome == "" {
		switch g.Phase {
		case capabilities.PhaseSuccess:
			outcome = OutcomeSuccess
		case capabilities.PhaseFailure:
			outcome = OutcomeFailure
		default:
			return Capsule{}, Impact{}, contracts.New(contracts.ErrValidationFailed, "sessions: an open goal needs an explicit outcome to complete")
		}
	}
	if !outcome.Valid() {
		return Capsule{}, Impact{}, contracts.Newf(contracts.ErrValidationFailed, "sessions: %q is not an outcome", string(outcome))
	}
	if g.ID == "" {
		g.ID = newGoalID()
	}
	var imp Impact
	if id := g.PreparedID; id != "" {
		imp.Voided = append(imp.Voided, Void{PreparedID: id, GoalID: g.ID, Reason: act.PreparedActionInvalidationConditionsItemGoalChanged})
	}
	c := capsuleOf(g, outcome, opts, now)
	s.addCapsule(c, cfg.MaxCapsules)
	s.Goal, s.Last = nil, nil
	s.SetAllowedNext(opts.AllowedNext)
	if g.ParentID != "" {
		if i := slices.IndexFunc(s.Suspended, func(p *Goal) bool { return p.ID == g.ParentID }); i >= 0 {
			parent := s.Suspended[i]
			s.Suspended = slices.Delete(slices.Clone(s.Suspended), i, i+1)
			s.startGoal(parent, now, cfg)
			imp.Resumed = parent.ID
		}
	}
	return c, imp, nil
}

func capsuleOf(g *Goal, outcome Outcome, opts CompleteOptions, now time.Time) Capsule {
	c := Capsule{
		GoalID: g.ID, CapabilityID: g.CapabilityID, Outcome: outcome,
		ReceiptID: opts.ReceiptID, TurnID: opts.TurnID, CompletedAt: now,
	}
	for name := range g.Slots {
		c.Fields = append(c.Fields, name)
	}
	sort.Strings(c.Fields)
	for _, name := range opts.KeepFields {
		sl, ok := g.Slots[name]
		if !ok || len(c.Facts) >= maxCapsuleFact {
			continue
		}
		if c.Facts == nil {
			c.Facts = map[string]string{}
		}
		c.Facts[name] = clip(renderValue(sl.Value), maxFactRunes)
	}
	for i, e := range g.Entities {
		if i == maxCapsuleEnts {
			break
		}
		c.Entities = append(c.Entities, e)
	}
	return c
}

// renderValue is the short text form of a slot value.
func renderValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		parts := make([]string, len(t))
		for i, x := range t {
			parts[i] = renderValue(x)
		}
		return strings.Join(parts, ", ")
	case []string:
		return strings.Join(t, ", ")
	case json.Number:
		return t.String()
	}
	return fmt.Sprint(v)
}

func (s *State) addCapsule(c Capsule, limit int) {
	s.Capsules = append(s.Capsules, c)
	if over := len(s.Capsules) - limit; over > 0 {
		s.Capsules = slices.Clone(s.Capsules[over:])
	}
}

// keepCapsuleOfFinished leaves the capsule of a goal that had already finished
// (success or failure) when it is discarded for being idle.
func (s *State) keepCapsuleOfFinished(g *Goal, now time.Time, limit int) {
	var outcome Outcome
	switch g.Phase {
	case capabilities.PhaseSuccess:
		outcome = OutcomeSuccess
	case capabilities.PhaseFailure:
		outcome = OutcomeFailure
	default:
		return
	}
	s.addCapsule(capsuleOf(g, outcome, CompleteOptions{}, now), limit)
}

// ---- bounding, deadlines and lookup rows ----

// normalize bounds the state to the configured limits before it is written and
// assigns what Save owns: goal IDs and a clarification's expiry.
func (s *Store) normalize(st *State, now time.Time) {
	cfg := s.cfg
	stamp := func(g *Goal) {
		if g == nil {
			return
		}
		if g.ID == "" {
			g.ID = newGoalID()
		}
		if p := g.Pending; p != nil {
			if p.AskedAt.IsZero() {
				p.AskedAt = now
			}
			if p.ExpiresAt.IsZero() && cfg.PendingTTL > 0 {
				p.ExpiresAt = p.AskedAt.Add(cfg.PendingTTL)
			}
		}
	}
	stamp(st.Goal)
	for _, g := range st.Suspended {
		stamp(g)
	}
	if over := len(st.Suspended) - cfg.MaxSuspended; over > 0 {
		st.Suspended = slices.Clone(st.Suspended[over:])
	}
	if over := len(st.Capsules) - cfg.MaxCapsules; over > 0 {
		st.Capsules = slices.Clone(st.Capsules[over:])
	}
	if over := len(st.Results) - cfg.MaxResults; over > 0 {
		st.Results = slices.Clone(st.Results[over:])
	}
	for i := range st.Results {
		st.Results[i].Items = clipItems(st.Results[i].Items)
		if st.Results[i].At.IsZero() {
			st.Results[i].At = now // a result the writer did not time is timed when it is first stored
		}
	}
	st.AllowedNext = boundIDs(st.AllowedNext)
}

// nextDeadline is the earliest moment any part of the state lapses.
func (s State) nextDeadline(cfg Config) (time.Time, bool) {
	var best time.Time
	see := func(t time.Time) {
		if !t.IsZero() && (best.IsZero() || t.Before(best)) {
			best = t
		}
	}
	goal := func(g *Goal) {
		if g == nil {
			return
		}
		see(g.ExpiresAt)
		if g.Pending != nil {
			see(g.Pending.ExpiresAt)
		}
		if g.PreparedID != "" {
			see(g.PreparedExpiresAt)
		}
	}
	goal(s.Goal)
	for _, g := range s.Suspended {
		goal(g)
	}
	for _, r := range s.Results {
		if !r.At.IsZero() {
			see(r.At.Add(cfg.GoalTTL))
		}
	}
	return best, !best.IsZero()
}

// Reference kinds of the session_refs table.
const (
	refEntity     = "entity"
	refCapability = "capability"
)

type ref struct{ kind, typ, id string }

// refs are the references the state's goals and results make, for the lookup
// table: what an event naming a record or a capability has to find.
func (s State) refs() []ref {
	var out []ref
	add := func(r ref) {
		if r.id != "" && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	goal := func(g *Goal) {
		if g == nil {
			return
		}
		add(ref{refCapability, "", g.CapabilityID})
		for _, e := range g.Entities {
			add(ref{refEntity, e.EntityType, e.EntityID})
		}
		if p := g.Pending; p != nil && p.EntityType != "" {
			for _, c := range p.Choices {
				add(ref{refEntity, p.EntityType, string(c.ChoiceID)})
			}
		}
	}
	goal(s.Goal)
	for _, g := range s.Suspended {
		goal(g)
	}
	for _, r := range s.Results {
		for _, it := range r.Items {
			add(ref{refEntity, it.EntityType, it.EntityID})
		}
	}
	return out
}
