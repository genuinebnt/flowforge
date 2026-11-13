# services

The internal services of Flowforge (spec 3.1-3.3, 11, 15, 21.6, 22.9). Each
is thin wiring of adapters to the domain modules (spec 24.1): handlers
convert proto <-> domain (`internal/convert`), call the module, map errors
(`internal/platform`). The public REST gateway (`services/gateway`) is built
separately and talks to these over gRPC.

## Topology

```
                 gateway (REST /v1, mints internal tokens)
                    | gRPC (authorization: Bearer <internal JWT>, x-tenant-id, x-actor,
                    |       x-request-id, traceparent, idempotency-key)
   +----------+-----+------+-----------+------------+
   v          v            v           v            v
 scope <--- engine ---> tool-runner   graph      notifier   similarity
   ^  \       |  \          |   \        ^  \        ^          ^   |
   |   \      |   \         |    \       |   \       |          |   | GetEntity/Search
   |    \     |    +--------|-----\----> IngestObservations     |   +--> graph
   |     \    |             |      v                 |          |
   |      +---|----- Check -+  egress-proxy --> targets (pinned IPs only)
   |          |                  | decision log
   +----------|------------------+
              |
   event bus (bus.Bus: in process, Postgres or Pub/Sub): run.events, approval.events (engine outbox),
   scope.events (scope outbox), graph.changes (graph change_events), auth.events (gateway, identity.outbox)
     -> engine (scope.events), notifier (all), similarity (graph.changes)
```

| Package | gRPC service | Owns | Publishes / consumes |
|---------|--------------|------|----------------------|
| `scope` | ScopeService | programs, scope versions, kill switches, decision log | publishes scope.events |
| `engine` | EngineService (+ the schedule API and `POST /internal/schedules:tick`, see below) | workflows, runs, events, approvals, schedules | publishes run.events, approval.events; consumes scope.events |
| `toolrunner` | ToolRunnerService | tool adapters | none |
| `egressproxy` | HTTP CONNECT / forward proxy, SOCKS5, DNS forwarder | tickets (in memory); records `intel_source` decisions | none |
| `graph` | GraphService, GraphAnalytics | entities, edges, tags, change events | publishes graph.changes |
| `similarity` | Similarity | vector indexes | consumes graph.changes |
| `notifier` | NotifierService | rules, channels, alert events, deliveries | consumes graph.changes, run.events, approval.events, auth.events |
| `devstack` | - | runs all of the above in one process | |
| `app` | - | entry point of the `cmd/<service>` binaries | |
| `internal/platform` | - | config, interceptors, errors, tracing, logging, metrics, health, serve | |
| `internal/convert` | - | every proto <-> domain conversion | |
| `internal/bus` | - | `Bus` port; in-process, Postgres (`PgBus`) and Pub/Sub (`pubsubbus`) adapters; `busconfig`; outbox relay; inbox; `bustest` contract | |
| `internal/blob` | - | blob store selection (memory, local directory, Cloud Storage) and the zstd archive writer | |
| `maintenance` | - | spec 10.7 retention, partitions, run archival (in process, `migrate maintain`, the tick) | |
| `internal/wire` | - | builds each service from an `Env` (shared by cmd and devstack) | |
| `internal/servicetest` | - | test helpers (fresh Postgres DB, counting resolver, webhook receiver) | |

## Tools (tool runner)

| Tool | DSL signature | Class | Reaches |
|------|---------------|-------|---------|
| `dnsresolve` | `(Fqdn) -> List<IpAddress>` | passive | proxy DNS forwarder (A, AAAA, CNAME chain, MX, NS, TXT) |
| `httpprobe` | `(InScope<Host>) -> Option<Service>` | active | proxy HTTP / CONNECT |
| `portscan` (alias `tcpconnect`) | `(InScope<Host>, List<Int>) -> List<Int>` | active | proxy CONNECT or SOCKS5 |
| `tlscert` | `(InScope<Host>, Int) -> Option<TlsCertificate>` | passive (D3) | proxy CONNECT, handshake only |
| `ctlog` | `(Fqdn) -> List<Fqdn>` | passive | crt.sh (intel source) |
| `rdap` | `(Host) -> List<Organization>` | passive | IANA bootstrap + registry RDAP (intel sources) |
| `asn` | `(IpAddress) -> Option<Asn>` | passive | RDAP + Team Cymru DNS (intel sources) |
| `mock` | `(String) -> String` | none | nothing |

