package htmlparse

import "unicode/utf8"

// state is a tokenizer state. There is exactly one constant per state in
// HTML §13.2.5 "Tokenization" (including the processing instruction states
// added in 2025), in the order the standard lists them. The constant stateFooBar is the spec's "foo bar state"; its handler is the
// method fooBarState (or a shared handler parameterized by state), and
// stateNames holds the exact spec names.
type state uint8

const (
	stateData state = iota
	stateRCDATA
	stateRAWTEXT
	stateScriptData
	statePLAINTEXT
	stateTagOpen
	stateEndTagOpen
	stateTagName
	stateRCDATALessThanSign
	stateRCDATAEndTagOpen
	stateRCDATAEndTagName
	stateRAWTEXTLessThanSign
	stateRAWTEXTEndTagOpen
	stateRAWTEXTEndTagName
	stateScriptDataLessThanSign
	stateScriptDataEndTagOpen
	stateScriptDataEndTagName
	stateScriptDataEscapeStart
	stateScriptDataEscapeStartDash
	stateScriptDataEscaped
	stateScriptDataEscapedDash
	stateScriptDataEscapedDashDash
	stateScriptDataEscapedLessThanSign
	stateScriptDataEscapedEndTagOpen
	stateScriptDataEscapedEndTagName
	stateScriptDataDoubleEscapeStart
	stateScriptDataDoubleEscaped
	stateScriptDataDoubleEscapedDash
	stateScriptDataDoubleEscapedDashDash
	stateScriptDataDoubleEscapedLessThanSign
	stateScriptDataDoubleEscapeEnd
	stateBeforeAttributeName
	stateAttributeName
	stateAfterAttributeName
	stateBeforeAttributeValue
	stateAttributeValueDoubleQuoted
	stateAttributeValueSingleQuoted
	stateAttributeValueUnquoted
	stateAfterAttributeValueQuoted
	stateSelfClosingStartTag
	stateBogusComment
	stateMarkupDeclarationOpen
	stateCommentStart
	stateCommentStartDash
	stateComment
	stateCommentLessThanSign
	stateCommentLessThanSignBang
	stateCommentLessThanSignBangDash
	stateCommentLessThanSignBangDashDash
	stateCommentEndDash
	stateCommentEnd
	stateCommentEndBang
	stateDOCTYPE
	stateBeforeDOCTYPEName
	stateDOCTYPEName
	stateAfterDOCTYPEName
	stateAfterDOCTYPEPublicKeyword
	stateBeforeDOCTYPEPublicIdentifier
	stateDOCTYPEPublicIdentifierDoubleQuoted
	stateDOCTYPEPublicIdentifierSingleQuoted
	stateAfterDOCTYPEPublicIdentifier
	stateBetweenDOCTYPEPublicAndSystemIdentifiers
	stateAfterDOCTYPESystemKeyword
	stateBeforeDOCTYPESystemIdentifier
	stateDOCTYPESystemIdentifierDoubleQuoted
	stateDOCTYPESystemIdentifierSingleQuoted
	stateAfterDOCTYPESystemIdentifier
	stateBogusDOCTYPE
	stateCDATASection
	stateCDATASectionBracket
	stateCDATASectionEnd
	stateProcessingInstructionOpen
	stateProcessingInstructionTarget
	stateAfterProcessingInstructionTarget
	stateProcessingInstructionData
	stateProcessingInstructionQuestionable
	stateCharacterReference
	stateNamedCharacterReference
	stateAmbiguousAmpersand
	stateNumericCharacterReference
	stateHexadecimalCharacterReferenceStart
	stateHexadecimalCharacterReference
	stateDecimalCharacterReference
	stateNumericCharacterReferenceEnd
	numStates
)

