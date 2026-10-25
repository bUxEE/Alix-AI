#!/usr/bin/env bash
# Host-side download of the pinned voice model files into a gitignored data directory, each verified against its sha256
# (the pins are those of models/voice-packs/*.yaml plus the extra ladder rungs of docs/evidence/m4-voice/protocol.md).
# A file whose hash differs is deleted and the script fails: nothing unverified stays on disk.
#
#   fetch-models.sh [data dir]      (default: benchmarks/tmp/voice-data, ignored by git)
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../../.." && pwd)"
DATA="${1:-$ROOT/benchmarks/tmp/voice-data}"
mkdir -p "$DATA/models"
WH=https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1
PV=https://huggingface.co/rhasspy/piper-voices/resolve/c10ece1aade47bb51c153c893d14e5bf8e5b7117
VAD=https://huggingface.co/ggml-org/whisper-vad/resolve/9ffd54a1e1ee413ddf265af9913beaf518d1639b
# file | url | sha256
FILES=(
  "ggml-tiny-q5_1.bin|$WH/ggml-tiny-q5_1.bin|818710568da3ca15689e31a743197b520007872ff9576237bda97bd1b469c3d7"
  "ggml-base-q5_1.bin|$WH/ggml-base-q5_1.bin|422f1ae452ade6f30a004d7e5c6a43195e4433bc370bf23fac9cc591f01a8898"
  "ggml-small-q5_1.bin|$WH/ggml-small-q5_1.bin|ae85e4a935d7a567bd102fe55afc16bb595bdb618e11b2fc7591bc08120411bb"
  "ggml-silero-v5.1.2.bin|$VAD/ggml-silero-v5.1.2.bin|29940d98d42b91fbd05ce489f3ecf7c72f0a42f027e4875919a28fb4c04ea2cf"
  "en_US-ljspeech-medium.onnx|$PV/en/en_US/ljspeech/medium/en_US-ljspeech-medium.onnx|6f52a751e2349abe7a76735eb09dc1875298c77ea2342ffd2fef79ff81b87f22"
  "en_US-ljspeech-medium.onnx.json|$PV/en/en_US/ljspeech/medium/en_US-ljspeech-medium.onnx.json|141d612cc0a95ed7efc1ca936b845c2364967f2e9217c5dbfcf69fc4d6c65860"
  "it_IT-paola-medium.onnx|$PV/it/it_IT/paola/medium/it_IT-paola-medium.onnx|6fc918b5a0ea6137382833dddfa567bffbe6a5060c02043c87192ee59c04210c"
  "it_IT-paola-medium.onnx.json|$PV/it/it_IT/paola/medium/it_IT-paola-medium.onnx.json|aea19c0a7fce29fbc359b93f10e7902854401e4c95ae2ea328ae516b15d296cf"
  "it_IT-riccardo-x_low.onnx|$PV/it/it_IT/riccardo/x_low/it_IT-riccardo-x_low.onnx|1368de15f123275a7ef951c9e5e30be0f58a032daa14a0da44037443c1d1d21b"
  "kokoro-v1.0.onnx|https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/kokoro-v1.0.onnx|7d5df8ecf7d4b1878015a32686053fd0eebe2bc377234608764cc0ef3636a6c5"
  "voices-v1.0.bin|https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/voices-v1.0.bin|bca610b8308e8d99f32e6fe4197e7ec01679264efed0cac9140fe9c29f1fbf7d"
  "it_IT-riccardo-x_low.onnx.json|$PV/it/it_IT/riccardo/x_low/it_IT-riccardo-x_low.onnx.json|146ab9c634afe524e9fb7530f2510df7a42fb1db56b52658ca1fb3d98001a62a"
)
for f in "${FILES[@]}"; do
  IFS='|' read -r name url sha <<<"$f"
  if [ -f "$DATA/models/$name" ] && [ "$(sha256sum "$DATA/models/$name" | cut -d' ' -f1)" = "$sha" ]; then echo "ok      $name"; continue; fi
  curl --fail --location --silent --show-error --retry 5 -o "$DATA/models/$name.part" "$url"
  got="$(sha256sum "$DATA/models/$name.part" | cut -d' ' -f1)"
  if [ "$got" != "$sha" ]; then rm -f "$DATA/models/$name.part"; echo "HASH MISMATCH $name: got $got want $sha" >&2; exit 1; fi
  mv "$DATA/models/$name.part" "$DATA/models/$name"; echo "fetched $name"
done
