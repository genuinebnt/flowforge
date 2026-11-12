package dsl

import (
	"sort"
	"strings"
	"unicode"
)

// This file holds the checker's environment: item collection, the type
// and value namespaces, lexical scopes, frames and captures, and the
// resolution of written types.

// itemSym is an entry of the item-level value namespace.
type itemSym struct {
	ref  Ref
	decl Span   // where it was declared (zero for builtins)
	file string // file of decl
}

// scope is one lexical block of local bindings.
type scope struct {
	parent *scope
	vars   map[string]*Binding
}

func (s *scope) lookup(name string) *Binding {
	for ; s != nil; s = s.parent {
		if b, ok := s.vars[name]; ok {
			return b
		}
	}
	return nil
}

// fnCtx is the function-like context being checked: an item body, a
// lambda or a compensation.
type fnCtx struct {
	parent *fnCtx
	frame  *Frame
	lambda *Lambda // nil for items
	// info is the enclosing item's FuncInfo (shared by nested lambdas).
	info *FuncInfo
	// itemFn is the enclosing fn declaration (nil in workflows).
	isWorkflow bool
	// calls collects user fns called from the item (for summaries).
	calls *[]*FnDecl
}

func (c *checker) newScope() *scope {
	return &scope{parent: c.scope, vars: map[string]*Binding{}}
}

// bind declares a binding in the current scope, reporting E0202 for a
// redefinition in the same scope.
func (c *checker) bind(name *Ident, kind BindingKind, t Type) *Binding {
	b := &Binding{ID: c.nextBinding, Name: name.Name, Kind: kind, Type: t, Decl: name.Span()}
	c.nextBinding++
	if c.fn != nil {
		b.Frame = c.fn.frame
		b.Slot = len(c.fn.frame.Bindings)
		c.fn.frame.Bindings = append(c.fn.frame.Bindings, b)
	}
	if name.Name == "_" {
		return b
	}
	if prev, ok := c.scope.vars[name.Name]; ok {
		c.sink.errorf(CodeDuplicateBind, name.Span(), "redefined here",
			"`%s` is already defined in this block", name.Name).
			withSecondary(c.sink.file, prev.Decl, "first defined here").
			withHelp("Bindings are immutable; use a different name, or shadow it inside a nested block.")
	} else {
		c.scope.vars[name.Name] = b
	}
	if kind != BindIt {
		c.checkValueName(name, "binding")
	}
	return b
}

// lookupLocal resolves a local name, recording captures for lambdas.
func (c *checker) lookupLocal(name string) (Ref, bool) {
	b := c.scope.lookup(name)
	if b == nil {
		return Ref{}, false
	}
	if c.fn == nil || b.Frame == c.fn.frame {
		return Ref{Kind: RefLocal, Binding: b}, true
	}
	idx := c.capture(c.fn, b)
	if idx < 0 {
		return Ref{Kind: RefLocal, Binding: b}, true
	}
	return Ref{Kind: RefCapture, Binding: b, Capture: idx}, true
}

// capture returns b's index in ctx's lambda captures, adding it (and the
// captures of the lambdas in between) as needed.
func (c *checker) capture(ctx *fnCtx, b *Binding) int {
	l := ctx.lambda
	if l == nil {
		return -1
	}
	for i, cp := range l.Captures {
		if cp.Binding == b {
			return i
		}
	}
	outer := -1
	if ctx.parent != nil && ctx.parent.frame != b.Frame {
		outer = c.capture(ctx.parent, b)
	}
	l.Captures = append(l.Captures, &Capture{Binding: b, Outer: outer})
	return len(l.Captures) - 1
}

// ---- Item collection ----

