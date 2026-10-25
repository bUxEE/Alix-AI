#!/usr/bin/env bash
# One-command reproduction of the published benchmark results (master plan 17.3, milestone 6).
#
#   benchmarks/repro/reproduce.sh [--profile lite|balanced|4g] [--runs id,id] [--out DIR] [--data DIR]
#                                 [--container-bin docker|podman] [--dry-run]
#
# What it does, in order, and stops at the first failure of a step that comes before the measurements:
#   1. verify-inputs: corpus checksums, model hashes against the registry and the model definitions, the
#      runtime image digest, the profile limits and every published result against manifest.json.
#   2. builds tools/bench, then for each (model, profile) pair of the plan: starts the digest-pinned LocalAI
#      container under the profile's memory/CPU caps (the shipped compose files plus compose.repro.yaml, which
#      mounts ONLY that model's definition: LocalAI downloads the file and fails closed on a hash mismatch),
#      and runs tools/bench once per configuration (A: model alone, G: with the deterministic gate).
#   3. compare: every reproduced result against the published one, flagging deviations beyond tolerance.
# Exit status: 0 within tolerance, 1 a run failed or a result deviates, 2 usage or a missing prerequisite.
#
# Needs: go, curl, and docker or podman with the compose plugin. Network is needed on the first run only
# (LocalAI pulls the pinned backend and the sha256-pinned model into --data). Nothing is sent anywhere else;
# no telemetry. The API key is generated per run, kept in this shell's environment and never written to disk.
# Every number it produces is emulated (a cgroup cap on one container), never certified.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"

profile=""; runs=""; out=""; data=""; ce="${CONTAINER_BIN:-}"; dry=0
usage() { sed -n '2,3p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2; exit 2; }
while [ $# -gt 0 ]; do
  case "$1" in
    --profile) profile="${2:?}"; shift 2 ;;
    --runs) runs="${2:?}"; shift 2 ;;
    --out) out="${2:?}"; shift 2 ;;
    --data) data="${2:?}"; shift 2 ;;
    --container-bin) ce="${2:?}"; shift 2 ;;
    --dry-run) dry=1; shift ;;
    -h|--help) usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
out="${out:-$ROOT/benchmarks/tmp/repro/$stamp}"            # benchmarks/tmp is git-ignored
data="${data:-$ROOT/benchmarks/tmp/repro/data}"            # kept between runs: models and backends download once
mkdir -p "$out/runs" "$out/bin" "$data/models" "$data/backends" "$data/state"
out="$(cd "$out" && pwd)"; data="$(cd "$data" && pwd)"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing prerequisite: $1" >&2; exit 2; }; }
need go; need curl
if [ -z "$ce" ]; then
  if command -v docker >/dev/null 2>&1; then ce=docker; elif command -v podman >/dev/null 2>&1; then ce=podman; fi
fi
[ -n "$ce" ] || { echo "missing prerequisite: docker or podman" >&2; exit 2; }
need "$ce"

# docker.exe and podman.exe cannot read Git Bash's /e/... paths; use the mixed form there.
winpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi; }
ctr() { MSYS_NO_PATHCONV=1 "$ce" "$@"; }
run() { if [ "$dry" = 1 ]; then printf '+ %s\n' "$*"; else "$@"; fi; }

cd "$ROOT"

echo "=== 1/3 verify-inputs (no model is run)"
vflags=(-models-dir "$data/models")
go run ./benchmarks/repro/cmd/verify-inputs "${vflags[@]}"

pflags=()
[ -z "$profile" ] || pflags+=(-profile "$profile")
[ -z "$runs" ] || pflags+=(-runs "$runs")
plan="$(go run ./benchmarks/repro/cmd/runplan "${pflags[@]}")"
ids="$(printf '%s\n' "$plan" | cut -f1 | paste -sd, -)"
echo "runs: $ids"

