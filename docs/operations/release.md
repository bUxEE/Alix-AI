# Release pipeline

Status: **built and tested locally, workflow not run** (task M5-03, 2026-10-03). Release criteria 9 and 10 ([release-gates.md](../release-gates.md)).
Security backlog: SEC-064 (backend or runtime substitution), SEC-090 (reproducible, signed, SBOM), SEC-091 (licences).

A release is a set of files plus one signed statement about them. The statement is the **release manifest**: it lists every binary,
archive and SBOM with its SHA-256, every container image by digest, the inference backends by digest, and every model of the registry
with the hash of the file an operator fetches and its licence and redistribution notes. Whoever holds the public key can check
the whole release against it, offline.

Everything the tool does works offline once the Go module cache is filled. It opens no network connection, sends no telemetry and runs no
program except the Go toolchain (`go env`, `go build`).

## 1. The tool

`go run ./tools/release <command>` (code in `tools/release/`). Every command below was run on the dev box; flags: `-h` on each.

| Command | What it does |
| --- | --- |
| `reproducible-check` | Builds a package twice and compares the SHA-256 of the two binaries. Exit 1 when they differ or a build fails. |
| `sbom binary` / `sbom image` | Writes a CycloneDX 1.6 SBOM of one binary, or of the service image as composed. Validated before it is written. |
| `manifest` | Writes the **unsigned** release manifest. |
| `sign` | Adds an Ed25519 signature (the `internal/trust` signer) to a manifest. Run on the machine that holds the key. |
| `verify` | Verifies the signature, and with `-dir` re-hashes the release files against the manifest. |

```bash
go run ./tools/release reproducible-check -package ./cmd/alixai -version 1.2.3 -commit <sha> -date <commit time>
go run ./tools/release sbom binary -in dist/alixai -version 1.2.3 -out alixai.cdx.json
go run ./tools/release sbom image -binary alixai=dist/alixai -binary alixai-netctl=dist/alixai-netctl \
    -reference ghcr.io/<owner>/alixai -digest sha256:<image digest> -version 1.2.3 -out alixai-image.cdx.json
go run ./tools/release manifest -version 1.2.3 -commit <sha> -artifact binary=dist/alixai -artifact sbom=alixai.cdx.json \
    -image alixai=ghcr.io/<owner>/alixai:1.2.3@sha256:<digest> -out release-manifest.json
go run ./tools/release sign   -manifest release-manifest.json -key release.key -key-id alixai-release-2026
go run ./tools/release verify -manifest release-manifest.json -pubkey alixai-release-2026=keys/alixai-release-2026.pub -dir .
```

### Reproducible builds

`reproducible-check` copies the source tree into a directory per build (dot-directories, `node_modules` and the root `bin`, `dist`, `data` are
left out; symbolic links are never followed) and builds in each copy with `-trimpath -buildvcs=false -ldflags "-s -w …"`, the flags of
`deploy/container/alixai/Dockerfile` and `.goreleaser.yaml`. The environment is rebuilt, not inherited: `CGO_ENABLED=0`, `GOPROXY=off`,
`GOFLAGS=-mod=readonly`, `GOTOOLCHAIN=local`, `GOWORK=off`, `GOENV=off` (no `go env -w` settings), a `GOCACHE` that starts empty for each build, and
the module cache of the host. With `GOPROXY=off` a build that needs the network fails, which is how "works offline" is enforced.
`-shared-cache` shares one cache between the two builds: faster, and a weaker check, because compilation is not repeated.
`-target os/arch` (repeatable) builds the other release targets; `-include` limits the copied source.

Measured: `./cmd/alixai` for windows/amd64 built twice from clean directories and empty caches gives byte-identical binaries (27,191,808 bytes,
about 1 minute for both builds, Go 1.27.0, Windows 11 dev box). The test `TestReproducibleBuild` does the same for the netctl binary, and
`TestReproducibleCheckReportsDifferingBuilds` shows the comparison can fail (a build without `-trimpath` differs between two directories).
What it proves: the same source, toolchain and flags give the same bytes in two directories. What it does not: a different Go version
gives different bytes, so a release records the toolchain and a rebuilder uses the same one (`go.mod` pins `go 1.27.3`, `GOTOOLCHAIN=local`).

### SBOM

CycloneDX **1.6** JSON, validated against the official schema (vendored in `tools/release/schemas/` from the CycloneDX specification repository,
commit `1ce97b2a7b8c`, Apache-2.0, compiled in so validation is offline). An SBOM that does not validate is never written.

- `sbom binary`: the binary with its SHA-256 and build settings, every Go module linked into it (read from the build information embedded in the
  binary, so it lists what was built, not what `go.mod` says) with its `go.sum` hash as a property, and the Go standard library.
