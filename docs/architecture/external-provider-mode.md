# External provider mode (milestone X)

Written 2026-10-03 for the engineers who implement and audit milestone X. The decisions behind it are ADRs 0021 to 0028
([index](../decisions/README.md)); the threats are in [external-provider threats](../security/external-provider-threats.md).

External mode is **additive and opt-in**. The core design, the Local mode and every existing contract stay as they
are ([overview.md](overview.md)). An external provider is an interchangeable inference backend: not a controller,
not a permission system, not a RAG authority and not an MCP security boundary.

State at this date: X-02 (schemas, merged) and X-05 (`internal/providerreg`, merged) exist. Everything else named
here as "planned" was a milestone-X task that had not landed at that date; its row says so. The current state is on
the [verification status page](../verification/README.md). Nothing on this page is a
measurement; measurements are owed to the tasks named in the ADRs.

## 1. Invariants every X task relies on

Each line is a testable sentence. "Exists" names a test on `main`; "planned" names the task that adds it.

| # | Invariant | Enforced by |
| --- | --- | --- |
| I1 | **External mode is off by default.** With it off, no external package is initialised, no outbound connection is opened and no external provider is contacted: behaviour is the Local mode. Absence of a policy means disabled at every level. | planned: `TestExternalDisabledByDefault` (X-04), `TestXLocalAIRemainsDefaultAndExternalIsOffByDefault`, `TestXLocalModeNeverContactsExternalWithNetworkBlocked` (X-GATE) |
| I2 | **Local mode is unchanged.** Milestone X adds packages, schemas and optional package fields; no existing schema, contract or test changes meaning. Selecting Local, External or Hybrid never changes the host's integration contract. | planned: `TestExistingPackagesStillValid` (X-03), `TestExistingProvidersUnchanged` (X-06), `TestModeSwitchDoesNotChangeHostContract` (X-11) |
| I3 | **The model never owns authority.** Provider output (text, tool-call proposals, Markdown, HTML, images) is untrusted data: validated, then used through the normal prepare / confirm / commit path. No type or API lets a provider invoke an adapter; the request envelope has no member for agents, hosted memory, file search, remote MCP or an MCP URL. | exists: schema and `TestProviderTypesFailClosed` in `pkg/contracts/gen/providers`; planned: `TestExternalProviderCannotCallAdapterDirectly` (X-12) |
| I4 | **No hidden fallback.** Invalid credentials, stale terms, denied egress and an exhausted budget never select another provider. A fallback happens only when the profile names one, only for `unavailable`, `timeout`, `rate_limited` or `unsupported_modality`, and only to a target that passes every gate itself; the hop is recorded. | exists: `TestInvalidCredentialNeverSelectsAlternate`, `TestFallbackChainFollowedOnlyWhenPolicyNamesIt` (`internal/providerreg`); planned: X-11, X-18 |
| I5 | **Secrets never leave the credential service.** Provider keys and tokens live only in `internal/credentials` (OS secret store or encrypted file). No schema has a member that can hold one: a credential is an opaque `cred_` reference and a masked fingerprint. Nothing logs a secret; usage counters are named `toks_in` and `toks_out` so the log redactor (substring `token`) does not mangle them or miss a leak. | exists: `TestPropertyNamesAvoidLogRedactor`, `TestUnknownMembersAndValuesFailDecoding` (`pkg/contracts/gen/providers`), `TestProfileStoresCredentialReferenceNeverSecret` (`internal/providerreg`); planned: `TestSecretNeverInLogsErrorsOrJSON` (X-07) |
| I6 | **`internal/dataegress` is not `internal/egress`.** `dataegress` decides what data may leave (classes, sources, regions, envelope); `egress` (exists) decides which network destinations may be dialled. A request needs both: a data decision first, then a dial through an allowlisted destination. Neither package imports the other. | planned: X-04, X-10 (provider hosts added to the `egress` allowlist); [module map](module-map.md) rows |
| I7 | **Data classes are ordered and unknown is most restrictive.** `public < internal < confidential < restricted`. `restricted` is never egress-eligible in version 1. Credentials, raw application database content, filesystem paths, database handles, MCP URLs and local-only fields never leave. RAG sources default to `local_only`. The most restrictive of deployment, application, tenant, workspace and source wins. | schema rules exist ([provider-schemas.md](../integration/provider-schemas.md) §5 and §6); engine planned: X-04 |
| I8 | **Every gate runs before the send.** Capability and profile gate, egress guard and budget reservation all pass before any adapter is called; a request that cannot be served is rejected before its content is built. | exists: `TestImpossibleRequestRejectedBeforeSend`; planned: `TestRouterAppliesEgressGuardBeforeCall`, `TestRouterChecksBudgetBeforeCall` (X-11) |
| I9 | **A profile that changed is not trusted.** Stale review, changed terms hash, drifted model version, changed region or a failing conformance report quarantines the profile; a quarantined profile serves nothing and selects no alternate ([ADR 0028](../decisions/0028-provider-change-and-terms-policy.md)). Production qualification needs a passing conformance report for the exact model version. | exists: `TestStaleTermsQuarantineProfile`, `TestModelVersionPinned`, `TestNotProductionQualifiedWithoutConformance`, `TestBillingNoticeAckRequiredBeforeActivation` |
| I10 | **The decision scorer is never external.** The role `router` is not a possible provider role, and the router refuses to send decision-scoring requests to an external provider (ADR 0029). | exists: schema (`role` enum); planned: `TestRouterRefusesDecisionScoringRoleForExternalProviders` (X-11) |
| I11 | **Money is integer micro-units and estimates are upper bounds.** No floats. An unpriced meter unit is not estimated and not sent ([ADR 0025](../decisions/0025-cost-estimation-and-budget-semantics.md)). | schema exists; planned: X-09 |
| I12 | **A provider outage cannot stop host-application work.** With every provider failing, deterministic application APIs and local search still work; only AI action execution is disabled. | planned: `TestExtProviderOutageDoesNotBlockHostWork` (X-18) |
| I13 | **External code is removable at build time.** A `-tags noexternal` build contains no external adapter and none of the milestone-X packages ([ADR 0022](../decisions/0022-external-provider-packaging.md), module-map rule 10). | planned: `TestXNoexternalBuildHasNoExternalCode` (X-GATE) |
| I14 | **Live provider calls are human-gated.** Real keys, real money and browser consent live behind `//go:build live` and environment variables; `verify` commands use the scripted fake provider, recorded fixtures and plain-HTTP loopback mocks. | convention; X-H01 to X-H03 |

