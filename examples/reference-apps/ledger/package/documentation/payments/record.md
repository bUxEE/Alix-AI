# Record a payment

Capability `payments.record`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

Record a payment received on an invoice of the caller's team, in exact minor units of the invoice's currency. A payment can never exceed the balance. The payment is prepared first, its exact effects are shown, and it is recorded only after the user confirms. Recording reduces the balance and sends a receipt to the customer.

Scope: workspace (one team). Roles that may use it: accountant, manager.

Operations: `validate` is the native method `payments_record_validate`; `prepare` is the native method `payments_record_prepare`; `commit` is the native method `payments_record_commit`; `status` is the native method `payments_record_status`.

A prepared action stays confirmable for 600 seconds. Commit is idempotent: the key is fixed when the action is prepared, a repeated commit returns the original receipt, and `status` reports what was recorded for a key so that a lost reply is never retried blindly. Validate, on unchanged data, reports the same data version as prepare, which is the snapshot of the records the action relies on (not a response tag and not an organisation-wide counter).

Every amount is an integer count of minor units with an ISO 4217 currency. The ledger turns it into the exact decimal text of a money fact by integer arithmetic, so no amount is ever rounded or invented.

## Fields

### Field: invoice

Type entity_ref of invoice, required. The invoice that was paid, named by its number, its identifier or the customer's name.

### Field: amount_minor

Type integer, required, from 1 to 100000000000000. The amount received as a whole number of minor units of the invoice's currency. A fraction is refused, never rounded.

### Field: currency

Type enum, optional, one of CHF, EUR, GBP, JPY, KWD, USD. Must be the invoice's own currency; when omitted it is the invoice's.

### Field: paid_on

Type date, optional. The date the money was received, from the invoice date to today. When omitted it is today in the ledger's time (UTC).

### Field: method

Type enum, optional, one of bank_transfer, card, cash, default bank_transfer. How the money was received.

## Validation rules

### Rule: invoice_open

The invoice must be issued or partially paid: a paid or void invoice takes no payment. A violation is reported as `conflict`.

### Rule: amount_in_range

The amount must be a whole number of minor units from 1 to 100000000000000. A violation is reported as `validation_failed`.

### Rule: currency_matches_invoice

The currency must be the invoice's currency. A violation is reported as `validation_failed`.

### Rule: amount_within_balance

The payment cannot be more than the balance still to pay. A violation is reported as `validation_failed`.

### Rule: paid_on_valid

The date must be a valid date from the invoice date to today. A violation is reported as `validation_failed`.

## Permissions

### Permission: payments.record

Allows recording a payment on an invoice of the caller's team. Granted to accountant, manager; checked by the host on every call. The host reauthorises every call from the identity it receives, from its own directory, and applies record-level visibility on top of the role, so holding the permission never reaches another organisation.

## Failure states

### Failure: permission_denied

The principal's role may not record a payment.

The user is told: "You are not allowed to record payments, so nothing was recorded."

### Failure: scope_violation

The record is outside the principal's reach (another team without manager access, or another organisation), or does not exist. The application does not say which.

The user is told: "I cannot record that: the invoice is not available to you. Nothing was recorded."

### Failure: validation_failed

A value is malformed or breaks one of the rules above. Nothing was changed.

The user is told: "I could not record that: the amount is more than the balance, the currency is not the invoice's, or the date is not valid. Nothing was recorded."

### Failure: missing_fields

A required field (invoice, amount_minor) was not supplied. Nothing was changed.

The user is told: "I still need the invoice and the amount to record the payment. Nothing was recorded."

### Failure: conflict

The invoice is paid or void and takes no payment, or the idempotency key was used for a different action. Nothing was changed.

The user is told: "That invoice takes no payment in its current state, or the request clashes with one already recorded. Nothing was recorded."

### Failure: stale

A record changed after the action was prepared, so the preparation is no longer valid. Nothing was changed; it can be prepared again.

The user is told: "The invoice changed after I prepared this payment, so I did not record it. Nothing was recorded; I can prepare it again."

### Failure: expired

The prepared action was not confirmed in time, or was withdrawn. Nothing was changed.

The user is told: "That was not confirmed in time, or it was withdrawn, so nothing was recorded."

### Failure: uncertain_outcome

The commit response was lost. What was recorded is established through the status operation before anything is retried.

The user is told: "I could not confirm whether it went through. I will check before doing anything else, so please do not ask for it again yet."

### Failure: unavailable

The ledger cannot be reached. Nothing was changed.

The user is told: "The ledger is not reachable right now, so nothing was recorded."

## Risk level

### Level 2

Level 2 (consequential or external side effect): the action is prepared, its exact effects are shown, and it is committed only after explicit confirmation. Effect class external: the customer is sent a receipt.

## Confirmation rule

### Confirmation: explicit

The user must confirm the exact prepared payment before it is committed. A prepared action is valid for 10 minutes and is invalidated early if the record changes, if the permissions or the user change, or if the ledger signs an event about it.

## User-visible outcomes

### Outcome: recorded

The user is told: "Done. Payment {payment_reference} of {payment_amount} is recorded on invoice {invoice_number}. Balance remaining: {balance_remaining}. Invoice status: {invoice_status}."

Every value in braces is a fact returned by the application, not wording of the model.
