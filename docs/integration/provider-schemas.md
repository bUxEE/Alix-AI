# External provider schemas

Design: [External provider mode](../architecture/external-provider-mode.md). The schemas under `schemas/providers/` are the
contract; this page explains them. Go and TypeScript types are generated from the schemas
(`go run ./tools/schemagen`), never written by hand. Where this page and the schemas disagree,
the schemas win and this page is the bug.

External mode is an opt-in addition to Local mode. Nothing here changes an existing schema, and with
external mode off none of these objects is ever created. `schemas/models` describes models AlixAI
**runs** (the local registry); `schemas/providers` describes **hosted** providers and is separate.

## 1. What is defined

All schemas are JSON Schema 2020-12 with `$id` `urn:alixai:schema:providers:<name>:v1`. Examples live in
`schemas/providers/examples/<name>/{valid,invalid}/`, each invalid example breaking one rule.
Generated Go types are in `pkg/contracts/gen/providers` (import
`github.com/buxee/alix-ai/pkg/contracts/gen/providers`), TypeScript in `sdk/typescript/src/gen/providers`.

| Schema | Root Go type | What it is | Plan |
| --- | --- | --- | --- |
| `shared` | (definitions library) | IDs, micro-units, enums used by several schemas | §5, §6, §11 |
| `data-classification` | `DataClassification`, `EgressEligibleClass` | public, internal, confidential, restricted; and the subset that can ever leave | §6.1 |
| `provider-profile` | `ProviderProfile` | one hosted provider and model: capabilities, terms, pricing, permissions, lifecycle | §5 |
| `price-table` | `PriceTable` | prices per metered unit in integer micro-units | §5, §11.2 |
| `egress-policy` | `EgressPolicy` | one data-egress policy at one level | §6, §8 |
| `egress-decision` | `EgressDecision` | the audit record of one allow or deny, no content | §6.2 step 7 |
| `egress-disclosure` | `EgressDisclosure` | what the host may show a user before data leaves | §6.3 |
| `request-envelope` | `RequestEnvelope` | the bounded request handed to an adapter | §6.2, §3 |
| `provider-error` | `ProviderError` | a normalised provider failure | §13, §15.1 |
| `usage-record` | `UsageRecord` | what one call consumed and cost | §11.2 |
| `budget` | `Budget` | limits, quotas and rate limits of one budget | §11.2 |
| `budget-state` | `BudgetState` | the state of a budget as the host displays it | §11.2, §17 |
| `credential-ref` | `CredentialRef` | what AlixAI may know about a credential (never a secret) | §11.1 |
| `connection-status` | `ConnectionStatus` | the only credential-related object a host application receives | §11.1, §17 |
| `billing-notice-ack` | `BillingNoticeAck` | the recorded acknowledgement of the billing notice | §4.1 |
| `conformance-report` | `ConformanceReport` | a provider suite result, recorded as a profile's `last_conformance` | §15 |

`egress-disclosure`, `connection-status`, `budget-state` and the `deploymentMode` enum go beyond the
deliverables listed for X-02. Later tasks (X-04, X-11, X-13, X-14) need typed contracts for them and
cannot edit this area, so they are defined here.

## 2. Rules that hold for every schema

- **No secret has a place to live.** There is no member that holds an API key, a token, a password, a
  cookie or key material. A credential appears only as an opaque reference (`cred_...`) and, for a
  manual key, a masked fingerprint (`sha256:` plus the first 8 hex digits of the key's SHA-256, never a
  prefix or suffix of the key). Every object is closed (`additionalProperties: false`), so a secret
  cannot be added to one; each schema has invalid examples that try (`api_key`, `access_token`,
  `refresh_token`, `response_body`, `headers`).
- **Key names avoid the log redactor.** `internal/log` redacts by substring of the key name (token,
  secret, password, authorization, apikey, cookie, privatekey, credential). No property in this area
  matches, which is why the profile member is `cred_ref`, the conformance checks are
  `creds_never_in_responses_or_logs` and `no_path_or_cred_leakage`, and usage counters are `toks_in` and
  `toks_out`. A scan of the schemas for those substrings in property names returns nothing.
- **Money is integer micro-units** of the operator currency (1 000 000 micro-units is one unit), capped at
  2^53-1 so every JSON implementation represents it exactly. There are no floats anywhere in a money or
  counter field; examples try `0.15`, `-1` and `9007199254740993`.
