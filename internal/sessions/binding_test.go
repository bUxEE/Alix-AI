package sessions

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// A session is bound to one tenant and workspace for life. Presenting the same
// session ID under another tenant or workspace is a new, empty session; it never
// inherits, reads or overwrites the first one, however the request is made.
//
// SEC-025
func TestTenantSwitchRequiresNewSession(t *testing.T) {
	s, clk, db := newStore(t, Config{})
	ctx := context.Background()
	scA := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	scB := scopeCaps("tenant-b", "user-1", "ses-1", "items.create")
	scWS := inWorkspace(scA, "ws-2")

	a := seed(t, s, scA, func(st *State) { st.Goal = preparedGoal("items.create", "prep_A", "t1", "v1", clk.Now()) })

	// The same token claims, another tenant or workspace: a fresh session, version 1,
	// bound to the scope that presented it.
	for name, sc := range map[string]contracts.Scope{"tenant": scB, "workspace": scWS} {
		other, err := s.Open(ctx, sc)
		if err != nil {
			t.Fatalf("%s switch: %v", name, err)
		}
		if other.State.Goal != nil || other.Version != 1 || other.seq == a.seq {
			t.Fatalf("%s switch inherited the first session: %+v", name, other)
		}
		if other.Scope.TenantID != sc.TenantID || other.Scope.WorkspaceID != sc.WorkspaceID {
			t.Fatalf("%s switch: the new session is bound to %+v", name, other.Scope)
		}
	}
	if got := reopen(t, s, scA); got.Version != a.Version || got.State.Goal.PreparedID != "prep_A" || got.Scope.TenantID != "tenant-a" {
		t.Fatalf("the first session changed: %+v", got)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM sessions WHERE id = 'ses-1'`); n != 3 {
		t.Fatalf("%d session rows for the three contexts, want 3", n)
	}

	// The new session saves into its own row. Putting tenant-a's Session value in
	// front of tenant-b's row, with the version made to match, cannot overwrite it.
	b := seed(t, s, scB, func(st *State) { st.Goal = newGoal("items.get", clk.Now(), nil) })
	if b.Version != a.Version {
		t.Fatalf("the test wants equal versions: %d %d", a.Version, b.Version)
	}
	forged := a
	forged.Scope = scB
	forged.State = State{Goal: newGoal("items.create", clk.Now(), map[string]Slot{"name": {Value: "evil", Origin: OriginUserTurn}})}
	if _, err := s.Save(ctx, forged, nil); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("saving tenant-a's session value as tenant-b: %v", err)
	}
	if got := reopen(t, s, scB); got.State.Goal.CapabilityID != "items.get" || got.Version != b.Version {
		t.Fatalf("tenant-b's session was overwritten: %+v", got.State.Goal)
	}
	// And the other way round.
	back := b
	back.Scope = scA
	if _, err := s.Save(ctx, back, nil); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("saving tenant-b's session value as tenant-a: %v", err)
	}

	// The database itself refuses to rebind a row: no statement can move a session
	// to another tenant, workspace, principal or ID.
	for _, set := range []string{"tenant_id = 'tenant-b'", "workspace_id = 'ws-9'", "application_id = 'other-app'", "principal_id = 'user-2'", "id = 'ses-2'", "scope_json = '{}'"} {
		_, err := db.ExecContext(ctx, `UPDATE sessions SET `+set+` WHERE tenant_id = 'tenant-a' AND workspace_id = 'ws-1' AND id = 'ses-1'`)
		if err == nil || !strings.Contains(err.Error(), "bound to one scope for life") {
			t.Errorf("UPDATE sessions SET %s: error = %v, want the row refused", set, err)
		}
	}
	if got := reopen(t, s, scA); got.Scope.TenantID != "tenant-a" || got.Scope.PrincipalID != "user-1" {
		t.Fatalf("a refused statement still changed the binding: %+v", got.Scope)
	}

	// Another principal of the same tenant cannot take the session over either.
	_, err := s.Open(ctx, scopeCaps("tenant-a", "user-2", "ses-1", "items.create"))
	if contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Fatalf("another principal: %v", err)
	}

	// An event for tenant-b reaches tenant-b's sessions only.
	ivr := &scopedInvalidator{}
	if _, err := s.InvalidateWorkspace(ctx, service("tenant-b"), Invalidation{Kind: KindApplicationEvent, Everything: true}, ivr); err != nil {
		t.Fatal(err)
	}
	if len(ivr.calls) != 0 || reopen(t, s, scA).State.Goal.PreparedID != "prep_A" {
		t.Fatalf("an event of tenant-b reached tenant-a: calls %v", ivr.ids())
	}
	imp, err := s.Invalidate(ctx, scB, Invalidation{Kind: KindApplicationEvent, Everything: true}, ivr)
	if err != nil || len(ivr.calls) != 0 || imp.Voided != nil {
		t.Fatalf("Invalidate as tenant-b = %+v %v, calls %v", imp, err, ivr.ids())
	}
	if _, err := s.Invalidate(ctx, scopeCaps("tenant-a", "user-2", "ses-1", "items.create"), Invalidation{Kind: KindTimeout}, ivr); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Fatalf("Invalidate as another principal: %v", err)
	}
}

func TestSaveRefusesASessionNotReadFromTheStore(t *testing.T) {
	s, _, _ := newStore(t, Config{})
	sc := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	if _, err := s.Save(context.Background(), Session{ID: sc.SessionID, Scope: sc, Version: 1}, nil); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("saving a hand-built session: %v", err)
	}
}

// A state written by the minimal M1-14 form (no goal ID, no suspended goals, no
// capsules) is read as it was and saved forward with the new members filled in.
func TestStateOfTheMinimalFormStillDecodes(t *testing.T) {
	s, clk, db := newStore(t, Config{})
	ctx := context.Background()
	sc := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	seed(t, s, sc, func(*State) {})
	legacy := `{"goal":{"capability_id":"items.create","phase":"prepared","slots":{"name":{"value":"x","origin":"user_turn"},"count":{"value":3,"origin":"correction"}},` +
		`"pending":{"field":"q","kind":"choice","choices":[{"choice_id":"c1","label":"One","value":{"kind":"identifier","value":"c1"}}],"entity_type":"item","version":"v1"},` +
		`"prepared_id":"prep_X","confirmed":true,"entities":[{"field":"item_id","entity_type":"item","entity_id":"c1","version":"v1"}],` +
		`"started_at":"2026-10-02T09:30:00Z","updated_at":"2026-10-02T09:31:00Z","expires_at":"2026-10-02T10:00:00Z"},` +
		`"last":{"turn_id":"trn_1","act":"ask_missing","missing_fields":["q"]}}`
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET state_json = ? WHERE id = 'ses-1'`, legacy); err != nil {
		t.Fatal(err)
	}
	sess := reopen(t, s, sc)
	g := sess.State.Goal
	if g == nil || g.PreparedID != "prep_X" || !g.Confirmed || g.ID != "" || g.Slots["name"].Origin != OriginUserTurn || g.Slots["count"].Origin != OriginCorrection ||
		g.Pending == nil || len(g.Pending.Choices) != 1 || len(g.Entities) != 1 || sess.State.Last.Act != "ask_missing" || len(sess.State.Suspended) != 0 {
		t.Fatalf("legacy state = %+v", sess.State)
	}
	if n, ok := g.Slots["count"].Value.(json.Number); !ok || n.String() != "3" {
		t.Fatalf("count = %#v", g.Slots["count"].Value)
	}
	saved, err := s.Save(ctx, sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopen(t, s, sc); got.State.Goal.ID == "" || got.State.Goal.PreparedID != "prep_X" || got.Version != saved.Version {
		t.Fatalf("after saving forward: %+v", got.State.Goal)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM session_refs WHERE session_id = 'ses-1'`); n != 2 { // the capability, and the record (resolved and offered: one reference)
		t.Fatalf("%d lookup rows, want 2", n)
	}
	_ = clk
}

// A database that has only M1-14's schema takes migration 0002 and the transcript
// module over its existing rows.
func TestMigrationOverTheMinimalSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	first, err := migrations.ReadFile("migrations/0001_sessions.sql")
	if err != nil {
		t.Fatal(err)
	}
	old := store.NewRegistry()
	if err := old.Register("sessions", fstest.MapFS{"migrations/0001_sessions.sql": {Data: first}}); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, store.Options{Path: path, Migrations: old})
	if err != nil {
		t.Fatal(err)
	}
	sc := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	scopeJSON, _ := json.Marshal(sc)
	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, application_id, tenant_id, workspace_id, principal_id, scope_json, status, state_json, version, turn_count, created_at, updated_at)
		VALUES ('ses-1', 'demo-app', 'tenant-a', 'ws-1', 'user-1', ?, 'active', '{"goal":{"capability_id":"items.create","phase":"","slots":{},"started_at":"2026-10-02T09:30:00Z","updated_at":"2026-10-02T09:30:00Z","expires_at":"2026-10-02T10:00:00Z"}}', 7, 3, 1, 1)`, string(scopeJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reg := store.NewRegistry()
	if err := reg.Register("sessions", migrations); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register("transcript", transcriptFS()); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, store.Options{Path: path, Migrations: reg})
	if err != nil {
		t.Fatalf("migrating an M1-14 database: %v", err)
	}
	defer db.Close()
	s, err := New(db, clock.NewFake(t0), Config{})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.Open(ctx, sc)
	if err != nil || sess.Version != 7 || sess.State.Goal == nil || sess.State.Goal.CapabilityID != "items.create" {
		t.Fatalf("existing row = %+v %v", sess, err)
	}
	saved, err := s.Save(ctx, sess, &TurnRecord{TurnID: "trn_1", User: "hello", Act: "present_result", Assistant: "hi"})
	if err != nil || saved.Version != 8 || saved.TurnCount != 4 {
		t.Fatalf("saving over the migrated row = %+v %v", saved, err)
	}
	if rows, err := s.Transcript(ctx, sc, 0, 10); err != nil || len(rows) != 2 {
		t.Fatalf("transcript = %v %v", rows, err)
	}
}
