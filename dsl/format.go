package dsl

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Format pretty-prints a file in canonical style: two-space indentation,
// one item per paragraph, pipes kept as written (CallExpr.Pipe), and
// parentheses only where precedence or statement position needs them.
// For any File produced by Parse without errors, parsing the output
// yields the same tree (spans aside): ASTJSON(Parse(Format(f))) equals
// ASTJSON(f). Comments are not preserved.
func Format(f *File) string {
	p := &printer{}
	for i, it := range f.Items {
		if i > 0 {
			p.b.WriteString("\n")
		}
		p.item(it)
		p.b.WriteString("\n")
	}
	return p.b.String()
}

// FormatExpr pretty-prints a single expression.
func FormatExpr(e Expr) string {
	p := &printer{}
	p.expr(e, 0)
	return p.b.String()
}

type printer struct {
	b      strings.Builder
	indent int
	// chain is set while printing the left spine of a multi-line pipe.
	chain bool
}

// pipeLen counts the pipe stages along the left spine of e.
func pipeLen(e Expr) int {
	n := 0
	for {
		c, ok := e.(*CallExpr)
		if !ok || c.Pipe == PipeNone || len(c.Args) == 0 {
			return n
		}
		n++
		e = c.Args[0].Value
	}
}

func (p *printer) ws(s string) { p.b.WriteString(s) }

func (p *printer) nl() {
	p.b.WriteByte('\n')
	for i := 0; i < p.indent; i++ {
		p.b.WriteString("  ")
	}
}

func (p *printer) annots(as []*Annotation) {
	for _, a := range as {
		p.ws("#[")
		p.ws(a.Name.Name)
		if a.HasParens || len(a.Args) > 0 {
			p.args(a.Args)
		}
		p.ws("]")
		p.nl()
	}
}

func (p *printer) args(as []*Arg) {
	p.ws("(")
	for i, a := range as {
		if i > 0 {
			p.ws(", ")
		}
		if a.Name != nil {
			p.ws(a.Name.Name)
			p.ws(" = ")
		}
		p.expr(a.Value, 0)
	}
	p.ws(")")
}

func (p *printer) params(ps []*Param) {
	p.ws("(")
	for i, prm := range ps {
		if i > 0 {
			p.ws(", ")
		}
		p.ws(prm.Name.Name)
		p.ws(": ")
		p.typ(prm.TypeX)
	}
	p.ws(")")
}

func (p *printer) generics(gs []*Ident) {
	if len(gs) == 0 {
		return
	}
	p.ws("<")
	for i, g := range gs {
		if i > 0 {
			p.ws(", ")
		}
		p.ws(g.Name)
	}
	p.ws(">")
}

func (p *printer) effects(ec *EffectClause) {
	if ec == nil {
		return
	}
	p.ws(" ! ")
	for i, n := range ec.Names {
		if i > 0 {
			p.ws(", ")
		}
		p.ws(n.Name)
	}
}

func (p *printer) item(it Item) {
	p.annots(it.Annots())
	switch d := it.(type) {
	case *WorkflowDecl:
		p.ws("workflow " + d.Name.Name)
		p.params(d.Params)
		if d.Result != nil {
			p.ws(" -> ")
			p.typ(d.Result)
		}
		p.effects(d.Effects)
		p.ws(" ")
		p.block(d.Body)
	case *FnDecl:
		p.ws("fn " + d.Name.Name)
		p.generics(d.Generics)
		p.params(d.Params)
		if d.Result != nil {
			p.ws(" -> ")
			p.typ(d.Result)
		}
		p.effects(d.Effects)
		p.ws(" ")
		p.block(d.Body)
	case *ToolDecl:
		p.ws("tool " + d.Name.Name)
		p.params(d.Params)
		p.ws(" -> ")
		p.typ(d.Result)
		p.effects(d.Effects)
	case *TypeDecl:
		p.ws("type " + d.Name.Name)
		p.generics(d.Generics)
		p.ws(" = {")
		for i, f := range d.Fields {
			if i > 0 {
				p.ws(",")
			}
			p.ws(" " + f.Name.Name + ": ")
			p.typ(f.TypeX)
		}
		if len(d.Fields) > 0 {
			p.ws(" ")
		}
		p.ws("}")
	case *EnumDecl:
		p.ws("enum " + d.Name.Name)
		p.generics(d.Generics)
		p.ws(" {")
		for i, v := range d.Variants {
			if i > 0 {
				p.ws(",")
			}
			p.ws(" " + v.Name.Name)
			if len(v.Payload) > 0 {
				p.ws("(")
				for j, t := range v.Payload {
					if j > 0 {
						p.ws(", ")
					}
					p.typ(t)
				}
				p.ws(")")
			}
		}
		p.ws(" }")
	case *UseDecl:
		p.ws("use " + pathString(d.Path) + ";")
	}
}

