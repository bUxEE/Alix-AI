# 0040 — Local STT ladder: Parakeet and Whisper large (candidates until voicebench)

- **Status:** provisional
- **Date:** 2026-10-26
- **Decided in:** release 1.1.0

## Context
ADR [0015](0015-voice-models-lite.md) measured Whisper on Lite: English speech input uses Whisper `tiny`
(`en-stt`, `qualified-emulated`); Italian needs Whisper `small` and is Balanced-only; no complete Lite voice
stack with TTS passes. The owner wants a **Balanced-and-up local STT** that can be either **NVIDIA Parakeet
TDT 0.6B v3** (via LocalAI `parakeet-cpp`) or a **high-quality Whisper** choice (`large-v3-turbo` on Balanced,
`large-v3` on Performance), with Whisper `tiny` remaining the Lite fallback. ADR 0015's rule still holds: a
model becomes a profile default only after it passes the voicebench gates (`tools/voicebench`, protocol in
[`docs/evidence/m4-voice/protocol.md`](../evidence/m4-voice/protocol.md)).

**CPU, not NVIDIA-only.** Parakeet via `parakeet.cpp` is a ggml port: it **runs on CPU**; CUDA, Metal and Vulkan
are optional acceleration. Source (checked 2026-10-26): [parakeet.cpp README](https://github.com/mudler/parakeet.cpp/blob/master/README.md)
("It runs on CPU and on GPU backends"). Whisper.cpp is likewise CPU-first with optional GPU. No option in this
ADR requires NVIDIA hardware.

Facts verified **2026-10-26**:

| Item | Finding |
| --- | --- |
| Parakeet model card | `nvidia/parakeet-tdt-0.6b-v3`, licence **CC-BY-4.0** |
| Parakeet languages | 25 European (incl. en, it) |
| Parakeet LocalAI backend | `parakeet-cpp` since LocalAI **v4.3.6**; in pinned **v4.10.0**. No image digest bump. |
| Pack GGUF (LocalAI path) | `mudler/parakeet-cpp-gguf` @ `c77726e46cea…`: `tdt-0.6b-v3-q4_k.gguf`, **675200864** B, sha256 **993d73fe…1d5ee8** |
| Backend OCI | `v4.10.0-cpu-parakeet-cpp` @ `sha256:372c7135…90bb75` |
| Whisper turbo | `ggerganov/whisper.cpp` @ `5359861c739e…`: `ggml-large-v3-turbo-q5_0.bin`, **574041195** B, sha256 **39422170…ffa7e2**; Hub licence **mit** |
| Whisper large-v3 | same repo: `ggml-large-v3-q5_0.bin`, **1081140203** B, sha256 **d75795ec…d98ad1** |
| Engine licences | parakeet.cpp **MIT**; whisper.cpp / Whisper weights **MIT** |

## Options
1. **Parakeet TDT 0.6B v3** via `parakeet.cpp` / `parakeet-cpp`. Multilingual (25 European). CC-BY-4.0 weights + MIT engine. Balanced candidate. CPU; GPU optional.
2. **Whisper large-v3-turbo** (q5_0 ggml). Multilingual Whisper set (incl. en, it); 809M params, 4 decoder layers. MIT. Balanced candidate (~547 MB disk).
3. **Whisper large-v3** (q5_0 ggml). Full large decoder. MIT. **Performance only**: ~1.01 GB weights; expected worker RSS above the Balanced STT budget (1000 MiB).
4. **Whisper `small` / `tiny` / `base`** (status quo from 0015). Already measured (emulated). Keep for Lite (`tiny`) and existing language packs.
5. **distil-large-v3** — **not taken.** English-only; cannot serve Italian.

## Decision
**Provisional.** Operators select the local STT pack per deployment (`voice.active_pack` / install).

### Ladder (shipped state: candidates stay opt-in)

| Profile | Packs | Default today |
| --- | --- | --- |
| **Lite** | `en-stt` (Whisper tiny) | `en-stt` (`qualified-emulated`) |
| **Balanced** | `parakeet-stt` **or** `whisper-turbo-stt` | neither — both `candidate` until voicebench |
| **Performance** | `whisper-large-v3-stt`, `parakeet-stt`, or `whisper-turbo-stt` | none of the new packs while `candidate` |

1. Ship packs `parakeet-stt`, `whisper-turbo-stt`, `whisper-large-v3-stt` — STT-only, `language: mul`, status
   **`candidate`**, pinned artefacts above.
2. **Lite** keeps **`en-stt`** unchanged. Italian `it` pack (Whisper small) unchanged.
3. **Defaults flip only after** a voicebench run on real LocalAI passes the STT gates for the languages required
   (at least `en` and `it`). Until then operators install a candidate explicitly.
4. **One-line flip after a passing run:** set `status: qualified-emulated` on the winning pack and set
   `voice.active_pack` to that pack id for the target profile(s); leave Lite on `en-stt`. Fill
   `quality` / `resources` / `measurement` from the run.
5. **VAD:** dictation (user-segmented) needs no VAD role in these STT-only packs. Conversation mode still pairs
   STT with Silero VAD (ADR 0015).
6. **STT-only LocalAI profile:** `compose.stt-only.yaml` + model lists for Parakeet, turbo, large-v3, and Lite
   tiny; operator sets `LOCALAI_MAX_ACTIVE_BACKENDS=1` on the command line (README).
7. **No LocalAI image digest bump.**

## Consequences
- Easier: Whisper-family path without Parakeet; Parakeet remains available; all run on CPU.
- Harder: large Whisper files; worker RSS unknown until measured; Parakeet CC-BY-4.0 attribution for weight redistributors.
- Forbidden: declaring any of these the Balanced/Performance default, or inventing WER/RSS, before a recorded
  voicebench run; shipping unpinned sha256; implying Parakeet requires an NVIDIA GPU.

## Reversal plan
Packs and LocalAI model YAML are configuration. Revert status / `active_pack`, or drop packs from the embedded
set in a later release. No API change.

## Overturn criteria
- Voicebench fails STT gates for a candidate on its target profile → leave it candidate or rejected; keep Lite on tiny.
- A smaller multilingual model passes the same gates with less memory → prefer it by the smallest-passing rule.
- Licence review rejects a weight or engine → do not qualify; record in `docs/verification/model-licences.md`.

## Sources (checked 2026-10-26)
- NVIDIA model card: <https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3>
- LocalAI GGUFs: <https://huggingface.co/mudler/parakeet-cpp-gguf>
- parakeet.cpp README (CPU + optional GPU): <https://github.com/mudler/parakeet.cpp/blob/master/README.md>
- whisper.cpp models (MIT; large-v3 / turbo q5_0 pins via Hub API): <https://huggingface.co/ggerganov/whisper.cpp>
- LocalAI releases: `parakeet-cpp` in [v4.3.6](https://github.com/mudler/LocalAI/releases/tag/v4.3.6); runtime [v4.10.0](https://github.com/mudler/LocalAI/releases/tag/v4.10.0)
- Prior Lite measurement: [0015](0015-voice-models-lite.md), [`docs/evidence/m4-voice/`](../evidence/m4-voice/report.md)
