package alerts

import (
	"regexp"
	"regexp/syntax"
	"strings"
)

// Regex limits for `matches` (spec 15.2: RE2 syntax, size-limited). The
// pattern must be a string literal so it is compiled and bounded once, at
// rule compile time.
const (
	MaxPatternLen  = 1024 // bytes of pattern text
	MaxPatternInst = 4096 // instructions of the compiled RE2 program
)

// checker type-checks an expression tree against the event schema and, in
// the same pass, compiles it into evaluator closures. A node that fails to
// check yields tInvalidT, which silences diagnostics that would only
// cascade from the first one.
type checker struct {
	diag *diagSink
}

func (c *checker) check(e expr) (typ, evalFn) {
	switch n := e.(type) {
	case *litExpr:
		v := n.val
		return typ{k: v.k}, func(*evalCtx) value { return v }
	case *pathExpr:
		return c.path(n)
	case *unaryExpr:
		return c.unary(n)
	case *binaryExpr:
		return c.binary(n)
	case *callExpr:
		return c.call(n)
	case *listExpr:
		return c.list(n)
	case *badExpr:
		return tInvalidT, nil
	case *rangeExpr:
		c.diag.add(CodeUnsupported, n.sp, "range here", "a range is only valid on the right of `in`")
		return tInvalidT, nil
	}
	return tInvalidT, nil
}

func (c *checker) path(n *pathExpr) (typ, evalFn) {
	root := n.parts[0]
	fields := schemaFields(root)
	if fields == nil {
		c.diag.add(CodeUnknownVariable, n.spans[0], "unknown variable", "unknown variable `%s`", root).
			Help = "variables in scope: " + strings.Join(rootVariables(), ", ")
		return tInvalidT, nil
	}
	if len(n.parts) == 1 {
		c.diag.add(CodeUnknownField, n.sp, "a whole record", "`%s` is a record; select a field such as `%s.%s`", root, root, fields[0].name)
		return tInvalidT, nil
	}
	f, ok := lookupField(root, n.parts[1])
	if !ok {
		names := make([]string, len(fields))
		for i, f := range fields {
			names[i] = f.name
		}
		c.diag.add(CodeUnknownField, n.spans[1], "unknown field", "`%s` has no field `%s`", root, n.parts[1]).
			Help = "known fields: " + strings.Join(names, ", ")
		return tInvalidT, nil
	}
	if len(n.parts) > 2 {
		sp := n.spans[2].to(n.spans[len(n.spans)-1])
		c.diag.add(CodeUnknownField, sp, "no such field", "`%s.%s` has type %s and no fields", root, f.name, f.typ).
			Help = "fields of patch, payload and attributes are not typed; use `\"key\" in " + root + "." + f.name + "`"
		return tInvalidT, nil
	}
	get := f.get
	return f.typ, func(ctx *evalCtx) value { return get(ctx.ev) }
}

func (c *checker) unary(n *unaryExpr) (typ, evalFn) {
	t, x := c.check(n.x)
	if t.k == kInvalid {
		return tInvalidT, nil
	}
	switch {
	case n.op == "!" && t.k == kBool:
		return tBoolT, func(ctx *evalCtx) value {
			v := x(ctx)
			if v.k != kBool {
				return nullValue
			}
			return boolVal(!v.b)
		}
	case n.op == "-" && (t.k == kInt || t.k == kDuration):
		return t, func(ctx *evalCtx) value {
			v := x(ctx)
			if v.k == kNull {
				return v
			}
			r, ok := subInt(0, v.i)
			if !ok {
				return ctx.fail(ErrArithOverflow)
			}
			v.i = r
			return v
		}
	case n.op == "-" && t.k == kFloat:
		return t, func(ctx *evalCtx) value {
			v := x(ctx)
			if v.k == kNull {
				return v
			}
			return floatVal(-v.f)
		}
	}
	c.diag.add(CodeOperandType, n.sp, "operand has type "+t.String(), "operator `%s` cannot be applied to %s", n.op, t)
	return tInvalidT, nil
}

