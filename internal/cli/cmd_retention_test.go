package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

const (
	retentionTestApp    = "demo-app"
	retentionTestOther  = "other-app"
	retentionTestSigner = "retention-test-key"
	retentionTestDay    = 24 * time.Hour
)

// retentionFakeSource is the days the "installed packages" declare; an application that is not in
// it has no active package.
type retentionFakeSource map[string]retentionDays

func (s retentionFakeSource) Retention(_ context.Context, app string) (retentionDays, bool, error) {
	d, ok := s[app]
	return d, ok, nil
}

// retentionRig is the retention job on the RAG fixture's store, queue and pool, with an audit log
// that holds a platform signing key registered in the trust store (as `keys init` does).
type retentionRig struct {
	t     *testing.T
	f     *ragFixture
	log   *audit.Log
	stack *retentionStack
}

func retentionNewAuditLog(t *testing.T, db *store.DB, clk clock.Clock, sign bool) *audit.Log {
	t.Helper()
	ts := trust.NewStore(db, clk)
	opts := audit.Options{Clock: clk, Verifier: trust.NewVerifier(ts, clk)}
	if sign {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ts.RegisterKey(context.Background(), trust.PlatformApplicationID, retentionTestSigner, pub, "retention test", trust.WithPurpose(trust.PurposeAuditCheckpoint)); err != nil {
			t.Fatal(err)
		}
		opts.Signing = &trust.SigningKey{KeyID: retentionTestSigner, Key: priv}
	}
	lg, err := audit.New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	return lg
}

func retentionNewRig(t *testing.T, src retentionSource) *retentionRig {
	t.Helper()
	historyRetentionDeployment(t)
	f := newRagFixture(t)
	r := &retentionRig{t: t, f: f, log: retentionNewAuditLog(t, f.db, f.clk, true)}

	oldEvery, oldCompose, oldSource := retentionEvery, retentionOnCompose, retentionSourceOf
	retentionEvery = 24 * time.Hour
	retentionOnCompose = func(s *retentionStack) { r.stack = s }
	retentionSourceOf = func(serveDeps) (retentionSource, error) { return src, nil }
	t.Cleanup(func() { retentionEvery, retentionOnCompose, retentionSourceOf = oldEvery, oldCompose, oldSource })

	services := gateway.NewServices()
	if err := gateway.Provide(services, f.stack); err != nil {
		t.Fatal(err)
	}
	d := serveDeps{Log: slog.New(slog.DiscardHandler), Clock: f.clk, DB: f.db, Audit: r.log, Services: services}
	if err := retentionProvide(context.Background(), d); err != nil {
		t.Fatalf("retentionProvide: %v", err)
	}
	if r.stack == nil {
		t.Fatal("the provider gave no job")
	}
	return r
}

// run queues the sweep's jobs and lets the stack's pool run them.
func (r *retentionRig) run() (queued int) {
	r.t.Helper()
	n, err := r.stack.enqueue(context.Background())
	if err != nil {
		r.t.Fatalf("enqueue: %v", err)
	}
	if n > 0 {
		if r.f.poll() == 0 {
			r.t.Fatal("the pool ran nothing")
		}
		r.f.drain()
	}
	return n
}

func (r *retentionRig) count(query string, args ...any) int {
	r.t.Helper()
	return r.f.countRows(query, args...)
}

func (r *retentionRig) exec(query string, args ...any) {
	r.t.Helper()
	if _, err := r.f.db.ExecContext(context.Background(), query, args...); err != nil {
		r.t.Fatalf("%s: %v", query, err)
	}
}

// retentionPrep inserts a preparation (and its receipt) in a state, last changed at the clock's now.
func retentionPrep(t *testing.T, db *store.DB, now time.Time, app, tenant, principal, id, state string) {
	t.Helper()
	ms := now.UnixMilli()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO action_preparations (id, application_id, tenant_id, workspace_id, principal_id, session_id, scope_json, capability_id,
		   state, risk_level, confirmation_mode, confirmation_mac, idempotency_key, fields_json, prepared_json, snapshot_version, data_digest,
		   prepared_at, expires_at, commit_attempts, receipt_id, updated_at)
		 VALUES (?, ?, ?, 'ws-1', ?, 'ses_1', '{}', 'items.create', ?, 1, 'none', '', ?, '{"name":"FIELDMARKER"}', '{}', 'v1', 'd', ?, ?, 0, ?, ?)`,
		id, app, tenant, principal, state, "idem_"+id, ms, ms+600000, "rcpt_"+id, ms); err != nil {
		t.Fatalf("insert preparation: %v", err)
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO action_receipts (id, application_id, tenant_id, workspace_id, preparation_id, status, receipt_json, issued_at)
		 VALUES (?, ?, ?, 'ws-1', ?, ?, '{"facts":"RECEIPTMARKER"}', ?)`,
		"rcpt_"+id, app, tenant, id, map[bool]string{true: "uncertain", false: "committed"}[state == "uncertain"], ms); err != nil {
		t.Fatalf("insert receipt: %v", err)
	}
}

// retentionEvent appends an audit event of a tenant chain at the log's clock.
func retentionEvent(t *testing.T, lg *audit.Log, app, tenant, principal string, typ audit.EventType, data map[string]any) audit.Event {
	t.Helper()
	loc, err := audit.ForIDs(app, tenant, "ws-1", principal, "ses_1")
	if err != nil {
		t.Fatal(err)
	}
	e, err := lg.AppendAt(context.Background(), loc, audit.NewEvent{Type: typ, Data: data})
	if err != nil {
		t.Fatalf("append %s: %v", typ, err)
	}
	return e
}