// stateNames are the exact spec state names (without the trailing
// "state"), for diagnostics.
var stateNames = [numStates]string{
	stateData: "data", stateRCDATA: "RCDATA", stateRAWTEXT: "RAWTEXT", stateScriptData: "script data",
	statePLAINTEXT: "PLAINTEXT", stateTagOpen: "tag open", stateEndTagOpen: "end tag open", stateTagName: "tag name",
	stateRCDATALessThanSign: "RCDATA less-than sign", stateRCDATAEndTagOpen: "RCDATA end tag open",
	stateRCDATAEndTagName: "RCDATA end tag name", stateRAWTEXTLessThanSign: "RAWTEXT less-than sign",
	stateRAWTEXTEndTagOpen: "RAWTEXT end tag open", stateRAWTEXTEndTagName: "RAWTEXT end tag name",
	stateScriptDataLessThanSign: "script data less-than sign", stateScriptDataEndTagOpen: "script data end tag open",
	stateScriptDataEndTagName: "script data end tag name", stateScriptDataEscapeStart: "script data escape start",
	stateScriptDataEscapeStartDash: "script data escape start dash", stateScriptDataEscaped: "script data escaped",
	stateScriptDataEscapedDash: "script data escaped dash", stateScriptDataEscapedDashDash: "script data escaped dash dash",
	stateScriptDataEscapedLessThanSign:       "script data escaped less-than sign",
	stateScriptDataEscapedEndTagOpen:         "script data escaped end tag open",
	stateScriptDataEscapedEndTagName:         "script data escaped end tag name",
	stateScriptDataDoubleEscapeStart:         "script data double escape start",
	stateScriptDataDoubleEscaped:             "script data double escaped",
	stateScriptDataDoubleEscapedDash:         "script data double escaped dash",
	stateScriptDataDoubleEscapedDashDash:     "script data double escaped dash dash",
	stateScriptDataDoubleEscapedLessThanSign: "script data double escaped less-than sign",
	stateScriptDataDoubleEscapeEnd:           "script data double escape end",
	stateBeforeAttributeName:                 "before attribute name", stateAttributeName: "attribute name",
	stateAfterAttributeName: "after attribute name", stateBeforeAttributeValue: "before attribute value",
	stateAttributeValueDoubleQuoted: "attribute value (double-quoted)",
	stateAttributeValueSingleQuoted: "attribute value (single-quoted)",
	stateAttributeValueUnquoted:     "attribute value (unquoted)",
	stateAfterAttributeValueQuoted:  "after attribute value (quoted)",
	stateSelfClosingStartTag:        "self-closing start tag", stateBogusComment: "bogus comment",
	stateMarkupDeclarationOpen: "markup declaration open", stateCommentStart: "comment start",
	stateCommentStartDash: "comment start dash", stateComment: "comment",
	stateCommentLessThanSign: "comment less-than sign", stateCommentLessThanSignBang: "comment less-than sign bang",
	stateCommentLessThanSignBangDash:     "comment less-than sign bang dash",
	stateCommentLessThanSignBangDashDash: "comment less-than sign bang dash dash",
	stateCommentEndDash:                  "comment end dash", stateCommentEnd: "comment end", stateCommentEndBang: "comment end bang",
	stateDOCTYPE: "DOCTYPE", stateBeforeDOCTYPEName: "before DOCTYPE name", stateDOCTYPEName: "DOCTYPE name",
	stateAfterDOCTYPEName: "after DOCTYPE name", stateAfterDOCTYPEPublicKeyword: "after DOCTYPE public keyword",
	stateBeforeDOCTYPEPublicIdentifier:            "before DOCTYPE public identifier",
	stateDOCTYPEPublicIdentifierDoubleQuoted:      "DOCTYPE public identifier (double-quoted)",
	stateDOCTYPEPublicIdentifierSingleQuoted:      "DOCTYPE public identifier (single-quoted)",
	stateAfterDOCTYPEPublicIdentifier:             "after DOCTYPE public identifier",
	stateBetweenDOCTYPEPublicAndSystemIdentifiers: "between DOCTYPE public and system identifiers",
	stateAfterDOCTYPESystemKeyword:                "after DOCTYPE system keyword",
	stateBeforeDOCTYPESystemIdentifier:            "before DOCTYPE system identifier",
	stateDOCTYPESystemIdentifierDoubleQuoted:      "DOCTYPE system identifier (double-quoted)",
	stateDOCTYPESystemIdentifierSingleQuoted:      "DOCTYPE system identifier (single-quoted)",
	stateAfterDOCTYPESystemIdentifier:             "after DOCTYPE system identifier",
	stateBogusDOCTYPE:                             "bogus DOCTYPE", stateCDATASection: "CDATA section",
	stateCDATASectionBracket: "CDATA section bracket", stateCDATASectionEnd: "CDATA section end",
	stateProcessingInstructionOpen:         "processing instruction open",
	stateProcessingInstructionTarget:       "processing instruction target",
	stateAfterProcessingInstructionTarget:  "after processing instruction target",
	stateProcessingInstructionData:         "processing instruction data",
	stateProcessingInstructionQuestionable: "processing instruction questionable",
	stateCharacterReference:                "character reference", stateNamedCharacterReference: "named character reference",
	stateAmbiguousAmpersand: "ambiguous ampersand", stateNumericCharacterReference: "numeric character reference",
	stateHexadecimalCharacterReferenceStart: "hexadecimal character reference start",
	stateHexadecimalCharacterReference:      "hexadecimal character reference",
	stateDecimalCharacterReference:          "decimal character reference",
	stateNumericCharacterReferenceEnd:       "numeric character reference end",
}