func (c *checker) binary(n *binaryExpr) (typ, evalFn) {
	if n.op == "in" {
		return c.in(n.l, n.r, n.sp, n.opSp)
	}
	if n.op == "matches" {
		return c.matches(n.l, n.r, n.sp)
	}
	lt, l := c.check(n.l)
	rt, r := c.check(n.r)
	if lt.k == kInvalid || rt.k == kInvalid {
		return tInvalidT, nil
	}
	t, fn := binaryOp(n.op, lt, rt, l, r)
	if fn == nil {
		d := c.diag.add(CodeOperandType, n.opSp, "operands are "+lt.String()+" and "+rt.String(),
			"operator `%s` cannot be applied to %s and %s", n.op, lt, rt)
		if lt.k == kTime && rt.k == kTime && n.op == "+" {
			d.Help = "subtract two times to get a Duration"
		}
		return tInvalidT, nil
	}
	return t, fn
}

func (c *checker) in(le, re expr, sp, opSp span) (typ, evalFn) {
	var rt typ
	var r evalFn
	var lo, hi evalFn
	if rg, ok := re.(*rangeExpr); ok {
		ltLo, l1 := c.check(rg.lo)
		ltHi, l2 := c.check(rg.hi)
		if ltLo.k == kInvalid || ltHi.k == kInvalid {
			return tInvalidT, nil
		}
		if ltLo.k != kInt || ltHi.k != kInt {
			c.diag.add(CodeOperandType, rg.sp, "bounds are "+ltLo.String()+" and "+ltHi.String(), "range bounds must be Int")
			return tInvalidT, nil
		}
		rt, lo, hi = tRangeT, l1, l2
	} else {
		rt, r = c.check(re)
	}
	lt, l := c.check(le)
	if lt.k == kInvalid || rt.k == kInvalid {
		return tInvalidT, nil
	}
	fn := inOp(lt, rt, l, r, lo, hi)
	if fn == nil {
		c.diag.add(CodeOperandType, opSp, "operands are "+lt.String()+" and "+rt.String(),
			"`in` cannot test %s against %s", lt, rt).
			Help = "use `x in [a, b]`, `n in 1..10`, `\"sub\" in text` or `\"key\" in change.patch`"
		return tInvalidT, nil
	}
	return tBoolT, fn
}

func (c *checker) matches(le, re expr, sp span) (typ, evalFn) {
	lt, l := c.check(le)
	if _, bad := re.(*badExpr); bad {
		return tInvalidT, nil
	}
	lit, ok := re.(*litExpr)
	if !ok || lit.val.k != kString {
		c.diag.add(CodeArgumentType, re.exprSpan(), "not a string literal", "the pattern of `matches` must be a string literal").
			Help = "patterns are compiled and size-checked when the rule is compiled"
		return tInvalidT, nil
	}
	rx := c.compileRegex(lit)
	if lt.k == kInvalid || rx == nil {
		return tInvalidT, nil
	}
	if lt.k != kString {
		c.diag.add(CodeOperandType, le.exprSpan(), "has type "+lt.String(), "`matches` needs a String, found %s", lt)
		return tInvalidT, nil
	}
	return tBoolT, func(ctx *evalCtx) value {
		v := l(ctx)
		if v.k != kString {
			return nullValue
		}
		return boolVal(rx.MatchString(v.s))
	}
}

func (c *checker) compileRegex(lit *litExpr) *regexp.Regexp {
	pat := lit.val.s
	if len(pat) > MaxPatternLen {
		c.diag.add(CodeRegexTooLarge, lit.sp, "pattern too long", "regular expression is %d bytes; the limit is %d", len(pat), MaxPatternLen)
		return nil
	}
	re, err := syntax.Parse(pat, syntax.Perl)
	if err != nil {
		c.diag.add(CodeInvalidRegex, lit.sp, "invalid pattern", "invalid regular expression: %v", err).
			Help = "patterns use RE2 syntax; write {{ and }} for literal braces inside strings"
		return nil
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		c.diag.add(CodeInvalidRegex, lit.sp, "invalid pattern", "invalid regular expression: %v", err)
		return nil
	}
	if len(prog.Inst) > MaxPatternInst {
		c.diag.add(CodeRegexTooLarge, lit.sp, "pattern too complex", "regular expression compiles to %d instructions; the limit is %d", len(prog.Inst), MaxPatternInst)
		return nil
	}
	rx, err := regexp.Compile(pat)
	if err != nil {
		c.diag.add(CodeInvalidRegex, lit.sp, "invalid pattern", "invalid regular expression: %v", err)
		return nil
	}
	return rx
}

