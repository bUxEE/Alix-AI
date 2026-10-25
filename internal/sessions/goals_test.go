package sessions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

func newGoal(capID string, now time.Time, slots map[string]Slot) *Goal {
	return &Goal{CapabilityID: capID, Slots: slots, StartedAt: now, UpdatedAt: now, ExpiresAt: now.Add(DefaultGoalTTL)}
}

// A topic switch sets the goal aside with its slots and question; the preparation
// it held is voided, the goal waits with its own expiry, and resuming it brings
// the state back, putting the goal that was active in its place.
func TestGoalSuspendResume(t *testing.T) {
	s, clk, _ := newStore(t, Config{GoalTTL: 10 * time.Minute, SuspendedTTL: time.Hour, MaxSuspended: 2})
	ctx := context.Background()
	cfg := s.Config()
	sc := scopeCaps("tenant-a", "user-1", "ses-1", "items.create", "items.get", "items.list")

	// Goal A: a half-filled creation with a question out and a preparation waiting.
	a := newGoal("items.create", clk.Now(), map[string]Slot{"name": {Value: "widget", Origin: OriginUserTurn}})
	a.Pending = &Clarification{Field: "quantity", Kind: ClarifyText}
	a.SetPrepared("prep_A", clk.Now().Add(10*time.Minute))
	sess := seed(t, s, sc, func(st *State) {
		st.Goal = a
		st.Last = &DirectiveSummary{TurnID: "trn_1", Act: "ask_missing", Question: &Question{Field: "quantity", AnswerKind: "text"}}
	})
	aID := sess.State.Goal.ID
	if aID == "" {
		t.Fatal("Save did not give the goal an ID")
	}

	// The user changes topic: A is suspended and B starts.
	clk.Advance(time.Minute)
	imp, err := sess.State.Suspend(clk.Now(), SuspendedTopicSwitch, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]Void{{PreparedID: "prep_A", GoalID: aID, Reason: act.PreparedActionInvalidationConditionsItemGoalChanged}}, imp.Voided); diff != "" {
		t.Fatalf("the preparation of a suspended goal must be voided (-want +got):\n%s", diff)
	}
	if sess.State.Goal != nil || sess.State.Last != nil || len(sess.State.Suspended) != 1 {
		t.Fatalf("state after suspend = %+v", sess.State)
	}
	susp := sess.State.Suspended[0]
	if susp.PreparedID != "" || susp.Phase != capabilities.PhaseOpen || susp.Confirmed {
		t.Fatalf("a suspended goal must not keep a confirmable preparation: %+v", susp)
	}
	if want := clk.Now().Add(time.Hour); !susp.ExpiresAt.Equal(want) || !susp.SuspendedAt.Equal(clk.Now()) || susp.SuspendReason != SuspendedTopicSwitch {
		t.Fatalf("suspended goal times/reason = %v %v %q", susp.SuspendedAt, susp.ExpiresAt, susp.SuspendReason)
	}
	sess.State.Goal = newGoal("items.get", clk.Now(), map[string]Slot{"item_id": {Value: "item_9", Origin: OriginUserTurn}})
	if sess, err = s.Save(ctx, sess, nil); err != nil {
		t.Fatal(err)
	}

	// Both goals survive a reopen with their slots, origins and question.
	got := reopen(t, s, sc)
	if got.State.Goal == nil || got.State.Goal.CapabilityID != "items.get" || len(got.State.Suspended) != 1 {
		t.Fatalf("reopened state = %+v", got.State)
	}
	gotA := got.State.Suspended[0]
	if gotA.ID != aID || gotA.CapabilityID != "items.create" || gotA.Slots["name"].Origin != OriginUserTurn || gotA.Pending == nil || gotA.Pending.Field != "quantity" {
		t.Fatalf("suspended goal after a round trip = %+v", gotA)
	}
	if got.State.Goal.ID == "" || got.State.Goal.ID == aID {
		t.Fatalf("goal IDs: active %q, suspended %q", got.State.Goal.ID, aID)
	}
	bID := got.State.Goal.ID

	// Resume A: B is set aside in its place, A gets a fresh idle expiry and its question back.
	clk.Advance(time.Minute)
	imp, err = got.State.Resume(aID, clk.Now(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if imp.Resumed != aID || len(imp.Voided) != 0 {
		t.Fatalf("resume impact = %+v", imp)
	}
	if g := got.State.Goal; g == nil || g.ID != aID || g.Pending == nil || !g.SuspendedAt.IsZero() || g.SuspendReason != "" ||
		!g.ExpiresAt.Equal(clk.Now().Add(10*time.Minute)) || g.Slots["name"].Value != "widget" {
		t.Fatalf("resumed goal = %+v", got.State.Goal)
	}
	if len(got.State.Suspended) != 1 || got.State.Suspended[0].ID != bID {
		t.Fatalf("the goal that was active must now be suspended: %+v", got.State.Suspended)
	}
	if got, err = s.Save(ctx, got, nil); err != nil {
		t.Fatal(err)
	}
	again := reopen(t, s, sc)
	if again.State.Goal.ID != aID || again.State.Suspended[0].ID != bID {
		t.Fatalf("resume was not persisted: %+v", again.State)
	}

	// Resuming with nothing suspended, or an unknown goal, is refused.
	var empty State
	if _, err := empty.Resume("", clk.Now(), cfg); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("resume of nothing: %v", err)
	}
	if _, err := again.State.Resume("goal_unknown", clk.Now(), cfg); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("resume of an unknown goal: %v", err)
	}

	// The stack is bounded: with two places, a third suspension drops the oldest.
	st := again.State
	for _, capID := range []string{"items.list", "items.get", "items.create"} {
		if _, err := st.Suspend(clk.Now(), SuspendedTopicSwitch, cfg); err != nil {
			t.Fatal(err)
		}
		st.Goal = newGoal(capID, clk.Now(), nil)
	}
	if len(st.Suspended) != 2 {
		t.Fatalf("suspended = %d goals, want the 2 allowed", len(st.Suspended))
	}
	// A suspended goal that waits longer than its expiry is gone, and so is an idle active goal.
	clk.Advance(2 * time.Hour)
	imp = st.Expire(clk.Now(), cfg)
	if len(st.Suspended) != 0 || st.Goal != nil || imp.GoalsEnded != 3 {
		t.Fatalf("after the expiry: state %+v, impact %+v", st, imp)
	}
}