func (s state) String() string {
	if s < numStates {
		return stateNames[s]
	}
	return "state?"
}

// eofRune is the conceptual EOF "character".
const eofRune rune = -1

// Tokenizer is the HTML tokenizer state machine. It reads UTF-8 input
// (see Decode for other encodings) and produces tokens one at a time.
// The tree builder drives it through SetState, SetAllowCDATA and
// SetLastStartTag between tokens, as §13.2.6 requires.
type Tokenizer struct {
	in       []byte
	pos      int // offset of the next input character
	curStart int // offset of the current input character
	hwm      int // input-stream errors have been reported below this offset

	state       state
	returnState state

	// The tag, comment, DOCTYPE or PI token being built.
	tagKind     TokenKind
	name        []byte
	selfClosing bool
	attrs       []Attr
	attrName    []byte
	attrValue   []byte
	attrOpen    bool                // an attribute is being built
	attrDrop    bool                // the current attribute is a duplicate (or over the limit)
	attrAdded   bool                // the current attribute has been appended to attrs
	attrIndex   map[string]struct{} // duplicate check for tags with many attributes (lookup only)
	attrLimited bool
	data        []byte // comment data, PI data
	pubID       []byte
	sysID       []byte
	hasName     bool
	hasPub      bool
	hasSys      bool
	forceQuirks bool

	temp    []byte // the temporary buffer
	refCode int32  // character reference code (saturates above 0x10FFFF)

	text     []byte // pending character data
	textEnd  int    // end offset of the pending character data
	tokStart int    // start offset of the next token

	lastStartTag string
	allowCDATA   bool
	maxAttrs     int
	charRef      CharRefStrategy

	queue [4]Token
	qhead int
	qlen  int
	cur   Token
	done  bool

	errs []ParseError
}

// NewTokenizer returns a tokenizer over input.
func NewTokenizer(input []byte, opts TokenizeOptions) *Tokenizer {
	t := &Tokenizer{
		maxAttrs:     limitOrDefault(opts.MaxAttributes, DefaultMaxAttributes),
		lastStartTag: opts.LastStartTag,
		allowCDATA:   opts.AllowCDATA,
		charRef:      opts.CharRefLookup,
	}
	if max := limitOrDefault(opts.MaxInputSize, DefaultMaxInputSize); len(input) > max {
		input = input[:max]
		t.errs = append(t.errs, ParseError{Code: ErrInputSizeLimit, Offset: max})
	}
	t.in = input
	t.SetState(opts.InitialState)
	return t
}

