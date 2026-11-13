# Flowforge v2 roadmap (`feat_v2`)

v1 implements the specification's modules except the agent step, with the
remaining gaps recorded in `docs/GAPS.md`. This roadmap proposed four
tracks for v2: closing the biggest gaps v1 left and adding content change
monitoring. Each track was also chosen for what you learn by building it.
**Not all of it was built in v2**; the status table below says what was,
and the rest rolls into v3 (`docs/ROADMAP_V3.md` T10):

| Track | Closes this v1 gap | You learn |
|---|---|---|
| **A. Real tools and a sandbox** | Only offline demo adapters, run in-process with no isolation (spec 6, 18.2) | Linux isolation (namespaces, seccomp, rlimits, cgroups), process supervision, streaming parsers, safe egress |
| **B. Distributed systems** | Single-owner leases only; no placement, no fairness, no replicated log (spec 23.3, 23.4 B, 23.9 D1–D4) | Leases and fencing at scale, linearizability, Raft, deterministic simulation |
| **C. Performance engineering** | The VM is slower than the tree-walker on list code; "SIMD" is unrolled Go; benchmarks are scaled down (spec 25.8) | Profiling, allocation control, interpreter design, SIMD assembly, ANN on SSD, write-ups |
| **D. Content capture, change diffs and diff search** | The graph records *that* a URL exists, not *what it serves* or how that changes; there is no way to ask "what changed in this program's JavaScript this week?" | HTML parsing, JavaScript lexing, diff algorithms, noise-robust normalization, inverted and trigram indexes, ranking |

The agent step stays deferred (`docs/DECISIONS.md` D1).

## Status of the v2 build

| Milestone | Status | Where |
|---|---|---|
| A1 sandbox runtime | **carried to v3** | ROADMAP_V3 T10 |
| A2 external-tool adapter framework, A4 httpx and subfinder | **carried to v3** | ROADMAP_V3 T10 |
| A3 passive intelligence adapters | **built**: `ctlog`, `rdap`, `asn`, plus `tlscert`, `portscan` and the extended `dnsresolve`, all through the egress proxy | `services/toolrunner`, spec Part II 36.1 |
| A5 attack lab | **partly built**: the in-process lab (TLS, HTTP, authoritative DNS, in- and out-of-scope hosts) and the content-monitoring acceptance test; the recon workflow with real tools, rebinding and slow-loris targets is v3 | `services/internal/lab`, spec Part II 38 |
| B1-B6 distributed systems | **carried to v3** | ROADMAP_V3 T10 |
| C0-C5 performance engineering | **carried to v3** (the v2 packages ship comparative `*Algo` benchmarks and baselines, not the C0 tooling) | ROADMAP_V3 T10 |
| D1-D5 content capture, diffs, search | **built**, as redesigned by `docs/V2_DESIGN.md` and specified normatively in spec Part II (sections 26-41) | track D below |
| D6 headless rendering | **carried to v3** | ROADMAP_V3 T10 |
| Egress gap fixes (CONNECT any port, SOCKS5, DNS forwarder, per-target GCRA, intel allowlist) | **built** (not a milestone of this roadmap; V2_DESIGN §9) | `services/egressproxy`, spec Part II 35 |

Open v2 items (re-derive jobs; the content service's Cloud Storage backend
and Terraform, in progress on `feat_v2`) are rows of `docs/GAPS.md`
(G-CST-*).

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

**Status:** A3 built, A5 partly built (the content lab), A1, A2 and A4
carried to v3.

### A1. Sandbox runtime (`sandbox` package; carried to v3)

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

### A2. External-tool adapter framework (carried to v3)

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

### A3. Passive intelligence adapters (pure Go, through the proxy; built)

* **Certificate transparency** (crt.sh JSON): `TLSCertificate` entities
  plus `common_name` and `san_dns_name` edges to `FQDN`.
* **RDAP** with IANA bootstrap: `DomainRecord`, `IPNetRecord`,
  `AutnumRecord`, `Organization`, `registration` edges. This also resolves
  the oam fields marked "verify" against real data.
* **ASN and netblock lookup**: `AutonomousSystem`, `Netblock`, `announces`
  and `contains` edges.
* Unit tests replay recorded responses (fixtures in `testdata/`). Live
  tests run only behind an explicit flag.

### A4. Wrapping real binaries (carried to v3)

* `httpx` and `subfinder` (ProjectDiscovery, JSONL output) run inside the
  A1 sandbox. A script downloads them and verifies their checksums. Golden
  tests parse recorded output.
* The spec 7.1 `recon` workflow then runs with the real tool names
  (`subfinder`, `httpx`).

### A5. Attack lab and end-to-end test (partly built)

* A local lab of mock targets: in-process HTTP and TLS servers behind a fake
  DNS server, with in-scope and out-of-scope hosts. It includes redirect
  chains to out-of-scope hosts, DNS rebinding, CNAMEs pointing outside
  scope, decimal and IPv6-mapped IP tricks, and a slow-loris target.