func TestSuspendRequiresAnActiveGoalAndAReason(t *testing.T) {
	var st State
	if _, err := st.Suspend(t0, SuspendedTopicSwitch, Config{}); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("suspend with no goal: %v", err)
	}
	st.Goal = newGoal("items.get", t0, nil)
	if _, err := st.Suspend(t0, "because", Config{}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("suspend with an unknown reason: %v", err)
	}
	if st.Goal == nil {
		t.Fatal("a refused suspend must leave the goal in place")
	}
}

// Every slot says where its value came from, and the origin survives a restart.
func TestSlotOriginTracked(t *testing.T) {
	s, clk, _ := newStore(t, Config{})
	sc := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	g := newGoal("items.create", clk.Now(), nil)

	set := func(name string, v any, o SlotOrigin, m ...SlotMeta) SlotChange {
		t.Helper()
		ch, err := g.SetSlot(name, v, o, m...)
		if err != nil {
			t.Fatalf("SetSlot(%s, %s): %v", name, o, err)
		}
		return ch
	}
	set("name", "widget", OriginUserTurn, SlotMeta{Type: "string", TurnID: "trn_1"})
	set("title", "t_77", OriginEntityResolution, SlotMeta{Type: "entity_ref"})
	g.Entities = append(g.Entities, ResolvedEntity{Field: "title", EntityType: "title", EntityID: "t_77", Version: "v3"})
	set("colour", "red", OriginChoice)
	set("quantity", json.Number("3"), OriginDefault)

	// A default never replaces a value somebody supplied, nor another default.
	if ch := set("name", "gadget", OriginDefault); ch.Changed || g.Slots["name"].Value != "widget" {
		t.Fatalf("a default replaced a user value: %+v %+v", ch, g.Slots["name"])
	}
	if ch := set("quantity", json.Number("9"), OriginDefault); ch.Changed {
		t.Fatalf("a default replaced a default: %+v", ch)
	}
	// The user then says the value themselves: same value, new origin, nothing stale.
	if ch := set("quantity", json.Number("3"), OriginUserTurn); !ch.Changed || ch.Replaced || g.Slots["quantity"].Origin != OriginUserTurn {
		t.Fatalf("restating a default: %+v %+v", ch, g.Slots["quantity"])
	}
	// A correction replaces the value, reports what it replaced, and drops the
	// entity the old value had been resolved to.
	ch := set("title", "t_78", OriginCorrection)
	if !ch.Replaced || ch.Previous.Origin != OriginEntityResolution || ch.Previous.Value != "t_77" {
		t.Fatalf("correction = %+v", ch)
	}
	if len(g.Entities) != 0 {
		t.Fatalf("the entity of a replaced value must be dropped: %+v", g.Entities)
	}

	// Unknown origins and values that are not scalars are refused.
	for name, c := range map[string]struct {
		v any
		o SlotOrigin
	}{
		"origin":  {"x", SlotOrigin("guess")},
		"empty":   {"x", SlotOrigin("")},
		"object":  {map[string]any{"a": "b"}, OriginUserTurn},
		"nil":     {nil, OriginUserTurn},
		"nested":  {[]any{[]any{"a"}}, OriginUserTurn},
		"toolong": {strings.Repeat("x", maxSlotRunes+1), OriginUserTurn},
	} {
		if _, err := g.SetSlot("f", c.v, c.o); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: SetSlot error = %v, want validation_failed", name, err)
		}
	}
	if _, ok := g.Slots["f"]; ok {
		t.Fatal("a refused value was stored")
	}

	// Origins, types and turns survive a restart.
	seed(t, s, sc, func(st *State) { st.Goal = g })
	want := map[string]SlotOrigin{"name": OriginUserTurn, "title": OriginCorrection, "colour": OriginChoice, "quantity": OriginUserTurn}
	got := reopen(t, s, sc).State.Goal.Slots
	for name, origin := range want {
		if got[name].Origin != origin {
			t.Errorf("slot %s origin = %q, want %q", name, got[name].Origin, origin)
		}
	}
	if got["name"].Type != "string" || got["name"].TurnID != "trn_1" {
		t.Errorf("slot name = %+v, want its type and turn kept", got["name"])
	}
	if n, ok := got["quantity"].Value.(json.Number); !ok || n.String() != "3" {
		t.Errorf("quantity = %#v", got["quantity"].Value)
	}
	for _, o := range []SlotOrigin{OriginUserTurn, OriginDefault, OriginEntityResolution, OriginCorrection, OriginChoice} {
		if !o.Valid() {
			t.Errorf("%q is not a valid origin", o)
		}
	}
}

