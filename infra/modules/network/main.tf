# Network for one Flowforge environment (spec 17.3): one VPC, Direct VPC
# egress subnets for Cloud Run, private services access for Cloud SQL and
# Memorystore, Cloud NAT with reserved static IPs used only by the egress
# proxy subnet, private DNS for Google APIs and run.app, and firewall rules
# that deny the tool-runner any direct path out.

terraform {
  required_version = ">= 1.6"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}

locals {
  # private.googleapis.com VIPs (Private Google Access without public IPs).
  private_googleapis_ips  = ["199.36.153.8", "199.36.153.9", "199.36.153.10", "199.36.153.11"]
  private_googleapis_cidr = "199.36.153.8/30"
}

resource "google_compute_network" "vpc" {
  project                         = var.project_id
  name                            = "${var.name}-vpc"
  auto_create_subnetworks         = false
  routing_mode                    = "REGIONAL"
  delete_default_routes_on_create = false
}

# Cloud Run services other than the tool-runner and the egress proxy.
resource "google_compute_subnetwork" "services" {
  project                  = var.project_id
  name                     = "${var.name}-services"
  region                   = var.region
  network                  = google_compute_network.vpc.id
  ip_cidr_range            = var.services_cidr
  private_ip_google_access = true

  dynamic "log_config" {
    for_each = var.flow_logs_enabled ? [1] : []
    content {
      aggregation_interval = "INTERVAL_5_MIN"
      flow_sampling        = 0.1
      metadata             = "INCLUDE_ALL_METADATA"
    }
  }
}

# The tool-runner gets its own subnet so firewall policy and NAT exclusion
# are unambiguous.
resource "google_compute_subnetwork" "tools" {
  project                  = var.project_id
  name                     = "${var.name}-tools"
  region                   = var.region
  network                  = google_compute_network.vpc.id
  ip_cidr_range            = var.tools_cidr
  private_ip_google_access = true

  dynamic "log_config" {
    for_each = var.flow_logs_enabled ? [1] : []
    content {
      aggregation_interval = "INTERVAL_5_MIN"
      flow_sampling        = 0.5
      metadata             = "INCLUDE_ALL_METADATA"
    }
  }
}

# The only subnet that Cloud NAT translates: the egress proxy lives here.
resource "google_compute_subnetwork" "egress" {
  project                  = var.project_id
  name                     = "${var.name}-egress"
  region                   = var.region
  network                  = google_compute_network.vpc.id
  ip_cidr_range            = var.egress_cidr
  private_ip_google_access = true

  dynamic "log_config" {
    for_each = var.flow_logs_enabled ? [1] : []
    content {
      aggregation_interval = "INTERVAL_5_MIN"
      flow_sampling        = 0.5
      metadata             = "INCLUDE_ALL_METADATA"
    }
  }
}

# Private services access (Cloud SQL private IP, Memorystore).
resource "google_compute_global_address" "psa" {
  count         = var.psa_enabled ? 1 : 0
  project       = var.project_id
  name          = "${var.name}-psa"
  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  address       = split("/", var.psa_cidr)[0]
  prefix_length = tonumber(split("/", var.psa_cidr)[1])
  network       = google_compute_network.vpc.id
}

resource "google_service_networking_connection" "psa" {
  count                   = var.psa_enabled ? 1 : 0
  network                 = google_compute_network.vpc.id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.psa[0].name]
}

# Reserved static egress IPs so programs can allowlist Flowforge's source
# addresses (spec 17.1, egress proxy row).
resource "google_compute_address" "nat" {
  count        = var.nat_enabled ? var.nat_ip_count : 0
  project      = var.project_id
  name         = "${var.name}-nat-${count.index}"
  region       = var.region
  address_type = "EXTERNAL"
  network_tier = "PREMIUM"
}

resource "google_compute_router" "router" {
  count   = var.nat_enabled ? 1 : 0
  project = var.project_id
  name    = "${var.name}-router"
  region  = var.region
  network = google_compute_network.vpc.id
}

resource "google_compute_router_nat" "egress" {
  count                               = var.nat_enabled ? 1 : 0
  project                             = var.project_id
  name                                = "${var.name}-egress-nat"
  region                              = var.region
  router                              = google_compute_router.router[0].name
  nat_ip_allocate_option              = "MANUAL_ONLY"
  nat_ips                             = google_compute_address.nat[*].self_link
  source_subnetwork_ip_ranges_to_nat  = "LIST_OF_SUBNETWORKS"
  min_ports_per_vm                    = 1024
  enable_endpoint_independent_mapping = false

  subnetwork {
    name                    = google_compute_subnetwork.egress.id
    source_ip_ranges_to_nat = ["ALL_IP_RANGES"]
  }

  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }
}

