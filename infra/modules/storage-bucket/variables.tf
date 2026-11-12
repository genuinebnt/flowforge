variable "project_id" {
  description = "GCP project id."
  type        = string
}

variable "name" {
  description = "Globally unique bucket name."
  type        = string
}

variable "location" {
  description = "Bucket location (region or multi-region)."
  type        = string
}

variable "lifecycle_rules" {
  description = "Delete objects under prefix after age_days (spec 17.1: raw output 90 days, archived events 400 days)."
  type = list(object({
    prefix   = string
    age_days = number
  }))
  default = []
}

variable "iam" {
  description = "Grant name => { role, member, prefix }. prefix (e.g. \"raw/\") adds an IAM condition limiting the grant to that object prefix."
  type = map(object({
    role   = string
    member = string
    prefix = optional(string)
  }))
  default = {}

  validation {
    condition     = alltrue([for g in values(var.iam) : contains(["roles/storage.objectAdmin", "roles/storage.objectCreator", "roles/storage.objectViewer", "roles/storage.objectUser"], g.role)])
    error_message = "Only object-level storage roles may be granted on the artifact bucket."
  }
}

variable "kms_key_name" {
  description = "Optional CMEK key (projects/…/cryptoKeys/…)."
  type        = string
  default     = null
}

variable "soft_delete_seconds" {
  description = "Soft-delete retention."
  type        = number
  default     = 604800
}

variable "force_destroy" {
  description = "Allow terraform destroy of a non-empty bucket (dev only)."
  type        = bool
  default     = false
}

variable "labels" {
  description = "Labels."
  type        = map(string)
  default     = {}
}
