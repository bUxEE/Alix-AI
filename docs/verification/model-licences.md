# Models and runtime: licence and supply-chain review

Status: **maintainer review, not legal advice.** Done by the project maintainer, not by a lawyer or an independent reviewer.
Upstream pages were checked on 2026-10-25, at the revisions AlixAI pins. Read the upstream licence yourself before you
deploy, and ask a lawyer if your use depends on it.

Back to the [verification index](README.md). The decision-layer components (CLM-v0.1-8B, Qwen3-8B, vLLM, the scorers) are in
[decision-layer-licences.md](decision-layer-licences.md).

## 1. How AlixAI obtains models and the runtime

- **AlixAI never redistributes model weights or the LocalAI image.** The repository and the release artifacts contain none
  of them.
- Each model has a registry entry (`models/registry/<id>.yaml`) that pins the **upstream repository, the quantised file's
  repository, commit revision, file name, size and sha256**, plus the declared licence, who quantised it, and whether that is
  the original publisher (`first_party`). The release manifest lists the same pins.
- The **operator** fetches the files. The AlixAI service hashes a model file before use and refuses one whose sha256 does
  not match the registry, or whose registry status does not allow it.
- LocalAI is pinned **by image digest**; its llama.cpp backend is pinned by digest in a local, read-only backend catalogue,
  and LocalAI's own galleries and auto-install are turned off, so it downloads nothing that is not pinned.

The licence terms below bind **the operator** who downloads and runs the files.

## 2. Models

Status column: `qualified-emulated` means it passed its bar on emulated hardware and may be used; `rejected` means it failed
the controller bar and the service refuses it; `candidate` means pinned but not qualified. No controller model is qualified
in 1.0 ([ADR 0010](../decisions/0010-lite-model-selection.md)).

| Model (registry id) | Status | Original | Quantised file | Licence (as declared) | Where the licence was seen | Notes |
| --- | --- | --- | --- | --- | --- | --- |
| Granite Embedding 97M Multilingual R2 (`granite-embedding-97m-multilingual-r2-q4_k_m`) | **qualified-emulated**, the default embedder | `ibm-granite/granite-embedding-97m-multilingual-r2` at `835ad140` | Q4_K_M by **mykor** (third party) at `45ce642d`, sha256 `355f1f30…6daf` | Apache-2.0 | IBM card metadata; the GGUF card also declares Apache-2.0 and names the IBM base; no LICENSE file in either repository | IBM publishes no GGUF of this model. The conversion's provenance is not stated by its publisher; another repository publishes byte-identical files, which corroborates it but is not an IBM attestation. Converting it yourself with llama.cpp is the alternative |
| Granite 4.0 350M (`granite-4.0-350m-q4_k_m`) | rejected | `ibm-granite/granite-4.0-350m` at `bd8a1497` | Q4_K_M by IBM (first party) at `b8208a86` | Apache-2.0 | card metadata and text in all repositories; no LICENSE file | not gated; no extra use policy |
| Granite 4.0 1B (`granite-4.0-1b-q4_k_m`) | rejected | `ibm-granite/granite-4.0-1b` at `6a7381ba` | Q4_K_M by IBM (first party) at `b27c2fe3` | Apache-2.0 | as above | as above |
| Qwen3.5-0.8B (`qwen3.5-0.8b-q4_k_m`) | rejected | `Qwen/Qwen3.5-0.8B` at `2fc06364` | Q4_K_M by **unsloth** (third party) at `6ab46149` | Apache-2.0 | LICENSE file in the Qwen repository ("Copyright 2026 Alibaba Cloud"); the unsloth card links back to it | the card's intended-use note ("prototyping, task-specific fine-tuning, and other research or development purposes") is guidance, not a licence term |
| Qwen3.5-2B (`qwen3.5-2b-q4_k_m`) | rejected | `Qwen/Qwen3.5-2B` at `15852e8c` | Q4_K_M by **unsloth** at `f6d5376b` | Apache-2.0 | as above | as above |
| MiniCPM5-1B (`minicpm5-1b-q4_k_m`) | rejected | `openbmb/MiniCPM5-1B` at `87179e5c` | Q4_K_M by openbmb (first party) at `3d55fac8` | Apache-2.0 | card metadata and text ("released under the Apache-2.0 License"); the project's GitHub LICENSE is Apache-2.0; no LICENSE file in the model repositories | earlier MiniCPM releases had a separate model licence with commercial registration; no such licence or registration requirement was found for MiniCPM5 |
| EmbeddingGemma 2, text part (`egemma2-text-q8_0`) | candidate; **cannot load** on the pinned LocalAI | `google/embeddinggemma-2` at `914f7f89` | Q8_0 by **ggml-org** (third party) at `bfcd2987` | Apache-2.0, **with a use policy on the card** | card metadata; no LICENSE file; the card's licence link resolves to the Apache-2.0 text | see flag 1 below. Not gated. Not used by any profile |

All model repositories above were checked at the pinned revision. None is gated.

## 3. Flags

