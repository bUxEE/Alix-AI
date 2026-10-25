package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	opsInspectReconcileTestApp    = "demo-app"
	opsInspectReconcileTestTenant = "tenant-a"
	opsInspectReconcileTestHost   = `{
  "policy_id": "default-access",
  "role_grants": [ { "role": "member", "capabilities": ["items.get", "items.create"] } ],
  "risk_overrides": [],
  "field_projection": []
}`
)

// opsInspectReconcileRig is a coordinator over the RAG fixture's store, audit log and clock, with the
// in-memory host behind it, and the serve provider of the reconcile job provided on the fixture's
// stack: the same parts serve builds, with a clock the test moves.
type opsInspectReconcileRig struct {
	t     *testing.T
	f     *ragFixture
	co    *actions.Coordinator
	app   *fake.App
	stack *opsInspectReconcileStack
	start func(context.Context) func()
}

func opsInspectReconcileTestPackage(t *testing.T) *policy.Package {
	t.Helper()
	hp, err := policy.ParseHostPolicy([]byte(opsInspectReconcileTestHost))
	if err != nil {
		t.Fatal(err)
	}
	p, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: opsInspectReconcileTestApp, Version: "1.0.0", Status: policy.StatusActive, Roles: []string{"member"},
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
	return p
}

func opsInspectNewReconcileRig(t *testing.T) *opsInspectReconcileRig {
	t.Helper()
	f := newRagFixture(t)
	r := &opsInspectReconcileRig{t: t, f: f, app: fake.NewApp(fake.WithClock(f.clk))}
	guard, err := adapters.Wrap(fake.NewAdapter(r.app), adapters.Options{ApplicationID: opsInspectReconcileTestApp})
	if err != nil {
		t.Fatal(err)
	}
	pkg := opsInspectReconcileTestPackage(t)
	r.co, err = actions.New(actions.Deps{
		DB: f.db, Policy: policy.NewGateway(f.clk), Audit: f.aud, Clock: f.clk,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return pkg, nil }),
		Adapters: actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}

	oldEvery, oldCompose := opsInspectReconcileEvery, opsInspectReconcileOnCompose
	opsInspectReconcileEvery = 10 * time.Minute
	opsInspectReconcileOnCompose = func(s *opsInspectReconcileStack) { r.stack = s }
	t.Cleanup(func() { opsInspectReconcileEvery, opsInspectReconcileOnCompose = oldEvery, oldCompose })

	services := gateway.NewServices()
	if err := gateway.Provide(services, f.stack); err != nil {
		t.Fatal(err)
	}
	d := serveDeps{
		Log: slog.New(slog.DiscardHandler), Clock: f.clk, DB: f.db, Audit: f.aud, Services: services, Actions: r.co,
		AddService: func(s func(context.Context) (stop func())) { r.start = s },
	}
	if err := opsInspectReconcileProvide(context.Background(), d); err != nil {
		t.Fatalf("opsInspectReconcileProvide: %v", err)
	}
	if r.stack == nil || r.start == nil {
		t.Fatalf("the provider gave no job (%t) or no loop (%t)", r.stack != nil, r.start != nil)
	}
	return r
}

func opsInspectReconcileTestScope(session string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-1", ApplicationID: opsInspectReconcileTestApp, TenantID: opsInspectReconcileTestTenant, WorkspaceID: "ws-1",
		PrincipalID: "usr_alice", Roles: []string{"member"}, CapabilityScopes: []string{fake.ReadCapability, fake.ActionCapability}, SessionID: session,
	}
}

// uncertain prepares an item and commits it with the faults given: the host's reply is lost or its
// request is dropped, and the Status call that follows is refused, so the outcome is uncertain.
func (r *opsInspectReconcileRig) uncertain(name string, applied bool) (scope contracts.Scope, id string) {
	r.t.Helper()
	ctx := context.Background()
	scope = opsInspectReconcileTestScope("ses_" + name)
	p, err := r.co.Prepare(ctx, scope, actions.PrepareInput{Session: scope, CapabilityID: fake.ActionCapability, Fields: map[string]any{"name": name}})
	if err != nil {
		r.t.Fatalf("Prepare %s: %v", name, err)
	}
	id = string(p.Action.PreparationID)
	if applied {
		r.app.Inject(adapters.OpCommit, fake.FaultDropReply) // applied, the reply is lost
	} else {
		r.app.Inject(adapters.OpCommit, fake.FaultDropRequest) // never applied
	}
	r.app.Inject(adapters.OpStatus, fake.FaultDeny) // and Status is refused: nothing proves which
	if _, err := r.co.Commit(ctx, scope, id); contracts.CodeOf(err) != contracts.ErrUncertainOutcome {
		r.t.Fatalf("Commit %s = %v, want an uncertain outcome", name, err)
	}
	if st := r.state(id); st != "uncertain" {
		r.t.Fatalf("%s is %s, want uncertain", name, st)
	}
	return scope, id
}