func (p *printer) typ(t TypeExpr) {
	switch t := t.(type) {
	case nil:
		p.ws("()")
	case *NamedTypeExpr:
		p.ws(t.Name.Name)
		if len(t.Args) > 0 {
			p.ws("<")
			for i, a := range t.Args {
				if i > 0 {
					p.ws(", ")
				}
				p.typ(a)
			}
			p.ws(">")
		}
	case *TupleTypeExpr:
		p.ws("(")
		for i, e := range t.Elems {
			if i > 0 {
				p.ws(", ")
			}
			p.typ(e)
		}
		p.ws(")")
	case *FuncTypeExpr:
		p.ws("(")
		for i, e := range t.Params {
			if i > 0 {
				p.ws(", ")
			}
			p.typ(e)
		}
		p.ws(") -> ")
		p.typ(t.Result)
	}
}

func (p *printer) block(b *BlockExpr) {
	if b == nil {
		p.ws("{}")
		return
	}
	if len(b.Stmts) == 0 && b.Tail == nil {
		p.ws("{}")
		return
	}
	p.ws("{")
	p.indent++
	for i, s := range b.Stmts {
		p.nl()
		last := i == len(b.Stmts)-1 && b.Tail == nil
		p.stmt(s, last)
	}
	if b.Tail != nil {
		p.nl()
		p.stmtExpr(b.Tail)
	}
	p.indent--
	p.nl()
	p.ws("}")
}

// stmtExpr prints an expression in statement position, where a leading
// block-like expression would end the statement early.
func (p *printer) stmtExpr(e Expr) {
	if !isBlockLike(e) && isBlockLike(leftmost(e)) {
		p.ws("(")
		p.expr(e, 0)
		p.ws(")")
		return
	}
	p.expr(e, 0)
}

// leftmost returns the expression whose first token starts e.
func leftmost(e Expr) Expr {
	for {
		switch x := e.(type) {
		case *BinaryExpr:
			e = x.X
		case *CallExpr:
			if x.Pipe != PipeNone && len(x.Args) > 0 {
				e = x.Args[0].Value
			} else {
				e = x.Fn
			}
		case *IndexExpr:
			e = x.X
		case *FieldExpr:
			e = x.X
		case *TryExpr:
			e = x.X
		case *CastExpr:
			e = x.X
		default:
			return e
		}
	}
}

func (p *printer) stmt(s Stmt, last bool) {
	switch s := s.(type) {
	case *LetStmt:
		p.annots(s.Annot)
		p.ws("let ")
		p.pat(s.Pat)
		if s.TypeX != nil {
			p.ws(": ")
			p.typ(s.TypeX)
		}
		p.ws(" = ")
		p.expr(s.Value, 0)
		if s.Compensate != nil {
			p.ws(" compensate ")
			p.expr(s.Compensate.Body, 0)
		}
		p.ws(";")
	case *ForStmt:
		p.ws("for ")
		p.pat(s.Pat)
		p.ws(" in ")
		p.head(s.Iter)
		p.ws(" ")
		p.block(s.Body)
	case *RequireStmt:
		p.ws("require approval")
		p.args(s.Args)
		p.ws(";")
	case *ReturnStmt:
		p.ws("return")
		if s.Value != nil {
			p.ws(" ")
			p.expr(s.Value, 0)
		}
		p.ws(";")
	case *AssignStmt:
		p.ws(s.Target.Name + " = ")
		p.expr(s.Value, 0)
		p.ws(";")
	case *ExprStmt:
		if isBlockLike(s.X) {
			p.expr(s.X, 0)
			if last {
				p.ws(";")
			}
			return
		}
		p.stmtExpr(s.X)
		p.ws(";")
	}
}

// head prints the head of an if, match or for, where record literals
// must be parenthesized.
func (p *printer) head(e Expr) {
	if containsRecordLit(e) {
		p.ws("(")
		p.expr(e, 0)
		p.ws(")")
		return
	}
	p.expr(e, 1)
}

func containsRecordLit(e Expr) bool {
	found := false
	Walk(e, func(n Node) bool {
		if _, ok := n.(*RecordLit); ok {
			found = true
		}
		return !found
	})
	return found
}

