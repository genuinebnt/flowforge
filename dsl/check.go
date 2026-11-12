package dsl

import (
	"fmt"
	"strings"
)

// This file checks item bodies, statements and non-call expressions.
// Checking is bidirectional: infer takes an optional hint (the expected
// type) which lambdas, empty lists, None and agent expressions use; the
// caller then unifies the inferred type with what it expects and reports
// any mismatch at the expression.

func (c *checker) newVar() *TypeVar {
	c.nextVar++
	return &TypeVar{ID: c.nextVar}
}

// instantiate maps each type parameter to a fresh inference variable.
func (c *checker) instantiate(params []*TypeParam) map[*TypeParam]Type {
	if len(params) == 0 {
		return nil
	}
	m := make(map[*TypeParam]Type, len(params))
	for _, p := range params {
		m[p] = c.newVar()
	}
	return m
}

// srcText returns the source text of a span (for help messages).
func (c *checker) srcText(sp Span) string {
	src := c.file.Src
	if sp.Start.Offset < 0 || sp.End.Offset > len(src) || sp.Start.Offset >= sp.End.Offset {
		return "..."
	}
	return string(src[sp.Start.Offset:sp.End.Offset])
}

// mismatch reports E0301 unless either type already contains an error.
func (c *checker) mismatch(sp Span, want, got Type, label, format string, args ...any) *Diagnostic {
	if hasError(want) || hasError(got) {
		return &Diagnostic{}
	}
	return c.sink.errorf(CodeTypeMismatch, sp, label, format, args...)
}

// expectType unifies got with want for expression e and reports E0301 on
// failure. It also rejects effectful function values flowing into
// function types written in source, which are pure (E0401).
func (c *checker) expectType(e Expr, got, want Type) bool {
	if !unify(got, want) {
		c.mismatch(e.Span(), want, got, "expected "+zonk(want).String(),
			"mismatched types: expected %s, found %s", zonk(want), zonk(got))
		return false
	}
	c.checkPureFunc(e.Span(), got, want)
	return true
}

func (c *checker) checkPureFunc(sp Span, got, want Type) {
	wf, ok := prune(want).(*FuncType)
	if !ok || wf.Open {
		return
	}
	gf, ok := prune(got).(*FuncType)
	if !ok {
		return
	}
	if extra := gf.Effects &^ wf.Effects; extra != 0 {
		c.sink.errorf(CodeEffectNotDeclared, sp, "performs "+extra.String(),
			"this function performs `%s` but function types are pure", extra).
			withHelp("Function types written in source carry no effects; call the effectful function directly or pass it to a standard-library function such as map.")
	}
}

// ---- Items ----

func (c *checker) checkBody(params []*Param, body *BlockExpr, info *FuncInfo, isWorkflow bool, generics []*Ident) {
	if body == nil || info == nil {
		return
	}
	c.sink = c.mainSink
	c.typeParams = map[string]*TypeParam{}
	for _, g := range info.Generics {
		c.typeParams[g.Name] = g
	}
	frame := &Frame{}
	info.Frame = frame
	c.fn = &fnCtx{frame: frame, info: info, isWorkflow: isWorkflow}
	c.scope = &scope{vars: map[string]*Binding{}}
	for i, p := range params {
		t := Type(TError)
		if i < len(info.Params) {
			t = info.Params[i]
		}
		if p.Name != nil {
			p.Binding = c.bind(p.Name, BindParam, t)
		}
	}
	c.deferred = nil
	got := c.inferBlock(body, info.Result)
	if !unify(got, info.Result) {
		sp := body.Span()
		label := "expected " + info.Result.String()
		if body.Tail != nil {
			sp = body.Tail.Span()
		} else {
			sp = Span{Start: body.Sp.End, End: body.Sp.End}
			if body.Sp.End.Offset > 0 {
				sp.Start = Pos{Line: body.Sp.End.Line, Col: body.Sp.End.Col - 1, Offset: body.Sp.End.Offset - 1}
			}
		}
		c.mismatch(sp, info.Result, got, label,
			"`%s` returns %s but its body has type %s", info.Name, zonk(info.Result), zonk(got))
	} else if body.Tail != nil {
		c.checkPureFunc(body.Tail.Span(), got, info.Result)
	}
	info.Inferred = body.Eff
	for _, fn := range c.deferred {
		fn()
	}
	c.deferred = nil
	c.finishItem(params, body)
	c.fn = nil
	c.scope = nil
	c.typeParams = nil
}

// finishItem zonks every type in a checked body and reports types that
// could not be inferred (E0304).
func (c *checker) finishItem(params []*Param, body *BlockExpr) {
	reported := map[*TypeVar]bool{}
	report := func(sp Span, t Type, what string) {
		if v, ok := hasVars(t); ok && !reported[v] && !hasError(t) {
			reported[v] = true
			c.sink.errorf(CodeCannotInfer, sp, "type must be known here",
				"cannot infer the type of this %s (found %s)", what, zonk(t)).
				withHelp("Add a type annotation, for example `let x: List<Int> = ...`.")
		}
	}
	Walk(body, func(n Node) bool {
		switch n := n.(type) {
		case Expr:
			b := n.base()
			if b.T == nil {
				b.T = TError
			}
			report(n.Span(), b.T, "expression")
			b.T = zonk(b.T)
			switch x := n.(type) {
			case *CallExpr:
				for i, a := range x.TypeArgs {
					x.TypeArgs[i] = zonk(a)
				}
			case *Lambda:
				for _, p := range x.Params {
					if p.Binding != nil {
						p.Binding.Type = zonk(p.Binding.Type)
					}
				}
			}
		case Pattern:
			b := n.pat()
			if b.T == nil {
				b.T = TError
			}
			b.T = zonk(b.T)
			switch p := n.(type) {
			case *BindPat:
				if p.Binding != nil {
					p.Binding.Type = zonk(p.Binding.Type)
				}
			case *RecordPat:
				for _, f := range p.Fields {
					if f.ShortBinding != nil {
						f.ShortBinding.Type = zonk(f.ShortBinding.Type)
					}
				}
			}
		case *ForStmt:
			n.ElemType = zonk(n.ElemType)
		}
		return true
	})
	for _, p := range params {
		if p.Binding != nil {
			p.Binding.Type = zonk(p.Binding.Type)
		}
	}
}

