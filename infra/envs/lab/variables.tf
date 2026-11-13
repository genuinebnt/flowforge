# Inputs of the lab environment (docs/GAPS.md "Deployment profiles", G-OPS-19).
# Defaults are the near-zero-cost learning setup; infra/COSTS.md lists what
# each choice costs.

variable "project_id" {
  description = "GCP project of the lab environment."
  type        = string
}

variable "region" {
  description = "Region. The Compute Engine e2-micro and Cloud Storage free tiers apply only in us-west1, us-central1 and us-east1."
  type        = string
  default     = "us-central1"

  validation {
    condition     = contains(["us-west1", "us-central1", "us-east1"], var.region)
    error_message = "The lab runs in a free-tier region: us-west1, us-central1 or us-east1."
  }
}

variable "zone" {
  description = "Zone of the VM (null: <region>-a, or us-east1-b, which has no zone a)."
  type        = string
  default     = null
}

variable "env" {
  description = "Environment name (labels, resource names). Always lab for this root."
  type        = string
  default     = "lab"

  validation {
    condition     = var.env == "lab"
    error_message = "This root is the lab environment; use infra/envs/dev or prod for the others."
  }
}

variable "ff_env" {
  description = "FF_ENV of the services. The Go config accepts dev or prod; the lab runs with production semantics (required JWKS, egress proxy, no ephemeral keys)."
  type        = string
  default     = "prod"

  validation {
    condition     = contains(["dev", "prod"], var.ff_env)
    error_message = "ff_env must be dev or prod."
  }
}

variable "allow_billable" {
  description = <<-EOT
    Explicit opt-ins that let the lab leave the free tier. Terraform
    preconditions and infra/policy (conftest, reading this variable from the
    plan JSON) refuse the following unless listed:
      "min_instances"    Cloud Run min instances > 0
      "max_instances"    Cloud Run max instances > 2
      "cpu_always"       CPU always allocated (instance-based billing)
      "vm_size"          a machine type other than e2-micro, or a second VM
      "vm_region"        a VM outside us-west1, us-central1, us-east1 (policy)
      "disk_size"        VM disks over 30 GB in total, or not pd-standard
      "secret_versions"  more than 6 Secret Manager secrets (one version each)
      "scheduler_jobs"   more than 3 Cloud Scheduler jobs (policy)
      and resource types the lab never creates by default:
      google_compute_router_nat, google_compute_global_forwarding_rule,
      google_compute_forwarding_rule, google_compute_security_policy,
      google_redis_instance, google_sql_database_instance,
      google_container_cluster, google_vpc_access_connector,
      google_compute_address / google_compute_global_address (external),
      google_redis_cluster, google_alloydb_instance, and "containerscanning"
      (the API, billed per scanned image).
  EOT
  type        = list(string)
  default     = []
}

variable "deletion_protection" {
  description = "Protect stateful resources from destroy (off in the lab so make gcp-lab-down works)."
  type        = bool
  default     = false
}

variable "images" {
  description = "Service => image by digest (REGION-docker.pkg.dev/PROJECT/flowforge/<service>@sha256:…). Keys: every Cloud Run service, egress-proxy (runs on the VM) and migrate."
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
  description = "Per Cloud Run service scaling. min must be 0 and max at most 2 unless allow_billable says otherwise; cpu_always (instance-based billing) defaults to false."
  type = map(object({
    concurrency = number
    min         = optional(number, 0)
    max         = number
    cpu         = optional(string, "1")
    memory      = optional(string, "512Mi")
    cpu_always  = optional(bool, false)
  }))

  validation {
    condition     = length(setsubtract(["gateway", "engine", "scope", "graph", "notifier", "similarity", "tool-runner", "content"], keys(var.scaling))) == 0
    error_message = "scaling must contain gateway, engine, scope, graph, notifier, similarity, tool-runner and content (the egress proxy runs on the VM)."
  }

  validation {
    condition     = try(var.scaling["content"].max, 0) == 1
    error_message = "content runs at most one instance: chunkstore allows one writer per tenant namespace (docs/GAPS.md G-CST-03)."
  }
}

