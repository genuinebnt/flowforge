# Flowforge environment root (spec 17). envs/dev/main.tf and
# envs/prod/main.tf are kept byte-identical (CI checks it); every difference
# between environments lives in terraform.tfvars.

terraform {
  required_version = ">= 1.6"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region

  # The Billing Budgets API needs a quota project.
  user_project_override = true
  billing_project       = var.project_id

  default_labels = {
    app = "flowforge"
    env = var.env
  }
}

data "google_project" "this" {
  project_id = var.project_id
}

locals {
  name           = "ff-${var.env}"
  project_number = data.google_project.this.number
  labels         = { app = "flowforge", env = var.env }

  service_names = ["gateway", "engine", "scope", "graph", "notifier", "similarity", "tool-runner", "egress-proxy"]

  # Cloud Run URLs are deterministic (https://<service>-<project number>.<region>.run.app),
  # which breaks the gateway <-> internal-service reference cycle (JWKS URL
  # and callee addresses).
  run_url = { for s in local.service_names : s => "https://ff-${s}-${local.project_number}.${var.region}.run.app" }

  # Databases users: one per service that owns a schema (spec 10), plus the
  # migrations owner. similarity reads/writes graph.embeddings (spec 22).
  db_users = ["gateway", "engine", "scope", "graph", "notifier", "similarity", "migrator"]

  signing_key_env = {
    for k in var.signing_key_kids : "FF_SIGNING_KEY_${upper(replace(k, "-", "_"))}" => { secret = "ff-signing-key-${k}" }
  }
  webhook_env = {
    for c in var.webhook_channels : "FF_WEBHOOK_${upper(replace(c, "-", "_"))}" => { secret = "ff-webhook-${c}" }
  }
  tool_secret_env = {
    for t in var.tool_secrets : "FF_TOOL_SECRET_${upper(replace(t, "-", "_"))}" => { secret = "ff-tool-${t}" }
  }
  llm_env = { for k, v in { FF_LLM_API_KEY = { secret = "ff-llm-api-key" } } : k => v if var.llm_api_key_enabled }

  secret_names = concat(
    [for k in var.signing_key_kids : "ff-signing-key-${k}"],
    var.llm_api_key_enabled ? ["ff-llm-api-key"] : [],
    [for c in var.webhook_channels : "ff-webhook-${c}"],
    [for t in var.tool_secrets : "ff-tool-${t}"],
    [for u in local.db_users : "ff-db-${u}-password"],
    ["ff-redis-auth", "ff-redis-ca"],
  )

  # --- spec 17.5 environment variables.
  env_common = {
    FF_ENV            = var.env
    FF_JWKS_URL       = "${local.run_url["gateway"]}/.well-known/jwks.json"
    FF_JWT_ISSUER     = var.jwt_issuer
    FF_JWT_AUDIENCE   = var.jwt_audience_internal
    FF_PUBSUB_PROJECT = var.project_id
    FF_TOPIC_PREFIX   = var.topic_prefix
  }
  env_redis = {
    FF_REDIS_ADDR = module.redis.address
  }
  secret_redis = {
    FF_REDIS_AUTH = { secret = "ff-redis-auth" }
    FF_REDIS_CA   = { secret = "ff-redis-ca" }
  }
  env_bucket = {
    FF_ARTIFACT_BUCKET = module.artifacts.name
  }
  env_db = {
    for u in local.db_users : u => {
      FF_DB_DSN = "host=/cloudsql/${module.sql.connection_name} dbname=${module.sql.database_name} user=${module.sql.user_names[u]} sslmode=disable"
    }
  }
  secret_db = { for u in local.db_users : u => { PGPASSWORD = { secret = "ff-db-${u}-password" } } }
  addr      = { for s in local.service_names : s => { "FF_${upper(replace(s, "-", "_"))}_ADDR" = local.run_url[s] } }

  # --- spec 17.1 service mapping and 17.2 roles. Resource-scoped grants
  # (topics, subscriptions, bucket prefixes, invokers) are declared below.
  services = {
    gateway = {
      ingress    = "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"
      public     = true
      http2      = false
      cpu_always = false
      subnet     = module.network.services_subnet_id
      sql        = true
      roles      = ["roles/cloudsql.client"]
      health     = "/readyz"
      liveness   = "/healthz"
      env = merge(local.env_common, local.env_db["gateway"], local.env_redis,
        local.addr["engine"], local.addr["scope"], local.addr["graph"], local.addr["notifier"], local.addr["similarity"],
        {
          FF_JWT_AUDIENCE      = var.jwt_audience_public
          FF_SIGNING_KID       = var.current_signing_kid
          FF_INTERNAL_AUDIENCE = var.jwt_audience_internal
      })
      secret_env = merge(local.secret_db["gateway"], local.secret_redis, local.signing_key_env)
    }
    engine = {
      ingress    = "INGRESS_TRAFFIC_INTERNAL_ONLY"
      public     = false
      http2      = true
      cpu_always = true
      subnet     = module.network.services_subnet_id
      sql        = true
      roles      = ["roles/cloudsql.client"]
      health     = null
      liveness   = null
      env = merge(local.env_common, local.env_db["engine"], local.env_redis, local.env_bucket,
        local.addr["scope"], local.addr["graph"], local.addr["tool-runner"],
        {
          FF_EGRESS_PROXY      = local.run_url["egress-proxy"]
          FF_LLM_ENDPOINT      = var.llm_endpoint
          FF_LLM_MODEL         = var.llm_model
          FF_FUEL_DEFAULT      = tostring(var.fuel_default)
          FF_PLAN_FUEL_DEFAULT = tostring(var.plan_fuel_default)
      })
      secret_env = merge(local.secret_db["engine"], local.secret_redis, local.llm_env)
    }
    scope = {
      ingress    = "INGRESS_TRAFFIC_INTERNAL_ONLY"
      public     = false
      http2      = true
      cpu_always = false
      subnet     = module.network.services_subnet_id
      sql        = true
      roles      = ["roles/cloudsql.client"]
      health     = null
      liveness   = null
      env        = merge(local.env_common, local.env_db["scope"], local.env_redis)
      secret_env = merge(local.secret_db["scope"], local.secret_redis)
    }
    graph = {
      ingress    = "INGRESS_TRAFFIC_INTERNAL_ONLY"
      public     = false
      http2      = true
      cpu_always = false
      subnet     = module.network.services_subnet_id
      sql        = true
      roles      = ["roles/cloudsql.client"]
      health     = null
      liveness   = null
      env        = merge(local.env_common, local.env_db["graph"], local.env_bucket)
      secret_env = local.secret_db["graph"]
    }
    notifier = {
      ingress    = "INGRESS_TRAFFIC_INTERNAL_ONLY"
      public     = false
      http2      = true
      cpu_always = true
      subnet     = module.network.services_subnet_id
      sql        = true
      roles      = ["roles/cloudsql.client"]
      health     = null
      liveness   = null
      env = merge(local.env_common, local.env_db["notifier"], {
        FF_EGRESS_PROXY = local.run_url["egress-proxy"]
      })
      secret_env = merge(local.secret_db["notifier"], local.webhook_env)
    }
    similarity = {
      ingress    = "INGRESS_TRAFFIC_INTERNAL_ONLY"
      public     = false
      http2      = true
      cpu_always = true # pull consumer of graph.changes
      subnet     = module.network.services_subnet_id
      sql        = true
      roles      = ["roles/cloudsql.client"]
      health     = null
      liveness   = null
      env        = merge(local.env_common, local.env_db["similarity"])
      secret_env = local.secret_db["similarity"]
    }
    tool-runner = {
      ingress    = "INGRESS_TRAFFIC_INTERNAL_ONLY"
      public     = false
      http2      = true
      cpu_always = false
      subnet     = module.network.tools_subnet_id
      sql        = false
      roles      = [] # no database access (spec 17.2)
      health     = null
      liveness   = null
      env = merge(local.env_common, local.env_redis, local.env_bucket, local.addr["scope"], {
        FF_EGRESS_PROXY = local.run_url["egress-proxy"]
      })
      secret_env = merge(local.secret_redis, local.tool_secret_env)
    }
    egress-proxy = {
      ingress    = "INGRESS_TRAFFIC_INTERNAL_ONLY"
      public     = false
      http2      = true
      cpu_always = false
      subnet     = module.network.egress_subnet_id
      sql        = false
      roles      = []
      health     = null
      liveness   = null
      env        = local.env_common
      secret_env = {}
    }
  }

  # Cloud Run Invoker grants (spec 17.2): callee => callers.
  invokers = {
    engine       = ["gateway"]
    scope        = ["gateway", "engine", "tool-runner"]
    graph        = ["gateway", "engine"]
    notifier     = ["gateway"]
    similarity   = ["gateway"]
    tool-runner  = ["engine"]
    egress-proxy = ["tool-runner", "engine", "notifier"]
  }
  invoker_pairs = merge([
    for callee, callers in local.invokers : { for c in callers : "${callee}/${c}" => { callee = callee, caller = c } }
  ]...)

  # Event topics (spec 11.7): producers and consumers.
  topics = {
    "run.events"      = { publishers = ["engine"], consumers = ["notifier", "gateway"] }
    "approval.events" = { publishers = ["engine"], consumers = ["notifier"] }
    "graph.changes"   = { publishers = ["graph"], consumers = ["notifier", "engine", "similarity"] }
    "scope.events"    = { publishers = ["scope"], consumers = ["engine", "tool-runner", "egress-proxy", "notifier"] }
    "auth.events"     = { publishers = ["gateway"], consumers = ["notifier"] }
  }

  runtime_sas = merge(
    { for s, m in module.svc : s => m.service_account_id },
    { migrate = google_service_account.migrate.name },
  )
}