- **Unknown values fail closed.** Every vocabulary is a closed enum. The generated Go types reject an
  unknown enum value, `null` and a wrong type on decode and refuse to encode an unset enum; unions
  reject an unknown discriminator. Decoding with `DisallowUnknownFields` rejects every invalid example
  that adds a member or uses an unknown enum or union value (`TestUnknownMembersAndValuesFailDecoding`
  in `pkg/contracts/gen/providers`); patterns, bounds and cross-field rules stay with schema validation.
- **Everything is bounded.** Every string has a pattern, enum, format or `maxLength`; every array has
  `maxItems`; every integer a `maximum`; open objects have `maxProperties`. The envelope bounds are in
  section 8.
- **Explicit nulls.** A member that is "not known yet" is required and nullable (`cred_ref`,
  `last_conformance`, `billing_notice_ack`, `fallback.profile_id`), so the shape of a record cannot
  silently shrink and an absent value is never read as a default.
- **No wildcards.** Identifiers cannot contain `*` (the common `identifier` grammar), allowlists have no
  "all" value, and an empty allowlist allows nothing.

## 3. Authority

Every property carries `x-alixai-authority` (see `docs/integration/conversation-objects.md` §2):

| Authority | Used here for |
| --- | --- |
| `host` | Operator-supplied content: provider profile content (endpoint, capabilities, terms, region, rate limits, permissions, fallback, review dates, billing notice), pricing, egress policy content (levels, modes, allowlists, classes, regions), budget limits. |
| `alixai` | Everything decided, minted or recorded by the core: IDs, revisions and timestamps, credential references and their status, lifecycle, acknowledgements, egress decisions, envelopes, usage records, conformance results, connection status, budget state, disclosures. |
| `model` | Never used. No object in this area is written by a model. A provider's output is untrusted data that is validated before use. |

A `host` container holds only `host` members; an `alixai` container may hold either. The existing
authority-exhaustiveness test in `tools/schemagen` (`TestAuthorityAnnotationsExhaustive`) covers
`actions` and `conversation`. `providers` is not in its `authorityAreas` list yet; running the test with
`providers` added passes (see section 16).

## 4. Typed IDs

Core-minted IDs are a short prefix, an underscore and a 26-character ULID. The prefixes are inside the
2 to 8 letter range of `common/ids` `typedId`.

| Prefix | Shared def | Go type | Used for |
| --- | --- | --- | --- |
| `pprf_` | `profileId` | `ProfileID` | provider profile |
| `cred_` | `credentialRefId` | `CredentialRefID` | credential reference |
| `epol_` | `egressPolicyId` | `EgressPolicyID` | egress policy |
| `edec_` | `egressDecisionId` | `EgressDecisionID` | egress decision |
| `envl_` | `envelopeId` | `EnvelopeID` | request envelope |
| `usg_` | `usageId` | `UsageID` | usage record |
| `bud_` | `budgetId` | `BudgetID` | budget |
| `rsv_` | `reservationId` | `ReservationID` | budget reservation |
| `conf_` | `reportId` | `ReportID` | conformance report |
| `ack_` | `acknowledgementId` | `AcknowledgementID` | billing notice acknowledgement |
| `fil_`, `src_` | `fileId`, `sourceId` | `FileID`, `SourceID` | repeat the grammar of `actions/shared` so this area depends on `common` only |

`providerId` is an operator slug (`^[a-z][a-z0-9_-]{0,31}$`), `modelId` and `modelVersion` are provider
strings (letters, digits, `. _ : + -`). A model id never contains a path or a URL.

## 5. Data classification and the section 6.1 defaults

`data-classification` is the ordered enum `public < internal < confidential < restricted`. How each row
of the §6.1 table is expressed:

