# Porting Flowforge to Rust

This guide is for whoever writes the Rust implementation of Flowforge (spec
section 20, "reference targets: Go, then Rust"). The Go code in this
repository is the reference. The Rust port must produce the same observable
behaviour, checked by the same conformance data, not the same code.

Status: every Go module referenced here is implemented, including the v2
packages of spec Part II (content capture, diffs, search; section 5A). The
agent step (spec 16) is deferred in both implementations
(`docs/DECISIONS.md` D1).

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
| `ff-dsl` | `dsl` | front end only |
| `ff-bytecode` | `bytecode` | |
| `ff-vm` | `vm` | |
| `ff-compiler` | `compiler`, `compiler/treewalk` | spec 24.2 puts compile() in ff-dsl; Go split it out because dsl and bytecode share a layer |
| `ff-eventstore` | `eventstore`, `eventstore/storetest`, `eventstore/pgstore` | Postgres impl behind a feature; the conformance suite as a `pub fn run_suite` in a `testing` module or a separate `ff-eventstore-testkit` crate |
| `ff-engine` | `engine`, `engine/enginetest`, `engine/pgrepo` | |
| `ff-graphstore` | `graphstore`, `graphstore/pg` | graph service domain logic (not a spec crate; Go needed one layer between oam and the service) |
| `ff-sim` | `sim` | |
| `services/*` | `services/*`, `cmd/*` | tonic + axum; `services/devstack` and `cmd/flowforge dev` have no spec counterpart |
| `xtask` | `cmd/xtask` | same commands: `verify-module`, `test-module`, `bench-module`, `bench-compare`, `fuzz-smoke` |

v2 crates (spec Part II 27; all layer 1 unless noted, each depending only
on `ff-core` plus the external crate named):

| Rust crate | Go reference | Notes |
|---|---|---|
| `ff-htmlparse` | `htmlparse` | `encoding_rs` for decoding; `html5ever` is a design reference, not a dependency |
| `ff-jsparse` | `jsparse` | arena AST; `unicode-id-start` pinned to Unicode 15.0 tables |
| `ff-cssparse` | `cssparse` | servo `cssparser` token names, own tokenizer |
| `ff-xmlparse` | `xmlparse`, `xmlparse/sitemap` | sitemap as a module of the crate; `flate2` for gzip sitemaps |
| `ff-sourcemap` | `sourcemap` | `serde_json` (last duplicate key wins) |
| `ff-wellknown` | `wellknown` | `chrono` for RFC 3339 |
| `ff-codec` | `codec` | `flate2`, `zstd` |
| `ff-dnsmsg` | `dnsmsg` | |
| `ff-socks5` | `socks5` | |
| `ff-diff` | `diff` | `serde_json` with `arbitrary_precision` and `preserve_order` |
| `ff-sketch` | `sketch` | shared with `ff-vecindex` (which then depends on it) |
| `ff-chunkstore` | `chunkstore`, `chunkstore/fsstore`, `chunkstore/gcsstore` | `zstd`, `crc32c`, `sha2`; filesystem and GCS backends behind features |
| `ff-textindex` | `textindex` | `zstd`, `crc32c`, `regex-syntax` |
| `ff-content` (layer 2) | `content` | depends on the parser crates, `ff-codec`, `ff-diff`, `ff-sketch`, `ff-oam` |
| `ff-contentstore` (layer 4) | `contentstore`, `contentstore/pg`, `contentstore/storetest` | `aes-gcm`, `hmac`; Postgres behind a feature; the Repo contract suite as a testing module |
| `services/content` | `services/content`, `cmd/content` | tonic, `proto/flowforge/v1/content.proto` |
| (test) `lab` | `services/internal/lab` | the Part II 38 lab; reuse `conformance/lab` sites |

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
| Bytecode module "FFBC" | `bytecode` (OPEN_QUESTIONS 1, 2, 11) | the module hash (SHA-256 of the encoded bytes) is stored with every run, so Go-compiled modules must load and run in the Rust VM and vice versa. Section details the spec leaves open are fixed in section 5.11 |
| Value encoding (args hash, recorded results) | `vm/codec.go` | deterministic CBOR (RFC 8949 §4.2, fxamacker `CoreDetEncOptions`: shortest ints and floats, definite lengths, sorted map keys) of a structural tree — see section 5.11. `args_hash = sha256(EncodeArgs(args))`; a mismatch on replay is `nondeterminism_detected`, so both implementations must agree byte for byte if they ever share an event log |
| VM snapshot | `vm/snapshot.go` | a fixed 19-field canonical CBOR array; Restore re-encodes and rejects non-canonical input. Snapshots are a cache, so cross-language snapshot compatibility is optional — but if you share event stores, either match it or drop foreign snapshots and replay |
| Alert dedupe key | `alerts` | `sha256(rule_id ‖ 0x00 ‖ canonical JSON of the group_by values)` |
| Webhook signature | `alerts` | `v1=hex(HMAC-SHA256(secret, timestamp + "." + raw_body))`, reject when `|now − ts| > 300 s` |
| Password hashes | `auth` | Argon2id PHC strings (`$argon2id$v=19$m=…,t=…,p=…$salt$hash`, unpadded standard base64). The `argon2` crate's `PasswordHash` format is compatible; verify with a Go-generated hash in conformance |
| JWT | `auth` | compact JWS, `alg` exactly `EdDSA`, Ed25519 (`ed25519-dalek`), JWKS as OKP/Ed25519 with base64url `x` |
| Token formats | `auth` | refresh `ffr_` + base64url(32 bytes), stored as SHA-256; API keys `ffk_<key_id>_<secret>` where the key id is letters and digits only (the secret may contain `_`) |
| GCRA Lua script | `limiter/redislimiter` | the spec 14.4 script **verbatim**; both implementations may share one Redis |
| Chunk formats FFPK, FFPI, FFMF, FFRF, FFZB | `chunkstore/FORMAT.md`, spec Part II 31 | FastCDC cut points from the gear table `SplitMix64(0x46464344)` and the fixed mask bit list; chunk id and ManifestID are SHA-256 and identical across languages; pack/index/ref bytes identical only with the same zstd encoder. Vectors: `conformance/chunkstore/` |
| Text-index files FFTX, FFTD, FFTC | `textindex/FORMAT.md`, spec Part II 32.6-32.7 | byte-identical segments except STORED's zstd data area (compare decompressed); minimal LEB128; CRC32C per section and file |
| Normalizer rule tables | `content`, spec Part II 33.3 | SHA-256 of the rule strings joined by `"\n"`, pinned in `conformance/content/normalizers/normalizers.json`; stored with every snapshot and diff |
| SimHash, MinHash, LSH | `sketch`, spec Part II 30.4 | FNV-1a 64; MinHash seed `0x46464d494e484153`, 128 permutations mod 2^61 − 1; 32 × 4 LSH bands |
| Content-store ids and ciphertexts | `contentstore`, spec Part II 34.3-34.4 | UUIDv8 from SHA-256 over `uvarint(len) ‖ part`; AES-256-GCM `0x01 ‖ nonce ‖ ct ‖ tag` with the documented additional data, so both implementations can open each other's findings |
| DNS wire format | `dnsmsg` | canonical compression; `conformance/dnsmsg/roundtrip` is an independent encoder's output, matched byte for byte |

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

