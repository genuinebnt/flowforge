package codec

import (
	"slices"
	"unicode/utf8"
)

// PercentSet is a percent-encode set: the bytes an encoder escapes and
// strict decoding requires to be escaped. The WHATWG sets are those of
// the URL Standard §1.3 (living standard, 2024 text with U+005E in the
// path set). Every set here also contains U+0025 (%), so that encoding
// round-trips; the URL parser leaves existing escapes alone and does not
// need it (OPEN_QUESTIONS.md).
type PercentSet uint8

const (
	// SetComponent is the WHATWG component percent-encode set: every byte
	// but ALPHA, DIGIT and - . _ ~ ! * ' ( ) is escaped (what JavaScript's
	// encodeURIComponent leaves alone). The zero value.
	SetComponent PercentSet = iota
	// SetUnreserved escapes everything but the RFC 3986 §2.3 unreserved
	// characters ALPHA, DIGIT, - . _ ~ (RFC 3986 §6.2.2.2 normal form).
	SetUnreserved
	// SetC0Control: C0 controls and bytes above U+007E.
	SetC0Control
	// SetFragment: C0 control set plus space " < > `.
	SetFragment
	// SetQuery: C0 control set plus space " # < >.
	SetQuery
	// SetSpecialQuery: query set plus '.
	SetSpecialQuery
	// SetPath: query set plus ? ^ ` { }.
	SetPath
	// SetUserinfo: path set plus / : ; = @ [ \ ] |.
	SetUserinfo
	// SetForm is the application/x-www-form-urlencoded percent-encode set
	// (component set plus ! ' ( ) ~). Percent functions escape space as
	// %20 with it; the form functions write '+'.
	SetForm
	numPercentSets
)

var percentSetNames = [numPercentSets]string{"component", "unreserved", "c0control", "fragment", "query",
	"specialquery", "path", "userinfo", "form"}

// String returns the set's configuration name ("component", "path", ...).
func (s PercentSet) String() string {
	if s < numPercentSets {
		return percentSetNames[s]
	}
	return "unknown"
}

// ParsePercentSet maps a configuration name to its set.
func ParsePercentSet(name string) (PercentSet, error) {
	for i, n := range percentSetNames {
		if n == name {
			return PercentSet(i), nil
		}
	}
	return 0, ErrUnknownAlgorithm
}

// percentSets[s][b] reports that byte b is in set s.
var percentSets = func() (t [numPercentSets][256]bool) {
	add := func(s *[256]bool, chars string) {
		for i := 0; i < len(chars); i++ {
			s[chars[i]] = true
		}
	}
	var c0 [256]bool
	for b := 0; b < 256; b++ {
		c0[b] = b < 0x20 || b > 0x7E
	}
	c0['%'] = true
	t[SetC0Control] = c0
	t[SetFragment] = c0
	add(&t[SetFragment], " \"<>`")
	t[SetQuery] = c0
	add(&t[SetQuery], " \"#<>")
	t[SetSpecialQuery] = t[SetQuery]
	add(&t[SetSpecialQuery], "'")
	t[SetPath] = t[SetQuery]
	add(&t[SetPath], "?^`{}")
	t[SetUserinfo] = t[SetPath]
	add(&t[SetUserinfo], "/:;=@[\\]|")
	t[SetComponent] = t[SetUserinfo]
	add(&t[SetComponent], "$%&+,")
	t[SetForm] = t[SetComponent]
	add(&t[SetForm], "!'()~")
	for b := 0; b < 256; b++ {
		c := byte(b)
		t[SetUnreserved][b] = !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~')
	}
	return t
}()

// InPercentSet reports whether byte b is escaped by set s.
func InPercentSet(s PercentSet, b byte) bool { return s < numPercentSets && percentSets[s][b] }

// PercentOptions configures percent-decoding (RFC 3986 §2.1).
type PercentOptions struct {
	// Set is the set strict decoding checks against (the input must be
	// exactly EncodePercent(decoded, Set)).
	Set PercentSet
	// Mode: Strict accepts only the canonical encoding for Set: upper-case
	// escapes (RFC 3986 §2.1 "should use uppercase"), every byte of Set
	// escaped and no other byte escaped. Lenient is the WHATWG URL §1.3
	// percent-decode: any case, any byte raw, and a '%' that does not
	// start a valid escape is kept as is.
	Mode Mode
	// RejectInvalidEscapes (Lenient only) makes a '%' not followed by two
	// hex digits CodeInvalidEscape instead of a literal: the RFC 3986
	// pct-encoded syntax check, and what net/url's PathUnescape accepts.
	RejectInvalidEscapes bool
}

// EncodePercent returns src with every byte of set s written as %XX
// (upper-case hex, RFC 3986 §2.1).
func EncodePercent(src []byte, s PercentSet) []byte {
	return AppendEncodePercent(nil, src, s)
}

