# Performance profile: hardening and prefix-cache isolation

Status: written 2026-10-27. Companion to [README.md](README.md). Verified against vLLM documentation
([Security — Multi-Tenant Deployments](https://docs.vllm.ai/en/latest/usage/security/), read 2026-10-27)
and PR [#17045](https://github.com/vllm-project/vllm/pull/17045) (`cache_salt`).

## Network and auth

- Publish only on `127.0.0.1` (compose `ports`). Beyond loopback needs a TLS terminator in front (same rule as LocalAI / CLM).
- Every OpenAI-compatible route except `/health` requires the bearer key (`VLLM_API_KEY` / `--api-key`).
- Telemetry off (`VLLM_NO_USAGE_STATS`, `DO_NOT_TRACK`, hub offline). Models are bind-mounted; the container does not pull from the hub at run time (`HF_HUB_OFFLINE=1`).

## Prefix caching and tenants

vLLM enables automatic prefix caching by default. Shared KV blocks across requests are a **timing side channel**
(CVE-2025-46570): a neighbour can infer whether a prompt prefix is cached. Options:

| Mode | How | Cross-request prefix sharing |
| --- | --- | --- |
| Single-tenant dedicated server | omit `cache_salt`; keep `--enable-prefix-caching` | Allowed (one account / one tenant owns the box) |
| Shared process, per-tenant isolation | send `cache_salt` on every chat/completions request | Only requests with the **same** salt share blocks |
| Hard disable | `--no-enable-prefix-caching` | None (highest isolation, lowest reuse) |

`cache_salt` is mixed into the hash of the first KV block. Treat it as a secret: long random value per tenant
(e.g. 256 bits), not a predictable tenant id. vLLM docs state that `cache_salt` **reduces** cross-tenant risk but
is **not** a full isolation boundary — for hard isolation run a dedicated vLLM per tenant (the private-LLM shape
in [`deploy/private-llm`](../../private-llm/README.md)).

Example (OpenAI-compatible body):

```json
{
  "model": "gpt-oss-20b",
  "messages": [{"role": "user", "content": "…"}],
  "cache_salt": "<per-tenant-secret>"
}
```

AlixAI's gateway (sibling change) should inject the salt from tenant scope; this profile only documents the vLLM feature.

## Caps

| Flag / env | Default here | Role |
| --- | --- | --- |
| `--max-model-len` / `VLLM_MAX_MODEL_LEN` | 32768 | Context sized below native until measured |
| `--max-num-seqs` / `VLLM_MAX_NUM_SEQS` | 64 | Concurrent sequences |
| `--gpu-memory-utilization` / `VLLM_GPU_MEMORY_UTILIZATION` | 0.90 | Fraction of GPU memory for weights + KV |

Per-model native maxima (registry): gpt-oss-20b / gpt-oss-120b 131072; qwen3-30b-a3b 262144. Raising
`VLLM_MAX_MODEL_LEN` toward those values needs a measured KV budget on the target GPU.

## LocalAI CUDA alternative

`compose.localai-cuda.yaml` keeps the same loopback + API-key posture as
[`../localai/hardening.md`](../localai/hardening.md). Prefix-cache salting is a vLLM feature; LocalAI does not
expose `cache_salt`. Prefer dedicated LocalAI per tenant for shared deployments, or use the vLLM compose.
