# Flowforge Go implementation conventions

This is the Go reference implementation of the Flowforge specification
(`docs/flowforge-spec.html`; a plain-text extraction with stable line numbers
is in `docs/spec.txt`). These rules translate the spec's Rust-flavoured
module contract (sections 24 and 25) into idiomatic Go.

## Layout

One Go module, `github.com/genuinebnt/flowforge` (Go 1.26). The spec's
"crates" are Go packages at the repository root. Go only compiles the
packages a test imports, so `go test ./scope/...` never builds pgx or gRPC,
which satisfies spec 24.5 without a multi-module `go.work`.

| Layer | Packages | May import (besides stdlib and lower layers' listed packages) |
|------:|----------|------|
| 0 | `core`, `core/netnorm` | stdlib only (`core/netnorm` also uses `golang.org/x/net` for IDNA and the public suffix list) |
| 1 | `scope`, `oam`, `limiter`, `csr`, `placement`, `auth` | `core`, `core/netnorm` (+ `golang.org/x/crypto` for auth) |
| 2 | `dsl`, `bytecode`, `vecindex`, `alerts` | `core`; `dsl` may import `scope` and `oam` for types only; `alerts` may import `oam` |
| 3 | `vm`, `eventstore` | `core`, `bytecode` (vm); `core` (eventstore). CBOR via `github.com/fxamacker/cbor/v2` |
| 4 | `engine`, `agent` | layers 0-3 |
| 5 | `sim` | everything above (test harness) |
| 6 | `services/...` | everything; the only place gRPC, HTTP, Postgres and Redis are wired |
| 7 | `cmd/...` | everything, including `services/...` (each main starts a service) |

Adapters with heavy dependencies live in sub-packages so the pure package
stays light: `eventstore/pgstore` (pgx), `limiter/redislimiter` (go-redis),
`vecindex/pgvector`, etc. A pure package never imports its adapter.

`testkit` is imported only from `_test.go` files.

## Code rules

* **No ambient time or randomness.** Only `core.SystemClock` and
  `core.SystemRng` may call `time.Now`, `time.Since` or `crypto/rand`;
  packages in layers 1-4 take a `core.Clock` / `core.Rng` (or a plain
  `time.Time` argument) instead. `math/rand` and `math/rand/v2` global
  functions are forbidden outside tests. The arch test enforces this.
* **No globals, no env reads** in layers 0-5. Configuration arrives as a
  struct.
* **Errors**: each package declares sentinel errors
  (`var ErrX = errors.New("scope: ...")`) or typed errors, wraps with `%w`,
  and never maps to gRPC/HTTP codes (services do that, spec 11.8).
  Library code does not panic on bad input; `panic` is reserved for
  programmer errors (impossible states), never for data.
* **Docs**: every exported identifier has a doc comment. Each package has a
  `doc.go` (package comment naming the spec sections it implements) and a
  short `README.md` (purpose, spec sections, how to run tests and
  benchmarks, known limitations), as spec 24.6 asks.
* Accept interfaces, return concrete types. Define a port interface only at
  a real boundary (spec 24.1); ship an in-memory fake for every port a
  package exposes.
* Pass `context.Context` as the first argument of anything that does I/O or
  may block. Pure functions take no context.
* Keep `gofmt` and `go vet` clean.

## Tests

* **Required case ids** (spec 25.3) map to Go test names as
  `Test<Prefix><NNN>_<Description>`, for example `scope_001` →
  `TestScope001_TrailingDot`, `lim_001` → `TestLim001_GCRAVector`,
  `vm_004` → `TestVM004_Replay`, `bc_002` → `TestBC002_VerifierRejects`.
  `cmd/xtask verify-module` greps for `func Test<Prefix><NNN>`. Use
  subtests for variants.
* **Property tests** (spec 25.4) are ordinary tests named
  `Test<Prefix>Prop_<Name>` (`scope_prop_idempotent_normalize` →
  `TestScopeProp_IdempotentNormalize`). They draw from `testkit.Rng(t)`
  (honours `FF_SEED`) and run `testkit.Cases()` iterations (1,000 by
  default, `FF_PROPTEST_CASES` for nightly).
* **Fuzz targets** (spec 25.5) are native Go fuzz functions,
  `fuzz_scope_normalize` → `func FuzzScopeNormalize(f *testing.F)`, with a
  seed corpus via `f.Add`. Crashers go into `testdata/fuzz/` (Go's default)
  and stay as regression inputs.
* **Benchmarks** (spec 25.8): `b_scope_01` → `func BenchmarkScope01_...(b
  *testing.B)` using `for b.Loop()`, and `b.ReportAllocs()`. Baselines are
  recorded by `cmd/xtask bench-module` under `bench/results/<package>/`.
* **Conformance data** lives in `conformance/<suite>/*.json` (spec 19, 25.9)
  and is read with `testkit.LoadCases[T](t, "<suite>")`. A package never
  hard-codes expected values that also exist as data.
* Deterministic: no sleeping on the wall clock, no network in unit tests.
  Integration tests that need Postgres or Redis are skipped unless
  `FF_TEST_PG_DSN` / `FF_TEST_REDIS_ADDR` are set.
* Spec ambiguities: record them in `<package>/OPEN_QUESTIONS.md`, choose
  the most conservative behaviour (fail closed for anything touching scope)
  and pin the choice with a test.

## Pluggable algorithms

When several algorithms or strategies were benchmarked and one was chosen,
**all candidates stay in the code**, so they can be swapped in and their
behaviour compared:

* One interface (or a small enum plus a constructor) in **non-test code**,
  with each candidate as an implementation. Reference or brute-force
  implementations used as test oracles are also non-test code when they
  are a meaningful choice (for tiny inputs, or for learning). Pure test
  doubles stay in tests.
* Selection is by configuration (a `Config`/`Options` field, or a CLI or
  env setting in services). The default is the chosen algorithm.
* One comparative benchmark per decision point, named
  `Benchmark<Pkg>Algo/<candidate>/<params>`, so
  `go test -run=^$ -bench <Pkg>Algo ./<pkg>` prints a direct comparison.
* Every decision point has an entry in `docs/ALGORITHMS.md`: the
  candidates, the interface and config key, the benchmark, measured
  results (with the machine and date), the default, why it won, and when
  each alternative is the better choice. Package READMEs link to their
  entries.
* Conformance and invariant tests run against every candidate that claims
  the same semantics. Approximate ones, such as ANN search or a sliding
  window counter, are compared with stated tolerances.

## Comments

Comments exist to make the code understandable to someone porting or
studying it. Every comment must be **correct, useful, and not a restatement
of the code**.

* **Doc comments** on every exported identifier say what it is or does,
  its contract (inputs, outputs, errors, units, concurrency safety), and
  the spec section, RFC or paper it implements.
* **Inline comments** explain *why*: the invariant being kept, the
  non-obvious reason for an ordering or bound, a tricky edge case, the
  spec step being followed ("WHATWG 13.2.5.38: …", "Myers 1986 §4b:
  middle snake"). State machines name each state after the spec and cite
  it.
* **Porting notes** use the prefix `PORT:` and explain anything
  Go-specific a Rust port must reproduce or deliberately change. Examples:
  "PORT: Go map iteration is random; keys are sorted here so output bytes
  match", "PORT: encoding/json escapes U+2028; replicate it".
* **Not allowed:**
  * comments that repeat the code (`// increment i`);
  * stale or speculative comments;
  * commented-out code;
  * TODOs without a GAPS.md id;
  * comments that disagree with the code or the spec.

  When code changes, its comments change in the same commit.
* Comment review is part of every change: a reviewer checks each comment
  against the code it describes.