// ---- Blocks and statements ----

func (c *checker) inferBlock(b *BlockExpr, hint Type) Type {
	saved := c.scope
	c.scope = c.newScope()
	defer func() { c.scope = saved }()
	var eff EffectSet
	diverges := false
	for _, s := range b.Stmts {
		e, div := c.stmt(s)
		eff |= e
		if div {
			diverges = true
		}
	}
	t := Type(TUnit)
	if b.Tail != nil {
		t = c.infer(b.Tail, hint)
		eff |= b.Tail.Effects()
	} else if diverges {
		t = TNever
	}
	b.T, b.Eff = t, eff
	return t
}

// stmt checks one statement and returns its effects and whether it
// always diverges (returns).
func (c *checker) stmt(s Stmt) (EffectSet, bool) {
	switch s := s.(type) {
	case *LetStmt:
		return c.letStmt(s), false
	case *ForStmt:
		return c.forStmt(s), false
	case *RequireStmt:
		_, eff := c.checkCallArgs(prelude.approve.Overloads[0], nil, s.Args, "require approval", s.Sp)
		c.recordOp(OpApproval)
		return eff, false
	case *ReturnStmt:
		var eff EffectSet
		var got Type = TUnit
		if s.Value != nil {
			var hint Type
			if c.fn != nil && c.fn.lambda == nil {
				hint = c.fn.info.Result
			}
			got = c.infer(s.Value, hint)
			eff = s.Value.Effects()
		}
		if c.fn == nil || c.fn.lambda != nil {
			c.sink.errorf(CodeBadReturn, s.Sp, "inside a lambda",
				"`return` is not allowed inside a lambda or compensation").
				withHelp("A lambda's value is its body expression.")
			return eff, true
		}
		want := c.fn.info.Result
		sp := s.Sp
		if s.Value != nil {
			sp = s.Value.Span()
		}
		if !unify(got, want) {
			c.mismatch(sp, want, got, "expected "+zonk(want).String(),
				"mismatched types: `%s` returns %s, found %s", c.fn.info.Name, zonk(want), zonk(got))
		}
		return eff, true
	case *ExprStmt:
		t := c.infer(s.X, nil)
		return s.X.Effects(), isNever(t)
	case *AssignStmt:
		c.infer(s.Value, nil)
		if ref, ok := c.lookupLocal(s.Target.Name); ok {
			c.sink.errorf(CodeImmutable, s.Sp, "cannot assign",
				"cannot assign twice to immutable binding `%s`", s.Target.Name).
				withSecondary(c.sink.file, ref.Binding.Decl, "first bound here").
				withHelp("Bindings are immutable; write `let %s = ...;` in a nested block to shadow it.", s.Target.Name)
		} else {
			c.sink.errorf(CodeImmutable, s.Sp, "cannot assign",
				"cannot assign to `%s`; the language has no assignment", s.Target.Name).
				withHelp("Introduce a new binding with `let %s = ...;`.", s.Target.Name)
		}
		return s.Value.Effects(), false
	}
	return 0, false
}

func (c *checker) letStmt(s *LetStmt) EffectSet {
	var want Type
	if s.TypeX != nil {
		want = c.resolveType(s.TypeX)
	}
	opsBefore := c.opEvents
	got := c.infer(s.Value, want)
	t := got
	if want != nil {
		c.expectType(s.Value, got, want)
		t = want
	}
	eff := s.Value.Effects()
	performs := eff != 0 || c.opEvents > opsBefore
	c.checkLetAnnotations(s, performs)
	c.bindPattern(s.Pat, t, BindLet)
	if !c.irrefutable(s.Pat) {
		c.sink.errorf(CodeRefutable, s.Pat.Span(), "pattern may not match",
			"refutable pattern in let statement").
			withHelp("Use match to handle the other cases.")
	}
	if s.Compensate != nil {
		if !eff.Has(EffWrite) && !eff.Has(EffFinancialWrite) {
			c.sink.errorf(CodeBadCompensate, s.Value.Span(), "performs no write",
				"compensate requires the right side to perform a write or financial_write effect").
				withHelp("Only writes can be compensated; remove the compensate clause.")
		}
		eff |= c.checkCompensation(s.Compensate, t)
	}
	return eff
}

// checkCompensation checks the synthetic `it` lambda of a compensation and
// returns its latent effects (which the enclosing item may perform).
func (c *checker) checkCompensation(l *Lambda, itType Type) EffectSet {
	saved, savedFn := c.scope, c.fn
	frame := &Frame{Parent: c.fn.frame, Lambda: l}
	l.Frame = frame
	c.fn = &fnCtx{parent: savedFn, frame: frame, lambda: l, info: savedFn.info, isWorkflow: savedFn.isWorkflow}
	c.scope = c.newScope()
	l.Params[0].Binding = c.bind(l.Params[0].Name, BindIt, itType)
	bt := c.infer(l.Body, nil)
	c.scope, c.fn = saved, savedFn
	l.T = &FuncType{Params: []Type{itType}, Result: bt, Effects: l.Body.Effects()}
	return l.Body.Effects()
}

