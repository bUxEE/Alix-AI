# 0019 — Public trademark and compatibility-label policy for third-party distributions

- **Status:** accepted
- **Date:** 2026-10-02 (proposed); accepted 2026-10-25 for 1.0
- **Master plan refs:** §3 (rule 15), §7.4, §16.3, §20.1, §22.2, §23, §25 item 10
- **Decided in task:** M0-16 (provisional proposal); finalised in M5-H03 (human)

## Context
The code is Apache-2.0 ([0008](0008-apache-2-licence.md)), so anyone may fork, repackage and
redistribute it, and Apache-2.0 section 6 grants no right to use the licensor's names or marks
beyond describing the origin of the work. §20.1 requires a trademark and project-name policy that
is *separate* from the licence, and §25 item 10 asks for the public trademark and compatibility-label
policy for third-party distributions.

This decision belongs to the project owner, and parts of it are legal. **Nothing here is legal
advice.** The default below is a proposal that lets the autonomous build proceed with a coherent
story and gives the owner something concrete to accept, change or replace in M5-H03.
`docs/governance/trademark-placeholder.md` (M0-01) already states what is true until then: no
certification or compatibility label may be claimed, and factual statements such as "based on
AlixAI" are fine. This ADR does not create `docs/governance/trademark.md`; M5-H03 owns it.

"Third-party distribution" means a rebuild, a repackaged container image or bundle (with or without
models), a fork, or a hosted offering built from AlixAI. It does not include an integrated host
application that only calls the service.

A vocabulary clash to settle early: the master plan uses *certified* for hardware profiles
(a profile is certified after real-hardware runs; resource-limited container runs are only ever
**emulated**, §7.4). A third party's *compatibility* claim is a different thing and must not borrow
that word.

## Options
Precedents (checked 2026-10-02): the Apache Software Foundation does not permit its marks in
product names of third-party software, allows nominative fair use and a "powered by" form, and
requires derivative works to use their own marks. The CNCF's Certified Kubernetes programme lets any
vendor use the mark after passing a published conformance suite and submitting the results by
pull request, renewed yearly.

A. **Reserve the name but state no policy and no label.** The Apache-2.0 position by default.
   Cheapest, but leaves forks free to trade on the name and users unable to tell a faithful build
   from a modified one. Weak for a product whose value is safety invariants.
B. **Strict control, Mozilla-style.** Only official builds carry the name; every modification or
   repackaging must rename; the label is closed. Strongest brand protection, but it discourages
   distro packagers and integrators, which a self-hosted, embeddable product needs.
C. **Nominative use plus a conformance-gated compatibility label** (CNCF-style, self-service
   with public evidence). Anyone may state facts; unmodified redistribution keeps the name;
   modified distributions rename but may say they are based on AlixAI; a *compatible* claim is
   earned by passing the project's own published conformance suites and publishing the report.
D. **A foundation-held mark with a paid certification programme.** Strong and formal, but needs a
   legal entity, fees and staff that a pre-1.0 project does not have. Revisit if a foundation
   exists.

Security impact: the label is a safety signal. If anyone may claim "compatible" without evidence, an
operator could believe a build keeps the prepare/confirm/commit, scope-isolation and fail-closed
invariants when it does not. Resource impact: none on the runtime; some on maintainers (review).

## Decision
**Option C**, proposed on 2026-10-02 and accepted by the owner on 2026-10-25. The policy text is
[docs/governance/trademark.md](../governance/trademark.md); where the two differ, that page wins.

1. **Facts are free.** "Built on AlixAI", "works with AlixAI", "a fork of AlixAI x.y", with no
   implied endorsement, need no permission.
2. **Unmodified redistribution** (the project's release artifacts, or a reproducible rebuild with
   identical hashes) may use the name and logo as published. Configuration-only changes within the
   documented configuration surface do not count as modification.
3. **Modified distributions** (changes to code, schemas, policy or safety defaults, or to anything
   that changes certified behaviour) must not use "AlixAI" as their product, package, image or
   domain name, and must say they are based on AlixAI and modified.
