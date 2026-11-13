package dsl

// This file finishes effect analysis after bodies are checked: transitive
// operation and tool summaries, declared-versus-inferred effects (E0401,
// W0402) and the compile-time policy check (E0402, E0403).

import "github.com/genuinebnt/flowforge/core"

// summarize propagates runtime operations and tool uses through calls to
// user fns until a fixpoint.
func (c *checker) summarize() {
	var infos []*FuncInfo
	for _, it := range c.file.Items {
		switch d := it.(type) {
		case *FnDecl:
			if d.Func != nil {
				infos = append(infos, d.Func)
			}
		case *WorkflowDecl:
			if d.Func != nil {
				infos = append(infos, d.Func)
			}
		}
	}
	for _, info := range infos {
		info.Ops = c.directOps[info]
		info.Tools = append([]*Tool{}, c.toolUses[info]...)
	}
	for changed := true; changed; {
		changed = false
		for _, info := range infos {
			for _, callee := range c.fnCalls[info] {
				ci := callee.Func
				if ci == nil || ci == info {
					continue
				}
				if ops := info.Ops | ci.Ops; ops != info.Ops {
					info.Ops = ops
					changed = true
				}
				for _, t := range ci.Tools {
					if !containsTool(info.Tools, t) {
						info.Tools = append(info.Tools, t)
						changed = true
					}
				}
			}
		}
	}
}

func containsTool(ts []*Tool, t *Tool) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

// siteEffects returns the effects a call or agent expression performs
// itself (not through its arguments' own calls): the callee's effects
// plus, for higher-order builtins, the latent effects of function
// arguments that are not lambda literals (lambda bodies are sites of
// their own).
func siteEffects(n Node) EffectSet {
	switch x := n.(type) {
	case *CallExpr:
		eff := x.Own
		if x.Kind == CallBuiltin && x.Builtin != nil && x.Builtin.HigherOrder {
			for _, a := range x.Args {
				if a.Value == nil {
					continue
				}
				if _, isLambda := a.Value.(*Lambda); isLambda {
					continue
				}
				if ft, ok := prune(a.Value.base().T).(*FuncType); ok {
					eff |= ft.Effects
				}
			}
		}
		return eff
	case *AgentExpr:
		return EffectSet(EffLLM)
	}
	return 0
}

// firstSite returns the span of the earliest expression in body that
// performs e.
func firstSite(body Node, e Effect) (Span, bool) {
	var best Span
	found := false
	Walk(body, func(n Node) bool {
		if siteEffects(n).Has(e) {
			sp := n.Span()
			if !found || sp.Start.Offset < best.Start.Offset {
				best, found = sp, true
			}
		}
		return true
	})
	return best, found
}

// checkDeclaredEffects compares a fn's or workflow's declared effects
// with the inferred ones.
func (c *checker) checkDeclaredEffects(name *Ident, clause *EffectClause, info *FuncInfo, body *BlockExpr, what string) {
	if info == nil || body == nil {
		return
	}
	if what == "workflow" && !info.HasDeclared {
		return
	}
	c.sink = c.mainSink
	missing := info.Inferred &^ info.Declared
	missing.each(func(e Effect, en string) {
		sp, ok := firstSite(body, e)
		if !ok {
			sp = name.Span()
		}
		d := c.sink.errorf(CodeEffectNotDeclared, sp, "performs "+en,
			"%s `%s` performs effect `%s` but does not declare it", what, info.Name, en)
		if info.HasDeclared {
			d.withSecondary(c.sink.file, clause.Span(), "declared effects")
			d.withHelp("Add `%s` to the effect clause: `! %s`.", en, (info.Declared | EffectSet(e)).String())
		} else {
			d.withSecondary(c.sink.file, name.Span(), "declared pure here")
			d.withHelp("A %s without `!` is pure; declare its effects with `! %s`.", what, info.Inferred.String())
		}
	})
	if !info.HasDeclared || clause == nil {
		return
	}
	extra := info.Declared &^ info.Inferred
	for _, n := range clause.Names {
		e, ok := ParseEffect(n.Name)
		if !ok || !extra.Has(e) {
			continue
		}
		c.sink.warnf(WarnOverBroad, n.Span(), "never performed",
			"%s `%s` declares effect `%s` but never performs it", what, info.Name, n.Name).
			withHelp("Remove `%s` from the effect clause.", n.Name)
		extra &^= EffectSet(e)
	}
}

