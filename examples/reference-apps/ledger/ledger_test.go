package ledger_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/buxee/alix-ai/examples/reference-apps/ledger"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/pkg/bridge"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// TestLedgerServesBridge drives the ledger through the real native adapter, configured
// from the shipped package, over loopback to the host bridge of pkg/bridge: discovery,
// provisioning, reads, the whole prepare, confirm and commit protocol for the four
// consequential capabilities, tenant and team isolation, role checks, exact money and
// the level 3 step-up. Every result is validated against the package's result schemas.
//
// The subtests share one disposable world and run in order.
func TestLedgerServesBridge(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	fx := w.provision()
	acctN, mgrN, viewN, acctS, acctB, mgrB := w.as("acct-a-north"), w.as("mgr-a-north"), w.as("view-a-north"), w.as("acct-a-south"), w.as("acct-b"), w.as("mgr-b")
	invN, invS := fx.record["inv-a-north"], fx.record["inv-a-south"]
	custN := fx.record["cust-a-north"]

	t.Run("discovery shows exactly what the package maps", func(t *testing.T) {
		d, err := w.ad.Describe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if d.Transport != "native" || d.ApplicationVersion != ledger.Version || len(d.Unmapped) != 0 {
			t.Fatalf("description = %+v", d)
		}
		if len(d.Capabilities) != len(w.pkg.Capabilities) {
			t.Fatalf("capabilities = %+v", d.Capabilities)
		}
		h, err := w.ad.Health(ctx)
		if err != nil || h.Status != adapters.HealthOK {
			t.Fatalf("health = %+v, %v", h, err)
		}
		ins, err := w.ad.Inspect(ctx, acctN, adapters.InspectRequest{})
		if err != nil || len(ins.Reports) != 9 || len(ins.Unverified()) != 0 {
			t.Fatalf("inspection = %+v, %v", ins, err)
		}
	})

	t.Run("a disposable tenant is created, refused twice, seeded and removed", func(t *testing.T) {
		if _, err := w.raw.Manage(ctx, ledger.EndpointID, "drop_everything", nil); err == nil {
			t.Fatal("an operation the adapter configuration does not approve was served")
		}
		args := map[string]any{"tenant_id": "tnt_scratch", "workspaces": []any{map[string]any{"workspace_id": "ws_scratch"}}}
		if got := w.manage("create_tenant", args); got["status"] != "ok" {
			t.Fatalf("create_tenant = %v", got)
		}
		if got := w.manage("create_tenant", args); got["status"] != "rejected" {
			t.Fatalf("a tenant that exists was created again: %v", got)
		}
		gone := w.manage("cleanup", map[string]any{"tenant_id": "tnt_scratch"})
		if gone["status"] != "ok" || gone["existed"] != true {
			t.Fatalf("cleanup = %v", gone)
		}
		again := w.manage("cleanup", map[string]any{"tenant_id": "tnt_scratch"})
		if again["status"] != "ok" || again["existed"] != false {
			t.Fatalf("a second cleanup = %v", again)
		}
		for k, v := range again["remaining"].(map[string]any) {
			if v != float64(0) {
				t.Fatalf("something remains of the removed tenant: %s=%v", k, v)
			}
		}
	})

	t.Run("find reads exact amounts in every currency", func(t *testing.T) {
		res := w.read(viewN, "invoices.find", map[string]any{"query": "INV-0001"})
		isOK(t, res)
		if integer(t, res, "invoice_count") != 1 || text(t, res, "invoice_1_number") != "INV-0001" || text(t, res, "invoice_1_customer") != "Acme Srl" ||
			money(t, res, "invoice_1_total") != "1250.50 EUR" || money(t, res, "invoice_1_balance") != "1250.50 EUR" || text(t, res, "invoice_1_status") != "issued" {
			t.Fatalf("find = %+v", res.Values)
		}
		// A manager reaches the other team of the organisation: yen have no minor digits.
		res = w.read(mgrN, "invoices.find", map[string]any{"query": "Tokyo"})
		if money(t, res, "invoice_1_total") != "480000 JPY" {
			t.Fatalf("a yen amount = %s", money(t, res, "invoice_1_total"))
		}
		// An accountant does not.
		res = w.read(acctN, "invoices.find", map[string]any{"query": "Tokyo"})
		if integer(t, res, "invoice_count") != 0 || text(t, res, "invoice_summary") != "no matching invoices" {
			t.Fatalf("an accountant found another team's invoice: %+v", res.Values)
		}
		// Kuwaiti dinars have three.
		res = w.read(acctB, "invoices.find", map[string]any{"query": "Gulf"})
		if money(t, res, "invoice_1_total") != "1234.567 KWD" {
			t.Fatalf("a dinar amount = %s", money(t, res, "invoice_1_total"))
		}
		if len(res.AllowedNextActions) == 0 {
			t.Fatal("an accountant is offered no follow-up")
		}
		for _, bad := range []map[string]any{{"query": " a "}, {"query": "Acme", "status": "frozen"}, {"query": "Acme", "max_results": 11}, {"query": "Acme", "limit": 3}} {
			rejectedWith(t, w.read(viewN, "invoices.find", bad), "validation_failed")
		}
		rejectedWith(t, w.read(viewN, "invoices.find", map[string]any{}), "missing_fields")
	})

	t.Run("entity resolvers answer for the caller's reach only", func(t *testing.T) {
		res := w.read(acctN, "payments.record", map[string]any{"entity_type": "invoice", "mention": "inv 1"})
		if integer(t, res, "candidate_count") != 1 || len(res.Identifiers) != 1 || string(res.Identifiers[0].EntityID) != invN {
			t.Fatalf("resolve = %+v", res)
		}
		res = w.read(acctN, "payments.record", map[string]any{"entity_type": "invoice", "mention": "Tokyo"})
		if integer(t, res, "candidate_count") != 0 {
			t.Fatalf("an accountant resolved another team's invoice: %+v", res)
		}
		res = w.read(mgrN, "invoices.void", map[string]any{"entity_type": "invoice", "mention": "Tokyo"})
		if integer(t, res, "candidate_count") != 1 {
			t.Fatalf("a manager could not resolve an invoice of the organisation: %+v", res)
		}
		rejectedWith(t, w.read(acctN, "payments.record", map[string]any{"entity_type": "invoice", "mention": invS}), "scope_violation")
		rejectedWith(t, w.read(viewN, "quotes.draft", map[string]any{"entity_type": "customer", "mention": "Acme"}), "permission_denied")
	})

	t.Run("tenant, team and role boundaries refuse without disclosing", func(t *testing.T) {
		rejectedWith(t, w.read(viewN, "invoices.find", map[string]any{"query": invS}), "scope_violation")
		rejectedWith(t, w.read(acctB, "invoices.find", map[string]any{"query": invN}), "scope_violation")
		rejectedWith(t, w.read(mgrB, "invoices.find", map[string]any{"query": invS}), "scope_violation")
		rejectedWith(t, w.read(viewN, "invoices.find", map[string]any{"query": "inv_9999"}), "scope_violation")
		rejectedWith(t, w.prepare(viewN, "quotes.draft", map[string]any{"customer": custN, "description": "Support", "amount_minor": 1000}), "permission_denied")
		rejectedWith(t, w.prepare(acctB, "quotes.draft", map[string]any{"customer": custN, "description": "Support", "amount_minor": 1000}), "scope_violation")
		rejectedWith(t, w.prepare(acctN, "quotes.draft", map[string]any{"customer": fx.record["cust-a-south"], "description": "Support", "amount_minor": 1000}), "scope_violation")
		rejectedWith(t, w.prepare(acctN, "payments.record", map[string]any{"invoice": invS, "amount_minor": 100}), "scope_violation")
		rejectedWith(t, w.prepare(mgrN, "payments.record", map[string]any{"invoice": invS, "amount_minor": 100}), "scope_violation") // workspace level, even for a manager
		rejectedWith(t, w.prepare(acctN, "invoices.void", map[string]any{"invoice": invN, "reason": "duplicate"}), "permission_denied")
		rejectedWith(t, w.prepare(mgrB, "invoices.void", map[string]any{"invoice": invN, "reason": "duplicate"}), "scope_violation")

		// The directory decides, not the asserted identity.
		liar := w.scope(fx.tenantOf["view-a-north"], fx.teamOf["view-a-north"], fx.principal["view-a-north"], "manager")
		rejectedWith(t, w.read(liar, "invoices.find", map[string]any{"query": "INV"}), "permission_denied")
		ghost := w.scope(fx.tenantOf["view-a-north"], fx.teamOf["view-a-north"], "usr_ghost", "viewer")
		rejectedWith(t, w.read(ghost, "invoices.find", map[string]any{"query": "INV"}), "permission_denied")
		wrongTeam := w.scope(fx.tenantOf["view-a-north"], fx.team["south-a"], fx.principal["view-a-north"], "viewer")
		rejectedWith(t, w.read(wrongTeam, "invoices.find", map[string]any{"query": "INV"}), "scope_violation")
		rejectedWith(t, w.read(w.scope("tnt_nobody", "ws_x", "usr_x", "viewer"), "invoices.find", map[string]any{"query": "INV"}), "scope_violation")
	})

	var quoteID, invoiceID string
	t.Run("a quote is drafted at level 1 and a repeat changes nothing", func(t *testing.T) {
		fields := map[string]any{"customer": custN, "description": "Annual support", "amount_minor": 99950}
		v := w.validate(acctN, "quotes.draft", fields)
		prep := w.prepare(acctN, "quotes.draft", fields)
		isOK(t, v)
		if v.Freshness.Version != prep.Freshness.Version {
			t.Fatalf("validate reports %s, prepare %s: on unchanged data they must agree", v.Freshness.Version, prep.Freshness.Version)
		}
		if money(t, prep, "quote_total") != "999.50 EUR" || prep.Preparation.DeclaredRiskLevel != 1 {
			t.Fatalf("prepare = %+v", prep)
		}
		p := w.preparedFrom("quotes.draft", prep)
		receipt := w.committed(acctN, "quotes.draft", p)
		isOK(t, receipt)
		quoteID = string(receipt.Identifiers[0].EntityID)
		if money(t, receipt, "quote_total") != "999.50 EUR" || text(t, receipt, "customer_name") != "Acme Srl" {
			t.Fatalf("receipt = %+v", receipt.Values)
		}
		again := w.committed(acctN, "quotes.draft", p)
		a, _ := json.Marshal(receipt)
		b, _ := json.Marshal(again)
		if string(a) != string(b) {
			t.Fatalf("a repeated commit answered differently:\n%s\n%s", a, b)
		}
		if got := w.status(acctN, "quotes.draft", p.IdempotencyKey); string(mustJSON(t, got.Values)) != string(mustJSON(t, receipt.Values)) || got.Freshness.Version != receipt.Freshness.Version {
			t.Fatalf("status = %+v", got)
		}
		// The same key for another action is a conflict, never a second effect.
		other := w.preparedFrom("quotes.draft", w.prepare(acctN, "quotes.draft", map[string]any{"customer": custN, "description": "Another", "amount_minor": 5}))
		other.IdempotencyKey = p.IdempotencyKey
		rejectedWith(t, w.committed(acctN, "quotes.draft", other), "conflict")
		// Another principal cannot read the key's outcome.
		rejectedWith(t, w.status(w.as("view-a-north"), "quotes.draft", p.IdempotencyKey), "permission_denied")
		// A key nobody committed with is the one answer that says nothing was applied.
		none, err := w.ad.Status(ctx, acctN, adapters.StatusRequest{CapabilityID: "quotes.draft", IdempotencyKey: actions.IdempotencyKey("idem_never_used_0000000000")})
		if err != nil || !adapters.StatusShowsNoRecord(none) {
			t.Fatalf("status of an unknown key = %+v, %v", none, err)
		}
		// A currency that is not the customer's, and an amount that is not whole minor units.
		rejectedWith(t, w.prepare(acctN, "quotes.draft", map[string]any{"customer": custN, "description": "Support", "amount_minor": 100, "currency": "USD"}), "validation_failed")
		for _, bad := range []any{12.5, json.Number("1e3"), "100", 0, -5, int64(1) << 60} {
			rejectedWith(t, w.prepare(acctN, "quotes.draft", map[string]any{"customer": custN, "description": "Support", "amount_minor": bad}), "validation_failed")
		}
	})

	t.Run("an invoice is issued at level 2 from the draft, once", func(t *testing.T) {
		prep := w.prepare(acctN, "invoices.issue", map[string]any{"quote": quoteID, "due_days": 14})
		isOK(t, prep)
		if prep.Preparation.DeclaredRiskLevel != 2 || money(t, prep, "invoice_total") != "999.50 EUR" || text(t, prep, "invoice_due_on") != "2026-10-17" {
			t.Fatalf("prepare = %+v", prep)
		}
		external := false
		for _, e := range prep.Preparation.Effects {
			external = external || e.External
		}
		if !external {
			t.Fatal("sending the invoice is not declared as an external effect")
		}
		receipt := w.committed(acctN, "invoices.issue", w.preparedFrom("invoices.issue", prep))
		isOK(t, receipt)
		invoiceID = string(receipt.Identifiers[0].EntityID)
		if money(t, receipt, "invoice_total") != "999.50 EUR" {
			t.Fatalf("receipt = %+v", receipt.Values)
		}
		rejectedWith(t, w.prepare(acctN, "invoices.issue", map[string]any{"quote": quoteID}), "conflict")
	})

	t.Run("payments are exact, never exceed the balance and move the version", func(t *testing.T) {
		pay := func(amount any, extra map[string]any) actions.DeterministicResult {
			f := map[string]any{"invoice": invoiceID, "amount_minor": amount}
			for k, v := range extra {
				f[k] = v
			}
			return w.prepare(acctN, "payments.record", f)
		}
		rejectedWith(t, pay(99951, nil), "validation_failed")                                   // one minor unit too much
		rejectedWith(t, pay(100, map[string]any{"currency": "USD"}), "validation_failed")       // not the invoice's currency
		rejectedWith(t, pay(100, map[string]any{"paid_on": "2026-10-04"}), "validation_failed") // in the future
		rejectedWith(t, pay(100, map[string]any{"paid_on": "2026-09-30"}), "validation_failed") // before the invoice
		rejectedWith(t, pay(100, map[string]any{"method": "barter"}), "validation_failed")
		rejectedWith(t, pay(12.5, nil), "validation_failed")

		first := w.prepare(acctN, "payments.record", map[string]any{"invoice": invoiceID, "amount_minor": 50000, "method": "card"})
		isOK(t, first)
		if money(t, first, "payment_amount") != "500.00 EUR" || money(t, first, "balance_after") != "499.50 EUR" || text(t, first, "invoice_status_after") != "partially_paid" {
			t.Fatalf("prepare = %+v", first.Values)
		}
		before := w.validate(acctN, "payments.record", map[string]any{"invoice": invoiceID, "amount_minor": 50000, "method": "card"})
		if before.Freshness.Version != first.Freshness.Version {
			t.Fatal("validate and prepare disagree on unchanged data")
		}
		receipt := w.committed(acctN, "payments.record", w.preparedFrom("payments.record", first))
		if money(t, receipt, "balance_remaining") != "499.50 EUR" || text(t, receipt, "invoice_status") != "partially_paid" {
			t.Fatalf("receipt = %+v", receipt.Values)
		}
		// The balance moved the invoice's version: a preparation made before is stale.
		if after := w.validate(acctN, "payments.record", map[string]any{"invoice": invoiceID, "amount_minor": 100}); after.Freshness.Version == first.Freshness.Version {
			t.Fatal("the version did not move after a payment")
		}
		rest := w.committed(acctN, "payments.record", w.preparedFrom("payments.record", pay(49950, nil)))
		if money(t, rest, "balance_remaining") != "0.00 EUR" || text(t, rest, "invoice_status") != "paid" {
			t.Fatalf("receipt = %+v", rest.Values)
		}
		rejectedWith(t, pay(1, nil), "conflict") // a paid invoice takes no payment
		kinds := eventTypes(w)
		if fmt.Sprint(kinds) != "[invoice.payment_recorded invoice.paid]" {
			t.Fatalf("events = %v", kinds)
		}
	})

	t.Run("a record that moved or a preparation that ran out is never committed", func(t *testing.T) {
		stale := w.preparedFrom("payments.record", w.prepare(acctN, "payments.record", map[string]any{"invoice": invN, "amount_minor": 10000}))
		other := w.preparedFrom("payments.record", w.prepare(acctN, "payments.record", map[string]any{"invoice": invN, "amount_minor": 5000}))
		isOK(t, w.committed(acctN, "payments.record", other))
		res, err := w.commit(acctN, "payments.record", stale)
		rejected := w.must(res, err, "payments.record", adapters.OpCommit, "")
		rejectedWith(t, rejected, "stale")
		if rejected.Freshness == nil {
			t.Fatal("a stale answer carries no snapshot version to tell data moved from a rule failing")
		}
		if got := w.read(viewN, "invoices.find", map[string]any{"query": "INV-0001"}); money(t, got, "invoice_1_balance") != "1200.50 EUR" {
			t.Fatalf("the stale commit changed the balance: %s", money(t, got, "invoice_1_balance"))
		}
		if none, err := w.ad.Status(ctx, acctN, adapters.StatusRequest{CapabilityID: "payments.record", IdempotencyKey: stale.IdempotencyKey}); err != nil || !adapters.StatusShowsNoRecord(none) {
			t.Fatalf("a refused commit left a record: %+v, %v", none, err)
		}

		late := w.preparedFrom("payments.record", w.prepare(acctN, "payments.record", map[string]any{"invoice": invN, "amount_minor": 100}))
		w.clk.Advance(11 * time.Minute)
		rejectedWith(t, w.committed(acctN, "payments.record", late), "expired")
		// The same commit from a principal who may not commit it is refused too.
		fresh := w.preparedFrom("payments.record", w.prepare(acctN, "payments.record", map[string]any{"invoice": invN, "amount_minor": 100}))
		rejectedWith(t, w.committed(viewN, "payments.record", fresh), "permission_denied")
	})

	t.Run("a void is level 3: the commit needs the application's own approval", func(t *testing.T) {
		prep := w.prepare(mgrN, "invoices.void", map[string]any{"invoice": invS, "reason": "duplicate"})
		isOK(t, prep)
		ref := text(t, prep, "void_reference")
		if prep.Preparation.DeclaredRiskLevel != 3 || ref != "VOID-INV-0002-1" || money(t, prep, "invoice_total") != "480000 JPY" {
			t.Fatalf("prepare = %+v", prep.Values)
		}
		p := w.preparedFrom("invoices.void", prep)
		rejectedWith(t, w.committed(mgrN, "invoices.void", p), "step_up_required")
		if got := w.read(mgrN, "invoices.find", map[string]any{"query": invS}); text(t, got, "invoice_1_status") != "issued" {
			t.Fatal("a void without step-up changed the invoice")
		}
		identity := func(ref string) bridge.Identity {
			sc := w.as(ref)
			return bridge.Identity{ApplicationID: sc.ApplicationID, TenantID: sc.TenantID, WorkspaceID: sc.WorkspaceID, PrincipalID: sc.PrincipalID, Roles: sc.Roles, SessionID: sc.SessionID}
		}
		if err := w.app.ApproveVoid(identity("acct-a-north"), ref); err == nil {
			t.Fatal("an accountant approved a void")
		}
		if err := w.app.ApproveVoid(identity("mgr-b"), ref); err == nil {
			t.Fatal("a manager of another organisation approved a void")
		}
		if err := w.app.ApproveVoid(identity("mgr-a-north"), "VOID-INV-0002-9"); err == nil {
			t.Fatal("an approval for a version that was not prepared was recorded")
		}
		rejectedWith(t, w.committed(mgrN, "invoices.void", p), "step_up_required")
		if err := w.app.ApproveVoid(identity("mgr-a-north"), ref); err != nil {
			t.Fatal(err)
		}
		receipt := w.committed(mgrN, "invoices.void", p)
		isOK(t, receipt)
		if text(t, receipt, "invoice_status") != "void" || text(t, receipt, "void_reference") != ref {
			t.Fatalf("receipt = %+v", receipt.Values)
		}
		if got := w.read(mgrN, "invoices.find", map[string]any{"query": invS}); text(t, got, "invoice_1_status") != "void" {
			t.Fatal("the invoice is not void")
		}
		rejectedWith(t, w.prepare(mgrN, "invoices.void", map[string]any{"invoice": invS, "reason": "duplicate"}), "conflict")        // already void
		rejectedWith(t, w.prepare(mgrN, "invoices.void", map[string]any{"invoice": invN, "reason": "duplicate"}), "conflict")        // has payments
		rejectedWith(t, w.prepare(mgrN, "invoices.void", map[string]any{"invoice": invN, "reason": "because"}), "validation_failed") // not a reason
		rejectedWith(t, w.prepare(acctS, "payments.record", map[string]any{"invoice": invS, "amount_minor": 100}), "conflict")       // a void invoice takes no payment
		if kinds := eventTypes(w); kinds[len(kinds)-1] != ledger.EventInvoiceVoided {
			t.Fatalf("events = %v", kinds)
		}
	})

	t.Run("cleanup removes everything the run created and can be repeated", func(t *testing.T) {
		for _, tid := range fx.tenant {
			got := w.manage("cleanup", map[string]any{"tenant_id": tid})
			if got["existed"] != true {
				t.Fatalf("cleanup(%s) = %v", tid, got)
			}
			for k, v := range got["remaining"].(map[string]any) {
				if v != float64(0) {
					t.Fatalf("cleanup(%s) left %s=%v", tid, k, v)
				}
			}
			if again := w.manage("cleanup", map[string]any{"tenant_id": tid}); again["existed"] != false {
				t.Fatalf("second cleanup = %v", again)
			}
		}
		rejectedWith(t, w.read(viewN, "invoices.find", map[string]any{"query": "INV-0001"}), "scope_violation")
		if got := w.manage("seed", map[string]any{"tenant_id": fx.tenant["org-a"], "records": []any{map[string]any{"ref": "x", "entity_type": "customer", "workspace_id": "w"}}}); got["status"] != "rejected" {
			t.Fatalf("a removed tenant was seeded: %v", got)
		}
	})
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// eventTypes lists the event types waiting in the ledger's outbox, oldest first.
func eventTypes(w *world) []string {
	var out []string
	for _, e := range w.app.Outbox() {
		out = append(out, e.Type)
	}
	return out
}

// approve_step_up is the audit's way to complete the level 3 void: a management operation, served
// only in test support, that records exactly the approval the application's own interface
// records (ApproveVoid) and applies every rule that interface applies.
func TestLedgerApproveStepUpManagementOperation(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	fx := w.provision()
	mgrN, mgrB := w.as("mgr-a-north"), w.as("mgr-b")
	invS := fx.record["inv-a-south"]
	prep := w.prepare(mgrN, "invoices.void", map[string]any{"invoice": invS, "reason": "duplicate"})
	isOK(t, prep)
	ref := text(t, prep, "void_reference")
	p := w.preparedFrom("invoices.void", prep)
	rejectedWith(t, w.committed(mgrN, "invoices.void", p), "step_up_required")

	args := func(principal string, values map[string]any) map[string]any {
		return map[string]any{
			"tenant_id": fx.tenantOf[principal], "workspace_id": fx.teamOf[principal], "principal_id": fx.principal[principal],
			"capability_id": "invoices.void", "values": values,
		}
	}
	refusedWith := func(name string, got map[string]any, code string) {
		t.Helper()
		errs, _ := got["errors"].([]any)
		if got["status"] != "rejected" || len(errs) != 1 || errs[0].(map[string]any)["code"] != code {
			t.Errorf("%s: %v, want rejected with %s", name, got, code)
		}
		rejectedWith(t, w.committed(mgrN, "invoices.void", p), "step_up_required") // nothing was approved
	}
	facts := map[string]any{"void_reference": ref, "invoice_total": "480000 JPY"}

	refusedWith("an accountant", w.manage("approve_step_up", args("acct-a-north", facts)), "permission_denied")
	refusedWith("another organisation's manager", w.manage("approve_step_up", map[string]any{
		"tenant_id": fx.tenantOf["mgr-a-north"], "workspace_id": fx.teamOf["mgr-b"], "principal_id": fx.principal["mgr-b"],
		"capability_id": "invoices.void", "values": facts,
	}), "scope_violation")
	refusedWith("a reference nobody prepared", w.manage("approve_step_up", args("mgr-a-north", map[string]any{"void_reference": "VOID-INV-0002-9"})), "scope_violation")
	refusedWith("no reference", w.manage("approve_step_up", args("mgr-a-north", map[string]any{})), "validation_failed")
	refusedWith("another capability", w.manage("approve_step_up", func() map[string]any {
		a := args("mgr-a-north", facts)
		a["capability_id"] = "payments.record"
		return a
	}()), "validation_failed")
	refusedWith("a tenant that does not exist", w.manage("approve_step_up", func() map[string]any {
		a := args("mgr-a-north", facts)
		a["tenant_id"] = "tnt_nobody"
		return a
	}()), "scope_violation")
	_ = mgrB

	done := w.manage("approve_step_up", args("mgr-a-north", facts))
	if done["status"] != "ok" || done["approved"] != true || done["tenant_id"] != fx.tenantOf["mgr-a-north"] {
		t.Fatalf("approve_step_up = %v", done)
	}
	receipt := w.committed(mgrN, "invoices.void", p)
	isOK(t, receipt)
	if text(t, receipt, "invoice_status") != "void" || text(t, receipt, "void_reference") != ref {
		t.Fatalf("receipt = %+v", receipt.Values)
	}
	// the approval is the one the interface records, so it applies once: the void is done, and another approval has nothing to approve
	refusedAgain := w.manage("approve_step_up", args("mgr-a-north", facts))
	if refusedAgain["status"] != "rejected" {
		t.Fatalf("a second approval of a void that is done = %v", refusedAgain)
	}

	// a ledger that does not run in test support has no such operation at all
	prod := newWorld(t, func(c *ledger.Config) { c.TestSupport = false })
	if _, err := prod.raw.Manage(ctx, ledger.EndpointID, "approve_step_up", map[string]any{"tenant_id": "tnt_x"}); err == nil {
		t.Fatal("a ledger outside test support served approve_step_up")
	}
}
