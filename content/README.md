# content

Turns captured HTTP responses into normalized, versioned, comparable
artifacts with typed level-3 features, lineage across renamed bundles,
change classifications with scored reasons, redaction, and oam
observations with coverage-gated authority. Layer 2: imports `core`,
`core/netnorm` (through `oam`), the layer-1 parsers (`htmlparse`,
`jsparse`, `cssparse`, `xmlparse` + `sitemap`, `sourcemap`, `wellknown`),
`diff`, `sketch` and `oam`. Pure: no I/O, clock, randomness or goroutines.

**Spec:** `docs/V2_DESIGN.md` §5 with the §12 amendments (which win:
gated authority and coverage §3.4/§12.1, cause and baseline §12.2–3,
scores §12.4, noise and collection integrity §12.5, module lineage §12.6,
documents inside documents §12.7, headers as artifacts §12.8, sniffing
§12.9, parser assurance §12.10, redaction §12.11, classes and newness
§12.13), decision D2 (TypeScript/TSX/JSX are text).

## Pipeline

```
Capture{URL, Profile, Status, Headers, Body, FetchedAt, RedirectChain, Truncated, Outcome}
   │
   ├─ Sniff(body, content-type, url) ──▶ Kind (+ Hint, Mismatch, Disclosure)
   │
   ├─ body ──Normalize(kind)──▶ Normalized{Version, RuleTableSHA256, Text, Grade, Masks, MaskDigest}
   │            │                    └─ Derived: #script-n  #style-n  #handlers  #src/<path>
   │            └─ each derived document is normalized and extracted like a file
   ├─ headers (final + every hop) ──HeaderBlock──Normalize(headers)──▶ #headers artifacts
   │
   ├─ Extract(kind) ──▶ FeatureSet{Features (kind|key, family), Grades per extractor, Findings}
   ├─ JS: SplitModules ──▶ []Module (positional hashes)   FingerprintOf ──▶ lineage sketch
   ├─ Redact(normalized text) ──▶ what is stored, indexed, hashed for the index or logged
   └─ CheckIntegrity(features, previous, host bodies) ──▶ Outcome + flags ──▶ Coverage, CoveredRules
                                       │
Processed ──Observe──▶ oam.Observation{entities, edges, tags, Authority{source, profile, coverage, covered_rules}}
(prev, cur) ──Compare──▶ ChangeSet{artifact diffs (hunks, side by side), levels, outcome, class,
                                   newness, cause, baseline, Score{value, reasons}} ──Patch──▶ content_changed
new script ──FindPredecessor(page's previous scripts)──▶ previous_version
```

`Process` runs the left column for one capture; `Compare`, `Observe`,
`FindPredecessor` and `DiffModules` are separate so the store can call
them with what it keeps.

## API

