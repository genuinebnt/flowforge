# Gap register (v1 to v3)

This register is the gate for v4 (`docs/ROADMAP_V4.md`). v4 starts only
when every row here is **closed** (fixed, with a regression test where
behaviour changed) or **accepted** by the operator with a recorded
reason. It was compiled on 2026-11-13 from every package README and
OPEN_QUESTIONS file, the roadmaps, the design brief, the spec's
definition of done (24.6), its required tests, fuzz targets, invariants
and benchmarks (25.3–25.8), the CI workflows, a code sweep and a review of
the GCP/Terraform setup.

## Branch policy

| Target | Goes on | Rule |
|---|---|---|
| **v1** | `feat_v1`, then merged forward into `feat_v2` (and `feat_v3` when it exists); `master` follows `feat_v1` | A defect in what v1 claims to do: correctness, safety, security, a spec MUST, a broken deployment path, or a doc that is wrong about v1. Fixed once on `feat_v1` with a regression test; the test travels forward with the merge. |
| **v2** | `feat_v2` | Work that v2 reworks anyway, or that sits next to v2's changes (content pipeline, egress, graph expansion, parsers, storage, search). Backporting would mean writing it twice. |
| **v3** | the v3 branch | New features or production readiness that the v3 roadmap owns, including v2-roadmap carry-overs (T10). |
| **accept** | — | Documented, fail-closed simplifications the operator accepts as-is. They must still be accepted explicitly before v4. |
| **spec** | spec Part II | The spec text is wrong or incomplete and the code is right. |

Merges run `feat_v1 → feat_v2` after each batch of v1 fixes. Where v2 has
rewritten the same code, the fix is re-applied in v2's terms in the merge
commit.

Severity: **H** = correctness, safety, security or a spec MUST; **M** = spec
SHOULD or production readiness; **L** = polish.

---

## Deployment profiles: local first, GCP optional and low-cost

Operator requirement (2026-11-13):

* Flowforge must run **fully locally with no cloud account**.
* It must also deploy to GCP for learning, at **low to no cost**.

Every cloud service therefore sits behind a port with a local adapter and
a GCP adapter, selected by configuration.

| Concern | Local adapter (no cloud) | GCP adapter |
|---|---|---|
| Event bus | in-process bus (single process); a **Postgres bus** (outbox tables + LISTEN/NOTIFY, consumer groups, dead letters) for multi-process local runs | Pub/Sub (the emulator for local tests) |
| Blob storage (raw output, snapshots, chunk packs, archives) | memory, local directory (`chunkstore/fsstore`) | Cloud Storage |
| Secrets and signing keys | env vars or a local key file | Secret Manager (mounted as env or files on Cloud Run) |
| Scheduled triggers | in-process scheduler | Cloud Scheduler (3 jobs free per billing account) |
| Database | Postgres 16 (docker compose or `scripts/devpg.sh`) | Postgres on the free-tier VM (lab) or Cloud SQL (prod) |
| Rate-limit store | in-process | Redis on the lab VM, or Memorystore (prod) |
| Egress proxy (HTTP, CONNECT, SOCKS5, DNS) | a local process | lab: the free-tier VM; prod: a small MIG or GKE. Cloud Run cannot serve raw TCP or UDP, so SOCKS5 and DNS cannot run there. |
| Tool sandbox (v3 A1) | Linux namespaces locally | GKE Autopilot with gVisor (prod only) |

### Three GCP environments

* **`lab` (new; the learning environment, near-zero cost):**
  * Cloud Run services with min instances 0 (they scale to zero), on
    `*.run.app` URLs with Google-managed TLS. No load balancer, Cloud
    Armor or Cloud NAT, which have no free tier.
  * One **e2-micro** VM in a free-tier region (us-west1, us-central1 or
    us-east1) running Postgres, optional Redis and the egress proxy in
    containers.
  * Pub/Sub, Cloud Storage, Secret Manager, Cloud Scheduler and Artifact
    Registry, all within their free tiers at lab volumes.
  * Direct VPC egress instead of a Serverless VPC Access connector, since
    a connector bills while it runs.
  * The VM's external IPv4 address is billed (a few dollars a month) and
    is the main fixed cost.