// SetState switches the tokenizer to one of the externally selectable
// states. The tree builder calls it after a start tag token for title,
// textarea, style, script, plaintext, ... (§13.2.6.2, §13.2.6.4).
func (t *Tokenizer) SetState(s TokenizerState) {
	switch s {
	case RCDATAState:
		t.state = stateRCDATA
	case RAWTEXTState:
		t.state = stateRAWTEXT
	case ScriptDataState:
		t.state = stateScriptData
	case PLAINTEXTState:
		t.state = statePLAINTEXT
	case CDATASectionState:
		t.state = stateCDATASection
	default:
		t.state = stateData
	}
}

// SetAllowCDATA tells the markup declaration open state whether there is
// an adjusted current node that is not in the HTML namespace.
func (t *Tokenizer) SetAllowCDATA(v bool) { t.allowCDATA = v }

// SetLastStartTag overrides the name of the last emitted start tag.
func (t *Tokenizer) SetLastStartTag(name string) { t.lastStartTag = name }

// Errors returns the parse errors reported so far (Line/Col not filled).
func (t *Tokenizer) Errors() []ParseError { return t.errs }

// Input returns the (possibly truncated) input buffer.
func (t *Tokenizer) Input() []byte { return t.in }

// Next returns the next token. The returned pointer is valid until the
// following call. After the EOF token Next keeps returning EOF tokens.
//
// PORT: the pointer aliases a buffer inside the tokenizer; in Rust return
// an owned Token (or a borrow tied to &mut self).
func (t *Tokenizer) Next() *Token {
	for t.qlen == 0 {
		if t.done {
			t.cur = Token{Kind: EOFToken, Span: Span{len(t.in), len(t.in)}}
			return &t.cur
		}
		t.step()
	}
	t.cur = t.queue[t.qhead]
	t.queue[t.qhead] = Token{}
	t.qhead = (t.qhead + 1) % len(t.queue)
	t.qlen--
	return &t.cur
}

// --- input reading ---

// next consumes and returns the next input character (eofRune at the
// end), applying newline normalization and reporting input-stream errors
// the first time each character is read.
func (t *Tokenizer) next() rune {
	t.curStart = t.pos
	if t.pos >= len(t.in) {
		return eofRune
	}
	b := t.in[t.pos]
	if b < utf8.RuneSelf {
		t.pos++
		if b == '\r' {
			if t.pos < len(t.in) && t.in[t.pos] == '\n' {
				t.pos++
			}
			if t.pos > t.hwm {
				t.hwm = t.pos
			}
			return '\n'
		}
		if t.pos > t.hwm {
			t.hwm = t.pos
			if (b < 0x20 && b != 0 && b != '\t' && b != '\n' && b != '\f') || b == 0x7F {
				t.errAt(ErrControlCharacterInInputStream, t.curStart)
			}
		}
		return rune(b)
	}
	r, w := decodeRune(t.in[t.pos:])
	t.pos += w
	fresh := t.pos > t.hwm
	if fresh {
		t.hwm = t.pos
	}
	switch {
	case isSurrogate(r):
		if fresh {
			t.errAt(ErrSurrogateInInputStream, t.curStart)
		}
		r = utf8.RuneError
	case isNoncharacter(r):
		if fresh {
			t.errAt(ErrNoncharacterInInputStream, t.curStart)
		}
	case r >= 0x80 && r <= 0x9F:
		if fresh {
			t.errAt(ErrControlCharacterInInputStream, t.curStart)
		}
	}
	return r
}

// reconsume makes the current input character the next one again.
func (t *Tokenizer) reconsume() { t.pos = t.curStart }

// skipASCII advances over n already-inspected ASCII bytes (used by the
// "if the next few characters are" lookahead steps).
func (t *Tokenizer) skipASCII(n int) {
	t.curStart = t.pos + n - 1 // the last consumed character is current
	t.pos += n
	if t.pos > t.hwm {
		t.hwm = t.pos
	}
}

