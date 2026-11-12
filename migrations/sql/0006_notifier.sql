-- 0006_notifier: spec section 10.6, transcribed verbatim.

CREATE SCHEMA notifier;

CREATE TABLE notifier.channels (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL, kind text NOT NULL CHECK (kind IN ('slack','email','webhook')),
  name text NOT NULL, config jsonb NOT NULL,                       -- holds secret references only, never secret values
  enabled boolean NOT NULL DEFAULT true, UNIQUE (tenant_id, name)
);

CREATE TABLE notifier.alert_rules (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL, program_id uuid, name text NOT NULL,
  source text NOT NULL, compiled jsonb NOT NULL,                   -- section 15
  enabled boolean NOT NULL DEFAULT true, version integer NOT NULL DEFAULT 1,
  created_by uuid NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), UNIQUE (tenant_id, name)
);

CREATE TABLE notifier.alert_events (
  id uuid PRIMARY KEY, tenant_id uuid NOT NULL, rule_id uuid NOT NULL REFERENCES notifier.alert_rules(id) ON DELETE CASCADE,
  trigger_event_id uuid NOT NULL, dedupe_key text NOT NULL,
  status text NOT NULL CHECK (status IN ('pending','sent','failed','suppressed')),
  payload jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (rule_id, dedupe_key, trigger_event_id)
);

CREATE TABLE notifier.deliveries (
  id uuid PRIMARY KEY, alert_event_id uuid NOT NULL REFERENCES notifier.alert_events(id) ON DELETE CASCADE,
  channel_id uuid NOT NULL REFERENCES notifier.channels(id),
  status text NOT NULL CHECK (status IN ('pending','sent','failed')), attempts integer NOT NULL DEFAULT 0,
  next_attempt_at timestamptz, last_status_code integer, last_error text, sent_at timestamptz
);
CREATE INDEX ON notifier.deliveries (next_attempt_at) WHERE status = 'pending';

CREATE TABLE notifier.processed_events (                           -- consumer idempotency (same table in every consumer)
  consumer text NOT NULL, event_id uuid NOT NULL, processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer, event_id)
);