func (c *checker) forStmt(s *ForStmt) EffectSet {
	it := c.infer(s.Iter, nil)
	elem := Type(TError)
	switch pt := prune(it).(type) {
	case *NamedType:
		switch pt.Def {
		case defList:
			elem, s.IterKind = pt.Args[0], IterList
		case defStream:
			elem, s.IterKind = pt.Args[0], IterStream
		case defMap:
			elem, s.IterKind = &TupleType{Elems: []Type{pt.Args[0], pt.Args[1]}}, IterMap
		default:
			c.mismatch(s.Iter.Span(), TError, it, "not iterable", "cannot iterate over %s", zonk(it))
		}
	case *TypeVar:
		pt.Ref = TError
		c.sink.errorf(CodeCannotInfer, s.Iter.Span(), "type must be known here",
			"cannot infer the type of the iterated value").
			withHelp("Add a type annotation to the value being iterated.")
	default:
		if !hasError(it) {
			c.sink.errorf(CodeTypeMismatch, s.Iter.Span(), "not iterable",
				"cannot iterate over %s", zonk(it)).
				withHelp("for loops iterate over List, Stream and Map values.")
		}
	}
	s.ElemType = elem
	saved := c.scope
	c.scope = c.newScope()
	c.bindPattern(s.Pat, elem, BindPattern)
	if !c.irrefutable(s.Pat) {
		c.sink.errorf(CodeRefutable, s.Pat.Span(), "pattern may not match",
			"refutable pattern in for loop").withHelp("Use match inside the loop body.")
	}
	c.inferBlock(s.Body, nil)
	c.scope = saved
	return s.Iter.Effects() | s.Body.Effects()
}

// ---- Expressions ----

// infer checks e, records its type and effects, and returns its type.
func (c *checker) infer(e Expr, hint Type) Type {
	t, eff := c.inferExpr(e, hint)
	if t == nil {
		t = TError
	}
	b := e.base()
	b.T, b.Eff = t, eff
	return t
}

func (c *checker) inferExpr(e Expr, hint Type) (Type, EffectSet) {
	switch e := e.(type) {
	case *IntLit:
		return TInt, 0
	case *FloatLit:
		return TFloat, 0
	case *DurationLit:
		return TDuration, 0
	case *BoolLit:
		return TBool, 0
	case *StringLit:
		var eff EffectSet
		for _, p := range e.Parts {
			if p.X == nil {
				continue
			}
			c.infer(p.X, nil)
			eff |= p.X.Effects()
			x := p.X
			c.later(func() {
				t := prune(x.base().T)
				if _, isVar := t.(*TypeVar); isVar {
					return // reported as E0304 when the item is finished
				}
				if !displayable(t) && !hasError(t) {
					c.sink.errorf(CodeNotDisplayable, x.Span(), "not displayable",
						"%s cannot be interpolated into a string", zonk(t)).
						withHelp("Interpolate String, Int, Float, Bool, Duration, Time or a domain scalar such as Fqdn.")
				}
			})
		}
		return TString, eff
	case *Ident:
		return c.inferIdent(e, hint), 0
	case *PathExpr:
		return c.inferPath(e), 0
	case *TupleExpr:
		if len(e.Elems) == 0 {
			return TUnit, 0
		}
		var hints []Type
		if ht, ok := prune(hint).(*TupleType); ok && len(ht.Elems) == len(e.Elems) {
			hints = ht.Elems
		}
		tt := &TupleType{}
		var eff EffectSet
		for i, x := range e.Elems {
			var h Type
			if hints != nil {
				h = hints[i]
			}
			tt.Elems = append(tt.Elems, c.infer(x, h))
			eff |= x.Effects()
		}
		return tt, eff
	case *ListExpr:
		var elem Type
		if n := namedOf(hint, defList); n != nil {
			elem = n.Args[0]
		} else {
			elem = c.newVar()
		}
		var eff EffectSet
		for _, x := range e.Elems {
			t := c.infer(x, elem)
			eff |= x.Effects()
			if !unify(t, elem) {
				c.mismatch(x.Span(), elem, t, "expected "+zonk(elem).String(),
					"list elements must have the same type: expected %s, found %s", zonk(elem), zonk(t))
			}
		}
		return &NamedType{Def: defList, Args: []Type{mergeLatent(elem, e.Elems)}}, eff
	case *RecordLit:
		return c.inferRecordLit(e)
	case *Lambda:
		return c.inferLambda(e, hint), 0
	case *IfExpr:
		return c.inferIf(e, hint)
	case *MatchExpr:
		return c.inferMatch(e, hint)
	case *AgentExpr:
		return c.inferAgent(e, hint)
	case *BlockExpr:
		t := c.inferBlock(e, hint)
		return t, e.Eff
	case *UnaryExpr:
		return c.inferUnary(e)
	case *BinaryExpr:
		return c.inferBinary(e)
	case *CallExpr:
		return c.inferCall(e, hint)
	case *IndexExpr:
		return c.inferIndex(e)
	case *FieldExpr:
		return c.inferField(e)
	case *TryExpr:
		return c.inferTry(e)
	case *CastExpr:
		return c.inferCast(e)
	case *BadExpr:
		return TError, 0
	}
	return TError, 0
}

// mergeLatent unions the latent effects of function-typed branch values
// into the joined type t, so that choosing between closures keeps the
// effects of all of them.
func mergeLatent(t Type, branches []Expr) Type {
	ft, ok := prune(t).(*FuncType)
	if !ok {
		return t
	}
	eff := ft.Effects
	for _, b := range branches {
		if b == nil || b.base().T == nil {
			continue
		}
		if bf, ok := prune(b.base().T).(*FuncType); ok {
			eff |= bf.Effects
		}
	}
	if eff == ft.Effects {
		return t
	}
	return &FuncType{Params: ft.Params, Result: ft.Result, Effects: eff}
}