# ---------------------------------------------------------------- APIs

resource "google_project_service" "apis" {
  for_each = toset([
    "artifactregistry.googleapis.com",
    "billingbudgets.googleapis.com",
    "cloudresourcemanager.googleapis.com",
    "cloudscheduler.googleapis.com",
    "compute.googleapis.com",
    "containerscanning.googleapis.com",
    "dns.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "logging.googleapis.com",
    "monitoring.googleapis.com",
    "pubsub.googleapis.com",
    "redis.googleapis.com",
    "run.googleapis.com",
    "secretmanager.googleapis.com",
    "servicenetworking.googleapis.com",
    "sqladmin.googleapis.com",
    "sts.googleapis.com",
  ])
  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

# ---------------------------------------------------------------- network, data stores

module "network" {
  source       = "../../modules/network"
  project_id   = var.project_id
  region       = var.region
  name         = local.name
  nat_ip_count = var.nat_ip_count

  depends_on = [google_project_service.apis]
}

module "sql" {
  source              = "../../modules/cloud-sql"
  project_id          = var.project_id
  region              = var.region
  instance_name       = "${local.name}-pg"
  tier                = var.db_tier
  high_availability   = var.db_high_availability
  network_id          = module.network.network_id
  psa_range_name      = module.network.psa_range_name
  users               = local.db_users
  deletion_protection = var.deletion_protection
  labels              = local.labels

  depends_on = [module.network]
}

module "redis" {
  source         = "../../modules/memorystore"
  project_id     = var.project_id
  region         = var.region
  name           = "${local.name}-redis"
  tier           = var.redis_tier
  memory_size_gb = var.redis_memory_gb
  network_id     = module.network.network_id
  psa_range_name = module.network.psa_range_name
  labels         = local.labels

  depends_on = [module.network]
}

module "artifacts" {
  source        = "../../modules/storage-bucket"
  project_id    = var.project_id
  name          = "${var.project_id}-ff-artifacts"
  location      = var.region
  kms_key_name  = var.artifact_kms_key
  force_destroy = !var.deletion_protection
  labels        = local.labels

  lifecycle_rules = [
    { prefix = "raw/", age_days = 90 },
    { prefix = "engine/archive/events/", age_days = 400 },
  ]

  # spec 17.2 prefix-scoped storage roles.
  iam = {
    engine = {
      role   = "roles/storage.objectAdmin"
      member = module.svc["engine"].service_account_member
      prefix = "engine/"
    }
    graph = {
      role   = "roles/storage.objectViewer"
      member = module.svc["graph"].service_account_member
      prefix = "raw/"
    }
    tool-runner = {
      role   = "roles/storage.objectCreator"
      member = module.svc["tool-runner"].service_account_member
      prefix = "raw/"
    }
  }
}

# ---------------------------------------------------------------- secrets (spec 17.1)

resource "google_secret_manager_secret" "this" {
  for_each  = toset(local.secret_names)
  project   = var.project_id
  secret_id = each.value
  labels    = local.labels

  replication {
    auto {}
  }

  depends_on = [google_project_service.apis]
}

# Terraform-generated values. Signing keys, the LLM key, webhook and tool
# secrets are added out of band (gcloud secrets versions add) so that their
# values never enter Terraform state.
resource "google_secret_manager_secret_version" "db" {
  for_each    = toset(local.db_users)
  secret      = google_secret_manager_secret.this["ff-db-${each.value}-password"].id
  secret_data = module.sql.user_passwords[each.value]
}

resource "google_secret_manager_secret_version" "redis_auth" {
  secret      = google_secret_manager_secret.this["ff-redis-auth"].id
  secret_data = module.redis.auth_string
}

resource "google_secret_manager_secret_version" "redis_ca" {
  secret      = google_secret_manager_secret.this["ff-redis-ca"].id
  secret_data = module.redis.server_ca_pem
}

# ---------------------------------------------------------------- Cloud Run services

module "svc" {
  for_each = local.services
  source   = "../../modules/cloud-run-service"

  project_id   = var.project_id
  region       = var.region
  name         = each.key
  service_name = "ff-${each.key}"
  account_id   = "sa-${each.key}"
  image        = var.images[each.key]

  ingress              = each.value.ingress
  public               = each.value.public
  use_http2            = each.value.http2
  cpu_always_allocated = each.value.cpu_always
  concurrency          = var.scaling[each.key].concurrency
  min_instances        = var.scaling[each.key].min
  max_instances        = var.scaling[each.key].max
  cpu                  = var.scaling[each.key].cpu
  memory               = var.scaling[each.key].memory

  network_id         = module.network.network_id
  subnetwork_id      = each.value.subnet
  network_tags       = ["ff-${each.key}"]
  cloudsql_instances = each.value.sql ? [module.sql.connection_name] : []

  project_roles = each.value.roles
  env           = each.value.env
  secret_env    = each.value.secret_env
  secret_ids    = { for e in values(each.value.secret_env) : e.secret => google_secret_manager_secret.this[e.secret].id }

  health_path         = each.value.health
  liveness_path       = each.value.liveness
  deletion_protection = var.deletion_protection
  labels              = local.labels

  depends_on = [
    google_secret_manager_secret_version.db,
    google_secret_manager_secret_version.redis_auth,
    google_secret_manager_secret_version.redis_ca,
  ]
}

resource "google_cloud_run_v2_service_iam_member" "invoker" {
  for_each = local.invoker_pairs
  project  = var.project_id
  location = var.region
  name     = module.svc[each.value.callee].name
  role     = "roles/run.invoker"
  member   = module.svc[each.value.caller].service_account_member
}

# ---------------------------------------------------------------- Pub/Sub (spec 11.7)

module "topic" {
  for_each = local.topics
  source   = "../../modules/pubsub-topic"

  project_id     = var.project_id
  project_number = local.project_number
  name           = each.key
  prefix         = var.topic_prefix
  publishers     = { for p in each.value.publishers : p => module.svc[p].service_account_member }
  consumers      = { for c in each.value.consumers : c => module.svc[c].service_account_member }
  labels         = local.labels

  depends_on = [google_project_service.apis]
}

# ---------------------------------------------------------------- migrations job (spec 10, 17.6)

resource "google_service_account" "migrate" {
  project      = var.project_id
  account_id   = "sa-migrate"
  display_name = "Flowforge migrations job"
}

resource "google_project_iam_member" "migrate_sql" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = google_service_account.migrate.member
}