1. **EmbeddingGemma 2: Apache-2.0 plus a mandatory use policy.** The card's licence field is Apache-2.0, but its ethics
   section says "Deployments must adhere to the Gemma Prohibited Use Policy". That policy forbids using Gemma or derivatives
   for "making automated decisions in domains that affect material or individual rights or well-being (e.g., finance, legal,
   employment, healthcare, housing, insurance, and social welfare)". Whether the policy binds on top of Apache-2.0 is
   **unclear and not resolved here**. The third-party GGUF card does not repeat the sentence. AlixAI does not use this model
   on any profile; anyone who enables it should read the policy and decide.
2. **Third-party quantisations.** The default embedder (mykor), the Qwen3.5 files (unsloth) and EmbeddingGemma 2 (ggml-org)
   are conversions by someone other than the original publisher. The sha256 pins guarantee you get the bytes that were
   reviewed, not that the conversion is faithful. Where that matters, convert the original weights yourself with llama.cpp
   and pin your own hash.
3. **Licence declared only in card metadata** for every model above except the Qwen ones: no LICENSE file in the model
   repository, so the licence text was not seen there, only the declaration. Apache-2.0 asks redistributors to include the
   licence text; take it from apache.org if you redistribute.
4. **No restrictive licence** (non-commercial, revenue threshold, registration) was found for any model AlixAI uses. A model
   with such a licence (for example one under a revenue-threshold licence) was screened out before benchmarking.

## 4. Runtime

| Component | Pin | Licence | Notes |
| --- | --- | --- | --- |
| LocalAI (`docker.io/localai/localai:v4.10.0`) | digest `sha256:0632c21d…705b` | **MIT** ("Copyright (c) 2023-2025 Ettore Di Giacinto"), LICENSE file at the `v4.10.0` tag | the image is a small core; inference backends are separate OCI images pulled on demand. No NOTICE or third-party licence file at the repository root, only an acknowledgements list in its README |
| LocalAI llama.cpp backend (`v4.10.0-cpu-llama-cpp`) | digest `sha256:d7b0d9c6…2579` | llama.cpp and ggml are **MIT** | llama.cpp vendors some code under its own licences (a `licenses/` directory). The GGUF format is specified in the MIT-licensed ggml repository |

What was **not** reviewed: the operating-system packages and libraries inside the LocalAI and backend images. They are pinned
through the image digests only; their licences and advisories are the image publisher's. The image SBOM AlixAI produces
describes AlixAI's own binaries, not a file-system scan of third-party images.

## 5. Not in this review: voice packs

The optional voice packs (`models/voice-packs/`) pin whisper.cpp models, Piper voices and Silero VAD files, all declared MIT
in their manifests. They were not re-checked for this page. Two open points are already recorded in the manifests: the Piper
**engine** is GPL-3.0 (it runs as a separate backend process; shipping it inside an image creates redistribution
obligations), and the Italian Piper voice was fine-tuned from an English voice whose dataset licence was not reviewed. Treat
voice packs as unreviewed until a later release says otherwise.

### 5.1 Local STT candidates (ADR 0040)

Checked on **2026-10-26** at the revisions the packs pin. Not a qualification and not legal advice; human sign-off still owed.

| Component | Pin | Licence (as declared) | Where seen | Notes |
| --- | --- | --- | --- | --- |
| NVIDIA Parakeet TDT 0.6B v3 (weights) | model card `nvidia/parakeet-tdt-0.6b-v3` | **CC-BY-4.0** | Hub card `license: cc-by-4.0` | AlixAI does not bundle weights |
| Pack GGUF (`tdt-0.6b-v3-q4_k.gguf`) | `mudler/parakeet-cpp-gguf` @ `c77726e46ceaab96b5a5a3328915edb44c73209d`, 675200864 bytes, sha256 `993d73fe…1d5ee8` | inherits upstream CC-BY-4.0 | pack manifest + Hub LFS oid | LocalAI/parakeet.cpp conversion |
| NVIDIA first-party Q8_0 GGUF (not the pack pin) | `parakeet-tdt-0.6b-v3.q8_0.gguf`, 713975456 bytes, sha256 `e3880d0a…f4d32e` | CC-BY-4.0 | Hub file metadata | recorded for operators; not installed by the pack |
| `parakeet.cpp` engine / LocalAI `parakeet-cpp` backend | LocalAI v4.10.0 backend image `v4.10.0-cpu-parakeet-cpp` @ `sha256:372c7135…90bb75` | **MIT** | `mudler/parakeet.cpp` LICENSE | CPU and optional GPU; not part of the pack |
| Whisper large-v3-turbo q5_0 (`whisper-turbo-stt`) | `ggerganov/whisper.cpp` @ `5359861c739e…`, `ggml-large-v3-turbo-q5_0.bin`, 574041195 bytes, sha256 `39422170…ffa7e2` | **MIT** | Hub card `license: mit` | multilingual Whisper; Balanced candidate |
| Whisper large-v3 q5_0 (`whisper-large-v3-stt`) | same repo, `ggml-large-v3-q5_0.bin`, 1081140203 bytes, sha256 `d75795ec…d98ad1` | **MIT** | Hub card `license: mit` | Performance candidate; larger than Balanced STT budget expects |
