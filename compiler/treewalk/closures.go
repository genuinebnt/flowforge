package treewalk

import (
	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler/internal/abi"
	"github.com/genuinebnt/flowforge/dsl"
	"github.com/genuinebnt/flowforge/vm"
)

// closureInfo is what a closure value's function index stands for.
type closureInfo struct {
	kind    abi.ClosureKind
	lambda  *dsl.Lambda
	fn      *dsl.FnDecl
	tool    *dsl.Tool
	builtin *dsl.Builtin
	variant *dsl.Variant
}

// funcIndex returns the function index for a closure target: the
// compiled module's when known, else a private one.
func (p *Program) funcIndex(key any, info closureInfo) uint16 {
	if id, ok := p.itemID[key]; ok {
		return id
	}
	id, ok := uint16(0), false
	if ci := p.cfg.Closures; ci != nil {
		switch info.kind {
		case abi.ClosureLambda, abi.ClosureCompensation:
			id, ok = ci.LambdaFunc(info.lambda)
		case abi.ClosureFn:
			id, ok = ci.FnFunc(info.fn)
		case abi.ClosureTool:
			id, ok = ci.ToolFunc(info.tool)
		case abi.ClosureBuiltin:
			id, ok = ci.BuiltinFunc(info.builtin)
		case abi.ClosureVariant:
			id, ok = ci.VariantFunc(info.variant)
		}
	}
	if !ok {
		for {
			id = p.nextID
			p.nextID--
			if _, used := p.closures[id]; !used {
				break
			}
		}
	}
	p.itemID[key] = id
	p.closures[id] = info
	return id
}

// lambdaValue builds the closure of lambda l in env: its captures in
// Lambda.Captures order (a compensation also captures `it` first).
func (ev *Eval) lambdaValue(l *dsl.Lambda, e *env, it *vm.Value) (vm.Value, error) {
	kind := abi.ClosureLambda
	var caps []vm.Value
	if l.Synthetic {
		kind = abi.ClosureCompensation
		if it == nil {
			return vm.Value{}, bug(l, "compensation without a value")
		}
		caps = append(caps, *it)
	}
	for _, c := range l.Captures {
		v, err := e.get(c.Binding, l)
		if err != nil {
			return vm.Value{}, err
		}
		caps = append(caps, v)
	}
	id := ev.p.funcIndex(l, closureInfo{kind: kind, lambda: l})
	return vm.NewClosure(id, caps), nil
}

// itemValue builds the closure of an item used as a value.
func (ev *Eval) itemValue(r dsl.Ref, n dsl.Node) (vm.Value, error) {
	switch r.Kind {
	case dsl.RefFn:
		return vm.NewClosure(ev.p.funcIndex(r.Fn, closureInfo{kind: abi.ClosureFn, fn: r.Fn}), nil), nil
	case dsl.RefTool:
		return vm.NewClosure(ev.p.funcIndex(r.Tool, closureInfo{kind: abi.ClosureTool, tool: r.Tool}), nil), nil
	case dsl.RefBuiltin:
		return vm.NewClosure(ev.p.funcIndex(r.Builtin, closureInfo{kind: abi.ClosureBuiltin, builtin: r.Builtin}), nil), nil
	case dsl.RefVariant:
		if len(r.Variant.Payload) == 0 {
			return ev.makeVariant(r.Variant, nil, n)
		}
		return vm.NewClosure(ev.p.funcIndex(r.Variant, closureInfo{kind: abi.ClosureVariant, variant: r.Variant}), nil), nil
	}
	return vm.Value{}, bug(n, "reference is not a value")
}

// enterCall accounts for one VM call frame (CALL or CALL_CLOSURE).
func (ev *Eval) enterCall(n dsl.Node) error {
	if ev.depth >= ev.p.vmcfg.Limits.CallDepth {
		return fail(bytecode.FailLimitExceeded, "call depth limit exceeded", n)
	}
	ev.depth++
	return nil
}

// callValue calls closure c with args (CALL_CLOSURE).
func (ev *Eval) callValue(c vm.Value, args []vm.Value, n dsl.Node) (vm.Value, error) {
	if c.Kind() != vm.KindClosure {
		return vm.Value{}, bug(n, "calling a %s", c.Kind())
	}
	info, ok := ev.p.closures[c.FuncIndex()]
	if !ok {
		return vm.Value{}, bug(n, "unknown closure %d", c.FuncIndex())
	}
	if err := ev.enterCall(n); err != nil {
		return vm.Value{}, err
	}
	defer func() { ev.depth-- }()
	caps := c.Fields()
	switch info.kind {
	case abi.ClosureLambda:
		l := info.lambda
		if len(args) != len(l.Params) || len(caps) != len(l.Captures) {
			return vm.Value{}, bug(n, "closure arity")
		}
		e := newEnv()
		for i, cp := range l.Captures {
			e.set(cp.Binding, caps[i])
		}
		for i, p := range l.Params {
			e.set(p.Binding, args[i])
		}
		return ev.expr(l.Body, e)
	case abi.ClosureCompensation:
		l := info.lambda
		if len(caps) != len(l.Captures)+1 || len(l.Params) != 1 {
			return vm.Value{}, bug(n, "compensation captures")
		}
		e := newEnv()
		e.set(l.Params[0].Binding, caps[0])
		for i, cp := range l.Captures {
			e.set(cp.Binding, caps[i+1])
		}
		return ev.expr(l.Body, e)
	case abi.ClosureFn:
		return ev.fnBody(info.fn, args, n)
	case abi.ClosureTool:
		return ev.toolCall(info.tool, args, n)
	case abi.ClosureBuiltin:
		return ev.builtinValues(info.builtin, args, n)
	case abi.ClosureVariant:
		return ev.makeVariant(info.variant, args, n)
	}
	return vm.Value{}, bug(n, "closure kind")
}

// fnBody evaluates a user fn's body with its parameters bound; the caller
// has accounted for the call frame.
func (ev *Eval) fnBody(fn *dsl.FnDecl, args []vm.Value, n dsl.Node) (vm.Value, error) {
	if len(args) != len(fn.Params) {
		return vm.Value{}, bug(n, "fn arity")
	}
	e := newEnv()
	for i, p := range fn.Params {
		e.set(p.Binding, args[i])
	}
	v, err := ev.block(fn.Body, e)
	if r, ok := err.(*returnSignal); ok {
		return r.v, nil
	}
	return v, err
}

// callFn calls a user fn directly (CALL).
func (ev *Eval) callFn(fn *dsl.FnDecl, args []vm.Value, n dsl.Node) (vm.Value, error) {
	if err := ev.enterCall(n); err != nil {
		return vm.Value{}, err
	}
	defer func() { ev.depth-- }()
	return ev.fnBody(fn, args, n)
}

// toolCall performs EFFECT tool_call; the VM's Args are [name, inputs...].
func (ev *Eval) toolCall(t *dsl.Tool, inputs []vm.Value, n dsl.Node) (vm.Value, error) {
	args := make([]vm.Value, 0, len(inputs)+1)
	args = append(args, vm.Str(t.Name))
	args = append(args, inputs...)
	return ev.effect(bytecode.EffectToolCall, args, n)
}

// makeVariant builds an enum value.
func (ev *Eval) makeVariant(v *dsl.Variant, payload []vm.Value, n dsl.Node) (vm.Value, error) {
	id, ok := ev.p.types.ID(v.Def)
	if !ok {
		return vm.Value{}, bug(n, "enum without an id")
	}
	return vm.Enum(id, abi.VariantIndex(v), payload...), nil
}
