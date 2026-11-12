# proto

The gRPC contracts of Flowforge (spec section 11, plus 21.6 and 22.9),
transcribed from `docs/spec.txt`, and the generated Go code in `gen/`.

## Layout

| File | Package | Go import path | Spec |
|------|---------|----------------|------|
| `flowforge/v1/common.proto` | `flowforge.v1` | `github.com/genuinebnt/flowforge/gen/flowforge/v1` (`flowforgev1`) | 11.2 (lines 1615-1647) |
| `flowforge/v1/scope.proto` | `flowforge.v1` | same | 11.3 (1649-1731) |
| `flowforge/v1/engine.proto` | `flowforge.v1` | same | 11.4 (1733-1810) |
| `flowforge/v1/toolrunner.proto` | `flowforge.v1` | same | 11.5 (1812-1864) |
| `flowforge/v1/graph.proto` | `flowforge.v1` | same | 11.6 (1866-1934) |
| `flowforge/v1/notifier.proto` | `flowforge.v1` | same | 11.7 (1936-1981) |
| `flowforge/v1/events.proto` | `flowforge.v1` | same | 11.7 (1983-2002) |
| `flowforge/graph/v1/analytics.proto` | `flowforge.graph.v1` | `github.com/genuinebnt/flowforge/gen/flowforge/graph/v1` (`graphv1`) | 21.6 (3040-3087) |
| `flowforge/similarity/v1/similarity.proto` | `flowforge.similarity.v1` | `github.com/genuinebnt/flowforge/gen/flowforge/similarity/v1` (`similarityv1`) | 22.9 (3253-3306) |

Generated code lives under `gen/` with `paths=source_relative`, so
`proto/flowforge/v1/scope.proto` becomes `gen/flowforge/v1/scope.pb.go` and
`gen/flowforge/v1/scope_grpc.pb.go`. Never edit generated files.
`gen/flowforge/v1/contract_test.go` is hand-written and checks, via
protoreflect over every registered `flowforge.*` file, that every enum's zero
value ends in `_UNSPECIFIED` (and every value carries the enum prefix) and
that each service exposes exactly the RPCs the spec lists, with the right
streaming shape (`EngineService.WatchRun`, `ToolRunnerService.Execute` and
`GraphService.WatchChanges` are server-streaming; everything else is unary).

The `.proto` text keeps the spec's compact layout (one-line messages) so it
can be diffed against the spec; `buf format` would reflow it, so it is not
applied.

## Regenerating

```sh
scripts/genproto.sh            # installs pinned tools if missing, buf lint, buf generate
SKIP_LINT=1 scripts/genproto.sh
go test ./gen/...
```

Tooling is local only: `buf` compiles and lints, and the local plugins
`protoc-gen-go` and `protoc-gen-go-grpc` generate code (`buf.gen.yaml`). No
remote buf.build plugins and no `protoc` binary are needed. The script
installs into `$(go env GOPATH)/bin` with `go install`, which never touches
`go.mod`. Versions used:

| Tool | Version | Note |
|------|---------|------|
| buf | v1.75.0 | |
| protoc-gen-go | v1.36.12 | must equal `google.golang.org/protobuf` in `go.mod` |
| protoc-gen-go-grpc | v1.6.2 | generated code needs grpc >= 1.64 (go.mod has 1.85.0) |

The well-known types (`google/protobuf/*.proto`) come from buf's built-in
copies. Generated headers show `protoc (unknown)` because buf, not protoc,
is the compiler.

`buf.yaml` lints with `STANDARD` (the buf v2 name of the `DEFAULT` category;
`DEFAULT` still works but is deprecated) and checks breaking changes with
`FILE` (`buf breaking --against '.git#branch=main'`). Lint exceptions, all
because the spec's names win over buf's style rules:

* `RPC_RESPONSE_STANDARD_NAME`, `RPC_REQUEST_RESPONSE_UNIQUE` (disabled):
  the contracts are resource-oriented (Google AIP-131/133 style). `Get`,
  `Create`, `Put` and similar RPCs return the resource itself (`Program`,
  `Run`, `Approval`, `ScopeVersion`, `Tag`, ...), streams carry the event
  type (`RunEvent`, `ChangeEvent`), and the similarity search RPCs share one
  `SearchResponse`.
* `RPC_REQUEST_STANDARD_NAME` ignored for `graph.proto` only:
  `IngestObservations` takes `IngestRequest`.
* `SERVICE_SUFFIX` ignored for `analytics.proto` and `similarity.proto`: the
  spec names the services `GraphAnalytics` and `Similarity`.

`buf lint` is otherwise clean.

## Deviations from the spec text

Each is the minimal change needed for valid, lint-clean proto3 or one the
spec's own conventions (11.1) require.

1. **`common.proto`: unused imports dropped.** The spec imports
   `google/protobuf/timestamp.proto`, `duration.proto` and `struct.proto` but
   uses none of them; protoc warns and buf's `IMPORT_USED` rule fails.
2. **`engine.proto`: added `import "google/protobuf/duration.proto"`.**
   `SubmitRunRequest.deadline` is a `google.protobuf.Duration` but the spec
   text does not import it, which is a compile error.
3. **11.7 split into two files.** `notifier.proto` holds `NotifierService`
   and its messages; `events.proto` holds `EventEnvelope`,
   `RunEventPayload` and `ApprovalEventPayload`. Each imports only what it
   uses: `notifier.proto` imports `common.proto` (Diagnostic, PageRequest),
   `graph.proto` (ChangeEvent), struct and timestamp; `events.proto` imports
   `graph.proto`, struct and timestamp (not `common.proto`, which it does not
   use). The topic table is kept as a comment in `events.proto`.
