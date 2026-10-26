# Resource budgets

Status: **provisional v0**, 2026-10-03, task M0-15. Owner of the numbers: this file. Decisions: ADR 0020 (the 4 GB profile), ADR 0010 (model ladder), ADR 0018 (quotas).

Every later task has to stay inside these budgets or say, in its efficiency review, which row it raises and why. A number here is only as
good as its **Basis** column says:

| Basis | Meaning |
| --- | --- |
| **M** | measured under an emulated profile (a cgroup cap on one container; the [4 GB report](../evidence/m0-4gb/report.md) and the [M0 spike report](../evidence/m0-spike/report.md)). Never certified |
| **D** | measured on the Windows 11 dev box without a container limit (store, queue, index, parser and core figures from M0-05, M0-06, M2-04, M2-07 and the M0-15 core-footprint run) |
| **A** | arithmetic from model cards, `config.json` or an ADR; not measured |
| **E** | estimate from a vendor size or from experience; not measured. The task that adds the component owns the measurement |
| **P** | a proposal made here because nothing measured exists yet; open to an ADR |

Nothing in this file is a certification. The 4 GB profile is an aspiration (§7, ADR 0020): its budgets exist so that the aspiration stays
possible, not to promise it.

## 1. Who uses this file

| Consumer | Takes from here |
| --- | --- |
| `internal/dialogue/context/profile.go` (M3-03, provisional table) and `internal/dialogue` (`DefaultMaxPromptTokens`, M1-14) | the token caps of section 6; M3-03 said to replace its table with these values and change nothing else |
| `internal/rag/query` (M2-09) | RAG candidate, parent-expansion and evidence caps (section 6.3) |
| `deploy/container/localai/compose.<profile>.yaml` | memory, swap and CPU caps; `LOCALAI_MAX_ACTIVE_BACKENDS` (rule R3) |
| voice (M4-06), the decision layer (milestone D), the reranker | the optional-component rows of section 4 and the admission rule R3; the decision layer's own rows and rule D1 are in section 4.4 |
| quotas (M5-06, ADR 0018) | finite default limits per profile derived from sections 4 to 7 |
| model qualification (M0-17, M5-09) | the per-profile memory line a controller has to fit (section 3) and the latency lines (section 7) |

## 2. Rules every task must respect

**R1. One resident controller, one bounded request.** One active default LLM per deployment, one slot (`parallel` 1, the measured default),
one bounded context per active request (§17.2). Concurrent interactive sessions are 1 by default; a second request queues (section 7.4).

**R2. Resident roles are declared per profile and nothing else is resident.** `experimental-4g`: the controller only (text-only; no embedder, no
voice, no reranker, no decision scorer). `lite`: the controller and the embedder; voice packs and the reranker only when installed and only
inside their row of section 4. `balanced`: controller, embedder, small STT and TTS; reranker and decision scorer optional. A role that is not
declared resident is loaded only for a bounded job.

**R3. No optional model may silently evict the controller, retrieval or speech workers.** The runtime's own policy is LRU eviction at
`LOCALAI_MAX_ACTIVE_BACKENDS` active backends: a request for a model that is not resident unloads the least recently used one. Measured under the
4 GiB profile (section 4.3), alternating a controller call and an embedding call with one active backend makes **every call a model reload**.
Therefore:

1. `LOCALAI_MAX_ACTIVE_BACKENDS` must be at least the number of roles the profile declares resident. A configuration that declares more resident
   roles than the setting allows is refused at start (a typed configuration error), never run with eviction.
2. Loading an optional model (reranker, decision scorer, specialist STT, another language pack, a second embedder) is **admitted** only when the
   projected resident memory fits the container budget of section 3. The projection uses the incoming model's measured peak from its registry
   entry (`memory.peak_rss_mb`) and the current measured use, never the file size. Otherwise the request is refused with `resource_exhausted`
   or the work is queued as a job; the resident roles are not touched.
3. Idle unload (`LOCALAI_WATCHDOG_IDLE`) applies to optional models only. The controller, the embedder where the profile declares it resident,
   and the speech workers are exempt, with a minimum warm period (§17.2). LocalAI's watchdog is global, though, so the runtime itself cannot exempt
   them: the service compensates by giving the first turn after a long idle the cold-load budget (M0-19), and the exemption stays a requirement
   on any runtime that can express it.
4. **The shipped deployment does not meet 3.1 today.** `deploy/container/localai/compose.yaml` defaults `LOCALAI_MAX_ACTIVE_BACKENDS` to 1 and
   `compose.balanced.yaml` only suggests 2 in a comment, while R2 declares two resident roles for `lite` (controller, embedder) and four for `balanced`
   (controller, embedder, STT, TTS). Lite needs 2 and balanced 4; only `experimental-4g` is right at 1. The files are M0-18's scope and nothing yet checks the
   setting against the roles.
5. An eviction of a resident role is a defect, not a degradation: the count of resident-role reloads must stay 0 in every certified profile
   run (§7.4 "avoid repeated model eviction and reload on ordinary turns"), and a task that can cause one adds a test.

**R4. Admission before exhaustion.** Work is refused or queued before memory runs out (§17.2): the check of R3.2, the token budgets of
section 6 enforced before the runtime is called, and the finite per-tenant quotas of ADR 0018. Out-of-memory fails closed (`resource_exhausted`,
never a partial result).

