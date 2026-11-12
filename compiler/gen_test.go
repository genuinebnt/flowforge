package compiler_test

import (
	"fmt"
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

// progGen generates well-typed, terminating Flowforge programs for the
// property and differential tests: fns over Int and List<Int> (each may
// call only earlier fns, so there is no recursion), lambdas with captures,
// every higher-order library function, matches on Option and a user
// enum, records, string interpolation, tool calls and par_map. Run-time
// failures (overflow, division by zero, index out of range, a failing
// tool) are allowed: both evaluators must agree on them too.
type progGen struct {
	r     core.Rng
	depth int
	n     int // fresh-name counter
	fns   []genFn
	vars  []genVar
}

type genType int

const (
	tInt genType = iota
	tBool
	tStr
	tList
	tOpt
	numGenTypes
)

var genTypeText = [...]string{"Int", "Bool", "String", "List<Int>", "Option<Int>"}

type genVar struct {
	name string
	t    genType
}

type genFn struct {
	name   string
	params []genType
	result genType
}

func (g *progGen) intn(k int) int { return core.IntN(g.r, k) }

func (g *progGen) chance(pct int) bool { return g.intn(100) < pct }

func (g *progGen) fresh(prefix string) string {
	g.n++
	return fmt.Sprintf("%s%d", prefix, g.n)
}

// genPrelude declares the types and the tool the generated code uses.
const genPrelude = `tool probe(n: Int) -> Int ! passive
enum Shape { Dot, Line(Int), Box(Int, Int) }
type Acc = { total: Int, label: String }
`

// program returns a complete program with a `main` workflow.
func (g *progGen) program(nfns int) string {
	var b strings.Builder
	b.WriteString(genPrelude)
	for i := 0; i < nfns; i++ {
		b.WriteString(g.fn())
	}
	g.vars = nil
	b.WriteString("workflow main() -> (Int, Bool, String, List<Int>, Option<Int>) {\n")
	for i := g.intn(4); i > 0; i-- {
		b.WriteString("  " + g.let() + "\n")
	}
	if g.chance(30) {
		v := g.fresh("v")
		fmt.Fprintf(&b, "  for %s in %s { log(\"info\", to_string(%s)); }\n", v, g.expr(tList), v)
	}
	fmt.Fprintf(&b, "  (%s, %s, %s, %s, %s)\n}\n", g.expr(tInt), g.expr(tBool), g.expr(tStr), g.expr(tList), g.expr(tOpt))
	return b.String()
}

func (g *progGen) fn() string {
	f := genFn{name: g.fresh("f"), result: genType(g.intn(int(numGenTypes)))}
	g.vars = nil
	var params []string
	for i := g.intn(3); i >= 0; i-- {
		t := tInt
		if g.chance(30) {
			t = tList
		}
		name := g.fresh("p")
		f.params = append(f.params, t)
		params = append(params, name+": "+genTypeText[t])
		g.vars = append(g.vars, genVar{name, t})
	}
	var body strings.Builder
	for i := g.intn(3); i > 0; i-- {
		body.WriteString("  " + g.let() + "\n")
	}
	body.WriteString("  " + g.expr(f.result) + "\n")
	g.fns = append(g.fns, f)
	return fmt.Sprintf("fn %s(%s) -> %s ! passive {\n%s}\n", f.name, strings.Join(params, ", "), genTypeText[f.result], body.String())
}

func (g *progGen) let() string {
	t := genType(g.intn(int(numGenTypes)))
	e := g.expr(t)
	name := g.fresh("x")
	g.vars = append(g.vars, genVar{name, t})
	return fmt.Sprintf("let %s: %s = %s;", name, genTypeText[t], e)
}

// withVar runs body with an extra variable in scope.
func (g *progGen) withVar(name string, t genType, body func() string) string {
	g.vars = append(g.vars, genVar{name, t})
	s := body()
	g.vars = g.vars[:len(g.vars)-1]
	return s
}

func (g *progGen) varOf(t genType) (string, bool) {
	var cands []string
	for _, v := range g.vars {
		if v.t == t {
			cands = append(cands, v.name)
		}
	}
	if len(cands) == 0 {
		return "", false
	}
	return cands[g.intn(len(cands))], true
}

func (g *progGen) expr(t genType) string {
	if g.depth > 3 || g.chance(25) {
		return g.leaf(t)
	}
	g.depth++
	defer func() { g.depth-- }()
	// Compound forms are parenthesized so that block-like expressions
	// (if, match, blocks, record literals) never meet an if/match/for head.
	switch t {
	case tInt:
		return "(" + g.intExpr() + ")"
	case tBool:
		return "(" + g.boolExpr() + ")"
	case tStr:
		return "(" + g.strExpr() + ")"
	case tList:
		return "(" + g.listExpr() + ")"
	}
	return "(" + g.optExpr() + ")"
}

func (g *progGen) leaf(t genType) string {
	if v, ok := g.varOf(t); ok && g.chance(60) {
		return v
	}
	switch t {
	case tInt:
		if g.chance(3) {
			return "9223372036854775807"
		}
		return fmt.Sprint(g.intn(20))
	case tBool:
		return []string{"true", "false"}[g.intn(2)]
	case tStr:
		return []string{`"a"`, `"Bb"`, `"x,y"`, `""`, `" sp "`}[g.intn(5)]
	case tList:
		n := g.intn(4)
		var el []string
		for i := 0; i < n; i++ {
			el = append(el, fmt.Sprint(g.intn(10)))
		}
		if n == 0 {
			return fmt.Sprintf("(0..%d)", g.intn(5))
		}
		return "[" + strings.Join(el, ", ") + "]"
	}
	if g.chance(50) {
		return `parse_int("none")` // None, typed without a hint
	}
	return fmt.Sprintf("Some(%d)", g.intn(10))
}

func (g *progGen) lambda(param genType, body genType) string {
	v := g.fresh("l")
	return g.withVar(v, param, func() string { return v + " => " + g.expr(body) })
}

func (g *progGen) callFn(want genType) (string, bool) {
	var cands []genFn
	for _, f := range g.fns {
		if f.result == want {
			cands = append(cands, f)
		}
	}
	if len(cands) == 0 {
		return "", false
	}
	f := cands[g.intn(len(cands))]
	args := make([]string, len(f.params))
	for i, p := range f.params {
		args[i] = g.expr(p)
	}
	return f.name + "(" + strings.Join(args, ", ") + ")", true
}

func (g *progGen) intExpr() string {
	switch g.intn(16) {
	case 0, 1:
		ops := []string{"+", "-", "*", "/", "%"}
		return "(" + g.expr(tInt) + " " + ops[g.intn(len(ops))] + " " + g.expr(tInt) + ")"
	case 2:
		return "len(" + g.expr(tList) + ")"
	case 3:
		return "if " + g.expr(tBool) + " { " + g.expr(tInt) + " } else { " + g.expr(tInt) + " }"
	case 4:
		v := g.fresh("s")
		some := g.withVar(v, tInt, func() string { return g.expr(tInt) })
		return "match " + g.expr(tOpt) + " { Some(" + v + ") => " + some + ", None => " + g.expr(tInt) + " }"
	case 5:
		a, x := g.fresh("a"), g.fresh("e")
		body := g.withVar(a, tInt, func() string { return g.withVar(x, tInt, func() string { return g.expr(tInt) }) })
		return "fold(" + g.expr(tList) + ", " + g.expr(tInt) + ", (" + a + ", " + x + ") => " + body + ")"
	case 6:
		return "count(" + g.expr(tList) + ", " + g.lambda(tInt, tBool) + ")"
	case 7:
		if s, ok := g.callFn(tInt); ok {
			return s
		}
		return "-" + g.expr(tInt)
	case 8:
		return "probe(" + g.expr(tInt) + ")"
	case 9:
		if g.chance(70) {
			return g.expr(tList) + "[" + fmt.Sprint(g.intn(2)) + "]"
		}
		return g.expr(tList) + "[" + g.expr(tInt) + "]"
	case 10:
		v := g.fresh("b")
		inner := g.withVar(v, tInt, func() string { return g.expr(tInt) })
		return "{ let " + v + " = " + g.expr(tInt) + "; " + inner + " }"
	case 11:
		w, h := g.fresh("w"), g.fresh("h")
		boxArm := g.withVar(w, tInt, func() string { return g.withVar(h, tInt, func() string { return g.expr(tInt) }) })
		lineArm := g.withVar(w, tInt, func() string { return g.expr(tInt) })
		shape := []string{"Shape::Dot", "Shape::Line(" + g.expr(tInt) + ")", "Shape::Box(" + g.expr(tInt) + ", " + g.expr(tInt) + ")"}[g.intn(3)]
		return "match " + shape + " { Shape::Box(" + w + ", " + h + ") if " + w + " > " + h + " => " + boxArm +
			", Shape::Box(_, " + h + ") => " + h + ", Shape::Line(" + w + ") => " + lineArm + ", Shape::Dot => 0 }"
	case 12:
		return "Acc { label: " + g.expr(tStr) + ", total: " + g.expr(tInt) + " }.total"
	case 13:
		f := g.fresh("c")
		lam := g.lambda(tInt, tInt)
		return "{ let " + f + " = " + lam + "; " + f + "(" + g.expr(tInt) + ") }"
	case 14:
		return "len(" + g.expr(tStr) + ")"
	}
	return "match parse_int(to_string(" + g.expr(tInt) + ")) { Some(q) => q, None => 0 }"
}

func (g *progGen) boolExpr() string {
	switch g.intn(9) {
	case 0:
		ops := []string{"<", "<=", ">", ">=", "==", "!="}
		return "(" + g.expr(tInt) + " " + ops[g.intn(len(ops))] + " " + g.expr(tInt) + ")"
	case 1:
		return "(" + g.expr(tBool) + " && " + g.expr(tBool) + ")"
	case 2:
		return "(" + g.expr(tBool) + " || " + g.expr(tBool) + ")"
	case 3:
		return "!" + g.expr(tBool)
	case 4:
		return "any(" + g.expr(tList) + ", " + g.lambda(tInt, tBool) + ")"
	case 5:
		return "all(" + g.expr(tList) + ", " + g.lambda(tInt, tBool) + ")"
	case 6:
		return "(" + g.expr(tInt) + " in " + g.expr(tList) + ")"
	case 7:
		return "starts_with(" + g.expr(tStr) + ", " + g.expr(tStr) + ")"
	}
	return "(" + g.expr(tStr) + " == " + g.expr(tStr) + ")"
}

func (g *progGen) strExpr() string {
	switch g.intn(8) {
	case 0:
		return `"<{` + g.expr(tInt) + `}|{` + g.expr(tBool) + `}|{` + g.expr(tStr) + `}>"`
	case 1:
		return "(" + g.expr(tStr) + " + " + g.expr(tStr) + ")"
	case 2:
		return "to_string(" + g.expr(tInt) + ")"
	case 3:
		return "join(" + g.expr(tList) + " |> map(" + g.lambda(tInt, tStr) + "), \",\")"
	case 4:
		return "upper(" + g.expr(tStr) + ")"
	case 5:
		return "trim(replace(" + g.expr(tStr) + ", \"a\", \"b\"))"
	case 6:
		return "if " + g.expr(tBool) + " { " + g.expr(tStr) + " } else { " + g.expr(tStr) + " }"
	}
	return "join(split(" + g.expr(tStr) + ", \",\") |> reverse, \"-\")"
}

func (g *progGen) listExpr() string {
	switch g.intn(13) {
	case 0:
		return g.expr(tList) + " |> map(" + g.lambda(tInt, tInt) + ")"
	case 1:
		return g.expr(tList) + " |> filter(" + g.lambda(tInt, tBool) + ")"
	case 2:
		return g.expr(tList) + " |> filter_map(" + g.lambda(tInt, tOpt) + ")"
	case 3:
		v := g.fresh("m")
		body := g.withVar(v, tInt, func() string { return "[" + v + ", " + g.expr(tInt) + "]" })
		return g.expr(tList) + " |> flat_map(" + v + " => " + body + ")"
	case 4:
		return g.expr(tList) + " |> sort_by(" + g.lambda(tInt, tInt) + ")"
	case 5:
		return "take(" + g.expr(tList) + ", " + g.expr(tInt) + ")"
	case 6:
		return "skip(" + g.expr(tList) + ", " + g.expr(tInt) + ")"
	case 7:
		return "concat(" + g.expr(tList) + ", " + g.expr(tList) + ")"
	case 8:
		return "reverse(dedupe(" + g.expr(tList) + "))"
	case 9:
		return g.expr(tList) + " |> par_map(" + g.lambda(tInt, tInt) + ", limit = 2)"
	case 10:
		if s, ok := g.callFn(tList); ok {
			return s
		}
		return g.leaf(tList)
	case 11:
		return g.expr(tList) + " |> map(probe)"
	}
	return fmt.Sprintf("(%d..%d)", g.intn(3), g.intn(6))
}

func (g *progGen) optExpr() string {
	switch g.intn(6) {
	case 0:
		return "Some(" + g.expr(tInt) + ")"
	case 1:
		return g.expr(tList) + " |> first(" + g.lambda(tInt, tBool) + ")"
	case 2:
		return "first(" + g.expr(tList) + ")"
	case 3:
		return "parse_int(" + g.expr(tStr) + ")"
	case 4:
		if s, ok := g.callFn(tOpt); ok {
			return s
		}
		return `parse_int("none")`
	}
	return "if " + g.expr(tBool) + " { " + g.expr(tOpt) + " } else { None }"
}
