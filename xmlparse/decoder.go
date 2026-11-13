package xmlparse

import (
	"bytes"
	"io"
	"unicode/utf16"
	"unicode/utf8"
)

// Options configure the decoder. The zero value uses the defaults.
type Options struct {
	// MaxDepth bounds element nesting; the root element has depth 1. 0
	// means DefaultMaxDepth.
	MaxDepth int
	// MaxInputBytes bounds the input read. 0 means DefaultMaxInputBytes.
	MaxInputBytes int
}

// Default limits, identical in every port (V2_DESIGN parser assurance
// bounds).
const (
	DefaultMaxDepth      = 64
	DefaultMaxInputBytes = 8 << 20
)

// Namespace names bound by definition (Namespaces in XML 1.0 §3).
const (
	XMLNamespace   = "http://www.w3.org/XML/1998/namespace"
	XMLNSNamespace = "http://www.w3.org/2000/xmlns/"
)

// Name is a namespace-resolved qualified name. Space is the namespace URI
// ("" for none); Prefix is the prefix as written.
type Name struct {
	Space, Prefix, Local string
}

// QName returns the name as written: "prefix:local" or "local".
func (n Name) QName() string {
	if n.Prefix == "" {
		return n.Local
	}
	return n.Prefix + ":" + n.Local
}

// Attr is one attribute with its normalized value (XML 1.0 §3.3.3, all
// attributes being CDATA without a DTD). Namespace declarations are kept,
// in the XMLNSNamespace.
type Attr struct {
	Name       Name
	Value      string
	Start, End int32 // span of name="value"
}

// EventKind is the type of a decoder event.
type EventKind uint8

// Event kinds.
const (
	EventNone         EventKind = iota
	EventXMLDecl                // [23] XMLDecl
	EventDoctype                // [28] doctypedecl (bare: name only)
	EventStartElement           // [40] STag or [44] EmptyElemTag
	EventEndElement             // [42] ETag, or the end of an EmptyElemTag
	EventText                   // [14] CharData with references resolved
	EventCData                  // [18] CDSect
	EventComment                // [15] Comment
	EventPI                     // [16] PI
)

var eventNames = [...]string{"none", "xml-decl", "doctype", "start", "end", "text", "cdata", "comment", "pi"}

// String returns the stable name of k.
func (k EventKind) String() string {
	if int(k) < len(eventNames) {
		return eventNames[k]
	}
	return "event?"
}

// Event is one decoder event. Attrs is reused by the next call to Next.
type Event struct {
	Kind EventKind
	// Start and End are the byte span of the construct.
	Start, End int32
	// Name is the element name, the PI target (Local only) or the
	// doctype name.
	Name  Name
	Attrs []Attr
	// Text is character data, CDATA content, comment text or PI data,
	// line endings normalized.
	Text string
	// SelfClosing marks both events of an EmptyElemTag.
	SelfClosing bool
	// XML declaration fields.
	Version, Encoding string
	// Standalone is -1 when absent, 0 for "no", 1 for "yes".
	Standalone int8
}

// state is the decoder state. Names follow the XML 1.0 productions each
// state recognizes.
type state uint8

const (
	// stateMisc: [27] Misc between top-level constructs (prolog [22] and
	// epilog); only whitespace, comments, PIs and (in the prolog) one
	// doctypedecl and the root element may appear.
	stateMisc state = iota
	// stateContent: [43] content: CharData, references and markup.
	stateContent
	// stateMarkup: after "<": decides STag/ETag/"!"/"?".
	stateMarkup
	// stateBang: after "<!": Comment, CDSect or doctypedecl keyword.
	stateBang
	// stateComment: [15] Comment body.
	stateComment
	// stateCDSect: [18]-[21] CDSect body.
	stateCDSect
	// statePI: [16] PI target and body; [23] XMLDecl when the target is
	// "xml" at the start of the document.
	statePI
	// stateDoctype: [28] doctypedecl, restricted to a bare name.
	stateDoctype
	// stateSTag: [40] STag / [44] EmptyElemTag with [41] Attribute.
	stateSTag
	// stateETag: [42] ETag.
	stateETag
	// stateDone: the epilog reached EOF.
	stateDone
)

type phase uint8

const (
	phaseProlog phase = iota
	phaseContent
	phaseEpilog
)

type binding struct{ prefix, uri string }

type openElem struct {
	raw    []byte // the QName as written, for ETag matching
	name   Name
	nsMark int
}

type rawAttr struct {
	qStart, qEnd, colon int
	value               string
	start, end          int
}

// Decoder is a pull parser over one document. It is not safe for
// concurrent use.
type Decoder struct {
	src       []byte
	n         int // readable length (after the input limit)
	truncated bool
	utf16     bool
	pos       int
	st        state
	ph        phase
	docStart  int
	atStart   bool
	doctype   bool
	maxDepth  int
	err       *SyntaxError
	open      []openElem
	ns        []binding
	raws      []rawAttr
	attrs     []Attr
	buf       []byte
	pending   *Event
	mstart    int // start of the markup being read
	steps     int
}

