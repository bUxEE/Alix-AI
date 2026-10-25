# M4-06 voice model qualification on CPU-only Lite: report

> The raw results, logs, scripts and configuration files this report names are not published with the source code; their
> names are kept as a record of how the figures were produced.

Everything here is **emulated**: LocalAI v4.10.0 (image digest in `deploy/container/localai/compose.yaml`) in a container capped at 8 GiB and 4 CPUs
(`compose.lite.yaml`, no swap, 4 threads), on an i7-10700K host (Docker VM: 16 logical CPUs, 32 GB), CPU only, one request at a time, greedy
decoding, default backend settings (nothing tuned). Never a certification. The gates were written before the first STT or TTS result:
`97d6e78` (gates, audio rules, prompts) and `35d4b8b` (Amendment 1, three cold-start runs per candidate); [`protocol.md`](protocol.md) is the text.

## Verdict

| | English | Italian |
| --- | --- | --- |
| Speech to text on Lite | **passes**: whisper tiny q5_1 (smallest rung that clears every gate; base also passes) | **fails**: tiny WER 0.299, base 0.197; small WER 0.083 but its worker peaks at 550 MiB over the 500 MiB budget |
| Voice activity detection | passes (the same VAD serves both languages) | passes |
| Text to speech on Lite | **fails**: Piper ljspeech medium and Kokoro v1.0 miss time to first audio and the 300 MiB worker budget | **fails**: Piper paola and riccardo miss round-trip WER and the 300 MiB budget; paola also time to first audio |
| Co-residency (Lite) | passes at 5 active backends: 0 reloads, 3012 MiB peak of 5811 | same |
| **Lite voice stack** | **no full stack**; speech *input* only (VAD + whisper tiny), the answer stays text | **none**: voice is Balanced-only |

So the §25 item 6 answer is: **no TTS and no Italian STT meets the quality and memory gates on Lite**. English speech input does, and is the
only voice capability the Lite profile may offer. A larger-than-Lite budget (Balanced: STT worker 1000 MiB, TTS worker 600 MiB) would hold whisper
small (550 MiB) and Piper paola (576 MiB), but no Italian TTS tried clears the intelligibility bar. All gates and values:
[`gates.md`](gates.md) (generated), `verdict.json` and the raw records (not published).

## What was measured, and with what audio

* **STT quality gate:** Google FLEURS `test` split, revision `70bb2e84b976b7e960aa89f1c648e09c59f894dd`, **CC-BY-4.0**, attribution in
  `fleurs-manifest.json` (clip ids, transcripts, sha256; the audio is not committed). First 40 clips per language in
  archive order: English 857 reference words (372 s), Italian 1012 words (581 s). en_us clips are 32-bit float WAV, converted to 16-bit PCM
  before sending (the adapter takes PCM16 only).
