output "topic_id" {
  description = "Topic id."
  value       = google_pubsub_topic.this.id
}

output "topic_name" {
  description = "Topic name (with prefix)."
  value       = google_pubsub_topic.this.name
}

output "dead_letter_topic_name" {
  description = "Dead-letter topic name."
  value       = google_pubsub_topic.dead_letter.name
}

output "subscriptions" {
  description = "consumer => subscription name."
  value       = { for k, s in google_pubsub_subscription.consumers : k => s.name }
}
