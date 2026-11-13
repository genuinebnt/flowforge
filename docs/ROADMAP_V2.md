# Flowforge v2 roadmap (`feat_v2`)

v1 implements the whole specification except the agent step. v2 closes the
three biggest gaps v1 left. Each one is also chosen for what you learn by
building it:

| Track | Closes this v1 gap | You learn |
|---|---|---|
| **A. Real tools and a sandbox** | Only offline demo adapters, run in-process with no isolation (spec 6, 18.2) | Linux isolation (namespaces, seccomp, rlimits, cgroups), process supervision, streaming parsers, safe egress |
| **B. Distributed systems** | Single-owner leases only; no placement, no fairness, no replicated log (spec 23.3, 23.4 B, 23.9 D1–D4) | Leases and fencing at scale, linearizability, Raft, deterministic simulation |
| **C. Performance engineering** | The VM is slower than the tree-walker on list code; "SIMD" is unrolled Go; benchmarks are scaled down (spec 25.8) | Profiling, allocation control, interpreter design, SIMD assembly, ANN on SSD, write-ups |

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

## Suggested order

Tracks are interleaved so that each step builds on finished work and every
week ends with something measurable:

1. **C0** measurement tooling (small; every later milestone uses it)
2. **A1** sandbox runtime
3. **B1** rendezvous claiming and heartbeats
4. **C1** faster VM
5. **A3** passive adapters (CT, RDAP, ASN)
6. **B3** linearizability checker
7. **A2 + A4** binary tool framework, httpx and subfinder
8. **C2** SIMD kernels
9. **B2** tenant fairness
10. **A5** attack lab end-to-end
11. **C4** engine and ingest throughput
12. **B4** Raft library
13. **C3** DiskANN at SIFT1M scale
14. **B5** Raft event store
15. **C5** CSR at scale
16. **B6** whole-cluster simulation

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
  A5 must pass before any real tool is enabled outside the lab.
