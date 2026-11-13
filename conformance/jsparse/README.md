# conformance/jsparse

Language-neutral data and tools for `jsparse` and its Rust port.

| Path | Contents |
|---|---|
| `test262_exceptions.json` | the exact files of `conformance/third_party/test262-parser-tests` that deviate from their directory's expectation, with reasons (see `jsparse/OPEN_QUESTIONS.md` §1) |
| `extract/*.json` | extraction cases: string literals with call context, module references and `sourceMappingURL`, literal tables |
| `tools/acorn_diff.js` | differential AST check against acorn (test-only; acorn is installed in a scratch directory, never vendored) |
| `tools/acorn_bench.js` | throughput comparison with acorn on the benchmark bundles |

## Running the same suites from the Rust port

1. **test262-parser-tests.** For each file in `pass/`, `pass-explicit/`,
   `fail/` and `early/` (files whose name contains `.module.js` are
   Modules, all others Scripts): pass and pass-explicit files must parse;
   fail and early files must be rejected (syntax or early error). A file
   listed under `fail` or `early` in `test262_exceptions.json` must instead
   be **accepted**, and a pass file listed under `explicit` must parse but
   may differ from its twin. The run fails unless the set of deviating
   files is exactly the listed set.
2. **Explicit equivalence.** For every pass file not listed under
   `explicit`, the canonical JSON of `pass/X.js` and `pass-explicit/X.js`
   must be equal after two normalizations (the suite's explicit files were
   generated from the Shift AST, whose equality this reproduces):
   * every non-computed key of a `Property`, `MethodDefinition` or
     `PropertyDefinition` written as a string or number literal is
     replaced by an `Identifier` whose name is its PropName (the string
     value, or `Number::toString` of the number);
   * a `SequenceExpression` element that is itself a `SequenceExpression`
     is replaced by its elements (recursively).
3. **Extraction.** Each `extract/*.json` file holds `cases` with a
   `source`, a `module` flag and the expected `strings`, `modules`,
   `sourceMappingURL` or `tables` (field meanings in the Go test
   `jsparse/extract_test.go` and the doc comments of `extract.go`).
   `LiteralTables` is called with minEntries 3 and maxEntries 100.
4. **Invariants.** Reimplement JS-INV-01 … JS-INV-10 from
   `jsparse/README.md` with the same bounds (JS-INV-04: 1.6), the same
   corpus (the pass files plus a grammar-driven generator seeded from
   SplitMix64) and the same hand-written lexical-goal table
   (`goalCases` in `jsparse/invariants_test.go`).
5. **Full test262** (optional, not vendored): run `test/language` and
   `test/annexB/language`, skipping the features listed in
   `test262Unsupported` (`jsparse/test262full_test.go`), identifier tests
   for Unicode newer than the port's tables, and
   `annexB/language/expressions/assignmenttargettype`.

## Canonical AST JSON

The canonical form is ESTree as acorn (`ecmaVersion: "latest"`) writes it,
made byte-comparable:

* One JSON object per node; `"type"` is the ESTree type name. Absent child
  nodes are `null`; array holes are `null` in `elements`.
* Object keys are sorted by byte value (the same order as
  `core.CanonicalJSON`), at every level including `regex` and
  TemplateElement `value`. No insignificant white space.
* Positions (`start`, `end`, UTF-8 byte offsets) and Literal `raw` are
  omitted unless requested (`JSONOptions`).
* Strings are escaped like `JSON.stringify`: `\"`, `\\`, `\b \f \n \r \t`,
  other control characters as `\u00XX` (lower-case hex), lone surrogates as
  `\uDXXX`; all other characters, U+2028 and U+2029 included, are written
  as UTF-8. Invalid UTF-8 bytes in the source are written as U+FFFD.
* Numbers are written with `Number::toString` (ECMA-262 6.1.6.1.20):
  shortest round-trip digits, exponent form for exponents ≥ 21 or ≤ −7
  (`1e+21`, `1e-7`); non-finite values are `null`.
* Literal: `value` is the string, number, boolean or `null`; RegExp
  literals have `value: null` and `regex: {"flags", "pattern"}`; BigInt
  literals have `value: null` and `bigint` (digits without `n` or
  separators, radix prefix kept).
* TemplateElement: `value: {"cooked", "raw"}`, `cooked` null when the
  template has an invalid escape (tagged templates only); `raw` has CR and
  CRLF normalized to LF.
* `ExpressionStatement.directive` is present only on Directive Prologue
  entries (the raw text between the quotes).
