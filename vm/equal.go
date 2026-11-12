package vm

import (
	"cmp"
	"encoding/binary"
	"math"
	"strings"
)

// Equal reports structural equality. Floats compare by IEEE equality
// (NaN cannot occur), so 0.0 == -0.0.
func Equal(a, b Value) bool {
	if a.k != b.k {
		return false
	}
	switch a.k {
	case KindUnit:
		return true
	case KindBool, KindInt, KindDuration, KindTime:
		return a.n == b.n
	case KindFloat:
		return math.Float64frombits(a.n) == math.Float64frombits(b.n)
	case KindList:
		if a.n != b.n {
			return false
		}
		ae, be := a.Elems(), b.Elems()
		if a.p == b.p {
			return true
		}
		for i := range ae {
			if !Equal(ae[i], be[i]) {
				return false
			}
		}
		return true
	case KindMap:
		am, bm := a.p.(*mapObj), b.p.(*mapObj)
		if am == bm {
			return true
		}
		if len(am.keys) != len(bm.keys) {
			return false
		}
		for i := range am.keys {
			if !Equal(am.keys[i], bm.keys[i]) || !Equal(am.vals[i], bm.vals[i]) {
				return false
			}
		}
		return true
	case KindRecord, KindEnum, KindClosure:
		return a.n == b.n && equalSlices(a.Fields(), b.Fields())
	case KindScopeProof:
		ap, bp := a.p.(*proofObj), b.p.(*proofObj)
		return ap.decision == bp.decision && Equal(ap.target, bp.target)
	}
	if a.k.isStringy() {
		return a.Str() == b.Str()
	}
	return false
}

func equalSlices(a, b []Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Compare is a total structural order: first by kind, then by payload
// (numbers numerically, strings bytewise, sequences lexicographically,
// records and enums by type id, variant, then fields). The comparison
// instructions only accept operands of the same kind.
func Compare(a, b Value) int {
	if a.k != b.k {
		return cmp.Compare(a.k, b.k)
	}
	switch a.k {
	case KindUnit:
		return 0
	case KindBool:
		return cmp.Compare(a.n, b.n)
	case KindInt, KindDuration, KindTime:
		return cmp.Compare(int64(a.n), int64(b.n))
	case KindFloat:
		x, y := math.Float64frombits(a.n), math.Float64frombits(b.n)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case KindList:
		return compareSlices(a.Elems(), b.Elems())
	case KindMap:
		am, bm := a.p.(*mapObj), b.p.(*mapObj)
		for i := 0; i < len(am.keys) && i < len(bm.keys); i++ {
			if c := Compare(am.keys[i], bm.keys[i]); c != 0 {
				return c
			}
			if c := Compare(am.vals[i], bm.vals[i]); c != 0 {
				return c
			}
		}
		return cmp.Compare(len(am.keys), len(bm.keys))
	case KindRecord, KindEnum, KindClosure:
		if c := cmp.Compare(a.n, b.n); c != 0 {
			return c
		}
		return compareSlices(a.Fields(), b.Fields())
	case KindScopeProof:
		ap, bp := a.p.(*proofObj), b.p.(*proofObj)
		if c := Compare(ap.target, bp.target); c != 0 {
			return c
		}
		return strings.Compare(ap.decision, bp.decision)
	}
	return strings.Compare(a.Str(), b.Str())
}

func compareSlices(a, b []Value) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}

// keyString is an injective binary encoding used to index maps and dedupe
// lists. Structurally equal values have equal keys.
func keyString(v Value) string {
	var b []byte
	b = appendKey(b, v)
	return string(b)
}

func appendKey(b []byte, v Value) []byte {
	b = append(b, byte(v.k))
	switch v.k {
	case KindUnit:
	case KindBool, KindInt, KindDuration, KindTime:
		b = binary.LittleEndian.AppendUint64(b, v.n)
	case KindFloat:
		f := math.Float64frombits(v.n)
		if f == 0 {
			f = 0 // fold -0 into +0
		}
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(f))
	case KindList:
		b = binary.AppendUvarint(b, v.n)
		for _, e := range v.Elems() {
			b = appendKey(b, e)
		}
	case KindMap:
		m := v.p.(*mapObj)
		b = binary.AppendUvarint(b, uint64(len(m.keys)))
		for i := range m.keys {
			b = appendKey(b, m.keys[i])
			b = appendKey(b, m.vals[i])
		}
	case KindRecord, KindEnum, KindClosure:
		b = binary.LittleEndian.AppendUint64(b, v.n)
		fs := v.Fields()
		b = binary.AppendUvarint(b, uint64(len(fs)))
		for _, e := range fs {
			b = appendKey(b, e)
		}
	case KindScopeProof:
		p := v.p.(*proofObj)
		b = appendKey(b, p.target)
		b = binary.AppendUvarint(b, uint64(len(p.decision)))
		b = append(b, p.decision...)
	default:
		s := v.Str()
		b = binary.AppendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	return b
}