// NewDecoder returns a decoder over src. UTF-8 (with or without BOM) is
// read in place; UTF-16 with a BOM is transcoded first. The decoder keeps
// a reference to src until it is done.
func NewDecoder(src []byte, opts Options) *Decoder {
	d := &Decoder{maxDepth: opts.MaxDepth, atStart: true}
	if d.maxDepth <= 0 {
		d.maxDepth = DefaultMaxDepth
	}
	limit := opts.MaxInputBytes
	if limit <= 0 {
		limit = DefaultMaxInputBytes
	}
	if len(src) > limit {
		src = src[:limit]
		d.truncated = true
	}
	switch {
	case len(src) >= 2 && ((src[0] == 0xFE && src[1] == 0xFF) || (src[0] == 0xFF && src[1] == 0xFE)):
		u, bad := transcodeUTF16(src)
		d.utf16 = true
		if bad >= 0 {
			d.src, d.n = u, len(u)
			d.fail(ErrInvalidUTF8, len(u))
			return d
		}
		src = u
	case len(src) >= 3 && src[0] == 0xEF && src[1] == 0xBB && src[2] == 0xBF:
		d.docStart = 3
	}
	d.src, d.n, d.pos = src, len(src), d.docStart
	return d
}

// transcodeUTF16 converts BOM-prefixed UTF-16 to UTF-8 (without the BOM).
// It returns the source byte offset of the first unpaired surrogate, or
// -1; a high surrogate cut by the end is dropped (an EOF condition). On a
// bad surrogate the caller reports invalid_utf8 at the end of the UTF-8
// produced so far.
func transcodeUTF16(src []byte) ([]byte, int) {
	be := src[0] == 0xFE
	units := make([]uint16, 0, len(src)/2)
	for i := 2; i+1 < len(src); i += 2 {
		if be {
			units = append(units, uint16(src[i])<<8|uint16(src[i+1]))
		} else {
			units = append(units, uint16(src[i+1])<<8|uint16(src[i]))
		}
	}
	out := make([]byte, 0, len(units)*2)
	for i := 0; i < len(units); i++ {
		u := units[i]
		switch {
		case u >= 0xD800 && u <= 0xDBFF:
			if i+1 < len(units) && units[i+1] >= 0xDC00 && units[i+1] <= 0xDFFF {
				out = utf8.AppendRune(out, utf16.DecodeRune(rune(u), rune(units[i+1])))
				i++
				continue
			}
			if i+1 >= len(units) {
				return out, -1 // truncated pair: an EOF condition
			}
			return out, 2 + 2*i
		case u >= 0xDC00 && u <= 0xDFFF:
			return out, 2 + 2*i
		}
		out = utf8.AppendRune(out, rune(u))
	}
	return out, -1
}

// UTF16 reports whether the input was UTF-16 (offsets then index the
// transcoded UTF-8).
func (d *Decoder) UTF16() bool { return d.utf16 }

// InputOffset returns the offset of the next unread byte.
func (d *Decoder) InputOffset() int { return d.pos }

// Depth returns the number of open elements.
func (d *Decoder) Depth() int { return len(d.open) }

// Err returns the sticky error, if any.
func (d *Decoder) Err() *SyntaxError { return d.err }

func (d *Decoder) fail(code ErrorCode, at int) {
	if d.err != nil {
		return
	}
	line, col := 1, 1
	for i := 0; i < at && i < len(d.src); i++ {
		switch d.src[i] {
		case '\n':
			if i > 0 && d.src[i-1] == '\r' {
				continue
			}
			line, col = line+1, 1
		case '\r':
			line, col = line+1, 1
		default:
			col++
		}
	}
	d.err = &SyntaxError{Code: code, Offset: at, Line: line, Column: col}
}

// failAt records code at i unless the character at i is itself invalid
// (that error wins) or cut by the end of input (an EOF condition).
func (d *Decoder) failAt(code ErrorCode, i int) {
	if _, w := d.char(i); w > 0 {
		d.fail(code, i)
	} else if w == 0 {
		d.eof()
	}
}

// eof records the error for running out of input in the current state.
func (d *Decoder) eof() {
	if d.truncated {
		d.fail(ErrInputTooLarge, d.n)
		return
	}
	if d.ph == phaseProlog && (d.st == stateMisc) {
		d.fail(ErrNoRootElement, d.n)
		return
	}
	d.fail(ErrUnexpectedEOF, d.n)
}

// Next returns the next event, io.EOF after a complete document, or the
// first *SyntaxError (returned again on every later call).
func (d *Decoder) Next() (Event, error) {
	if d.err != nil {
		return Event{}, d.err
	}
	if d.pending != nil {
		ev := *d.pending
		d.pending = nil
		return ev, nil
	}
	for {
		d.steps++
		var ev Event
		var ok bool
		switch d.st {
		case stateDone:
			return Event{}, io.EOF
		case stateMisc:
			ok = d.misc()
		case stateContent:
			ev, ok = d.content()
		case stateMarkup:
			d.markup()
		case stateBang:
			d.bang()
		case stateComment:
			ev, ok = d.comment()
		case stateCDSect:
			ev, ok = d.cdsect()
		case statePI:
			ev, ok = d.pi()
		case stateDoctype:
			ev, ok = d.doctypedecl()
		case stateSTag:
			ev, ok = d.stag()
		case stateETag:
			ev, ok = d.etag()
		}
		if d.err != nil {
			return Event{}, d.err
		}
		if ok && ev.Kind != EventNone {
			return ev, nil
		}
		if ok && d.st == stateDone {
			return Event{}, io.EOF
		}
	}
}

// Character classification.

