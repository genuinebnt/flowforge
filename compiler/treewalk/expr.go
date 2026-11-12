package treewalk

import (
	"strings"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler/internal/abi"
	"github.com/genuinebnt/flowforge/dsl"
	"github.com/genuinebnt/flowforge/vm"
)

// expr evaluates e in env.
func (ev *Eval) expr(e dsl.Expr, en *env) (vm.Value, error) {
	switch e := e.(type) {
	case *dsl.IntLit:
		return vm.Int(e.Value), nil
	case *dsl.FloatLit:
		return vm.Float(e.Value), nil
	case *dsl.DurationLit:
		return vm.Duration(e.Nanos), nil
	case *dsl.BoolLit:
		return vm.Bool(e.Value), nil
	case *dsl.StringLit:
		return ev.stringLit(e, en)
	case *dsl.Ident:
		return ev.ref(e.Ref, en, e)
	case *dsl.PathExpr:
		return ev.ref(e.Ref, en, e)
	case *dsl.TupleExpr:
		if len(e.Elems) == 0 {
			return vm.Unit(), nil
		}
		vals, err := ev.exprs(e.Elems, en)
		if err != nil {
			return vm.Value{}, err
		}
		return vm.Tuple(vals...), nil
	case *dsl.ListExpr:
		vals, err := ev.exprs(e.Elems, en)
		if err != nil {
			return vm.Value{}, err
		}
		if len(vals) > ev.p.vmcfg.Limits.List {
			return vm.Value{}, fail(bytecode.FailLimitExceeded, "list over the element limit", e)
		}
		return vm.List(vals...), nil
	case *dsl.RecordLit:
		return ev.recordLit(e, en)
	case *dsl.Lambda:
		return ev.lambdaValue(e, en, nil)
	case *dsl.IfExpr:
		c, err := ev.expr(e.Cond, en)
		if err != nil {
			return vm.Value{}, err
		}
		if c.AsBool() {
			return ev.block(e.Then, en)
		}
		if e.Else == nil {
			return vm.Unit(), nil
		}
		return ev.expr(e.Else, en)
	case *dsl.MatchExpr:
		return ev.match(e, en)
	case *dsl.AgentExpr:
		return ev.agent(e, en)
	case *dsl.BlockExpr:
		return ev.block(e, en)
	case *dsl.UnaryExpr:
		x, err := ev.expr(e.X, en)
		if err != nil {
			return vm.Value{}, err
		}
		return unary(e, x)
	case *dsl.BinaryExpr:
		return ev.binary(e, en)
	case *dsl.CallExpr:
		return ev.call(e, en)
	case *dsl.IndexExpr:
		x, err := ev.expr(e.X, en)
		if err != nil {
			return vm.Value{}, err
		}
		i, err := ev.expr(e.Index, en)
		if err != nil {
			return vm.Value{}, err
		}
		return index(e, x, i)
	case *dsl.FieldExpr:
		x, err := ev.expr(e.X, en)
		if err != nil {
			return vm.Value{}, err
		}
		fs := x.Fields()
		if e.Index < 0 || e.Index >= len(fs) {
			return vm.Value{}, bug(e, "field index")
		}
		return fs[e.Index], nil
	case *dsl.TryExpr:
		return ev.try(e, en)
	case *dsl.CastExpr:
		x, err := ev.expr(e.X, en)
		if err != nil {
			return vm.Value{}, err
		}
		return ev.cast(e, x)
	}
	return vm.Value{}, bug(e, "cannot evaluate %T", e)
}

