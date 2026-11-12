variable "project_id" {
  description = "GCP project id."
  type        = string
}

variable "region" {
  description = "Region."
  type        = string
}

variable "name" {
  description = "Instance name, e.g. ff-dev-redis."
  type        = string
}

variable "tier" {
  description = "BASIC (dev) or STANDARD_HA (prod)."
  type        = string
  default     = "BASIC"

  validation {
    condition     = contains(["BASIC", "STANDARD_HA"], var.tier)
    error_message = "tier must be BASIC or STANDARD_HA."
  }
}

variable "memory_size_gb" {
  description = "Memory size in GiB."
  type        = number
  default     = 1
}

variable "redis_version" {
  description = "Redis version."
  type        = string
  default     = "REDIS_7_2"
}

variable "network_id" {
  description = "Authorized VPC."
  type        = string
}

variable "psa_range_name" {
  description = "Private services access range name."
  type        = string
}

variable "labels" {
  description = "Labels."
  type        = map(string)
  default     = {}
}
