package jsparse

import (
	"errors"
	"fmt"
)

// Code is a stable diagnostic code. The list is closed: the Rust port
// declares the same codes with the same names (see Code.String). Codes are
// grouped by Class: syntax errors (the source does not match the
// grammar), early errors (it matches the grammar but violates a static
// semantics rule of ECMA-262) and limits (a configured resource bound).
type Code uint16

// Diagnostic codes. Append only; never renumber.
const (
	CodeNone Code = iota

	// Lexical errors (clause 12).
	CodeUnexpectedCharacter     // a code point that starts no token
	CodeUnterminatedString      // string literal not closed before a line terminator or EOF
	CodeUnterminatedTemplate    // template literal not closed before EOF
	CodeUnterminatedRegExp      // regular expression literal not closed on its line
	CodeUnterminatedComment     // /* without */
	CodeInvalidEscape           // malformed \x, \u or \u{} escape in a string literal
	CodeInvalidIdentifierEscape // \u escape in an identifier that is malformed or not ID_Start/ID_Continue
	CodeInvalidNumber           // malformed numeric literal (missing digits, bad BigInt, ...)
	CodeNumericSeparator        // misplaced numeric separator
	CodeIdentifierAfterNumber   // IdentifierStart or digit right after a numeric literal

	// Syntax errors (clauses 13-16).
	CodeUnexpectedToken  // token not allowed here
	CodeUnexpectedEOF    // input ended inside a construct
	CodeExpectedToken    // a specific token was required
	CodeInvalidCover     // cover grammar not reinterpretable: CoverInitializedName, `()` without `=>`, rest with trailing comma
	CodeNewlineForbidden // [no LineTerminator here] violated (arrow, async arrow)
	CodeMixedCoalesce    // ?? mixed with && or || without parentheses
	CodeExponentUnary    // unary expression as left operand of **
	CodeModuleItem       // import/export outside the module top level
	CodeEscapedKeyword   // keyword or contextual keyword written with \u escapes where it must be literal
	CodeForInOfHead      // invalid for-in/of head (initializer, multiple bindings, `let` / `async of` lookahead)
	CodeInvalidOptional  // template or `new` in an optional chain

	// Early errors (static semantics).
	CodeInvalidAssignTarget // not a valid assignment/update target
	CodeInvalidPattern      // invalid destructuring pattern or binding element
	CodeDuplicateProto      // duplicate __proto__ in an object literal
	CodeRedeclaration       // conflicting lexical/var declaration
	CodeReservedWord        // reserved word used as an identifier
	CodeStrictEvalArguments // eval/arguments bound or assigned in strict code
	CodeStrictWith          // with statement in strict code
	CodeStrictDelete        // delete of an unqualified identifier in strict code
	CodeStrictOctal         // legacy octal literal or escape in strict code / template
	CodeFunctionPosition    // function declaration where only a statement is allowed
	CodeIllegalBreak        // break outside loop/switch or to an unknown label
	CodeIllegalContinue     // continue outside loop or to a non-iteration label
	CodeDuplicateLabel      // label already in scope
	CodeIllegalReturn       // return outside a function
	CodeNewTarget           // new.target outside a function
	CodeImportMeta          // import.meta outside a module
	CodeSuperCall           // super() outside a derived constructor
	CodeSuperProperty       // super.x outside a method
	CodeYieldAwaitInParams  // yield/await expression in formal parameters
	CodeDuplicateParam      // duplicate parameter where not allowed
	CodeNonSimpleUseStrict  // "use strict" in a function with non-simple parameters
	CodePrivateName         // undeclared, duplicate or invalid private name
	CodeClassConstructor    // duplicate or special-method constructor
	CodeStaticPrototype     // static member named prototype
	CodeClassField          // field named constructor, or arguments in a field initializer
	CodeAwaitInStaticBlock  // await or arguments in a class static block
	CodeLetName             // let bound as a lexical name
	CodeExport              // export of an undeclared or duplicate name
	CodeInvalidRegExpFlags  // invalid or duplicate regular expression flags
	CodeInvalidRegExp       // regular expression pattern rejected by the validator
	CodeInvalidTemplate     // invalid escape in an untagged template
	CodeAccessorParams      // getter with parameters or setter without exactly one
	CodeLabelledFunction    // labelled function declaration in strict code or as a loop body
	CodeInvalidUseOfKeyword // yield/await/let/static used where reserved

	// Limits.
	CodeTooDeep // nesting deeper than Options.MaxDepth

	codeCount
)

// Class groups codes.
type Class uint8

// Diagnostic classes.
const (
	ClassSyntax Class = iota
	ClassEarly
	ClassLimit
)

