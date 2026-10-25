#!/usr/bin/env bash
# The M0-13 measurement matrix: for each (model, profile) restart the stack so every model starts cold under the profile's
# cgroup limits, sample process RSS, run tools/bench with each constraint (grammar is the gate run, schema and none are
# comparison runs), then the adapter's live tests against that model. Resumable: a finished step leaves a marker in
# $SPIKE_STATE and is skipped on the next invocation. SPIKE_RSS_SUFFIX (for example "-none") keeps a later run of the same
# model and profile from overwriting the RSS samples of the earlier one.
#
#   SPIKE_ENV_FILE=<env> SPIKE_STATE=<dir> matrix.sh "<id> <profile> <constraints,comma>" ...
#   e.g. matrix.sh "granite-4.0-350m-q4_k_m lite grammar,schema,none" "qwen3.5-2b-q4_k_m balanced grammar,schema,none"
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../../.." && pwd)"
: "${SPIKE_ENV_FILE:?}"; : "${SPIKE_STATE:?}"
mkdir -p "$SPIKE_STATE" "$ROOT/docs/evidence/m0-spike/logs" "$ROOT/docs/evidence/m0-spike/rss" "$ROOT/benchmarks/results/m0"
export DOCKER_BIN="${DOCKER_BIN:-C:\Program Files\Docker\Docker\resources\bin\docker.exe}"
KEY="$(grep -E '^LOCALAI_API_KEY=' "$SPIKE_ENV_FILE" | tail -1 | cut -d= -f2-)"
ready() { for _ in $(seq 1 120); do curl -sf -m 3 -o /dev/null http://127.0.0.1:8080/readyz && return 0; sleep 3; done; return 1; }

for spec in "$@"; do
  read -r id profile constraints <<<"$spec"
  tag="$id-$profile"
  echo "=== $(date '+%H:%M:%S') $tag ($constraints)"
  "$HERE/stack.sh" down "$profile" >/dev/null 2>&1
  "$HERE/stack.sh" up "$profile" >/dev/null 2>&1 || { echo "stack up failed for $tag"; continue; }
  ready || { echo "not ready: $tag"; continue; }
  "$HERE/rss-sampler.sh" "$ROOT/docs/evidence/m0-spike/rss/$tag${SPIKE_RSS_SUFFIX:-}.csv" & sampler=$!
  IFS=, read -ra cs <<<"$constraints"
  for c in "${cs[@]}"; do
    [ -f "$SPIKE_STATE/$tag-$c.done" ] && { echo "skip $tag $c (done)"; continue; }
    echo "--- $(date '+%H:%M:%S') bench $tag $c"
    if "$HERE/run-bench.sh" "$id" "$profile" "$c" benchmarks/results/m0 > "$ROOT/docs/evidence/m0-spike/logs/bench-$tag-$c.log" 2>&1; then
      touch "$SPIKE_STATE/$tag-$c.done"; tail -14 "$ROOT/docs/evidence/m0-spike/logs/bench-$tag-$c.log"
    else
      echo "bench FAILED $tag $c"; tail -5 "$ROOT/docs/evidence/m0-spike/logs/bench-$tag-$c.log"
    fi
  done
  if [ ! -f "$SPIKE_STATE/$tag-live.done" ]; then
    echo "--- $(date '+%H:%M:%S') live tests $tag"
    ( cd "$ROOT" && ALIXAI_LOCALAI_URL=http://127.0.0.1:8080 ALIXAI_LOCALAI_KEY="$KEY" ALIXAI_LOCALAI_MODEL="$id" \
        ./benchmarks/tmp/localai.live.test.exe -test.v -test.count=1 -test.run 'TestLive' ) > "$ROOT/docs/evidence/m0-spike/logs/live-$id.log" 2>&1
    echo "live exit=$?"; touch "$SPIKE_STATE/$tag-live.done"
  fi
  kill "$sampler" 2>/dev/null; wait "$sampler" 2>/dev/null
done
echo "=== matrix finished $(date '+%H:%M:%S')"
