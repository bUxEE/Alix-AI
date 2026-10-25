# Decision layer: threat-model addendum

- **Task:** D-12
- **Extends:** [threat-model.md](threat-model.md) and [test-backlog.md](test-backlog.md) (ids SEC-098 to SEC-113)
- **Design:** `docs/architecture/decision-layer.md` section 9, ADR 0029
- **Tests:** `tests/security/decision/` (`go test ./tests/security/decision/...`)

The decision layer is optional, bounded, non-generative and non-authoritative. A scorer (CLM-v0.1-8B, a smaller scorer, or the
scripted fake of the tests) receives a bounded request over candidates that deterministic code already authorised, and returns
scores. It never authorises, validates, prepares, confirms, commits, sends, deletes, discloses or changes anything, never lowers
risk, never decides who is authorised and never writes prose. This document lists what an attacker, or a fault, could try
against that boundary, which control stops it, which test proves the control, and what is still open.

## 1. What is trusted, and what is not

| Party | Trust |
| --- | --- |
| The deterministic side (policy gateway, application adapter, package, session, audit log) | Authoritative. Decides exposure, resolves every selected id, prepares, confirms and commits. |
| The candidate guard and the acceptance gates of `internal/decision` | Deterministic code that verifies the scorer. Fail closed with a typed fallback reason. |
| The scorer, its runtime, its weights and its answer | Untrusted. Every field of an answer is checked against the request and the operator's policy. |
| User text, documents, retrieved passages, slot values | Data. They may be read by the scorer only where listed in section 3, and never carry instructions. |
| The sidecar network path | Loopback or a verified TLS endpoint, with a required service key (D-06, D-14). |

## 2. Threats, controls, tests

Test names are in `tests/security/decision/`; a "control" in a test is a case that shows the check could have seen the thing it
says never happens (the same request without the fault is accepted, the canary does reach the user path, the planted source is
flagged).

