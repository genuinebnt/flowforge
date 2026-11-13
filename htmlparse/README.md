# htmlparse

A from-scratch WHATWG HTML parser for Flowforge v2 (layer 1; imports only
`core` and `golang.org/x/text`). It implements the HTML Living Standard
§13.2 (parsing), §13.3 (serialization), §13.4 (fragment parsing) and §13.5
(named character references) as of whatwg/html commit `c18a7c5`
(2026-11-12), including the 2025 changes: processing instructions
(`<?target data>`), the customizable `<select>` parser rules that removed
the "in select" / "in select in table" insertion modes, `<search>`, and
`selectedcontent` mirroring. It is written so the Rust port is a
translation of the same state machines, checked against the same data.

## Architecture

```
raw bytes ──Decode──▶ UTF-8 ──Tokenizer──▶ tokens ──treeBuilder──▶ arena Document ──▶ Render / Dump
          (encoding.go)    (preprocess.go +        (treebuilder.go,          (dom.go)       (serialize.go,
                            tokenizer*.go,          insertion_modes_*.go,                     dump.go)
                            charref.go)             adoption.go, foreign.go,
                                                    fragment.go)
```

* **Decoder** (`encoding.go`): BOM sniff → transport charset (a label or a
  whole Content-Type value) → meta prescan of the first 1024 bytes
  (§13.2.3.2, with the XML-declaration fallback) → UTF-8 autodetect →
  windows-1252. BOM/transport give confidence *certain*, the rest
  *tentative*. `ParseRaw` implements "change the encoding while parsing" as
  a second pass when a `<meta>` in head disagrees with a tentative guess.
  Labels resolve through `x/text/encoding/htmlindex` and are reported by
  their WHATWG canonical names (the names `encoding_rs` uses).
* **Preprocessing** (`preprocess.go`): done on the fly by `Tokenizer.next`
  so spans stay offsets into the original buffer: CR LF / CR → LF, WHATWG
  maximal-subpart U+FFFD for ill-formed UTF-8, and the
  surrogate / noncharacter / control-character input-stream errors
  (reported once per character, before the tokenizer error for it).
* **Tokenizer** (`tokenizer.go`, `tokenizer_states_text.go`,
  `tokenizer_states_tag.go`, `tokenizer_states_markup.go`, `charref.go`):
  an explicit `state` enum with one constant per spec state (84: 79 of the
  80 classic states of §13.2.5 — the decimal character reference start
  state is folded into the numeric character reference state, with
  identical behaviour — plus the five processing-instruction states),
  `stateFooBar` = spec "foo bar state", handler `fooBarState`; `stateNames`
  holds the spec names. `step` dispatches on the state; handlers that stay
  in their state loop, and the text states have byte fast paths
  (`asciiPlain`). Character data is accumulated and emitted as one
  coalesced token per run. Named references: longest match over the
  generated table (`entities_table.go`, from x/net/html's data with
  `internal/gen`; cross-checked identical to html5ever's 2231-entry
  table), with two selectable strategies (see below). Parse errors carry
  the spec names and source offsets.
* **Tree builder** (`treebuilder.go` + `insertion_modes_*.go`): one method
  per insertion mode (`inBodyMode`, ...) returning "reprocess?"; the
  dispatcher (`useForeignRules`) chooses between the mode and the foreign
  content rules (`foreign.go`). The stack of open elements caches tag atom,
  namespace and the annotation-xml integration-point flag per entry; scope
  checks (`isScopeBoundary`: default, list item, button, table — the
  current standard has no select scope, and `select` is now a default
  scope boundary) and `isSpecial` switch on them. The list of active
  formatting elements uses `id == None` markers, the Noah's Ark clause
  (3 per family) and reconstruction. `adoption.go` is the adoption agency
  algorithm (outer limit 8, inner limit 3). Foster parenting, template
  contents (separate DocumentFragment + stack of template insertion modes),
  quirks/limited-quirks from the DOCTYPE, implied end tags (normal and
  thorough), and the tokenizer controls (`SetState`, `SetAllowCDATA`,
  last start tag) are all explicit. The tree builder controls the
  tokenizer between tokens; the tokenizer returns right after any
  non-character token so a state switch takes effect on the next byte.