variable "content_kek_id" {
  description = "FF_CONTENT_KEK_ID: the name of the content key-encryption key in ff-content-kek (wrapped data keys record it)."
  type        = string
  default     = "kek-1"
}

# --- network
variable "services_cidr" {
  description = "Direct VPC egress subnet of the regular services (the only source allowed to reach Postgres and Redis on the VM)."
  type        = string
  default     = "10.10.0.0/24"
}

variable "tools_cidr" {
  description = "Direct VPC egress subnet of the tool-runner (default-deny egress except the proxy)."
  type        = string
  default     = "10.10.4.0/26"
}

variable "egress_cidr" {
  description = "Subnet of the VM (egress proxy, Postgres, Redis)."
  type        = string
  default     = "10.10.5.0/28"
}

variable "private_dns_enabled" {
  description = "Private DNS zones googleapis.com and run.app -> private.googleapis.com. Needed for internal-ingress service calls and the tool-runner's default-deny egress; Cloud DNS zones have no free tier (see infra/COSTS.md)."
  type        = bool
  default     = true
}

# --- VM (Postgres, optional Redis, egress proxy)
variable "vm_machine_type" {
  description = "VM machine type; e2-micro is the free-tier type."
  type        = string
  default     = "e2-micro"
}

variable "vm_boot_disk_gb" {
  description = "Boot disk (pd-standard). Container-Optimized OS needs 10 GB."
  type        = number
  default     = 10
}

variable "vm_data_disk_gb" {
  description = "Data disk (pd-standard) for Postgres and Redis; boot + data stay within the 30 GB free tier."
  type        = number
  default     = 20
}

variable "vm_disk_type" {
  description = "Disk type of both disks; pd-standard is the free-tier type."
  type        = string
  default     = "pd-standard"
}

variable "vm_image_family" {
  description = "Container-Optimized OS image family (project cos-cloud)."
  type        = string
  default     = "cos-stable"
}

variable "vm_postgres_image" {
  description = "Postgres 16 image (Debian variant: postgres runs as uid 999; contrib provides pg_trgm and citext). Docker Hub through Google's mirror; pin by digest if you like."
  type        = string
  default     = "mirror.gcr.io/library/postgres:16"
}

variable "vm_redis_image" {
  description = "Redis 7 image, used when redis_enabled."
  type        = string
  default     = "mirror.gcr.io/library/redis:7-alpine"
}

variable "vm_logging_enabled" {
  description = "Ship container logs to Cloud Logging (COS logging agent; free within the Logging allotment, costs some of the VM's 1 GB of memory)."
  type        = bool
  default     = true
}

variable "db_max_connections" {
  description = "Postgres max_connections on the VM (each Cloud Run instance opens a pgx pool of up to 4)."
  type        = number
  default     = 80
}

variable "redis_enabled" {
  description = "Run Redis on the VM (distributed rate limits, revocations). Off: the services use their in-process limiter."
  type        = bool
  default     = false
}

variable "proxy_port" {
  description = "HTTP/CONNECT port of the egress proxy on the VM (FF_EGRESS_PROXY)."
  type        = number
  default     = 3128
}

variable "socks_port" {
  description = "SOCKS5 port of the egress proxy on the VM (tool-runner subnet only). Above 1024: the container runs unprivileged."
  type        = number
  default     = 1080
}

variable "dns_port" {
  description = "DNS port (UDP and TCP) of the egress proxy on the VM (tool-runner subnet only). Not 53: the container has no CAP_NET_BIND_SERVICE; tickets advertise the port."
  type        = number
  default     = 8053
}

# --- storage
variable "raw_retention_days" {
  description = "Delete raw tool output (raw/) after this many days."
  type        = number
  default     = 30
}

variable "archive_retention_days" {
  description = "Delete archived run events (engine/archive/events/) after this many days."
  type        = number
  default     = 90
}

variable "artifact_keep_count" {
  description = "Artifact Registry cleanup: keep this many most recent versions per image (the 0.5 GB free tier fits about two of each)."
  type        = number
  default     = 2
}

