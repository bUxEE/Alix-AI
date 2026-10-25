# Hardware profiles: what was measured and what was not

Status: **analysis only.** Lite and Balanced were **emulated** with container memory and CPU caps on one x86-64 desktop.
Neither profile is certified on real hardware. **ARM64 is untested.** The decision layer's hardware gates (CPU, ARM64, CUDA,
TensorRT, VPS) are **unmeasured**, so CLM-v0.1-8B is off by default on every profile.

Back to the [verification index](README.md).

## 1. The one host everything ran on

| | |
| --- | --- |
| CPU | Intel i7-10700K, 8 cores / 16 threads, x86-64 |
| RAM | 64 GB |
| OS | Windows 11, Docker Desktop (WSL2 VM of 16 CPUs and about 31 GiB), no GPU used |
| Runtime | LocalAI v4.10.0 (pinned by digest), llama.cpp backend |
| How a profile was emulated | one container capped by cgroup: Lite 8 GiB / 4 CPUs, Balanced 16 GiB / 8 CPUs, experimental 4 GB profile 4 GiB / 4 CPUs; swap off (`memswap_limit` equal to `mem_limit`) |

Other jobs shared the host during several runs, so **latency figures are indicative**; memory figures are not affected by
that. Model files sat on a Windows bind mount seen through the Docker VM, so page-cache and fault behaviour on a native ext4
Linux host can differ.

**Why this is not certification.** A cgroup cap limits one container; it does not reproduce a real 8 GB machine's operating
system, its other processes, its disk, its CPU frequency or thermal behaviour. The memory outside the LocalAI container (the
operating system, the container runtime, the AlixAI core, the store, a parser worker) was budgeted, not measured. Results
from container caps are labelled **emulated**, never certified.

## 2. Theoretical fit, from measured numbers

The headroom rule: in steady state keep at least max(15% of system memory, 1 GiB) free, with no swap.

| Profile | System memory | Required headroom | Container budget (LocalAI, peak) | Allowance outside the container |
| --- | ---: | ---: | ---: | ---: |
| Lite (x86-64, 8 GB, 4 CPU) | 8,192 MiB | 1,229 MiB | 5,811 MiB | 1,152 MiB |
| Balanced (16 GB, 8 CPU) | 16,384 MiB | 2,458 MiB | 12,134 MiB | 1,792 MiB |
| Experimental 4 GB (not supported) | 4,096 MiB | 1,024 MiB | 2,048 MiB | 1,024 MiB |