## 2. Mode matrix

The mode is a deployment and tenant policy (`deploymentMode`: `local`, `external`, `hybrid`), never a field of the
application package, so switching it cannot change the integration contract.

| | Local | External | Hybrid |
| --- | --- | --- | --- |
| Status | default, certified, privacy-preserving | opt-in, network-dependent, provider-billed | opt-in, the recommended commercial expansion |
| What leaves the deployment | nothing | the minimum request the egress policy allows | structured task state and deterministic facts by default; RAG excerpts only from `allow_excerpt` sources ([ADR 0023](../decisions/0023-hybrid-default-egress-content.md)) |
| Conversational controller (`TaskProposal`) | local model | provider | local model by default; provider only for turns the policy allows |
| Language realisation | local model or template | provider | provider from a verified directive only ([ADR 0027](../decisions/0027-external-language-realization.md)), else local or template |
| Authority: identity, scope, policy, validation, prepare / confirm / commit, audit | local and host | local and host | local and host |
| Works without WAN | yes | no: declared degraded state, policy-named local fallback, or safe refusal | local fallback keeps the turn alive |
| Hardware targets | the release criteria, including the 4 GB direction | lowers the local model need; the connected 4 GB profile is benchmarked separately (X-23), not assumed | same |
| Who may enable | n/a | an authorised operator, per level, with the billing notice acknowledged | same |

How the mode is resolved: deployment, application and tenant each state a mode; **the most restrictive wins**;
Local is the default and the answer when nothing is configured; the global kill switch forces Local everywhere.
Local is the most restrictive mode; the order of Hybrid and External is fixed and tested in X-11
(`TestMostRestrictiveModeWins`, `TestKillSwitchForcesLocal`, planned).

## 3. Component map

```text
Host application ── AlixAI SDK / API ── internal/gateway (Need/Want by type)
                                              │
        Session / policy / RAG / action control plane (unchanged, local)
                                              │ interfaces owned by the core
                         ┌────────────────────┴─────────────────────┐
                 AIRuntimeProvider roles                 dialogue seams (PromptGuard, provider selection)
                         │                                          │
     LocalAIProvider ◄───┤                         ─────── milestone X (opt-in) ───────
   (adapters/localai)    │          internal/modes ── mode resolution, router, locality report
                         │            ├─ internal/providerreg     profiles, gate, lifecycle     (X-05, merged)
                         │            ├─ internal/dataegress      what may leave                  (X-04)
                         │            ├─ internal/providerbudget  budgets, reserve/settle         (X-09)
                         │            └─ internal/credentials ◄── internal/oauth                  (X-07, X-08)
                         └─ adapters/external/<provider>   dialled through internal/egress        (X-10, X-19..X-21)
```