func (c *checker) declareType(name *Ident, def *TypeDef, file string) {
	if _, ok := prelude.types[name.Name]; ok || primByName(name.Name) != nil {
		c.sinkFor(file).errorf(CodeDuplicateItem, name.Span(), "conflicts with a builtin type",
			"type `%s` is already defined by the standard library", name.Name)
		return
	}
	if prev, ok := c.typeDecls[name.Name]; ok {
		c.sinkFor(file).errorf(CodeDuplicateItem, name.Span(), "redefined here",
			"type `%s` is defined more than once", name.Name).
			withSecondary(prev.file, prev.decl, "first defined here")
		return
	}
	c.types[name.Name] = def
	c.typeDecls[name.Name] = itemSym{decl: name.Span(), file: file}
}

func (c *checker) declareValue(name *Ident, ref Ref, file string) bool {
	if prev, ok := c.values[name.Name]; ok {
		d := c.sinkFor(file).errorf(CodeDuplicateItem, name.Span(), "redefined here",
			"`%s` is defined more than once", name.Name)
		if prev.file != "" {
			d.withSecondary(prev.file, prev.decl, "first defined here")
		}
		return false
	}
	c.values[name.Name] = itemSym{ref: ref, decl: name.Span(), file: file}
	return true
}

// collect registers every item's name before any body is checked, so
// items are mutually visible in any order (spec 7.5).
func (c *checker) collect(items []Item, file string) {
	for _, it := range items {
		switch d := it.(type) {
		case *TypeDecl:
			def := &TypeDef{ID: len(builtinTypeDefs) + len(c.userTypes), Name: d.Name.Name, Kind: DefRecord, Decl: d}
			d.Def = def
			c.userTypes = append(c.userTypes, def)
			c.declareType(d.Name, def, file)
			c.checkTypeName(d.Name, "type")
		case *EnumDecl:
			def := &TypeDef{ID: len(builtinTypeDefs) + len(c.userTypes), Name: d.Name.Name, Kind: DefEnum, Decl: d}
			d.Def = def
			c.userTypes = append(c.userTypes, def)
			c.declareType(d.Name, def, file)
			c.checkTypeName(d.Name, "enum")
		case *FnDecl:
			c.declareValue(d.Name, Ref{Kind: RefFn, Fn: d}, file)
			c.checkValueName(d.Name, "function")
		case *WorkflowDecl:
			c.declareValue(d.Name, Ref{Kind: RefWorkflow, Flow: d}, file)
			c.checkValueName(d.Name, "workflow")
		case *ToolDecl:
			t := &Tool{Name: d.Name.Name, Decl: d, FromRegistry: file == registryFile}
			d.Tool = t
			if prev, ok := c.values[d.Name.Name]; ok && prev.ref.Kind == RefTool {
				// A source declaration of a registry tool: keep the
				// registry's, and require the two to agree.
				c.shadowedTools = append(c.shadowedTools, [2]*Tool{prev.ref.Tool, t})
				continue
			}
			if c.declareValue(d.Name, Ref{Kind: RefTool, Tool: t}, file) {
				c.tools = append(c.tools, t)
			}
			c.checkValueName(d.Name, "tool")
		}
	}
}

// resolveTypeBodies fills record fields and enum variants.
func (c *checker) resolveTypeBodies(items []Item) {
	for _, it := range items {
		switch d := it.(type) {
		case *TypeDecl:
			def := d.Def
			c.typeParams = c.genericParams(d.Generics, &def.Params)
			seen := map[string]Span{}
			for i, f := range d.Fields {
				t := c.resolveType(f.TypeX)
				if prev, dup := seen[f.Name.Name]; dup {
					c.sink.errorf(CodeDuplicateItem, f.Name.Span(), "redefined here",
						"field `%s` is declared more than once", f.Name.Name).
						withSecondary(c.sink.file, prev, "first declared here")
				}
				seen[f.Name.Name] = f.Name.Span()
				def.Fields = append(def.Fields, &Field{Name: f.Name.Name, Type: t, Index: i})
				c.checkValueName(f.Name, "field")
				if containsInScope(t) {
					c.scopeProofStored(f.TypeX.Span(), t, d.Name, "record field", "this type is a scope proof")
				}
			}
			c.typeParams = nil
		case *EnumDecl:
			def := d.Def
			c.typeParams = c.genericParams(d.Generics, &def.Params)
			seen := map[string]Span{}
			for i, v := range d.Variants {
				va := &Variant{Def: def, Name: v.Name.Name, Index: i, Decl: v}
				for _, px := range v.Payload {
					t := c.resolveType(px)
					va.Payload = append(va.Payload, t)
					if containsInScope(t) {
						c.scopeProofStored(px.Span(), t, d.Name, "enum variant", "this type is a scope proof")
					}
				}
				if prev, dup := seen[v.Name.Name]; dup {
					c.sink.errorf(CodeDuplicateItem, v.Name.Span(), "redefined here",
						"variant `%s` is declared more than once", v.Name.Name).
						withSecondary(c.sink.file, prev, "first declared here")
				}
				seen[v.Name.Name] = v.Name.Span()
				def.Variants = append(def.Variants, va)
				c.checkTypeName(v.Name, "variant")
			}
			c.typeParams = nil
		}
	}
}

