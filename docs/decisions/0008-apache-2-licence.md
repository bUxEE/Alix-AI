# 0008 — Apache License 2.0 for AlixAI-owned code

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-03

## Context
AlixAI-owned code, schemas, SDKs and conformance tools must be published under an OSI-approved
permissive licence (rule 15), so unrelated host applications (including closed-source ones) can
embed the SDKs and run the service. No closed component may be required for core operation
(§20.1). LocalAI, the default runtime, is MIT-licensed.

## Options
- **Apache-2.0.** Permissive, explicit patent grant and patent-retaliation clause, clear
  contribution terms (inbound equals outbound), requires preserving notices (`NOTICE`).
- **MIT.** Shorter and equally permissive, but no express patent grant; weaker for a project
  that ships SDKs embedded in third-party products.
- **MPL-2.0.** File-level copyleft with a patent grant. Acceptable for dependencies, but adds
  obligations for integrators who modify our files, which works against easy embedding.
- **AGPL-3.0 / source-available (BSL and similar).** AGPL is OSI-approved but not permissive and
  discourages embedding; BSL-style licences are not OSI-approved. Both conflict with rule 15.

## Decision
All AlixAI-owned code, schemas, SDKs and conformance tools are licensed under the Apache License
2.0. Contributions are accepted under the same licence (inbound equals outbound);
no DCO sign-off and no CLA is required. The `LICENSE` and `NOTICE` files stay at the repository
root. The trademark and project-name policy is separate from the licence and is handled by the
governance task and the human trademark decision (§25.10). Dependency licences are checked in CI
against an allowlist (Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause, ISC, MPL-2.0). Model licences
and redistribution terms are recorded per model in the registry and are not covered by this ADR
(§8.4, §16.3).

## Consequences
- Easier: embedding in any host application; patent clarity for adopters.
- Easier: dependency policy is a small allowlist that CI can enforce.
- Harder: copyleft-only dependencies (GPL, AGPL, LGPL for statically linked code) are not allowed
  in the core without a new ADR. Every release must carry the notices of its dependencies.
- Forbidden: a required component that is closed-source, source-available only or under a licence
  outside the allowlist.

## Reversal plan
Effectively one-way. Without a contributor licence agreement, relicensing code that others have
contributed needs every contributor's consent, and releases already published stay Apache-2.0
forever. The only cheap window to change the licence is before the first external contribution.
Superseding this ADR is therefore limited to adding a dual-licence option for new code, which
requires the same consent for existing contributions.
