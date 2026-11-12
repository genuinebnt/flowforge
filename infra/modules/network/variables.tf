variable "project_id" {
  description = "GCP project id."
  type        = string
}

variable "region" {
  description = "Region for subnets, router and NAT."
  type        = string
}

variable "name" {
  description = "Name prefix, e.g. ff-dev."
  type        = string
}

variable "services_cidr" {
  description = "Subnet for Cloud Run Direct VPC egress of the regular services."
  type        = string
  default     = "10.10.0.0/22"
}

variable "tools_cidr" {
  description = "Subnet for the tool-runner (no NAT, default-deny egress)."
  type        = string
  default     = "10.10.4.0/24"
}

variable "egress_cidr" {
  description = "Subnet for the egress proxy; the only subnet Cloud NAT translates."
  type        = string
  default     = "10.10.5.0/24"
}

variable "psa_cidr" {
  description = "Private services access range for Cloud SQL and Memorystore."
  type        = string
  default     = "10.20.0.0/16"
}

variable "nat_ip_count" {
  description = "Number of reserved static NAT IPs for the egress proxy."
  type        = number
  default     = 2

  validation {
    condition     = var.nat_ip_count >= 1 && var.nat_ip_count <= 8
    error_message = "nat_ip_count must be between 1 and 8."
  }
}

variable "tool_runner_tag" {
  description = "Network tag carried by the tool-runner's Direct VPC egress interface."
  type        = string
  default     = "ff-tool-runner"
}

variable "egress_proxy_tag" {
  description = "Network tag carried by the egress proxy's Direct VPC egress interface."
  type        = string
  default     = "ff-egress-proxy"
}
