package dsl

import (
	"strconv"
	"strings"
)

// This file checks patterns, binds pattern variables, and implements the
// usefulness algorithm (Maranget, "Warnings for pattern matching") used
// for exhaustiveness (E0501) and unreachable arms (W0501).

// bindPattern checks p against t and binds its variables in the current
// scope. Duplicate names within one pattern are E0202.
func (c *checker) bindPattern(p Pattern, t Type, kind BindingKind) {
	seen := map[string]Span{}
	c.pattern(p, t, kind, seen)
}

func (c *checker) patBind(name *Ident, t Type, kind BindingKind, seen map[string]Span) *Binding {
	if prev, dup := seen[name.Name]; dup && name.Name != "_" {
		c.sink.errorf(CodeDuplicateBind, name.Span(), "bound twice",
			"`%s` is bound more than once in this pattern", name.Name).
			withSecondary(c.sink.file, prev, "first bound here")
	}
	seen[name.Name] = name.Span()
	return c.bind(name, kind, t)
}

func (c *checker) pattern(p Pattern, t Type, kind BindingKind, seen map[string]Span) {
	b := p.pat()
	b.T = t
	switch p := p.(type) {
	case *WildcardPat:
	case *BindPat:
		// A bare name that is a unit variant in scope (None, or one brought
		// in by use) matches that variant; otherwise it binds.
		if c.scope.lookup(p.Name.Name) == nil {
			if ref, ok := c.resolveName(p.Name.Name); ok && ref.Kind == RefVariant && len(ref.Variant.Payload) == 0 {
				p.Variant = ref.Variant
				c.unifyVariant(p.Sp, ref.Variant, t)
				return
			}
		}
		p.Binding = c.patBind(p.Name, t, kind, seen)
	case *LitPat:
		lt := c.infer(p.Lit, nil)
		if !unify(lt, t) && !hasError(t) {
			c.sink.errorf(CodeBadLitPattern, p.Sp, "expected "+zonk(t).String(),
				"literal pattern of type %s cannot match %s", lt, zonk(t))
		}
	case *VariantPat:
		ref, ok := c.resolveVariantPath(p.Path)
		if !ok {
			for _, a := range p.Args {
				c.pattern(a, TError, kind, seen)
			}
			return
		}
		v := ref.Variant
		p.Variant = v
		m := c.unifyVariant(p.Sp, v, t)
		if len(p.Args) != len(v.Payload) {
			if len(v.Payload) > 0 || p.HasParens {
				c.sink.errorf(CodePatternArity, p.Sp, "wrong number of fields",
					"variant `%s` has %d field(s) but the pattern has %d", v.QualifiedName(), len(v.Payload), len(p.Args))
			}
		}
		for i, a := range p.Args {
			at := Type(TError)
			if i < len(v.Payload) {
				at = subst(v.Payload[i], m)
			}
			c.pattern(a, at, kind, seen)
		}
	case *TuplePat:
		if len(p.Elems) == 0 {
			if !unify(t, TUnit) && !hasError(t) {
				c.sink.errorf(CodePatternMisuse, p.Sp, "expected "+zonk(t).String(),
					"pattern () cannot match %s", zonk(t))
			}
			return
		}
		elems := make([]Type, len(p.Elems))
		for i := range elems {
			elems[i] = c.newVar()
		}
		tt := &TupleType{Elems: elems}
		if !unify(t, tt) && !hasError(t) {
			c.sink.errorf(CodePatternMisuse, p.Sp, "expected "+zonk(t).String(),
				"a %d-tuple pattern cannot match %s", len(p.Elems), zonk(t))
			for i := range elems {
				elems[i] = TError
			}
		}
		for i, e := range p.Elems {
			c.pattern(e, elems[i], kind, seen)
		}
	case *RecordPat:
		if len(p.Path) != 1 {
			c.sink.errorf(CodeBadRecordPat, p.Sp, "not a record", "`%s` is not a record type", pathString(p.Path))
			return
		}
		def := c.lookupTypeDef(p.Path[0].Name)
		if def == nil || def.Kind != DefRecord {
			c.sink.errorf(CodeBadRecordPat, p.Path[0].Span(), "not a record",
				"`%s` is not a record type", p.Path[0].Name)
			for _, f := range p.Fields {
				if f.Pat != nil {
					c.pattern(f.Pat, TError, kind, seen)
				} else {
					f.ShortBinding = c.patBind(f.Name, TError, kind, seen)
				}
			}
			return
		}
		p.Def = def
		m := c.instantiate(def.Params)
		self := &NamedType{Def: def, Args: paramArgs(def.Params, m)}
		if !unify(t, self) && !hasError(t) {
			c.sink.errorf(CodePatternMisuse, p.Sp, "expected "+zonk(t).String(),
				"pattern of type %s cannot match %s", def.Name, zonk(t))
		}
		dup := map[string]bool{}
		for _, f := range p.Fields {
			fd := def.FieldByName(f.Name.Name)
			ft := Type(TError)
			if fd == nil {
				c.sink.errorf(CodeBadField, f.Name.Span(), "unknown field",
					"record `%s` has no field `%s`", def.Name, f.Name.Name)
			} else {
				if dup[fd.Name] {
					c.sink.errorf(CodeBadRecordPat, f.Name.Span(), "listed twice",
						"field `%s` is listed more than once", fd.Name)
				}
				dup[fd.Name] = true
				f.Index = fd.Index
				ft = subst(fd.Type, m)
			}
			if f.Pat != nil {
				c.pattern(f.Pat, ft, kind, seen)
			} else {
				f.ShortBinding = c.patBind(f.Name, ft, kind, seen)
			}
		}
	}
}

