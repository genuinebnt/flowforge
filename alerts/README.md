# alerts

Alert rule language, evaluation and notifier semantics for Flowforge
(spec section 15; crate `ff-alerts`, layer 2). Pure Go: no clock, network,
DNS or environment access. Imports only the standard library, `core` and
`core/netnorm`.

## Spec sections

| Area | Spec | Files |
|------|------|-------|
| Rule statement `alert NAME when EXPR -> action(...) {clause}` | 15.2 | `parser.go`, `rule.go`, `ast.go` |
| Lexical rules (comments, literals, durations, escapes) | 7.2 | `lexer.go` |
| Expression grammar and precedence | 7.3 | `parser.go` |
| Type checking against the event schema | 15.2, 11.6, 11.7, 5.4 | `check.go`, `schema.go`, `types.go`, `event.go` |
| Diagnostics (7.7 JSON shape, `A` codes) | 7.7, 15.3 | `diag.go` |
| Evaluation, matches, payloads | 15.2, 15.4 | `eval.go`, `ruleset.go` |
| Dedupe key, throttle | 15.3 | `dedupe.go` |
| Digest | 15.3 | `digest.go` |
| Quiet hours | 15.3 | `quiet.go` |
| Consumer idempotency (inv_08), orchestration | 15.1, 10.6 | `processor.go` |
| Retry schedule | 15.1 | `retry.go` |
| Webhook signature | 15.4 | `webhook.go` |
| SSRF destination checks | 15.4 | `ssrf.go` |
| Default operational rules | 15.4 | `defaults.go` |

## Public API

* `Compile(src)` / `CompileWith(src, Options)` return `(*Rule, []Diagnostic)`;
  the rule is nil when any diagnostic is an error.
* `NewRuleSet(RenderOptions)`, `(*RuleSet).Add/Remove/Evaluate`,
  `EvaluateRule` (for TestRule).
* `NewProcessor(rules, ProcessorConfig{Clock, Store})`,
  `(*Processor).Process(ctx, *Event)`, `Flush()`, `NextFlush()`,
  `PruneThrottle()`; port `ProcessedStore` with fake `MemProcessedStore`.
* `DedupeKey`, `Throttle`, `Digester`, `QuietHours`.
* `Sign`, `Verify`, `WebhookHeaders`; `ValidateWebhookURL`, `CheckResolved`,
  `CheckAddr`, `IsForbiddenAddr`; `RetryBackoff`, `RetryDelay`, `NextAttempt`.
* `DefaultRules()` and the `DefaultRule*` source constants.

## Language summary

Literals: strings (`\n \t \" \\ \u{HEX}`, `{{`/`}}` for braces; interpolation
is rejected), integers (`1_000`, `0x10`), floats, durations (`250ms 15m 6h
1d`), `true`/`false`, lists `[a, b]`, ranges `lo..hi` (after `in` only).
Operators by precedence (low to high): `|>`, `||`, `&&`, `== !=`,
`< <= > >= in contains starts_with ends_with matches`, `..`, `+ -`,
`* / %`, unary `- !`. Functions: `len contains starts_with ends_with
matches in`. Time arithmetic: `Time - Time = Duration`,
`Time ± Duration = Time`.

Variables: `event` (id, type, schema_version, tenant, program,
occurred_at), `change` (id, seq, type, entity_id, edge_id, run_id, patch,
at), `entity` (id, asset_type, key, scope_status, confidence, state,
created_at, last_seen, first_seen_run, last_seen_run, plus the OAM content
fields of spec 5.4 flattened, see `contentFieldKinds`), `edge`, `run`,
`approval`. Absent sections and fields follow three-valued logic: a
condition that evaluates to absent does not match.

Diagnostic codes: `A0001-A0099` lexing, `A0100-A0199` parsing,
`A0200-A0299` names, `A0300-A0399` types, `A0400-A0499` values and limits;
the constants are in `diag.go`.

## Tests and benchmarks

```
go test -race ./alerts/...
go test ./alerts/ -run '^$' -fuzz FuzzAlertRule -fuzztime 20s
go test ./alerts/ -run '^$' -bench BenchmarkAlr01 -benchtime=3x
```

* `TestAlr001_Operators` runs `conformance/alerts/eval/{operators,diagnostics,spec_examples}.json`
  (every operator, invalid-rule diagnostics, the three spec 15.2 rules and
  the REST example rule of 12.7).
* `TestAlr002_DedupeThrottle` runs `conformance/alerts/eval/throttle.json` and
  a FakeClock scenario (one alert per window, the next after it).
* Digest, quiet hours, idempotent processing (inv_08), payload shape,
  diagnostics format, default rules, webhook signature, SSRF and retry
  schedule have their own tests; `TestAlrProp_DedupeFastPathCanonical`
  pins the dedupe encoding to `core.CanonicalJSON`.
* `BenchmarkAlr01_Evaluate` (b_alr_01) evaluates 1,000 rules per event and
  reports events/s.

## Known limitations

See `OPEN_QUESTIONS.md`. In short: throttle and digest state lives in the
processor; `Outcome.State` reports each change for the caller to persist
and `Processor.Restore` rebuilds it (the notifier does both); fields inside
`patch`, `payload` and `attributes` are not addressable beyond key tests;
opaque OAM types (Person, Location, ...) expose only the fixed columns.
