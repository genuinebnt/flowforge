package dsl

import (
	"fmt"
	"strings"
)

// callParam is one parameter of a resolved callee.
type callParam struct {
	name     string // "" when the callee has no parameter names
	t        Type
	optional bool
}

func (c *checker) recordOp(op OpKind) {
	if op == OpNone || c.fn == nil || c.fn.info == nil {
		return
	}
	c.directOps[c.fn.info] = c.directOps[c.fn.info].with(op)
	c.opEvents++
}

func (c *checker) noteTool(t *Tool, sp Span) {
	if c.fn == nil || c.fn.info == nil {
		return
	}
	if t.Decl != nil && t.Decl.Attrs.HasDeprecated {
		c.deprecatedWarning(sp, "tool", t.Name, t.Decl.Attrs.Deprecated)
	}
	uses := c.toolUses[c.fn.info]
	for _, u := range uses {
		if u == t {
			c.recordOp(OpToolCall)
			return
		}
	}
	c.toolUses[c.fn.info] = append(uses, t)
	c.recordOp(OpToolCall)
}

func (c *checker) noteFnCall(fn *FnDecl) {
	if c.fn == nil || c.fn.info == nil {
		return
	}
	c.fnCalls[c.fn.info] = append(c.fnCalls[c.fn.info], fn)
	c.opEvents++
}

func (c *checker) noteDeprecated(fn *FnDecl, sp Span, name string) {
	if fn.Attrs.HasDeprecated {
		c.deprecatedWarning(sp, "function", name, fn.Attrs.Deprecated)
	}
}

func (c *checker) deprecatedWarning(sp Span, what, name, msg string) {
	d := c.sink.warnf(WarnDeprecated, sp, "deprecated", "%s `%s` is deprecated", what, name)
	if msg != "" {
		d.withHelp("%s", msg)
	}
}

// calleeName renders a callee for messages.
func (c *checker) calleeName(fn Expr) string {
	switch f := fn.(type) {
	case *Ident:
		return f.Name
	case *PathExpr:
		return pathString(f.Segs)
	case *FieldExpr:
		if id, ok := f.X.(*Ident); ok {
			return id.Name + "." + f.Name.Name
		}
	}
	return "this function"
}

