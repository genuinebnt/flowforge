package compiler

import (
	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/dsl"
)

// block emits the statements of b and pushes its value (the tail, or ()).
func (x *fnCtx) block(b *dsl.BlockExpr) {
	if b == nil {
		x.fb.Op(bytecode.OpUnit)
		return
	}
	for _, s := range b.Stmts {
		x.stmt(s)
	}
	if b.Tail != nil {
		x.expr(b.Tail)
	} else {
		x.fb.Op(bytecode.OpUnit)
	}
}

// stmt emits one statement; the stack depth is unchanged afterwards.
func (x *fnCtx) stmt(s dsl.Stmt) {
	x.span(s)
	switch s := s.(type) {
	case *dsl.LetStmt:
		x.let(s)
	case *dsl.ForStmt:
		x.forStmt(s)
	case *dsl.RequireStmt:
		x.require(s)
	case *dsl.ReturnStmt:
		if s.Value != nil {
			x.expr(s.Value)
		} else {
			x.fb.Op(bytecode.OpUnit)
		}
		x.span(s)
		x.fb.Op(bytecode.OpRet)
	case *dsl.ExprStmt:
		x.expr(s.X)
		x.fb.Op(bytecode.OpPop)
	default:
		x.c.errorf(s, "cannot compile statement %T", s)
	}
}

// let evaluates the value under the statement's annotation, binds the
// pattern and registers the compensation (spec 8.5): the compensation
// closure captures the bound value as `it`, then PUSH_COMP.
func (x *fnCtx) let(s *dsl.LetStmt) {
	saved := x.annot
	x.annot = letAnnotation(s, saved)
	x.expr(s.Value)
	x.annot = saved

	var it uint16
	switch p := s.Pat.(type) {
	case *dsl.BindPat:
		if p.Binding != nil {
			x.storeBinding(p.Binding, p)
			it = x.locals[p.Binding]
			break
		}
		it = x.storeTemp()
		x.irrefutable(s.Pat, it)
	case *dsl.WildcardPat:
		if s.Compensate == nil {
			x.fb.Op(bytecode.OpPop)
			return
		}
		it = x.storeTemp()
	default:
		it = x.storeTemp()
		x.irrefutable(s.Pat, it)
	}
	if s.Compensate != nil {
		x.compensate(s.Compensate, it)
	}
}

// compensate emits PUSH_COMP for the compensation lambda l with `it`
// bound to local it.
func (x *fnCtx) compensate(l *dsl.Lambda, it uint16) {
	fn, ok := x.c.lambdaFunc(x, l)
	if !ok {
		return
	}
	x.fb.Load(it)
	for _, cp := range l.Captures {
		x.loadBinding(cp.Binding, l)
	}
	x.span(l)
	x.fb.PushComp(fn, uint8(1+len(l.Captures)))
}

// irrefutable binds a let or for pattern from local v. The checker
// guarantees the pattern always matches; the unreachable fall-back fails
// with match_failed.
func (x *fnCtx) irrefutable(p dsl.Pattern, v uint16) {
	m := newPatMatch(x)
	x.pattern(p, v, m)
	if m.used() {
		end := x.fb.NewLabel()
		x.fb.Jump(end)
		m.bindFailures()
		x.span(p)
		x.fb.Fail(bytecode.FailMatchFailed)
		x.fb.Bind(end)
	}
}

// require lowers `require approval(...)`: EFFECT approval, then the
// failure of a denial with approval_denied (spec 8.5), and drops the
// Approval value.
func (x *fnCtx) require(s *dsl.RequireStmt) {
	x.approvalArgs(s.Args)
	x.effect(bytecode.EffectApproval, 3, nil, s)
	if x.entry {
		x.span(s)
		x.fb.Op(bytecode.OpTry)
	} else {
		x.unwrapOrElse(bytecode.VariantOk, s, func() { x.fb.Fail(bytecode.FailApprovalDenied) })
	}
	x.fb.Op(bytecode.OpPop)
}

// forStmt lowers `for pat in xs { body }` to a stack-carried loop over
// the list (maps iterate as (key, value) tuples in insertion order).
func (x *fnCtx) forStmt(s *dsl.ForStmt) {
	x.expr(s.Iter)
	if s.IterKind == dsl.IterMap {
		m := x.storeTemp()
		x.fb.Load(m)
		x.fb.Builtin(bytecode.BMapKeys)
		x.fb.Load(m)
		x.fb.Builtin(bytecode.BMapValues)
		x.fb.Builtin(bytecode.BZip)
	}
	xs := x.storeTemp()
	lp := x.loopBegin(xs, x.loopLen(xs), 0)
	elem := x.loopElem(lp)
	switch p := s.Pat.(type) {
	case *dsl.BindPat:
		if p.Binding != nil {
			x.alias(p.Binding, elem, p)
		} else {
			x.irrefutable(p, elem)
		}
	case *dsl.WildcardPat:
	default:
		x.irrefutable(p, elem)
	}
	x.block(s.Body)
	x.fb.Op(bytecode.OpPop)
	x.loopNext(lp)
}