* **`dev` and `prod` (existing):** the full spec 17 design (Cloud SQL HA,
  Memorystore, load balancer, Cloud Armor, NAT), for learning the
  production topology. Cloud SQL, Memorystore, NAT and the load balancer
  have no free tier, so they are opt-in. A `make gcp-down` destroys them
  when you are done.
* **Cost guardrails, all environments:**
  * a budget with alerts (lab default: a small monthly amount);
  * an optional budget-triggered shutdown function that scales services
    to zero and stops the VM;
  * `make gcp-up` / `gcp-pause` / `gcp-down` (apply, stop the VM, destroy);
  * labels on every resource;
  * max instances on every Cloud Run service.

Check prices with the GCP pricing calculator before applying.
Free-tier terms change, and the Terraform README must state the
assumptions it was written against.

---

## Register

### High severity (all must close before v4)

| ID | Gap | Target |
|---|---|---|
| G-CORE-01 | A scope version's `effective_from` is never enforced, so a future-dated scope widening applies immediately (scope OPEN_QUESTIONS 14; services/scope PutScope) | **v1** |
| G-LIM-01 | Gateway API rate limits fail **open** when the limiter errors, against spec 2.4 "unreachable rate limiter means deny" (gateway OPEN_QUESTIONS 12) | **v1** (fail closed) |
| G-SVC-01 | No Pub/Sub adapter: standalone binaries run with no bus, so outbox rows are never published and consumers idle (services/app/app.go) | **v1** (Postgres bus locally + Pub/Sub adapter) |
| G-SVC-03 | Signing keys only in the in-memory dev key store, which rotates on restart, so instances cannot verify each other's tokens; no durable key custody | **v1** (env/file + Secret Manager key store) |
| G-OPS-01 | Per-role DB grants and removal from `cloudsqlsuperuser` are documented as done by the migrations job, but nothing implements them | **v1** |
| G-OPS-12 | Signing-key env mismatch: Terraform sets `FF_SIGNING_KEY_<KID>`; the services read `FF_SIGNING_KEY` + `FF_SIGNING_KID` | **v1** |
| G-OPS-13 | Terraform sets `FF_REDIS_CA` (Memorystore TLS), but no Go client configures Redis TLS, so production Redis connections fail | **v1** |
| G-SVC-05 | Tools run in-process without a sandbox (spec M7) | **v3** (A1, before any external binary) |

### Medium severity

