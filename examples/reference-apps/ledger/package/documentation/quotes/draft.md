# Draft a quote

Capability `quotes.draft`, contract version 1.0.0. Synthetic reference application: every record is made up.

## Purpose

Draft a quote for a customer of the caller's team: a description, an exact amount in minor units of the customer's currency and a validity period. A draft is reversible: nobody is billed until an invoice is issued from it. The summary is shown and the draft is created at once.

Scope: workspace (one team). Roles that may use it: accountant, manager.

Operations: `validate` is the native method `quotes_draft_validate`; `prepare` is the native method `quotes_draft_prepare`; `commit` is the native method `quotes_draft_commit`; `status` is the native method `quotes_draft_status`.

A prepared action stays confirmable for 600 seconds. Commit is idempotent: the key is fixed when the action is prepared, a repeated commit returns the original receipt, and `status` reports what was recorded for a key so that a lost reply is never retried blindly. Validate, on unchanged data, reports the same data version as prepare, which is the snapshot of the records the action relies on (not a response tag and not an organisation-wide counter).

Every amount is an integer count of minor units with an ISO 4217 currency. The ledger turns it into the exact decimal text of a money fact by integer arithmetic, so no amount is ever rounded or invented.

## Fields

### Field: customer

Type entity_ref of customer, required. The customer the quote is for, named by its name or identifier.

### Field: description

Type string, required, length 3 to 200. What the quote is for.

### Field: amount_minor

Type integer, required, from 1 to 100000000000000. The quoted amount as a whole number of minor units of the currency (cents for EUR, yen for JPY, fils for KWD). A fraction is refused, never rounded.

### Field: currency

Type enum, optional, one of CHF, EUR, GBP, JPY, KWD, USD. Must be the customer's own currency; when omitted it is the customer's.

### Field: valid_days

Type integer, optional, from 1 to 90, default 30. How many days the quote stays valid.

## Validation rules

### Rule: description_present

The description needs at least three characters that are not spaces. A violation is reported as `validation_failed`.

### Rule: amount_in_range

The amount must be a whole number of minor units from 1 to 100000000000000. A violation is reported as `validation_failed`.

### Rule: currency_matches_customer

The currency must be the customer's own currency. A violation is reported as `validation_failed`.

### Rule: valid_days_in_range

The validity must be from 1 to 90 days. A violation is reported as `validation_failed`.

## Permissions

### Permission: quotes.draft

Allows drafting a quote for a customer of the caller's team and resolving customers. Granted to accountant, manager; checked by the host on every call. The host reauthorises every call from the identity it receives, from its own directory, and applies record-level visibility on top of the role, so holding the permission never reaches another organisation.

## Failure states

### Failure: permission_denied

The principal's role may not draft a quote.

The user is told: "You are not allowed to draft quotes, so nothing was changed."

### Failure: scope_violation

The record is outside the principal's reach (another team without manager access, or another organisation), or does not exist. The application does not say which.

The user is told: "I cannot draft that: the customer is not available to you. Nothing was changed."

### Failure: validation_failed

A value is malformed or breaks one of the rules above. Nothing was changed.

The user is told: "I could not draft that: the amount, the currency or the validity breaks a rule (the currency must be the customer's). Nothing was changed."

### Failure: missing_fields

A required field (customer, description, amount_minor) was not supplied. Nothing was changed.

The user is told: "I still need the customer, a description and an amount to draft the quote. Nothing was changed."

### Failure: conflict

The idempotency key was used for a different action. Nothing was changed.

The user is told: "That request clashes with something already recorded, so nothing was changed."

### Failure: stale

A record changed after the action was prepared, so the preparation is no longer valid. Nothing was changed; it can be prepared again.

The user is told: "The customer changed while I was preparing this, so I did not draft the quote. Nothing was changed; I can prepare it again."

### Failure: expired

The prepared action was not confirmed in time, or was withdrawn. Nothing was changed.

The user is told: "That was not confirmed in time, or it was withdrawn, so nothing was changed."

### Failure: uncertain_outcome

The commit response was lost. What was recorded is established through the status operation before anything is retried.

The user is told: "I could not confirm whether it went through. I will check before doing anything else, so please do not ask for it again yet."

### Failure: unavailable

The ledger cannot be reached. Nothing was changed.

The user is told: "The ledger is not reachable right now, so nothing was changed."

## Risk level

### Level 1

Level 1 (reversible, low impact): a concise summary is shown and the application allows an immediate commit. Effect class reversible: a draft quote can be discarded and bills nobody.

## Confirmation rule

### Confirmation: none

No separate confirmation: the application allows an immediate commit after the concise summary. The prepared draft is valid for 10 minutes and is invalidated early if the customer changes, or if the permissions or the user change.

## User-visible outcomes

### Outcome: drafted

The user is told: "Done. Quote {quote_number} for {customer_name} of {quote_total} is drafted and valid until {quote_valid_until}."

Every value in braces is a fact returned by the application, not wording of the model.