var asciiChar = func() (t [128]bool) {
	t[0x09], t[0x0A], t[0x0D] = true, true, true
	for c := 0x20; c < 0x80; c++ {
		t[c] = true
	}
	return
}()

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// isNameStart is [4] NameStartChar.
func isNameStart(r rune) bool {
	switch {
	case r < 0x80:
		return r == ':' || r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
	case r >= 0xC0 && r <= 0xD6, r >= 0xD8 && r <= 0xF6, r >= 0xF8 && r <= 0x2FF,
		r >= 0x370 && r <= 0x37D, r >= 0x37F && r <= 0x1FFF, r >= 0x200C && r <= 0x200D,
		r >= 0x2070 && r <= 0x218F, r >= 0x2C00 && r <= 0x2FEF, r >= 0x3001 && r <= 0xD7FF,
		r >= 0xF900 && r <= 0xFDCF, r >= 0xFDF0 && r <= 0xFFFD, r >= 0x10000 && r <= 0xEFFFF:
		return true
	}
	return false
}

// isNameChar is [4a] NameChar.
func isNameChar(r rune) bool {
	if isNameStart(r) {
		return true
	}
	return r == '-' || r == '.' || (r >= '0' && r <= '9') || r == 0xB7 ||
		(r >= 0x300 && r <= 0x36F) || (r >= 0x203F && r <= 0x2040)
}

func isXMLChar(r rune) bool {
	return r == 0x9 || r == 0xA || r == 0xD || (r >= 0x20 && r <= 0xD7FF) ||
		(r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= 0x10FFFF)
}

// char decodes the Char [2] at i. It returns size 0 at the end of input
// (including a UTF-8 sequence cut by the end). On an invalid byte or
// character it records the error at the first offending byte and returns
// size -1.
func (d *Decoder) char(i int) (rune, int) {
	if i >= d.n {
		return 0, 0
	}
	b0 := d.src[i]
	if b0 < 0x80 {
		if asciiChar[b0] {
			return rune(b0), 1
		}
		if b0 == 0 && !d.utf16 && (i == d.docStart || (i == d.docStart+1 && d.src[d.docStart] == '<')) {
			// "\x00<" or "<\x00": UTF-16 without a byte order mark.
			d.fail(ErrUnsupportedEncoding, i)
			return 0, -1
		}
		d.fail(ErrInvalidChar, i)
		return 0, -1
	}
	need := 0
	lo, hi := byte(0x80), byte(0xBF)
	var r rune
	switch {
	case b0 >= 0xC2 && b0 <= 0xDF:
		need, r = 1, rune(b0&0x1F)
	case b0 == 0xE0:
		need, lo, r = 2, 0xA0, rune(b0&0x0F)
	case (b0 >= 0xE1 && b0 <= 0xEC) || b0 == 0xEE || b0 == 0xEF:
		need, r = 2, rune(b0&0x0F)
	case b0 == 0xED:
		need, hi, r = 2, 0x9F, rune(b0&0x0F)
	case b0 == 0xF0:
		need, lo, r = 3, 0x90, rune(b0&0x07)
	case b0 >= 0xF1 && b0 <= 0xF3:
		need, r = 3, rune(b0&0x07)
	case b0 == 0xF4:
		need, hi, r = 3, 0x8F, rune(b0&0x07)
	default:
		d.fail(ErrInvalidUTF8, i)
		return 0, -1
	}
	for k := 1; k <= need; k++ {
		j := i + k
		if j >= d.n {
			return 0, 0
		}
		c := d.src[j]
		l, h := byte(0x80), byte(0xBF)
		if k == 1 {
			l, h = lo, hi
		}
		if c < l || c > h {
			d.fail(ErrInvalidUTF8, j)
			return 0, -1
		}
		r = r<<6 | rune(c&0x3F)
	}
	if !isXMLChar(r) {
		d.fail(ErrInvalidChar, i)
		return 0, -1
	}
	return r, need + 1
}

type nameMode uint8

const (
	nameAny     nameMode = iota // [5] Name
	nameQName                   // Namespaces [7] QName
	nameNoColon                 // NCName ([4] NCName), e.g. PI targets
)

// scanName reads a name starting at i and returns the offset of the first
// character after it and the offset of its colon (-1 if none). It returns
// end < 0 at EOF or after recording an error.
func (d *Decoder) scanName(i int, mode nameMode) (end, colon int) {
	colon = -1
	r, w := d.char(i)
	if w <= 0 {
		return -1, -1
	}
	if !isNameStart(r) {
		d.fail(ErrInvalidName, i)
		return -1, -1
	}
	if r == ':' && mode != nameAny {
		if mode == nameQName {
			d.fail(ErrInvalidQName, i)
		} else {
			d.fail(ErrInvalidName, i)
		}
		return -1, -1
	}
	j := i + w
	afterColon := false
	for {
		d.steps++
		r, w = d.char(j)
		if w <= 0 {
			return -1, -1
		}
		if afterColon && (r == ':' || !isNameStart(r)) {
			d.fail(ErrInvalidQName, j)
			return -1, -1
		}
		afterColon = false
		if !isNameChar(r) {
			return j, colon
		}
		if r == ':' && mode != nameAny {
			if mode == nameNoColon {
				d.fail(ErrInvalidName, j)
				return -1, -1
			}
			if colon >= 0 {
				d.fail(ErrInvalidQName, j)
				return -1, -1
			}
			colon = j
			afterColon = true
		}
		j += w
	}
}

// skipSpace skips S [3] from i and returns the new offset.
func (d *Decoder) skipSpace(i int) int {
	for i < d.n && isSpace(d.src[i]) {
		d.steps++
		i++
	}
	return i
}

