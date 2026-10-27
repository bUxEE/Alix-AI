# Hetzner Cloud networking / firewall for a private-LLM endpoint.
#
# Dedicated GEX GPU servers (GEX45, GEX63, GEX131, …) are ordered through
# Hetzner Robot / installimage — Terraform cannot create them. Use
# scripts/hetzner-install.sh after the server is delivered.
#
# This module manages a Cloud Firewall (and optional Cloud Network) you can
# attach to a Hetzner Cloud vSwitch / adjacent CXs, or document as the rule set
# to mirror on Robot firewall. No API tokens in the repo.

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.49"
    }
  }
}

provider "hcloud" {
  # HCLOUD_TOKEN from the environment.
}

resource "hcloud_firewall" "private_llm" {
  name = "${var.name}-fw"

  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "22"
    source_ips = var.ssh_cidrs
  }

  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = tostring(var.tls_proxy_port)
    source_ips = var.client_cidrs
  }

  # Explicit deny of the raw vLLM port from the public internet.
  # (Default drop on Robot; this documents intent for Cloud Firewall.)
}

resource "hcloud_network" "private_llm" {
  count    = var.create_network ? 1 : 0
  name     = "${var.name}-net"
  ip_range = var.network_cidr
}

resource "hcloud_network_subnet" "private_llm" {
  count        = var.create_network ? 1 : 0
  network_id   = hcloud_network.private_llm[0].id
  type         = "cloud"
  network_zone = var.network_zone
  ip_range     = var.subnet_cidr
}

output "firewall_id" {
  value = hcloud_firewall.private_llm.id
}

output "network_id" {
  value = var.create_network ? hcloud_network.private_llm[0].id : null
}

output "base_url_hint" {
  description = "After Robot delivery + install script + TLS proxy, register https://<server-ip> in AlixAI."
  value       = "https://<gex-server-ip>"
}

output "health_url" {
  value = "http://127.0.0.1:8000/health"
}
