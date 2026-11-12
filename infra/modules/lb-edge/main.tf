# Public edge for the gateway (spec 17.1 edge row): global external
# Application Load Balancer -> serverless NEG -> gateway Cloud Run service,
# Cloud Armor (preconfigured WAF rules and a per-IP rate limit as the first
# layer), Google-managed certificate, TLS 1.2+ and an HTTP->HTTPS redirect.

terraform {
  required_version = ">= 1.6"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}

resource "google_compute_global_address" "edge" {
  project      = var.project_id
  name         = "${var.name}-ip"
  address_type = "EXTERNAL"
  ip_version   = "IPV4"
}

resource "google_compute_region_network_endpoint_group" "gateway" {
  project               = var.project_id
  name                  = "${var.name}-gateway-neg"
  region                = var.region
  network_endpoint_type = "SERVERLESS"

  cloud_run {
    service = var.cloud_run_service
  }
}

resource "google_compute_security_policy" "edge" {
  project     = var.project_id
  name        = "${var.name}-armor"
  description = "Flowforge edge: WAF and per-IP rate limit"
  type        = "CLOUD_ARMOR"

  advanced_options_config {
    json_parsing = "STANDARD"
    log_level    = "NORMAL"
  }

  # Per-IP rate limit: the first layer of abuse control (spec 13, 18.2).
  rule {
    action      = "throttle"
    priority    = 100
    description = "Per-IP rate limit"
    match {
      versioned_expr = "SRC_IPS_V1"
      config {
        src_ip_ranges = ["*"]
      }
    }
    rate_limit_options {
      conform_action = "allow"
      exceed_action  = "deny(429)"
      enforce_on_key = "IP"
      rate_limit_threshold {
        count        = var.rate_limit_per_minute
        interval_sec = 60
      }
    }
  }

  dynamic "rule" {
    for_each = { for i, w in var.waf_rules : w => i }
    content {
      action      = "deny(403)"
      priority    = 1000 + rule.value
      description = "Preconfigured WAF ${rule.key}"
      match {
        expr {
          expression = "evaluatePreconfiguredWaf('${rule.key}', {'sensitivity': ${var.waf_sensitivity}})"
        }
      }
    }
  }

  rule {
    action      = "allow"
    priority    = 2147483647
    description = "Default allow"
    match {
      versioned_expr = "SRC_IPS_V1"
      config {
        src_ip_ranges = ["*"]
      }
    }
  }
}

resource "google_compute_backend_service" "gateway" {
  project               = var.project_id
  name                  = "${var.name}-gateway-backend"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  protocol              = "HTTPS"
  security_policy       = google_compute_security_policy.edge.id

  backend {
    group = google_compute_region_network_endpoint_group.gateway.id
  }

  log_config {
    enable      = true
    sample_rate = var.log_sample_rate
  }
}

resource "google_compute_url_map" "https" {
  project         = var.project_id
  name            = "${var.name}-https"
  default_service = google_compute_backend_service.gateway.id
}

resource "google_compute_managed_ssl_certificate" "edge" {
  project = var.project_id
  name    = "${var.name}-cert"

  managed {
    domains = var.domains
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "google_compute_ssl_policy" "tls12" {
  project         = var.project_id
  name            = "${var.name}-tls12"
  profile         = "MODERN"
  min_tls_version = "TLS_1_2"
}

resource "google_compute_target_https_proxy" "edge" {
  project          = var.project_id
  name             = "${var.name}-https-proxy"
  url_map          = google_compute_url_map.https.id
  ssl_certificates = [google_compute_managed_ssl_certificate.edge.id]
  ssl_policy       = google_compute_ssl_policy.tls12.id
}

resource "google_compute_global_forwarding_rule" "https" {
  project               = var.project_id
  name                  = "${var.name}-https"
  target                = google_compute_target_https_proxy.edge.id
  ip_address            = google_compute_global_address.edge.id
  port_range            = "443"
  load_balancing_scheme = "EXTERNAL_MANAGED"
}

# HTTP only redirects to HTTPS.
resource "google_compute_url_map" "redirect" {
  project = var.project_id
  name    = "${var.name}-http-redirect"

  default_url_redirect {
    https_redirect         = true
    redirect_response_code = "MOVED_PERMANENTLY_DEFAULT"
    strip_query            = false
  }
}

resource "google_compute_target_http_proxy" "redirect" {
  project = var.project_id
  name    = "${var.name}-http-proxy"
  url_map = google_compute_url_map.redirect.id
}

resource "google_compute_global_forwarding_rule" "http" {
  project               = var.project_id
  name                  = "${var.name}-http"
  target                = google_compute_target_http_proxy.redirect.id
  ip_address            = google_compute_global_address.edge.id
  port_range            = "80"
  load_balancing_scheme = "EXTERNAL_MANAGED"
}