| Area | Identifiers |
|---|---|
| Capture model | `Capture`, `Profile` (+`Normalize`, `DefaultProfile`), `Header`, `Hop`, `Outcome` |
| Sniffing | `Sniff`, `SniffResult`, `Kind` (`Kinds`), `Disclosure*` |
| Normalization | `Normalize`, `NormalizeOptions`, `Normalized`, `Derived`, `Normalizers`, `NormalizerFor`, `NormalizerInfo`, `HeaderBlock`, `ParseCSP`, `ParseSetCookie`, `SecurityHeaders`, `MaskClass`, `MaskCount` |
| Learned volatility | `VolatilitySet`, `VolatileRule`, `IsFeaturePath`, `VolatilityCounter`, `VolatilityPolicy`, `VolatilityAlgo`, `LearnVolatility` |
| Features | `Extract`, `ExtractInput`, `FeatureSet`, `Feature`, `FeatureKind`, `FamilyOf`, `ExtractorGrade`, `Extractors`, `ExtractorsFor`, `RunCanaries`, `DisabledByCanaries` |
| Secrets, redaction | `ScanSecrets`, `SecretFinding`, `SecretTier`, `SecretRuleIDs`, `KeyedHasher`, `TenantKeyHasher`, `Redact`, `RedactionPlaceholder` |
| Integrity | `CheckIntegrity`, `IntegrityInput`, `IntegrityReport`, `IntegrityFlag`, `PreviousCapture`, `HostBody`, `BodySimHash`, `LooksLikeErrorPage` |
| Pipeline | `Process`, `ProcessOptions`, `Processed`, `Artifact`, `Redirect`, `Limits` |
| Lineage | `LineageKey`, `LineageTokens`, `FingerprintOf`, `Fingerprint`, `FindPredecessor`, `LineageCandidate`, `LineageMatch`, `LineageAlgo` |
| Modules (level 2.5) | `SplitModules`, `SplitOptions`, `SplitStrategy`, `Module`, `DiffModules`, `ModuleDiff`, `ModuleChange` |
| Diff, classification | `DiffArtifacts`, `Compare`, `DiffOptions`, `DiffPolicy`, `ArtifactDiff`, `Hunk`, `FeatureChange`, `ChangeSet` (+`Patch`, `IsChange`), `ClassOf`, `NoiseCounter`, `AuditSample` |
| Scores | `ScoreChanges`, `ScoreChange`, `Score`, `ScoreReason` (score*.go import only pure stdlib) |
| Graph | `Observe`, `ObserveOptions`, `ObservationJSON` |

Errors wrap `ErrInvalidProfile`, `ErrInvalidCapture`, `ErrInvalidRule`,
`ErrMaskOnFeaturePath`, `ErrIncomparable`, `ErrUnknownAlgorithm`. Content
is never fatal: unreadable input is graded (`complete`, `partial`,
`unsupported`, `failed`) and counted.

## Normalizers

Each is `name@n` with a SHA-256 of its rule table (the `rules` strings in
the code; `conformance/content/normalizers/normalizers.json` pins them, so
a rule change without a version bump fails). A learned `VolatilitySet` k
makes the version `name.learnedk@n` and is part of the rule-table hash.

| Version | Kinds | Rule table SHA-256 (prefix) | Rules (summary) |
|---|---|---|---|
| `html@1` | html | `db7c52c2f2bb` | WHATWG parse; one line per tag and text run; comments dropped; text collapsed; attributes sorted; class tokens sorted; masks: nonce, CSRF inputs/meta, hidden values, integrity, cache-bust and build hash in URL attributes, volatile `data-*`, ISO times in text; inline scripts/styles/handlers → derived documents; output re-normalized to a fixed point |
| `js@1` | js | `7965e46bd07a` | tolerant parse (script, else module); re-printed one statement per line; directives re-quoted; masks: timestamp/UUID/build-id strings (never path-like ones), ISO times in strings and templates, 13-digit epoch-ms numbers; any diagnostic → line fallback, grade partial |
| `css@1` | css | `c75df612bd0d` | tokens re-emitted; comments dropped; canonical spacing; one declaration per line; redundant `;` dropped; strings re-quoted; masks in `url()`; invalid tokens stabilized |
| `json@1` | json, sourcemap | `16a8ff48e698` | canonical members sorted, two-space indent; numbers verbatim; volatile keys masked; invalid → text fallback |
| `xml@1` | xml, sitemap | `27b640e17e5b` | xmlparse; one line per tag/text/PI; namespace declarations then attributes sorted; comments dropped; gzip sitemaps inflated within bounds |
| `headers@1` | headers | `531554724f87` | security headers only, sorted; CSP directives and sources sorted, nonces removed; Set-Cookie → name + attributes, no value, no Expires; Location query masked |
| `text@1` | text, robots, securitytxt, ts_text, server_source | `ac1845a78f7b` | line endings, trailing blanks, blank-line runs, BOM; ISO times masked. D2: TS/TSX/JSX never reach the JS parser |
| `binary@1` | binary | `08f677122eaa` | `binary sha256:<hex> size:<n>`; hash only |

Masks never touch features: features are extracted from the raw bytes, and
a learned rule on a level-3 path (`IsFeaturePath`) is refused.
`Normalized.MaskDigest` tells a `masked_only` change from no change.