4. **Graph analytics (21.6) in its own file and package.** The spec calls it
   "graph.proto additions (package flowforge.graph.v1)". A different package
   cannot share a file with `flowforge.v1`, so it is
   `flowforge/graph/v1/analytics.proto`. Its `Direction` enum would otherwise
   collide with `flowforge.v1.Direction` from 11.6; being in a separate
   package resolves that. It does not reuse `flowforge.v1.Direction`, to keep
   the analytics contract independent of the v1 graph service.
5. **`flowforge.graph.v1.Direction` values prefixed.** `OUT`, `IN`, `BOTH`
   became `DIRECTION_OUT`, `DIRECTION_IN`, `DIRECTION_BOTH` (spec 11.1 naming;
   proto3 enum values are scoped to the package, so unprefixed names collide
   easily and violate `ENUM_VALUE_PREFIX`). Numbers are unchanged.
6. **Analytics result messages filled in.** The spec describes them only in
   comments; they are defined as:
   `LevelResult { repeated Level levels }`, `Level { uint32 depth; repeated string entity_ids }`;
   `PathResult { repeated string entity_ids; repeated string edge_ids; double confidence }`;
   `ScoreResult { repeated Score top_n; uint32 iterations; bool converged }`, `Score { string entity_id; double score }`;
   `GroupResult { repeated Group groups }`, `Group { uint64 id; repeated string entity_ids }`.
   Paging for large `GroupResult`s uses the spec's
   `RunAnalysisResponse.next_page_token`.
7. **`analytics.proto`: added `import "google/protobuf/timestamp.proto"`**
   for `SnapshotInfo.built_at`.
8. **`flowforge.similarity.v1.Metric` values prefixed.** `L2`, `COSINE`,
   `INNER_PRODUCT` became `METRIC_L2`, `METRIC_COSINE`,
   `METRIC_INNER_PRODUCT` (same reasons as 5). Numbers are unchanged.
9. **`similarity.proto`: added `import "google/protobuf/timestamp.proto"`**
   for `Filter.last_seen_after`.
10. **`option go_package`** added to every file (not in the spec).

Checked and left as written: `oneof from` in `DiffRequest` and the `from`
field of `OamEdge` are valid (`from` is not a proto keyword; Go gets
`DiffRequest.From` / `GetFrom()`), and `ChangeEvent` resolves through the
`graph.proto` import in both `notifier.proto` and `events.proto`.

## Error model (spec 11.8)

Packages below the service layer never produce gRPC codes; services map
sentinel errors to a `google.golang.org/grpc/status` with a
`google.rpc.ErrorInfo` detail (`google.golang.org/genproto/googleapis/rpc/errdetails`).
`ErrorInfo.reason` is one of the strings below (the spec does not fix
`domain`; use the service name), and `metadata` carries free-form strings
(`request_id`, `rule_id`, ...).

| gRPC code | `ErrorInfo.reason` | REST | Client behaviour |
|-----------|--------------------|------|------------------|
| `INVALID_ARGUMENT` | `VALIDATION_FAILED`, `INVALID_TARGET`, `DSL_DIAGNOSTICS` | 400 / 422 | Fix the request; diagnostics are in details. |
| `UNAUTHENTICATED` | `TOKEN_MISSING`, `TOKEN_EXPIRED`, `TOKEN_INVALID` | 401 | Refresh or log in. |
| `PERMISSION_DENIED` | `FORBIDDEN_ROLE`, `SCOPE_DENIED`, `SELF_APPROVAL_FORBIDDEN`, `KILL_SWITCH_ACTIVE` | 403 | Do not retry. |
| `NOT_FOUND` | `NOT_FOUND` | 404 | Also for another tenant's resources (never reveal existence). |
| `ALREADY_EXISTS` | `ALREADY_EXISTS`, `IDEMPOTENCY_KEY_REUSED` | 409 | Same key with a different payload. |
| `ABORTED` | `VERSION_CONFLICT`, `FENCE_LOST`, `APPROVAL_ALREADY_DECIDED` | 409 | Re-read and retry (version conflict) or stop (fence lost). |
| `FAILED_PRECONDITION` | `RUN_NOT_CANCELLABLE`, `WORKFLOW_DEPRECATED`, `PROGRAM_PAUSED`, `BUDGET_EXCEEDED`, `OUTSIDE_WINDOW` | 409 / 412 | State must change first. |
| `RESOURCE_EXHAUSTED` | `RATE_LIMITED`, `QUOTA_EXCEEDED` | 429 | A `google.rpc.RetryInfo` detail with `retry_delay` is always attached; wait that long. |
| `UNAVAILABLE` / `DEADLINE_EXCEEDED` | `UNAVAILABLE`, `DEADLINE_EXCEEDED` | 503 / 504 | Retry with backoff; idempotency keys make this safe. |
| `INTERNAL` | `INTERNAL` | 500 | Bug; details carry only `request_id`. |

Notes:

* `DSL_DIAGNOSTICS` errors carry the `flowforge.v1.Diagnostic` list; RPCs
  such as `CheckWorkflow` and `TestRule` also return diagnostics in the
  response body when the request itself was well-formed.
* `RESOURCE_EXHAUSTED` must carry both `ErrorInfo` and `RetryInfo`; clients
  honour `RetryInfo.retry_delay` rather than their own backoff.
* `ScopeService.Check` fails closed: any error (including
  `DEADLINE_EXCEEDED` after its 500 ms deadline) means deny.
* Tenant and actor travel in metadata (`x-tenant-id`, `x-actor`), never in
  messages (11.1), so they cannot be spoofed in a body. The `tenant_id`
  fields in `flowforge.graph.v1` and `flowforge.similarity.v1` requests are
  part of the spec text and are cross-checked against the token.