func (c *checker) inferCall(call *CallExpr, hint Type) (Type, EffectSet) {
	name := c.calleeName(call.Fn)
	sp := call.Fn.Span()

	// Resolve the callee without checking it as a value where possible.
	var ref Ref
	resolved := false
	switch f := call.Fn.(type) {
	case *Ident:
		r, ok := c.resolveName(f.Name)
		if !ok {
			c.unknownName(f)
			f.T = TError
			c.inferArgsLoose(call.Args)
			return TError, argsEffects(call.Args)
		}
		f.Ref = r
		ref, resolved = r, true
	case *PathExpr:
		r, ok := c.resolvePath(f.Segs)
		if !ok {
			f.T = TError
			c.inferArgsLoose(call.Args)
			return TError, argsEffects(call.Args)
		}
		f.Ref = r
		ref, resolved = r, true
	case *FieldExpr:
		if id, ok := f.X.(*Ident); ok {
			if r, ok := c.resolveName(id.Name); ok && r.Kind == RefNamespace {
				id.Ref = r
				id.T = TError
				b := prelude.builtins["scope."+f.Name.Name]
				if b == nil {
					c.sink.errorf(CodeUnknownName, f.Name.Span(), "not found",
						"`scope` has no function `%s`", f.Name.Name).
						withHelp("The scope namespace provides check and allows.")
					f.T = TError
					c.inferArgsLoose(call.Args)
					return TError, argsEffects(call.Args)
				}
				f.Builtin = b
				f.T = TError
				ref, resolved = Ref{Kind: RefBuiltin, Builtin: b}, true
			}
		}
	}

	if resolved {
		switch ref.Kind {
		case RefBuiltin:
			if id, ok := call.Fn.(*Ident); ok {
				id.T = TError
			}
			return c.callBuiltin(call, ref.Builtin, name, hint)
		case RefTool:
			t := ref.Tool
			call.Fn.base().T = t.funcType()
			call.Kind, call.Tool, call.Own = CallTool, t, t.Effects
			c.noteTool(t, sp)
			ps := make([]callParam, len(t.Params))
			for i := range t.Params {
				ps[i] = callParam{name: t.ParamNames[i], t: t.Params[i]}
			}
			eff := c.checkArgsList(ps, call.Args, name, call.Span())
			return t.Result, eff | t.Effects
		case RefFn:
			fn := ref.Fn
			info := fn.Func
			if info == nil {
				c.inferArgsLoose(call.Args)
				return TError, argsEffects(call.Args)
			}
			c.noteDeprecated(fn, sp, name)
			c.noteFnCall(fn)
			m := c.instantiate(info.Generics)
			ps := make([]callParam, len(info.Params))
			for i, p := range info.Params {
				ps[i] = callParam{name: fn.Params[i].Name.Name, t: subst(p, m)}
			}
			res := subst(info.Result, m)
			call.Fn.base().T = &FuncType{Params: paramTypes(ps), Result: res, Effects: info.Declared}
			call.Kind, call.FnDecl, call.Own = CallFn, fn, info.Declared
			call.TypeArgs = paramArgs(info.Generics, m)
			eff := c.checkArgsList(ps, call.Args, name, call.Span())
			return res, eff | info.Declared
		case RefVariant:
			v := ref.Variant
			m := c.instantiate(v.Def.Params)
			self := &NamedType{Def: v.Def, Args: paramArgs(v.Def.Params, m)}
			if len(v.Payload) == 0 {
				c.sink.errorf(CodeNotCallable, call.Fn.Span(), "takes no arguments",
					"variant `%s` has no payload and cannot be called", v.QualifiedName()).
					withHelp("Write `%s` without parentheses.", name)
				c.inferArgsLoose(call.Args)
				return self, argsEffects(call.Args)
			}
			ps := make([]callParam, len(v.Payload))
			for i, p := range v.Payload {
				ps[i] = callParam{t: subst(p, m)}
			}
			call.Fn.base().T = &FuncType{Params: paramTypes(ps), Result: self}
			call.Kind, call.Variant = CallVariant, v
			call.TypeArgs = paramArgs(v.Def.Params, m)
			if hint != nil {
				unify(self, hint)
			}
			eff := c.checkArgsList(ps, call.Args, name, call.Span())
			return self, eff
		case RefWorkflow:
			c.sink.errorf(CodeNotCallable, call.Fn.Span(), "a workflow",
				"workflow `%s` cannot be called", name).
				withHelp("Workflows are started as runs; factor shared logic into a fn.")
			call.Fn.base().T = TError
			c.inferArgsLoose(call.Args)
			return TError, argsEffects(call.Args)
		case RefNamespace:
			c.sink.errorf(CodeNotCallable, call.Fn.Span(), "a namespace",
				"`scope` is a namespace and cannot be called").
				withHelp("Call scope.check(x) or scope.allows(x).")
			call.Fn.base().T = TError
			c.inferArgsLoose(call.Args)
			return TError, argsEffects(call.Args)
		}
		// Locals fall through to closure calls.
	}

	// A closure: any expression of function type.
	ft := c.infer(call.Fn, nil)
	calleeEff := call.Fn.Effects()
	switch f := prune(ft).(type) {
	case *FuncType:
		call.Kind, call.Own = CallClosure, f.Effects
		ps := make([]callParam, len(f.Params))
		for i, p := range f.Params {
			ps[i] = callParam{t: p}
		}
		eff := c.checkArgsList(ps, call.Args, name, call.Span())
		return f.Result, eff | calleeEff | f.Effects
	case *TypeVar:
		// Infer a function type from the arguments.
		ps := make([]Type, len(call.Args))
		var eff EffectSet
		for i, a := range call.Args {
			ps[i] = c.infer(a.Value, nil)
			a.Param = i
			eff |= a.Value.Effects()
		}
		res := c.newVar()
		unify(f, &FuncType{Params: ps, Result: res})
		call.Kind = CallClosure
		return res, eff | calleeEff
	}
	if !hasError(ft) {
		c.sink.errorf(CodeNotCallable, call.Fn.Span(), "not a function",
			"%s is not callable (it has type %s)", c.srcText(call.Fn.Span()), zonk(ft))
	}
	c.inferArgsLoose(call.Args)
	return TError, argsEffects(call.Args) | calleeEff
}

func paramTypes(ps []callParam) []Type {
	out := make([]Type, len(ps))
	for i, p := range ps {
		out[i] = p.t
	}
	return out
}

