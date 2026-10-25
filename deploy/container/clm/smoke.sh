#!/usr/bin/env bash
# Smoke test for the optional CLM sidecar: start the stack, wait for readiness, check the posture, send one rank
# request, tear down. Not run by `plan verify`; the live run is task D-08. Results are EMULATED, never certified.
#
#   ./smoke.sh [balanced|highmem|none]      (default: highmem; "none" = compose.yaml only)
#
# Requires the files in ./data (run ./fetch.sh first) and the CLM and encoder entries in models/registry (D-11).
# Wall-clock: re-hashing ~16 GB and loading the encoder take minutes; whether the CPU path works at all is untested.
#
# Environment:
#   COMPOSE              compose command             (default: "docker compose"; e.g. "podman compose")
#   SMOKE_READY_TIMEOUT  seconds to wait for /health  (default: 3600)
#   SMOKE_KEEP_UP=1      leave the stack running on success
#
# The API key comes from ./.env when present, otherwise a random one is generated for this run.
# shellcheck disable=SC2086  # $COMPOSE is intentionally word-split ("docker compose", "podman compose")
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

PROFILE="${1:-highmem}"
COMPOSE="${COMPOSE:-docker compose}"
READY_TIMEOUT="${SMOKE_READY_TIMEOUT:-3600}"

case "$PROFILE" in
  balanced | highmem) FILES=(-f compose.yaml -f "compose.${PROFILE}.yaml") ;;
  none) FILES=(-f compose.yaml) ;;
  *) echo "usage: $0 [balanced|highmem|none]" >&2; exit 2 ;;
esac

fail() { echo "SMOKE FAIL: $*" >&2; exit 1; }
step() { echo "==> $*"; }

TMP="$(mktemp -d)"
STARTED=0
cleanup() {
  local rc=$?
  if [ "$STARTED" = 1 ] && { [ "$rc" != 0 ] || [ "${SMOKE_KEEP_UP:-0}" != 1 ]; }; then
    [ "$rc" = 0 ] || $COMPOSE --env-file "$ENV_FILE" "${FILES[@]}" logs --tail 80 >&2 || true
    step "tearing down (data in ./data is kept)"
    $COMPOSE --env-file "$ENV_FILE" "${FILES[@]}" down --remove-orphans >/dev/null 2>&1 || true
  fi
  rm -rf "$TMP"
  exit "$rc"
}
trap cleanup EXIT

[ -d data/encoder ] && [ -d data/clm ] || fail "./data/encoder and ./data/clm are missing: run ./fetch.sh first"

# --- API key and env file ---------------------------------------------------------------------
if [ -f .env ]; then
  ENV_FILE=".env"
  KEY="$(grep -E '^CLM_API_KEY=' .env | tail -1 | cut -d= -f2- | tr -d "\"'" || true)"
else
  ENV_FILE="$TMP/smoke.env"
  KEY="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
  umask 077
  printf 'CLM_API_KEY=%s\n' "$KEY" > "$ENV_FILE"
fi
PORT="$(grep -E '^CLM_PORT=' "$ENV_FILE" | tail -1 | cut -d= -f2- || true)"
PORT="${PORT:-8700}"
BASE_URL="http://127.0.0.1:$PORT"
# Keep the key off the curl command line (visible in the process list).
printf 'Authorization: Bearer %s\n' "$KEY" > "$TMP/auth.hdr"

step "validating compose config (profile: $PROFILE)"
$COMPOSE --env-file "$ENV_FILE" "${FILES[@]}" config -q

step "starting"
STARTED=1
$COMPOSE --env-file "$ENV_FILE" "${FILES[@]}" up -d

# --- posture: the CLM port is on loopback, the encoder publishes nothing -----------------------
if published="$($COMPOSE --env-file "$ENV_FILE" "${FILES[@]}" port clm 8700 2>/dev/null)"; then
  case "$published" in
    127.0.0.1:*) echo "port 8700 published on $published" ;;
    *) fail "port 8700 is published on '$published', expected 127.0.0.1" ;;
  esac
else
  echo "note: '$COMPOSE port' unavailable, skipped published-address check" >&2
fi
if $COMPOSE --env-file "$ENV_FILE" "${FILES[@]}" port vllm 8090 2>/dev/null | grep -q ':'; then
  fail "the encoder publishes port 8090; it must have no published port"
fi

# --- readiness ----------------------------------------------------------------------------------
step "waiting for the CLM service (up to ${READY_TIMEOUT}s)"
deadline=$((SECONDS + READY_TIMEOUT))
until curl -fsS -m 5 -o /dev/null "$BASE_URL/health" 2>/dev/null; do
  if $COMPOSE --env-file "$ENV_FILE" "${FILES[@]}" ps -a 2>/dev/null | grep -Eqi 'exited|dead'; then
    fail "a container exited (exit 78 is a refused start: see the logs above)"
  fi
  [ "$SECONDS" -lt "$deadline" ] || fail "not ready after ${READY_TIMEOUT}s"
  sleep 10
done
echo "ready after ${SECONDS}s"

# --- auth fails closed --------------------------------------------------------------------------
step "unauthenticated request must be rejected"
code="$(curl -s -m 10 -o /dev/null -w '%{http_code}' "$BASE_URL/v1/models" || true)"
case "$code" in
  401 | 403) echo "GET /v1/models without key -> $code" ;;
  *) fail "GET /v1/models without a key returned '$code', expected 401/403" ;;
esac

step "the playground must be absent"
code="$(curl -s -m 10 -o /dev/null -w '%{http_code}' -H @"$TMP/auth.hdr" "$BASE_URL/" || true)"
case "$code" in
  404 | 405) echo "GET / -> $code" ;;
  *) fail "GET / returned '$code', expected 404/405 (--no-ui)" ;;
esac

# --- one rank request over a fixed, authorised candidate list (no text is generated) -------------
step "rank request (first call embeds with the CPU encoder)"
resp="$(curl -sS -m 600 -w '\n%{http_code}' -H @"$TMP/auth.hdr" -H 'Content-Type: application/json' \
  -d '{"context":"The user asks to move tomorrow'"'"'s appointment.","question":"Which capability does the state ask for?","answers":["reschedule_booking","cancel_booking","show_opening_hours"]}' \
  "$BASE_URL/v1/rank")" || fail "rank request failed"
code="${resp##*$'\n'}"
body="${resp%$'\n'*}"
[ "$code" = 200 ] || fail "rank returned HTTP $code: $body"
echo "$body" | grep -q '"ranked"' || fail "rank answer has no ranked list: $body"
echo "$body"

if [ "$PROFILE" = none ]; then label="unconstrained"; else label="EMULATED ($PROFILE)"; fi
step "SMOKE PASS [$label]"
