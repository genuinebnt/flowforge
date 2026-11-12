-- 0008_outbox_inbox: tables the spec implies but does not spell out.
--
-- Outbox (section 23.7, "same shape in every service schema", 10.4): one per
-- producing schema. identity publishes auth.events (the gateway hosts the
-- identity module), scope publishes scope.events, engine already has
-- engine.outbox (0004), and graph uses graph.change_events as its outbox.
CREATE TABLE identity.outbox (
  id bigserial PRIMARY KEY, event_id uuid NOT NULL UNIQUE, topic text NOT NULL, ordering_key text,
  payload jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), published_at timestamptz
);
CREATE INDEX ON identity.outbox (id) WHERE published_at IS NULL;

CREATE TABLE scope.outbox (
  id bigserial PRIMARY KEY, event_id uuid NOT NULL UNIQUE, topic text NOT NULL, ordering_key text,
  payload jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), published_at timestamptz
);
CREATE INDEX ON scope.outbox (id) WHERE published_at IS NULL;

-- Inbox (section 23.7, "same table in every consumer", 10.6): the engine
-- consumes scope.events (pause/resume) and graph.changes (event triggers);
-- notifier.processed_events already exists (0006).
CREATE TABLE engine.processed_events (
  consumer text NOT NULL, event_id uuid NOT NULL, processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer, event_id)
);
