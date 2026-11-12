# One Flowforge Cloud Run service (spec 17.1, 17.2): the runtime service
# account, its project-level roles, Secret Accessor on exactly the secrets the
# service mounts, Cloud Run Invoker for the listed callers, and the service
# itself (HTTP/2 for gRPC, scaling, CPU allocation, Direct VPC egress, Cloud
# SQL connector volume, env and secret env).

terraform {
  required_version = ">= 1.6"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}

locals {
  labels = merge(var.labels, {
    "ff-service" = var.name
    "managed-by" = "terraform"
  })
}

resource "google_service_account" "this" {
  project      = var.project_id
  account_id   = var.account_id
  display_name = "Flowforge ${var.name} runtime"
  description  = "Runtime identity of the ${var.service_name} Cloud Run service (spec 17.2)."
}

# Project-scoped predefined roles that have no narrower resource (for example
# roles/cloudsql.client). Resource-scoped grants (topics, buckets, other
# services) are made by the caller on the resource itself.
resource "google_project_iam_member" "roles" {
  for_each = toset(var.project_roles)
  project  = var.project_id
  role     = each.value
  member   = google_service_account.this.member
}

# Secret Accessor only on the secrets this service mounts.
resource "google_secret_manager_secret_iam_member" "accessor" {
  for_each  = var.secret_ids
  project   = var.project_id
  secret_id = each.value
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.this.member
}

resource "google_cloud_run_v2_service" "this" {
  project              = var.project_id
  name                 = var.service_name
  location             = var.region
  description          = "Flowforge ${var.name}"
  ingress              = var.ingress
  launch_stage         = "GA"
  deletion_protection  = var.deletion_protection
  invoker_iam_disabled = var.public
  labels               = local.labels

  template {
    service_account                  = google_service_account.this.email
    execution_environment            = var.execution_environment
    max_instance_request_concurrency = var.concurrency
    timeout                          = var.request_timeout
    labels                           = local.labels

    scaling {
      min_instance_count = var.min_instances
      max_instance_count = var.max_instances
    }

    vpc_access {
      egress = var.vpc_egress
      network_interfaces {
        network    = var.network_id
        subnetwork = var.subnetwork_id
        tags       = var.network_tags
      }
    }

    dynamic "volumes" {
      for_each = length(var.cloudsql_instances) > 0 ? [1] : []
      content {
        name = "cloudsql"
        cloud_sql_instance {
          instances = var.cloudsql_instances
        }
      }
    }

    containers {
      name  = var.name
      image = var.image

      ports {
        name           = var.use_http2 ? "h2c" : "http1"
        container_port = var.container_port
      }

      resources {
        limits = {
          cpu    = var.cpu
          memory = var.memory
        }
        # cpu_idle = false means CPU is always allocated (background loops).
        cpu_idle          = !var.cpu_always_allocated
        startup_cpu_boost = true
      }

      dynamic "env" {
        for_each = var.env
        content {
          name  = env.key
          value = env.value
        }
      }

      dynamic "env" {
        for_each = var.secret_env
        content {
          name = env.key
          value_source {
            secret_key_ref {
              secret  = var.secret_ids[env.value.secret]
              version = env.value.version
            }
          }
        }
      }

      dynamic "volume_mounts" {
        for_each = length(var.cloudsql_instances) > 0 ? [1] : []
        content {
          name       = "cloudsql"
          mount_path = "/cloudsql"
        }
      }

      dynamic "startup_probe" {
        for_each = var.health_path == null ? [] : [var.health_path]
        content {
          initial_delay_seconds = 0
          period_seconds        = 5
          failure_threshold     = 12
          timeout_seconds       = 3
          http_get {
            path = startup_probe.value
          }
        }
      }

      dynamic "liveness_probe" {
        for_each = var.liveness_path == null ? [] : [var.liveness_path]
        content {
          period_seconds    = 30
          failure_threshold = 3
          timeout_seconds   = 3
          http_get {
            path = liveness_probe.value
          }
        }
      }
    }
  }

  traffic {
    type    = "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
    percent = 100
  }

  lifecycle {
    # Images are rolled forward by .github/workflows/deploy.yml by digest
    # with a 10% -> 100% traffic split; Terraform sets the initial digest and
    # owns everything else.
    ignore_changes = [
      client,
      client_version,
      traffic,
      template[0].revision,
      template[0].containers[0].image,
    ]
  }

  depends_on = [
    google_project_iam_member.roles,
    google_secret_manager_secret_iam_member.accessor,
  ]
}

resource "google_cloud_run_v2_service_iam_member" "invokers" {
  for_each = var.invoker_members
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.this.name
  role     = "roles/run.invoker"
  member   = each.value
}
