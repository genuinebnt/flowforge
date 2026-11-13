# Flowforge lab: the near-zero-cost learning environment (docs/GAPS.md
# "Deployment profiles", G-OPS-19; costs in infra/COSTS.md).
#
# Differences from dev/prod (which keep the full spec 17 design):
#   - Cloud Run services scale to zero (min 0, max 1-2) and serve on run.app;
#     the gateway is INGRESS_TRAFFIC_ALL with the invoker check off (its own
#     auth applies), there is no load balancer, Cloud Armor or managed cert.
#   - Postgres 16, optional Redis and the egress proxy (HTTP/CONNECT, SOCKS5,
#     DNS) run in containers on ONE e2-micro Container-Optimized OS VM
#     (vm.tf); no Cloud SQL, Memorystore, Cloud NAT or Serverless VPC Access
#     connector.
#   - One database role and one password secret instead of one per service,
#     to stay within Secret Manager's free active-version allowance.
#   - One Cloud Scheduler job (the schedules tick), Artifact Registry cleanup
#     policies, no container scanning, no alert policies or log metrics,
#     topic-level message retention off.
#   - Every resource is labelled env=lab, app=flowforge.

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
  zone           = coalesce(var.zone, var.region == "us-east1" ? "us-east1-b" : "${var.region}-a")

  # Cloud Run services. The egress proxy is not one of them: it runs on the
  # VM, because the lab has no Cloud NAT and the v2 proxy needs raw TCP/UDP.
  service_names = ["gateway", "engine", "scope", "graph", "notifier", "similarity", "tool-runner", "content"]

  # Deterministic run.app URLs (https://<service>-<project number>.<region>.run.app).
  run_url = { for s in local.service_names : s => "https://ff-${s}-${local.project_number}.${var.region}.run.app" }

  vm_ip       = google_compute_address.vm.address
  proxy_url   = "http://${local.vm_ip}:${var.proxy_port}"
  jwt_issuer  = coalesce(var.jwt_issuer, local.run_url["gateway"])
  db_password = "ff-db-password"

  # --- secrets. Free tier: 6 active secret versions per billing account;
  # each secret here holds one version (rotation adds one until the old one
  # is disabled or destroyed).
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
    [local.db_password],
    # FF_CONTENT_KEK (32 bytes hex or base64), added out of band.
    ["ff-content-kek"],
    var.redis_enabled ? ["ff-redis-auth"] : [],
    var.llm_api_key_enabled ? ["ff-llm-api-key"] : [],
    [for c in var.webhook_channels : "ff-webhook-${c}"],
    [for t in var.tool_secrets : "ff-tool-${t}"],
  )

  # --- spec 17.5 environment variables (same names as dev/prod).
  env_common = {
    FF_ENV = var.ff_env
    FF_BUS = "pubsub"
    # Cloud Run gives a revision 10 s after SIGTERM; the engine's graceful
    # drain (G-ENG-05) must finish inside that, so it is shorter than the
    # 20 s default used off Cloud Run.
    FF_DRAIN_TIMEOUT = var.drain_timeout
    # Webhook and Slack hosts the egress proxy's notify path may reach
    # (G-SVC-08); empty refuses every delivery (fail closed).
    FF_NOTIFY_ALLOWLIST = join(",", var.notify_allowlist)
    FF_JWKS_URL         = "${local.run_url["gateway"]}/.well-known/jwks.json"
    FF_JWT_ISSUER       = local.jwt_issuer
    FF_JWT_AUDIENCE     = var.jwt_audience_internal
    FF_PUBSUB_PROJECT   = var.project_id
    FF_TOPIC_PREFIX     = var.topic_prefix
  }
  env_signing = {
    FF_SIGNING_KID  = var.current_signing_kid
    FF_SIGNING_KIDS = join(",", var.signing_key_kids)
  }
  # Postgres on the VM: one role, TLS when the VM generated its self-signed
  # certificate (sslmode=prefer: encrypted on the private network, the server
  # is not authenticated; see infra/README.md).
  env_db = {
    FF_DB_DSN = "host=${local.vm_ip} port=5432 dbname=flowforge user=flowforge sslmode=prefer"
  }
  secret_db = { PGPASSWORD = { secret = local.db_password } }
  # Redis on the VM is plaintext on the private network, AUTH on, reachable
  # only from the services subnet; FF_REDIS_CA is not set (G-OPS-13).
  env_redis    = var.redis_enabled ? { FF_REDIS_ADDR = "${local.vm_ip}:6379" } : {}
  secret_redis = var.redis_enabled ? { FF_REDIS_AUTH = { secret = "ff-redis-auth" } } : {}
  env_bucket   = { FF_ARTIFACT_BUCKET = module.artifacts.name }
  env_proxy    = { FF_EGRESS_PROXY = local.proxy_url }
  addr         = { for s in local.service_names : s => { "FF_${upper(replace(s, "-", "_"))}_ADDR" = local.run_url[s] } }

  services = {
    gateway = {
      ingress  = "INGRESS_TRAFFIC_ALL" # no load balancer in the lab: run.app is the public URL
      public   = true
      http2    = false
      subnet   = module.network.services_subnet_id
      health   = "/readyz"
      liveness = "/healthz"
      env = merge(local.env_common, local.env_db, local.env_redis, local.env_signing,
        local.addr["engine"], local.addr["scope"], local.addr["graph"], local.addr["notifier"], local.addr["similarity"], local.addr["tool-runner"],
        local.addr["content"],
        {
          FF_JWT_AUDIENCE      = var.jwt_audience_public
          FF_INTERNAL_AUDIENCE = var.jwt_audience_internal
          # Client IP for rate limits and login throttles: the last X-Forwarded-For
          # entry from the right; on run.app Google's front end appends the client address, so it is the last entry. Entries to its left are client-supplied
          # (G-SVC-17). Confirm on the first apply (G-OPS-17): too low keys every
          # client by a proxy address, too high lets a client choose its key.
          FF_TRUST_XFF = "1"
      })
      secret_env = merge(local.secret_db, local.secret_redis, local.signing_key_env)
    }
    engine = {
      ingress  = "INGRESS_TRAFFIC_INTERNAL_ONLY"
      public   = false
      http2    = true
      subnet   = module.network.services_subnet_id
      health   = null
      liveness = null
      env = merge(local.env_common, local.env_db, local.env_redis, local.env_bucket, local.env_signing, local.env_proxy,
        local.addr["scope"], local.addr["graph"], local.addr["tool-runner"],
        {
          FF_TICK_OIDC_AUDIENCE   = local.run_url["engine"]
          FF_TICK_SERVICE_ACCOUNT = google_service_account.scheduler.email
          FF_LLM_ENDPOINT         = var.llm_endpoint
          FF_LLM_MODEL            = var.llm_model
          FF_FUEL_DEFAULT         = tostring(var.fuel_default)
          FF_PLAN_FUEL_DEFAULT    = tostring(var.plan_fuel_default)
      })
      secret_env = merge(local.secret_db, local.secret_redis, local.llm_env, local.signing_key_env)
    }
    scope = {
      ingress    = "INGRESS_TRAFFIC_INTERNAL_ONLY"
      public     = false
      http2      = true
      subnet     = module.network.services_subnet_id
      health     = null
      liveness   = null
      env        = merge(local.env_common, local.env_db, local.env_redis)
      secret_env = merge(local.secret_db, local.secret_redis)
    }
    graph = {
      ingress    = "INGRESS_TRAFFIC_INTERNAL_ONLY"
      public     = false
      http2      = true
      subnet     = module.network.services_subnet_id
      health     = null
      liveness   = null
      env        = merge(local.env_common, local.env_db, local.env_bucket, local.env_signing, local.addr["scope"])
      secret_env = merge(local.secret_db, local.signing_key_env)
    }
    notifier = {
      ingress    = "INGRESS_TRAFFIC_INTERNAL_ONLY"
      public     = false
      http2      = true
      subnet     = module.network.services_subnet_id
      health     = null
      liveness   = null
      env        = merge(local.env_common, local.env_db, local.env_proxy)
      secret_env = merge(local.secret_db, local.webhook_env)
    }
    similarity = {
      ingress    = "INGRESS_TRAFFIC_INTERNAL_ONLY"
      public     = false
      http2      = true
      subnet     = module.network.services_subnet_id
      health     = null
      liveness   = null
      env        = merge(local.env_common, local.env_db, local.env_signing, local.addr["graph"], local.addr["scope"])
      secret_env = merge(local.secret_db, local.signing_key_env)
    }
    tool-runner = {
      ingress    = "INGRESS_TRAFFIC_INTERNAL_ONLY"
      public     = false
      http2      = true
      subnet     = module.network.tools_subnet_id
      health     = null
      liveness   = null
      env        = merge(local.env_common, local.env_bucket, local.env_signing, local.env_proxy, local.addr["scope"], local.addr["content"])
      secret_env = merge(local.tool_secret_env, local.signing_key_env)
    }
    # The content service (G-OPS-16, G-CST-01): chunk packs and text indexes
    # in the content bucket (G-CST-02), rows in the content schema on the
    # VM's Postgres (the lab's one database role, like every lab service).
    # Request-based CPU: its content.changes relay and sweeps progress
    # while it serves captures, which is when they have work. One instance
    # at most (G-CST-03).
    content = {
      ingress  = "INGRESS_TRAFFIC_INTERNAL_ONLY"
      public   = false
      http2    = true
      subnet   = module.network.services_subnet_id
      health   = null
      liveness = null
      env = merge(local.env_common, local.env_db, {
        FF_CONTENT_BUCKET = module.content_bucket.name
        FF_CONTENT_KEK_ID = var.content_kek_id
      })
      secret_env = merge(local.secret_db, { FF_CONTENT_KEK = { secret = "ff-content-kek" } })
    }
  }

  # Cloud Run Invoker grants (spec 17.2): callee => callers. The VM's
  # egress proxy also invokes scope (granted in vm.tf).
  invokers = {
    engine      = ["gateway"]
    scope       = ["gateway", "engine", "tool-runner", "graph", "similarity"]
    graph       = ["gateway", "engine", "similarity"]
    notifier    = ["gateway"]
    similarity  = ["gateway"]
    tool-runner = ["engine", "gateway"]
    content     = ["gateway", "tool-runner"]
  }
  invoker_pairs = merge([
    for callee, callers in local.invokers : { for c in callers : "${callee}/${c}" => { callee = callee, caller = c } }
  ]...)

  # Event topics (spec 11.7): producers and consumers ("egress-proxy" is the VM).
  topics = {
    "run.events"      = { publishers = ["engine"], consumers = ["notifier", "gateway"] }
    "approval.events" = { publishers = ["engine"], consumers = ["notifier"] }
    "graph.changes"   = { publishers = ["graph"], consumers = ["notifier", "engine", "similarity"] }
    "scope.events"    = { publishers = ["scope"], consumers = ["engine", "tool-runner", "egress-proxy", "notifier"] }
    "auth.events"     = { publishers = ["gateway"], consumers = ["notifier"] }
    "content.changes" = { publishers = ["content"], consumers = ["graph", "notifier"] }
  }
  members = merge(
    { for s, m in module.svc : s => m.service_account_member },
    { egress-proxy = google_service_account.vm.member },
  )

  runtime_sas = merge(
    { for s, m in module.svc : s => m.service_account_id },
    { migrate = google_service_account.migrate.name, vm = google_service_account.vm.name },
  )
}

