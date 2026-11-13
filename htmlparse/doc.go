// Package htmlparse is a from-scratch WHATWG HTML parser: input decoding
// (HTML §13.2.3: BOM, transport charset, meta prescan, confidence), the
// tokenizer state machine (§13.2.5, every state including the 2025
// processing-instruction states), tree construction (§13.2.6, every
// insertion mode of the current standard, foreign content, the adoption
// agency algorithm, foster parenting, templates), the fragment parsing
// algorithm (§13.4), the HTML fragment serialization algorithm (§13.3),
// and the named character reference table (§13.5). The contract is spec
// 29.2 (codes 40.1).
//
// The DOM is an arena (Document.Nodes, linked by NodeID indexes) so the
// Rust port can mirror it as a Vec<Node>. The package never executes
// script; the scripting flag only selects how noscript parses.
//
// Entry points:
//
//	Decode / Sniff / DecodeAs   bytes in any WHATWG encoding -> UTF-8
//	Tokenize / NewTokenizer     tokens with source spans and parse errors
//	Parse / ParseRaw            whole documents
//	ParseFragment               innerHTML-style parsing with a context element
//	Render / RenderNode         serialization
//	Dump                        the html5lib test tree format
//
// Parsing is total: any byte string yields a document, never a panic.
// Options carries the fail-closed limits (nesting depth, attributes per
// tag, input size); exceeding one records a stable error code and parsing
// continues. See README.md for the architecture, the invariants the port
// must keep, and conformance results, and OPEN_QUESTIONS.md for the
// remaining deviations.
package htmlparse
