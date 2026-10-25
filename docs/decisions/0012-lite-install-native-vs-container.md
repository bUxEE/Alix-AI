# 0012 — Native process versus container as the preferred Lite installation

- **Status:** accepted
- **Date:** 2026-10-03
- **Decided in task:** M0-16 (provisional default); finalised in M5-02 (measured on the dev box, accepted 2026-10-03)

## Context
§6 lists both container and native service packages as supported packaging, and §25 item 3 asks
which one is the *preferred* Lite installation: the one the documentation leads with and the one
certification is run against. The decision is about the shape of the install, not about whether
AlixAI may run natively: the core is a static, pure-Go binary and never requires a container
runtime ([0001](0001-go-core-service.md), [0009](0009-pure-go-sqlite-driver.md)).

Constraints that bear on it:

- LocalAI is the default runtime, behind `AIRuntimeProvider` ([0003](0003-localai-behind-runtime-provider.md)).
  Per its documentation (checked 2026-10-02) containers are the recommended install, backends are
  separate OCI images pulled on demand, and the **native binary is documented as having limited
  support**: Linux amd64/arm64 and macOS arm64 only, Windows via WSL, no Python-based backends, and
  on macOS and Linux arm64 no TTS backend. The current release is v4.10.0 (2026-09-17).
- §16.3 forbids runtime download or activation of unapproved code in production, so on-demand
  backend pulling must be replaced by pre-installed, hash-verified backends in either shape.
- §7.4 requires no swap and a measured headroom. Docker Desktop and WSL2 hosts run a VM whose
  default memory is 50% of host RAM and default swap 25% of host RAM, so a Windows or macOS container
  install needs explicit limits and `swap=0` to be certifiable at all.
- The project already plans both shapes (M5-01 container stack with Docker Compose and rootless
  Podman Quadlet; M5-02 native service packages; M5-07 supervisor for native LocalAI), so the
  question is which is *preferred*, not which exists.
- Certification evidence is produced under resource-limited compose profiles and labelled
  **emulated**, never certified, until real-hardware runs (M5-H04) exist.

## Options
A. **Everything native** (alixai plus LocalAI binary, supervised by M5-07, under systemd, a Windows
   service or launchd).
   - Lowest overhead: no container runtime, no VM on Windows/macOS. Best story for the 4 GB
     direction ([0020](0020-4gb-profile.md)).
   - But: LocalAI's native binary has the reduced backend coverage above (no Windows build, arm64
     without TTS, no Python backends), and the hardening work (separate OS users, service sandboxing,
     loopback or unix socket) is ours. Limits are kernel-enforced only where the service manager has
     cgroups: under systemd `MemoryMax`, `MemorySwapMax=0` and `CPUQuota` are cgroup limits, as in a
     container; a Windows service and a launchd daemon have no memory or CPU cap at all.
   - Security: depends on systemd unit hardening and on the supervisor; the weakest default
     isolation between AlixAI and LocalAI of the three shapes.
B. **Native alixai plus containerised LocalAI.**
   - The Go service stays a small native package; LocalAI runs in its recommended form with full
     backend coverage and cgroup limits. Two lifecycle models to document and test, and it still
     needs a container runtime, so the saving over C is only alixai's own container overhead.
   - Security: LocalAI is contained; alixai relies on host hardening.
