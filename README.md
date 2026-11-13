# Flowforge (Go reference implementation)

Flowforge is a durable workflow engine with a typed DSL, a bytecode VM and
scope-enforced tool execution. Sentinel, its asset graph, follows the
OWASP Amass Open Asset Model. This repository is the Go reference
implementation of the specification in
[`docs/flowforge-spec.html`](docs/flowforge-spec.html). A plain-text copy
with stable line numbers is in [`docs/spec.txt`](docs/spec.txt).

A program's scope bounds everything. No tool touches a target unless the
scope service allowed that exact normalized target, the decision is
recorded in the run's append-only event log, and the egress proxy dials
only the IPs pinned by that decision.

## Quick start

```bash
go run ./cmd/flowforge dev
```

This starts the REST gateway on `http://127.0.0.1:8080/v1` and every
internal gRPC service on loopback ports, with in-memory stores. Add
`-db postgres://...` to use Postgres (migrations are applied), and
`-log debug` to see the verification e-mails the dev mail sender logs.

A typical session through the public API (spec 12):

1. `POST /v1/auth/signup` → 202. In dev, the verification link is in the
   debug log.
2. `POST /v1/auth/verify-email`, then `POST /v1/auth/login`, which returns
   an access token.
3. `POST /v1/programs` with a scope document (spec 4.1).
4. `POST /v1/programs/{id}/scope:check` dry-runs a decision.
5. `POST /v1/workflows:check` returns diagnostics in the 7.7 format.
   `POST /v1/workflows` compiles, verifies and publishes.
6. `POST /v1/runs` with an `Idempotency-Key` header. Then
   `GET /v1/runs/{id}`, `GET /v1/runs/{id}/events` and
   `GET /v1/runs/{id}/watch` (server-sent events).
7. `GET /v1/graph/entities?program_id=...` and the other graph, alert and
   approval endpoints.

`services/gateway/gatewayd/e2e_test.go` runs exactly this flow against the
real stack.

## What is implemented

Every package listed in `modules.toml` passes
`go run ./cmd/xtask verify-module <name>`. That command checks for doc.go,
a README, the spec's required test ids, conformance data, property and
fuzz tests, a committed benchmark baseline, gofmt, vet, tests and the
architecture rules.

| Area | Packages | Spec |
|---|---|---|
| Foundations | `core` (ids, Clock/Rng ports, errors, canonical JSON), `core/netnorm` (target normalization), `testkit` | 1.2, 4.2, 24.4 |
| Scope | `scope`: normalization, indexed allow/deny matcher, decision algorithm, windows, IP pinning | 4 |
| Graph model | `oam`: OAM assets, relations, properties, observations, convergent merge | 5, 6, 23.5 |
| Graph store | `graphstore` (memory), `graphstore/pg` (Postgres): ingest, neighbours, path, diff, trigram search, change feed, staleness, CSR snapshots | 5.7, 5.9, 10.5 |
| Analytics | `csr`: CSR snapshots, FFCS format, BFS, Dijkstra, PageRank, SCC/WCC, k-core, cuts, triangles | 21 |
| Similarity | `vecindex`: distance kernels, PQ, Vamana, FFDA segments, segmented index with tombstones, RRF, SimHash, MinHash/LSH | 22 |
| Language | `dsl` (front end), `compiler` (lowering, tree-walking reference evaluator), `bytecode` (FFBC format, builder, verifier), `vm` (interpreter, fuel, effects, par_map, compensation, CBOR snapshots) | 7, 8 |
| Engine | `eventstore` (+ `pgstore`), `engine` (+ `pgrepo`): run state machine, leases and fencing, replay, retries, approvals, sagas, cancellation, deadlines, kill switch, submit idempotency | 9, 23.4 |
| Rate limiting | `limiter` (+ `redislimiter`): GCRA (mutex and lock-free), token bucket, sliding windows, leaky bucket, AIMD, WFQ, leased budgets, CRDT counter, the spec's Redis Lua script | 14 |
| Auth | `auth`: Argon2id, EdDSA JWT and JWKS, refresh rotation with reuse detection, API keys, RBAC | 13 |
| Alerts | `alerts`: rule language and checker, dedupe/throttle/digest, quiet hours, signed webhooks, SSRF checks, retry schedule | 15 |
| Placement | `placement`: rendezvous and consistent hashing, heartbeat membership | 23.3 |
| Simulation | `sim`: deterministic fault-injection cluster plus the inv_01–inv_11 checker | 23.8, 25.7 |
| Contracts | `proto/` (gRPC, generated into `gen/`), `migrations/sql/` (Postgres schema) | 10, 11 |
| Services | `services/*`, `cmd/*`: gateway (REST and identity), scope, engine, tool-runner (dnsresolve, httpprobe, tcpconnect, mock), egress proxy, graph and analytics, similarity, notifier, event bus with outbox relay | 3, 6, 11, 12, 15 |
| Deployment | `infra/` (Terraform for GCP), `deploy/` (Dockerfile, compose), `.github/workflows/` | 17, 18, 25.11 |

Deliberately not implemented: the agent step of spec 16 (see
[`docs/DECISIONS.md`](docs/DECISIONS.md)). Every library module in
`modules.toml` except `core` and `core/netnorm` has an `OPEN_QUESTIONS.md`,
and so does the gateway; the other services, adapters and commands keep
theirs in their README's limitations. They record each place the spec was
ambiguous, the behaviour chosen (fail closed for anything touching scope),
and what is simplified or left undone; `docs/GAPS.md` is the register of
the open gaps. The main simplifications:

- There is no Pub/Sub adapter. An in-process bus stands in for it.
- Tools run in-process, without the Cloud Run or gVisor sandbox.
- Secret Manager, a real email sender and a semantic embedder exist only
  as interfaces.
- The similarity service is in-memory only.

## Working on the code

```bash
make test          # go test ./...
make race          # go test -race ./...
make arch          # layer and determinism rules (internal/archtest)
make verify        # xtask verify-module for every module
make devpg-up      # throwaway Postgres 16 for integration tests
```

Then export the `FF_TEST_PG_DSN` that `make devpg-up` prints. Set
`FF_TEST_REDIS_ADDR` too, to run the Postgres and Redis integration tests.

Per module, with `xtask`:

```bash
go run ./cmd/xtask list
go run ./cmd/xtask verify-module scope
go run ./cmd/xtask bench-module scope
go run ./cmd/xtask fuzz-smoke -fuzztime 60s
go run ./cmd/xtask sim --profile chaos --seeds 200
```

The rules for code, tests, naming and layering are in
[`docs/CONVENTIONS.md`](docs/CONVENTIONS.md). Pure packages (layers 1–5)
never read the clock, randomness or the environment. They take
`core.Clock`/`core.Rng` ports instead, which is what makes replay and the
simulator deterministic.

## Porting

[`docs/PORTING_RUST.md`](docs/PORTING_RUST.md) explains how to build the
Rust implementation against the same conformance data. It covers the
formats that must match byte for byte (ids, canonical JSON and CBOR, file
formats, hashes) and the per-module decisions a port must reproduce.
