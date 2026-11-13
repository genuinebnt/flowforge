# Flowforge v2 design brief: content capture, change diffs, diff search

Status: **design record; folded into the specification.** This brief was
the contract every v2 package was built against. It, each package's README
and OPEN_QUESTIONS are now folded into `docs/flowforge-spec.html` as
**Part II** (sections 26-41, plain text in `docs/spec.txt`), with exact byte
formats, record fields, the invariant register and error codes. **Part II
is normative**; where this brief and the code disagreed, the code was
followed, this brief was amended below, and spec Part II section 41 lists
every difference. Edit Part II (and the code), not this file, for future
changes.

Goal: workflows capture what targets serve (HTML, JavaScript, CSS, XML,
JSON, headers, certificates) and store every version compactly. They
compute noise-robust diffs and turn meaningful changes into graph changes
and alerts. The whole change history is searchable. All parsers are our
own explicit state machines, written so the Rust port is a translation of
the same state machines checked against the same data.

---

## 1. Portability rules (v2 packages MUST follow; v1 packages SHOULD)

These are in addition to `docs/CONVENTIONS.md`.

1. **State machines are explicit.** A tokenizer is a `state` enum plus a
   loop over input that switches on it. State names match the governing
   specification's names (WHATWG "tag open state" → `stateTagOpen`), and
   each state's doc comment cites the spec section. No regexes in
   tokenizers. No parser generators.
2. **Bytes in, spans out.** Parsers take `[]byte` (UTF-8 after decoding),
   report positions as byte offsets (plus line/column where users see
   them), and never retain the input after returning unless documented.
3. **Trees are arenas.** DOM and AST nodes live in a slice and refer to
   each other by `int32` index (`NodeID`, `-1` = none), never by pointer.
   A node is a struct with a `Kind` enum plus fields. This maps 1:1 to a
   Rust `Vec<Node>` with `enum Kind` or a struct with index fields.
4. **No `any` / `interface{}` / reflection in core data paths.** Public
   parser and format APIs use concrete types. No `map` iteration order may
   affect output. Use sorted slices or insertion-ordered structures.
5. **No goroutines in pure packages** unless the parallelism is optional,
   deterministic and off by default (for example a parallel build that
   gives byte-identical output).
6. **Binary formats are specified byte by byte**: little-endian, explicit
   lengths, versioned magic, CRC32C (Castagnoli) per section, and a
   SHA-256 or CRC footer. No Go `encoding/gob`, no Go-specific text
   formats. Durations are integer milliseconds or nanoseconds, never Go
   duration text. Times are RFC 3339 UTC with milliseconds, or Unix
   nanoseconds.
7. **Compression is zstd** (RFC 8878) through
   `github.com/klauspost/compress/zstd`. Rust uses the `zstd` crate. Levels
   and window sizes are pinned in the format so output is reproducible.
8. **Errors are stable codes.** Each parser has a closed list of
   error/diagnostic codes (WHATWG parse-error names for HTML, our own for
   others), returned as values. Recovery behaviour is part of the spec.
9. **Determinism.** The same input gives the same bytes out: trees,
   diffs, chunk boundaries, index segments, hashes. Every seeded table
   (for example the FastCDC gear table) is generated from
   `core.SplitMix64` with a documented seed.