### 5.11 `ff-dsl`

* Read `dsl/OPEN_QUESTIONS.md` (34 decisions) before writing the checker.
  The ones with visible effects:
  * record literals accept `field: expr` **and** `field = expr` (the spec's
    own `Budget { tokens = 20000 }` example);
  * `inferred_effects` contains only the eight effect kinds of 7.5; scope
    checks, approvals, alerts, `log` and `execute` are tracked as runtime
    ops, so `POST /workflows:check` reports `["passive","active"]` like spec
    12.6;
  * a `fn` without `!` is pure; function types written in source are pure;
    higher-order builtins carry their argument closures' effects;
  * E0402/E0403 are only checked when the program policy is known;
  * `?` inside a lambda always fails the run;
  * extra codes inside the spec's ranges (E0305–E0314, E0404/E0405,
    E0490 for agent effects rejected by the engine, E0502–E0507,
    E0603/E0604, W0601) — reuse the same numbers;
  * the REST E0301 example's span is the argument's own span
    (`col 51 / offset 50` to `col 71 / offset 70`), not the spec's
    two-bytes-earlier start;
  * prelude records the spec only names (Product, Budget, GraphSummary,
    TaskError, ...) have the fields listed in `dsl/prelude.go`; the
    built-in records the VM constructs are pinned in bytecode
    OPEN_QUESTIONS 9: ScopeDenied `{target, reason, rule_id, decision_id}`,
    Approval `{id, role, decided_by, decided_at}`, Denied `{id, role,
    reason}`, PlanError/TaskError `{code, message}`, Budget `{tokens,
    tool_calls, iterations}`.
* `conformance/dsl/parse` stores ASTs in the canonical JSON form documented
  in `conformance/dsl/README.md`; port the AST-to-JSON printer exactly, or
  the parse goldens are useless. Diagnostics goldens compare codes, spans
  and messages.
* Known gaps you may close (both implementations together): `Format` drops
  comments; `-9223372036854775808` cannot be written as a literal; the
  22.9 `is InScope(_)` syntax is unsupported; a lambda created before an
  approval and called after it is rejected (fails closed).
* The AST is a good fit for arenas (`la-arena`, `id-arena`) with
  index-based nodes (spec 20.2); use `insta` for diagnostics snapshots.

### 5.12 `ff-bytecode`

The binary format is a cross-language contract. Beyond spec 8.3/8.4:

* FUNCS has an explicit `nspans u32` before the span table; META strings
  are `u32 len + bytes`; TYPES entries are `kind u8, name u32` then record
  fields `(name u32, type u32)`, enum variants `(name u32, arity u8,
  type u32*)` or a signature `(func u16, params, result u32)`; type
  references are String constants holding DSL type text, `0xFFFFFFFF` for
  none. Constant tags: 1 Int, 2 Float (finite only), 3 String (≤ 1 MiB),
  4 Duration (ns), 5 Time (Unix ns).
* Reserved type ids: Option 0, Result 1, Host 2, Severity 3, Tuple 4, ...;
  user types start at 32 (`bytecode/types.go`).
* **Option variant numbering:** the VM numbers `None = 0, Some = 1`, while
  the DSL declares `Some | None`. The compiler maps them
  (`compiler/internal/abi.VariantIndex`); every other enum keeps
  declaration order. A Rust compiler targeting Go-compatible bytecode must
  do the same.
* Effect mask bits (u16): read, passive, active, intrusive, write,
  financial_write, llm, time, then scope, approval, alert, plan (bits
  8–11). now/sleep count as time, agent_call/embed as llm,
  similarity_query as read, par_map as nothing.
* Tool calls: inputs, then `CONST <tool name>`, then `EFFECT 1` with
  `argc = inputs + 1`; that EFFECT must not be a jump target. The VM moves
  the name to `args[0]` of the suspension.
* Verifier: no per-function max-stack field exists; the verifier computes
  it and requires `nlocals + max_depth ≤ 65,536`. "Each local assigned
  once" is per path and per iteration: a LOOP back edge contributes no
  local definitions, a local written before a loop cannot be written in
  it, and loop state (index, list, accumulator) lives on the stack. A Rust
  verifier must accept and reject exactly the modules in
  `conformance/bytecode/verify` (49 cases, one stable rule id each).
* Failure code 16 `type_mismatch` exists beyond the spec's table.
* Optional ANNOTS section (flag bit 1) between META and entry, carrying
  workflow-level `#[timeout]`, `#[effects(max)]`, `#[on_unknown(retry)]`,
  `#[idempotent]` and per-EFFECT-site `#[timeout]`/`#[retry]` keyed by
  (function, code offset). Without the flag, the layout is exactly 8.3.
* Builtins never call closures: `sort_by`/`group_by` take precomputed key
  lists; `first(xs)` is `list_get_opt(xs, 0)`. The 40 builtin ids in
  `bytecode/builtins.go` are part of the format.

