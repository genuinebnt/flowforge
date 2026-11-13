package vm

import (
	"math"
	"math/bits"

	"github.com/genuinebnt/flowforge/bytecode"
)

func addInt(a, b int64) (int64, bool) {
	c := a + b
	return c, (c > a) == (b > 0)
}

func subInt(a, b int64) (int64, bool) {
	c := a - b
	return c, (c < a) == (b > 0)
}

func mulInt(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	hi, lo := bits.Mul64(uint64(absInt(a)), uint64(absInt(b)))
	neg := (a < 0) != (b < 0)
	if hi != 0 {
		return 0, false
	}
	if neg {
		if lo > 1<<63 {
			return 0, false
		}
		return -int64(lo), true
	}
	if lo > math.MaxInt64 {
		return 0, false
	}
	return int64(lo), true
}

// absInt returns |a| as a uint64-safe magnitude (MinInt64 maps to 1<<63).
func absInt(a int64) uint64 {
	if a < 0 {
		return uint64(-(a + 1)) + 1
	}
	return uint64(a)
}

// intArith applies a checked integer instruction to Int, Duration and Time
// operands (spec 8.4: overflow is arith_overflow, a zero divisor
// div_zero; division truncates toward zero and % takes the dividend's
// sign). Allowed combinations: Int op Int -> Int; Duration +- Duration ->
// Duration; Time + Duration, Duration + Time, Time - Duration -> Time;
// Time - Time -> Duration; Duration * Int, Int * Duration -> Duration;
// Duration / Int, Duration % Int -> Duration; Duration / Duration -> Int.
func intArith(op bytecode.Opcode, a, b Value) (Value, *fault) {
	x, y := int64(a.n), int64(b.n)
	var rk Kind
	switch {
	case a.k == KindInt && b.k == KindInt:
		rk = KindInt
	case op == bytecode.OpAddI && a.k == KindDuration && b.k == KindDuration,
		op == bytecode.OpSubI && a.k == KindDuration && b.k == KindDuration:
		rk = KindDuration
	case op == bytecode.OpAddI && ((a.k == KindTime && b.k == KindDuration) || (a.k == KindDuration && b.k == KindTime)),
		op == bytecode.OpSubI && a.k == KindTime && b.k == KindDuration:
		rk = KindTime
	case op == bytecode.OpSubI && a.k == KindTime && b.k == KindTime:
		rk = KindDuration
	case op == bytecode.OpMulI && ((a.k == KindDuration && b.k == KindInt) || (a.k == KindInt && b.k == KindDuration)):
		rk = KindDuration
	case (op == bytecode.OpDivI || op == bytecode.OpModI) && a.k == KindDuration && b.k == KindInt:
		rk = KindDuration
	case op == bytecode.OpDivI && a.k == KindDuration && b.k == KindDuration:
		rk = KindInt
	default:
		return Value{}, typeFault(op.String() + " on " + a.k.String() + " and " + b.k.String())
	}
	var r int64
	ok := true
	switch op {
	case bytecode.OpAddI:
		r, ok = addInt(x, y)
	case bytecode.OpSubI:
		r, ok = subInt(x, y)
	case bytecode.OpMulI:
		r, ok = mulInt(x, y)
	case bytecode.OpDivI:
		if y == 0 {
			return Value{}, errDivZero
		}
		if x == math.MinInt64 && y == -1 {
			return Value{}, errOverflow
		}
		r = x / y
	case bytecode.OpModI:
		if y == 0 {
			return Value{}, errDivZero
		}
		// x % -1 is 0 for every x. Spelling it out keeps MinInt64 % -1
		// well defined. PORT: Rust's % panics on that overflow.
		if y == -1 {
			r = 0
		} else {
			r = x % y
		}
	}
	if !ok {
		return Value{}, errOverflow
	}
	return Value{k: rk, n: uint64(r)}, nil
}

func negInt(a Value) (Value, *fault) {
	if a.k != KindInt && a.k != KindDuration {
		return Value{}, typeFault("NEG_I on " + a.k.String())
	}
	x := int64(a.n)
	if x == math.MinInt64 {
		return Value{}, errOverflow
	}
	return Value{k: a.k, n: uint64(-x)}, nil
}

func floatArith(op bytecode.Opcode, a, b Value) (Value, *fault) {
	if a.k != KindFloat || b.k != KindFloat {
		return Value{}, typeFault(op.String() + " on " + a.k.String() + " and " + b.k.String())
	}
	x, y := a.AsFloat(), b.AsFloat()
	var r float64
	switch op {
	case bytecode.OpAddF:
		r = x + y
	case bytecode.OpSubF:
		r = x - y
	case bytecode.OpMulF:
		r = x * y
	case bytecode.OpDivF:
		if y == 0 {
			return Value{}, errDivZero
		}
		r = x / y
	}
	return checkFloat(r)
}

func checkFloat(r float64) (Value, *fault) {
	if math.IsNaN(r) {
		return Value{}, errNaN
	}
	if math.IsInf(r, 0) {
		return Value{}, errFloatInf
	}
	return Float(r), nil
}

// compareOp evaluates LT, LE, GT and GE on two values of the same kind.
func compareOp(op bytecode.Opcode, a, b Value) (bool, *fault) {
	if a.k != b.k {
		return false, typeFault(op.String() + " on " + a.k.String() + " and " + b.k.String())
	}
	c := Compare(a, b)
	switch op {
	case bytecode.OpLt:
		return c < 0, nil
	case bytecode.OpLe:
		return c <= 0, nil
	case bytecode.OpGt:
		return c > 0, nil
	}
	return c >= 0, nil
}
