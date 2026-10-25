#!/usr/bin/env bash
# Footprint of the embedding model on a cold lite stack: time of the first single-text request (backend start + model load +
# one embedding), latency of 30 further single-text requests, and the resident set of the LocalAI processes throughout.
#
#   SPIKE_ENV_FILE=<env> embedding-footprint.sh [localai-model-name]
#
# Writes docs/evidence/m0-spike/rss/<model>-lite.csv and docs/evidence/m0-spike/embedding-footprint.json. The retrieval quality
# is the job of the live test TestSpikeEmbeddingQuality; this is only the cost.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../../.." && pwd)"
: "${SPIKE_ENV_FILE:?}"
model="${1:-granite-embedding-97m-multilingual-r2-q4_k_m}"
KEY="$(grep -E '^LOCALAI_API_KEY=' "$SPIKE_ENV_FILE" | tail -1 | cut -d= -f2-)"
URL=http://127.0.0.1:8080
csv="$ROOT/docs/evidence/m0-spike/rss/$model-lite.csv"
out="$ROOT/docs/evidence/m0-spike/embedding-footprint.json"
export DOCKER_BIN="${DOCKER_BIN:-C:\Program Files\Docker\Docker\resources\bin\docker.exe}"

"$HERE/stack.sh" down lite >/dev/null 2>&1; "$HERE/stack.sh" up lite >/dev/null 2>&1
for _ in $(seq 1 100); do curl -sf -m 3 -o /dev/null "$URL/readyz" && break; sleep 3; done
"$HERE/rss-sampler.sh" "$csv" & sampler=$!
sleep 4
emb() { curl -s -m 120 -o /dev/null -w '%{time_total} %{http_code}' -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$model\",\"input\":\"$1\"}" "$URL/v1/embeddings"; }
read -r first_s first_code <<<"$(emb 'Vorrei prenotare un tavolo per quattro persone domani sera.')"
lat=()
for i in $(seq 1 30); do
  read -r s code <<<"$(emb "Request number $i: do you have a vegetarian option on the menu this evening?")"
  [ "$code" = 200 ] && lat+=("$s")
done
sleep 6
kill "$sampler" 2>/dev/null; wait "$sampler" 2>/dev/null
node - "$(cygpath -m "$csv")" "$(cygpath -m "$out")" "$model" "$first_s" "$first_code" "${lat[*]}" <<'NODE'
const fs = require("node:fs");
const [csv, out, model, firstS, firstCode, latStr] = process.argv.slice(2);
const lat = latStr.split(" ").filter(Boolean).map(Number).sort((a, b) => a - b);
const pct = (p) => lat.length ? Math.round(lat[Math.min(lat.length - 1, Math.ceil(p * lat.length) - 1)] * 1000) : null;
const byT = new Map();
for (const l of fs.readFileSync(csv, "utf8").trim().split("\n").slice(1)) { const [t, , rss, anon] = l.split(","); if (!rss) continue; const o = byT.get(t) || { r: 0, a: 0 }; o.r += +rss; o.a += +anon; byT.set(t, o); }
const v = [...byT.values()].map((o) => o.r).sort((a, b) => a - b);
const anon = Math.max(...[...byT.values()].map((o) => o.a));
const res = {
  task: "M0-13", what: "cost of the embedding model on a cold lite stack (emulated: 8 GB / 4 CPU container cap, i7-10700K host, no GPU)", model, runtime: "LocalAI v4.10.0 llama-cpp",
  first_request_ms: Math.round(+firstS * 1000), first_request_status: +firstCode,
  first_request_note: "first single-text request after the container started: backend start, model load and one embedding",
  single_text_requests: { n: lat.length, p50_ms: pct(0.5), p95_ms: pct(0.95), max_ms: lat.length ? Math.round(lat[lat.length - 1] * 1000) : null },
  rss_mib: { samples: v.length, peak: Math.round(v[v.length - 1] / 1024), median: Math.round(v[Math.floor(v.length / 2)] / 1024), anon_peak: Math.round(anon / 1024), source: "docker exec VmRSS summed over local-ai and the backend; file-backed pages of the memory-mapped weights included" },
  rss_csv: "docs/evidence/m0-spike/rss/" + model + "-lite.csv",
};
fs.writeFileSync(out, JSON.stringify(res, null, 2) + "\n");
console.log(JSON.stringify(res, null, 2));
NODE