# ---------------------------------------------------------------- guardrails

# Cost guardrails checked at plan time. infra/policy repeats the resource-type
# checks on the plan JSON (conftest), so a later edit cannot slip past them.
resource "terraform_data" "guardrails" {
  input = var.allow_billable

  lifecycle {
    precondition {
      condition     = contains(var.allow_billable, "min_instances") || alltrue([for s in values(var.scaling) : s.min == 0])
      error_message = "lab: every Cloud Run service scales to zero (min = 0); add \"min_instances\" to allow_billable to override."
    }
    precondition {
      condition     = contains(var.allow_billable, "max_instances") || alltrue([for s in values(var.scaling) : s.max >= 1 && s.max <= 2])
      error_message = "lab: max instances must be 1 or 2; add \"max_instances\" to allow_billable to override."
    }
    precondition {
      condition     = contains(var.allow_billable, "cpu_always") || alltrue([for s in values(var.scaling) : !s.cpu_always])
      error_message = "lab: CPU always allocated is instance-based billing; add \"cpu_always\" to allow_billable to override."
    }
    precondition {
      condition     = contains(var.allow_billable, "vm_size") || var.vm_machine_type == "e2-micro"
      error_message = "lab: the VM must be e2-micro (free tier); add \"vm_size\" to allow_billable to override."
    }
    precondition {
      condition     = contains(var.allow_billable, "disk_size") || (var.vm_boot_disk_gb + var.vm_data_disk_gb <= 30 && var.vm_disk_type == "pd-standard")
      error_message = "lab: VM disks must be pd-standard and 30 GB in total (free tier); add \"disk_size\" to allow_billable to override."
    }
    precondition {
      condition     = contains(var.allow_billable, "secret_versions") || length(local.secret_names) <= 6
      error_message = "lab: more than 6 secrets exceeds Secret Manager's free active versions; add \"secret_versions\" to allow_billable to override."
    }
  }
}

