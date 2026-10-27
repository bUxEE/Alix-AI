# Scaleway GPU instance for a dedicated private-LLM server.
# No credentials in the repo: set SCW_ACCESS_KEY / SCW_SECRET_KEY / SCW_DEFAULT_PROJECT_ID
# (or a credentials file) in the operator environment.
#
# Instance types verified against Scaleway GPU pricing (read 2026-10-27):
#   L4-1-24G, L40S-1-48G, H100-1-80G (and multi-GPU variants).
# Prices change; re-check https://www.scaleway.com/en/pricing/gpu/

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    scaleway = {
      source  = "scaleway/scaleway"
      version = "~> 2.49"
    }
  }
}

provider "scaleway" {
  zone   = var.zone
  region = var.region
}

locals {
  # Map friendly GPU names to Scaleway commercial type names.
  gpu_type = lookup({
    L4   = "L4-1-24G"
    L40S = "L40S-1-48G"
    H100 = "H100-1-80G"
  }, var.gpu_type, var.gpu_type)
}

resource "scaleway_instance_security_group" "private_llm" {
  name                    = "${var.name}-sg"
  inbound_default_policy  = "drop"
  outbound_default_policy = "accept"
  # SSH from operator CIDR; LLM API stays on loopback — reach it via SSH tunnel or a TLS proxy you add.
  inbound_rule {
    action   = "accept"
    protocol = "TCP"
    port     = 22
    ip_range = var.ssh_cidr
  }
  inbound_rule {
    action   = "accept"
    protocol = "TCP"
    port     = var.tls_proxy_port
    ip_range = var.client_cidr
  }
}

resource "scaleway_instance_ip" "private_llm" {}

resource "scaleway_instance_server" "private_llm" {
  name              = var.name
  type              = local.gpu_type
  image             = var.image
  ip_id             = scaleway_instance_ip.private_llm.id
  security_group_id = scaleway_instance_security_group.private_llm.id

  # cloud-init: replace placeholders before apply (see variables / tfvars).
  user_data = {
    cloud-init = replace(replace(replace(replace(replace(
      file("${path.module}/../../cloud-init.yaml"),
      "##ALIXAI_VLLM_API_KEY##", var.bearer_secret),
      "##ALIXAI_MODEL_ID##", var.model_id),
      "##ALIXAI_MAX_MODEL_LEN##", tostring(var.max_model_len)),
      "##ALIXAI_MAX_NUM_SEQS##", tostring(var.max_num_seqs)),
      "##ALIXAI_READY_WEBHOOK##", var.ready_webhook)
  }

  tags = ["alixai", "private-llm", var.model_id]
}

output "base_url" {
  description = "Public HTTPS base URL the operator registers in AlixAI after the TLS proxy is up."
  value       = "https://${scaleway_instance_ip.private_llm.address}"
}

output "health_url" {
  description = "Loopback health on the instance (probe via SSH tunnel until a proxy is configured)."
  value       = "http://127.0.0.1:8000/health"
}

output "public_ip" {
  value = scaleway_instance_ip.private_llm.address
}

output "instance_id" {
  value = scaleway_instance_server.private_llm.id
}
