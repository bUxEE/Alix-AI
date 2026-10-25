# 0028 — Support policy when a provider changes model behaviour or terms

- **Status:** provisional
- **Date:** 2026-10-03
- **Decided in task:** X-01 (policy and the two resolutions below); implemented by X-05; stays provisional until the owed items at the end are closed

## Context

A hosted provider can change what a request costs, what it retains, where it processes, which model a name
points at and how a tool call behaves, without AlixAI shipping anything ("unexpected
provider/model substitution", "provider region or legal-policy change"). The plan requires revalidation "before
release and at configured intervals" and, in the §13 table, that a stale profile is quarantined pending review.
The open decision asks for the support policy when the provider does not preserve compatibility.

X-05 (merged) implemented the registry in `internal/providerreg`. Its work exposed two places where the
X-02 schema text and the registry disagree. This ADR fixes the policy and says which side wins; the code
described here exists on `main` at the date above, the rest is marked as owed.

## Options

Revalidation interval:

| Option | Security | Cost |
| --- | --- | --- |
| A. One fixed interval for everyone (30 days) | Strict, easy to audit | Operators with a quiet provider review more often than they would choose |
| B. Operator-set interval, 30 days is the **ceiling**, shorter allowed | Strict: the longest exposure is bounded, a regulated tenant can go shorter | None beyond A |
| C. Operator-set interval up to 365 days (what the X-02 schema bound allows) | A year of unreviewed terms and region drift is possible | Fewer reviews |

Model version change:

| Option | Security | Cost |
| --- | --- | --- |
| A. A new version is a **new profile id** (the X-02 schema description) | Clean audit trail per version, but permission grants, fallback pointers (`fallback.profile_id`) and the acknowledgement must be copied or re-created; a missed copy leaves a dangling pointer | Operator work on every version bump |
| B. The profile id is kept, the version is re-pinned, qualification resets (what X-05 implements) | Grants and fallback pointers stay valid and fail closed, because the re-pinned profile is no longer qualified until the suite passes for the new version; the event log keeps the old and new version under one id | One id spans several versions; the history is in `providerreg_events`, not in the id |

Capability revalidation: (1) automatic conformance re-run on a timer, which needs a live key, spends money and
cannot run in `verify`; (2) operator review on the interval plus quarantine on observed drift, with conformance
required again where the version or the review date demands it (the plan's "revalidation is required before
release and at configured intervals"). Option 2 is what exists.

## Decision

1. **Pin the model version.** A profile binds one `model_id` and one pinned `model_version`. The adapter sends
   the pinned id, never a floating alias. If the provider serves a different version, `Observe` quarantines the
   profile (`model_version_changed`).
2. **The review interval is 30 days, as a ceiling (option B).** `review_interval_days` may be 1 to 30. A
   profile is fresh until 00:00 UTC of the day `review_interval_days` after `last_policy_review` and stale from
   then on. **The registry wins over the schema:** `providerreg.Register` refuses a value above 30
   (`MaxReviewIntervalDays`) and the staleness check clamps to 30 as defence in depth, so a stored value above
   30 still goes stale at 30. A wider schema bound does not widen policy. Lengthening the ceiling needs a new
   ADR and the sign-off of X-H03.
3. **A changed model version keeps the profile id and resets qualification (option B).** `PinModelVersion`
   stores the new version, clears `last_conformance`, returns the profile to `candidate` and keeps the
   credential reference and the billing acknowledgement. **The code wins over the schema text** that says "a
   changed version is a different profile". What the schema sentence is right about is the consequence: the
   changed version is not trusted until a conformance report for the exact new version is recorded.
4. **A changed or stale profile is quarantined and never used.** Quarantine reasons are the schema's
   `stale_review`, `terms_changed`, `model_version_changed`, `conformance_failed`, `region_changed`,
   `excessive_permissions` and `operator_hold`. A quarantined profile is refused by `Check` and `Route` with
   `ErrQuarantined`. It never causes a fallback: the registry only follows a policy-named fallback for
   `unavailable`, `timeout`, `rate_limited` and `unsupported_modality`, and never for stale terms.
5. **What triggers a quarantine today.** `Check` (for a qualified profile), `Qualify` and `Revalidate` quarantine a
   profile whose review date passed the interval (`stale_review`). `Observe` quarantines on a changed terms hash, a different served model
   version and a changed processing-region set. A failing conformance report recorded for a qualified profile
   quarantines it (`conformance_failed`). Nothing else is observed: pricing changes and
   tool-behaviour changes are found by the operator's review or by the conformance suite, not by the registry.
6. **How a quarantine ends today** (from `internal/providerreg/lifecycle.go`):

   | Reason | Ends by | Result |
   | --- | --- | --- |
   | `stale_review`, `terms_changed`, `operator_hold` | `Review` with the terms hash that was read and a review date | `candidate`; `Qualify` is needed again |
   | `conformance_failed` | `RecordConformance` with a passing report for the exact version | `candidate` |
   | `model_version_changed` | `PinModelVersion` to the version now served | `candidate`, report cleared |
   | `region_changed`, `excessive_permissions` | `Review` does not release these | see "Known gaps" |

   `Qualify` never works on a quarantined profile. To qualify it needs, together: an active credential, the
   current billing notice acknowledged, a passing conformance report for the exact provider, model id and
   version, and a fresh review.

## Consequences

- Every provider term, region or version change costs the operator a review, and a bump of the model version
  costs a conformance run. That is the intent: nothing silently changes what leaves the deployment.
- The 30-day sweep is `Revalidate`, which nothing schedules yet. Until X-11 or X-24 schedules it, the lazy
  check inside `Check` is the enforcement, and it is sufficient: a stale profile cannot pass a request.
- The conformance report's age is checked at `Qualify` (the report must be within the interval). An
  already-qualified profile is held to its review date instead, so an ageing report does not by itself disable a
  profile. The operator's review procedure must therefore include re-running the suite when terms, pricing or
  tool behaviour changed. That procedure is a documentation item for X-24 (`docs/policies/provider-terms.md`).
- The registry holds the terms **hash** and the review date, not the terms text; the operator reads the terms
  and records the hash. Reading the terms is a human act (X-H01, X-H03).

## Known gaps (found in X-05, recorded here, not fixed here)

1. **Schema bound.** `schemas/providers/provider-profile.schema.json` allows `review_interval_days` 1 to 365 and
   describes 30 as a "default". Follow-up for a task that owns `schemas/providers`: maximum 30, wording
   "maximum", regenerate with `go run ./tools/schemagen`.
2. **Schema wording.** The `model_version` description says "a changed version is a different profile". Same
   follow-up: reword to "a changed version resets qualification (ADR-0028)".
3. **Re-pin lifts any quarantine.** `PinModelVersion` returns the profile to `candidate` from any state but
   retired, including a `terms_changed` or `region_changed` quarantine, without a `Review`. The profile still
   needs a passing report for the new version, a current review and an active credential to qualify, but the
   terms or region question is not forced to a human decision. Follow-up for the owner of `internal/providerreg`:
   re-pin should only lift `model_version_changed` (or leave other reasons in place).
4. **No release call for `region_changed` and `excessive_permissions`.** Only the re-pin above lifts them today.
   They need an explicit operator call; X-H03 decides the region case (ADR-0026).
5. **A terms-hash change does not clear the conformance report.** After `terms_changed` and a `Review`, the old
   report may still satisfy `Qualify` if it is inside the interval. The brief's rule "quarantined until
   conformance is re-run" therefore holds for a version change and for an old report, not for a terms change on a
   fresh report. Decision for the owner of `internal/providerreg`: clear the report on a changed hash, or accept
   that a changed hash needs only the human review.

## What confirms or overturns this (owed)

- The two schema follow-ups (1 and 2) merged, so the schema, the generated types and the registry agree.
- Gaps 3 to 5 each either fixed or accepted in writing.
- A scheduled `Revalidate` (X-11 or X-24) and the review procedure in `docs/policies/provider-terms.md` (X-24).
- Live evidence that a real provider's version and terms drift is detected: X-H02 (live) and X-H03 (review).
- No measurement is owed: this is a policy ADR. It stays `provisional` until the items above are closed.

## Reversal plan

The interval is the constant `MaxReviewIntervalDays` and the validation that uses it, in `internal/providerreg`.
Changing it, or adopting new-profile-per-version, is a change in that package plus a new ADR that supersedes
this one. Stored profiles keep their `model_version`, `last_policy_review` and `review_interval_days`, so no
data migration is needed to tighten the interval; loosening it needs the schema bound and the validation to
change together.
