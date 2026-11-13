# conformance/dsl

Language-neutral cases for the Flowforge DSL front end (spec 19, 25.3).
Any implementation must produce the same JSON for every case; comparison
is on the decoded JSON data, so whitespace and key order do not matter.

## Layout

```
parse/<case>.ff               source
parse/<case>.expected.json    {"ast": <canonical AST>}  or  {"diagnostics": [...]}
check/<case>.ff               source; an optional first line "// policy: {...}"
check/<case>.policy.json      optional policy (overrides the comment)
check/<case>.expected.json    {"ok", "inferred_effects", "inferred_class", "diagnostics"}
```

Case-name prefixes: `g_` grammar productions, `p_` precedence, `e_` syntax
errors (dsl_001, dsl_002); `n_` names, `t_` types, `m_` patterns (dsl_003);
`f_` effects and policy, `a_` annotations (dsl_004); `s_` scope typing and
`rest_check_example` (dsl_005); `c_` closures (dsl_006); `spec_` the three
programs of spec 7.1. A name segment that is a code (`e_E0102_...`,
`f_W0402_...`) means the case must report that code; a `_ok` suffix means
no errors.

The policy object is `{"max_class": "none|passive|active|intrusive",
"known": true, "prohibited": [...]}` (`prohibited` optional). A workflow
that may call a tool whose name or declared effect a `prohibited` entry
names (trimmed, ASCII case-insensitive) gets E0402 at the tool's first
use. Without a policy the checker checks internal consistency only (no
E0402/E0403).

Check cases are analyzed with the file name `<case>.ff`. Diagnostics use
the shape of spec 7.7 (`code`, `severity`, `message`, `primary`,
`secondary`, optional `help`; positions are 1-based line, 1-based column in
code points, 0-based byte offset, end exclusive), sorted by primary start
offset. `inferred_effects` lists effect kinds in the order read, passive,
active, intrusive, write, financial_write, llm, time.

The Go reference regenerates goldens with
`go test ./dsl -run TestDSLConformance -update`; review the diff by hand.

## Canonical AST JSON

Structure only: no spans and no checker results. Every node is an object
with a `kind`; absent optional parts, empty lists and false flags are
omitted. Integers, floats and durations are strings (`"value": "42"`,
`"nanos": "7200000000000"`) so 64-bit values survive any JSON reader;
floats use the shortest round-trip form (Go `strconv.FormatFloat(v, 'g',
-1, 64)`).

```
file        {"items": [item...]}
annotation  {"name", "args"?: [arg]}            args present when written with parentheses
arg         {"name"?, "value": expr}
param       {"name", "type": type}

items
  workflow  {name, annotations?, params?, result?: type, effects?: [name], body: block}
  fn        {name, annotations?, generics?: [name], params?, result?, effects?, body}
  tool      {name, annotations?, params?, result, effects}
  type      {name, annotations?, generics?, fields?: [{name, type}]}
  enum      {name, annotations?, generics?, variants: [{name, payload?: [type]}]}
  use       {annotations?, path: [name]}

types
  named     {name, args?: [type]}
  unit      {}
  tuple     {elems: [type]}                      two or more
  func      {params?: [type], result: type}

statements
  let               {annotations?, pattern, type?, value: expr, compensate?: expr}
  for               {pattern, iter: expr, body: block}
  require_approval  {args?}
  return            {value?}
  expr              {expr}
  assign            {target: name, value}        always rejected by the checker (E0312)

expressions
  int {value}  float {value}  duration {nanos}  bool {value: "true"|"false"}
  string    {parts?: [{"lit": text} | {"expr": expr}]}    adjacent text is merged
  ident     {name}
  path      {segments: [name]}                   Enum::Variant
  unit {}   tuple {elems}   list {elems?}
  record    {path: [name], fields?: [{name, value}]}
  lambda    {params?: [name], body}
  if        {cond, then: block, else?: block | if}
  match     {scrutinee, arms: [{pattern, guard?, body}]}
  agent     {name, args?}
  block     {stmts?, tail?}
  unary     {op: "-"|"!", operand}
  binary    {op: "||" "&&" "==" "!=" "<" "<=" ">" ">=" "in" ".." "+" "-" "*" "/" "%", left, right}
  call      {callee, args?, pipe?: "bare"|"call"}  pipes are desugared:
                a |> f      => call{callee f, args [a], pipe "bare"}
                a |> f(x)   => call{callee f, args [a, x], pipe "call"}
  index     {target, index}
  field     {target, name}
  try       {operand}
  cast      {operand, type}

patterns
  wildcard {}  bind {name}  literal {value: expr}  unit {}  tuple {elems}
  variant   {path: [name], parens?: true, args?: [pattern]}
  record    {path: [name], fields?: [{name, pattern?}]}
```

Parenthesized expressions leave no node. A bare identifier pattern is
always `bind` in the syntax tree, even `None`; the checker resolves it to
the unit variant when one is in scope.
