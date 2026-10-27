variable "name" {
  type        = string
  description = "Instance name."
  default     = "alixai-private-llm"
}

variable "region" {
  type        = string
  description = "Scaleway region (e.g. fr-par, pl-waw)."
  default     = "fr-par"
}

variable "zone" {
  type        = string
  description = "Scaleway zone (e.g. fr-par-2 for L40S/H100)."
  default     = "fr-par-2"
}

variable "gpu_type" {
  type        = string
  description = "Friendly GPU: L4 | L40S | H100, or a full commercial type (L4-1-24G, …)."
  default     = "L40S"
}

variable "image" {
  type        = string
  description = "OS image label Scaleway accepts (e.g. ubuntu_noble)."
  default     = "ubuntu_noble"
}

variable "model_id" {
  type        = string
  description = "Registry model id: gpt-oss-20b | qwen3-30b-a3b | gpt-oss-120b."
  default     = "gpt-oss-20b"
}

variable "max_model_len" {
  type    = number
  default = 32768
}

variable "max_num_seqs" {
  type    = number
  default = 64
}

variable "bearer_secret" {
  type        = string
  description = "vLLM API bearer secret. Prefer a secret manager reference injected at apply time; never commit."
  sensitive   = true
}

variable "ready_webhook" {
  type        = string
  description = "Optional HTTPS URL notified when /health succeeds."
  default     = ""
}

variable "ssh_cidr" {
  type        = string
  description = "CIDR allowed to SSH."
  default     = "0.0.0.0/0"
}

variable "client_cidr" {
  type        = string
  description = "CIDR allowed to the TLS proxy port (AlixAI host)."
  default     = "0.0.0.0/0"
}

variable "tls_proxy_port" {
  type        = number
  description = "External TLS port you terminate in front of vLLM (not opened to vLLM directly)."
  default     = 443
}