func (c *checker) call(n *callExpr) (typ, evalFn) {
	arity := map[string]int{"len": 1, "contains": 2, "starts_with": 2, "ends_with": 2, "matches": 2, "in": 2}
	want, known := arity[n.name]
	if !known {
		c.diag.add(CodeUnknownFunction, n.nameSp, "unknown function", "unknown function `%s`", n.name).
			Help = "functions: contains, starts_with, ends_with, matches, len, in"
		return tInvalidT, nil
	}
	if len(n.args) != want {
		c.diag.add(CodeArity, n.sp, "wrong number of arguments", "`%s` takes %d argument(s), found %d", n.name, want, len(n.args))
		return tInvalidT, nil
	}
	switch n.name {
	case "in":
		return c.in(n.args[0], n.args[1], n.sp, n.nameSp)
	case "matches":
		return c.matches(n.args[0], n.args[1], n.sp)
	case "len":
		t, x := c.check(n.args[0])
		switch t.k {
		case kInvalid:
			return tInvalidT, nil
		case kString, kList, kObject:
			return tIntT, func(ctx *evalCtx) value { return lenOf(x(ctx)) }
		}
		c.diag.add(CodeArgumentType, n.args[0].exprSpan(), "has type "+t.String(), "`len` needs a String, List or Object, found %s", t)
		return tInvalidT, nil
	}
	lt, l := c.check(n.args[0])
	rt, r := c.check(n.args[1])
	if lt.k == kInvalid || rt.k == kInvalid {
		return tInvalidT, nil
	}
	t, fn := binaryOp(n.name, lt, rt, l, r)
	if fn == nil {
		c.diag.add(CodeArgumentType, n.sp, "arguments are "+lt.String()+" and "+rt.String(), "`%s` cannot be applied to %s and %s", n.name, lt, rt)
		return tInvalidT, nil
	}
	return t, fn
}

func (c *checker) list(n *listExpr) (typ, evalFn) {
	elem := kAny
	fns := make([]evalFn, 0, len(n.elems))
	kinds := make([]kind, 0, len(n.elems))
	bad := false
	for _, e := range n.elems {
		t, fn := c.check(e)
		if t.k == kInvalid {
			bad = true
			continue
		}
		if !t.isScalar() {
			c.diag.add(CodeOperandType, e.exprSpan(), "has type "+t.String(), "list elements must be scalars, found %s", t)
			bad = true
			continue
		}
		switch {
		case elem == kAny || elem == t.k:
			elem = t.k
		case (elem == kInt || elem == kFloat) && t.isNumeric():
			elem = kFloat
		default:
			c.diag.add(CodeOperandType, e.exprSpan(), "has type "+t.String(), "list elements must share one type: found %s after %s", t, elem)
			bad = true
			continue
		}
		fns = append(fns, fn)
		kinds = append(kinds, t.k)
	}
	if bad {
		return tInvalidT, nil
	}
	promote := elem == kFloat
	t := typ{k: kList, elem: elem}
	if allLiterals(n.elems) {
		v := value{k: kList, list: make([]value, 0, len(n.elems))}
		for i, e := range n.elems {
			lv := e.(*litExpr).val
			if promote && kinds[i] == kInt {
				lv = floatVal(float64(lv.i))
			}
			v.list = append(v.list, lv)
		}
		return t, func(*evalCtx) value { return v }
	}
	return t, func(ctx *evalCtx) value {
		out := make([]value, 0, len(fns))
		for i, fn := range fns {
			v := fn(ctx)
			if v.k == kNull {
				continue
			}
			if promote && kinds[i] == kInt {
				v = floatVal(float64(v.i))
			}
			out = append(out, v)
		}
		return value{k: kList, list: out}
	}
}

func allLiterals(es []expr) bool {
	for _, e := range es {
		if _, ok := e.(*litExpr); !ok {
			return false
		}
	}
	return true
}
