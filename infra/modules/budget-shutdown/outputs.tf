output "topic_id" {
  description = "Budget notification topic (pass to the monitoring module's budget_pubsub_topic)."
  value       = google_pubsub_topic.budget.id
}

output "service_name" {
  description = "Handler Cloud Run service."
  value       = google_cloud_run_v2_service.handler.name
}

output "service_account_email" {
  description = "Handler identity."
  value       = google_service_account.handler.email
}
