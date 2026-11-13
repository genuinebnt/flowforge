// Package identity is the identity module hosted by the Flowforge gateway
// (spec sections 10.2, 13 and 14.3): tenants, users, memberships, invites,
// email verification, password reset, sessions (refresh-token families with
// reuse detection), API keys, signing keys with rotation, the auth audit
// log and the auth.events outbox.
//
// The package composes the primitives of package auth with a repository
// port (Store) and small ports for secrets (KeyStore), e-mail (EmailSender),
// token revocation (Revocations) and rate limits (LimiterFactory, applied
// per call by RateLimiter). In-memory
// implementations of every port live here; Postgres and Redis adapters live
// in the pgstore and redisstore sub-packages.
//
// Client-facing failures are *core.Error values carrying a spec 11.8
// reason, so the gateway maps them exactly like gRPC statuses.
package identity
