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
  value       = [google_logging_metric.scope_denials.name, google_logging_metric.approvals.name]
}