func argsEffects(args []*Arg) EffectSet {
	var eff EffectSet
	for _, a := range args {
		if a.Value != nil {
			eff |= a.Value.Effects()
		}
	}
	return eff
}

// inferArgsLoose checks arguments with no expectations (after an error).
func (c *checker) inferArgsLoose(args []*Arg) {
	for _, a := range args {
		if a.Value != nil && a.Value.base().T == nil {
			c.infer(a.Value, nil)
		}
	}
}

// bindArgs assigns each argument to a parameter index (Arg.Param) and
// reports named-argument (E0308) and arity (E0303) errors.
func (c *checker) bindArgs(ps []callParam, args []*Arg, callee string, sp Span) bool {
	ok := true
	bound := make([]bool, len(ps))
	seenNamed := false
	pos := 0
	for _, a := range args {
		a.Param = -1
		if a.Name == nil {
			if seenNamed {
				c.sink.errorf(CodeBadNamedArg, a.Value.Span(), "positional after named",
					"positional arguments must come before named arguments")
				ok = false
				continue
			}
			if pos >= len(ps) {
				c.sink.errorf(CodeArity, a.Value.Span(), "unexpected argument",
					"%s takes %d argument(s) but %d were given", callee, len(ps), countPositional(args)).
					withHelp("Remove the extra argument.")
				ok = false
				pos++
				continue
			}
			a.Param = pos
			bound[pos] = true
			pos++
			continue
		}
		seenNamed = true
		idx := -1
		for i, p := range ps {
			if p.name != "" && p.name == a.Name.Name {
				idx = i
			}
		}
		if idx < 0 {
			var names []string
			for _, p := range ps {
				if p.name != "" {
					names = append(names, p.name)
				}
			}
			if len(names) == 0 {
				c.sink.errorf(CodeBadNamedArg, a.Name.Span(), "named argument",
					"%s does not take named arguments", callee)
			} else {
				d := c.sink.errorf(CodeBadNamedArg, a.Name.Span(), "unknown parameter",
					"%s has no parameter named `%s`", callee, a.Name.Name)
				if sug := suggest(a.Name.Name, names); sug != "" {
					d.withHelp("Did you mean `%s`?", sug)
				} else {
					d.withHelp("Parameters: %s.", strings.Join(names, ", "))
				}
			}
			ok = false
			continue
		}
		if bound[idx] {
			c.sink.errorf(CodeBadNamedArg, a.Name.Span(), "given twice",
				"argument `%s` is given more than once", a.Name.Name)
			ok = false
			continue
		}
		a.Param = idx
		bound[idx] = true
	}
	var missing []string
	for i, p := range ps {
		if !bound[i] && !p.optional {
			if p.name != "" {
				missing = append(missing, "`"+p.name+"`")
			} else {
				missing = append(missing, fmt.Sprintf("#%d", i+1))
			}
		}
	}
	if len(missing) > 0 {
		required := 0
		for _, p := range ps {
			if !p.optional {
				required++
			}
		}
		if len(args) < required {
			c.sink.errorf(CodeArity, sp, "missing arguments",
				"%s expects %d argument(s) but %d were given (missing %s)", callee, required,
				len(args), strings.Join(missing, ", "))
		} else {
			c.sink.errorf(CodeArity, sp, "missing arguments",
				"%s is missing argument(s) %s", callee, strings.Join(missing, ", "))
		}
		ok = false
	}
	return ok
}

func countPositional(args []*Arg) int {
	n := 0
	for _, a := range args {
		if a.Name == nil {
			n++
		}
	}
	return n
}

// checkArgsList binds and checks arguments against parameters and
// returns the arguments' effects.
func (c *checker) checkArgsList(ps []callParam, args []*Arg, callee string, sp Span) EffectSet {
	return c.checkArgsWith(ps, args, callee, sp, nil)
}

func (c *checker) checkArgsWith(ps []callParam, args []*Arg, callee string, sp Span, pre map[*Arg]Type) EffectSet {
	c.bindArgs(ps, args, callee, sp)
	var eff EffectSet
	for _, a := range args {
		if a.Value == nil {
			continue
		}
		if a.Param < 0 {
			if _, done := pre[a]; !done {
				c.infer(a.Value, nil)
			}
			eff |= a.Value.Effects()
			continue
		}
		want := ps[a.Param].t
		got, done := pre[a]
		if !done {
			got = c.infer(a.Value, want)
		}
		eff |= a.Value.Effects()
		c.checkArg(a.Value, got, want, callee)
	}
	return eff
}

