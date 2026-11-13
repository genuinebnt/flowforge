# jsparse

ECMAScript 2024 lexer and parser (ECMA-262 15th edition, clauses 12–16 and
Annex B) producing an arena AST with ESTree node kinds, a canonical ESTree
JSON writer, a re-parseable printer and extraction helpers for `content`.
Layer 1: standard library only (V2_DESIGN §4.2).

```go
prog, err := jsparse.Parse(src, jsparse.Options{SourceType: jsparse.SourceModule})
// strict mode: err is *jsparse.Error with the first diagnostic (errors.Is(err, jsparse.ErrSyntax))
prog, _ = jsparse.Parse(src, jsparse.Options{Tolerant: true, Tokens: true})
// tolerant mode: never an error; prog.Diagnostics, best-effort tree, prog.Tokens tiles the input
canonical := prog.JSON(jsparse.JSONOptions{})            // sorted keys, no positions
code := jsparse.Print(prog, jsparse.PrintOptions{Minify: true})
uses := prog.StringLiterals()   // values + callee path ("fetch", "axios.get", "XMLHttpRequest.open")
refs := prog.ModuleRefs()       // import/export sources and import() specifiers
url, _, ok := prog.SourceMappingURL()
tables := prog.LiteralTables(3, 1000) // outermost object/array literals, flattened (route tables)
```

`NewLexer(src, module).Next(goal)` exposes the lexer; `Tokenize` lexes
without a parser (InputElementDiv only).

## Architecture

| File | Concern |
|---|---|
| `tokens.go` | token kinds, flags, keywords, the four lexical goals |
| `lexer.go`, `lexer_states.go` | the input-element state machine (`state*` constants named after the clause-12 productions), trivia, rewind |
| `unicode.go` | WhiteSpace, LineTerminator, ID_Start/ID_Continue (UAX #31 derivation from `unicode`) |
| `numbers.go` | NumericLiteral machine: separators, BigInt, hex/octal/binary, legacy octal, NonOctalDecimal |
| `strings.go`, `templates.go` | escapes, line continuations, cooked/raw template values |
| `regexp.go` | RegularExpressionLiteral scanning, flag and pattern validation (22.2.1, B.1.2) |
| `parser.go` | Options, token handling, halting, node arena |
| `statements.go`, `expressions.go`, `functions.go`, `classes.go`, `patterns.go`, `modules.go` | the syntactic grammar |
| `asi.go` | automatic semicolon insertion and tolerant-mode resynchronization |
| `early_errors.go` | function contexts ([Yield]/[Await]/[Return]), scopes, labels, private names, exports, cover-grammar errors |
| `ast.go`, `astjson.go` | arena, schema table, canonical JSON |
| `printer.go` | readable and minified printer |
| `extract.go` | extraction helpers |
| `alternatives.go` | measured algorithm candidates (`Options.Algorithms`) |
| `errors.go` | closed list of diagnostic codes |

### Goal-selection protocol

The lexer never decides between regular expression and division, or
between `}` and a template continuation. Every token is produced by
`Lexer.Next(goal)`, and the parser owns the goal:

1. By default the parser requests the next token under
   **InputElementDiv** (`parser.next`, `parser.peek`). Peeks happen only
   after identifiers and keywords that are not operators, where Div is the
   grammar's goal.
2. Where the grammar admits a RegularExpressionLiteral (the start of a
   PrimaryExpression), the parser re-requests the current token under
   **InputElementRegExp** (or **InputElementRegExpOrTemplateTail** inside a
   template substitution) with `regoalRegExp`.
3. When a template substitution's expression ends at `}`, the parser
   re-requests that token under **InputElementTemplateTail**
   (`regoalTemplateTail`), which yields TemplateMiddle or TemplateTail.

The four goals differ only for tokens beginning with `/` or `}`, so
re-requesting exactly those tokens at exactly those positions is the
grammar's goal choice, made before the parser looks at the token. A
re-request rewinds the lexer to the token start, drops the one-token
lookahead and any trivia recorded past that point. Every token records the
goal it was lexed under (`Token.Goal`); JS-INV-05 checks the decision on a
hand-written table of ambiguous contexts and on every `/` of the generated
corpus. This mirrors oxc's `re_lex_*` entry points (the parser asks the
lexer to re-read `/`, `/=` and `}`) rather than acorn's token-context
heuristic, which misreads for example `class A extends /re/ {}`.

### AST layout

`Program{Nodes []Node, Children []NodeID, Strings []string, Root, SourceType, Source, Diagnostics, Comments, Tokens}`.
A `Node` is 64 bytes and pointer-free:

```
Kind uint8 | Op uint8 | Flags uint32 | Span{Start,End int32}
A, B, C, D NodeID (int32, -1 = none) | List, List2 NodeList{Start,Len int32} into Children
Str, Str2 StrID (uint32 into Strings) | Num float64
```