### 5.13 `ff-vm`

* Effect sequence numbers start at **0**; par_map task `t` of effect `P`
  numbers its effects `P/t/0`, `P/t/1`, ...
* Fuel: per-instruction costs from 8.7; fuel is checked before an
  instruction runs; unused fuel of a slice that ended in `OutOfFuel`
  carries over; `range` is charged for the elements it produces.
* Heap limit: allocations are counted and the live heap is measured
  exactly only when the counter would exceed the limit, re-measuring after
  1/8 of the limit — the effective limit is 7/8 to 1× (fail closed). Both
  counters are in the snapshot so the decision replays identically.
* Floats are always finite: NaN → `arith_nan`, ±Inf → `arith_overflow`,
  float division by zero → `div_zero`.
* EQ/NE are structural across kinds (different kinds are unequal); LT..GE
  require the same kind and use the total order of `vm.Compare`, which
  `sort` also uses.
* A failed TRY in the workflow body fails with `approval_denied` if the Err
  carries the built-in `Denied` record, otherwise `unwrap_failed`; in
  par_map tasks and compensations it returns from the closure.
* par_map `Join`: with `on_error = fail` the first failing task in input
  order is reported; with `collect` each element is `Ok(v)` or
  `Err(TaskError{code, message})`; a task failing with
  `nondeterminism_detected` fails the par_map with that code in both modes.
* `scope.allows` is the `scope_allows` builtin over the run's pinned scope
  snapshot, which is the one VM input besides the module and recorded
  effects (the engine must pass the same snapshot on replay).
* **Value encoding (args hashes and recorded results)** — deterministic
  CBOR of this tree, which a Rust VM must produce byte for byte:

  ```text
  Unit [0]   Bool [1, b]   Int [2, i]   Duration [3, ns]   Time [4, ns]
  Float [5, f] (shortest exact float)   String [6, s]
  List/Stream [7, 0|1, [elems]]   Map [8, [k1, v1, k2, v2, ...]] (insertion order)
  Record [9, type_id, [fields]]   Enum [10, type_id, variant, [payload]]
  Closure [11, fn, [captures]]    Fqdn [12, s]  IpAddress [13, s]
  Netblock [14, s]  Asn [15, s]  Url [16, s]  ScopeProof [17, target, decision_id]
  EntityRef [18, s]
  ```

  Encode with RFC 8949 §4.2 core deterministic rules (shortest integer
  heads, shortest float that round-trips, definite lengths). `ciborium`
  needs care here: write values through an explicit canonical writer
  rather than relying on serde's defaults, and test against hashes taken
  from Go-recorded event logs.
* JSON forms (inputs, outputs, tool I/O): Option is `null` or the payload;
  Result is `{"ok": v}` / `{"err": e}`; other enums `{"variant": name,
  "values": [...]}`; maps with string-like keys are objects (sorted keys),
  others `[[k, v], ...]`; Time is RFC 3339 UTC; **Duration is Go duration
  text** (`time.Duration.String()`, e.g. `1h30m0s`, `250ms`) — Rust has no
  equivalent, so port Go's formatting algorithm exactly. ScopeProofs are
  rejected in outputs.
* Display (string interpolation, `to_string`): strings raw at top level and
  quoted inside collections; floats always show a decimal point; Duration
  as Go duration text. These strings flow into effect args (e.g. alert
  messages), so they affect args hashes.

### 5.14 `ff-compiler` and the tree-walker

* Lowering follows `bytecode/doc.go`: stack-carried loops, inlined lambda
  literals for map/filter/etc., captures by value, `compensate` as a
  closure binding `it`, `require approval` as EFFECT approval plus TRY.
  `?` uses TRY only where the VM's TRY semantics match the checker's mode;
  elsewhere explicit TEST_VARIANT/UNPACK + FAIL or RET.
* An omitted approval timeout is passed as Duration 0 ("engine default").
* Annotations reach the engine through ANNOTS (section 5.12). A known gap:
  effects inside fns called by an annotated let are not annotated.
* `conformance/vm/run` (54 cases: `.ff` + scripted effects + expected value
  and effect trace with args hashes) and `conformance/vm/replay` (6 cases,
  two tampered logs) are the main cross-language check for the whole
  language stack. vm_008 compares the tree-walker with the VM on every
  case; keep that differential test in Rust too.

### 5.15 `ff-engine`

Read `engine/OPEN_QUESTIONS.md` (21 decisions). The event-log shape is a
cross-language contract if Go and Rust workers ever share a store:

* Only the lease holder appends. Cancel, unknown-outcome resolution and
  scope changes are queued as run requests and applied at the next effect
  boundary.
* `effect_completed` carries both `result` (human JSON) and `value`
  (base64 canonical CBOR from the value encoding above), which replay feeds
  to the VM. `effect_failed` carries `final` and `fail_code`; a retryable
  failure is followed by `effect_retry_scheduled` in the same append.
* Instant effects (now, log, emit_alert, scope_check) record
  `effect_scheduled` and their result in one atomic append; only tool calls
  have `effect_started` and attempts.
* Concurrent tool calls (par_map children) record results in task order in
  one append, so logs never depend on timing.
* Before every tool call the engine re-decides each proof target with the
  tool's own class and records it (`purpose: recheck`).
* Approvals, timers, scope decisions and outbox events have ids derived
  from (run, path), so a re-run after a crash produces the same rows.
* Kill switches fail closed and cancel every run in reach; compensations
  run to the end (cancel and deadlines do not interrupt them);
  nondeterminism skips compensation and marks the outcome `failed`.
* A run whose outbox watermark is below its head stays schedulable until
  every event is published (found by the simulator).
* Retry: `#[retry(max = n, ...)]` means n attempts, multiplier 2; jitter
  comes from the injected seeded Rng so tests reproduce.
* Registration rejects agent effects with `E0490` and requires an entry
  signature in the module.
* The engine-level conformance golden is
  `conformance/engine/eng_001_recon.golden.json` (timestamps normalized).

### 5.16 `ff-sim`

