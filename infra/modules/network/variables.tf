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

# --- Cost toggles. dev and prod keep the defaults (the full spec 17.3
# design); the lab environment turns the billed pieces off.

variable "nat_enabled" {
  description = "Create Cloud Router + Cloud NAT and the reserved NAT IPs for the egress subnet (billed per gateway hour and per IP). lab: false (the egress proxy runs on the VM, which has its own external address)."
  type        = bool
  default     = true
}

variable "psa_enabled" {
  description = "Create the private services access range and peering for Cloud SQL and Memorystore. lab: false (no Cloud SQL or Memorystore)."
  type        = bool
  default     = true
}

variable "private_dns_enabled" {
  description = "Create the private DNS zones that map googleapis.com and run.app to private.googleapis.com (needed for internal-ingress service-to-service calls and the tool-runner's default-deny egress). Cloud DNS managed zones are billed per zone per month."
  type        = bool
  default     = true
}

variable "flow_logs_enabled" {
  description = "VPC flow logs on every subnet (billed as log generation)."
  type        = bool
  default     = true
}

variable "firewall_logs_enabled" {
  description = "Firewall rules logging on the deny rules (billed as Cloud Logging ingestion beyond the free allotment)."
  type        = bool
  default     = true
}
