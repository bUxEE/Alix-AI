# Issue an invoice

Capability `invoices.issue`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

Issue an invoice from a draft quote of the caller's team. The invoice takes the quote's exact amount and currency, a number and a due date. The invoice is prepared first, its exact effects are shown, and it is issued only after the user confirms. Issuing marks the quote as invoiced and sends the invoice to the customer.

Scope: workspace (one team). Roles that may use it: accountant, manager.

Operations: `validate` is the native method `invoices_issue_validate`; `prepare` is the native method `invoices_issue_prepare`; `commit` is the native method `invoices_issue_commit`; `status` is the native method `invoices_issue_status`.

A prepared action stays confirmable for 600 seconds. Commit is idempotent: the key is fixed when the action is prepared, a repeated commit returns the original receipt, and `status` reports what was recorded for a key so that a lost reply is never retried blindly. Validate, on unchanged data, reports the same data version as prepare, which is the snapshot of the records the action relies on (not a response tag and not an organisation-wide counter).

Every amount is an integer count of minor units with an ISO 4217 currency. The ledger turns it into the exact decimal text of a money fact by integer arithmetic, so no amount is ever rounded or invented.

## Fields

### Field: quote

Type entity_ref of quote, required. The draft quote to invoice, named by its number, its identifier or the customer's name.

### Field: due_days

Type integer, optional, from 0 to 120, default 30. How many days from today the invoice is due.

## Validation rules

### Rule: quote_is_draft

The quote must still be a draft: a quote that was already invoiced cannot be invoiced again. A violation is reported as `conflict`.

### Rule: quote_still_valid

The quote must not have passed its last valid day. A violation is reported as `validation_failed`.

### Rule: due_days_in_range

The due period must be from 0 to 120 days. A violation is reported as `validation_failed`.

## Permissions

### Permission: invoices.issue

Allows issuing an invoice from a draft quote of the caller's team and resolving quotes. Granted to accountant, manager; checked by the host on every call. The host reauthorises every call from the identity it receives, from its own directory, and applies record-level visibility on top of the role, so holding the permission never reaches another organisation.

## Failure states

### Failure: permission_denied

The principal's role may not issue an invoice.

The user is told: "You are not allowed to issue invoices, so nothing was issued."

### Failure: scope_violation

The record is outside the principal's reach (another team without manager access, or another organisation), or does not exist. The application does not say which.

The user is told: "I cannot issue that: the quote is not available to you. Nothing was issued."

### Failure: validation_failed

A value is malformed or breaks one of the rules above. Nothing was changed.

The user is told: "I could not issue that: the quote is past its last valid day or the due period is out of range. Nothing was issued."

### Failure: missing_fields

A required field (quote) was not supplied. Nothing was changed.

The user is told: "I still need to know which quote to invoice. Nothing was issued."

### Failure: conflict

The quote was already invoiced. Nothing was changed.

The user is told: "That quote was already invoiced, so nothing was issued."

### Failure: stale

A record changed after the action was prepared, so the preparation is no longer valid. Nothing was changed; it can be prepared again.

The user is told: "The quote changed after I prepared this invoice, so I did not issue it. Nothing was issued; I can prepare it again."

### Failure: expired

The prepared action was not confirmed in time, or was withdrawn. Nothing was changed.

The user is told: "That was not confirmed in time, or it was withdrawn, so nothing was issued."

### Failure: uncertain_outcome

The commit response was lost. What was recorded is established through the status operation before anything is retried.

The user is told: "I could not confirm whether it went through. I will check before doing anything else, so please do not ask for it again yet."

### Failure: unavailable

The ledger cannot be reached. Nothing was changed.

The user is told: "The ledger is not reachable right now, so nothing was issued."

## Risk level

### Level 2

Level 2 (consequential or external side effect): the action is prepared, its exact effects are shown, and it is committed only after explicit confirmation. Effect class external: the customer is sent an invoice.

## Confirmation rule

### Confirmation: explicit

The user must confirm the exact prepared invoice before it is committed. A prepared action is valid for 10 minutes and is invalidated early if the record changes, if the permissions or the user change, or if the ledger signs an event about it.

## User-visible outcomes

### Outcome: issued

The user is told: "Done. Invoice {invoice_number} for {customer_name} of {invoice_total} is issued from quote {quote_number} and due on {invoice_due_on}."

Every value in braces is a fact returned by the application, not wording of the model.