Which slot is which ESTree field is defined once, by the `schemas` table
in `astjson.go` (for example `ForStatement: init=A, test=B, update=C,
body=D`; `TemplateLiteral: quasis=List, expressions=List2`). The JSON
writer, `ChildIDs`/`Walk`, the printer and the arena-integrity invariant
all read that table, and the Rust port declares the same table. `Op` holds
operators and the sub-kinds of declarations, properties, methods and
literals. `NParenthesized` and `NTrailingComma` are parser-internal flags.
Lists are built on a scratch stack and copied into `Children` when they
end, so nested lists never interleave. Parentheses are not nodes (acorn's
default); cover-grammar reinterpretation changes node kinds in place
(`ObjectExpression` → `ObjectPattern`, `AssignmentExpression` →
`AssignmentPattern`, `SpreadElement` → `RestElement`).

oxc mapping for the port: the token `Kind` enum plus `regoal*` correspond
to oxc's `Kind` and `re_lex_*`; `fnCtx` corresponds to oxc's `Context`
flags ([In]/[Yield]/[Await]/[Return]); the cover conversions in
`patterns.go` correspond to oxc's `CoverGrammar` trait. The difference is
the arena: oxc allocates boxed nodes in a bump arena, while this package
uses index ids into a `Vec<Node>`-shaped slice as the portability rules
require. Early errors that oxc checks in `oxc_semantic` (redeclarations,
labels, private names) are checked during parsing here.

### Strict and tolerant mode

Errors never unwind with panics. A fatal error *halts* the parser: the
current token becomes EOF, so every loop ends and every production returns
a placeholder. Strict mode then returns the first diagnostic. Tolerant
mode re-lexes from the error token in the nearest statement list and skips
to a statement boundary (`asi.go`, OPEN_QUESTIONS §9); early errors and
lexical errors on usable tokens are recorded without halting. Diagnostic
codes are a closed list with stable names (`Code.String`, e.g.
`unexpected-token`, `redeclaration`), grouped by `Code.Class` into syntax,
early and limit.

## Invariants (port these)

