package sourcemap

import (
	"math"
	"strings"
)

// base64Value maps a byte to its Base64 digit (RFC 4648 §4 alphabet), or
// -1. ECMA-426 §"Base64 VLQ".
var base64Value = func() (t [256]int8) {
	for i := range t {
		t[i] = -1
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for i := 0; i < len(alphabet); i++ {
		t[alphabet[i]] = int8(i)
	}
	return
}()

const base64Digits = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// Mapping is one decoded segment. GenLine and GenCol are 0-based
// positions in the generated file; Source, OrigLine, OrigCol and Name are
// absolute (deltas resolved). Source, OrigLine and OrigCol are -1 for a
// 1-field segment, Name is -1 for 1- and 4-field segments.
type Mapping struct {
	GenLine, GenCol   int32
	Source            int32
	OrigLine, OrigCol int32
	Name              int32
}

// mapState is the "mappings" decoder state (ECMA-426 §"Mappings
// structure": lines separated by ";", segments by ",", each segment 1, 4
// or 5 Base64 VLQ fields).
type mapState uint8

const (
	// stateSegmentStart: at the start of a segment (after ",", ";" or at
	// the beginning), no digit read yet.
	stateSegmentStart mapState = iota
	// stateVLQ: inside a VLQ value (a continuation digit was read).
	stateVLQ
	// stateFieldEnd: after a complete VLQ value, before the next value
	// or a separator.
	stateFieldEnd
)

// decodeState carries the running absolute values across segments.
type decodeState struct {
	line, genCol              int64
	source, origLine, origCol int64
	name                      int64
}

// DecodeMappings decodes a "mappings" string. nSources and nNames bound
// the source and name indices (pass -1 to skip the check). Mappings come
// back in encoded order; lines counts the generated lines (separators
// plus one), so EncodeMappings(ms, lines) reproduces a canonical input.
//
// Values must fit a signed 32-bit integer (vlq_overflow otherwise), and
// every absolute value must be non-negative and within int32.
func DecodeMappings(s string, nSources, nNames int) (ms []Mapping, lines int, err *Error) {
	ms = make([]Mapping, 0, len(s)/4)
	st := stateSegmentStart
	var acc int64
	var shift uint
	var fields [5]int64
	nf := 0
	segStart := 0
	afterComma := false
	var ds decodeState
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' || s[i] == ';' {
			if st == stateVLQ {
				return nil, 0, errAt(CodeVLQTruncated, i)
			}
			switch {
			case nf == 0 && (afterComma || (i < len(s) && s[i] == ',')):
				return nil, 0, errAt(CodeInvalidSegment, i)
			case nf == 0:
				// An empty line (";;" or a trailing ";"): no segment.
			case nf == 2 || nf == 3:
				return nil, 0, errAt(CodeInvalidSegment, segStart)
			default:
				m, e := ds.apply(fields[:nf], nSources, nNames, segStart)
				if e != nil {
					return nil, 0, e
				}
				ms = append(ms, m)
			}
			nf, st = 0, stateSegmentStart
			afterComma = i < len(s) && s[i] == ','
			segStart = i + 1
			if i < len(s) && s[i] == ';' {
				ds.line++
				ds.genCol = 0 // the generated column restarts on each line
				if ds.line > math.MaxInt32 {
					return nil, 0, errAt(CodeVLQOverflow, i)
				}
			}
			continue
		}
		d := base64Value[s[i]]
		if d < 0 {
			return nil, 0, errAt(CodeInvalidVLQChar, i)
		}
		if st != stateVLQ {
			if nf == 5 {
				return nil, 0, errAt(CodeInvalidSegment, segStart)
			}
			acc, shift = 0, 0
		}
		// Seven digits carry 4+5*6 = 34 bits; an eighth can only overflow.
		if shift > 30 {
			return nil, 0, errAt(CodeVLQOverflow, i)
		}
		acc |= int64(d&31) << shift
		shift += 5
		if d&32 != 0 {
			st = stateVLQ
			continue
		}
		// The lowest bit of the first digit is the sign.
		v := acc >> 1
		if acc&1 != 0 {
			// PORT: "B" (negative zero) decodes to 0, as in mozilla/source-map
			// and the sourcemap crate (see OPEN_QUESTIONS.md).
			v = -v
		}
		if v > math.MaxInt32 || v < math.MinInt32 {
			return nil, 0, errAt(CodeVLQOverflow, i)
		}
		fields[nf] = v
		nf++
		st = stateFieldEnd
	}
	return ms, int(ds.line) + 1, nil
}

