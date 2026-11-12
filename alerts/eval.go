package alerts

import (
	"errors"
	"math"
	"strings"
	"unicode/utf8"
)

// Run-time evaluation failures. A rule whose condition or group_by fails
// does not match the event; the failure is reported in Evaluation.Errors.
var (
	ErrArithOverflow  = errors.New("alerts: arith_overflow")
	ErrDivisionByZero = errors.New("alerts: division by zero")
)

// evalCtx is the per-evaluation state threaded through the closures.
type evalCtx struct {
	ev  *Event
	err error
}

// fail records the first run-time error and yields null.
func (c *evalCtx) fail(err error) value {
	if c.err == nil {
		c.err = err
	}
	return nullValue
}

// evalFn is a compiled expression.
type evalFn func(ctx *evalCtx) value

// Absent values (kNull) follow three-valued (Kleene) logic: operators and
// functions on an absent operand yield absent, `false && absent` is false,
// `true || absent` is true, and a condition that evaluates to absent does
// not match. A rule over entity fields therefore never fires on a run
// event, whatever its comparison operators.

// binaryOp returns the result type and closure for a type-correct
// operator application, or a nil closure when the types do not fit.
func binaryOp(op string, lt, rt typ, l, r evalFn) (typ, evalFn) {
	switch op {
	case "&&":
		if lt.k == kBool && rt.k == kBool {
			return tBoolT, andFn(l, r)
		}
	case "||":
		if lt.k == kBool && rt.k == kBool {
			return tBoolT, orFn(l, r)
		}
	case "==", "!=":
		if comparable(lt, rt) {
			neg := op == "!="
			return tBoolT, strict2(l, r, func(_ *evalCtx, a, b value) value { return boolVal(valuesEqual(a, b) != neg) })
		}
	case "<", "<=", ">", ">=":
		if ordered(lt, rt) {
			test := orderTest(op)
			return tBoolT, strict2(l, r, func(_ *evalCtx, a, b value) value { return boolVal(test(compareValues(a, b))) })
		}
	case "+", "-", "*", "/", "%":
		return arith(op, lt, rt, l, r)
	case "contains":
		switch {
		case lt.k == kString && rt.k == kString:
			return tBoolT, strict2(l, r, func(_ *evalCtx, a, b value) value { return boolVal(strings.Contains(a.s, b.s)) })
		case lt.k == kList && rt.isScalar() && elemFits(lt.elem, rt.k):
			return tBoolT, strict2(l, r, func(_ *evalCtx, a, b value) value { return boolVal(listHas(a.list, b)) })
		case lt.k == kObject && rt.k == kString:
			return tBoolT, strict2(l, r, func(_ *evalCtx, a, b value) value { _, ok := a.obj[b.s]; return boolVal(ok) })
		}
	case "starts_with":
		if lt.k == kString && rt.k == kString {
			return tBoolT, strict2(l, r, func(_ *evalCtx, a, b value) value { return boolVal(strings.HasPrefix(a.s, b.s)) })
		}
	case "ends_with":
		if lt.k == kString && rt.k == kString {
			return tBoolT, strict2(l, r, func(_ *evalCtx, a, b value) value { return boolVal(strings.HasSuffix(a.s, b.s)) })
		}
	}
	return tInvalidT, nil
}

// inOp builds `x in collection`; lo/hi are set for ranges.
func inOp(lt, rt typ, l, r, lo, hi evalFn) evalFn {
	switch {
	case rt.k == kRange && lt.k == kInt:
		return func(ctx *evalCtx) value {
			x, a, b := l(ctx), lo(ctx), hi(ctx)
			if x.k == kNull || a.k == kNull || b.k == kNull {
				return nullValue
			}
			return boolVal(x.i >= a.i && x.i < b.i)
		}
	case rt.k == kList && lt.isScalar() && elemFits(rt.elem, lt.k):
		return strict2(l, r, func(_ *evalCtx, x, list value) value { return boolVal(listHas(list.list, x)) })
	case rt.k == kString && lt.k == kString:
		return strict2(l, r, func(_ *evalCtx, x, s value) value { return boolVal(strings.Contains(s.s, x.s)) })
	case rt.k == kObject && lt.k == kString:
		return strict2(l, r, func(_ *evalCtx, x, o value) value { _, ok := o.obj[x.s]; return boolVal(ok) })
	}
	return nil
}

// strict2 evaluates both operands and applies f unless one is absent.
func strict2(l, r evalFn, f func(ctx *evalCtx, a, b value) value) evalFn {
	return func(ctx *evalCtx) value {
		a := l(ctx)
		if a.k == kNull {
			return nullValue
		}
		b := r(ctx)
		if b.k == kNull {
			return nullValue
		}
		return f(ctx, a, b)
	}
}

func andFn(l, r evalFn) evalFn {
	return func(ctx *evalCtx) value {
		a := l(ctx)
		if a.k == kBool && !a.b {
			return a
		}
		b := r(ctx)
		if b.k == kBool && !b.b {
			return b
		}
		if a.k == kNull || b.k == kNull {
			return nullValue
		}
		return boolVal(true)
	}
}

func orFn(l, r evalFn) evalFn {
	return func(ctx *evalCtx) value {
		a := l(ctx)
		if a.k == kBool && a.b {
			return a
		}
		b := r(ctx)
		if b.k == kBool && b.b {
			return b
		}
		if a.k == kNull || b.k == kNull {
			return nullValue
		}
		return boolVal(false)
	}
}

func comparable(a, b typ) bool {
	if a.isNumeric() && b.isNumeric() {
		return true
	}
	return a.k == b.k && a.isScalar()
}