// scopeProofStored reports E0302 for a proof stored in a user type.
func (c *checker) scopeProofStored(sp Span, t Type, owner *Ident, where, label string) {
	inner := scopeProofInner(t)
	c.sink.errorf(CodeScopeProofMisuse, sp, label,
		"%s cannot be stored in a %s", scopeProofName(t), where).
		withSecondary(c.sink.file, owner.Span(), strings.SplitN(where, " ", 2)[0]+" declared here").
		withHelp("Store the %s and call scope.check again where it is used.", inner)
}

// scopeProofName returns the InScope<...> type mentioned in t.
func scopeProofName(t Type) string {
	if n := findInScope(t); n != nil {
		return n.String()
	}
	return "InScope<T>"
}

func scopeProofInner(t Type) string {
	if n := findInScope(t); n != nil && len(n.Args) == 1 {
		return zonk(n.Args[0]).String()
	}
	return "value"
}

func findInScope(t Type) *NamedType {
	switch tt := prune(t).(type) {
	case *NamedType:
		if tt.Def == defInScope {
			return &NamedType{Def: defInScope, Args: []Type{zonk(tt.Args[0])}}
		}
		for _, a := range tt.Args {
			if n := findInScope(a); n != nil {
				return n
			}
		}
	case *TupleType:
		for _, a := range tt.Elems {
			if n := findInScope(a); n != nil {
				return n
			}
		}
	case *FuncType:
		for _, a := range tt.Params {
			if n := findInScope(a); n != nil {
				return n
			}
		}
		return findInScope(tt.Result)
	}
	return nil
}

// genericParams creates type parameters for a declaration.
func (c *checker) genericParams(gs []*Ident, out *[]*TypeParam) map[string]*TypeParam {
	m := map[string]*TypeParam{}
	for _, g := range gs {
		tp := &TypeParam{Name: g.Name, ID: c.nextParam}
		c.nextParam++
		if _, dup := m[g.Name]; dup {
			c.sink.errorf(CodeDuplicateBind, g.Span(), "redefined here",
				"type parameter `%s` is declared more than once", g.Name)
		}
		m[g.Name] = tp
		*out = append(*out, tp)
		c.checkTypeName(g, "type parameter")
	}
	return m
}