if [ "$dry" = 0 ] && ctr ps -a --format '{{.Names}}' | grep -qx 'alixai-localai'; then
  echo "a container named alixai-localai already exists; stop and remove it first (this script never touches a container it did not start)" >&2
  exit 2
fi

echo "=== 2/3 measurements (output: $out)"
run go build -o "$out/bin/bench" ./tools/bench

if [ "$dry" = 0 ]; then
  KEY="$(openssl rand -hex 32 2>/dev/null || od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
else
  KEY="dry-run-key"
fi
export LOCALAI_API_KEY="$KEY" ALIXAI_LOCALAI_KEY="$KEY" ALIXAI_LOCALAI_URL="http://127.0.0.1:8080"
export LOCALAI_MAX_ACTIVE_BACKENDS=1 REPRO_DATA="$(winpath "$data")"

cur=""; cfiles=()
stack_down() {
  if [ -n "$cur" ] && [ "${#cfiles[@]}" -gt 0 ]; then
    run ctr compose "${cfiles[@]}" down --remove-orphans >/dev/null 2>&1 || true
  fi
  cur=""
}
trap stack_down EXIT

failures=0
while IFS=$'\t' read -r id model prof gate file defn corpus constraint ctx maxtoks warmup timeout sample compose pcompose override; do
  [ -n "$id" ] || continue
  key="$model/$prof"
  if [ "$key" != "$cur" ]; then
    stack_down
    export REPRO_MODELS_CONFIG; REPRO_MODELS_CONFIG="$(winpath "$ROOT/$defn")"
    cfiles=(-f "$(winpath "$ROOT/$compose")" -f "$(winpath "$ROOT/$pcompose")" -f "$(winpath "$ROOT/$override")")
    echo "--- $(date '+%H:%M:%S') start $model under $prof"
    if ! run ctr compose "${cfiles[@]}" up -d; then echo "stack failed to start" >&2; cur="$key"; stack_down; failures=$((failures + 1)); continue; fi
    cur="$key"
    ready=0
    if [ "$dry" = 1 ]; then ready=1; else
      for _ in $(seq 1 300); do curl -sf -m 3 -o /dev/null "$ALIXAI_LOCALAI_URL/readyz" && { ready=1; break; }; sleep 3; done
    fi
    if [ "$ready" != 1 ]; then echo "LocalAI did not become ready for $model" >&2; stack_down; failures=$((failures + 1)); continue; fi
  fi
  echo "--- $(date '+%H:%M:%S') bench $id"
  mkdir -p "$out/runs/$id"
  benchcmd=("$out/bin/bench" run --provider localai --model "$model" --profile "$prof" --constraint "$constraint"
    --corpus "$corpus" --out "$out/runs/$id" --artifact "$data/models/$file" --context-size "$ctx" --warmup "$warmup"
    --max-toks "$maxtoks" --timeout "$timeout" --docker-container alixai-localai --docker-bin "$ce" --sample-interval "$sample")
  [ "$gate" != true ] || benchcmd+=(--gate)
  if [ "$dry" = 1 ]; then printf '+ %s
' "${benchcmd[*]}"; continue; fi
  if ! "${benchcmd[@]}" > "$out/runs/$id/bench.log" 2>&1 </dev/null; then
    echo "bench $id failed; see $out/runs/$id/bench.log" >&2
    failures=$((failures + 1))
  fi
done <<< "$plan"
stack_down
trap - EXIT

echo "=== 3/3 compare with the published results"
if [ "$dry" = 1 ]; then
  echo "+ go run ./benchmarks/repro/cmd/compare -reproduced $out/runs -runs $ids"
  exit 0
fi
rc=0
go run ./benchmarks/repro/cmd/compare -reproduced "$out/runs" -runs "$ids" | tee "$out/compare.txt" || rc=$?
[ "$failures" = 0 ] || { echo "$failures run or stack step(s) failed" >&2; rc=1; }
exit "$rc"
