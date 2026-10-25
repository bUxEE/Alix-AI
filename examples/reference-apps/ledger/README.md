# ledger

A synthetic multi-tenant accounting application: the third neutral AlixAI reference application. It is a **test fixture with real business logic**, not a product. Every name and amount is made up.

It is unlike the other two on purpose:

- **Go**, with the host bridge of `pkg/bridge` embedded: AlixAI reaches it through the **native adapter** over loopback IPC (not MCP, not OpenAPI).
- **Organisations and teams**: tenant = organisation, workspace = team. An accountant or a viewer reaches their own team, a manager every team of the organisation, nobody another organisation.
- **Money** is an integer count of minor units and an ISO 4217 currency (EUR, USD, GBP, CHF, JPY, KWD: 2, 2, 2, 2, 0 and 3 digits). A result states an amount as a canonical `money` fact whose decimal text is derived from the integer by integer arithmetic; a fraction, an exponent or a string is refused, never rounded.
- **Level 3**: voiding an invoice can only be finished in the application's own UI.
- **Signed events** (an invoice was paid, a payment was recorded, an invoice was voided, a contract changed) that invalidate AlixAI state and never commit anything.
- **Document API** for contracts, read by AlixAI's application document API source provider.

```bash
go test ./examples/reference-apps/ledger/...        # the application, its package and its events against the real core code
go run ./cmd/alixai package validate examples/reference-apps/ledger/package
LEDGER_KEY_REF=env:LEDGER_BRIDGE_KEY LEDGER_DEMO_SEED=1 go run ./examples/reference-apps/ledger/cmd/ledger   # see cmd/ledger/main.go for the settings
```

The package is signed with the TEST-ONLY key (`tools/devkeys`; a real trust store refuses it):

```bash
go run ./cmd/alixai package sign -app ledger -version 1.0.0 \
  -key alixai-test-only-publisher=tools/devkeys/test-only-publisher.key.pem \
  -signed-at 2026-10-02T00:00:00Z -expires-at 2036-10-01T00:00:00Z \
  examples/reference-apps/ledger/package
```

## Roles and capabilities

| Capability | Level | Roles | Native methods |
| --- | --- | --- | --- |
| `invoices.find` | 0 | accountant, manager, viewer | `invoices_find` |
| `quotes.draft` | 1, no separate confirmation | accountant, manager | `quotes_draft_{validate,prepare,commit,status}` |
| `invoices.issue` | 2, explicit | accountant, manager | `invoices_issue_{validate,prepare,commit,status}` |
| `payments.record` | 2, explicit | accountant, manager | `payments_record_{validate,prepare,commit,status}` |
| `invoices.void` | 3, step-up in the application UI | manager | `invoices_void_{validate,prepare,commit,status}` |

Entity resolvers: `resolve_customer`, `resolve_quote`, `resolve_invoice` (the caller's team) and `resolve_invoice_for_void` (the organisation). The host reauthorises every call from its own directory and never trusts the asserted roles.

Business rules: a quote is drafted in the customer's own currency; an invoice is issued once from a draft quote that is still valid; a payment is in the invoice's currency, dated from the invoice date to today and never exceeds the balance; only an issued invoice with no payments can be voided.

## Contracts

- **Native protocol**: the package comment of `adapters/native`. The application key (32+ random bytes) is shared with AlixAI out of band; the package's adapter configuration holds only the `env:` reference AlixAI reads it from.
- **Validate** reports the `freshness.version` Prepare reported (a digest of the revisions of the records the action relies on); **commit** re-checks it (`stale`), the expiry (`expired`) and every rule; it is idempotent per key; **status** answers "nothing recorded" in the one shape that makes a retry safe.
- **Step-up (level 3)**: the void's prepare shows a *void reference* (`VOID-INV-0002-1`: invoice number and version). Commit answers `step_up_required` until a signed-in manager has approved exactly that reference in the application (`Ledger.ApproveVoid`, which the bridge does not expose).
- **Events**: Ed25519 over the RFC 8785 canonical JSON of the event, one signature, at most 15 minutes. They are queued in an outbox in the same step as the change and delivered best effort, in order.
- **Provisioning and inspection** for the integration audit: `create_tenant`, `seed`, `cleanup` and `approve_step_up` (management operations, declared only with `LEDGER_TEST_SUPPORT=1`; the last records the approval `Ledger.ApproveVoid` records, for the audit's disposable tenants) and an inspection of all nine aspects. The package's `tests/` suite uses the first three; the audit's step-up completer uses the fourth.
- Events about a record go to the team of the record: a manager's session in another team relies on the commit-time version check.