**R5. Token budgets are enforced twice, in estimated tokens.** By the prompt builder and again at the runtime boundary (`providers.WithBudget`).
A request over budget is `validation_failed`, nothing is sent, and the builder never widens a budget to make a prompt fit. The estimator is
`providers.EstimateTokens` (one token per three bytes, plus framing). Measured against the runtime's own counts the real count of the message
text is **70 to 83 percent of the estimate** (the estimate is 20 to 43 percent too high) and the estimate counts the output constraint, which the runtime does not tokenise (section 6.1). Do not lower the estimator
without a tokenizer, and do not raise a cap because the estimate looks pessimistic.

**R6. The context window is configured, never the model's own.** Every model entry sets `context_size` explicitly (4096 in the M0 runs). The
Qwen3.5 native window is 262,144 tokens and Granite 4.0 1B's is 131,072; a 131,072-token load was killed by the 4 GiB cgroup
(recorded in the M0 spike's raw results). The window must cover prompt cap plus output cap in real tokens, with the margin of section 6.1.

**R7. No swap in a certified profile.** Containers set `memswap_limit` equal to `mem_limit` (verified inside the container: `memory.swap.max`
is 0, recorded by the M0-15 runs). Native installs run with swap and zram off (ADR 0020). A run on a host or VM that gives the cap swap is
not evidence for any profile.

**R8. Background work yields and is bounded.** Class 5 and 6 jobs (parse, OCR, index, embed, maintenance) run at most one at a time on the 4g
and Lite profiles, inside the CPU and memory rows below, and never load a model while an interactive turn is in flight (queue instead).
Interactive work keeps priority (§17.1).

**R9. Every feature records its cost** (CPU, RAM steady and peak, storage, latency, concurrency; rule 16) in `docs/efficiency/<task>.md` and
states whether it preserves, improves or worsens the 4 GB line. A component whose measured peak is above its row here must shrink, or raise the
row through an ADR and a re-run of the 4g experiment. Lowering a row is free.

**R10. The deterministic path is the floor in every profile.** Typed forms, the rules router and the templates cost no resident memory and are
what a profile serves while its model is absent, unqualified or refused by R3 or R4 (registry `degraded_mode: deterministic-routing`).

**R11. Hosts that put an operating system, a VM or a desktop inside the same 4 GB do not run the 4g profile.** Docker Desktop, WSL2 and macOS VMs
are emulation hosts only, and every number measured there is labelled **emulated** (§7.4, ADR 0020).

## 3. Memory arithmetic per profile

§7.4: steady-state headroom of at least `max(15% of system memory, 1 GB)`. Everything outside the model runtime (the operating system, the AlixAI
core with its SQLite caches, a parser worker while it runs, the vector cache) is the **allowance**; what is left is the budget of the LocalAI
container, all its processes together, **at its measured peak**.

| Profile (compose file) | System memory | Required headroom | Left for everything | Allowance outside the container | **Container budget** (peak, all LocalAI processes) | Basis |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| `experimental-4g` (`compose.4g.yaml`: 4 GiB, 4 CPU) | 4096 MiB | 1024 MiB (the 1 GB floor binds; 15% is 614) | 3072 MiB | 1024 MiB | **2048 MiB** | A, P |
| `lite` (`compose.lite.yaml`: 8 GiB, 4 CPU) | 8192 MiB | 1229 MiB (15%) | 6963 MiB | 1152 MiB | **5811 MiB** | A, P |
| `balanced` (`compose.balanced.yaml`: 16 GiB, 8 CPU) | 16384 MiB | 2458 MiB (15%) | 13926 MiB | 1792 MiB | **12134 MiB** | A, P |

The allowance is built bottom-up in section 4.2 from the caps of the components that live outside the container, and it is an **assumption**,
the one weak number of the table: only the core, the store, the queue, the index and the parser have a measured figure (all on the Windows dev box,
basis D), and the operating system row is an estimate (E). Nobody has measured a real 4 GB machine (blocker P8 in `docs/evidence/m0-4gb/report.md`).
M0-13 judged Lite at 5.8 GiB (5939 MiB) with a flat 1 GiB allowance; the bottom-up allowance is 1152 MiB, so the Lite line is 5811 MiB. No measured
candidate is within 2 GiB of either number.

A profile **passes the memory line** when, under the full Lite workload of §7.4, the peak of `U = max(R, W)` of the LocalAI container stays at or
below its container budget. R is the summed resident set of the LocalAI processes (anonymous plus file-backed mapped pages); W is cgroup
`memory.current` minus `inactive_file` (what `docker stats` shows). R is the figure that matters for a memory-mapped model: the weights are
file-backed pages that the cgroup counts as inactive file cache while the backend is using them, so W understates what the model needs.

## 4. Memory budgets per component

All values are MiB at peak. "Container" rows must add up to at most the container budget of section 3; "outside" rows must add up to at most
the allowance. Measured figures are from the [4 GB report](../evidence/m0-4gb/report.md) (run tags in brackets) and are **emulated**.

### 4.1 Inside the LocalAI container

| Component | `experimental-4g` | `lite` | `balanced` | Basis and evidence |
| --- | ---: | ---: | ---: | --- |
| LocalAI service process (Go, no model) | 150 | 150 | 200 | M: 125 idle, 146 under load, all four runs; M2-15: peak 160 under the RAG sweep, so the row needs 160 |
| **Controller backend**: weights, KV cache, compute buffers, sampler, at the profile's token caps | **<= 1650** | **<= 3500** | **<= 6000** | M for 4g, ceilings P. Measured backend peak at context 4096 in the 80-case run: Granite 350M 704, Qwen3.5-0.8B 1489, Granite 1B 2498, Qwen3.5-2B 2568 |
| Embedder backend (Granite Embedding 97M Q4_K_M) | not resident | <= 450 | <= 450 | M: 390 alone (M0-13); about +250 in summed resident set beside the controller, `...-plus-embedder-4g-max2` |
| STT worker (voice pack) | not allowed | <= 500 | <= 1000 | E: whisper.cpp base about 388 MiB, small about 852 MiB (ADR 0015, upstream sizes). M4-06 measures |
| TTS worker (voice pack) | not allowed | <= 300 | <= 600 | E: Piper voice 64 MB file; Kokoro-82M. M4-06 measures |
| Reranker | not allowed | off; a bounded job <= 500 | <= 800 | E, P. M2-09 measures |
| Decision-layer scorer (milestone D) | not allowed | off; a small scorer only if measured <= 300 | <= 600; CLM-8B not allowed | P. CLM-8B has a frozen Qwen3-8B encoder and is a higher-memory candidate (ADR 0029). No registered candidate meets this row yet: section 4.4 |
| Spare (variance, a longer prompt, thread buffers) | >= 248 | >= 911 | >= 2484 | arithmetic: budget minus the rows above |
| **Sum of the rows above, controller at its ceiling** | **1800 of 2048** | **4900 of 5811** (5400 with the reranker job) | **9650 of 12134** | |

ADR 0020 sized the same three rungs from model cards at about 350, 600 and 1300 MiB (basis A). Measured backend peaks are 704, 1489 and 2498 MiB, **1.9 to 2.5
times higher**: the arithmetic had the KV cache right (measured 11.6 KiB per token against its 12) and missed the compute buffers and sampler state (360 MiB
at load and 940 MiB under load on the 0.8B), and the backend's mapped libraries; the LocalAI service process adds another 125 to 146 MiB on top. Use the measured column, not the card.

The controller row is the number a model has to meet to be a candidate for the profile. Measured against it (4 GiB cap, `U` is the judged
figure of section 3, bar 2048; backend peak is the figure to compare with the row):

| Model | Backend peak (R) | `U` peak, all LocalAI processes | Fits the 4g row (<= 1650) | Fits the 4g container budget (<= 2048) |
| --- | ---: | ---: | --- | --- |
| Granite 4.0 350M | 704 | 848 | yes | yes |
| Qwen3.5-0.8B | 1489 | 1634 | yes (161 spare) | yes |
| Granite 4.0 1B | 2498 | 2642 | **no** | **no** |
| Qwen3.5-2B | 2568 | 2714 | **no** | **no** |

### 4.2 Outside the container (the allowance)

| Component | `experimental-4g` | `lite` | `balanced` | Basis and evidence |
| --- | ---: | ---: | ---: | --- |
| AlixAI core process (Go, SQLite caches included) | 96 | 160 | 256 | D: 23 working set / 55 private idle (`core-footprint.json`); store profile Small +19 (4g), Default +54 (lite, balanced) at peak (M0-05); job queue about +3 (M0-06) |
| Vector index cache (`cache_mib`) | 0 (streamed; 25,000 vectors per workspace) | <= 64 (100,000 per workspace, 250,000 per deployment) | <= 128 | D: 0.38 KiB per hot vector, 37 MiB at 100,000 (M2-07) |
| Parser worker, one disposable process at a time (hard budget, `GOMAXPROCS=1`) | 256 | 256 | 512 | D: 123 MiB resident for an 8 MiB result, no format above 152 MiB (M2-04); the default 512 is a safety margin, the floor is 64 |
| Operating system, container runtime, everything else | <= 600 | <= 600 | <= 900 | E. Not measured |
| **Sum** | **952** | **1080** | **1692** | |
| **Allowance used in section 3** | **1024** | **1152** | **1792** | sums rounded up; spare 72, 72, 100 |

The spare of the allowance is under 100 MiB in every profile. On 4g it is 72 MiB with the parser worker running at its cap and the operating
system at 600 MiB, neither of which anybody has measured on a 4 GB machine.

### Light External mode (no local LLM)

Light mode = every model external. Local peaks that still apply: outside-container rows above (AlixAI core ≤ 160 MiB,
≈ 55 idle; vector index cache ≤ 64 MiB; parser worker ≤ 256 MiB one at a time) plus, when used, LocalAI service ≈ 150 MiB,
embedder Granite 97M ≤ 450 MiB (390 measured), and VAD ≈ 41 MiB. Peak of the largest light setup is about 1.5 GB; the
recommended host ceiling is **4 vCPU / 8 GB**. Local Whisper tiny / TTS / larger STT are **not** light mode (see the
guide). Host sizes: [light External deployments — Hardware](guides/light-external.md#hardware). Emulated; not certified.

### 4.3 What the co-residency measurement says (rule R3)

Controller (Qwen3.5-0.8B) and embedder (Granite Embedding 97M Q4_K_M) taking turns the way a RAG turn does (embed the question, then answer), six
rounds, 4 GiB cap (the raw samples are not published):

| `LOCALAI_MAX_ACTIVE_BACKENDS` | Chat after embed, median / max | Embed after chat, median / max | Major page faults in the 137 s window | Resident after both ran |
| --- | --- | --- | --- | --- |
| 1 (the shipped default) | **15.2 s / 17.0 s** (warm: 2.7 s) | **6.2 s / 7.0 s** (warm: 0.06 s) | 9,676 (70 per second) | one backend at a time, `U` falls to 149 MiB between calls |
| 2 | 3.1 s / 3.2 s | 0.06 s / 6.1 s (first load only) | 57 | both, `U` 1333 MiB at these small prompts |

With one active backend every call of a RAG turn reloads a model: 12.5 s of reload for the controller and 6.2 s for the embedder, about 18 s of
nothing but loading per turn, and 70 page faults per second of weight re-reading. That is the "repeated model eviction and reload on ordinary
turns" that §7.4 forbids. With two active backends nothing is evicted, and the memory adds up: the controller alone peaked at 1634 MiB in the
80-case run, the embedder adds about 250 MiB, so a controller plus a resident embedder is **projected** at about 1900 to 2020 MiB at the bench
prompts (arithmetic from two measurements, not a measurement), against the 2048 line, and a 3,000-token prompt would take the controller alone to
1942 MiB. This is why the 4g profile declares no resident embedder (R2): retrieval on 4g is lexical (FTS5) at query time, and the embedder runs only in
a serialised ingestion window, never while a turn is in flight (R8).

### 4.4 The decision layer (milestone D, task D-11)

The decision layer is optional, bounded, non-generative and non-authoritative (ADR 0029). Its scorer only ranks or scores a bounded set of
authorised candidates and is advisory: it never authorises anything. This section is the budget half of the registry entries `clm-v0.1-8b`,
`qwen3-8b`, `laya`, `laya-multilingual` and `gliner2.5-decide` (`models/registry/`). Every figure is arithmetic from a pinned file (basis A) or a
labelled vendor statement, and **nothing here has been measured by AlixAI**. Review date 2026-10-08.

**The zero-model option is the floor and the default.** Deterministic rules, the typed forms and the controller (rule R10) cost no resident memory.
With the layer off, unavailable, not admitted or failing, every flow produces the same outcome through the deterministic controller. The registry has no
`default_for` for the `decision-scorer` role on any profile, so `Registry.DefaultFor` answers "no model" and the deterministic baseline serves. The loader
refuses a scorer default unless the entry is qualified-emulated or certified, was measured on that profile, cites its results and has a measured peak inside
the profile's row. No entry has that evidence yet.

| | `experimental-4g` | `lite` | `balanced` | `high` (32768 MB, 8 CPU, `tools/decbench`) |
| --- | --- | --- | --- | --- |
| Default decision path | deterministic baseline | deterministic baseline | deterministic baseline | deterministic baseline until D-08 evidence |
| Scorer allowed | never (rule R2; the loader refuses a default) | optional, one small scorer, only if its measured peak is <= 300 MiB | optional, <= 600 MiB; a scorer with a frozen encoder is not allowed | optional; CLM-8B is judged here only |
| Memory reserve for the scorer | 0 | 300 MiB inside the container budget (section 4.1 row) | 600 MiB inside the container budget | no row yet: set from the D-08 measurement of the head plus the encoder |
| Load behaviour | none | demand-loaded for the call, unloaded after idle (rule R3.3); never made resident to save a reload | demand-loaded, or resident only if the measured sum fits | resident or demand-loaded, decided by D-08, which evaluates both |
| On a failed admission | typed fallback `resource_gate`, deterministic path | same | same | same |

**D1. A scorer must not silently evict the controller, retrieval or speech services.** Rule R3.2 applies unchanged: the scorer is admitted only when
the projected resident memory (the incoming model's measured `memory.peak_rss_mb` plus the current measured use, and for a bi-encoder the encoder's peak as
well) fits the container budget of section 3. A scorer whose registry entry has no measured peak cannot be admitted, because the projection has nothing to
use: that is the fail-closed answer, not an error. A failed admission is the typed fallback reason `resource_gate`. The controller, the embedder and the
speech workers are not touched, `LOCALAI_MAX_ACTIVE_BACKENDS` is not raised to make room, and the count of resident-role reloads stays 0 (R3.5). A scorer
that needs a second runtime process (the CLM sidecar and its encoder) is bounded by its own container limits (`deploy/container/clm`), never by shrinking
the LocalAI container.

#### Registered scorers, sized from the pinned files

| Registry id | Kind | Pinned file | Weights | Fits the 300 MiB lite row | Option count | Licence | Runtime support (unverified unless stated) |
| --- | --- | --- | ---: | --- | --- | --- | --- |
| `laya` | non-autoregressive ModernBERT-large encoder, 421M parameters, English | `model.safetensors` at `7b928d82`, F16 | 803 MiB (842609210 B) | no | about 20 (vendor limit; unverified) | Apache-2.0 (card metadata; no LICENSE file) | LocalAI gallery `laya-vllm-cpp`, about 800 MB per LocalAI docs; not run by AlixAI |
| `laya-multilingual` | non-autoregressive mmBERT-base, 322M parameters, 100+ languages including Italian (not qualified) | `model.safetensors` at `1720e3e3`, F16 | 614 MiB (643835514 B) | no | about 20 (assumed from the English figure; unverified) | Apache-2.0 (card metadata; no LICENSE file) | no separate LocalAI gallery entry; unverified |
| `gliner2.5-decide` | non-autoregressive DeBERTa-v3-large span extractor, English; 340M per the card, 486M F32 per the hub | `model.safetensors` at `5a7adf72`, F32 | 1856 MiB (1945828140 B) | no | not stated (unverified) | Apache-2.0 (card; no LICENSE file) | LocalAI gallery `gliner25-decide-vllm-cpp`, about 2 GB; not run by AlixAI |
| `clm-v0.1-8b` | bi-encoder: state and action heads on the frozen `qwen3-8b` encoder, cosine similarity | `CLM_v0.1-8B.pt` at `e939398d` (PyTorch pickle) | 72 MiB head | the head yes, the encoder no (below) | not stated for CLM; LocalAI's limit of 26 is for letter-based models and does not apply | Apache-2.0 (LICENSE file) | CLM sidecar (D-06); LocalAI gallery `clm-v0.1-8b-vllm-cpp`, about 16.5 GB, about 19 GB free RAM on CPU per LocalAI docs |
| `qwen3-8b` (encoder dependency) | BF16 decoder-only model used as a frozen encoder, 8190735360 parameters | 5 shards at `b968826d` | 15622 MiB (16381516776 B) | no | n/a | Apache-2.0 (LICENSE file) | vLLM in the sidecar |

Sources, all reviewed 2026-10-08: the Hugging Face hub API for each repository at the pinned commit
(`https://huggingface.co/api/models/<repo>/tree/<revision>`), the model cards, the CLM repository (reviewed 2026-10-02) and the LocalAI Decisions page
`https://localai.io/docs/features/decisions/`. The vendor latency and accuracy figures (Laya 32.8 ms on a T4 and 193 to 464 ms on CPU, GLiNER2.5-Decide 60.1%
average, CLM about 28 ms on an RTX 4090) were measured by the vendors on other hardware and workloads and are not evidence for any profile.

**What the arithmetic says.** Every registered candidate's weights alone are above the 300 MiB lite row and the 600 MiB balanced row. The smallest is
`laya-multilingual` at 614 MiB, which is over 600 as well. So no scorer can be admitted on lite or balanced today. The row was written before any
candidate was sized; raising it takes an ADR and a re-run of the 4g experiment (R9), so it is left as it is and the decision layer stays **off** on lite
and balanced until a smaller or quantised serving form is registered and measured. LocalAI's native llama.cpp decisions path for decision GGUFs (for example
`ggml-org/Kev-0.8B-GGUF`) is a lead for such a form and is unverified. For CLM-8B the BF16 encoder (15.6 GiB) plus the head is about 15.7 GiB of weights
before activations: it fits neither the lite container budget (5811 MiB) nor the balanced one (12134 MiB), only the `high` profile. A quantised encoder is a
different artefact with its own registry entry and its own CLM evidence (D-08); no such entry exists.

#### Researched and not registered

| Scorer | Why not an entry (2026-10-08) |
| --- | --- |
| Tev1 0.8B (`togethercomputer/Tev1-0.8B-experimental`, revision `6bb2dff1`, 873438784 BF16 parameters, about 1.8 GB) | autoregressive letter scorer (at most 24 options per LocalAI); the card metadata declares no licence and the registry requires one, so it cannot be recorded; unverified |
| Tev1 4B | about 9.3 GB per LocalAI docs; not sub-1B |
| kev 0.8B (`jaredpalmer/kev-0.8b`, revision `bf75a6a8`, Apache-2.0) | a LoRA adapter plus `head.pt` over `Qwen/Qwen3.5-0.8B-Base`, not a standalone artefact; LocalAI serves a converted vllm.cpp checkpoint of about 1.53 GB; the registry records one file and no conversion is pinned |
| Nimble 9B | 9B parameters, about 19.3 GB, about 20 GB free RAM on CPU per LocalAI docs; not sub-1B; the same fit as CLM-8B |

Re-evaluate when a registered scorer gains a smaller serving form, when LocalAI's llama.cpp decisions path is verified on the pinned release, or by
2027-01-08.

## 5. CPU and disk budgets

### 5.1 CPU

| | `experimental-4g` | `lite` | `balanced` | Basis |
| --- | --- | --- | --- | --- |
| CPU cap of the LocalAI container (cgroup quota; `LOCALAI_THREADS` equal to it) | 4 | 4 | 8 | `compose.<profile>.yaml` |
| Interactive turn (prefill and decode) | all of the cap: 3.5 to 3.9 cores used | same | same | M: `cpu.stat` over the 80-case runs |
| Model resident and idle | <= 0.1 core | same | same | M: 0.0 core (resolution 0.1) over a 90 s idle window, every run |
| AlixAI core, mean | <= 0.25 core; the deterministic turn pipeline is 9 to 22 ms of one core | same | same | D (M1-14); one 20 s sample, 0.33 CPU-seconds in total (`core-footprint.json`) |
| Background jobs (class 5 and 6), all together | <= 1 core, one at a time | <= 1 core, one at a time | <= 2 cores | P; parser workers `GOMAXPROCS=1` (M2-04) |
| CPU-quota throttling | <= 5% of the 100 ms periods | same | same | M: 1.5 to 2.3% in the four runs (36 to 212 throttled periods in windows of 248 to 1083 s) |

Power and thermal load were not measured (no instrument on the dev box); CPU-seconds per turn is the proxy.

### 5.2 Disk

| Item | `experimental-4g` | `lite` | `balanced` | Basis |
| --- | ---: | ---: | ---: | --- |
| LocalAI image (uncompressed, in the container store) | 829 MB | 829 MB | 829 MB | M: `docker image inspect` |
| Backend `cpu-llama-cpp` (`data/backends`) | 162 MB | 162 MB | 162 MB | M |
| Controller model file | <= 600 MB (0.8B: 533 MB) | <= 1.5 GB (1B 1.02 GB, 2B 1.28 GB) | <= 3 GB | M for the files, P for the ceilings |
| Embedder file | 105 MB, installed for ingestion | 105 MB | 105 MB | M |
| Voice packs | none | <= 600 MB each | <= 2 GB each | E |
| `alixai` binary | 27 MB | 27 MB | 27 MB | D (Windows, `CGO_ENABLED=0`) |
| Store: one vector | 586 bytes (55.9 MiB per 100,000) plus its chunk and FTS rows | same | same | D (M2-07) |
| WAL after checkpoint | <= 64 MiB | <= 64 MiB | <= 64 MiB | D (M0-05 `journal_size_limit`) |
| Audit | about 1.65 KB per policy decision, about 8 KB per model-backed turn of a 3-capability package; grows with the catalogue | same | same | D (M1-14); the audit write amplification is flagged there |
| Container log | <= 10 MB | <= 10 MB | <= 10 MB | P: the shipped compose sets no log cap; `hardening-live.md` used 10 MB in an experiment |
| Install before data and documents, controller at its ceiling | about 1.7 GB | about 2.6 GB | about 4.1 GB | sum of the rows above |
| Free space to provision | 8 GB | 20 GB (§7.1) plus model and document capacity | 40 GB | P for 4g and balanced |

## 6. Token budgets

Everything here is in **estimated tokens** (`providers.EstimateTokens`, the unit the builders and `providers.WithBudget` enforce) unless a column says
"real", meaning the runtime's own `prompt_tokens`.

### 6.1 The estimator against the runtime's count

Measured twice: on the 960 bench cases of M0-13 and M0-15 (`token-calibration.json`, runtime `toks_in` against the estimate of the same request) and on
the 19 real prompts of the dialogue engine (`measured-prompts-*.json`, built by a throw-away test on a copy of the tree, `prompts/README.md`).

| What is compared | Real / estimated |
| --- | --- |
| Message text only (bench with no output constraint) | **0.75 to 0.83** (Qwen3.5), **0.70 to 0.79** (Granite 4.0) |
| Engine response prompts (no constraint), 3 prompts | 0.75 to 0.76 |
| Engine proposal prompts, the estimate counting the output constraint | **0.49 to 0.57** (19 prompts; Italian 0.49 to 0.55) |
| Output constraint alone | about 270 to 535 estimated tokens (800 to 1,600 bytes of schema), **zero real prompt tokens**: the runtime turns it into a grammar |

So the estimator is 20 to 43 percent above the real count for message text, as its comment says, and counts the constraint as prompt although the model
never reads it. Both errors are on the safe side and neither is to be "corrected" without a tokenizer (R5). The Granite tokenizer produces 3 to 6
percent fewer tokens than Qwen's on English prompts and 6 percent more on Italian (the 5-capability Italian prompt: 570 against 537).

The cost of one shortlisted capability, measured on the engine's prompts (no history): about **75 estimated tokens, 36 real** (1, 2, 3, 5, 8 and 12
capabilities: 787, 887, 971, 1076, 1292 and 1618 estimated; 391, 436, 483, 530, 644 and 787 real). A goal and four turn rows add about 195 estimated, 170
real. A typical proposal turn is therefore 400 to 960 real prompt tokens, the response call 250 to 280.