// resolveSignatures computes fn, workflow and tool signatures.
func (c *checker) resolveSignatures(items []Item, file string) {
	c.sink = c.sinkFor(file)
	defer func() { c.sink = c.mainSink }()
	for _, it := range items {
		switch d := it.(type) {
		case *FnDecl:
			info := &FuncInfo{Name: d.Name.Name}
			c.typeParams = c.genericParams(d.Generics, &info.Generics)
			for _, p := range d.Params {
				info.Params = append(info.Params, c.resolveType(p.TypeX))
			}
			info.Result = TUnit
			if d.Result != nil {
				info.Result = c.resolveType(d.Result)
			}
			if d.Effects != nil {
				info.Declared = c.effectClause(d.Effects)
				info.HasDeclared = true
			}
			c.typeParams = nil
			d.Func = info
		case *WorkflowDecl:
			info := &FuncInfo{Name: d.Name.Name}
			for _, p := range d.Params {
				t := c.resolveType(p.TypeX)
				info.Params = append(info.Params, t)
				if containsInScope(t) {
					c.sink.errorf(CodeScopeProofMisuse, p.TypeX.Span(), "this type is a scope proof",
						"%s cannot be passed into a workflow", scopeProofName(t)).
						withHelp("Pass the %s and call scope.check inside the workflow.", scopeProofInner(t))
				}
			}
			info.Result = TUnit
			if d.Result != nil {
				info.Result = c.resolveType(d.Result)
				if containsInScope(info.Result) {
					c.sink.errorf(CodeScopeProofMisuse, d.Result.Span(), "this type is a scope proof",
						"%s cannot be returned from a workflow", scopeProofName(info.Result)).
						withHelp("Return the %s; proofs do not cross runs.", scopeProofInner(info.Result))
				}
			}
			if d.Effects != nil {
				info.Declared = c.effectClause(d.Effects)
				info.HasDeclared = true
			}
			d.Func = info
		case *ToolDecl:
			t := d.Tool
			for _, p := range d.Params {
				t.ParamNames = append(t.ParamNames, p.Name.Name)
				t.Params = append(t.Params, c.resolveType(p.TypeX))
				c.checkValueName(p.Name, "parameter")
			}
			t.Result = TUnit
			if d.Result != nil {
				t.Result = c.resolveType(d.Result)
				if containsInScope(t.Result) {
					c.sink.errorf(CodeScopeProofMisuse, d.Result.Span(), "this type is a scope proof",
						"tool `%s` cannot return %s", t.Name, scopeProofName(t.Result)).
						withHelp("Only scope.check produces scope proofs.")
				}
			}
			if d.Effects != nil {
				t.Effects = c.effectClause(d.Effects)
			}
		}
	}
}

// effectClause validates effect names (E0405).
func (c *checker) effectClause(ec *EffectClause) EffectSet {
	var s EffectSet
	for _, n := range ec.Names {
		e, ok := ParseEffect(n.Name)
		if !ok {
			d := c.sink.errorf(CodeUnknownEffect, n.Span(), "unknown effect",
				"unknown effect `%s`", n.Name)
			if sug := suggest(n.Name, allEffectNames()); sug != "" {
				d.withHelp("Did you mean `%s`? Effects are read, passive, active, intrusive, write, financial_write, llm and time.", sug)
			} else {
				d.withHelp("Effects are read, passive, active, intrusive, write, financial_write, llm and time.")
			}
			continue
		}
		s |= EffectSet(e)
	}
	ec.Set = s
	return s
}

func allEffectNames() []string {
	var out []string
	for _, e := range effectNames {
		out = append(out, e.name)
	}
	return out
}

// checkRegistryAgreement compares source tool declarations with the
// registry's (E0203 when they differ).
func (c *checker) checkRegistryAgreement() {
	for _, pair := range c.shadowedTools {
		reg, src := pair[0], pair[1]
		// Resolve the source declaration's signature for comparison.
		var params []Type
		for _, p := range src.Decl.Params {
			params = append(params, c.resolveType(p.TypeX))
		}
		res := Type(TUnit)
		if src.Decl.Result != nil {
			res = c.resolveType(src.Decl.Result)
		}
		var eff EffectSet
		if src.Decl.Effects != nil {
			eff = c.effectClause(src.Decl.Effects)
		}
		srcT := &FuncType{Params: params, Result: res}
		regT := &FuncType{Params: reg.Params, Result: reg.Result}
		if srcT.String() != regT.String() || eff != reg.Effects {
			c.sink.errorf(CodeUnknownTool, src.Decl.Name.Span(), "differs from the registry",
				"tool `%s` is declared as %s ! %s but the registry has %s ! %s",
				src.Name, srcT, eff, regT, reg.Effects).
				withHelp("Remove the declaration or regenerate it from the tool registry.")
		}
		src.Decl.Tool = reg
	}
}

