package ledger_test

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/adapters/native"
	"github.com/buxee/alix-ai/examples/reference-apps/ledger"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// TestLedgerAgreesWithItsPackage: the signed package and the application say the same
// thing. Every native method a capability maps is declared by the application and the
// application declares no other; the events the application emits are the events the
// package accepts; the provisioning operations are the ones the adapter configuration
// approves; and the policy's role grants are what the application enforces.
func TestLedgerAgreesWithItsPackage(t *testing.T) {
	pkg := loadPackage(t)

	t.Run("the methods", func(t *testing.T) {
		mapped := map[string]bool{}
		for _, id := range sortedKeys(pkg.Capabilities) {
			c := pkg.Capabilities[id]
			for _, b := range []*ap.CapabilityOperationBinding{c.Operations.Read, c.Operations.Validate, c.Operations.Prepare, c.Operations.Commit, c.Operations.Status} {
				if b != nil {
					mapped[string(*b.NativeMethod)] = true
				}
			}
			for _, et := range c.EntityTypes {
				if et.Resolve != nil {
					mapped[string(*et.Resolve.NativeMethod)] = true
				}
			}
		}
		declared := ledger.Methods()
		sort.Strings(declared)
		got := sortedKeys(mapped)
		if !slices.Equal(got, declared) {
			t.Fatalf("the package maps %v, the application declares %v", got, declared)
		}
	})

	t.Run("the events", func(t *testing.T) {
		var ev ap.ManifestEventsEndpoint
		for _, e := range pkg.Manifest.Endpoints {
			if v, ok := e.Value.(ap.ManifestEventsEndpoint); ok {
				ev = v
			}
		}
		if ev.KeyID == nil || string(*ev.KeyID) != eventKeyID {
			t.Fatalf("the events endpoint names the key %v", ev.KeyID)
		}
		for _, typ := range []string{ledger.EventInvoicePaid, ledger.EventInvoicePaymentRecorded, ledger.EventInvoiceVoided, ledger.EventContractUpdated} {
			et, ok := ev.EventTypes[typ]
			if !ok {
				t.Fatalf("the package does not accept the event %s", typ)
			}
			if et.Effect == "" || string(et.Effect) == "execute" {
				t.Fatalf("event %s has the effect %q", typ, et.Effect)
			}
		}
		if len(ev.EventTypes) != 4 {
			t.Fatalf("the package accepts events the application never emits: %v", ev.EventTypes)
		}
		src := pkg.Sources["ledger-contracts"]
		if src.Sync.Event == nil || string(src.Sync.Event.EventType) != ledger.EventContractUpdated {
			t.Fatalf("the contracts source is not synchronised by the contract.updated event: %+v", src.Sync)
		}
	})

	t.Run("the provisioning", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join(packageDir(), "schemas", "adapters", "ledger-bridge.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := native.ParseAdapterConfig(raw, ledger.EndpointID)
		if err != nil {
			t.Fatal(err)
		}
		fx := pkg.Tests[0].Fixtures.Provisioning
		// the three the suite names, and approve_step_up: the convention of the transport's step-up completer
		used := []string{string(*fx.CreateTenant.NativeMethod), string(*fx.Seed.NativeMethod), string(*fx.Cleanup.NativeMethod), "approve_step_up"}
		sort.Strings(used)
		if !slices.Equal(used, cfg.Management.Provisioning) {
			t.Fatalf("the suite uses %v, the adapter configuration approves %v", used, cfg.Management.Provisioning)
		}
	})

	t.Run("the roles", func(t *testing.T) {
		w := newWorld(t)
		fx := w.provision()
		grants := map[string][]string{}
		for _, g := range pkg.Policies[0].RoleGrants {
			grants[g.Role] = g.Capabilities
		}
		probe := map[string]func(role string) (denied bool){}
		for _, id := range sortedKeys(pkg.Capabilities) {
			id := id
			probe[id] = func(role string) bool {
				ref := map[string]string{"accountant": "acct-a-north", "manager": "mgr-a-north", "viewer": "view-a-north"}[role]
				var res actions.DeterministicResult
				switch id {
				case "invoices.find":
					res = w.read(w.as(ref), id, map[string]any{"query": "INV-0001"})
				case "quotes.draft":
					res = w.prepare(w.as(ref), id, map[string]any{"customer": fx.record["cust-a-north"], "description": "Support", "amount_minor": 100})
				case "invoices.issue":
					res = w.prepare(w.as(ref), id, map[string]any{"quote": fx.record["quote-a-north"]})
				case "payments.record":
					res = w.prepare(w.as(ref), id, map[string]any{"invoice": fx.record["inv-a-north"], "amount_minor": 100})
				case "invoices.void":
					res = w.prepare(w.as(ref), id, map[string]any{"invoice": fx.record["inv-a-north"], "reason": "duplicate"})
				}
				return res.Status == actions.DeterministicResultStatusRejected && len(res.Errors) == 1 && string(res.Errors[0].Code) == "permission_denied"
			}
		}
		for role := range pkg.Manifest.Tenancy.Roles {
			for id := range pkg.Capabilities {
				want := !slices.Contains(grants[role], id)
				if got := probe[id](role); got != want {
					t.Errorf("role %s on %s: the application denies=%v, the policy grants it=%v", role, id, got, !want)
				}
			}
		}
		if len(pkg.Manifest.Tenancy.Roles) != 3 {
			t.Fatalf("roles = %v", pkg.Manifest.Tenancy.Roles)
		}
	})
}