| Package | Task | State | Owns | Does not own |
| --- | --- | --- | --- | --- |
| `schemas/providers`, `pkg/contracts/gen/providers` | X-02 | merged | every typed contract of the milestone ([provider-schemas.md](../integration/provider-schemas.md)) | behaviour |
| `internal/apppkg` egress checks, `schemas/application-package` | X-03 | planned | how an application declares source egress and classification; `external.forbid` can only restrict | the mode |
| `internal/dataegress` | X-04 | planned | policy store, classification rules, envelope builder and redaction, RAG-excerpt filter, region check, disclosure, per-decision audit record, kill switch | network destinations, provider selection |
| `internal/providerreg` | X-05 | merged | profiles, capability gate (`Check`, `Route`), lifecycle, staleness, billing-notice acknowledgement, fallback chain data | credentials (it asks an interface), data policy, budgets |
| `internal/providers` extensions | X-06 | planned | content parts, capability discovery, tool-call **proposals**, streamed deltas, usage and cost | any provider-specific code |
| `internal/credentials` | X-07 | planned | the only store of provider secrets, `Resolver`, rotation, revocation, manual-key fallback | OAuth flows |
| `internal/oauth` | X-08 | planned | authorization-code + PKCE engine; tokens go only to `credentials` | opening a browser (the host does) |
| `internal/providerbudget` | X-09 | planned | budgets, quotas, rate limits, reservations, usage export | provider choice |
| `adapters/external/<provider>` | X-10, X-19, X-20, X-21 | planned | one provider's wire protocol, error and rate-limit mapping | policy, scope, credentials, other providers |
| `internal/modes`, `internal/modes/hybrid` | X-11, X-22 | planned | mode resolution, router, typed failure outcomes, locality report, Hybrid orchestration | HTTP, model calls |
| `internal/dialogue/extmode` | X-12 | planned | the dialogue seams in external mode, proposal validation, local-only evidence | the dialogue core |
| `internal/extmodal` | X-15 | planned | per-modality gating and media handling, all OFF by default | provider calls |
| `internal/gateway/routes_v1_providers.go` | X-13 | planned | the operator API: connect, mode, egress policy, budgets, disclosure, kill switch | provider logic |
| `tests/conformance/providers`, `tools/providerconf` | X-16 | planned | the provider conformance suite and the report that qualifies a profile | |

**Dependency rule.** Only `internal/cli/providers_external.go` (`//go:build !noexternal`, created by X-10)
imports these packages on the serve path; the core reaches them through interfaces it already owns, and a
`-tags noexternal` build contains none of them (module-map rule 10, [ADR 0022](../decisions/0022-external-provider-packaging.md)).

### `internal/dataegress` and `internal/egress`

| | `internal/dataegress` (planned, X-04) | `internal/egress` (exists) |
| --- | --- | --- |
| Question | may this **data** leave? | may this **host and port** be dialled? |
| Decides on | principal scope, provider profile, data class, source mode, region, mode, kill switch | destination allowlist per connector, resolved address safety |
| Output | allow or deny with a content-free decision record, plus the filtered request | a dialled connection or a typed denial |
| Default | external disabled; source `local_only` | deny everything |
| Used by | the router before every external call | every connector, and the provider adapters |

## 4. Request flow and who decides each step

External mode mapped onto the turn pipeline of [overview.md](overview.md) §5. "Det." is deterministic code with no
model in the decision.

