# Artifact bucket for one environment (spec 17.1): uniform bucket-level
# access, public access prevention, lifecycle rules per prefix, optional
# CMEK, and prefix-scoped IAM via IAM conditions (spec 17.2 "on its prefix").

terraform {
  required_version = ">= 1.6"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}

resource "google_storage_bucket" "this" {
  project                     = var.project_id
  name                        = var.name
  location                    = var.location
  storage_class               = "STANDARD"
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = var.force_destroy
  labels                      = var.labels

  versioning {
    enabled = false
  }

  soft_delete_policy {
    retention_duration_seconds = var.soft_delete_seconds
  }

  dynamic "encryption" {
    for_each = var.kms_key_name == null ? [] : [var.kms_key_name]
    content {
      default_kms_key_name = encryption.value
    }
  }

  dynamic "lifecycle_rule" {
    for_each = var.lifecycle_rules
    content {
      action {
        type = "Delete"
      }
      condition {
        age            = lifecycle_rule.value.age_days
        matches_prefix = [lifecycle_rule.value.prefix]
      }
    }
  }

  # Abort stale multipart uploads.
  lifecycle_rule {
    action {
      type = "AbortIncompleteMultipartUpload"
    }
    condition {
      age = 1
    }
  }
}

resource "google_storage_bucket_iam_member" "grants" {
  for_each = var.iam
  bucket   = google_storage_bucket.this.name
  role     = each.value.role
  member   = each.value.member

  dynamic "condition" {
    for_each = each.value.prefix == null ? [] : [each.value.prefix]
    content {
      title       = "prefix-${replace(trimsuffix(condition.value, "/"), "/", "-")}"
      description = "Objects under ${condition.value} only"
      expression  = "resource.name.startsWith(\"projects/_/buckets/${var.name}/objects/${condition.value}\")"
    }
  }
}
