output "name" {
  description = "Cloud Run service name."
  value       = google_cloud_run_v2_service.this.name
}

output "id" {
  description = "Cloud Run service id."
  value       = google_cloud_run_v2_service.this.id
}

output "uri" {
  description = "Service URL (run.app)."
  value       = google_cloud_run_v2_service.this.uri
}

output "service_account_email" {
  description = "Runtime service account email."
  value       = google_service_account.this.email
}

output "service_account_member" {
  description = "Runtime service account as an IAM member string."
  value       = google_service_account.this.member
}

output "service_account_id" {
  description = "Runtime service account resource name (for iam.serviceAccountUser grants)."
  value       = google_service_account.this.name
}
