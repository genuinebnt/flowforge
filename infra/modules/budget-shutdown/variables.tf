variable "project_id" {
  description = "GCP project id."
  type        = string
}

variable "project_number" {
  description = "Project number (Pub/Sub service agent)."
  type        = string
}

variable "region" {
  description = "Region of the handler, the Cloud Run services and the scheduler jobs."
  type        = string
}

variable "name" {
  description = "Name prefix, e.g. ff-lab (service ff-lab-budget-shutdown, topic ff-lab-budget)."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,10}$", var.name))
    error_message = "name must be 2-11 lowercase letters, digits or dashes (it prefixes service account ids)."
  }
}

variable "image" {
  description = "Image with gcloud and python3, pinned by digest, e.g. gcr.io/google.com/cloudsdktool/google-cloud-cli@sha256:<64 hex> (resolve the digest of the :slim tag when enabling)."
  type        = string

  validation {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.image)) && !can(regex("@sha256:0{64}$", var.image))
    error_message = "image must be pinned by a real digest (…@sha256:<64 hex>), not the all-zero placeholder."
  }
}

variable "threshold" {
  description = "Shut down when actual spend >= threshold x budget amount (1.0 = the whole budget)."
  type        = number
  default     = 1.0

  validation {
    condition     = var.threshold > 0 && var.threshold <= 2
    error_message = "threshold must be in (0, 2]."
  }
}

variable "gateway" {
  description = "Public Cloud Run service to close (ingress internal): { service = name, service_account_id = runtime SA resource id }. null = none."
  type = object({
    service            = string
    service_account_id = string
  })
  default = null
}

variable "scale_down_services" {
  description = "Cloud Run services whose min instances are set to 0: service name => runtime SA resource id (for iam.serviceAccountUser)."
  type        = map(string)
  default     = {}
}

variable "scheduler_jobs" {
  description = "Cloud Scheduler job names (in region) to pause."
  type        = list(string)
  default     = []
}

variable "vm" {
  description = "Compute Engine VM to stop: { name, zone }. null = none."
  type = object({
    name = string
    zone = string
  })
  default = null
}

variable "sql_instance" {
  description = "Cloud SQL instance to stop (activation policy NEVER). null = none."
  type        = string
  default     = null
}

variable "labels" {
  description = "Labels on every resource."
  type        = map(string)
  default     = {}
}