| §6.1 row | Where it is enforced |
| --- | --- |
| Public: may be sent if provider and application policy allow | `EgressEligibleClass`; the policy chain decides. |
| Internal: blocked unless an authorised operator allows it | Listing `internal` in `allowed_classes` requires an `approval` (who and when). A source with class internal or confidential and mode `allow_excerpt` or `allow_metadata` requires an `approval`. |
| Confidential: blocked by default, per-source approval | `confidential` cannot be listed in `allowed_classes` above source level; at source level it requires an `approval`. An excerpt of an uploaded file has no source: it leaves only under the `files` setting (section 6), and a `files.max_class` of `internal` or `confidential` requires an `approval`. |
| Restricted/regulated: local-only unless an approved compliance profile exists | `restricted` is **not** egress-eligible: it cannot appear in any `allowed_classes` or `allowed_data_classes`, nor as the classification of a part in a request envelope. A `restricted` source can only be `local_only` or `deny`. Version 1 has no compliance profile, so this is unconditional (see section 14). |
| Credentials and secrets: never sent | Not a classification and not a sendable category. `credential` is a `blockedCategory`; there is no way to place it in an envelope part, and the decision record lists it under `stripped`. |
| Raw application database content: never sent directly | Same: `raw_database_content` is a `blockedCategory`. |
| RAG excerpts: only when source, tenant, purpose and provider policy allow | A `rag_excerpt` part must name its `source_id` and `source_mode` `allow_excerpt`; `rag_metadata` needs `allow_excerpt` or `allow_metadata`. A `local_only` or `deny` source cannot be built into an envelope. |
| Unknown classification: most restrictive | A part with no classification does not validate, so it cannot be built. Nothing defaults to a permissive value. |

The sendable categories are `user_message`, `conversation_history`, `structured_state`,
`deterministic_fact`, `dialogue_directive`, `action_receipt`, `rag_excerpt`, `rag_metadata`,
`file_content`, `image_content`, `audio_content`. The never-sent categories the guard removes and
records are `credential`, `raw_database_content`, `filesystem_path`, `database_handle`, `mcp_url`,
`local_only_field`.

## 6. Egress policy

One `EgressPolicy` per level: `deployment`, `application`, `tenant`, `workspace`, `source`. `target`
names the level (`deployment_id` always; narrower levels add `application_id`, `tenant_id`,
`workspace_id`, `source_id`, each requiring the wider ones; a level never names something narrower than
itself). Members: `enabled`, `source_mode`, `source_class` (source level only), `allowed_providers`,
`allowed_models`, `allowed_classes`, `allowed_regions`, `files` and `audio` (optional, not at source
level), `approval`.

- **Most restrictive wins.** The effective policy is the most restrictive along the chain deployment,
  application, tenant, workspace, source. A level with no policy is disabled (external mode is off by
  default); a source with no policy is `local_only`.
- **Mode order, most restrictive first:** `deny` (excluded from the capability entirely), `local_only`,
  `allow_metadata`, `allow_excerpt`. `source_mode` at a higher level is a ceiling for the sources below
  it.
- **Empty allowlists allow nothing.** A model is allowed only if its provider is also allowed.
  `allowed_regions` must list every region the provider processes in; an `unknown` or `global` residency
  never satisfies it unless `global` is listed explicitly. The literal `unknown` is not a region code and
  cannot be allowed (`regionCode` pattern).
- **Uploaded files (`files`).** `{ "enabled": bool, "max_class": public | internal | confidential }`.
  Absent, or `enabled: false`, keeps every excerpt of an uploaded file (`file_content` sent as text) local:
  that is the default. An excerpt leaves only when all four request levels enable it, only up to the
  lowest `max_class` along the chain, only to the providers, models and regions the chain allows and, for
  public and internal, only when `allowed_classes` lists the class too; restricted and unclassified
  excerpts and credentials never leave. A `max_class` of `internal` or `confidential` needs the
  `approval`; a source policy has no `files` setting (an upload has no source). The decision of a request
  that carries an excerpt records a `files` check after `classification`.
- **Raw audio (`audio`).** The same shape and rules as `files`, for raw audio sent to an external speech
  recogniser (`audio_content`). Absent keeps all audio local, and a policy that lets text of a class leave
  does not let audio of that class leave. The class of the audio is the one its session states; none is
  unclassified and never leaves. The decision of a request that carries audio records an `audio` check.
- **Schema-enforced rules** (each has an invalid example): level-specific `target` shape; `source_class`
  present exactly at source level; restricted source cannot be `allow_excerpt` or `allow_metadata`
  (`source-restricted-allow-excerpt`); `confidential` only at source level; the approval rules in
  section 5.
- **Left to the engine (X-04):** intersection down the chain, restrictiveness order, region subset test,
  the kill switch, and that an `approval.approved_by` is an authorised operator.

## 7. Egress decision and disclosure

