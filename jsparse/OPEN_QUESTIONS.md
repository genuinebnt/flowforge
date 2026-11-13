# jsparse: open questions and recorded decisions

Each entry states the question, the choice made, and the test that pins it.

## 1. Conformance exceptions (test262-parser-tests)

The vendored suite (`conformance/third_party/test262-parser-tests`, commit
0e808c74) predates ECMAScript 2024 in a few places. The exact exception
list is data, `conformance/jsparse/test262_exceptions.json`, and
`TestJSConformanceTest262` asserts that **exactly** these files deviate.
Every deviation is a file the upstream directory expects to fail that is
valid ES2024, or an upstream explicit file that is not equivalent.

| Suite | Files | Reason |
|---|---|---|
| fail | 0d5e450f1da8a92a, 748656edbfb2d0bb, 79f882da06f88c9f, 92b6af54adef3624 | `\8` / `\9` are NonOctalDecimalEscapeSequences, valid in sloppy strings since ES2021 (B.1.2) |
| fail | 98204d734f8c72b3, ef81b93cf9bdb4ec | class fields (ES2022) |
| fail | e3fbcf63d7e43ead | `for (var x = 1 in o)` is allowed in sloppy code by B.3.5 |
| early | 12a74c60f52a60de, 1aff49273f3e3a98, be7329119eaa3d47, ec31fa5e521c5df4 | duplicate sloppy-mode function declarations in a block are allowed (B.3.2.4) |
| early | 0f5f47108da5c34e | `for (var e of ...)` may redeclare a simple catch parameter since ES2020 (B.3.4; test262 `annexB/language/statements/try/catch-redeclared-for-of-var.js`) |
| explicit | 36 files (listed in the JSON) | the upstream pass-explicit file renames identifiers or numbers, turns `'0x20'` into `0`, prints `a((x, y))` as two arguments, drops the parentheses of `('<U+2028>')` (turning it into a directive), or prints `09.5` as `9` |

The pass/pass-explicit comparison applies the suite's own equivalence
(the explicit files were generated from the Shift AST): non-computed
property keys compare by PropName and nested SequenceExpressions are
flattened. The relation is specified in `conformance/jsparse/README.md`.
acorn, normalized the same way, disagrees on all 36 explicit files too
(and on 29 more), which is why they are classified as upstream issues.

Result: 1983/1983 pass files parse, 1947/1983 have an equal explicit AST
(36 listed), 722/729 fail files and 663/668 early files are rejected
(7 and 5 listed).

## 2. Unicode version

ID_Start / ID_Continue come from Go's `unicode` package, which implements
Unicode 15.0.0. test262's `language/identifiers/*-unicode-15.1.0*`,
`-16.0.0*` and `-17.0.0*` tests need newer tables and are skipped by the
development harness `TestJSTest262Full`. The Rust port must use tables of
the same Unicode version (or both move together) or identifier acceptance
differs.

## 3. Call expressions as assignment targets (`f() = 1`)

ES2024 makes `f() = 1`, `f()++` and `for (f() in o)` early errors. Newer
test262 (`annexB/language/expressions/assignmenttargettype/`) treats them
as web-compat runtime errors in sloppy code, an editorial change after
ES2024. We follow ES2024; the vendored fail file `a8beb1480f385441.js`
(`func() = 4`) agrees. `TestJSTest262Full` skips that directory.

## 4. Early errors: coverage

Implemented (all exercised by the vendored `early` suite or test262):
duplicate lexical declarations and var/lexical conflicts (including Annex B
block functions, catch parameters, switch cases, module top level), invalid
assignment and update targets, duplicate `__proto__` (not in patterns),
CoverInitializedName outside patterns, break/continue targets and label
sets, duplicate labels, labelled functions in strict code and loop bodies,
`return` outside functions, `super()` outside derived constructors,
`super.x` outside methods, `super.#x`, `new.target` outside functions,
`import.meta` in scripts, private names (undeclared, duplicate except
getter+setter pairs, `#constructor`, `delete this.#x`), class constructor
rules (duplicate, accessor/generator/async constructor, field named
`constructor`, static `prototype`), `arguments` in field initializers and
static blocks (not in labels), `await` in static blocks, yield/await in
formal parameters and arrow parameters, duplicate parameters, `"use
strict"` with non-simple parameters, retroactive strict checks of the
function name and parameters, legacy octal literals and escapes in strict
code (including earlier directives of the same prologue), `with` and
`delete identifier` in strict code, strict reserved words, `let` as a
lexical name, escaped keywords, invalid template escapes in untagged
templates, regular-expression flags and patterns, export of undeclared or
duplicate names, non-well-formed string export names, duplicate import
attributes, getter/setter parameter counts.

