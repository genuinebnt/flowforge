package treewalk

import (
	"math"
	"math/bits"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler/internal/abi"
	"github.com/genuinebnt/flowforge/dsl"
	"github.com/genuinebnt/flowforge/vm"
)

// The arithmetic here is an independent implementation of spec 7.2 and
// 8.2 (checked i64, finite f64), so the differential test also checks the
// VM's arithmetic.

func isFloat(t dsl.Type) bool {
	p, ok := t.(*dsl.PrimType)
	return ok && p.Kind == dsl.PrimFloat
}

func primKind(t dsl.Type) dsl.PrimKind {
	if p, ok := t.(*dsl.PrimType); ok {
		return p.Kind
	}
	return dsl.PrimError
}

// intOf wraps an i64 as a value of the primitive kind k.
func intOf(k dsl.PrimKind, n int64) vm.Value {
	switch k {
	case dsl.PrimDuration:
		return vm.Duration(n)
	case dsl.PrimTime:
		return vm.Time(n)
	}
	return vm.Int(n)
}

func checkedAdd(a, b int64) (int64, bool) {
	c := a + b
	return c, (b >= 0) == (c >= a)
}

func checkedSub(a, b int64) (int64, bool) {
	c := a - b
	return c, (b >= 0) == (c <= a)
}

func checkedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	neg := (a < 0) != (b < 0)
	ua, ub := uabs(a), uabs(b)
	hi, lo := bits.Mul64(ua, ub)
	if hi != 0 {
		return 0, false
	}
	if neg {
		if lo > 1<<63 {
			return 0, false
		}
		return int64(-lo), true // two's complement of lo <= 2^63
	}
	if lo > math.MaxInt64 {
		return 0, false
	}
	return int64(lo), true
}

func uabs(a int64) uint64 {
	if a < 0 {
		return uint64(-(a + 1)) + 1
	}
	return uint64(a)
}

func (ev *Eval) arith(e *dsl.BinaryExpr, x, y vm.Value) (vm.Value, error) {
	if e.Op == dsl.OpAdd && primKind(e.X.Type()) == dsl.PrimString {
		s := x.Str() + y.Str()
		if len(s) > ev.p.vmcfg.Limits.String {
			return vm.Value{}, fail(bytecode.FailLimitExceeded, "string over 1 MiB", e)
		}
		return vm.Str(s), nil
	}
	if isFloat(e.X.Type()) || isFloat(e.Y.Type()) {
		a, b := x.AsFloat(), y.AsFloat()
		var r float64
		switch e.Op {
		case dsl.OpAdd:
			r = a + b
		case dsl.OpSub:
			r = a - b
		case dsl.OpMul:
			r = a * b
		case dsl.OpDiv:
			if b == 0 {
				return vm.Value{}, fail(bytecode.FailDivZero, "division by zero", e)
			}
			r = a / b
		default:
			return vm.Value{}, bug(e, "float %s", e.Op)
		}
		switch {
		case math.IsNaN(r):
			return vm.Value{}, fail(bytecode.FailArithNaN, "float result is NaN", e)
		case math.IsInf(r, 0):
			return vm.Value{}, fail(bytecode.FailArithOverflow, "float result is infinite", e)
		}
		return vm.Float(r), nil
	}
	a, b := x.AsInt(), y.AsInt()
	var r int64
	ok := true
	switch e.Op {
	case dsl.OpAdd:
		r, ok = checkedAdd(a, b)
	case dsl.OpSub:
		r, ok = checkedSub(a, b)
	case dsl.OpMul:
		r, ok = checkedMul(a, b)
	case dsl.OpDiv:
		if b == 0 {
			return vm.Value{}, fail(bytecode.FailDivZero, "division by zero", e)
		}
		if a == math.MinInt64 && b == -1 {
			ok = false
		} else {
			r = a / b
		}
	case dsl.OpRem:
		if b == 0 {
			return vm.Value{}, fail(bytecode.FailDivZero, "division by zero", e)
		}
		if b != -1 {
			r = a % b
		}
	default:
		return vm.Value{}, bug(e, "operator %s", e.Op)
	}
	if !ok {
		return vm.Value{}, fail(bytecode.FailArithOverflow, "integer overflow", e)
	}
	return intOf(primKind(e.Type()), r), nil
}

func unary(e *dsl.UnaryExpr, x vm.Value) (vm.Value, error) {
	if e.Op == dsl.OpNot {
		return vm.Bool(!x.AsBool()), nil
	}
	if isFloat(e.X.Type()) {
		return vm.Float(-x.AsFloat()), nil
	}
	n := x.AsInt()
	if n == math.MinInt64 {
		return vm.Value{}, fail(bytecode.FailArithOverflow, "integer overflow", e)
	}
	return intOf(primKind(e.X.Type()), -n), nil
}

func index(e *dsl.IndexExpr, x, i vm.Value) (vm.Value, error) {
	switch x.Kind() {
	case vm.KindList:
		n := i.AsInt()
		if n < 0 || n >= int64(x.Len()) {
			return vm.Value{}, fail(bytecode.FailIndexOOB, "list index out of range", e)
		}
		return x.Elems()[n], nil
	case vm.KindMap:
		v, ok := x.MapGet(i)
		if !ok {
			return vm.Value{}, fail(bytecode.FailIndexOOB, "key not in map", e)
		}
		return v, nil
	}
	return vm.Value{}, bug(e, "indexing a %s", x.Kind())
}

// cast converts per CastExpr.Kind.
func (ev *Eval) cast(e *dsl.CastExpr, x vm.Value) (vm.Value, error) {
	switch e.Kind {
	case dsl.CastNone:
		return x, nil
	case dsl.CastIntToFloat:
		return vm.Float(float64(x.AsInt())), nil
	case dsl.CastFloatToInt:
		f := math.Trunc(x.AsFloat())
		if f < -9.223372036854775808e18 || f >= 9.223372036854775808e18 {
			return vm.Value{}, fail(bytecode.FailArithOverflow, "integer overflow", e)
		}
		return vm.Int(int64(f)), nil
	case dsl.CastIntToDuration:
		return vm.Duration(x.AsInt()), nil
	case dsl.CastDurationToInt, dsl.CastTimeToInt:
		return vm.Int(x.AsInt()), nil
	case dsl.CastIntToTime:
		return vm.Time(x.AsInt()), nil
	case dsl.CastToString:
		return vm.Str(x.Str()), nil
	case dsl.CastParseDomain:
		var def *dsl.TypeDef
		if n, ok := e.Type().(*dsl.NamedType); ok {
			def = n.Def
		}
		id, ok := abi.DomainParser(def)
		if !ok {
			return vm.Value{}, bug(e, "no parser for %v", e.Type())
		}
		r, err := ev.native(id, []vm.Value{x}, e)
		if err != nil {
			return vm.Value{}, err
		}
		if !r.IsSome() {
			return vm.Value{}, fail(bytecode.FailUnwrapFailed, "invalid "+def.Name, e)
		}
		return r.Fields()[0], nil
	}
	return vm.Value{}, bug(e, "cast kind %d", e.Kind)
}