// peekFold reports whether the input at offset at starts with word,
// ASCII case-insensitively (word must be lower-case ASCII). It consumes
// nothing: the "if the next few characters are" steps use it.
func (t *Tokenizer) peekFold(at int, word string) bool {
	if len(t.in)-at < len(word) {
		return false
	}
	for i := 0; i < len(word); i++ {
		c := t.in[at+i]
		if c >= 'A' && c <= 'Z' {
			c += 0x20
		}
		if c != word[i] {
			return false
		}
	}
	return true
}

func (t *Tokenizer) err(code ErrorCode) { t.errAt(code, t.curStart) }

func (t *Tokenizer) errAt(code ErrorCode, off int) {
	t.errs = append(t.errs, ParseError{Code: code, Offset: off})
}

// --- emitting ---

// emitRune appends a character to the pending character data.
func (t *Tokenizer) emitRune(c rune) {
	if c < utf8.RuneSelf {
		t.text = append(t.text, byte(c))
	} else {
		t.text = utf8.AppendRune(t.text, c)
	}
	t.textEnd = t.pos
}

func (t *Tokenizer) emitString(s string) {
	t.text = append(t.text, s...)
	t.textEnd = t.pos
}

func (t *Tokenizer) emitBytes(b []byte) {
	t.text = append(t.text, b...)
	t.textEnd = t.pos
}

func (t *Tokenizer) push(tok Token) {
	t.queue[(t.qhead+t.qlen)%len(t.queue)] = tok
	t.qlen++
}

// flushText queues the pending character data as one character token.
func (t *Tokenizer) flushText() {
	if len(t.text) == 0 {
		return
	}
	t.push(Token{Kind: CharacterToken, Data: string(t.text), Span: Span{t.tokStart, t.textEnd}})
	t.tokStart = t.textEnd
	t.text = t.text[:0]
}

// emitToken queues tok (after any pending text) with span
// [tokStart, pos).
func (t *Tokenizer) emitToken(tok Token) {
	t.flushText()
	tok.Span = Span{t.tokStart, t.pos}
	t.tokStart = t.pos
	t.push(tok)
}

// emitEOF queues the pending text and the EOF token.
func (t *Tokenizer) emitEOF() {
	t.flushText()
	t.push(Token{Kind: EOFToken, Span: Span{t.tokStart, len(t.in)}})
	t.tokStart = len(t.in)
	t.done = true
}

// --- tag building ---

func (t *Tokenizer) newTag(kind TokenKind) {
	t.tagKind = kind
	t.name = t.name[:0]
	t.selfClosing = false
	t.attrs = nil
	t.attrOpen = false
	t.attrIndex = nil
	t.attrLimited = false
}

// startAttribute begins a new attribute (after committing the previous).
func (t *Tokenizer) startAttribute() {
	t.commitAttribute()
	t.attrName = t.attrName[:0]
	t.attrValue = t.attrValue[:0]
	t.attrOpen = true
	t.attrDrop = false
	t.attrAdded = false
}

// leaveAttributeName runs the duplicate-attribute check that the spec
// attaches to leaving the attribute name state (§13.2.5.33).
func (t *Tokenizer) leaveAttributeName() {
	if !t.attrOpen || t.attrDrop || t.attrAdded {
		return
	}
	if t.hasAttr(t.attrName) {
		t.err(ErrDuplicateAttribute)
		t.attrDrop = true
		return
	}
	if len(t.attrs) >= t.maxAttrs {
		if !t.attrLimited {
			t.err(ErrAttributeLimit)
			t.attrLimited = true
		}
		t.attrDrop = true
		return
	}
	// Reserve the name now so later duplicates are detected even before
	// this attribute's value is complete.
	name := internAttrName(t.attrName)
	t.attrs = append(t.attrs, Attr{Name: name})
	t.attrAdded = true
	if t.attrIndex != nil {
		t.attrIndex[name] = struct{}{}
	} else if len(t.attrs) > 16 {
		t.attrIndex = make(map[string]struct{}, 2*len(t.attrs))
		for _, a := range t.attrs {
			t.attrIndex[a.Name] = struct{}{}
		}
	}
}