* The real `recon` workflow runs against the lab. Assertions: the expected
  graph results, no packet to any out-of-scope address (checked by the
  lab's own logs), and rate limits respected per target (inv_10).

---

## Track B — Distributed systems

**Status:** carried to v3 (ROADMAP_V3 T10).

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

**Status:** carried to v3 (ROADMAP_V3 T10).

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

**Status: D1-D5 built; D6 carried to v3.** The milestones below were
redesigned by `docs/V2_DESIGN.md` before they were built, and the result is
specified normatively in spec Part II (`docs/flowforge-spec.html`, sections
26-41). This section keeps the goals and the reading lists and says what
was built; where the original plan differed, the built design is the one
described here.

The goal: every time a workflow fetches a page or script, keep what it
served, compute what changed since the last capture, turn meaningful
changes into graph changes and alerts, and make the history of changes
searchable ("new endpoints containing `/admin` since Monday", "where did
the CSP header disappear", "which bundles started calling `/api/v3`").

### D1. Content store and the collector tool (built)

* **Chunk store** (`chunkstore`, Part II 31): FastCDC content-defined
  chunks named by SHA-256, zstd-compressed packfiles (FFPK) with fan-out
  indexes (FFPI), manifests (FFMF), a ref as the single commit point
  (FFRF), delta frames (FFZB) and crash-safe GC; one namespace per tenant,
  no cross-tenant dedupe. Memory, directory and Cloud Storage
  (`chunkstore/gcsstore`, G-CST-02) backends.
* **Snapshot records** in their own `content` schema (migration 0031,
  Part II 34.2), owned by the content service, with validity intervals,
  retention (last 50 or 180 days per URL and profile, plus pins), quotas
  and encrypted findings.
* **The `collect` tool**: `collect(InScope<Url>, String) -> Snapshot`
  (the string is the capture profile). It fetches through the egress
  proxy with every redirect hop re-checked, caps the body at 8 MiB, and
  for a page also captures its scripts, styles and source maps (at most
  32, each scope-checked). The content service publishes the capture's
  graph observation; the tool emits none itself (Part II 36.6).
* **DSL surface**: `collect`, `crawl`, `wellknown`, `extract`,
  `unpack_sourcemap`, `diff_latest` and `search_changes`, and the
  standard-library records `Snapshot`, `Features`, `ChangeSet`,
  `ChangeHit` (Part II 36). The reference monitoring workflow is
  `conformance/workflows/monitor.ff`.

### D2. Parsers and extraction (built, our own parsers)

* The plan to parse HTML with `golang.org/x/net/html` was replaced by the
  brief's rule that every parser is our own explicit state machine:
  `htmlparse` (WHATWG tokenizer and tree construction, x/net/html as a
  test-only oracle), `jsparse` (a full ECMAScript 2024 lexer and parser,
  not a light one), `cssparse`, `xmlparse` (+ sitemaps), `sourcemap`,
  `wellknown`, `codec`, `dnsmsg`, `socks5` (Part II 29).
* **Extraction** (`content`, Part II 33.6): links, scripts, styles,
  frames, forms, endpoints from JS calls and literals, GraphQL operations,
  source maps, libraries, security headers, robots, security.txt,
  sitemaps, OpenAPI and GraphQL schemas, secrets and source disclosure.
  Features become oam observations with coverage-gated authority
  (Part II 28.4).
* **Secrets**: named structured rules, entropy only as a low-confidence
  tier, redaction before storage and indexing, findings encrypted per
  tenant (Part II 33.7, 34.4).

### D3. Diff engine (built)

* `diff`: Myers (linear space and greedy), patience, histogram and an
  O(NM) reference, hunks, git-compatible unified rendering, side by side,
  JSON structural diff (RFC 6902) and Zhang–Shasha tree diff, with the
  edit-distance and input-size bounds and recorded fallbacks of
  Part II 30.
* **Noise-robust normalization**: versioned normalizers with rule-table
  hashes, volatile masking and learned volatility that never masks a
  feature path (Part II 33.3, 33.4).
* **Three levels of change** (hash, text, semantic) plus outcome, class,
  newness, cause, baseline and a ranking-only score (Part II 33.10,
  33.11). Module-level lineage (level 2.5) makes reordered bundles no
  change.

### D4. Change events, alerts and the pipeline (built)

* Graph change events `content_changed` and `tag_changed` (migration
  0030, Part II 28.5). Level-3 kinds such as `endpoint_added` are
  content-change kinds, not graph event types.
* The alert language gains a `diff` variable (`diff.kinds`,
  `diff.changes`, `diff.url`, `diff.class`, `diff.alertable`, ...;
  Part II 36.5); the score is deliberately not addressable.
* Monitoring is the scheduled `monitor.ff` workflow with the existing
  idempotency, rate limits and scope checks; the §10 lab acceptance
  (Part II 38) runs it end to end.

### D5. Search over change history (built)

* `textindex` (Part II 32): an analyzer for code and URLs, FFTX segments
  with skip lists and doc values, tombstones and size-tiered merges, a
  specified query language, BM25 ranking and regex search through a
  trigram prefilter. One index per (tenant, program), rebuilt from the
  stored diffs when lost.
* Hybrid search with `vecindex` (RRF) is implemented in the library but
  not enabled: it ships only when it is at least as good as BM25 on the
  labelled set (Part II 34.7).
* REST: `GET /v1/changes/search?program_id=...&q=...`,
  `GET /v1/changes/first-appeared`, `GET /v1/snapshots/{id}`,
  `GET /v1/snapshots/{a}/diff/{b}` (Part II 37.2).
* *Read:* "Introduction to Information Retrieval" (chapters 1–6); Russ Cox,
  "Regular Expression Matching with a Trigram Index"; Lucene's segment and
  merge design.

### D6. Rendering single-page apps (stretch, after A1; carried to v3)

* Pages that build their content in JavaScript need a browser. Run headless
  Chromium inside the A1 sandbox with all traffic forced through the egress
  proxy, capture the rendered DOM and the network requests it made, and
  feed both into the same snapshot and diff pipeline. A headless browser
  is the most dangerous tool in the project, which is why it comes only
  after the sandbox and the attack lab.

## Suggested order (as planned)

The order originally proposed; the v2 build did track D, A3 and the egress
fixes, and the rest moves to v3 (status table above). Tracks are
interleaved so that each step builds on finished work and every week ends
with something measurable:

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
