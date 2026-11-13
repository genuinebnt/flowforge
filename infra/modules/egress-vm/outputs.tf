output "address" {
  description = "Internal IP of the proxy."
  value       = google_compute_address.this.address
}

output "proxy_url" {
  description = "FF_EGRESS_PROXY for the tool-runner and the notifier."
  value       = "http://${google_compute_address.this.address}:${var.proxy_port}"
}

output "socks_addr" {
  description = "SOCKS5 host:port advertised in tickets."
  value       = local.socks_addr
}

output "dns_addr" {
  description = "DNS host:port advertised in tickets."
  value       = local.dns_addr
}

output "instance_name" {
  description = "VM name (make gcp-pause / gcp-resume, budget shutdown, deploy rollout)."
  value       = google_compute_instance.this.name
}

output "zone" {
  description = "VM zone."
  value       = google_compute_instance.this.zone
}

output "service_account_email" {
  description = "Runtime service account email."
  value       = google_service_account.this.email
}

output "service_account_member" {
  description = "serviceAccount:<email>, for Pub/Sub and Cloud Run invoker grants."
  value       = google_service_account.this.member
}

output "service_account_id" {
  description = "Service account resource name (for roles/iam.serviceAccountUser grants)."
  value       = google_service_account.this.name
}

output "secret_env_names" {
  description = "Environment variables the VM reads from Secret Manager (names only)."
  value       = sort(keys(var.secret_env))
}