// resolveUses processes use declarations.
func (c *checker) resolveUses(items []Item) {
	for _, it := range items {
		u, ok := it.(*UseDecl)
		if !ok {
			continue
		}
		last := u.Path[len(u.Path)-1]
		switch len(u.Path) {
		case 1:
			if _, ok := c.values[last.Name]; ok {
				continue
			}
			if c.lookupTypeDef(last.Name) != nil {
				continue
			}
			c.sink.errorf(CodeUnknownName, last.Span(), "not found",
				"cannot find `%s` to use", last.Name)
		case 2:
			def := c.lookupTypeDef(u.Path[0].Name)
			if def == nil {
				c.sink.errorf(CodeUnknownType, u.Path[0].Span(), "not found",
					"unknown type `%s`", u.Path[0].Name)
				continue
			}
			v := def.VariantByName(last.Name)
			if v == nil {
				c.sink.errorf(CodeUnknownVariant, last.Span(), "not found",
					"`%s` has no variant `%s`", def.Name, last.Name)
				continue
			}
			c.declareValue(last, Ref{Kind: RefVariant, Variant: v}, c.sink.file)
		default:
			c.sink.errorf(CodeUnknownName, u.Path[0].Span().Join(last.Span()), "not found",
				"cannot resolve path `%s`", pathString(u.Path))
		}
	}
}

func pathString(p []*Ident) string {
	parts := make([]string, len(p))
	for i, id := range p {
		parts[i] = id.Name
	}
	return strings.Join(parts, "::")
}

func (c *checker) lookupTypeDef(name string) *TypeDef {
	if d, ok := c.types[name]; ok {
		return d
	}
	return prelude.types[name]
}

// ---- Written types ----

// mapKeyOK reports whether t may be a Map key (String, Int or a domain
// scalar; type parameters are allowed in generic declarations).
func mapKeyOK(t Type) bool {
	switch tt := prune(t).(type) {
	case *PrimType:
		return tt.Kind == PrimString || tt.Kind == PrimInt || tt.Kind == PrimError
	case *NamedType:
		return tt.Def.Kind == DefScalar
	case *TypeParam, *TypeVar:
		return true
	}
	return false
}

// resolveType converts a written type to a Type, reporting unknown names
// (E0204) and arity errors (E0303).
func (c *checker) resolveType(tx TypeExpr) Type {
	if tx == nil {
		return TError
	}
	t := c.resolveTypeInner(tx)
	switch b := tx.(type) {
	case *NamedTypeExpr:
		b.Res = t
	case *TupleTypeExpr:
		b.Res = t
	case *FuncTypeExpr:
		b.Res = t
	}
	return t
}

