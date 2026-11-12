# Flowforge dev. Replace the placeholders (project, billing account, domain,
# emails) before the first apply. Image digests are overwritten by the deploy
# workflow; the zero digests below only satisfy validation for plan.

project_id = "flowforge-dev"
region     = "us-central1"
env        = "dev"
domain     = "api.dev.flowforge.example"
jwt_issuer = "https://api.dev.flowforge.example"

deletion_protection = false

images = {
  gateway        = "us-central1-docker.pkg.dev/flowforge-dev/flowforge/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  engine         = "us-central1-docker.pkg.dev/flowforge-dev/flowforge/engine@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  scope          = "us-central1-docker.pkg.dev/flowforge-dev/flowforge/scope@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  graph          = "us-central1-docker.pkg.dev/flowforge-dev/flowforge/graph@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  notifier       = "us-central1-docker.pkg.dev/flowforge-dev/flowforge/notifier@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  similarity     = "us-central1-docker.pkg.dev/flowforge-dev/flowforge/similarity@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  "tool-runner"  = "us-central1-docker.pkg.dev/flowforge-dev/flowforge/tool-runner@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  "egress-proxy" = "us-central1-docker.pkg.dev/flowforge-dev/flowforge/egress-proxy@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  migrate        = "us-central1-docker.pkg.dev/flowforge-dev/flowforge/migrate@sha256:0000000000000000000000000000000000000000000000000000000000000000"
}

# spec 17.1: concurrency 80 (gateway), 20 (engine, graph); min instances 1
# for engine and scope, 0 elsewhere in dev. Max instances are the cost cap.
scaling = {
  gateway        = { concurrency = 80, min = 0, max = 3 }
  engine         = { concurrency = 20, min = 1, max = 3 }
  scope          = { concurrency = 80, min = 1, max = 3 }
  graph          = { concurrency = 20, min = 0, max = 3 }
  notifier       = { concurrency = 80, min = 0, max = 2 }
  similarity     = { concurrency = 40, min = 0, max = 2, memory = "1Gi" }
  "tool-runner"  = { concurrency = 10, min = 0, max = 3, memory = "1Gi" }
  "egress-proxy" = { concurrency = 80, min = 0, max = 2 }
}

nat_ip_count = 1

db_tier              = "db-custom-1-3840"
db_high_availability = false
redis_tier           = "BASIC"
redis_memory_gb      = 1

topic_prefix = "ff."

signing_key_kids    = ["2026-11"]
current_signing_kid = "2026-11"

llm_api_key_enabled = true
llm_endpoint        = "https://llm.example.invalid/v1"
llm_model           = "planner-default"

webhook_channels = ["slack", "email"]
tool_secrets     = []

schedules = {}

edge_rate_limit_per_minute = 300

github_repository  = "genuinebnt/flowforge"
github_environment = null

alert_emails    = ["oncall-dev@flowforge.example"]
billing_account = "000000-000000-000000"
monthly_budget  = 300
budget_currency = "USD"