`sim` is the strongest cross-implementation check after conformance data:
port the invariant checker (inv_01–inv_11) first and run it over event logs
exported from either implementation. Seeds in
`conformance/sim/regress_seeds.json` reproduced three real engine bugs;
their scenarios (workload + fault profile) are worth porting as Rust
regression tests even though seeds won't produce identical schedules in a
different scheduler.

### 5.17 Services and gateway

* Internal calls carry `authorization: Bearer <internal JWT>` (aud
  `flowforge-internal`, 60 s), `x-tenant-id` (`ten_...`), `x-actor`,
  `x-request-id`, `traceparent` and optional `idempotency-key`. **The actor
  is `user:<uuid>` / `apikey:<uuid>` / `service:<name>` / `schedule:<id>`
  in both the token subject and `x-actor`**; services reject a mismatch.
  REST responses render actors as public ids (`usr_...`, `key_...`).
* Error mapping 11.8 uses `google.rpc.ErrorInfo` (reason) and `RetryInfo`;
  REST errors are `{"error": {code, status, message, details,
  request_id}}` with `code` the lower-cased reason. Gateway-local reasons
  `CSRF_FAILED` (403) and `PRECONDITION_FAILED` (412) exist; bad login
  credentials are `401 token_invalid`.
* `conformance/api` (14 transcripts, 272 steps) is the REST contract; run
  it against an axum gateway with fake backends exactly as the Go test
  does (`services/gateway/conformance_test.go`).
* Topics, outbox and inbox follow spec 11.7/23.7; the Go in-process bus is
  a stand-in for Pub/Sub and has no wire format to match.

## 5A. v2 module notes

The v2 packages are specified in spec Part II (sections 26-41); their
`README.md` (invariant table, conformance data), `OPEN_QUESTIONS.md` and,
for `chunkstore` and `textindex`, `FORMAT.md` are the porting contract.
Every invariant id of Part II section 39 becomes a Rust test of the same id
(`html_inv_01_token_spans_tile_input`). The `PORT:` comments quoted below
come from `grep -rn "PORT:"` over the v2 packages; each names a Go
behaviour a translation would get wrong.

Common rules for all v2 crates (Part II 26.3): explicit state-machine
enums named after the governing specification's states, `&[u8]` in and
byte-offset spans out, arenas (`Vec<Node>` with `i32` ids, `-1` = none),
no `HashMap` iteration order reaching an output, closed error enums, and
grades `complete | partial | unsupported | failed` instead of fatal
errors. Bounds are pinned constants (Part II 26.4): 8 MiB per response,
HTML depth 512, JS nesting 4000, XML/CSS depth 64, 20,000 facts per
extractor, 200 inner documents.

### 5A.1 `ff-htmlparse` (`htmlparse`)

* 84 tokenizer states (79 of the 80 of WHATWG 13.2.5, the decimal
  character reference start state being folded into the numeric character
  reference state, plus five processing-instruction states of whatwg/html
  `c18a7c5`), the 2025
  `<select>` and `<search>` rules, scripting off. html5ever is the design
  reference (state enum, insertion-mode enum, `SplitWhitespace`, AFE
  markers) but not the oracle: it lacks the 2025 rules.
* Named references: longest match over the 2231-entry table
  (`conformance/htmlparse/entities.json`); both the trie and the sorted
  search must equal brute force (HTML-INV-11). Numeric references
  saturate. *PORT: keep this saturation; wrapping arithmetic would turn
  huge references into valid code points.*
* Encoding: BOM → transport charset → 1024-byte meta prescan → UTF-8
  autodetect → windows-1252; labels by WHATWG canonical names. *PORT:
  encoding_rs's UTF-8 decoder applies the same maximal-subpart rule; the
  other encodings come from x/text here and encoding_rs there, both built
  from the WHATWG index tables.*
* Error positions: *PORT: Col is in UTF-16 code units, not bytes or
  chars*; CR LF is one break. The tokenizer's and the tree builder's
  error lists (each in offset order) are merged by offset, tokenizer
  first on ties, which is emission order; the line/column sweep sorts an
  index stably (Rust's stable `sort_by` gives the same result).
* *PORT: Go strings may hold any bytes; this parser only ever stores valid
  UTF-8 (ill-formed input and surrogates become U+FFFD), so every string
  maps to a Rust String. The input itself stays &[u8].* *PORT:
  utf8.DecodeRune reports every ill-formed sequence as (RuneError, 1);
  the maximal-subpart width (one U+FFFD per subpart, as
  `Utf8Error::error_len` gives it) and the three-byte "generalized
  UTF-8" surrogate case (ED A0..BF xx: one surrogate-in-input-stream
  error and one U+FFFD in the tokenizer; three U+FFFD from Decode) are
  computed explicitly.*
* *PORT: the tokenizer returns a pointer aliasing an internal buffer; in
  Rust return an owned Token (or a borrow tied to &mut self).* Tag-name
  switches: match on `&[u8]` or intern. Foreign-content fix-up maps are
  lookup-only (a `match` or `phf`). `Descendants` is a push iterator; in
  Rust implement `Iterator` over the same explicit stack. *PORT: `Dump`
  and the selectedcontent clone recurse over tree depth (Go stacks grow);
  make them iterative.* The text buffer for multi-token Text nodes exists
  because Go strings are immutable (appending would copy); a Rust
  `String` can be appended in place.
* Tree errors are located at the start of the token being processed (for
  EOF: the end of the last emitted token, before any trailing unfinished
  tag). Limits: `MaxDepth` 512, Blink's and WebKit's parser depth (element
  inserted childless, parsing continues),
  `MaxAttributes` 1024, `MaxInputSize` 256 MiB (content passes 8 MiB).
* Conformance: html5lib tokenizer (7050 runs) and WPT `.dat` (1967/1973);
  the known-failure lists are asserted exactly, so port them as data.

### 5A.2 `ff-jsparse` (`jsparse`)

* The parser owns the lexical goal (Part II 29.3): lex under Div,
  re-request `/`, `/=` and `}` under RegExp or TemplateTail at exactly the
  grammar's positions (oxc's `re_lex_*`). Do not port acorn's token-context
  heuristic.
