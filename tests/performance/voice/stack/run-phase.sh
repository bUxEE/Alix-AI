#!/usr/bin/env bash
# One cold start, one measurement: restarts the LocalAI container (so the first call of the model is a cold load and the resident set
# starts from nothing), waits until it is ready, then runs voicebench with the given arguments.
#
#   VOICE_ENV_FILE=<env file with LOCALAI_API_KEY> run-phase.sh <voicebench command and flags...>
#   e.g. run-phase.sh run -phase stt -model whisper-base -backend whisper -files ggml-base-q5_1.bin
#
# The container must already exist (stack.sh up lite). voicebench is built once into benchmarks/tmp/.
set -euo pipefail
export PATH="/c/Program Files/Docker/Docker/resources/bin:/c/Program Files/Go/bin:$PATH"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "$ROOT"
: "${VOICE_ENV_FILE:?set VOICE_ENV_FILE}"
export ALIXAI_LOCALAI_KEY="$(grep '^LOCALAI_API_KEY=' "$VOICE_ENV_FILE" | cut -d= -f2)"
BIN=benchmarks/tmp/voicebench.exe
if [ ! -x "$BIN" ] || [ -n "$(find tools/voicebench -newer "$BIN" -name '*.go' 2>/dev/null | head -1)" ]; then go build -o "$BIN" ./tools/voicebench; fi
docker restart alixai-localai >/dev/null
for i in $(seq 1 90); do
  if curl -fsS -m 3 http://127.0.0.1:8080/readyz >/dev/null 2>&1; then break; fi
  sleep 2
done
curl -fsS -m 3 http://127.0.0.1:8080/readyz >/dev/null
sleep 3   # let the process tree settle so the first memory sample is the idle service
exec "$BIN" "$@"