// A hostile or buggy result is not waved through by the schema check the other tests rely
// on: the result schemas refuse a float amount, an amount in a currency that is not
// supported, a missing fact and an unexpected member.
func TestLedgerResultSchemasAreNotVacuous(t *testing.T) {
	w := newWorld(t)
	good := map[string]any{
		"status": "ok", "identifiers": []any{map[string]any{"entity_type": "payment", "entity_id": "pay_1"}, map[string]any{"entity_type": "invoice", "entity_id": "inv_1"}},
		"values": []any{
			map[string]any{"field": "payment_reference", "value": map[string]any{"kind": "text", "value": "PAY-0001"}},
			map[string]any{"field": "invoice_number", "value": map[string]any{"kind": "text", "value": "INV-0001"}},
			map[string]any{"field": "payment_amount", "value": map[string]any{"kind": "money", "value": "10.00", "currency": "EUR"}},
			map[string]any{"field": "balance_remaining", "value": map[string]any{"kind": "money", "value": "5.00", "currency": "EUR"}},
			map[string]any{"field": "invoice_status", "value": map[string]any{"kind": "text", "value": "partially_paid"}},
		},
		"errors": []any{}, "allowed_next_actions": []any{},
		"freshness": map[string]any{"as_of": "2026-10-03T09:30:00Z", "version": "rv1-0123456789abcdef0123456789abcdef"},
	}
	schema := w.schemaFor("schemas/payments.record.receipt.result.schema.json")
	if err := schema.Validate(good); err != nil {
		t.Fatalf("the reference result is refused: %v", err)
	}
	mutate := func(f func(m map[string]any)) map[string]any {
		b, _ := jsonRoundTrip(good)
		f(b)
		return b
	}
	for name, bad := range map[string]map[string]any{
		"a float amount": mutate(func(m map[string]any) {
			m["values"].([]any)[2].(map[string]any)["value"].(map[string]any)["value"] = 10.0
		}),
		"an unsupported currency": mutate(func(m map[string]any) {
			m["values"].([]any)[2].(map[string]any)["value"].(map[string]any)["currency"] = "XXX"
		}),
		"a fraction the currency does not have": mutate(func(m map[string]any) {
			m["values"].([]any)[2].(map[string]any)["value"].(map[string]any)["value"] = "10.0.0"
		}),
		"a missing fact":       mutate(func(m map[string]any) { m["values"] = m["values"].([]any)[:4] }),
		"an unexpected member": mutate(func(m map[string]any) { m["debug"] = "x" }),
		"no freshness":         mutate(func(m map[string]any) { delete(m, "freshness") }),
	} {
		if err := schema.Validate(bad); err == nil {
			t.Errorf("the schema accepted %s", name)
		}
	}
}

// Concurrent commits of one prepared action with one key apply it exactly once: the
// invoice is reduced by the amount one time, and every caller receives the same receipt.
func TestLedgerConcurrentCommitsApplyOnce(t *testing.T) {
	w := newWorld(t)
	fx := w.provision()
	sc := w.as("acct-a-north")
	inv := fx.record["inv-a-north"]
	p := w.preparedFrom("payments.record", w.prepare(sc, "payments.record", map[string]any{"invoice": inv, "amount_minor": 12345}))

	const n = 12
	var wg sync.WaitGroup
	results := make([]actions.DeterministicResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = w.commit(sc, "payments.record", p)
		}()
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("commit %d: %v", i, errs[i])
		}
		isOK(t, results[i])
		if string(mustJSON(t, results[i].Values)) != string(mustJSON(t, results[0].Values)) {
			t.Fatalf("commit %d answered differently from commit 0", i)
		}
	}
	got := w.read(w.as("view-a-north"), "invoices.find", map[string]any{"query": "INV-0001"})
	if money(t, got, "invoice_1_balance") != "1127.05 EUR" { // 1250.50 - 123.45, once
		t.Fatalf("balance = %s", money(t, got, "invoice_1_balance"))
	}
	if kinds := eventTypes(w); len(kinds) != 1 {
		t.Fatalf("one payment emitted %d events", len(kinds))
	}
}
