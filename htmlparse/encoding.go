package htmlparse

import (
	"bytes"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/unicode"
)

// Input decoding: HTML §13.2.3 "The input byte stream" — BOM sniffing,
// the transport-layer charset, the prescan of the first 1024 bytes for a
// <meta> charset declaration (§13.2.3.2), the XML-declaration fallback,
// confidence, and decoding to UTF-8 through golang.org/x/text (WHATWG
// Encoding labels; the Rust port uses encoding_rs, which has the same
// label table and canonical names).

// Encoding is a WHATWG Encoding standard encoding, identified by its
// canonical name ("UTF-8", "windows-1252", "Shift_JIS", ...).
type Encoding string

// Frequently used encodings.
const (
	EncodingUTF8         Encoding = "UTF-8"
	EncodingUTF16LE      Encoding = "UTF-16LE"
	EncodingUTF16BE      Encoding = "UTF-16BE"
	EncodingWindows1252  Encoding = "windows-1252"
	EncodingReplacement  Encoding = "replacement"
	EncodingXUserDefined Encoding = "x-user-defined"
)

// Confidence is the encoding confidence (§13.2.3.1).
type Confidence uint8

// Confidence values.
const (
	ConfidenceTentative Confidence = iota
	ConfidenceCertain
	ConfidenceIrrelevant
)

func (c Confidence) String() string {
	switch c {
	case ConfidenceCertain:
		return "certain"
	case ConfidenceIrrelevant:
		return "irrelevant"
	}
	return "tentative"
}

// PrescanLimit is how many bytes the meta prescan examines (the standard
// encourages 1024).
const PrescanLimit = 1024

// canonicalNames maps x/text's lower-case canonical names to the WHATWG
// canonical names.
var canonicalNames = map[string]Encoding{
	"utf-8": "UTF-8", "ibm866": "IBM866", "iso-8859-2": "ISO-8859-2", "iso-8859-3": "ISO-8859-3",
	"iso-8859-4": "ISO-8859-4", "iso-8859-5": "ISO-8859-5", "iso-8859-6": "ISO-8859-6",
	"iso-8859-7": "ISO-8859-7", "iso-8859-8": "ISO-8859-8", "iso-8859-8-i": "ISO-8859-8-I",
	"iso-8859-10": "ISO-8859-10", "iso-8859-13": "ISO-8859-13", "iso-8859-14": "ISO-8859-14",
	"iso-8859-15": "ISO-8859-15", "iso-8859-16": "ISO-8859-16", "koi8-r": "KOI8-R", "koi8-u": "KOI8-U",
	"macintosh": "macintosh", "windows-874": "windows-874", "windows-1250": "windows-1250",
	"windows-1251": "windows-1251", "windows-1252": "windows-1252", "windows-1253": "windows-1253",
	"windows-1254": "windows-1254", "windows-1255": "windows-1255", "windows-1256": "windows-1256",
	"windows-1257": "windows-1257", "windows-1258": "windows-1258", "x-mac-cyrillic": "x-mac-cyrillic",
	"gbk": "GBK", "gb18030": "gb18030", "big5": "Big5", "euc-jp": "EUC-JP", "iso-2022-jp": "ISO-2022-JP",
	"shift_jis": "Shift_JIS", "euc-kr": "EUC-KR", "replacement": "replacement",
	"utf-16be": "UTF-16BE", "utf-16le": "UTF-16LE", "x-user-defined": "x-user-defined",
}

// LookupEncoding is the Encoding standard's "get an encoding": label is
// trimmed of ASCII whitespace and matched case-insensitively against the
// WHATWG label table.
func LookupEncoding(label string) (Encoding, bool) {
	label = trimASCIISpace(label)
	if label == "" {
		return "", false
	}
	enc, err := htmlindex.Get(label)
	if err != nil {
		return "", false
	}
	name, err := htmlindex.Name(enc)
	if err != nil {
		return "", false
	}
	e, ok := canonicalNames[name]
	return e, ok
}