func ordered(a, b typ) bool {
	if a.isNumeric() && b.isNumeric() {
		return true
	}
	return a.k == b.k && (a.k == kString || a.k == kDuration || a.k == kTime)
}

func elemFits(elem, k kind) bool {
	if elem == kAny || elem == k {
		return true
	}
	return (elem == kInt || elem == kFloat) && (k == kInt || k == kFloat)
}

func orderTest(op string) func(int) bool {
	switch op {
	case "<":
		return func(c int) bool { return c < 0 }
	case "<=":
		return func(c int) bool { return c <= 0 }
	case ">":
		return func(c int) bool { return c > 0 }
	}
	return func(c int) bool { return c >= 0 }
}

// valuesEqual compares two present scalar values.
func valuesEqual(a, b value) bool {
	if a.k != b.k {
		if (a.k == kInt || a.k == kFloat) && (b.k == kInt || b.k == kFloat) {
			return a.num() == b.num()
		}
		return false
	}
	switch a.k {
	case kBool:
		return a.b == b.b
	case kInt, kDuration, kTime:
		return a.i == b.i
	case kFloat:
		return a.f == b.f
	case kString:
		return a.s == b.s
	}
	return false
}

// compareValues orders two present values of an ordered type. NaN
// compares as equal to everything, so every ordering test except <= and
// >= is false for it.
func compareValues(a, b value) int {
	if a.k == kFloat || b.k == kFloat {
		x, y := a.num(), b.num()
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	}
	if a.k == kString {
		return strings.Compare(a.s, b.s)
	}
	switch {
	case a.i < b.i:
		return -1
	case a.i > b.i:
		return 1
	}
	return 0
}

func listHas(list []value, x value) bool {
	for _, e := range list {
		if valuesEqual(e, x) {
			return true
		}
	}
	return false
}

func lenOf(v value) value {
	switch v.k {
	case kString:
		return intVal(int64(utf8.RuneCountInString(v.s)))
	case kList:
		return intVal(int64(len(v.list)))
	case kObject:
		return intVal(int64(len(v.obj)))
	}
	return nullValue
}

// arith types and builds the arithmetic operators, including the time
// arithmetic of rules such as approval.expires_at - event.occurred_at > 6h.
func arith(op string, lt, rt typ, l, r evalFn) (typ, evalFn) {
	switch {
	case lt.k == kInt && rt.k == kInt:
		return tIntT, strict2(l, r, func(ctx *evalCtx, a, b value) value { return intArith(ctx, op, a.i, b.i, kInt) })
	case lt.isNumeric() && rt.isNumeric() && op != "%":
		return tFloatT, strict2(l, r, func(_ *evalCtx, a, b value) value { return floatVal(floatArith(op, a.num(), b.num())) })
	case lt.k == kString && rt.k == kString && op == "+":
		return tStringT, strict2(l, r, func(_ *evalCtx, a, b value) value { return stringVal(a.s + b.s) })
	case lt.k == kDuration && rt.k == kDuration && (op == "+" || op == "-"):
		return tDurT, strict2(l, r, func(ctx *evalCtx, a, b value) value { return intArith(ctx, op, a.i, b.i, kDuration) })
	case lt.k == kTime && rt.k == kDuration && (op == "+" || op == "-"):
		return tTimeT, strict2(l, r, func(ctx *evalCtx, a, b value) value { return intArith(ctx, op, a.i, b.i, kTime) })
	case lt.k == kDuration && rt.k == kTime && op == "+":
		return tTimeT, strict2(l, r, func(ctx *evalCtx, a, b value) value { return intArith(ctx, op, a.i, b.i, kTime) })
	case lt.k == kTime && rt.k == kTime && op == "-":
		return tDurT, strict2(l, r, func(ctx *evalCtx, a, b value) value { return intArith(ctx, op, a.i, b.i, kDuration) })
	case lt.k == kDuration && rt.k == kInt && (op == "*" || op == "/"):
		return tDurT, strict2(l, r, func(ctx *evalCtx, a, b value) value { return intArith(ctx, op, a.i, b.i, kDuration) })
	case lt.k == kInt && rt.k == kDuration && op == "*":
		return tDurT, strict2(l, r, func(ctx *evalCtx, a, b value) value { return intArith(ctx, op, a.i, b.i, kDuration) })
	}
	return tInvalidT, nil
}

func intArith(ctx *evalCtx, op string, a, b int64, k kind) value {
	var r int64
	ok := true
	switch op {
	case "+":
		r, ok = addInt(a, b)
	case "-":
		r, ok = subInt(a, b)
	case "*":
		r, ok = mulInt(a, b)
	case "/":
		if b == 0 {
			return ctx.fail(ErrDivisionByZero)
		}
		if a == math.MinInt64 && b == -1 {
			return ctx.fail(ErrArithOverflow)
		}
		r = a / b
	case "%":
		if b == 0 {
			return ctx.fail(ErrDivisionByZero)
		}
		r = a % b
	}
	if !ok {
		return ctx.fail(ErrArithOverflow)
	}
	return value{k: k, i: r}
}

func floatArith(op string, a, b float64) float64 {
	switch op {
	case "+":
		return a + b
	case "-":
		return a - b
	case "*":
		return a * b
	}
	return a / b
}

func addInt(a, b int64) (int64, bool) {
	r := a + b
	return r, (r > a) == (b > 0)
}

func subInt(a, b int64) (int64, bool) {
	r := a - b
	return r, (r < a) == (b > 0)
}

func mulInt(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	r := a * b
	if r/b != a || (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
		return 0, false
	}
	return r, true
}
