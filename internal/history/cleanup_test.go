package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ---- a real action coordinator over the same store, so there are real receipts ----

const kitPolicyJSON = `{
  "policy_id": "default-access",
  "role_grants": [
    { "role": "member", "capabilities": ["items.get", "items.create"] }
  ],
  "risk_overrides": [],
  "field_projection": []
}`

const (
	kitApp    = "demo-app"
	kitTenant = "tenant-a"
	kitWs     = "ws-1"
)

// kit is the action coordinator of the product wired to a throwaway host
// application, sharing the store and the audit log of the test environment.
type kit struct {
	co *actions.Coordinator
}

func newKit(t *testing.T, e *env) *kit {
	t.Helper()
	hp, err := policy.ParseHostPolicy([]byte(kitPolicyJSON))
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: kitApp, Version: "1.0.0", Status: policy.StatusActive, Roles: []string{"member"},
		Capabilities: []policy.CapabilityDecl{
			{ID: fake.ReadCapability, Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead,
				Confirmation: policy.ConfirmNone, Fields: []policy.FieldDecl{{Name: "item_id", Required: true}}},
			{ID: fake.ActionCapability, Audience: policy.AudienceUser, EffectClass: policy.EffectReversible, RiskLevel: contracts.RiskReversible,
				Confirmation: policy.ConfirmNone, Fields: []policy.FieldDecl{{Name: "name", Required: true}}},
		},
		Policies: []*policy.HostPolicy{hp},
	})
	if err != nil {
		t.Fatalf("NewPackage: %v", err)
	}
	sink := audit.NewRecordSink[policy.Record](e.alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	gw := policy.NewGateway(e.clk, policy.WithSink(sink))
	guard, err := adapters.Wrap(fake.NewAdapter(fake.NewApp(fake.WithClock(e.clk))), adapters.Options{ApplicationID: kitApp})
	if err != nil {
		t.Fatal(err)
	}
	co, err := actions.New(actions.Deps{
		DB: e.db, Policy: gw, Audit: e.alog, Clock: e.clk,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return pkg, nil }),
		Adapters: actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &kit{co: co}
}

// commit prepares and commits one item and returns the preparation id and receipt.
func (k *kit) commit(t *testing.T, sc contracts.Scope, name string) (string, contracts.ActionReceipt) {
	t.Helper()
	ctx := context.Background()
	p, err := k.co.Prepare(ctx, sc, actions.PrepareInput{Session: sc, CapabilityID: fake.ActionCapability, Fields: map[string]any{"name": name}})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	id := string(p.Action.PreparationID)
	rcpt, err := k.co.Commit(ctx, sc, id)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return id, rcpt
}

// ---- a snapshot of everything outside the transcript ----

// snapshot hashes the rows of every table except the ones skip names. Rows are
// compared as text, sorted, so the order of the table does not matter.
func snapshot(t *testing.T, db *store.DB, skip func(table string) bool) map[string]string {
	t.Helper()
	ctx := context.Background()
	trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for trows.Next() {
		var n string
		if err := trows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	trows.Close()
	out := map[string]string{}
	for _, tbl := range tables {
		if skip(tbl) {
			continue
		}
		rows, err := db.QueryContext(ctx, `SELECT * FROM `+tbl)
		if err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		cols, _ := rows.Columns()
		var lines []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, fmt.Sprint(vals...))
		}
		rows.Close()
		sort.Strings(lines)
		sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
		out[tbl] = fmt.Sprintf("%d rows %s", len(lines), hex.EncodeToString(sum[:8]))
	}
	return out
}

// outsideTranscript skips what a history deletion is allowed to change: the
// transcript, its search index and the audit log (which only grows, checked on
// its own).
func outsideTranscript(table string) bool {
	return table == "transcript_turns" || strings.HasPrefix(table, "history_fts") ||
		table == "audit_events" || table == "audit_checkpoints"
}

