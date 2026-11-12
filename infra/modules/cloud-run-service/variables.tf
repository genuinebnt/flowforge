variable "project_id" {
  description = "GCP project id."
  type        = string
}

variable "region" {
  description = "Cloud Run region."
  type        = string
}

variable "name" {
  description = "Logical Flowforge service name (gateway, engine, scope, ...)."
  type        = string
}

variable "service_name" {
  description = "Cloud Run service name, e.g. ff-engine."
  type        = string
}

variable "account_id" {
  description = "Runtime service account id, e.g. sa-engine (spec 17.2)."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.account_id))
    error_message = "account_id must be 6-30 characters of lowercase letters, digits and dashes."
  }
}

variable "image" {
  description = "Container image pinned by digest (REGISTRY/PATH@sha256:<64 hex>)."
  type        = string

  validation {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.image))
    error_message = "image must be referenced by digest (…@sha256:<64 hex>), never by tag."
  }
}

variable "ingress" {
  description = "INGRESS_TRAFFIC_INTERNAL_ONLY for internal services; INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER for the gateway behind the LB."
  type        = string
  default     = "INGRESS_TRAFFIC_INTERNAL_ONLY"

  validation {
    condition     = contains(["INGRESS_TRAFFIC_INTERNAL_ONLY", "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"], var.ingress)
    error_message = "Flowforge services are never exposed with INGRESS_TRAFFIC_ALL; put the gateway behind the load balancer."
  }
}

variable "public" {
  description = "Disable the Cloud Run invoker IAM check. Only the gateway (reached through the LB and Cloud Armor) sets this; infra/policy enforces it."
  type        = bool
  default     = false
}

variable "use_http2" {
  description = "End-to-end HTTP/2 (h2c) for gRPC services."
  type        = bool
  default     = true
}

variable "container_port" {
  description = "Port the container listens on (also exported as PORT by Cloud Run)."
  type        = number
  default     = 8080
}

variable "concurrency" {
  description = "Maximum concurrent requests per instance."
  type        = number
  default     = 80
}

variable "min_instances" {
  description = "Minimum instances."
  type        = number
  default     = 0
}

variable "max_instances" {
  description = "Maximum instances (cost guardrail, spec 17.6)."
  type        = number
  default     = 10

  validation {
    condition     = var.max_instances >= 1 && var.max_instances <= 100
    error_message = "max_instances must be set explicitly between 1 and 100."
  }
}

variable "cpu" {
  description = "CPU limit, e.g. \"1\"."
  type        = string
  default     = "1"
}

variable "memory" {
  description = "Memory limit, e.g. \"512Mi\"."
  type        = string
  default     = "512Mi"
}

variable "cpu_always_allocated" {
  description = "Keep CPU allocated outside requests (background loops: engine, notifier, pull consumers)."
  type        = bool
  default     = false
}

variable "execution_environment" {
  description = "EXECUTION_ENVIRONMENT_GEN1 or EXECUTION_ENVIRONMENT_GEN2."
  type        = string
  default     = "EXECUTION_ENVIRONMENT_GEN2"
}

variable "request_timeout" {
  description = "Request timeout (duration string)."
  type        = string
  default     = "300s"
}

variable "network_id" {
  description = "VPC for Direct VPC egress."
  type        = string
}

variable "subnetwork_id" {
  description = "Subnet for Direct VPC egress."
  type        = string
}

variable "network_tags" {
  description = "Network tags on the Direct VPC egress interface (firewall targeting)."
  type        = list(string)
  default     = []
}

variable "vpc_egress" {
  description = "ALL_TRAFFIC routes everything (including run.app and Google APIs) through the VPC."
  type        = string
  default     = "ALL_TRAFFIC"
}

variable "cloudsql_instances" {
  description = "Cloud SQL connection names mounted at /cloudsql via the built-in Cloud SQL connector."
  type        = list(string)
  default     = []
}

variable "project_roles" {
  description = "Predefined project-level roles for the runtime account (no primitive roles; infra/policy enforces)."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for r in var.project_roles : !contains(["roles/owner", "roles/editor", "roles/viewer"], r) && !strcontains(r, "*")])
    error_message = "Primitive and wildcard roles are not allowed."
  }
}

variable "env" {
  description = "Plain environment variables (spec 17.5)."
  type        = map(string)
  default     = {}
}

variable "secret_ids" {
  description = "Secrets this service may read: short name => secret resource id. Secret Accessor is granted on each."
  type        = map(string)
  default     = {}
}

variable "secret_env" {
  description = "Environment variables sourced from Secret Manager: env name => { secret = short name in secret_ids, version }."
  type = map(object({
    secret  = string
    version = optional(string, "latest")
  }))
  default = {}
}

variable "invoker_members" {
  description = "Callers granted roles/run.invoker on this service: caller name => IAM member."
  type        = map(string)
  default     = {}

  validation {
    condition     = alltrue([for m in values(var.invoker_members) : !contains(["allUsers", "allAuthenticatedUsers"], m)])
    error_message = "Use public = true (gateway only) instead of allUsers/allAuthenticatedUsers."
  }
}

variable "health_path" {
  description = "HTTP startup probe path (null = Cloud Run's default TCP probe, used for h2c gRPC services)."
  type        = string
  default     = null
}

variable "liveness_path" {
  description = "HTTP liveness probe path (null = none)."
  type        = string
  default     = null
}

variable "deletion_protection" {
  description = "Protect the service from terraform destroy."
  type        = bool
  default     = true
}

variable "labels" {
  description = "Extra labels."
  type        = map(string)
  default     = {}
}
