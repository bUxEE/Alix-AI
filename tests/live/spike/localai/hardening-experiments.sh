#!/usr/bin/env bash
# The live part of deploy/container/localai/hardening.md section 8 and docs/security/localai-hardening-checklist.md section 5.
# One experiment = one compose up with an override from experiments/, a fixed set of probes, one log file, one down.
#
#   SPIKE_ENV_FILE=<env> hardening-experiments.sh <experiment>...
#   experiments: baseline restart internal-network no-caps only-dac-override nonroot readonly-rootfs strict-integrity
#                wrong-sha no-caps-install readonly-models trace-log debug-log info-log
#
# Every probe prints "CHECK <name>: <observed>" so the evidence is greppable. Nothing here changes the repository except the
# log files; the data it needs (models, backends) is the gitignored deploy/container/localai/data tree. Runs on Docker
# Desktop (a Linux VM on a Windows host): ownership, permission and capability results are that platform's, not a native
# Linux host's or rootless Podman's, and the report says so.
set -uo pipefail
# Git Bash rewrites absolute-path arguments for native programs. docker needs them untouched (/proc, /models, /dev/null inside
# the container), curl on the host needs them rewritten (-o /dev/null), so only docker is switched off.
docker() { MSYS_NO_PATHCONV=1 command docker "$@"; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && { pwd -W 2>/dev/null || pwd; })"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
DEPLOY="$ROOT/deploy/container/localai"
LOGDIR="$ROOT/docs/evidence/m0-spike/logs"
: "${SPIKE_ENV_FILE:?}"
KEY="$(grep -E '^LOCALAI_API_KEY=' "$SPIKE_ENV_FILE" | tail -1 | cut -d= -f2-)"
URL=http://127.0.0.1:8080
C=alixai-localai
mkdir -p "$LOGDIR" "$DEPLOY/data"
stack() { "$ROOT/tests/live/spike/localai/stack.sh" "$@"; }
ready() { for _ in $(seq 1 100); do curl -sf -m 3 -o /dev/null "$URL/readyz" && return 0; sleep 3; done; return 1; }
api() { curl -s -m 90 -H "Authorization: Bearer $KEY" "$@"; }
code() { curl -s -m 10 -o /dev/null -w '%{http_code}' "$@"; }
chat() { api -X POST "$URL/v1/chat/completions" -H 'Content-Type: application/json' -d "{\"model\":\"${1:-alixai-smoke}\",\"messages\":[{\"role\":\"user\",\"content\":\"${2:-Say ready.}\"}],\"max_tokens\":8,\"temperature\":0}"; }
check() { local name="$1"; shift; echo "CHECK $name: $*"; }
# Listening TCP sockets inside the container, decoded from /proc/net/tcp{,6} (the image has neither ss nor netstat).
listeners() {
  docker exec "$C" sh -c 'cat /proc/net/tcp /proc/net/tcp6' 2>/dev/null | awk '
    function hex(s,  i,n,c){n=0;for(i=1;i<=length(s);i++){c=index("0123456789ABCDEF",toupper(substr(s,i,1)))-1;n=n*16+c}return n}
    $4=="0A"{split($2,a,":"); ip=a[1]; port=hex(a[2]);
      if(length(ip)==8){addr=sprintf("%d.%d.%d.%d",hex(substr(ip,7,2)),hex(substr(ip,5,2)),hex(substr(ip,3,2)),hex(substr(ip,1,2)))}
      else if(ip=="00000000000000000000000000000000"){addr="[::]"} else if(ip=="00000000000000000000000001000000"){addr="[::1]"} else {addr="[" ip "]"}
      print addr ":" port}' | sort -u
}
caps() { docker exec "$C" sh -c 'grep -E "^(CapEff|CapBnd|NoNewPrivs|Seccomp):" /proc/1/status' 2>/dev/null | tr '\n\t' '; ' ; echo; }
common_probes() {
  check "readyz" "$(code "$URL/readyz")"
  check "models-without-key" "$(code "$URL/v1/models")"
  check "models-with-key" "$(code -H "Authorization: Bearer $KEY" "$URL/v1/models")"
  check "chat" "$(chat | head -c 300)"
  check "identity" "$(docker exec "$C" id 2>&1)"
  check "capabilities" "$(caps)"
  check "listeners" "$(listeners | tr '\n' ' ')"
  check "docker-port" "$(docker port "$C" 2>&1 | tr '\n' ' ')"
}
begin() { LOG="$LOGDIR/hardening-$1.log"; exec 3>&1; exec >"$LOG" 2>&1; echo "== $1 ($(date '+%Y-%m-%d %H:%M:%S')) emulated on Docker Desktop; profile lite"; }
end() { exec >&3 2>&1; echo "done $1 -> $LOG"; }

