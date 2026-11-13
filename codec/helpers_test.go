package codec

import (
	"bytes"
	"errors"
	"testing"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/core"
)

// textCodec is one text encoding variant under test.
type textCodec struct {
	name   string
	encode func(src []byte) []byte
	decode func(src []byte, m Mode) ([]byte, error)
	// fold maps an encoding to the form canonical comparison uses (ASCII
	// case for base32 and hex); nil is the identity.
	fold func([]byte) []byte
	// unique: the codec has one canonical encoding per input, so strict
	// accepts exactly encode's image (CODEC-INV-02). QP and JS do not.
	unique bool
	// alphabet drives near-valid input generation.
	alphabet string
	// domain restricts round-trip inputs (JS: UTF-8); nil means all.
	domain func([]byte) bool
}

func (c textCodec) canon(b []byte) []byte {
	if c.fold == nil {
		return b
	}
	return c.fold(b)
}

const b64Chars = alphaStd + "-_= \r\n\t!"

func base64Codec(name string, alpha Base64Alphabet, noPad bool, algo Base64Algo) textCodec {
	return textCodec{
		name: name,
		encode: func(src []byte) []byte {
			return EncodeBase64(src, Base64Options{Alphabet: alpha, NoPadding: noPad})
		},
		decode: func(src []byte, m Mode) ([]byte, error) {
			return DecodeBase64(src, Base64Options{Alphabet: alpha, NoPadding: noPad, Mode: m, Algo: algo})
		},
		unique: true, alphabet: b64Chars,
	}
}

func base32Codec(name string, alpha Base32Alphabet, noPad bool) textCodec {
	return textCodec{
		name:   name,
		encode: func(src []byte) []byte { return EncodeBase32(src, Base32Options{Alphabet: alpha, NoPadding: noPad}) },
		decode: func(src []byte, m Mode) ([]byte, error) {
			return DecodeBase32(src, Base32Options{Alphabet: alpha, NoPadding: noPad, Mode: m})
		},
		fold: bytes.ToUpper, unique: true, alphabet: alpha32Std + alpha32Hex + "abcxyz=1 \n",
	}
}

func hexCodec(name string, upper bool) textCodec {
	return textCodec{
		name:   name,
		encode: func(src []byte) []byte { return EncodeHex(src, HexOptions{Upper: upper}) },
		decode: func(src []byte, m Mode) ([]byte, error) { return DecodeHex(src, HexOptions{Mode: m}) },
		fold:   bytes.ToLower, unique: true, alphabet: "0123456789abcdefABCDEFg \n",
	}
}

func percentCodec(set PercentSet) textCodec {
	return textCodec{
		name:   "percent-" + set.String(),
		encode: func(src []byte) []byte { return EncodePercent(src, set) },
		decode: func(src []byte, m Mode) ([]byte, error) {
			return DecodePercent(src, PercentOptions{Set: set, Mode: m})
		},
		unique: true, alphabet: "%%%0123456789abcdefABCDEF/?#&=+ ~!*'()^`{}<>\"az",
	}
}

func textCodecs() []textCodec {
	cs := []textCodec{
		base64Codec("base64", Base64Std, false, 0),
		base64Codec("base64url", Base64URL, false, 0),
		base64Codec("base64-nopad", Base64Std, true, 0),
		base64Codec("base64url-nopad", Base64URL, true, 0),
		base32Codec("base32", Base32Std, false),
		base32Codec("base32hex", Base32Hex, false),
		base32Codec("base32-nopad", Base32Std, true),
		base32Codec("base32hex-nopad", Base32Hex, true),
		hexCodec("hex", false),
		hexCodec("hex-upper", true),
	}
	for s := PercentSet(0); s < numPercentSets; s++ {
		cs = append(cs, percentCodec(s))
	}
	cs = append(cs,
		textCodec{
			name:   "form-component",
			encode: func(src []byte) []byte { return AppendEncodeFormComponent(nil, src) },
			decode: func(src []byte, m Mode) ([]byte, error) {
				out, err := AppendDecodeFormComponent(nil, src, m)
				if err == nil && out == nil {
					out = []byte{}
				}
				return out, err
			},
			unique: true, alphabet: "%%0123456789ABCDEFabcdef+ ~!*-._=&",
		},
		textCodec{
			name:     "qp",
			encode:   EncodeQP,
			decode:   DecodeQP,
			alphabet: "=\r\n \t0123456789ABCDEFabcdefxyz\x01\x80",
		},
		textCodec{
			name:   "js",
			encode: func(src []byte) []byte { return EncodeJSString(src, '"') },
			decode: func(src []byte, m Mode) ([]byte, error) {
				return DecodeJSString(src, JSOptions{Mode: m, Quote: '"'})
			},
			alphabet: "\\\\\\xu{}0123456789abcdefABCDEFDdnrtbv'\"\n\r8",
			domain:   validWTF8,
		},
	)
	return cs
}