* `ExportNamedDeclaration.attributes` is present only when the
  declaration has a `source`; `ImportDeclaration` and
  `ExportAllDeclaration` always have `attributes`.
* Arrow functions have `id: null` and `generator: false`; every function
  has `expression`.

Node types and their keys (besides `type`, and `start`/`end` when
requested):

| type | keys |
|---|---|
| `Program` | `body`, `sourceType` |
| `Identifier` | `name` |
| `PrivateIdentifier` | `name` |
| `Literal` | `bigint`, `raw`, `regex`, `value` |
| `ExpressionStatement` | `directive`, `expression` |
| `BlockStatement` | `body` |
| `StaticBlock` | `body` |
| `EmptyStatement` | `` |
| `DebuggerStatement` | `` |
| `WithStatement` | `body`, `object` |
| `ReturnStatement` | `argument` |
| `LabeledStatement` | `body`, `label` |
| `BreakStatement` | `label` |
| `ContinueStatement` | `label` |
| `IfStatement` | `alternate`, `consequent`, `test` |
| `SwitchStatement` | `cases`, `discriminant` |
| `SwitchCase` | `consequent`, `test` |
| `ThrowStatement` | `argument` |
| `TryStatement` | `block`, `finalizer`, `handler` |
| `CatchClause` | `body`, `param` |
| `WhileStatement` | `body`, `test` |
| `DoWhileStatement` | `body`, `test` |
| `ForStatement` | `body`, `init`, `test`, `update` |
| `ForInStatement` | `body`, `left`, `right` |
| `ForOfStatement` | `await`, `body`, `left`, `right` |
| `FunctionDeclaration` | `async`, `body`, `expression`, `generator`, `id`, `params` |
| `VariableDeclaration` | `declarations`, `kind` |
| `VariableDeclarator` | `id`, `init` |
| `ThisExpression` | `` |
| `ArrayExpression` | `elements` |
| `ObjectExpression` | `properties` |
| `Property` | `computed`, `key`, `kind`, `method`, `shorthand`, `value` |
| `FunctionExpression` | `async`, `body`, `expression`, `generator`, `id`, `params` |
| `ArrowFunctionExpression` | `async`, `body`, `expression`, `generator`, `id`, `params` |
| `UnaryExpression` | `argument`, `operator`, `prefix` |
| `UpdateExpression` | `argument`, `operator`, `prefix` |
| `BinaryExpression` | `left`, `operator`, `right` |
| `AssignmentExpression` | `left`, `operator`, `right` |
| `LogicalExpression` | `left`, `operator`, `right` |
| `MemberExpression` | `computed`, `object`, `optional`, `property` |
| `ChainExpression` | `expression` |
| `ConditionalExpression` | `alternate`, `consequent`, `test` |
| `CallExpression` | `arguments`, `callee`, `optional` |
| `NewExpression` | `arguments`, `callee` |
| `SequenceExpression` | `expressions` |
| `YieldExpression` | `argument`, `delegate` |
| `AwaitExpression` | `argument` |
| `TemplateLiteral` | `expressions`, `quasis` |
| `TaggedTemplateExpression` | `quasi`, `tag` |
| `TemplateElement` | `tail`, `value` |
| `ObjectPattern` | `properties` |
| `ArrayPattern` | `elements` |
| `RestElement` | `argument` |
| `AssignmentPattern` | `left`, `right` |
| `ClassDeclaration` | `body`, `id`, `superClass` |
| `ClassExpression` | `body`, `id`, `superClass` |
| `ClassBody` | `body` |
| `MethodDefinition` | `computed`, `key`, `kind`, `static`, `value` |
| `PropertyDefinition` | `computed`, `key`, `static`, `value` |
| `MetaProperty` | `meta`, `property` |
| `Super` | `` |
| `SpreadElement` | `argument` |
| `ImportDeclaration` | `attributes`, `source`, `specifiers` |
| `ImportSpecifier` | `imported`, `local` |
| `ImportDefaultSpecifier` | `local` |
| `ImportNamespaceSpecifier` | `local` |
| `ImportAttribute` | `key`, `value` |
| `ExportNamedDeclaration` | `attributes`, `declaration`, `source`, `specifiers` |
| `ExportSpecifier` | `exported`, `local` |
| `ExportDefaultDeclaration` | `declaration` |
| `ExportAllDeclaration` | `attributes`, `exported`, `source` |
| `ImportExpression` | `options`, `source` |