Details, emitted assets and fixtures: `services/toolrunner/README.md`;
egress rules (port policy, tickets, DNS policy, per-target limits, intel
allowlist): `services/egressproxy/README.md`.

## Profiles

Every cloud dependency sits behind a port with a local adapter
(docs/GAPS.md, "Deployment profiles"). Three profiles:

| | mem (one process) | local multi-process | gcp |
|---|---|---|---|
| Start | `go run ./cmd/flowforge dev` | `make local-up` (deploy/compose.local.yml) or the binaries with the env below | Terraform (`infra/`, `make gcp-lab-up`) |
| Processes | one: gateway + every service | one per service (containers or OS processes) | one Cloud Run service each; egress proxy on the lab VM |
| Database | memory, or Postgres with `-db` | Postgres 16 | Postgres on the lab VM, or Cloud SQL |
| Event bus (`FF_BUS`) | `mem` | `postgres` (migration 0016, LISTEN/NOTIFY) | `pubsub` (`FF_PUBSUB_PROJECT`, `FF_TOPIC_PREFIX`) |
| Blobs (raw output, snapshots, archives) | memory | `FF_BLOB_DIR` | `FF_ARTIFACT_BUCKET` (Cloud Storage) |
| Signing keys | generated per process | `FF_SIGNING_KID` + `FF_SIGNING_KEY_<KID>` from an env file (`deploy/.local/local.env`) | Secret Manager, mounted as env |
| Schedules and maintenance | in process (engine) | in process (engine), or `migrate maintain` from cron | one Cloud Scheduler job: `POST /internal/schedules:tick` every minute; `FF_MAINTENANCE_INTERVAL=0` |
| Rate limits | in process | in process, or Redis (`--profile redis`) | Redis on the VM, or Memorystore |

`TestRegress_GOPS21_MultiProcessTopology` (services/gateway/gatewayd)
runs the multi-process profile without containers: it builds every binary,
starts each as its own process against one fresh database with the
Postgres bus, and drives the same REST flow as the one-process end-to-end
test.

## Running

```sh
go run ./cmd/flowforge dev                       # gateway on 127.0.0.1:8080 + every service, in-memory
go run ./cmd/flowforge dev -base-port 7100       # scope 7100, engine 7101, tool-runner 7102,
                                                 # egress-proxy 7103, graph 7104, similarity 7105, notifier 7106
eval "$(scripts/devpg.sh start)"
go run ./cmd/flowforge dev -db "$FF_TEST_PG_DSN" # Postgres (migrations are applied first)
```

