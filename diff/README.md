# diff

Differences between token sequences, JSON documents and ordered labelled
trees for Flowforge v2 change detection (V2_DESIGN §2, §5, §6.3). Layer 1:
standard library only. Pure, deterministic, no goroutines; every error wraps
a sentinel with a stable text (`errors.go`).

Governing references: Myers, "An O(ND) Difference Algorithm and Its
Variations" (1986); git's `xdiff/xpatience.c`, `xhistogram.c` and
`xdl_change_compact`; diff-match-patch `diff_cleanupSemantic`; RFC 6901 and
RFC 6902; Zhang & Shasha, "Simple fast algorithms for the editing distance
between trees" (1989).

## API

| Area | Identifiers |
|---|---|
| Interning | `Interner` (strings → dense `uint32` ids, first-seen order), `SplitLines` (keeps `\n`), `SplitWords` (word / space / single-rune tokens that tile the input) |
| Algorithms | `Diff(a, b []uint32, Options)`, `DiffStrings`, `Algorithm` (`MyersLinearSpace` default, `Myers`, `Patience`, `Histogram`, `Reference`), `Options.MaxEditDistance` (cap → `Result.Capped`), `Options.GitCompact` (`CompactGit`) |
| Edit scripts | `EditScript` (canonical Equal/Delete/Insert ops over index ranges plus the changed tokens), `Apply`, `Invert`, `Compose`, `Validate`, `Distance` |
| Presentation | `Hunks(script, context)`, `Unified` / `HunkHeader`, `ParseUnified`, `ApplyUnified` (strict), `SideBySideOf` + `MarshalSideBySide`, `RefineLine` (word-level spans) |
| Cleanup | `CleanupSemantic` with `CleanupOptions.Score` (`TextScorer`, `LineScorer`) |
| Bounds (§5) | `BoundedDiff(old, new, Bounds)`: token algorithm capped at `MaxEditDistance` (10,000) up to `MaxInputBytes` (4 MiB) → line algorithm up to `MaxLineInputBytes` (64 MiB) → hash only; `Level`, `Fallback`; both algorithms pluggable (`Bounds.TokenAlgorithm`, `Bounds.LineAlgorithm`) |
| JSON | `Value` (tagged tree; `Kind`), `ParseJSON` (UseNumber, duplicate keys rejected), `EncodeJSON`, `Canonical`, `Equal` (RFC 6902 equality), `JSONDiff` (add/remove/replace; arrays by Myers over hash-consed ids), `PatchOp`, `MarshalPatch`, `ParsePatch`, `ApplyPatch` (all six RFC 6902 ops), `ParsePointer`, `EscapePointerToken` |
| Trees | `Tree` (arena interface: `Root`, `Label`, `FirstChild`, `NextSibling`), `ArenaTree`, `ParseBracket` / `FormatBracket`, `TreeDiff` (Zhang–Shasha distance + optimal Tai mapping, `TreeCosts`, `MaxNodes` default 2000) |

```go
in := diff.NewInterner()
a, b := in.InternAll(diff.SplitLines(old)), in.InternAll(diff.SplitLines(new))
res, _ := diff.Diff(a, b, diff.Options{Algorithm: diff.Histogram, GitCompact: true})
text, _ := diff.Unified(in.Values(a), in.Values(b), res.Script, diff.UnifiedOptions{OldName: "a/x", NewName: "b/x", Context: 3})
```

## Choosing an algorithm

All five algorithms implement the same contract (canonical, Apply-correct
`EditScript` over the same ids) and are selected by `Options.Algorithm`
(config names `myers-linear`, `myers`, `patience`, `histogram`,
`reference`); `BoundedDiff`'s fallback chain is built from the same
selector. Benchmark: `go test -run '^$' -bench DiffAlgo ./diff`
(`BenchmarkDiffAlgo/<algo>/<size>/<edit%>`). Inputs are log-uniform tokens
over a 50,000-word vocabulary (a few very frequent, most rare) with random
replace / insert / delete / block-move edits. Measured 2026-11-13, 4-vCPU
Intel Xeon, Go 1.26, one run each (`-benchtime=1x`):

