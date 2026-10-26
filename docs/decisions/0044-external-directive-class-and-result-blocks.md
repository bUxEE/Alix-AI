# 0044 — Opt-in external realisation of live facts, with verified tables and charts

- **Status:** accepted
- **Date:** 2026-10-27
- **Decided in:** release 1.3.0

## Context

In External mode the dialogue core marks a response directive (and its live application facts)
restricted and local-only. An external provider therefore never realises a `present_result` reply:
the deterministic text template always does. Integrators who already allow conversation text to
leave at class `internal` asked for an opt-in way to send the verified directive so a hosted model
can word the answer — and for tables/charts of host records without inventing numbers.

## Options

### Sending the directive

- **A. Always send the directive in External mode.** Rejected: widens egress by default.
- **B. Operator opt-in `external_providers.directive_class` (`public` \| `internal` \| `confidential`,
  empty default).** Taken. The tenant egress policy must still allow categories
  `dialogue_directive` and `deterministic_fact` at that class; otherwise the template is used and
  the refusal is recorded. `restricted` is not eligible (same as other egress-eligible classes).

### Rich replies

- **A. Text only from the provider.** Incomplete for record lists and series.
- **B. Accept provider table/chart JSON parts, validate every number in text, table cells and chart
  data against the directive's facts; refuse → template.** Taken (`extmode.RichBlocks`,
  `verify.RealiseRich`).

### Privacy-first path (no model)

- **A. Text-only templates forever.** Incomplete.
- **B. When a `present_result` has host records (indexed facts / identifiers), emit a validated
  `table` block; when the conversation contract sets optional `presentation.chart`, also emit a
  matching `chart` block.** Taken. Additive optional schema field; absent means text (+ table when
  rows exist) only.

## Decision

Option B for all three. Defaults stay local (empty `directive_class`). Hybrid's
`hybrid_fact_class` is unchanged. Gateway emission of new `BlockEvent`s for template/model blocks
is a follow-on if the stream does not already carry them from another path.

## Consequences

Operators who set `directive_class` and allow the egress categories get externally worded answers
from live facts, still fail-closed on invented numbers. Packages may declare a chart hint without
breaking older cores (additive optional field).
