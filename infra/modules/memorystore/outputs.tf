output "host" {
  description = "Private IP of the primary endpoint."
  value       = google_redis_instance.this.host
}

output "port" {
  description = "Port (6378 when TLS is enabled)."
  value       = google_redis_instance.this.port
}

output "address" {
  description = "host:port, used as FF_REDIS_ADDR."
  value       = "${google_redis_instance.this.host}:${google_redis_instance.this.port}"
}

output "auth_string" {
  description = "AUTH string; store it in Secret Manager (ff-redis-auth) only."
  value       = google_redis_instance.this.auth_string
  sensitive   = true
}

output "server_ca_pem" {
  description = "PEM bundle of the server CA certificates for TLS verification."
  value       = join("\n", [for c in google_redis_instance.this.server_ca_certs : c.cert])
}
