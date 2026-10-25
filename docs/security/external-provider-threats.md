# External provider mode: threat-model addendum

- **Task:** X-17
- **Extends:** [threat-model.md](threat-model.md) and [test-backlog.md](test-backlog.md) (ids SEC-114 to SEC-129)
- **Design:** [external provider mode](../architecture/external-provider-mode.md), ADR 0021 to 0028
- **Tests:** `tests/security/extprovider/` (`go test ./tests/security/extprovider/...`)

External mode is opt-in. When an operator enables it, data that policy permits leaves the deployment for a hosted model provider.
That adds a second place where the assistant's input can be read, a second author of text that the assistant shows, and a
spend that nobody at the keyboard sees. This document lists what an attacker, a careless operator or a faulty provider could
try against that boundary, which control stops it, which test proves the control, and what is still open. The parent threat model
(threat-model.md) still applies unchanged: the external provider is an interchangeable inference backend, never a controller, a
permission system, a RAG authority or an MCP security boundary.

## 1. Assets

| Asset | Why it matters | Where it lives |
| --- | --- | --- |
| Provider credentials (API keys, OAuth access and refresh values) | They spend the operator's money and read what the provider keeps. They never leave the credential service. | OS secret store or encrypted file (`internal/credentials`); a reference id everywhere else |
| Tenant data in a prompt (user text, history, facts, RAG excerpts, files, images, audio) | It is what the provider receives and may keep. | The request envelope after the data-egress guard (`internal/dataegress`) |
| Local-only data (credentials, raw application database content, `local_only` and unregistered RAG sources, restricted data) | It must never leave, whatever a model, a document or a user says. | Removed or withheld by the guard before any envelope exists |
| Authority (permissions, scope, validation, prepare, confirm, commit) | A provider that gained any of it would be the controller. | Policy gateway, action coordinator, host adapter (`internal/policy`, `internal/actions`) |
| Provider qualification (pinned model and version, region, terms, conformance report) | It is the operator's evidence that this provider is fit to receive this data. | `internal/providerreg` |
| Money (budgets, quotas, rate limits, usage records) | An unattended burst is a bill. | `internal/providerbudget` |
| The emergency switch and the mode and egress settings | They are how an operator stops external processing. | `internal/dataegress` (switch, policies), `internal/modes` |
| Audit metadata | It is the local record of what left, for whom, to which provider and why. It holds no content. | The tenant's audit chain (`dataegress.decision`, credential and policy events) |

## 2. Trust boundaries

| Party | Trust |
| --- | --- |
| The deterministic core (policy gateway, action coordinator, host adapter, session store, audit log, verifier) | Authoritative. Decides exposure, validates, prepares, confirms and commits. |
| The data-egress guard, the mode manager, the router and the provider registry | Deterministic gates in front of every external call. They decide on metadata and policy, never on text, and fail closed with a typed reason. |
| The credential service | Holds secrets. Releases a value only for one scope, one provider and one use, audited, and never to a log, an error or a response. |
| The operator and the tenant administrator | Decide what is enabled and approved, through capabilities checked by the service, never through a role the token asserts. |
| The external provider and everything it returns (text, tool-call proposals, Markdown, images, audio, error bodies, reported model and usage) | **Untrusted data.** Validated, then used only through the normal prepare, confirm and commit path. |
| The network between the deployment and the provider | Untrusted. Reached only through the per-provider destination allowlist, with certificate verification and no proxy. |
| User text, documents, retrieved passages | Data. They can never change policy, enable a tool, name a model or request a secret. |

The boundary that matters is the one the request crosses after the guard (G1) and the one the reply crosses before the engine
sees it (G2):

```
  user, documents, RAG ---> dialogue engine ---> egress guard ---> router gates ---> adapter ---> [ provider ]
                               ^   (policy, scope)   (G1: what may leave)   (profile, budget)         |
                               |                                                                       |
                               +---- verifier, schema, shortlist, policy, host re-check <---- G2 ------+
                                      (the reply is data: validated, never executed)
```

## 3. Threats, controls, tests

Test names are in `tests/security/extprovider/`. Every "never" in a test has a positive control in the same world (the same
request without the fault is sent, the canary does reach the one place it is meant to reach, the permitted region passes).