- `sbom image`: the image as it is composed: the runtime base image by digest, the build-stage image marked `excluded`, the binaries the image carries
  with their hashes, and their modules. The base images come from the `ARG` lines of the Dockerfile, which must pin both by digest.
  **It is not a scan of the image's file system.**
- No timestamp unless `-timestamp` is given, and a serial number derived from the content: the same binary gives the same SBOM bytes.
- Dependency licences are not in the SBOM: the build information carries none. The licence allow-list of the CI `supply-chain` job gates them.

### Release manifest

`format: alixai.release-manifest.v1`; `signed` holds `kind`, `application_id` (`alixai.platform`), `version`, `commit`, `created_at`, `artifacts`,
`images`, `backends`, `models`; `signatures` are the records `internal/trust` uses for packages, over the canonical JSON of `signed`.
`kind` and `application_id` are inside the signed part, so the signature cannot be replayed as another kind of payload.

- **Artifacts**: kind `binary` (target read from the build information, not from the file name), `archive`, `sbom`, `checksums`; name, SHA-256, size.
- **Images**: the ones the release builds, and the third-party ones the stack pins (read from `deploy/container/stack/compose.yaml`,
  `deploy/container/localai/compose.yaml` and the Quadlet units `deploy/container/quadlet/*.container`: today the LocalAI image, listed once). An image
  without a `@sha256:` digest fails the manifest; a unit that builds locally (`Image=x.build`) has nothing to pin.
- **Backends**: the LocalAI backend catalogue `deploy/container/localai/config/backend-gallery.yaml`: name, version, licence, digest and mirrors.
- **Models**: every entry of `models/registry` (the registry embedded in the tool): the pinned file (repository, revision, size, SHA-256), the SPDX
  licence, its URL and the redistribution notes, the backend and the status. An entry with no pinned hash fails the manifest, as does an empty licence field.
  Weights are not shipped: the manifest says what an operator fetches and under which licence.

`manifest` and `verify` parse as strictly as `internal/trust` parses a package signature manifest: no duplicate key, float, null or unknown property,
sorted lists, exact letter case. Verification fails closed: no signature, unknown, revoked or expired key, a key registered for another application, a platform
key that is not a **release key** (section 2), or one changed byte under `signed` is an error.

Signatures expire (`-valid-for`, default 366 days; `internal/trust` accepts none without an expiry). Verifying an old release needs a fresh signature from the
key holder. Several signatures (at most 4) may be present: sign with the old and the new key during a rotation.

## 2. Keys

| Key | Where | Trusted by |
| --- | --- | --- |
| Release key, Ed25519, id `alixai-release-2026` | Generated by the maintainer (`alixai package keygen -key-id alixai-release-2026`) and held by the maintainer, never in CI or in the repository; the public half is [keys/alixai-release-2026.pub](../../keys/alixai-release-2026.pub) and is **built into the binary** (`internal/release`, `PinnedKeyID`). | The binary itself, with no setup: `alixai upgrade check` and `apply` accept it. Also `verify -pubkey ID=FILE`, and a key registered as a release key: `alixai trust add-key -app alixai.platform -key-id ID -public-key-file FILE -purpose release`. |
| TEST-ONLY key (`tools/devkeys`) | Committed on purpose; anyone can sign with it. | Nothing: `sign -dev-key` is a local run, and `verify` refuses it unless `-allow-dev-key`. A normal trust store refuses to register it, and a release signed with it is refused even when a development deployment registered it, unless the command is given `-allow-dev-key`. Both print a warning. |

**Only release keys sign releases.** Every AlixAI signing key is registered under the one application `alixai.platform`: the audit-checkpoint key that `serve` keeps online
in `<data_dir>/keys` (`alixai keys init`), integration-audit signers, release keys. Each platform key therefore has a **purpose**, set once when it is registered (`trust add-key -purpose`,
`trust list` shows it): `release` or `audit-checkpoint`, or none. A release manifest verifies only under the built-in key, a key registered with `-purpose release`, or a key given with `-pubkey`;
the checkpoint key, a key with no purpose and any other platform key are refused (`key_wrong_purpose`), so stealing the online checkpoint key does not let anyone sign a release. In the other
direction, audit checkpoints and tombstones verify only under `audit-checkpoint` keys. A key registered before purposes existed has none: give it one with the same `trust add-key` command plus
`-purpose` (it classifies the existing key instead of failing); the key `keys init` registered, and the configured checkpoint key at `serve` start, are classified as `audit-checkpoint` by themselves.
**Run:** `TestOnlyReleaseKeysSignReleases`, `TestPinnedReleaseKey` (`internal/release`), `TestCheckpointsAndTombstonesNeedTheCheckpointPurpose` (`internal/audit`).