func (t *Tokenizer) hasAttr(name []byte) bool {
	if t.attrIndex != nil {
		_, ok := t.attrIndex[string(name)]
		return ok
	}
	for i := range t.attrs {
		if t.attrs[i].Name == string(name) {
			return true
		}
	}
	return false
}

// commitAttribute stores the current attribute's value.
func (t *Tokenizer) commitAttribute() {
	if !t.attrOpen {
		return
	}
	t.attrOpen = false
	if t.attrAdded {
		t.attrs[len(t.attrs)-1].Value = string(t.attrValue)
	}
}

// inAttributeValue reports "consumed as part of an attribute".
func (t *Tokenizer) inAttributeValue() bool {
	switch t.returnState {
	case stateAttributeValueDoubleQuoted, stateAttributeValueSingleQuoted, stateAttributeValueUnquoted:
		return true
	}
	return false
}

// emitTag emits the current tag token.
func (t *Tokenizer) emitTag() {
	t.commitAttribute()
	atom := lookupTag(t.name)
	var name string
	if atom != tagUnknown {
		name = tagNames[atom]
	} else {
		name = string(t.name)
	}
	tok := Token{Kind: t.tagKind, Name: name, Attrs: t.attrs, SelfClosing: t.selfClosing, atom: atom}
	if t.tagKind == StartTagToken {
		t.lastStartTag = name
	} else {
		if len(t.attrs) > 0 {
			t.err(ErrEndTagWithAttributes)
		}
		if t.selfClosing {
			t.err(ErrEndTagWithTrailingSolidus)
		}
	}
	t.attrs = nil
	t.emitToken(tok)
}

// appropriateEndTag reports whether the current end tag token is an
// appropriate end tag token.
func (t *Tokenizer) appropriateEndTag() bool {
	return t.lastStartTag != "" && string(t.name) == t.lastStartTag
}

func (t *Tokenizer) emitComment() {
	t.emitToken(Token{Kind: CommentToken, Data: string(t.data)})
}

func (t *Tokenizer) newDoctype() {
	t.name = t.name[:0]
	t.pubID = t.pubID[:0]
	t.sysID = t.sysID[:0]
	t.hasName, t.hasPub, t.hasSys, t.forceQuirks = false, false, false, false
}

func (t *Tokenizer) emitDoctype() {
	tok := Token{Kind: DoctypeToken, HasName: t.hasName, HasPublicID: t.hasPub, HasSystemID: t.hasSys, ForceQuirks: t.forceQuirks}
	if t.hasName {
		tok.Name = string(t.name)
	}
	if t.hasPub {
		tok.PublicID = string(t.pubID)
	}
	if t.hasSys {
		tok.SystemID = string(t.sysID)
	}
	t.emitToken(tok)
}

// internAttrName returns a string for an attribute name, without
// allocating for the most common names.
//
// PORT: Go compiles switch string(b) to comparisons that do not allocate;
// in Rust match on the &[u8] (or intern into a string pool).
func internAttrName(b []byte) string {
	switch string(b) {
	case "class":
		return "class"
	case "id":
		return "id"
	case "href":
		return "href"
	case "src":
		return "src"
	case "style":
		return "style"
	case "type":
		return "type"
	case "name":
		return "name"
	case "value":
		return "value"
	case "rel":
		return "rel"
	case "content":
		return "content"
	case "alt":
		return "alt"
	case "title":
		return "title"
	case "width":
		return "width"
	case "height":
		return "height"
	case "action":
		return "action"
	case "method":
		return "method"
	case "charset":
		return "charset"
	case "lang":
		return "lang"
	case "target":
		return "target"
	case "data":
		return "data"
	}
	return string(b)
}