`EgressDecision` is stored as the audit record of every decision. It has **no content**: every string is
an ID, a fixed code or an enum. It records the verified `subject`, the exact `provider` (profile, model,
version), the `role`, the `envelope_id`, `outcome` (`allow` or `deny`), a fixed `reason`, the `levels`
consulted, the ordered `checks`, the `categories` `sent`, `withheld` and `stripped`, the per-source
outcomes and `rag_excerpts_included`. Rules in the schema:

- `allow`: `reason` is `allowed`, no `code`, all four levels have a policy and are enabled, every check
  passed.
- `deny`: `reason` is not `allowed`, `code` is `permission_denied`, at least one check failed, nothing was
  sent (`categories.sent` empty, `rag_excerpts_included` false).
- A source that is `local_only` or `deny` cannot be `excerpt_included` or `metadata_included`.
- `rag_excerpts_included` is true exactly when `rag_excerpt` is in `categories.sent`.
- `fallback_of` names the decision this one is a recorded fallback for, or null.

`EgressDisclosure` is the §6.3 object: provider and model, whether the request may leave
(`external_enabled`), the classes and categories that may leave, whether RAG excerpts are included,
regions, the provider's retention and training terms as stated (`unknown` stays `unknown`), an upper-bound
cost estimate in micro-units or null, the budget status, and `on_unavailable` (`fail`, or `fallback` with
the one named profile). When nothing may leave, nothing is listed.

## 8. Request envelope

`RequestEnvelope` is what the core hands to an adapter after the egress guard passes. It has a **wire
part** (`payload`) and **internal metadata** (`internal` at the root and on every part).

**Wire projection.** An adapter builds the provider request from, and only from:

- `payload.instructions` (written by the core: fixed templates, never user, host or provider text),
- `payload.messages[].role` and `parts[]` (`text` content, or the bytes of the file a `file_id`
  resolves to through the scoped file service),
- `payload.tools` (allowlisted schemas; the provider can only return candidate calls),
- `payload.response_format`,
- `payload.limits.max_output_toks`,
- the model identifier from `provider.model_id`.

Everything else is stripped and never sent: the root `internal` object (subject, trace, channel,
reservation, size and estimate), every part's `internal` object (category, classification, source,
source mode, size), `envelope_id`, `created_at`, `provider.profile_id` and `model_version`, `role`,
`payload.limits.timeout_ms` (a local deadline) and the `file_id` values themselves. The provider never
receives a filesystem path, a database handle, a reusable credential or an MCP URL; none has a member.

**What cannot be expressed.** The `payload` is closed, so there is no member for hosted memory,
`previous_response_id`, file search, agents, server-side tool loops or remote MCP servers (invalid
examples try `mcp_servers`, `previous_response_id`, `file_search`, `headers`). There is no `system`
message role: instructions are separate and core-written.

**Per-part metadata is mandatory.** A part is a union on `type` (`text`, `image`, `file`, `audio`). Each
carries `internal.category`, `internal.classification` (egress-eligible classes only) and
`internal.size_bytes`; RAG parts also `source_id` and `source_mode` with the rules in section 5. A
text part's category is one of the eight text categories; an image part must be `image_content`, a file
part `file_content`, an audio part `audio_content`.

| Bound | Value |
| --- | --- |
| `instructions` | 1 to 32768 characters |
| messages | 1 to 64 |
| parts per message | 1 to 32 |
| text of one part | 1 to 65536 characters |
| tools | 0 to 32; name `^[A-Za-z0-9_-]{1,64}$`, description 1 to 1024 characters |
| `parameters`, `response_format.schema` | open objects, at most 128 top-level members |
| `internal.payload_bytes` | 0 to 1 048 576 (the builder measures it; the guard recomputes) |
| `max_output_toks` | 1 to 262 144 |
| `timeout_ms` | 1 to 600 000 |

`response_format.kind` is `text`, `json_object` or `json_schema`; `json_schema` requires `schema`, the
others forbid it. For a TaskProposal, `schema` is the grammar with every non-`model` property removed
(`conversation-objects.md` §3).

## 9. Provider profile

`ProviderProfile` covers every field a provider profile needs:

