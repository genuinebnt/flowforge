output "instance_name" {
  description = "Instance name."
  value       = google_sql_database_instance.this.name
}

output "connection_name" {
  description = "PROJECT:REGION:INSTANCE, mounted by Cloud Run at /cloudsql/<connection_name>."
  value       = google_sql_database_instance.this.connection_name
}

output "private_ip" {
  description = "Private IP address."
  value       = google_sql_database_instance.this.private_ip_address
}

output "database_name" {
  description = "Database name."
  value       = google_sql_database.this.name
}

output "user_names" {
  description = "service => database user name."
  value       = { for k, u in google_sql_user.users : k => u.name }
}

output "user_passwords" {
  description = "service => generated password (store in Secret Manager only)."
  value       = { for k, p in random_password.users : k => p.result }
  sensitive   = true
}