// misc implements stateMisc.
func (d *Decoder) misc() bool {
	i := d.skipSpace(d.pos)
	d.pos = i
	if i >= d.n {
		if d.ph == phaseEpilog && !d.truncated {
			d.st = stateDone
			return true
		}
		d.eof()
		return false
	}
	if d.src[i] == '<' {
		d.mstart = i
		d.pos = i + 1
		d.st = stateMarkup
		return false
	}
	d.failAt(ErrTextOutsideRoot, i)
	return false
}

// markup implements stateMarkup (the character after "<").
func (d *Decoder) markup() {
	i := d.pos
	if i >= d.n {
		d.eof()
		return
	}
	switch c := d.src[i]; c {
	case '?':
		d.pos = i + 1
		d.st = statePI
		return
	case '!':
		d.pos = i + 1
		d.st = stateBang
		return
	case '/':
		if d.ph != phaseContent {
			d.fail(ErrUnexpectedEndTag, i)
			return
		}
		d.pos = i + 1
		d.st = stateETag
		return
	}
	r, w := d.char(i)
	if w <= 0 {
		if w == 0 {
			d.eof()
		}
		return
	}
	if !isNameStart(r) {
		d.fail(ErrUnexpectedChar, i)
		return
	}
	if d.ph == phaseEpilog {
		d.fail(ErrMultipleRoots, i)
		return
	}
	if len(d.open) >= d.maxDepth {
		d.fail(ErrDepthLimit, i)
		return
	}
	d.st = stateSTag
}

// matchKeyword matches kw at d.pos byte by byte, recording code at the
// first mismatching byte. It reports whether the whole keyword matched.
func (d *Decoder) matchKeyword(kw string, code ErrorCode) bool {
	for k := 0; k < len(kw); k++ {
		j := d.pos + k
		if j >= d.n {
			d.eof()
			return false
		}
		if d.src[j] != kw[k] {
			d.failAt(code, j)
			return false
		}
	}
	d.pos += len(kw)
	return true
}

// bang implements stateBang (after "<!").
func (d *Decoder) bang() {
	i := d.pos
	if i >= d.n {
		d.eof()
		return
	}
	switch d.src[i] {
	case '-':
		if d.matchKeyword("--", ErrUnexpectedChar) {
			d.st = stateComment
		}
	case '[':
		if d.ph != phaseContent {
			d.fail(ErrTextOutsideRoot, i)
			return
		}
		if d.matchKeyword("[CDATA[", ErrUnexpectedChar) {
			d.st = stateCDSect
		}
	case 'D':
		if d.ph != phaseProlog || d.doctype {
			d.fail(ErrMisplacedDoctype, i)
			return
		}
		if d.matchKeyword("DOCTYPE", ErrUnexpectedChar) {
			d.st = stateDoctype
		}
	default:
		d.failAt(ErrUnexpectedChar, i)
	}
}

// afterMarkup returns to content or misc after a construct.
func (d *Decoder) afterMarkup() {
	d.atStart = false
	if d.ph == phaseContent {
		d.st = stateContent
	} else {
		d.st = stateMisc
	}
}

// appendNormalized appends the char at i (width w) to buf with line-end
// normalization (§2.11) and returns the offset after it.
func (d *Decoder) appendNormalized(i, w int) int {
	c := d.src[i]
	if c == '\r' {
		d.buf = append(d.buf, '\n')
		if i+1 < d.n && d.src[i+1] == '\n' {
			return i + 2
		}
		return i + 1
	}
	d.buf = append(d.buf, d.src[i:i+w]...)
	return i + w
}

// comment implements stateComment (after "<!--").
func (d *Decoder) comment() (Event, bool) {
	d.buf = d.buf[:0]
	i := d.pos
	for {
		d.steps++
		if i >= d.n {
			d.eof()
			return Event{}, false
		}
		if d.src[i] == '-' {
			if i+1 >= d.n {
				d.eof()
				return Event{}, false
			}
			if d.src[i+1] == '-' {
				if i+2 >= d.n {
					d.eof()
					return Event{}, false
				}
				if d.src[i+2] != '>' {
					d.fail(ErrDoubleHyphenComment, i+2)
					return Event{}, false
				}
				d.pos = i + 3
				ev := Event{Kind: EventComment, Start: int32(d.mstart), End: int32(d.pos), Text: string(d.buf)}
				d.afterMarkup()
				return ev, true
			}
		}
		_, w := d.char(i)
		if w <= 0 {
			if w == 0 {
				d.eof()
			}
			return Event{}, false
		}
		i = d.appendNormalized(i, w)
	}
}

// cdsect implements stateCDSect (after "<![CDATA[").
func (d *Decoder) cdsect() (Event, bool) {
	d.buf = d.buf[:0]
	i := d.pos
	for {
		d.steps++
		if i >= d.n {
			d.eof()
			return Event{}, false
		}
		if d.src[i] == ']' && i+2 < d.n && d.src[i+1] == ']' && d.src[i+2] == '>' {
			d.pos = i + 3
			ev := Event{Kind: EventCData, Start: int32(d.mstart), End: int32(d.pos), Text: string(d.buf)}
			d.afterMarkup()
			return ev, true
		}
		_, w := d.char(i)
		if w <= 0 {
			if w == 0 {
				d.eof()
			}
			return Event{}, false
		}
		i = d.appendNormalized(i, w)
	}
}