func (c *checker) resolveVariantPath(path []*Ident) (Ref, bool) {
	if len(path) == 1 {
		ref, ok := c.resolveName(path[0].Name)
		if ok && ref.Kind == RefVariant {
			return ref, true
		}
		d := c.sink.errorf(CodeNoSuchVariant, path[0].Span(), "not a variant",
			"`%s` is not an enum variant", path[0].Name)
		var names []string
		for n := range prelude.variants {
			names = append(names, n)
		}
		for n, s := range c.values {
			if s.ref.Kind == RefVariant {
				names = append(names, n)
			}
		}
		if sug := suggest(path[0].Name, names); sug != "" {
			d.withHelp("Did you mean `%s`?", sug)
		} else {
			d.withHelp("Qualify variants with their enum, as in Severity::High.")
		}
		return Ref{}, false
	}
	return c.resolvePath(path)
}

// unifyVariant unifies t with an instance of v's enum and returns the
// instantiation.
func (c *checker) unifyVariant(sp Span, v *Variant, t Type) map[*TypeParam]Type {
	m := c.instantiate(v.Def.Params)
	self := &NamedType{Def: v.Def, Args: paramArgs(v.Def.Params, m)}
	if !unify(t, self) && !hasError(t) {
		c.sink.errorf(CodePatternMisuse, sp, "expected "+zonk(t).String(),
			"variant `%s` cannot match %s", v.QualifiedName(), zonk(t))
	}
	return m
}

// ---- Space of patterns ----

// spat is a simplified pattern: a wildcard, or a constructor with
// sub-patterns. Literals are constructors of infinite types.
type spat struct {
	wild bool
	ctor ctorKey
	args []*spat
}

// ctorKey identifies a constructor: a variant index, a Bool value, the
// single constructor of tuples, records and unit, or a literal value.
type ctorKey struct {
	kind  int // ctorVariant, ctorBool, ctorSingle, ctorLit
	index int
	lit   string
}

const (
	ctorVariant = iota
	ctorBool
	ctorSingle
	ctorLit
)

var wildPat = &spat{wild: true}

