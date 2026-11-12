# Porting Flowforge to Rust

This guide is for whoever writes the Rust implementation of Flowforge (spec
section 20, "reference targets: Go, then Rust"). The Go code in this
repository is the reference. The Rust port must produce the same observable
behaviour, checked by the same conformance data, not the same code.

Status: written while the Go implementation is being built. Sections marked
**pending** describe modules whose Go reference is not finished yet; they
list the contracts that are already fixed by the spec and will be completed
when the Go module lands.

---

## 1. Ground rules

1. **Port from three sources, in this order of authority:**
   1. the specification (`docs/flowforge-spec.html`, plain text with stable
      line numbers in `docs/spec.txt`);
   2. the conformance data under `conformance/` (language-neutral JSON, plus
      `.ff` sources for the DSL);
   3. each Go package's `OPEN_QUESTIONS.md`, which records every place where
      the spec was ambiguous and the behaviour the Go code pinned. **These
      decisions are normative for the port.** If you disagree with one, change
      the spec and the conformance data first, then both implementations.
2. **Do not translate Go files line by line** (spec 20.2). Use the Go code to
   answer "what exactly happens here?" questions, then write idiomatic Rust.
3. **Any Go/Rust difference is a bug in one of them or a gap in the spec**
   (spec 19). Fix the spec first, then the data, then the code.
4. **Same module order and definition of done** as Go (spec 24.6, 24.7):
   a crate is done when its required test ids, conformance suites, property
   tests, fuzz targets and benchmark baseline exist and pass.

---

## 2. Workspace layout

One Cargo workspace. Crate names follow spec 24.2; the right-hand column is
the Go package that is the reference for it.

| Rust crate | Go reference | Notes |
|---|---|---|
| `ff-core` | `core` | ids, `Clock`/`Rng` traits, error kinds, canonical JSON, `ActivityClass` |
| `ff-netnorm` (or a module of `ff-core`) | `core/netnorm` | spec 4.2 normalization. Go split it out so scope and oam could share it without a same-layer dependency; in Rust a feature-gated module of `ff-core` is fine |
| `ff-testkit` | `testkit` | `FakeClock`, `FF_SEED`, case counts, conformance loader (dev-dependency only) |
| `ff-scope` | `scope` | |
| `ff-oam` | `oam` | |
| `ff-limiter` | `limiter`, `limiter/redislimiter` | Redis adapter behind a cargo feature |
| `ff-csr` | `csr` | |
| `ff-placement` | `placement` | |
| `ff-auth` | `auth` | |
| `ff-alerts` | `alerts` | |
| `ff-vecindex` | `vecindex` | |
| `ff-dsl` | `dsl` | **pending** |
| `ff-bytecode` | `bytecode` | **pending** |
| `ff-vm` | `vm` | **pending** |
| `ff-eventstore` | `eventstore`, `eventstore/storetest`, `eventstore/pgstore` | Postgres impl behind a feature; the conformance suite as a `pub fn run_suite` in a `testing` module or a separate `ff-eventstore-testkit` crate |
| `ff-engine` | `engine` | **pending** |
| `ff-sim` | `sim` | **pending** |
| `services/*` | `services/*`, `cmd/*` | **pending** |
| `xtask` | `cmd/xtask` | same commands: `verify-module`, `test-module`, `bench-module`, `bench-compare`, `fuzz-smoke` |

Shared, language-neutral assets used by both implementations without
copying:

* `conformance/` — test data.
* `proto/` — protobuf contracts (generate with `prost`/`tonic` via
  `tonic-build`, or `buf` with the `prost` plugins).
* `migrations/sql/` — the SQL schema. Both implementations must apply the
  **same files** and agree on the bookkeeping table (see section 6).
* `modules.toml` — add a Rust section or a parallel `modules.rust.toml`
  using the crate names; keep the required ids identical.

Layer rules (spec 24.3) carry over unchanged. Replace the Go arch test
(`internal/archtest`) with an `xtask` check over `cargo metadata`, and use
Clippy `disallowed_methods` for `SystemTime::now`, `Instant::now`,
`rand::thread_rng` and `std::env::var` in layer 1–5 crates.

---

## 3. Translating the Go idioms

