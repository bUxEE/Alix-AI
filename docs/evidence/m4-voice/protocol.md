# M4-06 protocol: voice model qualification on CPU-only Lite (pre-registered)

This file is committed **before the first measurement** and is not edited afterwards except to add a dated
"Amendments" entry at the bottom (an amendment cannot lower a bar; it can only add detail the run showed was missing).
The commit that holds it is cited in `report.md`. Everything measured is **emulated**: a cgroup cap
on one container on a machine larger than the profile, never certified.

## What is measured

Runtime: the LocalAI release pinned in `deploy/container/localai/compose.yaml` (v4.10.0, image digest in that file), under
`compose.lite.yaml` (8 GiB, 4 CPU, no swap, `LOCALAI_THREADS=4`), CPU only, one request at a time, greedy decoding. Voice
backends are the v4.10.0 images of the LocalAI backend registry, pinned by digest in `tests/performance/voice/stack/` and
fetched host-side. Every request goes through the real adapter `adapters/localai/voice` (the allow-listed routes
`/v1/audio/transcriptions`, `/v1/audio/speech`, `/v1/vad`); the benchmark never widens that allowlist.

Candidates, smallest first, with the stop rule:

| Role | Ladder | Stop rule |
| --- | --- | --- |
| STT | whisper.cpp multilingual `tiny` q5_1, then `base` q5_1, then `small` q5_1 | `small` is measured only when `base` misses a quality gate in a language. A non-Whisper STT (parakeet.cpp) is measured only if no Whisper rung passes a language, and then only as the Balanced reference |
| TTS | Piper: `en_US-ljspeech-medium` (en); `it_IT-paola-medium` and `it_IT-riccardo-x_low` (it) | Kokoro is measured only if a Piper voice fails a gate for its language (the Rust `kokoros` backend lists no Italian) |
| VAD | LocalAI `silero-vad` backend | one candidate |

The smallest candidate that passes **every** gate of its role in a language wins that language. If no candidate of a role passes
in a language, that language has no Lite voice for that role (voice is then Balanced-only for it, which is an allowed outcome). A larger rung is never chosen over a passing smaller one.

## Test audio (licence-clean)

* **STT gating audio:** Google FLEURS (`google/fleurs`, dataset revision `70bb2e84b976b7e960aa89f1c648e09c59f894dd`), configs
  `en_us` and `it_it`, split `test`, licence **CC-BY-4.0** (attribution: Conneau et al., "FLEURS: Few-shot Learning Evaluation
  of Universal Representations of Speech", 2022). Human read speech; the audio is **not committed** (a manifest of clip ids,
  reference transcripts and sha256 is, plus the fetch command). Selection rule, fixed now: the **first 40 clips in the order they
  appear in the `test.tar.gz` archive** that have a row in `test.tsv`; no clip is skipped or replaced after the fact. The
  reference is the `transcription` column of the TSV.
* **TTS prompts:** `tests/performance/voice/tts-sentences-{en,it}.txt`, 16 original sentences per language written for this task
  (booking, confirmation and error phrases of the clinic-booking reference application, numbers spelled out). They are original
  text, dedicated to the public domain (CC0-1.0) for this purpose.
* **Synthetic speech is used only for the TTS round trip**, never for STT gating: engine-generated speech is cleaner than real
  speech and would flatter the STT. The round trip is judged by whisper.cpp `small` q5_1 (the judge is the same for every voice, and
  is stronger than the Lite STT rungs so that a weak STT does not hide in the TTS number). The judge's own WER on FLEURS is reported
  beside it.
* **Non-speech set:** 4 deterministic synthetic clips per language tag (exact digital silence 3 s; white noise at -50 dBFS RMS 3 s;
  50 Hz hum at -40 dBFS plus noise at -55 dBFS 3 s; low-pass noise at -30 dBFS 4 s; 16 kHz mono PCM16, fixed seed), 8 clips in all.
  They contain no person's voice.

## Text normalisation and WER (the same rule for references and hypotheses)

NFC is not applied (no dependency is added); then: lower-case; every character that is not a Unicode letter or digit becomes a space
(so apostrophes and hyphens separate words, in both languages, in reference and hypothesis alike); runs of spaces collapse; the result is
split on spaces. No numeral, spelling or accent mapping: an accent error is a word error. WER = (substitutions + deletions +
insertions) / reference words by word-level Levenshtein distance; the figure for a language is **micro-averaged** (total edits over total
reference words of its clips), never a mean of per-clip rates. A clip that returns an error counts every reference word as a deletion.

