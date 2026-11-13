package jsparse

// TokenKind is the kind of a lexical token (ECMA-262 clause 12). The
// syntactic kinds come first, then punctuators, then trivia (white space,
// line terminators and comments), which the lexer reports only when trivia
// recording is on.
type TokenKind uint8

// Token kinds.
const (
	TEOF     TokenKind = iota
	TInvalid           // a code point that starts no token; Err is set

	TIdentifier // IdentifierName, including reserved words (see Token.Kw)
	TPrivateName
	TNumeric
	TBigInt
	TString
	TNoSubstitutionTemplate
	TTemplateHead
	TTemplateMiddle
	TTemplateTail
	TRegExp

	// Punctuators (12.8), DivPunctuator and RightBracePunctuator.
	TLBrace
	TRBrace
	TLParen
	TRParen
	TLBracket
	TRBracket
	TDot
	TEllipsis
	TSemicolon
	TComma
	TLt
	TGt
	TLtEq
	TGtEq
	TEqEq
	TNotEq
	TEqEqEq
	TNotEqEq
	TPlus
	TMinus
	TStar
	TSlash
	TPercent
	TStarStar
	TPlusPlus
	TMinusMinus
	TShl
	TShr
	TUShr
	TAmp
	TPipe
	TCaret
	TBang
	TTilde
	TAmpAmp
	TPipePipe
	TQuestionQuestion
	TQuestion
	TQuestionDot
	TColon
	TEq
	TPlusEq
	TMinusEq
	TStarEq
	TSlashEq
	TPercentEq
	TStarStarEq
	TShlEq
	TShrEq
	TUShrEq
	TAmpEq
	TPipeEq
	TCaretEq
	TAmpAmpEq
	TPipePipeEq
	TQuestionQuestionEq
	TArrow

	// Trivia.
	TWhiteSpace
	TLineTerminator
	TLineComment      // // ...
	TBlockComment     // /* ... */
	THTMLOpenComment  // <!-- ... (Annex B.1.1, scripts only)
	THTMLCloseComment // --> ... at the start of a line (Annex B.1.1, scripts only)
	THashbang         // #! ... at offset 0

	tokenKindCount
)

var tokenKindNames = [tokenKindCount]string{
	TEOF: "EOF", TInvalid: "Invalid", TIdentifier: "Identifier", TPrivateName: "PrivateName",
	TNumeric: "Numeric", TBigInt: "BigInt", TString: "String",
	TNoSubstitutionTemplate: "NoSubstitutionTemplate", TTemplateHead: "TemplateHead",
	TTemplateMiddle: "TemplateMiddle", TTemplateTail: "TemplateTail", TRegExp: "RegularExpression",
	TLBrace: "{", TRBrace: "}", TLParen: "(", TRParen: ")", TLBracket: "[", TRBracket: "]",
	TDot: ".", TEllipsis: "...", TSemicolon: ";", TComma: ",", TLt: "<", TGt: ">", TLtEq: "<=",
	TGtEq: ">=", TEqEq: "==", TNotEq: "!=", TEqEqEq: "===", TNotEqEq: "!==", TPlus: "+",
	TMinus: "-", TStar: "*", TSlash: "/", TPercent: "%", TStarStar: "**", TPlusPlus: "++",
	TMinusMinus: "--", TShl: "<<", TShr: ">>", TUShr: ">>>", TAmp: "&", TPipe: "|", TCaret: "^",
	TBang: "!", TTilde: "~", TAmpAmp: "&&", TPipePipe: "||", TQuestionQuestion: "??",
	TQuestion: "?", TQuestionDot: "?.", TColon: ":", TEq: "=", TPlusEq: "+=", TMinusEq: "-=",
	TStarEq: "*=", TSlashEq: "/=", TPercentEq: "%=", TStarStarEq: "**=", TShlEq: "<<=",
	TShrEq: ">>=", TUShrEq: ">>>=", TAmpEq: "&=", TPipeEq: "|=", TCaretEq: "^=",
	TAmpAmpEq: "&&=", TPipePipeEq: "||=", TQuestionQuestionEq: "??=", TArrow: "=>",
	TWhiteSpace: "WhiteSpace", TLineTerminator: "LineTerminator", TLineComment: "SingleLineComment",
	TBlockComment: "MultiLineComment", THTMLOpenComment: "HTMLOpenComment",
	THTMLCloseComment: "HTMLCloseComment", THashbang: "HashbangComment",
}

func (k TokenKind) String() string {
	if k < tokenKindCount {
		return tokenKindNames[k]
	}
	return "?"
}

// IsTrivia reports whether k is white space, a line terminator or a comment.
func (k TokenKind) IsTrivia() bool { return k >= TWhiteSpace && k <= THashbang }

// IsPunctuator reports whether k is a punctuator.
func (k TokenKind) IsPunctuator() bool { return k >= TLBrace && k <= TArrow }

// isAssignOp reports whether k is = or a compound assignment operator.
func (k TokenKind) isAssignOp() bool { return k >= TEq && k <= TQuestionQuestionEq }

// Goal is the lexical goal symbol (12, "The InputElementDiv goal symbol is
// used in all syntactic grammar contexts where a RegularExpressionLiteral,
// a TemplateMiddle, or a TemplateTail is not permitted ..."). The parser
// selects it; the lexer never guesses.
type Goal uint8

// Lexical goals.
const (
	GoalDiv                  Goal = iota // InputElementDiv
	GoalRegExp                           // InputElementRegExp
	GoalRegExpOrTemplateTail             // InputElementRegExpOrTemplateTail
	GoalTemplateTail                     // InputElementTemplateTail
)

