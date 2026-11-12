variable "project_id" {
  description = "GCP project id."
  type        = string
}

variable "project_number" {
  description = "Project number (for the Pub/Sub service agent)."
  type        = string
}

variable "name" {
  description = "Topic name from spec 11.7, e.g. run.events."
  type        = string
}

variable "prefix" {
  description = "FF_TOPIC_PREFIX prepended to topic and subscription names."
  type        = string
  default     = ""
}

variable "publishers" {
  description = "Producer name => IAM member granted roles/pubsub.publisher on the topic."
  type        = map(string)
  default     = {}
}

variable "consumers" {
  description = "Consumer name => IAM member. One subscription <topic>.<consumer> per entry, Subscriber granted on it only."
  type        = map(string)
  default     = {}
}

variable "ack_deadline_seconds" {
  description = "Ack deadline (spec 17.1: 60 s)."
  type        = number
  default     = 60
}

variable "min_backoff" {
  description = "Exponential retry minimum backoff."
  type        = string
  default     = "10s"
}

variable "max_backoff" {
  description = "Exponential retry maximum backoff."
  type        = string
  default     = "600s"
}

variable "max_delivery_attempts" {
  description = "Deliveries before a message goes to the dead-letter topic."
  type        = number
  default     = 10
}

variable "topic_retention" {
  description = "Retention on the main topic (allows seek/replay)."
  type        = string
  default     = "604800s"
}

variable "subscription_retention" {
  description = "Unacked message retention on consumer subscriptions."
  type        = string
  default     = "604800s"
}

variable "dead_letter_retention" {
  description = "Retention of dead letters (spec 11.7: 14 days)."
  type        = string
  default     = "1209600s"
}

variable "labels" {
  description = "Labels."
  type        = map(string)
  default     = {}
}
