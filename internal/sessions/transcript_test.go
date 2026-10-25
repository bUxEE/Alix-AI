package sessions

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The transcript is the full history the user reads back; the model context is
// state plus a short clipped window. They are stored apart, and building context
// never reads the transcript.
func TestTranscriptSeparateFromContext(t *testing.T) {
	s, clk, db := newStore(t, Config{KeepTurns: 6, MaxTurnRunes: 30, MaxTranscriptRunes: 200})
	ctx := context.Background()
	sc := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	long := strings.Repeat("word ", 30) // 150 runes: far over the context window's clip, under the transcript's
	huge := strings.Repeat("y", 500)    // over both

	sess, err := s.Open(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	sess.State.Goal = newGoal("items.create", clk.Now(), map[string]Slot{"name": {Value: "widget", Origin: OriginUserTurn}})
	for i := 1; i <= 10; i++ {
		user := long
		if i == 10 {
			user = huge
		}
		rec := TurnRecord{TurnID: fmt.Sprintf("trn_%02d", i), User: user, Act: "present_result", Assistant: fmt.Sprintf("answer %d", i)}
		if sess, err = s.Save(ctx, sess, &rec); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
	}

	// The transcript holds every turn, in order, at its own (far higher) bound.
	rows, err := s.Transcript(ctx, sc, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 20 {
		t.Fatalf("transcript has %d rows, want all 20", len(rows))
	}
	if rows[0].Role != "user" || rows[0].Body != long || rows[0].Clipped || rows[0].TurnID != "trn_01" || rows[1].Role != "assistant" || rows[1].Body != "answer 1" || rows[1].Act != "present_result" {
		t.Fatalf("first turn = %+v / %+v", rows[0], rows[1])
	}
	last := rows[18]
	if n := len([]rune(last.Body)); !last.Clipped || n != 203 {
		t.Fatalf("an over-long turn is clipped at the transcript bound: clipped=%v, %d runes", last.Clipped, n)
	}
	if n, err := s.TranscriptLen(ctx, sc); err != nil || n != 20 {
		t.Fatalf("TranscriptLen = %d %v", n, err)
	}

	// The context window holds a few clipped turns.
	window, err := s.RecentTurns(ctx, sess, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(window) != 6 || window[0].N != 8 {
		t.Fatalf("window = %d rows from turn %d, want the last 3 turns", len(window), window[0].N)
	}
	for _, w := range window {
		if n := len([]rune(w.Body)); n > 33 {
			t.Fatalf("a window turn is %d runes: %q", n, w.Body)
		}
	}

	// Pages continue after a turn.
	page, err := s.Transcript(ctx, sc, 8, 100)
	if err != nil || len(page) != 4 || page[0].N != 9 {
		t.Fatalf("after turn 8: %d rows %v", len(page), err)
	}
	if page, err = s.Transcript(ctx, sc, 0, 3); err != nil || len(page) != 3 || page[2].N != 2 {
		t.Fatalf("limit 3: %+v %v", page, err)
	}

	// Another tenant's session with the same ID has its own, empty, transcript;
	// another principal of this tenant is refused.
	if rows, err := s.Transcript(ctx, scopeCaps("tenant-b", "user-1", "ses-1", "items.create"), 0, 100); err != nil || len(rows) != 0 {
		t.Fatalf("tenant-b reads %d rows of tenant-a: %v", len(rows), err)
	}
	if _, err := s.Transcript(ctx, scopeCaps("tenant-a", "user-2", "ses-1", "items.create"), 0, 100); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Fatalf("another principal reads the transcript: %v", err)
	}
	if _, err := s.Transcript(ctx, contracts.Scope{}, 0, 1); contracts.CodeOf(err) != contracts.ErrScopeMissing {
		t.Fatalf("no scope: %v", err)
	}

	// A save that loses the race writes no transcript either.
	if _, err := s.Save(ctx, Session{ID: sess.ID, Scope: sess.Scope, Version: 1, seq: sess.seq}, &TurnRecord{TurnID: "trn_x", User: "late", Assistant: "late"}); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("stale save: %v", err)
	}
	if n, _ := s.TranscriptLen(ctx, sc); n != 20 {
		t.Fatalf("a refused save left transcript rows: %d", n)
	}

	// Ending the session does not delete the history; clearing it does, and touches
	// neither the state nor the context window.
	if err := s.End(ctx, sc, nil); err != nil { // the goal holds no preparation, so End needs no invalidator
		t.Fatal(err)
	}
	if n, _ := s.TranscriptLen(ctx, sc); n != 20 {
		t.Fatalf("End deleted the transcript: %d rows left", n)
	}
	if _, err := s.Open(ctx, sc); contracts.CodeOf(err) != contracts.ErrExpired {
		t.Fatalf("open after End: %v", err)
	}

	s2, _, _ := newStore(t, Config{KeepTurns: 6})
	sc2 := scopeCaps("tenant-a", "user-1", "ses-2", "items.create")
	live := seed(t, s2, sc2, func(st *State) {
		st.Goal = preparedGoal("items.create", "prep_C", "t1", "v1", t0)
	})
	if live, err = s2.Save(ctx, live, &TurnRecord{TurnID: "trn_1", User: "hello", Act: "ask_confirm", Assistant: "sure?"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s2.ClearTranscript(ctx, sc2); err != nil || n != 2 {
		t.Fatalf("ClearTranscript = %d %v", n, err)
	}
	after := reopen(t, s2, sc2)
	if g := after.State.Goal; g == nil || g.PreparedID != "prep_C" || g.Phase != capabilities.PhasePrepared {
		t.Fatalf("clearing history changed the session state: %+v", g)
	}
	if w, err := s2.RecentTurns(ctx, after, 10); err != nil || len(w) != 2 {
		t.Fatalf("clearing history changed the context window: %v %v", w, err)
	}
	if _, err := s2.ClearTranscript(ctx, scopeCaps("tenant-a", "user-2", "ses-2", "items.create")); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Fatalf("another principal clears the transcript: %v", err)
	}

	// The model's context is built from state and the window alone: with the
	// transcript table gone, opening the session and reading the window still work.
	if _, err := db.ExecContext(ctx, `DROP TABLE transcript_turns`); err != nil {
		t.Fatal(err)
	}
	fresh := scopeCaps("tenant-a", "user-1", "ses-9", "items.create")
	got, err := s.Open(ctx, fresh)
	if err != nil {
		t.Fatalf("Open without a transcript table: %v", err)
	}
	if got, err = s.Save(ctx, got, nil); err != nil {
		t.Fatalf("a state-only Save without a transcript table: %v", err)
	}
	if _, err := s.RecentTurns(ctx, got, 10); err != nil {
		t.Fatalf("RecentTurns without a transcript table: %v", err)
	}
}

func TestPruneTranscriptIsScopedAndByAge(t *testing.T) {
	s, clk, _ := newStore(t, Config{})
	ctx := context.Background()
	scA := scopeCaps("tenant-a", "user-1", "ses-1", "items.create")
	scAws := inWorkspace(scA, "ws-2")
	scB := scopeCaps("tenant-b", "user-1", "ses-1", "items.create")
	write := func(sc contracts.Scope, text string) {
		sess := reopen(t, s, sc)
		if _, err := s.Save(ctx, sess, &TurnRecord{TurnID: "trn", User: text, Assistant: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, sc := range []contracts.Scope{scA, scAws, scB} {
		write(sc, "old")
	}
	clk.Advance(time.Hour)
	for _, sc := range []contracts.Scope{scA, scAws, scB} {
		write(sc, "new")
	}

	n, err := s.PruneTranscript(ctx, service("tenant-a"), clk.Now().Add(-30*time.Minute))
	if err != nil || n != 2 {
		t.Fatalf("pruned %d rows, want the 2 old rows of tenant-a ws-1: %v", n, err)
	}
	if rows, _ := s.Transcript(ctx, scA, 0, 10); len(rows) != 2 || rows[0].Body != "new" {
		t.Fatalf("tenant-a ws-1 = %+v", rows)
	}
	for name, sc := range map[string]contracts.Scope{"other workspace": scAws, "other tenant": scB} {
		if rows, _ := s.Transcript(ctx, sc, 0, 10); len(rows) != 4 {
			t.Fatalf("%s lost rows to a prune of another scope: %d left", name, len(rows))
		}
	}
	if _, err := s.PruneTranscript(ctx, contracts.Scope{}, clk.Now()); contracts.CodeOf(err) != contracts.ErrScopeMissing {
		t.Fatalf("prune without a scope: %v", err)
	}
}

// A prune deletes in bounded batches, each its own transaction, so it never holds
// the single writer for the whole delete: a failure in a later batch leaves the
// earlier ones done. The batches are scoped like the delete.
func TestPruneTranscriptDeletesInBatches(t *testing.T) {
	s, _, db := newStore(t, Config{})
	ctx := context.Background()
	old := pruneBatch
	pruneBatch = 2
	t.Cleanup(func() { pruneBatch = old })
	insert := func(tenant, body string, created int64) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO transcript_turns
 (application_id, tenant_id, workspace_id, session_id, principal_id, turn_id, n, role, act, body, created_at)
 VALUES ('demo-app', ?, 'ws-1', 'ses-1', 'user-1', 'trn', ?, 'user', '', ?, ?)`, tenant, created, body, created); err != nil {
			t.Fatal(err)
		}
	}
	for i := int64(1); i <= 3; i++ {
		insert("tenant-b", "older, other tenant", i) // older than all of tenant-a: an unscoped batch would pick these
	}
	for i := int64(10); i < 15; i++ {
		insert("tenant-a", "old", i)
	}
	insert("tenant-a", "poison", 15)
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER test_poison BEFORE DELETE ON transcript_turns WHEN old.body = 'poison'
 BEGIN SELECT RAISE(ABORT, 'poison row'); END`); err != nil {
		t.Fatal(err)
	}
	inA := `SELECT COUNT(*) FROM transcript_turns WHERE tenant_id = 'tenant-a'`
	inB := `SELECT COUNT(*) FROM transcript_turns WHERE tenant_id = 'tenant-b'`
	cutoff := time.UnixMilli(100)

	if _, err := s.PruneTranscript(ctx, service("tenant-a"), cutoff); err == nil {
		t.Fatal("the poisoned prune succeeded")
	}
	if n := countRows(t, db, inA); n >= 6 {
		t.Fatalf("%d of 6 rows left after a failure in the last batch: the earlier batches were not committed on their own", n)
	}
	if n := countRows(t, db, inB); n != 3 {
		t.Fatalf("tenant-b has %d rows, want 3", n)
	}

	if _, err := db.ExecContext(ctx, `DROP TRIGGER test_poison`); err != nil {
		t.Fatal(err)
	}
	left := countRows(t, db, inA)
	for i := int64(20); i < 25; i++ {
		insert("tenant-a", "old", i)
	}
	n, err := s.PruneTranscript(ctx, service("tenant-a"), cutoff)
	if err != nil || n != int64(left+5) {
		t.Fatalf("pruned %d rows, %v; want %d", n, err, left+5)
	}
	if got := countRows(t, db, inA); got != 0 {
		t.Errorf("tenant-a has %d rows left", got)
	}
	if got := countRows(t, db, inB); got != 3 {
		t.Errorf("tenant-b has %d rows, want 3", got)
	}
}

// The recent window of an ENDED session is a copy of its last turns that nothing reads any more:
// it goes with the transcript, by the same scope and cutoff, so it cannot outlive a retention
// policy or a clear. An ACTIVE session keeps its window whatever its age: the dialogue engine
// reads it every turn. A session ID is unique only inside its application, tenant and workspace,
// so a session that ended elsewhere under the same ID never stands for this one.
func TestClearAndPruneTakeTheWindowOfEndedSessionsOnly(t *testing.T) {
	s, clk, db := newStore(t, Config{})
	ctx := context.Background()
	old := pruneBatch
	pruneBatch = 1
	t.Cleanup(func() { pruneBatch = old })
	sc := func(tenant, ws, session string) contracts.Scope {
		return inWorkspace(scopeCaps(tenant, "user-1", session, "items.create"), ws)
	}
	var (
		ended       = sc("tenant-a", "ws-1", "ses-ended")
		active      = sc("tenant-a", "ws-1", "ses-x")
		endedTenant = sc("tenant-b", "ws-1", "ses-x") // ended, same ID as active, other tenant
		endedWs     = sc("tenant-a", "ws-2", "ses-x") // ended, same ID as active, other workspace
	)
	all := []contracts.Scope{ended, active, endedTenant, endedWs}
	for i := range 2 {
		for _, c := range all {
			sess := reopen(t, s, c)
			if _, err := s.Save(ctx, sess, &TurnRecord{TurnID: fmt.Sprintf("trn_%d", i), User: "hello", Assistant: "ok"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []contracts.Scope{ended, endedTenant, endedWs} {
		if err := s.End(ctx, c, nil); err != nil {
			t.Fatal(err)
		}
	}
	window := func(c contracts.Scope) int {
		t.Helper()
		return countRows(t, db, `SELECT COUNT(*) FROM session_turns WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND session_id = ?`,
			c.ApplicationID, c.TenantID, c.WorkspaceID, c.SessionID)
	}
	// want lists the window rows of ended, active, endedTenant and endedWs, in that order.
	want := func(when string, counts ...int) {
		t.Helper()
		for i, c := range all {
			if got := window(c); got != counts[i] {
				t.Errorf("%s: %s/%s/%s keeps %d window rows, want %d", when, c.TenantID, c.WorkspaceID, c.SessionID, got, counts[i])
			}
		}
	}
	clk.Advance(time.Hour)

	// A cutoff before the turns takes nothing.
	if n, err := s.PruneTranscript(ctx, service("tenant-a"), t0); err != nil || n != 0 {
		t.Fatalf("prune before the turns: %d %v", n, err)
	}
	want("prune before the turns", 4, 4, 4, 4)

	// A prune past them takes the transcript of tenant-a ws-1 and the window of its ended session.
	if n, err := s.PruneTranscript(ctx, service("tenant-a"), clk.Now()); err != nil || n != 8 {
		t.Fatalf("prune: %d rows %v, want the 8 transcript rows of tenant-a ws-1", n, err)
	}
	want("after the prune", 0, 4, 4, 4)

	// Clearing a session's transcript takes its window when it has ended, and only then.
	if n, err := s.ClearTranscript(ctx, endedWs); err != nil || n != 4 {
		t.Fatalf("clear of an ended session: %d %v", n, err)
	}
	if n, err := s.ClearTranscript(ctx, active); err != nil || n != 0 {
		t.Fatalf("clear of an active session: %d %v", n, err)
	}
	want("after the clears", 0, 4, 4, 0)
	if w, err := s.RecentTurns(ctx, reopen(t, s, active), 10); err != nil || len(w) != 4 {
		t.Fatalf("the active session's window: %d rows %v", len(w), err)
	}
}

// Every table of both modules carries the scope columns.
func TestSessionAndTranscriptTablesCarryTheScopeColumns(t *testing.T) {
	_, _, db := newStore(t, Config{})
	storetest.AssertScopedTables(t, db)
	for _, table := range []string{"sessions", "session_turns", "session_refs", "transcript_turns"} {
		if n := countRows(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table); n != 1 {
			t.Errorf("table %s is missing", table)
		}
	}
}