| Threat | Control | Test | SEC |
| --- | --- | --- | --- |
| A scorer revision is substituted, altered, truncated or padded, or an unsigned or forged manifest is accepted | `lifecycle.VerifyArtifacts` checks size and SHA-256 of every file against the manifest before anything loads, refuses unsafe or duplicate names and an endless file, and `Update` loads nothing on a mismatch; the engine compares every result with the manifest and the operator's pins (`model_mismatch`); the signature recipe of the schema (ed25519 over the canonical manifest without its `signature`) is tamper evident | `TestSecDecisionManifestSignatureAndRevisionHashVerified` | SEC-098 |
| A revision is activated although its manifest signature was never verified, is expired or was altered after signing | `Supervisor.Update` verifies the manifest signature first, through `lifecycle.ManifestVerifier` over `trust.Verifier`, and believes no hash of the manifest before that: an unsigned or non-ed25519 manifest, a missing or inverted validity window, an expired or future-dated signature, an unregistered, revoked or retired key, a key outside the allowlist, a trust store that cannot answer, and any field changed after signing (the signed bytes are the canonical manifest without its `signature`, together with `signed_at` and `expires_at`, so extending an expired window breaks the signature) are rejected with the reason `signature` before a file is read or a byte reserved; with no verifier configured `Update` trusts nothing (section 4, G11) | `TestSecScorerManifestSignatureIsVerified`, `TestUpdateRefusesAnUnsignedOrExpiredManifest` | SEC-099 |
| Another tenant's, application's, principal's or session's candidate, label, cached answer or audit event reaches a scorer or a user | The guard asks the resolver about every candidate (`cross_tenant`) before the scorer; the request must be bound to the verified scope (`tenant_mismatch`); the cache key holds application, tenant, workspace, principal and grants and is purged per tenant; an advice resolves only under its own scope; events go to the tenant's chain; metric labels have no tenant | `TestSecDecisionNoCrossTenantCandidateLeakage` | SEC-100 |
| Oversized or malformed input exhausts the scorer or is truncated and forwarded | Caps on candidates, label, text, state keys and state strings, scalar-only state, supported types, schema version, set age and selections; a violation is rejected before the resolver and the scorer, never truncated | `TestSecDecisionOversizedCandidateSetRejected` | SEC-101 |
| Injection in the user's text steers the scorer | The text is one bounded scalar of the state and never a candidate or an instruction channel; whatever the scorer then says passes the gates, the re-resolution, the policy gateway, prepare and confirm; the result equals the run without the layer | `TestSecDecisionInjectionInUserTextCannotChangeAuthority` | SEC-102 |
| Injection in documents, retrieved passages or collected values reaches the scorer | `dialogue.DecisionRequest` has no document or evidence field; the layer sends only the utterance, the language, capability titles from the signed package and state flags; retrieval and the controller are a separate path | `TestSecDecisionInjectionInDocumentOrRAGTextNeverReachesScorer` | SEC-103 |
| A selected id is used unresolved, or an invented, foreign or revoked id is accepted | `Engine.Resolve` is the only way to a `Resolution`; it re-asks the resolver for every id at use time, also after a cache hit, and refuses with `policy_conflict` or `tenant_mismatch`; an id outside the set is `malformed_output` | `TestSecDecisionSelectedIDAlwaysReresolved` | SEC-104 |
| The scorer path gains database, file-writing, network, exec or commit access | Import guard (go/parser) over `internal/decision`, `adapters/decision` and `internal/dialogue/decision`, a call walk for file writes, a transitive check of the adapters, and a check that the provider contract passes a scope and a bounded request, never a handle | `TestSecDecisionScorerPackageCannotImportDBFilesystemNetworkOrCommit` | SEC-105 |
| Timeout, crash, malformed or mismatched answer, low confidence, missing calibration or out-of-distribution input fails open | Every gate returns a typed fallback and no selection; the dialogue layer then serves exactly the controller-only outcome | `TestSecDecisionFailClosedOnTimeoutCrashMismatchAndLowConfidence` | SEC-106 |
| The scorer lowers a risk level or confirms an action | The scorer's answer carries no risk or confirmation; a scorer `yes` is a routing hint, the engine takes the transition itself, and a commit still needs the client's own token | `TestSecDecisionScorerCannotLowerRiskOrBypassConfirmation` | SEC-107 |
| The scorer discloses a source, writes prose or authorises a user | The request and result schemas are closed (`additionalProperties: false`) and hold no prose, citation, permission, role or principal; the advice holds ids, scores and codes; exposure is decided by the policy gateway before the scorer sees a candidate | `TestSecDecisionScorerCannotDiscloseASourceOrAuthorizeAUser` | SEC-108 |
| An invocation, acceptance, fallback or final action is not audited, or the audit leaks content | Four registered tenant-chain events with ids, counts and codes only; the invocation is recorded before the scorer is called; an acceptance or a final action that cannot be recorded is not used | `TestSecDecisionAuditEventsForInvocationAcceptanceFallbackAndFinalAction` | SEC-109 |
| A scorer panic leaves an invocation without a terminal event | `Engine.Score` recovers a panicking scorer around the provider call only, records `decision.fallback` (reason `unavailable`, `invoked` true) after the `decision.invoked`, returns a fallback advice, does not cache the crash and never puts the panic value in the audit log; `Supervisor.ScoreDecision` gives the in-flight slot back and marks the runtime dead before the panic travels on (section 4, G12) | `TestSecPanickingScorerLeavesATerminalAuditEvent`, `TestScorePanicIsRecoveredAndAudited` | SEC-113 |
| A failed or interrupted update replaces the serving revision, or a rollback does not restore it | Staged update: verify, admit a second copy through the resource gate (no eviction), load, probe, gate, switch, probation with the previous instance resident, rollback without reload; a rolled back revision's cached answers are not reused because the cache key holds the revision | `TestSecDecisionUpdateRollbackRestoresPreviousRevision` | SEC-110 |
| The pickled CLM checkpoint or its wheel runs attacker code | Exact commit and sha256 pins checked against the model registry before the service starts, read-only mounts, read-only root, no capabilities, no download by the service, no profile default; the weights-only loader of the service is unverified (section 5) | `TestSecDecisionCLMCheckpointIsPickleGuardedByPinsAndReadOnlyMounts` | SEC-111 |
| A licence or redistribution term blocks the way the models ship, or an artefact's supply chain is unreviewed | Human review (D-H01) | `docs/evidence/d-licence-review.md` | SEC-112 |

