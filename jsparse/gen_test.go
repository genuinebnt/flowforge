package jsparse

import (
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

// progGen generates syntactically valid ES2024 programs from the grammar
// (statements, declarations, classes, modules, every expression form),
// seeded by a core.Rng so a failing case reproduces from FF_SEED. It
// avoids every early error by construction: declared names are fresh,
// yield/await/super/new.target/private names appear only where allowed,
// break/continue only inside loops, sloppy-only syntax only outside strict
// code. Every `/` that starts a token is recorded in slashes (true =
// RegularExpressionLiteral, false = division) for JS-INV-05.
type progGen struct {
	r       core.Rng
	b       []byte
	module  bool
	depth   int
	names   int
	slashes []bool

	// newlines lets the generator break lines between tokens where no
	// restricted production is affected.
	newlines bool
	ctx      genCtx
}

// genCtx is the syntactic context the generator is in.
type genCtx struct {
	fn, gen, async, arrow, method, ctor, strict bool
	loop, sw                                    int
	labels                                      []string // loop labels in scope
	private                                     []string // private names of the enclosing class
}

func newProgGen(r core.Rng, module bool) *progGen {
	g := &progGen{r: r, module: module}
	g.ctx.strict = module
	g.ctx.async = module // top-level await
	g.newlines = g.chance(2)
	return g
}

func (g *progGen) n(k int) int      { return core.IntN(g.r, k) }
func (g *progGen) chance(k int) bool { return g.n(k) == 0 }

func (g *progGen) fresh() string {
	g.names++
	return "v" + strconv.Itoa(g.names)
}

// tok appends a token with the separators the printer would use plus
// random white space, comments and (when safe) line breaks.
func (g *progGen) tok(s string) {
	if n := len(g.b); n > 0 {
		last, first := g.b[n-1], s[0]
		if (isWordByte(last) && isWordByte(first)) || ((last == '+' || last == '-') && first == last) ||
			(last == '/' && (first == '/' || first == '*')) || (last == '<' && first == '!') || (last == '-' && first == '>') {
			g.b = append(g.b, ' ')
		}
		switch g.n(12) {
		case 0:
			g.b = append(g.b, ' ')
		case 1:
			g.b = append(g.b, "/*c*/"...)
		case 2:
			g.b = append(g.b, '\t')
		}
	}
	g.b = append(g.b, s...)
}

// brk may insert a line break (only called where one is harmless).
func (g *progGen) brk() {
	if g.newlines && g.chance(4) {
		if g.chance(5) {
			g.b = append(g.b, "//line\n"...)
		} else {
			g.b = append(g.b, '\n')
		}
	}
}

func (g *progGen) program() []byte {
	if g.module && g.chance(2) {
		g.importDecl()
	}
	if !g.module && g.chance(6) {
		g.tok(`"use strict"`)
		g.tok(";")
		g.ctx.strict = true
	}
	for k := g.n(6) + 1; k > 0; k-- {
		g.stmtListItem(true)
		g.b = append(g.b, '\n')
	}
	return g.b
}

// ---- statements ----

func (g *progGen) stmtListItem(top bool) {
	if g.depth > 6 {
		g.exprStmt()
		return
	}
	g.depth++
	defer func() { g.depth-- }()
	switch g.n(12) {
	case 0:
		g.functionDecl()
	case 1:
		g.classDecl()
	case 2, 3:
		g.lexicalDecl()
	case 4:
		if top && g.module {
			g.exportDecl()
			return
		}
		g.stmt()
	default:
		g.stmt()
	}
}

func (g *progGen) semi() {
	// ASI is exercised by the minified printer; the generator keeps
	// explicit semicolons except before a line break and a safe token.
	g.tok(";")
}

func (g *progGen) block() {
	g.tok("{")
	g.brk()
	for k := g.n(3); k > 0; k-- {
		g.stmtListItem(false)
		g.brk()
	}
	g.tok("}")
}

func (g *progGen) stmt() {
	if g.depth > 6 {
		g.exprStmt()
		return
	}
	g.depth++
	defer func() { g.depth-- }()
	switch g.n(20) {
	case 0:
		g.block()
	case 1:
		g.tok("var")
		g.bindingList(false)
		g.semi()
	case 2:
		g.tok("if")
		g.tok("(")
		g.expr(pSeq)
		g.tok(")")
		g.stmt()
		if g.chance(2) {
			g.tok("else")
			g.stmt()
		}
	case 3:
		g.forStmt()
	case 4:
		g.loop(func() {
			g.tok("while")
			g.tok("(")
			g.expr(pSeq)
			g.tok(")")
			g.stmt()
		})
	case 5:
		g.loop(func() {
			g.tok("do")
			g.stmt()
			g.tok("while")
			g.tok("(")
			g.expr(pSeq)
			g.tok(")")
			g.tok(";")
		})
	case 6:
		g.switchStmt()
	case 7:
		g.tryStmt()
	case 8:
		if g.ctx.loop > 0 || g.ctx.sw > 0 {
			g.tok("break")
			if len(g.ctx.labels) > 0 && g.chance(2) {
				g.tok(g.ctx.labels[g.n(len(g.ctx.labels))])
			}
			g.semi()
			return
		}
		g.exprStmt()
	case 9:
		if g.ctx.loop > 0 {
			g.tok("continue")
			if len(g.ctx.labels) > 0 && g.chance(2) {
				g.tok(g.ctx.labels[g.n(len(g.ctx.labels))])
			}
			g.semi()
			return
		}
		g.exprStmt()
	case 10:
		if g.ctx.fn {
			g.tok("return")
			if g.chance(3) {
				g.expr(pSeq)
			}
			g.semi()
			return
		}
		g.exprStmt()
	case 11:
		g.tok("throw")
		g.expr(pSeq)
		g.semi()
	case 12:
		l := "L" + g.fresh()
		g.tok(l)
		g.tok(":")
		saved := g.ctx.labels
		g.ctx.labels = append(append([]string(nil), saved...), l)
		g.loop(func() {
			g.tok("for")
			g.tok("(")
			g.tok(";")
			g.tok(";")
			g.tok(")")
			g.stmt()
		})
		g.ctx.labels = saved
	case 13:
		g.tok(";")
	case 14:
		if !g.ctx.strict {
			g.tok("with")
			g.tok("(")
			g.expr(pSeq)
			g.tok(")")
			g.stmt()
			return
		}
		g.tok("debugger")
		g.semi()
	default:
		g.exprStmt()
	}
}

func (g *progGen) loop(body func()) {
	g.ctx.loop++
	body()
	g.ctx.loop--
}

func (g *progGen) exprStmt() {
	mark := len(g.b)
	g.expr(pSeq)
	// ExpressionStatement lookahead: no leading `{`, function, class,
	// `let [` or async function.
	head := strings.TrimLeft(string(g.b[mark:]), " \t\n")
	head = strings.TrimPrefix(head, "/*c*/")
	head = strings.TrimLeft(head, " \t\n")
	if strings.HasPrefix(head, "{") || strings.HasPrefix(head, "function") || strings.HasPrefix(head, "class") ||
		strings.HasPrefix(head, "async") || strings.HasPrefix(head, "let") {
		inner := append([]byte(nil), g.b[mark:]...)
		g.b = append(g.b[:mark], '(')
		g.b = append(g.b, inner...)
		g.b = append(g.b, ')')
	}
	g.semi()
}

func (g *progGen) forStmt() {
	g.loop(func() {
		g.tok("for")
		switch g.n(4) {
		case 0:
			g.tok("(")
			if g.chance(2) {
				g.tok("let")
				g.bindingList(true)
			}
			g.tok(";")
			if g.chance(2) {
				g.expr(pSeq)
			}
			g.tok(";")
			if g.chance(2) {
				g.expr(pSeq)
			}
			g.tok(")")
		case 1, 2:
			awaitOf := g.ctx.async && g.n(3) == 0
			if awaitOf {
				g.tok("await")
			}
			g.tok("(")
			switch g.n(3) {
			case 0:
				g.tok([]string{"var", "let", "const"}[g.n(3)])
				g.bindingTarget(false)
			default:
				g.tok(g.fresh())
			}
			if awaitOf || g.chance(2) {
				g.tok("of")
				g.expr(pAssign)
			} else {
				g.tok("in")
				g.expr(pSeq)
			}
			g.tok(")")
		default:
			g.tok("(")
			g.tok("var")
			g.tok(g.fresh())
			g.tok("=")
			g.tok("0")
			g.tok(";")
			g.tok(";")
			g.tok(")")
		}
		g.stmt()
	})
}

func (g *progGen) switchStmt() {
	g.tok("switch")
	g.tok("(")
	g.expr(pSeq)
	g.tok(")")
	g.tok("{")
	g.ctx.sw++
	def := false
	for k := g.n(4); k > 0; k-- {
		if !def && g.chance(3) {
			def = true
			g.tok("default")
		} else {
			g.tok("case")
			g.expr(pSeq)
		}
		g.tok(":")
		for j := g.n(3); j > 0; j-- {
			g.stmt()
		}
	}
	g.ctx.sw--
	g.tok("}")
}

func (g *progGen) tryStmt() {
	g.tok("try")
	g.block()
	fin := g.chance(3)
	if !fin || g.chance(2) {
		g.tok("catch")
		if g.chance(4) {
			g.block()
		} else {
			g.tok("(")
			g.bindingTarget(false)
			g.tok(")")
			g.block()
		}
	}
	if fin {
		g.tok("finally")
		g.block()
	}
}

func (g *progGen) lexicalDecl() {
	if g.chance(2) {
		g.tok("let")
		g.bindingList(false)
	} else {
		g.tok("const")
		g.bindingList(true)
	}
	g.semi()
}

// bindingList emits declarators; withInit forces initializers.
func (g *progGen) bindingList(withInit bool) {
	for k := g.n(2) + 1; k > 0; k-- {
		pattern := g.chance(3)
		if pattern {
			g.bindingTarget(true)
		} else {
			g.tok(g.fresh())
		}
		if withInit || pattern || g.chance(2) {
			g.tok("=")
			g.expr(pAssign)
		}
		if k > 1 {
			g.tok(",")
		}
	}
}

// bindingTarget emits a BindingIdentifier or BindingPattern.
func (g *progGen) bindingTarget(allowPattern bool) {
	if !allowPattern || g.depth > 5 || g.chance(2) {
		g.tok(g.fresh())
		return
	}
	g.depth++
	defer func() { g.depth-- }()
	if g.chance(2) {
		g.tok("[")
		for k := g.n(3); k > 0; k-- {
			if g.chance(4) {
				g.tok(",")
				continue
			}
			g.bindingElement()
			g.tok(",")
		}
		if g.chance(3) {
			g.tok("...")
			g.bindingTarget(true)
		}
		g.tok("]")
		return
	}
	g.tok("{")
	for k := g.n(3); k > 0; k-- {
		switch g.n(3) {
		case 0:
			g.tok(g.fresh())
			if g.chance(2) {
				g.tok("=")
				g.expr(pAssign)
			}
		case 1:
			g.propName()
			g.tok(":")
			g.bindingElement()
		default:
			g.tok("[")
			g.expr(pAssign)
			g.tok("]")
			g.tok(":")
			g.bindingElement()
		}
		g.tok(",")
	}
	if g.chance(3) {
		g.tok("...")
		g.tok(g.fresh())
	}
	g.tok("}")
}

func (g *progGen) bindingElement() {
	g.bindingTarget(true)
	if g.chance(3) {
		g.tok("=")
		g.expr(pAssign)
	}
}

func (g *progGen) params() {
	g.tok("(")
	for k := g.n(3); k > 0; k-- {
		g.bindingElement()
		g.tok(",")
	}
	if g.chance(4) {
		g.tok("...")
		g.bindingTarget(true)
	}
	g.tok(")")
}

// withFn runs body in a function context.
func (g *progGen) withFn(async, gen, arrow, method, ctor bool, body func()) {
	saved := g.ctx
	g.ctx.async, g.ctx.gen, g.ctx.arrow = async, gen, arrow
	if !arrow {
		g.ctx.method, g.ctx.ctor = method, ctor
	}
	g.ctx.fn = true
	g.ctx.loop, g.ctx.sw, g.ctx.labels = 0, 0, nil
	body()
	saved.private = g.ctx.private
	g.ctx = saved
}

func (g *progGen) functionBody() {
	g.tok("{")
	if !g.ctx.strict && g.chance(8) {
		g.tok(`"use strict"`)
		g.tok(";")
		g.ctx.strict = true
	}
	for k := g.n(3); k > 0; k-- {
		g.stmtListItem(false)
	}
	g.tok("}")
}

func (g *progGen) functionDecl() {
	async, gen := g.chance(3), g.chance(3)
	if async {
		g.tok("async")
	}
	g.tok("function")
	if gen {
		g.tok("*")
	}
	g.tok(g.fresh())
	// Parameters of a function whose body may say "use strict" must be
	// simple; functionBody only adds the directive in sloppy code, so keep
	// patterns for strict contexts.
	g.withFn(async, gen, false, false, false, func() {
		if g.ctx.strict {
			g.params()
		} else {
			g.simpleParams()
		}
		g.functionBody()
	})
}

func (g *progGen) simpleParams() {
	g.tok("(")
	for k := g.n(3); k > 0; k-- {
		g.tok(g.fresh())
		if k > 1 {
			g.tok(",")
		}
	}
	g.tok(")")
}

func (g *progGen) classDecl() {
	g.tok("class")
	g.tok(g.fresh())
	g.classTail()
}

func (g *progGen) classTail() {
	derived := g.chance(2)
	if derived {
		g.tok("extends")
		g.primary()
	}
	saved := g.ctx
	g.ctx.strict = true
	var priv []string
	for k := g.n(3); k > 0; k-- {
		priv = append(priv, "p"+g.fresh())
	}
	g.ctx.private = priv
	g.tok("{")
	if g.chance(2) {
		g.tok("constructor")
		g.withFn(false, false, false, true, derived, func() {
			g.params()
			g.tok("{")
			if derived {
				g.tok("super")
				g.args()
				g.tok(";")
			}
			g.stmt()
			g.tok("}")
		})
	}
	for _, name := range priv {
		if g.chance(2) {
			g.tok("#" + name)
			if g.chance(2) {
				g.tok("=")
				g.fieldInit()
			}
			g.tok(";")
		} else {
			g.method("#" + name)
		}
	}
	for k := g.n(4); k > 0; k-- {
		switch g.n(5) {
		case 0:
			if g.chance(2) {
				g.tok("static")
			}
			g.tok(g.fresh())
			if g.chance(2) {
				g.tok("=")
				g.fieldInit()
			}
			g.tok(";")
		case 1:
			g.tok("static")
			g.withFn(false, false, false, true, false, func() {
				g.ctx.fn = false
				g.block()
			})
		case 2:
			if g.chance(2) {
				g.tok("static")
			}
			g.tok("[")
			g.expr(pAssign)
			g.tok("]")
			g.tok("=")
			g.fieldInit()
			g.tok(";")
		default:
			if g.chance(3) {
				g.tok("static")
			}
			g.method(g.fresh())
		}
	}
	g.tok("}")
	saved.private = saved.private[:len(saved.private):len(saved.private)]
	g.ctx = saved
}

func (g *progGen) fieldInit() {
	g.withFn(false, false, true, true, false, func() {
		g.ctx.fn = false
		g.ctx.async = false
		g.expr(pAssign)
	})
}

// method emits a class or object method, accessor or generator named name.
func (g *progGen) method(name string) {
	switch g.n(4) {
	case 0:
		g.tok("get")
		g.tok(name)
		g.withFn(false, false, false, true, false, func() {
			g.tok("(")
			g.tok(")")
			g.functionBody()
		})
	case 1:
		g.tok("set")
		g.tok(name)
		g.withFn(false, false, false, true, false, func() {
			g.tok("(")
			g.tok(g.fresh())
			g.tok(")")
			g.functionBody()
		})
	default:
		async, gen := g.chance(3), g.chance(3)
		if async {
			g.tok("async")
		}
		if gen {
			g.tok("*")
		}
		g.tok(name)
		g.withFn(async, gen, false, true, false, func() {
			g.params()
			g.tok("{")
			for k := g.n(3); k > 0; k-- {
				g.stmtListItem(false)
			}
			g.tok("}")
		})
	}
}

// ---- modules ----

func (g *progGen) moduleString() string {
	return `"./m` + strconv.Itoa(g.n(5)) + `.js"`
}

func (g *progGen) importDecl() {
	g.tok("import")
	switch g.n(5) {
	case 0:
		g.tok(g.moduleString())
	case 1:
		g.tok(g.fresh())
		g.tok("from")
		g.tok(g.moduleString())
	case 2:
		g.tok("*")
		g.tok("as")
		g.tok(g.fresh())
		g.tok("from")
		g.tok(g.moduleString())
	default:
		g.tok("{")
		for k := g.n(3) + 1; k > 0; k-- {
			if g.chance(3) {
				g.tok(`"str` + strconv.Itoa(g.n(9)) + `"`)
				g.tok("as")
			} else if g.chance(2) {
				g.tok("default")
				g.tok("as")
			}
			g.tok(g.fresh())
			g.tok(",")
		}
		g.tok("}")
		g.tok("from")
		g.tok(g.moduleString())
		if g.chance(3) {
			g.tok("with")
			g.tok("{")
			g.tok("type")
			g.tok(":")
			g.tok(`"json"`)
			g.tok("}")
		}
	}
	g.tok(";")
	g.b = append(g.b, '\n')
}

func (g *progGen) exportDecl() {
	g.tok("export")
	switch g.n(5) {
	case 0:
		g.tok("*")
		if g.chance(2) {
			g.tok("as")
			g.tok(g.fresh())
		}
		g.tok("from")
		g.tok(g.moduleString())
		g.tok(";")
	case 1:
		g.tok("const")
		g.tok(g.fresh())
		g.tok("=")
		g.expr(pAssign)
		g.tok(";")
	case 2:
		g.functionDecl()
	case 3:
		g.classDecl()
	default:
		g.tok("{")
		g.tok("x" + g.fresh())
		g.tok("as")
		g.tok(`"e` + g.fresh() + `"`)
		g.tok("}")
		g.tok("from")
		g.tok(g.moduleString())
		g.tok(";")
	}
}

// ---- expressions ----

// expr emits an expression whose precedence is at least min.
func (g *progGen) expr(min int) {
	if g.depth > 7 {
		g.atom()
		return
	}
	g.depth++
	defer func() { g.depth-- }()
	choice := g.n(30)
	var prec int
	switch {
	case choice < 10:
		prec = pPrimary
	case choice < 13:
		prec = pCall
	case choice < 15:
		prec = pUnary
	case choice < 21:
		prec = pBitOr + g.n(pExp-pBitOr+1) // a binary level
	case choice < 23:
		prec = pCoalesce + g.n(3) // ?? || &&
	case choice < 25:
		prec = pCond
	case choice < 29:
		prec = pAssign
	default:
		prec = pSeq
	}
	paren := prec < min
	if paren {
		g.tok("(")
		g.brk()
	}
	g.exprAt(prec)
	if paren {
		g.tok(")")
	}
}

func (g *progGen) exprAt(prec int) {
	switch prec {
	case pSeq:
		g.expr(pAssign)
		g.tok(",")
		g.brk()
		g.expr(pAssign)
	case pAssign:
		g.assignment()
	case pCond:
		g.expr(pCoalesce + 1)
		g.tok("?")
		g.expr(pAssign)
		g.tok(":")
		g.expr(pAssign)
	case pCoalesce:
		g.expr(pBitOr)
		g.tok("??")
		g.brk()
		g.expr(pBitOr)
	case pOr, pAnd:
		op := map[int]string{pOr: "||", pAnd: "&&"}[prec]
		g.expr(prec)
		g.tok(op)
		g.brk()
		g.expr(prec + 1)
	case pUnary:
		g.unary()
	case pCall:
		g.callOrMember()
	case pPrimary:
		g.primary()
	default:
		g.binary(prec)
	}
}

var binaryOpsByPrec = map[int][]string{
	pBitOr: {"|"}, pBitXor: {"^"}, pBitAnd: {"&"}, pEquality: {"==", "!=", "===", "!=="},
	pRelational: {"<", ">", "<=", ">=", "instanceof", "in"}, pShift: {"<<", ">>", ">>>"},
	pAdditive: {"+", "-"}, pMultiplicative: {"*", "/", "%"}, pExp: {"**"},
}

func (g *progGen) binary(prec int) {
	ops := binaryOpsByPrec[prec]
	op := ops[g.n(len(ops))]
	if prec == pExp {
		g.expr(pUpdate) // UpdateExpression ** ExponentiationExpression
		g.tok("**")
		g.expr(pExp)
		return
	}
	g.expr(prec)
	if op == "/" {
		g.slashes = append(g.slashes, false)
	}
	g.tok(op)
	g.brk()
	g.expr(prec + 1)
}

func (g *progGen) unary() {
	switch g.n(9) {
	case 0:
		if g.ctx.async && !g.ctx.arrow {
			g.tok("await")
			g.expr(pUnary)
			return
		}
		g.tok("typeof")
		g.expr(pUnary)
	case 1:
		g.tok("delete")
		g.member()
	case 2:
		g.tok("++")
		g.target()
	case 3:
		g.target()
		g.tok("--")
	default:
		g.tok([]string{"!", "~", "+", "-", "void", "typeof"}[g.n(6)])
		g.expr(pUnary)
	}
}

// target emits a simple assignment target.
func (g *progGen) target() {
	if g.chance(2) {
		g.tok(g.ref())
		return
	}
	g.member()
}

func (g *progGen) member() {
	g.primaryNoArrow()
	g.tok(".")
	g.tok(g.propIdent())
}

func (g *progGen) assignment() {
	switch g.n(8) {
	case 0:
		if g.ctx.gen && !g.ctx.arrow {
			g.tok("yield")
			if g.chance(2) {
				if g.chance(3) {
					g.tok("*")
				}
				g.expr(pAssign)
			}
			return
		}
		fallthrough
	case 1:
		g.arrow()
	case 2:
		// destructuring assignment
		g.tok("[")
		g.target()
		g.tok(",")
		g.tok("...")
		g.target()
		g.tok("]")
		g.tok("=")
		g.expr(pAssign)
	default:
		g.target()
		g.tok([]string{"=", "+=", "-=", "*=", "/=", "%=", "**=", "<<=", ">>=", ">>>=", "&=", "|=", "^=", "&&=", "||=", "??="}[g.n(16)])
		if strings.HasPrefix(string(g.b[len(g.b)-3:]), "/=") || strings.HasSuffix(string(g.b), "/=") {
			g.slashes = append(g.slashes, false)
		}
		g.brk()
		g.expr(pAssign)
	}
}

func (g *progGen) arrow() {
	async := g.chance(3)
	if async {
		g.tok("async")
	}
	g.withFn(async, false, true, false, false, func() {
		if !async && g.chance(3) {
			g.tok(g.fresh())
		} else {
			g.params()
		}
		g.tok("=>")
		if g.chance(2) {
			g.functionBody()
		} else {
			mark := len(g.b)
			g.expr(pAssign)
			if head := strings.TrimLeft(string(g.b[mark:]), " \t"); strings.HasPrefix(head, "{") || strings.HasPrefix(head, "/*c*/{") {
				inner := append([]byte(nil), g.b[mark:]...)
				g.b = append(append(g.b[:mark], '('), inner...)
				g.b = append(g.b, ')')
			}
		}
	})
}

func (g *progGen) callOrMember() {
	g.primaryNoArrow()
	for k := g.n(3) + 1; k > 0; k-- {
		switch g.n(7) {
		case 0:
			g.tok(".")
			g.tok(g.propIdent())
		case 1:
			g.tok("[")
			g.expr(pSeq)
			g.tok("]")
		case 2:
			g.tok("?.")
			g.tok(g.propIdent())
		case 3:
			g.tok("?.")
			g.args()
		case 4:
			g.template(true)
			return // a tagged template may not follow an optional chain
		case 5:
			if len(g.ctx.private) > 0 {
				g.tok(".")
				g.tok("#" + g.ctx.private[g.n(len(g.ctx.private))])
				continue
			}
			fallthrough
		default:
			g.args()
		}
	}
}

func (g *progGen) args() {
	g.tok("(")
	for k := g.n(3); k > 0; k-- {
		if g.chance(5) {
			g.tok("...")
		}
		g.expr(pAssign)
		g.tok(",")
		g.brk()
	}
	g.tok(")")
}

func (g *progGen) ref() string {
	switch g.n(4) {
	case 0:
		return "g" + strconv.Itoa(g.n(10))
	case 1:
		return "$x"
	case 2:
		return "_y"
	}
	return "z" + strconv.Itoa(g.n(4))
}

func (g *progGen) propIdent() string {
	return []string{"a", "b", "if", "class", "new", "default", "length", "then", "été", "of"}[g.n(10)]
}

func (g *progGen) propName() {
	switch g.n(4) {
	case 0:
		g.tok(`"key` + strconv.Itoa(g.n(9)) + `"`)
	case 1:
		g.tok(strconv.Itoa(g.n(100)))
	default:
		g.tok(g.propIdent())
	}
}

// primaryNoArrow emits a primary expression usable as an operand.
func (g *progGen) primaryNoArrow() {
	if g.chance(3) {
		g.atom()
		return
	}
	g.primary()
}

func (g *progGen) primary() {
	if g.depth > 7 {
		g.atom()
		return
	}
	g.depth++
	defer func() { g.depth-- }()
	switch g.n(14) {
	case 0:
		g.tok("[")
		for k := g.n(4); k > 0; k-- {
			if g.chance(5) {
				g.tok(",")
				continue
			}
			if g.chance(5) {
				g.tok("...")
			}
			g.expr(pAssign)
			g.tok(",")
		}
		g.tok("]")
	case 1:
		g.object()
	case 2:
		async, gen := g.chance(3), g.chance(3)
		if async {
			g.tok("async")
		}
		g.tok("function")
		if gen {
			g.tok("*")
		}
		if g.chance(2) {
			g.tok(g.fresh())
		}
		g.withFn(async, gen, false, false, false, func() {
			g.params()
			g.tok("{")
			for k := g.n(3); k > 0; k-- {
				g.stmtListItem(false)
			}
			if g.chance(3) {
				g.tok("new")
				g.tok(".")
				g.tok("target")
				g.tok(";")
			}
			g.tok("}")
		})
	case 3:
		g.tok("class")
		if g.chance(2) {
			g.tok(g.fresh())
		}
		g.classTail()
	case 4:
		g.template(false)
	case 5:
		g.tok("new")
		g.atom()
		if g.chance(2) {
			g.args()
		}
	case 6:
		if g.ctx.method && !g.ctx.ctor {
			g.tok("super")
			g.tok(".")
			g.tok(g.propIdent())
			return
		}
		if g.module {
			g.tok("import")
			g.tok(".")
			g.tok("meta")
			return
		}
		g.tok("this")
	case 7:
		g.tok("import")
		g.tok("(")
		g.expr(pAssign)
		g.tok(")")
	case 8:
		g.tok("(")
		g.expr(pSeq)
		g.tok(")")
	default:
		g.atom()
	}
}

func (g *progGen) object() {
	g.tok("{")
	proto := false
	for k := g.n(4); k > 0; k-- {
		switch g.n(8) {
		case 0:
			g.tok(g.ref())
		case 1:
			g.tok("...")
			g.expr(pAssign)
		case 2:
			g.tok("[")
			g.expr(pAssign)
			g.tok("]")
			g.tok(":")
			g.expr(pAssign)
		case 3:
			g.withFn(false, false, false, true, false, func() {
				g.method(g.propIdent())
			})
		case 4:
			if !proto {
				proto = true
				g.tok("__proto__")
				g.tok(":")
				g.expr(pAssign)
				break
			}
			fallthrough
		default:
			g.propName()
			g.tok(":")
			g.expr(pAssign)
		}
		g.tok(",")
		g.brk()
	}
	g.tok("}")
}

func (g *progGen) template(tagged bool) {
	parts := g.n(3)
	var b strings.Builder
	b.WriteByte('`')
	chunk := func() {
		switch g.n(6) {
		case 0:
			b.WriteString(`\n\u{1F600}\x41`)
		case 1:
			b.WriteString("line\nbreak")
		case 2:
			if tagged {
				b.WriteString(`\unicode`) // NotEscapeSequence: cooked undefined
			} else {
				b.WriteString(`\``)
			}
		case 3:
			b.WriteString("$ {not}")
		default:
			b.WriteString("txt")
		}
	}
	chunk()
	g.tok(b.String())
	for i := 0; i < parts; i++ {
		g.b = append(g.b, "${"...)
		g.expr(pSeq)
		b.Reset()
		b.WriteByte('}')
		chunk()
		g.b = append(g.b, b.String()...)
	}
	g.b = append(g.b, '`')
}

var genRegexps = []string{
	`/a+b?/g`, `/[/]/`, `/[a-z]{2,3}/i`, `/(?<year>\d{4})-\k<year>/u`, `/\p{L}+/u`, `/[\p{L}--[a-z]]/v`,
	`/(?:x|y)*?/dgimsy`, `/^\/\*.*\*\/$/m`, `/a(?=b)(?!c)(?<=d)(?<!e)/`, `/\u{1F600}/u`, `/=x/`,
}

func (g *progGen) atom() {
	switch g.n(12) {
	case 0:
		nums := []string{"0", "42", "3.25", ".5", "1e3", "1E-7", "0x1F", "0o17", "0b101", "1_000_000", "12n", "0xFFn", "9007199254740993"}
		if !g.ctx.strict {
			nums = append(nums, "017", "08", "09.5")
		}
		g.tok(nums[g.n(len(nums))])
	case 1:
		strs := []string{`"s"`, `'it''s'`, `"\u{1F600}\x41\n"`, `'éé'`, "\"line\\\ncont\"", `""`, "' '"}
		if !g.ctx.strict {
			strs = append(strs, `"\101\8"`)
		}
		g.tok(strs[g.n(len(strs))])
	case 2:
		g.slashes = append(g.slashes, true)
		g.tok(genRegexps[g.n(len(genRegexps))])
	case 3:
		g.tok([]string{"true", "false", "null", "this"}[g.n(4)])
	default:
		g.tok(g.ref())
	}
}

// genProgram returns one generated program and its `/` labels.
func genProgram(r core.Rng, module bool) ([]byte, []bool) {
	g := newProgGen(r, module)
	src := g.program()
	return src, g.slashes
}
