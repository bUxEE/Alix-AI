# 0037 - Integration readiness discovery: an operator-launched, model-assisted inventory that is advisory, evidence-bound and never certifies

- **Status:** accepted
- **Date:** 2026-10-08
- **Decided in task:** the owner's request of 2026-10-08, planned as milestone M8 (M8-01 to M8-GATE)

## Context
The owner asked for a layer, launched like a skill or a mode, that uses a model to inspect the project AlixAI is being added to, identifies everything integration needs (MCP servers, APIs, models, deterministic data, documentation and so on) and reports what is already covered and what is missing. It must be the first step of every integration and must be runnable again whenever the checklist needs to be re-checked.

Two rules of the integration audit constrain how this may be built:

- "The audit itself is deterministic and does not ask an LLM to certify another system."
- "AlixAI does not silently scan arbitrary files, databases or networks."

The integration audit (M1-17, M1-18 and the later audit tasks) already answers "is this package safe to activate". It starts from a signed application package. What is missing is the step before it: a project that has no package yet, or an incomplete one, needs a map of what it has and what it lacks.

## Options
- **A. Let the model decide readiness and feed the audit.** Rejected: it breaks the §4.5 sentence, makes a model the authority, and an injected README could mark everything as covered.
- **B. Deterministic scanner only.** Safe, and cheap, but it cannot read prose documentation, cannot recognise an API described only in a wiki page or a README, and cannot suggest a mapping from the project's vocabulary to capabilities. The owner asked for model help explicitly.
- **C. Deterministic scanner plus a model that only proposes, with every finding bound to evidence a deterministic verifier re-reads, and a report that is advisory and never certifies. Taken.**

## Decision
Option C.

1. **Positioning.** Readiness discovery is a separate, earlier, advisory step. It never certifies, never signs, never activates and never changes a package. The audit stays deterministic and still decides activation (§4.5 is unchanged and still true). §9.4 gains the readiness report as the first step.
2. **Scope.** An operator launches it on a directory they name (`alixai readiness scan <dir>`). It is read-only, makes no network connection unless the operator opts in to a live probe through the egress allowlist, and never scans anything implicitly. This keeps "AlixAI does not silently scan arbitrary files" true: the scan is explicit, bounded and visible.
3. **Evidence.** Every finding carries evidence: a path relative to the scanned root, a line span and the sha256 of the file. A finding without evidence cannot be `found`. A deterministic verifier re-reads each cited span and re-hashes the file; a finding that does not confirm is demoted to `unverified` and says why.
4. **Source of a finding.** `deterministic` (a rule found it), `model_verified` (the model proposed it and the verifier confirmed the evidence) or `model_unverified` (the model proposed it, the evidence did not confirm). `model_unverified` never counts as covered.
5. **The model's role.** The model receives bounded excerpts of project files as data, labelled untrusted (§12.6), with no tools, and must answer in a closed schema. It can add findings, suggest remediation text and group evidence. It cannot mark a requirement `found` without verified evidence, cannot remove a deterministic finding, and cannot change a status that a rule decided. The model is local by default. An external model is possible only through the X data-egress guard and router, is off by default, and project content is classified `local_only` unless the operator says otherwise. With no model configured, or with the model unavailable, the scan runs the deterministic leg alone and the report says so. The Lite controller is uncertified (M0-17, M0-21, M5-09): any model used here is labelled `unqualified, advisory` in the report.
6. **Secrets.** Files that look like secrets (`.env*`, key and certificate files, credential stores) are recorded as present and never read; secret-looking values inside readable files are redacted before anything reaches the report, the model or a log.
7. **Requirements checklist.** The checklist is derived from what already exists, not invented: the audit check ids (`package.authenticity`, `package.documentation`, `package.contracts`, `package.inventory`, `package.rag_inventory`, `application.control_surface`, `runtime.inspection` and the rest), the package layout of §9 and the transports of §9.3. Each requirement states the audit check it feeds, so a green readiness report points directly at what the audit will then verify.
8. **Re-runnable.** Each scan is stored in the data directory with a content hash of the inputs, which include the version of the readiness rules (`evaluator_version`, optional in `schemas/readiness`; a report without it counts as version 1), so a report made by older rules is evaluated again even when the files are unchanged. `alixai readiness status` re-evaluates the checklist and shows met, partial, missing, unverified and contradicted items, and what changed since the last scan. MCP, OpenAPI and a native adapter are alternative transports (§9.3): once a requirement of one is met, a missing requirement of another is reported `not_applicable`, not missing (an additive status value and an optional summary count in `schemas/readiness`); with none met, all of them stay missing with the fix that says to choose one.
9. **Output.** A report in JSON (schema `schemas/readiness`) and in markdown, in English and Italian. It lists what was found and where, what is missing and how to fix it, what is contradictory, and what could not be determined.
10. **Draft scaffolding.** An optional `--scaffold` writes an unsigned draft package skeleton from the verified findings. The draft is quarantined, unsigned and unusable until the operator completes and signs it; the audit still applies in full.

## Consequences
- Readiness never replaces the audit and a green readiness report is not a pass: the report says so on its first line.
- The model's possible mistakes are bounded by the verifier: the worst case is a missing or `unverified` finding, never a false `found`.
- A new schema area `schemas/readiness` is additive and is added to the frozen v1 set with `go run ./tools/compat freeze`.
- A new milestone M8 carries the work. It depends on the audit, the package loader, the provider seam and the egress guard, and it adds no `/v1` route in the first version (CLI only), so the OpenAPI document and the three SDKs do not change.
- Host note: this PC filters loopback paths ending in `/mcp` or `/sse`; readiness tests use fixture directories, and a live `--probe` is off by default.

## Rejected for now
- A `/v1` route and SDK helpers for readiness (CLI first; a route can follow as an operator-only route like X-13).
- Letting the model write the package documentation (the scaffold carries only verified facts; prose for a package is the application owner's signed work).