* **Arena DOM** (`dom.go`): `Document{Nodes []Node, Root, Mode}`; `Node`
  has `Kind`, namespace, name, attributes, data, doctype ids, the five
  `NodeID` links, `Content` (template contents) and `Span`. Mutation
  helpers keep links consistent and refuse cycles; traversal helpers
  (`Children`, `Descendants`, `Elements`, `Attr`, `TextContent`) are what
  the `content` package uses for extraction. Text coalescing appends to a
  per-node byte buffer during the parse (amortized O(1)) and materializes
  strings at the end.
* **Output**: `Render` is the §13.3 fragment serialization (innerHTML;
  `RenderNode` for outerHTML) with the current escaping rules (`&`, NBSP,
  `<`, `>` everywhere, `"` in attributes); `Dump` is the html5lib tree
  format used by the `.dat` files.

### Choices that mirror html5ever (for the port)

| Here | html5ever |
|---|---|
| `state` enum, names after the spec | `tokenizer::states::State` |
| `insertionMode` enum + one method per mode | `tree_builder::types::InsertionMode`, `rules.rs` |
| mode methods return `bool` reprocess; mode already switched | `ProcessResult::Reprocess(mode, token)` |
| character runs split into whitespace / NUL / other segments (`processCharacters`) | `ProcessResult::SplitWhitespace` |
| `tagID` atoms (`tags.go`) | `local_name!` atoms (web_atoms) |
| stack entries / AFE entries with markers | `open_elems`, `active_formatting` with `FormatEntry::Marker` |
| tokenizer driven by the tree builder between tokens | `TokenSinkResult` / `TokenizerResult` |

Differences: the arena replaces html5ever's `TreeSink` handles (index ids,
no `Rc`); character-reference states are states of the main enum (html5ever
has a separate `CharRefTokenizer`); this parser implements the 2025 rules
(processing instructions, the select changes, `<search>`) that html5ever's
current release still lacks, so html5ever is a design reference, not an
oracle, for those.

## Public API

```go
Decode(raw []byte, transportCharset string) ([]byte, Encoding, Confidence)
Sniff, DecodeAs, LookupEncoding, Prescan, SniffBOM
Tokenize(input []byte, opts TokenizeOptions) ([]Token, []ParseError)
NewTokenizer(input, opts) *Tokenizer  // Next(), SetState, SetAllowCDATA, SetLastStartTag
Parse(input []byte, opts Options) (*Document, []ParseError)
ParseRaw(raw []byte, transportCharset string, opts Options) (*Document, Encoding, []ParseError)
ParseFragment(input []byte, context ContextElement, opts Options) (*Document, []ParseError)
Render(*Document, NodeID) []byte; RenderWith; RenderNode
Dump(*Document) string; DumpNode
```

`Options`: `Scripting` (off by default), `MaxDepth` (512), `MaxAttributes`
(1024 per tag), `MaxInputSize` (256 MiB), `CharRefLookup`. Limits fail
closed with stable codes: an element beyond `MaxDepth` is inserted
childless and not opened (`nesting-depth-limit-exceeded`), extra attributes
are dropped (`attribute-limit-exceeded`), long input is truncated
(`input-size-limit-exceeded`). Error codes are the closed `ErrorCode` list
in `errors.go`: the 51 WHATWG tokenizer/input-stream names, plus Flowforge
names for tree-construction errors (the standard leaves those unnamed) and
limits.

## Conformance

