package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// retention erase and the conversation history.

// retentionHistorySessions are the conversations retentionHistoryWorld adds, one turn each (a user row
// and an assistant row, in the transcript and in the session's recent window): Alice in two workspaces
// of tenant-a (one session ended, one active), Bob in tenant-a (one ended under the id of Alice's
// ended session, in another workspace, and one active), and Alice in tenant-b. The word is in both
// rows of the turn.
var retentionHistorySessions = []struct {
	tenant, workspace, principal, session, word string
	ended                                       bool
}{
	{"tenant-a", "ws-1", "usr_alice", "ses_h1", "alicefirst", true},
	{"tenant-a", "ws-2", "usr_alice", "ses_h2", "alicesecond", false},
	{"tenant-a", "ws-2", "usr_bob", "ses_h1", "bobended", true},
	{"tenant-a", "ws-1", "usr_bob", "ses_h3", "bobactive", false},
	{"tenant-b", "ws-1", "usr_alice", "ses_h1", "alicetenantb", true},
}

func retentionHistoryScope(tenant, workspace, principal, session string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-1", ApplicationID: retentionTestApp, TenantID: tenant, WorkspaceID: workspace, PrincipalID: principal,
		Roles: []string{"member"}, CapabilityScopes: []string{history.CapSearch}, SessionID: session,
	}
}