func trimASCIISpace(s string) string {
	for len(s) > 0 && isHTMLSpaceB(s[0]) {
		s = s[1:]
	}
	for len(s) > 0 && isHTMLSpaceB(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

// SniffBOM is "BOM sniff": it returns the encoding a byte order mark
// selects and the BOM's length, or ("", 0).
func SniffBOM(raw []byte) (Encoding, int) {
	switch {
	case len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF:
		return EncodingUTF8, 3
	case len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF:
		return EncodingUTF16BE, 2
	case len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE:
		return EncodingUTF16LE, 2
	}
	return "", 0
}

// transportEncoding resolves the transport-layer charset. It accepts a
// bare label ("utf-8") or a Content-Type value ("text/html;
// charset=utf-8"), whose charset parameter is extracted with the meta
// algorithm.
func transportEncoding(charset string) (Encoding, bool) {
	if e, ok := LookupEncoding(charset); ok {
		return e, true
	}
	return extractCharsetFromContent(charset)
}

// Sniff is the encoding sniffing algorithm (§13.2.3.2) over raw. The
// optional steps are chosen as: no user override, the prescan runs on the
// first PrescanLimit bytes, no parent document or history, and the
// autodetection step accepts UTF-8 when all of raw is valid UTF-8. The
// final default is windows-1252 (the standard's suggested default for
// locales not in its table).
func Sniff(raw []byte, transportCharset string) (Encoding, Confidence) {
	if e, _ := SniffBOM(raw); e != "" {
		return e, ConfidenceCertain
	}
	if transportCharset != "" {
		if e, ok := transportEncoding(transportCharset); ok {
			return e, ConfidenceCertain
		}
	}
	if e, ok := Prescan(raw); ok {
		return e, ConfidenceTentative
	}
	if utf8.Valid(raw) {
		return EncodingUTF8, ConfidenceTentative
	}
	return EncodingWindows1252, ConfidenceTentative
}

// Decode determines the encoding of raw (Sniff) and decodes it to UTF-8
// with the leading BOM removed.
func Decode(raw []byte, transportCharset string) ([]byte, Encoding, Confidence) {
	enc, conf := Sniff(raw, transportCharset)
	return DecodeAs(raw, enc), enc, conf
}

// DecodeAs is the Encoding standard's "decode": a BOM overrides enc and
// is removed; the rest is decoded to UTF-8 with errors replaced by
// U+FFFD. Unknown encodings decode as UTF-8.
func DecodeAs(raw []byte, enc Encoding) []byte {
	if e, n := SniffBOM(raw); e != "" {
		enc, raw = e, raw[n:]
	}
	switch enc {
	case EncodingUTF8, "":
		return decodeUTF8(raw)
	case EncodingUTF16LE:
		return decodeWith(unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), raw)
	case EncodingUTF16BE:
		return decodeWith(unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM), raw)
	}
	e, err := htmlindex.Get(string(enc))
	if err != nil {
		return decodeUTF8(raw)
	}
	return decodeWith(e, raw)
}

func decodeWith(e encoding.Encoding, raw []byte) []byte {
	out, err := e.NewDecoder().Bytes(raw)
	if err != nil {
		// x/text decoders replace malformed input; an error here means the
		// encoding is unusable, so fail safe to UTF-8 decoding.
		return decodeUTF8(raw)
	}
	return out
}

// decodeUTF8 is the WHATWG UTF-8 decoder: each maximal subpart of an
// ill-formed sequence becomes one U+FFFD. Valid input is returned as is
// (the result aliases raw).
//
// PORT: encoding_rs's UTF-8 decoder applies the same maximal-subpart
// rule; the other encodings come from x/text here and encoding_rs there,
// both built from the WHATWG index tables.
func decodeUTF8(raw []byte) []byte {
	if utf8.Valid(raw) {
		return raw
	}
	out := make([]byte, 0, len(raw)+16)
	for i := 0; i < len(raw); {
		c := raw[i]
		if c < utf8.RuneSelf {
			out = append(out, c)
			i++
			continue
		}
		r, w := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && w == 1 {
			out = append(out, "�"...)
			i += maximalSubpart(raw[i:])
			continue
		}
		out = append(out, raw[i:i+w]...)
		i += w
	}
	return out
}

// Prescan is "prescan a byte stream to determine its encoding" over the
// first PrescanLimit bytes of raw, falling back to "get an XML encoding"
// when the prescan runs out of bytes.
func Prescan(raw []byte) (Encoding, bool) {
	b := raw
	if len(b) > PrescanLimit {
		b = b[:PrescanLimit]
	}
	if e, ok, done := prescan(b); done {
		return e, ok
	}
	return xmlEncoding(b)
}

