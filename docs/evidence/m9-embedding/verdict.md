# M9-05 EmbeddingGemma 2 trial: verdict

Date: 2026-10-08. Task M9-05. Verdict: **NOT LOADABLE under the pinned LocalAI v4.10.0. Stopped early; nothing was benchmarked.**
The harness refuses files named verdict.md, so this is `verdict.md`; the orchestrator renames it to `verdict.md`.

## 1. Dated primary-source findings (all fetched 2026-10-08)

| Claim | Source | Result |
|---|---|---|
| Model exists, Apache-2.0, not gated | huggingface.co/google/embeddinggemma-2 and /api/models (revision 914f7f89142e33e77833254d9c9b90c3cef7303b, created 2026-09-14) | confirmed |
| 740M parameters, text part about 270M | model card: 130M backbone + 140M embedder, plus 170M vision and 300M audio encoders; the text GGUF reports 271,002,648 parameters | confirmed |
| 768 dimensions, Matryoshka to 512, 256, 128 | model card | confirmed |
| 8,192-token window | model card | confirmed |
| 100+ languages | model card (pretraining data over 140) | confirmed as a claim; not qualified by us |
| MTEB multilingual 61.36 | model card: multilingual v2 mean 61.36, English v2 mean 68.46 | confirmed as Google's figure; a vendor claim |
| A GGUF exists | ggml-org/embeddinggemma-2-GGUF (2026-10-06): `embeddinggemma-2-Q8_0.gguf` 309,855,456 bytes sha256 2188ac1d...0b09135, BF16 557,950,176 bytes; also unsloth, prithivMLmods, ngquocvinh conversions | yes (third party) |
| GGUF architecture | header of the pinned Q8_0 file read directly (first 2 MiB): `general.architecture = gemma-embedding2` | confirmed |

## 2. Why it cannot load

- LocalAI v4.10.0 (published 2026-09-17) builds llama.cpp commit `38a5b42d9a3e82e0a586bcd1caed121f36c87a73` (2026-09-15; `backend/cpp/llama-cpp/Makefile` at the tag). Its `src/llama-arch.cpp` has `gemma-embedding` but **no** `gemma-embedding2`.
- llama.cpp added the architecture in PR #30054 "model: support embeddinggemma2 (text+vision+audio)", merged **2026-10-06T16:20:31Z** (commit 4fbc76dec51d0add466f0210855c0596589b60d4), 21 days after the pinned commit.
- LocalAI v4.11.0 (2026-10-02) pins `a868c3e3` (2026-10-01) and LocalAI master pins `bed0a856` (2026-10-02). Both also lack the architecture. No LocalAI release carries it yet.
- Every other published form (safetensors, ONNX, MLX, LiteRT-LM) is not loadable by the llama-cpp backend either, so no form works with the pinned runtime. Bumping LocalAI or building a custom llama.cpp is a runtime change outside this task and would need its own qualification (ADR 0037 style pin review).

The check is a source-level one (architecture string absent at the pinned commit; arch of the file read from its header). A load attempt in a container was not run because it could only add an "unknown model architecture" error to this evidence, and the memory rule makes unneeded containers unattractive.

## 3. What was written

- `models/registry/egemma2-text-q8_0.yaml`: status `candidate`, `default_for: []`, artefact pinned (repo, revision bfcd2987..., size, sha256), Apache-2.0 recorded, `allowed_runtime_versions: []` (fail closed), measurements all null, fallback = Granite Embedding 97M. The hash is the hub's LFS oid; the file was not downloaded in full, so it is not re-hashed locally.
- No default changed. No container was started. No model was downloaded in full.

## 4. Pre-registered rule for the re-run (written now, before any measurement)

Run only when a LocalAI release pins a llama.cpp at or after 4fbc76de. Same protocol as M0-13 and M2 for both models, on Balanced and Lite emulated profiles (cap at most 6 GiB, one container at a time):

1. Loads under the pinned backend, finite vectors, 768 dimensions, text-only (no mmproj).
2. Mean recall@1 over the four M0-13 synthetic sets at least 0.90 and no worse than Granite by more than 0.05 (qualification bar).
3. A follow-up default change for Balanced is justified only if, on the M2 EN/IT corpus through `tools/rageval`, recall@5 is at least 0.03 absolute above Granite 97M in both Italian-to-Italian and English-to-Italian, per-query latency p95 is at most 2x Granite's (about 183 ms), resident memory fits Balanced with the generator (peak below 1.5 GiB), and the one-off re-embed cost (every vector changes: 384 to 768 dimensions, doubled vector storage; ADR 0032, M5-24) is accepted. Compare 768 against 256-dimension Matryoshka truncation against 384 Granite; 256 is preferred if it keeps recall within 0.01 of 768.
4. Lite keeps Granite 97M regardless (Lite never requires a larger embedder).

## 5. Answer to the owner

Does EmbeddingGemma 2 help? Unknown today: it cannot be run on our pinned runtime, so there is no AlixAI measurement. Its vendor-reported MTEB figures are not comparable to our EN/IT corpus. Granite Embedding 97M stays the default for Lite and Balanced. A follow-up default change for Balanced is **not justified yet**; revisit when LocalAI ships a llama.cpp with PR #30054 (re-evaluation trigger recorded in the registry entry).
