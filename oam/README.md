# oam

Open Asset Model domain types and the convergent graph merge for Flowforge.

**Spec:** 5 (graph model), 6.1-6.3 (observations), 23.5 (convergent merge);
tests 25.3 `oam_001..007`, property 25.4, benchmark 25.8 `b_oam_01`.
Layer 1: imports only `core` and `core/netnorm`.

## What is here

| File | Contents |
|------|----------|
| `asset*.go`, `opaque.go` | `Asset` interface, 12 modeled asset types, `Opaque`; `DecodeAsset`/`EncodeAsset`, `SplitAsset`/`JoinAsset` (storage columns `asset_type` + `content`) |
| `relation.go`, `label.go` | 5 relation types, `Discriminator()` (`tcp/443`, `rr1`), `ValidateEdge` and the 5.5 label table (`*EdgeError`) |
| `property.go` | `SourceProperty`, `SimpleProperty`, `DNSRecordProperty`, `VulnProperty`, `FlowforgeProperty` |
| `observation.go` | The 6.1 envelope, `DecodeObservation(data, Strict|Lenient)`, `Validate` (refs, ids, batch <= 500) |
| `confidence.go`, `merge.go`, `id.go` | Pure merge primitives: `Sources`, `CombineConfidence`, `Content`/`JoinContent`, `Stamp`, `EdgeKey`, `EntityID`/`EdgeID` |
| `graph*.go`, `change.go` | In-memory reference `Graph`: `Apply`, `CompleteRun` (covering-run staleness), views, `Changes`, `CanonicalBytes` |
| `jsonstrict.go` | Exact-key strict JSON checking used by `Strict` decoding |

Conformance data: `conformance/oam/keys` (spellings -> key) and
`conformance/oam/merge` (observation lists -> entities, edges, confidences,
change events; includes the 5.8 fragment).

## Merge rules (5.7, 23.5)

* Entities upsert by (tenant, program, asset type, key); ids are derived
  from that tuple, so equal states encode to equal bytes.
* `created_at` = min, `last_seen` = max of `observed_at`.
* Content: each top-level field is a last-writer-wins register over
  non-empty values, ordered by (observed_at, tool, observation_id); ties
  are broken by the canonical JSON of the value.
* Confidence: noisy-or over distinct sources, each source's maximum,
  rounded to 4 decimals, capped at 0.99. Sources are the reporting tool
  plus any `SourceProperty` tags in the observation.
* Edges upsert by (from, to, relation type, label, discriminator).
* Tags: one per (property type, name) per target; content merges like
  entity content.
* Idempotent per (tenant, observation_id); `Apply` is atomic.
* Staleness: `CompleteRun` after each covering run; a record is stale once
  every tool that covered it has missed it `stale_after_runs` (default 3)
  consecutive times. A sighting reactivates it. Nothing is deleted.

## Running

```sh
go test ./oam/...                                   # unit, conformance, property (1,000 cases)
FF_PROPTEST_CASES=100000 go test ./oam/ -run Prop   # nightly property depth
FF_SEED=123 go test ./oam/ -run Prop                # replay a failing seed
go test ./oam/ -run XXX -fuzz FuzzOAMDecodeObservation -fuzztime 60s
go test ./oam/ -run XXX -bench OAM01 -benchtime 3x  # b_oam_01 (10k and 100k observations)
```

## Known limitations

* The in-memory `Graph` keeps every applied observation id and change event
  forever; it is a reference model, not a store.
* An edge or tag that names an existing entity id fails (`ErrUnknownEntity`)
  if that entity has not been ingested yet, so such observations are
  order-dependent; local refs are not.
* Nested objects and arrays in content are merged as whole values.
* Throughput (~50-70k observations/s, see the benchmark) is dominated by
  JSON round-trips used to split typed assets into content maps.

Spec ambiguities and the choices made are in `OPEN_QUESTIONS.md`.