// step runs one state handler. Handlers consume one or more characters;
// any handler that emits a non-character token returns right after it,
// so the tree builder can switch states before the next character.
func (t *Tokenizer) step() {
	switch t.state {
	case stateData:
		t.dataState()
	case stateRCDATA:
		t.rcdataState()
	case stateRAWTEXT:
		t.rawtextState()
	case stateScriptData:
		t.scriptDataState()
	case statePLAINTEXT:
		t.plaintextState()
	case stateTagOpen:
		t.tagOpenState()
	case stateEndTagOpen:
		t.endTagOpenState()
	case stateTagName:
		t.tagNameState()
	case stateRCDATALessThanSign:
		t.textLessThanSignState(stateRCDATAEndTagOpen, stateRCDATA)
	case stateRCDATAEndTagOpen:
		t.textEndTagOpenState(stateRCDATAEndTagName, stateRCDATA)
	case stateRCDATAEndTagName:
		t.textEndTagNameState(stateRCDATA)
	case stateRAWTEXTLessThanSign:
		t.textLessThanSignState(stateRAWTEXTEndTagOpen, stateRAWTEXT)
	case stateRAWTEXTEndTagOpen:
		t.textEndTagOpenState(stateRAWTEXTEndTagName, stateRAWTEXT)
	case stateRAWTEXTEndTagName:
		t.textEndTagNameState(stateRAWTEXT)
	case stateScriptDataLessThanSign:
		t.scriptDataLessThanSignState()
	case stateScriptDataEndTagOpen:
		t.textEndTagOpenState(stateScriptDataEndTagName, stateScriptData)
	case stateScriptDataEndTagName:
		t.textEndTagNameState(stateScriptData)
	case stateScriptDataEscapeStart:
		t.scriptDataEscapeStartState()
	case stateScriptDataEscapeStartDash:
		t.scriptDataEscapeStartDashState()
	case stateScriptDataEscaped:
		t.scriptDataEscapedState()
	case stateScriptDataEscapedDash:
		t.scriptDataEscapedDashState()
	case stateScriptDataEscapedDashDash:
		t.scriptDataEscapedDashDashState()
	case stateScriptDataEscapedLessThanSign:
		t.scriptDataEscapedLessThanSignState()
	case stateScriptDataEscapedEndTagOpen:
		t.textEndTagOpenState(stateScriptDataEscapedEndTagName, stateScriptDataEscaped)
	case stateScriptDataEscapedEndTagName:
		t.textEndTagNameState(stateScriptDataEscaped)
	case stateScriptDataDoubleEscapeStart:
		t.scriptDataDoubleEscapeStartState()
	case stateScriptDataDoubleEscaped:
		t.scriptDataDoubleEscapedState()
	case stateScriptDataDoubleEscapedDash:
		t.scriptDataDoubleEscapedDashState()
	case stateScriptDataDoubleEscapedDashDash:
		t.scriptDataDoubleEscapedDashDashState()
	case stateScriptDataDoubleEscapedLessThanSign:
		t.scriptDataDoubleEscapedLessThanSignState()
	case stateScriptDataDoubleEscapeEnd:
		t.scriptDataDoubleEscapeEndState()
	case stateBeforeAttributeName:
		t.beforeAttributeNameState()
	case stateAttributeName:
		t.attributeNameState()
	case stateAfterAttributeName:
		t.afterAttributeNameState()
	case stateBeforeAttributeValue:
		t.beforeAttributeValueState()
	case stateAttributeValueDoubleQuoted:
		t.attributeValueQuotedState('"', stateAttributeValueDoubleQuoted)
	case stateAttributeValueSingleQuoted:
		t.attributeValueQuotedState('\'', stateAttributeValueSingleQuoted)
	case stateAttributeValueUnquoted:
		t.attributeValueUnquotedState()
	case stateAfterAttributeValueQuoted:
		t.afterAttributeValueQuotedState()
	case stateSelfClosingStartTag:
		t.selfClosingStartTagState()
	case stateBogusComment:
		t.bogusCommentState()
	case stateMarkupDeclarationOpen:
		t.markupDeclarationOpenState()
	case stateCommentStart:
		t.commentStartState()
	case stateCommentStartDash:
		t.commentStartDashState()
	case stateComment:
		t.commentState()
	case stateCommentLessThanSign:
		t.commentLessThanSignState()
	case stateCommentLessThanSignBang:
		t.commentLessThanSignBangState()
	case stateCommentLessThanSignBangDash:
		t.commentLessThanSignBangDashState()
	case stateCommentLessThanSignBangDashDash:
		t.commentLessThanSignBangDashDashState()
	case stateCommentEndDash:
		t.commentEndDashState()
	case stateCommentEnd:
		t.commentEndState()
	case stateCommentEndBang:
		t.commentEndBangState()
	case stateDOCTYPE:
		t.doctypeState()
	case stateBeforeDOCTYPEName:
		t.beforeDOCTYPENameState()
	case stateDOCTYPEName:
		t.doctypeNameState()
	case stateAfterDOCTYPEName:
		t.afterDOCTYPENameState()
	case stateAfterDOCTYPEPublicKeyword:
		t.afterDOCTYPEKeywordState(true)
	case stateBeforeDOCTYPEPublicIdentifier:
		t.beforeDOCTYPEIdentifierState(true)
	case stateDOCTYPEPublicIdentifierDoubleQuoted:
		t.doctypeIdentifierQuotedState(true, '"')
	case stateDOCTYPEPublicIdentifierSingleQuoted:
		t.doctypeIdentifierQuotedState(true, '\'')
	case stateAfterDOCTYPEPublicIdentifier:
		t.afterDOCTYPEPublicIdentifierState()
	case stateBetweenDOCTYPEPublicAndSystemIdentifiers:
		t.betweenDOCTYPEPublicAndSystemIdentifiersState()
	case stateAfterDOCTYPESystemKeyword:
		t.afterDOCTYPEKeywordState(false)
	case stateBeforeDOCTYPESystemIdentifier:
		t.beforeDOCTYPEIdentifierState(false)
	case stateDOCTYPESystemIdentifierDoubleQuoted:
		t.doctypeIdentifierQuotedState(false, '"')
	case stateDOCTYPESystemIdentifierSingleQuoted:
		t.doctypeIdentifierQuotedState(false, '\'')
	case stateAfterDOCTYPESystemIdentifier:
		t.afterDOCTYPESystemIdentifierState()
	case stateBogusDOCTYPE:
		t.bogusDOCTYPEState()
	case stateCDATASection:
		t.cdataSectionState()
	case stateCDATASectionBracket:
		t.cdataSectionBracketState()
	case stateCDATASectionEnd:
		t.cdataSectionEndState()
	case stateProcessingInstructionOpen:
		t.processingInstructionOpenState()
	case stateProcessingInstructionTarget:
		t.processingInstructionTargetState()
	case stateAfterProcessingInstructionTarget:
		t.afterProcessingInstructionTargetState()
	case stateProcessingInstructionData:
		t.processingInstructionDataState()
	case stateProcessingInstructionQuestionable:
		t.processingInstructionQuestionableState()
	case stateCharacterReference:
		t.characterReferenceState()
	case stateNamedCharacterReference:
		t.namedCharacterReferenceState()
	case stateAmbiguousAmpersand:
		t.ambiguousAmpersandState()
	case stateNumericCharacterReference:
		t.numericCharacterReferenceState()
	case stateHexadecimalCharacterReferenceStart:
		t.hexadecimalCharacterReferenceStartState()
	case stateHexadecimalCharacterReference:
		t.hexadecimalCharacterReferenceState()
	case stateDecimalCharacterReference:
		t.decimalCharacterReferenceState()
	case stateNumericCharacterReferenceEnd:
		t.numericCharacterReferenceEndState()
	default:
		// Unreachable: every state constant is handled above.
		t.state = stateData
	}
}