// simplify converts a checked pattern.
func simplify(p Pattern) *spat {
	switch p := p.(type) {
	case *WildcardPat:
		return wildPat
	case *BindPat:
		if p.Variant != nil {
			return &spat{ctor: ctorKey{kind: ctorVariant, index: p.Variant.Index}}
		}
		return wildPat
	case *LitPat:
		switch l := p.Lit.(type) {
		case *BoolLit:
			idx := 0
			if l.Value {
				idx = 1
			}
			return &spat{ctor: ctorKey{kind: ctorBool, index: idx}}
		case *IntLit:
			return &spat{ctor: ctorKey{kind: ctorLit, lit: "i" + strconv.FormatInt(l.Value, 10)}}
		case *FloatLit:
			return &spat{ctor: ctorKey{kind: ctorLit, lit: "f" + strconv.FormatFloat(l.Value, 'g', -1, 64)}}
		case *DurationLit:
			return &spat{ctor: ctorKey{kind: ctorLit, lit: "d" + strconv.FormatInt(l.Nanos, 10)}}
		case *StringLit:
			var b strings.Builder
			for _, part := range l.Parts {
				b.WriteString(part.Lit)
			}
			return &spat{ctor: ctorKey{kind: ctorLit, lit: "s" + b.String()}}
		}
		return wildPat
	case *VariantPat:
		if p.Variant == nil {
			return wildPat
		}
		sp := &spat{ctor: ctorKey{kind: ctorVariant, index: p.Variant.Index}}
		for i := range p.Variant.Payload {
			if i < len(p.Args) {
				sp.args = append(sp.args, simplify(p.Args[i]))
			} else {
				sp.args = append(sp.args, wildPat)
			}
		}
		return sp
	case *TuplePat:
		sp := &spat{ctor: ctorKey{kind: ctorSingle}}
		for _, e := range p.Elems {
			sp.args = append(sp.args, simplify(e))
		}
		return sp
	case *RecordPat:
		if p.Def == nil {
			return wildPat
		}
		sp := &spat{ctor: ctorKey{kind: ctorSingle}, args: make([]*spat, len(p.Def.Fields))}
		for i := range sp.args {
			sp.args[i] = wildPat
		}
		for _, f := range p.Fields {
			if f.Index < 0 || f.Index >= len(sp.args) {
				continue
			}
			if f.Pat != nil {
				sp.args[f.Index] = simplify(f.Pat)
			}
		}
		return sp
	}
	return wildPat
}

// typeCtors returns the constructors of t with their argument types, and
// whether the set is finite (enumerable).
func typeCtors(t Type) ([]ctorKey, [][]Type, bool) {
	switch tt := prune(t).(type) {
	case *NamedType:
		switch tt.Def.Kind {
		case DefEnum:
			m := map[*TypeParam]Type{}
			for i, p := range tt.Def.Params {
				if i < len(tt.Args) {
					m[p] = tt.Args[i]
				}
			}
			keys := make([]ctorKey, len(tt.Def.Variants))
			args := make([][]Type, len(tt.Def.Variants))
			for i, v := range tt.Def.Variants {
				keys[i] = ctorKey{kind: ctorVariant, index: v.Index}
				for _, pt := range v.Payload {
					args[i] = append(args[i], subst(pt, m))
				}
			}
			return keys, args, true
		case DefRecord:
			m := map[*TypeParam]Type{}
			for i, p := range tt.Def.Params {
				if i < len(tt.Args) {
					m[p] = tt.Args[i]
				}
			}
			var fts []Type
			for _, f := range tt.Def.Fields {
				fts = append(fts, subst(f.Type, m))
			}
			return []ctorKey{{kind: ctorSingle}}, [][]Type{fts}, true
		}
	case *TupleType:
		return []ctorKey{{kind: ctorSingle}}, [][]Type{tt.Elems}, true
	case *PrimType:
		switch tt.Kind {
		case PrimBool:
			return []ctorKey{{kind: ctorBool, index: 0}, {kind: ctorBool, index: 1}}, [][]Type{nil, nil}, true
		case PrimUnit:
			return []ctorKey{{kind: ctorSingle}}, [][]Type{nil}, true
		}
	}
	return nil, nil, false
}

