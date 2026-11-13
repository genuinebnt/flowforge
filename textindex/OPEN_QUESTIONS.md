# textindex: open questions and decisions

Each item names the ambiguity in V2_DESIGN §7 (or the task brief), the
choice made, and the test or vector that pins it.

## Analyzer

1. **What is a word.** "Split on non-alphanumerics" would make `_` a
   delimiter and lose `user_id` as one token. A word is a maximal run of
   letters, marks, decimal digits and `_`; `_` only separates parts inside
   it. Other Unicode numbers (No, Nl: `²`, `Ⅻ`) are delimiters. Pinned by
   `conformance/textindex/analyzer`.
2. **Acronyms.** `HTTPServer` → `http`, `server` (the last capital of a
   run starts the next part). The heuristic splits `HTTPs` into `htt`,
   `ps`; accepted, as in most code tokenizers.
3. **Case folding.** "Simple Unicode case folding" is implemented as
   `toLower(toUpper(r))` with Go's single-rune tables (Unicode 15.0), not
   CaseFolding.txt's C+S mapping. It differs from C+S for a few scripts
   (Cherokee folds to lower case here) but is idempotent and identical for
   all case variants of a rune (TIDX-INV-11 checks every rune). PORT: Rust
   must use the same simple mappings (not `char::to_lowercase`, which is a
   full mapping, e.g. `İ` → `i̇`); the conformance vectors cover `İ`, `ı`,
   `ς`, `K`, `ǅ`, `ß`.
4. **Long tokens.** Tokens longer than 128 bytes (original span) are not
   emitted; a dropped part keeps its position so phrases never bridge it.
   A word whose compound is dropped is still one word for lowering.
5. **Field length.** BM25 *dl* is the number of positions (parts), not of
   tokens: a compound shares its first part's position (Lucene's
   `discountOverlaps`).
6. **Trigrams over bytes.** Following Cox, trigrams are byte triples of
   the raw text, with only ASCII letters folded. Byte trigrams keep the
   index independent of Unicode tables; non-ASCII case-insensitive regexes
   are handled on the query side by expanding fold orbits
   (`unicode.SimpleFold`). PORT: Rust's `regex-syntax` produces a different
   HIR; the trigram vectors pin the Go output and a port may emit an
   equivalent or weaker query (never a stronger one: TIDX-INV-03).

## Query language

7. **Term semantics.** A bare term `getUserById` must match `getuserbyid`
   and `GETUSERBYID` (single-part words) as well as `getUserById` and
   `get_user_by_id`. One-word text lowers to `term(compound) OR
   phrase(parts)`; several words lower to the phrase of all parts. Scores
   add up when both branches match. Known gap: a multi-word query whose
   word is itself a single-part compound in the query but multi-part in
   the document (`GETUSER x` against `getUser x`) does not match, because
   phrases are over parts and there is no token graph.
8. **Field values and URLs.** `word:` with an identifier word starts a
   field, so `https://x` is field `https`; quote URLs (`"https://x"`).
   The error is `EMPTY_REGEX` or `UNKNOWN_FIELD`, both with spans.
9. **Negation needs adjacency.** `- foo` is `EXPECTED_TERM` (not "NOT
   foo"), and `--foo` is `UNEXPECTED_TOKEN`; write `-(-foo)`.
10. **Precedence.** AND (explicit or implicit) binds tighter than OR; both
    are n-ary and flattened, so parentheses that do not change meaning
    disappear from the AST. Spans of a parenthesized clause include its
    parentheses.
11. **Time values.** `since:`/`until:` take `<1-9 digits><s|m|h|d|w>`,
    `YYYY-MM-DD` (UTC midnight) or `YYYY-MM-DDTHH:MM:SS[.f{1,3}]Z`. No
    months or years (their length varies). `until` is exclusive. A
    relative value is resolved against `SearchOptions.Now`, never the
    clock.
12. **Regex scope.** `/re/` searches every text field (a match in any
    one), `field:/re/` one text field. Regexes on keyword fields are
    `FIELD_TYPE`. Flags are `i`, `m`, `s` (RE2 inline flags); `\/` is an
    escaped slash, any other `\x` reaches RE2 unchanged. Regex clauses do
    not score (they add 0), like Lucene's constant-score queries with a
    zero boost.
13. **Regex without trigrams.** The brief says "rejected unless the
    filters narrow the candidate set below a cap". Filters are the root's
    positive keyword and since/until clauses (a regex nested elsewhere does
    not count them). The count is over live documents of the snapshot; at
    most `MaxRegexCandidates` (default 10,000) passes. The rule depends on
    the regex, not the regex strategy. Pinned by TIDX-INV-16.

## Ranking

14. **IDF.** Lucene's `ln(1 + (N - df + 0.5)/(df + 0.5))` (never
    negative), with the classic `(k1 + 1)` numerator that Lucene 8 dropped
    (it does not change order). Statistics (*N*, *df*, *avgdl*) are over
    the live documents of the whole snapshot, not per segment and not
    including tombstoned documents, so merges and segment layout never
    change scores (TIDX-INV-05). The cost is a count of live postings for
    terms of segments with deletions, cached per segment snapshot.
15. **Multi-field and phrase scores.** An unqualified term is the sum over
    text fields (schema order) of per-field BM25; a phrase uses the phrase
    frequency as *tf* and the sum of its terms' IDFs. Boolean scores are
    sums in clause order; negations and filters add 0.
16. **Recency tie-break.** Equal scores order by the time field
    descending, documents without it last, then by id ascending. Scores are
    compared exactly. PORT: Go's `math.Log` is not correctly rounded and
    nor is Rust's `ln`; scores may differ in the last bits across
    languages (conformance tolerance 1e-9 relative), but ties stay ties
    because equal inputs give equal outputs within one implementation.
17. **TF-IDF never beat BM25 here.** On the labelled set both reach MRR
    0.947; BM25 stays the default for its tf saturation and the design
    brief. The set is small (19 queries); see README "Choosing an
    algorithm".

## Index lifecycle

18. **Visibility.** All writes (add, update, delete) become visible
    together at `Refresh`, which is also the durable commit (Lucene's
    refresh and commit are merged). Unrefreshed writes are lost on Close.
19. **Add vs Update.** `Add` of a live or pending id is `ErrDuplicateID`;
    `Update` is an upsert; deleting an absent id is a no-op. Generations
    are per operation and stored per document; a merge keeps them.
20. **Merges rebuild.** A merge re-analyzes the live documents' stored
    text instead of merging postings. That makes the merged bytes equal to
    a fresh build (TIDX-INV-08) at the cost of merge throughput (about
    26k docs/s). A postings-level merge would need the analyzer to be
    reproducible from postings only; positions and trigrams make it a
    larger change.
21. **Merge policy.** Tiers are floor(log_F(live docs)); the first F
    segments (commit order) of the lowest full tier merge. Deletes are
    expunged only by merges; a fully deleted segment is dropped at Refresh.
22. **Self-check.** Every new segment is reopened with full validation
    before it is committed (it costs about a quarter of indexing time).
23. **zstd determinism.** Compressed bytes are reproducible with the
    pinned klauspost version and options, not across zstd
    implementations (FORMAT.md §1.9).
24. **Directory I/O takes no context.** Files are local and small; the
    `Directory` port can grow a context when a remote store appears.
