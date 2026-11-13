package codec

// Mode selects how strictly a decoder reads its input.
type Mode uint8

const (
	// Strict accepts exactly the canonical encodings: for base64, base32,
	// hex, percent-encoding and form-urlencoding the input must be the
	// encoder's output for the decoded bytes (CODEC-INV-02; base32 and hex
	// ignore letter case). For quoted-printable and JavaScript strings,
	// which have many valid encodings, Strict enforces the MUSTs of RFC
	// 2045 §6.7 and ECMA-262 strict mode code (CODEC-INV-03). Strict is
	// the zero value.
	Strict Mode = iota
	// Lenient accepts what real-world producers emit: white space, missing
	// padding, non-zero trailing bits, invalid escapes passed through as
	// the WHATWG and MIME decoders do. Lenient accepts every input Strict
	// accepts, with the same output (CODEC-INV-04).
	Lenient
)

// String returns "strict" or "lenient".
func (m Mode) String() string {
	if m == Lenient {
		return "lenient"
	}
	return "strict"
}

// isSpace reports the white space lenient decoders skip: SP, HT, LF, VT,
// FF and CR (the ASCII white space of RFC 2045 line wrapping and of
// pretty-printed hex dumps).
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

// hexVal maps an ASCII hex digit (either case) to its value, else 0xFF.
var hexVal = func() (t [256]byte) {
	for i := range t {
		t[i] = 0xFF
	}
	for c := byte('0'); c <= '9'; c++ {
		t[c] = c - '0'
	}
	for c := byte('a'); c <= 'f'; c++ {
		t[c] = c - 'a' + 10
		t[c-'a'+'A'] = c - 'a' + 10
	}
	return t
}()

const hexUpper = "0123456789ABCDEF"
const hexLower = "0123456789abcdef"

func isUpperHexDigit(c byte) bool { return c >= '0' && c <= '9' || c >= 'A' && c <= 'F' }
