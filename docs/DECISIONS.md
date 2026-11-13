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
  A Genkit-backed adapter and the planner/repair loop/eval harness are
  planned for v4 (roadmap item M1 in the v4 roadmap, which lives on the v2
  branch; `docs/GAPS.md` G-LANG-08, G-SPEC-10 and G-VEC-05 track the pieces).
* An optional MCP front (read-only graph and run tools plus
  `check_plan`/`submit_plan` routed through the normal compile, policy and
  approval gate) may be added after the gateway exists. External agents then
  drive Flowforge without any LLM inside it, preserving the invariant
  "agents propose, the engine disposes" (spec 2.4).