* AST node is 64 bytes, pointer-free; the `schemas` table (which slot is
  which ESTree field) is part of the contract: declare the same table.
  Canonical JSON: sorted keys, no positions. *PORT: offsets count UTF-8
  bytes, not UTF-16 code units as acorn's start/end do.*
* *PORT: a Rust &str is valid UTF-8 by construction; the port either
  validates first (and rejects or replaces) or lexes &[u8] with this same
  decoder to keep byte offsets identical.* Cooked template and string
  values may hold lone surrogates: *store cooked values as Vec<u16> or
  WTF-8 bytes and convert at the JSON boundary.*
* Numbers: *Rust's str::parse::<f64> is also correctly rounded* (round
  half to even, as NumericValue requires).
* *PORT: the Rust borrow checker rejects* the Go parser's in-place left
  operand rewrite in precedence climbing; restructure, keep the result.
* Strict mode returns the first diagnostic; tolerant mode resynchronizes
  at statement boundaries and must give the strict tree on valid input
  (JS-INV-07) and the strict error first on invalid input (JS-INV-08).
  `MaxDepth` 4000, code `too-deep`. Identifier tables: Unicode 15.0
  ID_Start/ID_Continue (match `unicode-id-start` on that version).
* Conformance: test262-parser-tests with the 7 + 5 listed exceptions and
  36 pass/pass-explicit mismatches.

### 5A.3 `ff-cssparse` (`cssparse`)

* Tokens map onto servo `cssparser::Token`; tokenize once into a
  `Vec<Token>` and build an arena (do not port cssparser's nested lazy
  parsers). *PORT: keep component-value consumption iterative; a recursive
  Rust version would overflow the thread stack on deep input long before
  MaxDepth matters.*
* *PORT: Go decodes an invalid or surrogate sequence as U+FFFD with width
  1; the Rust port must consume one byte per invalid byte the same way so
  spans and values agree.* URL references are stable-sorted by start
  offset.
* Limits 64 / 8 MiB / 20,000 URLs. The vendored css-parsing-tests run with
  `LegacyTokens`.

### 5A.4 `ff-xmlparse` (`xmlparse`, `xmlparse/sitemap`)

* Event API like quick-xml, tree like roxmltree, but **no DTD support at
  all**: refuse any internal subset or external id at its first byte.
  Errors carry a first-offending-byte offset (XML-INV-02): checks are
  incremental (end-tag names byte by byte, encoding names by prefix,
  namespace bindings at the closing `>` or `/`).
* *PORT: Go strings are byte slices; Rust names should be &str into the
  input (or String after reference decoding).* UTF-16 input is transcoded
  and offsets index the transcoded buffer; input ending inside a UTF-16
  character is an EOF condition (`unexpected_eof`), never complete.
  *PORT: time.Parse accepts fractional seconds in W3C Datetime values
  (sitemap lastmod); the Rust port must too.*
* Sitemaps: gzip decompressed to at most MaxBytes + 1; defaults 20,000
  entries / 8 MiB.

### 5A.5 `ff-sourcemap` (`sourcemap`)

* Mirrors getsentry `sourcemap` (`RawToken`, `decode_slice`,
  `SourceMapIndex::flatten`, `lookup_token`), but values must fit `i32`,
  sections must be strictly ordered (do not sort them), segments have 1, 4
  or 5 fields. *PORT: "B" (negative zero) decodes to 0, as in
  mozilla/source-map and the sourcemap crate.*
* *PORT: encoding/json is the JSON layer (serde_json in Rust). Unknown
  fields are ignored, as ECMA-426 requires. For a duplicate key
  encoding/json keeps the last value while serde's derive rejects the
  document; the port must keep the last value.* Mapping sort is stable.

### 5A.6 `ff-wellknown` (`wellknown`)

* robots.txt follows Google's `robotstxt` reference: pattern length as
  specificity, allow wins ties, leading-token user-agent matching,
  `/robots.txt` always allowed; 500 KiB cut at a line boundary.
* *PORT: time.RFC3339 parsing also accepts fractional seconds, which RFC
  3339 §5.6 allows; the Rust port must too* (`chrono::DateTime::parse_from_rfc3339`).
  `now` is an argument, never the clock.

### 5A.7 `ff-codec` (`codec`)

* Strict = canonical only (decode(s) accepted iff encode(decode(s)) == s);
  lenient ⊇ strict with the same output. Error = `{code, offset}`; a
  failing append-decoder leaves `dst` unchanged.
* Base64: three algorithms must agree byte for byte and error for error
  (CODEC-INV-06). *PORT: encoding/base64 skips CR and LF even in Strict
  mode; ours does not.*
* Percent/form: *Go's utf8.DecodeRune replaces one byte at a time and
  strings.ToValidUTF8 a whole run; neither is the Encoding Standard, so the
  decoder is written out. Rust's String::from_utf8_lossy matches it*
  (U+FFFD per maximal subpart).
* JS strings: *Go's utf8.AppendRune writes U+FFFD for surrogates, so WTF-8
  is built by hand; in Rust the same three bytes come from a wtf8 helper,
  not from char.*
* Inflate cap = min(MaxOutput, max(RatioFloor, MaxRatio × len(src)))
  (8 MiB, 100×, 1 MiB); at the cap exactly that prefix. *PORT:
  concurrency 1 makes klauspost's stream decoder synchronous. The window
  bound keeps memory proportional to the cap; a frame that declares a
  larger window stops with output_limit (offset: the bytes decoded so
  far). Rust: zstd::stream::Decoder with window_log_max.* gzip/zlib/DEFLATE come from `compress/flate`
  (Rust: `flate2` with the same caps); stream error offsets are the
  returned length.
* Sniff thresholds (base64 ≥ 24 data chars, ≥ 16 distinct, hex ≥ 32) and
  `DecodeLayers` defaults (depth 3, 8 MiB, 256 nodes) are conformance data
  (`conformance/codec/sniff.json`, `layers.json`).

### 5A.8 `ff-dnsmsg` (`dnsmsg`)

