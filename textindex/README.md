# textindex

Full-text search over Flowforge's change history (V2_DESIGN §7): an
inverted index with positions plus a trigram index, in immutable "FFTX"
segments, with its own query language, BM25 ranking and regex search via
a trigram prefilter. Layer 1: standard library and
`github.com/klauspost/compress/zstd` only. Byte formats are in
[FORMAT.md](FORMAT.md); decisions and spec ambiguities in
[OPEN_QUESTIONS.md](OPEN_QUESTIONS.md).

References: Manning, Raghavan, Schütze, *Introduction to Information
Retrieval* (IIR) ch. 1–6 (postings, skips, compression, scoring);
Robertson and Zaragoza, *The Probabilistic Relevance Framework: BM25 and
Beyond* (2009); Russ Cox, *Regular Expression Matching with a Trigram
Index* (2012) and google/codesearch `index/regexp.go`; Lucene's segment,
tombstone and merge-policy design.

## API at a glance

```go
ix, _ := textindex.Open(textindex.Config{Schema: textindex.DefaultSchema(), Dir: dir})
_ = ix.Add(textindex.Document{ID: 42,
    Text:    []textindex.FieldValue{{"text", "+ fetch('/api/v2/users/' + userId)"}, {"url", "https://shop.example/app.js"}},
    Keyword: []textindex.FieldValue{{"kind", "endpoint_added"}, {"side", "added"}, {"program", "p1"}},
    Numeric: []textindex.NumericValue{{"at", 1790000000000}}})
_ = ix.Update(doc)    // delete + add with a newer generation
_ = ix.Delete(7)
_ = ix.Refresh()      // writes become visible and durable together
res, err := ix.Search(`kind:endpoint_added since:7d "api v2" -side:removed`, textindex.SearchOptions{K: 10, Now: nowMs})
fused := textindex.Hybrid(res.Hits, vectorIDs, 0, 10) // RRF, k = 60
```

* `Index` (segmented) and `ScanIndex` (brute force) implement `Searcher`;
  `NewSearcher(cfg)` picks one by `Config.Engine`.
* `Parse`, `Query.String` (canonical print), `Query.JSON`; `Analyze`,
  `FieldLength`, `Trigrams`; `CompileRegex`, `RegexpTrigramQuery`,
  `TrigramQuery`; `EncodeSegment`, `OpenSegment`, `Segment.Documents`;
  `BM25`, `BM25IDF`; `RRF`, `Hybrid`.
* `Directory` is the storage port (`MemDirectory`, `FSDirectory`).
* Errors: sentinels (`ErrCorrupt`, `ErrChecksum`, `ErrDuplicateID`, …)
  and `*QueryError{Code, Span}` wrapping `ErrQuery`.

## Architecture

```
Add/Update/Delete ──> pending map (id -> doc | delete, generation)
Refresh: tombstone superseded versions (FFTD sidecars, copy-on-write bitmaps)
         buffered docs (sorted by id) ──analyzer──> one FFTX segment
         self-check (full validation) ──> COMMIT swapped atomically ──> new snapshot
         size-tiered merge: rebuild live docs of F same-tier segments
Search:  lexer ─> parser ─> AST ─> lowering (schema, analysis, regex, now) ─> plan
         stats over live docs of the snapshot (N, df, avgdl)
         per segment: candidates (postings, trigrams, doc-values) ─> eval (positions,
         regexp on stored text, filters) ─> score ─> top-k heap ─> merged ranking
```

Documents are ordered by external id inside a segment, so segment bytes do
not depend on insertion order and lookups by id are binary searches. A
document has a generation (the sequence number of its operation); an
update tombstones the older version and adds the newer. Searches read an
immutable snapshot (segments + tombstone bitmaps) and never block writers.

## Analyzer rules

The analyzer (`analyzer.go`) is one state machine over runes with states
`stDelimiter`, `stConnector`, `stUpper`, `stUpperRun`, `stLower`,
`stDigit`.

1. Classes: upper (Lu, Lt), lower (other letters and marks), digit (Nd),
   connector (`_`), delimiter (everything else, including invalid UTF-8).
2. A **word** is a maximal run of non-delimiters. Inside it **parts** are
   split at `_`, at lower→upper (`getUser`), before the last capital of a
   capital run followed by a lower-case letter (`HTTPServer` → `HTTP`,
   `Server`), and at letter↔digit (`v2`, `x1y2`).
