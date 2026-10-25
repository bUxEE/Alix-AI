# 0015 — Voice models that meet quality and memory gates on CPU-only Lite hardware

- **Status:** accepted
- **Date:** 2026-10-04
- **Decided in task:** M0-16 (provisional default and ladder); finalised in M4-06 (measured, accepted 2026-10-04)

## Context
Voice is an optional interface to the same application session (§13): audio, VAD, STT, a
finalised transcript that enters the normal turn pipeline, verified text, then streaming TTS.
§7.1 says Lite carries only an "optional small STT and lightweight CPU TTS profile", language
packs are installed and activated explicitly, voice workers are absent from text-only profiles, and
raw audio is not retained. §25 item 6 asks which models meet the quality and memory gates on
CPU-only Lite hardware. "None do, so voice is Balanced-only" is an allowed outcome (§7.1, §18:
STT unavailable leaves text input, TTS unavailable returns verified text).

Declared languages drive the gates: English and Italian first (the M4-06 benchmark languages).
Voice models also have the most awkward licences in the stack, so the §8.4 licence and
redistribution review is a first-class criterion here, not a footnote.

LocalAI backends relevant to CPU voice (docs checked 2026-10-02): STT `whisper.cpp`,
`parakeet.cpp`, `moss-transcribe.cpp`, `faster-whisper`; TTS `piper`, `Kokoro`, `VibeVoice`,
`Qwen3-TTS`; VAD Silero. Backends ship as OCI images, and the native LocalAI binary on arm64 has no
TTS backend ([0012](0012-lite-install-native-vs-container.md)).

## Options
No number below is a measurement by this project; M4-06 owns those. Sizes are from upstream.

STT
1. **whisper.cpp, multilingual `tiny` / `base` / `small`** (MIT). Disk / memory: 75 MB / about 273
   MB, 142 MB / about 388 MB, 466 MB / about 852 MB; Q5_0 and Q8_0 quantisation reduce both. Plain
   C/C++, CPU-only, one binary lineage already used by LocalAI. Not natively streaming, so it needs
   VAD segmentation. The smaller multilingual models are expected to be weak on Italian; this is
   the main thing to measure.
2. **Moonshine v2 streaming** (tiny 33.6M, small 123M, medium 245M parameters; MIT per the project).
   Low latency and tiny, but the published models are English plus a few monolingual models (Arabic,
   Chinese, Japanese, Korean, Ukrainian, Vietnamese); **no Italian STT was found**, and its legacy
   non-English models carry a non-commercial licence. A candidate for an English-only pack only.
3. **Parakeet TDT 0.6B v3 via `parakeet.cpp`** (a LocalAI backend). 25 European languages including
   Italian; GGUF Q4_K_M about 0.49 GB, Q8_0 about 0.74 GB. Strong accuracy but 4 to 8 times the
   memory of whisper `base`, so likely a Balanced candidate. Model licence not checked here.
4. **Not evaluated:** Vosk, sherpa-onnx, `faster-whisper` (CTranslate2; container-only on LocalAI),
   English-only distilled Whisper variants.

TTS
5. **Piper** (the maintained fork `OHF-Voice/piper1-gpl`, **GPL-3.0**, embeds espeak-ng). Small
   per-voice ONNX models: for Italian, `it_IT-paola-medium` is 63.5 MB at 22.05 kHz, the other
   Italian voice is x_low. Each voice has its own dataset licence, separate from the engine's.
   The Italian shelf is shallow.
6. **Kokoro-82M** (82M parameters, Apache-2.0 weights, v1.0 of 2025-01-27). Eight language
   codes per the card, and the voice list includes Italian (`if_sara`, `im_nicola`), but both
   Italian voices are graded C overall with 1-10 hours of training data. Its grapheme-to-phoneme path (misaki with espeak-ng) carries a GPL
   dependency. Memory and real-time factor are unmeasured.
7. **Larger engines** (VibeVoice, Qwen3-TTS): Balanced only; not sized here.
8. **No TTS:** return verified text (§18). Always available.

VAD: Silero VAD (LocalAI backend), a very small model; needed by whisper.cpp for segmentation.

Security impact applies to all options: audio is untrusted input and a transcript is data, never
instructions (§12.6). The finalised transcript is visible to the host and correctable by the user
before any consequential confirmation, and speaker recognition cannot replace application
authorisation (§13). No option stores raw audio by default. Model files are hash-verified and
licence-recorded like any model (§16.3, §8.4).

## Decision
Measured by M4-06 on LocalAI v4.10.0 under `compose.lite.yaml` (8 GiB, 4 CPU, CPU only, **emulated**), against gates fixed before any result
(`docs/evidence/m4-voice/protocol.md`, commits `97d6e78` and `35d4b8b`), three cold-start runs per candidate. Evidence:
[`docs/evidence/m4-voice/report.md`](../evidence/m4-voice/report.md) and `gates.md` (the verdict file and raw results are not published). The rule held: the smallest candidate that
passes every gate of its role in a language wins; a negative result is a result.

1. **VAD:** the ggml Silero v5.1.2 model, served by LocalAI through the whisper.cpp backend (`vad_only`), always in front of STT. It passes (no speech
   segment in 8 of 8 non-speech clips, speech in 80 of 80 FLEURS clips, 41 MiB). It is mandatory: every Whisper size turned all 8 silence and noise clips
   into text.
