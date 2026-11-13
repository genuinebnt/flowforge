# Observability and cost guardrails (spec 17.1 observability row, 17.6,
# 18.1): SLO alert policies over the OpenTelemetry metrics (exported to
# Google Cloud Managed Service for Prometheus, hence PromQL), log-based
# metrics for scope denials and approvals, a dashboard, and budget alerts at
# 50/80/100 % of the monthly budget.

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
  log_env  = coalesce(var.log_env, var.env)
  channels = [for c in google_monitoring_notification_channel.email : c.id]

  # spec 18.1 SLO candidates.
  slos = {
    scope_check_p99 = {
      display = "Scope check p99 > ${var.slo_scope_check_p99_seconds * 1000} ms"
      expr    = "histogram_quantile(0.99, sum by (le) (rate(${var.metric_scope_check_seconds}_bucket[5m])))"
      limit   = var.slo_scope_check_p99_seconds
      doc     = "Scope checks (spec 7, 18.1) are slower than 100 ms at p99. Check scope service latency, Cloud SQL and Redis."
    }
    run_submit_to_start_p95 = {
      display = "Run submit-to-start p95 > ${var.slo_submit_to_start_p95_seconds} s"
      expr    = "histogram_quantile(0.95, sum by (le) (rate(${var.metric_submit_to_start_seconds}_bucket[5m])))"
      limit   = var.slo_submit_to_start_p95_seconds
      doc     = "Runs take more than 5 s from submission to first effect at p95. Check engine instances, worker leases and the scheduler."
    }
    graph_ingest_p95 = {
      display = "Graph ingest p95 > ${var.slo_graph_ingest_p95_seconds} s per observation"
      expr    = "histogram_quantile(0.95, sum by (le) (rate(${var.metric_graph_ingest_seconds}_bucket[5m])))"
      limit   = var.slo_graph_ingest_p95_seconds
      doc     = "Graph ingestion exceeds 1 s per observation at p95. Check graph service and Cloud SQL."
    }
    outbox_lag_p99 = {
      display = "Outbox lag p99 > ${var.slo_outbox_lag_p99_seconds} s"
      expr    = "histogram_quantile(0.99, sum by (le) (rate(${var.metric_outbox_lag_seconds}_bucket[5m])))"
      limit   = var.slo_outbox_lag_p99_seconds
      doc     = "Outbox relays lag more than 10 s at p99. Check Pub/Sub publish errors and relay loops (CPU always allocated)."
    }
  }
}

resource "google_monitoring_notification_channel" "email" {
  for_each     = toset(var.notification_emails)
  project      = var.project_id
  display_name = "Flowforge ${var.env} ${each.value}"
  type         = "email"
  labels = {
    email_address = each.value
  }
}

resource "google_monitoring_alert_policy" "slo" {
  for_each     = var.alert_policies_enabled ? local.slos : {}
  project      = var.project_id
  display_name = "[${var.env}] ${each.value.display}"
  combiner     = "OR"
  severity     = var.env == "prod" ? "ERROR" : "WARNING"

  conditions {
    display_name = each.value.display
    condition_prometheus_query_language {
      query               = "${each.value.expr} > ${each.value.limit}"
      duration            = "300s"
      evaluation_interval = "60s"
    }
  }

  documentation {
    content   = each.value.doc
    mime_type = "text/markdown"
  }

  alert_strategy {
    auto_close = "1800s"
  }

  notification_channels = local.channels
  user_labels = {
    env = var.env
    slo = each.key
  }
}

# Undeliverable events pile up in *.dead-letter.ops (spec 11.7).
resource "google_monitoring_alert_policy" "dead_letters" {
  count        = var.alert_policies_enabled ? 1 : 0
  project      = var.project_id
  display_name = "[${var.env}] Pub/Sub dead letters present"
  combiner     = "OR"
  severity     = "WARNING"

  conditions {
    display_name = "Undelivered dead letters"
    condition_threshold {
      filter          = "resource.type = \"pubsub_subscription\" AND resource.labels.subscription_id = monitoring.regex.full_match(\".*\\\\.dead-letter\\\\.ops\") AND metric.type = \"pubsub.googleapis.com/subscription/num_undelivered_messages\""
      comparison      = "COMPARISON_GT"
      threshold_value = 0
      duration        = "300s"
      aggregations {
        alignment_period   = "300s"
        per_series_aligner = "ALIGN_MAX"
      }
    }
  }

  documentation {
    content   = "Events failed delivery repeatedly and were dead-lettered. Inspect the .dead-letter.ops subscription; messages are kept 14 days."
    mime_type = "text/markdown"
  }

  notification_channels = local.channels
  user_labels = {
    env = var.env
  }
}

# --- Log-based metrics (spec 17.1 observability row). Logs are JSON lines
# with the 18.1 field set; scope and engine log one line per decision.

