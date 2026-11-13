# Offline plan of the prod root with mocked providers (no GCP credentials):
#   terraform -chdir=infra/envs/prod init -backend=false && terraform -chdir=infra/envs/prod test
# Uses terraform.tfvars. dev/prod tests are kept identical apart from this header.

mock_provider "google" {
  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }
  mock_resource "google_service_account" {
    defaults = {
      member = "serviceAccount:mock@p.iam.gserviceaccount.com"
      email  = "mock@p.iam.gserviceaccount.com"
      name   = "projects/p/serviceAccounts/mock@p.iam.gserviceaccount.com"
    }
  }
  mock_resource "google_sql_database_instance" {
    defaults = {
      connection_name = "p:us-central1:ff-pg"
    }
  }
  mock_data "google_compute_image" {
    defaults = {
      self_link = "https://www.googleapis.com/compute/v1/projects/cos-cloud/global/images/cos-stable-0"
    }
  }
  mock_resource "google_compute_address" {
    override_during = plan
    defaults = {
      address = "10.10.5.2"
    }
  }
}

mock_provider "random" {}

run "plan" {
  command = plan

  assert {
    condition     = alltrue([for s in ["gateway", "engine", "tool-runner", "graph", "similarity"] : contains(keys(local.services[s].secret_env), "FF_SIGNING_KEY_2026_11") && local.services[s].env["FF_SIGNING_KID"] == var.current_signing_kid]) && contains(module.egress.secret_env_names, "FF_SIGNING_KEY_2026_11")
    error_message = "every calling service mounts FF_SIGNING_KEY_<KID> and FF_SIGNING_KID (G-OPS-12)."
  }

  assert {
    condition     = alltrue([for s in ["gateway", "engine", "scope", "tool-runner"] : contains(keys(local.services[s].secret_env), "FF_REDIS_CA")])
    error_message = "Memorystore TLS: FF_REDIS_CA is mounted where Redis is used (G-OPS-13)."
  }

  assert {
    condition     = alltrue([for s, v in local.services : v.env["FF_BUS"] == "pubsub"])
    error_message = "every service uses the Pub/Sub bus."
  }

  assert {
    condition     = length(google_cloud_scheduler_job.tick) == 1 && endswith(google_cloud_scheduler_job.tick[0].http_target[0].uri, "/internal/schedules:tick")
    error_message = "one schedules tick job."
  }

  assert {
    condition     = contains(keys(local.services["gateway"].env), "FF_TOOL_RUNNER_ADDR")
    error_message = "the gateway requires FF_TOOL_RUNNER_ADDR."
  }

  assert {
    condition     = length(module.budget_shutdown) == 0
    error_message = "budget shutdown is off by default."
  }

  assert {
    condition     = !contains(keys(module.svc), "egress-proxy") && alltrue([for s in ["tool-runner", "notifier", "engine"] : local.services[s].env["FF_EGRESS_PROXY"] == "http://10.10.5.2:3128"])
    error_message = "the egress proxy runs on the VM's internal address, not on Cloud Run (G-OPS-15, G-SVC-14)."
  }

  assert {
    condition     = module.egress.socks_addr == "10.10.5.2:1080" && module.egress.dns_addr == "10.10.5.2:8053"
    error_message = "the egress VM advertises SOCKS5 and DNS on its internal address."
  }

  assert {
    condition     = local.services["content"].env["FF_CONTENT_BUCKET"] == "${var.project_id}-ff-content" && contains(keys(local.services["content"].secret_env), "FF_CONTENT_KEK") && local.services["content"].secret_env["PGPASSWORD"].secret == "ff-db-content-password"
    error_message = "content: chunk bucket, KEK secret and its own database user (G-OPS-16, G-CST-01)."
  }

  assert {
    condition     = var.scaling["content"].max == 1 && contains(local.db_users, "content") && local.services["content"].sql
    error_message = "content runs one instance (G-CST-03) as ff_content (migration 0031)."
  }

  assert {
    condition     = local.services["gateway"].env["FF_CONTENT_ADDR"] == "https://ff-content-123456789012.us-central1.run.app" && local.services["tool-runner"].env["FF_CONTENT_ADDR"] == local.services["gateway"].env["FF_CONTENT_ADDR"]
    error_message = "the gateway and the tool runner reach the content service."
  }

  assert {
    condition     = contains(keys(module.topic), "content.changes") && module.content_bucket.name == "${var.project_id}-ff-content"
    error_message = "content.changes is a topic with its subscriptions, and the chunk bucket exists."
  }
}
