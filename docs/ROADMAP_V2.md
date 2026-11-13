# Flowforge v2 roadmap (`feat_v2`)

v1 implements the whole specification except the agent step. v2 closes the
biggest gaps v1 left and adds content change monitoring. Each track is also
chosen for what you learn by building it:

| Track | Closes this v1 gap | You learn |
|---|---|---|
| **A. Real tools and a sandbox** | Only offline demo adapters, run in-process with no isolation (spec 6, 18.2) | Linux isolation (namespaces, seccomp, rlimits, cgroups), process supervision, streaming parsers, safe egress |
| **B. Distributed systems** | Single-owner leases only; no placement, no fairness, no replicated log (spec 23.3, 23.4 B, 23.9 D1–D4) | Leases and fencing at scale, linearizability, Raft, deterministic simulation |
| **C. Performance engineering** | The VM is slower than the tree-walker on list code; "SIMD" is unrolled Go; benchmarks are scaled down (spec 25.8) | Profiling, allocation control, interpreter design, SIMD assembly, ANN on SSD, write-ups |
| **D. Content capture, change diffs and diff search** | The graph records *that* a URL exists, not *what it serves* or how that changes; there is no way to ask "what changed in this program's JavaScript this week?" | HTML parsing, JavaScript lexing, diff algorithms, noise-robust normalization, inverted and trigram indexes, ranking |

The agent step stays deferred (`docs/DECISIONS.md` D1).

## How every milestone is delivered

* **Design note first.** `docs/notes/<milestone>.md` covers the problem,
  the options considered, the choice, and how it is tested, in one or two
  pages. It is written before the code and reviewed with you.
* **Same definition of done as v1:** spec test ids where they exist,
  property and fuzz tests, a benchmark with a committed baseline,
  `verify-module` clean, and arch rules respected.
* **A "what to read" list in each note**, naming the paper or source the
  design follows, so the milestone doubles as a study unit.
* **Small reviewable commits on `feat_v2`.** Each milestone ends with a
  short summary of what was built, what the benchmark showed, and what
  surprised us.

## Environment facts that shape the design

Checked on the development machine:

* Unprivileged user, PID, mount and network namespaces are available, so a
  rootless sandbox works locally and in CI.
* cgroups are v1 and not delegated. Memory and CPU limits therefore use
  rlimits locally; cgroup limits are applied when a delegated cgroup v2
  hierarchy exists (production on GKE). The code must detect this and fail
  closed when a configured limit cannot be enforced.
* The CPU supports AVX2 and AVX-512F, so Go assembly kernels can be built
  and measured. A pure-Go fallback stays as the reference.
* The Docker client is present but there is no daemon. The sandbox does not
  depend on Docker.
* Production note: Cloud Run cannot create namespaces. The production
  target for sandboxed tools is GKE Autopilot with gVisor, as spec 17.1
  already anticipates. The local sandbox gives the same guarantees for
  development and CI.

---

## Track A — Real tools and a sandbox

### A1. Sandbox runtime (`sandbox` package)

Run a binary with no ambient authority: no host filesystem, no network
except the egress proxy, no privileges, bounded resources, killed on
deadline or cancel.

* Namespaces: user (map to an unprivileged uid), PID (the tool is PID 1's
  child; killing the namespace kills everything), mount, IPC, UTS and
  network. They are set up with `os/exec` `SysProcAttr` (`Cloneflags`,
  `UidMappings`) plus a small re-exec'd init stage that does `pivot_root`
  onto a read-only root built from bind mounts (the tool binary, its
  libraries, CA bundle, `/tmp` as size-limited tmpfs).
* `PR_SET_NO_NEW_PRIVS`, all capabilities dropped, and a seccomp-bpf
  allowlist built with `golang.org/x/sys/unix` (deny `ptrace`, `mount`,
  `keyctl`, `bpf`, raw sockets, ...).
* Limits: `RLIMIT_AS`/`RLIMIT_NPROC`/`RLIMIT_NOFILE`/`RLIMIT_FSIZE`
  locally, cgroup v2 when delegated, a wall-clock timeout, and kill of the
  whole namespace within 2 s of cancel (spec 6.3).
* **Egress:** the network namespace has only loopback. A forwarder inside
  the sandbox listens on `127.0.0.1:3128` and relays to the host's egress
  proxy over an inherited Unix socket, so `HTTPS_PROXY` works for ordinary
  tools and nothing else can leave. Every connection still needs a pinned
  scope decision at the proxy.
