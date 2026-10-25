# Void an invoice

Capability `invoices.void`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

Void an issued invoice that has no payments, in any team of the caller's organisation. Voiding is legally sensitive and cannot be undone: AlixAI can only start it. The manager finishes it in the ledger's own authenticated application, with the void reference that the prepared action shows. Voiding sends a notice to the customer.

Scope: tenant (one organisation; a manager reaches every team, everyone else their own team). Roles that may use it: manager.

Operations: `validate` is the native method `invoices_void_validate`; `prepare` is the native method `invoices_void_prepare`; `commit` is the native method `invoices_void_commit`; `status` is the native method `invoices_void_status`.

A prepared action stays confirmable for 900 seconds. Commit is idempotent: the key is fixed when the action is prepared, a repeated commit returns the original receipt, and `status` reports what was recorded for a key so that a lost reply is never retried blindly. Validate, on unchanged data, reports the same data version as prepare, which is the snapshot of the records the action relies on (not a response tag and not an organisation-wide counter).

Every amount is an integer count of minor units with an ISO 4217 currency. The ledger turns it into the exact decimal text of a money fact by integer arithmetic, so no amount is ever rounded or invented.

## Fields

### Field: invoice

Type entity_ref of invoice, required. The invoice to void, named by its number, its identifier or the customer's name.

### Field: reason

Type enum, required, one of duplicate, wrong_amount, customer_cancelled, other. Why the invoice is voided.

## Validation rules

### Rule: invoice_voidable

The invoice must be issued with no payments: a paid, partially paid or already void invoice cannot be voided. A violation is reported as `conflict`.

## Permissions

### Permission: invoices.void

Allows voiding an issued invoice of any team of the caller's organisation. Granted to manager; checked by the host on every call. The host reauthorises every call from the identity it receives, from its own directory, and applies record-level visibility on top of the role, so holding the permission never reaches another organisation.

## Failure states

### Failure: permission_denied

The principal's role may not void an invoice.

The user is told: "You are not allowed to void invoices, so nothing was voided."

### Failure: scope_violation

The record is outside the principal's reach (another team without manager access, or another organisation), or does not exist. The application does not say which.

The user is told: "I cannot void that: the invoice is not available to you. Nothing was voided."

### Failure: validation_failed

A value is malformed or breaks one of the rules above. Nothing was changed.

The user is told: "I could not void that: a value is not valid. Nothing was voided."

### Failure: missing_fields

A required field (invoice, reason) was not supplied. Nothing was changed.

The user is told: "I still need to know which invoice to void and why. Nothing was voided."

### Failure: conflict

The invoice is already void or has payments. Nothing was changed.

The user is told: "That invoice cannot be voided: it is already void or it has payments. Nothing was voided."

### Failure: stale

A record changed after the action was prepared, so the preparation is no longer valid. Nothing was changed; it can be prepared again.

The user is told: "The invoice changed after I prepared this, so I did not void it. Nothing was voided; I can prepare it again."

### Failure: expired

The prepared action was not confirmed in time, or was withdrawn. Nothing was changed.

The user is told: "That was not confirmed in time, or it was withdrawn, so nothing was voided."

### Failure: uncertain_outcome

The commit response was lost. What was recorded is established through the status operation before anything is retried.

The user is told: "I could not confirm whether it went through. I will check before doing anything else, so please do not ask for it again yet."

### Failure: step_up_required

The action can only be completed in the ledger application's own authenticated UI, with the void reference. Nothing was changed.

The user is told: "This can only be completed in the ledger application, so nothing was voided."

### Failure: unavailable

The ledger cannot be reached. Nothing was changed.

The user is told: "The ledger is not reachable right now, so nothing was voided."

## Risk level

### Level 3

Level 3 (high-risk, privileged or legally sensitive): AlixAI can prepare the void but cannot complete it. A manager finishes it in the ledger application's own authenticated UI. Effect class privileged.

## Confirmation rule

### Confirmation: step_up

AlixAI shows the exact void and its void reference; the manager must complete it in the ledger application, signed in with the manager role. A prepared void is valid for 15 minutes and is invalidated early if the invoice changes, or if the permissions or the user change.

## User-visible outcomes

### Outcome: voided

The user is told: "Done. Invoice {invoice_number} is void, finished in the ledger application with reference {void_reference}."

Every value in braces is a fact returned by the application, not wording of the model.
