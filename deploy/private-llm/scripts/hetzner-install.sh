#!/usr/bin/env bash
# Install AlixAI Performance vLLM on a Hetzner dedicated GEX server after
# Robot / installimage has delivered Ubuntu. Run as root on the server.
#
# Robot notes (not automated):
#   1. Order GEX45 / GEX63 / GEX131 in Hetzner Robot (not Cloud Console).
#   2. Activate installimage: choose Ubuntu 24.04, RAID per Hetzner defaults,
#      enable SSH key, set hostname.
#   3. Apply Robot firewall: allow 22 from operator CIDR, 443 from AlixAI CIDR;
#      deny 8000 from the public internet (vLLM stays on loopback).
#   4. Copy this repo's deploy/container/performance tree to /opt/alixai/…
#   5. Run this script with env: VLLM_API_KEY, ALIXAI_MODEL_ID, model files present.
set -euo pipefail

MODEL_ID="${ALIXAI_MODEL_ID:-gpt-oss-20b}"
PERF_DIR="${ALIXAI_PERF_DIR:-/opt/alixai/deploy/container/performance}"

if [[ -z "${VLLM_API_KEY:-}" || ${#VLLM_API_KEY} -lt 32 ]]; then
  echo "set VLLM_API_KEY to openssl rand -hex 32" >&2
  exit 1
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y curl ca-certificates gnupg jq

if ! command -v docker >/dev/null 2>&1; then
  curl -fsSL https://get.docker.com | sh
  systemctl enable --now docker
fi

if ! dpkg -l | grep -q nvidia-container-toolkit; then
  curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
    | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
  curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
    | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
    > /etc/apt/sources.list.d/nvidia-container-toolkit.list
  apt-get update
  apt-get install -y nvidia-container-toolkit
  nvidia-ctk runtime configure --runtime=docker
  systemctl restart docker
fi

if ! command -v nvidia-smi >/dev/null 2>&1; then
  echo "nvidia-smi missing: install the NVIDIA driver (Hetzner installimage often provides it; else apt nvidia-driver-550)" >&2
  exit 1
fi
nvidia-smi

if [[ ! -d "$PERF_DIR" ]]; then
  echo "missing $PERF_DIR — sync deploy/container/performance from the AlixAI release" >&2
  exit 1
fi

umask 077
cat >"$PERF_DIR/.env" <<EOF
VLLM_API_KEY=${VLLM_API_KEY}
ALIXAI_MODEL_ID=${MODEL_ID}
VLLM_MAX_MODEL_LEN=${VLLM_MAX_MODEL_LEN:-32768}
VLLM_MAX_NUM_SEQS=${VLLM_MAX_NUM_SEQS:-64}
VLLM_GPU_MEMORY_UTILIZATION=${VLLM_GPU_MEMORY_UTILIZATION:-0.90}
VLLM_PORT=8000
EOF

cd "$PERF_DIR"
docker compose --env-file .env -f compose.yaml pull
docker compose --env-file .env -f compose.yaml up -d

echo "Waiting for health…"
for i in $(seq 1 120); do
  if curl -fsS -m 3 http://127.0.0.1:8000/health >/dev/null 2>&1; then
    echo "healthy: http://127.0.0.1:8000/health"
    echo "Put a TLS reverse proxy on :443 → 127.0.0.1:8000, then register the base URL in AlixAI."
    exit 0
  fi
  sleep 30
done
echo "not healthy yet; check: docker compose -f $PERF_DIR/compose.yaml logs" >&2
exit 1
