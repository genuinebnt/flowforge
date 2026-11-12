# limiter: open questions

Ambiguities in spec section 14 and the choices made. Each choice is pinned
by the test named in brackets.

1. **Initial TAT and negative times.** The spec says the TAT is "initially
   0". A key without history is treated as having a TAT in the distant
   past, which equals 0 for non-negative clocks and stays correct for
   negative monotonic readings. The Redis script keeps the spec's
   `GET or '0'`; Redis TIME is always positive. [TestLim001, TestLim010]

2. **Units and rounding.** Times are nanoseconds. T = ceil(1e9 / rate) so
   rounding never admits more than the configured rate; the Redis adapter
   rounds T up to whole milliseconds for the same reason. Conformance
   `retry_after_ms` is compared as RetryAfter rounded up to a millisecond.
   [TestLimConformance_GCRA, TestMsParams]

3. **Token bucket representation.** The spec says `tokens (float)`; tokens
   are kept in fixed point (1e9 units per token, sub-unit remainder
   carried) so the bucket is exact for integer rates and matches the
   reference model without tolerances. [TestLim002, TestLimProp_MatchesReference]

4. **Backwards clock (lim_003).** Token bucket and sliding windows treat an
   earlier reading as the latest one seen (no refill, no window roll).
   GCRA needs no clamp: after any call at reading M its TAT is at least M,
   so an earlier reading only makes `now < allow_at` more likely. What is
   tested is the bound: admitted requests placed at the latest reading
   obey `burst + rate*w`. (Counting admissions against a clamped replay is
   not a valid criterion: a stricter early denial can leave budget for
   later admissions.) [TestLim003]

5. **Cost above capacity (lim_004).** Returned as `ErrCostExceedsCapacity`
   with `RetryAfter == Never`, never as a long retry. Cost below 1 is
   `ErrInvalidCost`. Duplicate keys in one AcquireAll are merged by summing
   their costs, so the merged cost is checked against capacity. [TestLim004]

6. **Unconfigured keys.** A key matching no rule is `ErrNoRule`, not a
   default allow (spec 2: "an unreachable ... rate limiter ... means deny").
   [TestLimInputValidation]

7. **Sliding window counter vs lim_005.** "Never exceeds the configured
   limit in any window" holds exactly only for the log. The counter blocks
   the start of the next window after a full window ends (tested on the
   boundary timeline) but in general guarantees `L + (L/W)*w`. Windows are
   aligned to multiples of W from the monotonic origin; W defaults to
   Burst/Rate. [TestLim005, TestLimProp_WindowBound]

8. **Leaky bucket API.** Modelled as virtual scheduling: admission equals
   GCRA, and an admitted request carries `Result.Delay` (an extension of
   the 14.1 Result) until its release; callers wait with their own timers.
   Denied means the bounded queue is full. [TestLimLeakyBucket_Delay]

9. **AIMD decrease condition.** "if any 429/timeout/5xx-rate > threshold"
   is read as: any 429, or any timeout, or a 5xx fraction strictly above
   the threshold. This backs off from targets more readily than a combined
   error rate would. The initial rate defaults to MinRate (probe upwards).
   Retry-After suspends Acquire until it passes; suspensions only extend.
   [TestLim006, conformance/limiter/models/aimd.json]

10. **WFQ virtual time.** "Advance V to the start time of the one served" is
    applied as V = max(V, start): serving by smallest finish can pick a
    request with an earlier start, and V must not move backwards.
    Unweighted tenants are rejected unless a DefaultWeight is configured.
    [TestLim007, TestLimWFQ_Errors]

11. **Hierarchy keys.** Components are escaped (`%` → `%25`, `:` → `%3A`)
    so IPv6 targets and identifiers with colons cannot collide. HostKey
    expects the registrable domain or IP already computed (for example
    with `core/netnorm.RegistrableDomain`); it only lowercases. The account
    key hashes the trimmed, lowercased email. "Program rate, divided if
    several tools target the same host" is a configuration concern left to
    the caller. A ToolCall without a target skips rl:h; any other missing
    level is an error. [TestLimKeys, TestLim008]

12. **Leases (lim_011).** The store never reclaims an expired lease by
    itself (it cannot know what was spent); only the holder's Return
    credits unused tokens, after the holder has closed the lease locally.
    Unreturned leases are forfeited (undershoot), and the store forgets
    them one TTL after expiry. `Margin` stops spending before expiry to
    absorb clock skew. [TestLim011]

13. **CRDT window.** The CRDT limiter keeps G-counters for the current and
    previous windows and applies the sliding-window-counter estimate, so a
    single node obeys the same bound as SlidingWindowCounter; a plain fixed
    window would allow 2L across a boundary. [TestLimProp_WindowBound,
    TestLimCRDT]

14. **Redis timelines (lim_010).** The production script uses Redis TIME,
    so a deterministic timeline cannot be replayed against it. The test
    (a) runs the script with only its clock line replaced by
    `tonumber(ARGV[#ARGV])` (`Options.CallerClock`) and requires exact
    agreement with the in-memory GCRA, and (b) checks the verbatim script
    on a burst, which does not depend on elapsed time. The script returns
    no remaining count, so `Result.Remaining` is 0 for Redis. With Redis
    Cluster, the keys of one call must share a hash slot.

15. **Loom (lim_009).** Go has no loom; the test is a `-race` stress test
    with 2, 3 and 8 goroutines and unordered clock readings, plus a Prune
    race test, asserting total admitted <= burst + rate * elapsed.