// checkPolicy enforces the program policy on every workflow.
func (c *checker) checkPolicy() {
	maxClass := c.opts.Policy.MaxClass
	// Fn summaries: gated effects a call needs approved beforehand.
	need := map[*FnDecl]EffectSet{}
	var fns []*FnDecl
	for _, it := range c.file.Items {
		if d, ok := it.(*FnDecl); ok && d.Func != nil && d.Body != nil {
			fns = append(fns, d)
			need[d] = (d.Func.Declared | d.Func.Inferred) & gatedEffects
		}
	}
	for i := 0; i <= len(fns); i++ {
		changed := false
		for _, fn := range fns {
			w := &approvalWalker{c: c, need: need}
			w.expr(fn.Body, false)
			if w.acc != need[fn] {
				need[fn] = w.acc
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	for _, it := range c.file.Items {
		d, ok := it.(*WorkflowDecl)
		if !ok || d.Func == nil || d.Body == nil {
			continue
		}
		d.Func.Inferred.each(func(e Effect, en string) {
			cls := effectClass(e)
			if cls <= maxClass {
				return
			}
			sp, ok := firstSite(d.Body, e)
			if !ok {
				sp = d.Name.Span()
			}
			c.sink.errorf(CodeEffectExceedsMax, sp, "performs "+en,
				"effect `%s` exceeds the program's maximum activity class `%s`", en, maxClass).
				withHelp("The program allows at most %s activity; remove this call or ask a program owner to raise max_class.", maxClass)
		})
		c.checkProhibited(d)
		w := &approvalWalker{c: c, need: need, report: true}
		w.expr(d.Body, false)
	}
}

// checkProhibited reports E0402 for every tool workflow d may call that
// the program's activity.prohibited list names, by tool name or by one of
// its declared effects (spec 4.1; scope OPEN_QUESTIONS 19). E0402 is the
// closest existing code: spec 7.7 has no code for a prohibited activity.
// Tools are reported in first-use order, at their first use.
func (c *checker) checkProhibited(d *WorkflowDecl) {
	if len(c.opts.Policy.Prohibited) == 0 {
		return
	}
	for _, t := range d.Func.Tools {
		entry, bad := core.ProhibitedActivity(c.opts.Policy.Prohibited, append([]string{t.Name}, t.Effects.Names()...)...)
		if !bad {
			continue
		}
		sp, ok := firstToolSite(d.Body, t)
		if !ok {
			sp = d.Name.Span()
		}
		c.sink.errorf(CodeEffectExceedsMax, sp, "prohibited",
			"tool `%s` is prohibited by the program (activity.prohibited `%s`)", t.Name, entry).
			withHelp("The program forbids this activity; remove this call or ask a program owner to change activity.prohibited.")
	}
}

// firstToolSite returns the span of the earliest expression in body that
// may call t: a call of t, t used as a function value, or a call of (or
// reference to) a fn whose summary includes t. On equal start offsets the
// outer expression wins, so a direct call reports the whole call.
func firstToolSite(body Node, t *Tool) (Span, bool) {
	var best Span
	found := false
	consider := func(sp Span) {
		if !found || sp.Start.Offset < best.Start.Offset {
			best, found = sp, true
		}
	}
	refs := func(ref Ref) bool {
		switch ref.Kind {
		case RefTool:
			return ref.Tool == t
		case RefFn:
			return ref.Fn != nil && ref.Fn.Func != nil && containsTool(ref.Fn.Func.Tools, t)
		}
		return false
	}
	Walk(body, func(n Node) bool {
		switch x := n.(type) {
		case *CallExpr:
			if x.Tool == t || (x.Kind == CallFn && x.FnDecl != nil && x.FnDecl.Func != nil && containsTool(x.FnDecl.Func.Tools, t)) {
				consider(x.Span())
			}
		case *Ident:
			if refs(x.Ref) {
				consider(x.Span())
			}
		}
		return true
	})
	return best, found
}

// approvalWalker checks that gated effects (financial_write, intrusive)
// are dominated by an approval: `require approval(...)`, `approve(...)?`,
// or the Ok arm of a match on approve(...). Approvals inside a branch,
// loop body, short-circuit operand or lambda do not count after it.
type approvalWalker struct {
	c      *checker
	need   map[*FnDecl]EffectSet
	report bool
	acc    EffectSet
}

func isApproveCall(e Expr) bool {
	call, ok := e.(*CallExpr)
	return ok && call.Kind == CallBuiltin && call.Builtin == prelude.approve
}

func (w *approvalWalker) site(n Node, gated EffectSet, ok bool) {
	if gated == 0 || ok {
		return
	}
	if !w.report {
		w.acc |= gated
		return
	}
	name := "this call"
	if call, isCall := n.(*CallExpr); isCall {
		name = "`" + w.c.calleeName(call.Fn) + "`"
	}
	w.c.sink.errorf(CodeMissingApproval, n.Span(), "needs approval",
		"%s performs `%s` without a preceding approval", name, gated).
		withHelp("Add `require approval(role = \"...\", reason = \"...\");` before this statement.")
}

func (w *approvalWalker) exprs(es []Expr, ok bool) bool {
	for _, e := range es {
		ok = w.expr(e, ok)
	}
	return ok
}

func (w *approvalWalker) args(as []*Arg, ok bool) bool {
	for _, a := range as {
		if a.Value != nil {
			ok = w.expr(a.Value, ok)
		}
	}
	return ok
}

func (w *approvalWalker) callGated(call *CallExpr) EffectSet {
	switch call.Kind {
	case CallFn:
		if call.FnDecl != nil {
			if g, ok := w.need[call.FnDecl]; ok {
				return g
			}
		}
		return call.Own & gatedEffects
	default:
		return siteEffects(call) & gatedEffects
	}
}

func (w *approvalWalker) expr(e Expr, ok bool) bool {
	switch e := e.(type) {
	case nil:
		return ok
	case *StringLit:
		for _, p := range e.Parts {
			if p.X != nil {
				ok = w.expr(p.X, ok)
			}
		}
		return ok
	case *TupleExpr:
		return w.exprs(e.Elems, ok)
	case *ListExpr:
		return w.exprs(e.Elems, ok)
	case *RecordLit:
		for _, f := range e.Fields {
			ok = w.expr(f.Value, ok)
		}
		return ok
	case *Lambda:
		w.expr(e.Body, ok)
		return ok
	case *IfExpr:
		ok = w.expr(e.Cond, ok)
		t := w.expr(e.Then, ok)
		if e.Else == nil {
			return ok
		}
		el := w.expr(e.Else, ok)
		return t && el
	case *MatchExpr:
		s := w.expr(e.Scrutinee, ok)
		if len(e.Arms) == 0 {
			return s
		}
		approves := isApproveCall(e.Scrutinee)
		all := true
		for _, arm := range e.Arms {
			a := s
			if approves {
				if vp, isV := arm.Pat.(*VariantPat); isV && vp.Variant != nil && vp.Variant.Def == defResult && vp.Variant.Name == "Ok" {
					a = true
				}
			}
			if arm.Guard != nil {
				a = w.expr(arm.Guard, a)
			}
			if !w.expr(arm.Body, a) {
				all = false
			}
		}
		return all
	case *AgentExpr:
		return w.args(e.Args, ok)
	case *BlockExpr:
		for _, s := range e.Stmts {
			ok = w.stmt(s, ok)
		}
		if e.Tail != nil {
			ok = w.expr(e.Tail, ok)
		}
		return ok
	case *UnaryExpr:
		return w.expr(e.X, ok)
	case *BinaryExpr:
		x := w.expr(e.X, ok)
		y := w.expr(e.Y, x)
		if e.Op == OpAnd || e.Op == OpOr {
			return x
		}
		return y
	case *CallExpr:
		if e.Pipe != PipeNone && len(e.Args) > 0 {
			ok = w.expr(e.Args[0].Value, ok)
			ok = w.expr(e.Fn, ok)
			ok = w.args(e.Args[1:], ok)
		} else {
			ok = w.expr(e.Fn, ok)
			ok = w.args(e.Args, ok)
		}
		w.site(e, w.callGated(e), ok)
		return ok
	case *IndexExpr:
		return w.expr(e.Index, w.expr(e.X, ok))
	case *FieldExpr:
		return w.expr(e.X, ok)
	case *TryExpr:
		s := w.expr(e.X, ok)
		if isApproveCall(e.X) {
			return true
		}
		return s
	case *CastExpr:
		return w.expr(e.X, ok)
	}
	return ok
}

func (w *approvalWalker) stmt(s Stmt, ok bool) bool {
	switch s := s.(type) {
	case *LetStmt:
		ok = w.expr(s.Value, ok)
		if s.Compensate != nil {
			w.expr(s.Compensate.Body, ok)
		}
		return ok
	case *ForStmt:
		ok = w.expr(s.Iter, ok)
		w.expr(s.Body, ok)
		return ok
	case *RequireStmt:
		w.args(s.Args, ok)
		return true
	case *ReturnStmt:
		return w.expr(s.Value, ok)
	case *ExprStmt:
		return w.expr(s.X, ok)
	case *AssignStmt:
		return w.expr(s.Value, ok)
	}
	return ok
}
