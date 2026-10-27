variable "name" {
  type    = string
  default = "alixai-private-llm"
}

variable "ssh_cidrs" {
  type        = list(string)
  description = "CIDRs allowed to SSH."
  default     = ["0.0.0.0/0", "::/0"]
}

variable "client_cidrs" {
  type        = list(string)
  description = "CIDRs allowed to the TLS proxy (AlixAI hosts)."
  default     = ["0.0.0.0/0", "::/0"]
}

variable "tls_proxy_port" {
  type    = number
  default = 443
}

variable "create_network" {
  type        = bool
  description = "Create a Hetzner Cloud private network (optional; GEX Robot servers may use vSwitch instead)."
  default     = false
}

variable "network_cidr" {
  type    = string
  default = "10.90.0.0/16"
}

variable "subnet_cidr" {
  type    = string
  default = "10.90.1.0/24"
}

variable "network_zone" {
  type    = string
  default = "eu-central"
}

variable "model_id" {
  type        = string
  description = "Documented for operators; install script consumes the same id."
  default     = "gpt-oss-20b"
}

variable "gpu_type" {
  type        = string
  description = "Robot product hint: GEX45 | GEX63 | GEX131 (not created by Terraform)."
  default     = "GEX131"
}
