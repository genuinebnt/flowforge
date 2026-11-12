package compiler

import (
	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler/internal/abi"
	"github.com/genuinebnt/flowforge/dsl"
)

// patMatch collects the failure exits of one pattern (one match arm).
// Every value a pattern tests lives in a local, so failures happen at one
// of two stack depths: right after TEST_VARIANT the enum is still on the
// stack (exit pop), everywhere else nothing is (exit fail). The failure
// block is "pop: POP; fail:", so a failed guard jumps past the POP, as
// bytecode/doc.go requires.
type patMatch struct {
	x        *fnCtx
	pop      bytecode.Label
	fail     bytecode.Label
	popUsed  bool
	failUsed bool
}

func newPatMatch(x *fnCtx) *patMatch {
	return &patMatch{x: x, pop: x.fb.NewLabel(), fail: x.fb.NewLabel()}
}

func (m *patMatch) used() bool { return m.popUsed || m.failUsed }

// failIfFalse pops a Bool and leaves the arm when it is false.
func (m *patMatch) failIfFalse() {
	m.failUsed = true
	m.x.fb.JumpIfFalse(m.fail)
}

// popIfFalse pops the Bool of TEST_VARIANT and leaves the arm (popping
// the enum) when it is false.
func (m *patMatch) popIfFalse() {
	m.popUsed = true
	m.x.fb.JumpIfFalse(m.pop)
}

// bindFailures places the failure block; code emitted next runs when the
// pattern did not match.
func (m *patMatch) bindFailures() {
	m.x.fb.Bind(m.pop)
	if m.popUsed {
		m.x.fb.Op(bytecode.OpPop)
	}
	m.x.fb.Bind(m.fail)
}

// match lowers a match expression (spec 8.5): the scrutinee goes into a
// temp local; each arm tests its pattern, binds, checks the guard,
// evaluates the body and jumps to the end; the final fall-through fails
// with match_failed (unreachable for checked programs).
func (x *fnCtx) match(e *dsl.MatchExpr) {
	x.expr(e.Scrutinee)
	v := x.storeTemp()
	end := x.fb.NewLabel()
	for _, arm := range e.Arms {
		m := newPatMatch(x)
		x.span(arm.Pat)
		x.pattern(arm.Pat, v, m)
		if arm.Guard != nil {
			x.expr(arm.Guard)
			m.failIfFalse()
		}
		x.expr(arm.Body)
		x.fb.Jump(end)
		m.bindFailures()
	}
	x.span(e)
	x.fb.Fail(bytecode.FailMatchFailed)
	x.fb.Bind(end)
}

// pattern tests p against the value in local v, binding its variables;
// it leaves the stack unchanged and exits through m on failure.
func (x *fnCtx) pattern(p dsl.Pattern, v uint16, m *patMatch) {
	switch p := p.(type) {
	case *dsl.WildcardPat:
	case *dsl.BindPat:
		if p.Variant != nil {
			x.fb.Load(v)
			x.testVariant(p.Variant, m)
			x.fb.Op(bytecode.OpPop)
			return
		}
		x.alias(p.Binding, v, p)
	case *dsl.LitPat:
		x.fb.Load(v)
		x.expr(p.Lit)
		x.fb.Op(bytecode.OpEq)
		m.failIfFalse()
	case *dsl.VariantPat:
		if p.Variant == nil {
			x.c.errorf(p, "unresolved variant pattern")
			return
		}
		x.fb.Load(v)
		x.testVariant(p.Variant, m)
		n := len(p.Args)
		if n == 0 {
			x.fb.Op(bytecode.OpPop)
			return
		}
		if n != len(p.Variant.Payload) || n > 255 {
			x.c.errorf(p, "variant pattern arity")
			x.fb.Op(bytecode.OpPop)
			return
		}
		x.fb.Emit(bytecode.OpUnpack, int64(n))
		slots := make([]uint16, n)
		nested := make([]bool, n)
		for j := n - 1; j >= 0; j-- {
			slots[j], nested[j] = x.bindSub(p.Args[j])
		}
		for j := 0; j < n; j++ {
			if nested[j] {
				x.pattern(p.Args[j], slots[j], m)
			}
		}
	case *dsl.TuplePat:
		for i, el := range p.Elems {
			if _, wild := el.(*dsl.WildcardPat); wild {
				continue
			}
			x.fb.Load(v)
			x.fb.Emit(bytecode.OpGetField, int64(i))
			if slot, nested := x.bindSub(el); nested {
				x.pattern(el, slot, m)
			}
		}
	case *dsl.RecordPat:
		for _, f := range p.Fields {
			if _, wild := f.Pat.(*dsl.WildcardPat); wild {
				continue
			}
			if f.Index < 0 || f.Index > 255 {
				x.c.errorf(p, "bad record pattern field index")
				continue
			}
			x.fb.Load(v)
			x.fb.Emit(bytecode.OpGetField, int64(f.Index))
			if f.Pat == nil {
				x.storeBinding(f.ShortBinding, p)
				continue
			}
			if slot, nested := x.bindSub(f.Pat); nested {
				x.pattern(f.Pat, slot, m)
			}
		}
	default:
		x.c.errorf(p, "cannot compile pattern %T", p)
	}
}

// bindSub pops a sub-value for pattern p: a variable binds it directly, a
// wildcard drops it, anything else stores it in a temp for further tests
// (nested reports that case).
func (x *fnCtx) bindSub(p dsl.Pattern) (slot uint16, nested bool) {
	switch p := p.(type) {
	case *dsl.WildcardPat:
		x.fb.Op(bytecode.OpPop)
		return 0, false
	case *dsl.BindPat:
		if p.Binding != nil {
			x.storeBinding(p.Binding, p)
			return 0, false
		}
	}
	return x.storeTemp(), true
}

// testVariant expects the enum on the stack and leaves it there when it
// is variant v.
func (x *fnCtx) testVariant(v *dsl.Variant, m *patMatch) {
	x.fb.Emit(bytecode.OpTestVariant, int64(abi.VariantIndex(v)))
	m.popIfFalse()
}