| ID | Gap | Target |
|---|---|---|
| G-CORE-02 | OAM fields tagged "verify" are unresolved (spec 20.3) | v2 (RDAP/CT adapters provide real data) |
| G-CORE-03 | An observation naming an existing `ent_` id before that entity is ingested fails, which breaks the 23.5 any-order convergence | **v1** (defer and resolve pending references) |
| G-CORE-04 | GCS BlobStore is a port only | v2 (with chunkstore; GCS adapter + fs adapter) |
| G-CORE-05 | Postgres ingest runs at about 470 obs/s | v3 (C4) |
| G-LANG-01 | `#[timeout]`/`#[retry]` do not reach effects inside called fns | v3 |
| G-LANG-02 | The VM is slower than the tree-walker on list building (b_vm_01 acceptance unmet) | v3 (C1) |
| G-ENG-01 | No worker heartbeats or rendezvous claiming; `engine.workers` is unused | v3 (B1) |
| G-ENG-02 | No tenant fairness rank, no per-tool-class semaphore, no submit back-pressure | v3 (B2) |
| G-ENG-03 | No linearizability checker or Raft store (Raft is optional in the spec) | v3 (B3–B5) |
| G-ENG-04 | Snapshots of finished runs are never deleted | **v1** |
| G-ENG-05 | Graceful shutdown behaves like a crash, so every deploy records outcome-unknown effects for non-idempotent tools | **v1** (drain on SIGTERM) |
| G-ENG-06 | The simulator does not exercise lease renewal mid-advance, the watchdog interrupt or CancelGrace | v3 |
| G-ENG-07 | The simulator uses one tenant, does not fault the Repo, and has no membership churn | v3 (with B1/B2) |
| G-LIM-02 | `limiter.Failover` (23.8: local budgets for passive work when Redis is down) is wired nowhere | **v1** |
| G-LIM-03 | Argon2id is at the 13.1 floor (~22 ms), against a 200–500 ms target | **v1** (production parameters by config, measured at startup) |
| G-LIM-04 | The breached-password check is a small built-in list | v3 |
| G-LIM-05 | Approvals with free-form roles (`finance_ap`, `lead`) can never be decided: no group store | **v1** |
| G-LIM-06 | Alert throttle and digest state is in memory, so alerts duplicate after a restart (inv_08 in production) | **v1** |
| G-LIM-07 | The default abuse rule matches `auth.signup_throttled`, which nothing emits | **v1** |
| G-VEC-01 | No SIMD kernels | v3 (C2) |
| G-VEC-02 | No pgvector adapter; similarity is in-memory only | v3 |
| G-VEC-03 | No recovery from disk for vector tombstones and the seq map | v3 |
| G-VEC-04 | Merges rebuild synchronously under the write lock | v3 (C3) |
| G-VEC-05 | No semantic embedder | v4 (M3) |
| G-VEC-06 | CSR cold open is linear in file size (b_csr_03 unmet) | v3 (C5) |
| G-SVC-02 | The identity outbox (auth.events) has no relay | **v1** (with G-SVC-01) |
| G-SVC-04 | The notifier cannot resolve `sm://` secret references | **v1** (with G-SVC-03) |
| G-SVC-06 | `dnsresolve` uses the runner's resolver, not one behind the proxy | v2 (§9, in progress) |
| G-SVC-07 | HTTPS redirects inside TLS tunnels are re-checked only by the runner's own client | v2 (§9) |
| G-SVC-08 | Notifier webhooks bypass the egress proxy (infra README says otherwise) | **v1** (route through the proxy's intel/allowlist path, or correct the doc; decide in the fix) |
| G-SVC-09 | Only three real adapters; the proto ToolSpec has no retry fields | v2 (§8 tools) |
| G-OPS-02 | RLS is ENABLEd but not FORCEd; child tables are uncovered; no BYPASSRLS scheduler role | **v1** |
| G-OPS-03 | Retention jobs (10.7), the monthly partition job and run_events archival are not scheduled | **v1** (in-process maintenance locally; Cloud Scheduler on GCP) |
| G-OPS-04 | The SLO alert metrics `ff_scope_check_seconds` and `ff_run_submit_to_start_seconds`, and the approval log metric, are never emitted | **v1** |
| G-OPS-05 | CI never sets `FF_SIM_SEEDS` (20 per profile instead of 200 per PR and 10,000 nightly) | **v1** |
| G-OPS-07 | No post-deploy stage (end-to-end, b_sys_*, chaos with the invariant checker) | v3 |
| G-OPS-08 | M10 not reached: placeholder image digests, billing and domains; no HTMX console | v3 (console); lab env in **v1** |
| G-OPS-09 | tool-runner runs as a Cloud Run service, not Jobs or GKE with gVisor | v3 (decide with A1) |
| G-OPS-10 | Workflow schedules created at runtime cannot create Cloud Scheduler jobs | **v1** (in-process scheduler + one Cloud Scheduler tick job) |
| G-OPS-14 | `FF_ARTIFACT_BUCKET` is set but unused: raw tool output and archives never reach object storage | **v1** (fs/GCS blob adapter for raw_ref and archives) |
| G-OPS-15 | The v2 egress proxy (SOCKS5, DNS over UDP) cannot run on Cloud Run | v2 (lab VM / prod MIG in Terraform) |
| G-OPS-16 | The v2 content service, content schema user and chunk-pack bucket lifecycle are not in Terraform | v2 |
| G-OPS-19 | No low-cost `lab` GCP environment; current envs assume paid services (LB, Armor, NAT, Cloud SQL HA, Memorystore) | **v1** |
| G-OPS-20 | No cost guardrails beyond budget alerts: no pause or destroy commands, no budget-triggered shutdown | **v1** |
| G-OPS-21 | No local multi-process topology: everything local is one `flowforge dev` process; no compose profile running each service with the Postgres bus | **v1** |
| G-SPEC-01 | Benchmarks b_sys_02 and b_sys_03 do not exist | v3 |
| G-SPEC-03 | CSR benchmarks stop at 100k nodes; cold open used 20 MB, not 1 GiB | v3 (C5) |
| G-SPEC-04 | Vector benchmarks use 20k synthetic vectors; no SIFT1M or GIST1M, no recall-vs-QPS chart | v3 (C3) |
| G-SPEC-06 | No `bench/budgets.toml`, `bench/REPORT.md`, `gen-bench-data` or open-loop load generator | v3 (C0) |
| G-SPEC-07 | graphstore has no language-neutral conformance data or required ids in modules.toml | v2 |
| G-SPEC-09 | The v2 packages are not in modules.toml | v2 (in progress) |
| G-DOC-02, 04, 05, 06, 07, 10, 11 | V2_DESIGN and ROADMAP_V2 inconsistencies: track D stale vs the brief; §2 table missing dnsmsg/socks5/crawl/lab; §6.2 DDL predates §12; Part II records undefined; quotas, score weights and labelled set unquantified; KMS port; PORTING_RUST v2 formats | v2 |
| G-DOC-15 | This register did not exist | **v1** (this file) |

### Low severity

| ID | Gap | Target |
|---|---|---|
| G-CORE-06 | Merge bookkeeping in hidden `ff:` tag rows instead of a column | v3 |
| G-CORE-07 | Time-based ageing not applied | v3 (T3) |
| G-LANG-03 | The builtin doc says `parse_int` accepts `_` separators; the VM rejects them | **v1** (fix doc) |
| G-LANG-04 | `Format` drops comments | v3 |
| G-LANG-05, G-LANG-06, G-VEC-08, G-DOC-14 | Spec text out of date or wrong: `is InScope(_)`, bytecode format additions, the 21.3 SCC listing, OPEN_QUESTIONS spec fixes | spec Part II |
| G-LANG-08 | The Plan record layout is provisional | v4 (M1) |
| G-ENG-09 | The inv_10 bound includes dispatch delay; kill-switch races are not reported | v2 (§9 proxy limiter) |
| G-LIM-08 | Default rules are not installed per tenant; TestRule cannot replay a historical event | v3 |
| G-LIM-09 | No Google ID token or mTLS service authentication | v3 |
| G-VEC-07 | `Key.Model` should carry the model version | v3 |
| G-SVC-10 | If-Match is read-then-write, not atomic | v3 |
| G-OPS-06 | Mutation testing configured but never run | v3 |
| G-SPEC-02 | b_lim_03 (Redis) has no baseline and was measured at 1 connection | **v1** |
| G-SPEC-05 | Other benchmarks scaled down (oam, eventstore, engine, sys_01) | v3 (C4) |
| G-SPEC-08 | Conformance suite names differ from spec 19; no vector for lim_008 atomicity | **v1** (vector) + spec |
| G-SPEC-10 | agt_001–004 absent | v4 (M1) |
| G-DOC-01, 03, 09 | ROADMAP_V2 intro overclaims; V2_DESIGN §6.2 heading; HTML/JS nesting bounds unpinned | v2 |
| G-DOC-08 | The `catalog` change class needs advisories, which D4 defers | v3 |
| G-DOC-12, 13 | Stale statements: archtest entries described as missing, puddle "not in go.mod", "every package has OPEN_QUESTIONS", D1 wording, auth README listing signup and reset as missing | **v1** |

### Accept as-is (operator to confirm before v4)

| ID | Item | Reason |
|---|---|---|
| G-CORE-08 | asn rules never match; edge key per 5.7; covering runs recounted after retention; WatchChanges polls; weak typo search; Mem keeps everything | documented, fail-closed, or dev-only |
| G-LANG-07 | The tree-walker does not model fuel | the differential test compares values and traces, and fuel has its own tests |
| G-LANG-09 | i64 minimum literal; lambda-before-approval rejected; 200k-step exhaustiveness bound; JSON loses `Some(None)` and Map order; Stream flag lost | fail closed, documented |
| G-ENG-08 | Whole-cluster simulation (D4), Pregel analytics, hedged requests | spec marks them as stretch goals; B6 covers simulation |
| G-ENG-10 | Block-level timeout; ready predicate; escalation roles from config; outbox after append | documented |
| G-LIM-10 | Sliding-window approximation; Redis Remaining = 0; Redis Cluster slots; LeasedBudget mutex; KeyCache serialization; rehash errors swallowed | documented |
| G-VEC-09 | Sequential beam reads, no age flush, no CSR `since`, overlay ignores staleness, u64 offsets | documented |
| G-SVC-11 | api/contract runs against fakes, not a local stack | the gatewayd end-to-end test covers the real stack |
| G-SVC-12 | PATCH /programs changes only status; `paused_runs` null; analytics snapshots in memory; MemRepo copy; no inter-service TLS in dev | documented |
| G-OPS-11 | No CMEK, VPC-SC or multi-region | cost and spec non-goals |
| G-OPS-17 | Terraform has never been planned or applied against a real project (this session has no GCP credentials) | the first `lab` apply is an operator step |
| G-SPEC-11 | No `ff-conform` cross-language harness | built with the Rust port |

### Pluggable algorithms and comments (operator rules, 2026-11-13)

| ID | Gap | Target |
|---|---|---|
| G-ALG-01 | scope: make the naive matcher a selectable `Matcher` in non-test code; add `BenchmarkScopeAlgo` | v1 |
| G-ALG-02 | csr: make the adjacency-list representation a selectable baseline for BFS and PageRank; add `BenchmarkCSRAlgo` | v1 |
| G-ALG-03 | limiter and placement: rename and consolidate their comparative benchmarks into `BenchmarkLimAlgo` and `BenchmarkPlcAlgo` | v1 |
| G-ALG-04 | vm/treewalk and eventstore: comparative benchmarks under the Algo naming | v1 |
| G-ALG-05 | vecindex: add HNSW as a candidate next to Vamana (recall against QPS) | v3 (C3) |
| G-ALG-06 | chunkstore: fixed-size, Rabin CDC and whole-blob candidates with dedupe, speed and shift-resistance comparisons | v2 |
| G-ALG-07 | v2 packages (diff, textindex, htmlparse, jsparse, sketch): pluggable candidates, benchmarks and ALGORITHMS.md entries | v2 (in progress) |
| G-CMT-01 | Comment pass on every v1 package: correct, useful, spec-referencing comments, `PORT:` notes, no restating, no stale text | v1 (then merged forward) |
| G-CMT-02 | Comment pass on every v2 package | v2 |

## Doc and code conflicts to resolve with the fixes

1. infra and cloud-sql READMEs vs code on DB grants (G-OPS-01).
2. infra vs services on webhook egress (G-SVC-08).
3. sim flag help vs CI seeds (G-OPS-05).
4. gateway OPEN_QUESTIONS "groups the engine owns" vs no groups (G-LIM-05).
5. alerts default rule vs identity events (G-LIM-07).
6. engine/graphstore/pgstore/migrations notes about archtest entries and puddle (G-DOC-12).
7. ROADMAP_V2 intro and track D vs README and V2_DESIGN (G-DOC-01/02).
8. ROADMAP_V3 T7 title vs D4 (G-DOC-08).
9. DECISIONS D1 "later" vs v4 (G-DOC-13).
10. builtin doc vs VM for `parse_int` (G-LANG-03).
11. spec 21.3 vs 21.5 SCC order (G-VEC-08).
12. spec M6 vs fake backends (G-SVC-11).
13. limiter README vs placement/engine on rendezvous ownership (G-ENG-01).

## Counts

* **v1 (fix on `feat_v1`, merge forward):** 30 rows, including 7 of the 8 high-severity ones.
* **v2:** 17 rows.
* **v3:** 30 rows.
* **v4:** 3 rows.
* **spec:** 4 rows.
* **accept:** 12 rows.
