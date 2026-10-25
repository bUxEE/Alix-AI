#!/usr/bin/env bash
# The measurement matrix of docs/evidence/m4-voice/protocol.md: every candidate phase three times, each from a cold start (run-phase.sh restarts the
# container). Results go to benchmarks/results/m4. Run from anywhere; the container must exist (stack.sh up lite) with LOCALAI_MAX_ACTIVE_BACKENDS=1.
#
#   VOICE_ENV_FILE=<env file> run-matrix.sh tts|tts-kokoro|judge-kokoro|vad|stt-tiny|stt-base|stt-small [repetitions (default 3)]
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
P="$HERE/run-phase.sh"
what="${1:?tts|tts-kokoro|judge-kokoro|vad|stt-tiny|stt-base|stt-small}"
reps="${2:-3}"
S=tests/performance/voice
for rep in $(seq 1 "$reps"); do
  echo "=== $what repetition $rep"
  case "$what" in
    tts)
      "$P" run -phase tts -model piper-en-ljspeech -backend piper -files en_US-ljspeech-medium.onnx,en_US-ljspeech-medium.onnx.json -lang en -sentences $S/tts-sentences-en.txt
      "$P" run -phase tts -model piper-it-paola -backend piper -files it_IT-paola-medium.onnx,it_IT-paola-medium.onnx.json -lang it -sentences $S/tts-sentences-it.txt
      "$P" run -phase tts -model piper-it-riccardo -backend piper -files it_IT-riccardo-x_low.onnx,it_IT-riccardo-x_low.onnx.json -lang it -sentences $S/tts-sentences-it.txt ;;
    tts-kokoro) # the English challenger; the backend streams, so the run asks for a stream (recorded as tts_streamed)
      "$P" run -phase tts -model kokoros-en -backend kokoros -files kokoro-v1.0.onnx,voices-v1.0.bin -lang en -sentences $S/tts-sentences-en.txt -tts-stream ;;
    judge-kokoro) # the same judge as for Piper, on the Kokoro audio the tts-kokoro phase wrote last
      "$P" run -phase stt -model whisper-small -backend whisper -files ggml-small-q5_1.bin -sets tts:kokoros-en ;;
    vad)
      "$P" run -phase vad -model silero-vad -backend whisper -files ggml-silero-v5.1.2.bin -sets fleurs,nonspeech ;;
    stt-tiny)
      "$P" run -phase stt -model whisper-tiny -backend whisper -files ggml-tiny-q5_1.bin -sets fleurs,nonspeech ;;
    stt-base)
      "$P" run -phase stt -model whisper-base -backend whisper -files ggml-base-q5_1.bin -sets fleurs,nonspeech ;;
    stt-small) # also the judge of the TTS round trip: it transcribes the audio the tts phases wrote last
      "$P" run -phase stt -model whisper-small -backend whisper -files ggml-small-q5_1.bin -sets fleurs,nonspeech,tts:piper-en-ljspeech,tts:piper-it-paola,tts:piper-it-riccardo ;;
    *) echo "unknown $what" >&2; exit 2 ;;
  esac
done