C. **Full container stack** (alixai and LocalAI containers, pinned by digest, non-root, `cap_drop`
   ALL, read-only rootfs, loopback or shared-socket only, bind mounts, healthcheck-gated start
   order, no one-shot init containers).
   - One artefact, reproducible, identical to what the certification harness measures; limits map
     one-to-one to the lite/balanced/4g profiles; Podman rootless with Quadlet on Linux avoids a
     root daemon. Costs: a container runtime resident (dockerd/containerd or Podman's conmon),
     image pulls, and a VM on non-Linux hosts.
   - Security: strongest default separation (separate users, no shared filesystem, default-deny
     capabilities), at the price of a larger trusted base (the runtime).

Measurements: taken in M5-02, below.

## Measurements
Dev box (Windows 11, 16 logical CPUs, 32 GB, Docker Desktop 29.7.2 on WSL2, cgroup v2), idle, `inference.models: []`, 2026-10-03.
**Dev-box and emulated figures, never certified**; the container column ran under the Lite stack's own caps
(`alixai` 512 MB, no swap, 1 CPU, read-only root, `cap_drop` ALL). Same commit, same flags (`-trimpath -ldflags "-s -w"`, `CGO_ENABLED=0`).
Five runs each; the process is sampled 8 s after its first `/healthz` 200. The LocalAI container is the same in B and C, so a
stand-in that only answers `/readyz` took its place (the real image is not on the dev box); the difference between B and C is
the `alixai` service, the anchor and the relay.

| | Native, Linux amd64 (WSL2, no limit) | Native, Windows amd64 | Container (Compose, `compose.lite.yaml`) |
| --- | --- | --- | --- |
| `alixai` on disk | binary 26.6 MB (linux arm64 24.5, darwin arm64 25.0) | binary 27.0 MB | image 34.9 MB; anchor 8.4 MB, relay 8.4 MB |
| `alixai` idle RSS | **36.8-37.1 MB** (anon 13.0-13.8, file 23.3-23.9) | working set 29.3-30.1 MB, private bytes 59.9-61.5 MB | **32.7-34.5 MB** (anon 11.4-12.7, file 21.3-21.8) |
| cgroup view | none (no limit) | none | `memory.current` 48.2 MB (anon 13.0, page cache 32.6, kernel 0.7); `docker stats` 19.5 MiB (that minus inactive file: not comparable with RSS) |
| Start to first `/healthz` 200 | 86-101 ms | 518-622 ms (includes process creation and antivirus scan) | 1.71-2.34 s from `docker start` of `alixai` with anchor and relay up (`docker start` itself returned after 1.45-2.09 s) |
| Whole stack cold start | one process, immediate | one process, immediate | 19.7 s to first 200 on the published port: the health-gated chain waits one health interval (10-15 s) per hop, a configurable cost |
| Other resident processes | none | none | anchor 5.9 MB RSS, relay 5.8 MB (`docker stats` 8.5 and 3.3 MiB); one containerd-shim per container, 11.2-12.1 MB each |
| Container runtime (shared by every container, not per stack) | none | none | dockerd 175 MB RSS and containerd 53 MB inside the Docker Desktop VM; Docker Desktop host processes about 585 MB working set on Windows |

`/readyz` answered 503 in every shape (no model is provisioned), as designed. What the figures say:

- **B versus C** (native `alixai` against the containerised stack, LocalAI a container in both): C adds about **44 MB resident**
  (anchor 5.9 + relay 5.8 + three shims about 35, less about 3 MB because the containerised process is smaller) and about **1.6-2.2 s** of
  start-up. The runtime cost is paid in both, because B still needs it for LocalAI.
- **A versus C** (everything native, no runtime): add the runtime, about **228 MB** (dockerd and containerd) on this box, and on
  Windows or macOS the VM and its host processes (about 0.6 GB working set here; the WSL2 VM itself, 4.6 GB working set, is shared with
  two other distributions and cannot be attributed). Roughly **270 MB** in total in Linux terms, 3% of an 8 GB host.
- The process itself costs the same in either shape (37 against 34 MB RSS): the Go service is small enough that packaging is not
  where the memory goes. The model weights are, and they are identical in every shape (ADR [0020](0020-4gb-profile.md),
  `docs/operations/resource-budgets.md`).

### What each shape can enforce
Static checks (`go test ./deploy/native ./tests/deploy`); the native columns were not run under systemd or launchd.

| Control | Container stack (M5-01) | systemd unit | Windows service | launchd daemon |
| --- | --- | --- | --- | --- |
| Memory, swap, CPU | cgroup (512 MB limit seen in `docker stats`; `memswap_limit` equal to it is M5-01's setting, not re-read here) | cgroup: `MemoryMax`, `MemorySwapMax=0`, `CPUQuota` | none | none |
| Own non-root identity | uid 65532 and 10001 | `DynamicUser` | virtual account `NT SERVICE\AlixAI` | `_alixai` |
| Capabilities, privilege gain | `cap_drop` ALL, `no-new-privileges` | empty bounding set, `NoNewPrivileges` | the account's own rights | the account's own rights |
| Filesystem | read-only root, bind mounts | `ProtectSystem=strict`, `PrivateTmp`, `ProtectHome` | ACL on the data directory | account, umask 0077 |
| Network | internal networks, no route out | `IPAddressDeny=any` plus loopback, `RestrictAddressFamilies` | loopback listener only | loopback listener only |
| System calls | the runtime's default seccomp | deny list on top of the parser's own filter | none | none |

### What was not measured
| Owed by this ADR | State | Owner |
| --- | --- | --- |
| LocalAI native (Linux binary) against containerised LocalAI | Not measured: the dev box has neither the multi-GB image nor the native binary, and no model is activatable. It separates A from B, not B from C. | M5-07 (supervisor), M5-H04 (real hardware) |
| Rootless Podman against Docker overhead | Not measured: Podman is not installed on the dev box. | M5-H04 |
| WSL2 VM overhead with `swap=0` | Not isolated: the VM is shared with other distributions. | M5-H04 |
| Start-up to `/readyz` 200, idle CPU wake-ups | Start-up measured to `/healthz` only (no model, so `/readyz` stays 503); wake-ups not measured. | M5-H04 |
| First token and tokens per second | Not measured: no activatable model. Expected near zero (same runtime). | M5-09, M5-H04 |
| The native unit and plist on a real Linux and macOS host | Static checks only (`go test ./deploy/native`). | M5-H04 |

## Decision
**Accepted: C, the full container stack, is the preferred Lite installation**, with
rootless Podman and Quadlet first on Linux and Docker Compose supported. B and A remain supported
alternatives and the native alixai package is always shippable (M5-02). Reasons:

1. It is the shape whose resource limits and hardening are enforced by the kernel and verified by
   M5-01's tests, and the shape certification is measured in, so what operators run is what was
   measured.
2. It is LocalAI's recommended form and the only one with full backend coverage on every host
   (including TTS on arm64, and Windows hosts through Docker/Podman).
3. It keeps AlixAI and LocalAI as separate OS identities by construction (§16.2).

The preference is overturned for a given target, not globally, if native is measurably the only
way to meet a gate there. The expected case is the 4 GB profile ([0020](0020-4gb-profile.md)).

**Why the measurements confirm it.** The overturn criteria (prefer native for a profile when it saves enough steady memory to
change the outcome of the §7.4 headroom rule **and** the same hardening guarantees can be demonstrated by test) were applied to
the figures above:

- *Memory.* Native saves about 44 MB over C (shape B) or about 270 MB (shape A, Linux terms). Under Lite (8 GB) the LocalAI
  container budget keeps at least 911 MiB spare and the allowance outside it is 1152 MiB (`docs/operations/resource-budgets.md`,
  section 3), so a saving of this size does not change the outcome of the headroom rule. Not met for Lite.
- *Hardening.* The native units follow the same rules (separate identity, no capability, protected filesystem, loopback only)
  and are checked statically, but nobody has run them on a real host, and a Windows service and a launchd daemon cannot cap memory or
  CPU at all. A native equivalent of M5-01's hardening checks, demonstrated by test on a real host, does not exist yet. Not met.
- *The 4 GB profile.* There the allowance is 1024 MiB and the spare inside the container 248 MiB, so a 270 MB saving could decide a
  run. That is the one open case: it needs real 4 GB hardware and a native LocalAI measurement (owners in the table above). Until a
  task shows the saving decides the headroom rule for `experimental-4g`, C stays; that finding would reopen this ADR for that profile only.

The native packages (systemd unit, launchd plist, Windows service) ship regardless and are documented in
[`deploy/native`](../../deploy/native/README.md).

## Consequences
- Easier: one reference install, one set of limits and hardening tests, identical to the
  certification harness; Podman-first matches the project convention for new container work.
- Harder: a container runtime is a hard prerequisite for the *preferred* path, so Windows and
  macOS hosts need a VM-backed runtime, and Lite certification there needs real hardware (M5-H04).
- Harder: native users carry more hardening work and a weaker LocalAI backend set; the docs must
  say so rather than present the shapes as equivalent. On Windows and macOS a native service has no resource cap, so a Lite
  install there that needs limits uses the container shape.
- Easier for native: the service is one 25-27 MB static binary, up in under a second with no runtime resident (measured above).
- Forbidden: publishing any container port on a non-loopback address, one-shot init containers,
  on-demand backend downloads in the production profile, and treating a container result as
  "certified" instead of "emulated".

## Reversal plan
The install shape is packaging, not architecture. `AIRuntimeProvider` already points at a LocalAI
endpoint (loopback or unix socket) whether it is a container or a supervised process, and the
same Go binary runs in both. Switching the preferred shape means changing which `deploy/` target
the documentation and certification harness lead with; no code or data migration. Data lives on
bind mounts in both shapes, so moving between them is a directory move.

## Sources (checked 2026-10-02; measurements 2026-10-03)
- LocalAI installation and binary limitations: <https://localai.io/docs/installation/>, <https://localai.io/docs/reference/binaries/>, backends: <https://localai.io/docs/backends/index.html>
- LocalAI releases (v4.10.0): <https://github.com/mudler/LocalAI/releases>
- WSL2 memory and swap defaults: <https://learn.microsoft.com/en-us/windows/wsl/wsl-config>