run_with() { # name, then override file basenames
  local name="$1"; shift
  local ovr=(); for o in "$@"; do ovr+=("$HERE/experiments/$o"); done
  begin "$name"
  stack down lite "${ovr[@]}" >/dev/null 2>&1
  echo "-- up with: ${ovr[*]:-no override}"
  stack up lite "${ovr[@]}" 2>&1 | tail -3
  if ready; then echo "ready"; else echo "NOT READY within 300 s"; fi
  common_probes
  echo "-- container log (tail 60)"; docker logs --tail 60 "$C" 2>&1 | cut -c1-300
  stack down lite "${ovr[@]}" >/dev/null 2>&1
  end "$name"
}


# HC-34: what do the debug and trace log levels print? Two canaries: one in the prompt, one the model is asked to repeat, so the
# probe can tell "the prompt is logged" from "the model's answer is logged" (matched case-insensitively: the 350M model answers
# "Canary-..." for "CANARY-..."). The log is read with the ANSI colour codes stripped
# (they sit between the fields of every line and defeat a plain grep), and the controls prove the request itself reached the log.
log_level_experiment() { # name, override file, prompt canary, echo canary
  local name="$1" ovr="$HERE/experiments/$2" canary="$3" echo_canary="$4" plain
  begin "$name"
  stack down lite "$ovr" >/dev/null 2>&1; stack up lite "$ovr" 2>&1 | tail -2; ready && echo ready
  chat alixai-smoke "$canary say ready" | head -c 200; echo
  api -X POST "$URL/v1/chat/completions" -H 'Content-Type: application/json' \
    -d "{\"model\":\"alixai-smoke\",\"messages\":[{\"role\":\"user\",\"content\":\"Repeat this word exactly and nothing else: $echo_canary\"}],\"max_tokens\":32,\"temperature\":0}" | head -c 400; echo
  plain="$(docker logs "$C" 2>&1 | sed 's/\x1b\[[0-9;]*m//g')"
  check "log-line-count" "$(printf '%s\n' "$plain" | wc -l) lines"
  check "control-chat-request-lines" "$(printf '%s\n' "$plain" | grep -c 'HTTP request method="POST" path="/v1/chat/completions" status=200') (the request line is in the log)"
  check "control-response-lines" "$(printf '%s\n' "$plain" | grep -c ' Response response=') ('Response response=' lines: the answer body)"
  check "prompt-canary-in-stdout" "$(printf '%s\n' "$plain" | grep -ci "$canary") matching lines"
  check "echo-canary-in-stdout" "$(printf '%s\n' "$plain" | grep -ci "$echo_canary") matching lines (the model was asked to repeat it)"
  printf '%s\n' "$plain" | grep -i "$echo_canary" | head -3 | cut -c1-300
  check "canary-on-disk-in-container" "$(docker exec "$C" sh -c "grep -rli -e '$canary' -e '$echo_canary' /data /tmp /models /configuration /backends 2>/dev/null | head -5 | tr '\n' ' '") (empty = none)"
  check "data-traces-files" "$(docker exec "$C" sh -c 'find /data/traces -type f 2>/dev/null | wc -l') files under /data/traces"
  stack down lite "$ovr" >/dev/null 2>&1; end "$name"
}

for exp in "$@"; do
  case "$exp" in
    baseline)
      begin baseline
      stack down lite >/dev/null 2>&1; stack up lite 2>&1 | tail -2; ready && echo ready
      common_probes
      check "mounts" "$(docker inspect "$C" --format '{{range .Mounts}}{{.Source}} -> {{.Destination}} ({{if .RW}}rw{{else}}ro{{end}}); {{end}}')"
      check "network-mode" "$(docker inspect "$C" --format '{{.HostConfig.NetworkMode}} privileged={{.HostConfig.Privileged}} readonly-rootfs={{.HostConfig.ReadonlyRootfs}} pids-limit={{.HostConfig.PidsLimit}}')"
      lan="$(ipconfig 2>/dev/null | tr -d '\r' | awk '/IPv4/ {print $NF}' | grep -E '^(192\.168|10\.|172\.(1[6-9]|2[0-9]|3[01]))\.' | head -1)"
      check "lan-address" "${lan:-none found}"
      [ -n "${lan:-}" ] && check "lan-reach" "GET http://$lan:8080/readyz -> $(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://$lan:8080/readyz" || true) (000 = refused or timed out)"
      # An unprompted canary must not appear in the info-level log; the chat path logs the request line only.
      chat alixai-smoke "CANARY-INFO-7f3a say ready" >/dev/null
      check "info-log-has-prompt" "$(docker logs "$C" 2>&1 | grep -c 'CANARY-INFO-7f3a') matching lines (0 = prompts are not logged at info)"
      check "info-log-chat-lines" "$(docker logs "$C" 2>&1 | grep -E 'chat/completions' | tail -2 | cut -c1-200 | tr '\n' '|')"
      check "outbound-https-from-container-default-network" "$(docker exec "$C" curl -sS -m 8 -o /dev/null -w 'http %{http_code}' https://huggingface.co 2>&1 | tr '