4. **"AlixAI Compatible <version>"** may be used for a specific release only if its publisher has
   passed, for that exact build, the conformance suite for the claimed role (a runtime distribution,
   an integration package or an SDK: `alixai-conformance` from M3-15 plus the security and
   degradation suites that apply), left the safety invariants unchanged (prepare before commit,
   scope isolation, fail closed, deterministic authority, mandatory provenance, no mandatory
   telemetry), and published the signed machine-readable report together with the exact models,
   quantisation, backend and configuration used. The claim names the version, expires with the next
   minor release, and can be withdrawn publicly for misrepresentation. Self-service submission
   with a public register, as in the CNCF model.
5. **"Certified" is reserved for profile results produced or witnessed by the project**, and
   resource-limited runs are always labelled **emulated**. A third party never uses "certified" for
   conformance.
6. The project does not grant the label without evidence, and the label never replaces an operator's
   own audit (§9.4).

**Evidence that was owed, and what happened at acceptance (2026-10-25):**

1. A proper trademark clearance search and a decision on registration and jurisdictions.
   A first web search on 2026-10-02 surfaced, as *leads only and with no legal conclusion*: a
   product site at `alix.ai` describing a "Smart Assistant", a USPTO registration for "ALIX" held by
   Alix, Inc. for software in the estate-planning and settlement field, and an unrelated
   manufacturing-software product called ALIX. The owner must judge whether any of these matter
   before the name is used publicly.
2. `docs/governance/trademark.md`, with the contact path for permission requests and misuse reports.
3. Confirmation that the suites gating the label exist and are stable (M3-15, M5-08) and a written
   procedure for reviewing a submission and withdrawing a claim.
4. A decision whether the compatibility label also covers hosted offerings.

Outcome at acceptance:

- Item 1: **not done.** No clearance search beyond the first web search above and no registration. The name is used
  publicly as an unregistered name, and the rename risk described under Consequences stands.
- Item 2: done. `docs/governance/trademark.md` is the policy; permission requests, compatibility claims and misuse
  reports go through issues in the public repository, and security reports through GitHub private vulnerability
  reporting (`SECURITY.md`).
- Item 3: the gating suites exist (`alixai-conformance` and the security and degradation suites). The review procedure is
  the one in the policy: a claim is an issue with its report, and the outcome or a withdrawal is recorded there. There is
  no separate register.
- Item 4: in 1.0 the label covers distributed software only; a hosted offering may state facts but not use the label.

**Overturn criteria.** Move to B if forks start causing user harm under the name; move to D if a
foundation or paid programme becomes viable; move to A only if the owner explicitly accepts the
confusion risk.

## Consequences
- Easier: packagers and distros can ship the project without asking; operators have a testable
  meaning for "compatible"; the policy matches the existing placeholder.
- Harder: someone must review conformance submissions and keep a register; the suites become part
  of the public contract, so their stability matters.
- Harder: if clearance fails, a rename is needed, and the name is embedded in the module path,
  binary and CLI, environment variables and the `urn:alixai:schema:...` schema identifiers. That cost
  grows after the stable release freezes identifiers, so the clearance decision should land
  before M6.
- Forbidden: any third-party use of "Certified" or "Compatible" without the evidence above; the
  project describing an emulated result as certified; treating the Apache-2.0 grant as narrowed by
  this policy (it is not).

## Reversal plan
A policy is a document: it can be tightened or loosened with public notice, and compatibility
labels already granted are version-bound and expire. The expensive reversal is a rename. The
mitigation is to decide clearance before the stable release, keep brand strings out of
behavioural contracts where possible, and record in the final policy which identifiers are
permanent.

## Sources (checked 2026-10-02)
- Apache-2.0 section 6 (trademarks): the repository `LICENSE`; ASF third-party marks policy: <https://www.apache.org/foundation/marks/>
- Certified Kubernetes conformance programme: <https://www.cncf.io/training/certification/software-conformance/>
- Clearance leads only: <https://alix.ai/>, <https://uspto.report/TM/97913144>