* **Escape test suite:** the sandbox is attacked by test programs that try
  to read host files, connect directly to the internet or to 127.0.0.1
  services, fork-bomb, allocate past the limit, write past the file-size
  limit, call blocked syscalls, outlive the deadline, and keep running
  after cancel. Every attempt must fail with a recorded reason.
* *Read:* nsjail and bubblewrap sources; the user-namespace and seccomp man
  pages; "gVisor: a container sandbox" (design docs).

### A2. External-tool adapter framework

* A declarative `BinaryTool` spec: binary path and pinned SHA-256 (spec 6.2
  `image@sha256`), argument template, input mode (stdin or file, one
  scope-checked target per line), output parser (JSON Lines → observations),
  progress and error mapping, and the retry and rate fields the proto lacks
  (added to the engine-side spec).
* Targets are re-checked one at a time before they reach the tool. Denied
  targets are streamed as `Skipped` (spec 4.4, tool-runner layer).
* Output is parsed as a stream, bounded per line and per run. Raw output is
  stored as a blob with `raw_ref` (spec 6.1), never shown to the model or
  user directly.

### A3. Passive intelligence adapters (pure Go, through the proxy)

* **Certificate transparency** (crt.sh JSON): `TLSCertificate` entities
  plus `common_name` and `san_dns_name` edges to `FQDN`.
* **RDAP** with IANA bootstrap: `DomainRecord`, `IPNetRecord`,
  `AutnumRecord`, `Organization`, `registration` edges. This also resolves
  the oam fields marked "verify" against real data.
* **ASN and netblock lookup**: `AutonomousSystem`, `Netblock`, `announces`
  and `contains` edges.
* Unit tests replay recorded responses (fixtures in `testdata/`). Live
  tests run only behind an explicit flag.

### A4. Wrapping real binaries

* `httpx` and `subfinder` (ProjectDiscovery, JSONL output) run inside the
  A1 sandbox. A script downloads them and verifies their checksums. Golden
  tests parse recorded output.
* The spec 7.1 `recon` workflow then runs with the real tool names
  (`subfinder`, `httpx`).

### A5. Attack lab and end-to-end test

* A local lab of mock targets: in-process HTTP and TLS servers behind a fake
  DNS server, with in-scope and out-of-scope hosts. It includes redirect
  chains to out-of-scope hosts, DNS rebinding, CNAMEs pointing outside
  scope, decimal and IPv6-mapped IP tricks, and a slow-loris target.
