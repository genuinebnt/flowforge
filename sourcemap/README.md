# sourcemap

Source Map v3 decoder and mappings encoder for Flowforge v2
(`docs/V2_DESIGN.md` §4.3), following ECMA-426. Layer 1: standard
library and `core` only.

## What it implements

* `Parse` — strict JSON (`encoding/json` into typed structs): `version`
  must be the number 3; regular maps need `mappings` and `sources`;
  `sources` entries may be null; `sourcesContent` (short lists mean "no
  content"), `names`, `file`, `sourceRoot`, `ignoreList` and the legacy
  `x_google_ignoreList`; the `)]}'` XSSI prefix is skipped; unknown
  fields are ignored.
* Index maps: sections need an offset, must be strictly increasing and
  must not overlap (a section's mappings must end before the next
  offset); nested index maps and `url` sections are refused. The result
  is flattened into one `Map`.
* `DecodeMappings` / `EncodeMappings` / `DecodeVLQ` / `AppendVLQ` — own
  Base64 VLQ state machine (`stateSegmentStart`, `stateVLQ`,
  `stateFieldEnd`) with 32-bit overflow checks, 1/4/5-field segments,
  bounds and non-negativity checks.
* `(*Map).OriginalPositionFor` — binary search over mappings sorted by
  generated position (greatest lower bound on the line).
* `(*Map).ResolveSources` — sourceRoot joining and resolution against
  the map URL.
* Limits: `MaxInputBytes` 8 MiB, `MaxSources` 20,000 → `partial`.

## Conformance

`conformance/sourcemap/vlq.json` (boundary encodings computed by an
independent reference), `mappings.json` (the ECMA-426 example, field
counts, separators, errors with offsets), `maps.json` (regular and index
maps, resolution, sourcesContent, ignoreList, every error code, grades).

## Invariants (port these)

| Id | Test | Statement |
|---|---|---|
| SMAP-INV-01 | `TestSMapInv01_VLQRoundTrip` | `DecodeVLQ(AppendVLQ(v)) = v` for random and boundary int32 values (MinInt32 included); encodings are minimal and at most 7 digits. |
| SMAP-INV-02 | `TestSMapInv02_MappingsRoundTrip` | `decode(encode(ms)) = ms` (in-line order kept, columns to MaxInt32) and `encode(decode(s)) = s` for canonical `s`. |
| SMAP-INV-03 | `TestSMapInv03_LookupMatchesBruteForce` | `OriginalPositionFor` agrees with a linear scan. |
| SMAP-INV-04 | `TestSMapInv04_SortedAndInBounds` | Every accepted map (regular, index, mutated) has mappings sorted by generated position with every index and position in range. |
| SMAP-INV-05 | `TestSMapInv05_IndexMapAgreesWithSections` | Lookup in a flattened index map equals lookup in the section's own map, offset-adjusted. |
| SMAP-INV-06 | `TestSMapInv06_LimitsGradePartial` | A limit hit is `partial`, never `complete`; partial maps keep SMAP-INV-04. |
| SMAP-INV-07 | `TestSMapInv07_LinearTime` | Allocations at 4n ≤ 4.4x of n; time per byte ≤ 1.5x. |

Fuzz target: `FuzzSourceMap` (INV-02, 04, 06 and totality).

## Rust mapping

Mirrors getsentry's `sourcemap` crate: `Mapping` ≈ `RawToken`
(`dst_line`, `dst_col`, `src_id`, `src_line`, `src_col`, `name_id`, with
-1 for `!0`), `Parse` ≈ `decode_slice` for regular and index maps
(`SourceMapIndex::flatten`), `OriginalPositionFor` ≈ `lookup_token`.
Differences: values are checked to fit `i32` (the crate uses `i64` and
checked shifts), sections must be strictly ordered (the crate sorts
them), and segments must have 1, 4 or 5 fields. The JSON layer is
`serde_json` in Rust.

## Running

```
go test ./sourcemap/
go test ./sourcemap/ -run XXX -fuzz FuzzSourceMap -fuzztime 20s
go test ./sourcemap/ -run XXX -bench . -benchmem
```

Benchmarks (shared 4 vCPU Xeon): `Parse` 60–65 MB/s on bundle-like maps
(256 KiB and 1 MiB), lookup ~300 ns, 0 allocations.

## Limitations

See `OPEN_QUESTIONS.md`.
