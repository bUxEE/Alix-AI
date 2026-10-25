# Security self-review of AlixAI 1.0

Status: **maintainer self-review, not independent.** Done by the project maintainer for 1.0 against the release
candidate and confirmed against the fixes that landed for that release. Nobody outside the project has reviewed this.
It is not a penetration test (that has its own record) and it is not a third-party audit.

Back to the [verification index](README.md). How to report a vulnerability:
[SECURITY.md](../../SECURITY.md). The vulnerability response policy is
[vulnerability-response.md](../policies/vulnerability-response.md).

## 1. Scope and method

The review covered the Go core, the release and upgrade path, backups and key files, External-mode egress and
validators, voice session limits, readiness redaction, RAG folder roots, archive handling, and the Python and React
client kits. It was a code and design walk by the maintainer: reading the paths that handle authority, signatures,
egress, quotas and client redirects, then checking that each finding was either fixed or left open with a reason.
No independent reviewer, no paid scanner beyond the CI `govulncheck` and licence jobs, and no live adversary
exercise beyond the internal penetration test already recorded.

`govulncheck` on Go 1.27.3 is clean. The eMudhra intermediate-certificate issue (GO-2026-6809) that appeared on
1.27.2 is fixed by the toolchain bump.

## 2. Findings

Severities match the project's scale (Critical, High, Medium, Low, Info). Status is **Fixed** when the change is in
the tree and the related tests pass, or **Open** when it remains. Descriptions are one line; there are no exploit
recipes beyond a short statement of impact.

### Core server

| ID | Severity | Finding | Status |
| --- | --- | --- | --- |
| H1 | High | Any `alixai.platform` key could sign a release manifest, including the online audit-checkpoint key | Fixed (purpose-bound keys and a built-in release key; see residual Open items below) |
| M1 | Medium | External-mode deployment operators and tenant admins were authorised by principal id only, so one app could act as another | Fixed |
| M2 | Medium | On Windows, backups, key files, the admin endpoint file and the data directory were not required to be owner-only | Fixed |
| L1 | Low | Voice sessions were capped only process-wide, so one tenant or principal could take every slot | Fixed |
| L2 | Low | `upgrade apply` did not require the release version (or binary hash) to match the binary running the command | Fixed |
| L3 | Low | Restoring an older backup could reactivate issuer and trust keys that had been revoked since | Fixed |
| L4a | Low | Master-key creation raced on Lstat then Rename | Fixed |
| L4b | Low | The Windows owner-only check ignored non-basic allow ACEs | Fixed |
| L4c | Low | A signed backup could name any top-level data tree | Fixed |
| Info-A1 | Info | Audit `chain_seq` gaps can reveal sibling-workspace activity volume | Accepted (by design of the shared sequence) |
| Info-A2 | Info | There is no HTTP Host allowlist; DNS rebinding reaches only `/healthz`, `/readyz` and the dedicated development route | Accepted |

### Data, egress and clients

| ID | Severity | Finding | Status |
| --- | --- | --- | --- |
| B1 | Medium | The release workflow restored a Go module/build cache, so third-party `@latest` tools in other workflows could poison a release build | Fixed (`cache: false` on every `setup-go` in the release workflow) |
| B2 | Low | GoReleaser was pinned to a floating major (`~> v2`) | Fixed (exact version) |
| B3 | Low | Egress redirect handling compared each hop with the previous hop, not the original origin, so a credential header could follow a redirect to a second host | Fixed |
| B4 | Low | The External-mode key-validation probe did not use the platform deny-list | Fixed |
| B5 | Low | External provider JSON could produce confirmation and action blocks that only the host should author | Fixed (model-authorable kinds only) |
| B6 | Low | Same voice session cap as L1 (process-wide only) | Fixed |
| B7 | Low | Readiness PEM redaction dropped only the BEGIN line, so the key body could reach a prompt | Fixed |
| B8 | Low | A `local_folder` RAG root had no sensitive-path denylist beyond volume roots | Fixed |
| B9 | Low | A host-supplied Python HTTP client could follow redirects and re-send confirmation tokens | Fixed (redirects refused per request) |
| B10 | Info | ZIP inspection ran `zip.NewReader` before checking the declared entry count | Fixed |
| B11 | Info | The React kit's Streamdown defaults allowed images and unrestricted links | Fixed (aligned with the Vue kit) |

### Residual (Open)

| ID | Severity | Finding | Status |
| --- | --- | --- | --- |
| R1 | Low | Integration-audit reports, and scorer manifests with no allowed-key list, still accept any platform key, including the audit-checkpoint key | Open |
| R2 | Low | When `serve` or `keys init` assign the audit-checkpoint purpose to an untagged key, no audit event is written (`trust add-key -purpose` does write one) | Open |

Nothing Critical is open. The High finding (H1) is fixed for release manifests; R1 is the same class of key-purpose gap on two other artifact types and is tracked as Low residual.

## 3. Breaking configuration change

Deployment operators are now written as `<app>/<principal>`, and tenant admins as `<app>/<tenant>/<principal>`. A
config that listed a bare principal id is refused. Operators upgrading External mode must rewrite
`deployment_operators` and `tenant_admins` before the service will start.

## 4. Operator-visible trust and backup changes

These landed with the trust hardening and are what operators see in day-to-day commands and docs:

- Platform keys carry a **purpose**: `release` or `audit-checkpoint`. Only a release-purpose key (or the built-in
  one) verifies a release; the audit-checkpoint key never does.
- The public release key **`alixai-release-2026`** is built into the binary; other platform keys cannot sign
  releases.
- On Windows, the data, key and backup directories must be **owner-only**. An older directory that inherits broader
  ACLs needs an `icacls` fix before upgrade or serve will accept it (see the [upgrades guide](../operations/guides/upgrades.md)).
- `backup restore` keeps newer revocations by default (`-skip-newer-revocations` when you deliberately want the
  backup's older set).
- A backup holds only the `files` and `packages` trees.
- `upgrade apply` only applies a release that matches the binary running the command (version or artifact hash).

## 5. Controls reviewed

The review walked the controls that already constrain what a model, package or remote provider can do, and checked
they still hold after the fixes above:

- **Authority stays out of the model.** Prepare, confirm and commit; capability allow-lists; typed validation of
  model output; External mode off by default and removed entirely under the `noexternal` build tag.
- **Identity and trust.** Application tokens, a trust store with revocation, purpose-bound platform keys, and a
  release public key embedded in the binary.
- **Network posture.** Loopback-first listeners, an egress allow-list plus a platform deny-list (gateway, admin,
  LocalAI and related ports), redirect origin checks, and no outbound traffic in Local mode after install.
- **Tenant and resource bounds.** Per-application and per-tenant quotas; voice sessions capped per process, per
  tenant and per principal.
- **Data boundaries.** Owner-only data and key directories (including Windows), backups limited to `files` and
  `packages`, restore that preserves newer revocations, RAG roots that refuse system, home, data and install paths,
  and readiness redaction that withholds whole PEM blocks.
- **Clients.** Python SDK requests never follow redirects; the React kit restricts links and does not render images
  or raw HTML on its own.
- **Supply chain in CI.** `govulncheck`, the Go licence allow-list, action pin checks, race and SDK suites, and
  release builds that do not restore a Go module cache.

## 6. What this is not

- Not an independent review. The same person who maintains the code wrote this report.
- Not a claim that every path was exercised under attack. The internal penetration test and the automated security
  suite cover specific threats; this review adds a maintainer pass over the paths named in section 1.
- Not a warranty. Operators who need independent assurance should commission their own review.
