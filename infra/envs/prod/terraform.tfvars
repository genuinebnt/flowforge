# Flowforge prod. Replace the placeholders (project, billing account, domain,
# emails) before the first apply. Image digests are overwritten by the deploy
# workflow; the zero digests below only satisfy validation for plan.

project_id = "flowforge-prod"
region     = "us-central1"
env        = "prod"
domain     = "api.flowforge.example"
jwt_issuer = "https://api.flowforge.example"

deletion_protection = true

images = {
  gateway        = "us-central1-docker.pkg.dev/flowforge-prod/flowforge/gateway@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  engine         = "us-central1-docker.pkg.dev/flowforge-prod/flowforge/engine@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  scope          = "us-central1-docker.pkg.dev/flowforge-prod/flowforge/scope@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  graph          = "us-central1-docker.pkg.dev/flowforge-prod/flowforge/graph@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  notifier       = "us-central1-docker.pkg.dev/flowforge-prod/flowforge/notifier@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  similarity     = "us-central1-docker.pkg.dev/flowforge-prod/flowforge/similarity@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  "tool-runner"  = "us-central1-docker.pkg.dev/flowforge-prod/flowforge/tool-runner@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  content        = "us-central1-docker.pkg.dev/flowforge-prod/flowforge/content@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  "egress-proxy" = "us-central1-docker.pkg.dev/flowforge-prod/flowforge/egress-proxy@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  migrate        = "us-central1-docker.pkg.dev/flowforge-prod/flowforge/migrate@sha256:0000000000000000000000000000000000000000000000000000000000000000"
}

# spec 17.1 concurrency and min instances; max instances are the cost cap
# (spec 17.6). Prod keeps a warm gateway. content stays at one instance
# (G-CST-03). The egress proxy is a VM (egress_machine_type).
scaling = {
  gateway       = { concurrency = 80, min = 1, max = 20 }
  engine        = { concurrency = 20, min = 1, max = 20, cpu = "2", memory = "1Gi" }
  scope         = { concurrency = 80, min = 1, max = 10 }
  graph         = { concurrency = 20, min = 1, max = 20, memory = "1Gi" }
  notifier      = { concurrency = 80, min = 1, max = 5 }
  similarity    = { concurrency = 40, min = 1, max = 10, cpu = "2", memory = "2Gi" }
  "tool-runner" = { concurrency = 10, min = 0, max = 20, cpu = "2", memory = "2Gi" }
  content       = { concurrency = 20, min = 1, max = 1, cpu = "2", memory = "2Gi" }
}

nat_ip_count = 2

# Smallest HA tier until load justifies more (spec 17.6).
db_tier              = "db-custom-1-3840"
db_high_availability = true
redis_tier           = "STANDARD_HA"
redis_memory_gb      = 1

topic_prefix = "ff."

signing_key_kids    = ["2026-11", "2027-02"]
current_signing_kid = "2026-11"

llm_api_key_enabled = true
llm_endpoint        = "https://llm.example.invalid/v1"
llm_model           = "planner-default"

webhook_channels = ["slack", "email"]
tool_secrets     = []

schedules = {}

edge_rate_limit_per_minute = 600

github_repository  = "genuinebnt/flowforge"
github_environment = "prod"

alert_emails    = ["oncall@flowforge.example"]
billing_account = "000000-000000-000000"
monthly_budget  = 2000
budget_currency = "USD"
