# Optional decision layer (CLM-v0.1-8B and smaller scorers)

Status: planning baseline, provisional ([ADR 0029](../decisions/0029-decision-layer-clm-8b.md)). Source of the
requirement: the project owner. Nothing in this document is a measured
result; every performance statement is a hypothesis to be tested by milestone D.

The decision layer is **optional, bounded, non-generative and non-authoritative**. AlixAI is correct with it
disabled, unavailable, below threshold, unsupported by the selected runtime or rejected by a resource gate.

## 1. What it is for

A scorer that takes **bounded decisions before the conversational controller is invoked**, to avoid unnecessary
controller calls and latency:

- capability and application-scope routing;
- confirmation, rejection, correction, cancellation and help detection;
- choosing one item from a short **authorised** candidate list;
- choosing among already-authorised application actions;
- bounded out-of-scope and prompt-injection classification;
- optional action-path, tool-path or response-path verification.

It is **not** the conversational controller, STT, TTS, embedding/retrieval model, reranker, RAG synthesiser, MCP or
application adapter, SDK, tenant control or response verifier. It never generates user-facing prose and never invents
an action, tool, file, API call or permission. It is never the authority for application facts, permissions, policy,
validation, RAG citations, tenant isolation or commits.

## 2. Roles (kept separate)

| Role | Owner | Authority |
| --- | --- | --- |
| Scope and policy engine | AlixAI core (`internal/policy`, `internal/identity`) | deterministic, final |
| **Decision scorer** (this layer) | `DecisionModelProvider` (`internal/decision`, adapters) | advisory only |
| Conversational controller | `TextGenerator` provider | proposes a typed task; realises language |
| Embedding, retrieval, reranker | RAG + providers | evidence only |
| STT / TTS | voice providers | I/O only |
| MCP / OpenAPI / native application adapter | `internal/adapters` | deterministic, host-authoritative |
| Response verifier, audit | `internal/verify`, `internal/audit` | deterministic |

## 3. Runtime authority flow

```text
application request, chat message or voice transcript
  -> tenant, application and session lookup
  -> signed integration manifest and conversation-contract verification
  -> deterministic scope, authentication, permission and freshness checks
  -> deterministic rules for obvious exact cases                      (may skip everything below)
  -> DecisionModelProvider: bounded candidate scoring, when useful     (optional, advisory)
  -> confidence, calibration, candidate-size and out-of-distribution gates
  -> deterministic application adapter / MCP tool resolver             (re-resolves every selected id)
  -> typed read or prepare result
  -> permission and confirmation policy
  -> commit only through the application adapter
  -> typed result, citations and allowed next actions
  -> conversational controller only when natural realisation, open text, RAG synthesis or clarification is needed
  -> response verifier
  -> API/SDK response and optional TTS/audio stream
```

A scorer result may *select or rank a permitted interpretation path*. It may never authorise, validate, prepare,
confirm, commit, send, delete, disclose or change anything, never lower risk or bypass confirmation, never change
permissions and never decide whether a user is authorised. The controller keeps natural multi-turn language, pronouns
and clarification, and receives deterministic facts, citations and allowed next actions from the application. The
scorer does not answer open-ended RAG questions or generate the final response.

## 4. `DecisionModelProvider` contract

Provider-neutral. No provider-specific behaviour leaks into the application contract; a future module or application
type never needs a change to this contract.

```text
scoreDecision(
  tenantId, applicationId, sessionId, contractVersion,
  boundedState, allowedNextActions, candidateSet,
  locale, deadline
) -> DecisionScoreResult
```

`DecisionScoreResult` carries: provider and model identifier; model revision and conversation-contract version;
selected candidate ids with scores and **calibrated confidence**; decision type and schema version; latency and cache
information; out-of-distribution / unsupported-shape status; trace and audit identifiers; and an explicit **fallback
reason** when no result is accepted. The result type is named and documented as *advisory* — the public API and SDK
cannot present a raw score as an authorisation or commit result.

Inputs are the minimum bounded state and the authorised candidate set only: never unrestricted application data,
undeclared files, arbitrary RAG text or uncontrolled user instructions.

The guard rejects empty, oversized, stale, duplicate, cross-tenant and unauthorised candidate sets. The provider fails
closed on timeout, malformed output, low confidence, calibration failure, model mismatch, tenant mismatch or policy
conflict. Candidate ids are re-resolved by the deterministic adapter before any tool call or commit.

Typed fallback reasons (stable strings): `disabled`, `unavailable`, `timeout`, `malformed_output`, `low_confidence`,
`calibration_failed`, `ood_unsupported_shape`, `model_mismatch`, `tenant_mismatch`, `policy_conflict`,
`candidate_set_rejected`, `resource_gate`, `unsupported_runtime`. An exact deterministic match is *not* a fallback.

## 5. Lifecycle

Health, loading, eviction, warm cache, timeout, fallback and provider replacement are explicit. The provider has a
supervised lifecycle (start, health, restart with backoff, demand-load, idle unload with a minimum warm period), a
cache keyed by scope, contract version and model revision, and a registry status
(`candidate`, `qualified-emulated`, `certified`, `rejected`, `retired`). It runs under the existing provider registry
as the `router` role and is replaceable without touching application contracts.

