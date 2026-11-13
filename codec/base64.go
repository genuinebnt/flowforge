package codec

import "slices"

// Base64Alphabet selects the 62nd and 63rd characters (RFC 4648 §4, §5).
type Base64Alphabet uint8

const (
	// Base64Std is the base64 alphabet of RFC 4648 §4 ('+', '/').
	Base64Std Base64Alphabet = iota
	// Base64URL is the URL- and filename-safe alphabet of RFC 4648 §5
	// ('-', '_').
	Base64URL
)

// Base64Options configures base64 encoding and decoding.
type Base64Options struct {
	// Alphabet is the encoder's alphabet and the only one strict decoding
	// accepts. Lenient decoding accepts both alphabets, also mixed.
	Alphabet Base64Alphabet
	// NoPadding drops the '=' padding (RFC 4648 §3.2). A strict decoder
	// then rejects '='; with padding it requires exactly the canonical
	// padding. Lenient decoding accepts full, partial or no padding.
	NoPadding bool
	// Mode is Strict (canonical only) or Lenient (MIME-style: ASCII white
	// space skipped, padding optional, trailing bits ignored, both
	// alphabets).
	Mode Mode
	// Algo selects the decoder implementation. Every candidate has the
	// same semantics, errors included (CODEC-INV-06); the zero value is
	// the default chosen in docs/ALGORITHMS.md.
	Algo Base64Algo
}

const (
	alphaStd = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	alphaURL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
)

// Decode-table markers. Values below 64 are sextets.
const (
	b64Space   = 0xFE // ASCII white space (lenient tables only)
	b64Pad     = 0xFD // '='
	b64Invalid = 0xFF
)

// b64Table indexes the three decode tables: strict standard, strict URL,
// and lenient (both alphabets plus white space).
type b64Table uint8

const (
	b64TabStd b64Table = iota
	b64TabURL
	b64TabLenient
)

var b64Tables = func() (t [3][256]byte) {
	for k := range t {
		for i := range t[k] {
			t[k][i] = b64Invalid
		}
		t[k]['='] = b64Pad
	}
	for i := 0; i < 64; i++ {
		t[b64TabStd][alphaStd[i]] = byte(i)
		t[b64TabURL][alphaURL[i]] = byte(i)
		t[b64TabLenient][alphaStd[i]] = byte(i)
		t[b64TabLenient][alphaURL[i]] = byte(i)
	}
	for _, c := range []byte(" \t\n\v\f\r") {
		t[b64TabLenient][c] = b64Space
	}
	return t
}()

// table picks the decode table: lenient takes both alphabets, strict only
// the configured one.
func (o Base64Options) table() b64Table {
	switch {
	case o.Mode == Lenient:
		return b64TabLenient
	case o.Alphabet == Base64URL:
		return b64TabURL
	}
	return b64TabStd
}

// EncodedLenBase64 is the length of the encoding of n bytes.
func EncodedLenBase64(n int, noPadding bool) int {
	if noPadding {
		return n/3*4 + (n%3*8+5)/6
	}
	return (n + 2) / 3 * 4
}

// DecodedLenMaxBase64 bounds the decoded length of n input bytes.
func DecodedLenMaxBase64(n int) int { return n/4*3 + 2 }

// EncodeBase64 returns the RFC 4648 encoding of src.
func EncodeBase64(src []byte, o Base64Options) []byte {
	return AppendEncodeBase64(make([]byte, 0, EncodedLenBase64(len(src), o.NoPadding)), src, o)
}

// AppendEncodeBase64 appends the RFC 4648 encoding of src to dst. The
// output is canonical: the trailing bits of a partial quantum are zero
// and padding (unless NoPadding) completes the last quantum.
func AppendEncodeBase64(dst, src []byte, o Base64Options) []byte {
	alpha := alphaStd
	if o.Alphabet == Base64URL {
		alpha = alphaURL
	}
	dst = slices.Grow(dst, EncodedLenBase64(len(src), o.NoPadding))
	i := 0
	for ; len(src)-i >= 3; i += 3 {
		v := uint32(src[i])<<16 | uint32(src[i+1])<<8 | uint32(src[i+2])
		dst = append(dst, alpha[v>>18], alpha[v>>12&63], alpha[v>>6&63], alpha[v&63])
	}
	switch len(src) - i {
	case 1:
		v := uint32(src[i]) << 16
		dst = append(dst, alpha[v>>18], alpha[v>>12&63])
		if !o.NoPadding {
			dst = append(dst, '=', '=')
		}
	case 2:
		v := uint32(src[i])<<16 | uint32(src[i+1])<<8
		dst = append(dst, alpha[v>>18], alpha[v>>12&63], alpha[v>>6&63])
		if !o.NoPadding {
			dst = append(dst, '=')
		}
	}
	return dst
}

// DecodeBase64 decodes src (RFC 4648 §4 or §5) under o.
func DecodeBase64(src []byte, o Base64Options) ([]byte, error) {
	out, err := AppendDecodeBase64(nil, src, o)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []byte{}
	}
	return out, nil
}

// AppendDecodeBase64 appends the decoding of src to dst. On error dst is
// returned unchanged with an *Error (CodeInvalidByte, CodeBadPadding,
// CodeTrailingBits or CodeTruncated).
func AppendDecodeBase64(dst, src []byte, o Base64Options) ([]byte, error) {
	switch o.Algo.resolve() {
	case Base64AlgoSWAR:
		return decodeBase64SWAR(dst, src, o)
	case Base64AlgoStdlib:
		return decodeBase64Stdlib(dst, src, o)
	}
	d := newB64Dec(dst, src, o) // a value: the decoder stays on the stack
	d.run(src, 0, false)
	return d.finish(dst, len(src))
}