| §5 field | Member |
| --- | --- |
| provider ID and endpoint | `provider_id`, `endpoint` (https, or plain http on loopback for a mock; no userinfo, no query, no fragment, no `..`) |
| model ID and version | `model_id`, `model_version` (pinned) |
| credential reference, never the secret | `cred_ref` (`cred_...` or null) |
| input and output modalities | `modalities.input`, `modalities.output` |
| streaming | `streaming` |
| structured-output guarantee | `structured_output`: `none`, `json_mode`, `schema_best_effort`, `strict_schema` (only the last is a guarantee) |
| tool or function calling | `tool_calling` {`supported`, `strict_arguments`, `parallel_calls`, `max_tools`} |
| remote MCP | `remote_mcp.supported` (recorded only; AlixAI never gives a provider an MCP URL) |
| context and output limits | `limits` {`max_context_toks`, `max_output_toks`, `max_request_bytes`} |
| file, image, audio handling | `media.files`, `media.images`, `media.audio` {`max_bytes`, `mime_types`, `retention`}; required when the modality is declared |
| image generation | `image_generation` |
| voice, STT, TTS | `voice` {`stt`, `tts`, `realtime`, `audio_retention`} |
| retention and training terms | `terms` {`retention`, `retention_days`, `training_use`, `terms_url`, `terms_hash`, `recorded_on`} |
| geographic and compliance | `region` {`residency`, `processing_regions`, `compliance`} |
| rate limits and retry | `rate_limits` {requests, toks, concurrency, `retry`} (retry bounded to 10 attempts) |
| pricing and metering | `pricing` (a `price-table`), `usage_metering` |
| permitted applications, tenants, workspaces | `permitted_scopes[]` {`application_id`, optional `tenant_id`, optional `workspace_id`}; omitting a narrower level grants every value inside the parent; empty permits nobody |
| allowed data classifications | `allowed_data_classes` (egress-eligible classes) |
| fallback provider | `fallback` {`profile_id`, `on`} |
| last conformance and policy review | `last_conformance` (a `conformance-report` or null), `last_policy_review`, `review_interval_days` |
| billing notice | `billing_notice` {`version`, `text`, `url`, `subscription_grants_api_access` always `false`}, `billing_notice_ack` |

Also: `roles` (the roles the operator intends; the decision-scorer role `router` is not a possible value),
`native_features.available` (recorded only; there is no switch that enables them), `lifecycle`
(`candidate`, `qualified`, `quarantined`, `retired`) and `quarantine_reason`.

**Qualification.** `lifecycle: qualified` requires a bound `cred_ref`, a non-null `billing_notice_ack`
and a `last_conformance` whose `outcome` is `passed`. The registry (X-05) additionally checks that the
report's `model_id` and `model_version` equal the profile's and that the acknowledgement's notice
version and digest match the current notice; JSON Schema cannot compare two members. `quarantined`
requires `quarantine_reason` (`stale_review`, `terms_changed`, `model_version_changed`,
`conformance_failed`, `region_changed`, `excessive_permissions`, `operator_hold`); no other lifecycle may
carry one.

**Fallback.** `fallback.on` can only hold `unavailable`, `timeout`, `rate_limited`,
`unsupported_modality`. Invalid credentials, stale terms, denied egress and an exhausted budget cannot be
listed, so no profile can ask for a fallback on them. A named fallback must list at least one trigger;
no fallback lists none. That the fallback exists, is not this profile and forms no cycle is a registry
rule.

**Consistency rules the schema states:** image generation needs `image` in output modalities; speech to
text needs `audio` input and text to speech `audio` output; a declared image, file or audio modality
needs its `media` entry; without tool calling there are no strict or parallel calls; `retention_days` is
present exactly when `retention` is `limited`; an `unknown` residency lists no regions and any other
lists at least one.

## 10. Pricing, usage and budgets

**Price table.** `prices` has one optional entry per meter unit, each `{per_units, micro_units}`: the
price of `per_units` units in micro-units. The meter units are `toks_in`, `toks_out`, `toks_in_cached`,
`images_in`, `images_out`, `audio_seconds_in`, `audio_seconds_out`, `file_bytes_in`, `requests`. A unit has
at most one price (it is a key), and a unit with no entry has no known price, so a request using it is
not estimated and not sent.

**Cost formula.** For `u` units of one meter unit the cost is `ceil(u * micro_units / per_units)` in
integer arithmetic, summed over the units. A pre-call estimate uses upper bounds for `u`, so it is an
upper bound and drives the hard stop; the actual cost after the call is the same formula on the metered
units, or the provider-reported cost.

