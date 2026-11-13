// Package cssparse implements the CSS Syntax Module Level 3 tokenizer and
// parser: §3.3 input preprocessing, §4 tokenization (as an explicit state
// machine over the spec's "consume" algorithms), §5 parsing ("parse a
// stylesheet / list of rules / rule / list of declarations / declaration
// / component value / list of component values", plus the current
// Editor's Draft's "parse a block's contents" with nesting), §9
// serialization, and extraction of URL references (url(), @import,
// @namespace, image-set(), @font-face src).
//
// Input is UTF-8 bytes. Tokens carry byte spans that tile the input
// exactly; comments are kept as trivia tokens. Trees are arenas of Node
// values linked by int32 NodeID. Parsing is total: every byte string gives
// a tree, and errors are Diagnostic values with stable codes. Limits
// (Options.MaxDepth 64, MaxInputBytes 8 MiB, MaxURLs 20,000) make a
// result GradePartial, never GradeComplete.
//
// The package keeps no reference to the caller's slice; token values point
// into a private copy of the input.
package cssparse
