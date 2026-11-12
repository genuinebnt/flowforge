output "name" {
  description = "Bucket name (FF_ARTIFACT_BUCKET)."
  value       = google_storage_bucket.this.name
}

output "url" {
  description = "gs:// URL."
  value       = google_storage_bucket.this.url
}