func (c *checker) unknownName(id *Ident) {
	d := c.sink.errorf(CodeUnknownName, id.Span(), "not found in this scope",
		"cannot find `%s` in this scope", id.Name)
	if sug := suggest(id.Name, c.valueNames()); sug != "" {
		d.withHelp("Did you mean `%s`?", sug)
	}
}

// resolveName resolves a value name: locals, then items, then the prelude.
func (c *checker) resolveName(name string) (Ref, bool) {
	if c.scope != nil {
		if r, ok := c.lookupLocal(name); ok {
			return r, true
		}
	}
	if s, ok := c.values[name]; ok {
		return s.ref, true
	}
	if v, ok := prelude.variants[name]; ok {
		return Ref{Kind: RefVariant, Variant: v}, true
	}
	if b, ok := prelude.builtins[name]; ok {
		return Ref{Kind: RefBuiltin, Builtin: b}, true
	}
	if name == "scope" {
		return Ref{Kind: RefNamespace}, true
	}
	return Ref{}, false
}

func (c *checker) inferIdent(id *Ident, hint Type) Type {
	ref, ok := c.resolveName(id.Name)
	if !ok {
		c.unknownName(id)
		return TError
	}
	id.Ref = ref
	return c.refValueType(ref, id.Span(), id.Name, hint)
}

// refValueType is the type of a reference used as a value.
func (c *checker) refValueType(ref Ref, sp Span, name string, hint Type) Type {
	switch ref.Kind {
	case RefLocal, RefCapture:
		return ref.Binding.Type
	case RefFn:
		info := ref.Fn.Func
		if info == nil {
			return TError
		}
		c.noteDeprecated(ref.Fn, sp, name)
		m := c.instantiate(info.Generics)
		ps := make([]Type, len(info.Params))
		for i, p := range info.Params {
			ps[i] = subst(p, m)
		}
		c.noteFnCall(ref.Fn)
		return &FuncType{Params: ps, Result: subst(info.Result, m), Effects: info.Declared}
	case RefTool:
		c.noteTool(ref.Tool)
		return ref.Tool.funcType()
	case RefWorkflow:
		c.sink.errorf(CodeNotCallable, sp, "a workflow",
			"workflow `%s` cannot be used as a value", name).
			withHelp("Workflows are started as runs; factor shared logic into a fn.")
		return TError
	case RefBuiltin:
		b := ref.Builtin
		sig := b.Overloads[0]
		if len(b.Overloads) > 1 || b.special != specialNone || hasOptional(sig) {
			c.sink.errorf(CodeNotCallable, sp, "call it instead",
				"builtin `%s` cannot be used as a function value", b.Name).
				withHelp("Wrap it in a lambda, for example `x => %s(x)`.", b.Name)
			return TError
		}
		m := c.instantiate(sig.Generics)
		ps := make([]Type, len(sig.Params))
		for i, p := range sig.Params {
			ps[i] = subst(p.Type, m)
		}
		c.recordOp(b.Op)
		return &FuncType{Params: ps, Result: subst(sig.Result, m), Effects: b.Effects}
	case RefVariant:
		v := ref.Variant
		m := c.instantiate(v.Def.Params)
		self := &NamedType{Def: v.Def, Args: paramArgs(v.Def.Params, m)}
		if len(v.Payload) == 0 {
			return self
		}
		ps := make([]Type, len(v.Payload))
		for i, p := range v.Payload {
			ps[i] = subst(p, m)
		}
		return &FuncType{Params: ps, Result: self}
	case RefNamespace:
		c.sink.errorf(CodeNotCallable, sp, "a namespace",
			"`scope` is a namespace, not a value").
			withHelp("Call scope.check(x) or scope.allows(x).")
		return TError
	}
	return TError
}

func hasOptional(s *Signature) bool {
	for _, p := range s.Params {
		if p.Optional {
			return true
		}
	}
	return false
}

func paramArgs(ps []*TypeParam, m map[*TypeParam]Type) []Type {
	out := make([]Type, len(ps))
	for i, p := range ps {
		out[i] = m[p]
	}
	return out
}

// resolvePath resolves A::B to an enum variant.
func (c *checker) resolvePath(segs []*Ident) (Ref, bool) {
	if len(segs) != 2 {
		c.sink.errorf(CodeUnknownName, segs[0].Span().Join(segs[len(segs)-1].Span()), "not found",
			"cannot resolve path `%s`", pathString(segs)).
			withHelp("Paths name enum variants, as in Severity::High.")
		return Ref{}, false
	}
	def := c.lookupTypeDef(segs[0].Name)
	if def == nil {
		d := c.sink.errorf(CodeUnknownType, segs[0].Span(), "not found", "unknown type `%s`", segs[0].Name)
		if sug := suggest(segs[0].Name, c.typeNames()); sug != "" {
			d.withHelp("Did you mean `%s`?", sug)
		}
		return Ref{}, false
	}
	if def.Kind != DefEnum {
		c.sink.errorf(CodeUnknownVariant, segs[0].Span().Join(segs[1].Span()), "not an enum",
			"`%s` is not an enum", def.Name)
		return Ref{}, false
	}
	v := def.VariantByName(segs[1].Name)
	if v == nil {
		names := make([]string, len(def.Variants))
		for i, vv := range def.Variants {
			names[i] = vv.Name
		}
		d := c.sink.errorf(CodeUnknownVariant, segs[1].Span(), "not a variant",
			"`%s` has no variant `%s`", def.Name, segs[1].Name)
		if sug := suggest(segs[1].Name, names); sug != "" {
			d.withHelp("Did you mean `%s::%s`?", def.Name, sug)
		} else {
			d.withHelp("Variants of %s: %s.", def.Name, strings.Join(names, ", "))
		}
		return Ref{}, false
	}
	return Ref{Kind: RefVariant, Variant: v}, true
}