* `RData` is a tagged struct: a Rust enum. *PORT: Go strings are arbitrary
  octets here, not UTF-8; use Vec<u8>* (TXT strings, CAA values, raw data).
* Pointer rule: strictly before the last pointer followed, never reaching
  it again after a jump, at most 127 hops; anything else `pointer_loop`.
* `Encode` must match the independent reference encoder of
  `conformance/dnsmsg/roundtrip/` byte for byte: compress owner, question
  and NS/CNAME/PTR/MX/SOA names against the first earlier occurrence of the
  longest suffix (exact octets, case kept); never compress SRV targets.
  Wire integers are big-endian (DNS), unlike the Flowforge file formats.

### 5A.9 `ff-socks5` (`socks5`)

* Pure codec plus `server_handshake` / `client_connect`. Short input
  returns `short` with `need` (total length known to be required). Only
  CONNECT is served; BIND and UDP ASSOCIATE parse and are refused with
  reply 7. Read helpers read exactly one message. *PORT: Go strings hold
  arbitrary octets; usernames, passwords and domain names are kept as
  received (the RFCs do not require UTF-8), so a Rust port stores them as
  Vec<u8>, not String.*

### 5A.10 `ff-diff` (`diff`)

* Edit scripts are canonical (Part II 30.1); every algorithm must return
  the canonical script for its alignment, so scripts compare with `==`.
  `scripts.json` pins alignment choices per algorithm; `unified_git.json`
  is git 2.43 output (histogram always; patience without Myers fallback).
* *PORT: frontier values and indices are Go int (64-bit); use
  isize/usize, and keep -1 as the "unreachable" marker.* Histogram uses
  `i32` positions as git does (inputs ≤ 2^31 − 1 tokens per side).
* *PORT: keys are raw byte strings (no Unicode normalization)*; interned
  ids are dense in first-seen order. *Letters, digits and white space are
  Go's unicode.IsLetter, unicode.IsDigit and unicode.IsSpace (Unicode 15.0
  tables); Rust's char::is_alphabetic/is_numeric/is_whitespace differ in
  places, so port the same categories (L*, Nd, and Go's White_Space set).*
* JSON: *encoding/json replaces invalid UTF-8 in strings with U+FFFD and
  keeps number text verbatim; in Rust use serde_json with
  "arbitrary_precision" and "preserve_order" plus a duplicate-key check.*
  Output escaping: *\b \f \n \r \t, other control characters as \u00XX,
  U+2028/U+2029 as `\u2028`/`\u2029`, leaves <>& alone and replaces invalid
  UTF-8 with the escape `\ufffd`; reproduce that exactly for byte-identical
  patches.*
  Hash-consed subtree ids feed Myers in first-seen order.
* Bounds: `BoundedDiff` tokens (10,000 edits, 4 MiB) → lines (64 MiB) →
  hash, recording level and fallback names (`tokens|lines|hash`,
  `none|edit_distance|input_size`).

### 5A.11 `ff-sketch` (`sketch`)

* FNV-1a 64 (*the multiply wraps modulo 2^64: wrapping_mul in Rust*);
  token ids hashed as 4-byte little-endian.
* *PORT: strings.ToLower maps rune by rune with the simple (1:1) Unicode
  lowercase mapping ('İ' becomes 'i'); Rust's str::to_lowercase uses the
  full mapping ('İ' becomes "i̇") and a final-sigma rule, so port it
  rune by rune with the simple mapping. Invalid UTF-8 bytes become U+FFFD,
  which is not a letter or digit and so separates tokens.*
* SimHash: 64 `i64` counters, ±weight per bit, bit set iff counter > 0.
  MinHash: 128 permutations `(a·x + b) mod (2^61 − 1)`, `x = h mod p`,
  coefficients from `SplitMix64(0x46464d494e484153)` drawing
  `next() >> 3` until in range, `a` (≥ 1) then `b` per permutation.
  *PORT: bits.Mul64 is a full 64x64→128 multiply (u128 in Rust)*; fold
  with 2^61 ≡ 1. LSH: 32 bands × 4 rows, band key FNV-1a over the rows'
  LE u64 bytes. SKETCH-INV-10 checks `mul_add_mod61` against a schoolbook
  reference.
* `conformance/sketch/tokens.json` was produced by an independent Python
  implementation; `vecindex_compat.json` pins the pre-refactor `vecindex`
  outputs.

### 5A.12 `ff-chunkstore` (`chunkstore`, `chunkstore/fsstore`)

* `chunkstore/FORMAT.md` is the byte contract (Part II 31): FastCDC with
  gear table `SplitMix64(0x46464344)`, masks from the fixed bit list
  (MaskS `0x0000d9f003530000`, MaskL `0x0000d90003530000` at 8 KiB),
  defaults 2048/8192/65536; chunk id SHA-256 of raw bytes; FFPK, FFPI
  (fan-out, exact size `1084 + 48N`), FFMF (`24 + 32N`), FFRF, FFZB.
  CRC32C Castagnoli everywhere (`crc32c` crate).
* zstd: readers accept any frame; writers keep zstd only when
  `8 × stored ≤ 7 × raw`. Go pins level 3 / 19, window 2^23, checksum on,
  single segment, concurrency 1, and verified `EncodeAll` is deterministic
  (CHUNK-INV-12). The C library behind the `zstd` crate produces different
  frames: manifest bytes and ManifestIDs are identical across languages,
  pack/index/ref bytes only for the same compressor. Round-trip the golden
  files (`golden_*.json`) byte for byte by re-encoding their decoded
  fields with the pinned frame.
* *PORT: the delta dictionary is raw content (zstd "prefix" /
  ZSTD_dct_rawContent in the C library); the Rust zstd crate's
  with_dictionary takes it as raw content because it lacks the dictionary
  magic.*
* Write order (pack, index, ref) and GC (new packs and indexes, swap the
  ref, then delete) are crash-safety contracts tested by fault injection
  at every backend call (CHUNK-INV-10, 15). One writer per namespace.

### 5A.13 `ff-textindex` (`textindex`)

* `textindex/FORMAT.md` (FFTX, FFTD, FFTC) is the byte contract; segments
  must be byte-identical across languages except STORED's zstd data area,
  compared decompressed. uvarint is minimal LEB128 ≤ 5 bytes. *PORT: Go
  map iteration order is random; maps only index into slices sorted
  before writing.*
* Analyzer: *the unicode.Is* predicates use Go's Unicode 15.0 tables.
  Rust's char methods follow the toolchain's Unicode version; generate
  fixed tables for 15.0 so token boundaries match the conformance
  vectors.* Fold with the simple mappings (`toLower(toUpper(r))`), *not
  Rust's char::to_lowercase/to_uppercase, which are full mappings*. *An
  invalid byte decodes as (RuneError, 1) and is a delimiter; a valid U+FFFD
  is a delimiter too.*
* Query language: the EBNF and error codes of Part II 32.2 and 40.3 are
  shared; `conformance/textindex/query_parse` includes generated and
  mutated queries with spans.
* BM25: *keep the operation order of the code. Go's math.Log is not
  correctly rounded and neither is Rust's f64::ln, so scores can differ in
  the last bits across languages; conformance compares them at 1e-9
  relative.*
* Regex: Go uses `regexp/syntax` and RE2 semantics; Rust uses
  `regex-syntax`. *PORT: this walks Go's regexp/syntax tree after Simplify
  (counted repeats expanded, case-insensitive literals as FoldCase flags,
  Perl classes). Rust's regex-syntax HIR is shaped differently (case
  folding pre-expanded into classes, Unicode-aware classes by default). A
  port re-derives the same analysis on its HIR; it may produce an
  equivalent or weaker query but never one that drops a match
  (TIDX-INV-03).* `conformance/textindex/trigram_query` is therefore
  informative for a Rust port, and `search/*` is normative.