| Input | `myers-linear` | `myers` (greedy) | `patience` | `histogram` | `reference` |
|---|---|---|---|---|---|
| 1k, 1 % | 24 µs, 35 KB | 16 µs, 22 KB | 100 µs, 137 KB | 89 µs, 74 KB | 2.6 ms, 4.0 MB |
| 100k, 0.1 % | 1.5 ms, 3.3 MB | 1.2 ms, 2.1 MB | 9.4 ms, 9.2 MB | 12 ms, 4.6 MB | — (O(NM)) |
| 100k, 1 % | 20 ms, 3.6 MB | 34 ms, 47 MB | 6.4 ms, 11 MB | 15 ms, 5.0 MB | — |
| 100k, 10 % | 1.5 s, 8.6 MB | — (O(D²) trace) | 19 ms, 24 MB | 38 ms, 10 MB | — |
| 1M, 0.1 % | 32 ms, 32 MB | 44 ms, 60 MB | 75 ms, 69 MB | 137 ms, 41 MB | — |
| 1M, 1 % | 2.3 s, 37 MB | — | 144 ms, 163 MB | 163 ms, 46 MB | — |
| 1M, 10 % | — (hours) | — | 175 ms, 273 MB | 285 ms, 97 MB | — |

Script size (inserted + deleted tokens): the three minimal algorithms are
equal by construction; patience and histogram matched the minimum on every
input above except 10 % edits (+0.05 %, e.g. 21,448 vs 21,446 at 100k).
Hunk counts at context 3 differed by under 1 %. On real code the difference
is alignment, not size: on lines that repeat (`{`, `}`, blank lines) Myers
may anchor on them, while patience and histogram anchor on rare lines and
keep moved functions intact (`conformance/diff/unified_git.json`,
`patience_c_functions`). `BenchmarkDiffCapped`: on 200k tokens with 10 %
edits Myers takes 6.0 s uncapped, 162 ms with the default 10,000 cap and
2.8 ms with a cap of 1,000, then reports `Capped`.