func (c *checker) inferPath(e *PathExpr) Type {
	ref, ok := c.resolvePath(e.Segs)
	if !ok {
		return TError
	}
	e.Ref = ref
	return c.refValueType(ref, e.Span(), pathString(e.Segs), nil)
}

func (c *checker) inferRecordLit(e *RecordLit) (Type, EffectSet) {
	var eff EffectSet
	inferFields := func() {
		for _, f := range e.Fields {
			c.infer(f.Value, nil)
			eff |= f.Value.Effects()
		}
	}
	if len(e.Path) != 1 {
		c.sink.errorf(CodeUnknownType, e.Path[0].Span(), "not a record",
			"`%s` is not a record type", pathString(e.Path))
		inferFields()
		return TError, eff
	}
	name := e.Path[0]
	def := c.lookupTypeDef(name.Name)
	if def == nil {
		d := c.sink.errorf(CodeUnknownType, name.Span(), "not found", "unknown type `%s`", name.Name)
		if sug := suggest(name.Name, c.typeNames()); sug != "" {
			d.withHelp("Did you mean `%s`?", sug)
		}
		inferFields()
		return TError, eff
	}
	if def.Kind != DefRecord {
		c.sink.errorf(CodeBadField, name.Span(), "not a record",
			"`%s` is not a record type and cannot be built with `{ ... }`", def.Name)
		inferFields()
		return TError, eff
	}
	e.Def = def
	m := c.instantiate(def.Params)
	self := &NamedType{Def: def, Args: paramArgs(def.Params, m)}
	seen := map[string]bool{}
	for _, f := range e.Fields {
		fd := def.FieldByName(f.Name.Name)
		if fd == nil {
			names := make([]string, len(def.Fields))
			for i, x := range def.Fields {
				names[i] = x.Name
			}
			d := c.sink.errorf(CodeBadField, f.Name.Span(), "unknown field",
				"record `%s` has no field `%s`", def.Name, f.Name.Name)
			if sug := suggest(f.Name.Name, names); sug != "" {
				d.withHelp("Did you mean `%s`?", sug)
			}
			c.infer(f.Value, nil)
			eff |= f.Value.Effects()
			continue
		}
		if seen[fd.Name] {
			c.sink.errorf(CodeBadField, f.Name.Span(), "duplicate field",
				"field `%s` is given more than once", fd.Name)
		}
		seen[fd.Name] = true
		f.Index = fd.Index
		want := subst(fd.Type, m)
		got := c.infer(f.Value, want)
		eff |= f.Value.Effects()
		if !unify(got, want) {
			c.mismatch(f.Value.Span(), want, got, "expected "+zonk(want).String(),
				"field `%s` of `%s` expects %s but found %s", fd.Name, def.Name, zonk(want), zonk(got))
			continue
		}
		c.checkPureFunc(f.Value.Span(), got, want)
		if !def.Builtin && containsInScope(got) {
			d := c.sink.errorf(CodeScopeProofMisuse, f.Value.Span(), "this value is a scope proof",
				"%s cannot be stored in a record field", scopeProofName(got))
			if td, ok := def.Decl.(*TypeDecl); ok {
				d.withSecondary(c.sink.file, td.Name.Span(), "record declared here")
			}
			d.withHelp("Store the %s and call scope.check again where it is used.", scopeProofInner(got))
		}
	}
	var missing []string
	for _, fd := range def.Fields {
		if !seen[fd.Name] {
			missing = append(missing, "`"+fd.Name+"`")
		}
	}
	if len(missing) > 0 {
		c.sink.errorf(CodeBadField, e.Span(), "missing fields",
			"missing field(s) %s in `%s`", strings.Join(missing, ", "), def.Name)
	}
	return self, eff
}

func (c *checker) inferLambda(l *Lambda, hint Type) Type {
	var hintF *FuncType
	if ft, ok := prune(hint).(*FuncType); ok && len(ft.Params) == len(l.Params) {
		hintF = ft
	}
	saved, savedFn := c.scope, c.fn
	frame := &Frame{Lambda: l}
	if savedFn != nil {
		frame.Parent = savedFn.frame
	}
	l.Frame = frame
	info := &FuncInfo{Result: TError}
	isWF := false
	if savedFn != nil {
		info, isWF = savedFn.info, savedFn.isWorkflow
	}
	c.fn = &fnCtx{parent: savedFn, frame: frame, lambda: l, info: info, isWorkflow: isWF}
	c.scope = c.newScope()
	ps := make([]Type, len(l.Params))
	for i, p := range l.Params {
		var t Type
		if hintF != nil {
			t = hintF.Params[i]
		} else {
			t = c.newVar()
		}
		ps[i] = t
		p.Binding = c.bind(p.Name, BindLambdaParam, t)
	}
	var rh Type
	if hintF != nil {
		rh = hintF.Result
	}
	rt := c.infer(l.Body, rh)
	c.scope, c.fn = saved, savedFn
	return &FuncType{Params: ps, Result: rt, Effects: l.Body.Effects()}
}

func (c *checker) expectBool(e Expr, what string) {
	t := c.infer(e, TBool)
	if !unify(t, TBool) {
		c.mismatch(e.Span(), TBool, t, "expected Bool",
			"%s must be Bool, found %s", what, zonk(t))
	}
}

