# 0014 — Cross-platform sandbox for parsers and optional adapters

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-16 (provisional default); finalised in M2-04

## Context
Parsing and OCR run on untrusted files, the highest-risk code path in the system (§16.4: archive
bombs, parser exploits, path traversal; §12.6: hidden text and hostile content). §16.2 requires
parser and OCR workers to be sandboxed and disposable, with per-provider CPU, memory, time, disk
and concurrency limits. The build rules fix the constraints: pure Go, `CGO_ENABLED=0`, Linux,
Windows and macOS on amd64 and arm64, and no shell-out to arbitrary commands.

§25 item 5 asks for the *exact* cross-platform mechanism. No single OS mechanism exists on all
three platforms, so the decision is a layered one with an honest statement of what each platform
guarantees, and of which of those statements a test has actually confirmed. The built-in parsers
(txt, md, html, csv, pdf, docx; M2-04) are pure Go, so the realistic threats are resource
exhaustion, logic bugs and hostile structure rather than memory corruption. "Optional adapters"
(OCR, converters wrapping native binaries) are different: native code is not memory-safe and needs
stronger confinement or must not run.

Platform facts (checked 2026-10-02):

- **Windows:** Job Objects give per-process and per-job memory limits (committed bytes), an
  active-process limit, a per-process CPU-time limit and kill-on-close. Restricted tokens, low
  integrity and AppContainer exist, but `os/exec` does not expose them; the Go proposal to add them
  to `SysProcAttr` (golang/go#80415) is still open, so they would be hand-written Win32 code.
- **Linux:** `setrlimit`, `no_new_privs`, seccomp-bpf and Landlock. Landlock needs kernel 5.13 for
  filesystem rules and 6.7 (ABI 4) for TCP rules. Namespace-based sandboxes (bubblewrap style) are
  fragile: Ubuntu 24.04 restricts unprivileged user namespaces by default through AppArmor.
- **macOS:** Seatbelt is the only process-sandbox mechanism for non-App-Store software.
  `sandbox_init` is a C API (cgo, unavailable here); `sandbox-exec` is deprecated but still shipped.
  macOS does not enforce `RLIMIT_AS` or `RLIMIT_RSS`.
- **WebAssembly:** `wazero` (pure Go) runs `wasip1` modules with no ambient authority, a memory page
  cap and context cancellation.

## Options
A. **Out-of-process worker with layered, platform-specific confinement** (below). Process boundary
   plus kernel-enforced limits on every platform; real syscall and filesystem confinement on Linux;
   weaker on Windows and macOS. Measured cost: a process per document takes 35 to 77 ms to start on
   Linux and 70 to 112 ms on Windows (Measurements, below); a parse itself is 1 ms to 1.5 s for
   100 KiB to 13 MiB. An idle worker is 4.4 MiB resident on Linux and
   7.3 MiB working set (44.8 MiB committed) on Windows.
B. **In-process parsing with goroutine and `context` limits only.** Cannot cap memory, cannot
   survive a fatal runtime error (a Go out-of-memory or stack overflow kills the whole service), and
   a parser bug runs with the core's database and key access. Rejected on the requirement.
C. **WASM sandbox (`wazero` v1.12.0, the parsers built for `wasip1`).** Uniform across operating
   systems and strong by construction. Measured (below): 20 to 60 times slower than A, 6 to 13
   times the memory, and a 3 to 4.5 s cold compile per module. Rejected for the built-in parsers; it
   also cannot contain native OCR or converters.
D. **Container per parse job.** Strongest isolation (read-only rootfs, `cap_drop` ALL, no network,
   memory limit) and the "strong option for server deployments". Needs a container runtime, adds
   seconds of latency and operational weight per document, and is unavailable on a native install.
   Not measured here. Offered as a deployment option (L3), not the default mechanism. The A worker
   was run inside such a container (read-only rootfs, user 65534, `--cap-drop ALL`,
   `--network none`, `no-new-privileges`, 512 MiB): it still reaches L2 inside it.
E. **Per-platform OS sandbox only** (Landlock on Linux, AppContainer on Windows, Seatbelt on macOS)
   with no common baseline. Rejected as the *only* layer: guarantees would differ silently and some
   are unavailable or deprecated.

## Decision
**Option A**, as four layers. The worker is `cmd/alixai-parser`; the launcher is
`internal/rag/parse` (`Runner`); the protocol is `internal/rag/parse/wire`.

- **L0, every platform, mandatory.** One process per document (disposable; any anomaly discards
  it). The document arrives as bytes on stdin, so the worker needs no filesystem path; file access
  stays with the source provider's canonical-path checks, and archive entry names are never used as
  paths. Empty environment, nothing inherited but stdio, a working directory that is an empty
  owner-only directory, a wall-clock deadline enforced by the host (the process or process group is
  killed), input, output and expanded-size caps, archive-bomb limits (ratio, entry count, total
  size, nesting depth), and the parser name and version recorded in every result (a worker whose
  version differs from the service's is refused at the handshake). The host treats every byte from
  the worker as untrusted: frame lengths are checked before allocation, the result is re-validated
  against its text (spans inside the text, on code point boundaries, UTF-8), and a worker that
  breaks the protocol is killed. "No network" at this layer holds **by construction**:
  `TestParserHasNoNetwork` fails if the worker's dependency graph (`go list -deps`, checked for six
  OS and CPU pairs) contains `net`, `crypto/tls` or `os/exec`, or if its own code names a socket
  primitive. That is a property of the code, not an operating-system guarantee, and the
  documentation says so for every platform where the OS does not also enforce it.
- **L1, resource limits, mandatory and fail-closed.** Windows: a Job Object (process and job memory
  limit in committed bytes, active-process limit 1, per-process CPU time, kill-on-close,
  die-on-unhandled-exception, UI restrictions), joined right after the process is created and
  before the host sends it anything; the worker reads the limits of the job it is in back and
  refuses to parse if they are not the ones it was told. Linux: `no_new_privs`, `RLIMIT_CPU`
  (SIGKILL at the limit), `RLIMIT_CORE=0`, `RLIMIT_FSIZE=0`, `RLIMIT_NOFILE=64`, and `RLIMIT_AS`
  set **relative to the address space the worker already holds** (see Linux measurements), all read
  back by the worker and again checked by the host; a host-side resident-set watchdog (every 10 ms,
  kills the process group above the budget); a Go soft memory limit at 80 % of the budget and an
  in-process memory guard. cgroup v2 `memory.max`, which the provisional draft listed, is deliberately
  **not used**: a delegated cgroup per worker is not generally available to an unprivileged service
  (it needs a systemd user slice or a root-owned hierarchy), and the three mechanisms above already
  bound memory from inside and outside the process; container-level cgroups are L3's job. macOS:
  the same `rlimit`s except `RLIMIT_AS`, plus the in-process guard and the wall-time kill. **If a
  mandatory mechanism cannot be installed or verified, the worker does not parse**: the job fails
  `unavailable` (`sandbox_unavailable`), never an unsandboxed run.
- **L2, OS confinement, best effort and reported.** Linux only: Landlock with every access right
  handled and none allowed (no filesystem access at all, and no TCP bind or connect on ABI 4 or
  later) and a seccomp-bpf deny list for sockets, program execution, `ptrace`, namespaces and
  mounts, `io_uring` and similar, with an architecture check; both written as raw system calls
  (no `go-landlock` dependency, no cgo) and installed on every thread. If the kernel or the
  container's own seccomp profile refuses either, the worker parses without it and reports the
  mechanism as missing; the host records level `L1` instead of `L2` in the result (`Sandbox.Level`,
  `Sandbox.Missing`) and the readiness probe (`Runner.Probe`) shows the same. Windows: restricted
  token and AppContainer are **not provided**. macOS: no Seatbelt wrapper is provided (`sandbox-exec`
  is deprecated and no maintained pure-Go route exists); macOS stays at L1.
- **L3, deployment level.** The container stack ([0012](0012-lite-install-native-vs-container.md))
  runs the whole service with a read-only rootfs, dropped capabilities and a memory limit. The parser
  child shares the service's network namespace (which must reach LocalAI on loopback), so L3 does
  not replace L0 and L2 for "no network"; a dedicated no-network parser container behind the same
  protocol is possible for high-risk deployments. Not built here.

**Disposable stays the default.** A worker can serve several documents (the protocol loops, and
`TestParseFootprint` measures it), but one worker handing document after document means a
compromised parse could shape the next one, possibly another tenant's. The measured price of a fresh
process is acceptable for background ingest (below), so no pool is built. If one is ever needed it
must be per scope, recycled after N documents or any anomaly, and it is a change to the `Runner`
alone.

**Optional adapters (OCR and any worker that is not memory-safe Go)** are disabled by default and
run only when L2 (Linux) or L3 is active. No adapter ships in this task: `ocr.enabled: true` is
refused by `Config.Validate` (a switch that did nothing would index a scanned document as empty), and
a PDF page that is only an image is reported as `needs_ocr` in the result; when no page has text the
parse fails `needs_ocr` rather than succeeding empty. The level a worker reached is in every result,
so the gate for a future adapter is a comparison, not a new mechanism. Built-in pure-Go parsers may
run at L1 with L2 absent, and say so.

### What each platform guarantees

"Tested" means the named suite ran green on that platform in this task. The eight required tests
(`TestParseFormatsGolden`, `TestZipBombRejected`, `TestOversizeRejected`, `TestParserTimeoutKilled`,
`TestParserMemoryLimitKilled`, `TestParserCrashIsolated`, `TestHiddenTextFlagged`,
`TestParserHasNoNetwork`) and the rest of `./internal/rag/parse/...` were run on:

- **Windows 11 amd64**, Go 1.27, native: green. Level L1.
- **Linux amd64**, alpine in Docker 29.7.2 on Docker Desktop (WSL2, kernel 6.18.33.2, Landlock ABI
  7, Docker's default seccomp profile), test binary and workers cross-compiled with
  `CGO_ENABLED=0`: green. Level L2. The import-graph subtests of `TestParserHasNoNetwork` skip there
  (no Go tool in the container); they cover all six targets on the Windows run. Also green, for the
  subset run (smoke, golden, no-network, memory, crash, directory), with the container hardened as
  in option D.
- **Linux arm64, macOS amd64 and arm64:** cross-compiled and `go vet` clean; **untested**, no runner.
  Everything said about them below is from the code and the platform documentation, not a run. On
  macOS the peak memory is read from `rusage`; if macOS seeds `ru_maxrss` from the parent as Linux
  does, the "a document that fits" peak assertion of `TestParserMemoryLimitKilled` will fail there, and
  the fix is the one used on Linux (read the worker's own high-water mark while it lives).

| Guarantee | Linux amd64 (tested) | Windows amd64 (tested) | macOS (untested) |
| --- | --- | --- | --- |
| Crash isolation, wall-time kill | yes (L0) | yes (L0) | expected |
| Memory cap enforced by the OS | `RLIMIT_AS` (hard, virtual) plus host RSS watchdog at the budget | Job Object commit limit (hard: allocation fails) | none: in-process guard and wall kill only |
| CPU cap | `RLIMIT_CPU` | Job Object per-process CPU time | `RLIMIT_CPU` |
| Cannot start a process | seccomp denies `execve`, `fork` (L2) | Job Object active-process limit 1 | by construction only |
| No network | seccomp denies sockets, Landlock denies TCP (L2), plus by construction | by construction only | by construction only |
| No filesystem | Landlock, no rights allowed (L2) | not enforced | not enforced |
| Peak memory reported | `VmHWM` of the worker | Job peak commit, process peak working set | `rusage`, not verified to exclude the host's |

On Windows and macOS a compromised parser can still read what the service account can read. That is
the gap against Linux, it is recorded in the threat model's open row, and the mitigations are the
service account's own least privilege, the empty environment and working directory, and L3.

### Linux measurements and defects found by running it (all fixed here)

The first container run of the suite failed, which the Windows suite had hidden:

1. **`ru_maxrss` is the parent's.** A child started with fork and exec on Linux begins with the
   high-water mark of the address space it was forked from; Go starts it with `CLONE_VM`, so every
   worker reported the host's resident set (283 MiB, 366 MiB, 384 MiB for documents of a few bytes).
   The Linux peak now comes from `/proc/<pid>/status` (`VmHWM`) read while the worker lives, which is
   the worker's own. macOS is the one platform still reading `rusage`, and says it is unverified.
2. **`RLIMIT_AS` as a fixed headroom does not work with Go.** A Go worker holds about 1.2 GiB of
   address space before `main` runs here (`VmSize` 1 262 112 kB with 16 CPUs, Go 1.27; it varies with
   the Go version and CPU count), more than the 800 MiB of headroom the provisional draft assumed
   from an earlier measurement. With a 128 MiB budget the worker had *no* room to map anything and
   died at its first allocation, so the memory test passed without testing memory; with the default
   512 MiB it had about 80 MiB, which would have failed legitimate documents. The limit is now
   `baseline + budget + 64 MiB`, where the baseline is the worker's own `VmSize` read when it applies
   the limit; the baseline is reported and the host checks the arithmetic.
3. **CPU kills were classified as "killed by the OS".** The kernel enforces `RLIMIT_CPU` on exact
   accounting; the `rusage` the host reads afterwards is tick-sampled and showed 1.83 s for a 2 s
   limit (92 %). A kill the host did not send, after 80 % of the CPU budget, is now a `cpu_time`
   timeout.

`TestParserMemoryLimitKilled` now also fails if the worker is stopped below half its budget, and
has a positive case (a worker that touches half its budget and returns must succeed on every
platform, with the guard on and off, and its host-measured peak must be its own).

**Failure mode when Landlock or seccomp are unavailable** (measured with a container seccomp profile
that answers `ENOSYS` to the calls): without `landlock_*`, the worker reports `landlock_fs: landlock
is not available (function not implemented)` and the level is `L1`; the seccomp filter still refuses
sockets. Without `seccomp` as well, nothing refuses sockets: a deliberately network-linked
fault-injection worker connected to a local listener (`dial succeeded`, level `L1`), which is why "no
network" there rests on the import-graph test alone. Parsing continues in both cases, at the level
reported.

## Measurements (all in docs/efficiency/M2-04.md)

1. **Spawn.** A process per document, to a verified sandbox: Linux 37 ms median (35 to 41 ms; 32 to
   57 ms in an earlier run), Windows 76 ms median (70 to 84 ms) in the quiet run and 95 to 112 ms in
   three earlier ones; times on this machine vary by up to 2 times between runs, and the efficiency
   review gives both. On Windows most of it is the operating system's and the antivirus's, not the
   sandbox's: starting and ending the worker with nothing else, back to back, took 63 to 65 ms for the
   5.8 MiB executable, 52 to 56 ms for the 3.9 MiB one built with `-ldflags="-s -w" -trimpath`, and 39
   to 42 ms for an empty Go program. The very first start of a freshly written executable took 26.3 s
   and 26.5 s with Avast scanning it, which is why `StartupTimeout` (30 s) is separate from the wall
   limit and why the service should probe the worker once at start-up. Reusing one worker costs 0.14 to
   0.47 ms per 4 KiB document against 40 to 177 ms for a fresh process, so process start and teardown
   are over 99 % of the cost of a small document; for a 1 MiB document of plain text or Markdown the start is about 55 % (Linux)
   and 80 % (Windows) of the end-to-end time. Parsing is a class-5 background job queued per document,
   and the stages after it (chunking, indexing, embeddings) are expected to cost more per document
   (not measured: they do not exist yet), so the disposable default stands, with the batching lever
   below if that expectation proves wrong.
2. **Throughput and memory per format**, in the efficiency review: 5 to 183 MiB/s of input (txt and
   md fastest, docx and pdf slowest); peak resident 5 to 123 MiB on Linux and peak commit 46 to
   152 MiB on Windows for documents of 100 KiB to 13 MiB.
3. **Landlock and seccomp** were available on kernel 6.18 (ABI 7) under Docker's default profile.
   Older kernels were not tested; the failure mode is the measured one above.
4. **PDF, native worker against `wasip1` on `wazero` v1.12.0** (compiler mode, a 512 MiB page cap),
   the same text extraction, Windows amd64 and Linux amd64. The native cells give the range over the
   two runs of the efficiency review:

   | Document | Native parse, Windows / Linux | `wazero` run, Windows / Linux | Native peak (working set / RSS) | `wazero` harness peak |
   | --- | --- | --- | --- | --- |
   | PDF 1.6 MiB (412 pages) | 95 to 188 ms / 155 to 226 ms | 3.6 s / 4.9 s | 26 MiB / 22 to 27 MiB | 153 MiB / 163 MiB |
   | PDF 13.1 MiB (3 292 pages) | 0.80 to 1.49 s / 0.86 to 1.49 s | 30.1 s / 42.9 s | 106 to 115 MiB / 104 to 123 MiB | 905 MiB / 910 MiB |
   | Markdown 1.0 MiB | 8 to 13 ms / 13 to 20 ms | 487 ms / 511 ms | 14 to 15 MiB / 12 MiB | 161 MiB / 150 MiB |

   The native figure leaves out process start (above) and the `wazero` figure leaves out the compile,
   which was 2.3 to 7.8 s for the 5.1 MiB module (a compilation cache would remove it after the first
   run; not tried); the harness peak is the whole harness process, compiled module included;
   `wazero`'s interpreter took 44 s for the 1.6 MiB PDF. Option C is not adopted: it is 20 to 60 times
   slower and 6 to 13 times larger on the Lite profile, and it closes no gap that A leaves open and
   the L2 layer does not.

Fuzzing: `FuzzParse` (all six parsers, small limits, 16 fuzz workers) found a PDF content-stream
parser panic in its first six seconds (an operator with no operands indexed `ops[-1]`; `Parse`
recovered it into an internal failure, so the worker lived, but the document was refused). Fixed,
and the input is kept as a regression seed under `internal/rag/parse/formats/testdata/fuzz`; two
further runs of 3 and 7 minutes (8.7 million inputs in the longer one) found nothing. M5-08 owns the
agreed fuzzing time (SEC-016).

## Consequences
- Easier: one worker protocol and one test suite everywhere, with a graded, auditable statement of
  confinement (`Sandbox.Level`, `Sandbox.Missing`) instead of a claim the platform cannot keep;
  parser name and version in every result, so a parser change creates a new index generation.
- Harder: platform code paths (Job Objects, `rlimit`s, Landlock, seccomp, raw system calls), a CI
  matrix (Linux arm64 and macOS have no runner yet), and honest documentation that Windows and macOS
  are weaker than Linux for "no filesystem" and "no network". The Linux test run needs a kernel and
  container profile that allow Landlock and seccomp to see level L2; the tests record what they got.
- Cost to plan for: a fresh process per document (above). On Windows ship the worker stripped
  (`-ldflags="-s -w" -trimpath`: 3.9 MiB, 10 to 15 ms faster to start in back-to-back runs, more when
  the antivirus is scanning), exclude it from on-access
  scanning where the operator can, and probe it at service start so the first real document does not
  pay for the first scan. Build flags are the packaging task's, not this package's.
- Security impact: a parser compromise or bomb is contained to one disposable process with no
  secrets, no network by construction and bounded resources; the core never runs parser code
  in-process; a platform that cannot apply L1 fails closed. Hidden text arrives as its own `Hidden`
  block, never as an unflagged gap (SEC-012).
- Forbidden: in-process parsing of untrusted content, an unsandboxed fallback, passing secrets or
  credentials to the worker, running a native-code adapter with neither L2 nor L3, an `ocr.enabled`
  switch that does nothing, and reading a worker's peak memory from `rusage` on Linux.

## Reversal plan
Everything sits behind the worker protocol, so swapping the confinement mechanism (for example
adopting `wazero`, or a pooled worker per scope) changes the launcher, not `internal/rag/parse`
callers or the ingestion pipeline. Results already stored carry the parser version and the sandbox
level, so a changed mechanism can be rolled out as a new parser version and old documents re-parsed
by queued jobs where needed.

**Overturn criteria.** Prefer C if a future `wazero` or Go `wasm` target is within about 2 times of
the native worker on the PDF case, or if a platform gives Windows and macOS no other way to confine
a parser. Move L2 on Windows from "not provided" to AppContainer only if a maintained, small
implementation exists (golang/go#80415 landing would make it cheap). Build a per-scope pooled worker
if ingest of small documents spends over about 25 % of its time in process start. Tighten the
optional-adapter rule if any L2 gap is found. Re-measure on a macOS and a Linux arm64 runner when
they exist; the table's "untested" cells must not be read as "works".

## Sources (checked 2026-10-02)
- Landlock kernel documentation and ABI versions: <https://docs.kernel.org/userspace-api/landlock.html>;
  the Go library the first draft planned to use: <https://github.com/landlock-lsm/go-landlock>
  (replaced by raw system calls, to stay dependency-free and install on all threads)
- wazero: <https://github.com/wazero/wazero> (v1.12.0 measured)
- Windows `SysProcAttr` proposal: <https://github.com/golang/go/issues/80415> (open on 2026-10-02);
  `x/sys/windows`: <https://pkg.go.dev/golang.org/x/sys/windows> (links `net` and `net/netip`, checked
  with `go list -deps`, which is why the worker calls `kernel32` through `syscall` and only the
  host launcher uses it)
- macOS `sandbox-exec` status: <https://github.com/microsoft/mxc/blob/main/docs/seatbelt/seatbelt-backend.md>
- Ubuntu 24.04 user-namespace restriction: <https://documentation.ubuntu.com/release-notes/24.04/>
