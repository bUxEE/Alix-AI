package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// retentionRig is a store with the audit and actions tables and a fake clock.
type retentionRig struct {
	t   *testing.T
	db  *store.DB
	clk *clock.Fake
	aud *audit.Log
}

func newRetentionRig(t *testing.T) *retentionRig {
	t.Helper()
	r := &retentionRig{t: t, clk: clock.NewFake(t0), db: storetest.Open(t, nil)}
	var err error
	if r.aud, err = audit.New(r.db, audit.Options{Clock: r.clk}); err != nil {
		t.Fatal(err)
	}
	return r
}

// prep inserts a preparation in the given state, last changed at `at`, with one
// receipt when withReceipt is set. Its fields hold a marker the erasure must remove.
func (r *retentionRig) prep(id, tenant, principal, state string, at time.Time, withReceipt bool) {
	r.t.Helper()
	ms := at.UnixMilli()
	receipt := any(nil)
	if withReceipt {
		receipt = "rcpt_" + id
	}
	if _, err := r.db.ExecContext(context.Background(),
		`INSERT INTO action_preparations (id, application_id, tenant_id, workspace_id, principal_id, session_id, scope_json, capability_id,
		   state, risk_level, confirmation_mode, confirmation_mac, idempotency_key, fields_json, prepared_json, snapshot_version, data_digest,
		   prepared_at, expires_at, commit_attempts, receipt_id, updated_at)
		 VALUES (?, 'demo-app', ?, 'ws-1', ?, 'ses_1', '{}', 'items.create', ?, 1, 'none', '', ?, '{"name":"FIELDMARKER"}', '{}', 'v1', 'd', ?, ?, 0, ?, ?)`,
		id, tenant, principal, state, "idem_"+id, ms, ms+600000, receipt, ms); err != nil {
		r.t.Fatalf("insert preparation: %v", err)
	}
	if withReceipt {
		if _, err := r.db.ExecContext(context.Background(),
			`INSERT INTO action_receipts (id, application_id, tenant_id, workspace_id, preparation_id, status, receipt_json, issued_at)
			 VALUES (?, 'demo-app', ?, 'ws-1', ?, 'committed', '{"facts":"RECEIPTMARKER"}', ?)`,
			"rcpt_"+id, tenant, id, ms); err != nil {
			r.t.Fatalf("insert receipt: %v", err)
		}
	}
}