// checkArg unifies an argument with its parameter type, reporting E0301
// in the style of spec 12.6 ("httpx expects InScope<Host> but found Host").
func (c *checker) checkArg(e Expr, got, want Type, callee string) {
	if unify(got, want) {
		c.checkPureFunc(e.Span(), got, want)
		return
	}
	if hasError(got) || hasError(want) {
		return
	}
	w, g := zonk(want), zonk(got)
	if namedOf(w, defInScope) != nil && namedOf(g, defInScope) == nil {
		c.sink.errorf(CodeTypeMismatch, e.Span(), "wrap in scope.check(...)?",
			"%s expects %s but found %s", callee, w, g).
			withHelp("Call scope.check(%s)? to obtain an %s.", c.srcText(e.Span()), w)
		return
	}
	c.sink.errorf(CodeTypeMismatch, e.Span(), "expected "+w.String(),
		"%s expects %s but found %s", callee, w, g)
}

// checkCallArgs checks arguments against a builtin signature and returns
// the instantiated function type and the arguments' effects.
func (c *checker) checkCallArgs(sig *Signature, pre map[*Arg]Type, args []*Arg, callee string, sp Span) (*FuncType, EffectSet) {
	m := c.instantiate(sig.Generics)
	ps := make([]callParam, len(sig.Params))
	for i, p := range sig.Params {
		ps[i] = callParam{name: p.Name, t: subst(p.Type, m), optional: p.Optional}
	}
	eff := c.checkArgsWith(ps, args, callee, sp, pre)
	return &FuncType{Params: paramTypes(ps), Result: subst(sig.Result, m)}, eff
}

// firstArg returns the argument that binds a builtin's first parameter.
func firstArg(b *Builtin, args []*Arg) *Arg {
	if len(args) == 0 {
		return nil
	}
	if args[0].Name == nil {
		return args[0]
	}
	for _, a := range args {
		if a.Name == nil {
			continue
		}
		for _, s := range b.Overloads {
			if len(s.Params) > 0 && s.Params[0].Name == a.Name.Name {
				return a
			}
		}
	}
	return nil
}

// headMatches reports whether arg has the same head type constructor as
// the overload parameter param (the same named type definition, or the
// same primitive), which is how an overloaded builtin is chosen by its
// first argument. Type arguments are not compared.
func headMatches(param, arg Type) bool {
	switch pt := param.(type) {
	case *NamedType:
		an, ok := prune(arg).(*NamedType)
		return ok && an.Def == pt.Def
	case *PrimType:
		ap, ok := prune(arg).(*PrimType)
		return ok && ap.Kind == pt.Kind
	}
	return false
}

func (c *checker) callBuiltin(call *CallExpr, b *Builtin, name string, hint Type) (Type, EffectSet) {
	call.Kind, call.Builtin, call.Own = CallBuiltin, b, b.Effects
	c.recordOp(b.Op)
	sig := b.Overloads[0]
	pre := map[*Arg]Type{}
	if len(b.Overloads) > 1 {
		if a := firstArg(b, call.Args); a != nil {
			t := c.infer(a.Value, nil)
			pre[a] = t
			chosen := (*Signature)(nil)
			for _, s := range b.Overloads {
				if len(s.Params) > 0 && headMatches(s.Params[0].Type, t) {
					chosen = s
					break
				}
			}
			if chosen == nil {
				if _, isVar := prune(t).(*TypeVar); !isVar && !hasError(t) {
					c.sink.errorf(CodeTypeMismatch, a.Value.Span(), "unsupported type",
						"%s expects %s but found %s", name, overloadHeads(b), zonk(t))
					c.inferArgsLoose(call.Args)
					return TError, argsEffects(call.Args)
				}
				chosen = b.Overloads[0]
			}
			sig = chosen
		}
	}
	ft, eff := c.checkCallArgs(sig, pre, call.Args, name, call.Span())
	ft.Effects = b.Effects
	ft.Open = b.HigherOrder
	call.Fn.base().T = ft
	res := ft.Result
	if b.HigherOrder {
		for _, a := range call.Args {
			if a.Value == nil {
				continue
			}
			if ft, ok := prune(a.Value.base().T).(*FuncType); ok {
				eff |= ft.Effects
			}
		}
	}
	switch b.special {
	case specialFlatMap:
		res = c.flatMapResult(call, sig)
	case specialScopeTarget:
		if a := firstArg(b, call.Args); a != nil && a.Value.base().T != nil {
			c.checkScopeTarget(a.Value, name)
		}
	case specialDisplay:
		if a := firstArg(b, call.Args); a != nil && a.Value.base().T != nil {
			x := a.Value
			c.later(func() {
				t := x.base().T
				if _, isVar := prune(t).(*TypeVar); !isVar && !displayable(t) && !hasError(t) {
					c.sink.errorf(CodeNotDisplayable, x.Span(), "not displayable",
						"%s cannot be converted to a string", zonk(t))
				}
			})
		}
	}
	if b.Name == "par_map" {
		res = c.parMapResult(call, res)
	}
	return res, eff | b.Effects
}