| Concern | Go reference does | Rust port should do |
|---|---|---|
| Sum types (AST, values, events, relations, assets) | interface + struct per variant + type switch | `enum` with data, exhaustive `match` |
| Absent values | zero values, pointers, `(v, ok)` | `Option<T>` |
| Errors | sentinel `var ErrX = errors.New(...)`, wrapping with `%w`, `errors.Is` | one `thiserror` enum per crate; variants correspond 1:1 to the Go sentinels so tests can assert the same reason |
| Cross-service errors | `core.Error{Kind, Reason, ...}` | `ff_core::Error { kind: Kind, reason: &'static str, .. }` mapped to `tonic::Status` / HTTP only in services |
| Validated strings (Fqdn, ids) | named types and constructor functions | newtypes with private fields and `TryFrom`/`FromStr` |
| Ports | small interfaces (`core.Clock`, `scope.Resolver`, `eventstore.Store`, ...) | traits; `async_trait` or native async fn in traits for I/O ports; `Send + Sync` bounds |
| Time | `core.Clock` with `Now()` (wall) and `Monotonic()` (`time.Duration` since an origin) | `trait Clock { fn now_wall(&self) -> Timestamp; fn now_mono(&self) -> Duration; }` |
| Randomness | `core.Rng` (`Uint64()`), `core.SplitMix64` | `trait Rng { fn next_u64(&mut self) -> u64; }` and the **same SplitMix64** (section 4.3) |
| Cancellation | `context.Context` | drop-based cancellation plus `tokio_util::sync::CancellationToken` where explicit |
| Concurrency | goroutines, channels, `sync.Mutex`, `sync/atomic` | tokio tasks, bounded channels, one owner task per piece of mutable state, atomics with explicit orderings |
| Iteration over a stream | `iter.Seq2[Event, error]` (eventstore `Subscribe`) | `impl Stream<Item = Result<Event, StoreError>>` |
| JSON | struct tags; strict decode via `core.DecodeStrict` | `serde` with `#[serde(deny_unknown_fields)]` on request types; tolerant on responses |
| Tests | table-driven `TestXxx`, native fuzzing, `testing.B` | `#[test]`, `proptest`, `cargo-fuzz`, `criterion` (section 7) |

---

## 4. Cross-language contracts that must be bit-exact

These are the places where "similar" is not good enough: the bytes or numbers
are compared across implementations, stored in shared databases, or sent on
the wire.

### 4.1 Identifiers (`core/id.go`)

* UUIDv7: 48-bit Unix milliseconds big-endian in bytes 0–5, version nibble 7,
  RFC 4122 variant, 74 random bits taken as
  `u16(rng.next_u64())` into bytes 6–7 and then `rng.next_u64()` big-endian
  into bytes 8–15 (before setting version/variant bits). With the same clock
  and the same seeded `Rng`, both implementations must produce the same UUID.
* Public ids: `<prefix>_` + 26 characters of **Crockford base32** (alphabet
  `0123456789ABCDEFGHJKMNPQRSTVWXYZ`) of the 128-bit value, most significant
  first; the first character carries only the top 3 bits (value 0–7).
  Parsing is case-insensitive and also accepts the bare 36-character UUID.
  Prefixes are listed in `core/id.go` (`run`, `prg`, `ent`, `edg`, `apr`,
  `wfv`, `dec`, `key`, `alr`, ...).
* Derived graph ids (`oam/id.go`): entity and edge ids are **not random**.
  `EntityUUID = derived("ff-oam-entity", tenant, program, asset_type, key)`
  and `EdgeUUID = derived("ff-oam-edge", tenant, program, from, to,
  rel_type, label, discriminator)`, where `derived` is SHA-256 over each part
  written as `uvarint(len(part)) || part` (Go `binary.PutUvarint`, i.e.
  LEB128), the first 16 bytes taken, then version nibble set to 8 and the
  RFC 9562 variant bits set. Any deviation makes the two implementations
  write different rows for the same asset.

### 4.2 Canonical JSON (`core/canon.go`)

Used for hashing (args hashes, request hashes, dedupe keys, stored payloads).
Rules: object keys sorted by byte order at every depth, no insignificant
whitespace, **no HTML escaping** (`<`, `>`, `&` stay literal), numbers kept in
their original textual form (Go decodes with `UseNumber` and re-emits the
digits). Pitfalls in Rust:

