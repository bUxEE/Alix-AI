package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func decodeExport(t *testing.T, b []byte) []ExportRow {
	t.Helper()
	var out []ExportRow
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	for dec.More() {
		var r ExportRow
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("the export is not JSON lines of ExportRow: %v\n%s", err, b)
		}
		out = append(out, r)
	}
	return out
}

// failAfter is a writer that fails once it has taken n bytes.
type failAfter struct {
	n   int
	buf bytes.Buffer
}

func (f *failAfter) Write(p []byte) (int, error) {
	if f.buf.Len()+len(p) > f.n {
		return 0, errors.New("connection reset")
	}
	return f.buf.Write(p)
}

// SEC-026
//
// An export is the caller's own history, written only to a caller the host
// authorised, and recorded before a byte leaves. A caller without the capability
// gets nothing and leaves no event; a session of another principal is refused;
// the export is complete across pages and carries nothing of another scope.
func TestExportAuthorized(t *testing.T) {
	rec := &recordingAudit{}
	e := newEnv(t, withDeps(func(d *Deps) {
		rec.Appender = d.Audit
		d.Audit = rec
		d.Config.ExportPage = 3 // several pages for a few rows
	}))
	ctx := context.Background()
	me := scopeOf(testApp, tenA, ws1, alice, "ses_1")

	// Alice has two sessions; everyone else has turns too.
	var mine []string
	for i := 1; i <= 4; i++ {
		mine = append(mine, fmt.Sprintf("alice one %d", i), fmt.Sprintf("reply one %d", i))
		e.say(me, fmt.Sprintf("alice one %d", i), fmt.Sprintf("reply one %d", i))
		e.clk.Advance(time.Minute)
	}
	second := scopeOf(testApp, tenA, ws1, alice, "ses_2")
	mine = append(mine, "alice two")
	e.say(second, "alice two", "")
	e.say(scopeOf(testApp, tenA, ws1, bob, "ses_3"), "bob secret", "bob reply")
	e.say(scopeOf(testApp, tenB, ws1, alice, "ses_1"), "tenant b secret", "")
	e.say(scopeOf(testApp, tenA, ws2, alice, "ses_1"), "workspace two secret", "")
	e.say(scopeOf(otherApp, tenA, ws1, alice, "ses_1"), "other app secret", "")

	t.Run("without the capability nothing is written or recorded", func(t *testing.T) {
		for _, caps := range [][]string{{CapRead, CapSearch, CapClear}, {"history.exports"}, {"History.Export"}, {"audit.export"}} {
			var out bytes.Buffer
			sc := me.Clone()
			sc.CapabilityScopes = caps
			_, err := e.svc.Export(ctx, sc, &out, ExportOptions{})
			wantCode(t, err, contracts.ErrPermissionDenied)
			if out.Len() != 0 {
				t.Fatalf("%d bytes were written to a caller without the capability", out.Len())
			}
		}
		if got := rec.seen(); len(got) != 0 {
			t.Fatalf("a refused export left events: %v", got)
		}
	})

	t.Run("a session of another principal is refused", func(t *testing.T) {
		var out bytes.Buffer
		_, err := e.svc.Export(ctx, scopeOf(testApp, tenA, ws1, bob, "ses_3"), &out, ExportOptions{SessionID: "ses_1"})
		wantCode(t, err, contracts.ErrScopeViolation)
		if out.Len() != 0 || len(rec.seen()) != 0 {
			t.Fatalf("a refused export wrote %d bytes and left events %v", out.Len(), rec.seen())
		}
	})

	t.Run("the host's extra authoriser refuses", func(t *testing.T) {
		deny := newEnv(t, withDeps(func(d *Deps) {
			d.Authorizer = func(_ context.Context, _ contracts.Scope, a Action) error {
				if a == ActionExport {
					return errors.New("policy says no")
				}
				return nil
			}
		}))
		deny.say(me, "hello", "hi")
		var out bytes.Buffer
		_, err := deny.svc.Export(ctx, me, &out, ExportOptions{})
		wantCode(t, err, contracts.ErrPermissionDenied)
		if out.Len() != 0 {
			t.Error("the export wrote after the host refused it")
		}
	})

	t.Run("a bad request is refused before anything is recorded", func(t *testing.T) {
		var out bytes.Buffer
		_, err := e.svc.Export(ctx, me, &out, ExportOptions{SessionID: "ses 1"})
		wantCode(t, err, contracts.ErrValidationFailed)
		_, err = e.svc.Export(ctx, me, &out, ExportOptions{Since: t0.Add(time.Hour), Until: t0})
		wantCode(t, err, contracts.ErrValidationFailed)
		_, err = e.svc.Export(ctx, me, nil, ExportOptions{})
		wantCode(t, err, contracts.ErrValidationFailed)
		if out.Len() != 0 || len(rec.seen()) != 0 {
			t.Fatalf("a bad request wrote %d bytes and left events %v", out.Len(), rec.seen())
		}
	})

	t.Run("the whole export is the caller's rows, across pages", func(t *testing.T) {
		var out bytes.Buffer
		res, err := e.svc.Export(ctx, me, &out, ExportOptions{})
		noErr(t, err)
		rows := decodeExport(t, out.Bytes())
		if res.Rows != len(rows) || len(rows) != 9 {
			t.Fatalf("exported %d rows (result %d), want 9 (alice's two sessions)", len(rows), res.Rows)
		}
		var bodies []string
		for _, r := range rows {
			bodies = append(bodies, r.Body)
			if strings.Contains(r.Body, "secret") {
				t.Errorf("another scope's text in the export: %q", r.Body)
			}
		}
		if diff := cmp.Diff(mine, bodies); diff != "" {
			t.Errorf("the export differs from the stored rows, in order (-want +got):\n%s", diff)
		}
		if rows[0].SessionID != "ses_1" || rows[0].Role != "user" || rows[0].N != 1 || !rows[0].CreatedAt.Equal(t0) {
			t.Errorf("first row = %+v", rows[0])
		}
		// The record came first, and carries no text.
		if got := rec.seen(); len(got) != 1 || got[0] != eventExported.Name() {
			t.Fatalf("events = %v, want one %s", got, eventExported.Name())
		}
		ev := lastEvent(t, e, string(eventExported.Name()))
		if strings.Contains(string(ev.Data), "alice") || strings.Contains(string(ev.Data), "reply") {
			t.Errorf("the export event carries text: %s", ev.Data)
		}
		if ev.PrincipalID != alice || ev.TenantID != tenA || ev.WorkspaceID != ws1 {
			t.Errorf("the export event is attributed to %s in %s/%s", ev.PrincipalID, ev.TenantID, ev.WorkspaceID)
		}
	})

	t.Run("one session and a time window", func(t *testing.T) {
		var out bytes.Buffer
		_, err := e.svc.Export(ctx, me, &out, ExportOptions{SessionID: "ses_2"})
		noErr(t, err)
		if rows := decodeExport(t, out.Bytes()); len(rows) != 1 || rows[0].Body != "alice two" {
			t.Errorf("session export = %+v", rows)
		}
		out.Reset()
		_, err = e.svc.Export(ctx, me, &out, ExportOptions{SessionID: "ses_1", Since: t0.Add(time.Minute), Until: t0.Add(3 * time.Minute)})
		noErr(t, err)
		rows := decodeExport(t, out.Bytes())
		if len(rows) != 4 || rows[0].N != 2 || rows[3].N != 3 {
			t.Errorf("window export = %+v, want turns 2 and 3", rows)
		}
	})

	t.Run("an audit log that is down refuses the export", func(t *testing.T) {
		down := newEnv(t, withDeps(func(d *Deps) { d.Audit = failingAudit{d.Audit} }))
		down.say(me, "hello", "hi")
		var out bytes.Buffer
		_, err := down.svc.Export(ctx, me, &out, ExportOptions{})
		wantCode(t, err, contracts.ErrUnavailable)
		if out.Len() != 0 {
			t.Fatalf("%d bytes left without a record of the export", out.Len())
		}
	})

	t.Run("a writer that fails stops the export with an error", func(t *testing.T) {
		w := &failAfter{n: 200}
		res, err := e.svc.Export(ctx, me, w, ExportOptions{})
		wantCode(t, err, contracts.ErrUnavailable)
		if res.Rows >= 9 {
			t.Errorf("exported %d rows into a writer that failed", res.Rows)
		}
	})

	t.Run("an empty history exports nothing", func(t *testing.T) {
		var out bytes.Buffer
		res, err := e.svc.Export(ctx, scopeOf(testApp, "ten_empty", ws1, alice, "ses_1"), &out, ExportOptions{})
		noErr(t, err)
		if res.Rows != 0 || out.Len() != 0 {
			t.Errorf("exported %d rows, %d bytes", res.Rows, out.Len())
		}
	})
}

