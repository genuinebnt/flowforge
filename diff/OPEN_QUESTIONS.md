# diff: open questions and decisions

Each item states the ambiguity, the choice and what pins it.

## Algorithms

1. **Algorithm names.** `MyersLinearSpace` ("myers-linear") is the default;
   `Myers` ("myers") is the greedy forward variant; `Reference` is the O(NM)
   DP (pluggable-algorithms rule, docs/CONVENTIONS.md). Pinned by
   `scripts.json` keys and `ParseAlgorithm`.
2. **Grid bounds in Myers.** The paper's pseudo-code lets frontier values
   leave the edit graph. Ours computes only diagonals in [-min(d,m),
   min(d,n)] and takes a down move only from y < m and a right move only
   from x < n, storing -1 for unreachable diagonals, so the middle-snake
   overlap test compares valid points only. Minimality pinned by
   DIFF-INV-02 and DIFF-INV-20.
3. **Tie-breaking.** Down (insertion) wins ties between equally far moves,
   as in the paper; the linear variant also strips common prefixes and
   suffixes before each middle-snake search. Alignments are pinned per
   algorithm in `scripts.json`.
4. **Git's Myers is not reproduced.** xdiff's `xdl_recs_cmp` discards
   lines without a match in the other file and splits with its own
   heuristics, so among equally short scripts it can pick another
   alignment (63 of 400 random cases; the C-functions example). Only cases
   where both agree are in `unified_git.json`. Porting `xdl_split` and
   `xdl_cleanup_records` would close this.
5. **Patience/histogram follow git**, including no prefix/suffix trimming
   (git's `xdl_prepare_env` skips `xdl_optimize_ctxs` for them),
   histogram's 64-occurrence anchor limit and its fallback, and patience's
   "no common line → replace, no unique common line → classic diff". Git's
   classic-diff fallback is our capped linear Myers, so patience ranges that
   fall back can differ from git (54 of 400 random cases, all of them
   fallback ranges). Git's hash-bucket chain limit in `scanA` has no
   counterpart: records are keyed by exact token id.
6. **Git compaction without the indent heuristic.** `Options.GitCompact`
   ports `xdl_change_compact` without `XDF_INDENT_HEURISTIC` (on by default
   in git since 2.14), so outputs equal `git diff --no-indent-heuristic`.
7. **Caps.** A capped Myers or Reference result keeps the common prefix and
   suffix and replaces the middle. Patience and histogram apply the cap to
   their Myers fallback ranges only. A cap ≥ D changes nothing
   (DIFF-INV-15).
8. **Reference size limit.** N·M above 2^24 cells fails with
   `ErrInputTooLarge` instead of allocating; `BoundedDiff` treats that as an
   input-size fallback.

## Scripts and text

9. **Canonical form.** Within a change region the Delete precedes the
   Insert (the unified-diff order), so equal alignments compare equal.
10. **Line tokens keep their terminator**, so a last line without "\n" is a
    different token (git's behaviour) and renders with "\ No newline at end
    of file". "\r\n" stays inside the line.
11. **`SplitWords`** treats '_' as a word character (identifiers), runs of
    white space as one token and every other rune (and every invalid UTF-8
    byte) as its own token.
12. **Interner overflow** past 2^32−1 distinct strings returns `NoToken`
    and sets `Overflowed` instead of panicking (unreachable in memory).
13. **Hunk merging** follows git: changes separated by at most 2·context
    equal lines share a hunk.
14. **Unified parsing is strict**: lines before "--- "/"@@" are ignored, a
    section heading after "@@ ... @@" is ignored, but empty body lines, a
    no-newline marker before any hunk or after a line that already lacks
    its newline, bodies longer or shorter than the header, and a range
    starting at line 0 with a non-zero length are errors. `ApplyUnified`
    has no offset or fuzz search: a hunk applies exactly where its header
    says. Pinned by `unified_parse.json`.
15. **Semantic cleanup** follows diff-match-patch (its test vectors are in
    `cleanup.json`) with two changes: rounds repeat until a fixpoint, which
    makes the result idempotent (DIFF-INV-16), and `Score` sees the token
    runs on both sides of a boundary; `TextScorer` reproduces
    `diff_cleanupSemanticScore` from the last three and first four tokens.
    Letters and digits are Go's `unicode.IsLetter`/`IsDigit` where
    diff-match-patch uses its host language's `isalnum`.

## JSON

16. **Value is a recursive tagged tree**, not an arena: JSON values are
    data, not a DOM, and the shape maps to a Rust `enum Value` with
    `Vec<Value>` and `Vec<(String, Value)>` (insertion order kept).
17. **Numbers** keep their source text; `Equal` compares them by exact
    decimal value ("1", "1.0", "10e-1" are equal; no float rounding).
    `JSONDiff` therefore emits nothing for 1 → 1.0.
18. **Duplicate object keys are rejected** (RFC 8259 leaves them
    undefined; RFC 6902 A.13 requires rejecting a duplicated "op"). Nesting
    deeper than `MaxJSONDepth` (1000) is rejected.
19. **`JSONDiff` emits add, remove and replace only.** Move/copy detection
    is not done (it would need a matching of removed and added subtrees);
    `ApplyPatch` implements all six operations. Object keys are visited in
    bytewise order; in arrays the i-th deleted and inserted elements of a
    change region pair up and are diffed recursively when both are objects
    or both arrays. A capped array diff (`MaxArrayEditDistance`) replaces
    the array.
20. **Root operations.** `replace` and `add` at "" replace the document;
    `remove` of "" is an error (RFC 6902 does not define a document without
    a root).

## Trees and bounds

21. **Costs.** The zero `TreeCosts` means unit costs; negative costs are
    rejected. `MaxNodes` defaults to 2000 per tree (O(n1·n2) memory); a
    `Tree` whose links do not end (a cycle) hits the same limit.
22. **Mapping choice.** Backtracking prefers deletion, then insertion, then
    a match, so among optimal mappings the result is deterministic.
23. **Bounded diff sizes** are measured as the larger side in bytes; the
    hash level always fills both SHA-256 values and `Same`.
