package compiler

import (
	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler/internal/abi"
	"github.com/genuinebnt/flowforge/dsl"
)

// call dispatches on the checked call kind.
func (x *fnCtx) call(e *dsl.CallExpr) {
	switch e.Kind {
	case dsl.CallBuiltin:
		x.builtinCall(e)
	case dsl.CallTool:
		n := len(e.Tool.Params)
		x.pushArgs(e.Args, n, nil)
		x.toolCall(e.Tool, n, e)
	case dsl.CallFn:
		fn, ok := x.c.fnFunc(e.FnDecl)
		if !ok {
			x.fb.Op(bytecode.OpUnit)
			return
		}
		n := len(e.FnDecl.Params)
		x.pushArgs(e.Args, n, nil)
		x.span(e)
		x.fb.Call(fn, uint8(n))
	case dsl.CallVariant:
		x.pushArgs(e.Args, len(e.Variant.Payload), nil)
		x.makeVariant(e.Variant, e)
	case dsl.CallClosure:
		x.expr(e.Fn)
		if len(e.Args) > 255 {
			x.c.limitf(e, "a call passes more than 255 arguments")
			return
		}
		for _, a := range e.Args {
			x.expr(a.Value)
		}
		x.span(e)
		x.fb.CallClosure(uint8(len(e.Args)))
	default:
		x.c.errorf(e, "unresolved call")
		x.fb.Op(bytecode.OpUnit)
	}
}

// pushArgs pushes the arguments of a call in parameter order. Arguments
// are evaluated in source order (named arguments may be reordered); when
// that differs from parameter order they go through temps. Missing
// parameters are pushed by def (an error when def is nil).
func (x *fnCtx) pushArgs(args []*dsl.Arg, n int, def func(i int)) {
	if n > 255 {
		x.c.limitf(nil, "a call passes more than 255 arguments")
		return
	}
	inOrder := true
	for i, a := range args {
		if a.Param < 0 || a.Param >= n || (i > 0 && a.Param <= args[i-1].Param) {
			inOrder = false
		}
	}
	missing := func(i int) {
		if def == nil {
			x.c.errorf(nil, "argument %d missing", i)
			x.fb.Op(bytecode.OpUnit)
			return
		}
		def(i)
	}
	if inOrder {
		next := 0
		for _, a := range args {
			for ; next < a.Param; next++ {
				missing(next)
			}
			x.expr(a.Value)
			next++
		}
		for ; next < n; next++ {
			missing(next)
		}
		return
	}
	slots := x.argTemps(args, n)
	for i, s := range slots {
		if s < 0 {
			missing(i)
			continue
		}
		x.fb.Load(uint16(s))
	}
}

// argTemps evaluates args in source order into temps and returns the temp
// of each parameter (-1 when absent).
func (x *fnCtx) argTemps(args []*dsl.Arg, n int) []int {
	slots := make([]int, n)
	for i := range slots {
		slots[i] = -1
	}
	for _, a := range args {
		x.expr(a.Value)
		if a.Param < 0 || a.Param >= n || slots[a.Param] >= 0 {
			x.c.errorf(a.Value, "argument bound to no parameter")
			x.fb.Op(bytecode.OpPop)
			continue
		}
		slots[a.Param] = int(x.storeTemp())
	}
	return slots
}

// toolCall emits the tool_call sequence for n inputs on the stack:
// CONST tool_name; EFFECT tool_call n+1 (bytecode/doc.go).
func (x *fnCtx) toolCall(t *dsl.Tool, n int, node dsl.Node) {
	if n > 254 {
		x.c.limitf(node, "tool `%s` takes too many inputs", t.Name)
		return
	}
	name := x.c.useTool(t)
	x.fb.String(name)
	x.effect(bytecode.EffectToolCall, n+1, t, node)
}

// approvalArgs pushes role, reason and timeout (Duration 0, the engine
// default, when omitted).
func (x *fnCtx) approvalArgs(args []*dsl.Arg) {
	x.pushArgs(args, 3, func(int) { x.fb.Duration(abi.ApprovalDefaultTimeout) })
}

