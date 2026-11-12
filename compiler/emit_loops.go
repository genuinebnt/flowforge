package compiler

import (
	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/dsl"
)

// Loops follow bytecode/doc.go: the loop-carried state (the index and
// the accumulators) travels on the operand stack across the back edge and
// is stored into fresh locals at the loop head, so no local is written
// twice and LOOP carries no local definitions. The list and its length
// do not change, so they are locals written once before the loop and only
// read inside it:
//
//	<xs> STORE xs; LOAD xs; LIST_LEN; STORE n
//	<acc...> CONST 0
//	head: STORE i; STORE acc...
//	      LOAD i; LOAD n; LT; JUMP_IF_FALSE exit
//	      LOAD xs; LOAD i; LIST_GET; STORE x
//	      ... body, ending with the next accumulators on the stack ...
//	      LOAD i; CONST 1; ADD_I; LOOP head
//	exit: (the accumulator locals hold the final values)

// loop is an open loop.
type loop struct {
	head, exit bytecode.Label
	xs, n, i   uint16
	acc        []uint16
}

// loopLen stores the length of the list in local xs; call it before the
// accumulators are pushed.
func (x *fnCtx) loopLen(xs uint16) uint16 {
	x.fb.Load(xs)
	x.fb.Op(bytecode.OpListLen)
	return x.storeTemp()
}

// loopBegin starts a loop over the list in local xs whose length is in
// local n; the stack holds nacc initial accumulators.
func (x *fnCtx) loopBegin(xs, n uint16, nacc int) *loop {
	lp := &loop{head: x.fb.NewLabel(), exit: x.fb.NewLabel(), xs: xs, n: n}
	x.fb.Int(0)
	x.fb.Bind(lp.head)
	lp.i = x.storeTemp()
	lp.acc = make([]uint16, nacc)
	for k := nacc - 1; k >= 0; k-- {
		lp.acc[k] = x.storeTemp()
	}
	x.fb.Load(lp.i)
	x.fb.Load(lp.n)
	x.fb.Op(bytecode.OpLt)
	x.fb.JumpIfFalse(lp.exit)
	return lp
}

// loopElem stores the current element in a fresh local and returns it.
func (x *fnCtx) loopElem(lp *loop) uint16 {
	x.fb.Load(lp.xs)
	x.fb.Load(lp.i)
	x.fb.Op(bytecode.OpListGet)
	return x.storeTemp()
}

// loopNext closes the loop; the stack holds the next accumulators.
func (x *fnCtx) loopNext(lp *loop) {
	x.fb.Load(lp.i)
	x.fb.Int(1)
	x.fb.Op(bytecode.OpAddI)
	x.fb.Loop(lp.head)
	x.fb.Bind(lp.exit)
}

// callable is a function argument of a higher-order library function.
// Lambda literals are inlined (their parameters alias the argument
// locals); fns, tools, variants and library functions are called
// directly; any other function value is in a local and called with
// CALL_CLOSURE.
type callable struct {
	lambda  *dsl.Lambda
	ref     dsl.Ref
	isRef   bool
	closure uint16
	node    dsl.Node
}

// call emits a call of f with the arguments in the given locals and
// leaves the result on the stack.
func (x *fnCtx) callWith(f callable, args ...uint16) {
	switch {
	case f.lambda != nil:
		if len(f.lambda.Params) != len(args) {
			x.c.errorf(f.lambda, "lambda takes %d arguments, called with %d", len(f.lambda.Params), len(args))
			x.fb.Op(bytecode.OpUnit)
			return
		}
		for i, p := range f.lambda.Params {
			x.alias(p.Binding, args[i], f.lambda)
		}
		x.expr(f.lambda.Body)
	case f.isRef:
		for _, a := range args {
			x.fb.Load(a)
		}
		x.callRef(f.ref, len(args), f.node)
	default:
		x.fb.Load(f.closure)
		for _, a := range args {
			x.fb.Load(a)
		}
		x.span(f.node)
		x.fb.CallClosure(uint8(len(args)))
	}
}

// callRef calls an item with its n arguments on the stack.
func (x *fnCtx) callRef(r dsl.Ref, n int, node dsl.Node) {
	x.span(node)
	switch r.Kind {
	case dsl.RefFn:
		fn, ok := x.c.fnFunc(r.Fn)
		if !ok {
			x.popN(n)
			x.fb.Op(bytecode.OpUnit)
			return
		}
		x.fb.Call(fn, uint8(n))
	case dsl.RefTool:
		x.toolCall(r.Tool, n, node)
	case dsl.RefBuiltin:
		x.builtinOnStack(r.Builtin, n, node)
	case dsl.RefVariant:
		x.makeVariant(r.Variant, node)
	default:
		x.c.errorf(node, "cannot call reference of kind %d", r.Kind)
		x.popN(n)
		x.fb.Op(bytecode.OpUnit)
	}
}