10. **Tests are invariants, not examples.** Every package ships:
    * **conformance data** in `conformance/<suite>/` (language-neutral
      JSON or the upstream suite in `conformance/third_party/`);
    * **numbered invariants** (`<PKG>-INV-NN`), each with a test named
      `Test<Pkg>Inv<NN>_<Name>` and a one-line statement in the package
      README. The Rust port reimplements exactly this list;
    * **differential tests** against an oracle where one exists
      (`golang.org/x/net/html` for HTML; brute-force reference
      implementations for diff and index);
    * **fuzz targets** with the invariants as fuzz assertions;
    * **benchmarks** with an acceptance shape (for example "linear in
      input size within 20 %").

    Tests that only check that a happy path returns something are not
    acceptable.

---

## 2. Packages and layers

| Layer | Package | Purpose | Governing spec |
|---|---|---|---|
| 1 | `htmlparse` | WHATWG HTML tokenizer + tree construction + arena DOM + serializer + encoding sniffing | HTML Living Standard §13.2 (parsing), §13.3 (serialization) |
| 1 | `jsparse` | ECMAScript 2024 lexer + parser → arena AST (ESTree node kinds) + scope-free analysis helpers | ECMA-262 15th ed. clauses 12–16 |
| 1 | `cssparse` | CSS Syntax Level 3 tokenizer + parser (rules, at-rules, declarations, `url()`) | CSS Syntax Module Level 3 |
| 1 | `xmlparse` | XML 1.0 (5th ed.) non-validating tokenizer; DTDs and external entities refused (XXE-safe) | XML 1.0 |
| 1 | `sourcemap` | Source Map v3: VLQ decoding, sections, `sourcesContent`, mapping lookup | ECMA-426 (Source Map) |
| 1 | `wellknown` | robots.txt (RFC 9309), security.txt (RFC 9116); sitemaps are `xmlparse/sitemap` (layer-1 packages may not import each other) | RFCs |
| 1 | `codec` | base64, base32, hex, percent and form encoding, quoted-printable, JS string escapes, bounded inflate (gzip, zlib, DEFLATE, zstd), blob sniffing, bounded recursive decoding | RFC 4648, 3986, 2045 §6.7, 1950-1952, 8878; WHATWG URL; ECMA-262 §12.9.4 |
| 1 | `dnsmsg` | DNS wire format: strict and lenient decoding, canonical encoding with compression, truncation, TCP framing (the proxy's DNS forwarder, §9) | RFC 1035 §4, 3596, 2782, 6891, 8659 |
| 1 | `socks5` | SOCKS 5 codec and handshakes, CONNECT only, username/password auth (§9) | RFC 1928, RFC 1929 |
| 1 | `diff` | Myers (linear space), patience, histogram over interned token ids; hunks; unified rendering; JSON structural diff (RFC 6902 ops); DOM tree diff | Myers 1986; RFC 6902 |
| 1 | `sketch` | SimHash, MinHash and LSH, and shingling, shared by `content` and `vecindex` | — |
| 1 | `chunkstore` | FastCDC content-defined chunking, zstd-compressed packfiles, manifests, GC; memory and filesystem backends | FastCDC (Xia et al. 2016) |
| 1 | `textindex` | Inverted index + trigram index: segments, posting compression, BM25, phrase, regex via trigram prefilter, query language | IIR ch. 1–6; Cox 2012 |
| 2 | `content` | Capture profiles, per-mime normalization (versioned), volatile masking, feature extraction, chunk-lineage matching, change classification → oam observations | this brief §5 |
| 4 | `contentstore` | Snapshot repository port (memory; `contentstore/pg` adapter), capture ingest, diff pipeline, search integration | this brief §6 |
| 6 | `services/content` | `ContentService` gRPC (`proto/flowforge/v1/content.proto`), events, sweeps; gateway REST routes in `services/gateway` | §7–§8 |
| 6 | `services/toolrunner` (v2 tools) | `collect`, `crawl`, `wellknown`, `extract`, `unpack_sourcemap`, `diff_latest`, `search_changes`, `ctlog`, `rdap`, `asn`, `tlscert`, `portscan`, `dnsresolve` 2 (`crawl` is a tool, not a package) | §8 |
| 6 | `services/egressproxy` (v2 features) | CONNECT any port, SOCKS5, DNS forwarder, per-target GCRA, intel allowlist, notify path | §9 |
| test | `services/internal/lab` | the in-process lab and the §10 acceptance test | §10 |

`textindex` and `chunkstore` import only `core` (and zstd).
`codec` imports only `core` and zstd; `dnsmsg` and `socks5` only `core`. `content` imports the layer-1 parsers, `diff`, `sketch`
and `oam`. `vecindex` is refactored to use `sketch` (no behaviour change;
its conformance data must still pass).

---

## 3. Graph expansion (oam, graphstore, migrations)

All additions are Flowforge extensions (ff). OAM import and export keep
working because unknown types are stored opaquely (spec 5.2).

### 3.1 Asset types

| Asset | Content fields | Canonical key |
|---|---|---|
| `Endpoint` | `method` (upper case or `*`), `url_template` (normalized URL with `{name}` placeholders for path segments that vary), `source` (`html_form` \| `js_literal` \| `js_call` \| `openapi` \| `graphql` \| `sitemap` \| `observed`) | `ep:<method> <url_template>` |

`url_template` normalization: netnorm URL rules on the concrete parts,
with path segments that are numeric, UUID-shaped, long hex, or a JS
template substitution replaced by `{id}`/`{uuid}`/`{hex}`/`{expr}`.
Hosts are never templated: an endpoint without a concrete host belongs to
the page's origin.

### 3.2 Relation labels (SimpleRelation unless noted)

| Label | From → To | Meaning |
|---|---|---|
| `loads_script` | URL → File | `<script src>`, module imports, `import()` |
| `loads_style` | URL/File → File | `<link rel=stylesheet>`, CSS `@import` |
| `embeds` | URL → URL | iframe, object, embed |
| `links_to` | URL → URL | anchors, `<link>` other than stylesheet |
| `redirects_to` | URL → URL | observed HTTP redirect hop (content: `status`) |
| `form_action` | URL → Endpoint | form submission target |
| `calls` | File/URL → Endpoint | API call found in JS or observed |
| `source_map` | File → File | bundle → its source map |
| `previous_version` | File → File | chunk lineage across renamed bundles |
| `includes_library` | File → ProductRelease | library banner or fingerprint inside a bundle |
| `documents` | URL → Endpoint | OpenAPI or GraphQL schema describing an endpoint |

The 5.5 endpoint validation table is extended with these rows.

### 3.3 Properties

* `ContentProperty {snapshot_id, body_sha256, size, mime, profile,
  normalizer_version, last_changed_at, change_count}`: one per (entity,
  profile), and the graph's pointer into the content store.
* `HTTPHeaderProperty {name, value}`: security-relevant headers (CSP,
  HSTS, CORS, X-Frame-Options, Server, cookies' attribute summary, never
  cookie values).
* `ParameterProperty {name, location: query|body|path|header|cookie,
  type_hint}` on Endpoint.
* Secrets: detection is by **named, structured rules** (provider key
  formats, connection strings, JWTs, private-key blocks). Bare entropy
  scoring is a low-confidence tier only, never a finding on its own.
  Sentinel rejected generic entropy detection as too noisy. Findings use
  `VulnProperty` with `category = "exposed_secret"`, holding only
  rule id, redacted preview (first 4 and last 2 characters) and a keyed
  hash (HMAC-SHA256 with a per-tenant key) for deduplication. The secret
  itself never enters the graph or logs. It is stored encrypted in the
  content store's `findings` (§6.4).

### 3.4 Change events and staleness

* New change-event types: `content_changed` (patch = `{snapshot_from,
  snapshot_to, diff_id, levels: [hash|text|semantic], summary}`) and
  `tag_changed` (entity or edge tag created, changed or removed; patch =
  JSON patch of the tag content). `tag_changed` applies to every tag
  type, not only content. A migration extends the check constraint.
* **Authoritative staleness, gated on coverage** (amended after the
  Sentinel review: an ungated rule would let one truncated, failed,
  login-redirected or WAF-challenged capture withdraw every edge). An
  observation may declare `authority = {source_entity, profile, coverage:
  complete | partial | failed | unsupported, covered_rules}`.
  * **Only `coverage = complete` withdraws.** Edges from `source_entity`
    whose rule family is in `covered_rules`, present in the previous
    authoritative set and missing now, are **withdrawn** immediately
    (`edge_removed`, state stale, history kept, can return).
  * A capture is complete only if the capture outcome is complete (no
    truncation, no collection-integrity flag, §12.5), the body normalized
    completely, and every extractor that ran reported complete with none
    canary-disabled. (Amended: coverage is decided per capture, before any
    diff, so a diff fallback does not affect it; spec Part II 28.4.)
  * Partial, failed or unsupported observations are **positive-only**: they
    add and refresh, and never withdraw.
  * An extractor that did not run, or ran partially, never withdraws its
    own edges.
  * Entities lose liveness only when no edge or complete authoritative
    source still reports them.
  * The covering-run staleness of spec 5.7 is unchanged for every other
    observation.
* **Commonness:** a node held by more than 50 distinct sources or hosts (a
  shared CDN script, a favicon, a common library) is marked `common`, and
  pivots and leads ignore it. Association is not permission: shared
  infrastructure never implies ownership.
* CSR blast-radius labels gain `loads_script`, `loads_style` and `embeds`.
* `graphstore` gains `AsOf(seq)` neighbour and entity reads, served by
  replaying change events (built: from the empty graph, which the nearest
  snapshot is in this version; spec Part II 28.7).

---

## 4. Parsers (requirements beyond §1)

### 4.1 `htmlparse`

* **Input decoding:** BOM sniffing, transport `charset`, and the meta
  prescan algorithm (§13.2.3.2) with the confidence states. Decoding goes
  through `golang.org/x/text/encoding` (WHATWG labels; Rust:
  `encoding_rs`). The tokenizer runs on UTF-8.
* **Tokenizer:** all states of §13.2.5, including RCDATA, RAWTEXT, script
  data with escape and double-escape states, PLAINTEXT, CDATA, character
  references with the full named-entity table (generated once from the
  WHATWG list into `htmlparse/entities_table.go` and
  `conformance/htmlparse/entities.json`), numeric reference fix-ups, and
  every parse error by its spec name.
* **Tree construction:** every insertion mode, the stack of open
  elements, active formatting elements with Noah's Ark clause, the
  adoption agency algorithm, foster parenting, `template` contents and the
  template insertion-mode stack, foreign content (SVG and MathML,
  attribute and tag name adjustments, integration points), quirks-mode
  determination, and fragment parsing with a context element. Scripting
  flag is **off** (we never execute script; `noscript` content parses as
  markup).
* **Output:** arena DOM (`Document{Nodes []Node}`), the html5lib test
  tree format for conformance, and the HTML fragment serialization
  algorithm.
* **Conformance:** `conformance/third_party/html5lib-tests/tokenizer`
  (all) and `conformance/third_party/wpt-html-parsing/*.dat` (the
  scripting-off and fragment cases). The target is 100 %; any remaining
  failure is listed by test id in `htmlparse/OPEN_QUESTIONS.md` with a
  reason.
* **Differential oracle:** `golang.org/x/net/html` (tests only). Our tree
  must equal its tree on the fuzz corpus and on generated documents,
  except known oracle deviations listed in OPEN_QUESTIONS.
* **Invariants (minimum):**
  1. The concatenation of token source spans covers the input exactly once.
  2. Parsing is total (any byte string yields a document, no panic).
  3. Serialize-then-reparse gives the same tree (for documents without
     the spec's known non-round-trip cases).
  4. Time and allocation are linear (a benchmark at n and 4n stays within
     20 % per byte).
  5. Open-element stack depth is bounded by the configured limit, **512 by
     default** (`htmlparse.DefaultMaxDepth`, also `content.MaxHTMLDepth`),
     with the error code `nesting-depth-limit-exceeded` when exceeded (the
     element is inserted childless and parsing continues).

### 4.2 `jsparse`

* **Lexer:** a state machine over UTF-8 with the lexical goals
  `InputElementDiv`, `InputElementRegExp`,
  `InputElementRegExpOrTemplateTail` and `InputElementTemplateTail`. The
  parser selects the goal. This is how regex versus division is decided
  correctly, not by heuristics. It handles Unicode escapes in identifiers
  (ID_Start/ID_Continue tables from `unicode`), numeric literals
  (separators, BigInt, legacy octal in sloppy mode), template literal raw
  and cooked values, the hashbang, line terminators (LS/PS), and HTML-like
  comments in scripts (Annex B).
* **Parser:** recursive descent with precedence climbing for binary
  operators. Covers the full ES2024 script and module grammar: classes
  (fields, private names, static blocks, accessors), async functions,
  generators, async generators, destructuring via cover grammars, arrow
  functions, optional chaining, `??` mixing rules, regex flags `v` and
  `d`, `import.meta`, top-level await in modules, labelled statements,
  ASI, `let` ambiguities, and Annex B web-compat syntax. Early errors are
  covered by the `early` suite as far as listed in OPEN_QUESTIONS.
  Error-tolerant mode (for minified or broken bundles) recovers at
  statement boundaries and reports diagnostics.
* **AST:** arena nodes whose kinds and field names follow ESTree, so the
  JSON form is comparable with acorn, oxc and swc output. A canonical JSON
  form is used for conformance.
* **Conformance:** `conformance/third_party/test262-parser-tests`: every
  `pass` file parses; every `fail` and `early` file is rejected (listed
  exceptions only). AST of `pass/X.js` equals AST of `pass-explicit/X.js`
  (vendor `pass-explicit` for this) — that is the ESTree-equivalence
  invariant the upstream suite is designed for.
* **Invariants (minimum):**
  1. Token spans tile the input (whitespace and comments included as
     trivia).
  2. Totality (error-tolerant mode never panics; strict mode returns a
     diagnostic or an AST).
  3. Print-then-reparse gives the same AST (our printer emits minimal,
     parenthesized code).
  4. Linear time on minified bundles (benchmark on generated 1–10 MB
     bundles).
  5. The regex/division decision matches the grammar on a generated
     corpus of ambiguous contexts.
  6. Syntactic nesting (statements, expressions, binary-operator chains)
     is bounded, **4000 by default** (`jsparse.DefaultMaxDepth`), code
     `too-deep`.

### 4.3 `cssparse`, `xmlparse`, `sourcemap`, `wellknown`

* **`cssparse`:** the CSS Syntax 3 tokenizer and "consume a list of
  rules / declarations", with `url()`, `@import`, `image-set()` and
  `src:` extraction. Conformance against the community css-parsing-tests
  JSON if vendorable, otherwise our own suite derived from the spec's
  examples. Invariants: token tiling, totality, linearity.
* **`xmlparse`:** elements, attributes, namespaces, text, CDATA,
  comments, PIs, predefined and numeric entity references. `<!DOCTYPE>`
  with an internal subset or any external reference is rejected with an
  error code (XXE and billion-laughs safe by construction). Invariants:
  well-formedness errors are reported at the first offending byte,
  totality, nesting depth cap.
* **`sourcemap`:** strict v3 parsing, VLQ base64 decoding with overflow
  checks, index maps (sections), `sourcesContent` extraction, and
  generated-to-original position lookup. Invariants: VLQ encode/decode
  round trip, and mappings sorted and in bounds.
* **`wellknown`:** RFC 9309 robots.txt (groups, longest match, allow over
  disallow tie-break, 500 KiB limit), RFC 9116 security.txt (fields,
  `Expires` required, signed-message tolerance), and sitemaps (urlset and
  sitemapindex; built in `xmlparse/sitemap` with defaults of 20,000
  entries / 8 MiB per §12.10, raisable to the protocol's 50,000 URLs /
  50 MB). Each with conformance cases
  from the RFC examples plus edge cases.

---

## 5. `content`: normalization, extraction, lineage, classification

* **Capture profile:** `{name, method, headers (fixed, sorted), user_agent,
  cookie_jar_ref (none by default), follow_redirects, accept}`. Diffs
  compare only snapshots with the same (URL, profile).
* **Normalizers per mime type**, each identified by `normalizer_version =
  "<name>@<n>"`, plus a SHA-256 of its rule table:
  * **HTML:** parse, drop comments, collapse whitespace in text, sort
    attributes, and mask volatile attribute values (nonce, CSRF-named
    inputs, `data-*` timestamps), giving one line per element or text
    node.
  * **JS:** lex and re-print one statement per line with canonical
    spacing. Mask literals matching volatile rules (timestamps, build
    hashes, random ids). If the bundle has a source map, also normalize
    the original sources.
  * **CSS:** tokenize and re-print one declaration per line.
  * **JSON:** canonical JSON. **XML:** canonical element and attribute
    order where the format allows. **Headers:** sorted, the volatile ones
    (Date, Age, request ids) dropped.
  * **Learned volatility:** per (URL template, profile, feature path) the
    store keeps counters of "changed while page hash otherwise stable".
    A rule that passes a threshold is frozen into a new
    `normalizer_version`, never applied silently. Every diff records the
    exact version it used, so diffs replay identically.
* **Feature extraction** (each feature is typed and keyed): links,
  scripts, styles, iframes, forms (action, method, inputs), endpoints from
  JS (`fetch`/XHR/axios/`$.ajax` call arguments, string and template
  literals that look like paths, route tables), GraphQL operations,
  `sourceMappingURL`, library banners and known globals, CSP and other
  headers, meta generator, and secrets (rule table plus entropy).
  Features become oam observations with `authority` set (§3.4).
* **Chunk lineage:** for a new script URL with no previous capture, find
  the best predecessor among the page's previously loaded scripts. Match
  by the URL with hash-like segments stripped, then SimHash Hamming
  distance ≤ 6 over normalized token shingles, with ties broken by size
  ratio. A match emits `previous_version` and the diff is computed against
  the predecessor.
* **Change classification:** level 1, hash differs; level 2, text hunks
  differ after normalization; level 3, the feature-set difference (added
  or removed endpoints, forms, parameters, scripts, headers, libraries,
  secrets). Level-3 changes map to graph changes. Summary counts go into
  `content_changed`.
* **Bounds:** Myers runs with a cap on edit distance D (default 10,000
  token edits) and on input size (default 4 MiB normalized). Past a cap,
  the diff falls back to histogram diff on lines, then to hash-only, and
  records which.
* **Noise metric:** `false_change_rate` per program, measured as text-level
  changes with zero level-3 changes on pages whose features are stable.
  Exported as a metric; target below 5 %.

---

## 6. Storage and compression

### 6.1 Chunk store (`chunkstore`)

* **FastCDC** with normalized chunking: min 2 KiB, average 8 KiB, max
  64 KiB, and two masks (`MaskS` and `MaskL` per the paper). The gear
  table holds 256 × u64 values from `SplitMix64(seed = 0x46464344)`
  ("FFCD"). Chunk id is SHA-256 of the uncompressed chunk.
* **Packfile "FFPK":** header (magic, version u16, flags u16, created
  Unix ns), then chunk records `{len u32, crc32c u32, zstd frame}`, then
  footer CRC32C. zstd level 3, no dictionary in version 1, frame checksum
  on. **Pack index "FFPI":** sorted chunk ids (32 bytes each), with
  offset u64, compressed length u32 and raw length u32, binary searchable,
  and a fan-out table on the first byte as in git.
* **Manifest:** a snapshot body is an ordered list of chunk ids plus the
  total length, encoded as a fixed binary record ("FFMF", spec Part II
  31.5) and itself stored as a chunk. A ref ("FFRF") names the live packs
  and is the single commit point; delta frames ("FFZB") serve the content
  store's delta codec.
* **Garbage collection:** mark from live manifests (retention-aware),
  sweep unreferenced chunks by rewriting packs whose live ratio is below
  50 %. GC is crash-safe: new packs are written first and old ones
  deleted after the index swap.
* **Metrics:** dedupe ratio and compression ratio per program. The
  acceptance benchmark stores 50 versions of a real-world-sized bundle
  with small edits and must use less than 15 % of the raw total.

### 6.2 Snapshot records (`contentstore`, migration 0031, schema `content`)

The records live in their own `content` schema, owned by role
`ff_content`, with forced RLS like the others (migration
`0031_content.sql`; the full DDL is spec Part II 34.2). As built, after
§6.4, §11.18 and the §12.12 storage rules:

```text
content.programs(tenant_id, program_id, diff_seq, created_at)
content.snapshots(id, tenant_id, program_id, entity_id, url, url_template, host,
  profile, run_id, effect_path, fetched_at, last_seen, valid_from, valid_to,
  status, mime, kind, signature, headers_ref, body_manifest, body_sha256,
  raw_size, stored_size, normalizer_version, normalized_manifest, parent_id,
  lineage_parent_id, features_ref, outcome, coverage, degraded, simhash,
  pinned, pin_reason, codec, delta_base, delta_depth, body_seal, headers_seal,
  key_id, created_at)   -- partial unique index on the open version (valid_to IS NULL)
content.diffs(id, tenant_id, program_id, seq, from_snapshot, to_snapshot, levels,
  algorithm, normalizer_version, stats, hunks_ref, semantic_ref, stored_size,
  index_docs, at, url, profile, created_at)
content.findings(id, tenant_id, program_id, snapshot_id, rule_id, preview,
  keyed_hash, ciphertext, key_id, created_at)  -- secrets, encrypted at rest
content.volatility(tenant_id, program_id, url_template, profile, feature_path,
  changes, stable_observations, streak, frozen_version)
content.quota_limits(tenant_id, captures_per_day, stored_bytes, index_bytes)
content.quota_usage(tenant_id, day, captures, stored_bytes, index_bytes)
content.index_state(tenant_id, program_id, watermark, docs, bytes)
content.data_keys(tenant_id, purpose, version, kek_id, wrapped, created_at)
content.outbox(id, event_id, topic, ordering_key, payload, created_at, published_at)
```

Each column holding a "...ref" or "...manifest" is a chunk-store
ManifestID of the tenant's chunk store. Per-program retention defaults:
last 50 snapshots or 180 days per (URL, profile), whichever keeps more,
plus pinned versions and the open version; diffs follow their snapshots.

### 6.3 Other compression points

* Diff hunks and semantic records are canonical JSON, zstd-compressed,
  stored as chunks.
* Text-index posting lists are delta and varint coded, with block skip
  pointers every 128 postings. Positions are delta coded per document.
* Raw tool output (spec 6.1 `raw_ref`) moves into the chunk store too.

### 6.4 Secrets

Findings are encrypted with AES-256-GCM using a per-tenant data key
(envelope encryption). As built (spec Part II 34.4): data keys are 32
random bytes per tenant and purpose (`enc` for findings and redaction
seals, `hash` for the HMAC-SHA256 keyed hashes), stored only wrapped by a
key-encryption key in `content.data_keys`. The KEK is the
`contentstore.KeyEncrypter` port (`ID`, `Wrap(tenant, dek)`,
`Unwrap(tenant, wrapped)`, bound to the tenant as additional data):
`LocalKEK` from `FF_CONTENT_KEK` in development and tests; a Cloud KMS
adapter fits the same port and is v3 work (it is not the v1 `Secrets`
port). Only the redacted preview and keyed hash are searchable.

---

## 7. Search (`textindex` + content service)

* **Documents** are diff hunks (added and removed sides indexed
  separately, with the `side` field), semantic change records, and
  optionally latest normalized snapshots. Fields: `program`, `url`,
  `url_template`, `kind`, `side`, `feature`, `mime`, `at` (date).
* **Tokenizer** for code and URLs: split on non-alphanumerics and on
  camelCase and snake_case boundaries, keep the original token as well,
  lowercase, no stemming.
* **Segment format "FFTX":** term dictionary (sorted, prefix-compressed
  blocks of 64), postings (delta+varint, skips), positions, doc-values
  columns for filters, stored fields (zstd), tombstone bitmap, and CRC32C
  per section. Immutable. Merged by a size-tiered policy.
* **Trigram index** over hunk text for regex queries. The regex is
  compiled (via `regexp/syntax`) into a trigram boolean query, Cox-style:
  candidates come from the trigram index and are verified by full regex
  match. A regex with no usable trigrams is rejected unless the filters
  narrow the candidate set below a cap.
* **Query language** (its own lexer state machine plus recursive
  descent): terms, `"phrases"`, `/regex/`, `field:value`, `-negation`,
  `AND` / `OR` / parentheses, `since:7d`, `kind:endpoint_added`,
  `side:added`. Grammar and errors are specified, so the Rust port shares
  them.
* **Ranking:** BM25 (k1 = 1.2, b = 0.75), with recency as a tie-breaker.
  Hybrid mode fuses with `vecindex` through the existing RRF.
* **Indexing** is incremental from content change events with a
  watermark, one index per (tenant, program). It can always be rebuilt
  from stored diffs.

---

## 8. Tools (tool-runner adapters) and the workflow surface

| Tool | DSL signature | Class | Emits |
|---|---|---|---|
| `collect` | `(InScope<Url>, String) -> Snapshot` (profile name) | active | snapshot; URL, File, `ContentProperty`, `HTTPHeaderProperty`, `redirects_to` |
| `crawl` | `(InScope<Url>, Int) -> List<Url>` (budget) | active | URL frontier: canonicalized, robots-aware, every URL scope-checked |
| `wellknown` | `(InScope<Host>) -> List<Url>` | active | robots.txt, sitemap, security.txt, OpenAPI/Swagger, GraphQL endpoint discovery |
| `extract` | `(Snapshot) -> Features` | read | Endpoint, File, Product(Release), edges with authority, findings |
| `unpack_sourcemap` | `(Snapshot) -> List<Snapshot>` | read | original sources as derived snapshots |
| `diff_latest` | `(Snapshot) -> ChangeSet` | read | `content_changed`, level-3 graph changes |
| `search_changes` | `(String, Int) -> List<ChangeHit>` | read | none (query) |
| `ctlog` | `(Fqdn) -> List<Fqdn>` | passive | TLSCertificate, `common_name` / `san_dns_name` edges |
| `rdap` | `(Host) -> List<Organization>` | passive | DomainRecord, IPNetRecord, AutnumRecord, Organization, registration edges |
| `asn` | `(IpAddress) -> Option<Asn>` | passive | AutonomousSystem, Netblock, `announces` / `contains` edges |
| `dnsresolve` (extended) | `(Fqdn) -> List<IpAddress>` | passive | adds CNAME, MX, NS, TXT records (DNSRecordProperty for TXT) |
| `portscan` | `(InScope<Host>, List<Int>) -> List<Int>` | active | Service + `port` PortRelation (supersedes `tcpconnect`, kept as an alias) |
| `tlscert` | `(InScope<Host>, Int) -> Option<TlsCertificate>` | passive (D3) | TLSCertificate, `certificate` edge, `common_name` / `san_dns_name` edges |
| `httpprobe` | unchanged | active | unchanged |

`Snapshot`, `Features`, `ChangeSet` and `ChangeHit` are standard-library
records added to the DSL prelude; their exact fields, in order, are spec
Part II 36.2 (`dsl/prelude.go` `contentRecords`):

* `Snapshot{id, url, profile, status, mime, kind, body_sha256, raw_size,
  fetched_at, parent_id?, lineage_parent_id?, coverage, degraded,
  new_in_run, redirects, resources}`;
* `Features{snapshot_id, url, coverage, links, scripts, styles, frames,
  sourcemaps, endpoints, forms, parameters, headers, libraries, secrets}`;
* `ChangeSet{snapshot_id, from_snapshot_id?, diff_id?, url, outcome, levels,
  semantic, class, newness, cause, baseline, alertable, score, changes,
  summary}`;
* `ChangeHit{score, at, diff_id, snapshot_id, url, kind, side, feature,
  class, newness, cause, text}`.

The v2 builtins `base64_decode/encode`, `url_decode/encode` and
`hex_decode/encode` are Part II 36.3. As built, `extract` and
`diff_latest` are read-only: the content service's event is the single
writer of the graph facts listed for them above (Part II 36.6). Passive tools that fetch from public sources (crt.sh, RDAP servers)
still go through the egress proxy, but they are checked against an
allowlist of intelligence sources, not the program scope: the target is
the source, not the program asset. That distinction is spelled out in
Part II.

A reference monitoring workflow ships in `conformance/workflows/monitor.ff`
and is exercised end to end against the lab (§10).

---

## 9. Egress: TCP, DNS and per-target rate limits (gap fixes)

* **TCP path:** the egress proxy accepts `CONNECT host:port` for any TCP
  port and serves **SOCKS5** (RFC 1928: CONNECT only, no BIND and no UDP
  ASSOCIATE, username/password auth carrying the run/decision token). Each
  tunnel requires a pinned decision covering (host, port). `tlscert` and
  `portscan` use it. (As built: username = decision id, password = a
  short-lived ticket from the proxy's control endpoint, because RFC 1929
  caps both at 255 octets; DNS queries carry the ticket in EDNS option
  65280. Spec Part II 35.)
* **DNS:** the proxy serves DNS over UDP and TCP (RFC 1035 wire format,
  our own message parser state machine) for sandboxed tools. A query is
  answered only if a passive scope check allows the name (or the name is
  an allowlisted intelligence source). Answers are recorded as decisions,
  and anything else gets `REFUSED`. This covers the gap where tools need
  raw DNS.
* **Per-target rate limiting in the proxy:** GCRA per
  `rl:h:<program>:<registrable domain or ip>` using the scope document's
  rate, plus a concurrency cap per target. It applies to every request and
  tunnel, so tools with internal parallelism cannot exceed program limits
  (inv_10 enforced at the network edge).
* **Intelligence-source allowlist:** a configured set of hosts (crt.sh,
  RDAP bootstrap and registries, Team Cymru) reachable only by passive
  tools, recorded as decisions with reason `intel_source`.

## 10. Lab and end-to-end acceptance

An in-process lab: HTTP and TLS servers, a fake authoritative DNS server,
and in-scope and out-of-scope hosts. It serves versioned sites whose
bundles change between "deploys" (renamed chunks, new endpoints, a removed
CSP header, a leaked key, volatile nonces and timestamps). The monitoring
workflow must:

1. capture each deploy;
2. report exactly the planted level-3 changes and no noise changes;
3. link renamed chunks with `previous_version`;
4. raise the configured alerts once each;
5. make every planted change findable through `search_changes`, with a
   regex query and a field query;
6. never contact an out-of-scope host (the lab logs prove it);
7. store all versions in less than 25 % of their raw size (measured on the
   raw snapshots, capture record plus redacted body: 6.8 % in the lab;
   18.4 % with every derived record and diff; spec Part II 38).

## 11. Gap list closed by this brief

1. DNS and non-HTTP egress → §9. 2. Per-target rate limits in the proxy →
§9. 3. Bundle renames → §5 lineage. 4. Profile-aware baselines → §5
capture profiles. 5. Spec coverage → Part II + conformance data + numbered
invariants (§1.10). 6. Source maps → `sourcemap` + `unpack_sourcemap`. 7.
Non-HTML formats → `xmlparse`, `wellknown`, JSON structural diff. 8. Crawl
frontier and revisit policy → `crawl` + adaptive revisit (interval halves
on change, doubles on no change, clamped [1 h, 7 d]). 9. Diff determinism
→ versioned normalizers (§5). 10. Huge inputs → bounds and fallbacks (§5).
11. Secrets sensitivity → §3.3, §6.4. 12. Noise measurement → §5 metric.
13. Index lifecycle → §7 (watermark, per-tenant, tombstones, query caps).
14. Viewing diffs → `GET /v1/snapshots/{a}/diff/{b}` (unified and
side-by-side JSON) in the gateway. 15. Tool-output parsers as attack
surface → all parsers are fuzzed. 16. Staleness speed → authoritative
staleness (§3.4). 17. Tag events → `tag_changed`. 18. Quotas → per-tenant
limits on captures per day, stored bytes and index bytes, enforced by the
content service (`QUOTA_EXCEEDED`). As built (Part II 34.6): three limits
per tenant in `content.quota_limits`, zero meaning unlimited, no defaults
(operators set them); checked exactly in the write transaction.

## Migration numbering

Migrations 0015–0029 are reserved for v1 fixes made on `feat_v1` and
merged forward. v2 migrations start at 0030.

## 12. Amendments from the Sentinel review (normative)

`genuinebnt/sentinel` (the same author's Rust change-intelligence
platform) already solves several of this brief's open problems. The
items below are adopted into v2. Where they conflict with earlier
sections, this section wins. Source references are to
`/home/user/sentinel` (`docs/`, `sentinel/crates/sentinel-core/src/`).

1. **Gated authority** (§3.4 above), after Sentinel's absence
   certificates (`docs/adr/2026-11-02-absence-certificates.md`):
   positive-only by default, and withdrawal only with a complete reading
   of the same artifact version.
2. **Cause.** Every `content_changed` and every level-3 change carries
   `cause = target | parser | normalizer`. Upgrading a parser,
   normalizer or extractor triggers a **re-derive job**. The job runs
   before new captures are accepted, re-reads stored snapshots with the
   new version, and records the resulting changes as
   `cause = parser/normalizer`. Default feeds and alerts hide
   non-target causes. (Status: cause is computed and carried on every
   event; the re-derive job is in progress on `feat_v2`, G-CST-04.)
3. **Baseline.** The first complete capture of a (URL, profile) is
   `baseline = true`. It produces graph data but no feed items or
   alerts.
4. **Scores with reasons.** Changes carry `score` (0–1) and
   `score_features` (`[{name, weight}]`, stored, so every rank is
   explainable). This is an additive, clamped, hand-set feature model in
   the style of Sentinel's `domain/analysis/changes.rs`:
   * base weights per change kind: exposed secret, source disclosure,
     new endpoint, removed security header, library version change, …;
   * word-list boosts (admin, auth, payment, internal, debug);
   * state-changing methods, `{id}`-style templates, missing SRI on a new
     third-party script, weakened cookie flags, and CSP loosened;
   * removals count at −½ of the positive weight, returns at −¼.

   The weights as built are pinned in spec Part II 33.11 (for example
   exposed_secret 0.9, source_disclosure 0.85, new_endpoint 0.4,
   removed_security_header 0.35, new_form 0.3, csp_loosened 0.3,
   library_version_change 0.2; word boosts 0.1-0.15).

   Scores **rank, never send**: no code path from a score to a request.
   A test asserts that scoring code imports no fetch or egress package
   and that fetch counts are unchanged by scoring. A learned ranker is
   v3.
5. **Noise and collection integrity:**
   * Soft-404 and template detection: SimHash clustering of bodies across
     a host's URLs, so a catch-all body answered by three or more
     distinct URLs proves nothing.
   * Integrity checks: a login redirect, challenge or CAPTCHA page, an
     error page on a URL that was previously 2xx, or a mass removal (more
     than 50 % of a page's features gone at once) flags the capture as
     **degraded**. A degraded capture gets no diff and no withdrawal, is
     reported as a collection problem, and updates source health.
   * Counted diff outcomes include `masked_only` (only masked volatiles
     changed) and `reordered` (same features, different order), which are
     not changes.
   * Learned volatility never masks a level-3 feature path. A small
     random sample of masked diffs is kept for audit.
6. **Module-level lineage (level 2.5)**, on top of file-level SimHash:
   * Split bundles into bundler modules (webpack, rollup/vite and esbuild
     runtime shapes) and hash each normalized module with local
     identifiers renamed to positional names. An unchanged module is no
     change, wherever it moved.
   * Suppress vendor code by module hash or `node_modules` path, and
     report a library change as a version change.
   * Diff original sources when a source map exists.
7. **Documents inside documents.** Inline scripts, inline styles and
   event-handler attributes become derived snapshots (`{page}#script-n`,
   `#style-n`, `#handlers`), normalized and diffed like external files.
8. **Headers as their own artifact.** Each response's security-relevant
   headers form a canonical block (sorted; CSP nonces removed; Set-Cookie
   reduced to name + attributes, values dropped). It is a versioned
   artifact for every hop, redirects included. A header-only change is a
   `content_changed` with `class = metadata`.
9. **Sniff, don't trust mime types.** The normalizer and extractor are
   chosen by sniffing the bytes, with the mime type as a hint, and
   mismatches are recorded. A server-side source file served as text
   (`<?php`, `<%`, a `.env` body) is itself a source-disclosure finding.
10. **Parser assurance:**
    * Every snapshot stores a reading grade per extractor (complete,
      partial, unsupported, failed). "Read fine, found nothing" is never
      confused with "could not read".
    * One canary fixture per extractor runs at startup and in CI. If a
      canary fails, that extractor is disabled for withdrawal purposes.
    * Explicit amplification bounds, identical in the Go and Rust
      versions: 8 MiB per response, XML/CSS depth 64, 20,000 facts per
      parser per document, 200 inner documents per capture; and, pinned
      when built, HTML open-element depth 512 and JS nesting 4000
      (spec Part II 26.4).
    * "Unreadable" is counted and audited, never fatal.
11. **Redaction before storage and indexing.** Credential-shaped strings
    (by the secret rules, plus connection-string credentials) are
    replaced by a typed placeholder with shape and length before a hunk
    or normalized snapshot is hashed for the index, indexed, embedded or
    logged. The original bytes stay only in the encrypted findings and in
    the raw snapshot. Hidden form fields keep only short numeric or
    boolean values. This complements spec 18.2's no-secrets-in-logs rule.
12. **Storage:**
    * **No cross-tenant dedupe**: a chunk store instance is scoped to one
      tenant, so no tenant can learn another holds identical content.
    * Versions cited by a reviewed or alerted change are **pinned** and
      skipped by retention.
    * An unchanged re-fetch extends `last_seen` on the open version
      instead of adding a row. The comparison uses a signature over the
      body hash plus non-volatile headers.
    * Snapshot validity is stored as `[valid_from, valid_to)` with a
      partial unique index on the open version, so an as-of lookup is one
      index probe.
    * GC may recompress at a higher zstd level and keeps compression only
      when it saves at least 1/8 (codec byte per record).
13. **Change classes and newness.** Besides `level` (hash, text,
    semantic), every change has:
    * `class`: structural, metadata, semantic_endpoint, observation or
      catalog. Catalog is a new advisory against an unchanged library,
      dated by the advisory, not the capture (the class exists; producing
      it needs the advisory matcher, deferred to v3 by D4).
    * `newness`: likely_new (a comparable earlier capture lacked it),
      pre_existing (it was there before we started watching) or
      first_seen_only (no comparable negative).
14. **Search defaults:**
    * vendored bundles and known library modules are excluded unless asked
      for;
    * gzip and base64 blobs found in content are decoded (bounded) before
      indexing;
    * "first appeared" queries return the earliest snapshot containing a
      string, served from the diff history;
    * hybrid mode ships only if it is at least as good as BM25 on recall@k
      and MRR over a labelled set of change queries. The labelled set is
      `conformance/textindex/ranking/labelled.json` (19 queries; BM25 MRR
      0.947, P@1 0.895). Hybrid is not enabled in v2.

**Decided (D3).** DNS resolution (public resolvers or directly to
authoritative servers) and TLS handshakes for certificate collection are
**passive**. They still require a scope check: the class is passive, the
decision is recorded and IPs are pinned. Passive only changes which
windows and program classes allow them. `portscan` and anything that
sends application payloads stay active.

Not in this brief (remain in `docs/ROADMAP_V2.md`): the A1 sandbox runtime
(the egress features above are what it will rely on), supply-chain signing
of external binaries, the Raft and distributed track, and performance track
C.