func (g Goal) String() string {
	switch g {
	case GoalDiv:
		return "InputElementDiv"
	case GoalRegExp:
		return "InputElementRegExp"
	case GoalRegExpOrTemplateTail:
		return "InputElementRegExpOrTemplateTail"
	default:
		return "InputElementTemplateTail"
	}
}

func (g Goal) allowsRegExp() bool { return g == GoalRegExp || g == GoalRegExpOrTemplateTail }

func (g Goal) allowsTemplateTail() bool {
	return g == GoalTemplateTail || g == GoalRegExpOrTemplateTail
}

// TokenFlags carries lexical facts the parser needs for early errors and
// restricted productions.
type TokenFlags uint8

// Token flags.
const (
	FlagNewlineBefore TokenFlags = 1 << iota // a LineTerminator precedes the token (12.10 ASI)
	FlagEscaped                              // identifier contains \u escapes
	FlagLegacyOctal                          // legacy octal / non-octal-decimal number, or such an escape in a string
	FlagInvalidEscape                        // template part has a NotEscapeSequence (cooked is undefined)
	FlagSeparator                            // numeric literal contains separators
)

// Token is one input element. Value holds the decoded text: the
// identifier name (escapes resolved), the string or template cooked value,
// the regular-expression body, or the numeric literal source. Raw holds
// the template raw value (CR and CRLF normalized to LF) or the regular
// expression flags.
type Token struct {
	Kind  TokenKind
	Kw    Keyword
	Flags TokenFlags
	Goal  Goal // goal the token was lexed under
	Start int32
	End   int32
	Err   Code  // lexical error attached to this token, if any
	ErrAt int32 // offset of the lexical error
	Num   float64
	Value string
	Raw   string
}

// Span returns the token's byte range.
func (t *Token) Span() Span { return Span{t.Start, t.End} }

// Keyword identifies reserved and contextual words (12.7.2).
type Keyword uint8

// Keywords. Reserved words come before kwFirstContextual.
const (
	KwNone Keyword = iota
	KwAwait
	KwBreak
	KwCase
	KwCatch
	KwClass
	KwConst
	KwContinue
	KwDebugger
	KwDefault
	KwDelete
	KwDo
	KwElse
	KwEnum
	KwExport
	KwExtends
	KwFalse
	KwFinally
	KwFor
	KwFunction
	KwIf
	KwImport
	KwIn
	KwInstanceof
	KwNew
	KwNull
	KwReturn
	KwSuper
	KwSwitch
	KwThis
	KwThrow
	KwTrue
	KwTry
	KwTypeof
	KwVar
	KwVoid
	KwWhile
	KwWith
	KwYield
	// Reserved only in strict mode code.
	KwImplements
	KwInterface
	KwLet
	KwPackage
	KwPrivate
	KwProtected
	KwPublic
	KwStatic
	// Contextual, never reserved.
	KwAs
	KwAsync
	KwFrom
	KwGet
	KwMeta
	KwOf
	KwSet
	KwTarget
	KwEval
	KwArguments
	KwConstructor
	KwProto // __proto__
)

// lookupKeyword maps an identifier name to its keyword.
func lookupKeyword(s string) Keyword {
	if len(s) < 2 || len(s) > 11 {
		return KwNone
	}
	switch s {
	case "await":
		return KwAwait
	case "break":
		return KwBreak
	case "case":
		return KwCase
	case "catch":
		return KwCatch
	case "class":
		return KwClass
	case "const":
		return KwConst
	case "continue":
		return KwContinue
	case "debugger":
		return KwDebugger
	case "default":
		return KwDefault
	case "delete":
		return KwDelete
	case "do":
		return KwDo
	case "else":
		return KwElse
	case "enum":
		return KwEnum
	case "export":
		return KwExport
	case "extends":
		return KwExtends
	case "false":
		return KwFalse
	case "finally":
		return KwFinally
	case "for":
		return KwFor
	case "function":
		return KwFunction
	case "if":
		return KwIf
	case "import":
		return KwImport
	case "in":
		return KwIn
	case "instanceof":
		return KwInstanceof
	case "new":
		return KwNew
	case "null":
		return KwNull
	case "return":
		return KwReturn
	case "super":
		return KwSuper
	case "switch":
		return KwSwitch
	case "this":
		return KwThis
	case "throw":
		return KwThrow
	case "true":
		return KwTrue
	case "try":
		return KwTry
	case "typeof":
		return KwTypeof
	case "var":
		return KwVar
	case "void":
		return KwVoid
	case "while":
		return KwWhile
	case "with":
		return KwWith
	case "yield":
		return KwYield
	case "implements":
		return KwImplements
	case "interface":
		return KwInterface
	case "let":
		return KwLet
	case "package":
		return KwPackage
	case "private":
		return KwPrivate
	case "protected":
		return KwProtected
	case "public":
		return KwPublic
	case "static":
		return KwStatic
	case "as":
		return KwAs
	case "async":
		return KwAsync
	case "from":
		return KwFrom
	case "get":
		return KwGet
	case "meta":
		return KwMeta
	case "of":
		return KwOf
	case "set":
		return KwSet
	case "target":
		return KwTarget
	case "eval":
		return KwEval
	case "arguments":
		return KwArguments
	case "constructor":
		return KwConstructor
	case "__proto__":
		return KwProto
	}
	return KwNone
}

// isReserved reports whether k is a ReservedWord (always reserved, apart
// from yield and await whose reservation depends on context).
func (k Keyword) isReserved() bool { return k >= KwAwait && k <= KwYield }

// isStrictReserved reports whether k is reserved in strict mode code only.
func (k Keyword) isStrictReserved() bool { return k >= KwImplements && k <= KwStatic }