| Suite | Result |
|---|---|
| html5lib tokenizer (`conformance/third_party/html5lib-tests/tokenizer`, 6824 tests × initial states = 7050 runs, incl. `lastStartTag`, `doubleEscaped`, xmlViolation) | tokens **7050/7050**; error codes **7050/7050**; error code+line+col 7034/7050 — the 16 misses are the `<?x x` EOF cases whose upstream positions contradict the state machine (OPEN_QUESTIONS Q-TOK-1) |
| tree construction (`conformance/third_party/wpt-html-parsing/*.dat`, 1973 cases) | **1967/1973**: documents 1725/1725, script-off 28/28, fragments 206/206, script-on 8/14 — the 6 `scripted_*.dat` cases need a JavaScript engine (Q-TREE-1) |

Both tests assert the known-failure lists exactly, so a newly passing or
newly failing case fails the test.

## Invariants (port these)

| Id | Test | Statement |
|---|---|---|
| HTML-INV-01 | `TestHTMLInv01_TokenSpansTileInput` | Token spans start at 0, are contiguous and the EOF token ends at len(input), in every initial state. |
| HTML-INV-02 | `TestHTMLInv02_ParsingIsTotal` | Tokenize, Parse and ParseFragment accept any bytes without panicking; Parse always yields an html document element. |
| HTML-INV-03 | `TestHTMLInv03_SerializeReparseFixpoint` | For round-trip-safe trees (`roundTripSafe`: well-formed generated documents and the safe subset of soup), Parse(Render(Parse(x))) dumps like Parse(x). |
| HTML-INV-04 | `TestHTMLInv04_LinearTimeAndAllocation` | Allocation count and bytes per input byte at 4n are within 20 % of n; time per byte within 50 % (skipped under -race/-short). |
| HTML-INV-05 | `TestHTMLInv05_ArenaIntegrity` | All links are in range and mutually consistent, sibling chains are acyclic, every node is reached exactly once from a parentless node. |
| HTML-INV-06 | `TestHTMLInv06_DepthLimit` | The stack of open elements never exceeds MaxDepth; exceeding it reports the error, keeps every element and keeps parsing. |
| HTML-INV-07 | `TestHTMLInv07_DecodeTokenizeEquivalence` | Valid UTF-8 decodes to itself with or without a BOM or utf-8 label, and tokenizes identically. |
| HTML-INV-08 | `TestHTMLInv08_ErrorsOrderedAndLocated` | Errors are sorted by offset, lie within the input, and Line/Col (UTF-16 columns, CR LF = one break) match the offset. |
| HTML-INV-09 | `TestHTMLInv09_AttributesUniqueAndLimited` | No element has duplicate (namespace, name) attributes; MaxAttributes caps each tag with an error. |
| HTML-INV-10 | `TestHTMLInv10_TextCoalesced` | The parser never leaves two adjacent Text siblings. |
| HTML-INV-11 | `TestHTMLInv11_NamedReferenceLongestMatch` | Both lookup strategies return the brute-force longest entity prefix; entities.json equals the table. |

Fuzz targets assert them too: `FuzzHTMLTokenizer` (01, 08, valid UTF-8
tokens), `FuzzHTMLParse` (05, 08, 09, 10, 03 on safe trees, fragments in 17
contexts), `FuzzHTMLDecode` (valid UTF-8 output, deterministic sniffing,
07). Seeds are every `.dat` input and tokenizer test input; crashers stay
in `testdata/fuzz/`.

## Differential oracle

`TestHTMLDiffOracle` compares `Dump` with a dump of golang.org/x/net/html
v0.62 (test-only) on all `.dat` inputs and on seeded tag soup (documents
and fragments in ten contexts). Every difference must belong to a
documented x/net deviation class (D1–D17, OPEN_QUESTIONS.md); on `.dat`
inputs x/net never matches the expectation where we do not. Typical run:
1856 `.dat` cases identical (all 103 differences are classes D1–D5), soup
≈ 99.7 % identical (the rest D8/D11/D12/D10).

## Performance comparison