## Gates (a candidate passes only if it clears every line of its role, per language)

Latency, memory and the Italian/English quality bars are fixed here. "Median" is over the warm clips (the first call of a model is its
cold load, recorded separately and not counted).

**STT** (per language, FLEURS, 40 clips):
1. WER at most **0.15** (micro-averaged). Rationale: FLEURS is clean read speech, real microphones are worse; above about one wrong
   word in seven a booking utterance usually carries a corrupted value and the user would have to correct the transcript
   too often. The same bar for both languages.
2. Real-time factor (call wall time / audio duration) median **below 1.0** on the 4-CPU profile (ADR 0015 starting point).
3. STT worker peak resident set, summed over the worker's processes, file-backed pages included (`R` of
   `docs/operations/resource-budgets.md` section 3), at most **500 MiB** (the Lite STT row of section 4.1).

**TTS** (per language voice, 16 sentences):
4. Round-trip WER (judge above) at most **0.10**.
5. Time to first audio, from the request to the first non-empty audio chunk the adapter returns from `SynthesizeStream`, median at most
   **1500 ms** (ADR 0015). A backend that answers with a complete file is measured as it is and recorded `streamed: false`; there is no credit
   for audio that has not arrived.
6. Real-time factor (total synthesis time / audio duration) median **below 1.0** (added by this protocol: streaming playback cannot keep up
   otherwise).
7. TTS worker peak resident set at most **300 MiB** (the Lite TTS row of section 4.1).

**VAD / non-speech safety** (the pipeline is VAD, then STT, then the normal turn pipeline; transcripts are data):
8. The VAD returns **no speech segment for all 8 non-speech clips**, so the pipeline produces no transcript (a false transcript from
   silence would enter the turn pipeline as a user turn). The STT-alone hallucination count on the same clips is reported, not gated.
9. The VAD returns at least one segment for at least **95%** of the FLEURS clips (a missed utterance is a silent failure).
10. VAD worker peak resident set at most **150 MiB** (a small ONNX/Go worker; the section 4.1 table has no VAD row, so this protocol sets one).

**Co-residency (Lite)**, once, with the controller and embedder of the Lite default (Granite 4.0 1B Q4_K_M, the Lite fallback, and the Granite
97M embedder) plus the winning voice workers, over a fixed mixed sequence (chat, embedding, STT, TTS, VAD, chat, repeated 3 times):
11. `LOCALAI_MAX_ACTIVE_BACKENDS` set to the number of declared resident roles: **0** reloads of a resident role (a chat or embedding call after
    a voice call that takes more than 3 s longer than the warm median of the same call counts as a reload, and is confirmed in the runtime log).
12. The summed resident set of all LocalAI processes stays at or below the Lite container budget, **5811 MiB** (`resource-budgets.md` section 3).
    A run at `LOCALAI_MAX_ACTIVE_BACKENDS=2` (the shipped Lite default) is recorded to show what R3 predicts; it is not gated.

Disk: a language pack at most 600 MB (the Lite voice-pack row of section 5.2).

## Verdict rules and what the numbers go to

* A negative result is valid and is reported as such: gates are not changed after results are seen.
* Packs (`models/voice-packs/<id>.yaml`): `quality.*` and `resources.*` are the measured figures of that pack's chosen STT and TTS, with
  `measurement.results_path` pointing at the result file; `status` becomes `qualified-emulated` only for a pack whose every role cleared its gates
  in that language, otherwise it stays `candidate` and the notes say which gate failed.
* Licences are recorded as found (SPDX, URL, notes) and are not reviewed or decided here; M5-H02 decides.

## Amendments

**Amendment 1 (2026-10-04, after the tool was built and before any STT or TTS result existed; two exploratory VAD runs had been made).**
The first VAD run showed a tenfold slowdown (real-time factor 0.008 to 0.1) that started at clip 28 and did not occur in an identical second run:
the development machine is shared with other build and test tasks, so one run can be disturbed by a neighbour. Detail added, no bar changed:
every candidate phase is run **three times, each from a cold start** (the container is restarted), and all three result files are kept. The
figure a gate uses is: **WER** the worst of the three (greedy decoding makes them identical; a difference is reported); **real-time factor and time
to first audio** the median of the three runs' medians; **resident set** the largest peak of the three; the VAD gates the worst run. A run is never
dropped or repeated because of its outcome; the two exploratory VAD runs count as two of the three VAD runs.