// pi implements statePI (after "<?").
func (d *Decoder) pi() (Event, bool) {
	start := d.pos
	end, _ := d.scanName(start, nameNoColon)
	if end < 0 {
		if d.err == nil {
			d.eof()
		}
		return Event{}, false
	}
	target := d.src[start:end]
	if len(target) == 3 && (target[0]|0x20) == 'x' && (target[1]|0x20) == 'm' && (target[2]|0x20) == 'l' {
		if string(target) == "xml" && d.atStart && d.mstart == d.docStart && isSpace(d.src[end]) {
			return d.xmlDecl(end)
		}
		d.fail(ErrReservedPITarget, end)
		return Event{}, false
	}
	i := end
	if isSpace(d.src[i]) {
		i = d.skipSpace(i)
	} else if d.src[i] != '?' {
		d.fail(ErrUnexpectedChar, i)
		return Event{}, false
	}
	d.buf = d.buf[:0]
	for {
		d.steps++
		if i >= d.n {
			d.eof()
			return Event{}, false
		}
		if d.src[i] == '?' {
			if i+1 >= d.n {
				d.eof()
				return Event{}, false
			}
			if d.src[i+1] == '>' {
				d.pos = i + 2
				ev := Event{Kind: EventPI, Start: int32(d.mstart), End: int32(d.pos),
					Name: Name{Local: string(target)}, Text: string(d.buf)}
				d.afterMarkup()
				return ev, true
			}
			if i == end {
				// "<?target?" must be followed by ">".
				d.failAt(ErrUnexpectedChar, i+1)
				return Event{}, false
			}
		}
		_, w := d.char(i)
		if w <= 0 {
			if w == 0 {
				d.eof()
			}
			return Event{}, false
		}
		i = d.appendNormalized(i, w)
	}
}

// xmlDecl parses [23] XMLDecl from i (just after "<?xml", at whitespace).
// Pseudo-attributes are matched incrementally so errors land on the first
// offending byte: version (required), encoding, standalone, in order.
func (d *Decoder) xmlDecl(i int) (Event, bool) {
	ev := Event{Kind: EventXMLDecl, Start: int32(d.mstart), Standalone: -1}
	allowed := []string{"version"}
	for {
		hadSpace := i < d.n && isSpace(d.src[i])
		i = d.skipSpace(i)
		if i >= d.n {
			d.eof()
			return Event{}, false
		}
		if d.src[i] == '?' {
			if ev.Version == "" {
				d.fail(ErrInvalidXMLDecl, i)
				return Event{}, false
			}
			if i+1 >= d.n {
				d.eof()
				return Event{}, false
			}
			if d.src[i+1] != '>' {
				d.fail(ErrInvalidXMLDecl, i+1)
				return Event{}, false
			}
			d.pos = i + 2
			ev.End = int32(d.pos)
			d.afterMarkup()
			return ev, true
		}
		if !hadSpace || len(allowed) == 0 {
			d.fail(ErrInvalidXMLDecl, i)
			return Event{}, false
		}
		// Match the pseudo-attribute name against the allowed ones.
		cands := allowed
		k := 0
		for {
			if i+k >= d.n {
				d.eof()
				return Event{}, false
			}
			c := d.src[i+k]
			if c == '=' || isSpace(c) {
				break
			}
			var next []string
			for _, a := range cands {
				if k < len(a) && a[k] == c {
					next = append(next, a)
				}
			}
			if len(next) == 0 {
				d.fail(ErrInvalidXMLDecl, i+k)
				return Event{}, false
			}
			cands = next
			k++
		}
		name := ""
		for _, a := range cands {
			if len(a) == k {
				name = a
			}
		}
		if name == "" {
			d.fail(ErrInvalidXMLDecl, i+k)
			return Event{}, false
		}
		i = d.skipSpace(i + k)
		if i >= d.n {
			d.eof()
			return Event{}, false
		}
		if d.src[i] != '=' {
			d.fail(ErrInvalidXMLDecl, i)
			return Event{}, false
		}
		i = d.skipSpace(i + 1)
		if i >= d.n {
			d.eof()
			return Event{}, false
		}
		q := d.src[i]
		if q != '"' && q != '\'' {
			d.fail(ErrInvalidXMLDecl, i)
			return Event{}, false
		}
		vs := i + 1
		j := vs
		for {
			if j >= d.n {
				d.eof()
				return Event{}, false
			}
			c := d.src[j]
			if c == q {
				break
			}
			if !d.declValueByte(name, d.src[vs:j], c, j) {
				return Event{}, false
			}
			j++
		}
		v := string(d.src[vs:j])
		if !d.declValueDone(name, v, j) {
			return Event{}, false
		}
		switch name {
		case "version":
			ev.Version = v
			allowed = []string{"encoding", "standalone"}
		case "encoding":
			ev.Encoding = v
			allowed = []string{"standalone"}
		case "standalone":
			ev.Standalone = 0
			if v == "yes" {
				ev.Standalone = 1
			}
			allowed = nil
		}
		i = j + 1
	}
}

func (d *Decoder) encodings() []string {
	if d.utf16 {
		return []string{"utf-16", "utf-16le", "utf-16be"}
	}
	return []string{"utf-8"}
}

func lowerByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// declValueByte checks byte c at offset j, which extends the value prefix
// read so far, so an impossible value is reported at its first wrong byte.
func (d *Decoder) declValueByte(name string, prefix []byte, c byte, j int) bool {
	k := len(prefix)
	switch name {
	case "version": // [26] VersionNum ::= '1.' [0-9]+
		ok := (k == 0 && c == '1') || (k == 1 && c == '.') || (k >= 2 && c >= '0' && c <= '9')
		if !ok {
			d.fail(ErrInvalidXMLDecl, j)
		}
		return ok
	case "encoding": // [81] EncName, restricted by policy to UTF-8 / UTF-16
		for _, e := range d.encodings() {
			if k < len(e) && e[k] == lowerByte(c) && asciiPrefixFold(prefix, e) {
				return true
			}
		}
		if k == 0 && !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) {
			d.fail(ErrInvalidXMLDecl, j)
			return false
		}
		if k > 0 && !(c == '.' || c == '_' || c == '-' || (c >= '0' && c <= '9') ||
			(c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) {
			d.fail(ErrInvalidXMLDecl, j)
			return false
		}
		d.fail(ErrUnsupportedEncoding, j)
		return false
	default: // [32] SDDecl: 'yes' | 'no'
		for _, e := range []string{"yes", "no"} {
			if k < len(e) && e[k] == c && string(prefix) == e[:k] {
				return true
			}
		}
		d.fail(ErrInvalidXMLDecl, j)
		return false
	}
}

func asciiPrefixFold(prefix []byte, s string) bool {
	if len(prefix) > len(s) {
		return false
	}
	for i, c := range prefix {
		if lowerByte(c) != s[i] {
			return false
		}
	}
	return true
}

// declValueDone checks a complete pseudo-attribute value v at its closing
// quote (offset j).
func (d *Decoder) declValueDone(name, v string, j int) bool {
	ok := true
	code := ErrInvalidXMLDecl
	switch name {
	case "version":
		ok = len(v) >= 3
	case "encoding":
		ok = false
		for _, e := range d.encodings() {
			if len(v) == len(e) && asciiPrefixFold([]byte(v), e) {
				ok = true
			}
		}
		if !ok && v != "" {
			code = ErrUnsupportedEncoding
		}
	default:
		ok = v == "yes" || v == "no"
	}
	if !ok {
		d.fail(code, j)
	}
	return ok
}

// doctypedecl implements stateDoctype (after "<!DOCTYPE"). Only
// "<!DOCTYPE" S Name S? ">" is accepted: an internal subset or an
// external identifier is refused at its first byte.
func (d *Decoder) doctypedecl() (Event, bool) {
	i := d.pos
	if i >= d.n {
		d.eof()
		return Event{}, false
	}
	if !isSpace(d.src[i]) {
		d.failAt(ErrUnexpectedChar, i)
		return Event{}, false
	}
	i = d.skipSpace(i)
	if i >= d.n {
		d.eof()
		return Event{}, false
	}
	end, colon := d.scanName(i, nameQName)
	if end < 0 {
		if d.err == nil {
			d.eof()
		}
		return Event{}, false
	}
	name := splitName(d.src[i:end], colon-i)
	j := d.skipSpace(end)
	if j >= d.n {
		d.eof()
		return Event{}, false
	}
	switch c := d.src[j]; {
	case c == '>':
		d.pos = j + 1
		d.doctype = true
		ev := Event{Kind: EventDoctype, Start: int32(d.mstart), End: int32(d.pos), Name: name}
		d.afterMarkup()
		return ev, true
	case c == '[':
		d.fail(ErrDTDInternalSubset, j)
	case j == end:
		d.fail(ErrUnexpectedChar, j)
	default:
		d.failAt(ErrDTDExternalID, j)
	}
	return Event{}, false
}

func splitName(q []byte, colon int) Name {
	if colon < 0 {
		return Name{Local: string(q)}
	}
	return Name{Prefix: string(q[:colon]), Local: string(q[colon+1:])}
}

// stag implements stateSTag at the element name.
func (d *Decoder) stag() (Event, bool) {
	nameStart := d.pos
	nameEnd, colon := d.scanName(nameStart, nameQName)
	if nameEnd < 0 {
		if d.err == nil {
			d.eof()
		}
		return Event{}, false
	}
	d.raws = d.raws[:0]
	i := nameEnd
	for {
		d.steps++
		hadSpace := i < d.n && isSpace(d.src[i])
		i = d.skipSpace(i)
		if i >= d.n {
			d.eof()
			return Event{}, false
		}
		c := d.src[i]
		if c == '>' || c == '/' {
			if !d.resolve(nameStart, nameEnd, colon, i) {
				return Event{}, false
			}
			if c == '/' {
				if i+1 >= d.n {
					d.eof()
					return Event{}, false
				}
				if d.src[i+1] != '>' {
					d.failAt(ErrUnexpectedChar, i+1)
					return Event{}, false
				}
				i++
			}
			d.pos = i + 1
			top := &d.open[len(d.open)-1]
			ev := Event{Kind: EventStartElement, Start: int32(d.mstart), End: int32(d.pos), Name: top.name, Attrs: d.attrs}
			d.atStart = false
			d.ph = phaseContent
			d.st = stateContent
			if c == '/' {
				ev.SelfClosing = true
				end := Event{Kind: EventEndElement, Start: int32(d.pos), End: int32(d.pos), Name: top.name, SelfClosing: true}
				d.pending = &end
				d.closeElement()
			}
			return ev, true
		}
		r, w := d.char(i)
		if w <= 0 {
			if w == 0 {
				d.eof()
			}
			return Event{}, false
		}
		if !hadSpace || !isNameStart(r) {
			d.fail(ErrUnexpectedChar, i)
			return Event{}, false
		}
		if !d.attribute(i) {
			return Event{}, false
		}
		i = d.raws[len(d.raws)-1].end
	}
}