// apply adds one segment's deltas to the running values and checks them.
func (ds *decodeState) apply(f []int64, nSources, nNames, at int) (Mapping, *Error) {
	ds.genCol += f[0]
	m := Mapping{GenLine: int32(ds.line), Source: -1, OrigLine: -1, OrigCol: -1, Name: -1}
	if ds.genCol < 0 || ds.genCol > math.MaxInt32 {
		return m, errAt(CodeNegativeValue, at)
	}
	m.GenCol = int32(ds.genCol)
	if len(f) == 1 {
		return m, nil
	}
	ds.source += f[1]
	ds.origLine += f[2]
	ds.origCol += f[3]
	for _, v := range []int64{ds.source, ds.origLine, ds.origCol} {
		if v < 0 || v > math.MaxInt32 {
			return m, errAt(CodeNegativeValue, at)
		}
	}
	if nSources >= 0 && ds.source >= int64(nSources) {
		return m, errAt(CodeIndexOutOfRange, at)
	}
	m.Source, m.OrigLine, m.OrigCol = int32(ds.source), int32(ds.origLine), int32(ds.origCol)
	if len(f) == 5 {
		ds.name += f[4]
		if ds.name < 0 || ds.name > math.MaxInt32 {
			return m, errAt(CodeNegativeValue, at)
		}
		if nNames >= 0 && ds.name >= int64(nNames) {
			return m, errAt(CodeIndexOutOfRange, at)
		}
		m.Name = int32(ds.name)
	}
	return m, nil
}

// AppendVLQ appends the Base64 VLQ encoding of v (ECMA-426 §"Base64 VLQ":
// sign in the lowest bit of the first digit, 5 bits per digit, low digits
// first, bit 6 marks continuation). The encoding is minimal.
func AppendVLQ(b []byte, v int32) []byte {
	x := int64(v) << 1
	if v < 0 {
		x = (-int64(v))<<1 | 1
	}
	for {
		d := x & 31
		x >>= 5
		if x > 0 {
			d |= 32
		}
		b = append(b, base64Digits[d])
		if x == 0 {
			return b
		}
	}
}

// DecodeVLQ decodes one VLQ value from the start of s and returns it with
// the number of bytes used.
func DecodeVLQ(s string) (int32, int, *Error) {
	var acc int64
	var shift uint
	for i := 0; i < len(s); i++ {
		d := base64Value[s[i]]
		if d < 0 {
			return 0, 0, errAt(CodeInvalidVLQChar, i)
		}
		if shift > 30 {
			return 0, 0, errAt(CodeVLQOverflow, i)
		}
		acc |= int64(d&31) << shift
		shift += 5
		if d&32 == 0 {
			v := acc >> 1
			if acc&1 != 0 {
				v = -v
			}
			if v > math.MaxInt32 || v < math.MinInt32 {
				return 0, 0, errAt(CodeVLQOverflow, i)
			}
			return int32(v), i + 1, nil
		}
	}
	return 0, 0, errAt(CodeVLQTruncated, len(s))
}

// EncodeMappings encodes ms (GenLine non-decreasing; order within a line
// is kept) as a "mappings" string with lines generated lines. It fails
// with CodeUnsortedMappings when GenLine decreases or a mapping lies past
// lines, and CodeNegativeValue for negative positions.
func EncodeMappings(ms []Mapping, lines int) (string, *Error) {
	var b []byte
	var prev decodeState
	line := int32(0)
	first := true
	for i, m := range ms {
		if m.GenLine < line || int(m.GenLine) >= max(lines, 1) {
			return "", errAt(CodeUnsortedMappings, i)
		}
		if m.GenCol < 0 || (m.Source >= 0 && (m.OrigLine < 0 || m.OrigCol < 0)) {
			return "", errAt(CodeNegativeValue, i)
		}
		for line < m.GenLine {
			b = append(b, ';')
			line++
			prev.genCol = 0
			first = true
		}
		if !first {
			b = append(b, ',')
		}
		first = false
		b = AppendVLQ(b, int32(int64(m.GenCol)-prev.genCol))
		prev.genCol = int64(m.GenCol)
		if m.Source < 0 {
			continue
		}
		b = AppendVLQ(b, int32(int64(m.Source)-prev.source))
		b = AppendVLQ(b, int32(int64(m.OrigLine)-prev.origLine))
		b = AppendVLQ(b, int32(int64(m.OrigCol)-prev.origCol))
		prev.source, prev.origLine, prev.origCol = int64(m.Source), int64(m.OrigLine), int64(m.OrigCol)
		if m.Name >= 0 {
			b = AppendVLQ(b, int32(int64(m.Name)-prev.name))
			prev.name = int64(m.Name)
		}
	}
	for int(line) < lines-1 {
		b = append(b, ';')
		line++
	}
	return string(b), nil
}

// isCanonical reports whether every VLQ in s is minimal and no "-0"
// appears: the strings EncodeMappings produces.
func isCanonical(s string) bool {
	for _, seg := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
		for len(seg) > 0 {
			v, n, err := DecodeVLQ(seg)
			if err != nil {
				return false
			}
			if string(AppendVLQ(nil, v)) != seg[:n] {
				return false
			}
			seg = seg[n:]
		}
	}
	return true
}
