# Hardware and pricing for private and shared LLM serving

Status: **operator guidance, 1.4.0.** Prices and published throughput were re-checked on
**2026-10-27** against public pages cited below. **Every latency and capacity figure that is not
from a filed `tools/llmbench` report is labelled estimate — run tools/llmbench to measure.**
AlixAI does not ship cloud credentials; hosts bring their own accounts.

Companion: [Performance compose](../../deploy/container/performance/README.md),
[private-LLM kit](../../deploy/private-llm/README.md), [light External](guides/light-external.md),
[verification hardware](../verification/hardware.md) (Lite/Balanced CPU emulation).

## 1. Price sources (re-verified 2026-10-27)

| Item | Figure | Source |
| --- | --- | --- |
| Hetzner GEX131-1 (RTX PRO 6000 96 GB) | €1,499 / month net + €1,499 setup | [hetzner.com/dedicated-rootserver/gex131](https://www.hetzner.com/dedicated-rootserver/gex131/) |
| Hetzner GEX63 (RTX PRO 6000 96 GB) | €999 / month net + setup | [hetzner.com GPU matrix](https://www.hetzner.com/dedicated-rootserver/matrix-gpu/) / WZ-IT comparison 13 Oct 2026 |
| Hetzner GEX45 | €214 / month net | WZ-IT comparison 13 Oct 2026 (GEX44 discontinued → GEX45) |
| Scaleway L4-1-24G | €0.79 / hour (~€575 / month if 24×30) | [scaleway.com/en/pricing/gpu](https://www.scaleway.com/en/pricing/gpu/) |
| Scaleway L40S | €1.47 / hour listed (PAR-2) | Scaleway L40S product page / gpueconomy Scaleway capture 26 Oct 2026 |
| Scaleway H100 PCIe | €2.87 / hour listed | gpueconomy Scaleway capture 26 Oct 2026 |
| RTX PRO 6000 purchase | ~$14–18k street (memory shortage) | Market commentary Oct 2026 — **estimate, shop locally** |
| Published ~32B-class vLLM throughput on RTX PRO 6000 | ~930–970 tok/s | Vendor/community benches cited in planning — **not AlixAI-measured**; treat as estimate |

Older snapshots (e.g. GEX131 ~€1,199 list mid-2026, GEX44 ~€214 with 20 GB) are superseded; use the table above.

## 2. Models in this wave

| Registry id | Quant | VRAM need (publisher / size estimate) | Context | Tool calling | Languages |
| --- | --- | ---: | ---: | --- | --- |
| `gpt-oss-20b` | MXFP4 | ~16 GB (OpenAI cookbook, 2026-10-27) | 131072 | claimed (OpenAI / vLLM) | en primary; unqualified |
| `qwen3-30b-a3b` | BF16 | ~57–70 GB weights (HF size 2026-10-27) or smaller with FP8/AWQ serving | 262144 | claimed (Qwen3 instruct) | card list empty; unqualified |
| `gpt-oss-120b` | MXFP4 | ≥60 GB / H100-class (OpenAI cookbook) | 131072 | claimed | en primary; unqualified |

**vLLM gpt-oss / MXFP4 (as of OpenAI + vLLM docs, 2025-08 / recipes 2026):** optimised native MXFP4 kernels target **NVIDIA Hopper (H100/H200) and Blackwell (B200)** (and listed AMD MI300-class). Ada Lovelace (RTX 4000 Ada, L4, consumer Ada) is **not** in that optimised path — for those GPUs prefer **`qwen3-30b-a3b`** (or another registry candidate) under vLLM, or **LocalAI GGUF** on the LocalAI CUDA compose. Do not assume MXFP4 gpt-oss runs on GEX45/L4 under vLLM.

Status of all three: **candidate** until `tools/llmbench` + quality gates. Details: `models/registry/*.yaml`.

## 3. Segments

Latency columns: **estimate — run tools/llmbench to measure.** Streaming TTFT assumes OpenAI-compatible SSE.

### 3.1 One user account / small team (dedicated private LLM)

| | |
| --- | --- |
| Recommended model | On **Hopper/Blackwell**: `gpt-oss-20b` (vLLM MXFP4). On **Ada/L4 (GEX45 / L4-1-24G)**: `qwen3`/`LocalAI GGUF` — not vLLM MXFP4 gpt-oss (see §2) |
| GPU / server | Budget Ada/L4: **Hetzner GEX45** or **Scaleway L4-1-24G** with LocalAI CUDA or a non-MXFP4 vLLM model. Native gpt-oss MXFP4: **H100/H200**-class (or Blackwell) |
| Monthly rent | GEX45 ~€214 net; Scaleway L4 ~€0.79/h (≈€575 if always on); H100-class much higher (see Scaleway H100 row in §1) |
| Purchase | Used workstation + 16–24 GB Ada GPU for LocalAI/Qwen; RTX PRO 6000 / H100 only if you need gpt-oss MXFP4 or 96 GB headroom |
| Full-answer / first-text | **estimate** — run tools/llmbench on the real GPU (target comfort only: TTFT &lt; 0.5 s, full ~300 tok &lt; 3–5 s at c=1) |
| Concurrent generating / online | 1 generating; a few online |
| Redundancy | None required; snapshot model disk; accept hours of recovery |
| Light/external alternative | [light External](guides/light-external.md): 4 vCPU / 8 GB, no GPU; pay per external token |

### 3.2 One company, 5–20 users

| | |
| --- | --- |
| Recommended model | `gpt-oss-20b` (or `qwen3-30b-a3b` on 80 GB if quality needs it) |
| GPU / server | **GEX63** or **GEX131-1** (96 GB) / Scaleway **L40S** |
| Monthly rent | GEX63 €999; GEX131-1 €1,499; L40S €1.47/h (≈€1,070 if always on) |
| Purchase | 1× RTX PRO 6000 ~$14–18k + host (Oct 2026 estimate) |
| Full-answer / first-text | estimate — run tools/llmbench to measure |
| Concurrent generating / online | estimate 2–8 generating / 20 online — run tools/llmbench to measure |
| Redundancy | Warm standby optional; nightly backup of config + model pins |
| Light/external alternative | External controllers for burst; keep AlixAI policy local |

### 3.3 20–100 users (shared)

| | |
| --- | --- |
| Recommended model | `gpt-oss-20b` multi-seq or `gpt-oss-120b` if quality requires |
| GPU / server | 1–2× 96 GB (GEX131 / L40S×2) or 1× H100 80 GB for 120b |
| Monthly rent | €1.5k–3k dedicated; Scaleway H100 ~€2.87/h (≈€2.1k if always on) |
| Purchase | 2× RTX PRO 6000 or 1× H100-class — shop quotes |
| Full-answer / first-text | estimate — run tools/llmbench to measure |
| Concurrent generating / online | estimate — run tools/llmbench (`-target`); use `cache_salt` per tenant |
| Redundancy | N+1 if SLA is business-hours chat; otherwise accept queueing |
| Light/external alternative | Hybrid: local RAG + external LLM for overflow ([light External](guides/light-external.md)) |

### 3.4 100–500 users

| | |
| --- | --- |
| Recommended model | `gpt-oss-120b` or sharded `gpt-oss-20b` fleet |
| GPU / server | Multi-GPU H100/L40S nodes or several GEX131 |
| Monthly rent | mid four-figures € / month (always-on cloud) or several dedicated GEX |
| Purchase | Multi-GPU server quotes; lead times matter |
| Full-answer / first-text | estimate — run tools/llmbench to measure |
| Concurrent generating / online | estimate — run tools/llmbench; horizontal scale before raising max-model-len |
| Redundancy | Active-active behind the AlixAI host; drain on deactivate |
| Light/external alternative | External for most tenants; dedicated private LLM only for the paid tier |

### 3.5 500–1,000+ users

| | |
| --- | --- |
| Recommended model | Fleet of `gpt-oss-120b` / `gpt-oss-20b` by SKU |
| GPU / server | Multi-node H100/L40S; EU regions (Scaleway PAR/WAW, Hetzner FSN/HEL) |
| Monthly rent | high four- to five-figures €; compare reserved vs dedicated |
| Purchase | CapEx only with staffing for ops |
| Full-answer / first-text | estimate — run tools/llmbench to measure on the chosen SKU |
| Concurrent generating / online | capacity plan from llmbench at 1/4/16/64 then extrapolate carefully |
| Redundancy | Required (multi-AZ or dual provider); chaos-test drain |
| Light/external alternative | Default path for cost; private LLM as premium SKU |

## 4. Cost-per-user and break-even (€100 / user / month)

Assumptions (honest): one dedicated GPU server, no multi-region redundancy, ops time valued separately.

| Monthly GPU rent | Users to break even at €100/user | Notes |
| ---: | ---: | --- |
| €214 (GEX45) | ≥3 paying users | Tiny margin after power/ops |
| €999 (GEX63) | ≥10 | Still single-box risk |
| €1,499 (GEX131-1) | ≥15 | Prefer ≥20 for spare capacity |
| €2,100 (H100 always-on ≈ €2.87×24×30) | ≥21 | Plus block storage / IP on Scaleway |

**Redundancy:** a hot spare roughly **doubles** rent → break-even users double. **Ops:** budget at least
0.1–0.5 FTE for upgrades, pin verification, and incident response; that dominates small fleets.
**External alternative:** at €100/user you may still prefer light External if token spend stays under
private-LLM rent — measure both.

## 5. How to replace estimates

```bash
go run ./tools/llmbench run \
  -endpoint http://127.0.0.1:8000/v1 \
  -key "$VLLM_API_KEY" \
  -model gpt-oss-20b \
  -out docs/evidence/llmbench/$(date +%Y%m%d) \
  -target 8s
```

File the JSON/Markdown report under `docs/evidence/` and update the segment rows from estimates to
measured (with host, image digest, and model revision).