3. Each word emits its **compound** (whole word, kind `compound`) at the
   position of its first part, then each part at consecutive positions. A
   word that is one part with no `_` emits a single `word` token.
4. Terms are case-folded rune by rune: `toLower(toUpper(r))` with the
   simple Unicode mappings. No stemming, no stop words.
5. Tokens longer than 128 bytes are not emitted (a dropped part keeps its
   position). The field length is the number of positions.
6. Trigrams are every three consecutive **bytes** of the raw text with
   ASCII letters folded (`Trigrams`), as in Cox's code search.

`getUserById(user_id)` → `getuserbyid@0 get@0 user@1 by@2 id@3 user_id@4
user@4 id@5`.

## Query language

```ebnf
query      = ws, or_expr, ws ;
or_expr    = and_expr, { ws, "OR", ws, and_expr } ;
and_expr   = unary, { ws, [ "AND", ws ], unary } ;          (* implicit AND *)
unary      = [ "-" ], primary ;                             (* "-" adjacent to primary *)
primary    = "(", ws, or_expr, ws, ")" | field_expr | term | phrase | regex ;
field_expr = field, ":", ( value_word | phrase | regex ) ;  (* no space after ":" *)
field      = ident ;   ident = ( letter | "_" ), { letter | digit | "_" } ;
term       = word - ( "AND" | "OR" ) ;
word       = word_start, { word_byte } ;
word_start = ? any byte except space, "(", ")", '"', "/", "-" ? ;
word_byte  = ? any byte except space, "(", ")", '"' ? ;    (* a word "ident:" is a field *)
value_word = ? one or more word_byte, may start with "-" ? ;
phrase     = '"', { ? any byte except '"' and "\" ? | "\", ? any byte ? }, '"' ;
regex      = "/", { ? any byte except "/" and "\" ? | "\", ? any byte ? }, "/", { "i" | "m" | "s" } ;
ws         = { " " | "\t" | "\n" | "\r" | "\f" | "\v" } ;
```

* `since:<t>` / `until:<t>` are reserved fields: `t` is `<1-9 digits><s|m|h|d|w>`
  (relative to `SearchOptions.Now`), `YYYY-MM-DD`, or
  `YYYY-MM-DDTHH:MM:SS[.fff]Z`; `since` is `>=`, `until` is `<`.
* In a regex `\/` is a literal `/`; every other `\x` reaches RE2 as is.
  Flags are canonicalized to the order `ims`. Patterns are RE2 syntax.
* AND binds tighter than OR; same-operator nesting is flattened; a
  parenthesized group's span includes its parentheses.
* Limits: 8 KiB query, 64 nested groups/negations, 1,024 clauses, 1 KiB
  regex.
* `Query.String` prints the canonical form (AND as a space, OR in
  parentheses inside AND, negated groups parenthesized, phrases and
  regexes re-escaped); `Parse(q.String())` gives the same AST.

### Errors

| Code | When | Span |
|---|---|---|
| `EMPTY_QUERY` | no clause | whole query |
| `QUERY_TOO_LONG` | > 8,192 bytes | from byte 8,192 |
| `INVALID_UTF8` | invalid UTF-8 | first bad byte |
| `UNEXPECTED_TOKEN` | unmatched `)`, `--x`, junk after a group | the token |
| `UNBALANCED_PAREN` | `(` never closed | `(` to end |
| `EXPECTED_TERM` | missing operand after `-`, `AND`, `OR`, `(` | the token found |
| `EXPECTED_VALUE` | `field:` not followed directly by a value | the position |
| `UNTERMINATED_PHRASE` / `UNTERMINATED_REGEX` | no closing quote / slash | opener to end |
| `EMPTY_REGEX` | `//` | the regex |
| `BAD_REGEX_FLAG` | flag other than `i`, `m`, `s`, or repeated | the flag |
| `BAD_REGEX` | RE2 syntax error or > 1 KiB | the regex |
| `BAD_TIME` | bad since/until value | the value |
| `NESTING_TOO_DEEP` / `TOO_MANY_CLAUSES` | limits | the offending token |
| `UNKNOWN_FIELD` | field not in the schema (lowering) | the field name |
| `FIELD_TYPE` | regex on a keyword field, numeric/stored field, no time field | the clause |
| `REGEX_NO_TRIGRAMS` | regex without trigrams and filters do not cap candidates | the regex |

### Query semantics

Lowering (`plan.go`) turns the AST into a plan both engines execute:

* **Text** (term or phrase, unqualified = every text field, `field:` = one
  text field): analyze the value. One word → `term(compound) OR seq(parts)`
  (just the term for a single-part word); several words → `seq` of all
  parts with their relative positions; `seq` of one part is a term, of more
  a phrase. No tokens → matches nothing.
* **Keyword field** (`kind:endpoint_added`, `side:"added"`): exact,
  case-sensitive equality on the doc-values column.
* **Regex**: RE2 search (unanchored) over each text field's stored text;
  matches if any field matches; scores 0.
* **Scores**: a term in field *f* scores `BM25(idf_f, tf, dl, avgdl_f)`;
  unqualified terms sum over text fields in schema order; a phrase uses
  the phrase frequency and the sum of its terms' IDFs; AND and OR sum the
  scores of matching children in clause order; NOT, filters and regexes
  add 0. *N*, *df* and *avgdl* are computed over the live documents of the
  whole snapshot, so results never depend on segmentation.
* **BM25**: `idf · tf·(k1+1) / (tf + k1·(1 − b + b·dl/avgdl))`, k1 = 1.2,
  b = 0.75, `idf = ln(1 + (N − df + 0.5)/(df + 0.5))` (Lucene).
* **Order**: score descending, then time field (`at`) descending
  (documents without one last), then id ascending; top-k by a bounded
  heap.

### Regex queries

`CompileRegex` parses with `regexp/syntax` (Perl flags), simplifies, and
runs Cox's analysis (exact/prefix/suffix string sets, `maxExact` 7,
`maxSet` 20) into a canonical `TrigramQuery` (sorted, flattened, deduped).
Changes from codesearch: strings become trigrams with ASCII folding (the
index folds ASCII), and a literal or class containing U+FFFD is "any
character" because RE2 matches invalid bytes as U+FFFD. Candidates come
from the trigram postings and are verified by the full regexp. A regex
whose query is `ALL` is allowed only when the root's positive keyword and
since/until filters select at most `MaxRegexCandidates` live documents.

## Invariants (port these)

| Id | Test | Statement |
|---|---|---|
| TIDX-INV-01 | `TestTIdxInv01_SearchMatchesBruteForce` | Hits, scores (bit-exact), order, totals and error codes equal ScanIndex for random corpora (up to 1,500 docs, skip blocks included) and random queries (terms, phrases, regexes, filters, negation, nesting). |
| TIDX-INV-02 | `TestTIdxInv02_OpsMatchBruteForce` | The same after random interleavings of add, update, delete, refresh, merge, force-merge and reopen; `Get` agrees. |
| TIDX-INV-03 | `TestTIdxInv03_RegexPrefilterNeverDrops` | If a regexp matches a text, the text's trigrams satisfy its trigram query; index regex totals equal a brute-force regexp scan. |
| TIDX-INV-04 | `TestTIdxInv04_BM25MatchesTextbook` | Single-term scores equal an independent textbook BM25 over live docs (1e-12 relative); BM25 is increasing and saturating in tf. |
| TIDX-INV-05 | `TestTIdxInv05_MergePreservesResults` | Policy and forced merges change no result; a full merge leaves no tombstones. |
| TIDX-INV-06 | `TestTIdxInv06_SegmentRoundTrip` | encode → open → decode is the identity on normalized documents and generations, for every codec; every analyzed term is in the dictionary. |
| TIDX-INV-07 | `TestTIdxInv07_CorruptionDetected` | Every single-byte change (three masks) and truncation of a segment (every codec), tombstone file or commit point is rejected. |
| TIDX-INV-08 | `TestTIdxInv08_SegmentDeterminism` | Segment bytes do not depend on input order; merging any split of a document set gives the bytes of encoding it directly. |
| TIDX-INV-09 | `TestTIdxInv09_CompoundTokensRetrievable` | Searching for any emitted compound or word token finds its document. |
| TIDX-INV-10 | `TestTIdxInv10_AnalyzerTokenInvariants` | Token spans are in bounds on rune boundaries, `Term = fold(span)`, positions are non-decreasing and below the field length, parts strictly increase and contain no delimiter, a compound shares its first part's position. |
| TIDX-INV-11 | `TestTIdxInv11_CaseInsensitive` | Folding is idempotent and equal for all case variants of every rune; random case variants of a document's words find it. |
| TIDX-INV-12 | `TestTIdxInv12_PrintParseIdentity` | `Parse(Print(Parse(q)))` equals `Parse(q)` up to spans and printing is a fixpoint. |
| TIDX-INV-13 | `TestTIdxInv13_ErrorSpansInsideQuery` | Every parse, lowering and execution error is a `*QueryError` with a listed code and a span inside the query. |
| TIDX-INV-14 | `TestTIdxInv14_VisibilityAndDurability` | Writes are invisible until Refresh; reopening a memory or file-system directory reproduces every result. |
| TIDX-INV-15 | `TestTIdxInv15_SkipsAgreeWithScan` | Postings `advance` over skip entries equals a linear scan; leapfrog intersection equals set intersection. |
| TIDX-INV-16 | `TestTIdxInv16_RegexWithoutTrigramsRule` | A trigram-less regex is rejected unless the root filters select at most the cap; then it is answered exactly. |
| TIDX-INV-17 | `TestTIdxInv17_RRFMatchesVecindex` | RRF reproduces `conformance/vecindex/rrf` (constant 60, 1-based ranks, first occurrence, ties by id). |
| TIDX-INV-18 | `TestTIdxInv18_AlgorithmsInterchangeable` | Every codec × intersector × regex strategy equals ScanIndex; with TF-IDF both engines still agree. |

