# services/internal/platform

Shared wiring: `Config`/`ConfigFromEnv` (spec 17.5, read in cmd only) and
`LoadKeys`; `NewGRPCServer` / `Dial` with the interceptor chains (internal
token -> `Identity`, request id, traceparent, default deadlines, panic
recovery, `ToStatus`/`FromStatus` error mapping with ErrorInfo and
RetryInfo); `NewLogger` (JSON, spec 18.1 fields); the Prometheus-text
`Registry` and `Metrics`; `Health` (gRPC health, /healthz, /readyz,
/metrics); `Handler` (gRPC and HTTP on one h2c port) and `Serve` (graceful
shutdown).

* `Dial` / `ParseTarget`: `https://host` targets use TLS (system roots,
  port 443) and, with `ClientOptions.IDTokens`, a Google ID token per call
  in `x-serverless-authorization` (Cloud Run IAM, spec 17.3);
  `host:port` stays h2c. `gcpauth.IDTokens` is the production source.
* `Secrets` (`env://`, `file://` via `LocalSecrets`; `sm://` via
  `smsecrets.Store`, Secret Manager), used by the notifier and the
  gateway's key stores.
* `redisconf`: go-redis options from `FF_REDIS_ADDR`/`FF_REDIS_AUTH`/
  `FF_REDIS_CA` (TLS against exactly that CA).
* Signing-key names: `SigningKeyVar` / `SigningKeyEnv` (Terraform's
  `FF_SIGNING_KEY_<KID>`), `DecodeSeed`.
* `Identity.Groups` (the token's `groups` claim) reaches the engine as
  extra `Caller.Roles`.
* Actor kinds: `IsMember` (`user:`, `apikey:`; spec 13.6 roles apply),
  `IsService` (`service:<name>`), and `IsOperator`: the platform operator,
  `OperatorActor` (`service:operator`) on the nil tenant (`Operator()`),
  the only identity the scope service lets set the global kill switch
  (GAPS G-SEC-02).
