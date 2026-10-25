# 0028 — Provider profile qualification and revalidation (addendum, closes the known gaps)

- **Status:** provisional (addendum to [0028 provider-change-and-terms-policy](0028-provider-change-and-terms-policy.md); same decision number, written by X-25)
- **Date:** 2026-10-07
- **Decided in task:** X-25 (implements the owed items 1 to 5 and the schedule of ADR 0028)

## Context

ADR 0028 fixed two disagreements with the code winning (30 days is a ceiling; a changed model version keeps the
profile id and resets to candidate) and recorded five gaps in `internal/providerreg` and `schemas/providers`.
X-25 closes them. Nothing here widens what a profile may do: every change makes a quarantine harder to leave.

## Decision

1. **Schema.** `review_interval_days` has `maximum: 30` (a ceiling; shorter is allowed) and the `model_version`
   description says a changed version keeps the profile id, clears the report and resets to candidate.
   Generated types were rebuilt by `go run ./tools/schemagen`.
2. **A pin never ends a foreign quarantine.** `PinModelVersion` stores the version and clears the report, but only a
   `model_version_changed` quarantine ends with it. Any other reason stays until its own call.
3. **Every quarantine reason has exactly one way out:**

   | Reason | Ends by | Result |
   | --- | --- | --- |
   | `stale_review`, `terms_changed`, `operator_hold` | `Review` | `candidate` |
   | `region_changed` | `Release` with a review date and the regions the operator accepted | `candidate`, recorded regions replaced |
   | `excessive_permissions` | `Release` with a review date, while the credential service reports the bound credential active | `candidate` |
   | `conformance_failed` | `RecordConformance` with a passing report for the exact version | `candidate` |
   | `model_version_changed` | `PinModelVersion` | `candidate`, report cleared |

   `Release` refuses any other reason, a profile that is not quarantined, a retired profile and a profile of another
   application. `Observe` gained `ExcessivePermissions` so that reason can be entered by the credential service's
   report. A released profile is not qualified: `Qualify` is still needed.
4. **A changed terms hash clears the conformance report** (`Review`, any lifecycle). The report was produced under the
   old terms, so the profile qualifies again only on a report recorded after the review. The earlier "accept that a
   changed hash needs only the human review" option is rejected.
5. **Revalidate is scheduled.** `JobRevalidate` is a class 6 (maintenance) job over the shared pool, run under the
   declared service identity `providerreg.revalidate` (the handler refuses a job without it). `ScheduleRevalidation`
   queues one job per application with a candidate or qualified profile per interval (default one hour, idempotent per
   period); `RunRevalidationSchedule` repeats it. The lazy check in `Check` remains the enforcement that cannot be
   missed; the job makes the quarantine and its audit event happen without a request.

## Consequences

- `internal/providerreg` now imports `internal/jobs` (base + store only); `docs/architecture/module-map.md` lists no such
  edge and needs one line (outside X-25's scope).
- The composition root (`internal/cli/providers_external.go`, `!noexternal`) must register `providerreg.JobSpec()`,
  call `Registry.Handle(pool)`, accept `providerreg.ServiceIdentity` in the pool's `ServiceAuthorizer` and run
  `RunRevalidationSchedule`. Until X-10/X-11 does that, only the lazy check runs.
- The review procedure (X-24, `docs/policies/provider-terms.md`) must say: a terms change costs a new conformance run;
  region and excessive-permission quarantines end through `Release`, not `Review`.

## Reversal plan

The release rules are in `lifecycle.go`; removing `Release` leaves region and permission quarantines with no exit, which
fails closed. The job is additive: not registering it returns to the lazy check alone.