// parMapResult checks par_map's on_error argument, which must be the
// string literal "fail" or "collect" because it decides the result type:
// with "collect" each element is Ok(value) or Err(TaskError) (spec 8.6).
func (c *checker) parMapResult(call *CallExpr, res Type) Type {
	for _, a := range call.Args {
		if a.Param != 3 || a.Value == nil {
			continue
		}
		s, ok := a.Value.(*StringLit)
		if !ok || s.Interpolated() {
			c.sink.errorf(CodeBadNamedArg, a.Value.Span(), "expected a string literal",
				"on_error must be the string literal \"fail\" or \"collect\"").
				withHelp("The on_error mode decides the result type, so it must be written literally.")
			return res
		}
		v := ""
		for _, p := range s.Parts {
			v += p.Lit
		}
		switch v {
		case "fail":
		case "collect":
			if n, ok := prune(res).(*NamedType); ok && len(n.Args) == 1 {
				elem := &NamedType{Def: defResult, Args: []Type{n.Args[0], &NamedType{Def: defTaskError}}}
				return &NamedType{Def: n.Def, Args: []Type{elem}}
			}
		default:
			c.sink.errorf(CodeBadNamedArg, a.Value.Span(), "expected \"fail\" or \"collect\"",
				"on_error must be \"fail\" or \"collect\", found %q", v)
		}
	}
	return res
}

