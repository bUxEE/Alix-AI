# Provider terms, pricing and quarantine

Status: written in task X-24, 2026-10-08. Code: `internal/providerreg` (lifecycle, jobs), `schemas/providers/provider-profile.schema.json`. ADRs: 0028 (provider profile qualification and revalidation), 0025 (cost), 0026 (region). Design: [External provider mode](../architecture/external-provider-mode.md).

**External mode is optional, off by default, network-dependent and billed by the provider.** A provider's terms, price, retention, region, model availability and tool behaviour belong to the provider and change without notice. AlixAI does not assume they stay as they were; it records what was true when an operator reviewed them and refuses to use a profile whose record is out of date.

## 1. What is recorded

Each provider profile (one provider, one pinned model id and version, one application) carries:

- `terms`: retention (and days when limited), training use, terms URL, `terms_hash` (SHA-256 of the terms text that was read), `recorded_on`;
- `region`: residency, processing regions, compliance claims; unknown stays `unknown` and is denied by egress policy;
- `pricing`: a price table in integer micro-units per metered unit (tokens in and out, cached tokens, images, audio seconds, file bytes, requests). A used unit with no price makes the request unpriceable, so it is not sent. Estimates are upper bounds; the provider's invoice is final;
- `billing_notice` and its acknowledgement (consumer subscription is not API access), see `docs/operations/external-mode.md`;
- `last_policy_review` and `review_interval_days` (at most 30), `last_conformance`, `lifecycle` and, if quarantined, `quarantine_reason`.

Provider facts quoted in the repository (for example in ADR 0021) are read from vendor pages through a summarising tool and are not verified by AlixAI. The operator reviews the real terms; the registry only checks that the review is recent and that its hash has not changed.

## 2. Lifecycle

`candidate -> qualified -> quarantined -> candidate ...`; `retired` is terminal and a retired profile is never routed or qualified again.

A profile is **qualified** only when all of these hold together: a bound credential the credential service reports active; the current billing notice acknowledged (version and digest); a policy review that is not stale; and a passing conformance report for the exact provider, model id and model version (a successful basic chat request qualifies nothing). A changed model version clears the report and returns the profile to candidate.

## 3. Revalidation

A review is fresh until 00:00 UTC of the day `review_interval_days` after `last_policy_review`, and stale from then on.

- **Lazily:** every gate check on a profile quarantines it first if it is stale, so a missed sweep never lets a stale profile through.
- **On a schedule:** `providerreg` provides a class 6 maintenance job (`providerreg.revalidate`) and a scheduler (`RunRevalidationSchedule`, default hourly). `alixai serve` does not start that scheduler in this release; the lazy check is what applies until a composition-root task wires it.
- **On observation:** when the adapter reports what it sees, a differing terms hash, model version, processing region or a reported excess of credential permissions quarantines the profile.
- **By the operator:** `Review` records a new review (terms hash and date, dated today or earlier and not before the previous review), and `Hold` quarantines on the operator's own decision.

## 4. What quarantine means

A quarantined profile is **never used**: every request that would use it fails closed with the typed code `quarantined` whose `details.reason` is the quarantine reason, and no other provider is chosen in its place. Quarantine blocks only that profile; the deployment keeps running in Local mode and application data is untouched. A credential quarantined for excessive permissions or an unsupported account policy is never released automatically.

Each reason ends only through the action that addresses it, and the profile returns to `candidate`, which must then be qualified again:

| `quarantine_reason` | Cause | Ends with |
| --- | --- | --- |
| `stale_review` | review older than its interval | `Review` |
| `terms_changed` | terms hash differs | `Review` (a changed hash always clears the conformance report) |
| `operator_hold` | operator quarantined it | `Review` |
| `model_version_changed` | model version drifted from the pin | `PinModelVersion` to the new version, then a new conformance report |
| `conformance_failed` | a recorded report failed | a passing `RecordConformance` |
| `region_changed` | the provider moved its processing region | `Release` naming the regions the operator accepted |
| `excessive_permissions` | credential grants more than needed | `Release`, after the credential is active again (rotated or narrowed) |

These are Go calls on `internal/providerreg`. In this release no HTTP route or command exposes them, so an operator's tooling or the composition root calls them; `GET /v1/providers` shows the resulting `lifecycle` of each profile.

## 5. Pricing

Pricing is part of the profile and is reviewed with it. A reservation keeps the price table it was priced with when it settles. No route or command edits a profile's price table in this release; it is set when the profile is registered through `internal/providerreg`. A price change is therefore a registry change that a later task must expose; until then record the provider's new prices when you re-review.
