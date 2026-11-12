# Inputs of an environment root. envs/dev/variables.tf and
# envs/prod/variables.tf are byte-identical; values live in terraform.tfvars.

variable "project_id" {
  description = "GCP project of this environment."
  type        = string
}

variable "region" {
  description = "Primary region."
  type        = string
  default     = "us-central1"
}

variable "env" {
  description = "Environment name exported as FF_ENV (spec 17.5)."
  type        = string

  validation {
    condition     = contains(["dev", "prod"], var.env)
    error_message = "env must be dev or prod."
  }
}

variable "domain" {
  description = "Public API host served by the load balancer, e.g. api.dev.flowforge.example."
  type        = string
}

variable "images" {
  description = "Service => image reference by digest (REGION-docker.pkg.dev/PROJECT/flowforge/<service>@sha256:…). Keys: every service plus migrate."
  type        = map(string)

  validation {
    condition     = alltrue([for i in values(var.images) : can(regex("@sha256:[0-9a-f]{64}$", i))])
    error_message = "Every image must be pinned by digest (…@sha256:<64 hex>)."
  }

  validation {
    condition     = length(setsubtract(["gateway", "engine", "scope", "graph", "notifier", "similarity", "tool-runner", "egress-proxy", "migrate"], keys(var.images))) == 0
    error_message = "images must contain gateway, engine, scope, graph, notifier, similarity, tool-runner, egress-proxy and migrate."
  }
}

variable "scaling" {
  description = "Per-service scaling and size (spec 17.1 concurrency/min instances, 17.6 max instances)."
  type = map(object({
    concurrency = number
    min         = number
    max         = number
    cpu         = optional(string, "1")
    memory      = optional(string, "512Mi")
  }))
}

variable "deletion_protection" {
  description = "Protect stateful resources and services from destroy."
  type        = bool
  default     = true
}

# --- network
variable "nat_ip_count" {
  description = "Static egress IPs for the egress proxy."
  type        = number
  default     = 2
}

# --- data stores
variable "db_tier" {
  description = "Cloud SQL tier."
  type        = string
  default     = "db-custom-1-3840"
}

variable "db_high_availability" {
  description = "Cloud SQL REGIONAL availability (prod)."
  type        = bool
  default     = false
}

variable "redis_tier" {
  description = "Memorystore tier (BASIC or STANDARD_HA)."
  type        = string
  default     = "BASIC"
}

variable "redis_memory_gb" {
  description = "Memorystore size."
  type        = number
  default     = 1
}

variable "artifact_kms_key" {
  description = "Optional CMEK key for the artifact bucket."
  type        = string
  default     = null
}

# --- events
variable "topic_prefix" {
  description = "FF_TOPIC_PREFIX: prefix of topic and subscription names."
  type        = string
  default     = "ff."
}

# --- tokens (spec 13.2, 17.5)
variable "jwt_issuer" {
  description = "FF_JWT_ISSUER, e.g. https://api.flowforge.example."
  type        = string
}

variable "jwt_audience_public" {
  description = "FF_JWT_AUDIENCE of the gateway (access tokens)."
  type        = string
  default     = "flowforge-api"
}

variable "jwt_audience_internal" {
  description = "FF_JWT_AUDIENCE of internal services (internal JWT minted by the gateway)."
  type        = string
  default     = "flowforge-internal"
}

variable "signing_key_kids" {
  description = "Key ids with a Secret Manager secret ff-signing-key-<kid> (current and next, for rotation, spec 13.7)."
  type        = list(string)
}

variable "current_signing_kid" {
  description = "Key id the gateway signs with (FF_SIGNING_KID)."
  type        = string
}

# --- engine
variable "llm_endpoint" {
  description = "FF_LLM_ENDPOINT for the planner."
  type        = string
  default     = ""
}

variable "llm_model" {
  description = "FF_LLM_MODEL for the planner."
  type        = string
  default     = ""
}

variable "llm_api_key_enabled" {
  description = "Create ff-llm-api-key and mount it on the engine (false for Vertex AI with IAM)."
  type        = bool
  default     = true
}

variable "fuel_default" {
  description = "FF_FUEL_DEFAULT (VM fuel per run)."
  type        = number
  default     = 1000000
}

variable "plan_fuel_default" {
  description = "FF_PLAN_FUEL_DEFAULT (planner fuel)."
  type        = number
  default     = 100000
}

# --- notifier and tool-runner secrets
variable "webhook_channels" {
  description = "Channels with a secret ff-webhook-<channel> mounted on the notifier."
  type        = list(string)
  default     = []
}

variable "tool_secrets" {
  description = "Per-tool secrets ff-tool-<name> mounted on the tool-runner (spec 17.2)."
  type        = list(string)
  default     = []
}

# --- scheduler
variable "schedules" {
  description = "Schedule id => { cron, time_zone }. Each fires POST /internal/schedules/<id>:fire on the engine with OIDC."
  type = map(object({
    cron      = string
    time_zone = optional(string, "Etc/UTC")
  }))
  default = {}
}

# --- edge
variable "edge_rate_limit_per_minute" {
  description = "Cloud Armor per-IP requests per minute."
  type        = number
  default     = 600
}

# --- CI identity
variable "github_repository" {
  description = "owner/repo allowed to impersonate sa-ci through Workload Identity Federation."
  type        = string
}

variable "github_environment" {
  description = "If set, only GitHub jobs bound to this protected environment may federate (prod)."
  type        = string
  default     = null
}

# --- monitoring and cost
variable "alert_emails" {
  description = "Alert and budget notification recipients."
  type        = list(string)
}

variable "billing_account" {
  description = "Billing account id for the budget."
  type        = string
}

variable "monthly_budget" {
  description = "Monthly budget amount."
  type        = number
}

variable "budget_currency" {
  description = "Budget currency."
  type        = string
  default     = "USD"
}
