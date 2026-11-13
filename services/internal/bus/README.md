# services/internal/bus

The event bus of the internal services (spec 3.2, 11.7, 23.7; G-SVC-01):
the `Bus` port, three adapters, the outbox relay and the inbox.

| Adapter | Package | Profile | Selected by |
|---|---|---|---|
| `bus.Mem` | `bus` | one process (`flowforge dev`, tests) | `FF_BUS=mem`, or neither a project nor a database |
| `bus.PgBus` | `bus` | several local processes on one Postgres | `FF_BUS=postgres`, default when `FF_DB_DSN` is set |
| `pubsubbus.Bus` | `bus/pubsubbus` | GCP (Pub/Sub; the emulator or `pstest` locally) | `FF_BUS=pubsub`, default when `FF_PUBSUB_PROJECT` is set |

`busconfig.Open` is the one constructor the services and the gateway use.
`FF_TOPIC_PREFIX` prefixes Pub/Sub names; `FF_PUBSUB_CREATE=true` lets the
adapter create missing topics and subscriptions (emulator, a lab project
without Terraform-made ones).

## Contract (`bustest.Run`)

Every adapter runs the same suite (`TestBusContract_Mem`,
`TestBusContract_Postgres`, `TestBusContract_PubSub`):

* at least once to every subscription of a topic;
* redelivery after a handler error;
* dead-lettering to `<topic>.dead-letter` after `MaxAttempts` deliveries
  (5 by default, the Terraform `max_delivery_attempts`);
* ordering per ordering key (`bus.OrderingKey`: run_id, approval_id,
  program_id, user_id per spec 11.7): a failing message holds back the rest
  of its key;
* exactly-once effects through an `Inbox` (processed_events).

Messages carry the envelope as proto bytes and the spec 11.7 attributes
(`bus.Attributes`: tenant_id, type, event_id, traceparent; role and status
on approval.events).

## Postgres bus (migration 0016)

`bus.subscriptions` (durable, one row per topic and consumer),
`bus.messages` and `bus.deliveries` (the fan-out, written in the publishing
transaction). A consumer claims deliveries with `FOR UPDATE SKIP LOCKED`
and a lease (the 60 s ack deadline of spec 17), so several processes share
a subscription and a crashed consumer's messages come back when its lease
expires. Nacks back off exponentially (100 ms doubling to 10 s); after
`MaxAttempts` the delivery is `dead` and the envelope is published to the
dead-letter topic in the same transaction. Ordering per key: a delivery is
claimable only while no earlier delivery of the same subscription and key
is pending. Wakeups use `LISTEN ff_bus` / `pg_notify` at commit, with a
1 s poll as the fallback. All times are the database's. Acked rows are
kept 24 h and dead ones 14 days (spec 11.7), then removed by
`PgBus.Cleanup`, which the maintenance job calls; pending deliveries are
never deleted.

## Pub/Sub adapter

Names follow `infra/modules/pubsub-topic`: topic `<prefix><topic>`,
dead-letter topic `<prefix><topic>.dead-letter`, subscription
`<prefix><topic>.<consumer>` (ordering on, retry policy, dead-letter
policy). Publish waits for the server's acknowledgement (the relay marks a
row published only after it) and resumes a paused ordering key after a
failure. A handler error nacks; Pub/Sub itself redelivers with the
subscription's backoff and dead-letters, unlike Mem and PgBus, which
republish themselves. Without `CreateMissing`, Subscribe fails if
Terraform did not create the subscription (no silent idling).

Known differences, pinned by the suite or documented here:

* `pstest` redelivers a nack at once (no backoff) and re-publishes a
  dead-lettered message on every delivery cycle (its ack of the original
  uses a stale ack id), so the suite counts distinct dead-letter event ids.
* Real Pub/Sub with ordering redelivers every later message of a key after
  a nack, so a consumer may see a later message before the redelivered
  one; consumers are idempotent by event_id and do not rely on stricter
  ordering. Mem, PgBus and pstest never deliver past an unacked message.

## Outbox relays

`Relay` publishes an `OutboxSource` (`PgOutbox` over engine.outbox,
scope.outbox, identity.outbox; `MemOutbox`) through whichever bus is
configured. The gateway relays identity.outbox (auth.events, G-SVC-02) the
same way, from Postgres or from its in-memory identity store. Every replica
runs a relay, but only one claims a table at a time (`PgOutbox.Claim`
takes a transaction-scoped advisory lock on the table name; the others
claim nothing until it commits), so rows leave in id order and events of
one ordering key never overtake each other across batches (G-SVC-25;
graphstore/pg.Relay does the same for graph.change_events).

## Comparative benchmark: `BenchmarkBusAlgo/<adapter>/<shape>`

```sh
eval "$(scripts/devpg.sh start)"
go test -run '^$' -bench BusAlgo -benchtime 2s ./services/internal/bus/
```

`latency`: one publish, then wait for its delivery (ns/op is the round
trip). `throughput`: batches of 100 messages on 10 ordering keys, waiting
for all (msgs/s). Measured 2026-11-13 on a 4-vCPU Intel Xeon VM (Go 1.26,
Postgres 16 on the same host, `fsync` on):

| Adapter | latency (round trip) | throughput |
|---|---|---|
| mem | 2.5 µs | ~94,000 msgs/s (bounded by the benchmark's 50 µs poll) |
| postgres | 1.2 ms | ~1,500 msgs/s |
| pubsub-pstest | 99 ms | ~89 msgs/s |

Reading the numbers:

* **mem** is the default for one process: no I/O, but nothing survives a
  restart and no other process sees the messages.
* **postgres** is the default when a database is configured and no Pub/Sub
  project is: one transaction per publish and per ack dominates. It is the
  right choice for local multi-process runs and small single-VM
  deployments (no extra service, durable, ordered). At sustained loads
  beyond a few thousand messages per second, or across regions, Pub/Sub
  is the better choice.
* **pubsub-pstest** measures the client library against the in-memory
  fake, whose delivery loop runs on a ~100 ms cycle; it says nothing about
  the Pub/Sub service's latency (tens of ms) or throughput (far above
  these). Use it only to compare client overhead, and benchmark real
  Pub/Sub in the lab project.

## Tests

```sh
go test -race ./services/internal/bus/...                 # mem + pstest
eval "$(scripts/devpg.sh start)"; go test -race ./services/internal/bus/...   # + Postgres
```