* **TTS round trip:** 16 original sentences per language (`tests/performance/voice/tts-sentences-*.txt`, public domain), synthesized by each voice and
  transcribed by whisper small as the judge (the judge's own WER on FLEURS is 0.073 en, 0.083 it). Synthetic speech is never used for STT gating.
* **Non-speech:** 8 deterministic synthetic clips (silence, white noise, 50 Hz hum, low-pass noise; 2 languages x 4), no voice in them.
* **WER:** micro-averaged, one normaliser for references and hypotheses (lower-case, non letters/digits become spaces, no numeral or accent mapping).
* **Runs:** every candidate three times, each after a container restart (cold start); gates read WER and memory worst-case, latency as the median of
  the three medians (Amendment 1: the machine is shared with other build tasks, and the first VAD run was visibly disturbed: real-time factor
  0.1 instead of 0.008 from clip 28 on; it is kept as run 1 of 3).
* **Path:** all calls go through `adapters/localai/voice` (the 3 allowlisted routes). Memory is the summed VmRSS of the worker process read from `/proc`
  every second (anonymous plus memory-mapped pages), the figure the budget (`resource-budgets.md` R) is written in.

## Speech to text (FLEURS, 40 clips per language, 3 runs)

| Model (q5_1) | File | WER en | WER it | RTF p50 en / it | RTF p95 (worst) | Worker peak / steady MiB | Cold load | Verdict |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| whisper tiny | 32 MB | 0.138 | 0.299 | 0.086 / 0.095 | 0.14 | 202 / 146 | 2.9 s | en passes; it misses WER |
| whisper base | 60 MB | 0.104 | 0.197 | 0.164 / 0.161 | 0.27 | 230 / 189 | 3.7 s | en passes (not smallest); it misses WER |
| whisper small | 190 MB | 0.073 | 0.083 | 0.547 / 0.471 | 1.02 | 550 / 394 | 7.5 s | WER passes; misses the 500 MiB budget |

The WER is identical across the three runs (greedy decoding). English tiny passes by 1.2 points on 857 words, a thin margin: real microphones are
worse than FLEURS read speech, and base (0.104) is the quality step if the operator wants margin. **Deviation from the protocol:** its stop rule called for
parakeet.cpp as the Balanced reference once no Whisper rung passed every Italian gate; it was not run. It cannot change the Lite verdict (it would only be
a Balanced reference, and its 0.49 GB model is larger than the rungs that already miss the Lite memory line).

**Whisper alone turns silence and noise into text.** Every Whisper size returned a non-empty transcript for **all 8 non-speech clips** in every run (silence and
noise become text). A VAD in front of STT is therefore mandatory, not an optimisation; the VAD below declines all 8.

## Voice activity detection (LocalAI `/v1/vad`, ggml Silero v5.1.2)

LocalAI v4.10.0 serves this file through the **whisper.cpp backend** with the `vad_only` option (gallery entry `silero-vad-ggml`), not through the
`silero-vad` ONNX backend, so the M4-05 pin is the right file and the engine label of the packs is corrected to `whisper.cpp`. Over 3 runs: speech
found in 80 of 80 FLEURS clips, **no speech segment in 8 of 8 non-speech clips**, worker peak 41 MiB (steady 37), cold load 2.1 s, real-time factor
about 0.01 on speech (0.12 on the 3 to 4 s non-speech clips; per-call overhead).

## Text to speech (16 sentences, 3 runs)

| Voice | File | Round-trip WER | Time to first audio p50 | RTF p50 | Worker peak / steady MiB | Cold | Verdict |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| Piper en_US ljspeech medium | 64 MB | 0.070 | 2292 ms | 0.63 | 644 / 499 | 4.5 s | fails time to first audio, memory |
| Piper it_IT riccardo x_low | 28 MB | 0.164 | 1422 ms | 0.43 | 354 / 245 | 3.5 s | fails intelligibility, memory |
| Piper it_IT paola medium | 64 MB | 0.153 | 2029 ms | 0.66 | 576 / 427 | 4.2 s | fails intelligibility, time to first audio, memory |
| Kokoro v1.0 82M (kokoros backend, en only, streamed) | 354 MB | 0.075 | 1892 ms | 0.47 | 796 / 710 | 7.9 s | fails time to first audio, memory |

* **LocalAI v4.10.0 does not stream Piper.** A request with `stream=true` to the piper backend hangs until the backend timeout and ends in a 500
  (observed twice, with a cold and a warm model); `stream=false` works. The measurements use the adapter's `TTSNoStream` option, so for Piper the
  time to first audio *is* the whole synthesis of the sentence (8 to 14 words, 3 to 5 s of audio). The Rust Kokoro backend does stream (first chunk
  after about 1.9 s median).
* Naturalness and voice quality were **not** measured (intelligibility only). The Italian round-trip misses are partly the judge writing digits for
  spelled-out numbers ("dieci e trenta" as "10.30"), which the pre-registered normaliser counts as errors; riccardo's misses are real mishearings
  ("narte di", "conmiume"), and paola fails two other gates regardless, so the verdict does not depend on it.
* Kokoro Italian (voices if_sara, im_nicola) is not measured: the Rust backend has no Italian, and the Python backend is a large image that fetches its
  model from the Hub at run time (blocked by the TLS-intercepting product on this machine). It is the first thing to try for a Balanced Italian voice.

## Co-residency under Lite (controller Granite 4.0 1B and embedder resident, three rounds of chat, embed, STT, TTS, VAD, chat, embed)

| `LOCALAI_MAX_ACTIVE_BACKENDS` | Reloads (calls over warm median + 3 s) | Summed worker RSS peak | cgroup W peak |
| --- | ---: | ---: | ---: |
| 5 (the declared resident roles) | **0** of 21 calls | 3012 MiB (llama 2243, piper 455, whisper STT + VAD 163, service 151) of 5811 | 1819 MiB |
| 2 (the shipped Lite default) | **8**: every chat and embedding that followed a voice call (about 5 s each) | 2395 MiB | 2307 MiB |

So the voice workers (tiny + Piper ljspeech + VAD, worst case 618 MiB) fit the Lite container budget with 2.8 GiB to spare, **but only if the runtime is
told to keep five backends**: at the shipped default every voice turn evicts the controller (R3 of the resource budgets). The sequence used a failing
TTS candidate to size the worst case; with no TTS the English input-only stack adds about 165 MiB.

## Findings for other tasks (out of this task's scope)

1. **Adapter configuration:** Piper needs `tts_no_stream: true` (M4-02 option); the composition root that builds the voice client (M4-04 area) must set it per
   TTS backend. Also decide a chunking policy: replies synthesized per sentence would cut the time to first audio (not measured).
2. **Deploy defaults (`deploy/`):** `LOCALAI_MAX_ACTIVE_BACKENDS` must be at least the resident roles including the voice workers (5 for a Lite
   stack with STT, TTS and VAD; 4 for an English input-only Lite stack: controller, embedder, STT, VAD; the 4 is derived, the measured run had 5 slots with TTS); `compose.balanced.yaml` counts four roles but a pack adds
   VAD as a fifth process. The backend gallery pins to add when voice ships: `v4.10.0-cpu-whisper` `sha256:cd3d42db...1475`, `v4.10.0-piper`
   `sha256:d02327c0...792a`; the VAD needs no backend of its own.
3. **Model registry:** it cannot hold a voice model today. `artifact.format` has no `ggml` and `roles` has no `vad` (schema, outside this task), and
   `internal/models/registry_test.go` asserts that every shipped entry is a pinned `.gguf` and that a candidate carries no measurement, so the four TTS
   ONNX entries written for this task (validated against the schema, with the measured figures) failed `TestRegistryEntriesValid` and are kept as
   drafts in `registry-drafts/` instead of `models/registry/`. Whisper and VAD entries could not be written at all; every pack `registry_id` is null.
   Needed: extend the schema (`ggml`, `vad`) and that test, then move the drafts in.
4. **`docs/operations/resource-budgets.md` section 4.1** rows are estimates: measured STT workers are 202 / 230 / 550 MiB (tiny / base / small), TTS workers 354 to
   796 MiB, VAD 41 MiB (a new row). The 300 MiB Lite TTS row cannot be met by any engine tried.
5. **Session service:** a VAD gate before STT is mandatory (above). Whisper memory grows with clip length (small: 550 MiB on 30 s clips, 378 MiB on 3 to 5 s
   sentences), so utterance length should be bounded.

## Licences, recorded as found (not reviewed or decided here: M5-H02)

| Item | Licence as found | Note |
| --- | --- | --- |
| whisper.cpp ggml models tiny/base/small (`ggerganov/whisper.cpp`) | MIT (repository card); OpenAI Whisper weights MIT | third-party conversion |
| Silero VAD ggml (`ggml-org/whisper-vad`) | MIT (repository card) | conversion of Silero VAD |
| Piper engine in the LocalAI `piper` backend image | LocalAI's backend index says MIT; ADR 0015 records the maintained engine fork as **GPL-3.0**; the image bundles `libespeak-ng.so` and `espeak-ng-data` | engine runs as a separate pinned image, no linking into AlixAI |
| Piper voice en_US ljspeech medium | MIT (piper-voices card); dataset LJ Speech public domain | |
| Piper voice it_IT paola medium | MIT (card); dataset CC0-1.0; **fine-tuned from the lessac voice, whose dataset is under the Blizzard 2013 licence** | lineage open for M5-H02 |
| Piper voice it_IT riccardo x_low | MIT (piper-voices card) | dataset not reviewed |
| Kokoro v1.0 weights and `kokoro-v1.0.onnx` | Apache-2.0 (card, LocalAI gallery); ONNX is a third-party release asset pinned by sha256 | phonemiser path uses GPL espeak-ng (ADR 0015) |
| FLEURS audio and transcripts | CC-BY-4.0 | attribution kept; not redistributed |
| Test sentences, non-speech clips | original, public domain / generated | |

Technical fact that does not decide the licence question: AlixAI bundles no weights and no engine; packs pin files the operator fetches and the engines
are separate digest-pinned backend images.

## Limits

One machine, shared with other tasks (the repeated cold-start runs and worst-case reading are the answer to that); 40 clips and about 1000 words per
language (a 95% interval on a 0.14 WER is roughly plus or minus 0.02); read speech only; no naturalness score; no power or thermal measurement; Piper, whisper
and Kokoro run with their default settings (no thread or arena tuning); Balanced was not measured; parakeet.cpp (a protocol deviation, see the STT section),
Moonshine, Vosk and sherpa-onnx were not run. Reproduce: `tests/performance/voice/stack/` (fetch scripts, compose override, `run-matrix.sh`), then
`go run ./tools/voicebench check|verdict|companion`; `go run ./tools/bench check benchmarks/results/m4` validates the bench-envelope companions of the
non-speech clips (the `.voice` and `.coresident` files hold what the bench schema cannot).
