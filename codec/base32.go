package codec

import "slices"

// Base32Alphabet selects the RFC 4648 base32 alphabet.
type Base32Alphabet uint8

const (
	// Base32Std is the base32 alphabet of RFC 4648 §6 (A-Z, 2-7).
	Base32Std Base32Alphabet = iota
	// Base32Hex is the "extended hex" alphabet of RFC 4648 §7 (0-9, A-V),
	// which preserves sort order.
	Base32Hex
)

// Base32Options configures base32 encoding and decoding. Decoding is
// case-insensitive in both modes (the encoder writes upper case), so
// "canonical" for base32 means canonical up to ASCII case.
type Base32Options struct {
	Alphabet Base32Alphabet
	// NoPadding drops the '=' padding; a strict decoder then rejects it.
	NoPadding bool
	// Mode is Strict (canonical structure: exact padding, zero trailing
	// bits, no white space) or Lenient (white space skipped, padding
	// optional, short or surplus, trailing bits ignored).
	Mode Mode
}

const (
	alpha32Std = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	alpha32Hex = "0123456789ABCDEFGHIJKLMNOPQRSTUV"
)

// b32Tables[alphabet] maps a character (either case) to its 5-bit value,
// '=' to b64Pad, white space to b64Space and the rest to b64Invalid.
var b32Tables = func() (t [2][256]byte) {
	for k, alpha := range []string{alpha32Std, alpha32Hex} {
		for i := range t[k] {
			t[k][i] = b64Invalid
		}
		t[k]['='] = b64Pad
		for _, c := range []byte(" \t\n\v\f\r") {
			t[k][c] = b64Space
		}
		for i := 0; i < 32; i++ {
			c := alpha[i]
			t[k][c] = byte(i)
			if c >= 'A' && c <= 'Z' {
				t[k][c+'a'-'A'] = byte(i)
			}
		}
	}
	return t
}()

// b32Partial[p] is the number of bytes a final quantum of p characters
// encodes (RFC 4648 §6 cases 1-5), or -1 when no encoder emits p.
var b32Partial = [8]int{0, -1, 1, -1, 2, 3, -1, 4}

// EncodedLenBase32 is the length of the encoding of n bytes.
func EncodedLenBase32(n int, noPadding bool) int {
	if noPadding {
		return n/5*8 + (n%5*8+4)/5
	}
	return (n + 4) / 5 * 8
}

// EncodeBase32 returns the RFC 4648 §6/§7 encoding of src.
func EncodeBase32(src []byte, o Base32Options) []byte {
	return AppendEncodeBase32(make([]byte, 0, EncodedLenBase32(len(src), o.NoPadding)), src, o)
}

// AppendEncodeBase32 appends the canonical upper-case encoding of src.
func AppendEncodeBase32(dst, src []byte, o Base32Options) []byte {
	alpha := alpha32Std
	if o.Alphabet == Base32Hex {
		alpha = alpha32Hex
	}
	dst = slices.Grow(dst, EncodedLenBase32(len(src), o.NoPadding))
	for i := 0; i < len(src); i += 5 {
		var q [5]byte
		n := copy(q[:], src[i:])
		v := uint64(q[0])<<32 | uint64(q[1])<<24 | uint64(q[2])<<16 | uint64(q[3])<<8 | uint64(q[4])
		chars := 8
		if n < 5 {
			chars = (n*8 + 4) / 5
		}
		for k := 0; k < chars; k++ {
			dst = append(dst, alpha[v>>(35-5*k)&31])
		}
		if chars < 8 && !o.NoPadding {
			for k := chars; k < 8; k++ {
				dst = append(dst, '=')
			}
		}
	}
	return dst
}

// b32State is the decoder state of RFC 4648 §6: the position inside the
// 8-character quantum (b32Pos0 + k), the padding of a final quantum, or
// done.
type b32State uint8

const (
	// b32Pos0: expecting the first character of a quantum or the end.
	// b32Pos0+k (k = 1..7): k characters of the quantum read.
	b32Pos0 b32State = iota
	b32Pos7 b32State = 7
	// b32Padding: a final quantum's '=' run has started; padLeft more
	// '=' complete it.
	b32Padding b32State = 8
	// b32Done: the padding is complete; only lenient white space follows.
	b32Done b32State = 9
)

// DecodeBase32 decodes src under o.
func DecodeBase32(src []byte, o Base32Options) ([]byte, error) {
	out, err := AppendDecodeBase32(nil, src, o)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []byte{}
	}
	return out, nil
}

// AppendDecodeBase32 appends the decoding of src to dst; on error dst is
// returned unchanged.
func AppendDecodeBase32(dst, src []byte, o Base32Options) ([]byte, error) {
	tab := &b32Tables[0]
	if o.Alphabet == Base32Hex {
		tab = &b32Tables[1]
	}
	strict, padded := o.Mode == Strict, !o.NoPadding
	out := slices.Grow(dst, len(src)/8*5+4)
	state, padLeft := b32Pos0, 0
	var acc uint64
	// flush emits the bytes of a final quantum of p characters and checks
	// (strict) that the bits after them are zero.
	flush := func(p, last int) error {
		nb := b32Partial[p]
		bits := uint(5*p - 8*nb)
		if strict && acc&(1<<bits-1) != 0 {
			return errAt(CodeTrailingBits, last)
		}
		v := acc >> bits << (40 - 8*uint(nb))
		for k := 0; k < nb; k++ {
			out = append(out, byte(v>>(32-8*k)))
		}
		return nil
	}
	for i := 0; i < len(src); i++ {
		v := tab[src[i]]
		switch {
		case v < 32:
			if state > b32Pos7 {
				return dst, errAt(CodeBadPadding, i)
			}
			acc = acc<<5 | uint64(v)
			if state == b32Pos7 {
				out = append(out, byte(acc>>32), byte(acc>>24), byte(acc>>16), byte(acc>>8), byte(acc))
				acc, state = 0, b32Pos0
			} else {
				state++
			}
		case v == b64Space && !strict:
		case v == b64Pad:
			switch {
			case state == b32Done && !strict:
				// Lenient: surplus padding is ignored (encoding/base32
				// accepts it too).
			case state == b32Padding:
				if padLeft--; padLeft == 0 {
					state = b32Done
				}
			case state <= b32Pos7 && b32Partial[state] > 0 && !(strict && !padded):
				if err := flush(int(state), i-1); err != nil {
					return dst, err
				}
				padLeft = 8 - int(state) - 1
				state = b32Padding
				if padLeft == 0 {
					state = b32Done
				}
			default:
				return dst, errAt(CodeBadPadding, i)
			}
		default:
			return dst, errAt(CodeInvalidByte, i)
		}
	}
	switch {
	case state == b32Pos0 || state == b32Done:
	case state == b32Padding:
		if strict {
			return dst, errAt(CodeBadPadding, len(src))
		}
	case b32Partial[state] < 0:
		return dst, errAt(CodeTruncated, len(src))
	default:
		if strict && padded {
			return dst, errAt(CodeBadPadding, len(src))
		}
		if err := flush(int(state), len(src)-1); err != nil {
			return dst, err
		}
	}
	return out, nil
}
