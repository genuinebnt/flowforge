package treewalk

import (
	"errors"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler/internal/abi"
	"github.com/genuinebnt/flowforge/dsl"
	"github.com/genuinebnt/flowforge/vm"
)

// call evaluates a call by its checked kind.
func (ev *Eval) call(e *dsl.CallExpr, en *env) (vm.Value, error) {
	switch e.Kind {
	case dsl.CallBuiltin:
		return ev.builtinCall(e, en)
	case dsl.CallTool:
		args, err := ev.args(e.Args, len(e.Tool.Params), en, nil)
		if err != nil {
			return vm.Value{}, err
		}
		return ev.toolCall(e.Tool, args, e)
	case dsl.CallFn:
		args, err := ev.args(e.Args, len(e.FnDecl.Params), en, nil)
		if err != nil {
			return vm.Value{}, err
		}
		return ev.callFn(e.FnDecl, args, e)
	case dsl.CallVariant:
		args, err := ev.args(e.Args, len(e.Variant.Payload), en, nil)
		if err != nil {
			return vm.Value{}, err
		}
		return ev.makeVariant(e.Variant, args, e)
	case dsl.CallClosure:
		f, err := ev.expr(e.Fn, en)
		if err != nil {
			return vm.Value{}, err
		}
		args := make([]vm.Value, len(e.Args))
		for i, a := range e.Args {
			if args[i], err = ev.expr(a.Value, en); err != nil {
				return vm.Value{}, err
			}
		}
		return ev.callValue(f, args, e)
	}
	return vm.Value{}, bug(e, "unresolved call")
}

// args evaluates arguments in source order and returns them in parameter
// order; absent parameters come from def.
func (ev *Eval) args(as []*dsl.Arg, n int, en *env, def func(i int) vm.Value) ([]vm.Value, error) {
	out := make([]vm.Value, n)
	set := make([]bool, n)
	for _, a := range as {
		v, err := ev.expr(a.Value, en)
		if err != nil {
			return nil, err
		}
		if a.Param < 0 || a.Param >= n {
			return nil, bug(a.Value, "argument bound to no parameter")
		}
		out[a.Param], set[a.Param] = v, true
	}
	for i := range out {
		if !set[i] {
			if def == nil {
				return nil, bug(nil, "argument %d missing", i)
			}
			out[i] = def(i)
		}
	}
	return out, nil
}

func (ev *Eval) approvalArgs(as []*dsl.Arg, en *env) ([]vm.Value, error) {
	return ev.args(as, 3, en, func(int) vm.Value { return vm.Duration(abi.ApprovalDefaultTimeout) })
}

// native applies a VM builtin; a failure fails the evaluation.
func (ev *Eval) native(id bytecode.BuiltinID, args []vm.Value, n dsl.Node) (vm.Value, error) {
	r, err := vm.CallBuiltin(ev.p.cfg.Module, ev.p.vmcfg, id, args)
	if err != nil {
		var f vm.Failed
		if errors.As(err, &f) {
			return vm.Value{}, fail(f.Code, f.Message, n)
		}
		return vm.Value{}, bug(n, "%v", err)
	}
	return r, nil
}

// builtinValues applies a library function that can be used as a value
// (a pure leaf function or an effect) to all its arguments.
func (ev *Eval) builtinValues(b *dsl.Builtin, args []vm.Value, n dsl.Node) (vm.Value, error) {
	if id, ok := abi.LeafBuiltin(b.Name); ok {
		return ev.native(id, args, n)
	}
	if kind, ok := abi.EffectKind(b); ok && kind != bytecode.EffectParMap {
		return ev.effect(kind, args, n)
	}
	return vm.Value{}, bug(n, "library function %s used as a value", b.Name)
}

// fnArg is the function argument of a higher-order library function,
// classified like the compiler does: lambda literals are evaluated inline
// (no call frame), item names are called directly, other function values
// with a closure call.
type fnArg struct {
	lambda *dsl.Lambda
	ref    dsl.Ref
	isRef  bool
	value  vm.Value
	node   dsl.Node
}

