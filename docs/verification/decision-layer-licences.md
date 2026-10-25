# Decision layer: licence and supply-chain review

Status: **maintainer review, not legal advice.** Done by the project maintainer, not by a lawyer or an independent reviewer.
Upstream pages were checked on 2026-10-25, at the revisions AlixAI pins. Read the upstream licence yourself before you
deploy, and ask a lawyer if your use depends on it.

Back to the [verification index](README.md). The generative and embedding models and the LocalAI runtime are in
[model-licences.md](model-licences.md).

## 1. What the decision layer is, and how AlixAI obtains its parts

The decision layer is optional, bounded, non-generative and non-authoritative: a scorer may rank candidates that policy has
already authorised and never decides anything. It is **off on every profile** in 1.0
([ADR 0031](../decisions/0031-decision-scorer-default-enable.md), [hardware.md](hardware.md)).

How every part below reaches a machine:

- **AlixAI never redistributes model weights, the CLM wheel or the vLLM image.** The repository and the release artifacts
  contain none of them.
- The registry (`models/registry/<id>.yaml`) pins each file by **repository, commit revision, file name, size and sha256**,
  together with its declared licence and redistribution notes. The release manifest lists the same pins.
- The **operator** downloads the files (`deploy/container/clm/fetch.sh` for the CLM sidecar). The sidecar entrypoint
  re-hashes every pinned file at every start and refuses a mismatch; the AlixAI service refuses a model whose sha256 does not
  match the registry.
- Container images are pinned **by digest**, never by a floating tag.

So the licence terms below bind **the operator** who downloads and runs the files. AlixAI's own Apache-2.0 licence does not
cover them.

## 2. Components

| Component | Pinned revision | Licence (as declared) | Where the licence was seen | Redistribution | Notes |
| --- | --- | --- | --- | --- | --- |
| **CLM-v0.1-8B heads** (`Contrastive-LM/CLM-v0.1-8B`, `CLM_v0.1-8B.pt`, 75,557,149 bytes) | `e939398d`, sha256 `b2b4a8c9…4eda5` | Apache-2.0 | card metadata, card text and a LICENSE file in the repository | permitted with the licence text and notices; changes stated | the LICENSE file is the unmodified Apache-2.0 text with its copyright placeholder line not filled in. Base model declared: `Qwen/Qwen3-8B`. Not gated |
| **contrastive-lm 0.1.0** (the CLM service, PyPI wheel, pure Python) | wheel sha256 `7f3eed12…055f` | Apache-2.0 | PyPI metadata (`License: Apache-2.0`, LICENSE file in the wheel); the project's GitHub LICENSE is Apache-2.0 | permitted | status "Alpha". Installed with no dependencies; its declared dependencies (torch, fastapi, uvicorn, numpy, requests, vLLM) come from the pinned image |
| **Qwen3-8B** (frozen encoder, bf16 safetensors, 5 shards, about 16.4 GB) | `b968826d`, sha256 of every shard pinned | Apache-2.0 | card metadata and a LICENSE file ("Copyright 2024 Alibaba Cloud") | permitted | not gated; no extra use policy on the card. First-party weights, unmodified |
| **vLLM** (`docker.io/vllm/vllm-openai-cpu:v0.31.0`) | image index digest `sha256:495c84fe…bd9` | Apache-2.0 | LICENSE file at the `v0.31.0` tag | permitted | the image bundles Python, torch 2.13.0+cpu and many Python packages under their own licences; those were **not** reviewed one by one. No NOTICE file at the repository root |
| **GLiNER2.5-Decide** (`fastino/GLiNER2.5-Decide`, F32, 1,945,828,140 bytes) | `5a7adf72`, sha256 `40a5a23f…c997` | Apache-2.0 | card metadata and card text at the pinned revision; **no LICENSE file in the repository** | permitted per the declaration | base `fastino/gliner2-large-v1` (Apache-2.0); encoder DeBERTa-v3-large (`microsoft/deberta-v3-large`, **MIT**). The repository has moved on since the pin; the pin is what was checked |
| **Laya** (`convaiinnovations/laya`, ModernBERT-large, F16) | `7b928d82`, sha256 `891102d3…d86c` | Apache-2.0 | card metadata only; **no LICENSE file** | permitted per the declaration | no `base_model` field; the card names ModernBERT-large (`answerdotai/ModernBERT-large`, Apache-2.0). The project's GitHub and PyPI package are Apache-2.0 |
| **Laya multilingual** (`convaiinnovations/laya-multilingual`, mmBERT-base, F16) | `1720e3e3`, sha256 `9d628fd9…f204` | Apache-2.0 | card metadata only; **no LICENSE file** | permitted per the declaration | the card names mmBERT-base (`jhu-clsp/mmBERT-base`, **MIT**) |

**Quantisations.** None of the decision-layer files is a third-party quantisation: the heads, Qwen3-8B (bf16), Laya and Laya
multilingual (F16) and GLiNER2.5-Decide (F32) are the publishers' own files, unmodified. The CLM head's dtype is not
recorded by its publisher; the registry marks it `unspecified`.

**EmbeddingGemma 2** appears in the registry as an *embedding* candidate, not a scorer. It carries the one use restriction
found in this review; see [model-licences.md](model-licences.md#3-flags).

## 3. Flags

1. **No restrictive licence was found** for any decision-layer component. Every one declares Apache-2.0, and two base models
   (DeBERTa-v3-large, mmBERT-base) are MIT. Both are compatible with Apache-2.0, but a redistributor would need to carry
   their MIT notices as well.
2. **Licence declared only in card metadata** for GLiNER2.5-Decide, Laya and Laya multilingual: no LICENSE file exists in
   those repositories, so the licence text was not seen there, only the declaration. That is common on Hugging Face and
   weaker evidence than a LICENSE file.
3. **The CLM head is a PyTorch pickle (`.pt`).** Loading a pickle can execute code. Mitigations in place: the file is pinned
   by sha256 and re-hashed at every start; the encoder sits on an internal container network with no route out, the CLM service publishes only on 127.0.0.1, and no GPU is used. What was checked in
   this review: the pinned wheel calls `torch.load(path, map_location="cpu")` **without** an explicit `weights_only`
   argument. The pinned image ships torch 2.13.0, where `weights_only` defaults to `True`, so the load is restricted to
   tensors and plain data. That protection comes from the torch default, not from the wheel; an image with an older torch
   would lose it. A compromised publisher at the pinned commit would not be detected by the hash, because the hash is the
   publisher's own.
4. **vLLM image contents** were not reviewed package by package. Its own licence is Apache-2.0; the bundled Python
   dependencies carry their own licences and advisories, and are pinned only through the image digest.
5. **Status of the files.** CLM-v0.1-8B and Qwen3-8B are `candidate` in the registry: pinned is not qualified. The scorers
   are `candidate` too and cannot run on the pinned LocalAI, which has no Decisions API.

## 4. What this review does not cover

- It is not a legal opinion and not a trademark or patent review.
- It does not cover the training data of any model.
- It covers the pinned revisions only. A new revision needs a new review before the pin moves; the registry records the
  date each pin was checked.