func (r *opsInspectReconcileRig) state(id string) string {
	r.t.Helper()
	var s string
	if err := r.f.db.QueryRowContext(context.Background(), `SELECT state FROM action_preparations WHERE id = ?`, id).Scan(&s); err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *opsInspectReconcileRig) receipts(id string) []string {
	r.t.Helper()
	rows, err := r.f.db.QueryContext(context.Background(), `SELECT status FROM action_receipts WHERE preparation_id = ? ORDER BY seq`, id)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// events are the action.* audit events that name the preparation, oldest first.
func (r *opsInspectReconcileRig) events(id string) string {
	r.t.Helper()
	rows, err := r.f.db.QueryContext(context.Background(),
		`SELECT event_type FROM audit_events WHERE event_type LIKE 'action.%' AND data LIKE ? ORDER BY seq`, "%"+id+"%")
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, strings.TrimPrefix(s, "action."))
	}
	return strings.Join(out, ",")
}

func (r *opsInspectReconcileRig) hostCalls(op adapters.Operation) int {
	n := 0
	for _, c := range r.app.Calls() {
		if c.Operation == op {
			n++
		}
	}
	return n
}

// age moves the clock past the time an outcome must have been uncertain before it is asked about.
func (r *opsInspectReconcileRig) age() { r.f.clk.Advance(opsInspectReconcileEvery + time.Minute) }

func (r *opsInspectReconcileRig) pass() opsInspectReconcilePass {
	r.t.Helper()
	res, err := r.stack.pass(context.Background())
	if err != nil {
		r.t.Fatalf("pass: %v", err)
	}
	return res
}

func (r *opsInspectReconcileRig) jobsOf(state string) int {
	return r.f.countRows(`SELECT count(*) FROM jobs WHERE type = ? AND class = ? AND service_identity = ? AND state = ?`,
		opsInspectReconcileJobType, int(jobs.ClassMaintenance), opsInspectReconcileServiceIdentity, state)
}