func (ev *Eval) apply(f *fnArg, args []vm.Value, en *env) (vm.Value, error) {
	switch {
	case f.lambda != nil:
		if len(args) != len(f.lambda.Params) {
			return vm.Value{}, bug(f.lambda, "lambda arity")
		}
		for i, p := range f.lambda.Params {
			en.set(p.Binding, args[i])
		}
		return ev.expr(f.lambda.Body, en)
	case f.isRef:
		switch f.ref.Kind {
		case dsl.RefFn:
			return ev.callFn(f.ref.Fn, args, f.node)
		case dsl.RefTool:
			return ev.toolCall(f.ref.Tool, args, f.node)
		case dsl.RefBuiltin:
			return ev.builtinValues(f.ref.Builtin, args, f.node)
		case dsl.RefVariant:
			return ev.makeVariant(f.ref.Variant, args, f.node)
		}
		return vm.Value{}, bug(f.node, "callable reference")
	}
	return ev.callValue(f.value, args, f.node)
}

func staticFn(e dsl.Expr) *fnArg {
	switch v := e.(type) {
	case *dsl.Lambda:
		if !v.Synthetic {
			return &fnArg{lambda: v, node: v}
		}
	case *dsl.Ident:
		if itemRef(v.Ref) {
			return &fnArg{ref: v.Ref, isRef: true, node: v}
		}
	case *dsl.PathExpr:
		if itemRef(v.Ref) {
			return &fnArg{ref: v.Ref, isRef: true, node: v}
		}
	}
	return nil
}

func itemRef(r dsl.Ref) bool {
	switch r.Kind {
	case dsl.RefFn, dsl.RefTool, dsl.RefBuiltin:
		return true
	case dsl.RefVariant:
		return r.Variant != nil && len(r.Variant.Payload) > 0
	}
	return false
}

// builtinCall evaluates a call of a standard-library function.
func (ev *Eval) builtinCall(e *dsl.CallExpr, en *env) (vm.Value, error) {
	b := e.Builtin
	if b == nil || len(b.Overloads) == 0 {
		return vm.Value{}, bug(e, "unresolved library call")
	}
	n := len(b.Overloads[0].Params)
	switch b.Name {
	case "map", "filter", "filter_map", "flat_map", "fold", "any", "all", "first", "count", "sort_by", "group_by":
		return ev.higherOrder(e, n, en)
	case "par_map":
		return ev.parMapCall(e, en)
	case "approve":
		args, err := ev.approvalArgs(e.Args, en)
		if err != nil {
			return vm.Value{}, err
		}
		return ev.effect(bytecode.EffectApproval, args, e)
	}
	args, err := ev.args(e.Args, n, en, nil)
	if err != nil {
		return vm.Value{}, err
	}
	return ev.builtinValues(b, args, e)
}

// hofArgs evaluates a higher-order call's arguments in source order; the
// function argument at fnAt is classified, not evaluated, when static.
func (ev *Eval) hofArgs(e *dsl.CallExpr, n, fnAt int, en *env) ([]vm.Value, *fnArg, error) {
	vals := make([]vm.Value, n)
	var f *fnArg
	for _, a := range e.Args {
		if a.Param < 0 || a.Param >= n {
			return nil, nil, bug(a.Value, "argument bound to no parameter")
		}
		if a.Param == fnAt {
			if s := staticFn(a.Value); s != nil {
				f = s
				continue
			}
		}
		v, err := ev.expr(a.Value, en)
		if err != nil {
			return nil, nil, err
		}
		if a.Param == fnAt {
			f = &fnArg{value: v, node: a.Value}
			continue
		}
		vals[a.Param] = v
	}
	return vals, f, nil
}