Measured peak U (the larger of the summed resident set of the LocalAI processes and the container's working set) inside
that container ([model report](../evidence/m5-models/report.md), [4 GB report](../evidence/m0-4gb/report.md)):

| What runs in LocalAI | Peak U | Lite fit (5,811) | Balanced fit (12,134) |
| --- | ---: | --- | --- |
| Service only, no model | 125 to 160 MiB | yes | yes |
| Embedder only (Granite Embedding 97M) | about 390 MiB | yes | yes |
| Granite 4.0 350M (candidate controller, rejected on quality) | 850 to 868 MiB | yes | yes |
| MiniCPM5-1B (rejected) | 1,519 MiB | yes | yes |
| Qwen3.5-0.8B (rejected) | 2,047 to 2,082 MiB | yes | yes |
| Granite 4.0 1B (rejected) | 2,557 to 2,570 MiB | yes | yes |
| Qwen3.5-2B (rejected) | 3,121 to 3,136 MiB | not run on Lite | yes |
| A controller plus a resident embedder | about +250 MiB over the controller alone | yes for every rung above | yes |
| CLM-v0.1-8B encoder (Qwen3-8B, bf16) | climbed to 13,742 MiB of a 14,336 MiB cap while loading and never became ready | no | **no** |

**Reading it.** Memory is not what limits Lite or Balanced: every small controller fits with a wide margin. What limits them
is **quality**: no candidate controller passes the pre-registered Lite bar (refusal of out-of-scope requests and injection
resistance are the lines nobody reaches), so 1.0 ships **no default controller model on any profile** and serves the
deterministic path, RAG and the embedder ([ADR 0010](../decisions/0010-lite-model-selection.md)). The default Lite stack is
predicted at about 0.7 to 1.2 GiB in total (see [soak analysis](soak-analysis.md), section 2).

**Latency, indicative.** On 4 CPUs, prefill ran at about 90 to 125 tokens per second and decode at 12 to 18 tokens per second
for Qwen3.5-0.8B; a two-call turn at the Lite prompt cap is 15 to 30 seconds by arithmetic from those rates. One model slot
means concurrent requests queue: two at once cost the second about the first one's whole duration. A real low-end machine is
expected to be slower than this desktop under a 4-CPU quota.

**The experimental 4 GB profile** is not supported: one small controller fits alone, but a resident embedder leaves no
margin and voice cannot be resident ([ADR 0020](../decisions/0020-4gb-profile.md)).

## 3. ARM64

Nothing ran on ARM64. What is known:

- The AlixAI binary is pure Go (`CGO_ENABLED=0`), so it cross-compiles for `linux/arm64`; it has not been run there.
- The parser sandbox's OS confinement on `linux/arm64` (Landlock and seccomp) is untested
  ([ADR 0014](../decisions/0014-parser-sandbox.md)); it fails closed if confinement cannot be applied.
- The pinned LocalAI image publishes an `arm64` variant; its llama.cpp backend performance on ARM is unmeasured.
- The pinned vLLM CPU image used by the optional CLM sidecar lists an `arm64` variant in its index; it was never run.

## 4. Decision-layer hardware gates

The decision layer is optional, bounded, non-generative and non-authoritative: a scorer may rank candidates that policy has
already authorised, and never decides. Its gates were meant to be measured per hardware class. State on 1.0:

| Gate | State | Note |
| --- | --- | --- |
| CPU, Balanced (16 GB) | measured: `resource_gate` | the Qwen3-8B encoder reached 13.4 GiB of a 14 GiB cap while loading, host free RAM fell to 2.3 GiB, the load never completed; not repeated |
| CPU, higher-memory host (about 32 GB) | not measured | the bf16 weights alone are about 15.3 GiB before activations; this host could not safely hold the caps |
| ARM64 | not measured | |
| CUDA | not measured | the shipped sidecar is CPU only (`CUDA_VISIBLE_DEVICES=-1`); there is no GPU deployment |
| TensorRT | not measured | no TensorRT build or deployment exists |
| VPS | not measured | |
| Sub-1B scorers (Laya, Laya multilingual, GLiNER2.5-Decide) | not measurable | the pinned LocalAI v4.10.0 has no Decisions API; no other shipped runtime serves them |

**Consequence: CLM-v0.1-8B is off by default on every profile, and so is every other scorer.** The deterministic rules stay
in front of the controller. This is the policy of [ADR 0031](../decisions/0031-decision-scorer-default-enable.md): a profile
changes only with a recorded benchmark on that host that passes the `tools/decbench` gate. Enabling a scorer is an explicit
operator opt-in on hardware the operator has measured.

## 5. How to run the suite on real hardware

On a real machine there is no container cap to emulate the profile: the machine is the profile. Use a machine that matches
it (Lite: x86-64 or ARM64, 4+ cores, 8 GB RAM, SSD, no GPU; Balanced: 16 GB, 8 cores), with swap and zram off.

1. Install the stack with the profile's compose file (`deploy/container/stack/compose.lite.yaml` or
   `compose.balanced.yaml`) or natively ([local machine guide](../operations/guides/local-machine.md)). Fetch the pinned
   models; the service refuses a file whose sha256 does not match the registry.
2. Run the automated suites: `go test ./...` and `go run ./tools/gates --run`.
3. Run the model benchmark against the live stack with `tools/bench` (corpus and bar in
   [the model report](../evidence/m5-models/report.md)); check results with `go run ./tools/bench check <dir>`.
4. Run the RAG evaluation (`tools/rageval`) and, if voice is enabled, `tools/voicebench`.
5. Run a 24-hour soak, then a 7-day soak ([soak analysis](soak-analysis.md), section 4).
6. For the decision layer: `go run ./tools/decbench` with a baseline run and a candidate run on the same host, then its
   `gate` command. Do not run the CLM sidecar on a machine with less free RAM than the encoder needs: on the test host it
   drove free memory down to 2.3 GiB.

## 6. Community results wanted

Real-hardware results are the missing evidence for both profiles and for ARM64. If you run any part of section 5, open an
issue titled "Hardware result: <profile>, <CPU>, <RAM>" with:

- the hardware: CPU model and architecture, cores, RAM, disk, board or VPS plan, OS and kernel, container runtime;
- whether swap and zram were off;
- the AlixAI version and commit, the models by registry id and their sha256, any configuration you changed;
- the result files (`tools/bench`, `tools/soak`, `tools/decbench`) or their summaries, and `go test` / `tools/gates` output;
- peak memory of the whole machine, not only of the containers.

Negative results (out of memory, swap, model reloads, timeouts) are wanted as much as passes. Remove keys and private data
first.