### 6.2 Caps per profile

The prompt, history, shortlist and utterance rows are the provisional table of M3-03 (`internal/dialogue/context/profile.go`), confirmed against
the measured sizes above; no value changes, so `profile.go` needs no edit. The Lite row is the one M1-14 proposed (2,048 prompt, 256 output, shortlist
5, ceiling 12) with measured values now behind it.

| Cap | `experimental-4g` | `lite` | `balanced` | `performance` | Basis |
| --- | ---: | ---: | ---: | ---: | --- |
| **Prompt, hard cap** (est tokens; messages, framing and output constraint) | 1536 | **2048** | 4096 | 8192 | P (M1-14, M3-03). Largest measured need: 12 capabilities, goal and four rows, **1814 est / 957 real**: it fits Lite and does not fit 4g |
| Same cap in real tokens, worst measured ratio (0.57 proposals, 0.83 text) | <= 880 (proposal) / <= 1275 (text) | <= 1170 / <= 1700 | <= 2340 / <= 3400 | <= 4670 / <= 6800 | arithmetic from 6.1 |
| **Output cap per call** (generated tokens) | 192 | **256** | 384 | 512 | P. Measured: proposal 57 to 156 real tokens for 1 to 5 capabilities, 235 to 248 for 12, **as the gateway sends it today** (the runtime drops the gateway's schema, M0-13 F1); with a grammar the 80-case runs gave mean 95, p95 134 and 4 runaway outputs at the 256 cap (Qwen3.5-0.8B); responses 9 to 22 tokens |
| Shortlist, default / hard ceiling | 3 / 5 | **5 / 12** | 8 / 12 | 12 / 12 | P (M1-14, M3-03). Cost 75 est per capability |
| Output constraint ("tool schema"), est tokens | 380 | 540 | 540 | 540 | the constraint of 5 capabilities is 368, of 12 is 535 (measured); counted in the prompt cap |
| History (turn rows, capsules, goals set aside), est tokens | 224 | 384 | 1024 | 2048 | M3-03; measured plateau at 500 turns: 1,435 est at Lite |
| User message, runes (longer is rejected, never clipped) | 600 | 1000 | 2000 | 4000 | M3-03 |
| Response prompt (directive facts and rules), est tokens | 512 | 512 | 768 | 1024 | measured 330 to 369 est, 249 to 278 real; P for the cap |
| Configured context window, tokens (R6) | 4096 | 4096 | 8192 | 8192 | M: all runs at 4096. 2048 is enough for 4g's caps and saves 22 MiB (KV is 11 to 12 KiB per token on Qwen3.5-0.8B, measured: 989 MiB at 2048, 1012 at 4096, 1156 at 16384) |

**Where each cap is enforced, and where it is not yet.** Prompt, output, history and utterance caps: `internal/dialogue/context` (`Profile`, M3-03) and
`providers.WithBudget` (M0-07) exist, and the engine of M1-14 enforces its own defaults until it adopts the profile. Not enforced anywhere today, each named
for the task that has to wire it: the output-constraint cap and the per-profile shortlist ceilings (the engine that adopts `dctx.Profile`; M1-08's router has a
single hard ceiling of 12), the response-prompt cap (`buildResponse`, M1-14's follow-up), the configured context window (registry `context.budget_tokens` and the
model definition in `deploy/container/localai`, M0-18), every RAG cap of 6.3 (`internal/rag/query`, M2-09), and the resident-role and admission checks of R3.

The Lite prompt budget is **decision-layer independent**: the numbers above hold with the optional decision layer on or off, and enabling it may
only make a prompt smaller (narrowing the shortlist from 5 to 2 capabilities: 887 estimated tokens instead of 1076, M1-14) and never raises a cap.
The layer's own model, if any, is an optional-component row of section 4.1 and obeys R3.

### 6.3 RAG caps (for M2-09 and M2-10)

The evidence is carved out of the same prompt cap; the answer prompt is the response frame (about 330 est) plus the question and history plus the
evidence plus the source list.

| Cap | `experimental-4g` | `lite` | `balanced` | `performance` | Basis |
| --- | ---: | ---: | ---: | ---: | --- |
| Evidence, est tokens | 480 | 1000 | 2000 | 4000 | P. Check, Lite: 330 + 384 (history cap) + 1000 = 1714 of 2048 |
| Evidence items in the package | 3 | 4 | 6 | 8 | P |
| Parent expansion | off | at most 1 parent of 300 est | at most 2 of 500 | at most 3 of 800 | P |
| Candidates per retrieval leg (FTS, vector) before fusion | 12 | 24 | 48 | 96 | P; the index already keeps at most 16 query terms (M2-06) |
| Candidates re-scored by an optional reranker | not allowed | not allowed | 20 | 40 | P |
| Retrieval on 4g | lexical (FTS5, reduced semantic recall labelled) at query time; the embedder only in a serialised ingestion window | hybrid | hybrid, optional rerank | hybrid, optional rerank | rule R2, section 4.3 |

At Lite a RAG answer prompt of the cap size is about 1,300 real tokens (evidence is plain text, ratio about 0.76), which the 0.8B prefills in about 11
seconds at the measured 118 tokens per second (section 7.1): computed from the measured rate, not measured with a RAG prompt.

## 7. Latency budgets (provisional)

M0-13 left the latency gate to this file. The numbers below are **proposals** (basis P) written from the measured rates; they are not user research.

### 7.1 Measured rates (Qwen3.5-0.8B, 4 CPUs, 4 GiB cap, emulated; other jobs ran on the host, so each is indicative)

| Real prompt tokens | TTFT median (s) | Prefill (tok/s) | Decode (tok/s) |
| ---: | ---: | ---: | ---: |
| 128 | 1.2 | 106 | 17.5 |
| 273 | 2.2 | 123 | 18.4 |
| 508 | 4.1 | 122 | 16.4 |
| 1025 | 9.2 | 112 | 12.3 |
| 2012 | 18.3 | 110 | 12.2 |
| 3046 | 27.0 | 113 | 13.6 |

Prefill is linear at **106 to 144 tokens per second** (the later sweeps of the same configuration gave 141 to 144, so the host's load alone moves it by
about 20 percent), decode is **12 to 18 tokens per second** and slows with a long prompt. Decode speed over the 80-case runs, other models: Granite 350M 28.9,
Granite 1B 9.4, Qwen3.5-2B 8.3 tokens per second. A prompt of the Lite cap (957 real tokens) therefore waits about 8 seconds for its first token on the
0.8B; **a prefix the runtime has already seen is not recomputed**: the same prompt sent again answered its first token in 0.14 to 0.34 s
(`measured-prompts-*.json`). Keeping the capability frame and the system rules first and stable between turns is the cheapest latency lever there is.

### 7.2 Budgets

| Path | `experimental-4g` and `lite` | `balanced` | Basis |
| --- | --- | --- | --- |
| Deterministic turn (no model call) | p95 <= 100 ms | p95 <= 100 ms | D: 9 to 22 ms (M1-14) |
| Proposal call (internal, not shown to the user) | p95 <= 20 s at a cap-sized prompt and the output cap | p95 <= 10 s | M: 80-case runs, 0.8B p95 14.4 s at 326 prompt tokens; a 957-token prompt plus 100 output tokens is about 14 to 16 s |
| Response call (streamed to the user) | time to first token p95 <= 5 s, whole call p95 <= 10 s | 3 s / 6 s | M: 250 to 280 real tokens prefill in 2.2 s |
| A whole model-backed turn (two calls) | p95 <= 35 s | p95 <= 20 s | P: sum of the above with margin. Template-only responses (`TemplateOnly`) make it one call |
| First request after a cold start, model load included | <= 60 s, the gateway's call budget | <= 60 s | M: 5.8 to 14.6 s (Docker VM page cache dropped, Windows host cache warm); M0-13 saw 50 s for a first-ever load |
| Time from start to ready | the service reports the controller ready only after one warm request succeeded | same | M0-13 F4 and F5: `/readyz` says nothing about inference |

The pre-registered 4g gate of `docs/evidence/m0-4gb/report.md` (call p95 <= 20 s and time to first token p95 <= 5 s on the bench prompt of about
326 tokens, cold <= 60 s) is the same budget applied to a short prompt. Qwen3.5-0.8B passes it (p95 14.4 s, TTFT p95 4.1 s), Granite 350M and 1B pass
it, Qwen3.5-2B fails it (TTFT p95 6.7 s). A proposal prompt of 650 to 960 real tokens does not meet a 5 second time to first token on the 0.8B,
which is why the proposal call is budgeted by its whole duration and the 5 second line is for the response call.

### 7.3 Cold start and eviction

| Event | Measured | Rule |
| --- | --- | --- |
| Boot to `/readyz`, model not yet loaded | 7 to 20 s | the readiness probe must exercise a model (M0-13 F5) |
| First request, model loaded from disk | 5.8 s (350M), 8.6 s (0.8B), 11.0 s (1B), 14.6 s (2B) | within 60 s with room to spare on this host; M0-13's 50 s was a first-ever load of a file not yet in any cache |
| A request after an eviction (one active backend) | controller 12.5 s extra, embedder 6.2 s | R3: never on an ordinary turn |

### 7.4 Concurrency

One slot (`parallel` 1): concurrent requests queue, they do not run side by side. Measured with unique 700-token prompts and 64 output tokens
(`concurrency-*.json`): one request 7.1 s; two at once 9.1 s median and 15.6 s worst, 14.7 s wall; four at once 15.2 s median, 31.7 s worst, 29.4 s
wall. Latency grows linearly with the number of sessions, so "1 concurrent interactive session by default" (§7.1) is a physical limit of the
4-CPU profile, not a configuration choice, and the whole-turn budget of section 7.2 holds for one session only.

## 8. Checking a feature against this file

1. Say which rows it touches (a resident model, a worker, a cap, a job) and what it adds to each, in `docs/efficiency/<task>.md` (R9).
2. Measure it under `compose.4g.yaml`: run the 80-case bench (`go run ./tools/bench run --profile 4g ...`, see
   [benchmarks/README.md](../../benchmarks/README.md)) with the cgroup, page-fault and CPU sampling, and judge `U`, headroom, swap, faults and the
   pre-registered bar as the [4 GB report](../evidence/m0-4gb/report.md) does. Results must pass `go run ./tools/bench check`.
3. Compare with the row. Above it, shrink the feature or raise the row through an ADR; a feature that would break R3 (cause an eviction of a resident
   role) or R7 (need swap) does not ship in that profile.
4. Label every number emulated until a real machine of the profile has run it.

## 9. Not measured, owed

| Gap | Owner |
| --- | --- |
| A real 4 GB machine; the operating system and container-runtime memory; ARM64; the whole workload (conversation corpus, RAG evaluation, injection suite, a concurrent indexing run, 24-hour mixed load, 7-day soak) | M5-09, M5-H04, ADR 0020 criteria |
| Memory and latency of a RAG answer prompt, of the embedder under load, of voice workers, of the reranker, of any decision-layer scorer | M2-15, M4-06, milestone D |
| A controller that passes the quality bar, so that the controller row has a real occupant | M0-17, M5-09 |
| ADR 0020's sizing table (350 / 600 / 1300 MiB) is 1.9 to 2.5 times below the measured backend peaks (section 4.1) and has to be replaced by the measured values | M5-09 |
| `LOCALAI_MAX_ACTIVE_BACKENDS` defaults (lite 2, balanced 4, 4g 1) and the startup guard that refuses a value below the number of resident models: done in M0-18; an unset value (it lives in the LocalAI container) is only noted when more than one model is resident | closed by M0-18 |
| The Lite and Balanced controller ceilings (3500, 6000) have no measured occupant above the 2B | M5-09 |
| CPU frequency, power and thermal behaviour of a sustained load | M5 |
| What attributes the backend's anonymous growth from 360 MiB at load to 940 to 1270 MiB under prompts (prompt length, the grammar sampler on a 248K-token vocabulary): not isolated | M0-17 |