// lastEvent returns the newest audit event of a type.
func lastEvent(t *testing.T, e *env, typ string) audit.Event {
	t.Helper()
	var ev audit.Event
	var ms int64
	var data string
	err := e.db.QueryRowContext(context.Background(),
		`SELECT id, application_id, tenant_id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, data, prev_hash, hash
 FROM audit_events WHERE event_type = ? ORDER BY seq DESC LIMIT 1`, typ).
		Scan(&ev.ID, &ev.ApplicationID, &ev.TenantID, &ev.WorkspaceID, &ev.ChainSeq, &ev.Type, &ms, &ev.PrincipalID, &ev.SessionID, &data, &ev.PrevHash, &ev.Hash)
	if err != nil {
		t.Fatalf("no %s event: %v", typ, err)
	}
	ev.OccurredAt, ev.Data = time.UnixMilli(ms).UTC(), json.RawMessage(data)
	return ev
}

// queryPlan returns the EXPLAIN QUERY PLAN lines of a statement.
func queryPlan(t *testing.T, e *env, query string, args ...any) string {
	t.Helper()
	rows, err := e.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "\n")
}

// An export reads the caller's rows of the workspace a page at a time in seq order. Without a
// session, every page must follow an index on the principal in seq order, not sort every row of
// the workspace in a temporary B-tree. With one, it reads that session's rows only.
func TestExportPageFollowsThePrincipalIndex(t *testing.T) {
	e := newEnv(t)
	a := scopeOf(testApp, tenA, ws1, alice, "ses-1")
	b := scopeOf(testApp, tenA, ws1, bob, "ses-2")
	for i := range 20 {
		e.say(a, fmt.Sprintf("alice %d", i), "ok")
		e.say(b, fmt.Sprintf("bob %d", i), "ok")
		e.clk.Advance(time.Minute)
	}
	for name, opts := range map[string]ExportOptions{
		"everything":  {},
		"a window":    {Since: t0, Until: t0.Add(time.Hour)},
		"since alone": {Since: t0.Add(5 * time.Minute)},
		"one session": {SessionID: "ses-1"},
	} {
		t.Run(name, func(t *testing.T) {
			where, args := exportFilter(a.PrincipalID, opts)
			args[0] = int64(0)
			query, qargs, err := e.svc.exportStmt(where, args).Build(a)
			if err != nil {
				t.Fatal(err)
			}
			plan := queryPlan(t, e, query, qargs...)
			t.Logf("plan:\n%s", plan)
			if opts.SessionID != "" {
				if !strings.Contains(plan, "SEARCH transcript_turns USING INDEX transcript_turns_order (application_id=? AND tenant_id=? AND workspace_id=? AND session_id=?") {
					t.Errorf("a session's export page does not read the session's rows through transcript_turns_order:\n%s", plan)
				}
				return
			}
			for _, bad := range []string{"TEMP B-TREE", "SCAN transcript_turns"} {
				if strings.Contains(plan, bad) {
					t.Errorf("the export page plan contains %q:\n%s", bad, plan)
				}
			}
			if !strings.Contains(plan, "SEARCH transcript_turns USING INDEX transcript_turns_principal ") {
				t.Errorf("the export page does not use transcript_turns_principal:\n%s", plan)
			}
		})
	}
}
