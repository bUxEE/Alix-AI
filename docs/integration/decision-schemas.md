# Decision-layer schemas

Source of truth: [docs/architecture/decision-layer.md](../architecture/decision-layer.md) §4 and
[ADR 0029](../decisions/0029-decision-layer-clm-8b.md). Where this page and those disagree, they win and this page is the bug. The schemas under `schemas/decision/`
are the contract; Go (`pkg/contracts/gen/decision`) and TypeScript (`sdk/typescript/src/gen/decision`) types are
generated from them by `go run ./tools/schemagen`, never written by hand.

The decision layer is **optional, bounded, non-generative and non-authoritative**. These schemas describe a scorer
that ranks a short list of candidates the core has already authorised. Nothing here is, or may be shaped like, an
authorisation, a permission, an approval, a validation, a preparation, a confirmation or a commit.

## 1. What is defined

| Contract | Schema | Purpose |
| --- | --- | --- |
| DecisionRequest | `decision-request.schema.json` | Input of `scoreDecision`: tenant, application, session, contract version, bounded state, allowed next actions, candidate set, locale, deadline. |
| CandidateSet | `candidate-set.schema.json` | 1 to 32 authorised candidates, `max_selections` (1 to 8) and the time the core built the set. |
| DecisionScoreResult | `decision-score-result.schema.json` | **Advisory** scorer output: either `accepted` or `fallback` with a typed reason. |
| FallbackReason | `fallback-reason.schema.json` | The 13 stable fallback strings. |
| DecisionStatus | `decision-status.schema.json` | Safe public status: mode, provider, model, revision, accepted and rejected counts, fallback counters. |
| ScorerManifest | `scorer-manifest.schema.json` | Provider, model, encoder, revision, file hashes, licence, signature. |
| shared definitions | `shared.schema.json` | `decisionType`, `schemaVersion`, `locale`, `unitScore`, `sha256`, `boundedScalar`. |

Every schema is JSON Schema 2020-12 with `$id` `urn:alixai:schema:decision:<name>:v1`. Objects are closed
(`additionalProperties: false`), so a field such as `authorised`, `approved`, `committed` or `permission` is rejected
rather than ignored. Examples live in `schemas/decision/examples/<name>/{valid,invalid}/` and are checked by
`go run ./tools/schemacheck schemas/decision`.

## 2. Authority

Every property carries `x-alixai-authority`, as in `schemas/actions` and `schemas/conversation`.

- **Every property of a DecisionScoreResult, DecisionStatus and ScorerManifest is `alixai`.** The scorer raw output is
  advisory input: the core stamps, gates and records each field after the call, and a model never writes one.
- In a DecisionRequest the scope, contract version, candidate-set envelope and allowed next actions are `alixai`
  (assembled after the deterministic checks); the bounded state and the candidate content (ID, label, text, data
  version) are `host`, because they come from the application through the deterministic adapter.
- There is no `model` authority anywhere in this area: the scorer proposes nothing that is not already in the candidate
  set.

## 3. The advisory result

`DecisionScoreResult` has `disposition` `accepted` or `fallback`:

- `accepted` means only that the confidence, calibration, candidate-size and out-of-distribution gates passed, so the
  core may use the ranking as one input to choosing an interpretation path. It requires `model_id`, `model_revision`,
  at least one selection and `ood_status` `in_distribution`, and forbids `fallback_reason`.
- `fallback` requires a `fallback_reason` and an empty `selections`; the controller path runs and produces the same
  deterministic outcome as with the layer off.

Each selection carries `candidate_id`, the raw `score` and `calibrated_confidence` (both 0 to 1). Every selected
candidate ID is re-resolved by the deterministic application adapter before any use; a high score never makes a
candidate authorised. SDKs and APIs must not present a result, a score or a status counter as an authorisation or a
commit result.

## 4. Fail closed

- Enums (`decision_type`, `disposition`, `fallback_reason`, `ood_status`, `cache_status`, `mode`, signature
  `algorithm`, licence `redistribution`) reject unknown values on decode, in Go and in the schemas.
- `schema_version` is the constant `1`; a different generation of the contract is rejected, not read loosely.
- Sizes are bounded: 1 to 32 candidates, at most 8 selections, at most 32 state entries of flat scalars, at most 16
  allowed next actions.
- Rules JSON Schema cannot state (duplicate candidate IDs, a stale `built_at`, a selected ID that is not in the request,
  tenant echo not matching the request, `rejected_count` equal to the sum of `fallback_counts`) belong to the guard in
  `internal/decision`, which answers `candidate_set_rejected`, `malformed_output` or `tenant_mismatch`. The generated
  types are the loose shape; schema validation stays authoritative.

## 5. Fallback reasons

| Reason | Meaning |
| --- | --- |
| `disabled` | The layer is off for this deployment, tenant or application. |
| `unavailable` | The provider is not reachable or not loaded. |
| `timeout` | The request deadline passed. |
| `malformed_output` | The provider output did not match the contract. |
| `low_confidence` | Calibrated confidence was below the gate. |
| `calibration_failed` | Calibration could not be applied or checked. |
| `ood_unsupported_shape` | The input was out of distribution or of an unsupported shape. |
| `model_mismatch` | The model or revision is not the one the manifest and registry pin. |
| `tenant_mismatch` | The result did not belong to the requesting tenant or application. |
| `policy_conflict` | The selection conflicted with a deterministic policy or scope check. |
| `candidate_set_rejected` | The guard rejected the candidate set (empty, oversized, stale, duplicate, cross-tenant, unauthorised). |
| `resource_gate` | The resource gate refused to run the scorer. |
| `unsupported_runtime` | The selected runtime or backend does not support the decision API. |

An exact deterministic match that skips the scorer is not a fallback.

## 6. Status and manifest

`DecisionStatus` is the safe operations view: `mode` (`off`, `shadow`, `advisory`), provider, model
and revision, `accepted_count`, `rejected_count` and one `fallback_counts` entry per reason that occurred. It carries
no thresholds, gates, scores, candidates, tenant data or keys.

`ScorerManifest` is verified before the scorer loads: provider, model and revision, the encoder
(an ID, a pinned revision and optionally its own SHA-256), every loaded file with its SHA-256 and size, the recorded
licence (SPDX ID, redistribution terms, optional https URL) and an `ed25519` signature. A scorer whose manifest does
not verify is not loaded. Field names avoid the substring `token` so the log redactor never masks them.

## 7. Checks

```bash
go run ./tools/schemacheck schemas/decision   # compile, valid examples pass, invalid examples fail
go run ./tools/schemagen --check              # generated Go and TypeScript are current
```