* The real `recon` workflow runs against the lab. Assertions: the expected
  graph results, no packet to any out-of-scope address (checked by the
  lab's own logs), and rate limits respected per target (inv_10).

---

## Track B — Distributed systems

### B1. Worker membership and rendezvous claiming in the engine

* Heartbeats in `engine.workers` (table exists from migration 0009). A
  worker is live for 15 s after its last heartbeat (spec 23.3).
* `ClaimReady` orders candidates by rendezvous rank: the preferred owner
  claims at once, and others wait `rank × delay`. Ranks change only who
  tries first, never who may write (fencing still decides).
* Run affinity keeps a run's VM snapshot warm on its preferred owner.
* `sim` gains membership churn. Measure b_sys_03 (time until orphaned runs
  resume), with an acceptance bound of lease TTL plus one scheduling
  interval.

### B2. Tenant fairness

* `tenant_fair_rank` as weighted round-robin (or the `limiter` WFQ),
  per-tenant concurrency caps, and back-pressure that returns
  `RESOURCE_EXHAUSTED` with `retry_after` (spec 9.4, 23.3).
* Property tested in `sim`: one tenant's backlog of 10,000 runs cannot delay
  another tenant's run beyond a stated bound.

### B3. Linearizability checker

* A Wing & Gong / WGL-style checker written from scratch (the Porcupine
  algorithm), with models for the event store's append, read and lease
  operations.
* Run against `MemStore` and `pgstore` under concurrent clients with
  recorded histories, and fed by `sim` histories. A checker bug is pinned
  by synthetic histories that are known non-linearizable.
* *Read:* Herlihy & Wing; Lowe, "Testing for linearizability"; the
  Porcupine and Knossos write-ups.

### B4. Raft library (`raft` package)

* Leader election with randomized timeouts, log replication, commit index,
  persistence through a `LogStorage` port, snapshots and InstallSnapshot,
  and single-server membership changes. ReadIndex gives linearizable reads.
* Runs on a **simulated network and clock** in the style of `sim`, with
  partitions, message loss, reordering and crash/restart.
* Invariants checked on every step: election safety, leader append-only,
  log matching, leader completeness, state machine safety.
* *Read:* the Raft paper and Ongaro's thesis (chapters 3, 4, 6); the
  etcd/raft design notes.

### B5. Raft-backed EventStore

* `eventstore/raftstore`: `S` shards (`hash(run_id) mod S`), one Raft group
  per shard, with multi-Raft sharing a transport. The state machine applies
  `Append`/`AcquireLease`/`RenewLease`/`SaveSnapshot` and checks
  `expected_seq` and the fence at apply time. The fence is (term, counter)
  (spec 23.4 B).
* Must pass, unchanged: the `storetest` suite (es_001–es_007), the B3
  linearizability checker, and `sim` chaos with leader isolation (the 23.8
  Raft row).

### B6. Whole-cluster deterministic simulation (D4)

* Engine workers, Raft nodes, scope, tools and the bus all run in one
  scheduler with one seed. Any failing seed reproduces exactly. It is
  extended to the services' wiring where feasible.

---

## Track C — Performance engineering

### C0. Measurement tooling (do first)

* `xtask bench-module --count 10` with median and confidence intervals,
  and `bench-compare` reporting statistically significant deltas only.
* `xtask profile <bench>` writes CPU, allocation and mutex profiles and an
  SVG flamegraph into `bench/profiles/`.
* `bench/REPORT.md` skeleton: per benchmark family, the question, setup,
  chart or table, interpretation and flamegraph (spec 25.8).

### C1. Faster VM

* Fix list building (map/filter over 1M elements is 2× slower than the
  tree-walker) and remove per-instruction allocation.
* Load-time translation to an internal pre-decoded form: operands decoded
  once, superinstructions for common sequences (for example `LOAD_LOCAL
  CONST ADD_I`) and jump targets resolved. The FFBC format does not change,
  so modules stay cross-language compatible and the verifier contract is
  untouched.
* Compare dispatch strategies: `switch` versus a function table versus
  threaded code. Measure branch mispredictions with `perf stat` if
  available.
* Acceptance: the VM is at least as fast as the tree-walker on every
  b_vm_01 case, ns/instruction is reported before and after, and vm_008
  (VM equals tree-walker) still passes.
* *Read:* Crafting Interpreters part III; "The Structure and Performance of
  Efficient Interpreters" (Ertl & Gregg); the Lua 5 VM paper.

### C2. SIMD kernels in Go assembly

* AVX2 and AVX-512 L2 and dot-product kernels with runtime CPU detection
  (`golang.org/x/sys/cpu`) and the pure-Go kernel as fallback and
  reference. vec_001 differential tests run on all three.
* Report GB/s against memory bandwidth to show where the kernel becomes
  memory-bound.

### C3. DiskANN on disk, for real

* A SIFT1M download script (with a synthetic fallback if the network
  blocks it).
* Concurrent sector reads per beam round, a hot-node cache, and a
  FreshDiskANN-style streaming merge in place of v1's rebuild-on-merge.
* Produce the recall-versus-QPS chart (spec 22.10: "the main learning
  artefact") for several L and beam widths. Acceptance: recall@10 ≥ 0.95
  on SIFT1M.

### C4. Engine and database throughput

* Profile b_eng_01 (about 370 allocations per step today): batched appends,
  fewer JSON round trips, prepared statements.
* Graph ingest on Postgres: about 470 obs/s today, target 10× via batched
  upserts or COPY, keeping the convergence property.
* b_sys_01 with a flamegraph that names the bottleneck.

### C5. CSR at full scale

* 1M- and 10M-node benchmarks. Make cold open independent of file size
  (verify per-section checksums lazily on first access; keep an opt-in full
  verification).
* Chart PageRank scaling against thread count.

---

## Track D — Content capture, change diffs and diff search

The goal: every time a workflow fetches a page or script, keep what it
served, compute what changed since the last capture, turn meaningful
changes into graph changes and alerts, and make the history of changes
searchable ("new endpoints containing `/admin` since Monday", "where did
the CSP header disappear", "which bundles started calling `/api/v3`").

### D1. Content store and the collector tool

* **Content-addressed blob store:** bodies are stored once by SHA-256
  (compressed), so an unchanged page costs nothing to keep. It sits behind
  the existing `BlobStore` port: local directory and memory now, GCS
  later.
* **Snapshot records** (new graph-schema tables, a migration): `(tenant,
  program, url entity, run, effect path, fetched_at, status, selected
  headers, mime, body_sha256, size, parent snapshot)`, plus retention
  rules per program.
* **The `collect` tool** (active, through the egress proxy, sandboxed once
  A1 lands): `collect(InScope<Url>) -> Snapshot`. It fetches with redirect
  re-checks, caps size and time, and follows referenced scripts, styles,
  source maps and manifests up to a per-run budget, each one scope-checked.
  It emits `URL` and `File` observations and a `ContentProperty` tag
  (hash, size, mime) on the entity. The snapshot is the tool's recorded
  result, so replay is exact without refetching.
* **DSL surface:** `collect`, plus a pure-data `diff(prev, next)` tool
  served by the content service. A monitoring workflow is then plain DSL
  that runs on a Cloud Scheduler trigger:
  `urls |> par_map(u => collect(scope.check(u)?), limit = 8) |> map(s => diff_latest(s))`.

### D2. Parsers and extraction (`content` package)

* **HTML:** parse with `golang.org/x/net/html` (already a dependency; the
  HTML5 tree-building algorithm is enormous and correctness matters more
  than reinventing it). Extract links, script and style references, inline
  scripts, forms (action, method, inputs), meta tags, comments, iframes,
  and security-relevant headers (CSP, HSTS, CORS) from the snapshot.
  *Learning option:* write our own HTML **tokenizer** from the WHATWG state
  machine and differential-test it against `x/net/html`.
* **JavaScript: our own lexer and a light parser.** This is a real learning
  piece:
  * Full ES2023 tokenization: regex-literal versus division ambiguity,
    template literals with nested `${}`, ASI-relevant newlines, Unicode
    escapes in identifiers, numeric separators, and hashbang.
  * A light structural parser on top: string and template literals with
    their call context, `fetch`/`XMLHttpRequest`/`axios` calls,
    `import()`, route tables, GraphQL operation strings, `sourceMappingURL`.
  * Fuzzing for hangs and panics, with a test262 lexer subset as
    conformance data.
* **Extracted features become graph data:** new endpoints and paths become
  `URL` entities; a script becomes a `File` with a `file` edge from its
  page; library banners become `Product`/`ProductRelease` with
  `product_used`. The ingest path stays the existing graph store, so dedupe
  and confidence come for free.
* **Secret and leak detection:** a rule set of named patterns plus entropy
  checks for keys and tokens in bundles. Findings are `VulnProperty` tags
  marked as hypotheses until verified (spec 16.1 fact/hypothesis rule).

### D3. Diff engine (`diff` package)

* **Algorithms from scratch:** Myers O(ND) (with the linear-space
  divide-and-conquer refinement), patience diff and histogram diff, with
  hunks, context and unified-diff rendering. Each is property-tested:
  applying the edit script to the old text gives the new text, edit
  distance is minimal for Myers, and two diffs agree on the edit-distance
  bound.
* **Noise-robust normalization before diffing.** Most raw diffs of web
  content are noise, so:
  * HTML is compared as a normalized token or DOM text stream, not bytes.
  * Minified JS is diffed on its token stream, re-printed one statement
    per line.
  * Volatile values are masked: nonces, CSRF tokens, timestamps,
    cache-busting hashes in file names, and session ids. This uses rules
    plus learned masks, where a token that changes on every capture of an
    unchanged page is marked volatile.
* **Three levels of change, each a different signal:**
  1. hash changed (cheap, always);
  2. text or token hunks (what a human reads);
  3. **semantic changes**: the set difference of extracted features (new or
     removed endpoint, form, parameter, script, header, library version).
     These are what drive alerts and graph changes.
* *Optional stretch:* tree diff for the DOM (Zhang–Shasha, or a
  GumTree-style matcher) to report moved and edited elements rather than
  line hunks.

### D4. Change events, alerts and the pipeline

* New change-event types in `graph.change_events` (migration):
  `content_changed`, `endpoint_added`, `endpoint_removed`,
  `header_changed`, `secret_suspected`, carrying a compact patch and a
  pointer to the stored diff.
* The alert language gains a `diff` variable (`diff.kind`, `diff.added`,
  `diff.removed`, `diff.url`, `diff.feature`), so rules like
  `alert new_admin_route when diff.kind == "endpoint_added" && diff.added
  contains "/admin"` work with the existing throttle, digest and dedupe.
* Monitoring as a scheduled workflow, with per-program schedules and the
  existing idempotency (`schedule:<id>:<fire time>`), rate limits and
  scope checks.

### D5. Search over change history (`textindex` package)

* **An inverted index built from scratch:** tokenization tuned for code and
  URLs (split on case, punctuation and path segments, keeping originals),
  delta- and varint-compressed posting lists, an immutable segment format
  with checksums, and segment merging. It follows the shape of
  `vecindex`'s segments, so the two can share ideas.
* **Query types:**
  * term and phrase queries with BM25 ranking;
  * filters for program, time range, URL prefix, change kind (added or
    removed) and feature type;
  * **regex search with a trigram prefilter**, as in Google Code Search: a
    regex compiles to a trigram AND/OR query, candidates come from the
    trigram index, and exact matching runs on survivors.
* **Hybrid search:** lexical results fused with `vecindex` semantic results
  through the existing RRF (spec 22.6). Each diff hunk is embedded as it is
  indexed.
* Postgres keeps the system of record (`pg_trgm` covers the simple cases).
  The custom index is the fast path and the learning piece, and it can
  always be rebuilt from stored diffs.
* REST: `GET /v1/programs/{pid}/changes/search?q=...&kind=added&since=...`,
  `GET /v1/snapshots/{id}`, `GET /v1/snapshots/{a}/diff/{b}`.
* *Read:* "Introduction to Information Retrieval" (chapters 1–6); Russ Cox,
  "Regular Expression Matching with a Trigram Index"; Lucene's segment and
  merge design.

### D6. Rendering single-page apps (stretch, after A1)

* Pages that build their content in JavaScript need a browser. Run headless
  Chromium inside the A1 sandbox with all traffic forced through the egress
  proxy, capture the rendered DOM and the network requests it made, and
  feed both into the same snapshot and diff pipeline. A headless browser
  is the most dangerous tool in the project, which is why it comes only
  after the sandbox and the attack lab.

## Suggested order

Tracks are interleaved so that each step builds on finished work and every
week ends with something measurable:

1. **C0** measurement tooling (small; every later milestone uses it)
2. **A1** sandbox runtime
3. **B1** rendezvous claiming and heartbeats
4. **C1** faster VM
5. **D1 + D2** content store, collector, HTML extraction and the JS lexer
6. **D3** diff engine and noise-robust normalization
7. **A3** passive adapters (CT, RDAP, ASN)
8. **D4** change events, alert rules on diffs, monitoring workflow
9. **B3** linearizability checker
10. **D5** inverted and trigram index, search over change history
11. **A2 + A4** binary tool framework, httpx and subfinder
12. **C2** SIMD kernels
13. **B2** tenant fairness
14. **A5** attack lab end-to-end
15. **C4** engine and ingest throughput
16. **B4** Raft library
17. **D6** headless rendering in the sandbox (stretch)
18. **C3** DiskANN at SIFT1M scale
19. **B5** Raft event store
20. **C5** CSR at scale
21. **B6** whole-cluster simulation

## Risks and decisions to make along the way

* **Sandbox in production.** Local namespaces do not exist on Cloud Run. If
  the production target stays Cloud Run, sandboxed tools must move to GKE
  with gVisor (spec 20.3 already leaves this open). Decide before A4 ships.
* **Third-party tools.** httpx and subfinder are external binaries with
  their own update cadence. Pin by checksum and record which versions the
  golden tests use.
* **Raft scope.** B4 and B5 are the largest items. If time is short, stop
  after B4 with the library proven on the simulated network; the Postgres
  store remains the default (spec 20.3).
* **New dependencies.** `golang.org/x/sys` (already indirect) becomes
  direct for seccomp, rlimits and CPU detection. Nothing else is planned.
* **Scope safety.** Real tools make the egress proxy's guarantees matter.
  A5 must pass before any real tool is enabled outside the lab. The
  collector follows links and script references, so every follow-up fetch
  is its own scope decision. Nothing is fetched just because a page
  referenced it.
* **Untrusted content.** Captured HTML and JS are data: never executed
  outside the D6 sandbox, never fed raw to a model, size-capped and
  stored compressed. Diff and search output escape everything they show.
* **Storage growth.** Content-addressed storage plus retention per program
  keeps this bounded. The snapshot and diff tables need a retention job
  (spec 10.7 style) from day one.
