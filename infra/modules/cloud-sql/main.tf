# Cloud SQL for PostgreSQL 16 (spec 17.1, relational store row): private IP
# only, TLS required, HA (REGIONAL) in prod, automated backups with PITR,
# one database (five schemas created by migrations, spec 10) and one built-in
# user per service with a generated password.

terraform {
  required_version = ">= 1.6"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

resource "google_sql_database_instance" "this" {
  project             = var.project_id
  name                = var.instance_name
  region              = var.region
  database_version    = "POSTGRES_16"
  deletion_protection = var.deletion_protection

  settings {
    edition           = "ENTERPRISE"
    tier              = var.tier
    availability_type = var.high_availability ? "REGIONAL" : "ZONAL"
    disk_type         = "PD_SSD"
    disk_size         = var.disk_size_gb
    disk_autoresize   = true
    user_labels       = var.labels

    ip_configuration {
      ipv4_enabled                                  = false
      private_network                               = var.network_id
      allocated_ip_range                            = var.psa_range_name
      ssl_mode                                      = "ENCRYPTED_ONLY"
      enable_private_path_for_google_cloud_services = true
    }

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
      start_time                     = "02:00"
      transaction_log_retention_days = 7

      backup_retention_settings {
        retained_backups = var.retained_backups
        retention_unit   = "COUNT"
      }
    }

    maintenance_window {
      day          = 7
      hour         = 3
      update_track = "stable"
    }

    insights_config {
      query_insights_enabled  = true
      record_application_tags = true
      record_client_address   = false
    }

    database_flags {
      name  = "log_min_duration_statement"
      value = "500"
    }

    database_flags {
      name  = "max_connections"
      value = tostring(var.max_connections)
    }
  }
}

resource "google_sql_database" "this" {
  project  = var.project_id
  instance = google_sql_database_instance.this.name
  name     = var.database_name
}

resource "random_password" "users" {
  for_each = toset(var.users)
  length   = 32
  special  = false
}

# Built-in users; the migrations job (spec 10) narrows each role to USAGE and
# DML grants on its own schema only.
resource "google_sql_user" "users" {
  for_each        = toset(var.users)
  project         = var.project_id
  instance        = google_sql_database_instance.this.name
  name            = "ff_${replace(each.value, "-", "_")}"
  password        = random_password.users[each.value].result
  deletion_policy = "ABANDON"
}