## 3. What the scorer is sent

By decision type, the layer in `internal/dialogue/decision` sends:

- capability routing: the user's utterance (at most the configured bytes, never truncated), the language, and the candidates
  (capability id and the title from the signed package, plus the package hash as data version);
- dialogue act: the utterance (short), the language, flags (a goal exists, a question is pending, an action is prepared, how many
  choices are offered) and the candidate acts (`act:help`, `act:cancel`, `act:confirm`, `act:reject`, `act:task`).

It sends no retrieved text, no document, no collected slot value, no identity, role or permission, and no secret. The utterance is
user text and is the one place an injection can enter; the controls in section 2 are about what the answer can do, not about
hiding the text from the scorer. External providers never receive decision requests (decision-layer.md section 8).

## 4. Findings of the D-12 suite and how they were closed

| Id | Finding | Resolution (D-16) |
| --- | --- | --- |
| G11 | `ScorerManifest.Signature` is required by the schema and described as verified before use, but nothing verified it. `Update` verified file hashes taken from the same unsigned manifest, so a forged manifest with matching hashes would load. The signature object has no validity window, while `trust.Verifier` requires one. | **Fixed for `Update`.** `lifecycle.ManifestVerifier` (`internal/decision/lifecycle/manifestsig.go`) checks the signature through `trust.Verifier` under the platform application before `VerifyArtifacts`; `lifecycle.Deps.Verifier` is optional and `Update` without one trusts nothing. The window is carried beside the manifest (`Revision.SignatureWindow`) and bound into the signed bytes (`lifecycle.ManifestSigningPayload`), because the schema's signature object still has no `signed_at` and `expires_at` members; adding them is a schema follow-up outside milestone D's write scope. The first load of the configured revision and the adapters' empty-signature manifests are not changed: a composition root calls `ManifestVerifier.Verify` for them. |
| G12 | A panicking scorer escaped `Engine.Score`. The dialogue engine recovered it and the turn fell back to `unavailable`, but the decision trail held `decision.invoked` and no terminal event. | **Fixed.** A deferred recover around the provider call in `Engine.Score` (see section 2, SEC-113). |

The import guard (SEC-105) now exempts exactly one more edge towards a store: `internal/decision/lifecycle` to `internal/trust`, which
the verifier needs for `trust.Verifier` and which it uses only through a `trust.KeyLookup` it is handed. Scoring does not touch it.

## 5. Supply chain and licence

- **Pins.** Qwen3-8B (twelve files), the CLM head and the contrastive-lm wheel are pinned by commit and SHA-256 in
  `deploy/container/clm/config/pins.txt`; the entrypoint compares them with the registry entry and re-hashes every file at every
  start; the model directory must hold exactly the pinned files; there is no pip, no index and no download in the running service.
  The registry entries are hash-pinned candidates and no profile defaults to them.
- **Pickle.** `CLM_v0.1-8B.pt` is a PyTorch checkpoint, which is a pickle container: loading it can execute code. The registry notes
  that the use of a weights-only loader by the contrastive-lm service is unverified and assigns the review to this task. This task
  could not verify it: the wheel is not in the repository and the suite makes no network call. **The claim is neither made nor
  denied here.** The mitigations that do not depend on it are tested (SEC-111): the file is fetched at an exact commit, hashed
  before the service starts, mounted read-only on a read-only root with no capabilities, and never replaced at run time. What they
  do not cover is a compromised publisher at the pinned commit; only a review of the wheel's loader and of the head would. A human
  records that review with the licence review (D-H01).