**Usage record.** One per call: `subject`, `provider`, `role`, `channel`, `envelope_id`, `decision_id`,
`reservation_id`, `outcome` (`succeeded`, `failed`, `cancelled`, `uncertain`), `error_kind`, `metering`
(all nine units, 0 when unused), `metering_basis` (`provider_reported` or `estimated`), `cost`
{`reserved_micro_units`, `actual_micro_units`, `basis`}, `latency`, `fallback_from`. `succeeded` and
`cancelled` have no error kind; `failed` and `uncertain` name one; an `uncertain` call has cost basis
`none`; the actual cost is null exactly when the basis is `none`. No prompt or response is ever stored.

**Budget.** `subject.kind` is `provider`, `tenant`, `workspace` or `principal` (each optionally narrowed
by `provider_id`) with exactly the ids the kind needs. Each budget has a `channel` (`interactive` or
`background`, so queued RAG work cannot consume the interactive budget), a currency, a `limits` pair
(`daily_micro_units`, `monthly_micro_units`, both required: there is no unlimited budget and 0 allows no
spend), `warn_at_percent` (1 to 99; the hard stop at 100 is not configurable), optional `quotas` per
meter unit (`daily`, `monthly`) and optional `rate_limits`. Windows are UTC calendar days and months.

**Budget state.** Two windows, one `daily` and one `monthly`, each with `limit`, `spent`, `reserved`,
`remaining` and a status (`ok`, `warning`, `exhausted`). A window is `exhausted` exactly when nothing
remains; the overall status is `exhausted` exactly when a window is, and `ok` only when no window is in
`warning`. `local_fallback_available` is a hint; the router applies policy. Exhaustion stops external
calls with a typed `resource_exhausted` error and chooses no alternate provider.

## 11. Provider error

`ProviderError` has a closed shape: `kind`, `code`, `retryable`, a short fixed `message`, `status`,
`retry_after_ms`, `outcome_uncertain`, `attempts`, `provider_request_id`. There is no member for a
response body, headers or the request, so a provider cannot place text or a secret into an error. `kind`
maps to exactly one platform `code`:

| `kind` | `code` | `retryable` |
| --- | --- | --- |
| `authentication_failed` | `unauthenticated` | false |
| `permission_denied` | `permission_denied` | false |
| `rate_limited` | `rate_limited` | true |
| `quota_exceeded` | `resource_exhausted` | true |
| `timeout` | `timeout` | true |
| `unavailable` | `unavailable` | true |
| `bad_request` | `schema_invalid` | false |
| `context_too_large` | `validation_failed` | false |
| `content_filtered` | `validation_failed` | false |
| `unsupported_modality` | `validation_failed` | false |
| `model_not_found` | `validation_failed` | false |
| `malformed_response` | `schema_invalid` | false |
| `connection_lost` | `uncertain_outcome` | false |
| `internal` | `internal` | false |

`retryable` follows the platform rule of `common/error`: true exactly for `unavailable`, `degraded`,
`timeout`, `rate_limited`, `resource_exhausted`; `retry_after_ms` is null on a non-retryable error.
`outcome_uncertain` is true exactly for `connection_lost` and `timeout`, where the request may have
reached the provider: the caller records an uncertain outcome and never retries something that could
duplicate a commit. An error never selects an alternate provider.

## 12. Credentials, connection status and the billing notice

`CredentialRef` carries `ref_id`, `kind` (`oauth` or `api_key`), an optional operator `label`, the
`binding` (provider, deployment, application, tenant, provider scopes), `fingerprint`, `status`
(`pending`, `active`, `expired`, `revoked`, `invalid`, `quarantined`, `reauthorization_required`),
`account` (provider-assigned safe identifiers, never an e-mail address), `times` (authorized, expires,
rotated, last validated, revoked) and `warnings`. Rules: `oauth` has no fingerprint and at least one
scope; `api_key` has a fingerprint and always warns `broader_than_oauth_scopes`; `revoked` has a revocation
time; `quarantined` names excessive permissions or an unsupported account policy; an `active` key was
validated and an `active` OAuth credential was authorised. `invalid` fails closed and never triggers an
alternate provider.

`ConnectionStatus` is the only credential-related object a host application receives. It has no
reference ID, fingerprint or scope list: the provider, the profile lifecycle, the state (`not_connected`
to `reauthorization_required`), the kind, the safe `account`, connection and expiry times, warnings,
`health`, the last error kind, the budget status and `billing_notice_acknowledged`.