func codecByName(t testing.TB, name string) textCodec {
	t.Helper()
	for _, c := range textCodecs() {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("no codec %q", name)
	return textCodec{}
}

// validWTF8 reports UTF-8 where unpaired surrogates (ED A0-BF xx) may
// appear, but never a high surrogate followed by a low one (WTF-8 writes
// that pair as one 4-byte character).
func validWTF8(b []byte) bool {
	prevHigh := false
	for i := 0; i < len(b); {
		if b[i] == 0xED && i+2 < len(b) && b[i+1] >= 0xA0 && b[i+1] <= 0xBF && b[i+2]&0xC0 == 0x80 {
			high := b[i+1] < 0xB0
			if !high && prevHigh {
				return false
			}
			prevHigh = high
			i += 3
			continue
		}
		r, n := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && n == 1 {
			return false
		}
		prevHigh = false
		i += n
	}
	return true
}

func randBytes(r core.Rng, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint64())
	}
	return b
}

// randInput draws round-trip inputs: random bytes, ASCII text with
// controls and white space, or UTF-8 with surrogates for JS.
func randInput(r core.Rng, c textCodec) []byte {
	n := core.IntN(r, 64)
	if core.IntN(r, 8) == 0 {
		n = core.IntN(r, 400)
	}
	switch core.IntN(r, 3) {
	case 0:
		if c.domain == nil {
			return randBytes(r, n)
		}
	case 1:
		b := make([]byte, n)
		const ascii = "abc XYZ09=+/%&\r\n\t\"'\\\x00\x7f~"
		for i := range b {
			b[i] = ascii[core.IntN(r, len(ascii))]
		}
		return b
	}
	var b []byte
	for len(b) < n {
		switch core.IntN(r, 6) {
		case 0:
			b = utf8.AppendRune(b, rune(0x80+core.IntN(r, 0x700)))
		case 1:
			b = utf8.AppendRune(b, rune(0x10000+core.IntN(r, 0x1000)))
		case 2:
			b = utf8.AppendRune(b, 0x2028+rune(core.IntN(r, 2)))
		case 3:
			// A lone surrogate in WTF-8.
			s := 0xD800 + core.IntN(r, 0x800)
			cand := append(append([]byte{}, b...), 0xED, byte(0x80|(s>>6)&0x3F), byte(0x80|s&0x3F))
			if validWTF8(cand) && (c.domain == nil || c.domain(cand)) {
				b = cand
			}
		default:
			b = append(b, byte(0x20+core.IntN(r, 0x5F)))
		}
	}
	return b
}

// randEncoded draws decoder inputs: canonical encodings, mutated
// encodings, and random strings over the codec's alphabet.
func randEncoded(r core.Rng, c textCodec) []byte {
	switch core.IntN(r, 4) {
	case 0:
		return c.encode(randInput(r, c))
	case 1, 2:
		return mutate(r, c.encode(randInput(r, c)), c.alphabet)
	}
	b := make([]byte, core.IntN(r, 40))
	for i := range b {
		b[i] = c.alphabet[core.IntN(r, len(c.alphabet))]
	}
	return b
}

// mutate applies one to three random edits drawn from alphabet.
func mutate(r core.Rng, b []byte, alphabet string) []byte {
	b = append([]byte{}, b...)
	for k := 1 + core.IntN(r, 3); k > 0; k-- {
		ch := alphabet[core.IntN(r, len(alphabet))]
		switch op := core.IntN(r, 4); {
		case len(b) == 0 || op == 0:
			i := core.IntN(r, len(b)+1)
			b = append(b[:i], append([]byte{ch}, b[i:]...)...)
		case op == 1:
			i := core.IntN(r, len(b))
			b = append(b[:i], b[i+1:]...)
		case op == 2:
			b[core.IntN(r, len(b))] = ch
		default:
			i := core.IntN(r, len(b))
			if i%2 == 0 {
				b[i] = byte(bytes.ToLower([]byte{b[i]})[0])
			} else {
				b[i] = byte(bytes.ToUpper([]byte{b[i]})[0])
			}
		}
	}
	return b
}

// checkErr asserts the CODEC-INV-11 contract of one decoder error.
func checkErr(t *testing.T, what string, err error, inputLen int) {
	t.Helper()
	if err == nil {
		return
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("%s: error %v is not *Error", what, err)
	}
	if e.Code == 0 || int(e.Code) >= len(codeNames) || !errors.Is(err, e.Code.Sentinel()) {
		t.Fatalf("%s: bad code %v (%v)", what, e.Code, err)
	}
	if inputLen >= 0 && (e.Offset < 0 || e.Offset > inputLen) {
		t.Fatalf("%s: offset %d outside [0, %d]", what, e.Offset, inputLen)
	}
}