* `serde_json::Value` with the default `Map` is a `BTreeMap`, which sorts
  keys — good. Do not enable `preserve_order` for canonical output.
* Use `serde_json` with the `arbitrary_precision` feature (or keep numbers as
  raw text) so that `1.0` and `1e3` round-trip with the same digits as the
  Go reference; otherwise float formatting differs (`ryu` vs Go's
  `strconv`), e.g. `1e21` vs `1e+21`.
* Go's encoder escapes U+2028 and U+2029 as ` `/` ` even with HTML
  escaping off, and replaces invalid UTF-8 bytes with U+FFFD; serde_json does
  neither the escaping nor (being `&str`-based) accept invalid UTF-8 at all.
  Match the Go behaviour: escape U+2028/U+2029 and lossily convert byte input
  before encoding. Add a conformance case before hashing any user text.

### 4.3 Seeded randomness

`core.SplitMix64` is the reference generator for everything that must
reproduce from a seed (shuffles in property tests, k-means init in
`vecindex`, Vamana random graph, retry jitter in tests, simulation seeds):

```rust
pub struct SplitMix64(u64);
impl SplitMix64 {
    pub fn next_u64(&mut self) -> u64 {
        self.0 = self.0.wrapping_add(0x9e37_79b9_7f4a_7c15);
        let mut z = self.0;
        z = (z ^ (z >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
        z ^ (z >> 31)
    }
}
```

Helpers derived from it must match too: `Float64 = (x >> 11) as f64 / 2^53`,
`IntN(n) = x % n`, and `Shuffle` is Fisher–Yates from the top
(`for i in (1..n).rev() { swap(i, IntN(i+1)) }`). Do **not** use the `rand`
crate's distributions where Go output is compared.

### 4.4 Hashes and formats