* **Default `myers-linear`** (`Diff`, and `BoundedDiff`'s token level):
  shortest scripts with O(N+M) memory (≈ 19 B per token, DIFF-INV-14), the
  fastest choice for the small edit rates of monitored content, and its cost
  is bounded by the D cap; past the cap §5 falls back to histogram.
* **`histogram`** (`BoundedDiff`'s line level, and for anything shown to
  people): git-compatible output, near-minimal, robust at high edit rates
  and on repeated lines, O(N) memory.
* **`patience`**: fastest at high edit rates on inputs with many unique
  lines; more memory; ranges without unique lines fall back to Myers (which
  is where it departs from git).
* **`myers`** (greedy): the textbook algorithm, kept as the readable
  reference for the linear variant; O(D²) memory, so only for small D.
* **`reference`**: the O(NM) dynamic program, for tiny inputs and as an
  oracle (refuses N·M > 2^24 with `ErrInputTooLarge`).

(Entry text for `docs/ALGORITHMS.md`; that file is outside this package's
write scope in the v2 build and is linked from here once it exists.)

## Invariants (port these)

| Id | Statement | Test |
|---|---|---|
| DIFF-INV-01 | Every algorithm returns a canonical script and `Apply(a, Diff(a, b)) == b` | `TestDiffInv01_ApplyCorrect` |
| DIFF-INV-02 | `myers-linear` and `myers` distances equal the `reference` DP's n+m−2·LCS | `TestDiffInv02_MyersMinimal` |
| DIFF-INV-03 | `patience` and `histogram` are Apply-correct with distance ≥ the Myers distance | `TestDiffInv03_HeuristicsBoundedByMyers` |
| DIFF-INV-04 | `Apply(b, Invert(s)) == a`; `Invert` is an involution preserving distance | `TestDiffInv04_Invert` |
| DIFF-INV-05 | `Compose(s1, s2)` is canonical, applies a→c, distance ≤ d1+d2, identity is neutral, mismatched scripts are rejected | `TestDiffInv05_Compose` |
| DIFF-INV-06 | Hunks cover every change exactly once, in order; context is exactly min(C, available); hunks are separated by > 2C equal tokens, so contexts never overlap or touch | `TestDiffInv06_Hunks` |
| DIFF-INV-07 | `Unified` → `ParseUnified` → `ApplyUnified(old)` == new for every algorithm and context; parsed headers equal `HunkHeader`; a tampered base is refused | `TestDiffInv07_UnifiedRoundTrip` |
| DIFF-INV-08 | Git compatibility: with `GitCompact`, `Unified` reproduces git 2.43's hunks byte for byte (histogram always; patience without Myers fallback; Myers where git agrees) | `TestDiffInv08_GitCompatibility` (`unified_git.json`) |
| DIFF-INV-09 | `ApplyPatch(a, JSONDiff(a, b))` is `Equal` to b; equal documents give an empty patch; patch and document encodings round trip | `TestDiffInv09_JSONPatchRoundTrip` |
| DIFF-INV-10 | `Equal` is RFC 6902 equality (reflexive, symmetric, order-insensitive objects, numeric numbers) and agrees with hash-consed ids | `TestDiffInv10_JSONEquality` |
| DIFF-INV-11 | Tree edit distance is a metric on random trees (identity, symmetry, triangle) and its mapping realises it | `TestDiffInv11_TreeMetric` |
| DIFF-INV-12 | Zhang–Shasha equals an exhaustive Tai-mapping search on tiny trees (unit and other costs); the mapping is a valid Tai mapping costing the distance | `TestDiffInv12_TreeBruteForce` |
| DIFF-INV-13 | Determinism: identical inputs give byte-identical scripts, unified text, side-by-side JSON, patches and tree mappings across runs | `TestDiffInv13_Determinism` |
| DIFF-INV-14 | Linear-space Myers allocates O(N+M) (≤ 96 B/token, ≤ 5× bytes for 4× input) while the greedy trace grows quadratically | `TestDiffInv14_LinearSpaceMemory` |
| DIFF-INV-15 | A cap below D yields `Capped` with an Apply-correct script keeping the common prefix; a cap ≥ D changes nothing; `BoundedDiff` (any pluggable algorithms) reports level and reason and its script applies at that level | `TestDiffInv15_CapsAndFallbacks` |
| DIFF-INV-16 | `CleanupSemantic` is Apply-correct, canonical, never below minimal, leaves no eliminable equality (except step-4 overlaps) and is idempotent | `TestDiffInv16_SemanticCleanup` |
| DIFF-INV-17 | Word refinement spans are sorted, disjoint, in bounds; removing them leaves equal text | `TestDiffInv17_WordRefinement` |
| DIFF-INV-18 | Side-by-side rows reconstruct each hunk's old and new lines with consecutive line numbers | `TestDiffInv18_SideBySide` |
| DIFF-INV-19 | `SplitLines`/`SplitWords` tile the input; interned ids are dense, first-seen, equal iff strings are equal | `TestDiffInv19_SplitAndIntern` |
| DIFF-INV-20 | `reference` equals exhaustive LCS recursion on tiny inputs and refuses inputs over `MaxReferenceCells` | `TestDiffInv20_ReferenceExhaustive` |

The fuzz targets assert the same invariants: `FuzzDiffApply` (01–04, 15,
GitCompact keeps distance), `FuzzUnifiedRoundTrip` (07, parser totality),
`FuzzJSONPatch` (09, `ParsePatch`/`ApplyPatch` totality), `FuzzTreeDiff`
(11–12 on ≤ 7 nodes).

## Conformance data (`conformance/diff/`)

| File | Content | Source of the expected values |
|---|---|---|
| `scripts.json` | Scripts per algorithm (± git compaction) for constructed and random token pairs, with the minimal distance | reference implementation (pins alignment choices); distance checked against the DP |
| `unified_git.json` | 320 unified renderings (constructed edge cases: zero-length ranges, missing final newline, CRLF, empty files, 2C gaps, plus random) | git 2.43 `diff --no-index --no-indent-heuristic` |
| `unified_parse.json` | Parse/apply edge cases and malformed patches | hand-written |
| `cleanup.json` | `cleanupMerge`, `cleanupSemanticLossless`, `cleanupSemantic` vectors | diff-match-patch's published test suite |
| `json_patch_apply.json` | RFC 6902 Appendix A.1–A.16 and pointer/index edge cases | RFC 6902 |
| `json_diff.json` | `JSONDiff` patches | reference implementation, round trip checked |
| `tree.json` | Tree distances incl. the Zhang–Shasha paper example | independent exhaustive Tai-mapping search (Python) |
| `bounded.json` | `BoundedDiff` levels and fallbacks | reference implementation |

`TestDiffConformance` only compares; it never rewrites the files.

## Tests and benchmarks

```sh
go test ./diff/...
FF_SEED=1 FF_PROPTEST_CASES=100000 go test ./diff/
go test ./diff -run '^$' -fuzz FuzzDiffApply -fuzztime 60s -fuzzminimizetime 2s
go test ./diff -run '^$' -bench DiffAlgo -benchtime=1x
go test ./diff -run '^$' -bench 'DiffCapped|DiffBounded|DiffJSON|DiffTree' -benchtime=1x
```

Other baselines (same machine): `BoundedDiff` on 2 × 1 MiB of code-like
text, 8–10 MB/s both at the token level and after falling back to lines;
`JSONDiff` of two 20,000-record arrays (1.4 MB each, 1 % changed) 56 ms,
41 MB/s; `TreeDiff` of two 400-node trees 9 ms.

## Limitations

See `OPEN_QUESTIONS.md`: git's Myers tie-breaking is not reproduced,
`JSONDiff` never emits move/copy, `ApplyUnified` has no offset or fuzz
search, and `TreeDiff` is quadratic in memory (bounded by `MaxNodes`).
