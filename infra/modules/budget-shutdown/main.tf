# Optional budget-triggered shutdown (cost guardrail, docs/GAPS.md G-OPS-20).
#
# Cloud Billing budget -> Pub/Sub topic -> push subscription (OIDC) -> a tiny
# Cloud Run service (google-cloud-cli image + handler.py, no build step) that,
# once actual spend reaches threshold x budget, closes the gateway to public
# traffic, pauses the Cloud Scheduler jobs, sets min instances 0, and stops
# the VM (lab) or the Cloud SQL instance (dev/prod). Everything it touches is
# granted on the narrowest resource; the scheduler pause needs a project-level
# custom role because Cloud Scheduler jobs have no resource-level IAM.
#
# Things it cannot stop (Memorystore, Cloud NAT, the load balancer, reserved
# IPs, disks, stored objects) keep billing: use `make gcp-down` for those.

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
  pubsub_agent = "serviceAccount:service-${var.project_number}@gcp-sa-pubsub.iam.gserviceaccount.com"
  # Cloud Billing publishes budget notifications as this Google-managed account.
  billing_publisher = "serviceAccount:billing-budget-alert@system.gserviceaccount.com"

  # Cloud Run services the handler updates: gateway (ingress) and the
  # scale-down list (min instances). service name => runtime SA resource id.
  updated_services = merge(
    var.gateway == null ? {} : { (var.gateway.service) = var.gateway.service_account_id },
    var.scale_down_services,
  )

  custom_permissions = concat(
    length(var.scheduler_jobs) > 0 ? ["cloudscheduler.jobs.get", "cloudscheduler.jobs.pause"] : [],
    var.sql_instance == null ? [] : ["cloudsql.instances.get", "cloudsql.instances.update"],
  )

  env = {
    FF_PROJECT             = var.project_id
    FF_REGION              = var.region
    FF_SHUTDOWN_THRESHOLD  = tostring(var.threshold)
    FF_GATEWAY_SERVICE     = var.gateway == null ? "" : var.gateway.service
    FF_SCHEDULER_JOBS      = join(",", var.scheduler_jobs)
    FF_SCALE_DOWN_SERVICES = join(",", keys(var.scale_down_services))
    FF_VM                  = var.vm == null ? "" : var.vm.name
    FF_VM_ZONE             = var.vm == null ? "" : var.vm.zone
    FF_SQL_INSTANCE        = var.sql_instance == null ? "" : var.sql_instance
  }
}

# ---------------------------------------------------------------- budget topic

resource "google_pubsub_topic" "budget" {
  project = var.project_id
  name    = "${var.name}-budget"
  labels  = var.labels
}

resource "google_pubsub_topic_iam_member" "billing_publisher" {
  project = var.project_id
  topic   = google_pubsub_topic.budget.name
  role    = "roles/pubsub.publisher"
  member  = local.billing_publisher
}

# ---------------------------------------------------------------- identities

resource "google_service_account" "handler" {
  project      = var.project_id
  account_id   = "sa-${var.name}-stop"
  display_name = "Flowforge budget shutdown handler"
  description  = "Scales the environment down when the budget is exhausted (infra/modules/budget-shutdown)."
}

resource "google_service_account" "push" {
  project      = var.project_id
  account_id   = "sa-${var.name}-budget-push"
  display_name = "Flowforge budget notifications push identity"
}

# Pub/Sub mints the push subscription's OIDC token as sa-...-budget-push.
resource "google_service_account_iam_member" "push_token_creator" {
  service_account_id = google_service_account.push.name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = local.pubsub_agent
}

resource "google_project_iam_custom_role" "handler" {
  count       = length(local.custom_permissions) > 0 ? 1 : 0
  project     = var.project_id
  role_id     = "ffBudgetShutdown${title(replace(var.name, "-", ""))}"
  title       = "Flowforge budget shutdown (${var.name})"
  description = "Pause Cloud Scheduler jobs and stop Cloud SQL when the budget is exhausted."
  permissions = local.custom_permissions
}

resource "google_project_iam_member" "handler_custom" {
  count   = length(local.custom_permissions) > 0 ? 1 : 0
  project = var.project_id
  role    = google_project_iam_custom_role.handler[0].id
  member  = google_service_account.handler.member
}

# Update (ingress, min instances) on exactly the services it changes.
resource "google_cloud_run_v2_service_iam_member" "handler_developer" {
  for_each = local.updated_services
  project  = var.project_id
  location = var.region
  name     = each.key
  role     = "roles/run.developer"
  member   = google_service_account.handler.member
}

# A new revision runs as the service's runtime account.
resource "google_service_account_iam_member" "handler_act_as" {
  for_each           = local.updated_services
  service_account_id = each.value
  role               = "roles/iam.serviceAccountUser"
  member             = google_service_account.handler.member
}

resource "google_compute_instance_iam_member" "handler_vm" {
  count         = var.vm == null ? 0 : 1
  project       = var.project_id
  zone          = var.vm.zone
  instance_name = var.vm.name
  role          = "roles/compute.instanceAdmin.v1"
  member        = google_service_account.handler.member
}

# ---------------------------------------------------------------- handler service

resource "google_cloud_run_v2_service" "handler" {
  project             = var.project_id
  name                = "${var.name}-budget-shutdown"
  location            = var.region
  description         = "Flowforge budget-triggered shutdown"
  ingress             = "INGRESS_TRAFFIC_INTERNAL_ONLY" # Pub/Sub push in the same project is internal traffic
  launch_stage        = "GA"
  deletion_protection = false
  labels              = var.labels

  template {
    service_account                  = google_service_account.handler.email
    max_instance_request_concurrency = 1
    timeout                          = "300s"
    labels                           = var.labels

    scaling {
      min_instance_count = 0
      max_instance_count = 1
    }

    containers {
      name    = "handler"
      image   = var.image
      command = ["python3", "-c"]
      args    = [file("${path.module}/handler.py")]

      dynamic "env" {
        for_each = local.env
        content {
          name  = env.key
          value = env.value
        }
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
        cpu_idle = true
      }
    }
  }

  depends_on = [
    google_project_iam_member.handler_custom,
    google_cloud_run_v2_service_iam_member.handler_developer,
    google_service_account_iam_member.handler_act_as,
    google_compute_instance_iam_member.handler_vm,
  ]
}

resource "google_cloud_run_v2_service_iam_member" "push_invoker" {
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.handler.name
  role     = "roles/run.invoker"
  member   = google_service_account.push.member
}

resource "google_pubsub_subscription" "push" {
  project              = var.project_id
  name                 = "${var.name}-budget.shutdown"
  topic                = google_pubsub_topic.budget.id
  ack_deadline_seconds = 300
  labels               = var.labels

  # Budget notifications are only useful while fresh.
  message_retention_duration = "3600s"

  retry_policy {
    minimum_backoff = "30s"
    maximum_backoff = "600s"
  }

  push_config {
    push_endpoint = google_cloud_run_v2_service.handler.uri

    oidc_token {
      service_account_email = google_service_account.push.email
      audience              = google_cloud_run_v2_service.handler.uri
    }
  }

  expiration_policy {
    ttl = ""
  }

  depends_on = [
    google_cloud_run_v2_service_iam_member.push_invoker,
    google_service_account_iam_member.push_token_creator,
  ]
}
