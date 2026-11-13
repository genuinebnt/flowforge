# Flowforge v3 roadmap

v3 builds on v2's content pipeline, which captures, diffs and searches
what targets serve, and moves the graph from "what we saw" to "what we
can justify". It has three goals:

* parse everything web targets ship: **TypeScript and JSX**, including the
  original sources recovered from source maps and framework build
  manifests;
* adopt the **evidence model** that `genuinebnt/sentinel` (the same
  author's Rust platform) proved out: claims with stance and lineage,
  typed coverage, time-aware reads, entity resolution;
* turn the graph into **ranked, explainable intelligence**: richer
  analytics, leads, advisory matching, a detection ledger and anomaly
  signals. Everything ranks; nothing sends.

Sentinel references below are to `/home/user/sentinel`. "Built" there
means recorded in `sentinel/docs/HANDOVER.md` or present in code. v3
adapts its designs to Flowforge's spec, conformance and portability
rules (`docs/V2_DESIGN.md` §1); it does not copy its code.

Delivery rules are the same as v2: a design note first, numbered
invariants that the Rust port reimplements, conformance data, fuzzing,
benchmarks with acceptance shapes, and no trivial tests.

## Tracks

| # | Track | Size | Depends on |
|---|---|---|---|
| T1 | TypeScript and JSX parsing, framework artifacts | L | v2 `jsparse`, `sourcemap`, `content` |
| T2 | Claim model, typed coverage, withdrawal licence | L | v2 graph expansion |
| T3 | Time: effective-time reads, freshness classes, epochs | L | T2 |
| T4 | Entity resolution, namespaces, scope as a view, ownership | M–L | T2 |
| T5 | Graph analytics II | M | v1 `csr` |
| T6 | Leads and learned ranking under a data-use policy | M | v2 scores, T5 |
| T7 | Detection framework and advisory matching | M–L | T1 (library detection), T2 |
| T8 | Anomaly toolkit | M | v2 content, T6 policy |
| T9 | Agent step and MCP front (spec 16, deferred in v1 and v2) | L | T6, T7 |
| T10 | Carry-overs from the v2 roadmap | — | — |

---

## T1. TypeScript and JSX parsing

**Why it matters for the tool:** targets serve compiled JavaScript, so
TypeScript and JSX are not needed to find things (`docs/DECISIONS.md`
D2). They matter for **diff quality** and for **framework artifacts**:

* original sources in source maps are usually `.ts`, `.tsx` or `.jsx`;
* route definitions, API clients and typed request shapes are clearest
  there.

**Scope:**

* **JSX** in `jsparse`: a JSX lexical goal chosen by the parser (JSX text,
  attribute strings, identifiers with `-`, HTML entities in JSX text, using
  the `htmlparse` entity table data shared through
  `conformance/htmlparse/entities.json`), and ESTree JSX nodes
  (JSXElement, JSXFragment, JSXAttribute, JSXSpreadAttribute,
  JSXExpressionContainer, JSXNamespacedName, JSXMemberExpression).
* **TypeScript** syntax, following typescript-estree node shapes:
  * type annotations everywhere, interfaces, type aliases, enums,
    `const` enums, namespaces and `declare module`;
  * `abstract`, accessibility modifiers, parameter properties, overloads,
    definite assignment, `declare` fields;
  * generics with **speculative parsing** (lexer checkpoint and rewind, as
    oxc does) for `a < b > (c)`, generic arrows in `.tsx` (`<T,>`), and
    type assertions `<T>x` (not allowed in `.tsx`);
  * `as`, `satisfies`, non-null `!`;
  * `import type` / `export type`, `export =`, `import x = require()`,
    triple-slash directives;
  * the full type grammar: conditional, mapped, template literal,
    indexed and `infer` types, type predicates, `asserts`, `unique
    symbol`, `accessor`, `const` type parameters;
  * TC39 decorators and TS legacy decorators;
  * `.d.ts` declaration files.
* **Type erasure:** the TS AST maps to a plain JS AST with types removed,
  used by normalization so TS-only edits (type changes) are classified
  separately from runtime changes.
* **Framework artifacts in `content`:**
  * Next.js `_buildManifest.js` and app-router manifests;
  * Nuxt, SvelteKit, Remix and Angular router configurations;
  * React Router route objects;
  * webpack and vite chunk maps;
  * Vue single-file components and Svelte components, split into blocks
    with `htmlparse`, with each block sent to the right parser.

  Each becomes `Endpoint` and `URL` data with `source = framework_manifest`.

**Conformance and oracles.** This machine has Node 22, `tsc`, bun and cargo,
and npm and crates.io are reachable:

* Babel parser fixtures (jsx and typescript plugins) and the TypeScript
  repository's `tests/cases` are vendored as a filtered subset, with
  coverage tracked as a committed snapshot of failing cases (oxc's
  `tasks/coverage` approach), so regressions and progress are both
  visible;
* `typescript-estree` and oxc (`oxc_parser` via a small cargo tool) are
  differential oracles in test-only scripts. They are never runtime
  dependencies.

**Invariants (examples):**

* print→reparse identity for TS and JSX;
* `erase(parse_ts(x))` printed parses as JS and equals the oracle's
  stripped output;
* totality and linear time, including adversarial `a<b<c<…` inputs, so
  speculative parsing never goes exponential (benchmark with a bound);
* JSX text tiling.

**Learning:** speculative parsing and cover grammars at their hardest, the
real-world TSX ambiguities, coverage-driven development the way oxc and
swc do it.

## T2. Claim model, typed coverage, withdrawal licence

**Why:** v1's noisy-or confidence double-counts sources that share an
origin. Subfinder-via-crt.sh plus Amass-via-crt.sh is one piece of
evidence, not two. v2 gates withdrawal on complete captures (V2_DESIGN
§3.4, §12.1); v3 generalizes that to every source.

**Scope** (after Sentinel's blueprint §5–6, `asset_graph/inventory/model.rs`
and `docs/adr/2026-11-02-absence-certificates.md`):

* Every fact is a **claim** with:
  * `stance`: support, contradict, withdraw, or retract ("was never
    true");
  * `basis`: declared, observed, derived, analyst or model;
  * method and source;
  * **lineage roots**, the upstream origins.
* `independent_support` counts distinct lineage roots and replaces
  noisy-or for ranking. Confidence is never summed across one root.
* **Assertion status** is computed from claims: supported, stale,
  conflict, contradicted, withdrawn or unknown.
* **Typed coverage:** each run reports, per rule or method family, an
  outcome (complete, partial, failed or unsupported) over a declared
  tested set. A **withdrawal licence** requires a complete outcome whose
  tested set covers the subject and whose method family and vantage
  point match. Failed and partial runs age nothing and update a
  **source health** record instead.
* **Migration:** v1/v2 tags map onto claims (SourceProperty → support
  claims, covering-run staleness → withdrawals under a licence).
  Convergence (spec 23.5) is re-proved over claims.

**Learning:** evidence modelling, provenance, and why "absence of
evidence" needs a licence.

## T3. Time: effective-time reads, freshness classes, epochs

* **Bitemporal reads:** keep the v2 `AsOf(seq)` axis (what we knew) and
  add `effective_at` (what was true then). Every read takes a
  `ReadContext{known_at, effective_at, freshness_policy_version}`.
* **Freshness computed at read time** per relation volatility class
  (Sentinel's defaults: dns_address 7 d, web_link 30 d, technology 28 d,
  ownership 180 d, routing 2 d), replacing write-time "stale" marking for
  those classes. Policy versions are recorded, so old answers can be
  reproduced.
* **Epochs:** domain re-registration (an RDAP creation date that changes)
  starts a new epoch, so old ownership claims do not carry over. IP
  reallocation horizons (netblock reassignment) do the same for
  addresses.
* Edge validity intervals (`valid_from`/`valid_to`) everywhere, as v2
  already does for snapshots.

**Learning:** bitemporal data modelling, and making time-dependent
answers reproducible.

## T4. Entity resolution, namespaces, scope as a view, ownership

* `equivalent_to` and `distinct_from` claims with evidence. Union-find is
  only a **projection** of the accepted claims, reversible when evidence
  is withdrawn, with stable cluster ids and a closure check
  (`distinct_from` wins over transitively implied equivalence).
* **Namespaces:** a global (public internet facts) versus `org:<id>`
  graph. Programs become **views** over the graph rather than owners of
  nodes (Sentinel ADR 2026-11-02 organisation identity), so one host shared
  by two programs is one node with two scope views.
* **`scope_distance` overlay:** hops from an in-scope seed, bounding
  passive expansion. Discovery stops expanding past a distance; it never
  decides scope.
* **Ownership attribution grades** (declared, registrar match, certificate
  organization, DNS delegation, …) where shared infrastructure (CDNs,
  hosting, SaaS) contributes zero towards ownership.

## T5. Graph analytics II

Added to `csr`, each deterministic, budgeted and checked against a naive
reference (Sentinel `asset_graph/algo/`):

* Yen k-shortest paths and bidirectional BFS;
* Louvain community detection (Sentinel rejected label propagation as
  unstable);
* sampled Brandes betweenness;
* FastRP node embeddings with similar-node search through `vecindex`;
* per-kind isolation-forest outliers, with reasons expressed in MAD units;
* dominator trees, as "single points of failure" for reachability;
* `compare(snapshot_a, snapshot_b)` structural deltas;
* a commonness cap generalized from v2 (high-degree shared nodes are not
  pivots).

Analyses stay read-only effects.

## T6. Leads and learned ranking under a data-use policy

* **"Where to start" leads:** additive, explained reasons over graph,
  content and score features (Sentinel `asset_graph/explore.rs`). Every
  lead is labelled "a lead, never a finding".
* **Learned ranker:**
  * a logistic or pairwise model trained on operator outcomes (reviewed,
    dismissed, reported);
  * it ships only if it beats the v2 hand-set scorer, through shadow mode
    with promote and rollback;
  * evaluation uses time and group splits plus PSI drift monitoring;
  * conformal prediction sets give it an abstention option.
* **Data-use policy** per tenant: `isolated` (the default), `use_only`
  or `learn`. `Lineaged<T>` values cannot exist without recorded lineage,
  so every model input is traceable. An **outcome ledger** records
  operator decisions.
* Scores and leads **rank, never send**, enforced by an architecture test
  (no import path from ranking packages to fetch or egress) and a
  fetch-count invariant.

**Learning:** applied ML engineering done carefully (baselines, shadow
deployment, calibration, drift), not model building for its own sake.

## T7. Detection framework and advisory matching

* **Tiers:**
  * tier 0: passive analysis;
  * tier 1: armed safe probes behind the existing approval gate;
  * tier 2: person-only, requiring a capability token minted by an
    approver.

  This maps onto spec activity classes and approvals.
* **Coverage ledger:** per (target, check), one of nine verdict states.
  `clean` requires an authoritative reading and a **canary** that fired
  (proof that the check could have seen the problem). A blind-spot list
  records what could not be checked.
* **Chains** are named graph queries that raise priority only. Example:
  new endpoint + missing auth header + an admin word list hit.
* **Library and advisory matching:**
  * Library identification from `includes_library` and module hashes
    (v2 level 2.5).
  * Advisory sources: Retire.js data, OSV, CISA KEV and EPSS.
  * Applicability is **three-valued** (affected, not affected,
    unknown). A failed version comparison is never "not affected".
  * npm, semver and range semantics follow the OSV schema's event model.
  * A **catalog** change class: a new advisory against an unchanged
    library is a change dated by the advisory.
  * Feeds are licence-gated, activated atomically, off by default.
  * The OSV API host is blocked by this environment's network policy, so
    feeds load from allowlisted mirrors or offline bundles.

## T8. Anomaly toolkit

From Sentinel `domain/anomaly/`, behind the T6 data-use policy:

* robust statistics (MAD, EWMA);
* **CUSUM/PELT change-point detection** that separates a source change
  (the collector or parser changed) from a target change;
* per-endpoint profiles (Welford) and a first-order Markov flow model
  (surprisal, skipped step) over observed request flows;
* drift rules: ASN change, certificate issuer change, hub change, TLS or
  protocol downgrade, authentication removed;
* a hypothesis tracker with log-odds belief, correlation groups and an
  information-value ordering of next checks.

Every output is a ranked suggestion with evidence.

## T9. Agent step and MCP front

v1 and v2 deferred the agent step (D1). T6 and T7 give agents something
worth reasoning over. Scope:

* **MCP server** (hybrid design from v1 planning): read-only graph,
  change and search tools, plus `check_plan` and `submit_plan` through the
  normal compile, policy and approval gate.
* **Planner:**
  * Claude or Genkit adapters behind the `Llm` and `Embedder` ports;
  * a repair loop that feeds compiler diagnostics back to the model;
  * budgets;
  * untrusted-content wrapping (spec 16.3);
  * an eval harness replaying recorded runs (16.6).
* **Model output is advisory:** a `Suggestion` with status `proposed`
  that records the question, evidence, alternatives, missing evidence and
  the permitted next step (Sentinel METHODOLOGY-INTELLIGENCE MI01–MI20).
  No tenant data goes to a third-party model unless the tenant's
  data-use policy allows it.

## T10. Carry-overs from the v2 roadmap

The v2 build implements track D (content capture, diffs, search, the
graph expansion) and the egress gap fixes (TCP, SOCKS5, DNS forwarding,
proxy rate limits, intel tools). These v2-roadmap items are not yet built
and roll into v3 unless pulled forward:

* A1 sandbox runtime, A2/A4 external binaries (httpx, subfinder), and the
  full A5 attack lab;
* B1–B6: rendezvous claiming, tenant fairness, the linearizability
  checker, Raft and its event store, whole-cluster simulation;
* C0–C5 performance work;
* D6 headless rendering in the sandbox.

## Suggested order

1. **T1 JSX**: small, immediately improves source-map diffs.
2. **T2** claim model and coverage: the foundation for T3, T4, T6 and T7.
3. **T5** analytics II, in parallel with T2 since it is independent.
4. **T1 TypeScript**: large; runs alongside T2.
5. **T7** advisory matching and detection ledger.
6. **T3** time model.
7. **T6** leads and ranking, with the data-use policy.
8. **T4** entity resolution and namespaces.
9. **T8** anomaly toolkit.
10. **T9** agent step and MCP.

Carry-overs (T10) are interleaved as capacity allows. A1 (sandbox) should
come before any tier-1 detection probes ship.

## Decisions needed

* **DNS and TLS activity class** (V2_DESIGN §12, open decision).
  Sentinel treats them as active; Flowforge treats public-resolver DNS as
  passive.
* **Advisory data sources and licences** for T7 (Retire.js, OSV, KEV,
  EPSS). Which ones, and through which allowed hosts?
* **Third-party model use** for T9. Default is none unless a tenant opts
  in.
* **Rust port timing.** Sentinel is already Rust and parses JS and TS;
  the Flowforge Rust port could share parser crates (or adopt oxc)
  instead of porting `jsparse`. Decide before T1 TypeScript starts, since
  it changes whether we build a TS parser in Go at all.