// serve runs the reconcile job: an outcome that ended uncertain is asked about again, through the
// coordinator, on the pool the RAG stack built, and what the application says is recorded with the
// coordinator's own receipts and audit events. A recorded outcome becomes a committed receipt; proof
// that the application never applied the commit reopens the preparation; an application that still
// cannot tell leaves it uncertain and is asked again by the next period's job. Nothing is ever
// committed again.
func TestServeReconcilesUncertainOutcomes(t *testing.T) {
	r := opsInspectNewReconcileRig(t)
	run := func() {
		t.Helper()
		if n := r.f.poll(); n == 0 {
			t.Fatal("the pool ran nothing")
		}
		r.f.drain()
	}

	t.Run("an applied commit whose reply was lost becomes a committed receipt", func(t *testing.T) {
		_, id := r.uncertain("applied", true)
		if got := r.receipts(id); len(got) != 1 || got[0] != "uncertain" {
			t.Fatalf("receipts before = %v", got)
		}
		// Not at once: the application was asked a moment ago, by the commit itself.
		if res := r.pass(); res.Examined != 0 || res.Enqueued != 0 {
			t.Fatalf("a pass right after the outcome became uncertain = %+v, want it to wait", res)
		}
		r.age()
		if res := r.pass(); res.Examined != 1 || res.Enqueued != 1 || res.Existing != 0 || res.Skipped != 0 {
			t.Fatalf("the pass = %+v, want one job queued", res)
		}
		// A restart or a second pass inside the period queues nothing again.
		if res := r.pass(); res.Enqueued != 0 || res.Existing != 1 {
			t.Fatalf("a second pass in the same period = %+v, want the job found and none added", res)
		}
		run()

		if st := r.state(id); st != "committed" {
			t.Fatalf("the preparation is %s, want committed", st)
		}
		// The ledger keeps the uncertain receipt and adds the committed one; the events are the coordinator's.
		if got := r.receipts(id); len(got) != 2 || got[0] != "uncertain" || got[1] != "committed" {
			t.Errorf("receipts = %v, want uncertain then committed", got)
		}
		if got := r.events(id); !strings.HasSuffix(got, "committing,uncertain,committed") {
			t.Errorf("events = %s, want ...committing,uncertain,committed", got)
		}
		// One job, class 6, under the service identity, and done.
		if got := r.jobsOf("succeeded"); got != 1 {
			t.Errorf("%d reconcile jobs succeeded, want 1\n%s", got, r.f.dumpJobs())
		}
		// Reconciling asked Status and never committed again.
		if got := r.hostCalls(adapters.OpCommit); got != 1 || r.app.Effects() != 1 {
			t.Errorf("the application received %d commits and has %d effects, want 1 and 1", got, r.app.Effects())
		}
		if res := r.pass(); res.Examined != 0 {
			t.Errorf("a resolved preparation is still read: %+v", res)
		}
	})

	t.Run("proof that the commit was never applied reopens the preparation", func(t *testing.T) {
		before := r.hostCalls(adapters.OpCommit)
		_, id := r.uncertain("never", false)
		r.age()
		r.pass()
		run()
		if st := r.state(id); st != "prepared" {
			t.Fatalf("the preparation is %s, want prepared (reopened, to be sent again with the same key)", st)
		}
		if got := r.events(id); !strings.HasSuffix(got, "committing,uncertain,prepared") {
			t.Errorf("events = %s, want ...committing,uncertain,prepared", got)
		}
		if got := r.receipts(id); len(got) != 1 || got[0] != "uncertain" {
			t.Errorf("a reopened preparation has receipts %v: reconciliation must not invent one", got)
		}
		if got := r.hostCalls(adapters.OpCommit); got != before+1 || r.app.Effects() != 1 {
			t.Errorf("the application received %d commits (%d before this case) and has %d effects: nothing may be sent again", got, before+1, r.app.Effects())
		}
	})

	t.Run("an application that still cannot tell is asked again by the next period's job", func(t *testing.T) {
		_, id := r.uncertain("undecided", true)
		r.age()
		r.app.Inject(adapters.OpStatus, fake.FaultDeny) // the first Status the job sends is refused
		if res := r.pass(); res.Examined != 1 || res.Enqueued != 1 {
			t.Fatalf("the pass = %+v", res)
		}
		run()
		if st := r.state(id); st != "uncertain" {
			t.Fatalf("the preparation is %s after a refused Status, want uncertain", st)
		}
		if got := r.receipts(id); len(got) != 1 {
			t.Errorf("receipts = %v: an undecided reconciliation writes nothing", got)
		}
		// The job is done (it asked), and is not retried in a loop by the pool.
		if got := r.jobsOf("succeeded"); got != 3 {
			t.Errorf("%d reconcile jobs succeeded, want 3\n%s", got, r.f.dumpJobs())
		}
		// Same period: nothing is queued again. Next period: a new job, and the answer is recorded.
		if res := r.pass(); res.Enqueued != 0 {
			t.Errorf("a pass in the same period queued %+v", res)
		}
		r.age()
		if res := r.pass(); res.Examined != 1 || res.Enqueued != 1 {
			t.Fatalf("the next period's pass = %+v, want the undecided preparation", res)
		}
		run()
		if st := r.state(id); st != "committed" {
			t.Errorf("the preparation is %s after the next period, want committed", st)
		}
		if got := r.hostCalls(adapters.OpCommit); got != 3 || r.app.Effects() != 2 {
			t.Errorf("the application received %d commits and has %d effects, want 3 and 2", got, r.app.Effects())
		}
	})
}

// The loop is a part of serve: it runs a pass when it starts and queues the jobs of what is old
// enough, and it stops, and is waited for, with the process.
func TestServeStartsTheReconcileLoop(t *testing.T) {
	r := opsInspectNewReconcileRig(t)
	_, id := r.uncertain("looped", true)
	r.age()

	passes := make(chan opsInspectReconcilePass, 4)
	old := opsInspectReconcileOnPass
	opsInspectReconcileOnPass = func(p opsInspectReconcilePass, err error) {
		if err != nil {
			t.Errorf("a pass failed: %v", err)
		}
		select {
		case passes <- p:
		default:
		}
	}
	t.Cleanup(func() { opsInspectReconcileOnPass = old })

	stop := r.start(context.Background())
	var first opsInspectReconcilePass
	select {
	case first = <-passes:
	case <-time.After(15 * time.Second):
		t.Fatal("the loop did not run a pass when it started")
	}
	stop() // returns only once the loop has ended
	if first.Enqueued != 1 {
		t.Fatalf("the first pass = %+v, want the one job queued", first)
	}
	r.f.poll()
	r.f.drain()
	if st := r.state(id); st != "committed" {
		t.Errorf("%s is %s after the job ran, want committed", id, st)
	}
}

