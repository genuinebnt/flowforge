// Package gateway is the public REST gateway of Flowforge (spec 12): every
// endpoint of 12.3 and the 22.9 additions over net/http, authenticated by
// access JWTs, ffk_ API keys or console cookies (spec 13), authorized with
// the 13.6 role matrix before any internal call, and forwarded to the
// internal gRPC services with a 60-second internal token and the spec
// 11.1 metadata (client.go).
//
// Cross-cutting behaviour lives in one place each: the request pipeline
// (pipeline.go: request ids, security headers, body limit, ":verb"
// routing, authentication, rate limits, CSRF, tenant cross-checks,
// idempotency), JSON <-> proto conversion (convert.go) and the error model
// of 12.2 mapped from gRPC statuses and identity errors (errors.go).
// Handlers are thin.
//
// The identity module the gateway hosts is in the identity sub-package.
package gateway