| Contract | Defined in | Must match |
|---|---|---|
| Placement hash | `placement` (OPEN_QUESTIONS #1) | FNV-1a 64 over `u64_le(len(key)) ‖ key ‖ worker`, then MurmurHash3 `fmix64`; vnode `i` of worker `w` at `hash(u64_le(i), w)`, key position `hash(key, "")`; ties by worker id. Vectors: `conformance/placement/hash64.json` |
| CSR snapshot file "FFCS" | `csr` (OPEN_QUESTIONS) | byte-identical output for the same graph (csr_009 compares SHA-256 **across languages**): u64 offsets, u32 `in_edge`, 64-byte aligned sections, CRC32C (Castagnoli) per section, u16 length-prefixed label strings (≤ 256 labels), `created_at` in Unix **milliseconds**, SHA-256 footer verified before anything else |
| Vector segment "FFDA" | `vecindex` (OPEN_QUESTIONS, segment format) | 4096-byte sectors; records never straddle a sector unless larger than one (dim 960 records start on a sector boundary and span sectors); per-section CRC32C plus whole-file checksum |
| Bytecode module "FFBC" | `bytecode` | **pending** — module hash = SHA-256 of the encoded bytes is stored with every run, so Go-compiled modules must load and run in the Rust VM and vice versa |
| VM snapshot | `vm` | **pending** — deterministic CBOR (RFC 8949 §4.2). In Rust use `ciborium` with sorted map keys and shortest float/int encodings, or write a small canonical encoder; verify against Go-produced snapshots |
| Effect `args_hash` | `vm`/`engine` | **pending** — SHA-256 of the canonical args encoding; a mismatch on replay is `nondeterminism_detected`, so the two implementations must agree byte for byte if they ever share an event log |
| Alert dedupe key | `alerts` | `sha256(rule_id ‖ 0x00 ‖ canonical JSON of the group_by values)` |
| Webhook signature | `alerts` | `v1=hex(HMAC-SHA256(secret, timestamp + "." + raw_body))`, reject when `|now − ts| > 300 s` |
| Password hashes | `auth` | Argon2id PHC strings (`$argon2id$v=19$m=…,t=…,p=…$salt$hash`, unpadded standard base64). The `argon2` crate's `PasswordHash` format is compatible; verify with a Go-generated hash in conformance |
| JWT | `auth` | compact JWS, `alg` exactly `EdDSA`, Ed25519 (`ed25519-dalek`), JWKS as OKP/Ed25519 with base64url `x` |
| Token formats | `auth` | refresh `ffr_` + base64url(32 bytes), stored as SHA-256; API keys `ffk_<key_id>_<secret>` where the key id is letters and digits only (the secret may contain `_`) |
| GCRA Lua script | `limiter/redislimiter` | the spec 14.4 script **verbatim**; both implementations may share one Redis |

---

## 5. Module-by-module notes

For each module: read its Go `README.md` and `OPEN_QUESTIONS.md`, run its
conformance data, and pay special attention to the points below.

### 5.1 `ff-core` and normalization (`core`, `core/netnorm`)

Normalization is the module where library differences between languages bite
hardest. The Go reference behaviour:

* **Hostnames:** trim, reject invalid UTF-8, drop one trailing dot, reject
  empty labels *before* IDNA, convert with UTS 46 lookup mapping,
  non-transitional, **no STD3 rules** (`golang.org/x/net/idna` with
  `MapForLookup`, `Transitional(false)`, `StrictDomainName(false)`,
  `BidiRule`), then apply Flowforge's own checks: ASCII result only of
  `[a-z0-9-_]`, label ≤ 63, name ≤ 253, and **reject names whose last label
  is numeric** (decimal or `0x` hex), because resolvers and WHATWG parsers
  read them as IPv4 (`2130706433`, `0x7f.1`, `0177.0.0.1`).
  In Rust use the `idna` crate's `domain_to_ascii` with an explicitly
  configured UTS 46 mode (no STD3 ASCII rules, non-transitional). The Unicode
  and UTS 46 table versions of the two libraries will not always match; that
  is exactly what `conformance/scope/normalize` is for — add a case for every
  divergence you find.
* **IP addresses:** strict dotted-quad without leading zeros, IPv6 without
  zones, IPv4-mapped IPv6 unmapped to IPv4. `std::net::IpAddr::from_str` is
  strict in the same way (it rejects octal-looking octets and short forms) and
  has no zone syntax; unmap with `Ipv6Addr::to_ipv4_mapped`.
* **URLs:** **do not use the `url` crate's WHATWG normalization as is.**
  WHATWG parsing turns `https://2130706433/` into `127.0.0.1`, treats `\` as
  `/`, and percent-decodes differently. The Go reference follows RFC 3986:
  scheme must be http/https, userinfo rejected, default port removed per
  scheme only (`:80` for http, `:443` for https), fragment dropped, only
  unreserved escapes decoded (remaining escapes uppercased), dot segments
  removed, empty path becomes `/`. Either write a small RFC 3986 parser or
  use the `url` crate only for splitting and re-validate every component with
  the netnorm rules. Golden cases: `core/netnorm/netnorm_test.go` and
  `conformance/scope/normalize`.
* **Registrable domain** (limiter target keys): public suffix list via the
  `publicsuffix` crate. Pin the PSL snapshot in both implementations or
  accept that keys can differ for brand-new suffixes.
* **Time windows:** Go embeds tzdata (`time/tzdata`); use `chrono-tz` (also
  embedded) and pin a tz database version.

### 5.2 `ff-scope`

Decisions to reproduce (all pinned by tests and data):

* A resolver error, empty answer, missing resolver or cancelled request is
  `deny(resolution_failed)` — fail closed.
* `ClassNone` or an out-of-range class is `deny(invalid_class)`.
* `Request.skip_resolution` mirrors the proto `resolve` flag; decisions made
  without resolution carry no pinned IPs. Conformance files without a
  `resolver` section imply skipping resolution.
* A `url_prefix` **deny** rule also denies host-level checks of that host.
* CIDR targets: an allow rule must **contain** the whole prefix; a deny rule
  matches on any **overlap**.
* `asn` rules never match, for any class; they are informational only.
* Wildcard `*.x` matches any depth below `x`, never `x` itself.
* URL-index lookups are bounded by the longest rule path (avoid quadratic
  behaviour on long paths).

Indexing (suffix trie for domains, prefix trie for CIDRs) is free to differ;
keep a naive matcher for the differential property test, as Go does.

### 5.3 `ff-oam`

* Derived ids (section 4.1) and `canonical_bytes()` must match Go so that
  convergence (oam_prop) can be compared across languages.
* Content merge is last-writer-wins **per top-level field**, ordered by
  `(observed_at, source)`; a single per-entity stamp does not converge.
* Renamed fields: `File.type` → `file_type`, `IPNetRecord.type` →
  `ipnet_type` (they collide with the asset discriminator).
* TLSCertificate requires the Flowforge field `fingerprint_sha256` (the key
  source); certificates without it are rejected.
* Product keeps the opaque content-hash key.
* Edge upsert key is the 5.7 tuple (from, to, relation type, label,
  discriminator); discriminator strings are `tcp/443` and `rr1`.
* Staleness: an entity goes stale only when **every** tool that ran a
  covering run over it missed it N times in a row.
* Strict decoding rejects case-variant and duplicate keys. `serde` derive
  already rejects duplicate struct fields; add `deny_unknown_fields` and do
  not use `#[serde(alias)]` or case-insensitive matching.

### 5.4 `ff-limiter`

* Token bucket tokens are **fixed-point**, not float, so integer rates are
  exact; match the Go representation if you compare traces.
* `Result.delay` is an addition used by the leaky bucket (admit like GCRA,
  return the wait until release).
* AIMD backs off on any 429, any timeout, or a 5xx fraction above the
  threshold, and starts at the **minimum** rate.
* WFQ virtual time is monotone: `V = max(V, start)`.
* Cost above capacity is an error with `retry_after == Never`, not a hang.
* lim_009: Go has no loom, so the reference is a `-race` stress test. The Rust
  port should do what the spec actually asks: a `loom` model of the atomic
  GCRA with 2–3 threads.
* lim_010: the Redis test swaps only the script's clock line for a
  caller-supplied time (`CallerClock`) to compare timelines exactly; keep the
  same trick.

### 5.5 `ff-csr`

* Output order of SCCs is **reverse topological**; conformance stores sets
  and checks order separately.
* Undirected algorithms (k-core, cuts, triangles, common neighbours) use a
  simple undirected view: no self loops, parallel and reciprocal edges
  collapsed.
* An unspecified direction is an error; blast radius caps depth at 6.
* Overlay edge removal removes all edges with the same `(src, dst, label)`.
* PageRank: compare with absolute tolerance 1e-5 and rank order; parallel
  summation order differs between implementations, so never compare floats
  exactly.
* Go uses `unsafe` for zero-copy views of the mmap; in Rust use
  `bytemuck`/`zerocopy` over a `memmap2` mapping, after the checksum pass,
  and run Miri on small inputs (spec 24.6).

### 5.6 `ff-placement`

Hash and tie-break rules in section 4.4. Liveness: a worker is live while
`elapsed < 15 s` since its last heartbeat (dead at exactly 15 s), on the
monotonic clock. Duplicates in the live set are ignored.

### 5.7 `ff-auth`

* JWT validation order and reasons: header `alg` must be exactly `EdDSA`
  (never let the header choose; `none` rejected), `typ` JWT, kid lookup,
  signature, then `iss`, `aud`, `exp`, `iat`, `nbf` with 30 s skew. Expired
  means `now ≥ exp + 30 s`. A future `iat` and a future `nbf` have different
  reasons. Retired keys have their own `key_retired` reason. Duplicate or
  case-variant JSON members in header or claims are rejected — serde's
  default for maps keeps the last duplicate, so decode into structs or check
  explicitly. Hand-roll the JWS verification (as Go does) rather than using
  a JWT crate whose rejection reasons you cannot map 1:1.
* A missing session-version cache entry means "not revoked".
* Login: pending/disabled status is checked only after the password verifies;
  responses for unknown and known emails are identical (auth_005).
* Approval roles must match exactly; the 13.6 table wins over the "Min.
  role" column in 12.3.
* Argon2id at the policy parameters takes ~22 ms on the reference machine;
  both implementations should raise the cost together after measuring.
* Secrets come from an injected `io.Reader` in Go; take a
  `&mut dyn rand_core::CryptoRngCore` (or `RngCore + CryptoRng`) in Rust,
  never the seeded `SplitMix64`.

### 5.8 `ff-alerts`

* Own small parser (the alert language reuses the DSL expression grammar
  but does not depend on `ff-dsl`). Diagnostic codes `A00xx` lexing,
  `A01xx` parsing, `A02xx` names, `A03xx` types, `A04xx` values/limits.
* Default severity Medium; `severity = X` inside an action sets the rule
  severity; two different severities is `A0307`.
* Quiet hours delay Info, Low and Medium; High and Critical go out
  immediately.
* Throttle windows use the injected clock, not event time; suppressed matches
  are stored and the next alert carries `suppressed_count`.
* A lone `{` in a string is an error; regex braces are written `{{2}}`.
* Missing fields follow three-valued logic and never match.
* Retry: 10 s, 30 s, 2 m, 10 m, 1 h, then 1 h repeated until 24 h; jitter has
  a 1 s floor.
* Regex: Go's `regexp` is RE2; the Rust `regex` crate is also linear-time and
  close in syntax. Keep the pattern size limit and add conformance cases for
  any syntax you rely on.

### 5.9 `ff-vecindex`

* Inner-product graphs are built over an **L2-equivalent transform** that
  ranks exactly like `-dot` (building with raw `-dot` gave recall ≈ 0.51).
* Vamana is followed by a **reachability repair pass** so every node is
  reachable from the medoid (vec_004).
* Merge when tombstones are strictly above 20 %, or merge the smallest
  segments when there are more than 8.
* A write applies only if its seq is strictly newer; deletes of unknown ids
  are recorded.
* RRF: the spec's example rounds each term before summing; compare with
  tolerance 1e-5.
* SimHash shingling and MinHash permutation choices are fixed by the
  conformance data — reuse them exactly.
* Model version is part of the index key (`e5-base@2`).
* Rust can do what Go could not: real SIMD kernels via `std::arch`
  (AVX2/NEON) or portable `std::simd`, checked against the scalar kernel
  (vec_001, relative 1e-4).

### 5.10 `ff-eventstore`

* Trait shape (Go `eventstore.Store`): `create_run`, `get_run`, `append`,
  `read`, `subscribe`, `save_snapshot`, `load_snapshot`, `acquire_lease`,
  `renew_lease`, `release_lease`, `list_ready`, `claim_ready`, `wake`.
  `create_run` writes the initial events (seq 1..n) atomically with the run.
* `Fenced` is reported before `SeqConflict` when both apply.
* An expired lease nobody replaced can still append; a released lease cannot.
* `from_seq` is inclusive.
* `release_lease` re-wakes the run if `wake` was called during the lease (so
  out-of-band wake-ups are not lost).
* Payloads are stored as canonical JSON and compared by value (jsonb rewrites
  text).
* Shard = last 4 UUID bytes (big-endian u32) mod n.
* Postgres notifications: appends on `engine_events`; `engine_wake` only when
  a run becomes ready.
* Fences are stored in the single `bigint` column as term 0.
* Test harness: `elapse(d)` advances a fake clock for the memory store and
  shifts stored lease/wake timestamps back by `d` for Postgres, so no test
  sleeps. Port the suite as a generic `run_suite<H: Harness>()`.

### 5.11 `ff-dsl`, `ff-bytecode`, `ff-vm` — pending

Fixed by the spec regardless of the Go details:

* The bytecode module format (8.3) and opcode numbering (8.4) are a
  cross-language contract: a module compiled by Go must load, verify and run
  in the Rust VM with identical results and effect traces. Add a
  `conformance/vm/run` case runner to both implementations that executes
  **both** `.ff` sources and pre-compiled `.ffbc` modules.
* Fuel costs (8.7), limits, failure code names, and effect path numbering
  (`seq` and `parent/task/child` for `par_map`) must match exactly; they
  appear in event logs.
* The AST is a good fit for arenas (`la-arena`, `id-arena`) with
  index-based nodes (spec 20.2); insta snapshots for diagnostics.

This section will list the Go decisions (builtin id table, local/loop
allocation rules for the verifier, args encoding, value JSON form) when the
packages land.

### 5.12 Engine, simulation, services — pending

Will be filled in when the Go engine and services exist. Fixed by the spec:
the event types and payloads (9.3), idempotency key
`hex(sha256(run_id || "/" || path))`, retry delay formula with full jitter,
submit idempotency (9.10), error mapping (11.8), REST shapes (12).

---

## 6. Shared infrastructure

* **Database:** both implementations run against the schema in
  `migrations/sql/`. The Go runner records applied files in a
  `schema_migrations` table keyed by file name with a checksum, takes a
  per-file advisory lock and refuses to run if an applied file was edited.
  Either reuse that table format in Rust (do not use `sqlx migrate`'s own
  `_sqlx_migrations` table side by side), or make one runner own migrations
  for both. The pgvector migration is recorded as skipped when the extension
  is unavailable — keep that behaviour.
* **gRPC:** generate from `proto/` with `tonic-build`. Note the deviations
  in `proto/README.md` (renamed enum values `DIRECTION_OUT`, `METRIC_L2`,
  analytics and similarity in their own packages).
* **Redis:** the limiter keys (spec 14.3) and the GCRA script are shared, so
  Go and Rust services can rate-limit against the same Redis.
* **Container images and CI:** `deploy/Dockerfile` builds one Go service per
  image; add a sibling Rust Dockerfile and a CI job that runs the shared
  conformance suites against both.

---

## 7. Tests and benchmarks

| Go | Rust |
|---|---|
| `func TestScope001_TrailingDot` | `#[test] fn scope_001_trailing_dot()` (spec ids, snake case) |
| `TestScopeProp_DenyMonotone` + `testkit.Cases()` + `FF_SEED` | `proptest!` with `ProptestConfig::with_cases(cases())` reading `FF_PROPTEST_CASES`; print and accept `FF_SEED` |
| `func FuzzScopeNormalize(f *testing.F)` | `cargo fuzz` target `fuzz_scope_normalize` with the same invariants; seed the corpus from Go's `testdata/fuzz/` |
| `func BenchmarkScope01_Decide` | `criterion` bench `b_scope_01`; also count allocations (`dhat` or a counting allocator) |
| `testkit.LoadCases[T](t, "scope/decide")` | `ff_testkit::load_cases::<T>("scope/decide")`, failing when the directory is empty |
| `go test -race` stress (lim_009) | `loom` model |
| `unsafe` in `csr` | Miri over small inputs |

Benchmark results: write the same JSON schema `cmd/xtask` writes
(`bench/results/<module>/<date>-<commit>.json` with `bench_id`, metrics map
and `env`) so `xtask bench-compare` can compare Go and Rust runs of the same
benchmark id directly.

---

## 8. Differential testing between Go and Rust

The conformance files compare each implementation against expected values.
To catch the cases nobody wrote down, add a cross-language differential
harness (spec 19, 25.6):

1. Give each implementation a small CLI, `ff-conform`, that reads a request
   in canonical JSON on stdin (`{"suite": "scope/decide", "input": {...}}`)
   and prints the result in canonical JSON.
2. A driver generates inputs from a seed (SplitMix64 so the seed means the
   same thing everywhere), runs both CLIs and compares outputs byte for byte.
3. Start with the pure modules: netnorm (random strings), scope (random
   rule sets and targets), oam (random observation lists → canonical bytes),
   limiter (random timelines), csr (random edge lists → file SHA-256 and
   algorithm outputs), placement (random keys and worker sets), alerts
   (random events against fixed rules), then the VM on generated programs.
4. Every divergence becomes a conformance case and, if the spec was unclear,
   an `OPEN_QUESTIONS.md` entry plus a spec fix.

---

## 9. Suggested order

Follow spec 24.7, which is also the order the Go reference was built in:

1. workspace, `ff-core` (with netnorm), `ff-testkit`, `xtask`, CI;
2. `ff-limiter`, `ff-scope`, `ff-oam`, `ff-csr`, `ff-placement`, `ff-auth`,
   `ff-alerts` — pure and fully covered by conformance data today;
3. `ff-dsl`, `ff-bytecode`, `ff-vm` (prove cross-loading of Go-compiled
   modules early);
4. `ff-eventstore` (memory, then Postgres against the shared schema),
   `ff-engine`;
5. `ff-placement` in multi-worker mode, `ff-sim`;
6. services and gateway (tonic, axum), sharing `proto/`, the schema and
   Redis with the Go services;
7. `ff-vecindex` (with real SIMD);
8. agent step — deferred in Go as well (`docs/DECISIONS.md`).