// usefulness computes whether vector q is useful with respect to matrix
// rows, returning a witness (a vector of patterns q matches but no row
// does) when it is.
type usefulness struct {
	budget int
}

const usefulnessBudget = 200000

func (u *usefulness) useful(rows [][]*spat, q []*spat, types []Type) ([]*spat, bool) {
	u.budget--
	if u.budget < 0 {
		// Give up conservatively: treat as not useful (no diagnostic).
		return nil, false
	}
	if len(q) == 0 {
		if len(rows) == 0 {
			return []*spat{}, true
		}
		return nil, false
	}
	head := q[0]
	ctors, argTypes, finite := typeCtors(types[0])
	if !head.wild {
		at := argTypesFor(head.ctor, ctors, argTypes, len(head.args))
		w, ok := u.useful(specialize(rows, head.ctor, len(head.args)), append(append([]*spat{}, head.args...), q[1:]...), append(at, types[1:]...))
		if !ok {
			return nil, false
		}
		return rebuild(head.ctor, len(head.args), w), true
	}
	// Wildcard head: which constructors appear in the first column?
	present := map[ctorKey]bool{}
	for _, r := range rows {
		if !r[0].wild {
			present[r[0].ctor] = true
		}
	}
	complete := finite && len(ctors) > 0
	if complete {
		for _, k := range ctors {
			if !present[k] {
				complete = false
				break
			}
		}
	}
	if complete {
		for i, k := range ctors {
			n := len(argTypes[i])
			sub := make([]*spat, n)
			for j := range sub {
				sub[j] = wildPat
			}
			w, ok := u.useful(specialize(rows, k, n), append(sub, q[1:]...), append(append([]Type{}, argTypes[i]...), types[1:]...))
			if ok {
				return rebuild(k, n, w), true
			}
		}
		return nil, false
	}
	// Default matrix: rows whose head is a wildcard.
	var def [][]*spat
	for _, r := range rows {
		if r[0].wild {
			def = append(def, r[1:])
		}
	}
	w, ok := u.useful(def, q[1:], types[1:])
	if !ok {
		return nil, false
	}
	// Witness head: a missing constructor if the type is finite.
	if finite {
		for i, k := range ctors {
			if !present[k] {
				sub := make([]*spat, len(argTypes[i]))
				for j := range sub {
					sub[j] = wildPat
				}
				return append([]*spat{{ctor: k, args: sub}}, w...), true
			}
		}
	}
	return append([]*spat{wildPat}, w...), true
}

func argTypesFor(k ctorKey, ctors []ctorKey, argTypes [][]Type, n int) []Type {
	for i, c := range ctors {
		if c == k && len(argTypes[i]) == n {
			return append([]Type{}, argTypes[i]...)
		}
	}
	out := make([]Type, n)
	for i := range out {
		out[i] = TError
	}
	return out
}

// specialize keeps rows matching constructor k, expanding its arguments.
func specialize(rows [][]*spat, k ctorKey, n int) [][]*spat {
	var out [][]*spat
	for _, r := range rows {
		h := r[0]
		switch {
		case h.wild:
			nr := make([]*spat, 0, n+len(r)-1)
			for i := 0; i < n; i++ {
				nr = append(nr, wildPat)
			}
			out = append(out, append(nr, r[1:]...))
		case h.ctor == k:
			nr := make([]*spat, 0, n+len(r)-1)
			for i := 0; i < n; i++ {
				if i < len(h.args) {
					nr = append(nr, h.args[i])
				} else {
					nr = append(nr, wildPat)
				}
			}
			out = append(out, append(nr, r[1:]...))
		}
	}
	return out
}

func rebuild(k ctorKey, n int, w []*spat) []*spat {
	if len(w) < n {
		return append([]*spat{{ctor: k}}, w...)
	}
	return append([]*spat{{ctor: k, args: w[:n]}}, w[n:]...)
}

