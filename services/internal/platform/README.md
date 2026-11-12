# services/internal/platform

Shared wiring: `Config`/`ConfigFromEnv` (spec 17.5, read in cmd only) and
`LoadKeys`; `NewGRPCServer` / `Dial` with the interceptor chains (internal
token -> `Identity`, request id, traceparent, default deadlines, panic
recovery, `ToStatus`/`FromStatus` error mapping with ErrorInfo and
RetryInfo); `NewLogger` (JSON, spec 18.1 fields); the Prometheus-text
`Registry` and `Metrics`; `Health` (gRPC health, /healthz, /readyz,
/metrics); `Handler` (gRPC and HTTP on one h2c port) and `Serve` (graceful
shutdown).