resource "google_secret_manager_secret_iam_member" "migrate_db" {
  project   = var.project_id
  secret_id = google_secret_manager_secret.this["ff-db-migrator-password"].id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.migrate.member
}

resource "google_cloud_run_v2_job" "migrate" {
  project             = var.project_id
  name                = "ff-migrate"
  location            = var.region
  launch_stage        = "GA"
  deletion_protection = var.deletion_protection
  labels              = local.labels

  template {
    task_count  = 1
    parallelism = 1

    template {
      service_account       = google_service_account.migrate.email
      execution_environment = "EXECUTION_ENVIRONMENT_GEN2"
      max_retries           = 0
      timeout               = "900s"

      vpc_access {
        egress = "ALL_TRAFFIC"
        network_interfaces {
          network    = module.network.network_id
          subnetwork = module.network.services_subnet_id
          tags       = ["ff-migrate"]
        }
      }

      volumes {
        name = "cloudsql"
        cloud_sql_instance {
          instances = [module.sql.connection_name]
        }
      }

      containers {
        image = var.images["migrate"]

        env {
          name  = "FF_ENV"
          value = var.env
        }
        env {
          name  = "FF_DB_DSN"
          value = local.env_db["migrator"].FF_DB_DSN
        }
        env {
          name = "PGPASSWORD"
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.this["ff-db-migrator-password"].id
              version = "latest"
            }
          }
        }

        volume_mounts {
          name       = "cloudsql"
          mount_path = "/cloudsql"
        }

        resources {
          limits = {
            cpu    = "1"
            memory = "512Mi"
          }
        }
      }
    }
  }

  lifecycle {
    ignore_changes = [client, client_version, template[0].template[0].containers[0].image]
  }

  depends_on = [
    google_project_iam_member.migrate_sql,
    google_secret_manager_secret_iam_member.migrate_db,
    google_secret_manager_secret_version.db,
  ]
}

