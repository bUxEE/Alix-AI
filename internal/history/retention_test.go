package history

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const day = 24 * time.Hour

// newPool builds the pool the composition root would: a scope checker that
// refuses every user (nobody's token is valid when a job runs), and a service
// authoriser that accepts only the retention job's identity.
func newPool(t *testing.T, e *env) *jobs.Pool {
	t.Helper()
	pool, err := jobs.NewPool(e.q, jobs.PoolOptions{
		ScopeChecker: jobs.ScopeCheckerFunc(func(context.Context, contracts.Scope) error {
			return contracts.New(contracts.ErrPermissionDenied, "the user is signed out")
		}),
		ServiceAuthorizer: jobs.ServiceAuthorizerFunc(func(_ context.Context, id string, _ jobs.Job) error {
			if id != ServiceIdentity {
				return contracts.New(contracts.ErrPermissionDenied, "unknown service")
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Handle(pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Shutdown(context.Background()) })
	return pool
}

// runJobs lets the pool run what is due and waits until its handlers finish.
func runJobs(t *testing.T, pool *jobs.Pool) int {
	t.Helper()
	total := 0
	for round := 0; round < 20; round++ {
		n, err := pool.Poll(context.Background())
		noErr(t, err)
		deadline := time.Now().Add(10 * time.Second)
		for pool.Running(jobs.ClassMaintenance) != 0 {
			if time.Now().After(deadline) {
				t.Fatal("the retention jobs did not finish")
			}
			time.Sleep(2 * time.Millisecond)
		}
		if n == 0 {
			break
		}
		total += n
	}
	return total
}

// bodiesOf lists the transcript bodies of a workspace, sorted by insertion.
func (e *env) bodiesOf(app, tenant, ws string) []string {
	e.t.Helper()
	rows, err := e.db.QueryContext(context.Background(),
		`SELECT body FROM transcript_turns WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? ORDER BY seq`, app, tenant, ws)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func sameBodies(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%s: transcript = %q, want %q", what, got, want)
	}
}

func allow(context.Context, contracts.Scope, Action) error { return nil }

// Retention is configurable per application and tenant and is applied by a
// scheduled maintenance job: a tenant's own (shorter) policy beats the
// application's, a tenant without one gets the application's, an application
// without one keeps everything, and the job runs under the service identity, not
// as a user.
func TestRetentionPolicyApplied(t *testing.T) {
	e := newEnv(t, withDeps(func(d *Deps) { d.Operator = allow }))
	ctx := context.Background()
	pool := newPool(t, e)

	opScope := scopeOf(testApp, tenA, ws1, "usr_operator", "ses_op")
	adminA := scopeOf(testApp, tenA, ws1, alice, "ses_a", adminCaps...)
	adminB := scopeOf(testApp, tenB, ws1, bob, "ses_b", adminCaps...)

	pol, err := e.svc.SetApplicationRetention(ctx, opScope, 90)
	noErr(t, err)
	if pol.EffectiveDays != 90 || pol.Source != "application" {
		t.Fatalf("application policy = %+v", pol)
	}
	pol, err = e.svc.SetRetention(ctx, adminA, 30)
	noErr(t, err)
	if pol.EffectiveDays != 30 || pol.Source != "tenant" || pol.Application == nil || pol.Application.Days != 90 {
		t.Fatalf("tenant A policy = %+v", pol)
	}
	if pol, err = e.svc.RetentionPolicy(ctx, adminB); err != nil || pol.EffectiveDays != 90 || pol.Source != "application" || pol.Tenant != nil {
		t.Fatalf("tenant B (no policy of its own) = %+v, %v; want the application's 90", pol, err)
	}

	// Four workspaces: tenant A (two), tenant B, and another application that
	// has no policy at all. Old turns at t0, newer ones twenty days later.
	places := []struct {
		name string
		sc   contracts.Scope
	}{
		{"a1", scopeOf(testApp, tenA, ws1, alice, "ses_1")},
		{"a2", scopeOf(testApp, tenA, ws2, alice, "ses_1")},
		{"b1", scopeOf(testApp, tenB, ws1, bob, "ses_1")},
		{"o1", scopeOf(otherApp, tenA, ws1, alice, "ses_1")},
	}
	for _, p := range places {
		e.say(p.sc, p.name+" old", "")
	}
	e.clk.Advance(20 * day)
	for _, p := range places {
		e.say(p.sc, p.name+" new", "")
	}

	// Day 35: tenant A's old turns are past 30 days; tenant B's are not past 90.
	e.clk.Advance(15 * day)
	res, err := e.svc.ScheduleRetention(ctx)
	noErr(t, err)
	if res.Workspaces != 4 || res.Enqueued != 2 || res.Existing != 0 {
		t.Fatalf("schedule = %+v, want 4 workspaces looked at and 2 jobs (tenant A's two workspaces)", res)
	}
	// Scheduling again in the same period queues nothing new.
	if again, err := e.svc.ScheduleRetention(ctx); err != nil || again.Enqueued != 0 || again.Existing != 2 {
		t.Fatalf("second schedule = %+v, %v; want the 2 jobs already queued", again, err)
	}
	if n := runJobs(t, pool); n != 2 {
		t.Fatalf("the pool ran %d jobs, want 2", n)
	}
	sameBodies(t, "tenant A workspace 1", e.bodiesOf(testApp, tenA, ws1), "a1 new")
	sameBodies(t, "tenant A workspace 2", e.bodiesOf(testApp, tenA, ws2), "a2 new")
	sameBodies(t, "tenant B (90 days)", e.bodiesOf(testApp, tenB, ws1), "b1 old", "b1 new")
	sameBodies(t, "the application without a policy", e.bodiesOf(otherApp, tenA, ws1), "o1 old", "o1 new")
	if got := e.count(`SELECT COUNT(*) FROM jobs WHERE type = ? AND state = 'succeeded'`, JobRetention); got != 2 {
		t.Errorf("%d retention jobs succeeded, want 2", got)
	}
	e.checkIndex()

	// The deletions are recorded, with counts and no text, as the service.
	var cleared int
	for _, tenant := range []string{tenA} {
		rows, err := e.db.QueryContext(ctx, `SELECT principal_id, data FROM audit_events WHERE event_type = ? AND tenant_id = ?`, string(audit.EventDeletionHistoryCleared), tenant)
		noErr(t, err)
		for rows.Next() {
			var principal, data string
			noErr(t, rows.Scan(&principal, &data))
			cleared++
			if principal != servicePrincipal || !strings.Contains(data, `"reason":"retention"`) || !strings.Contains(data, `"retain_days":30`) || strings.Contains(data, "old") {
				t.Errorf("tombstone by %s = %s", principal, data)
			}
		}
		rows.Close()
	}
	if cleared != 2 {
		t.Errorf("%d tombstones for tenant A, want 2", cleared)
	}

	// Day 95: tenant B's old turns are now past 90 days, tenant A's newer ones
	// past 30; the application with no policy still keeps everything.
	e.clk.Advance(60 * day)
	res, err = e.svc.ScheduleRetention(ctx)
	noErr(t, err)
	if res.Enqueued != 3 {
		t.Fatalf("schedule on day 95 = %+v, want 3 jobs", res)
	}
	runJobs(t, pool)
	sameBodies(t, "tenant A workspace 1 on day 95", e.bodiesOf(testApp, tenA, ws1))
	sameBodies(t, "tenant A workspace 2 on day 95", e.bodiesOf(testApp, tenA, ws2))
	sameBodies(t, "tenant B on day 95", e.bodiesOf(testApp, tenB, ws1), "b1 new")
	sameBodies(t, "the application without a policy on day 95", e.bodiesOf(otherApp, tenA, ws1), "o1 old", "o1 new")
	e.checkIndex()

	// A policy is read again when the job runs: a job queued under 30 days and
	// run after the tenant moved to 90 deletes nothing that is not past 90.
	e.say(places[0].sc, "a1 fresh", "")
	e.clk.Advance(40 * day)
	res, err = e.svc.ScheduleRetention(ctx)
	noErr(t, err)
	if res.Enqueued != 2 { // tenant A (40 days old, past 30) and tenant B's turn (past 90? 135 days: yes)
		t.Fatalf("schedule = %+v, want 2 jobs", res)
	}
	if _, err := e.svc.SetRetention(ctx, adminA, 90); err != nil {
		t.Fatal(err)
	}
	runJobs(t, pool)
	sameBodies(t, "tenant A after the policy was lengthened", e.bodiesOf(testApp, tenA, ws1), "a1 fresh")
	sameBodies(t, "tenant B past the application's 90 days", e.bodiesOf(testApp, tenB, ws1))
}

// With no policy nothing is ever deleted, however old it is, and nothing is
// even queued.
func TestRetentionWithoutPolicyDeletesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sc := scopeOf(testApp, tenA, ws1, alice, "ses_1")
	e.say(sc, "kept for ever", "yes")
	e.clk.Advance(10 * 365 * day)
	res, err := e.svc.ScheduleRetention(ctx)
	noErr(t, err)
	if res.Workspaces != 1 || res.Enqueued != 0 {
		t.Fatalf("schedule = %+v, want one workspace looked at and no job", res)
	}
	if got, err := e.svc.applyRetention(ctx, sc); err != nil || got.Rows != 0 {
		t.Fatalf("applyRetention = %+v, %v", got, err)
	}
	if e.transcriptRows() != 2 {
		t.Fatalf("%d rows left, want 2", e.transcriptRows())
	}
	if e.count(`SELECT COUNT(*) FROM audit_events WHERE event_type = ?`, string(audit.EventDeletionHistoryCleared)) != 0 {
		t.Error("a deletion was recorded though nothing was deleted")
	}
}

// Who may set what: the tenant sets its own policy with the capability, never
// longer than its application allows; only an operator sets the application's.
func TestRetentionPolicyAuthorisation(t *testing.T) {
	var asked []Action
	e := newEnv(t, withDeps(func(d *Deps) {
		d.Operator = func(_ context.Context, sc contracts.Scope, a Action) error {
			asked = append(asked, a)
			if sc.PrincipalID != "usr_operator" {
				return errors.New("not an operator")
			}
			return nil
		}
	}))
	ctx := context.Background()
	admin := scopeOf(testApp, tenA, ws1, alice, "ses_a", adminCaps...)
	op := scopeOf(testApp, tenA, ws1, "usr_operator", "ses_op")

	t.Run("the capability", func(t *testing.T) {
		for _, caps := range [][]string{userCaps, {CapCleanup}, {"history.retentions"}} {
			sc := with(admin, caps...)
			_, err := e.svc.SetRetention(ctx, sc, 30)
			wantCode(t, err, contracts.ErrPermissionDenied)
			_, err = e.svc.RetentionPolicy(ctx, sc)
			wantCode(t, err, contracts.ErrPermissionDenied)
			_, err = e.svc.ClearRetention(ctx, sc)
			wantCode(t, err, contracts.ErrPermissionDenied)
		}
		if e.count(`SELECT COUNT(*) FROM history_retention`) != 0 {
			t.Fatal("a refused call changed a policy")
		}
	})
	t.Run("a tenant cannot set the application's policy", func(t *testing.T) {
		_, err := e.svc.SetApplicationRetention(ctx, admin, 30)
		wantCode(t, err, contracts.ErrPermissionDenied) // even with every history capability
		if len(asked) != 1 || asked[0] != ActionApplicationRetention {
			t.Errorf("operator hook asked %v", asked)
		}
		none := newEnv(t) // no operator hook at all: denied
		_, err = none.svc.SetApplicationRetention(ctx, op, 30)
		wantCode(t, err, contracts.ErrPermissionDenied)
		_, err = none.svc.ClearApplicationRetention(ctx, op)
		wantCode(t, err, contracts.ErrPermissionDenied)
	})
	t.Run("a tenant keeps history no longer than its application", func(t *testing.T) {
		_, err := e.svc.SetApplicationRetention(ctx, op, 60)
		noErr(t, err)
		_, err = e.svc.SetRetention(ctx, admin, 61)
		wantCode(t, err, contracts.ErrValidationFailed)
		pol, err := e.svc.SetRetention(ctx, admin, 60)
		noErr(t, err)
		if pol.EffectiveDays != 60 {
			t.Errorf("policy = %+v", pol)
		}
		// Lowering the application's policy below a tenant's holds the tenant to
		// the shorter one: the effective retention is the minimum.
		pol, err = e.svc.SetApplicationRetention(ctx, op, 14)
		noErr(t, err)
		if pol.EffectiveDays != 14 || pol.Source != "application" || pol.Tenant == nil || pol.Tenant.Days != 60 {
			t.Errorf("after lowering the application's policy: %+v", pol)
		}
		for _, bad := range []int{0, -1, MaxRetainDays + 1} {
			_, err := e.svc.SetRetention(ctx, admin, bad)
			wantCode(t, err, contracts.ErrValidationFailed)
		}
	})
	t.Run("clearing a policy goes back to the next level", func(t *testing.T) {
		pol, err := e.svc.ClearRetention(ctx, admin)
		noErr(t, err)
		if pol.Tenant != nil || pol.EffectiveDays != 14 {
			t.Errorf("after clearing the tenant's policy: %+v", pol)
		}
		pol, err = e.svc.ClearApplicationRetention(ctx, op)
		noErr(t, err)
		if pol.Application != nil || pol.EffectiveDays != 0 || pol.Source != "none" {
			t.Errorf("after clearing both: %+v", pol)
		}
	})
	t.Run("every change is recorded with the change itself", func(t *testing.T) {
		// 60 (app), 60 (tenant), 14 (app), clear tenant, clear app; the refused ones left nothing.
		if got := e.count(`SELECT COUNT(*) FROM audit_events WHERE event_type = ?`, string(audit.EventSettingsChanged)); got != 5 {
			t.Errorf("%d settings.changed events, want 5", got)
		}
		ev := lastEvent(t, e, string(audit.EventSettingsChanged))
		if !strings.Contains(string(ev.Data), `"setting":"history.retention"`) || !strings.Contains(string(ev.Data), `"level":"application"`) || ev.TenantID != "" {
			t.Errorf("the last change = %s on chain %q", ev.Data, ev.TenantID)
		}
	})
	t.Run("a policy of one tenant is not another's", func(t *testing.T) {
		other := newEnv(t, withDeps(func(d *Deps) { d.Operator = allow }))
		_, err := other.svc.SetRetention(ctx, admin, 10)
		noErr(t, err)
		pol, err := other.svc.RetentionPolicy(ctx, scopeOf(testApp, tenB, ws1, bob, "ses_b", adminCaps...))
		noErr(t, err)
		if pol.Tenant != nil || pol.EffectiveDays != 0 {
			t.Errorf("tenant B sees %+v", pol)
		}
		pol, err = other.svc.RetentionPolicy(ctx, scopeOf(otherApp, tenA, ws1, alice, "ses_a", adminCaps...))
		noErr(t, err)
		if pol.Tenant != nil {
			t.Errorf("another application's tenant of the same name sees %+v", pol)
		}
	})
}

// The retention job runs only as its declared service, and what it deletes is
// only what the policy says.
func TestRetentionJobRunsOnlyAsTheService(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sc := e.svc.serviceScope(testApp, tenA, ws1)
	if err := sc.Validate(); err != nil {
		t.Fatalf("the service scope is invalid: %v", err)
	}
	e.say(scopeOf(testApp, tenA, ws1, alice, "ses_1"), "old", "")
	e.clk.Advance(400 * day)

	for name, job := range map[string]jobs.Job{
		"a user's own job":      {Scope: scopeOf(testApp, tenA, ws1, alice, "ses_1", adminCaps...), Type: JobRetention},
		"another identity":      {Scope: sc, Type: JobRetention, ServiceIdentity: "rag.maintenance"},
		"an unknown payload":    {Scope: sc, Type: JobRetention, ServiceIdentity: ServiceIdentity, Payload: []byte(`{"workspace":"all"}`)},
		"a scope that is bogus": {Scope: contracts.Scope{TenantID: "x"}, Type: JobRetention, ServiceIdentity: ServiceIdentity},
	} {
		err := e.svc.retentionHandler(ctx, job)
		if err == nil || !jobs.IsPermanent(err) {
			t.Errorf("%s: err = %v, want a permanent failure", name, err)
		}
	}
	if e.transcriptRows() != 1 {
		t.Fatal("a refused retention job deleted rows")
	}
	// The real thing, with a policy.
	_, err := e.svc.SetRetention(ctx, scopeOf(testApp, tenA, ws1, alice, "ses_a", adminCaps...), 30)
	noErr(t, err)
	for _, payload := range []string{"", "null", "{}"} {
		e.say(scopeOf(testApp, tenA, ws1, alice, "ses_2"), "again", "") // something old enough each round
		e.clk.Advance(40 * day)
		err := e.svc.retentionHandler(ctx, jobs.Job{Scope: sc, Type: JobRetention, ServiceIdentity: ServiceIdentity, Payload: []byte(payload)})
		noErr(t, err)
	}
	if e.transcriptRows() != 0 {
		t.Errorf("%d rows left after the retention jobs", e.transcriptRows())
	}
}

func TestScheduleRetentionNeedsAQueue(t *testing.T) {
	e := newEnv(t, withDeps(func(d *Deps) { d.Queue, d.DeploymentID = nil, "" }))
	_, err := e.svc.ScheduleRetention(context.Background())
	wantCode(t, err, contracts.ErrUnavailable)
	if _, err := New(Deps{DB: e.db, Transcripts: e.sess, Audit: e.alog, Queue: e.q}); err == nil {
		t.Error("a service with a queue and no deployment id was built")
	}
	for name, d := range map[string]Deps{
		"no store":      {Transcripts: e.sess, Audit: e.alog},
		"no transcript": {DB: e.db, Audit: e.alog},
		"no audit":      {DB: e.db, Transcripts: e.sess},
	} {
		if _, err := New(d); err == nil || contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: New = %v", name, err)
		}
	}
}

// The sweep reads the workspaces a batch at a time and misses none and repeats
// none, whatever the batch size.
func TestScheduleRetentionPagesWorkspaces(t *testing.T) {
	for _, batch := range []int{1, 2, 3, 200} {
		e := newEnv(t, withDeps(func(d *Deps) { d.Config.SweepBatch = batch; d.Operator = allow }))
		ctx := context.Background()
		_, err := e.svc.SetApplicationRetention(ctx, scopeOf(testApp, tenA, ws1, "usr_operator", "ses_op"), 7)
		noErr(t, err)
		for _, ten := range []string{tenA, tenB, "ten_c"} {
			for _, ws := range []string{ws1, ws2} {
				e.say(scopeOf(testApp, ten, ws, alice, "ses_1"), "old", "")
			}
		}
		// A workspace of an application with no policy is looked at but never queued.
		e.say(scopeOf(otherApp, tenA, ws1, alice, "ses_1"), "old", "")
		e.clk.Advance(10 * day)
		res, err := e.svc.ScheduleRetention(ctx)
		noErr(t, err)
		if res.Workspaces != 7 || res.Enqueued != 6 || res.Existing != 0 {
			t.Errorf("batch %d: schedule = %+v, want 7 workspaces and 6 jobs", batch, res)
		}
	}
}
