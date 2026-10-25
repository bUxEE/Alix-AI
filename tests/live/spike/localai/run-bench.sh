#!/usr/bin/env bash
# One tools/bench run against the running spike stack (stack.sh up <profile> ...), with the flags every M0-13 result uses.
#
#   SPIKE_ENV_FILE=<env file> run-bench.sh <registry-id> <lite|balanced|4g> <grammar|schema|none> [out-dir] [extra bench flags...]
#
# out-dir defaults to benchmarks/results/m0 (evidence); trial runs go to benchmarks/tmp. The model file's SHA-256 is
# computed by the tool (--artifact) and must equal the registry pin. Memory is sampled from the container with docker stats
# (an absolute docker path is passed because Go's exec.LookPath cannot find docker.exe on this machine).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../../.." && pwd)"
id="${1:?registry id}"; profile="${2:?profile}"; constraint="${3:?constraint}"; out="${4:-benchmarks/results/m0}"   # relative to the repository root
shift $(( $# < 4 ? $# : 4 ))
: "${SPIKE_ENV_FILE:?set SPIKE_ENV_FILE}"
export ALIXAI_LOCALAI_URL="${ALIXAI_LOCALAI_URL:-http://127.0.0.1:8080}"
export ALIXAI_LOCALAI_KEY="$(grep -E '^LOCALAI_API_KEY=' "$SPIKE_ENV_FILE" | tail -1 | cut -d= -f2-)"
file="$(awk -v id="$id" '$0 ~ "^id: " id "$" {f=1} f && /^  filename:/ {print $2; exit}' "$ROOT/models/registry/$id.yaml")"
[ -n "$file" ] || { echo "no artifact.filename for $id" >&2; exit 2; }
BENCH="${BENCH_BIN:-$ROOT/benchmarks/tmp/bench.exe}"
DOCKER_BIN="${DOCKER_BIN:-C:\Program Files\Docker\Docker\resources\bin\docker.exe}"
cd "$ROOT"
exec "$BENCH" run --provider localai --model "$id" --profile "$profile" --constraint "$constraint" \
  --corpus tests/performance/corpus/routing-v0.jsonl --out "$out" \
  --artifact "deploy/container/localai/data/models/$file" \
  --context-size 4096 --warmup 1 --max-toks 256 --timeout 3m \
  --docker-container alixai-localai --docker-bin "$DOCKER_BIN" --sample-interval 2s "$@"