// b64State is the decoder state of RFC 4648 §4: the position inside the
// current 4-character quantum, or the padding that closes the data.
type b64State uint8

const (
	// b64Quantum0: expecting the first character of a quantum, or the
	// end of the data.
	b64Quantum0 b64State = iota
	// b64Quantum1: one sextet of the quantum read; a second is required
	// (a single character never encodes a byte).
	b64Quantum1
	// b64Quantum2: two sextets read; a third, or "==" ending a final
	// quantum of one byte (§4 case 3), or (unpadded) the end.
	b64Quantum2
	// b64Quantum3: three sextets read; a fourth, or "=" ending a final
	// quantum of two bytes (§4 case 2), or (unpadded) the end.
	b64Quantum3
	// b64PadSecond: "xx=" read; the second '=' of a one-byte final
	// quantum must follow (lenient: may be missing).
	b64PadSecond
	// b64Done: padding closed the data; only lenient white space may
	// follow.
	b64Done
)

// b64Dec is the explicit state machine shared by every candidate: the
// fast candidates hand it every quantum they cannot take whole, so errors
// and edge cases are decided in one place.
type b64Dec struct {
	tab    *[256]byte
	strict bool
	padded bool
	state  b64State
	acc    uint32 // sextets of the current quantum, first at bit 18
	out    []byte
	err    error
}

// newB64Dec returns a decoder appending to dst, with room for the longest
// output src can produce.
func newB64Dec(dst, src []byte, o Base64Options) b64Dec {
	return b64Dec{
		tab:    &b64Tables[o.table()],
		strict: o.Mode == Strict,
		padded: !o.NoPadding,
		out:    slices.Grow(dst, DecodedLenMaxBase64(len(src))),
	}
}

// run feeds src[i:] to the machine. With boundary it returns as soon as a
// quantum completes, so a fast path can take over; it also returns at the
// end of src or at the first error. It returns the next input index.
func (d *b64Dec) run(src []byte, i int, boundary bool) int {
	for ; i < len(src); i++ {
		v := d.tab[src[i]]
		if v < 64 {
			switch d.state {
			case b64Quantum0:
				d.acc, d.state = uint32(v)<<18, b64Quantum1
			case b64Quantum1:
				d.acc, d.state = d.acc|uint32(v)<<12, b64Quantum2
			case b64Quantum2:
				d.acc, d.state = d.acc|uint32(v)<<6, b64Quantum3
			case b64Quantum3:
				a := d.acc | uint32(v)
				d.out = append(d.out, byte(a>>16), byte(a>>8), byte(a))
				d.state = b64Quantum0
				if boundary {
					return i + 1
				}
			default:
				// RFC 4648 §3.3: data after the padding is not base64.
				d.err = errAt(CodeBadPadding, i)
				return i
			}
			continue
		}
		switch v {
		case b64Space:
			continue
		case b64Pad:
			switch d.state {
			case b64Quantum2:
				if d.strict && !d.padded {
					d.err = errAt(CodeBadPadding, i)
					return i
				}
				if !d.flush(1, i-1) {
					return i
				}
				d.state = b64PadSecond
			case b64Quantum3:
				if d.strict && !d.padded {
					d.err = errAt(CodeBadPadding, i)
					return i
				}
				if !d.flush(2, i-1) {
					return i
				}
				d.state = b64Done
			case b64PadSecond:
				d.state = b64Done
			default:
				// '=' at the start of a quantum, after one character, or
				// after the padding is complete.
				d.err = errAt(CodeBadPadding, i)
				return i
			}
		default:
			d.err = errAt(CodeInvalidByte, i)
			return i
		}
	}
	return i
}

// flush emits the n bytes of a final partial quantum and, in strict mode,
// checks that the bits after them are zero (RFC 4648 §3.5: canonical
// encodings set them to zero). last is the offset of the last data
// character, used in the error.
func (d *b64Dec) flush(n, last int) bool {
	a := d.acc
	if n == 1 {
		if d.strict && a&0xF000 != 0 {
			d.err = errAt(CodeTrailingBits, last)
			return false
		}
		d.out = append(d.out, byte(a>>16))
		return true
	}
	if d.strict && a&0xC0 != 0 {
		d.err = errAt(CodeTrailingBits, last)
		return false
	}
	d.out = append(d.out, byte(a>>16), byte(a>>8))
	return true
}

// finish applies the end-of-data rules and returns the output or the
// first error (with dst unchanged).
func (d *b64Dec) finish(dst []byte, n int) ([]byte, error) {
	if d.err == nil {
		switch d.state {
		case b64Quantum1:
			d.err = errAt(CodeTruncated, n)
		case b64Quantum2, b64Quantum3:
			if d.strict && d.padded {
				d.err = errAt(CodeBadPadding, n)
				break
			}
			k := 1
			if d.state == b64Quantum3 {
				k = 2
			}
			d.flush(k, n-1)
		case b64PadSecond:
			if d.strict {
				d.err = errAt(CodeBadPadding, n)
			}
		}
	}
	if d.err != nil {
		return dst, d.err
	}
	return d.out, nil
}
