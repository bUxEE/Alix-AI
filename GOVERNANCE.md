# Governance

AlixAI is a small, security-sensitive open-source project. Its governance is deliberately light:
a few named maintainers, decisions written down as architecture decision records (ADRs), and a
release process with a named owner. This document says who decides what and how. It will grow with
the project; changes to it follow the same ADR process it describes.

## Principles

- **Decisions are written down.** Anything material is an ADR in [`docs/decisions/`](docs/decisions/),
  reviewed in public, so a future contributor can see what was decided, why and how to reverse it.
- **The non-negotiable rules come first.** The project's non-negotiable rules (self-hosted, local inference
  by default, no mandatory telemetry, deterministic authority, tenant isolation, fail closed, open
  source, efficiency as a permanent objective) bind every decision. No maintainer, vote or deadline
  overrides them except through the procedure under [Changing the rules](#changing-the-rules).
- **Open by default.** Issues, roadmap, design discussion, release notes and ADRs are public.
  Vulnerabilities are the exception and follow [SECURITY.md](SECURITY.md).
- **Licence and name are separate.** The code is Apache-2.0 for everyone. The project name and
  logo are governed by a separate trademark policy (see [Trademark](#trademark)).

## Roles

### Contributors

Anyone who files an issue, reviews, improves documentation or submits a change. Contributors follow
[CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md). Contributions are
licensed under Apache-2.0 on inbound = outbound terms, with no CLA and no DCO requirement.

### Maintainers

Maintainers review and merge changes, triage the tracker, accept ADRs, enforce the Code of Conduct
and keep the project aligned with its non-negotiable rules.

| Maintainer | Handle | Areas |
|---|---|---|
| Alberto Miconi (project owner) | [@bUxEE](https://github.com/bUxEE) | all |

The project owner, Alberto Miconi (@bUxEE), is the sole maintainer until others are added.

**Becoming a maintainer.** A contributor with a sustained record of good changes and reviews, and a
demonstrated understanding of the authority, isolation and efficiency rules, may be nominated by any
maintainer. The nomination is a public pull request against this file and needs the approval of
every current maintainer. New maintainers get merge rights only after their first review has been
countersigned by an existing maintainer.

**Stepping down and removal.** A maintainer can step down at any time. A maintainer who is inactive
for twelve months is moved to emeritus status after one public notice period of 30 days. Removal for
conduct is handled under the Code of Conduct. Merge rights and release keys are revoked promptly on
departure.

### Release managers

A release manager owns one release from branch cut to publication. A maintainer appoints the release
manager for each release; the role may be held by the same person repeatedly. The release manager:

- decides, with the maintainers, what goes into the release and writes the release notes;
- applies semantic versioning: public contracts (`pkg/contracts`, `schemas/`, the OpenAPI spec,
  SDKs, CLI and configuration surface) only break in a major version, and every deprecation is
  announced a release ahead;
- ensures the release passes the full gate: CI green on every platform, `govulncheck` and the
  licence check clean, conformance and security suites passing, documented known issues;
- produces the supply-chain artifacts: reproducible build, SBOM, signed
  release artifacts and containers, verified model and backend hashes and recorded model licences;
- coordinates any security fix release with the reporter under [SECURITY.md](SECURITY.md);
- never publishes a certified claim for a profile that was only emulated.

Custody of release signing keys is a project-owner decision (a human-gated task before the public
beta) and is recorded in an ADR; until then, no automation signs a release.

## Decision process

### Everyday changes

Bug fixes, tests, documentation and contained improvements are decided in pull-request review.
One maintainer approval is enough, and the author of a change does not approve their own pull
request once there is more than one maintainer. Maintainers prefer rough consensus; disagreement
is resolved on the pull request with reasoning, not by seniority.

### Material changes need an ADR

An ADR is required for any change that is hard to reverse or that shapes other work, including:

- a new or changed public contract, schema, API or SDK surface;
- persistence, queue, indexing or migration design;
- the security, identity, authority or audit model;
- a new runtime, provider, model or backend, and any default-model change;
- a dependency with licence, cgo or footprint implications;
- deployment profiles, resource targets and container or install layout;
- any change that touches the non-negotiable rules or the licence;
- changes to this governance document.

An ADR is written from [`docs/decisions/TEMPLATE.md`](docs/decisions/TEMPLATE.md) and numbered
`NNNN-title.md`. Its status moves through `proposed`, optionally `provisional` (accepted pending a
named measurement, with the task and test that will confirm or overturn it), `accepted`, and
`superseded by NNNN`. The ADR is proposed in a pull request together with the change that needs it,
or before it when the change is large.

**Acceptance.** An ADR is accepted when at least one maintainer other than the author approves it
and no maintainer has an unresolved objection after a review window of seven days (shorter for
urgent security fixes, with the reasoning recorded in the ADR). A maintainer who objects must state
which rule, measurement or risk the ADR fails. Where maintainers cannot agree, the project owner
decides and records the dissent in the ADR. Accepted ADRs are never edited to change the decision;
a new ADR supersedes them and the old one's status is updated.

**Measurement beats opinion.** Where an ADR or a release criterion names a measurement (footprint, latency,
accuracy, safety gate), the result decides, and the efficiency review in
[`docs/efficiency/`](docs/efficiency/) records it.

### Changing the rules

The non-negotiable rules and the licence can only change through a
superseding ADR that is approved by every maintainer, is open for comment for at least 30 days, and
explains what guarantee operators lose and how they are told. Relicensing additionally follows
whatever legal process the contributors' retained copyrights require.

## Public roadmap, issues and releases

- The issue tracker is public and the place to report bugs, propose features and discuss design.
- The roadmap is the public milestone board and issue tracker on GitHub. Dates are targets; the
  [release criteria](docs/release-gates.md) and the [verification status](docs/verification/README.md)
  are what count.
- Release notes are published with every release, list contract changes and known issues, and
  state which resource profiles were emulated and which were certified.

## Security response

Vulnerability reports are handled privately by the maintainers under [SECURITY.md](SECURITY.md).
The release manager of the affected line coordinates the fix and the advisory.

## Conduct

The [Code of Conduct](CODE_OF_CONDUCT.md) applies to every project space. The maintainers are the
community leaders who enforce it. A maintainer who is the subject of a report does not take part in
handling it.

## Trademark

The name AlixAI and its logo are not licensed by Apache-2.0. The trademark and project-name policy,
including how third parties may describe compatibility, is separate from the licence and is
pending: see [docs/governance/trademark-placeholder.md](docs/governance/trademark-placeholder.md).

## Amending this document

Changes to GOVERNANCE.md follow the ADR process above and need the approval of every maintainer.