| Threat (plan section 14) | Assets | Boundary | Controls | Test | SEC |
| --- | --- | --- | --- | --- | --- |
| API-key theft or accidental client exposure | Credentials | credential service to adapter; deployment to client | The key lives only in the credential service (OS store or encrypted file), is resolved per call for one scope, provider and use, and is audited; it prints as a placeholder in every format; the adapter attaches it as an Authorization header only, never follows a redirect, and maps a provider error to a fixed message so an echoed key cannot reach a result, a log or an audit row; no response schema of the providers and OAuth API has a secret property, the one request that carries a key is write-only; no SDK, example or web source reads a provider key | `TestSecExtAPIKeyNeverInClientResponsesOrLogs` | SEC-114 |
| Provider-side prompt or file retention | Tenant data | guard to provider | The disclosure states the registry profile's retention, retention days, training use and residency exactly and is recorded with each send; unknown terms are shown as unknown, never as zero; with external mode off nothing is listed as leaving; the provider's own terms are pinned in the profile and a changed hash quarantines it | `TestSecExtProviderRetentionDisclosedBeforeDataLeaves` | SEC-115 |
| Cross-tenant provider request mixing | Tenant data, credentials, money | engine to router to provider | A request is released for one verified scope and found again only under that scope, once; a profile's `permitted_scopes` are checked per tenant and workspace; evidence carries its origin and a package built for another scope is `scope_violation`; source policies are per tenant; a reservation can be settled only by the scope that made it; the adapter's request holds no tenant, principal, session, workspace or deployment id; each decision is in its own tenant's audit chain | `TestSecExtCrossTenantRequestMixingImpossible` | SEC-116 |
| Provider response injection | Authority | provider to engine | The reply is data: the verifier replaces any text that fails with the core's template, so active content, new instructions and a fake receipt never reach the user; a tool call in a reply that was not asked for tools is ignored; tool arguments are mapped onto the capability's own fields and anything else is dropped; the tool set is rebuilt from the signed package and the role on every turn | `TestSecExtProviderResponseInjectionCannotExpandScope` | SEC-117 |
| Provider-side tool execution bypass | Authority | provider to host | A proposal becomes a TaskProposal and goes through the engine's schema, shortlist, policy, entity resolution and host Prepare; the host refuses what the role may not do or the rules forbid; only the client's single-use token confirms and commits, once; provider-native agents, memory, file search and server tool loops are off for every call that offers tools, and a server loop is never on; no external-mode package imports the policy gateway, the coordinator, an adapter, a process runner or a plugin loader | `TestSecExtProviderToolCallCannotBypassHostReauthorization` | SEC-118 |
| Data egress caused by an over-broad RAG source | Local-only data | guard | A source with no policy is `local_only`; the most restrictive source mode along the deployment, application, tenant, workspace and source levels wins; restricted, unknown and empty classes never leave; credentials, paths and handles are scrubbed from allowed excerpts; evidence that policy keeps out and the answer needs is the typed `egress_restricted` result and nothing is sent; text in a source cannot rewrite its own policy | `TestSecExtOverBroadSourceCannotLeakLocalOnly` | SEC-119 |
| Unexpected provider or model substitution | Qualification | registry to provider | The model, version and provider come from the registry, never from the request; each level of the egress policy allowlists providers and models; an observed version drift quarantines the profile, no other profile or the local runtime takes over, a review does not release it, and a new pin needs a passing conformance report for that exact version. The model a reply names is compared with the pinned id on every reply (the id itself, or the id with the pinned version as its snapshot suffix); another model's reply is discarded, settled as failed and quarantines the profile through `providerreg.Observe` (G13, closed by X-28). A reply that names no model is refused, not quarantined. | `TestSecExtModelSubstitutionDetected`; `TestSecReplyModelMustMatchThePinnedModel` | SEC-120, SEC-128 |
| Hidden provider fallback | Qualification, money, data | router | A fallback exists only where a profile or the role policy names it, only for transport conditions, and is recorded as a hop; an invalid credential, changed or stale terms, a hold, denied egress and an exhausted budget each end with their own typed failure and select nothing else; the registry refuses to name an alternate for those conditions whatever the caller says; the router skips a candidate only for a missing capability | `TestSecExtNoHiddenFallback` | SEC-121 |
| Cost exhaustion and quota abuse | Money | router to budget | Every call reserves an upper bound before it is sent and settles after; a burst is bounded by the budget in one transaction; a reported overspend is charged in full; rate and concurrency limits refuse with a retry-after; no budget means no call; an exhausted budget never moves a request elsewhere | `TestSecExtCostExhaustionStopsExternalCalls` | SEC-122 |
| Provider region or legal-policy change | Qualification, tenant data | registry and guard | An observed region or terms change quarantines the profile; only a release with the accepted regions (region) or a review (terms) ends it, and either needs a new qualification; the egress policy lists regions at every level and an unknown residency is denied; a changed terms hash clears the conformance report | `TestSecExtRegionChangeQuarantinesProvider` | SEC-123 |
| Network metadata leakage | Tenant data, credentials | adapter to network | Requests carry a fixed small header set and no identity of the user, tenant or machine; every destination is on the provider's exact allowlist (no wildcards), the cloud metadata and link-local addresses are never dialled, certificates are always verified at TLS 1.2 or later, the proxy environment is ignored and redirects are not followed | `TestSecExtNetworkMetadataLeavesOnlyToAllowlistedDestination` | SEC-124 |
| External audio, image and file retention | Tenant data | guard and multimodal gate to provider | Every modality is off until the provider and the tenant both enable it; the modality stays off for a provider whose retention for it is not stated; raw audio needs a separate switch at both levels and a local VAD cut, then the data-egress guard's `audio` setting for the class its session states (with a recorded decision) and a provider-budget reservation, and a local transcript keeps audio home; a file goes only by reference, only with an explicit approval, and an application-private one also needs the egress permission; produced media is stored locally and returned as a reference | `TestSecExtAudioImageFileRetentionDisclosed` | SEC-125 |