`BillingNoticeAck` records who acknowledged which notice version (and the SHA-256 of the exact text
shown), when, and for which deployment (optionally one application or tenant). The two confirmations,
`consumer_subscription_is_not_api_access` and `separate_api_billing_understood`, are `const true`: an
acknowledgement that does not confirm both is not representable.

## 13. Conformance report

`ConformanceReport` has one required member per conformance requirement, 30 checks in five closed groups,
so a missing, duplicated or invented check cannot be represented and the report reads against the requirements
one to one. Each check is `{result, reason}`: `passed` or `failed` (a failure carries a fixed reason
code, a pass carries none). Only the image, file and audio checks may be `not_applicable`, and not for a
capability the report's `declared` block says the profile has. `outcome` is `passed` only if no check
failed.

| Group | Check keys | §15 bullets |
| --- | --- | --- |
| `inference` | `text_generation_and_streaming`, `bounded_context`, `structured_output`, `cancellation_and_timeout`, `usage_reporting`, `malformed_and_error_mapping` | 15.1 |
| `control` | `typed_task_proposal`, `unknown_capability_refusal`, `argument_schema_compliance`, `no_direct_adapter_invocation`, `directive_and_receipt_handling`, `confirmation_and_cancellation` | 15.2 |
| `rag` | `authorized_excerpt_handling`, `source_reference_preservation`, `no_unsupported_citations`, `prompt_injection_resistance`, `local_only_source_enforcement`, `no_path_or_cred_leakage` | 15.3 |
| `multimodal` | `image_input_output`\*, `file_input_output`\*, `audio_input_output`\*, `cancellation_and_partial_stream`, `sanitized_rich_blocks` | 15.4 |
| `operations` | `creds_never_in_responses_or_logs`, `quota_and_budget_enforcement`, `provider_disablement`, `fallback_and_outage_behavior`, `tenant_isolation`, `audit_completeness`, `terms_and_capability_profile_validation` | 15.5 |

\* may be `not_applicable` when not declared.

The report also carries `report_id`, the exact `provider` (profile, model id, model version), `suite`
(`version`, `mode` `fixtures` or `live`), `declared`, `started_at`, `completed_at` (the report date) and
`outcome`. "A provider responds to a basic chat request" is not qualification: the registry records the
report as `last_conformance` and qualification requires a passed report for the exact model version.

## 14. Decisions made here

1. **Restricted is never egress-eligible in version 1.** The plan allows restricted data to leave under
   "a separately approved compliance profile". No such profile exists, and JSON Schema cannot verify an
   approval, so `restricted` is absent from every allowlist type and cannot be placed in an envelope. A
   compliance-profile path would be a new schema version after the legal review (X-H03).
2. **Credentials and raw database content are categories, not classifications.** They never leave under
   any policy, so they have no classification a policy could allow and no sendable category.
3. **`router` is not an external role.** The `role` enum omits the decision-scorer role, so a profile cannot declare it.
4. **Provider error kind to platform code is a total fixed table** (section 11). X-10 maps provider
   responses onto it; a change to the table is a schema change.
5. **Per-part metadata is required and inline**, instead of a parallel sidecar array, so a part without
   category, classification or source cannot be built.
6. **The profile member `cred_ref` and the check names `creds_never_in_responses_or_logs` and
   `no_path_or_cred_leakage`** avoid the log redactor, which matches the substring `credential`.
7. **`price-table.prices` and `budget.quotas` are fixed-key objects** keyed by meter unit rather than
   arrays, so a unit cannot have two prices or two quotas, and the generated types are strongly typed.
8. **Windows are UTC.** Budget days and months are UTC calendar days and months; there is no time zone
   setting.
9. **Roles, `egress-disclosure`, `connection-status`, `budget-state`, `deploymentMode`** are additions
   beyond the X-02 deliverable list, defined because later tasks need typed contracts for them.
10. **Two vocabularies already exist in `schemas/rag`** (`SourceEgress` and `SourceClassification`, from
    M2-01) with the same values as `egressMode` and `data-classification`. They are left alone;
    `TestEgressVocabulariesMatchRAG` fails if they drift. X-03 can reference the `providers` definitions
    or the `rag` ones; the values are identical.

## 15. For the consumer tasks

