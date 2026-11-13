# Flowforge lab: the near-zero-cost learning environment. Replace the
# placeholders (project, billing account, emails, repository) before the
# first apply; infra/README.md "Bootstrap the lab" has the order. Image
# digests are written by infra/scripts/push-images.sh (images.auto.tfvars.json)
# and afterwards rolled forward by the deploy workflow; the zero digests
# below only satisfy validation for plan.

project_id = "flowforge-lab"
region     = "us-central1" # free tier: us-west1, us-central1 or us-east1

images = {
  gateway        = "us-central1-docker.pkg.dev/flowforge-lab/flowforge/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  engine         = "us-central1-docker.pkg.dev/flowforge-lab/flowforge/engine@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  scope          = "us-central1-docker.pkg.dev/flowforge-lab/flowforge/scope@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  graph          = "us-central1-docker.pkg.dev/flowforge-lab/flowforge/graph@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  notifier       = "us-central1-docker.pkg.dev/flowforge-lab/flowforge/notifier@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  similarity     = "us-central1-docker.pkg.dev/flowforge-lab/flowforge/similarity@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  "tool-runner"  = "us-central1-docker.pkg.dev/flowforge-lab/flowforge/tool-runner@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  content        = "us-central1-docker.pkg.dev/flowforge-lab/flowforge/content@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  "egress-proxy" = "us-central1-docker.pkg.dev/flowforge-lab/flowforge/egress-proxy@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  migrate        = "us-central1-docker.pkg.dev/flowforge-lab/flowforge/migrate@sha256:0000000000000000000000000000000000000000000000000000000000000000"
}

# Everything scales to zero; max instances are the cost cap. CPU is
# allocated only during requests (request-based billing), so background
# loops (outbox relays, Pub/Sub pull consumers) progress while an instance
# serves requests, e.g. the per-minute schedules tick on the engine. Set
# cpu_always = true (and allow_billable = ["cpu_always"]) to keep them
# running continuously, which leaves the free tier.
scaling = {
  gateway       = { concurrency = 80, max = 2 }
  engine        = { concurrency = 20, max = 1 }
  scope         = { concurrency = 80, max = 1 }
  graph         = { concurrency = 20, max = 1 }
  notifier      = { concurrency = 80, max = 1 }
  similarity    = { concurrency = 40, max = 1, memory = "1Gi" }
  "tool-runner" = { concurrency = 10, max = 1, memory = "1Gi" }
  content       = { concurrency = 20, max = 1, memory = "1Gi" } # one writer (G-CST-03)
}

# e2-micro, 10 GB boot + 20 GB data, pd-standard: the Compute Engine free tier.
vm_machine_type = "e2-micro"
vm_boot_disk_gb = 10
vm_data_disk_gb = 20
redis_enabled   = false

topic_prefix = "ff."

# One signing key: secrets = ff-signing-key-2026-11 + ff-db-password +
# ff-content-kek = 3 of the 6 free active versions (redis_enabled,
# llm_api_key_enabled, webhooks and tool secrets each add one).
signing_key_kids    = ["2026-11"]
current_signing_kid = "2026-11"

llm_api_key_enabled = false
llm_endpoint        = ""
llm_model           = ""

webhook_channels = []
tool_secrets     = []

github_repository  = "genuinebnt/flowforge"
github_environment = "lab"

alert_emails    = ["you@flowforge.example"]
billing_account = "000000-000000-000000"
monthly_budget  = 5
budget_currency = "USD"

# Budget-triggered shutdown (off): set true and pin budget_shutdown_image to
# a google-cloud-cli digest (infra/modules/budget-shutdown/README.md).
budget_shutdown_enabled = false

allow_billable = []