// renderWitness prints a witness pattern for messages.
func renderWitness(p *spat, t Type) string {
	if p.wild {
		return "_"
	}
	tt := prune(t)
	switch p.ctor.kind {
	case ctorBool:
		if p.ctor.index == 1 {
			return "true"
		}
		return "false"
	case ctorVariant:
		nt, ok := tt.(*NamedType)
		if !ok || p.ctor.index >= len(nt.Def.Variants) {
			return "_"
		}
		v := nt.Def.Variants[p.ctor.index]
		name := v.Name
		if nt.Def != defOption && nt.Def != defResult {
			name = v.QualifiedName()
		}
		if len(v.Payload) == 0 {
			return name
		}
		_, argTypes, _ := typeCtors(nt)
		parts := make([]string, len(p.args))
		for i, a := range p.args {
			var at Type = TError
			if i < len(argTypes[p.ctor.index]) {
				at = argTypes[p.ctor.index][i]
			}
			parts[i] = renderWitness(a, at)
		}
		return name + "(" + strings.Join(parts, ", ") + ")"
	case ctorSingle:
		_, argTypes, _ := typeCtors(tt)
		parts := make([]string, len(p.args))
		for i, a := range p.args {
			var at Type = TError
			if len(argTypes) > 0 && i < len(argTypes[0]) {
				at = argTypes[0][i]
			}
			parts[i] = renderWitness(a, at)
		}
		if nt, ok := tt.(*NamedType); ok && nt.Def.Kind == DefRecord {
			return nt.Def.Name + " { .. }"
		}
		if len(parts) == 0 {
			return "()"
		}
		return "(" + strings.Join(parts, ", ") + ")"
	}
	return "_"
}

// checkMatch reports non-exhaustive matches (E0501) and unreachable arms
// (W0501). Guarded arms never count toward exhaustiveness.
func (c *checker) checkMatch(m *MatchExpr, st Type) {
	if hasError(st) {
		return
	}
	u := &usefulness{budget: usefulnessBudget}
	var rows [][]*spat
	types := []Type{st}
	broken := false
	for _, arm := range m.Arms {
		if patHasError(arm.Pat) {
			// Already reported; leave the arm out of the analysis.
			broken = true
			continue
		}
		sp := simplify(arm.Pat)
		if _, ok := u.useful(rows, []*spat{sp}, types); !ok && u.budget >= 0 {
			c.sink.warnf(WarnUnreachable, arm.Pat.Span(), "unreachable pattern",
				"this pattern can never match because earlier arms cover it").
				withHelp("Remove the arm or reorder the arms.")
		}
		if arm.Guard == nil {
			rows = append(rows, []*spat{sp})
		}
	}
	if broken {
		return
	}
	if w, ok := u.useful(rows, []*spat{wildPat}, types); ok && u.budget >= 0 {
		missing := renderWitness(w[0], st)
		c.sink.errorf(CodeNonExhaustive, m.Scrutinee.Span(), "pattern `"+missing+"` not covered",
			"non-exhaustive match: pattern `%s` is not covered", missing).
			withHelp("Add an arm for `%s`, or end with a wildcard arm `_ => ...`.", missing)
	}
}

// irrefutable reports whether p matches every value of its type.
func (c *checker) irrefutable(p Pattern) bool {
	if p.Type() == nil || hasError(p.Type()) {
		return true
	}
	u := &usefulness{budget: usefulnessBudget}
	rows := [][]*spat{{simplify(p)}}
	_, ok := u.useful(rows, []*spat{wildPat}, []Type{p.Type()})
	return !ok
}

// patHasError reports whether a checked pattern contains an error.
func patHasError(p Pattern) bool {
	bad := false
	Walk(p, func(n Node) bool {
		switch x := n.(type) {
		case *VariantPat:
			if x.Variant == nil {
				bad = true
			}
		case *RecordPat:
			if x.Def == nil {
				bad = true
			}
		}
		if pt, ok := n.(Pattern); ok && (pt.Type() == nil || hasError(pt.Type())) {
			bad = true
		}
		return !bad
	})
	return bad
}