Two controls cut across the table and have their own tests.

| Control | Controls | Test | SEC |
| --- | --- | --- | --- |
| Emergency global external-provider disable switch | The switch is a persistent row read fresh on every resolution, by the router after the profile gate and by the guard before it builds the envelope; an unreadable switch counts as engaged; engaging it while the router runs the profile gate stops the request; the router reads it a last time after the budget reservation and immediately before the send, gives the reservation back and serves locally (G15, closed by X-28); only a release ends it. | `TestSecExtGlobalKillSwitchStopsAllEgress`; `TestSecKillSwitchIsReReadAfterReservation` | SEC-126, SEC-129 |
| Explicit external-mode enablement, off by default | Three mode levels and four egress levels must all opt in; a level never set is Local or disabled; only a principal with the level's write capability, and the approval capability for internal data, can widen a level; only the composition root, the adapters and the dev tools import an external adapter and the `noexternal` tag excludes it | `TestSecExtExternalModeOffByDefaultNeedsEveryLevel` | SEC-127 |

## 4. The required controls of section 14, one by one

| Required control | Where it is enforced | Proved by |
| --- | --- | --- |
| Explicit external-mode enablement | Mode manager (three levels), egress policy chain (four levels), operator-only deployment path | SEC-127 |
| Per-source and per-tenant egress policy | `internal/dataegress` policy store and chain | SEC-119, SEC-116, SEC-127 |
| Local secret storage | `internal/credentials` | SEC-114 |
| Provider and model allowlists | Each egress level, the registry's pinned profile | SEC-120 |
| mTLS or strong TLS validation | The egress transport: certificates always verified, no skip switch, TLS 1.2 or later even when configured weaker. Client-certificate (mTLS) support exists only as `egress.WithTLSConfig`; no adapter or test here uses it, because hosted providers' public APIs use server-authenticated TLS. A deployment that fronts a provider with its own mTLS gateway lists that gateway as the destination. | SEC-124 |
| Outbound destination allowlists | `internal/egress` per-provider allowlist, default deny | SEC-124 |
| Request redaction and classification | The guard: classification, blocked categories, scrubbing | SEC-119 |
| Local request and response audit metadata | `dataegress.decision` in the tenant chain, usage records, credential events; none holds content | SEC-116, SEC-114 |
| Response schema and fact verification | The dialogue verifier, the TaskProposal schema, tool mapping | SEC-117, SEC-118 |
| Cost and rate limits | `internal/providerbudget` | SEC-122 |
| Provider health and terms revalidation | `Registry.Observe`, `Check`, `Revalidate`. **Observe has no production caller yet (G13).** | SEC-123, SEC-120 |
| Emergency global external-provider disable switch | Guard, resolver, router | SEC-126, SEC-129 |
| No client-side API keys | Write-only submit, no secret property in any response, no SDK or example reads a key | SEC-114 |

## 5. Mutation checks

The security suite is only worth its controls. After the tests were written, each control below was removed from production code in
turn (a one-line change), the suite was run, and the code was restored (`git checkout`). The table records which test caught
the change.

