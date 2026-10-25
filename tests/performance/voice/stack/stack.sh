#!/usr/bin/env bash
# Thin wrapper over docker compose for M4-06: the shipped compose files plus the voice override.
#
#   VOICE_ENV_FILE=<env file holding LOCALAI_API_KEY=...> stack.sh up|down|ps|logs|config <lite|balanced> [extra override files...]
#
# The env file stays outside the repository (it holds the API key). VOICE_DATA defaults to benchmarks/tmp/voice-data.
# Extra override files (for example compose.coresident.yaml, or a LOCALAI_MAX_ACTIVE_BACKENDS value in the env file) come last.
set -euo pipefail
export PATH="/c/Program Files/Docker/Docker/resources/bin:$PATH"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && { pwd -W 2>/dev/null || pwd; })"   # Windows form: docker.exe cannot read /e/... paths
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
export VOICE_DATA="${VOICE_DATA:-$(cd "$ROOT/benchmarks/tmp/voice-data" && { pwd -W 2>/dev/null || pwd; })}"
DEPLOY="$ROOT/deploy/container/localai"
cmd="${1:?usage: stack.sh up|down|ps|logs|config <profile> [override...]}"
profile="${2:?profile: lite|balanced}"
shift 2
: "${VOICE_ENV_FILE:?set VOICE_ENV_FILE to an env file that holds LOCALAI_API_KEY}"
files=(-f compose.yaml -f "compose.${profile}.yaml" -f "$HERE/compose.voice.yaml")
for extra in "$@"; do files+=(-f "$extra"); done
cd "$DEPLOY"
case "$cmd" in
  up) exec docker compose --env-file "$VOICE_ENV_FILE" "${files[@]}" up -d ;;
  down) exec docker compose --env-file "$VOICE_ENV_FILE" "${files[@]}" down --remove-orphans ;;
  ps) exec docker compose --env-file "$VOICE_ENV_FILE" "${files[@]}" ps -a ;;
  logs) exec docker compose --env-file "$VOICE_ENV_FILE" "${files[@]}" logs --no-color localai ;;
  config) exec docker compose --env-file "$VOICE_ENV_FILE" "${files[@]}" config ;;
  *) echo "unknown command $cmd" >&2; exit 2 ;;
esac
