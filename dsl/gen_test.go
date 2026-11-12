package dsl

import (
	"fmt"
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

// This file holds test-only generators: random syntax trees (for the
// print/parse round trip) and random well-typed programs (for the
// checker property, fuzz seeds and the front-end benchmark).

// ---- Random syntax trees ----

type astGen struct {
	r     core.Rng
	depth int
}

func (g *astGen) n(k int) int { return core.IntN(g.r, k) }

func (g *astGen) chance(pct int) bool { return g.n(100) < pct }

var genNames = []string{"a", "b", "xs", "foo_bar", "_x", "n1", "value", "it"}
var genTypeNames = []string{"Int", "String", "List", "Option", "Report", "T"}
var genUpper = []string{"Report", "Severity", "Host", "Box", "P"}

func (g *astGen) name() *Ident { return &Ident{Name: genNames[g.n(len(genNames))]} }

func (g *astGen) upper() *Ident { return &Ident{Name: genUpper[g.n(len(genUpper))]} }

func (g *astGen) enter() bool {
	if g.depth > 4 {
		return false
	}
	g.depth++
	return true
}

func (g *astGen) leave() { g.depth-- }

func (g *astGen) typeX() TypeExpr {
	if !g.enter() {
		return &NamedTypeExpr{Name: &Ident{Name: "Int"}}
	}
	defer g.leave()
	switch g.n(6) {
	case 0:
		return &TupleTypeExpr{}
	case 1:
		return &TupleTypeExpr{Elems: []TypeExpr{g.typeX(), g.typeX()}}
	case 2:
		ft := &FuncTypeExpr{Result: g.typeX()}
		for i := g.n(3); i > 0; i-- {
			ft.Params = append(ft.Params, g.typeX())
		}
		return ft
	case 3:
		return &NamedTypeExpr{Name: &Ident{Name: "Map"}, Args: []TypeExpr{g.typeX(), g.typeX()}}
	case 4:
		return &NamedTypeExpr{Name: &Ident{Name: "List"}, Args: []TypeExpr{g.typeX()}}
	}
	return &NamedTypeExpr{Name: &Ident{Name: genTypeNames[g.n(len(genTypeNames))]}}
}

func (g *astGen) literal() Expr {
	switch g.n(6) {
	case 0:
		return &IntLit{Value: int64(g.n(1 << 20))}
	case 1:
		return &FloatLit{Value: float64(g.n(10000)) / 8}
	case 2:
		units := []int64{1_000_000, 1_000_000_000, 60_000_000_000, 3_600_000_000_000, 86_400_000_000_000}
		return &DurationLit{Nanos: int64(g.n(500)) * units[g.n(len(units))]}
	case 3:
		return &BoolLit{Value: g.chance(50)}
	}
	return g.str(false)
}

var genTexts = []string{"plain", "with \"quotes\"", "tab\there", "brace {x}", "back\\slash", "line\nbreak", "ünïcode ✓", "\x01ctl"}

func (g *astGen) str(interp bool) *StringLit {
	s := &StringLit{}
	for i := g.n(4); i >= 0; i-- {
		if interp && g.chance(40) && g.enter() {
			s.Parts = append(s.Parts, StrPart{X: g.expr()})
			g.leave()
			continue
		}
		if len(s.Parts) > 0 && s.Parts[len(s.Parts)-1].X == nil {
			continue // the lexer merges adjacent literal text
		}
		s.Parts = append(s.Parts, StrPart{Lit: genTexts[g.n(len(genTexts))]})
	}
	return s
}

func (g *astGen) args(named bool) []*Arg {
	var out []*Arg
	for i := g.n(3); i > 0; i-- {
		a := &Arg{Value: g.expr()}
		if named || (len(out) > 0 && out[len(out)-1].Name != nil) || g.chance(25) {
			a.Name = g.name()
		}
		out = append(out, a)
	}
	return out
}

func (g *astGen) expr() Expr {
	if !g.enter() {
		if g.chance(50) {
			return g.literal()
		}
		return g.name()
	}
	defer g.leave()
	switch g.n(24) {
	case 0, 1:
		return g.literal()
	case 2, 3:
		return g.name()
	case 4:
		return &PathExpr{Segs: []*Ident{g.upper(), g.upper()}}
	case 5:
		if g.chance(30) {
			return &TupleExpr{}
		}
		return &TupleExpr{Elems: []Expr{g.expr(), g.expr()}}
	case 6:
		l := &ListExpr{}
		for i := g.n(3); i > 0; i-- {
			l.Elems = append(l.Elems, g.expr())
		}
		return l
	case 7:
		r := &RecordLit{Path: []*Ident{g.upper()}}
		for i := g.n(3); i > 0; i-- {
			r.Fields = append(r.Fields, &FieldInit{Name: g.name(), Value: g.expr()})
		}
		return r
	case 8:
		l := &Lambda{Body: g.expr()}
		for i := g.n(3); i > 0; i-- {
			l.Params = append(l.Params, &LambdaParam{Name: g.name()})
		}
		return l
	case 9:
		e := &IfExpr{Cond: g.expr(), Then: g.block()}
		switch g.n(3) {
		case 1:
			e.Else = g.block()
		case 2:
			e.Else = &IfExpr{Cond: g.expr(), Then: g.block()}
		}
		return e
	case 10:
		m := &MatchExpr{Scrutinee: g.expr()}
		for i := g.n(3); i >= 0; i-- {
			arm := &MatchArm{Pat: g.pattern(), Body: g.expr()}
			if g.chance(30) {
				arm.Guard = g.expr()
			}
			m.Arms = append(m.Arms, arm)
		}
		return m
	case 11:
		return &AgentExpr{Name: g.name(), Args: g.args(true)}
	case 12:
		return g.block()
	case 13:
		op := OpNeg
		if g.chance(50) {
			op = OpNot
		}
		return &UnaryExpr{Op: op, X: g.expr()}
	case 14, 15, 16:
		return &BinaryExpr{Op: BinaryOp(g.n(int(OpRem) + 1)), X: g.expr(), Y: g.expr()}
	case 17:
		return &CallExpr{Fn: g.expr(), Args: g.args(false)}
	case 18:
		c := &CallExpr{Fn: g.expr(), Args: append([]*Arg{{Value: g.expr()}}, g.args(false)...)}
		if len(c.Args) == 1 && g.chance(50) {
			c.Pipe = PipeBare
		} else {
			c.Pipe = PipeCall
		}
		return c
	case 19:
		return &IndexExpr{X: g.expr(), Index: g.expr()}
	case 20:
		return &FieldExpr{X: g.expr(), Name: g.name()}
	case 21:
		return &TryExpr{X: g.expr()}
	case 22:
		return &CastExpr{X: g.expr(), To: g.typeX()}
	}
	return g.str(true)
}

func (g *astGen) pattern() Pattern {
	if !g.enter() {
		return &WildcardPat{}
	}
	defer g.leave()
	switch g.n(8) {
	case 0:
		return &WildcardPat{}
	case 1:
		return &BindPat{Name: &Ident{Name: []string{"a", "b", "rest", "_y"}[g.n(4)]}}
	case 2:
		lit := g.literal()
		if il, ok := lit.(*IntLit); ok && g.chance(30) {
			il.Value = -il.Value - 1
		}
		return &LitPat{Lit: lit}
	case 3:
		vp := &VariantPat{Path: []*Ident{g.upper(), g.upper()}}
		if g.chance(60) {
			vp.HasParens = true
			for i := g.n(3); i > 0; i-- {
				vp.Args = append(vp.Args, g.pattern())
			}
		}
		return vp
	case 4:
		vp := &VariantPat{Path: []*Ident{{Name: "Some"}}, HasParens: true}
		vp.Args = []Pattern{g.pattern()}
		return vp
	case 5:
		if g.chance(30) {
			return &TuplePat{}
		}
		return &TuplePat{Elems: []Pattern{g.pattern(), g.pattern()}}
	case 6:
		rp := &RecordPat{Path: []*Ident{g.upper()}}
		for i := g.n(3); i > 0; i-- {
			fp := &FieldPat{Name: g.name()}
			if g.chance(50) {
				fp.Pat = g.pattern()
			}
			rp.Fields = append(rp.Fields, fp)
		}
		return rp
	}
	return &WildcardPat{}
}

func (g *astGen) block() *BlockExpr {
	b := &BlockExpr{}
	if !g.enter() {
		return b
	}
	defer g.leave()
	for i := g.n(4); i > 0; i-- {
		b.Stmts = append(b.Stmts, g.stmt())
	}
	if g.chance(60) {
		b.Tail = g.expr()
	}
	return b
}

func (g *astGen) annots(let bool) []*Annotation {
	var out []*Annotation
	for i := g.n(3); i > 0 && g.chance(40); i-- {
		a := &Annotation{Name: g.name()}
		if g.chance(50) {
			a.HasParens = true
			a.Args = g.args(false)
		}
		out = append(out, a)
	}
	return out
}

func (g *astGen) stmt() Stmt {
	switch g.n(7) {
	case 0, 1:
		s := &LetStmt{Annot: g.annots(true), Pat: g.pattern(), Value: g.expr()}
		if g.chance(30) {
			s.TypeX = g.typeX()
		}
		if g.chance(20) {
			s.Compensate = &Lambda{Params: []*LambdaParam{{Name: &Ident{Name: "it"}}}, Body: g.expr(), Synthetic: true}
		}
		return s
	case 2:
		return &ForStmt{Pat: g.pattern(), Iter: g.expr(), Body: g.block()}
	case 3:
		return &RequireStmt{Args: g.args(true)}
	case 4:
		s := &ReturnStmt{}
		if g.chance(70) {
			s.Value = g.expr()
		}
		return s
	case 5:
		return &AssignStmt{Target: g.name(), Value: g.expr()}
	}
	return &ExprStmt{X: g.expr()}
}

func (g *astGen) params() []*Param {
	var ps []*Param
	for i := g.n(3); i > 0; i-- {
		ps = append(ps, &Param{Name: g.name(), TypeX: g.typeX()})
	}
	return ps
}

func (g *astGen) effects() *EffectClause {
	if g.chance(50) {
		return nil
	}
	ec := &EffectClause{}
	for i := g.n(3); i >= 0; i-- {
		ec.Names = append(ec.Names, &Ident{Name: effectNames[g.n(len(effectNames))].name})
	}
	return ec
}

func (g *astGen) item() Item {
	switch g.n(6) {
	case 0:
		d := &WorkflowDecl{Name: g.name(), Params: g.params(), Body: g.block(), Effects: g.effects()}
		d.Annot = g.annots(false)
		if g.chance(50) {
			d.Result = g.typeX()
		}
		return d
	case 1:
		d := &FnDecl{Name: g.name(), Params: g.params(), Body: g.block(), Effects: g.effects()}
		if g.chance(40) {
			d.Generics = []*Ident{{Name: "T"}, {Name: "U"}}[:1+g.n(2)]
		}
		if g.chance(50) {
			d.Result = g.typeX()
		}
		return d
	case 2:
		ec := g.effects()
		if ec == nil {
			ec = &EffectClause{Names: []*Ident{{Name: "passive"}}}
		}
		return &ToolDecl{Name: g.name(), Params: g.params(), Result: g.typeX(), Effects: ec}
	case 3:
		d := &TypeDecl{Name: g.upper()}
		for i := g.n(3); i > 0; i-- {
			d.Fields = append(d.Fields, &FieldDecl{Name: g.name(), TypeX: g.typeX()})
		}
		return d
	case 4:
		d := &EnumDecl{Name: g.upper()}
		for i := g.n(3); i >= 0; i-- {
			v := &VariantDecl{Name: g.upper()}
			for j := g.n(3); j > 0; j-- {
				v.Payload = append(v.Payload, g.typeX())
			}
			d.Variants = append(d.Variants, v)
		}
		return d
	}
	return &UseDecl{Path: []*Ident{g.upper(), g.upper()}}
}

func genFile(r core.Rng) *File {
	g := &astGen{r: r}
	f := &File{Name: "gen.ff"}
	for i := g.n(4); i >= 0; i-- {
		f.Items = append(f.Items, g.item())
	}
	return f
}

// ---- Random well-typed programs ----

// tyKind enumerates the types the program generator uses.
type tyKind int

const (
	tyInt tyKind = iota
	tyBool
	tyString
	tyDuration
	tyListInt
	tyOptInt
	tyCount
)

var tyNames = [...]string{"Int", "Bool", "String", "Duration", "List<Int>", "Option<Int>"}

type progVar struct {
	name string
	ty   tyKind
}

type progFn struct {
	name   string
	params []tyKind
	result tyKind
}

type progGen struct {
	r     core.Rng
	vars  []progVar
	fns   []progFn
	depth int
	fresh int
}

func (g *progGen) n(k int) int { return core.IntN(g.r, k) }

func (g *progGen) freshName(prefix string) string {
	g.fresh++
	return fmt.Sprintf("%s%d", prefix, g.fresh)
}

func (g *progGen) varOf(t tyKind) (string, bool) {
	var cands []string
	for _, v := range g.vars {
		if v.ty == t {
			cands = append(cands, v.name)
		}
	}
	if len(cands) == 0 {
		return "", false
	}
	return cands[g.n(len(cands))], true
}

// withVar runs fn with v bound, then unbinds it.
func (g *progGen) withVar(v progVar, fn func() string) string {
	g.vars = append(g.vars, v)
	s := fn()
	g.vars = g.vars[:len(g.vars)-1]
	return s
}

func (g *progGen) expr(t tyKind) string {
	g.depth++
	defer func() { g.depth-- }()
	leaf := g.depth > 4 || g.n(3) == 0
	if v, ok := g.varOf(t); ok && (leaf || g.n(4) == 0) {
		return v
	}
	if leaf {
		return g.leaf(t)
	}
	if g.n(8) == 0 {
		return g.compound(t)
	}
	switch t {
	case tyInt:
		switch g.n(9) {
		case 0:
			return "(" + g.expr(tyInt) + " + " + g.expr(tyInt) + ")"
		case 1:
			return "(" + g.expr(tyInt) + " * " + g.expr(tyInt) + ")"
		case 2:
			return "len(" + g.expr(tyListInt) + ")"
		case 3:
			return "(" + g.expr(tyListInt) + " |> fold(0, (acc, x) => acc + x))"
		case 4:
			return "match " + g.expr(tyOptInt) + " { Some(v) => v, None => " + g.expr(tyInt) + " }"
		case 5:
			return g.callFn(tyInt)
		case 6:
			return "(" + g.expr(tyDuration) + " as Int)"
		case 7:
			return "-" + g.leaf(tyInt)
		}
		return "(" + g.expr(tyInt) + " - " + g.expr(tyInt) + ")"
	case tyBool:
		switch g.n(7) {
		case 0:
			return "(" + g.expr(tyInt) + " < " + g.expr(tyInt) + ")"
		case 1:
			return "(" + g.expr(tyBool) + " && " + g.expr(tyBool) + ")"
		case 2:
			return "!" + g.leaf(tyBool)
		case 3:
			return "(" + g.expr(tyString) + " == " + g.expr(tyString) + ")"
		case 4:
			x := g.freshName("e")
			return "(" + g.expr(tyListInt) + " |> any(" + x + " => " + g.withVar(progVar{x, tyInt}, func() string { return g.expr(tyBool) }) + "))"
		case 5:
			return "contains(" + g.expr(tyListInt) + ", " + g.expr(tyInt) + ")"
		}
		return g.callFn(tyBool)
	case tyString:
		switch g.n(5) {
		case 0:
			return "\"v={ " + g.expr(tyInt) + "} d={ " + g.expr(tyDuration) + "}\""
		case 1:
			return "(" + g.expr(tyString) + " + " + g.expr(tyString) + ")"
		case 2:
			return "to_string(" + g.expr(tyInt) + ")"
		case 3:
			return "upper(" + g.expr(tyString) + ")"
		}
		return g.callFn(tyString)
	case tyDuration:
		switch g.n(3) {
		case 0:
			return "(" + g.expr(tyDuration) + " + " + g.expr(tyDuration) + ")"
		case 1:
			return "(" + g.expr(tyDuration) + " * " + g.expr(tyInt) + ")"
		}
		return g.callFn(tyDuration)
	case tyListInt:
		switch g.n(6) {
		case 0:
			return "[" + g.expr(tyInt) + ", " + g.expr(tyInt) + "]"
		case 1:
			x := g.freshName("e")
			return "(" + g.expr(tyListInt) + " |> map(" + x + " => " + g.withVar(progVar{x, tyInt}, func() string { return g.expr(tyInt) }) + "))"
		case 2:
			x := g.freshName("e")
			return "(" + g.expr(tyListInt) + " |> filter(" + x + " => " + g.withVar(progVar{x, tyInt}, func() string { return g.expr(tyBool) }) + "))"
		case 3:
			return "(0.." + g.leaf(tyInt) + ")"
		case 4:
			return "(" + g.expr(tyListInt) + " |> take(" + g.expr(tyInt) + ") |> reverse)"
		}
		return g.callFn(tyListInt)
	case tyOptInt:
		switch g.n(4) {
		case 0:
			return "Some(" + g.expr(tyInt) + ")"
		case 1:
			return "first(" + g.expr(tyListInt) + ")"
		case 2:
			return "parse_int(" + g.expr(tyString) + ")"
		}
		return g.callFn(tyOptInt)
	}
	return g.leaf(t)
}

// compound produces if, block and let forms of any type.
func (g *progGen) compound(t tyKind) string {
	switch g.n(3) {
	case 0:
		return "if " + g.expr(tyBool) + " { " + g.expr(t) + " } else { " + g.expr(t) + " }"
	case 1:
		v := g.freshName("l")
		vt := tyKind(g.n(int(tyCount)))
		init := g.expr(vt)
		return "{ let " + v + ": " + tyNames[vt] + " = " + init + "; " +
			g.withVar(progVar{v, vt}, func() string { return g.expr(t) }) + " }"
	}
	f := g.freshName("f")
	x := g.freshName("p")
	body := g.withVar(progVar{x, tyInt}, func() string { return g.expr(t) })
	return "{ let " + f + " = " + x + " => " + body + "; " + f + "(" + g.expr(tyInt) + ") }"
}

func (g *progGen) leaf(t tyKind) string {
	switch t {
	case tyInt:
		return fmt.Sprint(g.n(1000))
	case tyBool:
		if g.n(2) == 0 {
			return "true"
		}
		return "false"
	case tyString:
		return fmt.Sprintf("\"s%d\"", g.n(100))
	case tyDuration:
		return fmt.Sprintf("%d%s", 1+g.n(59), []string{"ms", "s", "m", "h", "d"}[g.n(5)])
	case tyListInt:
		return fmt.Sprintf("[%d]", g.n(10))
	}
	return "None"
}

func (g *progGen) callFn(t tyKind) string {
	var cands []progFn
	for _, f := range g.fns {
		if f.result == t {
			cands = append(cands, f)
		}
	}
	if len(cands) == 0 || g.depth > 3 {
		return g.leaf(t)
	}
	f := cands[g.n(len(cands))]
	args := make([]string, len(f.params))
	for i, p := range f.params {
		args[i] = g.expr(p)
	}
	return f.name + "(" + strings.Join(args, ", ") + ")"
}

// fn generates one function definition and registers it.
func (g *progGen) fn() string {
	name := g.freshName("fun")
	nparams := g.n(3)
	pf := progFn{name: name, result: tyKind(g.n(int(tyCount)))}
	var ps []string
	g.vars = g.vars[:0]
	for i := 0; i < nparams; i++ {
		pt := tyKind(g.n(int(tyCount)))
		pn := fmt.Sprintf("arg%d", i)
		pf.params = append(pf.params, pt)
		ps = append(ps, pn+": "+tyNames[pt])
		g.vars = append(g.vars, progVar{pn, pt})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "fn %s(%s) -> %s {\n", name, strings.Join(ps, ", "), tyNames[pf.result])
	for i := g.n(4); i > 0; i-- {
		v := g.freshName("v")
		vt := tyKind(g.n(int(tyCount)))
		fmt.Fprintf(&b, "  let %s: %s = %s;\n", v, tyNames[vt], g.expr(vt))
		g.vars = append(g.vars, progVar{v, vt})
	}
	fmt.Fprintf(&b, "  %s\n}\n", g.expr(pf.result))
	g.vars = g.vars[:0]
	g.fns = append(g.fns, pf) // registered after the body: no recursion
	return b.String()
}

// genProgram returns a well-typed program of roughly size bytes.
func genProgram(r core.Rng, size int) string {
	g := &progGen{r: r}
	var b strings.Builder
	b.WriteString("tool lookup(name: Fqdn) -> Option<IpAddress> ! passive\n\n")
	for b.Len() < size {
		b.WriteString(g.fn())
		b.WriteString("\n")
		if len(g.fns) > 40 {
			g.fns = g.fns[1:]
		}
	}
	b.WriteString("workflow main(roots: List<Fqdn>) -> Int {\n  let ips = roots |> filter_map(lookup);\n  len(ips)\n}\n")
	return b.String()
}