func (c *checker) inferIf(e *IfExpr, hint Type) (Type, EffectSet) {
	c.expectBool(e.Cond, "if condition")
	eff := e.Cond.Effects()
	tt := c.inferBlock(e.Then, hint)
	eff |= e.Then.Effects()
	if e.Else == nil {
		if !unify(tt, TUnit) {
			sp := e.Then.Span()
			if e.Then.Tail != nil {
				sp = e.Then.Tail.Span()
			}
			c.mismatch(sp, TUnit, tt, "expected ()",
				"if without else must have type (), found %s", zonk(tt)).
				withHelp("Add an else branch, or end the block with `;`.")
		}
		return TUnit, eff
	}
	h := hint
	if h == nil {
		h = tt
	}
	et := c.infer(e.Else, h)
	eff |= e.Else.Effects()
	result := tt
	if isNever(tt) {
		result = et
	}
	if !unify(tt, et) {
		sp := e.Else.Span()
		if b, ok := e.Else.(*BlockExpr); ok && b.Tail != nil {
			sp = b.Tail.Span()
		}
		c.mismatch(sp, tt, et, "expected "+zonk(tt).String(),
			"if and else have different types: %s and %s", zonk(tt), zonk(et))
	}
	return mergeLatent(result, []Expr{e.Then, e.Else}), eff
}

func (c *checker) inferMatch(e *MatchExpr, hint Type) (Type, EffectSet) {
	st := c.infer(e.Scrutinee, nil)
	eff := e.Scrutinee.Effects()
	var result Type = hint
	if result == nil {
		result = c.newVar()
	}
	bodies := make([]Expr, 0, len(e.Arms))
	sawValue := false
	for _, arm := range e.Arms {
		saved := c.scope
		c.scope = c.newScope()
		c.bindPattern(arm.Pat, st, BindPattern)
		if arm.Guard != nil {
			c.expectBool(arm.Guard, "match guard")
			eff |= arm.Guard.Effects()
		}
		bt := c.infer(arm.Body, result)
		eff |= arm.Body.Effects()
		c.scope = saved
		bodies = append(bodies, arm.Body)
		if !unify(bt, result) {
			c.mismatch(arm.Body.Span(), result, bt, "expected "+zonk(result).String(),
				"match arms have different types: expected %s, found %s", zonk(result), zonk(bt))
		}
		if !isNever(bt) {
			sawValue = true
		}
	}
	if !sawValue {
		if _, isVar := prune(result).(*TypeVar); isVar {
			result = TNever
		}
	}
	c.checkMatch(e, st)
	return mergeLatent(result, bodies), eff
}

func (c *checker) inferUnary(e *UnaryExpr) (Type, EffectSet) {
	t := c.infer(e.X, nil)
	eff := e.X.Effects()
	switch e.Op {
	case OpNot:
		if !unify(t, TBool) {
			c.badOperand(e.X.Span(), t, "`!` expects Bool, found %s")
			return TError, eff
		}
		return TBool, eff
	default:
		pt := prune(t)
		if _, isVar := pt.(*TypeVar); isVar {
			unify(t, TInt)
			return TInt, eff
		}
		if isPrim(pt, PrimInt) || isPrim(pt, PrimFloat) || isPrim(pt, PrimDuration) || hasError(pt) {
			return pt, eff
		}
		c.badOperand(e.X.Span(), t, "unary `-` expects Int, Float or Duration, found %s")
		return TError, eff
	}
}

func (c *checker) badOperand(sp Span, t Type, format string) {
	if hasError(t) {
		return
	}
	c.sink.errorf(CodeBadOperand, sp, "invalid operand", format, zonk(t))
}

// arith lists the accepted operand and result types of arithmetic.
var arith = map[BinaryOp][][3]*PrimType{
	OpAdd: {{TInt, TInt, TInt}, {TFloat, TFloat, TFloat}, {TDuration, TDuration, TDuration}, {TTime, TDuration, TTime}, {TString, TString, TString}},
	OpSub: {{TInt, TInt, TInt}, {TFloat, TFloat, TFloat}, {TDuration, TDuration, TDuration}, {TTime, TDuration, TTime}, {TTime, TTime, TDuration}},
	OpMul: {{TInt, TInt, TInt}, {TFloat, TFloat, TFloat}, {TDuration, TInt, TDuration}, {TInt, TDuration, TDuration}},
	OpDiv: {{TInt, TInt, TInt}, {TFloat, TFloat, TFloat}, {TDuration, TInt, TDuration}},
	OpRem: {{TInt, TInt, TInt}},
}