**Revoking the built-in key.** It cannot be removed from a binary, but it is revoked like any registered key: register it, then revoke it,
`alixai trust add-key -app alixai.platform -key-id alixai-release-2026 -public-key-file keys/alixai-release-2026.pub -purpose release` and
`alixai trust revoke-key -app alixai.platform -key-id alixai-release-2026 -reason ...`. From then on no manifest it signed verifies through that trust store, under any key id, and also
not when the key is named again with `upgrade -pubkey`: the command line adds trust, it never undoes a revocation. A key id `alixai-release-2026` with any other key material is refused.
`go run ./tools/release verify` has no trust store, so it sees no revocation: it checks a signature, not whether the key is still trusted.

`sign -key FILE` refuses the TEST-ONLY key file. The key never reaches CI: the workflow has no signing key and no secret but its own `GITHUB_TOKEN`.

## 3. The workflow

`.github/workflows/release.yml`, run by a `vX.Y.Z` tag (or re-run by hand on a tag). **Syntax-checked only** (`yamlcheck`, and the action-pin check of
the repository-wide invariant tests); it has not run, and the first real run will show what needs adjusting. Every action is pinned by commit SHA.

| Job | Does |
| --- | --- |
| `prepare` | Version from the tag, commit, commit time. Refuses a non-tag ref. |
| `reproducible` | `reproducible-check` for the four release targets and for netctl. |
| `binaries` | GoReleaser builds the archives twice with `--parallelism 1` (nothing published) and the two `SHA256SUMS` must be identical; extracts the binaries; one SBOM each. Parallelism stays at 1 so the two binaries (`alixai`, `alixai-parser`) keep a stable archive member order between the two runs. |
| `image` | Builds the service and netctl images, pushes them, **signs each image by digest with cosign (keyless, GitHub OIDC)**, attaches the image SBOM as a cosign CycloneDX attestation. |
| `manifest` | Writes the **unsigned** manifest from the downloaded files and the image digests. |
| `draft` | Behind the `release` environment (configure required reviewers): creates a **draft** GitHub release holding everything. |

After the run, the key holder downloads `release-manifest.json` (and the files) from the draft, runs `sign` offline, uploads the signed file over the
unsigned one and publishes the release by hand.

Verify a released image (replace owner and repository with the real ones):

```bash
cosign verify ghcr.io/<owner>/alixai@sha256:<digest> \
  --certificate-identity-regexp '^https://github.com/<owner>/<repo>/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify-attestation --type cyclonedx ghcr.io/<owner>/alixai@sha256:<digest> <same two flags>
```

Verify the release files: download them into one directory, then
`go run ./tools/release verify -manifest release-manifest.json -pubkey alixai-release-2026=keys/alixai-release-2026.pub -dir .`. Every listed file must be there with the
listed hash, and no other file may be in the directory.

## 4. What this does not do yet

- **Update metadata (TUF-style).** Not built. The manifest is a signed statement about one release, with no timestamp, snapshot or
  threshold role, no freeze or rollback protection and no key-compromise recovery. Follow-up: layer TUF-style metadata over it when staged
  upgrades (M5-04) consume releases; `verify` is the check they should call before applying anything.
- **The workflow has never run.** The GoReleaser and cosign flags follow the pinned action versions and are unverified until the first tag.
- **The image SBOM is not a file-system scan**, and image builds are not checked for reproducibility (the binaries inside are). Follow-up: scan the pushed image
  and compare two builds with `SOURCE_DATE_EPOCH`.
- **No vulnerability scan of the SBOM** and no SLSA provenance attestation. The CI `supply-chain` job runs `govulncheck` and the licence allow-list on the source.
- **Licence sign-off is human** (M5-H02, gate 10): the manifest carries the licence record of every model, it does not judge it.
- The binary `reproducible-check` builds has not been compared with the GoReleaser one (build settings such as `-ldflags` are recorded in the binary, so they need not match); the `binaries` job compares GoReleaser to itself.

## 5. Tests

`tools/release`: `TestReproducibleBuild`, `TestSBOMValidCycloneDX`, `TestReleaseManifestSignedAndVerifiable` (SEC-064, SEC-090), `TestManifestIncludesModelLicences`
(SEC-091), and the edge cases beside them (a tampered manifest, an unregistered, revoked or expired key, key rotation, the TEST-ONLY key refused, unpinned
images, backends and models refused, a strict parser, the files check). Run `go test ./tools/release/...`; about a minute, because builds start from empty caches.