| Mutation | Caught by |
| --- | --- |
| The router's kill-switch re-read before the profile attempt removed | Not caught alone: the guard reads the switch again. This is deliberate defence in depth, not a gap. |
| The router's kill-switch re-read after the budget reservation removed | `TestSecKillSwitchIsReReadAfterReservation`, `TestKillSwitchEngagedAfterTheReservationStopsTheSend` |
| The reply model not compared with the pinned model (or the comparison skipped for a sender) | `TestSecReplyModelMustMatchThePinnedModel`, `TestReplyModelMustMatchThePinnedModel`, `TestRealisationReplyOfAnotherModelIsDiscarded` |
| A credential refusal read as a lost connection after the send | `TestCredentialRefusalIsNeverAnUncertainOutcome` |
| The guard's kill-switch check removed | `TestSecExtProviderRetentionDisclosedBeforeDataLeaves` (the disclosure shares the check); the router and the resolver still stop the request |
| Both removed | `TestSecExtGlobalKillSwitchStopsAllEgress` |
| Scope dropped from the key that finds a released request | `TestSecExtCrossTenantRequestMixingImpossible` |
| The `region_changed` case removed from `Observe` | `TestSecExtRegionChangeQuarantinesProvider` |
| Native features allowed for a call that offers tools | `TestSecExtProviderToolCallCannotBypassHostReauthorization` |
| The source-mode ceiling of the policy chain ignored | `TestSecExtOverBroadSourceCannotLeakLocalOnly` |
| The router skips a profile for any refusal, not only a missing capability | `TestSecExtNoHiddenFallback`, `TestSecExtRegionChangeQuarantinesProvider` |
| The provider's error body copied into the adapter's error | `TestSecExtAPIKeyNeverInClientResponsesOrLogs` |
| A budget refusal ignored | `TestSecExtCostExhaustionStopsExternalCalls`, `TestSecExtNoHiddenFallback` |
| The model allowlist of the policy chain not enforced | `TestSecExtModelSubstitutionDetected` |
| A credential-service error read as a usable credential | `TestSecExtNoHiddenFallback` |
| The confirmation token not compared | `TestSecExtProviderToolCallCannotBypassHostReauthorization` |

## 6. Findings that needed code outside this task's write scope

All three are closed by X-28; the table keeps what was found and where it was fixed.

| Id | Finding | Fixed in |
| --- | --- | --- |
| G13 | The model a reply reports was never compared with the profile's pinned model, and `providerreg.Observe` had no production caller. | `modes.ExternalResult.ReplyModel` is compared by the router with `providerreg.ReplyModelMatches` on every reply; a mismatch settles the call as failed, discards the reply and calls `Observe` (`modes.Router`, `extmode` and `hybrid` senders). The terms hash and the regions are still observed by the revalidation job and the operator, not per reply. |
| G14 | `extmode.callError` mapped a credential refusal that nothing was sent for (`scope_violation`, `sent=false`) to `connection_lost` with `Sent: true`, so it became `uncertain_outcome` and the budget settled at the reserved bound. | One mapper, `modes.CallErrorOf`, used by `extmode` and `hybrid`: it reads the adapters' `provider_error_kind` and `sent` details first and maps the credential codes to `authentication_failed`; the router also reads a bare sender error that proves `sent=false`. |
| G15 | Nothing read the kill switch after the budget reservation. | `Router.attempt` reads it again right after `Reserve`, releases the reservation (or settles it as cancelled) and serves locally. |

## 7. What this suite does not prove

- **A real provider.** Every test uses the scripted fake provider or a loopback mock that plays the provider. Whether a hosted provider keeps
  data as long as its terms say, serves the model it names, or honours a deletion is a statement of the provider. The profile records what the
  provider states and the operator reviews it (X-H01, X-H03); the suite proves that nothing is sent on the strength of a statement that is
  missing, stale or changed.
- **Live conformance and legal review.** Provider accounts, budgets, terms, data-residency and legal review are human items
  (X-H01, X-H02, X-H03).
- **Network behaviour beyond the dialer.** DNS rebinding, redirect policy and address classification are tested in the egress suite (M1-09);
  here they are only exercised through the adapter.
- **The operator's own configuration.** An operator who enables a provider, approves internal data for it and lists a region has made a
  decision the guard enforces but does not second-guess. The disclosure exists so that the decision is informed.