| Id | Test | Statement |
|---|---|---|
| JS-INV-01 | `TestJSInv01_TokenTiling` | With `Options.Tokens`, tokens and trivia tile the input exactly once, in order, for every valid input and, in tolerant mode, for every input. |
| JS-INV-02 | `TestJSInv02_Totality`, `FuzzJSParse` | Tolerant mode returns a program for any byte string and never panics; strict mode returns an error or a program; diagnostics have known codes and in-range spans. |
| JS-INV-03 | `TestJSInv03_PrintReparse`, `TestJSInv03_PrintReparseGenerated`, `FuzzJSPrintReparse` | `Parse(Print(p))` has the same canonical JSON as `p`, in readable and minified layouts (all pass files, generated programs, and the whole positive test262 corpus in `TestJSTest262Full`). |
| JS-INV-04 | `TestJSInv04_LinearTime` | Parsing a 4n-byte bundle costs at most 1.6 × 4 × the time and allocated bytes of an n-byte bundle (best of five). |
| JS-INV-05 | `TestJSInv05_LexicalGoals` | Every `/`-initial token is a RegularExpressionLiteral exactly where the grammar admits one (hand-written ambiguous contexts and the generator's recorded answers); regex tokens are lexed under a RegExp goal. |
| JS-INV-06 | `TestJSInv06_ArenaIntegrity` | Child ids are in range and not self; list ranges are inside `Children`; holes only in array elements; no node has two parents; child spans lie within parent spans; in successful parses every node is reachable from the root. |
| JS-INV-07 | `TestJSInv07_TolerantAgrees` | On valid input tolerant mode gives the strict tree and no diagnostics. |
| JS-INV-08 | `TestJSInv07_TolerantAgrees`, `TestJSInv02_Totality` | On invalid input the first tolerant diagnostic equals the strict-mode error. |
| JS-INV-09 | `TestJSInv09_CanonicalJSON` | The canonical JSON is valid JSON with strictly sorted object keys and is identical across parses. |
| JS-INV-10 | `TestJSInv10_LexerTiling`, `FuzzJSLexer` | The lexer alone, under any goal sequence, terminates and tiles the input with tokens and trivia. |

The invariants run on the vendored pass files plus programs from the
grammar-based generator (`gen_test.go`, seeded through `testkit.Rng`, so
`FF_SEED` reproduces a failure). The generator covers declarations,
classes (fields, private names, accessors, static blocks), every
statement, modules (imports, exports, attributes) and every expression
form, avoids all early errors by construction, inserts random white space,
comments and line breaks, and records the answer for every `/`. The
minified printer mode re-parses the generated programs without line
breaks, which stresses ASI and the regex/division decision.

## Conformance

* `TestJSConformanceTest262`: the vendored tc39/test262-parser-tests.
  1983/1983 pass files parse; 1947 have an AST equal to their
  pass-explicit twin (36 upstream mismatches listed); 722/729 fail and
  663/668 early files are rejected; the 7 + 5 exceptions are valid ES2024
  (OPEN_QUESTIONS §1). The test asserts the exception list exactly.
* `TestJSTest262Full` (development aid, `FF_TEST262_DIR=<tc39/test262>`):
  all of `test/language` and `test/annexB/language`, sloppy and strict
  where the flags allow: 44,116 runs, 0 failures, with ES2025+ proposals,
  Unicode >15.0 identifier tests and the post-2024 Annex B call-target
  tests skipped; also checks JS-INV-03 on every positive test.
* `conformance/jsparse/tools/acorn_diff.js` with `TestJSOracleDump`:
  canonical JSON equal to acorn's on 18,937 of 18,955 positive test262
  files (OPEN_QUESTIONS §11).
* `TestJSExtract`: `conformance/jsparse/extract/*.json`.

## Benchmarks

Generated minified bundles (seed 1, each program wrapped in a function
like a bundler module), `go test -run='^$' -bench 'JSLex|JSParse'
-benchtime=3x ./jsparse`, 4-vCPU Xeon, 2026-11-13, with other builds
running:

| Input | Lex MB/s | Lex allocs | Parse MB/s | Parse B/op | Parse allocs |
|---|---|---|---|---|---|
| 100 KB | 71 | 1,542 | 21 | 5.7 M | 4,858 |
| 1 MB | 74 | 14,685 | 27 | 58 M | 47,978 |
| 10 MB | 71 | 147,337 | 25 | 583 M | 477,138 |

The lexer benchmark lexes exactly the parser's token stream with the goals
the parser chose. Parse allocations are dominated by the arena (64 bytes
per node, about one node per 3.3 bytes of minified code, sized up front at
one node per 4 bytes and doubled explicitly). JS-INV-04 measured a per-byte
ratio of 1.08 (time) and 0.99 (bytes) between 256 KB and 1 MB.

acorn 8.14 (node 22) on the same machine, `tools/acorn_bench.js`: 14.7
MB/s on the vendored pass suite wrapped as one script (jsparse 22.2 MB/s)
and 7.9 MB/s on the acorn-compatible 100 KB bundle (jsparse 17.8 MB/s);
acorn rejects the larger generated bundles (regex heuristics).

## Design choices measured

Selectable through `Options.Algorithms`; `TestJSAlgoEquivalent` checks that
every candidate gives identical trees and diagnostics on the pass corpus,
generated programs and the fail/early suites. Benchmark:
`go test -run='^$' -bench JSAlgo -benchtime=10x -count=3 ./jsparse`
(MB/s, median of three, same machine and date):

| Decision | Candidate | Input | MB/s | Default |
|---|---|---|---|---|
| keyword recognition | `KeywordSwitch` (length-guarded string switch) | bundle 1 MB | 19.3 | ✓ |
| | `KeywordMap` (hash map) | bundle 1 MB | 19.9 | |
| identifier classification (non-ASCII) | `IdentUnicodeTables` (Go range tables) | non-ASCII 1 MB | 34.9 | ✓ |
| | `IdentBMPBitmap` (2 × 8 KiB BMP bitmaps) | non-ASCII 1 MB | 37.8 | |
| binary expressions | `BinaryClimbing` (precedence climbing) | bundle 1 MB | 21.2 | ✓ |
| | `BinaryCascade` (one function per level) | bundle 1 MB | 19.5 | |

* **Keywords:** the two are within noise of each other on real bundles
  (keyword lookup is a few percent of lexing). The switch stays the
  default: no hashing, no map initialization, and it maps 1:1 to a Rust
  `match` on `&str`.
* **Identifier classification:** the bitmap is about 8 % faster only on
  input made almost entirely of non-ASCII identifiers; ASCII never reaches
  either path. The tables stay the default because they need no 16 KiB
  table built on first use, cover the whole code space without a
  fallback, and match what the Rust `unicode-id-start` crate does. Pick
  the bitmap for scripts in non-Latin alphabets.
* **Binary expressions:** precedence climbing is about 8 % faster than
  the per-level cascade (which makes twelve calls per operand) and is
  what V2_DESIGN §4.2 specifies. The cascade mirrors the grammar's
  productions one to one and is the clearer reference for reading the
  specification.

(Entry text for `docs/ALGORITHMS.md`: this section.)

## Tests

```bash
go test ./jsparse/...                    # conformance, invariants, extraction
go test -run='^$' -fuzz FuzzJSParse -fuzztime 30s ./jsparse   # also FuzzJSLexer, FuzzJSPrintReparse
go test -run='^$' -bench . -benchtime=3x ./jsparse
FF_TEST262_DIR=/path/to/test262 go test -run TestJSTest262Full ./jsparse
```

The acorn tools are test-only scripts under `conformance/jsparse/tools/`;
acorn is not a dependency. Known limitations and decisions:
`OPEN_QUESTIONS.md`.
