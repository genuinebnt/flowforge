# Offline plan of the lab root with mocked providers (no GCP credentials):
#   terraform -chdir=infra/envs/lab init -backend=false && terraform -chdir=infra/envs/lab test
# Uses terraform.tfvars; each run overrides what it checks.

mock_provider "google" {
  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }
  mock_data "google_compute_image" {
    defaults = {
      self_link = "https://www.googleapis.com/compute/v1/projects/cos-cloud/global/images/cos-stable-0"
    }
  }
  mock_resource "google_service_account" {
    defaults = {
      member = "serviceAccount:mock@flowforge-lab.iam.gserviceaccount.com"
      email  = "mock@flowforge-lab.iam.gserviceaccount.com"
      name   = "projects/flowforge-lab/serviceAccounts/mock@flowforge-lab.iam.gserviceaccount.com"
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

run "defaults_plan" {
  command = plan

  assert {
    condition     = alltrue([for s, m in module.svc : m.name == "ff-${s}"]) && length(module.svc) == 7
    error_message = "lab runs seven Cloud Run services (the egress proxy is on the VM)."
  }

  assert {
    condition     = google_compute_instance.vm.machine_type == "e2-micro"
    error_message = "the lab VM is an e2-micro."
  }

  assert {
    condition     = length(local.secret_names) <= 6
    error_message = "lab secrets exceed the free active-version allowance."
  }

  assert {
    condition     = local.services["gateway"].env["FF_DB_DSN"] == "host=10.10.5.2 port=5432 dbname=flowforge user=flowforge sslmode=prefer"
    error_message = "the services connect to Postgres on the VM."
  }

  assert {
    condition     = local.services["engine"].env["FF_EGRESS_PROXY"] == "http://10.10.5.2:3128" && local.services["engine"].env["FF_BUS"] == "pubsub"
    error_message = "the engine uses the VM's proxy and the Pub/Sub bus."
  }

  assert {
    condition     = contains(keys(local.services["engine"].secret_env), "FF_SIGNING_KEY_2026_11") && local.services["engine"].env["FF_SIGNING_KID"] == "2026-11"
    error_message = "signing keys are mounted as FF_SIGNING_KEY_<KID> with FF_SIGNING_KID."
  }

  assert {
    condition     = !contains(keys(local.services["engine"].env), "FF_REDIS_ADDR")
    error_message = "Redis is off by default."
  }

  assert {
    condition     = google_cloud_scheduler_job.tick.http_target[0].uri == "https://ff-engine-123456789012.us-central1.run.app/internal/schedules:tick"
    error_message = "one tick job posts /internal/schedules:tick on the engine."
  }

  assert {
    condition     = length(module.budget_shutdown) == 0
    error_message = "budget shutdown is off by default."
  }
}

run "redis_and_shutdown" {
  command = plan

  variables {
    redis_enabled           = true
    budget_shutdown_enabled = true
    budget_shutdown_image   = "gcr.io/google.com/cloudsdktool/google-cloud-cli@sha256:1111111111111111111111111111111111111111111111111111111111111111"
  }

  assert {
    condition     = local.services["scope"].env["FF_REDIS_ADDR"] == "10.10.5.2:6379" && !contains(keys(local.services["scope"].env), "FF_REDIS_CA")
    error_message = "lab Redis is plaintext on the private network (no FF_REDIS_CA)."
  }

  assert {
    condition     = length(module.budget_shutdown) == 1
    error_message = "budget shutdown is created when enabled."
  }
}

run "min_instances_refused" {
  command = plan

  variables {
    scaling = {
      gateway       = { concurrency = 80, max = 2 }
      engine        = { concurrency = 20, min = 1, max = 1 }
      scope         = { concurrency = 80, max = 1 }
      graph         = { concurrency = 20, max = 1 }
      notifier      = { concurrency = 80, max = 1 }
      similarity    = { concurrency = 40, max = 1 }
      "tool-runner" = { concurrency = 10, max = 1 }
    }
  }

  expect_failures = [terraform_data.guardrails]
}

run "bigger_vm_refused" {
  command = plan

  variables {
    vm_machine_type = "e2-small"
  }

  expect_failures = [terraform_data.guardrails]
}

run "disk_over_free_tier_refused" {
  command = plan

  variables {
    vm_data_disk_gb = 40
  }

  expect_failures = [terraform_data.guardrails]
}

run "non_free_tier_region_refused" {
  command = plan

  variables {
    region = "europe-west1"
  }

  expect_failures = [var.region]
}

run "bigger_vm_allowed_explicitly" {
  command = plan

  variables {
    vm_machine_type = "e2-small"
    allow_billable  = ["vm_size"]
  }

  assert {
    condition     = google_compute_instance.vm.machine_type == "e2-small"
    error_message = "allow_billable lifts the guardrail."
  }
}
