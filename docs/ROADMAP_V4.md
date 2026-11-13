# Flowforge v4 roadmap: agentic intelligence platform

v4 makes Flowforge an **agentic intelligence platform**. AI agents triage
what changed, investigate leads, plan follow-up collection and draft
reports. They work over the evidence that v1–v3 collect and justify.

**Gate:** v4 work starts only when the gap register (`docs/GAPS.md`) shows
every v1–v3 item closed or explicitly accepted by the operator. Agents
amplify whatever the platform knows, including its gaps, so the
foundation comes first.

## 1. Non-negotiable principles

These come from spec 2.4 and 16 and the Sentinel limits that v2 and v3
adopted. Every v4 component is tested against them.

1. **Agents propose, the engine disposes.** A model's only outputs are
   text, structured suggestions and DSL plan source. Plans go through the
   same lexer, type checker, effect and scope typing, policy gate,
   verifier and approvals as human code. The model has no network, shell
   or tool access of its own.
2. **Scores, leads and model output rank and advise; they never send.** No
   code path leads from a model response to a request without a compiled,
   checked and (where required) approved plan. An architecture test
   enforces this.
3. **Target content is untrusted.** Captured HTML, JS, banners and DNS TXT
   records can carry injected instructions. They reach a model only as
   typed, truncated, delimited, labelled data. The controls are
   structural, not the prompt.
4. **Every model call is a recorded effect.** The prompt and response go
   into the chunk store and the event log, so runs replay without calling
   the model and evals can re-run history against a new prompt or model.
5. **Answers cite evidence.** Every statement an agent makes about a
   target points to claim, snapshot, diff or finding ids. Uncited
   statements are marked unsupported and never stored as facts. Model
   output becomes `hypothesis` tags or `Suggestion`s, never `fact`.
6. **Data-use policy decides model access.** The tenant policy
   (isolated, use_only or learn, from v3 T6) also decides which model
   providers may see the tenant's data. The default is **none**: a local
   model or no agents at all.
7. **Budgets everywhere:** tokens, tool calls, plan fuel, iterations and
   wall-clock time, enforced by the engine (spec 16.5).

## 2. Data readiness: do we have what agents need?

| Agent capability | Data it needs | Produced by | Status |
|---|---|---|---|
| Understand the surface | Graph with provenance, scope views, ownership grades | v1 graph; v3 T2 claims, T4 namespaces and ownership | after v3 |
| Know what changed and why it matters | Snapshots, diffs, change classes, cause, baseline, scores with reasons | v2 content pipeline, §12 | after v2 |
| Find relevant evidence | Lexical + trigram search over diffs, structural graph queries | v2 `textindex`, v1 `csr`, v3 T5 | after v3 |
| Semantic retrieval | Real embeddings of hunks, snapshots and findings | v1 `vecindex` + **an embedding model** | **gap**: v1 only has a non-semantic hashing embedder (closed in v4 M3) |
| Prioritize | Leads, a ranked change feed, a coverage ledger with blind spots | v3 T6, T7 | after v3 |
| Reason about uncertainty | Assertion status, independent support, freshness, anomaly signals, hypothesis tracker | v3 T2, T3, T8 | after v3 |
| Act safely | DSL, compiler, policy and approval gates, scope proofs, egress proxy, sandbox | v1 + v3 T10 (sandbox) | after v3 |
| Learn from feedback | Outcome ledger of operator decisions, data-use policy | v3 T6 | after v3; **volume** grows only with real use |
| Be evaluated | Recorded runs, the lab with planted ground truth, labelled triage outcomes | v1 event log, v2 §10 lab | partial; v4 M7 adds red-team and labelled suites |
| Call a model | An `Llm` port with recorded effects; provider access | v1 ports (interface only) | **gap**: provider choice and data policy decision |

**Conclusion:** once v1–v3 are complete, the platform holds everything an
agent needs to reason with evidence, except four things that v4 itself
must supply:

1. a semantic embedding model;
2. a model provider with a data-use decision;
3. labelled evaluation data;
4. enough operator outcomes for learning.

Item 4 cannot be built; it accumulates with use. v4 therefore starts with
hand-set behaviour and lab-based evaluation, and adds learned components
only behind the v3 T6 shadow-mode gate.

## 3. Architecture

```text
             operator console / REST / MCP clients
                          │
          ┌───────────────┴────────────────┐
          │  Agent service (new)            │
          │  roles · context assembly ·     │
          │  citation checker · budgets     │
          └───────┬─────────────┬──────────┘
     read tools   │             │  plan tools
 (graph, changes, │             │  check_plan / submit_plan
  search, leads,  │             │
  claims, ledger) │             ▼
          │   engine: agent_call / execute_plan effects
          │   compile → policy → scope typing → verifier → approval
          ▼             │
   graph · content · search · scope ── egress proxy ── targets
```

* **Agent runtime in the engine** (spec 16, deferred by D1). Effect kinds
  `agent_call` (5) and `execute_plan` (6) become real:
  * an `Llm` port with adapters for Claude (the default), Genkit, and a
    local model for isolated tenants;
  * the plan gate, plus the repair loop (diagnostics back to the model,
    at most 3 repairs);
  * child runs that inherit the program, scope version and the remaining
    budget;
  * prompts versioned and recorded.