' ' ')"
      check "outbound-plain-http-from-container-default-network" "$(docker exec "$C" curl -sS -m 8 -o /dev/null -w 'http %{http_code}' http://example.com 2>&1 | tr '
' ' ')"
      check "dns-from-container-default-network" "$(docker exec "$C" getent hosts huggingface.co 2>&1 | head -1)"
      check "periodic-upgrade-check-in-log" "$(docker logs "$C" 2>&1 | grep -ci 'upgrade')"
      end baseline ;;
    restart)
      begin restart
      stack up lite 2>&1 | tail -1; ready && echo ready
      check "before" "readyz $(code "$URL/readyz") models-nokey $(code "$URL/v1/models") app $(code -H "Authorization: Bearer $KEY" "$URL/app") settings $(code -H "Authorization: Bearer $KEY" "$URL/api/settings") apply $(code -X POST -H "Authorization: Bearer $KEY" "$URL/models/apply")"
      docker restart "$C" >/dev/null 2>&1; ready && echo "ready after restart"
      check "after" "readyz $(code "$URL/readyz") models-nokey $(code "$URL/v1/models") app $(code -H "Authorization: Bearer $KEY" "$URL/app") settings $(code -H "Authorization: Bearer $KEY" "$URL/api/settings") apply $(code -X POST -H "Authorization: Bearer $KEY" "$URL/models/apply")"
      check "runtime-settings-file-on-disk" "$(docker exec "$C" sh -c 'ls -la /configuration /data 2>&1 | head -20' | tr '\n' '|')"
      stack down lite >/dev/null 2>&1; end restart ;;
    internal-network)
      begin internal-network
      ovr="$HERE/experiments/internal-network.yaml"
      stack down lite "$ovr" >/dev/null 2>&1; stack up lite "$ovr" 2>&1 | tail -2
      ok=0; for _ in $(seq 1 100); do docker exec "$C" curl -sf -m 3 -o /dev/null http://127.0.0.1:8080/readyz && { ok=1; break; }; sleep 3; done
      [ "$ok" = 1 ] && echo "ready (checked from inside the container: no port is published)" || echo "NOT READY"
      img="$(docker inspect "$C" --format '{{.Config.Image}}')"
      check "published-ports" "$(docker port "$C" 2>&1 | tr '\n' ' ') (empty = none)"
      check "host-reach" "GET $URL/readyz from the host -> $(curl -s -m 5 -o /dev/null -w '%{http_code}' "$URL/readyz" || true) (000 = unreachable)"
      check "peer-container-with-key" "$(docker run --rm --network alixai-localai_default --entrypoint curl "$img" -s -m 20 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $KEY" http://alixai-localai:8080/v1/models 2>&1 | tail -1)"
      check "peer-container-no-key" "$(docker run --rm --network alixai-localai_default --entrypoint curl "$img" -s -m 20 -o /dev/null -w '%{http_code}' http://alixai-localai:8080/v1/models 2>&1 | tail -1)"
      check "peer-container-chat" "$(docker run --rm --network alixai-localai_default --entrypoint curl "$img" -s -m 120 -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"model":"alixai-smoke","messages":[{"role":"user","content":"Say ready."}],"max_tokens":8,"temperature":0}' http://alixai-localai:8080/v1/chat/completions 2>&1 | head -c 300)"
      check "outbound-from-localai-container" "$(docker exec "$C" curl -sS -m 8 -o /dev/null -w 'http %{http_code}' http://example.com 2>&1 | tr '
' ' ')"
      check "outbound-from-peer-container" "$(docker run --rm --network alixai-localai_default --entrypoint curl "$img" -sS -m 8 -o /dev/null -w 'http %{http_code}' http://example.com 2>&1 | tr '