// retentionErased reports whether the event's payload is gone (replaced by the marker).
func (r *retentionRig) erased(id string) bool {
	r.t.Helper()
	return r.count(`SELECT COUNT(*) FROM audit_events WHERE id = ? AND data = ?`, id, audit.ErasedMarker) == 1
}

// serve queues the retention job and the RAG stack's pool runs it: receipts (with their preparations)
// past action_receipt_days are removed, audit payloads past audit_days become signed tombstones, and
// nothing that a pending or uncertain action depends on, nothing recent and nothing of an application
// with no declared retention is touched. The audit chains still verify against a checkpoint taken
// before, and the sweep is recorded in them.
func TestRetentionJobRemovesExpiredReceiptsAndAuditPayloads(t *testing.T) {
	r := retentionNewRig(t, retentionFakeSource{retentionTestApp: {Receipts: 30, Audit: 60}})
	ctx := context.Background()
	t0 := r.f.clk.Now()

	// Old rows (now minus 100 days), then recent ones (minus 5 days).
	retentionPrep(t, r.f.db, t0, retentionTestApp, "tenant-a", "usr_alice", "prep_done_old", "committed")
	retentionPrep(t, r.f.db, t0, retentionTestApp, "tenant-a", "usr_alice", "prep_unc_old", "uncertain")
	retentionPrep(t, r.f.db, t0, retentionTestApp, "tenant-b", "usr_bob", "prep_b_old", "rejected")
	retentionPrep(t, r.f.db, t0, retentionTestOther, "tenant-x", "usr_x", "prep_x_old", "committed")
	oldDone := retentionEvent(t, r.log, retentionTestApp, "tenant-a", "usr_alice", audit.EventActionCommitted, map[string]any{"preparation_id": "prep_done_old", "note": "OLDSECRET"})
	oldUncertain := retentionEvent(t, r.log, retentionTestApp, "tenant-a", "usr_alice", audit.EventActionUncertain, map[string]any{"preparation_id": "prep_unc_old"})
	oldPlain := retentionEvent(t, r.log, retentionTestApp, "tenant-a", "usr_alice", audit.EventSettingsChanged, map[string]any{"note": "OLDSECRET"})
	oldB := retentionEvent(t, r.log, retentionTestApp, "tenant-b", "usr_bob", audit.EventSettingsChanged, map[string]any{"note": "OLDSECRET-B"})
	appLoc, _ := audit.ForApplication(retentionTestApp, "")
	oldApp, err := r.log.AppendAt(ctx, appLoc, audit.NewEvent{Type: audit.EventPackageInstalled, Data: map[string]any{"note": "OLDSECRET-APP"}})
	if err != nil {
		t.Fatal(err)
	}
	oldOther := retentionEvent(t, r.log, retentionTestOther, "tenant-x", "usr_x", audit.EventSettingsChanged, map[string]any{"note": "OLDSECRET-X"})

	r.f.clk.Advance(95 * retentionTestDay)
	retentionPrep(t, r.f.db, r.f.clk.Now(), retentionTestApp, "tenant-a", "usr_alice", "prep_done_recent", "committed")
	recent := retentionEvent(t, r.log, retentionTestApp, "tenant-a", "usr_alice", audit.EventSettingsChanged, map[string]any{"note": "RECENT"})
	r.f.clk.Advance(5 * retentionTestDay)

	// A checkpoint taken before the sweep: every hash it covers must be unchanged after it.
	keyA, keyB := audit.ChainKey{ApplicationID: retentionTestApp, TenantID: "tenant-a"}, audit.ChainKey{ApplicationID: retentionTestApp, TenantID: "tenant-b"}
	keyApp := audit.ChainKey{ApplicationID: retentionTestApp}
	checkpoints := map[audit.ChainKey]audit.Checkpoint{}
	for _, k := range []audit.ChainKey{keyA, keyB, keyApp} {
		cp, err := r.log.Checkpoint(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		checkpoints[k] = cp
	}

	// The job: one per application, class 6, under the retention service identity.
	if n := r.run(); n != 2 {
		t.Fatalf("%d jobs were queued, want one per application", n)
	}
	if n := r.count(`SELECT count(*) FROM jobs WHERE type = ? AND class = ? AND service_identity = ? AND state = 'succeeded'`,
		retentionJobType, int(jobs.ClassMaintenance), retentionServiceIdentity); n != 2 {
		t.Fatalf("%d retention jobs succeeded, want 2\n%s", n, r.f.dumpJobs())
	}

	// Receipts: expired and finished are gone with their preparation; recent, pending and uncertain stay.
	for _, id := range []string{"prep_done_old", "prep_b_old"} {
		if n := r.count(`SELECT COUNT(*) FROM action_preparations WHERE id = ?`, id) + r.count(`SELECT COUNT(*) FROM action_receipts WHERE preparation_id = ?`, id); n != 0 {
			t.Errorf("%s: %d rows are left", id, n)
		}
	}
	for _, id := range []string{"prep_done_recent", "prep_unc_old", "prep_x_old"} {
		if n := r.count(`SELECT COUNT(*) FROM action_preparations WHERE id = ?`, id) + r.count(`SELECT COUNT(*) FROM action_receipts WHERE preparation_id = ?`, id); n != 2 {
			t.Errorf("%s: %d rows are left, want its preparation and its receipt", id, n)
		}
	}

	// Audit payloads: expired ones are tombstones, the others are untouched.
	for name, e := range map[string]audit.Event{"old action event": oldDone, "old plain event": oldPlain, "tenant-b": oldB, "application chain": oldApp} {
		if !r.erased(e.ID) {
			t.Errorf("%s: the payload is still there", name)
		}
	}
	for name, e := range map[string]audit.Event{"event of an uncertain action": oldUncertain, "recent event": recent, "application with no declared retention": oldOther} {
		if r.erased(e.ID) {
			t.Errorf("%s was erased", name)
		}
	}
	if n := r.count(`SELECT COUNT(*) FROM audit_events WHERE data LIKE '%OLDSECRET%' AND application_id = ?`, retentionTestApp); n != 0 {
		t.Errorf("%d events of the application still hold an old secret", n)
	}

	// The tombstones say who, why and when; the sweep is recorded in the chains it touched.
	var body string
	if err := r.f.db.QueryRowContext(ctx, `SELECT body FROM audit_tombstones WHERE event_id = ?`, oldPlain.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var tb audit.TombstoneBody
	if err := json.Unmarshal([]byte(body), &tb); err != nil {
		t.Fatal(err)
	}
	if tb.Operator != retentionOperator || tb.Reason != "audit retention of 60 days" || tb.EventHash != oldPlain.Hash {
		t.Errorf("tombstone = %+v", tb)
	}
	if n := r.count(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'audit.erased' AND application_id = ?`, retentionTestApp); n != 3 {
		t.Errorf("%d audit.erased records, want one per chain (3)", n)
	}
	if n := r.count(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'deletion.receipts' AND application_id = ?`, retentionTestApp); n != 2 {
		t.Errorf("%d deletion.receipts records, want one per tenant (2)", n)
	}

	// The chains verify, anchored to the checkpoints from before the sweep.
	for k, cp := range checkpoints {
		rep, err := r.log.VerifyChain(ctx, k, audit.VerifyOptions{RequireAnchor: true, Expected: &cp})
		switch {
		case err != nil:
			t.Errorf("chain %s: %v %+v", k, err, rep.Problems)
		case rep.Erased == 0 || !rep.Anchored:
			t.Errorf("chain %s verifies but reports %d erased events (anchored %t)", k, rep.Erased, rep.Anchored)
		}
	}

	// Run again: the same period queues nothing; the next period queues the jobs and they find nothing.
	erasedRecords := r.count(`SELECT COUNT(*) FROM audit_events WHERE event_type IN ('audit.erased', 'deletion.receipts')`)
	if n := r.run(); n != 0 {
		t.Errorf("a second pass in the same period queued %d jobs", n)
	}
	r.f.clk.Advance(retentionEvery)
	if n := r.run(); n != 2 {
		t.Fatalf("the next period queued %d jobs, want 2", n)
	}
	if got := r.count(`SELECT COUNT(*) FROM audit_events WHERE event_type IN ('audit.erased', 'deletion.receipts')`); got != erasedRecords {
		t.Errorf("an idle sweep recorded %d new events", got-erasedRecords)
	}
	for _, k := range []audit.ChainKey{keyA, keyB, keyApp} {
		if _, err := r.log.VerifyChain(ctx, k, audit.VerifyOptions{}); err != nil {
			t.Errorf("chain %s after the second sweep: %v", k, err)
		}
	}
}

// Without the platform signing key audit payloads are not erased (never without their tombstone),
// and the sweep says so; receipts still go.
func TestRetentionWithoutASigningKeyKeepsAuditPayloads(t *testing.T) {
	r := retentionNewRig(t, retentionFakeSource{retentionTestApp: {Receipts: 30, Audit: 60}})
	t0 := r.f.clk.Now()
	retentionPrep(t, r.f.db, t0, retentionTestApp, "tenant-a", "usr_alice", "prep_done_old", "committed")
	old := retentionEvent(t, r.log, retentionTestApp, "tenant-a", "usr_alice", audit.EventSettingsChanged, map[string]any{"note": "OLDSECRET"})
	r.f.clk.Advance(100 * retentionTestDay)

	unsigned := retentionNewAuditLog(t, r.f.db, r.f.clk, false)
	eng := &retentionEngine{db: r.f.db, log: unsigned, clk: r.f.clk, src: retentionFakeSource{retentionTestApp: {Receipts: 30, Audit: 60}}, lg: slog.New(slog.DiscardHandler)}
	res, err := eng.application(context.Background(), retentionTestApp)
	if err != nil {
		t.Fatal(err)
	}
	if res.PreparationsRemoved != 1 || res.AuditErased != 0 || !strings.Contains(res.AuditSkipped, "signing key") {
		t.Errorf("result = %+v", res)
	}
	if r.erased(old.ID) || r.count(`SELECT COUNT(*) FROM audit_tombstones`) != 0 {
		t.Error("a payload was erased with no key")
	}
}

// A sweep tombstones a long chain in short transactions: the single writer is never held for more
// than 200 events (each a signature and its writes), so prepare, confirm and commit are not stalled
// behind it. The result is the same as one long transaction: every expired payload is gone and the
// chain verifies.
func TestRetentionErasesInShortTransactions(t *testing.T) {
	ctx := context.Background()
	db := opsInspectDB(t, filepath.Join(t.TempDir(), "alixai.db"))
	clk := clock.NewFake(time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC))
	lg := retentionNewAuditLog(t, db, clk, true)
	const events = 450
	for i := 0; i < events; i++ {
		retentionEvent(t, lg, retentionTestApp, "tenant-a", "usr_alice", audit.EventSettingsChanged, map[string]any{"n": i})
	}
	clk.Advance(100 * retentionTestDay)
	eng := &retentionEngine{db: db, log: lg, clk: clk, src: retentionFakeSource{retentionTestApp: {Receipts: 30, Audit: 60}}, lg: slog.New(slog.DiscardHandler)}
	res, err := eng.application(ctx, retentionTestApp)
	if err != nil || res.AuditErased != events || res.More {
		t.Fatalf("result = %+v, %v", res, err)
	}
	rows, err := db.QueryContext(ctx, `SELECT data FROM audit_events WHERE event_type = 'audit.erased' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	var per []int
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			t.Fatal(err)
		}
		var rec audit.EraseRecord
		if err := json.Unmarshal([]byte(data), &rec); err != nil {
			t.Fatal(err)
		}
		per = append(per, rec.Erased)
	}
	_ = rows.Close()
	if fmt.Sprint(per) != "[200 200 50]" {
		t.Errorf("the erasure transactions tombstoned %v events, want at most 200 each ([200 200 50])", per)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE data = ?`, audit.ErasedMarker).Scan(&n); err != nil || n != events {
		t.Errorf("%d payloads were erased, want %d (%v)", n, events, err)
	}
	if _, err := lg.VerifyChain(ctx, audit.ChainKey{ApplicationID: retentionTestApp, TenantID: "tenant-a"}, audit.VerifyOptions{}); err != nil {
		t.Errorf("the chain after the sweep: %v", err)
	}
}

// An application with no active package declares nothing, and nothing of it is removed: unknown is
// never read as expired. Retention below the schema's minimum is not applied either.
func TestRetentionNeverGuessesWhatAPackageDeclares(t *testing.T) {
	r := retentionNewRig(t, retentionFakeSource{})
	retentionPrep(t, r.f.db, r.f.clk.Now(), retentionTestApp, "tenant-a", "usr_alice", "prep_done_old", "committed")
	e := retentionEvent(t, r.log, retentionTestApp, "tenant-a", "usr_alice", audit.EventSettingsChanged, map[string]any{"note": "OLDSECRET"})
	r.f.clk.Advance(3000 * retentionTestDay)
	for name, src := range map[string]retentionSource{
		"no package": retentionFakeSource{},
		"zero days":  retentionFakeSource{retentionTestApp: {Receipts: 0, Audit: 0}},
	} {
		eng := &retentionEngine{db: r.f.db, log: r.log, clk: r.f.clk, src: src, lg: slog.New(slog.DiscardHandler)}
		res, err := eng.application(context.Background(), retentionTestApp)
		if err != nil || res.Skipped == "" || res.PreparationsRemoved != 0 || res.AuditErased != 0 {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}
	if n := r.count(`SELECT COUNT(*) FROM action_preparations`); n != 1 || r.erased(e.ID) {
		t.Error("something was removed")
	}
	// The platform's own chain has no package either.
	eng := &retentionEngine{db: r.f.db, log: r.log, clk: r.f.clk, src: retentionFakeSource{trust.PlatformApplicationID: {Receipts: 1, Audit: 1}}, lg: slog.New(slog.DiscardHandler)}
	if res, _ := eng.application(context.Background(), trust.PlatformApplicationID); res.Skipped == "" {
		t.Errorf("the platform application was swept: %+v", res)
	}
}

// The pool runs the job only for the service identity, the job type, a payload that names exactly the
// application of the scope, and the retention service's own scope.
func TestRetentionJobAuthority(t *testing.T) {
	ok := jobs.Job{Type: retentionJobType, ServiceIdentity: retentionServiceIdentity,
		Scope: retentionServiceScope("dep-1", retentionTestApp), Payload: json.RawMessage(`{"application_id":"demo-app"}`)}
	if err := (retentionAuthority{}).AuthorizeService(context.Background(), retentionServiceIdentity, ok); err != nil {
		t.Fatalf("the real job is refused: %v", err)
	}
	mut := func(f func(*jobs.Job)) jobs.Job { j := ok; j.Scope = ok.Scope.Clone(); f(&j); return j }
	for name, tc := range map[string]struct {
		identity string
		job      jobs.Job
	}{
		"another identity":         {"history.retention", ok},
		"another job type":         {retentionServiceIdentity, mut(func(j *jobs.Job) { j.Type = "history.retention" })},
		"a payload of another app": {retentionServiceIdentity, mut(func(j *jobs.Job) { j.Payload = json.RawMessage(`{"application_id":"other-app"}`) })},
		"an extra payload field":   {retentionServiceIdentity, mut(func(j *jobs.Job) { j.Payload = json.RawMessage(`{"application_id":"demo-app","all":true}`) })},
		"no payload":               {retentionServiceIdentity, mut(func(j *jobs.Job) { j.Payload = nil })},
		"a real tenant's scope":    {retentionServiceIdentity, mut(func(j *jobs.Job) { j.Scope.TenantID = "tenant-a" })},
		"a user's principal":       {retentionServiceIdentity, mut(func(j *jobs.Job) { j.Scope.PrincipalID = "usr_alice" })},
		"an invalid scope":         {retentionServiceIdentity, mut(func(j *jobs.Job) { j.Scope.DeploymentID = "" })},
	} {
		if err := (retentionAuthority{}).AuthorizeService(context.Background(), tc.identity, tc.job); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// ---- retention erase -----------------------------------------------------------------

// retentionData opens the data directory's store for a test, runs fn and closes it, so the command
// that follows has the file to itself.
func retentionData(t *testing.T, dataDir string, fn func(db *store.DB)) {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dataDir, store.DefaultFileName)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fn(db)
}

func retentionCount(t *testing.T, dataDir, query string, args ...any) (n int) {
	t.Helper()
	retentionData(t, dataDir, func(db *store.DB) {
		if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
	})
	return n
}

// retentionEraseWorld makes a data directory with the platform key (`keys init`, as an operator
// does) and, in tenant-a, a finished action with its receipt and events, an uncertain action with
// its receipt and events, and plain events; plus a finished action and event of tenant-b.
func retentionEraseWorld(t *testing.T) (cfg, dataDir string) {
	t.Helper()
	cfg, dataDir = minimalConfig(t, "")
	if code, out, errOut := runCLI(t, "keys", "init", "-config", cfg); code != ExitOK {
		t.Fatalf("keys init = %d\n%s\n%s", code, out, errOut)
	}
	retentionData(t, dataDir, func(db *store.DB) {
		clk := clock.NewFake(time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC))
		lg, err := audit.New(db, audit.Options{Clock: clk})
		if err != nil {
			t.Fatal(err)
		}
		retentionPrep(t, db, clk.Now(), retentionTestApp, "tenant-a", "usr_alice", "prep_done", "committed")
		retentionPrep(t, db, clk.Now(), retentionTestApp, "tenant-a", "usr_bob", "prep_live", "uncertain")
		retentionPrep(t, db, clk.Now(), retentionTestApp, "tenant-b", "usr_alice", "prep_b", "committed")
		for _, e := range []struct {
			tenant, principal string
			typ               audit.EventType
			data              map[string]any
		}{
			{"tenant-a", "usr_alice", audit.EventActionPrepared, map[string]any{"preparation_id": "prep_done"}},
			{"tenant-a", "usr_alice", audit.EventActionCommitted, map[string]any{"preparation_id": "prep_done"}},
			{"tenant-a", "usr_bob", audit.EventActionUncertain, map[string]any{"preparation_id": "prep_live"}},
			{"tenant-a", "usr_bob", audit.EventSettingsChanged, map[string]any{"note": "BOBSECRET"}},
			{"tenant-a", "usr_carol", audit.EventSettingsChanged, map[string]any{"note": "PAYLOAD naming", "subject": "usr_alice"}},
			{"tenant-b", "usr_alice", audit.EventSettingsChanged, map[string]any{"note": "OTHERTENANT"}},
		} {
			clk.Advance(time.Minute)
			retentionEvent(t, lg, retentionTestApp, e.tenant, e.principal, e.typ, e.data)
		}
	})
	return cfg, dataDir
}

// A tenant's erasure is refused, whole and with nothing written, while an event of it is depended on by
// a pending or uncertain action; the refusal is the same on a dry run; once that action is over the
// request goes through, the audit chain still verifies with the product's own command, and the other
// tenant is untouched.
func TestErasureRefusesEventsPendingActionsDependOn(t *testing.T) {
	cfg, dataDir := retentionEraseWorld(t)
	erase := func(extra ...string) (int, string, string) {
		args := append([]string{"retention", "erase", "-config", cfg, "-application", retentionTestApp, "-tenant", "tenant-a",
			"-reason", "tenant erasure request", "-operator", "ops-alice"}, extra...)
		return runCLI(t, args...)
	}
	snapshot := func() string {
		var sb strings.Builder
		retentionData(t, dataDir, func(db *store.DB) {
			for _, q := range []string{
				`SELECT group_concat(id || ':' || hash || ':' || data, '|') FROM (SELECT id, hash, data FROM audit_events ORDER BY seq)`,
				`SELECT group_concat(id || ':' || state, '|') FROM (SELECT id, state FROM action_preparations ORDER BY seq)`,
				`SELECT group_concat(id, '|') FROM (SELECT id FROM action_receipts ORDER BY seq)`,
				`SELECT COUNT(*) FROM audit_tombstones`,
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
	before := snapshot()

	for _, extra := range [][]string{nil, {"-dry-run"}} {
		code, out, errOut := erase(extra...)
		if code == ExitOK {
			t.Fatalf("erase %v succeeded while an action is uncertain:\n%s", extra, out)
		}
		if !strings.Contains(errOut, "refused") || !strings.Contains(errOut, "nothing was erased") {
			t.Errorf("erase %v: stderr does not say it was refused:\n%s", extra, errOut)
		}
		if got := snapshot(); got != before {
			t.Fatalf("a refused erasure (%v) changed the database:\nbefore:\n%s\nafter:\n%s", extra, before, got)
		}
	}
	// A principal's erasure that does not touch the uncertain action's events, or Bob's, goes through
	// while it is still pending: what depends on nothing of Alice's may go.
	code, out, errOut := runCLI(t, "retention", "erase", "-config", cfg, "-application", retentionTestApp, "-tenant", "tenant-a",
		"-principal", "usr_carol", "-reason", "principal erasure request", "-operator", "ops-alice", "-dry-run")
	if code != ExitOK {
		t.Fatalf("a dry run for a principal the pending action does not involve = %d\n%s\n%s", code, out, errOut)
	}
	var dry retentionEraseReport
	if err := json.Unmarshal([]byte(out), &dry); err != nil || !dry.DryRun || dry.AuditEventsErased != 1 || dry.PreparationsRemoved != 0 {
		t.Errorf("dry run = %+v (%v)\n%s", dry, err, out)
	}
	if got := snapshot(); got != before {
		t.Fatal("a dry run changed the database")
	}

	// An event of Alice's that names Bob's uncertain action is depended on although Alice has no pending
	// action of her own: only the audit guard stands between her erasure and that event.
	retentionData(t, dataDir, func(db *store.DB) {
		lg, err := audit.New(db, audit.Options{Clock: clock.NewFake(time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))})
		if err != nil {
			t.Fatal(err)
		}
		retentionEvent(t, lg, retentionTestApp, "tenant-a", "usr_alice", audit.EventActionConfirmed, map[string]any{"preparation_id": "prep_live"})
	})
	before = snapshot()
	code, out, errOut = runCLI(t, "retention", "erase", "-config", cfg, "-application", retentionTestApp, "-tenant", "tenant-a",
		"-principal", "usr_alice", "-reason", "principal erasure request", "-operator", "ops-alice")
	if code == ExitOK || !strings.Contains(errOut, "refused") {
		t.Fatalf("erasing the events of a principal that name an uncertain action = %d\n%s\n%s", code, out, errOut)
	}
	if got := snapshot(); got != before {
		t.Fatal("a refused principal erasure changed the database")
	}

	// The action ends: the same request now goes through.
	retentionData(t, dataDir, func(db *store.DB) {
		if _, err := db.ExecContext(context.Background(), `UPDATE action_preparations SET state = 'committed' WHERE id = 'prep_live'`); err != nil {
			t.Fatal(err)
		}
	})
	code, out, errOut = erase()
	if code != ExitOK {
		t.Fatalf("erase after the action ended = %d\n%s\n%s", code, out, errOut)
	}
	var rep retentionEraseReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.DryRun || rep.AuditEventsErased != 6 || rep.PreparationsRemoved != 2 || rep.ReceiptsRemoved != 2 {
		t.Fatalf("report = %+v (%v)\n%s", rep, err, out)
	}
	if n := retentionCount(t, dataDir, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = 'tenant-a' AND data LIKE '%SECRET%'`); n != 0 {
		t.Errorf("%d events of the tenant still hold a payload", n)
	}
	if n := retentionCount(t, dataDir, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = 'tenant-a' AND data = ?`, audit.ErasedMarker); n != 6 {
		t.Errorf("%d erased events, want 6", n)
	}
	if n := retentionCount(t, dataDir, `SELECT COUNT(*) FROM action_preparations WHERE tenant_id = 'tenant-a'`); n != 0 {
		t.Errorf("%d preparations of the tenant are left", n)
	}
	if n := retentionCount(t, dataDir, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = 'tenant-b' AND data LIKE '%OTHERTENANT%'`) +
		retentionCount(t, dataDir, `SELECT COUNT(*) FROM action_preparations WHERE tenant_id = 'tenant-b'`); n != 2 {
		t.Error("another tenant's data was touched")
	}

	// It is shown with the product's own command that the chain still verifies.
	code, out, errOut = runCLI(t, "audit", "verify-chain", "-data-dir", dataDir)
	if code != ExitOK || !strings.Contains(out, `"chains_failed": 0`) {
		t.Fatalf("audit verify-chain after the erasure = %d\n%s\n%s", code, out, errOut)
	}
	retentionData(t, dataDir, func(db *store.DB) {
		clk := clock.Real{}
		lg, err := audit.New(db, audit.Options{Clock: clk, Verifier: trust.NewVerifier(trust.NewStore(db, clk), clk)})
		if err != nil {
			t.Fatal(err)
		}
		rep, err := lg.VerifyChain(context.Background(), audit.ChainKey{ApplicationID: retentionTestApp, TenantID: "tenant-a"}, audit.VerifyOptions{})
		if err != nil || rep.Erased != 6 || rep.Events != 8 {
			t.Errorf("chain tenant-a: %v %+v", err, rep)
		}
	})
}

// A principal's erasure takes the events that name the principal (envelope or payload) and the
// finished preparations they made, and leaves the rest of the tenant.
func TestErasureOfAPrincipalTakesWhatNamesThem(t *testing.T) {
	cfg, dataDir := retentionEraseWorld(t)
	code, out, errOut := runCLI(t, "retention", "erase", "-config", cfg, "-application", retentionTestApp, "-tenant", "tenant-a",
		"-principal", "usr_alice", "-reason", "principal erasure request", "-operator", "ops-alice")
	if code != ExitOK {
		t.Fatalf("erase = %d\n%s\n%s", code, out, errOut)
	}
	var rep retentionEraseReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	// Alice's two events by envelope, and Carol's whose payload names her; her finished preparation.
	if rep.AuditEventsErased != 3 || rep.PreparationsRemoved != 1 || rep.ReceiptsRemoved != 1 || rep.PrincipalID != "usr_alice" {
		t.Errorf("report = %+v", rep)
	}
	if n := retentionCount(t, dataDir, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = 'tenant-a' AND data LIKE '%BOBSECRET%'`); n != 1 {
		t.Error("Bob's event was erased by Alice's erasure")
	}
	if n := retentionCount(t, dataDir, `SELECT COUNT(*) FROM action_preparations WHERE id = 'prep_live'`); n != 1 {
		t.Error("Bob's uncertain preparation was removed")
	}
	// The tombstones name the operator and the reason, never the principal.
	if n := retentionCount(t, dataDir, `SELECT COUNT(*) FROM audit_tombstones WHERE body LIKE '%"operator":"ops-alice"%' AND body LIKE '%principal erasure request%'`); n != 3 {
		t.Errorf("%d tombstones carry the operator and the reason, want 3", n)
	}
	if n := retentionCount(t, dataDir, `SELECT COUNT(*) FROM audit_events WHERE event_type IN ('audit.erased', 'deletion.receipts') AND data LIKE '%usr_alice%'`); n != 0 {
		t.Errorf("%d erasure records name the principal", n)
	}
}

// An erasure larger than the batches one run may take is not reported as done: the
// command prints what it removed, says there is more and exits non-zero; the next
// run takes the rest.
func TestErasureThatStopsAtTheBatchLimitSaysSo(t *testing.T) {
	cfg, dataDir := minimalConfig(t, "")
	if code, out, errOut := runCLI(t, "keys", "init", "-config", cfg); code != ExitOK {
		t.Fatalf("keys init = %d\n%s\n%s", code, out, errOut)
	}
	retentionData(t, dataDir, func(db *store.DB) {
		ms := time.Now().UnixMilli()
		if _, err := db.ExecContext(context.Background(),
			`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i <= ?)
			 INSERT INTO action_preparations (id, application_id, tenant_id, workspace_id, principal_id, session_id, scope_json, capability_id,
			   state, risk_level, confirmation_mode, confirmation_mac, idempotency_key, fields_json, prepared_json, snapshot_version, data_digest,
			   prepared_at, expires_at, commit_attempts, updated_at)
			 SELECT 'prep_' || i, ?, 'tenant-a', 'ws-1', 'usr_alice', 'ses_1', '{}', 'items.create', 'committed', 1, 'none', '', 'idem_' || i,
			   '{}', '{}', 'v1', 'd', ?, ?, 0, ? FROM n`,
			actions.MaxEraseBatch, retentionTestApp, ms, ms+600000, ms); err != nil {
			t.Fatal(err)
		}
	})
	old := retentionEraseMaxBatches
	retentionEraseMaxBatches = 1
	t.Cleanup(func() { retentionEraseMaxBatches = old })
	erase := func() (int, string, string) {
		return runCLI(t, "retention", "erase", "-config", cfg, "-application", retentionTestApp, "-tenant", "tenant-a",
			"-reason", "tenant erasure request", "-operator", "ops-alice")
	}

	code, out, errOut := erase()
	if code == ExitOK || !strings.Contains(errOut, "run it again") {
		t.Fatalf("an erasure stopped by the batch limit = %d\n%s\n%s", code, out, errOut)
	}
	var rep retentionEraseReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || !rep.More || rep.DryRun || rep.PreparationsRemoved != actions.MaxEraseBatch {
		t.Fatalf("report = %+v (%v)\n%s", rep, err, out)
	}
	if n := retentionCount(t, dataDir, `SELECT COUNT(*) FROM action_preparations`); n != 1 {
		t.Fatalf("%d preparations left, want 1", n)
	}
	code, out, errOut = erase()
	if code != ExitOK {
		t.Fatalf("the run that takes the rest = %d\n%s\n%s", code, out, errOut)
	}
	if n := retentionCount(t, dataDir, `SELECT COUNT(*) FROM action_preparations`); n != 0 {
		t.Errorf("%d preparations left after the second run", n)
	}
}

// Usage and fail-closed refusals of the commands.
func TestRetentionCommandsRefuse(t *testing.T) {
	cfg, dataDir := retentionEraseWorld(t)
	before := retentionCount(t, dataDir, `SELECT COUNT(*) FROM audit_events WHERE data = ?`, audit.ErasedMarker)
	for name, args := range map[string][]string{
		"no application": {"retention", "erase", "-config", cfg, "-tenant", "tenant-a", "-reason", "r", "-operator", "o"},
		"no tenant":      {"retention", "erase", "-config", cfg, "-application", retentionTestApp, "-reason", "r", "-operator", "o"},
		"no reason":      {"retention", "erase", "-config", cfg, "-application", retentionTestApp, "-tenant", "tenant-a", "-operator", "o"},
		"no operator":    {"retention", "erase", "-config", cfg, "-application", retentionTestApp, "-tenant", "tenant-a", "-reason", "r"},
		"an argument":    {"retention", "erase", "-config", cfg, "-application", retentionTestApp, "-tenant", "tenant-a", "-reason", "r", "-operator", "o", "extra"},
		"run argument":   {"retention", "run", "-config", cfg, "extra"},
	} {
		if code, _, _ := runCLI(t, args...); code != ExitUsage {
			t.Errorf("%s: exit %d, want %d", name, code, ExitUsage)
		}
	}
	if got := retentionCount(t, dataDir, `SELECT COUNT(*) FROM audit_events WHERE data = ?`, audit.ErasedMarker); got != before {
		t.Error("a refused command erased something")
	}

	// No platform key: nothing is erased, receipts included.
	cfg2, dataDir2 := minimalConfig(t, "")
	retentionData(t, dataDir2, func(db *store.DB) {
		retentionPrep(t, db, time.Now(), retentionTestApp, "tenant-a", "usr_alice", "prep_done", "committed")
	})
	code, _, errOut := runCLI(t, "retention", "erase", "-config", cfg2, "-application", retentionTestApp, "-tenant", "tenant-a", "-reason", "r", "-operator", "o")
	if code == ExitOK || !strings.Contains(errOut, "signing key") {
		t.Errorf("erase with no platform key = %d\n%s", code, errOut)
	}
	if n := retentionCount(t, dataDir2, `SELECT COUNT(*) FROM action_preparations`); n != 1 {
		t.Error("receipts were erased with no key to tombstone the audit payloads")
	}

	// `retention run` with no package installed removes nothing, whatever the age.
	code, out, errOut := runCLI(t, "retention", "run", "-config", cfg)
	if code != ExitOK || !strings.Contains(out, "no active package declares a retention") {
		t.Errorf("retention run = %d\n%s\n%s", code, out, errOut)
	}
	if n := retentionCount(t, dataDir, `SELECT COUNT(*) FROM action_preparations`); n != 3 {
		t.Error("retention run removed a preparation of an application with no declared retention")
	}
}

// `retention run` reads the days from the manifest of the application's ACTIVE installed package (the
// real installer, the real signature check): receipts past action_receipt_days (365 in the example
// package) and audit payloads past audit_days (730) are removed; younger rows, the package's own
// lifecycle events and a quarantined package's application are left alone.
func TestRetentionRunUsesTheDaysTheActivePackageDeclares(t *testing.T) {
	e := newOpsEnv(t, "")
	e.mustRun("keys", "init")
	e.mustRun("trust", "add-key", "-app", "demo-library", "-key-id", devkeys.KeyID, "-public-key", devkeys.PublicKeyBase64(), "-allow-test-keys")
	var inst installOut
	decode(t, e.mustRun("package", "install", "-operator", "root", filepath.Join(fixtures, "valid-minimal")).stdout, &inst)
	if inst.State != "quarantined" {
		t.Fatalf("install = %+v", inst)
	}

	now := time.Now().UTC()
	seed := func(activate bool) {
		retentionData(t, filepath.Join(e.dir, "data"), func(db *store.DB) {
			ctx := context.Background()
			if activate {
				if _, err := db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rpt_test' WHERE application_id = 'demo-library'`); err != nil {
					t.Fatal(err)
				}
				return
			}
			clk := clock.NewFake(now.Add(-800 * 24 * time.Hour))
			lg, err := audit.New(db, audit.Options{Clock: clk})
			if err != nil {
				t.Fatal(err)
			}
			retentionPrep(t, db, now.Add(-400*24*time.Hour), "demo-library", "tenant-a", "usr_alice", "prep_old", "committed")
			retentionPrep(t, db, now.Add(-300*24*time.Hour), "demo-library", "tenant-a", "usr_alice", "prep_mid", "committed")
			retentionEvent(t, lg, "demo-library", "tenant-a", "usr_alice", audit.EventSettingsChanged, map[string]any{"note": "OLDSECRET"})
			clk.Set(now.Add(-300 * 24 * time.Hour))
			retentionEvent(t, lg, "demo-library", "tenant-a", "usr_alice", audit.EventSettingsChanged, map[string]any{"note": "MIDSECRET"})
		})
	}
	seed(false)

	// Quarantined: no active package declares anything, so nothing is removed, however old.
	out := e.mustRun("retention", "run").stdout
	if !strings.Contains(out, "no active package declares a retention") {
		t.Fatalf("a quarantined package's application was swept:\n%s", out)
	}
	if n := retentionCount(t, filepath.Join(e.dir, "data"), `SELECT COUNT(*) FROM action_preparations`); n != 2 {
		t.Fatalf("%d preparations after a sweep of a quarantined application, want 2", n)
	}

	seed(true)
	out = e.mustRun("retention", "run").stdout
	var rep struct {
		Applications []retentionResult `json:"applications"`
	}
	decode(t, out, &rep)
	if len(rep.Applications) != 1 {
		t.Fatalf("report = %s", out)
	}
	got := rep.Applications[0]
	if got.ReceiptDays != 365 || got.AuditDays != 730 || got.PreparationsRemoved != 1 || got.ReceiptsRemoved != 1 || got.AuditErased != 1 || got.Skipped != "" {
		t.Errorf("result = %+v\n%s", got, out)
	}
	data := filepath.Join(e.dir, "data")
	if n := retentionCount(t, data, `SELECT COUNT(*) FROM action_preparations WHERE id = 'prep_mid'`); n != 1 {
		t.Error("a preparation inside the declared days was removed")
	}
	if n := retentionCount(t, data, `SELECT COUNT(*) FROM audit_events WHERE data LIKE '%OLDSECRET%'`); n != 0 {
		t.Error("an audit payload past the declared days is still there")
	}
	if n := retentionCount(t, data, `SELECT COUNT(*) FROM audit_events WHERE data LIKE '%MIDSECRET%'`); n != 1 {
		t.Error("an audit payload inside the declared days was erased")
	}
	if n := retentionCount(t, data, `SELECT COUNT(*) FROM audit_events WHERE event_type = 'package.installed' AND data = ?`, audit.ErasedMarker); n != 0 {
		t.Error("the package's own lifecycle event was erased")
	}
	// The chains still verify with the product's own command.
	if res := e.run("audit", "verify-chain", "-application", "demo-library"); res.code != ExitOK || !strings.Contains(res.stdout, `"chains_failed": 0`) {
		t.Errorf("audit verify-chain after the sweep = %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	// A second sweep has nothing to do and records nothing.
	before := retentionCount(t, data, `SELECT COUNT(*) FROM audit_events`)
	e.mustRun("retention", "run")
	if after := retentionCount(t, data, `SELECT COUNT(*) FROM audit_events`); after != before {
		t.Errorf("an idle sweep appended %d events", after-before)
	}
}
