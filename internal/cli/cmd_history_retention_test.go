package cli

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/settings"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	historyRetentionTestApp = "app_demo"
	historyRetentionTestDep = "dep_cli"
)

// historyRetentionTestScope is a user of the demo application with these capability scopes.
func historyRetentionTestScope(tenant string, caps ...string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: historyRetentionTestDep, ApplicationID: historyRetentionTestApp, TenantID: tenant, WorkspaceID: "wsp_1",
		PrincipalID: "usr_a", Roles: []string{"member"}, CapabilityScopes: caps, SessionID: "ses_a",
	}
}

// historyRetentionSay stores one turn of the scope's session, written at the time `at`: the
// session store is the one that owns the transcript, so the turn is stored the way the dialogue
// engine stores it.
func historyRetentionSay(t *testing.T, db *store.DB, at time.Time, sc contracts.Scope, id string) {
	t.Helper()
	ctx := context.Background()
	st, err := sessions.New(db, clock.NewFake(at), sessions.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.Open(ctx, sc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.Save(ctx, sess, &sessions.TurnRecord{TurnID: "trn_" + id, User: "question " + id, Act: "answer", Assistant: "answer " + id}); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func historyRetentionCount(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// historyRetentionDeployment sets identity.deployment_id for a test that calls the provider
// directly (serve reads it from the configuration it loads).
func historyRetentionDeployment(t *testing.T) {
	t.Helper()
	old := *identity.ConfigSection()
	identity.ConfigSection().DeploymentID = historyRetentionTestDep
	t.Cleanup(func() { *identity.ConfigSection() = old })
}

// serve schedules the retention job and the RAG stack's one pool runs it: turns past the tenant's
// policy are deleted by a history.retention job under the service identity, a turn inside the
// policy stays, and the job went through the pool the stack built (it has no other).
func TestHistoryRetentionJobRunsInServe(t *testing.T) {
	oldCfg, oldHist, oldRag := historyRetentionConfig, historyRetentionOnCompose, ragOnCompose
	histories, stacks := make(chan *history.Service, 1), make(chan *ragStack, 1)
	historyRetentionConfig = history.Config{RetentionInterval: 100 * time.Millisecond}
	historyRetentionOnCompose = func(s *history.Service) { histories <- s }
	ragOnCompose = func(s *ragStack) { stacks <- s }
	t.Cleanup(func() { historyRetentionConfig, historyRetentionOnCompose, ragOnCompose = oldCfg, oldHist, oldRag })

	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	cfg, _ := serveConfig(t, f, identityYAML)
	srv := startServe(t, cfg)
	var svc *history.Service
	var st *ragStack
	for svc == nil || st == nil {
		select {
		case svc = <-histories:
		case st = <-stacks:
		case <-time.After(15 * time.Second):
			t.Fatal("serve did not build the history service and the RAG stack")
		}
	}
	db := st.input.DB
	sc := historyRetentionTestScope("ten_a", history.CapRetention)
	other := historyRetentionTestScope("ten_b", history.CapRetention)
	old := time.Now().Add(-30 * 24 * time.Hour)
	historyRetentionSay(t, db, old, sc, "old1")
	historyRetentionSay(t, db, old, sc, "old2")
	historyRetentionSay(t, db, time.Now(), sc, "recent")
	historyRetentionSay(t, db, old, other, "other")

	// Only ten_a has a policy: ten_b keeps everything.
	if _, err := svc.SetRetention(context.Background(), sc, 7); err != nil {
		t.Fatal(err)
	}
	turns := func(tenant string) int {
		return historyRetentionCount(t, db, "SELECT count(*) FROM transcript_turns WHERE tenant_id = ?", tenant)
	}
	before := turns("ten_a")
	pollUntil(t, 30*time.Second, "the retention job to delete the old turns", func() bool { return turns("ten_a") < before })
	pollUntil(t, 30*time.Second, "the retention job to succeed", func() bool {
		return historyRetentionCount(t, db, "SELECT count(*) FROM jobs WHERE type = ? AND state = 'succeeded' AND service_identity = ?",
			history.JobRetention, history.ServiceIdentity) > 0
	})
	if got := historyRetentionCount(t, db, "SELECT count(*) FROM transcript_turns WHERE tenant_id = 'ten_a' AND turn_id LIKE 'trn_old%'"); got != 0 {
		t.Errorf("%d turns past the policy are left", got)
	}
	if turns("ten_a") == 0 {
		t.Error("the turn inside the policy was deleted too")
	}
	if got := turns("ten_b"); got != 2 {
		t.Errorf("a tenant with no policy has %d turns left, want its 2", got)
	}
	if err := srv.stop(); err != nil {
		t.Fatalf("serve stopped with %v", err)
	}
}

// historyRetentionBoot is one start of serve's history provider over a RAG stack: it provides the
// service, runs the scheduling loop's start function and reports the result of its first pass.
func historyRetentionBoot(t *testing.T, f *ragFixture, sess *sessions.Store) (svc *history.Service, first history.ScheduleResult) {
	t.Helper()
	services := gateway.NewServices()
	if err := gateway.Provide(services, sess); err != nil {
		t.Fatal(err)
	}
	if err := gateway.Provide(services, f.stack); err != nil {
		t.Fatal(err)
	}
	var start func(context.Context) func()
	d := serveDeps{
		Log: slog.New(slog.DiscardHandler), Clock: f.clk, DB: f.db, Audit: f.aud, Services: services,
		AddService: func(s func(context.Context) (stop func())) { start = s },
	}
	if err := historyProvide(context.Background(), d); err != nil {
		t.Fatalf("historyProvide: %v", err)
	}
	var ok bool
	if svc, ok = gateway.Lookup[*history.Service](services); !ok || start == nil {
		t.Fatalf("the provider gave no history service (%t) or no scheduling loop (%t)", ok, start != nil)
	}
	passes := make(chan history.ScheduleResult, 1)
	oldPass := historyRetentionOnPass
	historyRetentionOnPass = func(r history.ScheduleResult, err error) {
		if err != nil {
			t.Errorf("a scheduling pass failed: %v", err)
		}
		select {
		case passes <- r:
		default:
		}
	}
	t.Cleanup(func() { historyRetentionOnPass = oldPass })
	stop := start(context.Background())
	select {
	case first = <-passes:
	case <-time.After(15 * time.Second):
		t.Fatal("the scheduling loop did not run a pass at start")
	}
	stop()
	return svc, first
}

// A restart inside a period queues nothing a second time: the job of a workspace is keyed by the
// period, so the loop's first pass after a restart finds the job already queued. The next period
// queues one more, so the key is the period and not a permanent suppression.
func TestHistoryRetentionIsNotScheduledTwiceForOnePeriod(t *testing.T) {
	historyRetentionDeployment(t)
	f := newRagFixture(t)
	sess, err := sessions.New(f.db, f.clk, sessions.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sc := historyRetentionTestScope("ten_a", history.CapRetention)
	historyRetentionSay(t, f.db, f.clk.Now().Add(-30*24*time.Hour), sc, "old")
	jobsOf := func() int {
		return historyRetentionCount(t, f.db, "SELECT count(*) FROM jobs WHERE type = ? AND service_identity = ?", history.JobRetention, history.ServiceIdentity)
	}

	svc, first := historyRetentionBoot(t, f, sess)
	if _, err := svc.SetRetention(context.Background(), sc, 7); err != nil {
		t.Fatal(err)
	}
	// The first pass ran before the policy existed: it found nothing due and queued nothing.
	if first.Enqueued != 0 || jobsOf() != 0 {
		t.Fatalf("a deployment with no policy queued %d jobs", jobsOf())
	}
	res, err := svc.ScheduleRetention(context.Background())
	if err != nil || res.Enqueued != 1 || jobsOf() != 1 {
		t.Fatalf("the pass with a policy queued %d (%+v, %v), want one job", jobsOf(), res, err)
	}

	// A restart: a new process, a new stack and pool over the same store, the same period.
	f2 := newRagFixture(t, func(o *ragFixtureOpts) { o.over = f })
	svc2, first := historyRetentionBoot(t, f2, sess)
	if first.Enqueued != 0 || first.Existing != 1 || jobsOf() != 1 {
		t.Fatalf("after a restart in the same period: %+v, %d jobs; want the one job found, none added", first, jobsOf())
	}
	if res, err := svc2.ScheduleRetention(context.Background()); err != nil || res.Enqueued != 0 || res.Existing != 1 {
		t.Errorf("a second pass in the period = %+v, %v", res, err)
	}

	f.clk.Advance(2 * history.DefaultRetentionInterval)
	if res, err := svc2.ScheduleRetention(context.Background()); err != nil || res.Enqueued != 1 || jobsOf() != 2 {
		t.Errorf("the next period queued %d jobs (%+v, %v), want a second one", jobsOf(), res, err)
	}
}

// The pool accepts history.ServiceIdentity only once the history area registered it, only for the
// retention job type and only with a complete scope; the identity of ingestion stays ingestion's.
func TestHistoryRetentionPoolAuthority(t *testing.T) {
	f := newRagFixture(t)
	ctx := context.Background()
	auth := ragAuthorizer{packages: f.pkgs, sources: f.stack.sources, others: f.stack.others}
	job := func(typ string, sc contracts.Scope) jobs.Job { return jobs.Job{Type: typ, Scope: sc} }
	sc := historyRetentionTestScope("ten_a", history.CapCleanup)

	if err := auth.AuthorizeService(ctx, history.ServiceIdentity, job(history.JobRetention, sc)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Fatalf("an identity nobody registered was accepted: %v", err)
	}
	if err := f.stack.shareJobs(history.ServiceIdentity, historyRetentionAuthority{}, func(*jobs.Pool) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := auth.AuthorizeService(ctx, history.ServiceIdentity, job(history.JobRetention, sc)); err != nil {
		t.Errorf("the retention job was refused: %v", err)
	}
	for name, tc := range map[string]struct {
		identity string
		job      jobs.Job
	}{
		"another job type":     {history.ServiceIdentity, job(ingest.JobSync, sc)},
		"an empty scope":       {history.ServiceIdentity, job(history.JobRetention, contracts.Scope{})},
		"an unregistered name": {"history.retention.x", job(history.JobRetention, sc)},
		"ingestion's own name": {ragServiceIdentity, job(history.JobRetention, sc)},
		"the empty identity":   {"", job(history.JobRetention, sc)},
	} {
		if err := auth.AuthorizeService(ctx, tc.identity, tc.job); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// Ingestion's identity cannot be taken over, and one name is registered once.
	if err := f.stack.shareJobs(ragServiceIdentity, historyRetentionAuthority{}, func(*jobs.Pool) error { return nil }); err == nil {
		t.Error("the identity of ingestion was registered for another area")
	}
	if err := f.stack.shareJobs(history.ServiceIdentity, historyRetentionAuthority{}, func(*jobs.Pool) error { return nil }); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("a name registered twice: %v", err)
	}
}

// The settings adapter: a tenant write needs no history capability (settings authorised it), the
// application-wide policy needs the token's settings.deployment.write (the Operator hook), and
// history's own refusal, a tenant above its application, reaches the caller.
func TestHistoryRetentionSettingsAdapter(t *testing.T) {
	db := storetest.Open(t, nil)
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	alog, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.New(db, clk, sessions.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := history.New(history.Deps{DB: db, Transcripts: sess, Audit: alog, Clock: clk, Operator: historyRetentionOperator})
	if err != nil {
		t.Fatal(err)
	}
	a := historyRetentionSettings{svc: svc}
	ctx := context.Background()
	// set writes through the adapter in a transaction of its own, as the settings Put does in its own.
	set := func(sc contracts.Scope, level settings.Level, days int) error {
		return db.Tx(ctx, func(tx *store.Tx) error { return a.SetRetention(ctx, tx, sc, level, days) })
	}
	tenantScope := historyRetentionTestScope("ten_a", settings.CapabilityTenantWrite, settings.CapabilityUserRead)
	deploymentScope := historyRetentionTestScope("ten_a", settings.CapabilityDeploymentWrite)

	if err := set(deploymentScope, settings.LevelDeployment, 30); err != nil {
		t.Fatalf("the application policy with settings.deployment.write: %v", err)
	}
	if err := set(tenantScope, settings.LevelDeployment, 10); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("the application policy without settings.deployment.write = %v, want permission_denied", err)
	}
	if err := set(tenantScope, settings.LevelTenant, 60); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("a tenant above its application = %v, want validation_failed", err)
	}
	if err := set(tenantScope, settings.LevelTenant, 14); err != nil {
		t.Fatalf("a tenant policy with only settings.tenant.write: %v", err)
	}
	got, err := a.Retention(ctx, tenantScope)
	if err != nil || got != (settings.Retention{Application: 30, Tenant: 14, EffectiveDays: 14, Source: "tenant"}) {
		t.Errorf("Retention = %+v, %v", got, err)
	}
	// The tenant's token alone does not let the application's policy through history either.
	if _, err := svc.SetApplicationRetention(ctx, tenantScope, 5); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("history.SetApplicationRetention with a tenant token = %v", err)
	}
	if err := set(tenantScope, settings.LevelTenant, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Retention(ctx, tenantScope); got.Tenant != 0 || got.EffectiveDays != 30 || got.Source != "application" {
		t.Errorf("after clearing the tenant policy: %+v", got)
	}
	if err := set(tenantScope, settings.LevelUser, 5); err == nil {
		t.Error("the user level was sent to history")
	}
}
