# services/notifier

NotifierService (spec 11.7) and the alerting pipeline of spec 15.

* `PutRule` compiles with `alerts.Compile`; a rule with error diagnostics is
  stored only disabled (enabling it is INVALID_ARGUMENT `DSL_DIAGNOSTICS`
  with the diagnostics as details). `TestRule` evaluates a sample change.
  `PutChannel` validates webhook URLs (https, not internal) and requires
  secret references.
* `Service.HandleEvent` consumes graph.changes, run.events, approval.events
  and auth.events: it evaluates the tenant's enabled rules (per program)
  with `alerts.Processor` (dedupe, throttle, digest, quiet hours) and, in one
  transaction, records `(consumer, event_id)` in processed_events and stores
  the alert events and one delivery per action and channel. The same
  transaction upserts the throttle windows and open digests the event
  changed (`alerts.Outcome.State` -> notifier.throttle_windows,
  notifier.digest_buffers); a processor built after a restart or a rule
  change restores them (`Repo.LoadAlertState`, `Processor.Restore`), and a
  flushed digest is deleted with the alert it produces (G-LIM-06,
  `TestRegress_GLIM06_ThrottleSurvivesRestart_{Memory,Postgres}`). A failed
  transaction drops the cached processor so the redelivery starts from the
  persisted state.
* The delivery worker (`DeliverDue`) sends due deliveries; failures are
  rescheduled with `alerts.NextAttempt` (10 s, 30 s, 2 min, 10 min, 1 h with
  jitter; failed after 24 h); one channel never blocks another.
* Channels (`Senders`): webhook (HMAC `alerts.WebhookHeaders`, destinations
  resolved, checked with `alerts.CheckResolved` and pinned; http://127.0.0.1
  only with `AllowInsecureLoopback`, for tests), slack (incoming-webhook URL
  from `webhook_secret_ref`), email (`EmailSender`: `SMTPEmail` over
  net/smtp, `LogEmail` for dev).
* Egress (G-SVC-08): with `FF_EGRESS_PROXY` set (required in prod, spec
  17.1), webhook and Slack deliveries are CONNECT tunnels through the
  egress proxy's notify path (`Senders.Egress`, `NotifyEgress`): the URL
  rules are checked here, and the proxy applies its `FF_NOTIFY_ALLOWLIST`,
  resolves the host, refuses non-public addresses and pins the connection.
  Without it (local development) the notifier resolves, checks and pins
  itself, as above. `TestRegress_G_SVC_08_WebhooksThroughEgressProxy`.
* `Secrets` (= `platform.Secrets`) resolves references: `cmd/notifier`
  wires `platform.LocalSecrets` (`env://NAME`, `file:///path`) plus
  `smsecrets.Store` for `sm://projects/P/secrets/S/versions/V` (or the
  short `sm://S` with `FF_SECRET_PROJECT`, else `FF_PUBSUB_PROJECT`, as the
  default project); `MapSecrets` in tests. An unresolvable reference fails
  the delivery attempt (it is retried), never sends unsigned.
* `MemRepo` / `PgRepo` (notifier schema, migrations 0006, 0014 and 0017).
