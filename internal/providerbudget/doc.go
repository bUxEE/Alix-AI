// Package providerbudget is the spend and usage control of external provider mode (additive plan
// section 11.2, ADR-0025): budgets per provider, tenant, workspace and principal, quotas per metered
// unit, rate and concurrency limits, estimate-before-call from the provider price table, reserve /
// settle / release accounting that survives a crash, and usage export with reconciliation. Nothing in
// this package contacts a provider; the router (X-11) calls Reserve before an external call and
// Settle or Release after it.
//
// # Money and units
//
// Money is an integer number of micro-units of one operator currency, capped at 2^53-1, never a
// float. The nine meter units are those of schemas/providers (toks_in, toks_out, toks_in_cached,
// images_in, images_out, audio_seconds_in, audio_seconds_out, file_bytes_in, requests). Estimate prices
// a request from the gen PriceTable as an UPPER BOUND: each unit rounds up on its own, input toks use
// the higher of the uncached and cached rates, and a used unit with no price is refused, so a request
// that cannot be priced is not sent. Cost prices actual metering the same way, so a call that stays
// within its reserved units never costs more than its estimate.
//
// # Budgets
//
// A budget (PutBudget) belongs to one subject (a provider, or a tenant, workspace or principal,
// optionally narrowed to one provider) and one channel, interactive or background. It always has a
// daily and a monthly limit (0 allows no spend, there is no unlimited budget), a warning percentage and
// optional quotas and rate limits. Windows are UTC calendar days and months from the injected clock.
// A call is charged against EVERY budget that applies to its scope, provider and channel, and is
// admitted only if all of them admit it. A call with no applicable budget on its channel is refused
// (reason no_budget): spending is never unmetered.
//
// The channel is part of the budget key, so background work (ingestion and embeddings, OCR and
// maintenance: ChannelForClass) can only draw on a background budget and never on the interactive one.
//
// # Reserve, settle, release
//
// Reserve checks and records the estimate in one write transaction: nothing is taken when any budget
// refuses. The reservation holds its amounts until it is settled or released, or until its timeout
// passes: a hold whose expires_at has passed is ignored by every sum, so a crash between Reserve and
// Settle cannot lock a budget, and the amount returns without a sweeper (Sweep only tidies the rows).
// Settle charges the ACTUAL (provider-reported cost when given, else the metered units priced from the
// reservation's price table) to the counters of the window the call was reserved in, even when the
// reservation had already timed out or the call went over its estimate: spend that happened is
// recorded. An uncertain outcome (the provider may have billed it) settles at the reserved upper bound
// with cost basis none, per ADR-0025. A reservation settles once (a second Settle is refused with
// ErrConflict); Release is idempotent and is refused only for a reservation already settled.
//
// # Exhaustion
//
// A window with nothing left is the hard stop: Reserve returns resource_exhausted whose Details name
// the reason (budget_exhausted, quota_exhausted, concurrency_limit, no_budget), the budget_state
// object, a local_fallback_available hint (from Config.LocalFallback, which carries the policy) and
// alternate_provider "none". The package takes one provider and has no way to name another: only an
// explicit policy-named fallback in the router (X-11) may switch. The per-minute rate limit returns
// rate_limited with retry_after_ms. A warning does not stop a call; it is reported on the Reservation.
//
// # Scope and the shared counter layout
//
// Every table carries application_id, tenant_id and workspace_id. Statements about a caller's own
// reservations and usage use store.ScopedWhere; the operator functions work at application level.
// Budget-owned rows (budgets, counters, holds, rate events) carry the scope of the budget; call-owned
// rows (reservations, usage) carry the scope of the call. A reservation can be settled or released only
// by the principal that made it. PutBudget, ListBudgets, State and the application-wide export are
// operator functions: the gateway must gate them by role; the package checks only that the subject
// lies in the caller's application. The counter table has the shape of internal/quotas' quota_counters
// (scope, dimension, window_start, used) so the per-tenant quota task can share counters later.
//
// # Reconciliation
//
// Every settled call writes one usage record (the gen UsageRecord, no prompt or response).
// ExportUsage returns them as JSON or CSV with totals; Reconcile compares the totals with the settled
// spend in the budget counters and with the provider's invoice figures, which AlixAI's numbers remain
// an estimate of until they match. Rows are selected by the UTC day the spend was charged to (the day
// the call was reserved), so a call reserved at 23:59 and settled at 00:01 counts in one day on both
// sides.
package providerbudget

import "github.com/buxee/alix-ai/internal/store"

// Module is the store module name of the migrations.
const Module = "providerbudget"

func init() { store.RegisterMigrations(Module, migrations) }
