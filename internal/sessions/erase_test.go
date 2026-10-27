package sessions

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// eraseSeed opens the session of sc, gives it a goal (so it has lookup rows), saves
// one turn and, when ended, ends it.
func eraseSeed(t *testing.T, s *Store, sc contracts.Scope, text string, ended bool) {
	t.Helper()
	ctx := context.Background()
	sess, err := s.Open(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	sess.State.Goal = newGoal("items.create", t0, nil)
	if _, err := s.Save(ctx, sess, &TurnRecord{TurnID: "trn_1", User: text, Act: "present_result", Assistant: "reply " + text}); err != nil {
		t.Fatal(err)
	}
	if ended {
		if err := s.End(ctx, sc, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// eraseRun erases spec in transactions of at most eraseBatch rows until a batch is
// not full, and returns the totals.
func eraseRun(t *testing.T, db *store.DB, spec EraseSpec) (total EraseOutcome) {
	t.Helper()
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatal("the erasure does not end")
		}
		var out EraseOutcome
		if err := db.Tx(context.Background(), func(tx *store.Tx) (err error) {
			out, err = EraseHistory(context.Background(), tx, spec)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if n := out.TranscriptTurns + out.SessionTurns + out.Sessions + out.Ended; n > eraseBatch {
			t.Fatalf("one transaction changed %d counted rows, more than the batch of %d", n, eraseBatch)
		}
		total.TranscriptTurns += out.TranscriptTurns
		total.SessionTurns += out.SessionTurns
		total.Sessions += out.Sessions
		total.Ended += out.Ended
		if !out.More {
			return total
		}
	}
}

// An erasure takes the transcript, the recent windows, the lookup rows and the
// session rows of a principal (ending an active session first) in batches, and
// nothing of another principal, even one whose session has the same id in another
// workspace, or of another tenant. The counts CountHistory gives before are what
// the erasure removes.
func TestEraseHistoryOfAPrincipalAndATenant(t *testing.T) {
	s, clk, db := newStore(t, Config{})
	ctx := context.Background()
	old := eraseBatch
	eraseBatch = 3
	t.Cleanup(func() { eraseBatch = old })

	alice1 := scopeCaps("tenant-a", "alice", "ses-1", "items.create")
	alice2 := inWorkspace(scopeCaps("tenant-a", "alice", "ses-2", "items.create"), "ws-2")
	bob := inWorkspace(scopeCaps("tenant-a", "bob", "ses-1", "items.create"), "ws-2") // alice's first session id, another workspace
	other := scopeCaps("tenant-b", "alice", "ses-1", "items.create")
	eraseSeed(t, s, alice1, "alice one", true)
	eraseSeed(t, s, alice2, "alice two", false)
	eraseSeed(t, s, bob, "bob", true)
	eraseSeed(t, s, other, "other tenant", false)

	spec := EraseSpec{ApplicationID: "demo-app", TenantID: "tenant-a", PrincipalID: "alice", Now: clk.Now()}
	counted, err := CountHistory(ctx, db, spec)
	if err != nil {
		t.Fatal(err)
	}
	want := EraseOutcome{TranscriptTurns: 4, SessionTurns: 4, Sessions: 2, Ended: 1}
	if counted != want {
		t.Fatalf("CountHistory = %+v, want %+v", counted, want)
	}
	if got := eraseRun(t, db, spec); got != want {
		t.Fatalf("the erasure removed %+v, want what was counted: %+v", got, want)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM transcript_turns WHERE tenant_id = 'tenant-a' AND principal_id = 'alice'`,
		`SELECT COUNT(*) FROM sessions WHERE tenant_id = 'tenant-a' AND principal_id = 'alice'`,
		`SELECT COUNT(*) FROM session_turns WHERE tenant_id = 'tenant-a' AND (workspace_id = 'ws-1' OR session_id = 'ses-2')`,
		`SELECT COUNT(*) FROM session_refs WHERE tenant_id = 'tenant-a' AND (workspace_id = 'ws-1' OR session_id = 'ses-2')`,
	} {
		if n := countRows(t, db, q); n != 0 {
			t.Errorf("%s = %d after the erasure", q, n)
		}
	}
	// Bob's session (same id, other workspace) and the other tenant's are untouched.
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM transcript_turns WHERE tenant_id = 'tenant-a' AND principal_id = 'bob'`:                        2,
		`SELECT COUNT(*) FROM session_turns WHERE tenant_id = 'tenant-a' AND workspace_id = 'ws-2' AND session_id = 'ses-1'`: 2,
		`SELECT COUNT(*) FROM sessions WHERE tenant_id = 'tenant-a' AND principal_id = 'bob' AND status = 'ended'`:           1,
		`SELECT COUNT(*) FROM transcript_turns WHERE tenant_id = 'tenant-b'`:                                                 2,
		`SELECT COUNT(*) FROM session_turns WHERE tenant_id = 'tenant-b'`:                                                    2,
		`SELECT COUNT(*) FROM session_refs WHERE tenant_id = 'tenant-b'`:                                                     1,
		`SELECT COUNT(*) FROM sessions WHERE tenant_id = 'tenant-b' AND status = 'active'`:                                   1,
	} {
		if n := countRows(t, db, q); n != want {
			t.Errorf("%s = %d, want %d", q, n, want)
		}
	}
	// The erased active session is gone, not ended and kept: its id opens a new, empty session.
	if sess, err := s.Open(ctx, alice2); err != nil || sess.TurnCount != 0 || sess.State.Goal != nil {
		t.Errorf("the erased session's id opens %+v, %v; want a new, empty session", sess, err)
	}

	// The whole tenant: everything of tenant-a, nothing of tenant-b.
	spec = EraseSpec{ApplicationID: "demo-app", TenantID: "tenant-a", Now: clk.Now()}
	if counted, err = CountHistory(ctx, db, spec); err != nil {
		t.Fatal(err)
	}
	want = EraseOutcome{TranscriptTurns: 2, SessionTurns: 2, Sessions: 2, Ended: 1} // bob's, and alice's new session
	if counted != want {
		t.Fatalf("CountHistory of the tenant = %+v, want %+v", counted, want)
	}
	if got := eraseRun(t, db, spec); got != want {
		t.Fatalf("the tenant's erasure removed %+v, want %+v", got, want)
	}
	for _, table := range []string{"transcript_turns", "session_turns", "session_refs", "sessions"} {
		if n := countRows(t, db, `SELECT COUNT(*) FROM `+table+` WHERE tenant_id = 'tenant-a'`); n != 0 {
			t.Errorf("%s: %d rows of the tenant are left", table, n)
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM `+table+` WHERE tenant_id = 'tenant-b'`); n == 0 {
			t.Errorf("%s: the other tenant's rows are gone", table)
		}
	}
	if got := eraseRun(t, db, spec); got != (EraseOutcome{}) {
		t.Errorf("a second erasure removed %+v", got)
	}
}

func TestEraseHistoryWorkspaceFilter(t *testing.T) {
	s, clk, db := newStore(t, Config{})
	ctx := context.Background()
	alice1 := scopeCaps("tenant-a", "alice", "ses-1", "items.create")
	alice2 := inWorkspace(scopeCaps("tenant-a", "alice", "ses-2", "items.create"), "ws-2")
	eraseSeed(t, s, alice1, "alice one", true)
	eraseSeed(t, s, alice2, "alice two", false)

	wsSpec := EraseSpec{ApplicationID: "demo-app", TenantID: "tenant-a", WorkspaceID: "ws-1", PrincipalID: "alice", Now: clk.Now()}
	before, err := CountHistory(ctx, db, wsSpec)
	if err != nil || before.Sessions == 0 {
		t.Fatalf("CountHistory = %+v %v", before, err)
	}
	if got := eraseRun(t, db, wsSpec); got.Sessions != before.Sessions {
		t.Fatalf("workspace erase = %+v before = %+v", got, before)
	}
	left, err := CountHistory(ctx, db, EraseSpec{ApplicationID: "demo-app", TenantID: "tenant-a", WorkspaceID: "ws-2", PrincipalID: "alice", Now: clk.Now()})
	if err != nil || left.Sessions == 0 {
		t.Fatalf("other workspace wiped: %+v %v", left, err)
	}
	gone, err := CountHistory(ctx, db, wsSpec)
	if err != nil || gone.Sessions != 0 {
		t.Fatalf("target workspace still has %+v %v", gone, err)
	}
}

// An erasure always names an application and a tenant, and the time an active
// session is ended at.
func TestEraseHistoryRefusesAnIncompleteSpec(t *testing.T) {
	_, clk, db := newStore(t, Config{})
	for name, spec := range map[string]EraseSpec{
		"no application": {TenantID: "tenant-a", Now: clk.Now()},
		"no tenant":      {ApplicationID: "demo-app", Now: clk.Now()},
		"no time":        {ApplicationID: "demo-app", TenantID: "tenant-a"},
	} {
		if _, err := CountHistory(context.Background(), db, spec); err == nil && name != "no time" {
			t.Errorf("%s: CountHistory accepted it", name)
		}
		err := db.Tx(context.Background(), func(tx *store.Tx) error {
			_, err := EraseHistory(context.Background(), tx, spec)
			return err
		})
		if err == nil {
			t.Errorf("%s: EraseHistory accepted it", name)
		}
	}
}
