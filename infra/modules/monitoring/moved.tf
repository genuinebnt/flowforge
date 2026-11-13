# Alert policies and log-based metrics became optional (lab); keep existing
# dev/prod state in place.

moved {
  from = google_monitoring_alert_policy.dead_letters
  to   = google_monitoring_alert_policy.dead_letters[0]
}

moved {
  from = google_logging_metric.scope_denials
  to   = google_logging_metric.scope_denials[0]
}

moved {
  from = google_logging_metric.approvals
  to   = google_logging_metric.approvals[0]
}
