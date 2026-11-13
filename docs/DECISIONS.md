# Implementation decisions

Decisions taken while building the Go reference implementation that change
the scope or order of the spec's milestones (section 20.1).

## D1. Agent step (spec 16, milestone M9) deferred

The in-engine agent step is not implemented in this phase.

* The language keeps `agent NAME(...)`, `Plan<T>` and `execute(plan)`, and
  the VM keeps effect kinds 5 (`agent_call`) and 6 (`execute_plan`), so
  nothing needs redesigning when the step is added.
* The engine rejects, at registration time, any workflow whose inferred
  effects include `agent_call` or `execute_plan`, with a clear
  "agent effects not enabled" diagnostic. A run can therefore never fail
  half-way because of it.
* The `Llm` and `Embedder` ports (spec 24.4) are defined as interfaces only.
  A Genkit-backed adapter and the planner/repair loop/eval harness come later.
* An optional MCP front (read-only graph and run tools plus
  `check_plan`/`submit_plan` routed through the normal compile, policy and
  approval gate) may be added after the gateway exists. External agents then
  drive Flowforge without any LLM inside it, preserving the invariant
  "agents propose, the engine disposes" (spec 2.4).

## D2. No TypeScript/JSX parser in v2

Targets serve compiled JavaScript, so a JS parser covers every v2
capability (extraction, semantic diffs, endpoints, secrets). TypeScript and
JSX appear mainly as original sources inside source maps. In v2,
`unpack_sourcemap` stores and diffs `.ts`/`.tsx`/`.jsx` originals as
normalized text (line-level diff with noise masking) and never feeds them
to the JS parser; extraction keeps running on the compiled bundle, which
is authoritative. The `jsparse` AST is designed so TS and JSX node kinds
can be added. TS and JSX parsing is v3 track T1 (`docs/ROADMAP_V3.md`).

## D3. DNS and TLS handshakes are passive

DNS resolution (public resolvers or directly to authoritative servers) and
TLS handshakes for certificate collection are passive activity. They still
need a scope check (class passive, decision recorded, IPs pinned); the
class only changes which time windows and program max classes allow them.
`portscan` and anything that sends application payloads stay active.
(Sentinel treats both as active; Flowforge consciously differs.)

## D4. Vulnerability advisory sources deferred

Retire.js, OSV, CISA KEV and EPSS feeds are not integrated in v2 or the
first v3 cut. Library identification still ships; advisory matching waits
for a decision on sources, licences and allowed hosts.

## D5. TypeScript and JSX parsers are built in Go

v3 track T1 adds TypeScript and JSX to `jsparse`. The Rust port translates
the Go parsers instead of adopting oxc or Sentinel's parsers, because the
project's purpose is learning; oxc, swc and typescript-estree are design
references and test-only differential oracles.
