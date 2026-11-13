output "notification_channels" {
  description = "Notification channel ids."
  value       = local.channels
}

output "slo_alert_policies" {
  description = "SLO name => alert policy id."
  value       = { for k, p in google_monitoring_alert_policy.slo : k => p.id }
}

output "log_metrics" {
  description = "Log-based metric names."
  value       = concat(google_logging_metric.scope_denials[*].name, google_logging_metric.approvals[*].name)
}

output "budget_name" {
  description = "Billing budget resource name."
  value       = google_billing_budget.monthly.name
}
