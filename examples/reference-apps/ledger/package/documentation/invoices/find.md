# Find an invoice

Capability `invoices.find`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

Find the invoices of the caller's reach by number or customer name, with their exact totals, balances and states. An accountant or a viewer reaches the invoices of their own team, a manager every team of the organisation. Read-only.

Scope: tenant (one organisation; a manager reaches every team, everyone else their own team). Roles that may use it: accountant, manager, viewer.

Operations: `read` is the native method `invoices_find`.

Every amount is an integer count of minor units with an ISO 4217 currency. The ledger turns it into the exact decimal text of a money fact by integer arithmetic, so no amount is ever rounded or invented.

## Fields

### Field: query

Type string, required, length 2 to 100. An invoice number such as INV-0007, an invoice identifier or part of a customer name.

### Field: status

Type enum, optional, one of issued, partially_paid, paid, void. Only list invoices in this state.

### Field: max_results

Type integer, optional, from 1 to 10, default 5. How many invoices to list at most.

## Validation rules

### Rule: query_not_blank

The query must contain at least two characters that are not spaces. A violation is reported as `validation_failed`.

## Permissions

### Permission: invoices.read

Allows finding invoices and resolving them by mention. Granted to accountant, manager, viewer; checked by the host on every call. The host reauthorises every call from the identity it receives, from its own directory, and applies record-level visibility on top of the role, so holding the permission never reaches another organisation.

## Failure states

### Failure: permission_denied

The principal's role may not look up invoices.

The user is told: "You are not allowed to look up invoices, so nothing was searched."

### Failure: scope_violation

The record is outside the principal's reach (another team without manager access, or another organisation), or does not exist. The application does not say which.

The user is told: "I cannot show that: the invoice is not available to you."

### Failure: validation_failed

A value is malformed or breaks one of the rules above. Nothing was searched.

The user is told: "I could not search with that: give me at least two characters of an invoice number or a customer name."

### Failure: missing_fields

A required field (query) was not supplied. Nothing was searched.

The user is told: "I still need an invoice number or a customer name to search for."

### Failure: unavailable

The ledger cannot be reached. Nothing was searched.

The user is told: "The ledger is not reachable right now, so I could not search."

## Risk level

### Level 0

Level 0 (read-only, low sensitivity): the read runs after authorisation. Effect class none.

## Confirmation rule

### Confirmation: none

No confirmation: a read changes nothing.

## User-visible outcomes

### Outcome: found

The user is told: "I found {invoice_count} matching invoice(s): {invoice_summary}"

Every value in braces is a fact returned by the application, not wording of the model.
