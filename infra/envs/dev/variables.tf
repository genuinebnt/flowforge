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

variable "zone" {
  description = "Zone of the egress proxy VM (null: <region>-a)."
  type        = string
  default     = null
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
  description = "Service => image reference by digest (REGION-docker.pkg.dev/PROJECT/flowforge/<service>@sha256:…). Keys: every Cloud Run service, egress-proxy (runs on the VM) and migrate."
  type        = map(string)

  validation {
    condition     = alltrue([for i in values(var.images) : can(regex("@sha256:[0-9a-f]{64}$", i))])
    error_message = "Every image must be pinned by digest (…@sha256:<64 hex>)."
  }

  validation {
    condition     = length(setsubtract(["gateway", "engine", "scope", "graph", "notifier", "similarity", "tool-runner", "content", "egress-proxy", "migrate"], keys(var.images))) == 0
    error_message = "images must contain gateway, engine, scope, graph, notifier, similarity, tool-runner, content, egress-proxy and migrate."
  }
}

variable "scaling" {
  description = "Per Cloud Run service scaling and size (spec 17.1 concurrency/min instances, 17.6 max instances). The egress proxy runs on a VM (egress_machine_type)."
  type = map(object({
    concurrency = number
    min         = number
    max         = number
    cpu         = optional(string, "1")
    memory      = optional(string, "512Mi")
  }))

  validation {
    condition     = sort(keys(var.scaling)) == sort(["gateway", "engine", "scope", "graph", "notifier", "similarity", "tool-runner", "content"])
    error_message = "scaling must contain exactly gateway, engine, scope, graph, notifier, similarity, tool-runner and content (the egress proxy runs on the VM)."
  }

  validation {
    condition     = try(var.scaling["content"].max, 0) == 1
    error_message = "content runs at most one instance: chunkstore allows one writer per tenant namespace (docs/GAPS.md G-CST-03)."
  }
}

variable "egress_machine_type" {
  description = "Machine type of the egress proxy VM (G-OPS-15)."
  type        = string
  default     = "e2-small"
}

variable "content_kek_id" {
  description = "FF_CONTENT_KEK_ID: the name of the content key-encryption key in ff-content-kek (wrapped data keys record it; change it when the secret's value is rotated)."
  type        = string
  default     = "kek-1"
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
  description = "Key ids with a Secret Manager secret ff-signing-key-<kid> (current and next, for rotation, spec 13.7), mounted as FF_SIGNING_KEY_<KID> with KID upper-cased and \"-\" replaced by \"_\"."
  type        = list(string)

  validation {
    condition     = length(var.signing_key_kids) > 0 && alltrue([for k in var.signing_key_kids : can(regex("^[a-z0-9][a-z0-9-]{0,40}$", k))])
    error_message = "Key ids must be lowercase letters, digits and dashes (they become secret ids and env var names)."
  }
}

variable "current_signing_kid" {
  description = "Key id the services sign internal tokens with (FF_SIGNING_KID); must be one of signing_key_kids."
  type        = string

  validation {
    condition     = contains(var.signing_key_kids, var.current_signing_kid)
    error_message = "current_signing_kid must be one of signing_key_kids."
  }
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
variable "schedule_tick_enabled" {
  description = "Create the Cloud Scheduler job that POSTs /internal/schedules:tick on the engine (G-OPS-10)."
  type        = bool
  default     = true
}

variable "schedule_tick_cron" {
  description = "Cron of the schedules tick."
  type        = string
  default     = "* * * * *"
}

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

variable "budget_shutdown_enabled" {
  description = "Create the budget-triggered shutdown (infra/modules/budget-shutdown). Off by default."
  type        = bool
  default     = false
}

variable "budget_shutdown_image" {
  description = "google-cloud-cli image pinned by digest for the shutdown handler (required when budget_shutdown_enabled)."
  type        = string
  default     = "gcr.io/google.com/cloudsdktool/google-cloud-cli@sha256:0000000000000000000000000000000000000000000000000000000000000000"
}

variable "budget_shutdown_threshold" {
  description = "Shut down when actual spend >= this fraction of monthly_budget."
  type        = number
  default     = 1.0
}

variable "drain_timeout" {
  description = "Engine graceful drain on SIGTERM (Go duration). Must stay below Cloud Run's 10 s termination grace."
  type        = string
  default     = "8s"
}

variable "notify_allowlist" {
  description = "HTTPS hosts the notifier may deliver webhooks and Slack messages to, through the egress proxy."
  type        = list(string)
  default     = ["hooks.slack.com"]
}
