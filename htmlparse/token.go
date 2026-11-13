package htmlparse

// TokenKind is the kind of a tokenizer output token (HTML §13.2.5: DOCTYPE,
// start tag, end tag, comment, character, end-of-file, and the 2025
// processing instruction).
type TokenKind uint8

// Token kinds.
const (
	CharacterToken TokenKind = iota
	StartTagToken
	EndTagToken
	CommentToken
	DoctypeToken
	ProcessingInstructionToken
	EOFToken
)

// tokenKindNames are the html5lib output type names, indexed by kind.
var tokenKindNames = [...]string{"Character", "StartTag", "EndTag", "Comment", "DOCTYPE", "ProcessingInstruction", "EOF"}

// String returns the html5lib token type name ("StartTag", "DOCTYPE", ...).
func (k TokenKind) String() string {
	if int(k) < len(tokenKindNames) {
		return tokenKindNames[k]
	}
	return "Token?"
}

// Token is one tokenizer token. Adjacent character tokens are coalesced:
// a CharacterToken carries a run of text (after character-reference
// decoding and NUL handling) whose source is Span.
//
// Field use by kind:
//
//	Character:             Data
//	StartTag, EndTag:      Name (lower-cased), Attrs, SelfClosing
//	Comment:               Data
//	DOCTYPE:               Name/HasName, PublicID/HasPublicID, SystemID/HasSystemID, ForceQuirks
//	ProcessingInstruction: Name (target), Data
//	EOF:                   -
//
// Spans tile the input (HTML-INV-01): the first token starts at 0, each
// token starts where the previous one ended and the EOF token ends at
// len(input). Bytes that produce no token of their own (for example the
// ignored "</>") belong to the following token.
type Token struct {
	Kind        TokenKind
	Name        string
	Data        string
	Attrs       []Attr
	SelfClosing bool

	PublicID, SystemID                string
	HasName, HasPublicID, HasSystemID bool
	ForceQuirks                       bool

	Span Span

	atom tagID // interned Name for tags (tagUnknown for other names)
}

// TokenizerState names the tokenizer states a caller may start in (the
// html5lib "initialStates" plus the CDATA section state).
type TokenizerState uint8

// Externally selectable tokenizer states.
const (
	DataState TokenizerState = iota
	RCDATAState
	RAWTEXTState
	ScriptDataState
	PLAINTEXTState
	CDATASectionState
)

// TokenizeOptions configures Tokenize and NewTokenizer.
type TokenizeOptions struct {
	// InitialState is the state the tokenizer starts in (DataState by
	// default).
	InitialState TokenizerState
	// LastStartTag, if non-empty, is taken as the tag name of the last
	// start tag emitted (the "appropriate end tag" check).
	LastStartTag string
	// AllowCDATA makes "<![CDATA[" start a CDATA section, as when the
	// adjusted current node is a foreign element. A standalone tokenizer
	// has no tree, so this is off by default (CDATA becomes a bogus
	// comment, per the spec when there is no adjusted current node).
	AllowCDATA bool
	// MaxAttributes limits attributes per tag (0: DefaultMaxAttributes,
	// negative: unlimited).
	MaxAttributes int
	// MaxInputSize limits the input length in bytes (0:
	// DefaultMaxInputSize, negative: unlimited). Longer input is
	// truncated, with ErrInputSizeLimit.
	MaxInputSize int
	// CharRefLookup selects the named character reference lookup
	// algorithm (CharRefTrie by default).
	CharRefLookup CharRefStrategy
}

// Default limits (spec Part II 26.4, pinned constants of the port).
//
// DefaultMaxDepth bounds the stack of open elements. 512 is the depth
// at which Blink and WebKit stop nesting parser-inserted elements, so
// real pages that render are not cut short, while the work per token of
// the stack scans (scope checks, adoption agency) stays bounded.
// DefaultMaxAttributes bounds the per-tag attribute list and its
// duplicate check. DefaultMaxInputSize (256 MiB) bounds memory: the
// arena and the decoded text grow linearly with the input (the content
// package passes a smaller 8 MiB limit per response).
const (
	DefaultMaxDepth      = 512
	DefaultMaxAttributes = 1024
	DefaultMaxInputSize  = 256 << 20
)

// limitOrDefault maps an Options limit to its value: 0 selects def and a
// negative value means unlimited (the largest int).
func limitOrDefault(v, def int) int {
	switch {
	case v == 0:
		return def
	case v < 0:
		return int(^uint(0) >> 1)
	}
	return v
}

// Tokenize runs the tokenizer alone over input (UTF-8; see Decode) and
// returns every token, ending with the EOF token, and the parse errors
// with line/column filled in.
func Tokenize(input []byte, opts TokenizeOptions) ([]Token, []ParseError) {
	t := NewTokenizer(input, opts)
	var out []Token
	for {
		tok := t.Next()
		out = append(out, *tok)
		if tok.Kind == EOFToken {
			break
		}
	}
	errs := t.Errors()
	fillPositions(t.in, errs)
	return out, errs
}
