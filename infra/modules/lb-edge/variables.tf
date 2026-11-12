variable "project_id" {
  description = "GCP project id."
  type        = string
}

variable "region" {
  description = "Region of the gateway Cloud Run service."
  type        = string
}

variable "name" {
  description = "Name prefix, e.g. ff-dev-edge."
  type        = string
}

variable "cloud_run_service" {
  description = "Name of the gateway Cloud Run service."
  type        = string
}

variable "domains" {
  description = "Domains on the managed certificate (the API host, e.g. api.flowforge.example)."
  type        = list(string)
}

variable "rate_limit_per_minute" {
  description = "Requests per minute per client IP before Cloud Armor answers 429."
  type        = number
  default     = 600
}

variable "waf_rules" {
  description = "Cloud Armor preconfigured WAF rule sets."
  type        = list(string)
  default = [
    "sqli-v33-stable",
    "xss-v33-stable",
    "lfi-v33-stable",
    "rfi-v33-stable",
    "rce-v33-stable",
    "methodenforcement-v33-stable",
    "scannerdetection-v33-stable",
    "protocolattack-v33-stable",
    "sessionfixation-v33-stable",
  ]
}

variable "waf_sensitivity" {
  description = "Paranoia level for the preconfigured WAF rules (1 = fewest false positives)."
  type        = number
  default     = 1
}

variable "log_sample_rate" {
  description = "LB request log sampling."
  type        = number
  default     = 1.0
}