| # | Step | Decides | Package | On failure |
| --- | --- | --- | --- | --- |
| 1 | User input or finalised voice transcript; identity and scope verified | Det. | `gateway`, `identity` | reject; there is no default scope |
| 2 | Goal and capability resolution | Det. | `sessions`, `capabilities`, `policy` | clarification or refusal |
| 3 | Local RAG retrieval; each source authorised before any excerpt is read into a prompt | Det. | `rag`, `policy` | `no_supported_evidence`, never an uncited answer |
| 4 | Data classification and redaction of the candidate request | Det. | `dataegress` | unknown class is `restricted`; nothing built |
| 5 | Mode and provider selection (kill switch, mode, role, profile) | Det. | `modes` | Local, or a typed outcome |
| 6 | Provider capability and profile gate (permitted scope, qualified and fresh, data class, modalities, tools, limits, active credential) | Det. | `providerreg.Check` / `Route` | typed refusal with reasons; no content built |
| 7 | Egress guard: policy chain, region, source modes; the filtered envelope and the decision record are produced | Det. | `dataegress` | `permission_denied`, data stays local |
| 8 | Budget reservation (upper-bound estimate) | Det. | `providerbudget` | `resource_exhausted`, no alternate provider |
| 9 | External model request, through an allowlisted destination, key from the credential service | **Model proposes** | adapter, `egress`, `credentials` | typed provider error; uncertain outcome never retried blindly |
| 10 | Parse and validate the structured proposal as a `TaskProposal` (schema, capability, arguments, scope, entities, risk, freshness) | Det. | `dialogue/extmode`, `contracts` | reject, deterministic clarification or fallback |
| 11 | Host reauthorisation and validation | Host app | `adapters`, `policy` | directive with the missing or invalid fields |
| 12 | Prepare, confirm when the risk level requires it, idempotent commit, `ActionReceipt` | Det., Human via host UI, Host app | `actions`, `audit` | uncertain outcome: query status before any retry |
| 13 | Response directive and source verification | Det. | `dialogue`, `verify` | template for the same directive |
| 14 | Language realisation, local or external from the verified directive | **Model verbalises** | `providers`, `modes/hybrid` | local fallback, recorded |
| 15 | Final fact, claim and citation verification; rich output becomes validated blocks | Det. | `verify`, `blocks` | deterministic template |
| 16 | Usage record settles the reservation; response goes to the host | Det. | `providerbudget`, `gateway` | reservation returns after its timeout |

Steps 5 to 8 are the design order; X-11 implements the router and may refine it. The fixed point is I8: every
gate precedes the send, and a request that cannot be served is rejected before its content is built (the registry
gate takes sizes, classes and capability needs, never content). A provider never sees steps 10 to 13: it cannot
call a tool or an adapter, and its output is checked at 10 and 15.

## 5. Feature locality

What runs where in each mode. The locality report of X-11 emits the same labels per capability: **local**,
**remote** (provider), **offline-unavailable** and **provider-dependent**, so a deployment can never look local
when it is not.

| Capability | Local | External | Hybrid |
| --- | --- | --- | --- |
| Identity, context token, scope | local | local | local |
| Policy gateway, tool exposure, risk floors | local | local | local |
| Capability shortlist (deterministic) | local | local | local |
| Decision scorer (optional, ADR 0029) | local, optional | local only | local only |
| `TaskProposal` generation (controller) | local model | remote | local by default; remote only where the policy allows |
| Schema validation, entity resolution, host reauthorisation | local, host | local, host | local, host |
| Prepare, confirm, commit, receipts, audit | local, host | local, host | local, host |
| Session state and history | local | local | local |
| RAG source access, parsing, chunking, indexing, retrieval, authorisation | local | local | local |
| Embeddings and reranking | local | local; remote only if a profile declares the `embedding` role and the source is `allow_excerpt` | local |
| RAG excerpt filter by source mode | n/a | local (`dataegress`) | local (`dataegress`) |
| Grounded answer text | local model | remote, from `allow_excerpt` excerpts | remote realisation from facts ([ADR 0027](../decisions/0027-external-language-realization.md)), else local |
| Fact, claim and citation verification | local | local | local |
| STT, TTS | local | off by default; remote only per [ADR 0024](../decisions/0024-provider-side-audio-image-file.md) | local; remote TTS of verified text only per ADR 0024 |
| Vision, image generation | local if installed | off by default; remote per ADR 0024 | off by default; remote per ADR 0024 |
| Provider credentials | none exist | local credential service | local credential service |
| Budgets, usage, egress policy, kill switch | n/a | local | local |
| Behaviour with WAN down | works | provider-dependent: degraded, policy-named local fallback, or safe refusal | local fallback |

## 6. Failure and fallback

Each failure case, with the typed outcome the code and schemas already name (`provider-error` kind and
platform code, [provider-schemas.md](../integration/provider-schemas.md) §11) and the test that will assert it.
The table is a contract: X-11 implements it and X-18 asserts each row literally.

