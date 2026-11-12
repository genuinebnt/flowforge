# eventstore/storetest: open questions and decisions

1. **Imports testkit.** The suite needs seeded randomness (`FF_SEED`), the
   case count (`FF_PROPTEST_CASES`) and the conformance loader; it is test
   support code like testkit and is imported only from `_test.go` files.
2. **Time without sleeping.** Postgres cannot fake `now()`. Instead of real
   sleeps with short TTLs, `Harness.Elapse(d)` shifts every stored lease and
   wake timestamp back by d, which is equivalent for every comparison
   against `now()`. Event timestamps are not shifted (`run_events` is
   append-only) and the suite never compares them with lease times.
3. **Exact fence values are not asserted**, only strict increase
   (`fence_greater_than` in the scripts), so a replicated log with
   term-based fences passes the same data.
4. **Model timings.** The property uses a 10 s TTL, 4 s parked wake offsets
   and 3 s/12 s elapse steps, so model time never equals a deadline and the
   milliseconds of real time a Postgres run takes cannot change an outcome.
5. **Fence forgery is out of scope.** The fence is the write credential;
   the property's workers use only fences they were granted. "Each fence
   value has one writer" is checked over the stored log.
6. **ListReady results are compared as sets** in the property (runs created
   in the same model instant have distinct database timestamps, so ties
   order differently); the scripts and ReadyAndClaim check the order where
   wake times differ.
7. **Isolation.** A harness must start empty because ListReady and
   ClaimReady see every run; the Postgres harness truncates the engine
   tables. The property finishes its runs (status succeeded) after each
   iteration so later iterations see only their own ready runs.