Not implemented: none known from either suite. Two approximations are
deliberate: `await` inside a class field initializer of an async function
is reported as reserved (the specification parses it as an
AwaitExpression and then forbids it; both reject), and `arguments` used
as a *binding* inside an arrow in a field initializer is not reported (the
rule is about IdentifierReferences).

## 5. Regular expression validation level

`validateRegExpPattern` checks the full Pattern grammar of 22.2.1 with the
Annex B.1.2 extensions when neither `u` nor `v` is set, the static
semantics of 22.2.1.1 (quantifier bounds, class range order, duplicate
group names, backreferences to existing groups / names) and the `v`-mode
ClassSetExpression grammar (operators, nested classes, `\q{}`, reserved
double punctuators). Unicode property escapes are checked by name:
General_Category values (Go's categories plus long names), the binary
properties of table 67, properties of strings with `v`, and Script /
Script_Extensions values from Go's script table plus any four-letter
title-case alias (Go has no ISO 15924 aliases). Not modelled: case-folding
semantics. ES2025 syntax (`(?i:...)` modifiers, duplicate named groups in
different alternatives) is rejected.

## 6. ESTree shape

The canonical JSON follows acorn (`ecmaVersion: "latest"`), so it is
directly comparable: `expression` on every function, `id: null,
generator: false` on arrows, ChainExpression around optional chains,
`attributes` on ImportDeclaration / ExportAllDeclaration always and on
ExportNamedDeclaration only when it has a `source`, `bigint` = the digits
without `n` or separators (hex prefix kept), RegExp and BigInt `value` =
null, TemplateElement `value: {cooked, raw}` with cooked null for invalid
escapes, `directive` on directive statements. Differences from acorn's
JSON: positions are byte offsets and omitted by default; `raw` is omitted
by default.

## 7. Import attributes

`with { type: "json" }` (ES2025) is supported because bundles use it; the
withdrawn `assert { }` syntax is not.

## 8. TypeScript

Out of scope. The AST is ready for it: `Kind` and `Op` are append-only
enums, slot meanings live in one schema table (`schemas` in astjson.go),
and nodes have spare slots (D, List2, Str2) so TS nodes such as
`TSTypeAnnotation` can be added as new kinds without changing existing
ones. Type annotations would hang off existing nodes through a side table
indexed by NodeID rather than new fields, to keep `Node` at 64 bytes.

## 9. Tolerant-mode recovery

Recovery is heuristic by nature: after a halt the parser re-lexes from
the error token and skips to a `;`, the `}` closing the enclosing block,
or a line that starts with a statement-starting token, balancing brackets
and template substitutions; a stray `}` at the top level is dropped on its
own. Skipped tokens are lexed under InputElementDiv, so a regular
expression containing `;` or `}` inside a skipped region can move the
boundary. `TestJSTolerantRecovery` pins the behaviour; the first tolerant
diagnostic always equals the strict-mode error (JS-INV-08).

## 10. Depth limit

Parsing is recursive. `Options.MaxDepth` (default 4000) bounds statement
and expression nesting, counting each element of a binary-operator chain,
so the AST depth (and the recursion of the printer and JSON writer) stays
bounded; exceeding it is the `too-deep` diagnostic. The Rust port must use
the same limit or deep inputs parse differently.

## 11. Differences found in acorn

The differential runs (`conformance/jsparse/tools/acorn_diff.js`) agree on
18,937 of 18,955 positive test262 files; the rest are Unicode 15.1+
identifiers and two acorn bugs (it rejects `arguments:` labels in field
initializers and static blocks). acorn's heuristic regex detection also
rejects valid generated code such as `class A extends /re/ {}` and some
`yield /re/` forms, which is why the throughput comparison uses
"acorn-safe" bundles.
