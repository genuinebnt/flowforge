variable "project_id" {
  description = "GCP project id."
  type        = string
}

variable "region" {
  description = "Region of the instance."
  type        = string
}

variable "instance_name" {
  description = "Instance name, e.g. ff-dev-pg."
  type        = string
}

variable "database_name" {
  description = "The single Flowforge database."
  type        = string
  default     = "flowforge"
}

variable "tier" {
  description = "Machine tier; the smallest HA-capable tier until load justifies more (spec 17.6)."
  type        = string
  default     = "db-custom-1-3840"
}

variable "high_availability" {
  description = "REGIONAL (HA) when true, ZONAL otherwise."
  type        = bool
  default     = false
}

variable "disk_size_gb" {
  description = "Initial disk size; autoresize is on."
  type        = number
  default     = 10
}

variable "max_connections" {
  description = "max_connections flag; size for sum(max_instances x pool size)."
  type        = number
  default     = 200
}

variable "retained_backups" {
  description = "Number of automated backups to keep."
  type        = number
  default     = 7
}

variable "network_id" {
  description = "VPC with private services access."
  type        = string
}

variable "psa_range_name" {
  description = "Allocated private services access range."
  type        = string
}

variable "users" {
  description = "Service names that get a built-in user ff_<service>."
  type        = list(string)
}

variable "deletion_protection" {
  description = "Protect the instance from deletion."
  type        = bool
  default     = true
}

variable "labels" {
  description = "Labels."
  type        = map(string)
  default     = {}
}