func (ev *Eval) exprs(es []dsl.Expr, en *env) ([]vm.Value, error) {
	out := make([]vm.Value, len(es))
	for i, x := range es {
		v, err := ev.expr(x, en)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (ev *Eval) ref(r dsl.Ref, en *env, n dsl.Node) (vm.Value, error) {
	switch r.Kind {
	case dsl.RefLocal, dsl.RefCapture:
		return en.get(r.Binding, n)
	case dsl.RefFn, dsl.RefTool, dsl.RefBuiltin, dsl.RefVariant:
		return ev.itemValue(r, n)
	}
	return vm.Value{}, bug(n, "unresolved name")
}

// stringLit interpolates with the display form of the VM's INTERP.
func (ev *Eval) stringLit(s *dsl.StringLit, en *env) (vm.Value, error) {
	var sb strings.Builder
	for _, p := range s.Parts {
		if p.X == nil {
			sb.WriteString(p.Lit)
		} else {
			v, err := ev.expr(p.X, en)
			if err != nil {
				return vm.Value{}, err
			}
			sb.WriteString(vm.Display(v))
		}
		if sb.Len() > ev.p.vmcfg.Limits.String {
			return vm.Value{}, fail(bytecode.FailLimitExceeded, "string over 1 MiB", s)
		}
	}
	return vm.Str(sb.String()), nil
}

func (ev *Eval) recordLit(e *dsl.RecordLit, en *env) (vm.Value, error) {
	id, ok := ev.p.types.ID(e.Def)
	if !ok {
		return vm.Value{}, bug(e, "record without an id")
	}
	fields := make([]vm.Value, len(e.Def.Fields))
	for _, f := range e.Fields {
		v, err := ev.expr(f.Value, en)
		if err != nil {
			return vm.Value{}, err
		}
		if f.Index < 0 || f.Index >= len(fields) {
			return vm.Value{}, bug(e, "field index")
		}
		fields[f.Index] = v
	}
	return vm.Record(id, fields...), nil
}

// agent performs EFFECT agent_call with (planner, goal, context, tools,
// budget), () for absent arguments.
func (ev *Eval) agent(e *dsl.AgentExpr, en *env) (vm.Value, error) {
	vals := []vm.Value{vm.Unit(), vm.Unit(), vm.Unit(), vm.Unit()}
	for _, a := range e.Args {
		if a.Value == nil || a.Param == 2 {
			continue
		}
		v, err := ev.expr(a.Value, en)
		if err != nil {
			return vm.Value{}, err
		}
		if a.Param >= 0 && a.Param < 4 {
			vals[a.Param] = v
		}
	}
	tools := make([]vm.Value, len(e.Tools))
	for i, t := range e.Tools {
		tools[i] = vm.Str(t.Name)
	}
	vals[2] = vm.List(tools...)
	name := ""
	if e.Name != nil {
		name = e.Name.Name
	}
	return ev.effect(bytecode.EffectAgentCall, append([]vm.Value{vm.Str(name)}, vals...), e)
}

func (ev *Eval) binary(e *dsl.BinaryExpr, en *env) (vm.Value, error) {
	x, err := ev.expr(e.X, en)
	if err != nil {
		return vm.Value{}, err
	}
	switch e.Op {
	case dsl.OpAnd:
		if !x.AsBool() {
			return vm.Bool(false), nil
		}
		return ev.expr(e.Y, en)
	case dsl.OpOr:
		if x.AsBool() {
			return vm.Bool(true), nil
		}
		return ev.expr(e.Y, en)
	}
	y, err := ev.expr(e.Y, en)
	if err != nil {
		return vm.Value{}, err
	}
	switch e.Op {
	case dsl.OpEq:
		return vm.Bool(vm.Equal(x, y)), nil
	case dsl.OpNe:
		return vm.Bool(!vm.Equal(x, y)), nil
	case dsl.OpLt, dsl.OpLe, dsl.OpGt, dsl.OpGe:
		if x.Kind() != y.Kind() {
			return vm.Value{}, bug(e, "comparing different kinds")
		}
		c := vm.Compare(x, y)
		switch e.Op {
		case dsl.OpLt:
			return vm.Bool(c < 0), nil
		case dsl.OpLe:
			return vm.Bool(c <= 0), nil
		case dsl.OpGt:
			return vm.Bool(c > 0), nil
		}
		return vm.Bool(c >= 0), nil
	case dsl.OpIn:
		return ev.native(bytecode.BContains, []vm.Value{y, x}, e)
	case dsl.OpRange:
		return ev.native(bytecode.BRange, []vm.Value{x, y}, e)
	}
	return ev.arith(e, x, y)
}

// try evaluates `x?` according to TryExpr.Mode (dsl OPEN_QUESTIONS 12).
func (ev *Eval) try(e *dsl.TryExpr, en *env) (vm.Value, error) {
	x, err := ev.expr(e.X, en)
	if err != nil {
		return vm.Value{}, err
	}
	if x.Kind() != vm.KindEnum {
		return vm.Value{}, bug(e, "? on %s", x.Kind())
	}
	if x.Variant() == abi.OkVariant(e.OnOption) {
		fs := x.Fields()
		if len(fs) != 1 {
			return vm.Value{}, bug(e, "? payload")
		}
		return fs[0], nil
	}
	if e.Mode == dsl.TryFailRun {
		if !e.OnOption {
			if fs := x.Fields(); len(fs) == 1 && fs[0].Kind() == vm.KindRecord && fs[0].TypeID() == bytecode.TypeDenied {
				return vm.Value{}, fail(bytecode.FailApprovalDenied, "approval denied", e)
			}
		}
		return vm.Value{}, fail(bytecode.FailUnwrapFailed, "? failed", e)
	}
	return vm.Value{}, &returnSignal{v: x}
}

// block evaluates statements and the tail.
func (ev *Eval) block(b *dsl.BlockExpr, en *env) (vm.Value, error) {
	if b == nil {
		return vm.Unit(), nil
	}
	for _, s := range b.Stmts {
		if err := ev.stmt(s, en); err != nil {
			return vm.Value{}, err
		}
	}
	if b.Tail == nil {
		return vm.Unit(), nil
	}
	return ev.expr(b.Tail, en)
}

// stmt evaluates one statement.
func (ev *Eval) stmt(s dsl.Stmt, en *env) error {
	switch s := s.(type) {
	case *dsl.LetStmt:
		v, err := ev.expr(s.Value, en)
		if err != nil {
			return err
		}
		if ok, err := ev.bindPattern(s.Pat, v, en); err != nil {
			return err
		} else if !ok {
			return fail(bytecode.FailMatchFailed, "let pattern did not match", s.Pat)
		}
		if s.Compensate != nil {
			c, err := ev.lambdaValue(s.Compensate, en, &v)
			if err != nil {
				return err
			}
			ev.comps = append(ev.comps, c)
		}
		return nil
	case *dsl.ForStmt:
		return ev.forStmt(s, en)
	case *dsl.RequireStmt:
		args, err := ev.approvalArgs(s.Args, en)
		if err != nil {
			return err
		}
		r, err := ev.effect(bytecode.EffectApproval, args, s)
		if err != nil {
			return err
		}
		if !r.IsOk() {
			return fail(bytecode.FailApprovalDenied, "approval denied", s)
		}
		return nil
	case *dsl.ReturnStmt:
		v := vm.Unit()
		if s.Value != nil {
			var err error
			if v, err = ev.expr(s.Value, en); err != nil {
				return err
			}
		}
		return &returnSignal{v: v}
	case *dsl.ExprStmt:
		_, err := ev.expr(s.X, en)
		return err
	}
	return bug(s, "cannot evaluate statement %T", s)
}

func (ev *Eval) forStmt(s *dsl.ForStmt, en *env) error {
	it, err := ev.expr(s.Iter, en)
	if err != nil {
		return err
	}
	var elems []vm.Value
	if s.IterKind == dsl.IterMap {
		ks, vs := it.MapKeys(), it.MapValues()
		for i := range ks {
			elems = append(elems, vm.Tuple(ks[i], vs[i]))
		}
	} else {
		elems = it.Elems()
	}
	for _, x := range elems {
		if ok, err := ev.bindPattern(s.Pat, x, en); err != nil {
			return err
		} else if !ok {
			return fail(bytecode.FailMatchFailed, "for pattern did not match", s.Pat)
		}
		if _, err := ev.block(s.Body, en); err != nil {
			return err
		}
	}
	return nil
}
