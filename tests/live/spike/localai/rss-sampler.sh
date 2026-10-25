#!/usr/bin/env bash
# Samples the resident set of the LocalAI processes inside the container every 2 s into a CSV, until it is killed.
# This is the figure docker stats cannot give (docker stats reports cgroup usage minus inactive file cache): per process
# VmRSS split into anonymous pages and file-backed pages (the memory-mapped model weights are RssFile).
#
#   rss-sampler.sh <out.csv> [container]
set -u
export MSYS_NO_PATHCONV=1   # the /proc paths in the docker exec script must reach the container unchanged
out="${1:?out csv}"; c="${2:-alixai-localai}"
DOCKER="${DOCKER_BIN:-docker}"
echo "unix_s,process,rss_kb,rss_anon_kb,rss_file_kb" > "$out"
while true; do
  now="$(date +%s)"
  "$DOCKER" exec "$c" sh -c '
    for p in /proc/[0-9]*; do
      n=$(cat "$p/comm" 2>/dev/null) || continue
      case "$n" in
        local-ai|llama*|ld.so) awk -v n="$n" "/^VmRSS:/{r=\$2} /^RssAnon:/{a=\$2} /^RssFile:/{f=\$2} END{print n \",\" r \",\" a \",\" f}" "$p/status" ;;
      esac
    done' 2>/dev/null | sed "s/^/$now,/" >> "$out"
  sleep 2
done