// prescan returns done=false when it ran out of bytes (the caller then
// tries the XML declaration).
func prescan(b []byte) (Encoding, bool, bool) {
	if len(b) >= 6 {
		if bytes.HasPrefix(b, []byte{0x3C, 0x00, 0x3F, 0x00, 0x78, 0x00}) {
			return EncodingUTF16LE, true, true
		}
		if bytes.HasPrefix(b, []byte{0x00, 0x3C, 0x00, 0x3F, 0x00, 0x78}) {
			return EncodingUTF16BE, true, true
		}
	}
	pos := 0
	for pos < len(b) {
		switch {
		case bytes.HasPrefix(b[pos:], []byte("<!--")):
			// Advance to the '>' of the first "-->" after the '<'.
			i := bytes.Index(b[pos+2:], []byte("-->"))
			if i < 0 {
				return "", false, false
			}
			pos = pos + 2 + i + 2
		case len(b)-pos >= 6 && b[pos] == '<' && (b[pos+1]|0x20) == 'm' && (b[pos+2]|0x20) == 'e' &&
			(b[pos+3]|0x20) == 't' && (b[pos+4]|0x20) == 'a' && (isHTMLSpaceB(b[pos+5]) || b[pos+5] == '/'):
			pos += 5
			e, ok, next, done := prescanMeta(b, pos)
			if !done {
				return "", false, false
			}
			if ok {
				return e, true, true
			}
			pos = next
		case len(b)-pos >= 3 && b[pos] == '<' && (isASCIIAlnumB(b[pos+1]) && isASCIIAlpha(rune(b[pos+1])) ||
			(b[pos+1] == '/' && isASCIIAlpha(rune(b[pos+2])))):
			// A start or end tag: skip its name, then its attributes.
			for pos < len(b) && !isHTMLSpaceB(b[pos]) && b[pos] != '>' {
				pos++
			}
			for {
				_, _, np, found, ok := getAttribute(b, pos)
				if !ok {
					return "", false, false
				}
				pos = np
				if !found {
					break
				}
			}
		case bytes.HasPrefix(b[pos:], []byte("<!")), bytes.HasPrefix(b[pos:], []byte("</")), bytes.HasPrefix(b[pos:], []byte("<?")):
			i := bytes.IndexByte(b[pos+1:], '>')
			if i < 0 {
				return "", false, false
			}
			pos = pos + 1 + i
		}
		pos++ // next byte
	}
	return "", false, false
}

// prescanMeta runs the <meta> branch of the prescan with pos at the
// space or slash after "<meta". It returns the encoding (ok), the
// position to continue from, and done=false if it ran out of bytes.
func prescanMeta(b []byte, pos int) (Encoding, bool, int, bool) {
	var attrList []string
	gotPragma := false
	needPragma := 0 // 0: null, 1: true, 2: false
	var charset Encoding
	charsetSet, charsetFail := false, false
	for {
		name, value, np, found, ok := getAttribute(b, pos)
		if !ok {
			return "", false, 0, false
		}
		pos = np
		if !found {
			break
		}
		dup := false
		for _, a := range attrList {
			if a == name {
				dup = true
			}
		}
		if dup {
			continue
		}
		attrList = append(attrList, name)
		switch name {
		case "http-equiv":
			if value == "content-type" {
				gotPragma = true
			}
		case "content":
			if e, ok := extractCharsetFromContent(value); ok && !charsetSet {
				charset, charsetSet, charsetFail = e, true, false
				needPragma = 1
			}
		case "charset":
			e, ok := LookupEncoding(value)
			charset, charsetSet, charsetFail = e, true, !ok
			needPragma = 2
		}
	}
	if needPragma == 0 || (needPragma == 1 && !gotPragma) || !charsetSet || charsetFail {
		return "", false, pos, true
	}
	switch charset {
	case EncodingUTF16LE, EncodingUTF16BE:
		charset = EncodingUTF8
	case EncodingXUserDefined:
		charset = EncodingWindows1252
	}
	return charset, true, pos, true
}