// The pool runs the job only for the identity, the type, the payload and the scope the sweep made:
// a job that names one preparation under another scope, another payload or another identity is
// denied, so a queued job can never reconcile or read the state of another preparation.
func TestReconcileAuthorityBindsTheJobToItsPreparation(t *testing.T) {
	r := opsInspectNewReconcileRig(t)
	scope, id := r.uncertain("authority", true)
	other, otherID := r.uncertain("other", true)
	auth := opsInspectReconcileAuthority{db: r.f.db}
	job := func(mutate func(*jobs.Job)) jobs.Job {
		j := jobs.Job{Type: opsInspectReconcileJobType, Scope: scope, Payload: json.RawMessage(fmt.Sprintf(`{"preparation_id":%q}`, id)), ServiceIdentity: opsInspectReconcileServiceIdentity}
		if mutate != nil {
			mutate(&j)
		}
		return j
	}
	ctx := context.Background()
	if err := auth.AuthorizeService(ctx, opsInspectReconcileServiceIdentity, job(nil)); err != nil {
		t.Fatalf("the job the sweep makes is denied: %v", err)
	}
	for name, tc := range map[string]struct {
		identity string
		mutate   func(*jobs.Job)
	}{
		"another identity":            {"rag-ingest", nil},
		"another job type":            {opsInspectReconcileServiceIdentity, func(j *jobs.Job) { j.Type = "history.retention" }},
		"no payload":                  {opsInspectReconcileServiceIdentity, func(j *jobs.Job) { j.Payload = nil }},
		"an extra payload member":     {opsInspectReconcileServiceIdentity, func(j *jobs.Job) { j.Payload = json.RawMessage(fmt.Sprintf(`{"preparation_id":%q,"force":true}`, id)) }},
		"no such preparation":         {opsInspectReconcileServiceIdentity, func(j *jobs.Job) { j.Payload = json.RawMessage(`{"preparation_id":"prep_none"}`) }},
		"another preparation":         {opsInspectReconcileServiceIdentity, func(j *jobs.Job) { j.Payload = json.RawMessage(fmt.Sprintf(`{"preparation_id":%q}`, otherID)) }},
		"another tenant":              {opsInspectReconcileServiceIdentity, func(j *jobs.Job) { j.Scope.TenantID = "tenant-b" }},
		"another principal":           {opsInspectReconcileServiceIdentity, func(j *jobs.Job) { j.Scope.PrincipalID = "usr_mallory" }},
		"wider capability scopes":     {opsInspectReconcileServiceIdentity, func(j *jobs.Job) { j.Scope.CapabilityScopes = append(j.Scope.CapabilityScopes, "items.delete") }},
		"another role":                {opsInspectReconcileServiceIdentity, func(j *jobs.Job) { j.Scope.Roles = []string{"admin"} }},
		"a scope that is not a scope": {opsInspectReconcileServiceIdentity, func(j *jobs.Job) { j.Scope = contracts.Scope{} }},
	} {
		if err := auth.AuthorizeService(ctx, tc.identity, job(tc.mutate)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("%s: AuthorizeService = %v, want permission_denied", name, err)
		}
	}
	// The other preparation, under its own scope, is fine.
	oj := job(func(j *jobs.Job) {
		j.Scope = other
		j.Payload = json.RawMessage(fmt.Sprintf(`{"preparation_id":%q}`, otherID))
	})
	if err := auth.AuthorizeService(ctx, opsInspectReconcileServiceIdentity, oj); err != nil {
		t.Errorf("the other preparation under its own scope: %v", err)
	}
	// The handler holds the same line: it reconciles the preparation of the scope it was given.
	bad := job(func(j *jobs.Job) { j.Scope = other })
	if err := r.stack.handle(ctx, bad); err == nil {
		t.Error("the handler reconciled a preparation that is not in the job's scope")
	}
	if st := r.state(id); st != "uncertain" {
		t.Errorf("a refused job changed %s to %s", id, st)
	}
}