# --- Private DNS: Google APIs and run.app resolve to private.googleapis.com,
# so internal-ingress Cloud Run services are reachable from the VPC and no
# workload needs a public route to reach Google APIs.

resource "google_dns_managed_zone" "googleapis" {
  count       = var.private_dns_enabled ? 1 : 0
  project     = var.project_id
  name        = "${var.name}-googleapis"
  dns_name    = "googleapis.com."
  visibility  = "private"
  description = "Private Google Access for googleapis.com"

  private_visibility_config {
    networks {
      network_url = google_compute_network.vpc.id
    }
  }
}

resource "google_dns_record_set" "googleapis_a" {
  count        = var.private_dns_enabled ? 1 : 0
  project      = var.project_id
  managed_zone = google_dns_managed_zone.googleapis[0].name
  name         = "private.googleapis.com."
  type         = "A"
  ttl          = 300
  rrdatas      = local.private_googleapis_ips
}

resource "google_dns_record_set" "googleapis_cname" {
  count        = var.private_dns_enabled ? 1 : 0
  project      = var.project_id
  managed_zone = google_dns_managed_zone.googleapis[0].name
  name         = "*.googleapis.com."
  type         = "CNAME"
  ttl          = 300
  rrdatas      = ["private.googleapis.com."]
}

resource "google_dns_managed_zone" "run_app" {
  count       = var.private_dns_enabled ? 1 : 0
  project     = var.project_id
  name        = "${var.name}-run-app"
  dns_name    = "run.app."
  visibility  = "private"
  description = "Route Cloud Run service-to-service calls through the VPC"

  private_visibility_config {
    networks {
      network_url = google_compute_network.vpc.id
    }
  }
}

resource "google_dns_record_set" "run_app_a" {
  count        = var.private_dns_enabled ? 1 : 0
  project      = var.project_id
  managed_zone = google_dns_managed_zone.run_app[0].name
  name         = "*.run.app."
  type         = "A"
  ttl          = 300
  rrdatas      = local.private_googleapis_ips
}

# --- Firewall (spec 17.3: tool-runner egress only to the egress proxy).

# Default-deny egress for the tool-runner.
resource "google_compute_firewall" "tools_deny_all_egress" {
  project            = var.project_id
  name               = "${var.name}-tools-deny-all-egress"
  network            = google_compute_network.vpc.id
  direction          = "EGRESS"
  priority           = 65000
  target_tags        = [var.tool_runner_tag]
  destination_ranges = ["0.0.0.0/0"]

  deny {
    protocol = "all"
  }

  dynamic "log_config" {
    for_each = var.firewall_logs_enabled ? [1] : []
    content {
      metadata = "INCLUDE_ALL_METADATA"
    }
  }
}

# The tool-runner may reach Cloud Run (egress proxy, scope) and Cloud Storage
# only through the private Google APIs VIP, on 443.
resource "google_compute_firewall" "tools_allow_google_private" {
  project            = var.project_id
  name               = "${var.name}-tools-allow-google-private"
  network            = google_compute_network.vpc.id
  direction          = "EGRESS"
  priority           = 1000
  target_tags        = [var.tool_runner_tag]
  destination_ranges = [local.private_googleapis_cidr]

  allow {
    protocol = "tcp"
    ports    = ["443"]
  }
}

# The tool-runner reads per-tool rate state from Memorystore (TLS port 6378).
resource "google_compute_firewall" "tools_allow_redis" {
  count              = var.psa_enabled ? 1 : 0
  project            = var.project_id
  name               = "${var.name}-tools-allow-redis"
  network            = google_compute_network.vpc.id
  direction          = "EGRESS"
  priority           = 1000
  target_tags        = [var.tool_runner_tag]
  destination_ranges = [var.psa_cidr]

  allow {
    protocol = "tcp"
    ports    = ["6378"]
  }
}

# Defence in depth against SSRF: the egress proxy never talks to private
# ranges (it reaches Google APIs through the private VIP, which is public
# address space and therefore not covered here).
resource "google_compute_firewall" "egress_proxy_deny_private" {
  project            = var.project_id
  name               = "${var.name}-egress-proxy-deny-private"
  network            = google_compute_network.vpc.id
  direction          = "EGRESS"
  priority           = 900
  target_tags        = [var.egress_proxy_tag]
  destination_ranges = ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"]

  deny {
    protocol = "all"
  }

  dynamic "log_config" {
    for_each = var.firewall_logs_enabled ? [1] : []
    content {
      metadata = "INCLUDE_ALL_METADATA"
    }
  }
}
