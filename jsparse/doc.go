// Package jsparse is an ECMAScript 2024 lexer and parser (ECMA-262 15th
// edition, clauses 12-16 and Annex B) that produces an arena AST with
// ESTree node kinds.
//
// Lexer (clause 12): an explicit state machine over UTF-8 (lexer.go,
// lexer_states.go) with the four lexical goals InputElementDiv,
// InputElementRegExp, InputElementRegExpOrTemplateTail and
// InputElementTemplateTail. The parser chooses the goal for every token
// (README "Goal-selection protocol"); the lexer never guesses whether `/`
// starts a regular expression. Numeric literals (12.9.3, numbers.go),
// string literals (12.9.4, strings.go), templates with cooked and raw
// values (12.9.6, templates.go), regular expressions with a pattern
// validator (12.9.5, 22.2.1, regexp.go), the hashbang (12.5) and the
// HTML-like comments of B.1.1 are covered.
//
// Parser (clauses 13-16): recursive descent with precedence climbing for
// binary operators (expressions.go), cover grammars for destructuring and
// arrow parameters (patterns.go), statements (statements.go), functions
// (functions.go), classes (classes.go), modules (modules.go), automatic
// semicolon insertion (12.10, asi.go) and the early errors exercised by
// the test262 suites (early_errors.go). Strict mode returns the first
// error; tolerant mode (Options.Tolerant) records diagnostics with stable
// codes (errors.go) and resynchronizes at statement boundaries.
//
// Output: Program holds the node arena (ast.go), a canonical ESTree JSON
// writer (astjson.go), a printer whose output re-parses to the same tree
// (printer.go) and extraction helpers for the content package
// (extract.go). The invariants the Rust port must reproduce are listed in
// README.md under "Invariants (port these)".
//
// The package imports only the standard library. It never panics on
// input, starts no goroutines and keeps no mutable global state.
package jsparse