Fuzz targets: `FuzzQueryParse` (INV-12, -13), `FuzzSegmentOpen` (CRCs
resealed so mutations reach structural validation; accepted segments
decode and search), `FuzzAnalyzer` (INV-10), `FuzzIndexOps` (byte string →
operation sequence checked against ScanIndex, INV-02).

## Conformance data

`conformance/textindex/`:

* `analyzer/vectors.json`: input (or `input_hex`) → tokens (term, pos,
  start, end, kind), field length, distinct trigrams.
* `query_parse/vectors.json`: query → AST JSON with spans and canonical
  print, or error code and span (curated plus generated and mutated).
* `trigram_query/vectors.json`: regex and flags → canonical trigram query.
* `search/{curated,generated}.json`: documents, `now`, regex cap, and
  queries with expected totals and ranked hits (id, score, at) from the
  brute-force reference; scores compared at 1e-9 relative. The test runs
  them on one segment and on several.
* `ranking/labelled.json`: hand-labelled relevance set for scorer quality.

Regenerate with `FF_UPDATE_CONFORMANCE=1 go test ./textindex -run
Conformance` and review the diff (analyzer, parse and trigram vectors come
from this implementation; search expectations from ScanIndex).

## Choosing an algorithm

All candidates are in non-test code behind one interface, selected by a
`Config` field; the default is the chosen one. Side-by-side benchmark:

```sh
go test -run '^$' -bench TIdxAlgo -benchmem ./textindex
```

Measured 2026-11-13 on the 4-vCPU Xeon development VM, shared with other
builds (load average 4–18), so compare rows within a table, not absolute
values. TIDX-INV-18 checks that every non-scoring choice returns identical
results.

### Posting codec — `PostingCodec`, `Config.Codec` (`codec/<codec>`)

20,000-document segment of the benchmark corpus; bytes cover term and
trigram postings with skip tables; decode iterates every term list; AND is
`get ∧ user`.

| Codec | B/posting | decode ns/posting | AND µs |
|---|---:|---:|---:|
| `plain` (u32) | 4.82 | 11.1 | 296 |
| **`varint`** (LEB128, default) | **1.35** | 14.3 | 396 |
| `groupvarint` | 1.65 | 15.5 | 420 |

Default **varint**: 3.6× smaller than plain for 30 % more decode time,
and postings are most of the index. Group-varint is larger here (its
control byte is pure overhead when most deltas fit one byte) and our
scalar decoder is not faster than LEB128; it pays off only with
table-driven or SIMD decoding and larger deltas. Pick `plain` when the
index is memory-resident, small and latency-bound.

### Intersection — `Intersector`, `Config.Intersector` (`intersect/<algo>/1:<r>`)

200,000 docs; list A has 100,000 postings, list B is *r* times shorter.

| Algorithm | 1:1 | 1:10 | 1:100 | 1:1000 |
|---|---:|---:|---:|---:|
| **`skip`** (leapfrog over skip entries, default) | 3.05 ms | 1.12 ms | 0.61 ms | 0.089 ms |
| `gallop` (materialize + exponential search) | 4.26 ms | 1.73 ms | 1.32 ms | 1.28 ms |
| `linear` (materialize + two-pointer merge) | 4.42 ms | 2.06 ms | 2.95 ms | 4.85 ms |

