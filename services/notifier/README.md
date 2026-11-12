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
  the alert events and one delivery per action and channel.
* The delivery worker (`DeliverDue`) sends due deliveries; failures are
  rescheduled with `alerts.NextAttempt` (10 s, 30 s, 2 min, 10 min, 1 h with
  jitter; failed after 24 h); one channel never blocks another.
* Channels (`Senders`): webhook (HMAC `alerts.WebhookHeaders`, destinations
  resolved, checked with `alerts.CheckResolved` and pinned; http://127.0.0.1
  only with `AllowInsecureLoopback`, for tests), slack (incoming-webhook URL
  from `webhook_secret_ref`), email (`EmailSender`: `SMTPEmail` over
  net/smtp, `LogEmail` for dev).
* `Secrets` resolves references: `EnvSecrets` (`env://NAME`, `file://`),
  `MapSecrets` (tests); `sm://` Secret Manager is future work.
* `MemRepo` / `PgRepo` (notifier schema, migrations 0006 and 0014).