// attribute parses [41] Attribute at i, appending to d.raws.
func (d *Decoder) attribute(i int) bool {
	end, colon := d.scanName(i, nameQName)
	if end < 0 {
		if d.err == nil {
			d.eof()
		}
		return false
	}
	q := d.src[i:end]
	for _, a := range d.raws {
		if bytes.Equal(d.src[a.qStart:a.qEnd], q) {
			d.fail(ErrDuplicateAttribute, end)
			return false
		}
	}
	j := d.skipSpace(end)
	if j >= d.n {
		d.eof()
		return false
	}
	if d.src[j] != '=' {
		d.failAt(ErrUnexpectedChar, j)
		return false
	}
	j = d.skipSpace(j + 1)
	if j >= d.n {
		d.eof()
		return false
	}
	quote := d.src[j]
	if quote != '"' && quote != '\'' {
		d.failAt(ErrUnexpectedChar, j)
		return false
	}
	j++
	d.buf = d.buf[:0]
	for {
		d.steps++
		if j >= d.n {
			d.eof()
			return false
		}
		c := d.src[j]
		switch {
		case c == quote:
			a := rawAttr{qStart: i, qEnd: end, colon: colon, value: string(d.buf), start: i, end: j + 1}
			if !d.checkNSDecl(&a, j) {
				return false
			}
			d.raws = append(d.raws, a)
			return true
		case c == '<':
			d.fail(ErrLtInAttribute, j)
			return false
		case c == '&':
			n, ok := d.reference(j)
			if !ok {
				return false
			}
			j = n
		case c == '\r':
			d.buf = append(d.buf, ' ')
			j++
			if j < d.n && d.src[j] == '\n' {
				j++
			}
		case c == '\n' || c == '\t':
			d.buf = append(d.buf, ' ')
			j++
		default:
			_, w := d.char(j)
			if w <= 0 {
				if w == 0 {
					d.eof()
				}
				return false
			}
			d.buf = append(d.buf, d.src[j:j+w]...)
			j += w
		}
	}
}

// checkNSDecl validates a namespace declaration attribute at its closing
// quote (offset at), per Namespaces in XML 1.0 §3.
func (d *Decoder) checkNSDecl(a *rawAttr, at int) bool {
	q := d.src[a.qStart:a.qEnd]
	var prefix string
	switch {
	case string(q) == "xmlns":
		prefix = ""
	case a.colon >= 0 && string(d.src[a.qStart:a.colon]) == "xmlns":
		prefix = string(d.src[a.colon+1 : a.qEnd])
	default:
		return true
	}
	v := a.value
	ok := true
	switch {
	case prefix == "xmlns":
		ok = false
	case prefix == "xml":
		ok = v == XMLNamespace
	case v == XMLNamespace || v == XMLNSNamespace:
		ok = false
	case prefix != "" && v == "":
		ok = false
	}
	if !ok {
		d.fail(ErrInvalidNamespaceDecl, at)
	}
	return ok
}

// lookup returns the namespace bound to prefix.
func (d *Decoder) lookup(prefix string) (string, bool) {
	switch prefix {
	case "xml":
		return XMLNamespace, true
	case "xmlns":
		return XMLNSNamespace, true
	}
	for k := len(d.ns) - 1; k >= 0; k-- {
		if d.ns[k].prefix == prefix {
			return d.ns[k].uri, true
		}
	}
	return "", prefix == ""
}

// resolve applies the namespace declarations of the tag being closed at
// offset at, resolves the element and attribute names, checks expanded
// attribute names for duplicates and pushes the element.
func (d *Decoder) resolve(nameStart, nameEnd, colon, at int) bool {
	mark := len(d.ns)
	for i := range d.raws {
		a := &d.raws[i]
		q := d.src[a.qStart:a.qEnd]
		switch {
		case string(q) == "xmlns":
			d.ns = append(d.ns, binding{"", a.value})
		case a.colon >= 0 && string(d.src[a.qStart:a.colon]) == "xmlns":
			d.ns = append(d.ns, binding{string(d.src[a.colon+1 : a.qEnd]), a.value})
		}
	}
	name := splitName(d.src[nameStart:nameEnd], colon-nameStart)
	if name.Prefix == "xmlns" {
		d.fail(ErrReservedPrefix, at)
		return false
	}
	uri, ok := d.lookup(name.Prefix)
	if !ok {
		d.fail(ErrUnboundPrefix, at)
		return false
	}
	name.Space = uri
	d.attrs = d.attrs[:0]
	for i := range d.raws {
		a := &d.raws[i]
		an := splitName(d.src[a.qStart:a.qEnd], a.colon-a.qStart)
		switch {
		case an.Prefix == "" && an.Local == "xmlns":
			an.Space = XMLNSNamespace
		case an.Prefix != "":
			u, ok := d.lookup(an.Prefix)
			if !ok {
				d.fail(ErrUnboundPrefix, at)
				return false
			}
			an.Space = u
			for j := range d.attrs {
				if d.attrs[j].Name.Prefix != "" && d.attrs[j].Name.Space == u && d.attrs[j].Name.Local == an.Local {
					d.fail(ErrDuplicateAttribute, at)
					return false
				}
			}
		}
		d.attrs = append(d.attrs, Attr{Name: an, Value: a.value, Start: int32(a.start), End: int32(a.end)})
	}
	d.open = append(d.open, openElem{raw: d.src[nameStart:nameEnd], name: name, nsMark: mark})
	return true
}