## 6. Deployment

- **LocalAI Decisions/SystemOne** (`POST /v1/systemone`; typed choice/score/noul questions; request body capped at
  64 KiB, at most 64 questions) *when the selected provider and backend support it*. LocalAI's documentation lists CLM
  among the models of its vllm.cpp backend; **that is not verified for our pinned LocalAI v4.10.0, and AlixAI does not
  claim LocalAI hosts CLM-8B automatically.**
- **Supervised CLM sidecar** (vLLM serving the frozen Qwen3-8B encoder plus the CLM service, on loopback or a Unix
  socket), the provisional preference.
- A native in-process provider is not planned (Python/PyTorch/vLLM stack).

The choice is made by ADR-0030 after testing. Exact CPU, ARM64, CUDA, TensorRT, container, VPS and LocalAI
compatibility gates are recorded per host; **no desktop result is proof of deployment performance.**

## 7. Profiles, benchmark and default-enable gates

| Profile | Decision layer |
| --- | --- |
| Lite / smallest host | deterministic routing first; a qualified sub-1B scorer only if useful and measurable; CLM-8B disabled unless the full workload proves it fits; no model duplication and no silent eviction of the controller, retrieval or speech services |
| Balanced machine or VPS | CLM-8B evaluated resident and demand-loaded; eviction, latency spikes and reduced concurrency measured; deterministic and smaller-scorer fallback retained |
| Higher-memory host | CLM-8B may be active if conformance, latency, quality, security, thermal and reliability gates pass; resident, demand-loaded and sidecar compared; storage, cache, update and rollback documented |

The frozen Qwen3-8B encoder makes CLM-8B a **higher-memory candidate**. The permanent rule applies: test the smallest
deterministic method, then the smallest model, and larger candidates only when measured quality, safety or capability
justifies the cost. The 4 GB direction stays active and is not claimed.

**Benchmark matrix** (same versioned en/it fixtures and signed conversation contracts for all five):

| # | Configuration |
| --- | --- |
| 1 | deterministic rules only |
| 2 | qualified sub-1B scorer |
| 3 | CLM-8B scorer |
| 4 | direct conversational-controller routing |
| 5 | scorer (CLM-8B or smaller) followed by the controller when needed |

Fixtures: exact commands, natural paraphrases, confirmations, corrections, cancellations, ambiguous entities, RAG
operations, unauthorised requests, prompt-injection strings and out-of-distribution cases.

Metrics: exact capability and candidate selection; confirmation, cancellation and correction accuracy; false in-scope
and false out-of-scope; unauthorised-action rejection; prompt-injection and untrusted-document handling; calibration
(Brier score, expected calibration error); end-to-first-response and optional end-to-first-audio latency; p50/p95/p99
decision latency; controller calls and generated tokens avoided; task completion and clarification rate; RAM peak and
reserve; cold/warm load, cache reuse and eviction; concurrent RAG, voice and application-tool workloads;
multilingual quality and regression rate. Every profile also records load time, concurrency, recovery and (where
relevant) power, and the effect on RAG, voice and application-control workloads.

**Gate.** CLM-8B is enabled by default for a profile only if it improves measured latency, capacity or safety without
reducing task success, citation correctness, multilingual quality, fail-closed behaviour or the required memory
reserve. Otherwise the smaller scorer or deterministic route stays and the rejection is recorded in the model
registry. The correct result is never an unconditional CLM-8B default.

## 8. API, UI neutrality, external providers

- The API/SDK exposes a safe **decision status** surface: provider, model, revision, mode, fallback counters and
  accepted/rejected state — without authority internals. All writes and side effects return deterministic typed
  results from the application adapter.
- AlixAI owns no product UI. Future UI kits may show understandable decision states ("fast route selected",
  "clarification required"); UI code is never an authority layer.
- External conversational providers stay optional and separate. **Decision-scoring requests are never
  routed to external providers in this release**; the decision layer stays local unless a future provider is
  explicitly qualified for the same bounded contract. External providers never bypass deterministic authority,
  signed contracts, permission checks, confirmation, citations, tenant isolation or audit.

## 9. Security and supply chain

Signed model and provider manifests; hash-verified model revisions; tenant, application and session isolation; no
cross-tenant candidate leakage; bounded input and candidate count; prompt-injection resistance for user text,
documentation and RAG content; deterministic re-resolution of every selected id; **no direct database, filesystem,
network or MCP-commit access from the scorer**; fail-closed timeout, crash, mismatch and low-confidence behaviour;
audit events for invocation, accepted result, fallback and final action; safe model update, rollback and recovery;
licence and supply-chain review of CLM, its encoder, runtime and quantisations (human item D-H01).

## 10. Reversal

`decision.enabled=false` (or a build without decision providers) returns the pipeline to deterministic routing plus
the controller; no application data, contract or public API changes. See ADR-0029 for removal criteria.