// retentionHistoryWorld is retentionEraseWorld with the conversations of retentionHistorySessions,
// written by the session store itself.
func retentionHistoryWorld(t *testing.T) (cfg, dataDir string) {
	t.Helper()
	cfg, dataDir = retentionEraseWorld(t)
	retentionData(t, dataDir, func(db *store.DB) {
		ctx := context.Background()
		ss, err := sessions.New(db, clock.NewFake(time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)), sessions.Config{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range retentionHistorySessions {
			sc := retentionHistoryScope(c.tenant, c.workspace, c.principal, c.session)
			sess, err := ss.Open(ctx, sc)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ss.Save(ctx, sess, &sessions.TurnRecord{TurnID: "trn_1", User: "about " + c.word, Act: "present_result", Assistant: "reply " + c.word}); err != nil {
				t.Fatal(err)
			}
			if c.ended {
				if err := ss.End(ctx, sc, nil); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	return cfg, dataDir
}

// retentionSearch is how many hits history search gives the principal for the word in a workspace,
// after FTS5 has checked that its index matches the transcript table.
func retentionSearch(t *testing.T, dataDir, tenant, workspace, principal, word string) (hits int) {
	t.Helper()
	retentionData(t, dataDir, func(db *store.DB) {
		ctx := context.Background()
		if _, err := db.ExecContext(ctx, `INSERT INTO history_fts (history_fts, rank) VALUES ('integrity-check', 1)`); err != nil {
			t.Fatalf("the search index is out of step with the transcript: %v", err)
		}
		ss, err := sessions.New(db, nil, sessions.Config{})
		if err != nil {
			t.Fatal(err)
		}
		lg, err := audit.New(db, audit.Options{})
		if err != nil {
			t.Fatal(err)
		}
		svc, err := history.New(history.Deps{DB: db, Transcripts: ss, Audit: lg})
		if err != nil {
			t.Fatal(err)
		}
		page, err := svc.Search(ctx, retentionHistoryScope(tenant, workspace, principal, "ses_search"), history.SearchQuery{Text: word})
		if err != nil {
			t.Fatal(err)
		}
		hits = len(page.Hits)
	})
	return hits
}

// retentionHistorySnapshot is every row of the conversation history, for "nothing changed".
func retentionHistorySnapshot(t *testing.T, dataDir string) string {
	t.Helper()
	var sb strings.Builder
	retentionData(t, dataDir, func(db *store.DB) {
		for _, q := range []string{
			`SELECT group_concat(seq || ':' || principal_id || ':' || body, '|') FROM (SELECT * FROM transcript_turns ORDER BY seq)`,
			`SELECT group_concat(seq || ':' || session_id || ':' || body, '|') FROM (SELECT * FROM session_turns ORDER BY seq)`,
			`SELECT group_concat(seq || ':' || id || ':' || status || ':' || version, '|') FROM (SELECT * FROM sessions ORDER BY seq)`,
			`SELECT COUNT(*) FROM session_refs`,
		} {
			var s any
			if err := db.QueryRowContext(context.Background(), q).Scan(&s); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&sb, "%v\n", s)
		}
	})
	return sb.String()
}

// retentionEraseHistoryRun runs `retention erase` and decodes its report; it fails the test unless the
// command succeeds.
func retentionEraseHistoryRun(t *testing.T, cfg string, args ...string) retentionEraseReport {
	t.Helper()
	all := append([]string{"retention", "erase", "-config", cfg, "-application", retentionTestApp, "-reason", "erasure request", "-operator", "ops-alice"}, args...)
	code, out, errOut := runCLI(t, all...)
	if code != ExitOK {
		t.Fatalf("retention erase %v = %d\n%s\n%s", args, code, out, errOut)
	}
	var rep retentionEraseReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	return rep
}

// A tenant-scoped principal erasure takes their conversation history in that tenant only: the
// transcript (and so every search hit), the recent windows and the sessions, the ended one and the
// active one (ended first). Bob's history in the same tenant, under the same session id in another
// workspace, and Alice's in another tenant stay. A dry run reports the same counts and changes
// nothing; the removal is recorded without naming her, and the chain still verifies.
func TestRetentionEraseOfAPrincipalTakesTheirHistory(t *testing.T) {
	cfg, dataDir := retentionHistoryWorld(t)
	before := retentionHistorySnapshot(t, dataDir)

	dry := retentionEraseHistoryRun(t, cfg, "-tenant", "tenant-a", "-principal", "usr_alice", "-tenant-scoped", "-dry-run")
	if !dry.DryRun || dry.TranscriptTurnsRemoved != 4 || dry.SessionTurnsRemoved != 4 || dry.SessionsRemoved != 2 || dry.SessionsEnded != 1 {
		t.Errorf("dry run = %+v", dry)
	}
	if got := retentionHistorySnapshot(t, dataDir); got != before {
		t.Fatalf("a dry run changed the history:\nbefore:\n%s\nafter:\n%s", before, got)
	}

	rep := retentionEraseHistoryRun(t, cfg, "-tenant", "tenant-a", "-principal", "usr_alice", "-tenant-scoped")
	if rep.DryRun || rep.TranscriptTurnsRemoved != dry.TranscriptTurnsRemoved || rep.SessionTurnsRemoved != dry.SessionTurnsRemoved ||
		rep.SessionsRemoved != dry.SessionsRemoved || rep.SessionsEnded != dry.SessionsEnded || rep.PreparationsRemoved != 1 {
		t.Errorf("report = %+v, want the dry run's counts %+v", rep, dry)
	}
	for q, n := range map[string]int{
		`SELECT COUNT(*) FROM transcript_turns WHERE tenant_id = 'tenant-a' AND principal_id = 'usr_alice'`:                   0,
		`SELECT COUNT(*) FROM sessions WHERE tenant_id = 'tenant-a' AND principal_id = 'usr_alice'`:                           0,
		`SELECT COUNT(*) FROM session_turns WHERE tenant_id = 'tenant-a' AND workspace_id = 'ws-1' AND session_id = 'ses_h1'`: 0,
		`SELECT COUNT(*) FROM session_turns WHERE tenant_id = 'tenant-a' AND workspace_id = 'ws-2' AND session_id = 'ses_h2'`: 0,
		`SELECT COUNT(*) FROM transcript_turns WHERE tenant_id = 'tenant-a' AND principal_id = 'usr_bob'`:                     4,
		`SELECT COUNT(*) FROM session_turns WHERE tenant_id = 'tenant-a' AND workspace_id = 'ws-2' AND session_id = 'ses_h1'`: 2,
		`SELECT COUNT(*) FROM sessions WHERE tenant_id = 'tenant-a' AND principal_id = 'usr_bob' AND status = 'active'`:       1,
		`SELECT COUNT(*) FROM transcript_turns WHERE tenant_id = 'tenant-b'`:                                                  2,
		`SELECT COUNT(*) FROM sessions WHERE tenant_id = 'tenant-b'`:                                                          1,
	} {
		if got := retentionCount(t, dataDir, q); got != n {
			t.Errorf("%s = %d, want %d", q, got, n)
		}
	}

	// History search finds nothing of hers in either workspace, and still finds the rest.
	for _, c := range []struct {
		tenant, workspace, principal, word string
		found                              bool
	}{
		{"tenant-a", "ws-1", "usr_alice", "alicefirst", false},
		{"tenant-a", "ws-2", "usr_alice", "alicesecond", false},
		{"tenant-a", "ws-2", "usr_bob", "bobended", true},
		{"tenant-a", "ws-1", "usr_bob", "bobactive", true},
		{"tenant-b", "ws-1", "usr_alice", "alicetenantb", true},
	} {
		if hits := retentionSearch(t, dataDir, c.tenant, c.workspace, c.principal, c.word); (hits > 0) != c.found {
			t.Errorf("search %s/%s/%s for %q: %d hits, want found=%t", c.tenant, c.workspace, c.principal, c.word, hits, c.found)
		}
	}

	// One record of the removal, in the tenant's chain, with the counts and without the principal.
	var data string
	retentionData(t, dataDir, func(db *store.DB) {
		if err := db.QueryRowContext(context.Background(),
			`SELECT data FROM audit_events WHERE event_type = ? AND tenant_id = 'tenant-a'`, string(audit.EventDeletionHistoryCleared)).Scan(&data); err != nil {
			t.Fatalf("the history removal is not recorded once: %v", err)
		}
	})
	var rec map[string]any
	if err := json.Unmarshal([]byte(data), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["kind"] != "principal" || rec["reason"] != "erasure request" || rec["operator"] != "ops-alice" ||
		rec["transcript_turns"] != float64(4) || rec["session_turns"] != float64(4) || rec["sessions"] != float64(2) || rec["sessions_ended"] != float64(1) {
		t.Errorf("record = %s", data)
	}
	if n := retentionCount(t, dataDir, `SELECT COUNT(*) FROM audit_events WHERE event_type IN ('audit.erased', 'deletion.receipts', 'deletion.history_cleared') AND data LIKE '%usr_alice%'`); n != 0 {
		t.Errorf("%d erasure records name the principal", n)
	}
	if code, out, errOut := runCLI(t, "audit", "verify-chain", "-data-dir", dataDir); code != ExitOK || !strings.Contains(out, `"chains_failed": 0`) {
		t.Fatalf("audit verify-chain after the erasure = %d\n%s\n%s", code, out, errOut)
	}
}

// A tenant's erasure takes all of its conversation history and nothing of another tenant's; the dry
// run's counts are what the erasure removes. While an action of the tenant is uncertain the erasure is
// refused, dry run or not, and not a row of the history moves (Bob's active session stays active).
func TestRetentionEraseOfATenantTakesAllItsHistory(t *testing.T) {
	cfg, dataDir := retentionHistoryWorld(t)
	before := retentionHistorySnapshot(t, dataDir)
	for _, extra := range [][]string{{"-dry-run"}, nil} {
		args := append([]string{"retention", "erase", "-config", cfg, "-application", retentionTestApp, "-tenant", "tenant-a",
			"-reason", "erasure request", "-operator", "ops-alice"}, extra...)
		code, out, errOut := runCLI(t, args...)
		if code == ExitOK || !strings.Contains(errOut, "refused") {
			t.Fatalf("erase %v while an action is uncertain = %d\n%s\n%s", extra, code, out, errOut)
		}
		if got := retentionHistorySnapshot(t, dataDir); got != before {
			t.Fatalf("a refused erasure (%v) changed the history:\nbefore:\n%s\nafter:\n%s", extra, before, got)
		}
	}

	retentionData(t, dataDir, func(db *store.DB) {
		if _, err := db.ExecContext(context.Background(), `UPDATE action_preparations SET state = 'committed' WHERE id = 'prep_live'`); err != nil {
			t.Fatal(err)
		}
	})
	dry := retentionEraseHistoryRun(t, cfg, "-tenant", "tenant-a", "-dry-run")
	if dry.TranscriptTurnsRemoved != 8 || dry.SessionTurnsRemoved != 8 || dry.SessionsRemoved != 4 || dry.SessionsEnded != 2 {
		t.Errorf("dry run = %+v", dry)
	}
	if got := retentionHistorySnapshot(t, dataDir); got != before {
		t.Fatal("a dry run changed the history")
	}
	rep := retentionEraseHistoryRun(t, cfg, "-tenant", "tenant-a")
	if rep.TranscriptTurnsRemoved != dry.TranscriptTurnsRemoved || rep.SessionTurnsRemoved != dry.SessionTurnsRemoved ||
		rep.SessionsRemoved != dry.SessionsRemoved || rep.SessionsEnded != dry.SessionsEnded {
		t.Errorf("report = %+v, want the dry run's counts %+v", rep, dry)
	}
	for _, table := range []string{"transcript_turns", "session_turns", "session_refs", "sessions"} {
		if n := retentionCount(t, dataDir, `SELECT COUNT(*) FROM `+table+` WHERE tenant_id = 'tenant-a'`); n != 0 {
			t.Errorf("%s: %d rows of the tenant are left", table, n)
		}
	}
	if n := retentionCount(t, dataDir, `SELECT COUNT(*) FROM transcript_turns WHERE tenant_id = 'tenant-b'`) +
		retentionCount(t, dataDir, `SELECT COUNT(*) FROM session_turns WHERE tenant_id = 'tenant-b'`) +
		retentionCount(t, dataDir, `SELECT COUNT(*) FROM sessions WHERE tenant_id = 'tenant-b'`); n != 5 {
		t.Errorf("the other tenant's history has %d rows, want its 5", n)
	}
	for _, c := range retentionHistorySessions {
		if hits := retentionSearch(t, dataDir, c.tenant, c.workspace, c.principal, c.word); (hits > 0) != (c.tenant == "tenant-b") {
			t.Errorf("search %s/%s/%s for %q after the tenant's erasure: %d hits", c.tenant, c.workspace, c.principal, c.word, hits)
		}
	}
	if code, out, errOut := runCLI(t, "audit", "verify-chain", "-data-dir", dataDir); code != ExitOK || !strings.Contains(out, `"chains_failed": 0`) {
		t.Fatalf("audit verify-chain after the erasure = %d\n%s\n%s", code, out, errOut)
	}
}