' ' ')"
      check "dns-from-peer" "$(docker run --rm --network alixai-localai_default --entrypoint getent "$img" hosts huggingface.co 2>&1 | head -1) (empty = no resolution)"
      docker logs --tail 40 "$C" 2>&1 | cut -c1-300
      stack down lite "$ovr" >/dev/null 2>&1; end internal-network ;;
    no-caps) run_with no-caps no-caps.yaml ;;
    only-dac-override) run_with only-dac-override only-dac-override.yaml ;;
    nonroot) run_with nonroot nonroot.yaml ;;
    readonly-rootfs)
      begin readonly-rootfs
      ovr="$HERE/experiments/readonly-rootfs.yaml"
      stack down lite "$ovr" >/dev/null 2>&1; stack up lite "$ovr" 2>&1 | tail -2; ready && echo ready
      common_probes
      check "rootfs-and-limits" "$(docker inspect "$C" --format 'readonly-rootfs={{.HostConfig.ReadonlyRootfs}} pids-limit={{.HostConfig.PidsLimit}} log={{.HostConfig.LogConfig.Type}} {{.HostConfig.LogConfig.Config}}')"
      check "writes-outside-mounts" "$(docker exec "$C" sh -c 'touch /usr/x 2>&1; touch /tmp/x 2>&1 && echo tmp-ok' | tr '\n' '|')"
      check "files-written-outside-mounts-and-tmp" "$(docker diff "$C" 2>/dev/null | grep -v -E ' /(tmp|run/localai)' | head -10 | tr '\n' '|') (docker diff on a read-only rootfs: expected empty)"
      docker logs --tail 30 "$C" 2>&1 | cut -c1-300
      stack down lite "$ovr" >/dev/null 2>&1; end readonly-rootfs ;;
    strict-integrity)
      mkdir -p "$DEPLOY/data/backends-empty"
      rm -rf "$DEPLOY/data/backends-empty"/* 2>/dev/null
      begin strict-integrity
      ovr="$HERE/experiments/strict-integrity.yaml"
      stack down lite "$ovr" >/dev/null 2>&1; stack up lite "$ovr" 2>&1 | tail -2; ready && echo ready || echo "NOT READY"
      check "strict-env" "$(docker exec "$C" printenv LOCALAI_REQUIRE_BACKEND_INTEGRITY)"
      check "backends-dir" "$(docker exec "$C" ls -la /backends 2>&1 | tr '\n' '|')"
      check "chat" "$(chat | head -c 400)"
      echo "-- install/integrity lines"; docker logs "$C" 2>&1 | grep -iE 'backend|integrity|signature|verif|sigstore|cosign' | cut -c1-400 | head -20
      stack down lite "$ovr" >/dev/null 2>&1; end strict-integrity ;;
    wrong-sha)
      m="$DEPLOY/data/models-wrongsha"; mkdir -p "$m"; cp -n "$DEPLOY/data/models/granite-4.0-350m-Q4_K_M.gguf" "$m/"
      begin wrong-sha
      ovr="$HERE/experiments/wrong-sha.yaml"
      stack down lite "$ovr" >/dev/null 2>&1; stack up lite "$ovr" 2>&1 | tail -2; ready && echo ready || echo "NOT READY"
      check "chat" "$(chat | head -c 300)"
      check "file-before-and-after" "size $(docker exec "$C" stat -c %s /models/granite-4.0-350m-Q4_K_M.gguf 2>&1) sha256 $(docker exec "$C" sha256sum /models/granite-4.0-350m-Q4_K_M.gguf 2>&1 | cut -c1-64)"
      echo "-- preload lines"; docker logs "$C" 2>&1 | grep -iE 'sha|hash|checksum|download|preload|mismatch' | cut -c1-400 | head -20
      stack down lite "$ovr" >/dev/null 2>&1; end wrong-sha ;;
    no-caps-install)
      rm -rf "$DEPLOY/data/backends-nocaps"; mkdir -p "$DEPLOY/data/backends-nocaps"
      begin no-caps-install
      ovr="$HERE/experiments/no-caps-install.yaml"
      stack down lite "$ovr" >/dev/null 2>&1; stack up lite "$ovr" 2>&1 | tail -2; ready && echo ready || echo "NOT READY"
      check "capabilities" "$(caps)"
      check "backends-dir" "$(docker exec "$C" ls -la /backends 2>&1 | tr '
' '|')"
      check "backend-owner-of-unpacked-files" "$(docker exec "$C" sh -c 'ls -ln /backends/cpu-llama-cpp 2>&1 | head -6' | tr '
' '|')"
      check "chat" "$(chat | head -c 300)"
      echo "-- install lines"; docker logs "$C" 2>&1 | grep -iE 'backend|install|pull|unpack|extract|chown|operation not permitted' | cut -c1-1200 | head -20
      stack down lite "$ovr" >/dev/null 2>&1; end no-caps-install ;;
    readonly-models) run_with readonly-models readonly-models.yaml ;;
    trace-log) log_level_experiment trace-log trace-log.yaml CANARY-TRACE-44ee CANARY-TRACE-ECHO-5d1f ;;
    debug-log) log_level_experiment debug-log debug-log.yaml CANARY-DEBUG-91bc CANARY-DEBUG-ECHO-0a7e ;;
    info-log) log_level_experiment info-log info-log.yaml CANARY-INFO-2c9d CANARY-INFO-ECHO-8b3a ;;
    *) echo "unknown experiment $exp" >&3 2>&1 || echo "unknown experiment $exp" ;;
  esac
done
