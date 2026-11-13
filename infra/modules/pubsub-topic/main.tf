# One event topic from spec 11.7 with its dead-letter topic, one pull
# subscription per consumer (ordering on, exponential retry, 60 s ack
# deadline, dead-lettering) and resource-level IAM: Publisher on the topic for
# the producer, Subscriber on its own subscription for each consumer.

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
  topic_name   = "${var.prefix}${var.name}"
  dlq_name     = "${var.prefix}${var.name}.dead-letter"
  pubsub_agent = "serviceAccount:service-${var.project_number}@gcp-sa-pubsub.iam.gserviceaccount.com"
  label_safe   = replace(var.name, ".", "-")
}

resource "google_pubsub_topic" "this" {
  project                    = var.project_id
  name                       = local.topic_name
  message_retention_duration = var.topic_retention_enabled ? var.topic_retention : null
  labels                     = merge(var.labels, { "ff-topic" = local.label_safe })
}

resource "google_pubsub_topic" "dead_letter" {
  project                    = var.project_id
  name                       = local.dlq_name
  message_retention_duration = var.topic_retention_enabled ? var.dead_letter_retention : null
  labels                     = merge(var.labels, { "ff-topic" = "${local.label_safe}-dead-letter" })
}

# Operators inspect dead letters here; messages are kept 14 days (spec 11.7).
resource "google_pubsub_subscription" "dead_letter_ops" {
  project                    = var.project_id
  name                       = "${local.dlq_name}.ops"
  topic                      = google_pubsub_topic.dead_letter.id
  ack_deadline_seconds       = var.ack_deadline_seconds
  message_retention_duration = var.dead_letter_retention
  retain_acked_messages      = false
  labels                     = var.labels

  expiration_policy {
    ttl = ""
  }
}

resource "google_pubsub_subscription" "consumers" {
  for_each = var.consumers

  project                    = var.project_id
  name                       = "${local.topic_name}.${each.key}"
  topic                      = google_pubsub_topic.this.id
  ack_deadline_seconds       = var.ack_deadline_seconds
  enable_message_ordering    = true
  message_retention_duration = var.subscription_retention
  labels                     = merge(var.labels, { "ff-consumer" = each.key })

  retry_policy {
    minimum_backoff = var.min_backoff
    maximum_backoff = var.max_backoff
  }

  dead_letter_policy {
    dead_letter_topic     = google_pubsub_topic.dead_letter.id
    max_delivery_attempts = var.max_delivery_attempts
  }

  expiration_policy {
    ttl = ""
  }

  depends_on = [google_pubsub_topic_iam_member.agent_dead_letter_publisher]
}

resource "google_pubsub_topic_iam_member" "publishers" {
  for_each = var.publishers
  project  = var.project_id
  topic    = google_pubsub_topic.this.name
  role     = "roles/pubsub.publisher"
  member   = each.value
}

resource "google_pubsub_subscription_iam_member" "consumers" {
  for_each     = var.consumers
  project      = var.project_id
  subscription = google_pubsub_subscription.consumers[each.key].name
  role         = "roles/pubsub.subscriber"
  member       = each.value
}

# The Pub/Sub service agent forwards undeliverable messages: it needs
# Publisher on the dead-letter topic and Subscriber on each source
# subscription.
resource "google_pubsub_topic_iam_member" "agent_dead_letter_publisher" {
  project = var.project_id
  topic   = google_pubsub_topic.dead_letter.name
  role    = "roles/pubsub.publisher"
  member  = local.pubsub_agent
}

resource "google_pubsub_subscription_iam_member" "agent_subscriber" {
  for_each     = var.consumers
  project      = var.project_id
  subscription = google_pubsub_subscription.consumers[each.key].name
  role         = "roles/pubsub.subscriber"
  member       = local.pubsub_agent
}