// builtinOnStack emits a library function whose n arguments (all of them,
// in parameter order) are already on the stack. It covers the functions
// that can be used as values: pure leaf functions and effects.
func (x *fnCtx) builtinOnStack(b *dsl.Builtin, n int, node dsl.Node) {
	x.span(node)
	if id, ok := abi.LeafBuiltin(b.Name); ok {
		x.fb.Builtin(id)
		return
	}
	if kind, ok := abi.EffectKind(b); ok && kind != bytecode.EffectParMap {
		x.effect(kind, n, nil, node)
		return
	}
	x.c.errorf(node, "library function %s cannot be called through a value", b.Name)
	x.popN(n)
	x.fb.Op(bytecode.OpUnit)
}

// builtinCall lowers a call of a standard-library function (spec 7.6,
// 8.5): higher-order functions become loops, the rest BUILTIN or EFFECT.
func (x *fnCtx) builtinCall(e *dsl.CallExpr) {
	b := e.Builtin
	if b == nil || len(b.Overloads) == 0 {
		x.c.errorf(e, "unresolved library call")
		x.fb.Op(bytecode.OpUnit)
		return
	}
	nparams := len(b.Overloads[0].Params)
	switch b.Name {
	case "map", "filter", "filter_map", "flat_map", "fold", "any", "all", "first", "count", "sort_by", "group_by":
		x.higherOrder(e)
		return
	case "par_map":
		x.parMap(e)
		return
	case "approve":
		x.approvalArgs(e.Args)
		x.effect(bytecode.EffectApproval, 3, nil, e)
		return
	}
	x.pushArgs(e.Args, nparams, nil)
	x.builtinOnStack(b, nparams, e)
}

// hofArgs evaluates the arguments of a higher-order call in source order.
// Function arguments that are lambda literals or item names need no code;
// every other argument is stored in a temp.
type hofArgs struct {
	vals  []int
	funcs []*callable
}

func (x *fnCtx) evalHOFArgs(e *dsl.CallExpr, n, fnAt int) *hofArgs {
	h := &hofArgs{vals: make([]int, n), funcs: make([]*callable, n)}
	for i := range h.vals {
		h.vals[i] = -1
	}
	for _, a := range e.Args {
		if a.Param < 0 || a.Param >= n {
			x.c.errorf(a.Value, "argument bound to no parameter")
			continue
		}
		if a.Param == fnAt {
			if f := staticCallable(a.Value); f != nil {
				h.funcs[a.Param] = f
				continue
			}
			x.expr(a.Value)
			h.funcs[a.Param] = &callable{closure: x.storeTemp(), node: a.Value}
			continue
		}
		x.expr(a.Value)
		h.vals[a.Param] = int(x.storeTemp())
	}
	return h
}

// staticCallable returns the callable of a lambda literal or an item name.
func staticCallable(e dsl.Expr) *callable {
	switch v := e.(type) {
	case *dsl.Lambda:
		if !v.Synthetic {
			return &callable{lambda: v, node: v}
		}
	case *dsl.Ident:
		if isItemRef(v.Ref) {
			return &callable{ref: v.Ref, isRef: true, node: v}
		}
	case *dsl.PathExpr:
		if isItemRef(v.Ref) {
			return &callable{ref: v.Ref, isRef: true, node: v}
		}
	}
	return nil
}

func isItemRef(r dsl.Ref) bool {
	switch r.Kind {
	case dsl.RefFn, dsl.RefTool, dsl.RefBuiltin:
		return true
	case dsl.RefVariant:
		return r.Variant != nil && len(r.Variant.Payload) > 0
	}
	return false
}

