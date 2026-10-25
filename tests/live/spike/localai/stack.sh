#!/usr/bin/env bash
# Thin wrapper over docker compose for the M0-13 spike: the shipped compose files plus the spike overrides.
#
#   SPIKE_ENV_FILE=<env file with LOCALAI_API_KEY> \
#     stack.sh up|down|ps|logs|config <lite|balanced|4g|none> [extra override files...]
#
# The env file stays outside the repository (it holds the API key). Extra override files are appended last
# (for example the hardening experiments).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && { pwd -W 2>/dev/null || pwd; })"   # Windows form on Git Bash: docker.exe cannot read /e/... paths
DEPLOY="$(cd "$HERE/../../../../deploy/container/localai" && pwd)"
cmd="${1:?usage: stack.sh up|down|ps|logs|config <profile> [override...]}"
profile="${2:?profile: lite|balanced|4g|none}"
shift 2
: "${SPIKE_ENV_FILE:?set SPIKE_ENV_FILE to an env file that holds LOCALAI_API_KEY}"

files=(-f compose.yaml)
[ "$profile" = none ] || files+=(-f "compose.${profile}.yaml")
files+=(-f "$HERE/compose.spike.yaml")
for extra in "$@"; do files+=(-f "$extra"); done

cd "$DEPLOY"
mkdir -p data/models data/backends data/state
case "$cmd" in
  up) exec docker compose --env-file "$SPIKE_ENV_FILE" "${files[@]}" up -d ;;
  down) exec docker compose --env-file "$SPIKE_ENV_FILE" "${files[@]}" down --remove-orphans ;;
  ps) exec docker compose --env-file "$SPIKE_ENV_FILE" "${files[@]}" ps -a ;;
  logs) exec docker compose --env-file "$SPIKE_ENV_FILE" "${files[@]}" logs --no-color localai ;;
  config) exec docker compose --env-file "$SPIKE_ENV_FILE" "${files[@]}" config ;;
  *) echo "unknown command $cmd" >&2; exit 2 ;;
esac