func (x *fnCtx) popN(n int) {
	for range n {
		x.fb.Op(bytecode.OpPop)
	}
}

// mapLoop pushes map(xs, f) for the list in local xs.
func (x *fnCtx) mapLoop(xs uint16, f callable) {
	n := x.loopLen(xs)
	x.fb.Op(bytecode.OpNewList)
	lp := x.loopBegin(xs, n, 1)
	e := x.loopElem(lp)
	x.fb.Load(lp.acc[0])
	x.callWith(f, e)
	x.fb.Op(bytecode.OpListPush)
	x.loopNext(lp)
	x.fb.Load(lp.acc[0])
}

// filterLoop pushes filter(xs, f).
func (x *fnCtx) filterLoop(xs uint16, f callable) {
	n := x.loopLen(xs)
	x.fb.Op(bytecode.OpNewList)
	lp := x.loopBegin(xs, n, 1)
	e := x.loopElem(lp)
	skip, cont := x.fb.NewLabel(), x.fb.NewLabel()
	x.callWith(f, e)
	x.fb.JumpIfFalse(skip)
	x.fb.Load(lp.acc[0])
	x.fb.Load(e)
	x.fb.Op(bytecode.OpListPush)
	x.fb.Jump(cont)
	x.fb.Bind(skip)
	x.fb.Load(lp.acc[0])
	x.fb.Bind(cont)
	x.loopNext(lp)
	x.fb.Load(lp.acc[0])
}

// filterMapLoop pushes filter_map(xs, f): the payloads of the Some results.
func (x *fnCtx) filterMapLoop(xs uint16, f callable) {
	n := x.loopLen(xs)
	x.fb.Op(bytecode.OpNewList)
	lp := x.loopBegin(xs, n, 1)
	e := x.loopElem(lp)
	none, cont := x.fb.NewLabel(), x.fb.NewLabel()
	x.callWith(f, e)
	x.fb.Emit(bytecode.OpTestVariant, int64(bytecode.VariantSome))
	x.fb.JumpIfFalse(none)
	x.fb.Emit(bytecode.OpUnpack, 1)
	p := x.storeTemp()
	x.fb.Load(lp.acc[0])
	x.fb.Load(p)
	x.fb.Op(bytecode.OpListPush)
	x.fb.Jump(cont)
	x.fb.Bind(none)
	x.fb.Op(bytecode.OpPop)
	x.fb.Load(lp.acc[0])
	x.fb.Bind(cont)
	x.loopNext(lp)
	x.fb.Load(lp.acc[0])
}

// foldLoop pushes fold(xs, init, f).
func (x *fnCtx) foldLoop(xs, init uint16, f callable) {
	n := x.loopLen(xs)
	x.fb.Load(init)
	lp := x.loopBegin(xs, n, 1)
	e := x.loopElem(lp)
	x.callWith(f, lp.acc[0], e)
	x.loopNext(lp)
	x.fb.Load(lp.acc[0])
}

// searchLoop pushes any(xs, f) (wantTrue), all(xs, f) or first(xs, f).
// It leaves the loop early through found.
func (x *fnCtx) searchLoop(xs uint16, f callable, kind string) {
	lp := x.loopBegin(xs, x.loopLen(xs), 0)
	e := x.loopElem(lp)
	found, end := x.fb.NewLabel(), x.fb.NewLabel()
	x.callWith(f, e)
	if kind == "all" {
		x.fb.JumpIfFalse(found)
	} else {
		next := x.fb.NewLabel()
		x.fb.JumpIfFalse(next)
		x.fb.Jump(found)
		x.fb.Bind(next)
	}
	x.loopNext(lp)
	switch kind {
	case "any":
		x.fb.Op(bytecode.OpFalse)
		x.fb.Jump(end)
		x.fb.Bind(found)
		x.fb.Op(bytecode.OpTrue)
	case "all":
		x.fb.Op(bytecode.OpTrue)
		x.fb.Jump(end)
		x.fb.Bind(found)
		x.fb.Op(bytecode.OpFalse)
	default: // first
		x.optionNone()
		x.fb.Jump(end)
		x.fb.Bind(found)
		x.fb.Load(e)
		x.optionSome()
	}
	x.fb.Bind(end)
}

// countLoop pushes count(xs, f).
func (x *fnCtx) countLoop(xs uint16, f callable) {
	n := x.loopLen(xs)
	x.fb.Int(0)
	lp := x.loopBegin(xs, n, 1)
	e := x.loopElem(lp)
	skip, cont := x.fb.NewLabel(), x.fb.NewLabel()
	x.callWith(f, e)
	x.fb.JumpIfFalse(skip)
	x.fb.Load(lp.acc[0])
	x.fb.Int(1)
	x.fb.Op(bytecode.OpAddI)
	x.fb.Jump(cont)
	x.fb.Bind(skip)
	x.fb.Load(lp.acc[0])
	x.fb.Bind(cont)
	x.loopNext(lp)
	x.fb.Load(lp.acc[0])
}
