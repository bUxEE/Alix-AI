# AlixAI 1.0 release record

Status: **release record for 1.0.0.** Written by the maintainer. It lists what was verified automatically, how the release
is signed, and what was not done. The release commit is the commit tagged `v1.0.0`.

Back to the [verification index](README.md), which has the full status table.

## 1. What 1.0 is, and what was skipped

1.0 is the first public release of AlixAI. **The planned public beta was skipped**: the repository goes from private
development straight to 1.0. The consequence is that 1.0 has had no outside users before release. The contracts frozen in
`schemas/_frozen/v1/` are stable from this release on, under the
[compatibility and deprecation policy](../policies/compatibility.md).

What runs by default: Local mode only (LocalAI and approved local models, no network use after install). **No local
controller model is qualified on any profile**, so the default stack serves the deterministic path (typed forms, the rules
router, templates), scoped RAG with the Granite Embedding 97M embedder, and the full authority layer
([ADR 0010](../decisions/0010-lite-model-selection.md)). External mode and the decision layer are optional and off.

## 2. Automated checks

Each check below is a command anyone can run on the release commit. CI runs the first group on every change.

| Check | Command | What it proves |
| --- | --- | --- |
| Go tests: unit, integration, end-to-end, security, degradation, disaster recovery and release-gate tests | `go test ./...` | the behaviour the tests describe, on the platform they run on |
| Race detector (Linux, cgo) | `go test -race ./...` | no data race in the exercised paths |
| Formatting and vet | `gofmt -l`, `go vet ./...` | |
| Module integrity | `go mod verify` | the module cache matches `go.sum` |
| TypeScript and Python SDKs | `pnpm --filter ./sdk/typescript run test`; `python -m pytest` in `sdk/python` | |
| Security suite and threat coverage | `go test ./tests/security/...`; `go run ./tests/security/cmd/coverage` | every threat id in the security backlog has a test |
| Known Go vulnerabilities | `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` | no known vulnerability reachable from the code, as of the run |
| Dependency licence allow-list | `go-licenses check ./... --allowed_licenses=Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0` | every linked Go dependency has a permitted licence |
| Contract compatibility | `go run ./tools/compat check` | no breaking change against the frozen v1 contracts |
| Release gates | `go run ./tools/gates --auto` | the automated part of every release gate |
| Documentation rules for the decision layer | `go run ./tools/doclint docs` | no document makes CLM-v0.1-8B a default or an unlabelled claim |
| Reproducible build | `go run ./tools/release reproducible-check -package ./cmd/alixai ...` | two builds of the same commit give the same binary |

### Results recorded for this release

Recorded by the maintainer on 2026-10-25 on the release candidate (Windows 11 x86-64), and re-checked on the release toolchain (**Go 1.27.3**). `govulncheck` and the licence allow-list are both clean; see [dependency-licences.md](dependency-licences.md).