func (ev *Eval) higherOrder(e *dsl.CallExpr, n int, en *env) (vm.Value, error) {
	name := e.Builtin.Name
	fnAt := 1
	if name == "fold" {
		fnAt = 2
	}
	vals, f, err := ev.hofArgs(e, n, fnAt, en)
	if err != nil {
		return vm.Value{}, err
	}
	xs := vals[0].Elems()
	if f == nil {
		switch name {
		case "first":
			return ev.native(bytecode.BListGetOpt, []vm.Value{vals[0], vm.Int(0)}, e)
		case "count":
			return ev.native(bytecode.BLen, []vm.Value{vals[0]}, e)
		}
		return vm.Value{}, bug(e, "%s without a function", name)
	}
	one := func(x vm.Value) (vm.Value, error) { return ev.apply(f, []vm.Value{x}, en) }
	switch name {
	case "map", "flat_map", "sort_by", "group_by":
		out := make([]vm.Value, 0, len(xs))
		for _, x := range xs {
			r, err := one(x)
			if err != nil {
				return vm.Value{}, err
			}
			if len(out) >= ev.p.vmcfg.Limits.List {
				return vm.Value{}, fail(bytecode.FailLimitExceeded, "list over the element limit", e)
			}
			out = append(out, r)
		}
		mapped := vm.List(out...)
		switch name {
		case "flat_map":
			return ev.native(bytecode.BFlatten, []vm.Value{mapped}, e)
		case "sort_by":
			return ev.native(bytecode.BSortByKeys, []vm.Value{mapped, vals[0]}, e)
		case "group_by":
			return ev.native(bytecode.BGroupByKeys, []vm.Value{mapped, vals[0]}, e)
		}
		return mapped, nil
	case "filter", "filter_map":
		var out []vm.Value
		for _, x := range xs {
			r, err := one(x)
			if err != nil {
				return vm.Value{}, err
			}
			keep, item := r.AsBool(), x
			if name == "filter_map" {
				keep = r.IsSome()
				if keep {
					item = r.Fields()[0]
				}
			}
			if !keep {
				continue
			}
			if len(out) >= ev.p.vmcfg.Limits.List {
				return vm.Value{}, fail(bytecode.FailLimitExceeded, "list over the element limit", e)
			}
			out = append(out, item)
		}
		return vm.List(out...), nil
	case "fold":
		acc := vals[1]
		for _, x := range xs {
			r, err := ev.apply(f, []vm.Value{acc, x}, en)
			if err != nil {
				return vm.Value{}, err
			}
			acc = r
		}
		return acc, nil
	case "any", "all", "first":
		for _, x := range xs {
			r, err := one(x)
			if err != nil {
				return vm.Value{}, err
			}
			switch {
			case name == "any" && r.AsBool():
				return vm.Bool(true), nil
			case name == "all" && !r.AsBool():
				return vm.Bool(false), nil
			case name == "first" && r.AsBool():
				return vm.Some(x), nil
			}
		}
		switch name {
		case "any":
			return vm.Bool(false), nil
		case "all":
			return vm.Bool(true), nil
		}
		return vm.None(), nil
	case "count":
		k := int64(0)
		for _, x := range xs {
			r, err := one(x)
			if err != nil {
				return vm.Value{}, err
			}
			if r.AsBool() {
				k++
			}
		}
		return vm.Int(k), nil
	}
	return vm.Value{}, bug(e, "higher-order %s", name)
}

// parMapCall performs EFFECT par_map with (list, closure, limit, mode).
func (ev *Eval) parMapCall(e *dsl.CallExpr, en *env) (vm.Value, error) {
	vals := [4]vm.Value{}
	var fn dsl.Expr
	mode := int64(bytecode.ParMapFail)
	for _, a := range e.Args {
		switch a.Param {
		case 1:
			fn = a.Value
			if staticFn(a.Value) != nil {
				continue
			}
		case 3:
			if s, ok := a.Value.(*dsl.StringLit); ok && !s.Interpolated() {
				text := ""
				for _, p := range s.Parts {
					text += p.Lit
				}
				mode = abi.ParMapMode(text)
				continue
			}
			return vm.Value{}, bug(a.Value, "par_map on_error is not a literal")
		}
		v, err := ev.expr(a.Value, en)
		if err != nil {
			return vm.Value{}, err
		}
		if a.Param >= 0 && a.Param < 4 {
			vals[a.Param] = v
		}
	}
	if fn == nil {
		return vm.Value{}, bug(e, "par_map without a function")
	}
	if s := staticFn(fn); s != nil {
		var err error
		if s.lambda != nil {
			vals[1], err = ev.lambdaValue(s.lambda, en, nil)
		} else {
			vals[1], err = ev.itemValue(s.ref, fn)
		}
		if err != nil {
			return vm.Value{}, err
		}
	}
	vals[3] = vm.Int(mode)
	return ev.effect(bytecode.EffectParMap, vals[:], e)
}
