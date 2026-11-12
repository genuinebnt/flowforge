package dsl

import "fmt"

// Pos is a position in a source file. Line and Col are 1-based; Col counts
// Unicode code points from the start of the line. Offset is the 0-based
// byte offset into the source (spec 7.7).
type Pos struct {
	Line   int `json:"line"`
	Col    int `json:"col"`
	Offset int `json:"offset"`
}

// Span is a half-open source range [Start, End).
type Span struct {
	Start Pos `json:"start"`
	End   Pos `json:"end"`
}

// Join returns the smallest span covering s and o.
func (s Span) Join(o Span) Span {
	r := s
	if o.Start.Offset < r.Start.Offset {
		r.Start = o.Start
	}
	if o.End.Offset > r.End.Offset {
		r.End = o.End
	}
	return r
}

// TokenKind classifies a token.
type TokenKind int

// Token kinds. Keywords follow the identifier kinds; operators follow the
// keywords.
const (
	TokEOF TokenKind = iota
	TokIdent
	TokInt
	TokFloat
	TokDuration
	TokString

	// Keywords (spec 7.2).
	TokWorkflow
	TokFn
	TokTool
	TokType
	TokEnum
	TokLet
	TokFor
	TokIn
	TokMatch
	TokIf
	TokElse
	TokReturn
	TokRequire
	TokApproval
	TokAgent
	TokCompensate
	TokTrue
	TokFalse
	TokAs
	TokUse

	// Operators and punctuation.
	TokPlus     // +
	TokMinus    // -
	TokStar     // *
	TokSlash    // /
	TokPercent  // %
	TokEq       // ==
	TokNe       // !=
	TokLt       // <
	TokLe       // <=
	TokGt       // >
	TokGe       // >=
	TokAndAnd   // &&
	TokOrOr     // ||
	TokBang     // ! (logical not and the effect marker)
	TokPipe     // |>
	TokFatArrow // =>
	TokArrow    // ->
	TokQuestion // ?
	TokDot      // .
	TokDotDot   // ..
	TokColons   // ::
	TokAssign   // =
	TokColon    // :
	TokComma    // ,
	TokSemi     // ;
	TokLParen   // (
	TokRParen   // )
	TokLBracket // [
	TokRBracket // ]
	TokLBrace   // {
	TokRBrace   // }
	TokHash     // #
)

var tokenNames = map[TokenKind]string{
	TokEOF: "end of file", TokIdent: "identifier", TokInt: "integer literal",
	TokFloat: "float literal", TokDuration: "duration literal", TokString: "string literal",
	TokPlus: "+", TokMinus: "-", TokStar: "*", TokSlash: "/", TokPercent: "%",
	TokEq: "==", TokNe: "!=", TokLt: "<", TokLe: "<=", TokGt: ">", TokGe: ">=",
	TokAndAnd: "&&", TokOrOr: "||", TokBang: "!", TokPipe: "|>", TokFatArrow: "=>",
	TokArrow: "->", TokQuestion: "?", TokDot: ".", TokDotDot: "..", TokColons: "::",
	TokAssign: "=", TokColon: ":", TokComma: ",", TokSemi: ";", TokLParen: "(",
	TokRParen: ")", TokLBracket: "[", TokRBracket: "]", TokLBrace: "{", TokRBrace: "}",
	TokHash: "#",
}

// keywords maps each reserved word to its token kind.
var keywords = map[string]TokenKind{
	"workflow": TokWorkflow, "fn": TokFn, "tool": TokTool, "type": TokType,
	"enum": TokEnum, "let": TokLet, "for": TokFor, "in": TokIn, "match": TokMatch,
	"if": TokIf, "else": TokElse, "return": TokReturn, "require": TokRequire,
	"approval": TokApproval, "agent": TokAgent, "compensate": TokCompensate,
	"true": TokTrue, "false": TokFalse, "as": TokAs, "use": TokUse,
}

func init() {
	for w, k := range keywords {
		tokenNames[k] = w
	}
}

// IsKeyword reports whether s is a reserved word.
func IsKeyword(s string) bool { _, ok := keywords[s]; return ok }

// String returns the token kind as it would appear in a diagnostic.
func (k TokenKind) String() string {
	if n, ok := tokenNames[k]; ok {
		return n
	}
	return fmt.Sprintf("token(%d)", int(k))
}

// isKeyword reports whether k is a keyword kind.
func (k TokenKind) isKeyword() bool { return k >= TokWorkflow && k <= TokUse }

// Token is one lexical token.
type Token struct {
	Kind TokenKind
	Span Span
	// Text is the raw source text of the token.
	Text string
	// Int holds the value of TokInt and the nanoseconds of TokDuration.
	Int int64
	// Float holds the value of TokFloat.
	Float float64
	// Parts holds the decoded pieces of a TokString: literal text and
	// interpolated expressions (as token streams ending in TokEOF).
	Parts []StrPartToken
}

// StrPartToken is one piece of a string literal token. Exactly one of Lit
// (with Interp false) or Tokens (with Interp true) is meaningful.
type StrPartToken struct {
	Interp bool
	Lit    string
	Tokens []Token // interpolated expression tokens, terminated by TokEOF
	Span   Span    // the literal text, or the expression inside the braces
}

// describe returns a short human description of the token for messages.
func (t Token) describe() string {
	switch t.Kind {
	case TokEOF:
		return "end of file"
	case TokIdent:
		return fmt.Sprintf("identifier `%s`", t.Text)
	case TokInt, TokFloat, TokDuration, TokString:
		txt := t.Text
		if len(txt) > 24 {
			txt = txt[:21] + "..."
		}
		return fmt.Sprintf("%s `%s`", t.Kind, txt)
	}
	if t.Kind.isKeyword() {
		return fmt.Sprintf("keyword `%s`", t.Kind)
	}
	return fmt.Sprintf("`%s`", t.Kind)
}
