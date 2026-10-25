#!/usr/bin/env bash
# What leaves the LocalAI container (hardening.md section 8 item 6): a tcpdump in a sidecar that shares the LocalAI container's
# network namespace, attached before LocalAI's first outbound connection (compose.capture.yaml delays LocalAI's start), for a
# boot with everything already provisioned, one chat request and a quiet period. The sidecar is a throw-away `docker run`, not
# a service of the stack, and it is removed when the capture ends.
#
#   SPIKE_ENV_FILE=<env> egress-capture.sh [image-with-tcpdump] [quiet-seconds]
set -uo pipefail
docker() { MSYS_NO_PATHCONV=1 command docker "$@"; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && { pwd -W 2>/dev/null || pwd; })"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
: "${SPIKE_ENV_FILE:?}"
IMG="${1:-nicolaka/netshoot:latest}"; QUIET="${2:-90}"
KEY="$(grep -E '^LOCALAI_API_KEY=' "$SPIKE_ENV_FILE" | tail -1 | cut -d= -f2-)"
OUT="$ROOT/docs/evidence/m0-spike/egress/capture-provisioned-boot.txt"
stack() { "$HERE/stack.sh" "$@"; }
stack down lite "$HERE/compose.capture.yaml" >/dev/null 2>&1
SPIKE_CAPTURE_DELAY=15 stack up lite "$HERE/compose.capture.yaml" 2>&1 | tail -1
docker run --rm --net container:alixai-localai --cap-add NET_RAW --cap-add NET_ADMIN --name alixai-m013-tcpdump "$IMG" \
  timeout $((QUIET + 60)) tcpdump -nn -tttt -i any 'not port 22' > "$OUT" 2>&1 &
cap=$!
for _ in $(seq 1 100); do curl -sf -m 3 -o /dev/null http://127.0.0.1:8080/readyz && break; sleep 3; done
curl -s -m 60 -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"alixai-smoke","messages":[{"role":"user","content":"Say ready."}],"max_tokens":4,"temperature":0}' http://127.0.0.1:8080/v1/chat/completions > /dev/null
sleep "$QUIET"
docker stop alixai-m013-tcpdump >/dev/null 2>&1; wait "$cap" 2>/dev/null
stack down lite "$HERE/compose.capture.yaml" >/dev/null 2>&1
node "$ROOT/tests/live/spike/localai/summarize-capture.mjs" "$(cygpath -m "$OUT")"
