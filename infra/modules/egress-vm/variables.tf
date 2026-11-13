variable "project_id" {
  description = "GCP project id."
  type        = string
}

variable "region" {
  description = "Region of the internal address."
  type        = string
}

variable "zone" {
  description = "Zone of the VM."
  type        = string
}

variable "name" {
  description = "Name prefix, e.g. ff-dev. Creates <name>-egress (VM and address)."
  type        = string
}

variable "account_id" {
  description = "Service account id of the VM (6-30 characters)."
  type        = string
  default     = "sa-egress-proxy"
}

variable "network_id" {
  description = "VPC id."
  type        = string
}

variable "subnetwork_id" {
  description = "The egress subnet (the only subnet Cloud NAT translates)."
  type        = string
}

variable "network_tags" {
  description = "Extra network tags, e.g. the network module's egress_proxy_tag (deny-private egress rule)."
  type        = list(string)
  default     = []
}

variable "machine_type" {
  description = "Machine type. e2-small fits the proxy (Go, a few hundred MB); e2-micro is the free-tier type."
  type        = string
  default     = "e2-small"
}

variable "boot_disk_gb" {
  description = "Boot disk size (Container-Optimized OS needs 10 GB)."
  type        = number
  default     = 10
}

variable "boot_disk_type" {
  description = "Boot disk type."
  type        = string
  default     = "pd-balanced"
}

variable "image_family" {
  description = "Container-Optimized OS image family (project cos-cloud)."
  type        = string
  default     = "cos-stable"
}

variable "external_ip" {
  description = "Give the VM an ephemeral external IPv4 address (only without Cloud NAT; dev/prod leave it off and egress through NAT's static IPs)."
  type        = bool
  default     = false
}

variable "registry" {
  description = "Artifact Registry host the proxy image is pulled from, e.g. us-central1-docker.pkg.dev."
  type        = string
}

variable "repository" {
  description = "Artifact Registry repository id holding the image (roles/artifactregistry.reader is granted on it)."
  type        = string
}

variable "image" {
  description = "Initial egress proxy image by digest. The deploy workflow rolls it forward through the instance metadata key ff-egress-proxy-image, which Terraform then ignores."
  type        = string

  validation {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.image))
    error_message = "The image must be pinned by digest (…@sha256:<64 hex>)."
  }
}

variable "env" {
  description = "Non-secret environment of the proxy (FF_* variables). PORT and the FF_EGRESS_SOCKS_*/FF_EGRESS_DNS_* listen and advertise addresses are set by the module."
  type        = map(string)
  default     = {}
}

variable "secret_env" {
  description = "Environment variable => Secret Manager secret id (in project_id), read with the VM's service account at boot into a tmpfs."
  type        = map(string)
  default     = {}
}

variable "secret_ids" {
  description = "Secret id => full secret resource id, for the accessor grants (one entry per value of secret_env)."
  type        = map(string)
  default     = {}
}

variable "proxy_port" {
  description = "HTTP proxy port (plain requests and CONNECT to any port the decision covers): FF_EGRESS_PROXY."
  type        = number
  default     = 3128
}

variable "socks_port" {
  description = "SOCKS5 port (RFC 1928 CONNECT). Above 1024: the container runs unprivileged."
  type        = number
  default     = 1080
}

variable "dns_port" {
  description = "DNS port, UDP and TCP (the scope-checked forwarder). Not 53: the container runs unprivileged without CAP_NET_BIND_SERVICE; tickets advertise the port."
  type        = number
  default     = 8053
}

variable "tool_cidrs" {
  description = "Source ranges allowed on every proxy port (the tool-runner's Direct VPC egress subnet)."
  type        = list(string)
}

variable "http_client_cidrs" {
  description = "Source ranges allowed on the HTTP proxy port only (the services subnet: the notifier's notify path, G-SVC-08). Empty: tool-runner only."
  type        = list(string)
  default     = []
}

variable "tool_runner_tag" {
  description = "Network tag of the tool-runner; its default-deny egress gets an exception to the VM's ports."
  type        = string
}

variable "iap_ssh" {
  description = "Allow SSH from the Identity-Aware Proxy range (gcloud compute ssh --tunnel-through-iap)."
  type        = bool
  default     = true
}

variable "logging_enabled" {
  description = "Ship container logs to Cloud Logging (COS logging agent)."
  type        = bool
  default     = true
}

variable "deletion_protection" {
  description = "Protect the VM from destroy."
  type        = bool
  default     = true
}

variable "labels" {
  description = "Labels."
  type        = map(string)
  default     = {}
}
