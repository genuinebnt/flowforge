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
| `endpoint.go` | v2 `Endpoint` asset (`ep:<METHOD> <url_template>`), `NormalizeURLTemplate`, `NormalizeHTTPMethod` |
| `label.go`, `family.go` | v2 relation labels (`loads_script` ... `documents`) and their 5.5 rows; rule families (`FamilyOfLabel`, `FamilyOfProperty`) |
| `property_v2.go`, `secret.go` | `ContentProperty`, `HTTPHeaderProperty`, `ParameterProperty`; exposed_secret conventions, `LooksLikeSecret`, `CheckNoSecrets` |
| `authority.go` | Coverage-gated authoritative staleness (`Authority`, `AuthUnit`), `tag_changed` emission (`TagPatch`) |
| `content_change.go` | `content_changed` patches (`ContentChangePatch`) and `Graph.RecordContentChange` |

Conformance data: `conformance/oam/keys` (spellings -> key, now with
`endpoint.json`), `conformance/oam/merge` (observation lists -> entities,
edges, confidences, change events; includes the 5.8 fragment and the
authority / `tag_changed` cases), `conformance/oam/url_template`
(template normalization), `conformance/oam/labels` (v2 label endpoints),
`conformance/oam/properties` (v2 properties and secret rules) and
`conformance/oam/content_change` (content_changed patches).

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

## v2 graph expansion (V2 design §3)

* **Endpoint** `{method, url_template, source}`, key
  `ep:<METHOD> <url_template>`. `NormalizeURLTemplate`: netnorm rules on
  the concrete parts; `${...}` and `{name}` become `{expr}` (`{id}`,
  `{uuid}`, `{hex}`, `{expr}` kept); all-digit / UUID-shaped /
  hex (>= 16) segments become `{id}` / `{uuid}` / `{hex}`; query and
  fragment dropped; scheme and host must be concrete. Rules in the
  function's doc comment.
* **Labels** `loads_script`, `loads_style`, `embeds`, `links_to`,
  `redirects_to` (SimpleRelation with optional `status` 300-399),
  `form_action`, `calls`, `source_map`, `previous_version`,
  `includes_library`, `documents`, validated like the 5.5 docs labels.
* **Authority** (gated on coverage): an observation may carry
  `authority {source, profile, coverage, covered_rules}`. Edges leaving
  the source and tags on it go to the (source, profile, rule family)
  unit; only a `complete` reading of a covered family withdraws (edge
  `edge_removed` / stale, tag removed); partial, failed and unsupported
  readings only add. Liveness: classic support (covering runs, 5.7,
  unchanged) or authoritative support (held edge; entity pointed at by a
  held edge). Exact rules and the convergence argument:
  `OPEN_QUESTIONS.md` 22-27.
* **Events**: `tag_changed` for every visible non-source tag change
  (root add / root remove / field ops, one per tag per operation);
  `content_changed` via `RecordContentChange`; `entity_added` and
  creating `edge_added` carry `base` (state at creation) so the log
  replays; every event carries the observation's `cause` / `baseline`.
* **Commonness** (a node held by more than 50 distinct hosts is
  `common`; association is not permission) is a graph query in
  `graphstore` (`Store.Commonness`), not oam state.

## Invariants (port these)

| Id | Test | Statement |
|----|------|-----------|
| OAM-INV-01 | `TestOAMInv01_URLTemplateIdempotent` | `NormalizeURLTemplate` is a fixed point on every accepted input. |
| OAM-INV-02 | `TestOAMInv02_URLTemplateEquivalentSpellings` | Equivalent spellings (case, default ports, IDN, escapes, dot segments, query/fragment, placeholder spellings, id-like literals) give the same template. |
| OAM-INV-03 | `TestOAMInv03_URLTemplateConcreteHost` | Every accepted template has a concrete, netnorm-canonical http(s) origin; placeholder-free URLs agree with netnorm. |
| OAM-INV-04 | `TestOAMInv04_EndpointKey` | Endpoint key = `ep:` + normalized method + ` ` + normalized template; Normalize is idempotent; source never changes the key. |
| OAM-INV-05 | `TestOAMInv05_AuthoritativeConvergence` (oam, graphstore, graphstore/pg) | With authoritative observations of every coverage, any permutation and duplication gives byte-identical state. |
| OAM-INV-06 | `TestOAMInv06_LatestAuthoritativeNeverStale` | An edge held by a unit's effective member set is active (covering runs included); one held by none and never reported classically is stale; held edges keep their targets active; header tags are visible iff held or classic. |
| OAM-INV-07 | `TestOAMInv07_TagChangedExactlyOnce` | Exactly one `tag_changed` per tag whose visible content changed per operation, none otherwise; its patch maps old to new. |
| OAM-INV-08 | `TestOAMInv08_ChangeReplay` | Folding the change log (base + patches) reproduces every entity's content, scope, state and tags and every edge's state and tags after every operation. |
| OAM-INV-09 | `TestOAMInv09_ContentChangeIdempotent` | `RecordContentChange` changes no state, is idempotent per (entity, from, to, diff), stores the normalized patch. |
| OAM-INV-10 | `TestOAMInv10_CauseAndBaselineOnEvents` | Every event of an observation carries its cause and baseline; covering-run events carry none. |
| OAM-INV-11 | `TestOAMInv11_NoSecretValues` | exposed_secret tags never contain the secret; `LooksLikeSecret` flags credentials, passes digests, UUIDs, previews and keyed hashes. |
| OAM-INV-12..15 | graphstore | AsOf, Mem == pg, commonness, kernel == oam (see `graphstore/README.md`). |

## Running

```sh
go test ./oam/...                                   # unit, conformance, property (1,000 cases)
FF_PROPTEST_CASES=100000 go test ./oam/ -run Prop   # nightly property depth
FF_SEED=123 go test ./oam/ -run Prop                # replay a failing seed
go test ./oam/ -run XXX -fuzz FuzzOAMDecodeObservation -fuzztime 60s
go test ./oam/ -run XXX -fuzz FuzzURLTemplate -fuzztime 60s
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
* Withdrawn tags and authority units are kept forever (hidden), so a
  re-add merges with the old content stamps; nothing is ever deleted.

Spec ambiguities and the choices made are in `OPEN_QUESTIONS.md`.