### 5A.14 `ff-content` (`content`)

* Normalizer rule tables are portable strings; their SHA-256 over the
  lines joined by `"\n"` is pinned in
  `conformance/content/normalizers/normalizers.json` (Part II 33.3). Port
  the rule strings verbatim: a different hash means a different
  normalizer version, and every stored diff records it. A learned set k
  yields `<name>.learned<k>@<n>` with its rule lines appended.
* *PORT: the package uses no maps whose iteration order reaches an
  output; every map is either a lookup table or iterated over sorted keys.
  Strings are UTF-8 byte strings; offsets are byte offsets.* Classification
  and masking sort explicitly.
* Mask placeholders `__FF_MASK_<CLASS>__`; timestamp detection window
  2000-01-01 to 2100-01-01 UTC; JSON output uses RFC 8259 §7 escaping
  (*unlike encoding/json, no HTML or U+2028 escaping*).
* Scores: *map order is random; sort before summing so float addition
  order (and so the last bit of the sum) is fixed.* Per-reason sums
  clamped to [−1, 1] and rounded to 4 decimals, total clamped to [0, 1]
  and rounded (Part II 33.11). The weight table is part of the contract.
* Lineage: *PORT: keep the linear scan; it is exact at any threshold*
  (SimHash ≤ 6 over 3-token fingerprint shingles; the multi-index of
  `sketch` is exact only to 3).
* Redaction rescans until stable (CONTENT-INV-10); the secret rule list
  and placeholder `[REDACTED:<rule>:<shape>:<length>]` are conformance
  data (`conformance/content/secrets/cases.json`).
