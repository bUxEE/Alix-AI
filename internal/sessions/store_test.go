package sessions

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

var t0 = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)

func scopeOf(tenant, principal, session string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-1", ApplicationID: "demo-app", TenantID: tenant, WorkspaceID: "ws-1",
		PrincipalID: principal, Roles: []string{"member"}, CapabilityScopes: []string{"items.get"}, SessionID: session,
	}
}

func newStore(t *testing.T, cfg Config) (*Store, *clock.Fake, *store.DB) {
	t.Helper()
	// Only this module's tables: importing capabilities registers apppkg and
	// trust migrations in the default registry, which are not session tables.
	reg := store.NewRegistry()
	if err := reg.Register("sessions", migrations); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register("transcript", transcriptFS()); err != nil {
		t.Fatal(err)
	}
	db := storetest.Open(t, reg)
	clk := clock.NewFake(t0)
	s, err := New(db, clk, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, clk, db
}

func wantCode(t *testing.T, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", code)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("error code = %s (%v), want %s", got, err, code)
	}
}

func TestSessionTablesCarryTheScopeColumns(t *testing.T) {
	_, _, db := newStore(t, Config{})
	storetest.AssertScopedTables(t, db)
}

func TestOpenCreatesThenReturnsTheSameSession(t *testing.T) {
	s, _, _ := newStore(t, Config{})
	ctx := context.Background()
	sc := scopeOf("tenant-a", "user-1", "ses-1")
	a, err := s.Open(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "ses-1" || a.Version != 1 || a.Scope.PrincipalID != "user-1" {
		t.Fatalf("created session = %+v", a)
	}
	b, err := s.Open(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if b.Version != a.Version || b.ID != a.ID {
		t.Fatalf("second open = %+v, want the same session", b)
	}
}

func TestOpenRefusesInvalidScope(t *testing.T) {
	s, _, _ := newStore(t, Config{})
	sc := scopeOf("tenant-a", "user-1", "ses-1")
	sc.TenantID = ""
	_, err := s.Open(context.Background(), sc)
	wantCode(t, err, contracts.ErrScopeMissing)
}

// A session cannot change principal, tenant or workspace without a new
// authenticated context (master plan 10.2).
func TestOpenChecksContinuity(t *testing.T) {
	s, _, _ := newStore(t, Config{})
	ctx := context.Background()
	if _, err := s.Open(ctx, scopeOf("tenant-a", "user-1", "ses-1")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Open(ctx, scopeOf("tenant-a", "user-2", "ses-1"))
	wantCode(t, err, contracts.ErrScopeViolation)

	// Roles and capability scopes may differ: a fresh token narrows or widens.
	narrow := scopeOf("tenant-a", "user-1", "ses-1")
	narrow.Roles, narrow.CapabilityScopes = []string{"visitor"}, []string{"items.get", "items.create"}
	if _, err := s.Open(ctx, narrow); err != nil {
		t.Fatalf("a narrower token of the same context: %v", err)
	}
}

// The same session ID in another tenant is another session: nothing is shared.
func TestSessionsAreScopedByTenant(t *testing.T) {
	s, _, _ := newStore(t, Config{})
	ctx := context.Background()
	a, err := s.Open(ctx, scopeOf("tenant-a", "user-1", "ses-1"))
	if err != nil {
		t.Fatal(err)
	}
	a.State.Goal = &Goal{CapabilityID: "items.get", Slots: map[string]Slot{"item_id": {Value: "x", Origin: OriginUserTurn}}}
	if _, err := s.Save(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	b, err := s.Open(ctx, scopeOf("tenant-b", "user-9", "ses-1"))
	if err != nil {
		t.Fatalf("the same session ID in another tenant: %v", err)
	}
	if b.State.Goal != nil {
		t.Fatalf("tenant-b sees the goal of tenant-a: %+v", b.State.Goal)
	}
}

func TestSaveRoundTripsStateAndNumbers(t *testing.T) {
	s, _, _ := newStore(t, Config{})
	ctx := context.Background()
	sc := scopeOf("tenant-a", "user-1", "ses-1")
	sess, _ := s.Open(ctx, sc)
	sess.State = State{
		Goal: &Goal{
			CapabilityID: "items.create", Phase: capabilities.PhasePrepared, PreparedID: "prep_X",
			Slots:    map[string]Slot{"count": {Value: json.Number("3"), Origin: OriginUserTurn}, "name": {Value: "x", Origin: OriginCorrection}},
			Pending:  &Clarification{Field: "name", Kind: ClarifyText},
			Entities: []ResolvedEntity{{Field: "title", EntityType: "title", EntityID: "t1", Version: "v3"}},
		},
		Last: &DirectiveSummary{TurnID: "trn_1", Act: "ask_missing", MissingFields: []string{"name"}, Question: &Question{Field: "name", AnswerKind: "text"}},
	}
	saved, err := s.Save(ctx, sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 2 {
		t.Fatalf("version = %d, want 2", saved.Version)
	}
	got, err := s.Open(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	g := got.State.Goal
	if g == nil || g.PreparedID != "prep_X" || g.Phase != capabilities.PhasePrepared || g.Pending == nil || g.Pending.Field != "name" {
		t.Fatalf("goal = %+v", g)
	}
	if n, ok := g.Slots["count"].Value.(json.Number); !ok || n.String() != "3" {
		t.Fatalf("count slot = %#v, want json.Number 3", g.Slots["count"].Value)
	}
	if g.Slots["name"].Origin != OriginCorrection || len(g.Entities) != 1 || got.State.Last.Act != "ask_missing" {
		t.Fatalf("state = %+v", got.State)
	}
	if a := g.Active(); a == nil || a.CapabilityID != "items.create" || a.Phase != capabilities.PhasePrepared {
		t.Fatalf("Active() = %+v", a)
	}
}

func TestSaveRefusesAStaleVersion(t *testing.T) {
	s, _, _ := newStore(t, Config{})
	ctx := context.Background()
	sess, _ := s.Open(ctx, scopeOf("tenant-a", "user-1", "ses-1"))
	if _, err := s.Save(ctx, sess, nil); err != nil {
		t.Fatal(err)
	}
	// The second writer still holds version 1.
	_, err := s.Save(ctx, sess, nil)
	wantCode(t, err, contracts.ErrConflict)
}

func TestTurnWindowIsClippedAndPruned(t *testing.T) {
	s, _, db := newStore(t, Config{KeepTurns: 6, MaxTurnRunes: 20})
	ctx := context.Background()
	sc := scopeOf("tenant-a", "user-1", "ses-1")
	sess, _ := s.Open(ctx, sc)
	var err error
	for i := 1; i <= 10; i++ {
		rec := TurnRecord{TurnID: "trn_" + string(rune('a'+i)), User: "user turn " + strings.Repeat("x", 50), Act: "present_result", Assistant: "assistant turn"}
		if sess, err = s.Save(ctx, sess, &rec); err != nil {
			t.Fatal(err)
		}
	}
	turns, err := s.RecentTurns(ctx, sess, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 6 {
		t.Fatalf("kept %d turn rows, want 6 (3 turns)", len(turns))
	}
	if turns[0].N != 8 || turns[len(turns)-1].N != 10 || turns[0].Role != "user" || turns[1].Role != "assistant" {
		t.Fatalf("window = %+v", turns)
	}
	for _, tr := range turns {
		if n := len([]rune(tr.Body)); n > 23 { // 20 runes and the marker
			t.Fatalf("turn body is %d runes, want it clipped to 20: %q", n, tr.Body)
		}
	}
	last2, err := s.RecentTurns(ctx, sess, 2)
	if err != nil || len(last2) != 2 || last2[0].N != 10 || last2[0].Role != "user" || last2[1].Role != "assistant" {
		t.Fatalf("last two rows = %+v, %v", last2, err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_turns`).Scan(&n); err != nil || n != 6 {
		t.Fatalf("stored rows = %d (%v), want 6", n, err)
	}
	// Another tenant never reads this window.
	other, _ := s.Open(ctx, scopeOf("tenant-b", "user-9", "ses-1"))
	if tt, _ := s.RecentTurns(ctx, other, 100); len(tt) != 0 {
		t.Fatalf("tenant-b reads %d turns of tenant-a", len(tt))
	}
}

// Save rewrites only the lookup rows that changed: a reference the state still makes keeps its row,
// one it dropped goes, one it gained comes, and the rows are exactly the state's references.
func TestSaveRewritesOnlyTheChangedRefs(t *testing.T) {
	s, clk, db := newStore(t, Config{})
	ctx := context.Background()
	a := scopeCaps("tenant-a", "user-1", "ses-a", "items.create")
	sess := seed(t, s, a, func(st *State) { st.Goal = preparedGoal("items.create", "prep_a", "t1", "v1", clk.Now()) })
	// Another session's rows come after: a row deleted and inserted again would get a new seq.
	seed(t, s, scopeCaps("tenant-a", "user-1", "ses-b", "items.create"), func(st *State) {
		st.Goal = preparedGoal("items.create", "prep_b", "t2", "v1", clk.Now())
	})
	refRows := func() map[string]int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT kind, ref_type, ref_id, seq FROM session_refs
 WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND session_id = ?`, a.ApplicationID, a.TenantID, a.WorkspaceID, a.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int64{}
		for rows.Next() {
			var kind, typ, id string
			var seq int64
			if err := rows.Scan(&kind, &typ, &id, &seq); err != nil {
				t.Fatal(err)
			}
			out[kind+"/"+typ+"/"+id] = seq
		}
		return out
	}
	before := refRows()
	if len(before) != 2 {
		t.Fatalf("the session has %d lookup rows, want its capability and its entity: %v", len(before), before)
	}

	var err error
	if sess, err = s.Save(ctx, sess, &TurnRecord{TurnID: "trn_1", User: "hi", Assistant: "ok"}); err != nil {
		t.Fatal(err)
	}
	if got := refRows(); !maps.Equal(got, before) {
		t.Errorf("a save that changed no reference rewrote the rows: %v, were %v", got, before)
	}

	sess.State.Goal.Entities = []ResolvedEntity{{Field: "title", EntityType: "title", EntityID: "t9", Version: "v1"}}
	if sess, err = s.Save(ctx, sess, nil); err != nil {
		t.Fatal(err)
	}
	got := refRows()
	keep := refCapability + "//items.create"
	if len(got) != 2 || got[keep] != before[keep] || got[refEntity+"/title/t9"] == 0 {
		t.Errorf("after the entity changed: %v, were %v; want the capability row kept and only t9 beside it", got, before)
	}

	if err := s.End(ctx, a, &scopedInvalidator{}); err != nil {
		t.Fatal(err)
	}
	if got := refRows(); len(got) != 0 {
		t.Errorf("an ended session keeps lookup rows: %v", got)
	}
}

func TestExpireGoalDiscardsAnIdleGoal(t *testing.T) {
	g := &Goal{CapabilityID: "items.create", PreparedID: "prep_X", ExpiresAt: t0.Add(time.Minute)}
	sess := Session{State: State{Goal: g, Last: &DirectiveSummary{Act: "ask_missing"}}}
	if expired, _ := sess.ExpireGoal(t0); expired || sess.State.Goal == nil {
		t.Fatal("a goal before its expiry was discarded")
	}
	expired, prep := sess.ExpireGoal(t0.Add(time.Minute))
	if !expired || prep != "prep_X" || sess.State.Goal != nil || sess.State.Last != nil {
		t.Fatalf("expired=%v prep=%q state=%+v", expired, prep, sess.State)
	}
}

type recordingInvalidator struct {
	calls  []string
	reason act.PreparedActionInvalidationConditionsItem
	err    error
}

func (r *recordingInvalidator) Invalidate(_ context.Context, _ contracts.Scope, id string, reason act.PreparedActionInvalidationConditionsItem) (contracts.PreparedAction, error) {
	r.calls = append(r.calls, id)
	r.reason = reason
	return contracts.PreparedAction{}, r.err
}

func TestEndInvalidatesThePreparationAndClosesTheSession(t *testing.T) {
	s, _, _ := newStore(t, Config{})
	ctx := context.Background()
	sc := scopeOf("tenant-a", "user-1", "ses-1")
	sess, _ := s.Open(ctx, sc)
	sess.State.Goal = &Goal{CapabilityID: "items.create", Phase: capabilities.PhasePrepared, PreparedID: "prep_X"}
	if _, err := s.Save(ctx, sess, nil); err != nil {
		t.Fatal(err)
	}
	inv := &recordingInvalidator{}
	if err := s.End(ctx, sc, inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.calls) != 1 || inv.calls[0] != "prep_X" || inv.reason != act.PreparedActionInvalidationConditionsItemSessionEnded {
		t.Fatalf("invalidator calls = %v reason = %s", inv.calls, inv.reason)
	}
	_, err := s.Open(ctx, sc)
	wantCode(t, err, contracts.ErrExpired)
}

func TestEndKeepsTheSessionOpenWhenInvalidationFails(t *testing.T) {
	s, _, _ := newStore(t, Config{})
	ctx := context.Background()
	sc := scopeOf("tenant-a", "user-1", "ses-1")
	sess, _ := s.Open(ctx, sc)
	sess.State.Goal = &Goal{CapabilityID: "items.create", Phase: capabilities.PhasePrepared, PreparedID: "prep_X"}
	if _, err := s.Save(ctx, sess, nil); err != nil {
		t.Fatal(err)
	}
	err := s.End(ctx, sc, &recordingInvalidator{err: contracts.New(contracts.ErrUnavailable, "down")})
	wantCode(t, err, contracts.ErrUnavailable)
	if _, err := s.Open(ctx, sc); err != nil {
		t.Fatalf("the session must stay open (and confirmable state must not be orphaned): %v", err)
	}
	// A preparation that already left the confirmable states is not an obstacle.
	if err := s.End(ctx, sc, &recordingInvalidator{err: contracts.New(contracts.ErrConflict, "already committed")}); err != nil {
		t.Fatalf("End over an already-final preparation: %v", err)
	}
}

// A preparation the coordinator no longer holds (retention removed it once it was
// over) is not an obstacle either; any other scope refusal still is.
func TestEndOverAPreparationNoLongerStored(t *testing.T) {
	s, _, _ := newStore(t, Config{})
	ctx := context.Background()
	sc := scopeOf("tenant-a", "user-1", "ses-1")
	seed(t, s, sc, func(st *State) {
		st.Goal = &Goal{CapabilityID: "items.create", Phase: capabilities.PhasePrepared, PreparedID: "prep_X"}
	})
	err := s.End(ctx, sc, &recordingInvalidator{err: contracts.New(contracts.ErrScopeViolation, "another session")})
	wantCode(t, err, contracts.ErrScopeViolation)
	gone := contracts.New(contracts.ErrScopeViolation, "actions: no such preparation in this scope").WithDetails("reason", "not_found")
	if err := s.End(ctx, sc, &recordingInvalidator{err: gone}); err != nil {
		t.Fatalf("End over a preparation that is no longer stored: %v", err)
	}
	_, err = s.Open(ctx, sc)
	wantCode(t, err, contracts.ErrExpired)
}

// racingInvalidator runs race once, on its first call, before recording it: a
// writer that saves while the caller is voiding.
type racingInvalidator struct {
	scopedInvalidator
	race func()
}

func (r *racingInvalidator) Invalidate(ctx context.Context, sc contracts.Scope, id string, reason act.PreparedActionInvalidationConditionsItem) (contracts.PreparedAction, error) {
	if f := r.race; f != nil {
		r.race = nil
		f()
	}
	return r.scopedInvalidator.Invalidate(ctx, sc, id, reason)
}

// A turn that saves a new preparation while End voids the old one does not leave
// it confirmable in the ended session: End reads the session again and voids it.
func TestEndVoidsAPreparationSavedWhileItRan(t *testing.T) {
	s, _, _ := newStore(t, Config{})
	ctx := context.Background()
	sc := scopeOf("tenant-a", "user-1", "ses-1")
	seed(t, s, sc, func(st *State) {
		st.Goal = &Goal{CapabilityID: "items.create", Phase: capabilities.PhasePrepared, PreparedID: "prep_X"}
	})
	inv := &racingInvalidator{race: func() {
		sess := reopen(t, s, sc)
		sess.State.Goal.PreparedID = "prep_Y"
		if _, err := s.Save(ctx, sess, nil); err != nil {
			t.Fatal(err)
		}
	}}
	if err := s.End(ctx, sc, inv); err != nil {
		t.Fatal(err)
	}
	if ids := inv.ids(); len(ids) != 2 || ids[0] != "prep_X" || ids[1] != "prep_Y" {
		t.Fatalf("voided %v, want prep_X then prep_Y", ids)
	}
	_, err := s.Open(ctx, sc)
	wantCode(t, err, contracts.ErrExpired)
}

func TestStoredStateHoldsNoSecrets(t *testing.T) {
	s, _, db := newStore(t, Config{})
	ctx := context.Background()
	sc := scopeOf("tenant-a", "user-1", "ses-1")
	sess, _ := s.Open(ctx, sc)
	sess.State.Goal = &Goal{CapabilityID: "items.create", PreparedID: "prep_X"}
	if _, err := s.Save(ctx, sess, nil); err != nil {
		t.Fatal(err)
	}
	var stateJSON string
	if err := db.QueryRowContext(ctx, `SELECT state_json FROM sessions`).Scan(&stateJSON); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"cfm_", "idem_", "token", "secret", "password", "Bearer"} {
		if strings.Contains(stateJSON, banned) {
			t.Fatalf("stored state contains %q: %s", banned, stateJSON)
		}
	}
}
