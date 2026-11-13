# cssparse

CSS Syntax Module Level 3 tokenizer and parser for Flowforge v2
(`docs/V2_DESIGN.md` §4.3). Layer 1: standard library and `core` only.

## What it implements

| Spec section | Code |
|---|---|
| §3.3 preprocessing (CR LF / CR / FF → LF, U+0000 and invalid UTF-8 → U+FFFD) | `tokenizer.cpAt`, done on the fly so spans stay in source bytes |
| §4.3.1–§4.3.14 tokenization | `tokenizer.go`: `tokState` enum, one state per "consume" algorithm |
| 2014 CR unicode-range and match tokens | `Options.LegacyTokens` (only for the vendored suite) |
| §5.3 / §5.4 parsing (2021 CR) | `parser.go`: `ParseStylesheet`, `ParseRuleList`, `ParseRule`, `ParseDeclarationList`, `ParseDeclaration`, `ParseComponentValue`, `ParseComponentValues` |
| current ED "parse a block's contents" (nesting) | `ParseBlockContentsSource`, `(*Document).ParseBlockContents` |
| §9 serialization (with the "insert /**/" table) | `Serialize` |
| URL references | `ExtractURLs`, `(*Document).URLs`: `url()` tokens and functions, `src()`, `@import`, `@namespace`, `image-set()`, `@font-face` `src` |

Trees are arenas: `Document.Nodes []Node`, children linked by `NodeID`
(`int32`, `-1` = none). Rule blocks are stored raw (as the suite's
representation expects) and parsed on demand with `ParseBlockContents`.
Component values with blocks are consumed with an explicit stack, never
recursion. Every result has a `Grade`: `complete`, or `partial` when
`MaxDepth` (64), `MaxInputBytes` (8 MiB) or `MaxURLs` (20,000) was hit.

## Conformance

* `conformance/third_party/css-parsing-tests/` — vendored from
  github.com/SimonSapin/css-parsing-tests (CC0, commit `203ce36`):
  `component_value_list`, `one_component_value`, `declaration_list`,
  `one_declaration`, `blocks_contents`, `rule_list`, `one_rule`,
  `stylesheet`. **All 149 cases pass** (`TestCSSConformanceUpstream`,
  run with `LegacyTokens`).
* `conformance/cssparse/*.json` — our cases for the current token set
  (no legacy tokens), URL extraction with contexts, limits and grades.

## Invariants (port these)

| Id | Test | Statement |
|---|---|---|
| CSS-INV-01 | `TestCSSInv01_TokenTiling` | Token spans are non-empty, contiguous, and cover the input exactly once. |
| CSS-INV-02 | `TestCSSInv02_Totality` | Every entry point accepts any byte string and yields a well-formed arena (parent/child links, ordered in-range spans); URL extraction never fails. |
| CSS-INV-03 | `TestCSSInv03_Reserialization` | Concatenating the token source spans reproduces the input byte for byte; `Serialize` (from values) re-tokenizes to the same kinds, flags, values and numbers. |
| CSS-INV-04 | `TestCSSInv04_LinearSteps`, `TestCSSInv04_LinearTime` | Tokenizer state-machine steps ≤ 4·n + 8; nodes ≤ (MaxDepth+2)·tokens; allocations and time at 4n within 4.4x / 1.5x of n. |
| CSS-INV-05 | `TestCSSInv05_LosslessComponentTree` | In a component-value parse without a depth hit, every non-comment token is exactly one node's token or one block's closing token. |
| CSS-INV-06 | `TestCSSInv06_DepthBound` | No block or function is nested deeper than MaxDepth (re-parsed blocks included); `depth_limit` is reported exactly when the true nesting exceeds it. |
| CSS-INV-07 | `TestCSSInv07_EveryURLTokenReported` | Every `<url-token>` of a stylesheet appears in `URLs()` with its span, in source order. |
| CSS-INV-08 | `TestCSSInv08_LimitsGradePartial` | A result is `partial` exactly when a limit was hit, never `complete` then. |

Fuzz targets: `FuzzCSSTokenize` (INV-01, 03, 04), `FuzzCSSParse`
(INV-02, 05, 06, 08).

## Rust mapping

The design follows servo's `cssparser` crate where the port benefits:
token kinds map onto `cssparser::Token` (`Ident`, `AtKeyword`, `Hash` /
`IDHash` ↔ `FlagHashID`, `QuotedString`, `UnquotedUrl`, `Delim`,
`Number { has_sign, value, int_value }` ↔ `Number` + `FlagInteger` + the
representation span, `Percentage`, `Dimension`, `WhiteSpace`, `Comment`,
`CDO`/`CDC`, `Function`, the bracket tokens, `BadUrl`, `BadString`), and
`IncludeMatch` … `SubstringMatch` exist in both (ours behind
`LegacyTokens`). Unlike cssparser (which hands out nested parsers over a
lazily tokenized input), we tokenize once into a `Vec<Token>` and build
an arena, which is simpler to port and gives the tiling invariant.
Values are `String`s in Rust where Go uses substrings of one copy.

## Running

```
go test ./cssparse/
go test ./cssparse/ -run XXX -fuzz FuzzCSSTokenize -fuzztime 20s
go test ./cssparse/ -run XXX -bench . -benchmem
```

Benchmarks on the shared CI-class Xeon (4 vCPU, noisy): tokenize
55–78 MB/s, `ParseStylesheet` 30–37 MB/s, `ExtractURLs` 18 MB/s, linear
between 64 KiB and 256 KiB.

## Limitations

See `OPEN_QUESTIONS.md`: declaration values keep surrounding whitespace
as the vendored suite expects, `stylesheet_bytes` (encoding detection)
is not covered, the ident-code-point definition is the 2021 CR one.