# --- events
variable "topic_prefix" {
  description = "FF_TOPIC_PREFIX: prefix of topic and subscription names."
  type        = string
  default     = "ff."
}

# --- tokens (spec 13.2, 17.5)
variable "jwt_issuer" {
  description = "FF_JWT_ISSUER; null = the gateway's run.app URL."
  type        = string
  default     = null
}

variable "jwt_audience_public" {
  description = "FF_JWT_AUDIENCE of the gateway (access tokens)."
  type        = string
  default     = "flowforge-api"
}

variable "jwt_audience_internal" {
  description = "FF_JWT_AUDIENCE of internal services (internal JWT)."
  type        = string
  default     = "flowforge-internal"
}

variable "signing_key_kids" {
  description = "Key ids with a secret ff-signing-key-<kid>, mounted as FF_SIGNING_KEY_<KID> (upper case, \"-\" -> \"_\"). One is enough in the lab (each id is one secret version)."
  type        = list(string)

  validation {
    condition     = length(var.signing_key_kids) > 0 && alltrue([for k in var.signing_key_kids : can(regex("^[a-z0-9][a-z0-9-]{0,40}$", k))])
    error_message = "Key ids must be lowercase letters, digits and dashes (they become secret ids and env var names)."
  }
}

variable "current_signing_kid" {
  description = "Key id the services sign internal tokens with (FF_SIGNING_KID)."
  type        = string

  validation {
    condition     = contains(var.signing_key_kids, var.current_signing_kid)
    error_message = "current_signing_kid must be one of signing_key_kids."
  }
}

# --- engine
variable "llm_endpoint" {
  description = "FF_LLM_ENDPOINT for the planner (reached through the egress proxy)."
  type        = string
  default     = ""
}

variable "llm_model" {
  description = "FF_LLM_MODEL for the planner."
  type        = string
  default     = ""
}

variable "llm_api_key_enabled" {
  description = "Create ff-llm-api-key and mount it on the engine (one more secret)."
  type        = bool
  default     = false
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

variable "webhook_channels" {
  description = "Channels with a secret ff-webhook-<channel> on the notifier (each one more secret)."
  type        = list(string)
  default     = []
}

variable "tool_secrets" {
  description = "Per-tool secrets ff-tool-<name> on the tool-runner (each one more secret)."
  type        = list(string)
  default     = []
}

# --- scheduler
variable "schedule_tick_cron" {
  description = "Cron of the single Cloud Scheduler job (POST /internal/schedules:tick on the engine). Cloud Scheduler's free tier is 3 jobs per billing account."
  type        = string
  default     = "* * * * *"
}

# --- CI identity
variable "github_repository" {
  description = "owner/repo allowed to impersonate sa-ci through Workload Identity Federation."
  type        = string
}

variable "github_environment" {
  description = "Only GitHub jobs bound to this environment may federate (the lab deploy runs on manual dispatch in environment lab)."
  type        = string
  default     = "lab"
}

# --- cost guardrails
variable "alert_emails" {
  description = "Budget notification recipients."
  type        = list(string)
}

variable "billing_account" {
  description = "Billing account id for the budget."
  type        = string
}

variable "monthly_budget" {
  description = "Monthly budget in budget_currency units (alerts at 50/80/100 % and on the forecast)."
  type        = number
  default     = 5
}

variable "budget_currency" {
  description = "Budget currency (the billing account's currency)."
  type        = string
  default     = "USD"
}

variable "budget_shutdown_enabled" {
  description = "Budget-triggered shutdown: when spend reaches budget_shutdown_threshold x monthly_budget, close the gateway, pause the tick and stop the VM. Off by default."
  type        = bool
  default     = false
}

variable "budget_shutdown_image" {
  description = "google-cloud-cli image pinned by digest for the shutdown handler (required when budget_shutdown_enabled)."
  type        = string
  default     = "gcr.io/google.com/cloudsdktool/google-cloud-cli@sha256:0000000000000000000000000000000000000000000000000000000000000000"
}

variable "budget_shutdown_threshold" {
  description = "Fraction of monthly_budget at which the shutdown runs."
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