func (d *Decoder) closeElement() {
	top := d.open[len(d.open)-1]
	d.ns = d.ns[:top.nsMark]
	d.open = d.open[:len(d.open)-1]
	if len(d.open) == 0 {
		d.ph = phaseEpilog
		d.st = stateMisc
	}
}

// etag implements stateETag (after "</"): the name is matched byte by
// byte against the open element so a mismatch is reported at its first
// differing byte.
func (d *Decoder) etag() (Event, bool) {
	top := d.open[len(d.open)-1]
	want := top.raw
	i := d.pos
	for k := 0; ; k++ {
		d.steps++
		j := i + k
		if j >= d.n {
			d.eof()
			return Event{}, false
		}
		c := d.src[j]
		if k < len(want) {
			if c != want[k] {
				d.failAt(ErrMismatchedEndTag, j)
				return Event{}, false
			}
			continue
		}
		if c == '>' || isSpace(c) {
			j = d.skipSpace(j)
			if j >= d.n {
				d.eof()
				return Event{}, false
			}
			if d.src[j] != '>' {
				d.failAt(ErrUnexpectedChar, j)
				return Event{}, false
			}
			d.pos = j + 1
			ev := Event{Kind: EventEndElement, Start: int32(d.mstart), End: int32(d.pos), Name: top.name}
			d.closeElement()
			if d.ph == phaseContent {
				d.st = stateContent
			}
			return ev, true
		}
		d.failAt(ErrMismatchedEndTag, j)
		return Event{}, false
	}
}

// content implements stateContent: CharData and references up to the
// next markup, emitted as one Text event.
func (d *Decoder) content() (Event, bool) {
	start := d.pos
	d.buf = d.buf[:0]
	i := start
	for {
		d.steps++
		if i >= d.n {
			if i > start {
				// Emit the text read so far; the next call reports EOF.
				d.pos = i
				return d.textEvent(start, i), true
			}
			d.eof()
			return Event{}, false
		}
		c := d.src[i]
		switch c {
		case '<':
			d.pos = i
			if i > start {
				return d.textEvent(start, i), true
			}
			d.mstart = i
			d.pos = i + 1
			d.st = stateMarkup
			return Event{}, false
		case '&':
			n, ok := d.reference(i)
			if !ok {
				return Event{}, false
			}
			i = n
			continue
		case '>':
			if i-2 >= start && d.src[i-1] == ']' && d.src[i-2] == ']' {
				d.fail(ErrCDataEndInContent, i)
				return Event{}, false
			}
		}
		_, w := d.char(i)
		if w <= 0 {
			if w == 0 {
				d.eof()
			}
			return Event{}, false
		}
		i = d.appendNormalized(i, w)
	}
}

func (d *Decoder) textEvent(start, end int) Event {
	return Event{Kind: EventText, Start: int32(start), End: int32(end), Text: string(d.buf)}
}

// reference parses [67] Reference at i ("&") and appends its replacement
// to d.buf. Only character references and the five predefined entities
// exist: no entity is ever expanded beyond one character, so output never
// grows past input (XML-INV-05).
func (d *Decoder) reference(i int) (int, bool) {
	j := i + 1
	if j >= d.n {
		d.eof()
		return 0, false
	}
	if d.src[j] == '#' {
		j++
		hex := false
		if j < d.n && d.src[j] == 'x' {
			hex = true
			j++
		}
		v := 0
		digits := 0
		for {
			d.steps++
			if j >= d.n {
				d.eof()
				return 0, false
			}
			c := d.src[j]
			dv := -1
			switch {
			case c >= '0' && c <= '9':
				dv = int(c - '0')
			case hex && c >= 'a' && c <= 'f':
				dv = int(c-'a') + 10
			case hex && c >= 'A' && c <= 'F':
				dv = int(c-'A') + 10
			}
			if dv >= 0 {
				if hex {
					v = v*16 + dv
				} else {
					v = v*10 + dv
				}
				if v > utf8.MaxRune {
					d.fail(ErrInvalidCharRef, j)
					return 0, false
				}
				digits++
				j++
				continue
			}
			if c != ';' || digits == 0 {
				d.failAt(ErrInvalidReference, j)
				return 0, false
			}
			if !isXMLChar(rune(v)) {
				d.fail(ErrInvalidCharRef, j)
				return 0, false
			}
			d.buf = utf8.AppendRune(d.buf, rune(v))
			return j + 1, true
		}
	}
	if r, w := d.char(j); w > 0 && !isNameStart(r) {
		d.fail(ErrInvalidReference, j)
		return 0, false
	}
	end, _ := d.scanName(j, nameAny)
	if end < 0 {
		if d.err == nil {
			d.eof()
		}
		return 0, false
	}
	if d.src[end] != ';' {
		d.fail(ErrInvalidReference, end)
		return 0, false
	}
	var r byte
	switch string(d.src[j:end]) {
	case "lt":
		r = '<'
	case "gt":
		r = '>'
	case "amp":
		r = '&'
	case "apos":
		r = '\''
	case "quot":
		r = '"'
	default:
		d.fail(ErrUndefinedEntity, end)
		return 0, false
	}
	d.buf = append(d.buf, r)
	return end + 1, true
}
