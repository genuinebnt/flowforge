# scope

Layer-1 package implementing the Flowforge scope model (spec section 4):
scope documents, target normalization (via `core/netnorm`), the decision
algorithm, passive scope status for graph ingest, the DSL's
`scope.allows` pre-filter, and redirect/CNAME chain re-checks.

| Spec | What |
|------|------|
| 2.4  | Scope and "no silent success" invariants: every failure is a deny |
| 4.1  | `Document`, `Rule`, `Activity`, `Window`; `Document.Validate` |
| 4.2  | `Normalize` (delegates to `core/netnorm`) |
| 4.3  | `Policy.Check`, `Decision`, reason constants |
| 4.4, 5.7, 7.6 | `Policy.Status`, `Policy.Allows` (passive, no resolution) |
| 4.5  | `Policy.CheckChain` for redirect and CNAME hops |
| 25.3-25.9 | Required tests, properties, fuzz target, benchmark, conformance |

## Usage

```go
p, err := scope.Compile(doc, scope.WithResolver(resolver))
d := p.Check(ctx, scope.Request{
	Target: "https://api.acme.com/v2/users", Kind: scope.TargetURL,
	Class: core.ClassActive, Now: recordedNow,
})
if d.Allowed() { /* connect only to d.PinnedIPs */ }
```

A passive check resolves and pins only when `Request.ResolvePassive` is
set (DECISIONS D3: tlscert and DNS work are passive but connect only to
pinned addresses); active checks resolve unless `SkipResolution`.

`Policy` is immutable and safe for concurrent use. The package reads no
clock (`Request.Now` is passed in); name resolution goes through the
`Resolver` port, with `StaticResolver` as the in-memory fake.

## Matching

Decision cost grows with the target's depth, not the rule count:

* `domain` / `wildcard_domain`: reversed-label trie (`domaintrie.go`);
  `*.x` matches any depth below `x`, never `x`.
* `cidr`: binary prefix trie per address family, slice-backed
  (`cidrtrie.go`); supports contains, containment and overlap queries.
* `url_prefix`: per (scheme, host, port) map of rule paths, probed once
  per path segment (`urlindex.go`).

`linearMatcher` (`matcher.go`) is a direct transcription of the spec and
of `compiledRule.matches`; it is the reference for differential tests and
the benchmark baseline.

## Tests and benchmarks

```sh
go test ./scope/...                      # required, conformance, property, unit
go test -race ./scope/...
FF_PROPTEST_CASES=100000 go test -run Prop ./scope   # nightly-size properties
go test -run=^$ -fuzz=FuzzScopeNormalize -fuzztime=60s ./scope
go test -run=^$ -bench=BenchmarkScope01 ./scope
```

Conformance data lives in `conformance/scope/normalize/*.json` and
`conformance/scope/decide/*.json` (the latter includes the spec's
`wildcards.json` verbatim). Decide files hold a scope document, optional
resolver answers and errors, a default clock, and cases with optional
hops and expected decision fields. Required tests `TestScope001..016` run
the cases whose id carries their number.

## Known limitations

* asn rules are informational and never match (no ASN data here).
* Spec ambiguities and the choices made are listed in `OPEN_QUESTIONS.md`.
* `Normalize` allocates (inside `netnorm`); a decision costs about 7
  small allocations regardless of rule count.

## Choosing an algorithm

Decision point: first-matching-rule lookup (spec 4.3). `Compile(doc,
WithMatcher(m))` selects it; `Matchers()` lists the candidates. Both run
the conformance suite and `TestScopeProp_IndexedMatchesNaive` compares
them decision by decision.

| Matcher | 10 rules | 1,000 rules | 10,000 rules |
|---|---|---|---|
| `indexed` (default) | 975 ns | 1.26 µs | 1.30 µs |
| `linear` | 941 ns | 6.09 µs | 136 µs |

`go test -run '^$' -bench ScopeAlgo ./scope` (normalize + decide, 7
allocations per check either way). Measured 2026-11-13 on a 4-vCPU Linux container (Go 1.26, `GOMAXPROCS=4`); see `docs/ALGORITHMS.md` for the consolidated entry. The indexed matcher is flat in the
rule count; the linear scan is on par only for a handful of rules, where it
also skips building the tries.