// Printing precedences: 0 lambda, 1 pipe, 2-8 binary (spec 7.3), 9 unary,
// 10 postfix, 11 primary.
func exprPrec(e Expr) int {
	switch x := e.(type) {
	case *Lambda:
		return 0
	case *CallExpr:
		if x.Pipe != PipeNone {
			return 1
		}
		return 10
	case *BinaryExpr:
		return x.Op.precedence()
	case *UnaryExpr:
		return 9
	case *IndexExpr, *FieldExpr, *TryExpr, *CastExpr:
		return 10
	}
	return 11
}

func (p *printer) expr(e Expr, ctx int) {
	chain := p.chain
	p.chain = false
	prec := exprPrec(e)
	_, isCast := e.(*CastExpr)
	if prec < ctx || (isCast && ctx >= 2) {
		p.ws("(")
		p.exprInner(e)
		p.ws(")")
		return
	}
	if c, ok := e.(*CallExpr); ok && c.Pipe != PipeNone {
		p.pipe(c, chain)
		return
	}
	p.exprInner(e)
}

// pipe prints a pipe call; chains of three or more stages put each stage
// on its own line.
func (p *printer) pipe(x *CallExpr, inChain bool) {
	multi := inChain || pipeLen(x) >= 3
	outer := multi && !inChain
	if outer {
		p.indent++
	}
	p.chain = multi
	p.expr(x.Args[0].Value, 1)
	p.chain = false
	if multi {
		p.nl()
		p.ws("|> ")
	} else {
		p.ws(" |> ")
	}
	if x.Pipe == PipeBare && len(x.Args) == 1 {
		if c, ok := x.Fn.(*CallExpr); ok && c.Pipe == PipeNone {
			p.ws("(")
			p.call(c)
			p.ws(")")
		} else {
			p.expr(x.Fn, 2)
		}
	} else {
		p.expr(x.Fn, 10)
		p.args(x.Args[1:])
	}
	if outer {
		p.indent--
	}
}

func (p *printer) exprInner(e Expr) {
	switch x := e.(type) {
	case *IntLit:
		p.ws(strconv.FormatInt(x.Value, 10))
	case *FloatLit:
		p.ws(formatFloat(x.Value))
	case *DurationLit:
		p.ws(formatDuration(x.Nanos))
	case *BoolLit:
		p.ws(strconv.FormatBool(x.Value))
	case *StringLit:
		p.str(x)
	case *Ident:
		p.ws(x.Name)
	case *PathExpr:
		p.ws(pathString(x.Segs))
	case *TupleExpr:
		p.ws("(")
		for i, el := range x.Elems {
			if i > 0 {
				p.ws(", ")
			}
			p.expr(el, 0)
		}
		p.ws(")")
	case *ListExpr:
		p.ws("[")
		for i, el := range x.Elems {
			if i > 0 {
				p.ws(", ")
			}
			p.expr(el, 0)
		}
		p.ws("]")
	case *RecordLit:
		p.ws(pathString(x.Path))
		if len(x.Fields) == 0 {
			p.ws(" {}")
			return
		}
		p.ws(" { ")
		for i, f := range x.Fields {
			if i > 0 {
				p.ws(", ")
			}
			p.ws(f.Name.Name + ": ")
			p.expr(f.Value, 0)
		}
		p.ws(" }")
	case *Lambda:
		if len(x.Params) == 1 {
			p.ws(x.Params[0].Name.Name)
		} else {
			p.ws("(")
			for i, prm := range x.Params {
				if i > 0 {
					p.ws(", ")
				}
				p.ws(prm.Name.Name)
			}
			p.ws(")")
		}
		p.ws(" => ")
		p.expr(x.Body, 0)
	case *IfExpr:
		p.ws("if ")
		p.head(x.Cond)
		p.ws(" ")
		p.block(x.Then)
		if x.Else != nil {
			p.ws(" else ")
			if ei, ok := x.Else.(*IfExpr); ok {
				p.exprInner(ei)
			} else if b, ok := x.Else.(*BlockExpr); ok {
				p.block(b)
			} else {
				p.ws("{ ")
				p.expr(x.Else, 0)
				p.ws(" }")
			}
		}
	case *MatchExpr:
		p.ws("match ")
		p.head(x.Scrutinee)
		p.ws(" {")
		p.indent++
		for _, arm := range x.Arms {
			p.nl()
			p.pat(arm.Pat)
			if arm.Guard != nil {
				p.ws(" if ")
				p.expr(arm.Guard, 1)
			}
			p.ws(" => ")
			p.expr(arm.Body, 0)
			p.ws(",")
		}
		p.indent--
		if len(x.Arms) > 0 {
			p.nl()
		}
		p.ws("}")
	case *AgentExpr:
		p.ws("agent " + x.Name.Name)
		p.args(x.Args)
	case *BlockExpr:
		p.block(x)
	case *UnaryExpr:
		if x.Op == OpNeg {
			p.ws("-")
		} else {
			p.ws("!")
		}
		p.expr(x.X, 9)
	case *BinaryExpr:
		lv := x.Op.precedence()
		left, right := lv, lv+1
		switch lv {
		case 4, 5, 6: // non-associative
			left = lv + 1
		}
		p.expr(x.X, left)
		p.ws(" " + x.Op.String() + " ")
		p.expr(x.Y, right)
	case *CallExpr:
		p.call(x)
	case *IndexExpr:
		p.expr(x.X, 10)
		p.ws("[")
		p.expr(x.Index, 0)
		p.ws("]")
	case *FieldExpr:
		p.expr(x.X, 10)
		p.ws("." + x.Name.Name)
	case *TryExpr:
		p.expr(x.X, 10)
		p.ws("?")
	case *CastExpr:
		p.expr(x.X, 10)
		p.ws(" as ")
		p.typ(x.To)
	case *BadExpr:
		p.ws("()")
	default:
		p.ws(fmt.Sprintf("/* %T */", e))
	}
}

