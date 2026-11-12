// Package redislimiter is the central (Redis) distributed strategy of spec
// 14.4: GCRA for several keys in one atomic Lua script, using Redis TIME
// as the clock so that every node agrees.
//
// It is an adapter: the pure limiter package never imports it. Errors from
// Redis wrap limiter.ErrUnavailable so that limiter.Failover can apply the
// outage policy (deny active work, local budgets for passive work; spec
// 23.8).
package redislimiter