// AppendEncodePercent appends the percent-encoding of src under s.
func AppendEncodePercent(dst, src []byte, s PercentSet) []byte {
	return appendPercent(dst, src, s, false)
}

func appendPercent(dst, src []byte, s PercentSet, spaceAsPlus bool) []byte {
	set := &percentSets[s%numPercentSets]
	dst = slices.Grow(dst, len(src))
	for _, c := range src {
		switch {
		case spaceAsPlus && c == ' ':
			dst = append(dst, '+')
		case set[c]:
			dst = append(dst, '%', hexUpper[c>>4], hexUpper[c&15])
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

// pctState is the percent-decoder state (WHATWG URL §1.3
// "percent-decode", RFC 3986 §2.1 pct-encoded = "%" HEXDIG HEXDIG).
type pctState uint8

const (
	// pctData: an ordinary byte, or '%' starting an escape.
	pctData pctState = iota
	// pctEscape: '%' read; expecting the first hex digit.
	pctEscape
	// pctEscapeHex: '%' and one hex digit read; expecting the second.
	pctEscapeHex
)

// DecodePercent decodes src under o.
func DecodePercent(src []byte, o PercentOptions) ([]byte, error) {
	out, err := AppendDecodePercent(nil, src, o)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []byte{}
	}
	return out, nil
}

// AppendDecodePercent appends the decoding of src to dst; on error dst is
// returned unchanged.
func AppendDecodePercent(dst, src []byte, o PercentOptions) ([]byte, error) {
	return decodePercent(dst, src, o, false)
}

// decodePercent runs the state machine. With plus (form components), a
// raw '+' decodes to a space (WHATWG URL §5.1 step 3.4) and strict mode
// expects space written as '+'.
func decodePercent(dst, src []byte, o PercentOptions, plus bool) ([]byte, error) {
	set := &percentSets[o.Set%numPercentSets]
	strict := o.Mode == Strict
	out := slices.Grow(dst, len(src))
	state, esc := pctData, 0
	var hi byte
	// literal handles a '%' that does not start a valid escape: WHATWG
	// keeps the bytes; strict mode and RejectInvalidEscapes refuse them.
	literal := func() error {
		if strict || o.RejectInvalidEscapes {
			return errAt(CodeInvalidEscape, esc)
		}
		out = append(out, src[esc:esc+int(state)]...)
		state = pctData
		return nil
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case pctData:
			switch {
			case c == '%':
				state, esc = pctEscape, i
			case plus && c == '+':
				out = append(out, ' ')
			case strict && (set[c] || plus && c == ' '):
				return dst, errAt(CodeNonCanonical, i)
			default:
				out = append(out, c)
			}
		case pctEscape:
			if hexVal[c] == 0xFF {
				if err := literal(); err != nil {
					return dst, err
				}
				i-- // reconsume c as data
				continue
			}
			hi, state = hexVal[c], pctEscapeHex
		case pctEscapeHex:
			if hexVal[c] == 0xFF {
				if err := literal(); err != nil {
					return dst, err
				}
				i--
				continue
			}
			b := hi<<4 | hexVal[c]
			if strict {
				// Canonical: upper-case digits, and only bytes of the set
				// are escaped (a form space is '+', never %20).
				if !isUpperHexDigit(src[i-1]) || !isUpperHexDigit(c) || !set[b] || plus && b == ' ' {
					return dst, errAt(CodeNonCanonical, esc)
				}
			}
			out = append(out, b)
			state = pctData
		}
	}
	if state != pctData {
		if err := literal(); err != nil {
			return dst, err
		}
	}
	return out, nil
}

// FormPair is one name-value pair of an application/x-www-form-urlencoded
// string (WHATWG URL §5).
type FormPair struct {
	Name, Value string
}

// AppendEncodeFormComponent appends src encoded with the
// application/x-www-form-urlencoded percent-encode set, space as '+'
// (WHATWG URL §5.2 serializer, step 1.2).
func AppendEncodeFormComponent(dst, src []byte) []byte { return appendPercent(dst, src, SetForm, true) }

// AppendDecodeFormComponent appends the decoding of one form name or
// value: '+' is a space, then percent-decoding. Strict mode accepts only
// AppendEncodeFormComponent output; Lenient is WHATWG (invalid escapes
// kept). No UTF-8 decoding is done here (ParseForm does it).
func AppendDecodeFormComponent(dst, src []byte, mode Mode) ([]byte, error) {
	return decodePercent(dst, src, PercentOptions{Set: SetForm, Mode: mode}, true)
}

// EncodeForm serializes pairs (WHATWG URL §5.2): name=value joined by '&',
// both encoded with AppendEncodeFormComponent. Names and values must be
// UTF-8; other bytes are escaped as they are.
func EncodeForm(pairs []FormPair) []byte { return AppendEncodeForm(nil, pairs) }

// AppendEncodeForm appends the serialization of pairs to dst.
func AppendEncodeForm(dst []byte, pairs []FormPair) []byte {
	for i, p := range pairs {
		if i > 0 {
			dst = append(dst, '&')
		}
		dst = AppendEncodeFormComponent(dst, []byte(p.Name))
		dst = append(dst, '=')
		dst = AppendEncodeFormComponent(dst, []byte(p.Value))
	}
	return dst
}

// ParseForm parses an application/x-www-form-urlencoded string (WHATWG URL
// §5.1): split on '&', skip empty sequences, split each at its first '='
// (none: the value is empty), decode '+' and escapes, then UTF-8 decode
// with replacement (Encoding Standard "UTF-8 decode without BOM": each
// maximal invalid subpart becomes U+FFFD).
//
// Strict accepts exactly EncodeForm output: no empty sequence, every
// sequence has '=', canonical components, and UTF-8 that needs no
// replacement (CodeInvalidByte at the component otherwise).
func ParseForm(src []byte, mode Mode) ([]FormPair, error) {
	var out []FormPair
	var buf []byte
	for start := 0; start <= len(src); {
		end := start
		for end < len(src) && src[end] != '&' {
			end++
		}
		seq := src[start:end]
		if len(seq) == 0 {
			if mode == Strict && len(src) > 0 {
				return nil, errAt(CodeNonCanonical, start)
			}
		} else {
			eq := -1
			for k, c := range seq {
				if c == '=' {
					eq = k
					break
				}
			}
			name, value, valueAt := seq, []byte(nil), end
			if eq >= 0 {
				name, value, valueAt = seq[:eq], seq[eq+1:], start+eq+1
			} else if mode == Strict {
				return nil, errAt(CodeNonCanonical, end)
			}
			var p FormPair
			for k, part := range [2][]byte{name, value} {
				at := start
				if k == 1 {
					at = valueAt
				}
				var err error
				buf, err = AppendDecodeFormComponent(buf[:0], part, mode)
				if err != nil {
					e := err.(*Error)
					return nil, errAt(e.Code, at+e.Offset)
				}
				if !utf8.Valid(buf) {
					if mode == Strict {
						return nil, errAt(CodeInvalidByte, at)
					}
					buf = appendUTF8Replacing(nil, buf)
				}
				if k == 0 {
					p.Name = string(buf)
				} else {
					p.Value = string(buf)
				}
			}
			out = append(out, p)
		}
		start = end + 1
	}
	return out, nil
}

// utf8State is the Encoding Standard §8.1.1 UTF-8 decoder state: how many
// continuation bytes are still needed (0 = between characters).
type utf8State struct {
	needed, seen int
	cp           rune
	lower, upper byte
}

// appendUTF8Replacing appends src decoded as UTF-8 with the Encoding
// Standard's error handling: every maximal subpart of an ill-formed
// sequence becomes one U+FFFD.
//
// PORT: Go's utf8.DecodeRune replaces one byte at a time and
// strings.ToValidUTF8 a whole run; neither is the Encoding Standard, so
// the decoder is written out. Rust's String::from_utf8_lossy matches it.
func appendUTF8Replacing(dst, src []byte) []byte {
	st := utf8State{lower: 0x80, upper: 0xBF}
	for i := 0; i < len(src); i++ {
		b := src[i]
		if st.needed == 0 {
			switch {
			case b < 0x80:
				dst = append(dst, b)
			case b >= 0xC2 && b <= 0xDF:
				st.needed, st.cp = 1, rune(b&0x1F)
			case b >= 0xE0 && b <= 0xEF:
				if b == 0xE0 {
					st.lower = 0xA0
				}
				if b == 0xED {
					st.upper = 0x9F
				}
				st.needed, st.cp = 2, rune(b&0xF)
			case b >= 0xF0 && b <= 0xF4:
				if b == 0xF0 {
					st.lower = 0x90
				}
				if b == 0xF4 {
					st.upper = 0x8F
				}
				st.needed, st.cp = 3, rune(b&7)
			default:
				dst = utf8.AppendRune(dst, utf8.RuneError)
			}
			continue
		}
		if b < st.lower || b > st.upper {
			// The sequence so far is a maximal subpart: replace it and
			// reprocess b from the start state.
			st = utf8State{lower: 0x80, upper: 0xBF}
			dst = utf8.AppendRune(dst, utf8.RuneError)
			i--
			continue
		}
		st.lower, st.upper = 0x80, 0xBF
		st.cp = st.cp<<6 | rune(b&0x3F)
		if st.seen++; st.seen == st.needed {
			dst = utf8.AppendRune(dst, st.cp)
			st = utf8State{lower: 0x80, upper: 0xBF}
		}
	}
	if st.needed != 0 {
		dst = utf8.AppendRune(dst, utf8.RuneError)
	}
	return dst
}
