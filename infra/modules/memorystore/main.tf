# Memorystore for Redis (spec 17.1): cache, limiter state and token denylist.
# Private services access, AUTH and in-transit TLS on, STANDARD_HA in prod.

terraform {
  required_version = ">= 1.6"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}

resource "google_redis_instance" "this" {
  project                 = var.project_id
  name                    = var.name
  region                  = var.region
  tier                    = var.tier
  memory_size_gb          = var.memory_size_gb
  redis_version           = var.redis_version
  authorized_network      = var.network_id
  connect_mode            = "PRIVATE_SERVICE_ACCESS"
  reserved_ip_range       = var.psa_range_name
  auth_enabled            = true
  transit_encryption_mode = "SERVER_AUTHENTICATION"
  labels                  = var.labels

  redis_configs = {
    maxmemory-policy = "volatile-lru"
  }

  maintenance_policy {
    weekly_maintenance_window {
      day = "SUNDAY"
      start_time {
        hours   = 3
        minutes = 0
      }
    }
  }
}