resource "google_logging_metric" "scope_denials" {
  count   = var.log_metrics_enabled ? 1 : 0
  project = var.project_id
  name    = "ff_scope_denials"
  filter  = "resource.type=\"cloud_run_revision\" AND jsonPayload.env=\"${local.log_env}\" AND jsonPayload.service=\"scope\" AND jsonPayload.msg=\"${var.scope_decision_msg}\" AND jsonPayload.decision=\"deny\""

  metric_descriptor {
    metric_kind  = "DELTA"
    value_type   = "INT64"
    unit         = "1"
    display_name = "Flowforge scope denials"
    labels {
      key         = "reason"
      value_type  = "STRING"
      description = "Denial reason code"
    }
    labels {
      key         = "tenant_id"
      value_type  = "STRING"
      description = "Tenant"
    }
  }

  label_extractors = {
    reason    = "EXTRACT(jsonPayload.reason)"
    tenant_id = "EXTRACT(jsonPayload.tenant_id)"
  }
}

resource "google_logging_metric" "approvals" {
  count   = var.log_metrics_enabled ? 1 : 0
  project = var.project_id
  name    = "ff_approvals"
  filter  = "resource.type=\"cloud_run_revision\" AND jsonPayload.env=\"${local.log_env}\" AND jsonPayload.service=\"engine\" AND jsonPayload.msg=\"${var.approval_msg}\""

  metric_descriptor {
    metric_kind  = "DELTA"
    value_type   = "INT64"
    unit         = "1"
    display_name = "Flowforge approvals"
    labels {
      key         = "status"
      value_type  = "STRING"
      description = "requested, approved, rejected or expired"
    }
    labels {
      key         = "role"
      value_type  = "STRING"
      description = "Approver role"
    }
  }

  label_extractors = {
    status = "EXTRACT(jsonPayload.status)"
    role   = "EXTRACT(jsonPayload.role)"
  }
}

resource "google_monitoring_dashboard" "overview" {
  project = var.project_id
  dashboard_json = jsonencode({
    displayName = "Flowforge ${var.env}"
    mosaicLayout = {
      columns = 12
      tiles = concat(
        [for i, k in keys(local.slos) : {
          xPos   = (i % 2) * 6
          yPos   = floor(i / 2) * 4
          width  = 6
          height = 4
          widget = {
            title = local.slos[k].display
            xyChart = {
              dataSets = [{
                plotType        = "LINE"
                timeSeriesQuery = { prometheusQuery = local.slos[k].expr }
              }]
            }
          }
        }],
        var.log_metrics_enabled ? [
          {
            xPos   = 0
            yPos   = 8
            width  = 6
            height = 4
            widget = {
              title = "Scope denials by reason"
              xyChart = {
                dataSets = [{
                  plotType = "STACKED_BAR"
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.scope_denials[0].name}\" resource.type=\"cloud_run_revision\""
                      aggregation = {
                        alignmentPeriod    = "300s"
                        perSeriesAligner   = "ALIGN_SUM"
                        crossSeriesReducer = "REDUCE_SUM"
                        groupByFields      = ["metric.label.reason"]
                      }
                    }
                  }
                }]
              }
            }
          },
          {
            xPos   = 6
            yPos   = 8
            width  = 6
            height = 4
            widget = {
              title = "Approvals by status"
              xyChart = {
                dataSets = [{
                  plotType = "STACKED_BAR"
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.approvals[0].name}\" resource.type=\"cloud_run_revision\""
                      aggregation = {
                        alignmentPeriod    = "300s"
                        perSeriesAligner   = "ALIGN_SUM"
                        crossSeriesReducer = "REDUCE_SUM"
                        groupByFields      = ["metric.label.status"]
                      }
                    }
                  }
                }]
              }
            }
          },
        ] : [],
      )
    }
  })
}

# --- Budget alerts at 50 %, 80 % and 100 % (spec 17.6).

resource "google_billing_budget" "monthly" {
  billing_account = var.billing_account
  display_name    = "Flowforge ${var.env} monthly"

  budget_filter {
    projects               = ["projects/${var.project_number}"]
    calendar_period        = "MONTH"
    credit_types_treatment = "INCLUDE_ALL_CREDITS"
  }

  amount {
    specified_amount {
      currency_code = var.budget_currency
      units         = tostring(var.monthly_budget)
    }
  }

  dynamic "threshold_rules" {
    for_each = var.budget_thresholds
    content {
      threshold_percent = threshold_rules.value
      spend_basis       = "CURRENT_SPEND"
    }
  }

  # Forecast warning ahead of the 100 % actual threshold.
  threshold_rules {
    threshold_percent = 1.0
    spend_basis       = "FORECASTED_SPEND"
  }

  # Every budget update (several a day) also goes to budget_pubsub_topic
  # when set; the budget-shutdown module consumes it.
  all_updates_rule {
    monitoring_notification_channels = local.channels
    disable_default_iam_recipients   = false
    pubsub_topic                     = var.budget_pubsub_topic
    schema_version                   = var.budget_pubsub_topic == null ? null : "1.0"
  }
}