// auditRows reads every audit event as text, keyed by id.
func auditRows(t *testing.T, db *store.DB) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT id, application_id, tenant_id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, data, prev_hash, hash FROM audit_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, a, tn, w, typ, p, s, d, ph, h string
		var seq, at int64
		if err := rows.Scan(&id, &a, &tn, &w, &seq, &typ, &at, &p, &s, &d, &ph, &h); err != nil {
			t.Fatal(err)
		}
		out[id] = fmt.Sprint(a, tn, w, seq, typ, at, p, s, d, ph, h)
	}
	return out
}

// SEC-087
//
// Clearing history is the transcript and nothing else: the session state, the
// prepared action, its committed receipt, the audit events (and the chain they
// form) and every other module's rows are exactly as they were, and the clearing
// itself is recorded as a tombstone in that chain. Cleanup of the workspace
// leaves the other workspaces and tenants alone too.
func TestCleanupKeepsReceiptsAndAudit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	k := newKit(t, e)

	user := scopeOf(kitApp, kitTenant, kitWs, "user-1", "ses-1", append([]string{CapRead, CapSearch, CapClear, CapCleanup}, fake.ReadCapability, fake.ActionCapability)...)
	second := with(user, user.CapabilityScopes...)
	second.SessionID = "ses-2"
	otherWs := user.Clone()
	otherWs.WorkspaceID = "ws-2"
	otherTenant := user.Clone()
	otherTenant.TenantID = "tenant-b"

	// Three turns of conversation, one of which prepared and committed an action.
	e.say(user, "create an item called widget", "I prepared it")
	prepID, receipt := k.commit(t, user, "widget")
	e.clk.Advance(time.Minute)
	e.say(user, "yes", "done, the item exists")
	e.say(second, "what did I order?", "a widget")
	e.say(otherWs, "workspace two keeps its history", "yes")
	e.say(otherTenant, "tenant b keeps its history", "yes")

	sessionBefore, err := e.sess.Open(ctx, user)
	noErr(t, err)
	if e.count(`SELECT COUNT(*) FROM action_receipts`) != 1 || e.count(`SELECT COUNT(*) FROM action_preparations`) != 1 {
		t.Fatal("the fixture has no receipt")
	}
	if e.count(`SELECT COUNT(*) FROM audit_events WHERE event_type IN ('action.prepared', 'action.committing', 'action.committed')`) != 3 {
		t.Fatal("the fixture has no action audit events")
	}
	others := snapshot(t, e.db, outsideTranscript)
	auditBefore := auditRows(t, e.db)
	rowsBefore := e.transcriptRows()

	// 1. The user clears one session.
	res, err := e.svc.ClearSession(ctx, user, "ses-1")
	noErr(t, err)
	if res.Rows != 4 {
		t.Fatalf("cleared %d rows, want 4", res.Rows)
	}
	// 2. The manager cleans the workspace up to now.
	e.clk.Advance(time.Hour)
	res, err = e.svc.Cleanup(ctx, user, e.clk.Now())
	noErr(t, err)
	if res.Rows != 2 {
		t.Fatalf("cleanup removed %d rows, want 2 (the other session of the workspace)", res.Rows)
	}

	// The transcript of the workspace is gone, the others' is not.
	if got := e.transcriptRows(); got != rowsBefore-6 {
		t.Errorf("%d transcript rows left, want %d", got, rowsBefore-6)
	}
	if n := e.count(`SELECT COUNT(*) FROM transcript_turns WHERE application_id = ? AND tenant_id = ? AND workspace_id = ?`, kitApp, kitTenant, kitWs); n != 0 {
		t.Errorf("%d transcript rows of the cleaned workspace are left", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM transcript_turns WHERE workspace_id = 'ws-2' OR tenant_id = 'tenant-b'`); n != 4 {
		t.Errorf("another workspace's or tenant's rows: %d left, want 4", n)
	}
	e.checkIndex()

	// Everything outside the transcript is byte-identical.
	if diff := cmp.Diff(others, snapshot(t, e.db, outsideTranscript)); diff != "" {
		t.Errorf("a table outside the transcript changed (-before +after):\n%s", diff)
	}
	// The session itself, with its goal state and turn count, survives its
	// transcript.
	sessionAfter, err := e.sess.Open(ctx, user)
	noErr(t, err)
	if sessionAfter.Version != sessionBefore.Version || sessionAfter.TurnCount != sessionBefore.TurnCount || !sessionAfter.UpdatedAt.Equal(sessionBefore.UpdatedAt) {
		t.Errorf("the session changed: before %+v, after %+v", sessionBefore, sessionAfter)
	}
	// The receipt is still the very one the host was given.
	got, err := k.co.Receipt(ctx, user, prepID)
	noErr(t, err)
	if diff := cmp.Diff(receipt, got); diff != "" {
		t.Errorf("the receipt changed (-before +after):\n%s", diff)
	}

	// The audit log only grew: every event is there, untouched, the chain
	// verifies, and the two deletions are tombstones in it.
	auditAfter := auditRows(t, e.db)
	for id, row := range auditBefore {
		if auditAfter[id] != row {
			t.Errorf("audit event %s changed or went missing", id)
		}
	}
	var added []string
	for id := range auditAfter {
		if _, was := auditBefore[id]; !was {
			added = append(added, auditAfter[id])
		}
	}
	if len(added) != 2 {
		t.Fatalf("%d audit events were added, want the 2 deletions: %v", len(added), added)
	}
	for _, row := range added {
		if !strings.Contains(row, string(audit.EventDeletionHistoryCleared)) {
			t.Errorf("an audit event that is not a deletion was added: %s", row)
		}
		if strings.Contains(row, "widget") || strings.Contains(row, "order") {
			t.Errorf("the tombstone carries transcript text: %s", row)
		}
	}
	if _, err := e.alog.VerifyChain(ctx, audit.ChainKey{ApplicationID: kitApp, TenantID: kitTenant}, audit.VerifyOptions{}); err != nil {
		t.Errorf("the audit chain no longer verifies: %v", err)
	}
}

// PruneTranscript deletes across the whole workspace, so the caller is
// authorised first; and a deletion is recorded before it happens and refused
// when it cannot be recorded.
func TestCleanupAuthorisedBeforeAnythingIsDeleted(t *testing.T) {
	var order []string
	spy := &spyTranscripts{order: &order}
	rec := &recordingAudit{}
	e := newEnv(t, withDeps(func(d *Deps) {
		spy.TranscriptStore = d.Transcripts
		d.Transcripts = spy
		rec.Appender = d.Audit
		d.Audit = rec
	}))
	ctx := context.Background()
	admin := scopeOf(testApp, tenA, ws1, alice, "ses_1", adminCaps...)
	e.say(admin, "one", "two")
	e.say(scopeOf(testApp, tenA, ws1, bob, "ses_b"), "bob's turn", "")
	e.clk.Advance(time.Hour)
	now := e.clk.Now()

	t.Run("a user without the cleanup capability cannot reach the whole workspace", func(t *testing.T) {
		for _, caps := range [][]string{userCaps, {CapClear}, {"history.cleanups"}, {"History.Cleanup"}} {
			_, err := e.svc.Cleanup(ctx, with(admin, caps...), now)
			wantCode(t, err, contracts.ErrPermissionDenied)
		}
		// Not even the call to the session store was made.
		if _, prunes := spy.calls(); prunes != 0 {
			t.Fatalf("PruneTranscript was called %d times for a caller that is not authorised", prunes)
		}
		if e.transcriptRows() != 3 || len(rec.seen()) != 0 {
			t.Fatalf("%d rows left, events %v", e.transcriptRows(), rec.seen())
		}
	})

	t.Run("the host's extra authoriser refuses before the call", func(t *testing.T) {
		var denySpy *spyTranscripts
		deny := newEnv(t, withDeps(func(d *Deps) {
			denySpy = &spyTranscripts{TranscriptStore: d.Transcripts}
			d.Transcripts = denySpy
			d.Authorizer = func(_ context.Context, _ contracts.Scope, a Action) error {
				if a == ActionCleanup {
					return contracts.New(contracts.ErrPermissionDenied, "not today")
				}
				return nil
			}
		}))
		deny.say(admin, "one", "two")
		deny.clk.Advance(time.Hour)
		_, err := deny.svc.Cleanup(ctx, admin, deny.clk.Now())
		wantCode(t, err, contracts.ErrPermissionDenied)
		if _, prunes := denySpy.calls(); prunes != 0 || deny.transcriptRows() != 2 {
			t.Fatalf("prunes %d, rows %d: the host's refusal did not stop the deletion", prunes, deny.transcriptRows())
		}
	})

	t.Run("a cutoff in the future or none is refused", func(t *testing.T) {
		_, err := e.svc.Cleanup(ctx, admin, now.Add(time.Second))
		wantCode(t, err, contracts.ErrValidationFailed)
		_, err = e.svc.Cleanup(ctx, admin, time.Time{})
		wantCode(t, err, contracts.ErrValidationFailed)
		if e.transcriptRows() != 3 {
			t.Fatal("a refused cleanup deleted rows")
		}
	})

	t.Run("one principal cannot clear another's session", func(t *testing.T) {
		_, err := e.svc.ClearSession(ctx, scopeOf(testApp, tenA, ws1, bob, "ses_b", CapClear), "ses_1")
		wantCode(t, err, contracts.ErrScopeViolation)
		_, err = e.svc.ClearSession(ctx, with(admin, CapRead, CapSearch, CapExport), "ses_1")
		wantCode(t, err, contracts.ErrPermissionDenied)
		_, err = e.svc.ClearSession(ctx, admin, "ses 1")
		wantCode(t, err, contracts.ErrValidationFailed)
		if e.transcriptRows() != 3 {
			t.Fatal("a refused clear deleted rows")
		}
	})

	t.Run("a deletion is recorded before it happens", func(t *testing.T) {
		rec.mu.Lock()
		rec.types = nil
		rec.mu.Unlock()
		order = nil
		res, err := e.svc.ClearSession(ctx, admin, "ses_1")
		noErr(t, err)
		if res.Rows != 2 {
			t.Fatalf("cleared %d rows, want 2", res.Rows)
		}
		if got := rec.seen(); len(got) != 1 || got[0] != audit.EventDeletionHistoryCleared {
			t.Fatalf("events = %v", got)
		}
		if len(order) != 1 || order[0] != "clear" {
			t.Fatalf("session store calls = %v", order)
		}
		ev := lastEvent(t, e, string(audit.EventDeletionHistoryCleared))
		if !strings.Contains(string(ev.Data), `"rows":2`) || !strings.Contains(string(ev.Data), `"scope":"session"`) {
			t.Errorf("the tombstone = %s", ev.Data)
		}
		if ev.SessionID != "ses_1" || ev.PrincipalID != alice {
			t.Errorf("the tombstone is attributed to %s in %s", ev.PrincipalID, ev.SessionID)
		}
		// Nothing to clear is nothing recorded.
		res, err = e.svc.ClearSession(ctx, admin, "ses_1")
		noErr(t, err)
		if res.Rows != 0 || len(rec.seen()) != 1 {
			t.Errorf("clearing an empty session: %d rows, events %v", res.Rows, rec.seen())
		}
	})

	t.Run("without a record nothing is deleted", func(t *testing.T) {
		down := newEnv(t, withDeps(func(d *Deps) { d.Audit = failingAudit{d.Audit} }))
		down.say(admin, "one", "two")
		down.clk.Advance(time.Hour)
		_, err := down.svc.ClearSession(ctx, admin, "ses_1")
		wantCode(t, err, contracts.ErrUnavailable)
		_, err = down.svc.Cleanup(ctx, admin, down.clk.Now())
		wantCode(t, err, contracts.ErrUnavailable)
		if down.transcriptRows() != 2 {
			t.Fatalf("%d rows left: history was deleted with no record of it", down.transcriptRows())
		}
	})
}
