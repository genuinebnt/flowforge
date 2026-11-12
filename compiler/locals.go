package compiler

import (
	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/dsl"
)

// fnCtx is the emission context of one bytecode function.
//
// Local allocation: every dsl.Binding the function owns gets a fresh local
// slot when it is first stored (parameters are slots 0..arity-1), and
// every temporary (scrutinees, loop state, reordered arguments) gets its
// own fresh slot too. Slots are never reused, so each local is written at
// most once on every path, as the verifier requires. Because a slot is
// allocated where its single STORE is emitted, a value first written
// inside a loop body is fresh in every iteration and is never written
// before the loop.
//
// Captured bindings are read with LOAD_CAP. Lambdas inlined into a loop
// share the enclosing function's context: their parameters become locals
// and their captures resolve to whatever the enclosing function holds the
// binding in.
type fnCtx struct {
	c      *compiler
	fb     *bytecode.FuncBuilder
	locals map[*dsl.Binding]uint16
	caps   map[*dsl.Binding]uint8
	// entry is set for the workflow body: TRY there fails the run.
	entry bool
	// annot is the annotation of the let statement being emitted.
	annot *letAnnot
	// idempotent is set inside an #[idempotent] fn or workflow.
	idempotent bool
}

func newFnCtx(c *compiler, fb *bytecode.FuncBuilder, entry bool) *fnCtx {
	return &fnCtx{c: c, fb: fb, entry: entry, locals: map[*dsl.Binding]uint16{}, caps: map[*dsl.Binding]uint8{}}
}

// temp allocates a fresh local.
func (x *fnCtx) temp() uint16 {
	if x.fb.NumLocals() >= 65535 {
		x.c.limitf(x.c.wf.Name, "a function needs more than 65,535 locals")
		return 0
	}
	return x.fb.Local()
}

// storeTemp pops the top of the stack into a fresh local.
func (x *fnCtx) storeTemp() uint16 {
	t := x.temp()
	x.fb.Store(t)
	return t
}

// storeBinding pops the top of the stack into a fresh local for b.
func (x *fnCtx) storeBinding(b *dsl.Binding, n dsl.Node) {
	if b == nil {
		x.c.errorf(n, "pattern without a binding")
		x.fb.Op(bytecode.OpPop)
		return
	}
	if _, dup := x.locals[b]; dup {
		x.c.errorf(n, "binding %s stored twice", b.Name)
	}
	x.locals[b] = x.storeTemp()
}

// alias makes b denote the existing local slot (no code).
func (x *fnCtx) alias(b *dsl.Binding, slot uint16, n dsl.Node) {
	if b == nil {
		x.c.errorf(n, "pattern without a binding")
		return
	}
	if _, dup := x.locals[b]; dup {
		x.c.errorf(n, "binding %s bound twice", b.Name)
	}
	x.locals[b] = slot
}

// loadBinding pushes the value of b.
func (x *fnCtx) loadBinding(b *dsl.Binding, n dsl.Node) {
	if b != nil {
		if l, ok := x.locals[b]; ok {
			x.fb.Load(l)
			return
		}
		if c, ok := x.caps[b]; ok {
			x.fb.Cap(c)
			return
		}
		x.c.errorf(n, "binding %s is not available in this function", b.Name)
	} else {
		x.c.errorf(n, "unresolved binding")
	}
	x.fb.Op(bytecode.OpUnit)
}

// span attaches the source range of n to the next instruction.
func (x *fnCtx) span(n dsl.Node) {
	if n == nil {
		return
	}
	sp := n.Span()
	start, end := sp.Start.Offset, sp.End.Offset
	if start < 0 || end < start {
		return
	}
	x.fb.Span(uint32(start), uint32(end-start))
}