Default **skip**: it never decodes skipped 128-posting blocks, so cost
follows the shorter list (14× faster than galloping at 1:1000) and it
allocates nothing. Galloping and linear merge must decode both lists
first; galloping is the right choice for lists already in memory (e.g. a
cached filter), linear merge only for equal-sized in-memory lists.

### Ranking — `Scorer`, `Config.Scorer` (`rank/<scorer>`)

| Scorer | MRR | P@1 | term/phrase p50 on 100k docs |
|---|---:|---:|---:|
| **`bm25`** (default) | 0.947 | 0.895 | 6.5 ms |
| `tfidf` (Lucene classic) | 0.947 | 0.895 | 6.3 ms |

On the 19-query labelled set the two tie (they fail on the same two
queries, where a long route table outranks the focused hunk). BM25 stays
the default: the design brief specifies it, its tf saturation bounds what
repetitive minified code can gain, and its length normalization is tunable
(b). The speed difference is noise. TF-IDF is there as a baseline and for
corpora of uniformly short documents, where saturation matters little.

### Regex — `RegexStrategy`, `Config.Regex` (`regex/<strategy>`)

| Strategy | mean | p50 | (4 regexes, 100k docs) |
|---|---:|---:|---|
| **`trigram`** (Cox prefilter, default) | 83 ms | 68 ms | |
| `fullscan` (verify every admitted doc) | 349 ms | 143 ms | |

Default **trigram**: 4× faster on average, more for selective literals.
The gap closes for patterns whose trigrams are frequent (`\.js`), where
most documents are candidates anyway. Full scan is the better choice for
tiny segments and when filters already select few documents. Both
strategies apply the same `REGEX_NO_TRIGRAMS` rule.

### Engine — `Searcher`, `Config.Engine`

`ScanIndex` (brute force) re-analyzes every document per query: exact,
trivially correct, no disk format; for corpora of a few hundred documents
and as the test oracle. `Index` (default) for everything else.

## Benchmarks

100,000 synthetic diff hunks (2–7 lines of JavaScript-like code with
API paths, URLs, hashes; 349 B of raw text per doc), DefaultSchema, one
run each, same machine (load average about 18 during the run):

| Benchmark | Result |
|---|---|
| `BenchmarkIndexing` (refresh every 10k, merge factor 10 → 1 segment) | 11,100 docs/s (13,300 on a quieter run); 770 index B/doc; index/raw 2.21 |
| `BenchmarkQuery_Term` (8 terms, ~23k matches each) | p50 1.7 ms, 53–78 allocs |
| `BenchmarkQuery_Phrase` | p50 6.4–10.9 ms |
| `BenchmarkQuery_Regex` (5 regexes, one matching `\.js` in every doc) | p50 110–151 ms |
| `BenchmarkQuery_Filtered` (keyword + since + term/regex) | p50 2.4–3.0 ms |
| `BenchmarkMerge` (10 × 10k docs, 10 % deleted → 1) | 15,000–26,500 docs/s |

Index composition for a 10,000-doc segment (B/doc): trigrams 297, stored
(zstd) 140, postings 134, term dictionary 106, positions 79, keywords 24,
ids 16, norms 8, numerics 8. The trigram index dominates for small
documents (each hunk has about 300 distinct trigrams); see limitations.

Acceptance shape: query cost is linear in the number of matches (term and
phrase queries score every match; no WAND), regex cost in the number of
candidates verified.

## Tests

```sh
go test ./textindex/...                       # invariants, conformance, rank quality
go test -race -short ./textindex/...
FF_SEED=123 FF_PROPTEST_CASES=10000 go test -run TIdxInv ./textindex/
go test ./textindex -run '^$' -fuzz FuzzIndexOps -fuzztime 60s -fuzzminimizetime 2s
go test ./textindex -run '^$' -bench . -benchtime 1x
```

## Known limitations and future work

* Merges rebuild from stored text (simple and byte-deterministic, but
  merge throughput equals indexing throughput); no postings-level merge.
* No dynamic pruning (WAND/MaxScore): every match is scored.
* Trigram postings are about 40 % of the index; block-packed (PFOR)
  trigram postings or per-field trigram sets would shrink them.
* Keyword fields are single-valued; regex on keyword fields is not
  supported.
* No token graph: a multi-word query matches documents only through
  parts (OPEN_QUESTIONS 7).
* Writes are visible only after Refresh, which is also the commit; there
  is no separate near-real-time reader.
