output "ip_address" {
  description = "Global anycast IP; point the API domain's A record here."
  value       = google_compute_global_address.edge.address
}

output "security_policy" {
  description = "Cloud Armor policy name."
  value       = google_compute_security_policy.edge.name
}

output "backend_service" {
  description = "Backend service name."
  value       = google_compute_backend_service.gateway.name
}