func (r *retentionRig) count(query string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		r.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (r *retentionRig) erase(spec EraseSpec) (EraseOutcome, error) {
	r.t.Helper()
	var out EraseOutcome
	err := r.db.Tx(context.Background(), func(tx *store.Tx) error {
		var err error
		out, err = EraseFinished(context.Background(), tx, r.aud, spec)
		return err
	})
	return out, err
}

func eraseSpec(tenant string) EraseSpec {
	return EraseSpec{ApplicationID: "demo-app", TenantID: tenant, Kind: "tenant", Reason: "erasure request", Operator: "ops-alice"}
}

func TestEraseFinishedRemovesPreparationsAndReceipts(t *testing.T) {
	r := newRetentionRig(t)
	for i, st := range finishedStates {
		r.prep(fmt.Sprintf("prep_a%d", i), "tenant-a", "usr_alice", st, t0, true)
	}
	r.prep("prep_bob", "tenant-a", "usr_bob", "committed", t0, true)
	r.prep("prep_other", "tenant-b", "usr_alice", "committed", t0, true)

	// A principal erasure removes that principal's finished preparations and their receipts, and only those.
	spec := eraseSpec("tenant-a")
	spec.Kind, spec.PrincipalID = "principal", "usr_alice"
	out, err := r.erase(spec)
	if err != nil {
		t.Fatalf("EraseFinished: %v", err)
	}
	if out.Preparations != 4 || out.Receipts != 4 || out.Pending != 0 || out.RecordID == "" {
		t.Fatalf("outcome = %+v", out)
	}
	if n := r.count(`SELECT COUNT(*) FROM action_preparations`); n != 2 {
		t.Errorf("%d preparations left, want bob's and the other tenant's", n)
	}
	if n := r.count(`SELECT COUNT(*) FROM action_receipts`); n != 2 {
		t.Errorf("%d receipts left, want 2", n)
	}
	if n := r.count(`SELECT COUNT(*) FROM action_preparations WHERE principal_id = 'usr_alice' AND tenant_id = 'tenant-a'`); n != 0 {
		t.Errorf("%d of alice's preparations left", n)
	}

	// It is recorded, and the record names nobody.
	var data string
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT data FROM audit_events WHERE event_type = ? AND tenant_id = 'tenant-a'`, string(audit.EventDeletionReceipts)).Scan(&data); err != nil {
		t.Fatalf("no deletion record: %v", err)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		t.Fatal(err)
	}
	if d["preparations"] != float64(4) || d["receipts"] != float64(4) || d["kind"] != "principal" || d["operator"] != "ops-alice" {
		t.Errorf("record = %s", data)
	}
	if strings.Contains(data, "usr_alice") {
		t.Errorf("the record names the principal: %s", data)
	}

	// A tenant erasure then takes the rest of that tenant and leaves the other.
	out, err = r.erase(eraseSpec("tenant-a"))
	if err != nil || out.Preparations != 1 || out.Receipts != 1 {
		t.Fatalf("tenant erasure: %+v %v", out, err)
	}
	if n := r.count(`SELECT COUNT(*) FROM action_preparations WHERE tenant_id = 'tenant-b'`); n != 1 {
		t.Error("another tenant's preparation was erased")
	}
	if n := r.count(`SELECT COUNT(*) FROM action_receipts WHERE receipt_json LIKE '%RECEIPTMARKER%' AND tenant_id = 'tenant-a'`); n != 0 {
		t.Error("a receipt body is still there")
	}

	// Nothing left to erase: no event.
	before := r.count(`SELECT COUNT(*) FROM audit_events`)
	if out, err := r.erase(eraseSpec("tenant-a")); err != nil || out != (EraseOutcome{}) {
		t.Errorf("empty erasure: %+v %v", out, err)
	}
	if after := r.count(`SELECT COUNT(*) FROM audit_events`); after != before {
		t.Error("an erasure that erased nothing recorded an event")
	}
}

// A pending or uncertain action still depends on its preparation and receipts:
// the request is refused whole and nothing is removed (mutation: without the
// check these rows would be deleted). Once the action is over, the same request
// goes through.
func TestEraseFinishedRefusesWhileAnActionIsPending(t *testing.T) {
	for _, st := range pendingStates {
		t.Run(st, func(t *testing.T) {
			r := newRetentionRig(t)
			r.prep("prep_done", "tenant-a", "usr_alice", "committed", t0, true)
			r.prep("prep_live", "tenant-a", "usr_alice", st, t0, st == "uncertain")
			_, err := r.erase(eraseSpec("tenant-a"))
			if !errors.Is(err, ErrErasureBlocked) || contracts.CodeOf(err) != contracts.ErrConflict {
				t.Fatalf("err = %v", err)
			}
			if n := r.count(`SELECT COUNT(*) FROM action_preparations`); n != 2 {
				t.Errorf("%d preparations after a refused erasure, want 2", n)
			}
			if n := r.count(`SELECT COUNT(*) FROM action_receipts`); n < 1 {
				t.Error("a receipt was removed by a refused erasure")
			}
			if n := r.count(`SELECT COUNT(*) FROM audit_events`); n != 0 {
				t.Errorf("a refused erasure recorded %d events", n)
			}

			// The same request, once the action is over.
			if _, err := r.db.ExecContext(context.Background(), `UPDATE action_preparations SET state = 'rejected' WHERE id = 'prep_live'`); err != nil {
				t.Fatal(err)
			}
			out, err := r.erase(eraseSpec("tenant-a"))
			if err != nil || out.Preparations != 2 {
				t.Errorf("after the action ended: %+v %v", out, err)
			}
		})
	}

	t.Run("another tenant's or principal's pending action does not block", func(t *testing.T) {
		r := newRetentionRig(t)
		r.prep("prep_done", "tenant-a", "usr_alice", "committed", t0, true)
		r.prep("prep_live_b", "tenant-b", "usr_alice", "uncertain", t0, true)
		r.prep("prep_live_bob", "tenant-a", "usr_bob", "committing", t0, false)
		spec := eraseSpec("tenant-a")
		spec.Kind, spec.PrincipalID = "principal", "usr_alice"
		if out, err := r.erase(spec); err != nil || out.Preparations != 1 {
			t.Errorf("%+v %v", out, err)
		}
	})
}

func TestEraseFinishedRetention(t *testing.T) {
	r := newRetentionRig(t)
	old, recent := t0.Add(-400*24*time.Hour), t0.Add(-time.Hour)
	r.prep("prep_old", "tenant-a", "usr_alice", "committed", old, true)
	r.prep("prep_old_rej", "tenant-a", "usr_alice", "rejected", old, true)
	r.prep("prep_recent", "tenant-a", "usr_alice", "committed", recent, true)
	r.prep("prep_old_unc", "tenant-a", "usr_alice", "uncertain", old, true)
	r.prep("prep_old_new", "tenant-a", "usr_alice", "prepared", old, false)

	spec := eraseSpec("tenant-a")
	spec.Kind, spec.Reason, spec.Operator = "retention", "receipts past retention", "alixai.retention"
	spec.Before = t0.Add(-365 * 24 * time.Hour)

	// Without SkipPending the old uncertain preparation refuses the sweep.
	if _, err := r.erase(spec); !errors.Is(err, ErrErasureBlocked) {
		t.Fatalf("err = %v", err)
	}
	spec.SkipPending = true
	out, err := r.erase(spec)
	if err != nil || out.Preparations != 2 || out.Receipts != 2 || out.Pending != 2 {
		t.Fatalf("outcome = %+v, err %v", out, err)
	}
	if n := r.count(`SELECT COUNT(*) FROM action_preparations WHERE id IN ('prep_recent', 'prep_old_unc', 'prep_old_new')`); n != 3 {
		t.Errorf("%d of the recent and pending preparations are left, want 3", n)
	}
	if n := r.count(`SELECT COUNT(*) FROM action_receipts WHERE preparation_id = 'prep_old_unc'`); n != 1 {
		t.Error("the receipt of an uncertain action was removed")
	}
	// A second pass has nothing more and loops no further.
	if out, err := r.erase(spec); err != nil || out.Preparations != 0 || out.Pending != 2 {
		t.Errorf("second pass: %+v %v", out, err)
	}
}

// The receipts of an erased chunk are found by an index on the application, tenant and
// preparation, not by walking every receipt of the tenant once per chunk (the scope index has the
// workspace, which the erasure does not name, before the preparation).
func TestEraseReceiptsPlanUsesThePreparationIndex(t *testing.T) {
	r := newRetentionRig(t)
	for i := range 30 {
		r.prep(fmt.Sprintf("prep_%02d", i), "tenant-a", "usr_alice", "committed", t0, true)
	}
	rows, err := r.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+eraseReceiptsSQL("?, ?"), "demo-app", "tenant-a", "prep_01", "prep_02")
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
	if !strings.Contains(joined, "action_receipts_erase (application_id=? AND tenant_id=? AND preparation_id=?)") {
		t.Errorf("the receipts of a chunk are not found by preparation:\n%s", joined)
	}
}

func TestEraseFinishedValidationAndAtomicity(t *testing.T) {
	r := newRetentionRig(t)
	r.prep("prep_a", "tenant-a", "usr_alice", "committed", t0, true)

	for name, mutate := range map[string]func(*EraseSpec){
		"no tenant":      func(s *EraseSpec) { s.TenantID = "" },
		"no application": func(s *EraseSpec) { s.ApplicationID = "" },
		"unknown kind":   func(s *EraseSpec) { s.Kind = "whim" },
		"no reason":      func(s *EraseSpec) { s.Reason = "" },
		"no operator":    func(s *EraseSpec) { s.Operator = "" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := eraseSpec("tenant-a")
			mutate(&spec)
			if _, err := r.erase(spec); err == nil {
				t.Error("accepted")
			}
			if n := r.count(`SELECT COUNT(*) FROM action_preparations`); n != 1 {
				t.Errorf("%d preparations after a refused request", n)
			}
		})
	}

	t.Run("an audit append that fails removes nothing", func(t *testing.T) {
		err := r.db.Tx(context.Background(), func(tx *store.Tx) error {
			_, err := EraseFinished(context.Background(), tx, failingAppender{}, eraseSpec("tenant-a"))
			return err
		})
		if err == nil {
			t.Fatal("erased with no audit record")
		}
		if n := r.count(`SELECT COUNT(*) FROM action_preparations`) + r.count(`SELECT COUNT(*) FROM action_receipts`); n != 2 {
			t.Errorf("%d rows left, want 2: the erasure was not rolled back", n)
		}
	})
}

type failingAppender struct{}

func (failingAppender) AppendAt(context.Context, audit.Locator, audit.NewEvent) (audit.Event, error) {
	return audit.Event{}, contracts.New(contracts.ErrUnavailable, "the audit log is down")
}

func (failingAppender) AppendTx(context.Context, *store.Tx, audit.Locator, audit.NewEvent) (audit.Event, error) {
	return audit.Event{}, contracts.New(contracts.ErrUnavailable, "the audit log is down")
}

// The guard keeps the payload of an event that names a pending action of the
// same tenant, and of nothing else.
func TestPendingGuard(t *testing.T) {
	r := newRetentionRig(t)
	for i, st := range append(append([]string{}, pendingStates...), finishedStates...) {
		r.prep(fmt.Sprintf("prep_%d", i), "tenant-a", "usr_alice", st, t0, false)
	}
	r.prep("prep_other_tenant", "tenant-b", "usr_alice", "uncertain", t0, false)

	keep, err := PendingGuard(context.Background(), r.db, audit.ChainKey{ApplicationID: "demo-app", TenantID: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	ev := func(data string) audit.Event { return audit.Event{Data: json.RawMessage(data)} }
	for i, st := range append(append([]string{}, pendingStates...), finishedStates...) {
		got := keep(ev(fmt.Sprintf(`{"preparation_id":"prep_%d"}`, i)))
		if want := i < len(pendingStates); (got != "") != want {
			t.Errorf("an event of a %s preparation: keep reason %q, want kept %t", st, got, want)
		}
	}
	for name, data := range map[string]string{
		"an unknown preparation":          `{"preparation_id":"prep_nope"}`,
		"another tenant's pending action": `{"preparation_id":"prep_other_tenant"}`,
		"no preparation":                  `{"n":1}`,
		"the erased marker":               audit.ErasedMarker,
		"something that is not an object": `[1,2]`,
		"a preparation id in another key": `{"note":"prep_0"}`,
		"an empty preparation id":         `{"preparation_id":""}`,
	} {
		if got := keep(ev(data)); got != "" {
			t.Errorf("%s was kept: %q", name, got)
		}
	}

	// An application chain names no preparation.
	appKeep, err := PendingGuard(context.Background(), r.db, audit.ChainKey{ApplicationID: "demo-app"})
	if err != nil || appKeep(ev(`{"preparation_id":"prep_0"}`)) != "" {
		t.Errorf("application chain: %v", err)
	}
}
