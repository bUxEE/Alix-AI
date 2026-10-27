# Hetzner Robot / installimage notes (GEX dedicated GPU)

Terraform does **not** create GEX servers. Order and OS install happen in
[Hetzner Robot](https://robot.hetzner.com/).

## Order

| Product (Oct 2026 public list) | GPU | Notes |
| --- | --- | --- |
| GEX45 | RTX 4000 Ada-class ~24 GB (successor to GEX44) | Entry dedicated GPU |
| GEX63 | RTX PRO 6000 96 GB | Mid tier |
| GEX131 (-1/-2/-3) | RTX PRO 6000 Blackwell Max-Q 96 GB | High tier; RAM/disk variants |

Prices: see [hardware-and-pricing](../../../docs/operations/hardware-and-pricing.md) (sourced; re-verify before purchase).

## installimage

1. Robot → server → **Linux** → installimage (or Rescue → `installimage`).
2. Choose **Ubuntu 24.04** (or current LTS), software RAID as Hetzner recommends for the SKU.
3. Install your SSH public key; disable password login.
4. Reboot into the installed system; confirm `nvidia-smi` (Hetzner GPU images usually ship a driver; if not, install `nvidia-driver-550` or the version Hetzner documents for that SKU).

## Firewall (Robot)

Mirror the intent of `terraform/hetzner`:

- Allow TCP 22 from the operator CIDR only.
- Allow TCP 443 from the AlixAI host / tenant CIDR only.
- Do **not** publish 8000 publicly; vLLM listens on `127.0.0.1:8000`.

## After delivery

```bash
# on the GEX host
export VLLM_API_KEY=$(openssl rand -hex 32)
export ALIXAI_MODEL_ID=gpt-oss-20b
# sync AlixAI tree, place model shards under deploy/container/performance/data/models/$ALIXAI_MODEL_ID
sudo -E ./deploy/private-llm/scripts/hetzner-install.sh
```

Then terminate TLS (Caddy/nginx) to `127.0.0.1:8000` and continue the activate sequence in [README.md](../README.md).
