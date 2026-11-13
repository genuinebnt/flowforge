# services/gateway

The public REST gateway (spec 12) and the identity service it hosts
(spec 13, `identity/`). `cmd/gateway` is the binary.

## Wiring

```go
keys, _ := identity.NewKeyManager(ctx, identity.KeyManagerConfig{Store: st, Secrets: ks, Clock: clk, IDs: rng, Random: rand.Reader, Issuer: iss})
svc, _ := identity.New(identity.Config{Store: st, Keys: keys, Revocations: rev, Limiter: lim, Lockouts: lo, ...})
cc, _ := grpc.NewClient(addr, append([]grpc.DialOption{creds}, gateway.DialOptions(keys)...)...)
srv, _ := gateway.New(gateway.Config{
    Identity: svc, Revocations: rev, Limiter: lim, Clock: clk, IDs: rng, Issuer: iss,
    Backends: gateway.BackendsFromConns(scope, engine, graph, notifier, toolrunner, similarity),
    Cookies: gateway.CookieConfig{Domain: "...", Insecure: false}, Plans: gateway.DefaultPlanLimits(),
})
http.ListenAndServe(addr, srv.Handler()) // or srv.HTTPServer(addr)
```

The gateway talks to internal services only through the generated client
interfaces in `gateway.Backends`. `DialOptions` installs client
interceptors that, for every call, mint an internal JWT
(`Minter.Internal`: aud `flowforge-internal`, 60 s, tenant/actor/role from
the caller's token) and send `authorization: Bearer <token>`,
`x-tenant-id`, `x-actor` (`user:usr_…` / `apikey:key_…`), `x-request-id`,
`traceparent` (client's or generated) and `idempotency-key`, plus the
spec 11.1 deadlines (5 s unary, 500 ms `ScopeService.Check`; SSE streams
are bounded at 10 min). Internal services verify those tokens with the
gateway's JWKS (`FF_JWKS_URL=https://<gateway>/.well-known/jwks.json`,
same `FF_JWT_ISSUER`).

`cmd/gateway` environment: `FF_GATEWAY_ADDR` (or `PORT`), `FF_ENV`, `FF_DB_DSN`
(empty: in-memory identity), `FF_DB_MIGRATE`, `FF_REDIS_ADDR`/`FF_REDIS_AUTH`
(empty: per-process revocation, lockouts and limits), `FF_REDIS_CA` (PEM
or path; set = TLS to Redis), signing keys (below: `FF_SIGNING_KEY_STORE`,
`FF_SIGNING_KID`, `FF_SIGNING_KEY_<KID>`, `FF_SIGNING_KEY_DIR`,
`FF_SECRET_PROJECT`), Argon2id (`FF_ARGON2_MEMORY_KIB`, `FF_ARGON2_TIME`,
`FF_ARGON2_PARALLELISM`, each defaulting to and never below the 13.1
floor; `FF_ARGON2_TARGET_MS` calibrates the iterations at start to that
hash time, clamped to 200-500 ms, and logs the result; hashes with other
parameters are re-hashed at the next login), `FF_ID_TOKEN_AUDIENCE_AUTO`
(ID tokens to `https://` backends; on when `K_SERVICE` is set), `FF_JWT_ISSUER`
(required), `FF_JWT_AUDIENCE` (must be `flowforge-api`), `FF_PUBLIC_URL`,
`FF_COOKIE_DOMAIN`, `FF_COOKIE_INSECURE`, `FF_TRUST_XFF` (the number of
trusted proxies in front of the gateway that append to `X-Forwarded-For`;
the client IP is that many entries from the right, `true` = 1, unset = the
connection address; 1 on a `run.app` URL, 2 behind the external load
balancer in dev/prod, G-SVC-17), `FF_GRPC_TLS`,
and `FF_SCOPE_ADDR`, `FF_ENGINE_ADDR`, `FF_GRAPH_ADDR` (also analytics),
`FF_NOTIFIER_ADDR`, `FF_TOOL_RUNNER_ADDR`, `FF_SIMILARITY_ADDR`.

## Endpoints

All under `/v1`. Permission = spec 13.6 (`conformance/auth/rbac/matrix.json`),
checked before any internal call; `read` = every role.

| Method and path | Permission (min. role) | Backend |
|---|---|---|
| POST /auth/signup, /auth/verify-email, /auth/verify-email:resend, /auth/login, /auth/refresh, /auth/password/forgot, /auth/password/reset, /auth/invites:accept | public | identity |
| POST /auth/logout | any token, or a refresh token | identity |
| GET /me | any | identity |
| GET /tenant/members, GET/PATCH /tenant/members/{user_id}, POST /tenant/invites | members:admin (admin) | identity |
| GET /tenant/role-groups, PUT/DELETE /tenant/role-groups/{name}/members/{user_id} | members:admin (admin) | identity |
| GET, POST /api-keys; DELETE /api-keys/{id} | members:admin (admin) | identity |
| GET /programs, GET /programs/{id}, GET /programs/{id}/scope, GET /programs/{id}/scope/versions | read | scope |
| POST /programs, PATCH /programs/{id}, PUT /programs/{id}/scope, POST /programs/{id}/kill-switch | programs:admin (admin) | scope |
| POST /programs/{id}/scope:check | workflows:write (analyst) | scope |
| POST /workflows:check, POST /workflows | workflows:write (analyst) | engine |
| GET /workflows, GET /workflows/{name}/versions/{v} | read | engine |
| GET /tools | read | tool runner |
| POST /runs, POST /runs/{id}:cancel | runs:submit (analyst) | engine |
| GET /runs, GET /runs/{id}, GET /runs/{id}/events, GET /runs/{id}/watch (SSE) | read | engine |
| POST /schedules, DELETE /schedules/{id} | runs:submit (analyst) | engine (HTTP `/internal/schedules`, G-OPS-10) |
| GET /schedules, GET /schedules/{id} | read | engine (HTTP) |
| GET /approvals, GET /approvals/{id} | read | engine |
| POST /approvals/{id}:decide | approvals:decide (approver) + approval role matches the membership role or a role group | engine |
| GET /graph/entities, /graph/entities/{id}, /graph/entities/{id}/neighbors, /graph/search, /graph/diff, /graph/changes/watch (SSE); POST /graph/paths | read | graph |
| GET /graph/entities/{id}/similar, POST /graph/similar; GET /programs/{pid}/entities/{id}/similar, POST /programs/{pid}/similar | read | similarity (+ graph for hit types) |
| POST /graph/analysis, POST /programs/{pid}/graph/analysis | runs:submit (analyst) | analytics |
| GET /graph/snapshots, GET /programs/{pid}/graph/snapshots | read | analytics |
| GET /programs/{pid}/similarity/index | programs:admin (admin) | similarity |
| GET /alerts/rules, GET /alerts/rules/{id}, GET /alerts/events | read | notifier |
| POST /alerts/rules, PUT/DELETE /alerts/rules/{id}, POST /alerts/rules:test | alerts:write (analyst) | notifier |
| GET, POST /alerts/channels; PUT /alerts/channels/{id} | members:admin (admin) | notifier |
| GET /healthz, /readyz, /.well-known/jwks.json (also at the root) | public | — |

`:verb` custom methods (`/runs/{id}:cancel`, `/workflows:check`, …) are
routed by rewriting a trailing `:verb` into its own path segment before
Go 1.22 `ServeMux` matching (`rewriteVerb`), so patterns stay standard.

## Auth flows

* **Signup** is always `202 {user_id, tenant_id, status:"pending"}`; an
  existing address gets a "someone tried to sign up" mail and fresh,
  unused ids. Passwords: 12-128 chars, breached list, Argon2id (13.1).
  The verification token (32 bytes, SHA-256 stored, 24 h, single use) is
  e-mailed; pending users cannot log in.
* **Login** goes through `auth.LoginService`: unknown e-mails verify a
  dummy hash, failures and throttles answer identically (auth_005);
  throttles `rl:auth:ip:<ip>:login` 20/10 min, `rl:auth:acct:<sha256>`
  10/10 min, lockout after 10 failures for 15 min. Signup 5/h per IP,
  resend 3/h per account, forgot 5/h per IP and 3/h per account.
* **Tokens**: EdDSA access JWT (10 min, `sid` = refresh family, `sv` =
  session version), `ffr_` refresh token (30 d sliding, 90 d absolute),
  rotation with reuse detection (family revoked, session version bumped,
  `auth_audit` + `auth.events` `refresh_reuse`). Revocation: `deny:jti:<jti>`
  and `sv:<usr_…>` in Redis (`redisstore`) or memory.
* **Session version bumps** (refresh reuse, password reset, role change)
  are published to the revocation cache, so older access tokens die at
  once while refresh still works where allowed.
* **API keys** `ffk_<id>_<secret>`: SHA-256 of the secret stored, shown
  once, role ≤ admin, revocable, optional expiry.
* **Signing keys**: `KeyManager` keeps one active key (private half only in
  the `KeyStore`, the table holds `private_key_ref`); the previous one is
  retiring until its tokens expire, then retired and unpublished. Stores
  (`identity/keystore`, `FF_SIGNING_KEY_STORE`; empty picks `env` when
  `FF_SIGNING_KID` is set, `dir` when `FF_SIGNING_KEY_DIR` is, else
  `memory`, which `FF_ENV=prod` refuses):
  * `env` (the Terraform layout): signs with `FF_SIGNING_KID`, seed in
    `FF_SIGNING_KEY_<KID>` (or `FF_SIGNING_KEY`). Every instance and
    restart registers and uses the same kid. Rotation is
    operator-driven: add secret `ff-signing-key-<new>`, add the kid to
    `signing_key_kids`, set `current_signing_kid`, apply; the gateway
    then activates the new kid and the old one retires. `Rotate` returns
    `ErrOperatorRotation` and the 90-day automatic rotation is off.
    Internal services mount the same variables to sign their own calls.
  * `sm`: the same, reading `sm://projects/<FF_SECRET_PROJECT>/secrets/ff-signing-key-<kid>/versions/latest`.
  * `sm-managed`: the gateway generates keys and stores each as secret
    `ff-signing-key-<kid>` (needs secret create rights); automatic 90-day
    rotation.
  * `dir`: key files in `FF_SIGNING_KEY_DIR` (0600, written atomically);
    gateways sharing the directory and database share rotations (local
    multi-process runs).
  * `memory`: dev; every restart rotates to a new key.
  `/.well-known/jwks.json` (`Cache-Control: public, max-age=300`).

## Cookies and CSRF

`login {"use_cookies": true}` sets `ff_at` (HttpOnly, Secure, Lax, `/`),
`ff_rt` (HttpOnly, Secure, Strict, `/v1/auth`) and `ff_csrf` (Secure,
Strict, readable) and omits `refresh_token` from the body. Requests
authenticated by `ff_at` (or refresh/logout via `ff_rt`) that change state
must send `X-CSRF-Token` equal to `ff_csrf` (`403 csrf_failed`); bearer
requests are exempt. Every response carries `Cache-Control: no-store`
(JWKS excepted), a script-free CSP, `nosniff`, `X-Frame-Options: DENY`
and HSTS (unless cookies are insecure).

## Requests and responses

* Strict JSON (`core.DecodeStrict`): unknown fields, trailing data, wrong
  types → `400 validation_failed`; bodies over 1 MiB → `413`.
* Tenant comes only from the token; `tenant_id` in a query or body that
  disagrees → `404 not_found` (an agreeing one is accepted and dropped).
* Error model 12.2: `{"error": {code, status, message, details,
  request_id}}`, `code` = lower-cased reason, mapped from gRPC status +
  `ErrorInfo` (metadata becomes detail fields; `*version`, `*_seq`, `*_ms`,
  `*count` render as numbers), `RetryInfo` → `Retry-After`, Diagnostic
  details for `dsl_diagnostics` (422), `BadRequest` violations; INTERNAL
  hides the message.
* `X-Request-Id`: the client's (`[A-Za-z0-9._-]{1,128}`) or `req_<ULID>`.
* Pagination: `page_size` 1-500 (default 50), opaque `page_token`
  (backend tokens pass through; identity uses keyset cursors),
  `{"items": [...], "next_page_token": "…" | null}`.
* `If-Match` on PATCH /tenant/members, PATCH /programs, PUT /alerts/rules,
  PUT /alerts/channels; GETs return `ETag`. Mismatch → `412
  precondition_failed`. Scope uses `base_version` (409 version_conflict).
* SSE (`/runs/{id}/watch`, `/graph/changes/watch`): `Last-Event-ID` (or
  `after_seq`) resumes, `id:` = seq, `event: run_event | graph_change`,
  `: keep-alive` every 15 s, `Cache-Control: no-store`, a final
  `event: error` with the error body if the upstream fails. A missing or
  foreign run/program is a plain 404 before the stream starts.

## Idempotency

`Idempotency-Key` (1-128 printable ASCII) is required on POST /runs and
honoured on every authenticated POST: stored per (tenant, key) in
`identity.http_idempotency` (or memory) for 7 days with the SHA-256 of
method, path and canonical JSON body. A replay returns the original status
and body with `Idempotent-Replayed: true` (and `Location` for runs); a
different request with the same key, or a duplicate while the first is in
flight, is `409 idempotency_key_reused`. Responses ≥ 500 are not stored.
The key is also forwarded as `idempotency-key` metadata (SubmitRun's own
dedupe, 9.10). Public `/auth/*` POSTs validate but never store the key
(a replayed login would hand out tokens).

## Rate limits

`rl:api:<tenant>:<class>` (classes `read`, `write`, `stream`; `auth` for
public endpoints keyed by client IP as `rl:api:ip/<ip>:<class>`), GCRA
parameters per tenant plan (`Config.Plans`, plan read from
`identity.tenants.plan`, cached 1 min). Every limited response carries
`RateLimit-Limit` (burst), `RateLimit-Remaining`, `RateLimit-Reset`
(seconds to a full bucket); 429 adds `Retry-After`. An unreachable
limiter denies with `503 unavailable` and `Retry-After` (spec 2.4, fail
closed). Probes and JWKS are not limited.

## Tests

```sh
go test -race ./services/gateway/... ./cmd/gateway/...
FF_TEST_PG_DSN=... FF_TEST_REDIS_ADDR=... go test ./services/gateway/...
go test ./services/gateway -run '^$' -fuzz FuzzRESTJSON -fuzztime 20s -fuzzminimizetime 2s
```

* `TestGatewayConformanceAPI` runs `conformance/api/*.json` (HTTP
  transcripts: request, status, headers, JSON shape with placeholders;
  format documented on `Transcript`) against the gateway wired to the
  in-memory identity service and `internal/fakebackend` — real gRPC
  servers on bufconn that verify the internal token and partition data by
  its tenant.
* `TestGatewayRBACMatrix` (every role × every endpoint of the matrix;
  denials never reach a backend), `TestGatewayRouteTableMatchesMatrix`,
  `TestGatewayTenantIsolation` (every get/list with two tenants),
  `TestGatewayAuth005NoEnumeration`, `TestGatewayRefreshCookieFlow`,
  `TestGatewayAPIKeyFlow`, `TestGatewayInternalMetadata`,
  `TestGatewaySSEResume`, `TestGatewaySSEGraphChanges`,
  `TestGatewayErrorMapping`, `FuzzRESTJSON`.
* Identity: `TestIdentity*` in `identity/` (flows, key rotation), the
  Store contract against `MemStore` and Postgres (`pgstore`, needs
  `FF_TEST_PG_DSN`; `scripts/devpg.sh start`), Redis adapters (needs
  `FF_TEST_REDIS_ADDR`).
* The fuzzer's default 60 s minimization makes progress lines stall at
  0 execs/s; use `-fuzzminimizetime`.

## Simplified or missing

* A real e-mail provider is a port only (dev: log mailer). KMS-held
  signing keys are not implemented (Secret Manager is).
* Breached-password check is a small built-in list in `cmd/gateway`.
* The outbox is written (`identity.outbox`, topic `auth.events`,
  including `auth.signup_throttled` and `auth.login_throttled` for the
  default signup-abuse alert rule) but no relay publishes it here.
* PATCH /programs/{id} only changes `status` (active/paused via the
  program kill switch): the scope contract has no update RPC.
* `paused_runs` in PUT scope is `null` (not in `ScopeVersion`; pausing is
  asynchronous). `waiting_on` is derived from the run's pending approval.
* With the Redis limiter `RateLimit-Remaining` is always 0 (the script does
  not report it).
* See [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md).