// getAttribute is the prescan's "get an attribute". found=false means
// there was no attribute (at '>'); ok=false means the bytes ran out.
func getAttribute(b []byte, pos int) (name, value string, next int, found, ok bool) {
	for pos < len(b) && (isHTMLSpaceB(b[pos]) || b[pos] == '/') {
		pos++
	}
	if pos >= len(b) {
		return "", "", pos, false, false
	}
	if b[pos] == '>' {
		return "", "", pos, false, true
	}
	var nb, vb []byte
	lower := func(c byte) byte {
		if c >= 'A' && c <= 'Z' {
			return c + 0x20
		}
		return c
	}
	for {
		if pos >= len(b) {
			return "", "", pos, false, false
		}
		c := b[pos]
		if c == '=' && len(nb) > 0 {
			pos++
			goto value
		}
		if isHTMLSpaceB(c) {
			break
		}
		if c == '/' || c == '>' {
			return string(nb), "", pos, true, true
		}
		nb = append(nb, lower(c))
		pos++
	}
	// Spaces.
	for pos < len(b) && isHTMLSpaceB(b[pos]) {
		pos++
	}
	if pos >= len(b) {
		return "", "", pos, false, false
	}
	if b[pos] != '=' {
		return string(nb), "", pos, true, true
	}
	pos++
value:
	for pos < len(b) && isHTMLSpaceB(b[pos]) {
		pos++
	}
	if pos >= len(b) {
		return "", "", pos, false, false
	}
	if q := b[pos]; q == '"' || q == '\'' {
		for {
			pos++
			if pos >= len(b) {
				return "", "", pos, false, false
			}
			if b[pos] == q {
				return string(nb), string(vb), pos + 1, true, true
			}
			vb = append(vb, lower(b[pos]))
		}
	}
	if b[pos] == '>' {
		return string(nb), "", pos, true, true
	}
	vb = append(vb, lower(b[pos]))
	pos++
	for {
		if pos >= len(b) {
			return "", "", pos, false, false
		}
		c := b[pos]
		if isHTMLSpaceB(c) || c == '>' {
			return string(nb), string(vb), pos, true, true
		}
		vb = append(vb, lower(c))
		pos++
	}
}

// extractCharsetFromContent is the "algorithm for extracting a character
// encoding from a meta element" applied to s.
func extractCharsetFromContent(s string) (Encoding, bool) {
	pos := 0
	for {
		i := indexFold(s[pos:], "charset")
		if i < 0 {
			return "", false
		}
		pos += i + len("charset")
		for pos < len(s) && isHTMLSpaceB(s[pos]) {
			pos++
		}
		if pos >= len(s) || s[pos] != '=' {
			continue
		}
		pos++
		for pos < len(s) && isHTMLSpaceB(s[pos]) {
			pos++
		}
		if pos >= len(s) {
			return "", false
		}
		if q := s[pos]; q == '"' || q == '\'' {
			end := indexByteFrom(s, q, pos+1)
			if end < 0 {
				return "", false
			}
			return LookupEncoding(s[pos+1 : end])
		}
		end := pos
		for end < len(s) && !isHTMLSpaceB(s[end]) && s[end] != ';' {
			end++
		}
		return LookupEncoding(s[pos:end])
	}
}

func indexByteFrom(s string, c byte, from int) int {
	for i := from; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// indexFold is an ASCII case-insensitive strings.Index for a lower-case
// needle.
func indexFold(s, needle string) int {
	for i := 0; i+len(needle) <= len(s); i++ {
		if hasPrefixFoldASCII(s[i:], needle) {
			return i
		}
	}
	return -1
}

// xmlEncoding is "get an XML encoding".
func xmlEncoding(b []byte) (Encoding, bool) {
	if !bytes.HasPrefix(b, []byte("<?xml")) {
		return "", false
	}
	end := bytes.IndexByte(b, '>')
	if end < 0 {
		return "", false
	}
	i := bytes.Index(b[:end], []byte("encoding"))
	if i < 0 {
		return "", false
	}
	p := i + len("encoding")
	for p < len(b) && b[p] <= 0x20 {
		p++
	}
	if p >= len(b) || b[p] != '=' {
		return "", false
	}
	p++
	for p < len(b) && b[p] <= 0x20 {
		p++
	}
	if p >= len(b) || (b[p] != '"' && b[p] != '\'') {
		return "", false
	}
	q := b[p]
	p++
	e := bytes.IndexByte(b[p:], q)
	if e < 0 {
		return "", false
	}
	label := b[p : p+e]
	for _, c := range label {
		if c <= 0x20 {
			return "", false
		}
	}
	enc, ok := LookupEncoding(string(label))
	if enc == EncodingUTF16LE || enc == EncodingUTF16BE {
		enc = EncodingUTF8
	}
	return enc, ok
}
