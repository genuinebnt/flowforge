# monitoring

Alerting, log-based metrics, a dashboard and budget alerts (spec 17.1
observability row, 17.6, 18.1).

- Email notification channels from `notification_emails`.
- One PromQL alert policy per 18.1 SLO, over histograms exported by the
  services' OpenTelemetry Prometheus exporters into Managed Service for
  Prometheus:

  | SLO | Metric (default) | Threshold |
  |-----|------------------|-----------|
  | scope check p99 | `ff_scope_check_seconds` | 100 ms |
  | run submit-to-start p95 | `ff_run_submit_to_start_seconds` | 5 s |
  | graph ingest p95 | `ff_graph_ingest_seconds` | 1 s |
  | outbox lag p99 | `ff_outbox_lag_seconds` | 10 s |

  Metric names are variables: 18.1 names the last two; the first two are
  Flowforge choices that the services must export.
- An alert on undelivered messages in any `*.dead-letter.ops` subscription.
- Log-based metrics `ff_scope_denials` (labels reason, tenant_id) and
  `ff_approvals` (labels status, role) over the 18.1 JSON log lines; the
  matched `msg` values are variables.
- A dashboard with the four SLO series and both log metrics.
- `google_billing_budget` with actual-spend thresholds at 50/80/100 % and a
  forecast warning at 100 %. Creating budgets needs
  `roles/billing.costsManager` on the billing account for the identity that
  runs Terraform.

Toggles for the lab: `alert_policies_enabled` and `log_metrics_enabled`
(default `true`; alerting and user-defined metrics may be billed),
`log_env` (the `env` field of log lines when it differs from `env`; the lab
runs `FF_ENV=prod`), and `budget_pubsub_topic`, which sends every budget
notification to a Pub/Sub topic (the `budget-shutdown` module).
