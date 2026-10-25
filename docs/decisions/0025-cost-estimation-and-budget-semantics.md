# 0025 — Cost estimation and budget semantics across different billing units

- **Status:** provisional
- **Date:** 2026-10-03
- **Decided in task:** X-01; finalised by X-09

## Context

Providers bill in different units: input and output tokens, cached input tokens, images, audio seconds, file
bytes, requests. Prices also carry multipliers that depend on where processing happens: as fetched on 2026-10-03
(summarised by a fetch tool, not verified by AlixAI), OpenAI charges a 10 % uplift on data-residency endpoints for
eligible models released on or after 2026-03-05, and Anthropic prices US-only inference at 1.1 times the standard
rate for Claude 4.6 and later models. External mode needs budgets, quotas, "estimated cost before expensive
operations", hard stops and reconciliation, and an exhausted budget stops external calls and shows the
budget state. The project's money rule is integer micro-units, never floats.

X-02 (merged) already fixed the data model; this ADR records the semantics on top of it
(`docs/integration/provider-schemas.md` §10):

- nine meter units: `toks_in`, `toks_out`, `toks_in_cached`, `images_in`, `images_out`, `audio_seconds_in`,
  `audio_seconds_out`, `file_bytes_in`, `requests`;
- a price table has at most one entry per unit, `{per_units, micro_units}`; a unit with no entry has no known
  price;
- cost for `u` units is `ceil(u * micro_units / per_units)` in integer arithmetic, summed over units;
- budget subjects are provider, tenant, workspace or principal, on an `interactive` or `background` channel,
  with a daily and a monthly limit (both required, no unlimited budget, 0 allows no spend), UTC calendar windows,
  `warn_at_percent` 1 to 99 and a hard stop at 100 that is not configurable; money is capped at 2^53-1.

## Options

| Option | Security | Cost and accuracy | Verdict |
| --- | --- | --- | --- |
| A. Floating-point currency | rounding drift, no exact reconciliation | easy | forbidden by the money rule |
| B. Token counts only | cannot cover images, audio, files; hides price changes | simple | rejected |
| C. Integer micro-units of an operator-set currency from a per-profile price table keyed by meter unit; pre-call estimate is an upper bound; actuals reconcile after the call | an unpriced unit is not estimated and not sent; hard stop cannot be overspent by an estimate that is too low | needs an operator-entered price table per profile | **default** |
| D. Trust the provider's reported cost only | no pre-call stop | no estimate before an expensive call | rejected as the only source; used as the actual when reported |

## Decision

1. **Money is an integer number of micro-units of one operator-set currency** (1,000,000 micro-units is one
   unit). AlixAI does no currency conversion; the operator enters prices in the currency they budget in.
2. **Prices come from the profile's price table.** A residency or tier multiplier is part of the price the
   operator enters for that profile (a US-only variant is priced at its multiplied rate, for example), so the
   estimate matches the invoice. A unit without a price means the request cannot be estimated, and an
   unestimated request is not sent.
3. **The pre-call estimate is an upper bound and drives the hard stop.** It uses the largest the request can
   consume: the bounded input size, the declared `max_output_toks`, and for any unit with more than one
   applicable rate (for example cached and uncached input) the higher rate. A request is refused when
   estimate plus what is reserved and spent in the window would exceed the limit. Warning at `warn_at_percent`,
   stop at 100.
4. **Reserve, settle, release.** The estimate is reserved before the call; the actual (provider-reported when
   given, otherwise metered) settles it and releases the difference; a reservation never settled returns after a
   timeout, so a crash cannot lock a budget (X-09). A call whose outcome is uncertain (connection lost, timeout)
   has cost basis `none` in its usage record, and its reservation is settled at the reserved upper bound until
   reconciliation, because the provider may have billed it. X-09 may overturn this default with a reason.
5. **Exhaustion is typed and chooses nothing.** The call fails with `resource_exhausted` and a budget-state
   detail; no alternate provider is selected unless the policy names a fallback (X-11).
6. **Background work has its own budget** (`channel: background`) and cannot consume the interactive one.
7. **Usage and reconciliation.** Every call writes a usage record (no prompt or response); X-09 exports JSON and
   CSV so an operator can reconcile against the provider's invoice. AlixAI's number is an estimate until it
   matches an invoice.

## Consequences

Easier: one comparable unit across providers, exact arithmetic, a stop that cannot be overshot by an
underestimate. Harder: operators keep price tables current (a stale price is a stale-terms case, ADR-0028), and
an upper bound over-reserves, so budgets run tighter than actual spend. Forbidden: floats, a hidden provider
switch on exhaustion, a request that uses an unpriced unit.

## What confirms or overturns this (owed)

- X-09: `TestEstimateIsUpperBound`, `TestReserveSettleRelease`, `TestUnsettledReservationReturnsAfterTimeout`,
  `TestWarningThenHardStop`, `TestExhaustedBudgetIsTypedAndNoAlternateProvider` (all planned) and the recorded
  ratio of estimate to actual on fixtures (no number is claimed here).
- X-H02: compare reported usage and cost with the provider's invoice for a live run.
- If over-reservation makes the hard stop unusable in practice (budgets exhausted well before real spend), X-09
  records the ratio and proposes a tighter bound for estimates; the bound must stay an upper bound.

## Reversal plan

The cost formula and reservation logic are inside `internal/providerbudget`; the price table and budget shapes are
schemas, so changing the estimate rule is a package change plus this ADR. Stored usage records keep their
metering and cost basis, so history stays readable under a changed rule.