`flowforge dev` prints every address, including the egress proxy's SOCKS5
listener (`egress-socks5`) and DNS forwarder (`egress-dns`, UDP and TCP on
one port). Each gRPC service also serves
`GET /healthz`, `/readyz` and `/metrics` (Prometheus text) on the same
port (h2c, Cloud Run's HTTP/2 mode); the egress proxy serves them on its
port for non-proxy requests. The internal services trust the gateway's
signing keys (`devstack.Options.TrustKeys`), so the REST API on the gateway
works end to end; to call a service directly in tests, use
`devstack.Start` (which exposes `Stack.Tokens`).

The per-service binaries (`cmd/scope`, `cmd/engine`, `cmd/tool-runner`,
`cmd/egress-proxy`, `cmd/graph`, `cmd/similarity`, `cmd/notifier`) are what
`deploy/Dockerfile` builds; `cmd/migrate` applies `migrations.Apply`.

### Gateway integration (`devstack`)

```go
st, err := devstack.Start(ctx, devstack.Options{})   // or DBDSN, Resolver, ProbePorts, Ports, ...
defer st.Close()
ctx = st.As(ctx, tenantUUID, "user:"+userUUID, auth.RoleAdmin) // identity the clients mint tokens for
prog, err := st.Scope.CreateProgram(ctx, req)       // st.Engine, st.Graph, st.Notifier, st.Similarity, ...
st.Addrs["engine"]                                  // host:port of every service
st.Keys / st.Tokens / st.Verifier                    // the dev signing key, a token source, the verifier
```

## Internal authentication

Every call carries `authorization: Bearer <internal token>`: an EdDSA JWT
minted with `auth.Minter.Internal(auth.InternalGrant{TenantID, Actor, Role})`
(audience `flowforge-internal`, 60 s lifetime). The server interceptor
verifies it with `auth.InternalVerifierConfig` and puts
`platform.Identity{Tenant, Actor, Role}` into the context; handlers read it
with `platform.MustIdentity` and never take tenant or actor from a body.
`x-tenant-id` and `x-actor`, when present, must equal the token's.

A caller sets the identity and lets the client interceptor mint:

```go
ctx = platform.WithIdentity(ctx, platform.Identity{Tenant: t, Actor: "user:" + u, Role: auth.RoleAnalyst})
conn, _ := platform.Dial(addr, platform.ClientOptions{Tokens: platform.NewMinterTokens(minter, clock)})
```

Background work uses `platform.AsService(ctx, tenant, "engine")` (actor
`service:engine`). Health checks need no token.

## Environment (spec 17.5)

| Variable | Used by | Meaning |
|----------|---------|---------|
| `FF_ENV` | all | `dev` or `prod` |
| `FF_LISTEN_ADDR` / `PORT` | all | listen address (default `:8080`) |
| `FF_LOG_LEVEL` | all | debug, info, warn, error |
| `FF_DB_DSN` | scope, engine, graph, notifier, egress-proxy | Postgres; unset means in-memory stores |
| `FF_REDIS_ADDR`, `FF_REDIS_AUTH` | engine, egress-proxy | distributed limiter (else in-process GCRA; the proxy's per-target limit) |
| `FF_REDIS_CA` | engine, egress-proxy (and gateway) | PEM of the Redis server CA, or a path to it; set means TLS verified against only that CA (Memorystore, `redisconf`) |
| `FF_JWKS_URL`, `FF_JWT_ISSUER`, `FF_JWT_AUDIENCE` | all | verification keys (`https://`, `http://` or `file://`) |
| `FF_SIGNING_KID`, `FF_SIGNING_KEY_<KID>` | all callers | the internal-token key: kid, and its 32-byte Ed25519 seed (hex, base64 or raw) under the name Terraform mounts `ff-signing-key-<kid>` as (kid upper-cased, `-` as `_`). `FF_SIGNING_KEY` is still read when `FF_SIGNING_KEY_<KID>` is unset. It is the gateway's provisioned key, so the gateway's JWKS publishes it |
| `FF_SECRET_PROJECT` | notifier | default project of short `sm://NAME` references (else `FF_PUBSUB_PROJECT`) |
| `FF_ID_TOKEN_AUDIENCE_AUTO` | all callers | attach a Google ID token (`x-serverless-authorization`) to calls to `https://` addresses; defaults to on when `K_SERVICE` is set (Cloud Run) |
| `FF_SCOPE_ADDR`, `FF_ENGINE_ADDR`, `FF_GRAPH_ADDR`, `FF_TOOL_RUNNER_ADDR`, `FF_NOTIFIER_ADDR`, `FF_SIMILARITY_ADDR` | callers | service addresses |
| `FF_EGRESS_PROXY` | tool-runner, notifier | `http://host:port` or `https://host` of the egress proxy (required in prod) |
| `FF_EGRESS_PORTS`, `FF_EGRESS_INTEL`, `FF_EGRESS_SOCKS_LISTEN`, `FF_EGRESS_SOCKS_ADVERTISE`, `FF_EGRESS_DNS_LISTEN`, `FF_EGRESS_DNS_ADVERTISE`, `FF_EGRESS_DNS_UPSTREAM` | egress-proxy | port policy, intel allowlist, SOCKS5 and DNS listeners (services/egressproxy/README.md) |
| `FF_NOTIFY_ALLOWLIST` | egress-proxy | destination hosts of the notifier's webhook and Slack deliveries (notify path, G-SVC-08) |
| `FF_ENGINE_WORKERS`, `FF_FUEL_DEFAULT` | engine | worker count, VM fuel |
| `FF_BUS` | all, gateway | `mem`, `postgres` or `pubsub`; empty: `pubsub` with `FF_PUBSUB_PROJECT`, else `postgres` with `FF_DB_DSN`, else `mem` |
| `FF_PUBSUB_PROJECT`, `FF_TOPIC_PREFIX`, `FF_PUBSUB_CREATE` | all, gateway | Pub/Sub project, name prefix (`infra` `topic_prefix`), create missing topics and subscriptions (emulator) |
| `FF_ARTIFACT_BUCKET`, `FF_BLOB_DIR` | tool-runner, graph, engine | blob store: Cloud Storage bucket, else a local directory, else memory (raw_ref objects, snapshot files, run archives) |
| `FF_MAINTENANCE_INTERVAL` | engine | in-process retention pass interval with Postgres (default `1h`; `0` disables: the tick runs it) |
| `FF_TICK_OIDC_AUDIENCE`, `FF_TICK_SERVICE_ACCOUNT` | engine | accept Cloud Scheduler's Google ID token on `POST /internal/schedules:tick` (besides internal tokens) |

In dev without `FF_JWKS_URL` and a signing key a binary generates an
ephemeral key, which only works inside one process (`flowforge dev`).
Service addresses: `https://host[:port]` (a Cloud Run URL) is dialled with
TLS (system roots, port 443 by default) and, with ID tokens on, carries a
Google ID token whose audience is `https://host` next to the internal JWT
(Cloud Run IAM admits the calling service, the JWT says which tenant and
actor it acts for); `host:port` and `http://host:port` are h2c.

## Schedules (G-OPS-10)

The gateway exposes `POST/GET /v1/schedules` and `GET/DELETE
/v1/schedules/{id}` (create and delete need `runs:submit`, analyst and
above) and forwards them over HTTP to the engine's JSON API on its port
(`/internal/schedules`, same internal token as gRPC; the proto
EngineService is unchanged). A schedule is a five-field UTC cron
expression, a workflow (name, optional version and program) and its input.
The engine stores schedules (engine.schedules, migration 0017; memory
without Postgres) and fires due ones every 15 s in process, and on every
`POST /internal/schedules:tick`. Each firing submits a run with
idempotency key `schedule:<id>:<fire time>`, so two engines (or the loop
and a tick) firing the same minute create one run; firings missed while
no engine ran are coalesced into one. On GCP one Cloud Scheduler job calls
the tick every minute with an OIDC token (`FF_TICK_OIDC_AUDIENCE`,
`FF_TICK_SERVICE_ACCOUNT`), so schedules created at runtime need no new
Cloud Scheduler jobs; the tick also runs a maintenance pass.
`POST /internal/schedules/{id}:fire` (spec 17) still works for
operator-made Cloud Scheduler jobs.

## Maintenance (G-OPS-03, G-ENG-04)

`services/maintenance` applies spec 10.7: scope_decisions partitions
created three months ahead and dropped after 13 months; run_events of runs
finished more than 400 days ago archived to the blob store as zstd JSON
Lines (`archives/run_events/<tenant>/<run>.jsonl.zst`, recorded in
engine.run_archives) and the whole run deleted; snapshots of finished runs
removed (the engine also deletes them when a run finishes); observations
after 90 days; auth_audit after 12 months; published outbox rows after
7 days; processed_events after 30 days; acked and dead Postgres-bus rows;
ended throttle windows. Every job is idempotent and bounded per pass, and
passes are serialized by an advisory lock. It runs in the engine
(`FF_MAINTENANCE_INTERVAL`), as `migrate maintain`, and on each tick.
Runs are never archived to the in-memory blob store.

## Errors, deadlines, telemetry

* `core.Error` -> gRPC status with `google.rpc.ErrorInfo{reason, domain, metadata.request_id}`
  (spec 11.8); RESOURCE_EXHAUSTED always carries `RetryInfo`; DSL
  diagnostics travel as `flowforge.v1.Diagnostic` details; INTERNAL never
  reveals the cause. Clients get the reverse mapping (`platform.FromStatus`).
* Deadlines when the caller set none: unary 5 s, `ScopeService.Check` and
  `CheckBatch` 500 ms, streams 10 min, keepalive 30 s.
* Logs: JSON, one line per event with ts, level, service, env, trace_id,
  span_id, request_id, tenant_id, run_id, effect_path, msg. W3C
  `traceparent` is propagated through gRPC metadata and envelopes (no span
  exporter: there is no OpenTelemetry SDK in the module).
* Metrics (`/metrics`): ff_runs_total{status}, ff_run_duration_seconds,
  ff_effects_total{kind,outcome}, ff_scope_decisions_total{decision,reason},
  ff_limiter_denied_total{level}, ff_replay_nondeterminism_total,
  ff_agent_tokens_total, ff_agent_plan_rejected_total,
  ff_outbox_lag_seconds{topic}, ff_graph_ingest_seconds,
  ff_approvals_pending, plus ff_egress_refused_total{reason},
  ff_alerts_total, ff_deliveries_total, ff_bus_dead_letters_total,
  ff_rpc_requests_total{method,code}, ff_rpc_duration_seconds, and the
  SLO histograms of `infra/modules/monitoring`: ff_scope_check_seconds
  (one scope Check, decision log write included) and
  ff_run_submit_to_start_seconds (run_submitted to run_started event
  times), plus ff_graph_pending_observations_total{event}. The engine logs
  `msg="approval.changed"` with `status` (requested, approved, rejected,
  expired, cancelled) and `role` for the ff_approvals log metric (G-OPS-04).

## Tests

```sh
go test -race ./services/... ./cmd/...                 # in memory
eval "$(scripts/devpg.sh start)"
go test -race ./services/...                           # + Postgres variants (fresh database per test)
scripts/devpg.sh stop
```

`services/devstack/e2e_test.go` is the end-to-end test: a program whose
scope allows `app.test` (mapped to a local httptest server by an injected
resolver), the recon workflow (dnsresolve + httpprobe through the egress
proxy), a run to success, URL/FQDN/IPAddress/Service/Product entities in
the graph, a signed webhook alert to a local receiver, and proof that the
out-of-scope `evil.test` was never resolved or contacted. It runs on memory
and, with `FF_TEST_PG_DSN`, on Postgres. `services/egressproxy/proxy_test.go`
is the M7 mock-target test (allowed request, redirect to an out-of-scope
host, DNS rebinding, userinfo and decimal-IP tricks); `v2_test.go` covers
TCP ports, SOCKS5, the DNS forwarder, per-target rate limits and intel
sources. Tool adapters run against recorded fixtures in
`conformance/tools/`.

## What is simplified or left undone

* **Event bus**: a standalone binary without `FF_DB_DSN` or
  `FF_PUBSUB_PROJECT` gets the in-process bus, which reaches only its own
  process; give the processes a shared database (or Pub/Sub). See
  `internal/bus/README.md` for the adapters' differences.
* **Memory mode**: the graph publishes changes inline (no outbox), and the
  engine and scope outboxes are in memory, so a crash loses unpublished
  events (Postgres mode uses the outbox tables and relays).
* **Similarity** keeps vectors in memory (BruteForce index per tenant,
  program and model; no pgvector, no persistence), and the bundled
  embedder is a deterministic feature-hashing embedder, not a semantic
  model. A Vertex AI / Genkit embedder is future work.
* **Notifier**: throttle windows and open digests are persisted with the
  alert events (notifier.throttle_windows, notifier.digest_buffers) and
  restored when a processor is rebuilt (restart or rule change); several
  notifier replicas each cache their own copy, so two replicas can each
  fire once for the same window (run one replica, or partition
  consumers). Default operational rules are not installed
  per tenant; `TestRule` with a historical event id is not supported
  (events are not retained); webhook and Slack deliveries go through the
  egress proxy's notify path when the notifier has `FF_EGRESS_PROXY`
  (G-SVC-08), else directly (SSRF checked and pinned).
* **Egress proxy** reads the scope decision log directly (in process, or
  the scope schema in Postgres); HTTPS is tunnelled, so redirects inside
  TLS are re-checked by the tool runner's client only. Tickets and the
  concurrency cap are per replica; the per-target rate is shared through
  Redis when `FF_REDIS_ADDR` is set.
* **Tool runner**: tools run in-process (no sandbox); `dnsresolve` falls
  back to the runner's resolver when the proxy has no DNS forwarder;
  the proto
  ToolSpec carries no retry policy, so every tool gets
  `convert.DefaultToolRetry`; httpprobe returns the prelude `Service`
  record (URL, status, title and products go to the graph).
* **Engine** in memory uses a copy of enginetest's MemRepo/MemTools (test
  support may not be imported by services). Runs without a program keep no
  assets (the graph is per program).
* **Graph analytics** re-build a snapshot in memory when the cached one is
  older than 30 s or the requested watermark; with a blob store each new
  watermark is also written as a snapshot file (`snapshots/...ffcs`) and
  catalogued, so `GetSnapshotInfo` reports its URI.
* **Raw tool output**: with a blob store the tool runner stores each
  observation as the tool reported it (`raw/<tenant>/<program>/<run>/<obs>.json`)
  and sets `raw_ref`; adapters do not keep protocol-level raw bytes (HTTP
  bodies, DNS packets) yet.
* Services listen in h2c and rely on Cloud Run's front end for TLS
  (callers dial `https://` addresses with TLS); no authorization
  beyond tenant isolation, the global kill switch (owner) and similarity's
  include_out_of_scope (analyst+): permissions are the gateway's job.