// higherOrder lowers map, filter, ... to loops (bytecode/doc.go).
func (x *fnCtx) higherOrder(e *dsl.CallExpr) {
	b := e.Builtin
	n := len(b.Overloads[0].Params)
	fnAt := 1
	if b.Name == "fold" {
		fnAt = 2
	}
	h := x.evalHOFArgs(e, n, fnAt)
	xs := h.vals[0]
	if xs < 0 {
		x.c.errorf(e, "%s without a list", b.Name)
		x.fb.Op(bytecode.OpUnit)
		return
	}
	f := h.funcs[fnAt]
	if f == nil {
		// first(xs) and count(xs) without a predicate.
		x.fb.Load(uint16(xs))
		switch b.Name {
		case "first":
			x.fb.Int(0)
			x.span(e)
			x.fb.Builtin(bytecode.BListGetOpt)
		case "count":
			x.span(e)
			x.fb.Builtin(bytecode.BLen)
		default:
			x.c.errorf(e, "%s without a function", b.Name)
		}
		return
	}
	l := uint16(xs)
	switch b.Name {
	case "map":
		x.mapLoop(l, *f)
	case "filter":
		x.filterLoop(l, *f)
	case "filter_map":
		x.filterMapLoop(l, *f)
	case "flat_map":
		x.mapLoop(l, *f)
		x.span(e)
		x.fb.Builtin(bytecode.BFlatten)
	case "fold":
		if h.vals[1] < 0 {
			x.c.errorf(e, "fold without an initial value")
			x.fb.Op(bytecode.OpUnit)
			return
		}
		x.foldLoop(l, uint16(h.vals[1]), *f)
	case "any", "all", "first":
		x.searchLoop(l, *f, b.Name)
	case "count":
		x.countLoop(l, *f)
	case "sort_by", "group_by":
		x.mapLoop(l, *f)
		x.fb.Load(l)
		x.span(e)
		if b.Name == "sort_by" {
			x.fb.Builtin(bytecode.BSortByKeys)
		} else {
			x.fb.Builtin(bytecode.BGroupByKeys)
		}
	}
}

// parMap lowers par_map to one EFFECT par_map with (list, closure, limit,
// on_error) (spec 8.5).
func (x *fnCtx) parMap(e *dsl.CallExpr) {
	slots := [4]int{-1, -1, -1, -1}
	var fn dsl.Expr
	mode := int64(bytecode.ParMapFail)
	for _, a := range e.Args {
		switch a.Param {
		case 1:
			if staticCallable(a.Value) == nil {
				x.expr(a.Value)
				slots[1] = int(x.storeTemp())
			}
			fn = a.Value
		case 3:
			if s, ok := a.Value.(*dsl.StringLit); ok && !s.Interpolated() {
				text := ""
				for _, p := range s.Parts {
					text += p.Lit
				}
				mode = abi.ParMapMode(text)
				continue
			}
			x.c.errorf(a.Value, "par_map on_error is not a literal")
		case 0, 2:
			x.expr(a.Value)
			slots[a.Param] = int(x.storeTemp())
		default:
			x.c.errorf(a.Value, "argument bound to no parameter")
		}
	}
	if slots[0] < 0 || slots[2] < 0 || fn == nil {
		x.c.errorf(e, "par_map without list, function or limit")
		x.fb.Op(bytecode.OpUnit)
		return
	}
	x.fb.Load(uint16(slots[0]))
	switch {
	case slots[1] >= 0:
		x.fb.Load(uint16(slots[1]))
	default:
		x.funcValue(fn)
	}
	x.fb.Load(uint16(slots[2]))
	x.fb.Int(mode)
	x.effect(bytecode.EffectParMap, 4, nil, e)
}

// funcValue pushes a closure for a lambda literal or item name.
func (x *fnCtx) funcValue(e dsl.Expr) {
	switch v := e.(type) {
	case *dsl.Lambda:
		x.pushClosure(v)
	case *dsl.Ident:
		x.itemValue(v.Ref, v)
	case *dsl.PathExpr:
		x.itemValue(v.Ref, v)
	default:
		x.expr(e)
	}
}