# ---------------------------------------------------------------- APIs

# No containerscanning (billed per scanned image), sqladmin, redis or
# servicenetworking: the lab uses none of them.
resource "google_project_service" "apis" {
  for_each = toset([
    "artifactregistry.googleapis.com",
    "billingbudgets.googleapis.com",
    "cloudresourcemanager.googleapis.com",
    "cloudscheduler.googleapis.com",
    "compute.googleapis.com",
    "dns.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "iap.googleapis.com",
    "logging.googleapis.com",
    "monitoring.googleapis.com",
    "oslogin.googleapis.com",
    "pubsub.googleapis.com",
    "run.googleapis.com",
    "secretmanager.googleapis.com",
    "storage.googleapis.com",
    "sts.googleapis.com",
  ])
  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

# ---------------------------------------------------------------- network

# The shared module with its billed pieces off: no Cloud NAT, no private
# services access, no flow or firewall logs. The VM sits in the "egress"
# subnet and has its own (ephemeral) external address for the proxy.
module "network" {
  source        = "../../modules/network"
  project_id    = var.project_id
  region        = var.region
  name          = local.name
  services_cidr = var.services_cidr
  tools_cidr    = var.tools_cidr
  egress_cidr   = var.egress_cidr

  nat_enabled           = false
  psa_enabled           = false
  private_dns_enabled   = var.private_dns_enabled
  flow_logs_enabled     = false
  firewall_logs_enabled = false

  depends_on = [google_project_service.apis]
}

# ---------------------------------------------------------------- storage (spec 17.1)

module "artifacts" {
  source        = "../../modules/storage-bucket"
  project_id    = var.project_id
  name          = "${var.project_id}-ff-artifacts"
  location      = upper(var.region) # Standard class in a free-tier US region
  force_destroy = !var.deletion_protection
  labels        = local.labels

  # Soft-deleted objects are billed as stored; the lab keeps none.
  soft_delete_seconds = 0

  lifecycle_rules = [
    { prefix = "raw/", age_days = var.raw_retention_days },
    { prefix = "engine/archive/events/", age_days = var.archive_retention_days },
  ]

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

  depends_on = [google_project_service.apis]
}

# The content service's chunk packs and text indexes (chunkstore/gcsstore):
# chunks/t/<tenant>/... and index/<tenant>/<program>/... No lifecycle rule
# deletes anything (the content service's GC removes unreferenced packs
# after swapping the ref); soft delete is off because soft-deleted objects
# are billed as stored.
module "content_bucket" {
  source        = "../../modules/storage-bucket"
  project_id    = var.project_id
  name          = "${var.project_id}-ff-content"
  location      = upper(var.region)
  force_destroy = !var.deletion_protection
  labels        = local.labels

  soft_delete_seconds = 0
  lifecycle_rules     = []

  iam = {
    content = {
      role   = "roles/storage.objectUser"
      member = module.svc["content"].service_account_member
    }
  }

  depends_on = [google_project_service.apis]
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

# Generated here (the values are in Terraform state, as dev/prod's database
# passwords are). Signing keys and the other secrets are added out of band.
resource "random_password" "db" {
  length  = 32
  special = false
}

resource "random_password" "redis" {
  count   = var.redis_enabled ? 1 : 0
  length  = 32
  special = false
}

resource "google_secret_manager_secret_version" "db" {
  secret      = google_secret_manager_secret.this[local.db_password].id
  secret_data = random_password.db.result
}

resource "google_secret_manager_secret_version" "redis" {
  count       = var.redis_enabled ? 1 : 0
  secret      = google_secret_manager_secret.this["ff-redis-auth"].id
  secret_data = random_password.redis[0].result
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
  cpu_always_allocated = var.scaling[each.key].cpu_always
  concurrency          = var.scaling[each.key].concurrency
  min_instances        = var.scaling[each.key].min
  max_instances        = var.scaling[each.key].max
  cpu                  = var.scaling[each.key].cpu
  memory               = var.scaling[each.key].memory

  # Direct VPC egress (no connector), all traffic: internal-ingress callees,
  # Google APIs through private.googleapis.com, the VM on its private IP.
  network_id    = module.network.network_id
  subnetwork_id = each.value.subnet
  network_tags  = ["ff-${each.key}"]

  env        = each.value.env
  secret_env = each.value.secret_env
  secret_ids = { for e in values(each.value.secret_env) : e.secret => google_secret_manager_secret.this[e.secret].id }

  health_path         = each.value.health
  liveness_path       = each.value.liveness
  deletion_protection = var.deletion_protection
  labels              = local.labels

  depends_on = [
    terraform_data.guardrails,
    google_secret_manager_secret_version.db,
    google_secret_manager_secret_version.redis,
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
  publishers     = { for p in each.value.publishers : p => local.members[p] }
  consumers      = { for c in each.value.consumers : c => local.members[c] }
  labels         = local.labels

  # Free tier: 10 GiB of throughput a month. Topic retention (message
  # storage) is off; subscriptions keep undelivered messages 7 days.
  topic_retention_enabled = false
  subscription_retention  = "604800s"
  dead_letter_retention   = "604800s"

  depends_on = [google_project_service.apis]
}

# ---------------------------------------------------------------- migrations job (spec 10)

resource "google_service_account" "migrate" {
  project      = var.project_id
  account_id   = "sa-migrate"
  display_name = "Flowforge migrations job"
}

resource "google_secret_manager_secret_iam_member" "migrate_db" {
  project   = var.project_id
  secret_id = google_secret_manager_secret.this[local.db_password].id
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

      containers {
        image = var.images["migrate"]

        env {
          name  = "FF_ENV"
          value = var.ff_env
        }
        env {
          name  = "FF_DB_DSN"
          value = local.env_db.FF_DB_DSN
        }
        env {
          name = "PGPASSWORD"
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.this[local.db_password].id
              version = "latest"
            }
          }
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
    google_secret_manager_secret_iam_member.migrate_db,
    google_secret_manager_secret_version.db,
  ]
}

# ---------------------------------------------------------------- Cloud Scheduler: the one tick job

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

# Each tick is one short request (request-based billing; 43,200 a month at
# one per minute), and the engine's in-process scheduler fires whatever
# schedules are due (G-OPS-10).
resource "google_cloud_scheduler_job" "tick" {
  project     = var.project_id
  region      = var.region
  name        = "${local.name}-schedules-tick"
  description = "Flowforge schedules tick (POST /internal/schedules:tick)"
  schedule    = var.schedule_tick_cron
  time_zone   = "Etc/UTC"

  retry_config {
    retry_count          = 0 # the next tick catches up
    min_backoff_duration = "5s"
    max_backoff_duration = "60s"
  }

  http_target {
    http_method = "POST"
    uri         = "${local.run_url["engine"]}/internal/schedules:tick"

    oidc_token {
      service_account_email = google_service_account.scheduler.email
      audience              = local.run_url["engine"]
    }
  }

  depends_on = [google_cloud_run_v2_service_iam_member.scheduler_engine, google_project_service.apis]
}

# ---------------------------------------------------------------- images and CI identity (spec 17.1, 17.2)

resource "google_artifact_registry_repository" "images" {
  project       = var.project_id
  location      = var.region
  repository_id = "flowforge"
  format        = "DOCKER"
  description   = "Flowforge service images (deployed by digest); cleanup keeps the newest versions"
  labels        = local.labels

  docker_config {
    immutable_tags = true
  }

  # On-push scanning is billed per image; the lab does not scan.
  vulnerability_scanning_config {
    enablement_config = "DISABLED"
  }

  # Stay near the 0.5 GB free tier: keep the newest artifact_keep_count
  # versions of every image (KEEP wins over DELETE), delete the rest after a day.
  cleanup_policy_dry_run = false
  cleanup_policies {
    id     = "keep-newest"
    action = "KEEP"
    most_recent_versions {
      keep_count = var.artifact_keep_count
    }
  }
  cleanup_policies {
    id     = "delete-older"
    action = "DELETE"
    condition {
      tag_state  = "ANY"
      older_than = "86400s"
    }
  }

  depends_on = [google_project_service.apis]
}

resource "google_service_account" "ci" {
  project      = var.project_id
  account_id   = "sa-ci-deploy"
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

# The deploy workflow rolls the VM's egress proxy forward by setting the
# instance metadata key ff-egress-proxy-image (vm.tf watches it).
resource "google_compute_instance_iam_member" "ci_vm" {
  project       = var.project_id
  zone          = local.zone
  instance_name = google_compute_instance.vm.name
  role          = "roles/compute.instanceAdmin.v1"
  member        = google_service_account.ci.member
}

resource "google_service_account_iam_member" "ci_act_as" {
  for_each           = toset(concat(local.service_names, ["migrate", "vm"]))
  service_account_id = local.runtime_sas[each.value]
  role               = "roles/iam.serviceAccountUser"
  member             = google_service_account.ci.member
}

# ---------------------------------------------------------------- cost guardrails (G-OPS-20)

module "monitoring" {
  source              = "../../modules/monitoring"
  project_id          = var.project_id
  project_number      = local.project_number
  env                 = var.env
  log_env             = var.ff_env
  notification_emails = var.alert_emails
  billing_account     = var.billing_account
  monthly_budget      = var.monthly_budget
  budget_currency     = var.budget_currency
  budget_pubsub_topic = var.budget_shutdown_enabled ? module.budget_shutdown[0].topic_id : null

  alert_policies_enabled = false
  log_metrics_enabled    = false

  depends_on = [google_project_service.apis]
}

module "budget_shutdown" {
  count          = var.budget_shutdown_enabled ? 1 : 0
  source         = "../../modules/budget-shutdown"
  project_id     = var.project_id
  project_number = local.project_number
  region         = var.region
  name           = local.name
  image          = var.budget_shutdown_image
  threshold      = var.budget_shutdown_threshold
  labels         = local.labels

  gateway = {
    service            = module.svc["gateway"].name
    service_account_id = module.svc["gateway"].service_account_id
  }
  scheduler_jobs = [google_cloud_scheduler_job.tick.name]
  vm = {
    name = google_compute_instance.vm.name
    zone = local.zone
  }

  depends_on = [google_project_service.apis]
}
