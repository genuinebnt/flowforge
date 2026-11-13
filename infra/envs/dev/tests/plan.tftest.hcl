# Offline plan of the dev root with mocked providers (no GCP credentials):
#   terraform -chdir=infra/envs/dev init -backend=false && terraform -chdir=infra/envs/dev test
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
}

mock_provider "random" {}

run "plan" {
  command = plan

  assert {
    condition     = alltrue([for s in ["gateway", "engine", "tool-runner", "egress-proxy", "graph", "similarity"] : contains(keys(local.services[s].secret_env), "FF_SIGNING_KEY_2026_11") && local.services[s].env["FF_SIGNING_KID"] == var.current_signing_kid])
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
}
