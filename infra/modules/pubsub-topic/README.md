# pubsub-topic

One event topic from spec 11.7 (`run.events`, `approval.events`,
`graph.changes`, `scope.events`, `auth.events`), prefixed with
`FF_TOPIC_PREFIX`.

- Topic `<prefix><name>` and dead-letter topic `<prefix><name>.dead-letter`
  (14-day retention) with an operator subscription `….dead-letter.ops`.
- One pull subscription `<prefix><name>.<consumer>` per entry in `consumers`:
  message ordering enabled (publishers set the ordering key from 11.7),
  exponential retry (10 s to 600 s), ack deadline 60 s, dead-lettering after
  `max_delivery_attempts`, never expires.
- IAM at resource level only: `roles/pubsub.publisher` on the topic for each
  `publishers` member, `roles/pubsub.subscriber` on its own subscription for
  each consumer, and the Pub/Sub service agent's publisher/subscriber grants
  that dead-lettering requires.

`topic_retention_enabled = false` (lab) leaves message retention off on the
topics themselves; topic retention is billed as message storage, and
subscriptions still keep undelivered messages for their retention.