* Go library behaviours the outputs depend on (each marked `PORT:` in
  the code): *strconv.Quote is Go's quoting (\xNN for invalid bytes,
  \u escapes for runes strconv.IsPrint rejects, U+00AD and U+2028 among
  them)*; module hashes and lineage tokens are built from it.
  *strings.Fields / strings.TrimSpace split on Unicode White_Space* (HTML
  class tokens, CSP sources; Rust's split_whitespace / trim match).
  *bytes.ToLower is Unicode-aware (the Kelvin sign lowers to "k") and
  turns invalid bytes into U+FFFD* before the integrity markers are
  matched. *encoding/json.Valid accepts invalid UTF-8 inside strings, and
  json.Unmarshal matches struct keys case-insensitively (last duplicate
  wins)* in sniffing. Attribute names of a normalized start tag are
  unique except a pre-existing `data-ff-<name>` marker, ordered by Go's
  unstable sort (insertion sort up to 12 elements). The observation id
  hashes the fetch time with exactly nine fraction digits, UTC. Of two
  features that differ only in Ref (two bundles with one lineage key),
  the smallest Ref is kept (sortFeatures).
* `conformance/content/spa/expected.json` is the end-to-end oracle:
  normalized SHA-256 per artifact, features, coverage, classes, scores,
  module diffs, lineage and observation edges.

### 5A.15 `ff-contentstore` (`contentstore`, `contentstore/pg`)

* Shares migration 0031 with Go (Part II 34.2); RLS as role `ff_content`
  with `app.tenant_id` set per transaction.
* Derived ids: *PORT: the encoding is uvarint(len) || bytes for the domain
  and each part, SHA-256, first 16 bytes, version nibble 8, variant 10*
  (same construction as the oam ids, different domains). *Record encodings
  are canonical JSON (core.CanonicalJSON).*
* Encryption: AES-256-GCM `0x01 ‖ nonce(12) ‖ ct ‖ tag`, additional data
  `ff-content-kek|<tenant>`, `ff-content-seal|<tenant>|<snapshot>|body` or
  `|headers`, `ff-content-finding|<tenant>|<snapshot>|<finding>`. *PORT:
  the nonce is 12 bytes from two Rng draws (little-endian u64, then the low
  4 bytes of the second).* A Rust store must open Go-written ciphertexts
  (and vice versa) when they share a database and KEK.
* Blob codec `delta` uses FFZB frames whose dictionaries are the delta
  base's blobs; keyframe interval 16. *PORT: container/list stores
  interface values; a Rust port uses an indexed LRU (e.g. the lru crate).
  The cache never affects output.*
* Text-index document ids `seq << 20 | position`; quotas checked exactly
  in the write transaction (CSTORE-INV-06).
* *PORT: records go through encoding/json: invalid UTF-8 in a string
  becomes U+FFFD (normalized texts that are not UTF-8 are stored as
  base64 for that reason), U+2028/U+2029 are escaped, []byte is padded
  standard base64, time.Time is RFC 3339 with up to nine fraction digits,
  float64 uses Go's shortest form.* A port that writes records into a
  shared chunk store must match them byte for byte. *The simhash bigint
  column holds the uint64 bit pattern.*

### 5A.16 v2 parts of `ff-oam`, `ff-graphstore`, `ff-alerts`, `ff-dsl`, `ff-bytecode`

* oam: Endpoint keys and `NormalizeURLTemplate` (Part II 28.1; *PORT:
  step 1 trims with strings.TrimSpace (Unicode White_Space, as Rust's
  str::trim); step 6 inherits netnorm's EscapedPath percent-encoding*);
  authority units, the coverage gate and the event order of Part II 28.4
  (event ids derive from the emission index, so the order is a contract);
  `tag_changed` patches with root add/remove; `ContentChangePatch`
  normalization.
* oam: *ContentChangePatch.Normalize and CheckNoSecrets walk maps, so
  with several bad keys the one an error names varies (the verdict does
  not); LooksLikeSecret's entropy uses math.Log2, not correctly rounded,
  so a run within an ulp of 3.5 bits may be judged differently.*
* graphstore: AsOf replay from the empty graph; commonness threshold 50;
  `conformance/graphstore` (gs_001-gs_006) runs against every store
  (`TestGraph001_*`).
* alerts: the `diff` section (Part II 36.5); `diff.score` is `A0202`.
* dsl: the content records are appended after `TaskError` and the six
  codec builtins after `list_append`; builtin type ids and builtin ids are
  append-only, so a Go-compiled FFBC module that calls them must load in
  the Rust VM.
* tools (services/toolrunner): *Go's x509 splits extended key usages
  into known ones and unknown OIDs, so the TLSCertificate lists the known
  usages first, whatever their DER order.* The REST snapshot body is a
  JSON string, so invalid UTF-8 becomes U+FFFD there.
* egress (services): EDNS ticket option code 65280; SOCKS5 username =
  decision id, password = ticket; refusal reasons (Part II 40.4) are
  stable strings shared by both implementations' proxies.

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

## 9. Knowing what callers need before porting them

Porting bottom-up means porting `core` before any of the code that uses
it. Three sources tell you what that code will need, in this order:

1. **The contract:** the spec section the package implements, its
   `doc.go`, and its conformance data. Port the behaviour these define,
   not the uses you happen to know about.
2. **The uses, from the type checker:**

   ```sh
   go run ./cmd/xtask usage core                     # each exported name: who uses it, where
   go run ./cmd/xtask usage -by consumer core        # each using package: what it needs, in layer order
   go run ./cmd/xtask usage -tests -md core > /tmp/core-usage.md
   ```

   The argument is a module name from `modules.toml` or a package
   directory (`core/netnorm`). The report comes from `go/types`, so it
   counts method calls through interfaces, struct fields and aliased
   imports, which a grep misses. Each name shows how many packages use it,
   their layers, and example `file:line` sites to read for intent.
3. **The leftovers:** the report ends with two lists.
   * *Called implicitly:* methods with no call site that something still
     calls, either an interface in the same package (`SystemClock.Now`
     through `Clock.Now`) or the standard library (`Error.Error`,
     `UUID.MarshalText`, `UUID.Scan`). Port these as trait impls
     (`Display`, `std::error::Error`, `Serialize`, `FromSql`/`ToSql`).
   * *Not used outside the package:* candidates to postpone, unless the
     package's own tests or conformance data need them.

Port the top of the by-identifier list first; it is what most of the
module depends on. Before starting a crate, run `-by consumer` on each
package it depends on, to get its exact needs.

## 10. Suggested order

Follow spec 24.7, which is also the order the Go reference was built in.
The v2 layer-1 crates depend only on `ff-core`, so they come right after
it; the rest of v2 follows v1's order (spec Part II 27):

1. workspace, `ff-core` (with netnorm), `ff-testkit`, `xtask`, CI;
2. **v2 layer-1 crates, in any order or in parallel**: `ff-codec`,
   `ff-sketch`, `ff-diff`, `ff-htmlparse`, `ff-jsparse`, `ff-cssparse`,
   `ff-xmlparse`, `ff-sourcemap`, `ff-wellknown`, `ff-dnsmsg`,
   `ff-socks5`, `ff-chunkstore`, `ff-textindex`. They are the most
   conformance-heavy, self-contained crates, a good place to prove the
   Rust toolchain (fuzzing, fixed Unicode 15.0 tables, zstd settings)
   before anything stateful;
3. `ff-limiter`, `ff-scope`, `ff-oam`, `ff-csr`, `ff-placement`,
   `ff-auth`, `ff-alerts` — pure and fully covered by conformance data
   today;
4. `ff-dsl`, `ff-bytecode`, `ff-vm`, `ff-compiler` (prove cross-loading of
   Go-compiled modules and identical args hashes early);
5. `ff-eventstore` (memory, then Postgres against the shared schema),
   `ff-engine`;
6. `ff-placement` in multi-worker mode, `ff-sim`;
7. services and gateway (tonic, axum), sharing `proto/`, the schema and
   Redis with the Go services;
8. `ff-vecindex` (with real SIMD; it uses `ff-sketch`);
9. **the rest of v2**, in dependency order: the v2 parts of `ff-oam`
   (Endpoint, labels, authority, `tag_changed`, `content_changed`) and of
   `ff-alerts` (the `diff` section) and the content records and codec
   builtins of `ff-dsl`/`ff-bytecode`; `ff-content`; `ff-graphstore`'s
   AsOf and commonness with `conformance/graphstore`; `ff-contentstore`
   (memory, then Postgres against migration 0031); the content service,
   the v2 tools and the egress proxy's SOCKS5, DNS and per-target limits;
   then the lab acceptance of Part II 38;
10. agent step — deferred in Go as well (`docs/DECISIONS.md`).