// A finished goal leaves a compact capsule, not the goal or its turns.
func TestCapsuleOnCompletion(t *testing.T) {
	s, clk, _ := newStore(t, Config{MaxCapsules: 2})
	cfg := s.Config()
	sc := scopeCaps("tenant-a", "user-1", "ses-1", "items.create", "items.get")

	g := newGoal("items.create", clk.Now(), map[string]Slot{
		"name":     {Value: "widget", Origin: OriginUserTurn},
		"note":     {Value: "call me about the delivery to 5 Elm Street", Origin: OriginUserTurn},
		"quantity": {Value: json.Number("3"), Origin: OriginUserTurn},
	})
	g.Phase = capabilities.PhaseSuccess
	g.Entities = []ResolvedEntity{{Field: "title", EntityType: "title", EntityID: "t_1", Version: "v2"}}
	sess := seed(t, s, sc, func(st *State) {
		st.Goal = g
		st.Last = &DirectiveSummary{Act: "report_outcome"}
		st.SetAllowedNext([]string{"items.get"})
	})

	clk.Advance(time.Minute)
	c, imp, err := sess.State.Complete(clk.Now(), CompleteOptions{TurnID: "trn_4", ReceiptID: "rcpt_1", KeepFields: []string{"name", "quantity", "absent"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if imp.Changed() {
		t.Fatalf("a finished goal with no preparation has nothing to void: %+v", imp)
	}
	want := Capsule{
		GoalID: g.ID, CapabilityID: "items.create", Outcome: OutcomeSuccess,
		Fields: []string{"name", "note", "quantity"}, Facts: map[string]string{"name": "widget", "quantity": "3"},
		Entities: g.Entities, ReceiptID: "rcpt_1", TurnID: "trn_4", CompletedAt: clk.Now(),
	}
	if diff := cmp.Diff(want, c); diff != "" {
		t.Fatalf("capsule (-want +got):\n%s", diff)
	}
	if _, kept := c.Facts["note"]; kept {
		t.Fatal("a field the writer did not name must never be copied into a capsule")
	}
	if sess.State.Goal != nil || sess.State.Last != nil || len(sess.State.Capsules) != 1 || len(sess.State.AllowedNext) != 0 {
		t.Fatalf("state after completion = %+v", sess.State)
	}

	// The capsule is what is stored: no goal, no slots, no values but the kept ones.
	if _, err := s.Save(context.Background(), sess, nil); err != nil {
		t.Fatal(err)
	}
	got := reopen(t, s, sc).State
	if got.Goal != nil || len(got.Capsules) != 1 || got.Capsules[0].Facts["name"] != "widget" {
		t.Fatalf("reopened = %+v", got)
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), "delivery") || strings.Contains(string(raw), "5 Elm") {
		t.Fatalf("the capsule leaked an unkept slot value: %s", raw)
	}

	// An open goal needs an explicit outcome; cancelling keeps a capsule too.
	st := got
	st.Goal = newGoal("items.get", clk.Now(), map[string]Slot{"item_id": {Value: "i1", Origin: OriginUserTurn}})
	if _, _, err := st.Complete(clk.Now(), CompleteOptions{}, cfg); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("completing an open goal without an outcome: %v", err)
	}
	if _, _, err := st.Complete(clk.Now(), CompleteOptions{Outcome: "done"}, cfg); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("completing with an unknown outcome: %v", err)
	}
	if _, _, err := (&State{}).Complete(clk.Now(), CompleteOptions{Outcome: OutcomeCancelled}, cfg); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("completing with no goal: %v", err)
	}
	// Cancelling a goal that still holds a preparation reports it to be voided.
	st.Goal.SetPrepared("prep_Z", clk.Now().Add(time.Minute))
	c2, imp, err := st.Complete(clk.Now(), CompleteOptions{Outcome: OutcomeCancelled}, cfg)
	if err != nil || c2.Outcome != OutcomeCancelled || len(imp.Voided) != 1 || imp.Voided[0].PreparedID != "prep_Z" {
		t.Fatalf("cancel = %+v %+v %v", c2, imp, err)
	}
	// The capsule list is bounded: the oldest goes first.
	st.Goal = newGoal("items.get", clk.Now(), nil)
	st.Goal.Phase = capabilities.PhaseFailure
	c3, _, err := st.Complete(clk.Now(), CompleteOptions{}, cfg)
	if err != nil || c3.Outcome != OutcomeFailure {
		t.Fatalf("failure capsule = %+v %v", c3, err)
	}
	if len(st.Capsules) != 2 || st.Capsules[0].Outcome != OutcomeCancelled || st.Capsules[1].Outcome != OutcomeFailure {
		t.Fatalf("capsules = %+v, want the newest 2", st.Capsules)
	}
}

