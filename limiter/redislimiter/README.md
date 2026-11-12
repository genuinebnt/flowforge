# limiter/redislimiter

The central distributed strategy of spec 14.4: GCRA for several keys in
one atomic Lua script (the spec's script, verbatim, exported as `Script`),
using Redis TIME as the shared clock. Implements `limiter.ContextLimiter`;
Redis errors wrap `limiter.ErrUnavailable` so `limiter.Failover` can fail
closed for active work (spec 23.8).

```sh
redis-server --port 6391 --save '' --appendonly no --daemonize yes
FF_TEST_REDIS_ADDR=127.0.0.1:6391 go test ./limiter/redislimiter/              # lim_010
FF_TEST_REDIS_ADDR=127.0.0.1:6391 go test -run '^$' -bench . ./limiter/redislimiter/  # b_lim_03
redis-cli -p 6391 shutdown nosave
```

Without `FF_TEST_REDIS_ADDR` the integration test and benchmark are
skipped. Limitations: millisecond resolution (T rounded up), no remaining
count in the reply, and with Redis Cluster all keys of one call must hash
to one slot. `Options.CallerClock` swaps the TIME call for a caller-given
time; it exists for deterministic tests and simulations.