# ---------------------------------------------------------------- Cloud Scheduler (spec 17.1)

resource "google_service_account" "scheduler" {
  project      = var.project_id
  account_id   = "sa-scheduler"
  display_name = "Flowforge Cloud Scheduler invoker"
}

resource "google_cloud_run_v2_service_iam_member" "scheduler_engine" {
  project  = var.project_id
  location = var.region
  name     = module.svc["engine"].name
  role     = "roles/run.invoker"
  member   = google_service_account.scheduler.member
}

# The engine creates runs with idempotency key schedule:<id>:<fire time>.
resource "google_cloud_scheduler_job" "schedule" {
  for_each    = var.schedules
  project     = var.project_id
  region      = var.region
  name        = "${local.name}-schedule-${each.key}"
  description = "Fire Flowforge schedule ${each.key}"
  schedule    = each.value.cron
  time_zone   = each.value.time_zone

  retry_config {
    retry_count          = 3
    min_backoff_duration = "10s"
    max_backoff_duration = "300s"
  }

  http_target {
    http_method = "POST"
    uri         = "${local.run_url["engine"]}/internal/schedules/${each.key}:fire"

    oidc_token {
      service_account_email = google_service_account.scheduler.email
      audience              = local.run_url["engine"]
    }
  }

  depends_on = [google_cloud_run_v2_service_iam_member.scheduler_engine]
}

