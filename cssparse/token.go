package cssparse

import "strconv"

// TokenKind is the type of a CSS token (CSS Syntax 3 §4, "Tokenization").
// The names follow the spec's <ident-token>, <function-token>, ...
type TokenKind uint8

// Token kinds. TokenComment is trivia: the spec's tokenizer consumes
// comments without producing a token (§4.3.2); this tokenizer keeps them so
// token spans tile the input. The parser skips them.
const (
	TokenInvalid     TokenKind = iota
	TokenIdent                 // <ident-token>
	TokenFunction              // <function-token>; Value is the name without "("
	TokenAtKeyword             // <at-keyword-token>; Value is the name without "@"
	TokenHash                  // <hash-token>; FlagHashID marks type "id"
	TokenString                // <string-token>; Value is the decoded content
	TokenBadString             // <bad-string-token>
	TokenURL                   // <url-token>; Value is the decoded URL
	TokenBadURL                // <bad-url-token>
	TokenDelim                 // <delim-token>; Value is the one code point
	TokenNumber                // <number-token>; FlagInteger marks type "integer"
	TokenPercentage            // <percentage-token>
	TokenDimension             // <dimension-token>; Value is the unit
	TokenWhitespace            // <whitespace-token>
	TokenCDO                   // <CDO-token> "<!--"
	TokenCDC                   // <CDC-token> "-->"
	TokenColon                 // <colon-token>
	TokenSemicolon             // <semicolon-token>
	TokenComma                 // <comma-token>
	TokenOpenSquare            // <[-token>
	TokenCloseSquare           // <]-token>
	TokenOpenParen             // <(-token>
	TokenCloseParen            // <)-token>
	TokenOpenCurly             // <{-token>
	TokenCloseCurly            // <}-token>
	TokenComment               // trivia: /* ... */

	// Legacy tokens of the CSS Syntax 3 2014 Candidate Recommendation,
	// produced only with Options.LegacyTokens (see OPEN_QUESTIONS.md).
	TokenUnicodeRange   // <unicode-range-token>; RangeStart/RangeEnd
	TokenIncludeMatch   // "~="
	TokenDashMatch      // "|="
	TokenPrefixMatch    // "^="
	TokenSuffixMatch    // "$="
	TokenSubstringMatch // "*="
	TokenColumn         // "||"

	tokenKindCount
)

var tokenKindNames = [...]string{
	"invalid", "ident", "function", "at-keyword", "hash", "string", "bad-string",
	"url", "bad-url", "delim", "number", "percentage", "dimension", "whitespace",
	"CDO", "CDC", "colon", "semicolon", "comma", "[", "]", "(", ")", "{", "}",
	"comment", "unicode-range", "~=", "|=", "^=", "$=", "*=", "||",
}

// String returns the stable lower-case name of k.
func (k TokenKind) String() string {
	if int(k) < len(tokenKindNames) {
		return tokenKindNames[k]
	}
	return "token(" + strconv.Itoa(int(k)) + ")"
}

// TokenFlags carries the type flags the spec attaches to some tokens.
type TokenFlags uint8

const (
	// FlagHashID is the hash token's type flag "id" (otherwise
	// "unrestricted").
	FlagHashID TokenFlags = 1 << iota
	// FlagInteger is the numeric type flag "integer" (otherwise "number").
	FlagInteger
	// FlagUnterminated marks a string, url or comment that ran into EOF
	// (a parse error; the token is still produced).
	FlagUnterminated
)

// Token is one token with its byte span in the source. Spans of
// consecutive tokens tile the input exactly (CSS-INV-01).
type Token struct {
	Kind  TokenKind
	Flags TokenFlags
	// Start and End are byte offsets into the source, End exclusive.
	Start, End int32
	// ReprEnd is, for numeric tokens, the end of the number's source text
	// (the spec's "representation"): Src[Start:ReprEnd].
	ReprEnd int32
	// Value is the decoded value (escapes resolved, U+0000 and invalid
	// UTF-8 replaced by U+FFFD): ident/function/at-keyword/hash name,
	// string and url contents, the delim code point, the dimension unit.
	Value string
	// Number is the numeric value of number, percentage and dimension
	// tokens.
	Number float64
	// RangeStart and RangeEnd are the bounds of a legacy unicode-range.
	RangeStart, RangeEnd uint32
}

// IsTrivia reports whether the parser ignores t (comments).
func (t *Token) IsTrivia() bool { return t.Kind == TokenComment }