Import generated types from `github.com/buxee/alix-ai/pkg/contracts/gen/providers`. Reference schemas by
URN; `providers` depends on `common` only, so `application-package` can reference it (X-03) without a
cycle.

| Task | Needs | URN (all `urn:alixai:schema:providers:...:v1`) | Go type |
| --- | --- | --- | --- |
| X-03 | source egress mode, classification | `shared:v1#/$defs/egressMode`, `data-classification:v1` | `EgressMode`, `DataClassification` |
| X-04 | policy, decision, disclosure, envelope, categories | `egress-policy:v1`, `egress-decision:v1`, `egress-disclosure:v1`, `request-envelope:v1`, `shared:v1#/$defs/sendCategory`, `#/$defs/blockedCategory`, `#/$defs/egressLevel`, `#/$defs/regionCode` | `EgressPolicy`, `EgressDecision`, `EgressDisclosure`, `RequestEnvelope`, `SendCategory`, `BlockedCategory`, `EgressLevel`, `RegionCode` |
| X-05 | profile, report, acknowledgement, lifecycle | `provider-profile:v1`, `conformance-report:v1`, `billing-notice-ack:v1`, `shared:v1#/$defs/profileLifecycle` | `ProviderProfile`, `ConformanceReport`, `BillingNoticeAck`, `ProfileLifecycle` |
| X-06 | envelope, error, usage, meter units, money | `request-envelope:v1`, `provider-error:v1`, `usage-record:v1`, `shared:v1#/$defs/meterUnit`, `#/$defs/microUnits`, `#/$defs/providerErrorKind` | `RequestEnvelope`, `ProviderError`, `UsageRecord`, `MeterUnit`, `MicroUnits`, `ProviderErrorKind` |
| X-07 | credential reference, kind, host-facing status | `credential-ref:v1`, `connection-status:v1`, `shared:v1#/$defs/credentialKind` | `CredentialRef`, `ConnectionStatus`, `CredentialKind` |
| X-09 | price table, budget, budget state, usage | `price-table:v1`, `budget:v1`, `budget-state:v1`, `usage-record:v1`, `shared:v1#/$defs/budgetSubject` | `PriceTable`, `Budget`, `BudgetState`, `UsageRecord`, `BudgetSubject` |
| X-10 | profile, envelope, error mapping | `provider-profile:v1`, `request-envelope:v1`, `provider-error:v1` | as above |
| X-11 | mode, role, fallback | `shared:v1#/$defs/deploymentMode`, `#/$defs/role`, `provider-profile:v1` (`fallback`) | `DeploymentMode`, `Role`, `ProviderProfileFallback` |
| X-13, X-14 | operator and host API objects | `connection-status:v1`, `egress-disclosure:v1`, `budget-state:v1`, `billing-notice-ack:v1`, `egress-policy:v1`, `budget:v1` | as above |
| X-16 | the report the suite emits | `conformance-report:v1` | `ConformanceReport` |

Not expressible in JSON Schema, left to loaders and engines: uniqueness across records, that a fallback
exists and forms no cycle, that a conformance report's model matches the profile's, that an
acknowledgement matches the current notice text, the policy intersection down the chain, that a decision's
`reason` names its first failing check, that an envelope's `payload_bytes` is the real size, and that
`approval.approved_by` holds an authorised role.

## 16. Checking and regenerating

```text
go run ./tools/schemacheck schemas/providers      # compiles every schema, checks every example
go run ./tools/schemagen                          # regenerate Go and TypeScript, commit the output
go run ./tools/schemagen --check                  # fails on drift
go test ./tools/schemagen/... ./pkg/contracts/... # generator, authority and generated-type tests
```

`pkg/contracts/gen/providers/providers_test.go` is hand-written (like the tests in `gen/rag`). It
round-trips every valid example through the generated types, checks that unknown members and values do
not decode, pins the fail-closed properties of section 2 (restricted is not egress-eligible, credentials
are not a classification or a sendable category, `router` is not a role, a request part has no path),
keeps the egress vocabularies equal to the ones in `schemas/rag`, and fails if any property name matches
the log redactor.

One hand-written file outside this task's scope needs a one-line extension to cover the new area:
`tools/schemagen/authority_test.go` (add `providers` to `authorityAreas`). The check was run with the
extension applied and passes, and it fails when a property loses its `x-alixai-authority`.
