// Package limiter is the Flowforge rate-limiting library (spec section 14):
// a language-neutral contract and several algorithms that can be compared
// on the same timelines.
//
// The contract (14.1) is the Limiter interface: Acquire(key, cost, now),
// AcquireAll for several keys at once (atomic: if any key denies, none is
// charged and RetryAfter is the largest denial) and Configure(pattern,
// params). Time is always an argument, a monotonic reading such as
// core.Clock.Monotonic(); no algorithm reads a clock.
//
// Algorithms (14.2), one file each:
//
//   - GCRA, the default, and AtomicGCRA, its lock-free sharded variant
//     with identical decisions;
//   - TokenBucket (fixed-point tokens);
//   - SlidingWindowCounter (two fixed windows, approximate) and
//     SlidingWindowLog (exact baseline);
//   - LeakyBucket (shaping: admitted requests carry a release Delay);
//   - AIMD, an adaptive per-target rate controller;
//   - WFQ, weighted fair queueing across tenants.
//
// Keys and hierarchy (14.3): GlobalKey, TenantKey, ProgramKey, HostKey,
// ToolKey, AuthIPKey, AuthAccountKey and APIKey build the rl:... keys, and
// ToolCall.Requests lists the keys a tool call acquires atomically.
//
// Distributed strategies (14.4): LeasedBudget spends chunks leased from a
// central Store (MemStore is the in-memory fake); CRDTLimiter keeps
// gossiped GCounters per window; the Redis script lives in the adapter
// package limiter/redislimiter. Failover applies the outage policy of
// 23.2 and 23.8: deny active work, serve passive work from a local
// conservative limiter.
//
// Robustness: a clock reading earlier than one already seen never yields
// extra allowance (lim_003), a cost above a key's capacity is an error
// with RetryAfter Never rather than an endless wait (lim_004), keys
// without a matching rule are an error (fail closed), and arithmetic is
// overflow-free for all times within ±MaxTime.
package limiter