var codeNames = [codeCount]string{
	CodeNone:                    "none",
	CodeUnexpectedCharacter:     "unexpected-character",
	CodeUnterminatedString:      "unterminated-string",
	CodeUnterminatedTemplate:    "unterminated-template",
	CodeUnterminatedRegExp:      "unterminated-regexp",
	CodeUnterminatedComment:     "unterminated-comment",
	CodeInvalidEscape:           "invalid-escape",
	CodeInvalidIdentifierEscape: "invalid-identifier-escape",
	CodeInvalidNumber:           "invalid-number",
	CodeNumericSeparator:        "numeric-separator",
	CodeIdentifierAfterNumber:   "identifier-after-number",
	CodeUnexpectedToken:         "unexpected-token",
	CodeUnexpectedEOF:           "unexpected-eof",
	CodeExpectedToken:           "expected-token",
	CodeInvalidCover:            "invalid-cover-grammar",
	CodeNewlineForbidden:        "newline-forbidden",
	CodeMixedCoalesce:           "mixed-coalesce",
	CodeExponentUnary:           "exponent-unary",
	CodeModuleItem:              "module-item-position",
	CodeEscapedKeyword:          "escaped-keyword",
	CodeForInOfHead:             "for-in-of-head",
	CodeInvalidOptional:         "invalid-optional-chain",
	CodeInvalidAssignTarget:     "invalid-assignment-target",
	CodeInvalidPattern:          "invalid-pattern",
	CodeDuplicateProto:          "duplicate-proto",
	CodeRedeclaration:           "redeclaration",
	CodeReservedWord:            "reserved-word",
	CodeStrictEvalArguments:     "strict-eval-arguments",
	CodeStrictWith:              "strict-with",
	CodeStrictDelete:            "strict-delete",
	CodeStrictOctal:             "strict-octal",
	CodeFunctionPosition:        "function-position",
	CodeIllegalBreak:            "illegal-break",
	CodeIllegalContinue:         "illegal-continue",
	CodeDuplicateLabel:          "duplicate-label",
	CodeIllegalReturn:           "illegal-return",
	CodeNewTarget:               "new-target",
	CodeImportMeta:              "import-meta",
	CodeSuperCall:               "super-call",
	CodeSuperProperty:           "super-property",
	CodeYieldAwaitInParams:      "yield-await-in-params",
	CodeDuplicateParam:          "duplicate-param",
	CodeNonSimpleUseStrict:      "non-simple-use-strict",
	CodePrivateName:             "private-name",
	CodeClassConstructor:        "class-constructor",
	CodeStaticPrototype:         "static-prototype",
	CodeClassField:              "class-field",
	CodeAwaitInStaticBlock:      "static-block",
	CodeLetName:                 "let-name",
	CodeExport:                  "export-name",
	CodeInvalidRegExpFlags:      "regexp-flags",
	CodeInvalidRegExp:           "regexp-pattern",
	CodeInvalidTemplate:         "invalid-template-escape",
	CodeAccessorParams:          "accessor-params",
	CodeLabelledFunction:        "labelled-function",
	CodeInvalidUseOfKeyword:     "invalid-keyword-use",
	CodeTooDeep:                 "too-deep",
}

// String returns the stable name of the code, e.g. "unexpected-token".
func (c Code) String() string {
	if c < codeCount {
		return codeNames[c]
	}
	return fmt.Sprintf("code(%d)", uint16(c))
}

// Class returns the class of the code.
func (c Code) Class() Class {
	switch {
	case c == CodeTooDeep:
		return ClassLimit
	case c >= CodeInvalidAssignTarget && c < CodeTooDeep:
		return ClassEarly
	default:
		return ClassSyntax
	}
}

// Span is a half-open byte range [Start, End) in the source.
// PORT: offsets count UTF-8 bytes, not UTF-16 code units as acorn's
// start/end do; positions in the canonical JSON are byte offsets too.
type Span struct {
	Start, End int32
}

// Len returns End - Start.
func (s Span) Len() int32 { return s.End - s.Start }

// Contains reports whether o lies within s.
func (s Span) Contains(o Span) bool { return o.Start >= s.Start && o.End <= s.End }

// Diagnostic is one reported problem.
type Diagnostic struct {
	Code    Code
	Span    Span
	Message string
}

func (d Diagnostic) String() string {
	return fmt.Sprintf("%d-%d %s: %s", d.Span.Start, d.Span.End, d.Code, d.Message)
}

// ErrSyntax is matched (errors.Is) by every *Error returned by Parse.
var ErrSyntax = errors.New("jsparse: syntax error")

// Error is returned by Parse in strict (non-tolerant) mode. It carries the
// first diagnostic.
type Error struct {
	Diagnostic
}

func (e *Error) Error() string {
	return "jsparse: " + e.Diagnostic.String()
}

// Is makes errors.Is(err, ErrSyntax) true.
func (e *Error) Is(target error) bool { return target == ErrSyntax }