# ---------------------------------------------------------------- edge (spec 17.1)

module "edge" {
  source            = "../../modules/lb-edge"
  project_id        = var.project_id
  region            = var.region
  name              = "${local.name}-edge"
  cloud_run_service = module.svc["gateway"].name
  domains           = [var.domain]

  rate_limit_per_minute = var.edge_rate_limit_per_minute
}

# ---------------------------------------------------------------- images and CI identity (spec 17.1, 17.2)

resource "google_artifact_registry_repository" "images" {
  project       = var.project_id
  location      = var.region
  repository_id = "flowforge"
  format        = "DOCKER"
  description   = "Flowforge service images (deployed by digest)"
  labels        = local.labels

  docker_config {
    immutable_tags = true
  }

  depends_on = [google_project_service.apis]
}

resource "google_service_account" "ci" {
  project      = var.project_id
  account_id   = "sa-ci-deploy" # spec 17.2 sa-ci (account ids need 6+ characters)
  display_name = "Flowforge CI/CD (GitHub Actions via WIF)"
}

resource "google_iam_workload_identity_pool" "github" {
  project                   = var.project_id
  workload_identity_pool_id = "ff-github"
  display_name              = "GitHub Actions"

  depends_on = [google_project_service.apis]
}

resource "google_iam_workload_identity_pool_provider" "github" {
  project                            = var.project_id
  workload_identity_pool_id          = google_iam_workload_identity_pool.github.workload_identity_pool_id
  workload_identity_pool_provider_id = "github-actions"
  display_name                       = "GitHub Actions OIDC"

  attribute_mapping = {
    "google.subject"        = "assertion.sub"
    "attribute.repository"  = "assertion.repository"
    "attribute.ref"         = "assertion.ref"
    "attribute.environment" = "assertion.environment"
  }

  # Only this repository, and in prod only jobs bound to the protected
  # GitHub environment (manual approval).
  attribute_condition = var.github_environment == null ? "assertion.repository == \"${var.github_repository}\"" : "assertion.repository == \"${var.github_repository}\" && assertion.environment == \"${var.github_environment}\""

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }
}

