#!/usr/bin/env bash
# Smoke test for the private LocalAI runtime: start the stack, wait for readiness, check the
# security posture, send one chat completion, tear down.
#
#   ./smoke.sh [lite|balanced|4g|none]      (default: lite; "none" = compose.yaml only)
#
# Not run by `plan verify`; the live run is task M0-13. First run downloads the backend image
# and the model file (see README.md), so allow time; both are cached in ./data afterwards.
#
# Environment:
#   COMPOSE              compose command            (default: "docker compose"; e.g. "podman compose")
#   SMOKE_MODEL          model name to exercise     (default: alixai-smoke, from config/models.yaml)
#   SMOKE_READY_TIMEOUT  seconds to wait for /readyz (default: 1800)
#   SMOKE_CHAT_TIMEOUT   seconds for the chat call   (default: 300; includes first backend start)
#   SMOKE_KEEP_UP=1      leave the stack running on success
#
# The API key comes from ./.env when present, otherwise a random one is generated for this run.
# Results are labelled EMULATED unless profile is "none".
# shellcheck disable=SC2086  # $COMPOSE is intentionally word-split ("docker compose", "podman compose")
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

PROFILE="${1:-lite}"
COMPOSE="${COMPOSE:-docker compose}"
SMOKE_MODEL="${SMOKE_MODEL:-alixai-smoke}"
READY_TIMEOUT="${SMOKE_READY_TIMEOUT:-1800}"
CHAT_TIMEOUT="${SMOKE_CHAT_TIMEOUT:-300}"
BASE_URL="http://127.0.0.1:8080"

case "$PROFILE" in
  lite | balanced | 4g) FILES=(-f compose.yaml -f "compose.${PROFILE}.yaml") ;;
  none) FILES=(-f compose.yaml) ;;
  *) echo "usage: $0 [lite|balanced|4g|none]" >&2; exit 2 ;;
esac

fail() { echo "SMOKE FAIL: $*" >&2; exit 1; }
step() { echo "==> $*"; }

TMP="$(mktemp -d)"
STARTED=0
cleanup() {
  local rc=$?
  if [ "$STARTED" = 1 ] && { [ "$rc" != 0 ] || [ "${SMOKE_KEEP_UP:-0}" != 1 ]; }; then
    [ "$rc" = 0 ] || $COMPOSE --env-file "$ENV_FILE" "${FILES[@]}" logs --tail 80 localai >&2 || true
    step "tearing down (data in ./data is kept)"
    $COMPOSE --env-file "$ENV_FILE" "${FILES[@]}" down --remove-orphans >/dev/null 2>&1 || true
  fi
  rm -rf "$TMP"
  exit "$rc"
}
trap cleanup EXIT

# Podman does not create missing bind-mount sources; Docker would create them as root.
mkdir -p data/models data/backends data/state

# --- API key and env file -------------------------------------------------------------------
if [ -f .env ]; then
  ENV_FILE=".env"
  KEY="$(grep -E '^LOCALAI_API_KEY=' .env | tail -1 | cut -d= -f2- | tr -d "\"'" || true)"
  [ -n "$KEY" ] || fail ".env has no LOCALAI_API_KEY"
else
  ENV_FILE="$TMP/smoke.env"
  KEY="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
  umask 077
  printf 'LOCALAI_API_KEY=%s\n' "$KEY" > "$ENV_FILE"
fi
# Keep the key off the curl command line (visible in the process list).
printf 'Authorization: Bearer %s\n' "$KEY" > "$TMP/auth.hdr"

step "validating compose config (profile: $PROFILE)"
$COMPOSE --env-file "$ENV_FILE" "${FILES[@]}" config -q

step "starting"
STARTED=1
$COMPOSE --env-file "$ENV_FILE" "${FILES[@]}" up -d

# --- posture: published on loopback only ----------------------------------------------------
if published="$($COMPOSE --env-file "$ENV_FILE" "${FILES[@]}" port localai 8080 2>/dev/null)"; then
  case "$published" in
    127.0.0.1:8080*) echo "port 8080 published on $published" ;;
    *) fail "port 8080 is published on '$published', expected 127.0.0.1" ;;
  esac
else
  echo "note: '$COMPOSE port' unavailable, skipped published-address check" >&2
fi

# --- readiness ------------------------------------------------------------------------------
step "waiting for /readyz (up to ${READY_TIMEOUT}s; first run downloads backend + model)"
deadline=$((SECONDS + READY_TIMEOUT))
until curl -fsS -m 5 -o /dev/null "$BASE_URL/readyz" 2>/dev/null; do
  if $COMPOSE --env-file "$ENV_FILE" "${FILES[@]}" ps -a 2>/dev/null | grep -Eqi 'exited|restarting|dead'; then
    fail "container exited or is restarting (see logs above)"
  fi
  [ "$SECONDS" -lt "$deadline" ] || fail "not ready after ${READY_TIMEOUT}s"
  sleep 5
done
echo "ready after ${SECONDS}s"

# --- auth fails closed ----------------------------------------------------------------------
step "unauthenticated request must be rejected"
code="$(curl -s -m 10 -o /dev/null -w '%{http_code}' "$BASE_URL/v1/models" || true)"
case "$code" in
  401 | 403) echo "GET /v1/models without key -> $code" ;;
  *) fail "GET /v1/models without a key returned '$code', expected 401/403" ;;
esac

# --- disabled surfaces really are gone ------------------------------------------------------
step "web UI and gallery endpoints must be absent"
for path in /app /models/available /api/settings /v1/mcp/servers/x; do
  code="$(curl -s -m 10 -o /dev/null -w '%{http_code}' -H @"$TMP/auth.hdr" "$BASE_URL$path" || true)"
  case "$code" in
    404 | 405) echo "GET $path -> $code" ;;
    *) fail "GET $path returned '$code' with a valid key, expected 404/405 (surface should be disabled)" ;;
  esac
done

# --- the model really loaded (readiness alone does not prove it: preload errors are only logged) ---
step "model list must contain $SMOKE_MODEL"
models="$(curl -fsS -m 10 -H @"$TMP/auth.hdr" "$BASE_URL/v1/models")" || fail "GET /v1/models with key failed"
echo "$models" | grep -q "\"$SMOKE_MODEL\"" || fail "/v1/models does not list '$SMOKE_MODEL': $models"

# --- one chat completion --------------------------------------------------------------------
step "chat completion (first call starts the backend and loads the model)"
resp="$(curl -sS -m "$CHAT_TIMEOUT" -w '\n%{http_code}' -H @"$TMP/auth.hdr" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$SMOKE_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with the single word: ready\"}],\"max_tokens\":16,\"temperature\":0}" \
  "$BASE_URL/v1/chat/completions")" || fail "chat completion request failed"
code="${resp##*$'\n'}"
body="${resp%$'\n'*}"
[ "$code" = 200 ] || fail "chat completion returned HTTP $code: $body"
echo "$body" | grep -Eq '"content"[[:space:]]*:[[:space:]]*"[^"]' || fail "chat completion has no content: $body"
echo "$body"

if [ "$PROFILE" = none ]; then label="unconstrained"; else label="EMULATED ($PROFILE)"; fi
step "SMOKE PASS [$label]"