## Feature families

Every feature has a family equal to `oam.FamilyOfLabel` of its edge label
or `oam.FamilyOfProperty` of its tag type, so covered rules line up with
the graph's authority units.

| Extractor | Kinds | Features (family) | Graph mapping |
|---|---|---|---|
| `html.links@1` | html | link (links) | `links_to` URL→URL |
| `html.scripts@1` | html | script, lineage-keyed, sri, third_party (scripts) | `loads_script` URL→File |
| `html.styles@1` | html | style (styles) | `loads_style` URL→File |
| `html.frames@1` | html | frame (frames) | `embeds` URL→URL |
| `html.forms@1` | html | form → Endpoint `html_form`, parameter (forms, parameters) | `form_action`, ParameterProperty |
| `html.meta@1` | html | generator (libraries) | feature only |
| `js.endpoints@1` | js | endpoint `js_call` (fetch, XHR, axios, $.ajax, sendBeacon, EventSource, `*.http.get` …) or `js_literal` (path-like strings), query parameters (endpoints, parameters) | `calls` File/URL→Endpoint |
| `js.graphql@1` | js | graphql_operation (parameters) | ParameterProperty when its endpoint is known |
| `js.sourcemap@1` | js | sourcemap (sourcemaps) | `source_map` File→File |
| `js.libraries@1` | js | library from banners and known globals (libraries) | `includes_library` File→ProductRelease |
| `js.modules@1` | js | module_import (scripts) | feature only |
| `css.urls@1` | css | css_import (styles), css_asset (links) | `loads_style` File→File |
| `headers.security@1` | headers | header (headers) | HTTPHeaderProperty |
| `wellknown.robots@1` | robots | robots_rule, sitemap link (links) | `links_to` |
| `wellknown.securitytxt@1` | securitytxt | security_field (links) | feature only |
| `xml.sitemap@1` | sitemap | sitemap_entry (links) | `links_to` |
| `sourcemap.sources@1` | sourcemap | sourcemap_source (sourcemaps) | feature only; originals become `#src/` documents |
| `schema.openapi@1` | json | schema_endpoint `openapi`, parameter (schemas, parameters) | `documents` URL→Endpoint |
| `schema.graphql@1` | json, text | schema_endpoint `graphql`, graphql_operation (schemas, parameters) | `documents` |
| `secrets@1` | all text | secret (findings) | VulnProperty `exposed_secret` (keyed hash from the caller's `KeyedHasher`) |
| `disclosure@1` | all | source_disclosure (findings) | VulnProperty `source_disclosure` |

Every extractor reports a grade per document and has a canary fixture
(`RunCanaries`); a failed canary disables the extractor for withdrawal.
Bounds (§12.10, as in the Rust port): 8 MiB per response, XML/CSS depth
64, 20,000 facts per extractor per document, 200 inner documents.

### Secret rules (`secrets@1`)

Named, structured rules; the entropy tier is low confidence and never a
finding on its own (it is still redacted). Findings carry the rule id,
`oam.SecretPreview` and the keyed-hash input; the hash is computed by the
caller (`KeyedHasher`, e.g. `TenantKeyHasher`).

`aws_access_key_id`, `aws_secret_access_key`, `github_token`,
`github_fine_grained_pat`, `gitlab_token`, `slack_token`, `slack_webhook`,
`stripe_secret_key`, `google_api_key`, `openai_api_key`,
`anthropic_api_key`, `sendgrid_api_key`, `npm_token`,
`azure_storage_key`, `jwt`, `private_key`, `connection_string` (password
only), `url_credentials` (password only), and the low tier
`generic_high_entropy`. Redaction placeholder:
`[REDACTED:<rule>:<shape>:<length>]`.

## Coverage, classification and scores

* **Coverage** is `complete` only when the capture outcome is complete
  (no truncation, no integrity flag), the body normalized completely,
  every extractor that ran graded complete and none is canary-disabled;
  `failed` for degraded or failed captures (which emit only the source and
  its redirects and never withdraw); `unsupported` for binary bodies;
  `partial` otherwise. `CoveredRules` lists the families read completely
  (for a page: also the inline-script and inline-style families).
* **Outcomes** (counted, §12.5): `no_change`, `normalized_equal`,
  `masked_only`, `reordered` (same lines, or same modules in another
  order), `structural` (text only), `changed` (level 3), `degraded` (no
  diff). Level-3 changes are computed over the whole capture, so a feature
  moving between documents is not a change.
* **Class**: `semantic_endpoint` (endpoint, form, parameter, GraphQL
  operation), `metadata` (headers only), `observation` (other level-3),
  `structural` (text only). `catalog` comes from the advisory matcher
  (D4), not from here. **Newness**: `likely_new` against a complete
  previous capture, `first_seen_only` without one, `pre_existing` for the
  baseline and for returns. **Cause**: `target` unless every raw body is
  identical and a normalizer (`normalizer`) or extractor (`parser`)
  version differs. **Baseline**: the first complete capture.
* **Scores** (§12.4): additive, clamped, named reasons stored with the
  change; base weights (exposed_secret 0.9, source_disclosure 0.85,
  new_endpoint 0.4, removed_security_header 0.35, csp_loosened 0.3,
  new_form 0.3, source_map_exposed 0.3, cookie_flags_weakened 0.25,
  third_party_script_without_sri 0.25, library_version_change 0.2, …),
  word boosts (admin, auth, payment, internal, debug), state-changing
  methods and `{id}` templates; removals count −½ of the positive weight,
  returns −¼. Scores rank, never send: `score*.go` imports only pure
  standard-library packages (checked by CONTENT-INV-11).

## Invariants (port these)

| Id | Test | Statement |
|---|---|---|
| CONTENT-INV-01 | `TestContentInv01_NormalizeIdempotent` | For every kind, `Normalize(k, Normalize(k, x).Text).Text == Normalize(k, x).Text` on fixtures and generated, often malformed, documents, with and without learned rules. |
| CONTENT-INV-02 | `TestContentInv02_VolatileInvariantSemanticDetected` | Normalization is invariant under the declared volatile transformations (nonce, CSRF, timestamps, cache-bust, build hashes, mark-up and minifier re-formatting); a planted semantic change (link, script, form, endpoint, frame) is always detected as a feature. |
| CONTENT-INV-03 | `TestContentInv03_MasksNeverHideFeatures` | Learned rules never change features; rules on level-3 paths are refused by `Validate` and never frozen by `LearnVolatility` (both detectors); path-like values are never masked. |
| CONTENT-INV-04 | `TestContentInv04_HiddenFieldsRedacted` | Hidden form fields keep only short numeric or boolean values in normalized output. |
| CONTENT-INV-05 | `TestContentInv05_IdenticalDiffEmpty` | Diffing identical captures under every diff policy gives no levels, no hunks, no feature changes, outcome no_change, no class, score 0. |
| CONTENT-INV-06 | `TestContentInv06_DegradedNeverComplete` | Login redirect, challenge, CAPTCHA with a blocking status, error after success, soft 404, mass removal, catch-all, truncation, fetch failure and a partial collector outcome never give coverage complete; their observations never withdraw; degraded captures get no diff. |
| CONTENT-INV-07 | `TestContentInv07_PartialGradeNotComplete` | Any extractor grade below complete makes coverage not complete and leaves that extractor's families uncovered. |
| CONTENT-INV-08 | `TestContentInv08_LineageRenamedMatched` | Every lineage matcher links a renamed chunk with a few edits and renamed locals to its predecessor and never to an unrelated bundle; a unique lineage key decides first. |
| CONTENT-INV-09 | `TestContentInv09_ModuleMovesNoChange` | Reordering bundler modules and renaming their locals yields no module change and no semantic change; editing one module yields exactly one changed module. |
| CONTENT-INV-10 | `TestContentInv10_RedactionRemovesSecrets` | Over generated secrets of every rule planted in HTML, JS, JSON and text, no secret survives in normalized snapshots, hunks, side-by-side rows, features, change sets or observations; `Redact` is idempotent. |
| CONTENT-INV-11 | `TestContentInv11_ScoreImportsNoEgress` | Scores lie in [0, 1] and equal the clamped sum of their reasons; reasons are unique, sorted, valid for oam; removals and returns alone score 0; `score*.go` imports nothing that reaches `net`, `os` or `syscall`. |
| CONTENT-INV-12 | `TestContentInv12_Determinism` | The same inputs give byte-identical processed captures, change sets and observations. |
| CONTENT-INV-13 | `TestContentInv13_SniffByContent` | Decisive bodies keep their kind under any media type; Mismatch is exactly "hint known and incompatible"; server-side source is a disclosure finding; Sniff is total and deterministic. |
| CONTENT-INV-14 | `TestContentInv14_CoverageCompleteIff` | Coverage is complete iff the outcome and body normalization are complete, every extractor that ran is complete and none applicable is canary-disabled. |
| CONTENT-INV-15 | `TestContentInv15_Cause` | Identical bodies with another normalizer version give cause normalizer, with another extractor set cause parser; changed bodies give target; the first complete capture is the baseline. |
| CONTENT-INV-16 | `TestContentInv16_Canaries` | Every extractor has exactly one passing canary; a disabled extractor's families are never covered. |

Fuzz targets assert the invariants on arbitrary input: `FuzzContentSniff`
(13), `FuzzContentNormalize` (01 for every kind, 10's idempotence),
`FuzzContentExtract` (totality, a grade for every extractor, sorted
features with their family, no secret value in features).

## Conformance data (`conformance/content/`)

| Path | Content | Source of the expected values |
|---|---|---|
| `sites/spa/` | The SPA fixture: page v1–v3 with headers, webpack chunk deploys `main.3f9a1c2b` → `main.77e0d41a` (reordered, renamed) → `main.5b6c7d8e` (new admin endpoint, vendor library bump), vite-style chunks `index-a1b2c3d4` → `app-e5f6a7b8` (renamed, edited) and an unrelated `other-0f0f0f0f` | hand-written / generated once by a seeded script |
| `spa/expected.json` | Features, normalized SHA-256 per artifact, coverage, classifications, scores, module diffs, lineage and observation edges of the site | generated (`FF_CONTENT_UPDATE=1`) and reviewed by hand: v1→v2 masked_only; v2→v3 exactly the planted changes; bundle reorder = reordered; bundle v3 = one endpoint + one library version |
| `normalize/cases.json` | Exact normalized outputs, grades, masks and derived documents per kind | inputs hand-written, outputs generated and reviewed |
| `sniff/cases.json`, `secrets/cases.json` | Sniffing and secret rules / redaction | hand-written |
| `normalizers/normalizers.json` | Versions and rule-table hashes | generated; changes need a version bump |

`go test ./content` only compares; `FF_CONTENT_UPDATE=1` rewrites the
generated files (review the diff before keeping it).

## Choosing an algorithm

Every decision keeps all candidates selectable in non-test code; the
comparative benchmark is `go test -run '^$' -bench ContentAlgo -benchmem
./content` (`BenchmarkContentAlgo/<decision>/<candidate>`). Measured
2026-11-13 on the 4-vCPU Xeon development machine (Go 1.26), `-benchtime=3x`,
with other work running, so the numbers are relative.

| Decision (config) | Candidate | Measured | Default and why |
|---|---|---|---|
| lineage matcher (`ProcessOptions.Lineage`, `LineageOptions.Algo`; `simhash`/`minhash`/`jaccard`) | `simhash` | 13.2–13.3 ms to fingerprint a 200 KB bundle and scan 20 candidates; 8 B stored per version | **default** (V2 §5): fixed 8-byte sketch per stored version, Hamming ≤ 6 via a linear scan (sketch's multi-index is exact only to 3) |
| | `minhash` | 21–22 ms; 1 KiB stored | when the Jaccard value itself matters (estimate with a known error) |
| | `jaccard` | 5.7–6.0 ms; 8 B × shingles stored | exact reference; cheapest when both token streams are at hand, but every old version must keep its shingle set |
| module splitter (`SplitOptions.Strategy`; `auto`/`webpack`/`esbuild`/`toplevel`) | `auto` | 38–41 MB/s on a 2 MB webpack bundle | **default**: webpack tables + esbuild wrappers + rollup/vite top-level statements, so every bundler shape gets module identity |
| | `webpack` | 40–47 MB/s | webpack-only sites (the rest is one runtime unit) |
| | `esbuild` | 33 MB/s | esbuild-only sites |
| | `toplevel` | 36–40 MB/s | scope-hoisted (rollup/vite) output; no table detection |
| volatility detector (`VolatilityPolicy.Algo`; `ratio`/`streak`) | `ratio` | 1.6–1.7 ms per 10,000 counters | **default**: robust to an occasional stable observation (≥ 8 observations, ≥ 90 % changed) |
| | `streak` | 1.5 ms | learns faster (5 consecutive changes) but one stable observation resets it |
| diff policy (`DiffOptions.Policy`; `size`/`myers`/`histogram`/`patience`) | `size` | 34–44 MB/s on a 512 KB normalized bundle, 1 % edited | **default**: MyersLinearSpace tokens up to 16 KiB (minimal scripts on small artifacts), Histogram lines above (docs/ALGORITHMS.md: Histogram for large normalized snapshots); a reordered 20 KB bundle took ≈ 0.2 s per diff at token level against the 10,000-edit cap |
| | `myers` | 5–7 MB/s | minimal scripts up to the §5 4 MiB cap |
| | `histogram` | 26–42 MB/s | lines always; git-compatible hunks |
| | `patience` | 30 MB/s | lines always; more memory |

All policies go through `diff.BoundedDiff`, so the §5 bounds (edit cap,
4 MiB token / 64 MiB line caps, hash-only fallback, recorded level and
reason) hold for every candidate.

The same table is the `content` entry in `docs/ALGORITHMS.md`.

## Benchmarks

`go test -run '^$' -bench 'ContentNormalize|ContentExtract|ContentPipeline' -benchtime=3x -benchmem ./content`
(1 MiB inputs, same machine and caveats; ranges over two runs):

| Kind | Normalize MB/s | Extract MB/s |
|---|---|---|
| html (includes the fixed-point re-parse) | 9–12 | 11.6–12.5 |
| js (generated webpack bundle) | 17–20 | 8.9–9.3 |
| css | 29–34 | 8–11 |
| json | 3.7–4.9 | 10.7–13 |
| xml / sitemap | 8.7–11.4 | 16.6–18.7 |
| text | 92–102 | 42–43 |

End to end (`BenchmarkContentPipeline`): a 2 MB bundle captured, sniffed,
normalized, redacted, extracted, split into modules, fingerprinted, then
diffed and classified against the previous deploy: 1.3–1.5 s, 1.5–1.7 MB/s,
dominated by the JS parse, the line diff, and the SimHash over all
shingles.

## Running

```sh
go test ./content/...
FF_SEED=1 FF_PROPTEST_CASES=100000 go test ./content/ -run Inv
go test ./content -run '^$' -fuzz FuzzContentNormalize -fuzztime 60s -fuzzminimizetime 2s   # also FuzzContentSniff, FuzzContentExtract
go test ./content -run '^$' -bench . -benchtime=3x -benchmem
FF_CONTENT_UPDATE=1 go test ./content -run Conformance                                      # regenerate generated conformance files
```

## Known limitations

See `OPEN_QUESTIONS.md`: JSON normalization is slow (encoding/json token
reader), HTML normalization re-parses its output, route-table keys in JS
are not read, GraphQL SDL root types are found by their conventional
names, and endpoint method pairing in JS calls is a short look-ahead.
