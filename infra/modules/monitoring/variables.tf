variable "project_id" {
  description = "GCP project id."
  type        = string
}

variable "project_number" {
  description = "Project number (budget filter)."
  type        = string
}

variable "env" {
  description = "dev or prod (matches the env field of log lines and the env metric label)."
  type        = string
}

variable "notification_emails" {
  description = "On-call email addresses for alerts and budget notifications."
  type        = list(string)
}

variable "billing_account" {
  description = "Billing account id (XXXXXX-XXXXXX-XXXXXX) for the budget."
  type        = string
}

variable "monthly_budget" {
  description = "Monthly budget in budget_currency units."
  type        = number
}

variable "budget_currency" {
  description = "Budget currency code."
  type        = string
  default     = "USD"
}

variable "budget_thresholds" {
  description = "Actual-spend alert thresholds (spec 17.6: 50/80/100 %)."
  type        = list(number)
  default     = [0.5, 0.8, 1.0]
}

# spec 18.1 SLO candidates.
variable "slo_scope_check_p99_seconds" {
  description = "Scope check p99 target."
  type        = number
  default     = 0.1
}

variable "slo_submit_to_start_p95_seconds" {
  description = "Run submit-to-start p95 target."
  type        = number
  default     = 5
}

variable "slo_graph_ingest_p95_seconds" {
  description = "Graph ingest p95 target per observation."
  type        = number
  default     = 1
}

variable "slo_outbox_lag_p99_seconds" {
  description = "Outbox lag p99 target."
  type        = number
  default     = 10
}

# Metric names as exported by the services' OpenTelemetry Prometheus
# exporters (histograms). ff_graph_ingest_seconds and ff_outbox_lag_seconds
# are named in spec 18.1; the other two are Flowforge-chosen names.
variable "metric_scope_check_seconds" {
  description = "Histogram of scope check latency."
  type        = string
  default     = "ff_scope_check_seconds"
}

variable "metric_submit_to_start_seconds" {
  description = "Histogram of run submit-to-start latency."
  type        = string
  default     = "ff_run_submit_to_start_seconds"
}

variable "metric_graph_ingest_seconds" {
  description = "Histogram of graph ingest latency per observation."
  type        = string
  default     = "ff_graph_ingest_seconds"
}

variable "metric_outbox_lag_seconds" {
  description = "Histogram of outbox lag."
  type        = string
  default     = "ff_outbox_lag_seconds"
}

variable "scope_decision_msg" {
  description = "msg field of the scope service's per-decision log line."
  type        = string
  default     = "scope.decision"
}

variable "approval_msg" {
  description = "msg field of the engine's approval state-change log line."
  type        = string
  default     = "approval.changed"
}
