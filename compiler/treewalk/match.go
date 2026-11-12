package treewalk

import (
	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler/internal/abi"
	"github.com/genuinebnt/flowforge/dsl"
	"github.com/genuinebnt/flowforge/vm"
)

// match evaluates the first arm whose pattern matches and whose guard
// holds; no match fails with match_failed.
func (ev *Eval) match(e *dsl.MatchExpr, en *env) (vm.Value, error) {
	v, err := ev.expr(e.Scrutinee, en)
	if err != nil {
		return vm.Value{}, err
	}
	for _, arm := range e.Arms {
		ok, err := ev.bindPattern(arm.Pat, v, en)
		if err != nil {
			return vm.Value{}, err
		}
		if !ok {
			continue
		}
		if arm.Guard != nil {
			g, err := ev.expr(arm.Guard, en)
			if err != nil {
				return vm.Value{}, err
			}
			if !g.AsBool() {
				continue
			}
		}
		return ev.expr(arm.Body, en)
	}
	return vm.Value{}, fail(bytecode.FailMatchFailed, "no match arm matched", e)
}

// bindPattern tests p against v, binding variables in en as it goes
// (left to right, like the compiled tests).
func (ev *Eval) bindPattern(p dsl.Pattern, v vm.Value, en *env) (bool, error) {
	switch p := p.(type) {
	case *dsl.WildcardPat:
		return true, nil
	case *dsl.BindPat:
		if p.Variant != nil {
			return v.Kind() == vm.KindEnum && v.Variant() == abi.VariantIndex(p.Variant), nil
		}
		en.set(p.Binding, v)
		return true, nil
	case *dsl.LitPat:
		lit, err := ev.expr(p.Lit, en)
		if err != nil {
			return false, err
		}
		return vm.Equal(v, lit), nil
	case *dsl.VariantPat:
		if p.Variant == nil {
			return false, bug(p, "unresolved variant pattern")
		}
		if v.Kind() != vm.KindEnum || v.Variant() != abi.VariantIndex(p.Variant) {
			return false, nil
		}
		fs := v.Fields()
		if len(p.Args) > 0 && len(fs) != len(p.Args) {
			return false, bug(p, "variant payload arity")
		}
		for i, a := range p.Args {
			ok, err := ev.bindPattern(a, fs[i], en)
			if err != nil || !ok {
				return ok, err
			}
		}
		return true, nil
	case *dsl.TuplePat:
		if len(p.Elems) == 0 {
			return true, nil
		}
		fs := v.Fields()
		if len(fs) != len(p.Elems) {
			return false, bug(p, "tuple arity")
		}
		for i, el := range p.Elems {
			ok, err := ev.bindPattern(el, fs[i], en)
			if err != nil || !ok {
				return ok, err
			}
		}
		return true, nil
	case *dsl.RecordPat:
		fs := v.Fields()
		for _, f := range p.Fields {
			if f.Index < 0 || f.Index >= len(fs) {
				return false, bug(p, "record pattern field")
			}
			if f.Pat == nil {
				en.set(f.ShortBinding, fs[f.Index])
				continue
			}
			ok, err := ev.bindPattern(f.Pat, fs[f.Index], en)
			if err != nil || !ok {
				return ok, err
			}
		}
		return true, nil
	}
	return false, bug(p, "cannot match pattern %T", p)
}