`go test -run '^$' -bench HTMLAlgo -benchmem ./htmlparse/`
(`BenchmarkHTMLAlgo/charref/<trie|sorted|map>/<entities|urls|page-1MB>`,
`BenchmarkHTMLAlgo/{parse,tokenize}/{ours,xnethtml}/<size>`; generated
well-formed documents, seed 42). Measurements, the default and when to
pick each strategy: [docs/ALGORITHMS.md, "Named character references and
the parser"](../docs/ALGORITHMS.md). 2026-11-13, shared 4-core machine,
`-benchtime 2x`:

| Benchmark | ours MB/s | x/net/html MB/s | ours B/op | x/net B/op | ours allocs/op | x/net allocs/op |
|---|---|---|---|---|---|---|
| Parse 10 KB | 38 | 33 | 177 K | 109 K | 879 | 3 244 |
| Parse 1 MB | 44 | 40 | 17.2 M | 10.4 M | 84 653 | 322 839 |
| Parse 10 MB | 44 | 44 | 173 M | 104 M | 843 161 | 3 220 975 |
| Tokenize 10 KB | 86 | 54 | 23 K | 45 K | 764 | 2 663 |
| Tokenize 1 MB | 104 | 63 | 2.1 M | 4.0 M | 75 730 | 265 260 |
| Tokenize 10 MB | 107 | 66 | 21.3 M | 39.6 M | 755 438 | 2 646 220 |

| Named reference lookup (tokenizer MB/s) | entities | urls | page-1MB |
|---|---|---|---|
| `CharRefTrie` (default) | 90-93 | 147-159 | 89-100 |
| `CharRefSortedSearch` | 54-62 | 113-130 | 90-95 |
| `CharRefMap` | 130-132 | 112-116 | 90-105 |

What drives the differences:

* **Tokenizer allocation strategy.** Ours reuses scratch buffers for names,
  attribute values and text, emits one coalesced character token per text
  run, interns tag names to atoms and common attribute names to constants,
  and allocates one string per value; x/net materializes an `html.Token`
  per token (attribute slice, unescaped copies), about 3.5× the
  allocations. This makes our tokenizer 1.6× faster.
* **Arena vs pointer tree.** Our tree is one `[]Node` (pre-sized from the
  count of `<` bytes) instead of one heap object per node: 4× fewer
  allocations and less GC work, but each `Node` is a wide value struct
  (doctype fields, five links, span), so bytes allocated are ~1.7× x/net's.
  Shrinking `Node` (int32 spans, doctype ids out of line) is the obvious
  next step if memory matters more than API simplicity.
* **Character reference lookup.** The trie (`CharRefTrie`, ~100 KB of
  generated tables) does one binary search over a node's edges per byte
  and stops at the first byte no name continues with; the sorted search
  (`CharRefSortedSearch`, no extra tables) does two binary searches over a
  shrinking range of the 2231 names per byte; the map (`CharRefMap`)
  reads the alphanumeric run and probes every prefix length from the
  longest down. The map wins on entity-dense text, the trie on the
  `&param=` of query strings, where the map pays for every prefix of the
  parameter name; the trie stays the default. All three are checked
  against brute force by HTML-INV-11, and the html5lib suites run under
  each.

## Running

```
go test ./htmlparse/...                       # conformance, invariants, oracle
go test -race ./htmlparse/...
go test -run XXX -fuzz FuzzHTMLParse -fuzztime 30s ./htmlparse/
go test -run XXX -bench . -benchtime 3x ./htmlparse/
go generate ./htmlparse                       # regenerate entities_table.go + entities.json
FF_SEED=n / FF_PROPTEST_CASES=n               # reproduce / scale the randomized tests
```

## Known limitations

See OPEN_QUESTIONS.md: no script execution (6 scripted tree tests),
selectedcontent is reproduced as an end-of-parse DOM pass, declarative
shadow roots and the sanitizer hooks of the current standard are not
implemented (no `shadowrootmode` handling; the standard's fragment and
`DOMParser` defaults also disable it), and the 16 upstream tokenizer error
positions noted above.