| Condition | Required behaviour | Typed outcome | Decides | Test |
| --- | --- | --- | --- | --- |
| Provider unavailable | policy-named local fallback, queue safe work, or report unavailable; never pretend completion | `unavailable`, retryable | `modes` | `TestExtProviderUnavailable` (X-18) |
| Network lost during request | cancel or mark uncertain; never duplicate a commit | `connection_lost` becomes `uncertain_outcome`; query status before any retry | `modes`, `actions` | `TestExtNetworkLostDuringRequest`, `TestNetworkLossMarksUncertainNotDuplicatedCommit` |
| Provider timeout | typed timeout, local goal state preserved | `timeout`, outcome uncertain | `modes` | `TestExtProviderTimeout` |
| Provider rate limit | bounded retry with backoff and budget policy | `rate_limited`, `retry_after_ms` | adapter, `providerbudget` | `TestExtProviderRateLimit` |
| Malformed tool call | reject; deterministic clarification or fallback | `malformed_response`, `schema_invalid` | `dialogue/extmode` | `TestExtMalformedToolCall`, `TestMalformedToolCallRejectedWithClarification` |
| Unsupported modality | another qualified provider, or report unsupported | `unsupported_modality`, `validation_failed`; `providerreg` reasons `input_modality_unsupported` and siblings (exist) | `providerreg`, `modes` | `TestExtUnsupportedModality` |
| External egress denied | data stays local; local provider if available | `permission_denied` with a deny decision record | `dataegress` | `TestExtEgressDenied`, `TestEgressDeniedKeepsDataLocal` |
| Budget exhausted | stop external calls, show the budget state | `resource_exhausted`, budget-state detail; no alternate provider | `providerbudget` | `TestExtBudgetExhausted` |
| Terms or configuration stale | quarantine the profile pending review | `quarantined` with the reason (`stale_review`, `terms_changed`, ...; exists) | `providerreg` | `TestExtTermsStale`; exists: `TestStaleTermsQuarantineProfile` |
| Local fallback unavailable | application UI, deterministic APIs and local search keep working; AI action execution disabled | `unavailable` for AI actions only | `modes` | `TestExtLocalFallbackUnavailable`, `TestExtProviderOutageDoesNotBlockHostWork` |

Invalid credentials are not a row of this table but follow the same rule (I4): typed refusal
(`credential_invalid` in `providerreg`, `authentication_failed` from a provider), never an alternate provider.

## 7. What stays local in every mode

- Identity, application context tokens and scope; the policy gateway and its second run at prepare and commit.
- Application state, adapters and the host's reauthorisation; prepare / confirm / commit, idempotency and
  receipts; the audit log.
- The RAG corpus: original files, parsing, chunking, indexes, source versions, availability, retrieval and the
  authorisation that runs before any excerpt is read; citations and their resolution.
- Session state, history and the structured goal state.
- Fact, claim and citation verification and the deterministic response templates.
- The credential service and every provider secret; budgets, usage records, egress policy, the kill switch and
  the provider registry.
- Wake word and microphone state where Alix Box hardware is used.
- The optional decision scorer (ADR 0029).
- Raw filesystem paths, database connections, reusable application credentials and MCP URLs: no mode has a place
  to send them.

## 8. Decisions on this page

| ADR | Decision | Status | Finalised by |
| --- | --- | --- | --- |
| [0021](../decisions/0021-first-external-provider.md) | OpenAI first; manual key first | provisional | X-16, X-H02 |
| [0022](../decisions/0022-external-provider-packaging.md) | one binary, disabled by default, `noexternal` tag | provisional | X-10 |
| [0023](../decisions/0023-hybrid-default-egress-content.md) | Hybrid sends facts; excerpts only from `allow_excerpt` | provisional | X-22 |
| [0024](../decisions/0024-provider-side-audio-image-file.md) | provider-side media OFF in the first release | provisional | X-15 |
| [0025](../decisions/0025-cost-estimation-and-budget-semantics.md) | micro-units, upper-bound estimates | provisional | X-09 |
| [0026](../decisions/0026-data-residency-and-region-policy.md) | region mechanism; jurisdiction rules are the owner's | provisional | X-04, X-H03 |
| [0027](../decisions/0027-external-language-realization.md) | external realisation from verified directives only | provisional | X-22 |
| [0028](../decisions/0028-provider-change-and-terms-policy.md) | pin versions, 30-day ceiling, quarantine on change | provisional | X-05 follow-ups |

Open items found while writing this page, none resolvable in X-01: the two schema-text fixes and the re-pin
release gap of ADR 0028; the X-GATE package list should also name `internal/extmodal` and
`internal/dialogue/extmode`; and X-13 and X-12 must take their services by interface so the `noexternal` build
stays clean, and X-13 must tag its route file `!noexternal` or use `Want` only, because the gateway refuses a
partly provisioned area (ADR 0022); no task is assigned yet to register the X-13 services in the root.