resource "google_service_account_iam_member" "ci_wif" {
  service_account_id = google_service_account.ci.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github.name}/attribute.repository/${var.github_repository}"
}

resource "google_artifact_registry_repository_iam_member" "ci_writer" {
  project    = var.project_id
  location   = var.region
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.writer"
  member     = google_service_account.ci.member
}

# Cloud Run Admin on its services (and the migrations job) only.
resource "google_cloud_run_v2_service_iam_member" "ci_admin" {
  for_each = local.services
  project  = var.project_id
  location = var.region
  name     = module.svc[each.key].name
  role     = "roles/run.admin"
  member   = google_service_account.ci.member
}

resource "google_cloud_run_v2_job_iam_member" "ci_admin_migrate" {
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_job.migrate.name
  role     = "roles/run.admin"
  member   = google_service_account.ci.member
}

# Service Account User on the runtime accounts, so CI can deploy revisions
# that run as them.
resource "google_service_account_iam_member" "ci_act_as" {
  for_each           = toset(concat(local.service_names, ["migrate"]))
  service_account_id = local.runtime_sas[each.value]
  role               = "roles/iam.serviceAccountUser"
  member             = google_service_account.ci.member
}

# ---------------------------------------------------------------- monitoring (spec 18.1, 17.6)

module "monitoring" {
  source              = "../../modules/monitoring"
  project_id          = var.project_id
  project_number      = local.project_number
  env                 = var.env
  notification_emails = var.alert_emails
  billing_account     = var.billing_account
  monthly_budget      = var.monthly_budget
  budget_currency     = var.budget_currency

  depends_on = [google_project_service.apis]
}

# ---------------------------------------------------------------- outputs

output "api_ip" {
  description = "Point the API domain's A record here."
  value       = module.edge.ip_address
}

output "egress_ips" {
  description = "Static egress IPs of the egress proxy (publish to programs for allowlisting)."
  value       = module.network.nat_ips
}

output "service_urls" {
  description = "Cloud Run service URLs."
  value       = { for s, m in module.svc : s => m.uri }
}

output "artifact_registry" {
  description = "Docker repository for images."
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
}

output "ci_service_account" {
  description = "sa-ci email for GitHub Actions (vars.GCP_DEPLOY_SA)."
  value       = google_service_account.ci.email
}

output "workload_identity_provider" {
  description = "Full provider name for google-github-actions/auth (vars.GCP_WIF_PROVIDER)."
  value       = google_iam_workload_identity_pool_provider.github.name
}

output "topics" {
  description = "Topic => subscriptions."
  value       = { for t, m in module.topic : m.topic_name => m.subscriptions }
}
