package codec

import "slices"

// HexOptions configures base16 (RFC 4648 §8). Decoding is
// case-insensitive in both modes.
type HexOptions struct {
	// Upper makes the encoder write A-F (the RFC 4648 §8 alphabet); the
	// default is lower case, as hash digests are usually written.
	Upper bool
	// Mode is Strict (pairs of hex digits only) or Lenient (ASCII white
	// space between pairs is skipped).
	Mode Mode
}

// EncodeHex returns the base16 encoding of src.
func EncodeHex(src []byte, o HexOptions) []byte {
	return AppendEncodeHex(make([]byte, 0, 2*len(src)), src, o)
}

// AppendEncodeHex appends the base16 encoding of src to dst.
func AppendEncodeHex(dst, src []byte, o HexOptions) []byte {
	digits := hexLower
	if o.Upper {
		digits = hexUpper
	}
	dst = slices.Grow(dst, 2*len(src))
	for _, c := range src {
		dst = append(dst, digits[c>>4], digits[c&15])
	}
	return dst
}

// hexState is the base16 decoder state (RFC 4648 §8: each byte is two
// characters, high nibble first).
type hexState uint8

const (
	// hexHigh: expecting the high nibble of a byte, white space
	// (lenient), or the end.
	hexHigh hexState = iota
	// hexLow: expecting the low nibble.
	hexLow
)

// DecodeHex decodes src under o.
func DecodeHex(src []byte, o HexOptions) ([]byte, error) {
	out, err := AppendDecodeHex(nil, src, o)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []byte{}
	}
	return out, nil
}

// AppendDecodeHex appends the decoding of src to dst; on error dst is
// returned unchanged. An odd number of digits is CodeTruncated.
func AppendDecodeHex(dst, src []byte, o HexOptions) ([]byte, error) {
	out := slices.Grow(dst, len(src)/2)
	state := hexHigh
	var hi byte
	for i := 0; i < len(src); i++ {
		c := src[i]
		v := hexVal[c]
		switch {
		case v != 0xFF && state == hexHigh:
			hi, state = v, hexLow
		case v != 0xFF:
			out = append(out, hi<<4|v)
			state = hexHigh
		case state == hexHigh && o.Mode == Lenient && isSpace(c):
		default:
			return dst, errAt(CodeInvalidByte, i)
		}
	}
	if state == hexLow {
		return dst, errAt(CodeTruncated, len(src))
	}
	return out, nil
}