// A goal that finished and then sat idle leaves its capsule when it expires; one
// that never finished leaves nothing.
func TestExpiredFinishedGoalLeavesACapsule(t *testing.T) {
	done := Session{State: State{Goal: newGoal("items.create", t0, nil)}}
	done.State.Goal.Phase = capabilities.PhaseSuccess
	if expired, _ := done.ExpireGoal(t0.Add(time.Hour)); !expired || len(done.State.Capsules) != 1 || done.State.Capsules[0].Outcome != OutcomeSuccess {
		t.Fatalf("finished goal: %+v", done.State)
	}
	open := Session{State: State{Goal: newGoal("items.create", t0, nil)}}
	if expired, _ := open.ExpireGoal(t0.Add(time.Hour)); !expired || len(open.State.Capsules) != 0 {
		t.Fatalf("open goal: %+v", open.State)
	}
}

// A nested goal returns to its parent when it completes.
func TestNestedGoalCompletionResumesTheParent(t *testing.T) {
	cfg := Config{}.withDefaults()
	st := State{Goal: newGoal("items.create", t0, map[string]Slot{"name": {Value: "w", Origin: OriginUserTurn}})}
	st.Goal.ID = "goal_parent"
	if _, err := st.Nest(newGoal("items.get", t0, nil), t0, cfg); err != nil {
		t.Fatal(err)
	}
	child := st.Goal
	if child.CapabilityID != "items.get" || child.ParentID != "goal_parent" || len(st.Suspended) != 1 || st.Suspended[0].SuspendReason != SuspendedNested {
		t.Fatalf("after nesting: %+v / %+v", st.Goal, st.Suspended)
	}
	child.Phase = capabilities.PhaseSuccess
	_, imp, err := st.Complete(t0.Add(time.Minute), CompleteOptions{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if imp.Resumed != "goal_parent" || st.Goal == nil || st.Goal.ID != "goal_parent" || len(st.Suspended) != 0 {
		t.Fatalf("parent not resumed: impact %+v state %+v", imp, st)
	}
	if st.Goal.Slots["name"].Value != "w" || !st.Goal.ExpiresAt.Equal(t0.Add(time.Minute+cfg.GoalTTL)) {
		t.Fatalf("resumed parent = %+v", st.Goal)
	}
	if _, err := (&State{}).Nest(newGoal("x", t0, nil), t0, cfg); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("nesting with no parent: %v", err)
	}
}

// Results and next actions are bounded when written.
func TestResultsAndNextActionsAreBounded(t *testing.T) {
	cfg := Config{MaxResults: 2}
	s, clk, _ := newStore(t, cfg)
	sc := scopeCaps("tenant-a", "user-1", "ses-1", "items.list")
	long := strings.Repeat("x", 500)
	seed(t, s, sc, func(st *State) {
		for i := range 4 {
			var items []ResultItem
			for j := range 15 {
				items = append(items, ResultItem{EntityType: "item", EntityID: string(rune('a'+j)) + string(rune('0'+i)), Version: "v1", Label: long})
			}
			st.RecordResult(ResultSummary{CapabilityID: "items.list", Kind: ResultRead, At: clk.Now(), Items: items}, cfg)
		}
		st.SetAllowedNext([]string{"a", "b", "a", "", "c", "d", "e", "f", "g", "h", "i", "j"})
	})
	got := reopen(t, s, sc).State
	if len(got.Results) != 2 {
		t.Fatalf("kept %d results, want 2", len(got.Results))
	}
	for _, r := range got.Results {
		if len(r.Items) != maxResultItems {
			t.Fatalf("a result keeps %d records, want %d", len(r.Items), maxResultItems)
		}
		if n := len([]rune(r.Items[0].Label)); n > maxLabelRunes+3 {
			t.Fatalf("label is %d runes", n)
		}
	}
	if len(got.AllowedNext) != maxAllowedNext || got.AllowedNext[0] != "a" || got.AllowedNext[1] != "b" || got.AllowedNext[2] != "c" {
		t.Fatalf("allowed next = %v", got.AllowedNext)
	}
}

// A result the writer did not time is timed when it is first stored: it neither
// makes the session due at the dawn of time nor is dropped by the next sweep, and
// it lapses with the goal TTL like any other.
func TestResultWithoutATimeIsStampedAndSurvivesASweep(t *testing.T) {
	s, clk, db := newStore(t, Config{GoalTTL: 10 * time.Minute})
	ctx := context.Background()
	sc := scopeCaps("tenant-a", "user-1", "ses-1", "items.list")
	seed(t, s, sc, func(st *State) {
		st.Results = []ResultSummary{{CapabilityID: "items.list", Kind: ResultRead}}
	})
	got := reopen(t, s, sc).State.Results
	if len(got) != 1 || !got[0].At.Equal(clk.Now()) {
		t.Fatalf("results = %+v, want the result stamped with the time it was stored", got)
	}
	var d int64
	if err := db.QueryRowContext(ctx, `SELECT next_deadline FROM sessions WHERE id = 'ses-1'`).Scan(&d); err != nil || d != toMS(clk.Now().Add(10*time.Minute)) {
		t.Fatalf("next deadline = %v (%v), want the result's lapse", fromMS(d), err)
	}
	res, err := s.InvalidateWorkspace(ctx, service("tenant-a"), Invalidation{Kind: KindTimeout}, &scopedInvalidator{})
	if err != nil || res.Sessions != 0 || len(reopen(t, s, sc).State.Results) != 1 {
		t.Fatalf("an early sweep = %+v %v", res, err)
	}
	// An unsaved result with no time is never treated as lapsed either.
	st := State{Results: []ResultSummary{{CapabilityID: "items.list", Kind: ResultRead}}}
	if imp := st.Expire(clk.Now().Add(24*time.Hour), s.Config()); imp.Changed() || len(st.Results) != 1 {
		t.Fatalf("Expire dropped a result with no time: %+v", imp)
	}
	clk.Advance(11 * time.Minute)
	if res, err = s.InvalidateWorkspace(ctx, service("tenant-a"), Invalidation{Kind: KindTimeout}, &scopedInvalidator{}); err != nil || res.Sessions != 1 || len(reopen(t, s, sc).State.Results) != 0 {
		t.Fatalf("a sweep after the lapse = %+v %v", res, err)
	}
}