// call prints a call written without a pipe (pipes go through pipe).
func (p *printer) call(x *CallExpr) {
	if x.Pipe != PipeNone && len(x.Args) > 0 {
		p.pipe(x, false)
		return
	}
	p.expr(x.Fn, 10)
	p.args(x.Args)
}

func (p *printer) pat(pt Pattern) {
	switch x := pt.(type) {
	case *WildcardPat:
		p.ws("_")
	case *BindPat:
		p.ws(x.Name.Name)
	case *LitPat:
		p.exprInner(x.Lit)
	case *VariantPat:
		p.ws(pathString(x.Path))
		if x.HasParens || len(x.Args) > 0 {
			p.ws("(")
			for i, a := range x.Args {
				if i > 0 {
					p.ws(", ")
				}
				p.pat(a)
			}
			p.ws(")")
		}
	case *TuplePat:
		p.ws("(")
		for i, e := range x.Elems {
			if i > 0 {
				p.ws(", ")
			}
			p.pat(e)
		}
		p.ws(")")
	case *RecordPat:
		p.ws(pathString(x.Path) + " {")
		for i, f := range x.Fields {
			if i > 0 {
				p.ws(",")
			}
			p.ws(" " + f.Name.Name)
			if f.Pat != nil {
				p.ws(": ")
				p.pat(f.Pat)
			}
		}
		if len(x.Fields) > 0 {
			p.ws(" ")
		}
		p.ws("}")
	}
}

func (p *printer) str(s *StringLit) {
	p.ws(`"`)
	for _, part := range s.Parts {
		if part.X != nil {
			sub := &printer{indent: p.indent}
			sub.expr(part.X, 0)
			inner := sub.b.String()
			p.ws("{")
			if strings.HasPrefix(inner, "{") {
				p.ws(" ") // `{{` would be a literal brace
			}
			p.ws(inner)
			p.ws("}")
			continue
		}
		p.ws(escapeString(part.Lit))
	}
	p.ws(`"`)
}

// escapeString escapes literal string text for source.
func escapeString(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '{':
			b.WriteString("{{")
		case '}':
			b.WriteString("}}")
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u{%X}`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// formatFloat prints a float so that it lexes back as a float literal.
func formatFloat(v float64) string {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return "0.0"
	}
	neg := v < 0 || (v == 0 && math.Signbit(v))
	if neg {
		v = -v
	}
	s := strconv.FormatFloat(v, 'g', -1, 64)
	mant, exp, hasExp := strings.Cut(s, "e")
	if !strings.Contains(mant, ".") {
		mant += ".0"
	}
	if hasExp {
		s = mant + "e" + exp
	} else {
		s = mant
	}
	if neg {
		return "-" + s
	}
	return s
}

// formatDuration prints nanoseconds with the largest exact unit.
func formatDuration(ns int64) string {
	if ns == 0 {
		return "0s"
	}
	for _, u := range []struct {
		name string
		n    int64
	}{{"d", durationUnits["d"]}, {"h", durationUnits["h"]}, {"m", durationUnits["m"]}, {"s", durationUnits["s"]}, {"ms", durationUnits["ms"]}} {
		if ns%u.n == 0 {
			return strconv.FormatInt(ns/u.n, 10) + u.name
		}
	}
	// Not expressible as a literal (sub-millisecond); round to ms.
	return strconv.FormatInt(ns/durationUnits["ms"], 10) + "ms"
}