func (c *checker) resolveTypeInner(tx TypeExpr) Type {
	switch t := tx.(type) {
	case *NamedTypeExpr:
		name := t.Name.Name
		if name == "_" {
			return TError
		}
		args := make([]Type, len(t.Args))
		for i, a := range t.Args {
			args[i] = c.resolveType(a)
		}
		if tp, ok := c.typeParams[name]; ok {
			if len(args) > 0 {
				c.sink.errorf(CodeArity, t.Span(), "no type arguments expected",
					"type parameter `%s` takes no type arguments", name)
			}
			return tp
		}
		if p := primByName(name); p != nil {
			if len(args) > 0 {
				c.sink.errorf(CodeArity, t.Span(), "no type arguments expected",
					"type `%s` takes no type arguments", name)
			}
			return p
		}
		def := c.lookupTypeDef(name)
		if def == nil {
			d := c.sink.errorf(CodeUnknownType, t.Name.Span(), "not found", "unknown type `%s`", name)
			if sug := suggest(name, c.typeNames()); sug != "" {
				d.withHelp("Did you mean `%s`?", sug)
			} else {
				d.withHelp("Declare it with `type %s = { ... }` or `enum %s { ... }`.", name, name)
			}
			return TError
		}
		if len(args) != len(def.Params) {
			c.sink.errorf(CodeArity, t.Span(), "wrong number of type arguments",
				"type `%s` expects %d type argument(s) but %d were given", name, len(def.Params), len(args))
			return TError
		}
		nt := &NamedType{Def: def, Args: args}
		if def == defMap && !mapKeyOK(args[0]) {
			c.sink.errorf(CodeBadMapKey, t.Args[0].Span(), "not a valid key type",
				"`%s` cannot be a Map key", args[0]).
				withHelp("Map keys are String, Int or a domain scalar such as Fqdn.")
		}
		if def == defPlan && containsInScope(args[0]) {
			c.sink.errorf(CodeScopeProofMisuse, t.Args[0].Span(), "this type is a scope proof",
				"a plan cannot produce %s", scopeProofName(args[0]))
		}
		return nt
	case *TupleTypeExpr:
		if len(t.Elems) == 0 {
			return TUnit
		}
		tt := &TupleType{}
		for _, e := range t.Elems {
			tt.Elems = append(tt.Elems, c.resolveType(e))
		}
		return tt
	case *FuncTypeExpr:
		ft := &FuncType{}
		for _, e := range t.Params {
			ft.Params = append(ft.Params, c.resolveType(e))
		}
		ft.Result = c.resolveType(t.Result)
		return ft
	}
	return TError
}

func (c *checker) typeNames() []string {
	var out []string
	for n := range c.types {
		out = append(out, n)
	}
	for n := range prelude.types {
		out = append(out, n)
	}
	out = append(out, "Int", "Float", "Bool", "String", "Duration", "Time", "Unit")
	sort.Strings(out)
	return out
}

func (c *checker) valueNames() []string {
	var out []string
	for n := range c.values {
		out = append(out, n)
	}
	for n := range prelude.builtins {
		if !strings.Contains(n, ".") {
			out = append(out, n)
		}
	}
	for n := range prelude.variants {
		out = append(out, n)
	}
	for s := c.scope; s != nil; s = s.parent {
		for n := range s.vars {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// ---- Naming (W0101) ----

func isUpperCamel(s string) bool {
	if s == "" || !unicode.IsUpper(rune(s[0])) {
		return false
	}
	return !strings.Contains(s, "_")
}

func isSnake(s string) bool {
	t := strings.TrimLeft(s, "_")
	if t == "" {
		return true
	}
	for _, r := range t {
		if unicode.IsUpper(r) {
			return false
		}
	}
	return true
}

func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 && s[i-1] != '_' {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func toCamel(s string) string {
	var b strings.Builder
	up := true
	for _, r := range s {
		if r == '_' {
			up = true
			continue
		}
		if up {
			b.WriteRune(unicode.ToUpper(r))
			up = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (c *checker) checkTypeName(id *Ident, what string) {
	if isUpperCamel(id.Name) {
		return
	}
	c.sink.warnf(WarnNaming, id.Span(), "not UpperCamelCase",
		"%s `%s` should have an UpperCamelCase name", what, id.Name).
		withHelp("Rename it to `%s`.", toCamel(id.Name))
}

func (c *checker) checkValueName(id *Ident, what string) {
	if isSnake(id.Name) {
		return
	}
	c.sink.warnf(WarnNaming, id.Span(), "not snake_case",
		"%s `%s` should have a snake_case name", what, id.Name).
		withHelp("Rename it to `%s`.", toSnake(id.Name))
}

// ---- Suggestions ----

// suggest returns the candidate closest to name by edit distance, or "".
func suggest(name string, candidates []string) string {
	best, bestD := "", 1<<30
	limit := len(name)/3 + 1
	for _, cand := range candidates {
		if cand == name {
			continue
		}
		d := editDistance(name, cand, limit)
		if d <= limit && d < bestD {
			best, bestD = cand, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance, giving up above limit.
func editDistance(a, b string, limit int) int {
	if abs(len(a)-len(b)) > limit {
		return limit + 1
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
