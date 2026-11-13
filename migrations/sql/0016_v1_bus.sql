-- 0016_v1_bus: the Postgres event bus (G-SVC-01), the multi-process local
-- adapter of services/internal/bus (bus.PgBus). On GCP the same Bus port is
-- served by Pub/Sub and these tables stay empty.
--
-- Model (one table per concept, not per topic, so a new topic needs no DDL):
--
-- * bus.subscriptions: one row per (topic, consumer), the equivalent of a
--   Pub/Sub subscription. It is durable: messages published while a
--   consumer is down wait for it, as Pub/Sub retains them (spec 11.7).
-- * bus.messages: one row per published envelope (proto bytes). The
--   ordering key is the spec 11.7 key of the topic (run_id, approval_id,
--   program_id, user_id).
-- * bus.deliveries: the fan-out, one row per (subscription, message),
--   written in the publishing transaction. A consumer claims rows with
--   FOR UPDATE SKIP LOCKED and a lease (the ack deadline); a row is acked,
--   rescheduled with backoff (nack), or marked dead after max attempts
--   (the message is then also published to <topic>.dead-letter).
--
-- Delivery is at least once. Ordering per key holds because a delivery is
-- claimable only when no earlier delivery of the same (subscription,
-- ordering key) is still pending, leased or backing off.
CREATE SCHEMA IF NOT EXISTS bus;

CREATE TABLE bus.subscriptions (
  topic text NOT NULL, consumer text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (topic, consumer)
);

CREATE TABLE bus.messages (
  id bigserial PRIMARY KEY, topic text NOT NULL, event_id text NOT NULL, ordering_key text NOT NULL DEFAULT '',
  envelope bytea NOT NULL, published_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX messages_published ON bus.messages (published_at);

CREATE TABLE bus.deliveries (
  topic text NOT NULL, consumer text NOT NULL,
  message_id bigint NOT NULL REFERENCES bus.messages(id) ON DELETE CASCADE,
  ordering_key text NOT NULL DEFAULT '',
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','acked','dead')),
  attempts integer NOT NULL DEFAULT 0,
  available_at timestamptz NOT NULL DEFAULT now(),
  lease_until timestamptz,
  last_error text NOT NULL DEFAULT '',
  done_at timestamptz,
  PRIMARY KEY (topic, consumer, message_id),
  FOREIGN KEY (topic, consumer) REFERENCES bus.subscriptions(topic, consumer) ON DELETE CASCADE
);
-- The claim scan: pending rows of one subscription in publication order.
CREATE INDEX deliveries_pending ON bus.deliveries (topic, consumer, message_id) WHERE status = 'pending';
-- The ordering guard: is an earlier delivery of the same key still pending?
CREATE INDEX deliveries_key ON bus.deliveries (topic, consumer, ordering_key, message_id)
  WHERE status = 'pending' AND ordering_key <> '';
-- Retention of acked and dead rows (maintenance).
CREATE INDEX deliveries_done ON bus.deliveries (done_at) WHERE status <> 'pending';
