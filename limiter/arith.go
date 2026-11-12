package limiter

import "math/bits"

// floorDiv returns floor(a / b) for b > 0.
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && (a < 0) {
		q--
	}
	return q
}

// u128 is an unsigned 128-bit integer for products of counts and window
// lengths, which can exceed 64 bits.
type u128 struct{ hi, lo uint64 }

func mul(a, b int64) u128 {
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	return u128{hi, lo}
}

func (x u128) add(y u128) u128 {
	lo, carry := bits.Add64(x.lo, y.lo, 0)
	hi, _ := bits.Add64(x.hi, y.hi, carry)
	return u128{hi, lo}
}

func (x u128) sub(y u128) u128 {
	lo, borrow := bits.Sub64(x.lo, y.lo, 0)
	hi, _ := bits.Sub64(x.hi, y.hi, borrow)
	return u128{hi, lo}
}

func (x u128) cmp(y u128) int {
	switch {
	case x.hi != y.hi:
		if x.hi < y.hi {
			return -1
		}
		return 1
	case x.lo < y.lo:
		return -1
	case x.lo > y.lo:
		return 1
	}
	return 0
}

// div returns x / d; the caller guarantees the quotient fits in 64 bits.
func (x u128) div(d int64) int64 {
	q, _ := bits.Div64(x.hi, x.lo, uint64(d))
	return int64(q)
}
