// Package platform is the shared wiring of the Flowforge internal services
// (spec 3.2, 11.1, 11.8, 17.5, 18.1): configuration structs, gRPC server and
// client constructors with the interceptor chain every call goes through
// (internal-token authentication, request id, W3C traceparent, deadlines,
// panic recovery, error mapping in both directions), the JSON slog logger
// with the 18.1 fields, a tiny Prometheus-text metrics registry, health
// endpoints and graceful shutdown.
//
// It holds no domain logic. Identity always comes from the verified
// internal token (authorization metadata), never from a request body:
// the server interceptor puts an Identity into the context and every
// handler reads it with IdentityFrom or MustIdentity.
package platform
