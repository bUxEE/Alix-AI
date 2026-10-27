# Private-LLM deployment kit

Creates a **dedicated** OpenAI-compatible GPU endpoint for one user account or one
tenant (the host turns the paid feature on). A **shared** many-user server uses the
same Performance compose with per-tenant `cache_salt` (or one process per tenant).

No cloud credentials belong in this repository. Secrets are injected at apply /
install time (`bearer_secret`, `.env`, provider secret stores).

## Layout

| Path | Role |
| --- | --- |
| [`cloud-init.yaml`](cloud-init.yaml) | Provider-neutral: NVIDIA driver/toolkit, pull pinned vLLM image, start service, health + readiness hook |
| [`terraform/scaleway`](terraform/scaleway) | Scaleway GPU instances: **L4 / L40S / H100** (hourly), firewall, outputs `base_url` / `health_url` |
| [`terraform/hetzner`](terraform/hetzner) | Firewall (+ optional Cloud network). **GEX dedicated servers are ordered in Robot**, not Terraform |
| [`scripts/hetzner-install.sh`](scripts/hetzner-install.sh) | Post-installimage bootstrap on a GEX host |
| [`scripts/hetzner-robot-notes.md`](scripts/hetzner-robot-notes.md) | Robot / installimage / firewall checklist |

Runtime compose: [`../container/performance`](../container/performance). Models:
`gpt-oss-20b`, `qwen3-30b-a3b`, `gpt-oss-120b` in `models/registry/`.

## Variables (both providers)

| Variable | Meaning |
| --- | --- |
| `model_id` / `ALIXAI_MODEL_ID` | Registry id |
| `gpu_type` | Scaleway: `L4` / `L40S` / `H100`; Hetzner: `GEX45` / `GEX63` / `GEX131` (documentation + install target) |
| `region` / `zone` | Scaleway region/zone (L40S/H100 often `fr-par-2`) |
| `bearer_secret` / `VLLM_API_KEY` | API bearer; sensitive; never commit |

Outputs: **base URL** (HTTPS after your proxy), **health URL** (`http://127.0.0.1:8000/health`).

## Activate sequence (host)

1. **Create** — Scaleway: `terraform apply` with `bearer_secret` and `model_id`. Hetzner GEX: order in Robot → installimage → `hetzner-install.sh`. Place model shards (pins in `deploy/container/performance/config/pins.txt`).
2. **Wait healthy** — `curl http://127.0.0.1:8000/health` (SSH tunnel) or the readiness webhook from cloud-init.
3. **TLS** — terminate HTTPS on 443 to loopback 8000; do not publish 8000.
4. **Register** the HTTPS OpenAI-compatible base URL in AlixAI (sibling operator UI / API; not in this kit).
5. **Pin / bind** the endpoint to the paying account or tenant.

## Deactivate sequence (host)

1. **Drain** — stop accepting new sessions for that account/tenant; wait in-flight turns to finish.
2. **Unregister** the endpoint in AlixAI.
3. **Destroy** — Scaleway: `terraform destroy`. Hetzner: stop compose, wipe model disks if required by contract, cancel the Robot server.

## Shared vs dedicated

| Shape | Isolation | Prefix cache |
| --- | --- | --- |
| Dedicated private LLM (this kit) | One tenant owns the GPU process | Prefix caching on; `cache_salt` optional |
| Shared many users | One process, many tenants | **Required:** per-tenant `cache_salt` on every request ([hardening](../container/performance/hardening.md)), or one vLLM per tenant |

## Related

- [Hardware and pricing](../../docs/operations/hardware-and-pricing.md)
- [Performance compose](../container/performance/README.md)
- [llmbench](../../tools/llmbench/README.md)