func (c *checker) inferBinary(e *BinaryExpr) (Type, EffectSet) {
	switch e.Op {
	case OpAnd, OpOr:
		c.expectBool(e.X, fmt.Sprintf("operand of `%s`", e.Op))
		c.expectBool(e.Y, fmt.Sprintf("operand of `%s`", e.Op))
		return TBool, e.X.Effects() | e.Y.Effects()
	}
	xt := c.infer(e.X, nil)
	// Only operators whose operands have equal types pass the left type
	// down as a hint (arithmetic such as Duration * Int does not).
	var yHint Type
	switch e.Op {
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		yHint = xt
	}
	yt := c.infer(e.Y, yHint)
	eff := e.X.Effects() | e.Y.Effects()
	if hasError(xt) || hasError(yt) {
		switch e.Op {
		case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe, OpIn:
			return TBool, eff
		}
		return TError, eff
	}
	switch e.Op {
	case OpEq, OpNe:
		if !unify(xt, yt) {
			c.mismatch(e.Y.Span(), xt, yt, "expected "+zonk(xt).String(),
				"cannot compare %s with %s", zonk(xt), zonk(yt))
		} else if !equatable(xt) {
			c.badOperand(e.Span(), xt, "values of type %s cannot be compared with ==")
		}
		return TBool, eff
	case OpLt, OpLe, OpGt, OpGe:
		if !unify(xt, yt) {
			c.mismatch(e.Y.Span(), xt, yt, "expected "+zonk(xt).String(),
				"cannot compare %s with %s", zonk(xt), zonk(yt))
		} else if _, isVar := prune(xt).(*TypeVar); !isVar && !ordered(xt) {
			c.badOperand(e.Span(), xt, "values of type %s are not ordered")
		}
		return TBool, eff
	case OpIn:
		switch pt := prune(yt).(type) {
		case *NamedType:
			switch pt.Def {
			case defList, defStream:
				if !unify(xt, pt.Args[0]) {
					c.mismatch(e.X.Span(), pt.Args[0], xt, "expected "+zonk(pt.Args[0]).String(),
						"`in` expects an element of type %s, found %s", zonk(pt.Args[0]), zonk(xt))
				}
				return TBool, eff
			case defMap:
				if !unify(xt, pt.Args[0]) {
					c.mismatch(e.X.Span(), pt.Args[0], xt, "expected "+zonk(pt.Args[0]).String(),
						"`in` expects a key of type %s, found %s", zonk(pt.Args[0]), zonk(xt))
				}
				return TBool, eff
			}
		case *PrimType:
			if pt.Kind == PrimString {
				if !unify(xt, TString) {
					c.mismatch(e.X.Span(), TString, xt, "expected String",
						"`in` on a String expects a String, found %s", zonk(xt))
				}
				return TBool, eff
			}
		}
		c.badOperand(e.Y.Span(), yt, "`in` expects a List, Stream, Map or String, found %s")
		return TBool, eff
	case OpRange:
		if !unify(xt, TInt) {
			c.badOperand(e.X.Span(), xt, "ranges are over Int, found %s")
		}
		if !unify(yt, TInt) {
			c.badOperand(e.Y.Span(), yt, "ranges are over Int, found %s")
		}
		return &NamedType{Def: defList, Args: []Type{TInt}}, eff
	}
	// Arithmetic. When one operand is still unknown, pick the operand
	// type the table implies: the only possible one, else the same type
	// as the known side.
	px, py := prune(xt), prune(yt)
	_, xv := px.(*TypeVar)
	_, yv := py.(*TypeVar)
	if xv && !yv {
		guess := arithGuess(e.Op, py, false)
		unify(px, guess)
		px = prune(px)
	} else if yv && !xv {
		guess := arithGuess(e.Op, px, true)
		unify(py, guess)
		py = prune(py)
	} else if xv && yv {
		unify(px, TInt)
		unify(py, TInt)
		return TInt, eff
	}
	a, aok := px.(*PrimType)
	b, bok := py.(*PrimType)
	if aok && bok {
		for _, row := range arith[e.Op] {
			if row[0] == a && row[1] == b {
				return row[2], eff
			}
		}
	}
	c.sink.errorf(CodeBadOperand, e.Span(), "invalid operands",
		"`%s` cannot be applied to %s and %s", e.Op, zonk(xt), zonk(yt)).
		withHelp("There are no implicit conversions; use `as` to convert, for example `n as Float`.")
	return TError, eff
}

func (c *checker) inferIndex(e *IndexExpr) (Type, EffectSet) {
	xt := c.infer(e.X, nil)
	eff := e.X.Effects()
	switch pt := prune(xt).(type) {
	case *NamedType:
		switch pt.Def {
		case defList, defStream:
			it := c.infer(e.Index, TInt)
			if !unify(it, TInt) {
				c.mismatch(e.Index.Span(), TInt, it, "expected Int", "list index must be Int, found %s", zonk(it))
			}
			return pt.Args[0], eff | e.Index.Effects()
		case defMap:
			it := c.infer(e.Index, pt.Args[0])
			if !unify(it, pt.Args[0]) {
				c.mismatch(e.Index.Span(), pt.Args[0], it, "expected "+zonk(pt.Args[0]).String(),
					"map key must be %s, found %s", zonk(pt.Args[0]), zonk(it))
			}
			return pt.Args[1], eff | e.Index.Effects()
		}
	case *TypeVar:
		pt.Ref = TError
		c.infer(e.Index, nil)
		c.sink.errorf(CodeCannotInfer, e.X.Span(), "type must be known here",
			"cannot infer the type of the indexed value").withHelp("Add a type annotation.")
		return TError, eff | e.Index.Effects()
	}
	c.infer(e.Index, nil)
	c.badOperand(e.X.Span(), xt, "%s cannot be indexed")
	return TError, eff | e.Index.Effects()
}

func (c *checker) inferField(e *FieldExpr) (Type, EffectSet) {
	if id, ok := e.X.(*Ident); ok {
		if ref, ok := c.resolveName(id.Name); ok && ref.Kind == RefNamespace {
			id.Ref = ref
			id.T = TError
			b := prelude.builtins["scope."+e.Name.Name]
			if b == nil {
				c.sink.errorf(CodeUnknownName, e.Name.Span(), "not found",
					"`scope` has no function `%s`", e.Name.Name).
					withHelp("The scope namespace provides check and allows.")
				return TError, 0
			}
			e.Builtin = b
			return c.refValueType(Ref{Kind: RefBuiltin, Builtin: b}, e.Span(), b.Name, nil), 0
		}
	}
	xt := c.infer(e.X, nil)
	eff := e.X.Effects()
	switch pt := prune(xt).(type) {
	case *NamedType:
		if pt.Def.Kind == DefRecord {
			fd := pt.Def.FieldByName(e.Name.Name)
			if fd == nil {
				names := make([]string, len(pt.Def.Fields))
				for i, f := range pt.Def.Fields {
					names[i] = f.Name
				}
				d := c.sink.errorf(CodeBadField, e.Name.Span(), "unknown field",
					"`%s` has no field `%s`", pt.Def.Name, e.Name.Name)
				if sug := suggest(e.Name.Name, names); sug != "" {
					d.withHelp("Did you mean `%s`?", sug)
				}
				return TError, eff
			}
			e.Index = fd.Index
			m := map[*TypeParam]Type{}
			for i, p := range pt.Def.Params {
				m[p] = pt.Args[i]
			}
			return subst(fd.Type, m), eff
		}
	case *TypeVar:
		pt.Ref = TError
		c.sink.errorf(CodeCannotInfer, e.X.Span(), "type must be known here",
			"cannot infer the type of this value to access field `%s`", e.Name.Name).
			withHelp("Add a type annotation to the binding.")
		return TError, eff
	}
	if !hasError(xt) {
		c.sink.errorf(CodeBadField, e.Name.Span(), "no fields",
			"%s has no field `%s`", zonk(xt), e.Name.Name)
	}
	return TError, eff
}