func overloadHeads(b *Builtin) string {
	var names []string
	seen := map[string]bool{}
	for _, s := range b.Overloads {
		if len(s.Params) == 0 {
			continue
		}
		n := s.Params[0].Type.String()
		if nt, ok := s.Params[0].Type.(*NamedType); ok {
			n = nt.Def.Name
		}
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	if len(names) <= 1 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// flatMapResult computes flat_map's result: the function returns List<B>
// or Stream<B>; the result is a Stream when the input or the function's
// result is a Stream, otherwise a List.
func (c *checker) flatMapResult(call *CallExpr, sig *Signature) Type {
	var xsT, fT Type
	for _, a := range call.Args {
		switch a.Param {
		case 0:
			xsT = a.Value.base().T
		case 1:
			fT = a.Value.base().T
		}
	}
	if xsT == nil || fT == nil {
		return TError
	}
	ft, ok := prune(fT).(*FuncType)
	if !ok {
		return TError
	}
	r := prune(ft.Result)
	var elem Type
	stream := namedOf(xsT, defStream) != nil
	switch {
	case namedOf(r, defList) != nil:
		elem = namedOf(r, defList).Args[0]
	case namedOf(r, defStream) != nil:
		elem = namedOf(r, defStream).Args[0]
		stream = true
	default:
		if _, isVar := r.(*TypeVar); isVar {
			// Default to a List-returning function.
			elem = c.newVar()
			unify(r, &NamedType{Def: defList, Args: []Type{elem}})
		} else {
			if !hasError(r) {
				c.sink.errorf(CodeTypeMismatch, call.Args[1].Value.Span(), "expected a List or Stream result",
					"flat_map expects a function returning List or Stream but found %s", zonk(ft))
			}
			return TError
		}
	}
	if stream {
		return &NamedType{Def: defStream, Args: []Type{elem}}
	}
	return &NamedType{Def: defList, Args: []Type{elem}}
}

func (c *checker) checkScopeTarget(e Expr, callee string) {
	t := prune(e.base().T)
	if hasError(t) {
		return
	}
	if v, isVar := t.(*TypeVar); isVar {
		v.Ref = TError
		c.sink.errorf(CodeCannotInfer, e.Span(), "type must be known here",
			"cannot infer the type of the scope target").withHelp("Add a type annotation.")
		return
	}
	if n, ok := t.(*NamedType); ok {
		for _, d := range scopeTargets {
			if n.Def == d {
				return
			}
		}
	}
	c.sink.errorf(CodeTypeMismatch, e.Span(), "not a scope target",
		"%s expects a scope target (Fqdn, IpAddress, Netblock, Asn, Url, Host or Entity) but found %s", callee, zonk(t))
}

// agentParams are the arguments of an agent expression.
var agentParams = []string{"goal", "context", "tools", "budget"}

func (c *checker) inferAgent(e *AgentExpr, hint Type) (Type, EffectSet) {
	c.recordOp(OpAgentCall)
	var eff EffectSet
	seen := map[string]bool{}
	for _, a := range e.Args {
		if a.Value == nil {
			continue
		}
		if a.Name == nil {
			c.infer(a.Value, nil)
			eff |= a.Value.Effects()
			continue
		}
		n := a.Name.Name
		if seen[n] {
			c.sink.errorf(CodeBadNamedArg, a.Name.Span(), "given twice", "argument `%s` is given more than once", n)
		}
		seen[n] = true
		switch n {
		case "goal":
			a.Param = 0
			t := c.infer(a.Value, TString)
			c.checkArg(a.Value, t, TString, "agent")
		case "context":
			a.Param = 1
			t := c.infer(a.Value, nil)
			if containsInScope(t) {
				c.sink.errorf(CodeScopeProofMisuse, a.Value.Span(), "this value is a scope proof",
					"%s cannot be passed to an agent", scopeProofName(t))
			}
		case "tools":
			a.Param = 2
			e.Tools = c.agentTools(a.Value)
		case "budget":
			a.Param = 3
			bt := &NamedType{Def: defBudget}
			t := c.infer(a.Value, bt)
			c.checkArg(a.Value, t, bt, "agent")
		default:
			d := c.sink.errorf(CodeBadNamedArg, a.Name.Span(), "unknown parameter",
				"agent has no parameter named `%s`", n)
			if sug := suggest(n, agentParams); sug != "" {
				d.withHelp("Did you mean `%s`?", sug)
			} else {
				d.withHelp("Parameters: goal, context, tools, budget.")
			}
			c.infer(a.Value, nil)
		}
		eff |= a.Value.Effects()
	}
	if !seen["goal"] {
		c.sink.errorf(CodeArity, e.Span(), "missing goal", "agent expression is missing argument `goal`")
	}
	elem := Type(c.newVar())
	if n := namedOf(hint, defPlan); n != nil {
		elem = n.Args[0]
	}
	return &NamedType{Def: defPlan, Args: []Type{elem}}, eff | EffectSet(EffLLM)
}

// agentTools resolves the tools list of an agent expression.
func (c *checker) agentTools(v Expr) []*Tool {
	l, ok := v.(*ListExpr)
	if !ok {
		c.infer(v, nil)
		c.sink.errorf(CodeUnknownTool, v.Span(), "expected a list of tools",
			"the tools argument must be a list of tool names, as in [resolve, httpx]")
		return nil
	}
	var out []*Tool
	for _, el := range l.Elems {
		id, ok := el.(*Ident)
		if !ok {
			c.infer(el, nil)
			c.sink.errorf(CodeUnknownTool, el.Span(), "not a tool", "expected a tool name")
			continue
		}
		ref, found := c.resolveName(id.Name)
		if !found || ref.Kind != RefTool {
			d := c.sink.errorf(CodeUnknownTool, id.Span(), "not a tool", "unknown tool `%s`", id.Name)
			var names []string
			for _, t := range c.tools {
				names = append(names, t.Name)
			}
			if sug := suggest(id.Name, names); sug != "" {
				d.withHelp("Did you mean `%s`?", sug)
			}
			id.T = TError
			continue
		}
		id.Ref = ref
		id.T = ref.Tool.funcType()
		out = append(out, ref.Tool)
	}
	l.T = &NamedType{Def: defList, Args: []Type{TError}}
	return out
}
