# Security policy

AlixAI is a self-hosted service that sits between an application and a local language model, so a
security flaw can mean an unauthorised action, a cross-tenant data leak or a forged audit trail.
We take reports seriously and handle them privately first.

## Reporting a vulnerability

**Do not open a public issue, pull request or discussion for a suspected vulnerability.**

Report it privately through **GitHub private vulnerability reporting**: open the
[Security tab](https://github.com/bUxEE/Alix-AI/security) of https://github.com/bUxEE/Alix-AI and choose
**"Report a vulnerability"** (direct link: <https://github.com/bUxEE/Alix-AI/security/advisories/new>). You need a
GitHub account. The report is visible only to you and the maintainers until an advisory is published, and the fix can be
prepared by the maintainers in a temporary private fork.

This is the only private channel. There is no security mailbox and no encryption key; GitHub's private reporting is the
confidential path.

Please include:

1. what you found and why you think it is a vulnerability (impact in one or two sentences);
2. the affected version or commit, the deployment profile and how it was configured;
3. steps to reproduce, or a proof of concept (a minimal one is better than a complete exploit);
4. whether the issue is already public or exploited, and any disclosure deadline you are working to;
5. how you would like to be credited, or whether you prefer to stay anonymous.

## What we will do

These are targets, not contractual guarantees:

| Step | Target |
|---|---|
| Acknowledge your report | within 3 business days |
| Initial assessment (accepted, needs more information, or not a vulnerability) | within 10 business days |
| Fix or mitigation for a confirmed issue | as soon as a safe fix exists; we aim for 90 days at most |
| Public advisory | when a fix is released, coordinated with you |

We keep you informed of progress, credit you in the advisory unless you ask us not to, and will tell
you if we decide the report is not a vulnerability and why. Confirmed issues are published as a
security advisory and in the release notes, with a CVE identifier where one applies. If a fix cannot
be ready within 90 days we will agree an extension with you rather than let the deadline pass quietly.

## Scope

In scope, because they break the guarantees in the master plan (section 3):

- **Authority and permissions.** Any way a model output, a prompt or a document causes an action,
  data access or capability the caller's scope does not allow; any bypass of
  prepare-before-commit confirmation, of capability allow-lists or of validation.
- **Tenant isolation.** Cross-application, cross-tenant or cross-workspace access to sessions,
  documents, memory, indexes, jobs or audit records.
- **Identity.** Forgery, replay or confusion of application tokens, principal context, session
  bindings or prepared-action receipts.
- **Provenance and audit.** Tampering with, suppressing or forging audit records or source
  provenance.
- **Secrets.** Leakage of credentials, tokens or private data into logs, errors, model context or
  exports; failure of log redaction.
- **Network exposure.** Listeners reachable beyond the intended loopback or authenticated surface;
  unauthenticated administrative endpoints.
- **Supply chain.** Tampering with build, release, SBOM, signature or model-hash verification;
  runtime download or activation of unapproved code in the production profile.
- **Unsafe parsing and resource abuse.** Document, audio or model-file input that escapes its
  sandbox or lets a low-privileged caller exhaust the host.

Out of scope:

- Findings that need prior control of the host, the operator account or the AlixAI data directory.
- Behaviour of the host application itself, which remains the source of truth and authority for its
  own data; report that to its maintainers.
- Vulnerabilities in third-party runtimes or models (for example LocalAI or a model checkpoint)
  that AlixAI only configures. Please report them upstream. Tell us as well if AlixAI's use of them
  makes the impact worse.
- Model quality problems (wrong, rude or low-quality answers) that do not breach one of the
  guarantees above. File those as ordinary issues.
- Denial of service by an operator-authorised caller within the limits the operator configured.
- Automated scanner output without a demonstrated impact.

## Safe harbour

We will not pursue or support legal action against you for good-faith security research that:
follows this policy, stays within your own installation or test data, does not access or modify
other people's data, does not degrade a service you do not own, and gives us reasonable time to
respond before disclosure. If you are unsure whether something is covered, ask first.

## Supported versions

| Version | Security fixes |
| --- | --- |
| The latest minor of 1.x, today 1.0.x | yes |
| The previous minor of 1.x, once a newer minor ships | for 90 days after the newer minor is released |
| The previous major, once a 2.0 ships | for 12 months after the newer major's `.0` |
| 0.x pre-releases and older | no |
| `main` | development branch; fixes land here first |

A fix is released as a patch of every supported line it affects. The windows come from the
[compatibility and deprecation policy](docs/policies/compatibility.md), section 6, which is the authority if the two differ.

## How we find vulnerabilities ourselves

Every change runs `govulncheck` and a dependency licence check in CI (see
[`.github/workflows/ci.yml`](.github/workflows/ci.yml)); dependencies are pinned. The release pipeline produces an SBOM per
binary and image and a signed release manifest ([release pipeline](docs/operations/release.md), [1.0 release
record](docs/verification/release-1.0.md)). How the project responds to a finding is in the
[vulnerability response policy](docs/policies/vulnerability-response.md). AlixAI sends no telemetry, so we learn about
problems from reports like yours.
