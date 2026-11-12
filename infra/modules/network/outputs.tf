output "network_id" {
  description = "VPC id (projects/<p>/global/networks/<n>)."
  value       = google_compute_network.vpc.id
}

output "network_name" {
  description = "VPC name."
  value       = google_compute_network.vpc.name
}

output "services_subnet_id" {
  description = "Subnet for regular Cloud Run services."
  value       = google_compute_subnetwork.services.id
}

output "tools_subnet_id" {
  description = "Subnet for the tool-runner."
  value       = google_compute_subnetwork.tools.id
}

output "egress_subnet_id" {
  description = "Subnet for the egress proxy (NATed)."
  value       = google_compute_subnetwork.egress.id
}

output "psa_range_name" {
  description = "Name of the private services access range."
  value       = google_compute_global_address.psa.name
}

output "psa_connection" {
  description = "Service networking connection; depend on it before creating private-IP services."
  value       = google_service_networking_connection.psa.id
}

output "nat_ips" {
  description = "Static egress IPs that programs can allowlist."
  value       = google_compute_address.nat[*].address
}

output "tool_runner_tag" {
  description = "Network tag for the tool-runner."
  value       = var.tool_runner_tag
}

output "egress_proxy_tag" {
  description = "Network tag for the egress proxy."
  value       = var.egress_proxy_tag
}