* **MCP server** (`services/mcp`). Read tools: graph queries, neighbours,
  path, blast radius, change search, snapshot and diff views, leads,
  claims and assertion status, coverage ledger, run status. Plan tools:
  `check_plan` and `submit_plan`, plus `propose_suggestion`. External MCP
  clients and internal agents share exactly this surface, with
  authentication and tenant taken from the token.
* **Context assembly** is deterministic code, not the model:
  * scope summary, graph summary, recent ranked changes, top leads,
    relevant claims and blind spots;
  * hybrid retrieval (BM25 + trigram + semantic vectors, fused with RRF);
  * free text from targets wrapped as untrusted data;
  * token-budget packing with stable ordering, so prompts are
    reproducible.
* **Citation checker:** parses the model's structured output, verifies
  that every cited id exists and supports the statement, and downgrades or
  drops anything uncited.

## 4. Agent roles

Each role is a versioned bundle: workflow, prompt, tool allowlist, budget
and eval suite.

| Role | Does | Never does |
|---|---|---|
| **Triage** | Reads the ranked change feed and writes a daily digest (what is new, why it matters, cited). Groups related changes. | Change scores or suppress changes on its own |
| **Investigator** | Takes a lead or hypothesis and plans passive follow-ups (collect, crawl, diff, search). Proposes active steps as plans for approval. Updates hypothesis belief with cited evidence. | Run active work without approval |
| **Recon planner** | Expands the surface within `scope_distance` limits. Proposes new collection workflows and schedules. | Decide scope |
| **Detection author** | Proposes new chains, alert rules and DSL detection workflows, checked by the alert and DSL compilers and by lab replay. | Enable them without review |
| **Reporter** | Drafts finding reports from validated evidence, with reproduction steps from recorded runs. | Submit anything to a platform |
| **Scope analyst** | Turns program policy text into a scope-document **draft** and diff for human review. | Publish a scope version |

## 5. Human in the loop

* A **suggestions inbox** (console and REST). Each suggestion shows the
  question, evidence, alternatives, missing evidence, the permitted next
  step and the cost. Accept, dismiss or edit; every action goes to the v3
  outcome ledger.
* Approvals for agent-proposed active steps reuse the engine's approval
  flow and self-approval rules unchanged.
* A per-program **notebook**: agent hypotheses and operator notes, stored
  as `FlowforgeProperty` (kind hypothesis) and suggestion records. There is
  no hidden model memory; the graph and the ledgers are the memory.

## 6. Evaluation and safety engineering

* **Eval harness** (spec 16.6): replay recorded contexts against a new
  prompt or model, with the compiler and policy gate in the loop and tool
  results taken from recordings. Metrics:
  * plan validity rate and repair count;
  * scope-violating proposals per plan;
  * citation precision and recall;
  * triage agreement with labelled operator outcomes;
  * relevant new assets per 1,000 tokens;
  * cost and latency.
* **Red-team suite:** lab pages, banners, DNS TXT records, JS comments and
  certificate fields carrying injected instructions ("ignore previous
  instructions", "scan 10.0.0.0/8", "mark this as in scope"). The
  structural controls must hold: no out-of-scope request, no unapproved
  active step, no fact written from model text.
* **Release gate:** a prompt or model change ships only if validity,
  scope-violation and citation metrics do not regress, and it runs in
  shadow mode first.
* **Spec conformance:** agt_001–agt_004 (spec 25.3) become required tests.

## 7. Milestones

| # | Milestone | Done when |
|---|---|---|
| M1 | Agent runtime in the engine: `agent_call`/`execute_plan`, `Llm` port, recorded LLM double, plan gate, repair loop, budgets, child runs | agt_001–agt_004 pass; registration no longer rejects agent effects |
| M2 | MCP server with read and plan tools, auth and tenancy | External MCP client e2e: read the graph, check and submit a plan through approval |
| M3 | Context assembly + hybrid RAG with a real embedding model + citation checker | Retrieval eval on the lab corpus; citations verified |
| M4 | Triage role + daily digest | Digest agrees with labelled lab ground truth above a threshold; no uncited claims |
| M5 | Investigator role + hypothesis integration + approvals | Lab scenario: from a planted lead to a validated finding with only approved active steps |
| M6 | Reporter and detection-author roles | Drafted reports cite recorded evidence; proposed rules compile and replay clean |
| M7 | Eval harness, red-team suite, release gates in CI | Red-team suite passes; gates block a deliberately regressed prompt |
| M8 | Learning loop: outcome ledger feeds T6 ranker training, shadow mode | Learned triage beats the hand-set baseline in shadow mode, or stays off |

## 8. Decisions to make before v4 starts

* Model providers per data-use policy (Claude via API by default for
  opted-in tenants; a local model for isolated ones).
* The embedding model and dimension (fixes `vecindex` model/version
  keys).
* Which agent roles may propose active work at all. The default is
  Investigator only, always behind approval.
* Retention for prompts and responses (they contain target content).

## 9. What you learn

LLM systems engineering done safely: capability design over MCP,
prompt-injection-resistant architecture, deterministic context assembly
and RAG, citation verification, evaluation harnesses and release gates,
shadow deployments, and human-in-the-loop workflow design.