| Check | Result |
| --- | --- |
| `go run ./tools/compat check` | **0 breaking**, 13 additive, 8 documentation-only changes against v1. The additive changes (the opt-in `files` and `audio` egress settings, readiness report fields) are allowed in a minor release. They are recorded into the frozen set with `go run ./tools/compat freeze` on the release commit, so the frozen v1 set matches 1.0 |
| `go run ./tools/gates --auto` | **23 gates pass, 0 fail, 10 report "human evidence missing"**; verdict printed: NOT STABLE. The tool looks for human evidence at fixed file paths; the ten items it names (real-hardware certification, soaks, signing keys and licence sign-off, the external security review, the decision-layer licence review and hardware gates, provider accounts, live conformance, the legal review, the release record) are handled as the [status table](README.md#verification-status-of-10) says: some done by the maintainer, some analysed, some not done. The verdict is not overridden |
| `go run ./tools/doclint docs` | ok |
| `go mod verify` | all modules verified |
| `go run ./tools/release reproducible-check -target linux/amd64` | reproducible: two independent builds of `./cmd/alixai` for linux/amd64 gave the same SHA-256 (Go 1.27.3) |
| Offline provider conformance | `go run ./tools/providerconf run --provider openai` and `--provider anthropic`: all 30 checks passed or not applicable against recorded fixtures, and `providerconf check` accepted both reports. The Gemini and Meta adapters' fixture conformance tests pass in `go test ./...` |
| `go test ./...` | **all 171 test packages pass.** In the first full run, 8 packages failed for environment reasons only: the clinic-booking reference application's Node dependencies were not installed (`pnpm install --ignore-workspace --offline --frozen-lockfile` in `examples/reference-apps/clinic-booking`), and three failed on time limits on a heavily loaded host (two reached the default 10-minute package timeout, one a scorer call deadline). With the dependencies installed and `-timeout 40m` they pass. The race detector, the TypeScript and Python SDK tests and the Linux runs are CI jobs and were not re-run for this record |
| `govulncheck` | **clean** on Go 1.27.3. The eMudhra intermediate-certificate issue (GO-2026-6809) reported on 1.27.2 is fixed by the toolchain bump; no called vulnerability remains in the standard library or in any imported module |
| Dependency licence allow-list | **passes.** Two modules are checked by hand because the classifier does not recognise their licence text: `modernc.org/mathutil` (BSD-3-Clause) and `github.com/segmentio/asm` (MIT-0). Inventory: [dependency-licences.md](dependency-licences.md) |

## 3. Signing

| What | How | Verify with |
| --- | --- | --- |
| Release manifest (every binary, archive and SBOM with its SHA-256, image digests, backend digests, the model registry pins) | signed **offline** with the maintainer's Ed25519 release key, key id `alixai-release-2026`; the key never reaches CI. The same public key is built into the `alixai` binary as the release-purpose key, so `alixai upgrade` accepts a release it signed with no extra setup. Platform keys of other purposes (including the audit-checkpoint key) cannot sign releases | `go run ./tools/release verify -manifest release-manifest.json -pubkey alixai-release-2026=keys/alixai-release-2026.pub -dir .` |
| Container images | signed by digest with cosign, keyless, from the release workflow's GitHub OIDC identity; the image SBOM is attached as an attestation | `cosign verify` and `cosign verify-attestation --type cyclonedx`, as in the [release pipeline](../operations/release.md#3-the-workflow) |
| SBOMs | CycloneDX 1.6, one per binary and per image | listed and hashed in the signed manifest |

**Release key:**

| Field | Value |
| --- | --- |
| Key id | `alixai-release-2026` |
| Public key (base64) | `ok5oQ98ihECPbbWYuXWyfAlVbK6kZV97vQliBsYfzOU=` |
| File in the repository | [`keys/alixai-release-2026.pub`](../../keys/alixai-release-2026.pub) |
| SHA-256 of the public key bytes | `8516e49a6d32b9673747bdebb77b8c1fa8a4531bc4cdcc586b8d2e7fc1bbb4fe` |

Trust model, plainly: the release key is held by the one maintainer. There is no second signer, no hardware-backed key
requirement, no transparency log for the manifest and no TUF-style update metadata (no timestamp, freeze or rollback
protection). The release workflow runs for the first time on the `v1.0.0` tag; if it fails, nothing is published until it
passes. Until the tag, none of the signatures above exists yet. The image SBOM describes AlixAI's own binaries, not a scan of the base image.

## 4. Not done for 1.0

Said plainly, so nobody has to infer it:

- **No independent external security review.** The security review and the internal penetration test were done by the
  maintainer ([status table](README.md#verification-status-of-10)).
- **No real-hardware certification** of any profile, and nothing run on ARM64 ([hardware.md](hardware.md)).
- **No 24-hour or 7-day soak** ([soak-analysis.md](soak-analysis.md)).
- **No qualified local controller model** on any profile.
- **No provider account and no live conformance run** for External mode ([provider-terms.md](provider-terms.md)).
- **No legal review by a lawyer** of licences, provider terms or data residency; the reviews here are the maintainer's.
- **No trademark clearance or registration** ([trademark.md](../governance/trademark.md)).
- **No vulnerability scan** of TypeScript, Python or container-image dependencies in CI. The
  [vulnerability response policy](../policies/vulnerability-response.md) asks for a manual review of those before a
  release, recorded in the release notes.

## 5. Summary of the status table

Of the areas in the [verification status table](README.md#verification-status-of-10): the automated checks, the
disaster-recovery drill and the security contact are **done**; signing, the licence reviews, the provider terms, the
External-mode review, the security self-review, the internal penetration test and the name policy are **done by the
maintainer, not independently**; the Lite and Balanced profiles and the soaks are **analysis only**; controller
qualification, ARM64, the decision-layer hardware gates, live provider conformance, the independent security review and
the public beta are **not done**.