func (c *checker) inferTry(e *TryExpr) (Type, EffectSet) {
	xt := c.infer(e.X, nil)
	eff := e.X.Effects()
	var inner, errT Type
	switch pt := prune(xt).(type) {
	case *NamedType:
		switch pt.Def {
		case defResult:
			inner, errT = pt.Args[0], pt.Args[1]
		case defOption:
			inner = pt.Args[0]
			e.OnOption = true
		}
	case *TypeVar:
		pt.Ref = TError
		c.sink.errorf(CodeCannotInfer, e.X.Span(), "type must be known here",
			"cannot infer the type of the operand of `?`").withHelp("Add a type annotation.")
		return TError, eff
	}
	if inner == nil {
		if !hasError(xt) {
			c.sink.errorf(CodeBadTry, e.Span(), "not a Result or Option",
				"`?` applies to Result and Option, found %s", zonk(xt))
		}
		return TError, eff
	}
	if c.fn == nil || c.fn.lambda != nil {
		e.Mode = TryFailRun
		return inner, eff
	}
	ret := c.fn.info.Result
	if rn := namedOf(ret, defResult); rn != nil {
		if e.OnOption {
			c.sink.errorf(CodeBadTry, e.Span(), "Option in a Result function",
				"`?` on an Option in `%s`, which returns %s", c.fn.info.Name, ret).
				withHelp("Match on the Option and return an Err explicitly.")
			return inner, eff
		}
		if !unify(errT, rn.Args[1]) && !hasError(errT) && !hasError(rn.Args[1]) {
			c.sink.errorf(CodeBadTry, e.Span(), "error type "+zonk(errT).String(),
				"`?` cannot convert error type %s into %s (the error type of `%s`)", zonk(errT), zonk(rn.Args[1]), c.fn.info.Name).
				withHelp("Error types must be equal; there is no implicit conversion. Map the error with match.")
		}
		e.Mode = TryReturnErr
		return inner, eff
	}
	if namedOf(ret, defOption) != nil && e.OnOption {
		e.Mode = TryReturnNone
		return inner, eff
	}
	e.Mode = TryFailRun
	return inner, eff
}

func (c *checker) inferCast(e *CastExpr) (Type, EffectSet) {
	xt := c.infer(e.X, nil)
	to := c.resolveType(e.To)
	eff := e.X.Effects()
	if hasError(xt) || hasError(to) {
		return to, eff
	}
	if containsInScope(to) {
		c.sink.errorf(CodeScopeProofMisuse, e.To.Span(), "this type is a scope proof",
			"cannot cast to %s", scopeProofName(to)).
			withHelp("Only scope.check produces scope proofs.")
		return to, eff
	}
	from := prune(xt)
	if v, isVar := from.(*TypeVar); isVar {
		v.Ref = TError
		c.sink.errorf(CodeCannotInfer, e.X.Span(), "type must be known here",
			"cannot infer the type of the value being cast")
		return to, eff
	}
	kind, ok := castKind(from, prune(to))
	if !ok {
		c.sink.errorf(CodeBadCast, e.Span(), "invalid cast",
			"cannot cast %s to %s", zonk(from), zonk(to)).
			withHelp("Casts convert between Int, Float, Duration and Time, and between String and domain scalars.")
		return to, eff
	}
	e.Kind = kind
	return to, eff
}

func castKind(from, to Type) (CastKind, bool) {
	if unify(from, to) {
		return CastNone, true
	}
	fp, fok := from.(*PrimType)
	tp, tok := to.(*PrimType)
	if fok && tok {
		switch {
		case fp.Kind == PrimInt && tp.Kind == PrimFloat:
			return CastIntToFloat, true
		case fp.Kind == PrimFloat && tp.Kind == PrimInt:
			return CastFloatToInt, true
		case fp.Kind == PrimInt && tp.Kind == PrimDuration:
			return CastIntToDuration, true
		case fp.Kind == PrimDuration && tp.Kind == PrimInt:
			return CastDurationToInt, true
		case fp.Kind == PrimInt && tp.Kind == PrimTime:
			return CastIntToTime, true
		case fp.Kind == PrimTime && tp.Kind == PrimInt:
			return CastTimeToInt, true
		}
		return CastNone, false
	}
	if fn, ok := from.(*NamedType); ok && fn.Def.Kind == DefScalar && tok && tp.Kind == PrimString {
		return CastToString, true
	}
	if tn, ok := to.(*NamedType); ok && tn.Def.Kind == DefScalar && fok && fp.Kind == PrimString {
		return CastParseDomain, true
	}
	return CastNone, false
}

// arithGuess returns the type an unknown operand of op most likely has,
// given the known operand (known is the left operand when left is true).
func arithGuess(op BinaryOp, known Type, left bool) Type {
	kp, ok := known.(*PrimType)
	if !ok {
		return known
	}
	var cands []*PrimType
	for _, row := range arith[op] {
		if left && row[0] == kp {
			cands = append(cands, row[1])
		}
		if !left && row[1] == kp {
			cands = append(cands, row[0])
		}
	}
	if len(cands) == 1 {
		return cands[0]
	}
	return known
}

// later defers a check to the end of the current item, when inference
// variables have been solved.
func (c *checker) later(fn func()) { c.deferred = append(c.deferred, fn) }