- **Licence.** Licence and redistribution review of CLM-v0.1-8B, the Qwen3-8B encoder, vLLM, any sub-1B scorer and any
  quantisation is the human item D-H01 (SEC-112, `docs/evidence/d-licence-review.md`). A vendor statement of Apache-2.0 is not a
  clearance, and this document does not assert one.
- **Not done anywhere yet.** Image signature verification, an SBOM and a vulnerability scan of the sidecar image
  (`deploy/container/clm/hardening.md` section 4); release signing belongs to M5-03.

## 6. Mutation checks

The suite was run against deliberate faults in the production code (each applied, the suite run, the file restored; nothing of
`internal/` or `adapters/` is changed by this task). Every fault below made at least one test fail (the four rows ending in D-16 are added by that task, which did change `internal/decision`).

| Fault | Failing test |
| --- | --- |
| candidate guard stops asking the resolver about exposure | SEC-100, SEC-108, SEC-109 |
| request not bound to the caller's tenant, application and session | SEC-100 |
| selected id no longer checked against the set | SEC-100, SEC-102, SEC-104 |
| selection used without re-resolution | SEC-102, SEC-104, SEC-108, SEC-109 |
| candidate cap removed | SEC-101 |
| confidence floor removed | SEC-106, SEC-109 |
| model, revision and contract echo no longer compared | SEC-106 |
| cache key without tenant, or without principal | SEC-100 (and SEC-104, SEC-109 for tenant) |
| invocation not required to be recorded before the call; acceptance used when it cannot be recorded | SEC-109 |
| artifact verification skipped, hash comparison skipped, read not bounded to the declared size | SEC-098, SEC-110 |
| rollback keeps the bad revision, or reloads instead of resuming; failed probe or gate ignored; staged identity unchecked | SEC-110 |
| dialogue resolver exposes everything, or does not re-ask the gateway on `Resolve` | SEC-102, SEC-108 |
| scorer call without a deadline (the test run is cut by its timeout), a late answer accepted | SEC-106 |
| state caps removed, nested state allowed, set age unchecked | SEC-101 |
| final action that cannot be recorded no longer withholds the selection | SEC-109 |
| tenant echo of the result unchecked | SEC-100 |
| calibration gate removed, distribution status unchecked | SEC-106 |
| a file write added to the dialogue layer, a commit-path import added to the engine | SEC-105 |
| `Update` skips the signature check, or `Verify` skips `trust.Verifier`, or the unsigned test is dropped, or the window is left out of the signed bytes | SEC-099 (both tests; D-16) |
| the zero or inverted validity window no longer checked in `Verify` (D-16) | `TestUpdateRefusesAnUnsignedOrExpiredManifest` only: the security test still rejects it one layer later, in `ManifestSigningPayload` |
| the deferred recover around the provider call removed (D-16) | SEC-113 (both tests) |
| the supervisor no longer gives back the in-flight slot of a panicking instance (D-16) | `TestScorePanicReleasesTheInflightSlot` |

One fault survived and is an equivalent mutant: dropping only the size comparison in `VerifyArtifacts`, because the hash is taken
over at most size plus one bytes and so already differs for any truncated or padded file.

## 7. Residual risk

- The utterance reaches the scorer by design. A scorer that is itself compromised can say anything; the gates and the
  re-resolution bound what that can do to a routing hint, and nothing it says can commit.
- The cache is keyed by scope and input, and a hit is re-resolved, but a hit does skip the scorer's own runtime checks for that
  request. Purge per tenant exists (`Engine.PurgeTenant`).
- `internal/decision` reaches the audit store through the audit appender it is given, by design; the engine cannot write anything
  else. `internal/dialogue/decision` links the dialogue package that owns the commit path, and holds no handle to it: the
  import guard and the dialogue-world tests (zero Commit calls from a scorer answer) are the control.
- The real CLM sidecar was not run: every sidecar test uses a loopback stand-in built from the published API examples.
  Observed behaviour of the real service under attack is for D-08 and the pentest (M5-12).