2. **English on Lite: speech input only.** STT is **whisper.cpp multilingual `tiny` q5_1** (FLEURS WER 0.138 against 0.15, real-time factor 0.09, worker
   202 MiB against 500); `base` (0.104, 230 MiB) is the quality step and also passes. The answer stays text. Shipped as the STT-only
   pack `en-stt` (`qualified-emulated`).
3. **Text to speech is not offered on Lite.** Piper (ljspeech, paola, riccardo x_low) and Kokoro v1.0 all exceed the 300 MiB TTS worker budget (peaks 354 to
   796 MiB) and, but for riccardo, the 1500 ms time to first audio (LocalAI v4.10.0 returns a complete file for Piper; the streamed request hangs). Both
   Italian voices also miss round-trip intelligibility (0.164 and 0.153 against 0.10).
4. **Italian voice is Balanced-only.** Whisper tiny (WER 0.299) and base (0.197) miss the bar; `small` (0.083, real-time factor 0.47) passes it but peaks at
   550 MiB against the Lite 500 MiB row (fits Balanced's 1000). No Italian TTS passes; a Balanced Italian voice needs another engine (Kokoro Italian
   through the Python backend is the first to measure). The `it` pack stays `candidate` and carries small + paola + VAD as that candidate.
5. **Packs en and it** keep all three roles (the M4-05 tests require it) and status `candidate`; their STT is the winning rung of the language (tiny, small),
   their VAD `engine` is corrected to `whisper.cpp`. Measured quality and resources are in the manifests.
6. **Operation:** `LOCALAI_MAX_ACTIVE_BACKENDS` must cover the resident roles including the voice workers (4 in total for the English input-only stack, derived from the 5-slot run, 5 with TTS and
   VAD beside controller and embedder). At the shipped Lite default of 2, every voice call evicted the controller (8 reloads of about 5 s in 21 calls);
   at 5 there were none and the summed resident set peaked at 3012 MiB of 5811. The voice adapter must set `tts_no_stream` for Piper.
7. **Licences are listed, not decided.** As found (full table in the report): Whisper ggml models, Silero ggml and the Piper voices MIT (paola's lineage
   includes the Blizzard-2013-licensed lessac voice), Kokoro weights Apache-2.0, the Piper engine GPL-3.0 per this ADR (its image bundles espeak-ng), FLEURS
   CC-BY-4.0. AlixAI bundles no weights and no engine: packs pin files the operator fetches, engines are separate digest-pinned images. The posture and
   redistribution questions are for M5-H02.

**Overturn criteria (unchanged in spirit).** Reopen Lite TTS when a measured engine fits 300 MiB and 1500 ms, or when the budget row is revised with
evidence; reopen Italian on Lite when a model with Italian WER at most 0.15 fits 500 MiB; adopt Kokoro over Piper only if it passes the gates and the
licence posture is acceptable (it did not pass here). Re-run `voicebench` on each LocalAI release and on any new candidate.

## Consequences
- Easier: text-only Lite stays small; voice is opt-in, per language, and degrades to text (§18). English speech input is cheap (243 MiB peak, 33 MB).
- Harder: there is no Lite spoken answer in any declared language, and no Italian voice below Balanced. The resource-budget rows for the STT and TTS workers
  (`docs/operations/resource-budgets.md` 4.1) were estimates and are now measured; the Lite TTS row cannot be met.
- Harder: the model registry cannot describe a ggml artefact or a VAD role, and its test accepts only pinned `.gguf` entries without measurements, so no voice
  entry is shipped (the four TTS entries are unpublished drafts; every pack `registry_id` is null) until the schema and
  that test are extended.
- Harder: the GPL components make packaging and redistribution a legal, not just technical, task (M5-H02).
- Forbidden: audio retained by default; letting STT output or speaker identity authorise an action; loading voice workers in a text-only profile; STT
  without the VAD in front; distributing a voice or engine whose licence has not been recorded.

## Reversal plan
Voice roles are separate `AIRuntimeProvider` roles (reserved in M0-07), so STT and TTS models are
voice-pack manifests (registry entries once the registry can hold them). Changing the default is a pack edit with
a re-run of `voicebench`; no API change. Sessions are text-compatible, so disabling voice entirely
leaves the application working.

## Sources (checked 2026-10-02, measurements 2026-10-04)
- Measurements: [`docs/evidence/m4-voice/`](../evidence/m4-voice/report.md) (protocol, report, gates; the raw records are not published), tool `tools/voicebench`
- LocalAI v4.10.0 gallery entry `silero-vad-ggml` and backend index: <https://github.com/mudler/LocalAI/tree/v4.10.0>
- FLEURS: <https://huggingface.co/datasets/google/fleurs> (CC-BY-4.0), Conneau et al. 2022
- whisper.cpp model sizes: <https://github.com/ggml-org/whisper.cpp>
- Moonshine: <https://github.com/moonshine-ai/moonshine>; Parakeet GGUF sizes: <https://github.com/mudler/parakeet.cpp>
- Kokoro-82M and voices: <https://huggingface.co/hexgrad/Kokoro-82M>, <https://huggingface.co/hexgrad/Kokoro-82M/blob/main/VOICES.md>
- Piper: <https://github.com/OHF-Voice/piper1-gpl>, Italian voice: <https://huggingface.co/paolapersico1/Piper-TTS-Italian>
- LocalAI backends: <https://localai.io/docs/backends/index.html>
