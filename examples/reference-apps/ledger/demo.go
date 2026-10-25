package ledger

import "fmt"

// Demo tenant IDs, for a local run and for the application's own tests.
const (
	DemoTenant    = "org_demo"
	DemoTeamNorth = "team_north"
	DemoTeamSouth = "team_south"
)

// SeedDemo creates a small synthetic organisation: two teams, one principal of each
// role, customers in three currencies, invoices in several states and a contract per
// team. Every name is made up. It is for a local run (LEDGER_DEMO_SEED=1); the
// disposable tenants of the audit are created through the management operations.
func (l *Ledger) SeedDemo() error {
	created := l.createTenant(map[string]any{
		"tenant_id": DemoTenant, "name": "Synthetic Holdings",
		"workspaces": []any{
			map[string]any{"workspace_id": DemoTeamNorth, "name": "North team"},
			map[string]any{"workspace_id": DemoTeamSouth, "name": "South team"},
		},
		"principals": []any{
			map[string]any{"principal_id": "usr_ada", "workspace_id": DemoTeamNorth, "roles": []any{"accountant"}},
			map[string]any{"principal_id": "usr_marta", "workspace_id": DemoTeamNorth, "roles": []any{"manager"}},
			map[string]any{"principal_id": "usr_vito", "workspace_id": DemoTeamNorth, "roles": []any{"viewer"}},
			map[string]any{"principal_id": "usr_sara", "workspace_id": DemoTeamSouth, "roles": []any{"accountant"}},
		},
	})
	if created["status"] != "ok" {
		return fmt.Errorf("ledger: the demo tenant was refused: %v", created["errors"])
	}
	record := func(ref, entity, team string, extra map[string]any) any {
		m := map[string]any{"ref": ref, "entity_type": entity, "workspace_id": team}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	seeded := l.seedTenant(map[string]any{"tenant_id": DemoTenant, "records": []any{
		record("acme", "customer", DemoTeamNorth, map[string]any{"name": "Acme Srl", "currency": "EUR"}),
		record("kuwait", "customer", DemoTeamNorth, map[string]any{"name": "Gulf Trading", "currency": "KWD"}),
		record("tokyo", "customer", DemoTeamSouth, map[string]any{"name": "Tokyo Parts", "currency": "JPY"}),
		record("quote-acme", "quote", DemoTeamNorth, map[string]any{"customer": "acme", "amount_minor": 125050}),
		record("inv-acme", "invoice", DemoTeamNorth, map[string]any{"customer": "acme", "amount_minor": 125050}),
		record("inv-gulf", "invoice", DemoTeamNorth, map[string]any{"customer": "kuwait", "amount_minor": 1234567, "paid_minor": 234567}),
		record("inv-tokyo", "invoice", DemoTeamSouth, map[string]any{"customer": "tokyo", "amount_minor": 480000}),
		record("contract-north", "contract", DemoTeamNorth, map[string]any{
			"key": "master-services", "title": "Master services agreement",
			"body": "# Master services agreement\n\nPayment is due 30 days after the invoice date. Late payments carry a fee of 1.5 percent per month.",
		}),
		record("contract-south", "contract", DemoTeamSouth, map[string]any{
			"key": "master-services", "title": "Master services agreement",
			"body": "# Master services agreement\n\nPayment is due 45 days after the invoice date. Invoices are issued in the currency of the customer.",
		}),
	}})
	if seeded["status"] != "ok" {
		return fmt.Errorf("ledger: the demo records were refused: %v", seeded["errors"])
	}
	l.mu.Lock()
	// The demo organisation is not disposable: the management operations refuse it.
	l.tenants[DemoTenant].disposable = false
	l.mu.Unlock()
	return nil
}
