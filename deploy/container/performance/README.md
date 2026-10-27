# Performance GPU profile (vLLM + LocalAI CUDA)

OpenAI-compatible inference on an NVIDIA GPU for the Performance / private-LLM models in
`models/registry/` (`gpt-oss-20b`, `qwen3-30b-a3b`, `gpt-oss-120b`). Primary runtime is **vLLM**;
**LocalAI CUDA** is the alternative compose file.

> **Untested on GPU here.** No accelerator was available when this was written. Digests and model hashes
> were resolved from Docker Hub and Hugging Face APIs on **2026-10-27**. Latency and capacity figures live
> in [hardware-and-pricing](../../../docs/operations/hardware-and-pricing.md) as estimates until
> `go run ./tools/llmbench` measures them.

## Pinned versions

| Component | Pin | Source (2026-10-27) |
| --- | --- | --- |
| vLLM OpenAI CUDA | `docker.io/vllm/vllm-openai:v0.11.2@sha256:2c908d5a84ed251b6a17d179f42d06df1aff353007779ac5eecd8a0ea3fe9331` (index; amd64 `47a9896f…6f680a`) | Docker Hub tag metadata |
| LocalAI CUDA 12 | `docker.io/localai/localai:v4.10.0-gpu-nvidia-cuda-12@sha256:9f999a8cdb4d5468a6ed6574cfeab08ab1efe49466f5d88a3cc146b372c48d65` | Docker Hub tag metadata |
| Models | revisions + LFS sha256 in [`config/pins.txt`](config/pins.txt) and `models/registry/*.yaml` | Hugging Face API |

## Files

| File | Purpose |
| --- | --- |
| `compose.yaml` | vLLM server, digest-pinned, `127.0.0.1` only, bearer key, prefix caching, health `/health` |
| `compose.localai-cuda.yaml` | LocalAI CUDA alternative, same loopback/key rules |
| `entrypoint.sh` | Refuses a weak key / missing model dir; execs vLLM with env caps |
| `config/pins.txt` | Every safetensors shard size + sha256 |
| `.env.example` | `VLLM_API_KEY`, model id, max context / seqs / GPU util |
| `hardening.md` | `cache_salt`, TLS/loopback, caps |

## Run (vLLM)

Needs Docker Compose v2 (or Podman), NVIDIA driver + [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/),
and the model directory populated under `./data/models/<ALIXAI_MODEL_ID>/` (all shards from `config/pins.txt`,
plus `config.json` / tokenizer files from the same revision).

```bash
cd deploy/container/performance
cp .env.example .env
# edit: VLLM_API_KEY=$(openssl rand -hex 32); ALIXAI_MODEL_ID=gpt-oss-20b
mkdir -p data/models
# place pinned files into data/models/gpt-oss-20b/ (verify sha256 against pins.txt)
docker compose --env-file .env -f compose.yaml config -q    # validate
docker compose --env-file .env -f compose.yaml up -d
curl -fsS http://127.0.0.1:8000/health
curl -H "Authorization: Bearer $VLLM_API_KEY" http://127.0.0.1:8000/v1/models
```

**Compose config validation:** run the `config -q` line on a host with Docker. If Docker is absent, treat the
files as **unvalidated** until that host runs them.

### Per-model context defaults

| `ALIXAI_MODEL_ID` | Native context (registry) | Suggested `VLLM_MAX_MODEL_LEN` until measured |
| --- | ---: | ---: |
| `gpt-oss-20b` | 131072 | 32768 |
| `qwen3-30b-a3b` | 262144 | 32768 |
| `gpt-oss-120b` | 131072 | 32768 |

`VLLM_MAX_NUM_SEQS` default 64; `VLLM_GPU_MEMORY_UTILIZATION` default 0.90. Tune after `tools/llmbench`.

## LocalAI CUDA alternative

```bash
docker compose --env-file .env -f compose.localai-cuda.yaml config -q
docker compose --env-file .env -f compose.localai-cuda.yaml up -d
```

Use LocalAI model YAMLs (operator-supplied) that point at the same pinned GGUF/safetensors the registry allows.
Hardening mirrors [`../localai`](../localai/README.md); see [hardening.md](hardening.md) for prefix-cache limits.

## Prefix cache / tenants

See [hardening.md](hardening.md). Dedicated private-LLM servers omit `cache_salt`. Shared servers must send a
per-tenant `cache_salt` on every request, or disable prefix caching, or run one process per tenant
([`deploy/private-llm`](../../private-llm/README.md)).

## Related

- [Hardware and pricing](../../../docs/operations/hardware-and-pricing.md)
- [Benchmark harness](../../../tools/llmbench/README.md): `go run ./tools/llmbench run -endpoint http://127.0.0.1:8000/v1 -key …`