// The reconcile job is maintenance: class 6, behind every live turn and every other class.
func TestReconcileJobIsMaintenance(t *testing.T) {
	r := opsInspectNewReconcileRig(t)
	r.uncertain("class", true)
	r.age()
	r.pass()
	if got := r.jobsOf("pending"); got != 1 {
		t.Fatalf("%d pending reconcile jobs of class %d under %q\n%s", got, jobs.ClassMaintenance, opsInspectReconcileServiceIdentity, r.f.dumpJobs())
	}
}

// A pass lists the uncertain preparations of every tenant. It must follow the partial index of the
// uncertain ones, not walk every preparation of the deployment after the cursor.
func TestReconcileListUsesTheUncertainIndex(t *testing.T) {
	db := storetest.Open(t, nil)
	rows, err := db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+opsInspectReconcileListSQL, 1, 0, opsInspectReconcileBatch)
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
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "action_preparations_uncertain") || strings.Contains(joined, "TEMP B-TREE") {
		t.Errorf("the reconcile pass does not follow the uncertain index:\n%s", joined)
	}
}

// serve builds the job with the coordinator it runs the other sweeps with: with an uncertain
// preparation in the store, the loop of the real process queues a reconciliation under the service
// identity, and the pool the RAG stack built accepts it and runs it. (This deployment has no active
// package, so the application cannot be asked: the job fails with the reason, which is the proof that
// it reached the coordinator and was not denied by the pool's authority.)
func TestServeRunsTheReconcileJobWithTheCoordinator(t *testing.T) {
	oldEvery, oldCompose := opsInspectReconcileEvery, opsInspectReconcileOnCompose
	stacks := make(chan *opsInspectReconcileStack, 1)
	opsInspectReconcileEvery = 50 * time.Millisecond
	opsInspectReconcileOnCompose = func(s *opsInspectReconcileStack) { stacks <- s }
	t.Cleanup(func() { opsInspectReconcileEvery, opsInspectReconcileOnCompose = oldEvery, oldCompose })

	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	cfg, dataDir := serveConfig(t, f, "")

	// A preparation whose outcome ended uncertain in an earlier run.
	scope := contracts.Scope{DeploymentID: "dep_cli", ApplicationID: "app_demo", TenantID: "ten_acme", WorkspaceID: "wsp_main",
		PrincipalID: "usr_alice", Roles: []string{"member"}, CapabilityScopes: []string{"items.create"}, SessionID: "ses_dead_01"}
	var pa contracts.PreparedAction
	if err := json.Unmarshal([]byte(interruptedPrepared), &pa); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seed, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dataDir, store.DefaultFileName)})
	if err != nil {
		t.Fatal(err)
	}
	scopeJSON, _ := json.Marshal(scope)
	past := time.Now().Add(-time.Hour).UnixMilli()
	_, err = store.InsertScoped(context.Background(), seed, scope, "action_preparations",
		[]string{"id", "principal_id", "session_id", "scope_json", "capability_id", "state", "risk_level", "confirmation_mode", "confirmation_mac",
			"idempotency_key", "fields_json", "prepared_json", "snapshot_version", "data_digest", "prepared_at", "expires_at", "confirmed_at",
			"committing_at", "committing_from", "commit_attempts", "updated_at"},
		string(pa.PreparationID), scope.PrincipalID, scope.SessionID, string(scopeJSON), "items.create", "uncertain", 2, "explicit", strings.Repeat("a", 64),
		string(pa.IdempotencyKey), `{"name":"widget","quantity":3}`, interruptedPrepared, "v0", strings.Repeat("b", 64),
		past, past+300000, past, past, "confirmed", 1, past)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	srv := startServe(t, cfg)
	var st *opsInspectReconcileStack
	select {
	case st = <-stacks:
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not build the reconcile job")
	}
	if st.co == nil {
		t.Fatal("serve built the reconcile job without the action coordinator: it would reconcile nothing")
	}
	var code string
	pollUntil(t, 30*time.Second, "the reconcile job to run", func() bool {
		var c sql.NullString
		err := st.db.QueryRowContext(context.Background(),
			`SELECT error_code FROM jobs WHERE type = ? AND service_identity = ? AND attempts >= 1 AND error_code IS NOT NULL LIMIT 1`,
			opsInspectReconcileJobType, opsInspectReconcileServiceIdentity).Scan(&c)
		code = c.String
		return err == nil
	})
	t.Logf("the job ended with %q", code)
	if code == string(contracts.ErrPermissionDenied) {
		t.Errorf("the pool denied the reconcile job it was queued: its authority does not accept what the sweep queues")
	}
	if err := srv.stop(); err != nil {
		t.Fatalf("serve stopped with %v", err)
	}
}
