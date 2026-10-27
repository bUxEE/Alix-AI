#!/usr/bin/env bash
# Guard: require a strong API key, then exec vLLM with that key (never logged).
set -euo pipefail

key="${VLLM_API_KEY:-}"
if [[ -z "$key" || "$key" == change-me* || ${#key} -lt 32 ]]; then
  echo "alixai-performance: set VLLM_API_KEY to openssl rand -hex 32 (or longer)" >&2
  exit 78
fi

model_id="${ALIXAI_MODEL_ID:-gpt-oss-20b}"
model_path="/models/${model_id}"
if [[ ! -d "$model_path" ]]; then
  echo "alixai-performance: model directory missing: ${model_path}" >&2
  exit 78
fi

max_len="${VLLM_MAX_MODEL_LEN:-32768}"
max_seqs="${VLLM_MAX_NUM_SEQS:-64}"
gpu_util="${VLLM_GPU_MEMORY_UTILIZATION:-0.90}"

exec python3 -m vllm.entrypoints.openai.api_server \
  --model "$model_path" \
  --served-model-name "$model_id" \
  --host 0.0.0.0 \
  --port 8000 \
  --api-key "$key" \
  --enable-prefix-caching \
  --max-model-len "$max_len" \
  --max-num-seqs "$max_seqs" \
  --gpu-memory-utilization "$gpu_util" \
  --disable-log-requests \
  "$@"
