package jsparse

import (
	"math"
	"strings"
)

// PrintOptions selects the printer's layout.
type PrintOptions struct {
	// Minify emits no optional white space or line breaks: tokens are
	// separated only where they would otherwise merge. It stresses ASI
	// and regex/division decisions when the output is re-parsed.
	Minify bool
}

// Print renders prog as ECMAScript source. The output re-parses (with the
// same SourceType) to a tree whose canonical JSON equals prog's
// (JS-INV-03). Parentheses are inserted exactly where precedence, the
// statement-start lookahead restrictions, `in` in for heads, the `??`
// mixing rule, the `**` unary rule or the directive prologue require
// them; every NParenthesized hint is otherwise ignored.
func Print(prog *Program, opts PrintOptions) []byte {
	w := &printer{p: prog, min: opts.Minify, out: make([]byte, 0, len(prog.Source)+64)}
	if prog.Root != NoNode && int(prog.Root) < len(prog.Nodes) {
		w.statementList(prog.Nodes[prog.Root].List, true)
	}
	return w.out
}

// Expression precedence levels used by the printer (higher binds
// tighter).
const (
	pSeq = iota + 1
	pAssign
	pCond
	pCoalesce
	pOr
	pAnd
	pBitOr
	pBitXor
	pBitAnd
	pEquality
	pRelational
	pShift
	pAdditive
	pMultiplicative
	pExp
	pUnary
	pUpdate
	pNew
	pCall
	pPrimary
)

type printer struct {
	p      *Program
	min    bool
	out    []byte
	indent int
	// afterRegex is set right after a regular expression literal, whose
	// flags would absorb a following identifier character.
	afterRegex bool
}

func (w *printer) n(id NodeID) *Node { return &w.p.Nodes[id] }

func (w *printer) list(l NodeList) []NodeID { return w.p.Children[l.Start : l.Start+l.Len] }

// tok appends a token, inserting a space when the previous token would
// otherwise merge with it (identifier characters, `+ +`, `- -`, `/ /`,
// `<!`, `-->`) — the only spaces minified output needs.
func (w *printer) tok(s string) {
	if s == "" {
		return
	}
	if n := len(w.out); n > 0 {
		last, first := w.out[n-1], s[0]
		need := false
		switch {
		case isWordByte(last) && isWordByte(first):
			need = true
		case (last == '+' || last == '-') && first == last:
			need = true
		case last == '/' && (first == '/' || first == '*'):
			need = true
		case last == '<' && first == '!':
			need = true
		case last == '-' && first == '>' && n > 1 && w.out[n-2] == '-':
			need = true
		case last == '.' && isDecimalDigit(first):
			need = true
		case w.afterRegex && isWordByte(first):
			need = true
		}
		if need {
			w.out = append(w.out, ' ')
		}
	}
	w.out = append(w.out, s...)
	w.afterRegex = false
}

// isWordByte reports bytes that continue an identifier or number,
// including any non-ASCII byte (conservatively).
func isWordByte(c byte) bool {
	return c >= 0x80 || c == '$' || c == '_' || c == '\\' || (c >= '0' && c <= '9') || (c|0x20 >= 'a' && c|0x20 <= 'z')
}

// sp emits an optional space (readable mode only).
func (w *printer) sp() {
	if !w.min {
		w.out = append(w.out, ' ')
	}
}

func (w *printer) newline() {
	if w.min {
		return
	}
	w.out = append(w.out, '\n')
	for i := 0; i < w.indent; i++ {
		w.out = append(w.out, ' ', ' ')
	}
}

// ---- statements ----

// statementList prints a body; with prologue, leading string-literal
// expression statements that are not directives are parenthesized so they
// do not become directives.
func (w *printer) statementList(l NodeList, prologue bool) {
	for i, id := range w.list(l) {
		if i > 0 || len(w.out) > 0 {
			w.newline()
		}
		if prologue {
			n := w.n(id)
			if n.Kind == KExpressionStatement && n.Flags&NDirective != 0 {
				w.directive(id)
				continue
			}
			prologue = false
			if n.Kind == KExpressionStatement && n.A != NoNode && w.n(n.A).Kind == KLiteral && w.n(n.A).Op == LitString {
				w.tok("(")
				w.expr(n.A, pSeq)
				w.tok(")")
				w.tok(";")
				continue
			}
		}
		w.stmt(id)
	}
}

func (w *printer) directive(id NodeID) {
	e := w.n(w.n(id).A)
	w.tok(w.p.Source[e.Span.Start:e.Span.End])
	w.tok(";")
}

func (w *printer) block(l NodeList, prologue bool) {
	w.tok("{")
	if l.Len > 0 {
		w.indent++
		if !w.min {
			w.newline()
			// statementList starts each item after the first with a newline;
			// the first is placed here.
			first := true
			for _, id := range w.list(l) {
				if !first {
					w.newline()
				}
				first = false
				w.stmtInList(id, &prologue)
			}
		} else {
			for _, id := range w.list(l) {
				w.stmtInList(id, &prologue)
			}
		}
		w.indent--
		w.newline()
	}
	w.tok("}")
}

func (w *printer) stmtInList(id NodeID, prologue *bool) {
	if *prologue {
		n := w.n(id)
		if n.Kind == KExpressionStatement && n.Flags&NDirective != 0 {
			w.directive(id)
			return
		}
		*prologue = false
		if n.Kind == KExpressionStatement && n.A != NoNode && w.n(n.A).Kind == KLiteral && w.n(n.A).Op == LitString {
			w.tok("(")
			w.expr(n.A, pSeq)
			w.tok(")")
			w.tok(";")
			return
		}
	}
	w.stmt(id)
}

// stmt prints one statement.
func (w *printer) stmt(id NodeID) {
	if id == NoNode {
		w.tok(";")
		return
	}
	n := w.n(id)
	switch n.Kind {
	case KExpressionStatement:
		if n.Flags&NDirective != 0 {
			w.directive(id)
			return
		}
		if n.A == NoNode {
			w.tok(";")
			return
		}
		if w.startsBadly(n.A, true) {
			w.tok("(")
			w.expr(n.A, pSeq)
			w.tok(")")
		} else {
			w.expr(n.A, pSeq)
		}
		w.tok(";")
	case KBlockStatement:
		w.block(n.List, false)
	case KStaticBlock:
		w.tok("static")
		w.sp()
		w.block(n.List, false)
	case KEmptyStatement:
		w.tok(";")
	case KDebuggerStatement:
		w.tok("debugger")
		w.tok(";")
	case KWithStatement:
		w.tok("with")
		w.sp()
		w.tok("(")
		w.expr(n.A, pSeq)
		w.tok(")")
		w.sp()
		w.stmt(n.B)
	case KReturnStatement, KThrowStatement:
		if n.Kind == KReturnStatement {
			w.tok("return")
		} else {
			w.tok("throw")
		}
		if n.A != NoNode {
			w.sp()
			w.expr(n.A, pSeq)
		}
		w.tok(";")
	case KLabeledStatement:
		w.expr(n.A, pPrimary)
		w.tok(":")
		w.sp()
		w.stmt(n.B)
	case KBreakStatement, KContinueStatement:
		if n.Kind == KBreakStatement {
			w.tok("break")
		} else {
			w.tok("continue")
		}
		if n.A != NoNode {
			w.tok(w.n(n.A).Str)
		}
		w.tok(";")
	case KIfStatement:
		w.tok("if")
		w.sp()
		w.tok("(")
		w.expr(n.A, pSeq)
		w.tok(")")
		w.sp()
		if n.C != NoNode && w.endsWithOpenIf(n.B) {
			w.tok("{")
			w.stmt(n.B)
			w.tok("}")
		} else {
			w.stmt(n.B)
		}
		if n.C != NoNode {
			w.sp()
			w.tok("else")
			w.sp()
			w.stmt(n.C)
		}
	case KSwitchStatement:
		w.tok("switch")
		w.sp()
		w.tok("(")
		w.expr(n.A, pSeq)
		w.tok(")")
		w.sp()
		w.tok("{")
		w.indent++
		for _, c := range w.list(n.List) {
			cn := w.n(c)
			w.newline()
			if cn.A == NoNode {
				w.tok("default")
			} else {
				w.tok("case")
				w.sp()
				w.expr(cn.A, pSeq)
			}
			w.tok(":")
			w.indent++
			for _, s := range w.list(cn.List) {
				w.newline()
				w.stmt(s)
			}
			w.indent--
		}
		w.indent--
		w.newline()
		w.tok("}")
	case KTryStatement:
		w.tok("try")
		w.sp()
		w.stmt(n.A)
		if n.B != NoNode {
			h := w.n(n.B)
			w.sp()
			w.tok("catch")
			if h.A != NoNode {
				w.sp()
				w.tok("(")
				w.pattern(h.A)
				w.tok(")")
			}
			w.sp()
			w.stmt(h.B)
		}
		if n.C != NoNode {
			w.sp()
			w.tok("finally")
			w.sp()
			w.stmt(n.C)
		}
	case KWhileStatement:
		w.tok("while")
		w.sp()
		w.tok("(")
		w.expr(n.A, pSeq)
		w.tok(")")
		w.sp()
		w.stmt(n.B)
	case KDoWhileStatement:
		w.tok("do")
		w.sp()
		w.stmt(n.A)
		w.sp()
		w.tok("while")
		w.sp()
		w.tok("(")
		w.expr(n.B, pSeq)
		w.tok(")")
		w.tok(";")
	case KForStatement:
		w.tok("for")
		w.sp()
		w.tok("(")
		if n.A != NoNode {
			w.forInit(n.A)
		}
		w.tok(";")
		if n.B != NoNode {
			w.sp()
			w.expr(n.B, pSeq)
		}
		w.tok(";")
		if n.C != NoNode {
			w.sp()
			w.expr(n.C, pSeq)
		}
		w.tok(")")
		w.sp()
		w.stmt(n.D)
	case KForInStatement, KForOfStatement:
		w.tok("for")
		if n.Flags&NAwait != 0 {
			w.tok("await")
		}
		w.sp()
		w.tok("(")
		left := w.n(n.A)
		if left.Kind == KVariableDeclaration {
			w.varDecl(n.A, false)
		} else if left.Kind == KIdentifier && (left.Str == "let" || (left.Str == "async" && n.Kind == KForOfStatement)) {
			w.tok("(")
			w.pattern(n.A)
			w.tok(")")
		} else if w.leftmostIsLet(n.A) {
			w.tok("(")
			w.pattern(n.A)
			w.tok(")")
		} else {
			w.pattern(n.A)
		}
		if n.Kind == KForInStatement {
			w.tok("in")
			w.sp()
			w.expr(n.B, pSeq)
		} else {
			w.tok("of")
			w.sp()
			w.expr(n.B, pAssign)
		}
		w.tok(")")
		w.sp()
		w.stmt(n.C)
	case KFunctionDeclaration:
		w.function(id)
	case KVariableDeclaration:
		w.varDecl(id, false)
		w.tok(";")
	case KClassDeclaration:
		w.class(id)
	case KImportDeclaration:
		w.importDecl(id)
	case KExportNamedDeclaration, KExportDefaultDeclaration, KExportAllDeclaration:
		w.exportDecl(id)
	default:
		// A placeholder or an expression node in statement position
		// (tolerant-mode trees): print it as an expression statement.
		w.expr(id, pSeq)
		w.tok(";")
	}
}

// endsWithOpenIf reports whether stmt ends with an if statement without
// else, which an outer else would otherwise attach to.
func (w *printer) endsWithOpenIf(id NodeID) bool {
	for id != NoNode {
		n := w.n(id)
		switch n.Kind {
		case KIfStatement:
			if n.C == NoNode {
				return true
			}
			id = n.C
		case KWhileStatement, KWithStatement, KLabeledStatement:
			id = n.B
		case KForStatement:
			id = n.D
		case KForInStatement, KForOfStatement:
			id = n.C
		default:
			return false
		}
	}
	return false
}

// forInit prints a for-statement initializer, parenthesizing it when it
// contains an `in` operator (for heads are parsed with [~In]).
func (w *printer) forInit(id NodeID) {
	n := w.n(id)
	if n.Kind == KVariableDeclaration {
		w.varDecl(id, true)
		return
	}
	if w.containsIn(id) || w.leftmostIsLet(id) || w.startsBadly(id, false) {
		w.tok("(")
		w.expr(id, pSeq)
		w.tok(")")
		return
	}
	w.expr(id, pSeq)
}

// containsIn reports whether an `in` operator occurs anywhere below id.
func (w *printer) containsIn(id NodeID) bool {
	found := false
	w.p.Walk(id, func(c NodeID) bool {
		if found {
			return false
		}
		if n := w.n(c); n.Kind == KBinaryExpression && n.Op == OpIn {
			found = true
		}
		return !found
	})
	return found
}

func (w *printer) varDecl(id NodeID, forInit bool) {
	n := w.n(id)
	w.tok(n.Op.String())
	for i, d := range w.list(n.List) {
		if i > 0 {
			w.tok(",")
			w.sp()
		}
		dn := w.n(d)
		w.pattern(dn.A)
		if dn.B != NoNode {
			w.sp()
			w.tok("=")
			w.sp()
			if forInit && w.containsIn(dn.B) {
				w.tok("(")
				w.expr(dn.B, pSeq)
				w.tok(")")
			} else {
				w.expr(dn.B, pAssign)
			}
		}
	}
}

// leftmostNode follows the left spine of an expression to the node whose
// first token starts it.
func (w *printer) leftmostNode(id NodeID) NodeID {
	for id != NoNode {
		n := w.n(id)
		switch n.Kind {
		case KBinaryExpression, KLogicalExpression, KAssignmentExpression, KAssignmentPattern:
			id = n.A
		case KConditionalExpression:
			id = n.A
		case KSequenceExpression:
			if n.List.Len == 0 {
				return id
			}
			id = w.list(n.List)[0]
		case KMemberExpression, KCallExpression, KTaggedTemplateExpression, KChainExpression:
			id = n.A
		case KUpdateExpression:
			if n.Flags&NPrefix != 0 {
				return id
			}
			id = n.A
		default:
			return id
		}
	}
	return id
}

// startsBadly reports whether an expression statement (or arrow body when
// !stmt... callers pass stmt) would begin with a token that the
// ExpressionStatement lookahead restriction forbids: `{`, `function`,
// `class`, `let [`, `async function`.
func (w *printer) startsBadly(id NodeID, stmt bool) bool {
	l := w.leftmostNode(id)
	if l == NoNode {
		return false
	}
	ln := w.n(l)
	switch ln.Kind {
	case KObjectExpression, KObjectPattern:
		return true
	case KFunctionExpression, KClassExpression:
		return stmt
	case KIdentifier:
		return stmt && ln.Str == "let"
	}
	return false
}

func (w *printer) leftmostIsLet(id NodeID) bool {
	l := w.leftmostNode(id)
	return l != NoNode && w.n(l).Kind == KIdentifier && w.n(l).Str == "let"
}

// ---- functions and classes ----

func (w *printer) function(id NodeID) {
	n := w.n(id)
	if n.Flags&NAsync != 0 {
		w.tok("async")
	}
	w.tok("function")
	if n.Flags&NGenerator != 0 {
		w.tok("*")
	}
	if n.A != NoNode {
		w.tok(w.n(n.A).Str)
	}
	w.params(n.List)
	w.sp()
	w.body(n.B)
}

func (w *printer) params(l NodeList) {
	w.tok("(")
	for i, prm := range w.list(l) {
		if i > 0 {
			w.tok(",")
			w.sp()
		}
		w.pattern(prm)
	}
	w.tok(")")
}

func (w *printer) body(id NodeID) {
	if id == NoNode {
		w.tok("{")
		w.tok("}")
		return
	}
	w.block(w.n(id).List, true)
}

func (w *printer) arrow(id NodeID) {
	n := w.n(id)
	if n.Flags&NAsync != 0 {
		w.tok("async")
	}
	w.params(n.List)
	w.sp()
	w.tok("=>")
	w.sp()
	if n.Flags&NExpression != 0 {
		if w.startsBadly(n.B, false) {
			w.tok("(")
			w.expr(n.B, pSeq)
			w.tok(")")
		} else {
			w.expr(n.B, pAssign)
		}
		return
	}
	w.body(n.B)
}

func (w *printer) class(id NodeID) {
	n := w.n(id)
	w.tok("class")
	if n.A != NoNode {
		w.tok(w.n(n.A).Str)
	}
	if n.B != NoNode {
		w.tok("extends")
		w.sp()
		w.callee(n.B, false)
	}
	w.sp()
	w.tok("{")
	if n.C != NoNode {
		w.indent++
		for _, m := range w.list(w.n(n.C).List) {
			w.newline()
			w.classMember(m)
		}
		w.indent--
		if w.n(n.C).List.Len > 0 {
			w.newline()
		}
	}
	w.tok("}")
}

func (w *printer) classMember(id NodeID) {
	n := w.n(id)
	switch n.Kind {
	case KStaticBlock:
		w.stmt(id)
	case KMethodDefinition:
		if n.Flags&NStatic != 0 {
			w.tok("static")
		}
		w.method(n.A, n.B, n.Op, n.Flags&NComputed != 0)
	case KPropertyDefinition:
		if n.Flags&NStatic != 0 {
			w.tok("static")
		}
		w.propKey(n.A, n.Flags&NComputed != 0)
		if n.B != NoNode {
			w.sp()
			w.tok("=")
			w.sp()
			w.expr(n.B, pAssign)
		}
		w.tok(";")
	default:
		w.tok(";")
	}
}

// method prints a method, getter, setter or constructor whose function
// is fn.
func (w *printer) method(key, fn NodeID, kind Op, computed bool) {
	f := w.n(fn)
	switch kind {
	case OpGet:
		w.tok("get")
	case OpSet:
		w.tok("set")
	}
	if f.Flags&NAsync != 0 {
		w.tok("async")
	}
	if f.Flags&NGenerator != 0 {
		w.tok("*")
	}
	w.propKey(key, computed)
	w.params(f.List)
	w.sp()
	w.body(f.B)
}

func (w *printer) propKey(key NodeID, computed bool) {
	if computed {
		w.tok("[")
		w.expr(key, pAssign)
		w.tok("]")
		return
	}
	k := w.n(key)
	switch k.Kind {
	case KIdentifier:
		w.tok(k.Str)
	case KPrivateIdentifier:
		w.tok("#" + k.Str)
	default:
		w.expr(key, pPrimary)
	}
}

// ---- expressions ----

// precOf returns the precedence of the expression node.
func (w *printer) precOf(id NodeID) int {
	n := w.n(id)
	switch n.Kind {
	case KSequenceExpression:
		return pSeq
	case KAssignmentExpression, KArrowFunctionExpression, KYieldExpression, KAssignmentPattern:
		return pAssign
	case KConditionalExpression:
		return pCond
	case KLogicalExpression:
		switch n.Op {
		case OpCoalesce:
			return pCoalesce
		case OpOr:
			return pOr
		}
		return pAnd
	case KBinaryExpression:
		return binaryPrintPrec(n.Op)
	case KUnaryExpression, KAwaitExpression:
		return pUnary
	case KUpdateExpression:
		return pUpdate
	case KNewExpression:
		return pCall
	case KCallExpression, KMemberExpression, KTaggedTemplateExpression, KChainExpression, KImportExpression:
		return pCall
	}
	return pPrimary
}

func binaryPrintPrec(op Op) int {
	switch op {
	case OpBitOr:
		return pBitOr
	case OpBitXor:
		return pBitXor
	case OpBitAnd:
		return pBitAnd
	case OpEq, OpNotEq, OpStrictEq, OpStrictNe:
		return pEquality
	case OpLt, OpLe, OpGt, OpGe, OpIn, OpInstanceof:
		return pRelational
	case OpShl, OpShr, OpUShr:
		return pShift
	case OpAdd, OpSub:
		return pAdditive
	case OpMul, OpDiv, OpMod:
		return pMultiplicative
	case OpExp:
		return pExp
	}
	return pPrimary
}

// expr prints id, parenthesized when its precedence is below min.
func (w *printer) expr(id NodeID, min int) {
	if id == NoNode {
		return
	}
	if w.precOf(id) < min {
		w.tok("(")
		w.exprInner(id)
		w.tok(")")
		return
	}
	w.exprInner(id)
}

// operand prints the operand of a binary/logical operator, adding the
// parentheses the `??` mixing rule needs.
func (w *printer) operand(parent Op, id NodeID, min int) {
	c := w.n(id)
	if c.Kind == KLogicalExpression {
		mix := (parent == OpCoalesce && c.Op != OpCoalesce) || (parent != OpCoalesce && c.Op == OpCoalesce)
		if mix {
			w.tok("(")
			w.exprInner(id)
			w.tok(")")
			return
		}
	}
	w.expr(id, min)
}

func (w *printer) exprInner(id NodeID) {
	n := w.n(id)
	switch n.Kind {
	case KIdentifier:
		w.tok(n.Str)
	case KPrivateIdentifier:
		w.tok("#" + n.Str)
	case KLiteral:
		w.literal(n)
	case KThisExpression:
		w.tok("this")
	case KSuper:
		w.tok("super")
	case KArrayExpression, KArrayPattern:
		w.array(n)
	case KObjectExpression, KObjectPattern:
		w.object(n)
	case KFunctionExpression:
		w.function(id)
	case KArrowFunctionExpression:
		w.arrow(id)
	case KClassExpression:
		w.class(id)
	case KTemplateLiteral:
		w.template(n)
	case KTaggedTemplateExpression:
		w.callee(n.A, false)
		w.template(w.n(n.B))
	case KSequenceExpression:
		for i, e := range w.list(n.List) {
			if i > 0 {
				w.tok(",")
				w.sp()
			}
			w.expr(e, pAssign)
		}
	case KUnaryExpression:
		w.tok(n.Op.String())
		if n.Op == OpTypeof || n.Op == OpVoid || n.Op == OpDelete {
			w.sp()
		}
		w.expr(n.A, pUnary)
	case KAwaitExpression:
		w.tok("await")
		w.sp()
		w.expr(n.A, pUnary)
	case KUpdateExpression:
		if n.Flags&NPrefix != 0 {
			w.tok(n.Op.String())
			w.expr(n.A, pUpdate+1)
		} else {
			w.expr(n.A, pNew)
			w.tok(n.Op.String())
		}
	case KBinaryExpression, KLogicalExpression:
		prec := w.precOf(id)
		left, right := prec, prec+1
		if n.Op == OpExp {
			// Right-associative; the left operand may not be a unary expression.
			left, right = pUpdate, prec
		}
		w.operand(n.Op, n.A, left)
		w.sp()
		w.tok(n.Op.String())
		w.sp()
		w.operand(n.Op, n.B, right)
	case KAssignmentExpression:
		w.assignTarget(n.A)
		w.sp()
		w.tok(n.Op.String())
		w.sp()
		w.expr(n.B, pAssign)
	case KAssignmentPattern:
		w.pattern(n.A)
		w.sp()
		w.tok("=")
		w.sp()
		w.expr(n.B, pAssign)
	case KConditionalExpression:
		w.expr(n.A, pCoalesce)
		w.sp()
		w.tok("?")
		w.sp()
		w.expr(n.B, pAssign)
		w.sp()
		w.tok(":")
		w.sp()
		w.expr(n.C, pAssign)
	case KYieldExpression:
		w.tok("yield")
		if n.Flags&NDelegate != 0 {
			w.tok("*")
		}
		if n.A != NoNode {
			w.sp()
			w.expr(n.A, pAssign)
		}
	case KSpreadElement, KRestElement:
		w.tok("...")
		if n.Kind == KRestElement {
			w.pattern(n.A)
		} else {
			w.expr(n.A, pAssign)
		}
	case KMemberExpression:
		w.member(n, false)
	case KCallExpression:
		w.call(n, false)
	case KChainExpression:
		w.chainElem(n.A)
	case KNewExpression:
		w.tok("new")
		w.newCallee(n.A)
		w.args(n.List)
	case KMetaProperty:
		w.tok(w.n(n.A).Str)
		w.tok(".")
		w.tok(w.n(n.B).Str)
	case KImportExpression:
		w.tok("import")
		w.tok("(")
		w.expr(n.A, pAssign)
		if n.B != NoNode {
			w.tok(",")
			w.sp()
			w.expr(n.B, pAssign)
		}
		w.tok(")")
	default:
		w.tok("(")
		w.tok(")")
	}
}

// assignTarget prints the left side of an assignment.
func (w *printer) assignTarget(id NodeID) {
	n := w.n(id)
	switch n.Kind {
	case KObjectPattern, KArrayPattern, KObjectExpression, KArrayExpression:
		w.pattern(id)
	default:
		w.expr(id, pNew)
	}
}

// pattern prints a binding or assignment pattern element.
func (w *printer) pattern(id NodeID) {
	if id == NoNode {
		return
	}
	n := w.n(id)
	switch n.Kind {
	case KIdentifier:
		w.tok(n.Str)
	case KObjectPattern, KArrayPattern, KAssignmentPattern, KRestElement, KObjectExpression, KArrayExpression:
		w.exprInner(id)
	default:
		w.expr(id, pNew)
	}
}

func (w *printer) array(n *Node) {
	w.tok("[")
	elems := w.list(n.List)
	for i, e := range elems {
		if i > 0 {
			w.tok(",")
			if e != NoNode {
				w.sp()
			}
		}
		if e == NoNode {
			if i == len(elems)-1 {
				w.tok(",")
			}
			continue
		}
		if n.Kind == KArrayPattern {
			w.pattern(e)
		} else {
			w.expr(e, pAssign)
		}
	}
	w.tok("]")
}

func (w *printer) object(n *Node) {
	w.tok("{")
	for i, pr := range w.list(n.List) {
		if i > 0 {
			w.tok(",")
			w.sp()
		}
		pn := w.n(pr)
		switch pn.Kind {
		case KProperty:
			w.property(pn, n.Kind == KObjectPattern)
		case KSpreadElement, KRestElement:
			w.exprInner(pr)
		}
	}
	w.tok("}")
}

func (w *printer) property(pn *Node, isPattern bool) {
	computed := pn.Flags&NComputed != 0
	if pn.Op == OpGet || pn.Op == OpSet || pn.Flags&NMethod != 0 {
		w.method(pn.A, pn.B, pn.Op, computed)
		return
	}
	if pn.Flags&NShorthand != 0 {
		v := w.n(pn.B)
		if v.Kind == KAssignmentPattern {
			w.pattern(pn.B)
			return
		}
		w.tok(w.n(pn.A).Str)
		return
	}
	w.propKey(pn.A, computed)
	w.tok(":")
	w.sp()
	if isPattern {
		w.pattern(pn.B)
	} else {
		w.expr(pn.B, pAssign)
	}
}

// callee prints the object of a member access, the callee of a call or
// the tag of a template: a call/member-level expression, where an
// optional chain must be closed with parentheses.
func (w *printer) callee(id NodeID, inChain bool) {
	n := w.n(id)
	if n.Kind == KChainExpression && !inChain {
		w.tok("(")
		w.exprInner(id)
		w.tok(")")
		return
	}
	if n.Kind == KLiteral && n.Op == LitNumber {
		w.tok("(")
		w.literal(n)
		w.tok(")")
		return
	}
	w.expr(id, pCall)
}

// newCallee prints the callee of `new`, which may not contain a call on
// its left spine.
func (w *printer) newCallee(id NodeID) {
	for c := id; c != NoNode; {
		cn := w.n(c)
		if cn.Kind == KCallExpression || cn.Kind == KChainExpression || cn.Kind == KImportExpression {
			w.tok("(")
			w.exprInner(id)
			w.tok(")")
			return
		}
		if cn.Kind == KMemberExpression || cn.Kind == KTaggedTemplateExpression {
			c = cn.A
			continue
		}
		break
	}
	w.callee(id, false)
}

func (w *printer) member(n *Node, inChain bool) {
	w.callee(n.A, inChain)
	if n.Flags&NOptional != 0 {
		w.tok("?.")
		if n.Flags&NComputed != 0 {
			w.tok("[")
			w.expr(n.B, pSeq)
			w.tok("]")
			return
		}
		w.memberName(n.B)
		return
	}
	if n.Flags&NComputed != 0 {
		w.tok("[")
		w.expr(n.B, pSeq)
		w.tok("]")
		return
	}
	w.tok(".")
	w.memberName(n.B)
}

func (w *printer) memberName(id NodeID) {
	n := w.n(id)
	if n.Kind == KPrivateIdentifier {
		w.tok("#" + n.Str)
		return
	}
	w.tok(n.Str)
}

func (w *printer) call(n *Node, inChain bool) {
	w.callee(n.A, inChain)
	if n.Flags&NOptional != 0 {
		w.tok("?.")
	}
	w.args(n.List)
}

// chainElem prints the inside of a ChainExpression: member and call links
// stay inside the chain.
func (w *printer) chainElem(id NodeID) {
	n := w.n(id)
	switch n.Kind {
	case KMemberExpression:
		w.chainObject(n.A)
		w.memberTail(n)
	case KCallExpression:
		w.chainObject(n.A)
		if n.Flags&NOptional != 0 {
			w.tok("?.")
		}
		w.args(n.List)
	default:
		w.expr(id, pCall)
	}
}

func (w *printer) chainObject(id NodeID) {
	n := w.n(id)
	if n.Kind == KMemberExpression || n.Kind == KCallExpression {
		w.chainElem(id)
		return
	}
	w.callee(id, false)
}

func (w *printer) memberTail(n *Node) {
	if n.Flags&NOptional != 0 {
		w.tok("?.")
		if n.Flags&NComputed != 0 {
			w.tok("[")
			w.expr(n.B, pSeq)
			w.tok("]")
			return
		}
		w.memberName(n.B)
		return
	}
	if n.Flags&NComputed != 0 {
		w.tok("[")
		w.expr(n.B, pSeq)
		w.tok("]")
		return
	}
	w.tok(".")
	w.memberName(n.B)
}

func (w *printer) args(l NodeList) {
	w.tok("(")
	for i, a := range w.list(l) {
		if i > 0 {
			w.tok(",")
			w.sp()
		}
		w.expr(a, pAssign)
	}
	w.tok(")")
}

func (w *printer) template(n *Node) {
	quasis := w.list(n.List)
	exprs := w.list(n.List2)
	w.tok("`")
	for i, q := range quasis {
		w.out = append(w.out, w.n(q).Str2...)
		if i < len(exprs) {
			w.out = append(w.out, '$', '{')
			w.expr(exprs[i], pSeq)
			w.out = append(w.out, '}')
		}
	}
	w.out = append(w.out, '`')
}

func (w *printer) literal(n *Node) {
	switch n.Op {
	case LitString:
		w.tok(quoteJS(n.Str))
	case LitNumber:
		switch {
		case math.IsInf(n.Num, 1):
			w.tok("1e999")
		case math.IsNaN(n.Num):
			w.tok("NaN")
		default:
			w.tok(NumberToString(n.Num))
		}
	case LitBigInt:
		w.tok(n.Str + "n")
	case LitBoolean:
		if n.Num != 0 {
			w.tok("true")
		} else {
			w.tok("false")
		}
	case LitNull:
		w.tok("null")
	case LitRegExp:
		w.tok("/" + n.Str + "/" + n.Str2)
		w.afterRegex = true
	}
}

// quoteJS returns s as a double-quoted string literal whose cooked value
// is s (WTF-8 lone surrogates become \u escapes).
func quoteJS(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x80 {
			switch c {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			default:
				if c < 0x20 || c == 0x7f {
					b.WriteString(`\x`)
					b.WriteByte(hexDigits[c>>4])
					b.WriteByte(hexDigits[c&15])
				} else {
					b.WriteByte(c)
				}
			}
			i++
			continue
		}
		r, n := decodeRuneSlow(s, i)
		// Invalid UTF-8 bytes are copied unchanged so that re-lexing yields
		// the same bytes (PORT: Rust strings cannot hold them; keep the
		// cooked value as bytes there or reject non-UTF-8 input).
		if (r >= 0xD800 && r <= 0xDFFF) || r == cpLS || r == cpPS {
			b.WriteString(`\u`)
			b.WriteByte(hexDigits[(r>>12)&15])
			b.WriteByte(hexDigits[(r>>8)&15])
			b.WriteByte(hexDigits[(r>>4)&15])
			b.WriteByte(hexDigits[r&15])
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	b.WriteByte('"')
	return b.String()
}

// ---- modules ----

func (w *printer) moduleName(id NodeID) {
	n := w.n(id)
	if n.Kind == KLiteral {
		w.literal(n)
		return
	}
	w.tok(n.Str)
}

func (w *printer) withClause(l NodeList) {
	if l.Len == 0 {
		return
	}
	w.sp()
	w.tok("with")
	w.sp()
	w.tok("{")
	for i, a := range w.list(l) {
		if i > 0 {
			w.tok(",")
			w.sp()
		}
		an := w.n(a)
		w.moduleName(an.A)
		w.tok(":")
		w.sp()
		w.literal(w.n(an.B))
	}
	w.tok("}")
}

func (w *printer) importDecl(id NodeID) {
	n := w.n(id)
	w.tok("import")
	specs := w.list(n.List)
	if len(specs) > 0 {
		named := false
		for i, s := range specs {
			sn := w.n(s)
			switch sn.Kind {
			case KImportDefaultSpecifier:
				w.tok(w.n(sn.A).Str)
			case KImportNamespaceSpecifier:
				if i > 0 {
					w.tok(",")
					w.sp()
				}
				w.tok("*")
				w.sp()
				w.tok("as")
				w.tok(w.n(sn.A).Str)
			case KImportSpecifier:
				if !named {
					if i > 0 {
						w.tok(",")
						w.sp()
					}
					w.tok("{")
					named = true
				} else {
					w.tok(",")
					w.sp()
				}
				w.moduleName(sn.A)
				if w.n(sn.A).Kind == KLiteral || w.n(sn.A).Str != w.n(sn.B).Str {
					w.tok("as")
					w.tok(w.n(sn.B).Str)
				}
			}
		}
		if named {
			w.tok("}")
		}
		w.sp()
		w.tok("from")
		w.sp()
	}
	w.literal(w.n(n.A))
	w.withClause(n.List2)
	w.tok(";")
}

func (w *printer) exportDecl(id NodeID) {
	n := w.n(id)
	w.tok("export")
	switch n.Kind {
	case KExportAllDeclaration:
		w.sp()
		w.tok("*")
		if n.A != NoNode {
			w.sp()
			w.tok("as")
			w.sp()
			w.moduleName(n.A)
		}
		w.sp()
		w.tok("from")
		w.sp()
		w.literal(w.n(n.B))
		w.withClause(n.List2)
		w.tok(";")
	case KExportDefaultDeclaration:
		w.tok("default")
		d := w.n(n.A)
		switch d.Kind {
		case KFunctionDeclaration, KClassDeclaration:
			w.sp()
			w.stmt(n.A)
		default:
			w.sp()
			if w.startsBadly(n.A, true) {
				w.tok("(")
				w.expr(n.A, pSeq)
				w.tok(")")
			} else {
				w.expr(n.A, pAssign)
			}
			w.tok(";")
		}
	case KExportNamedDeclaration:
		if n.A != NoNode {
			w.sp()
			w.stmt(n.A)
			return
		}
		w.sp()
		w.tok("{")
		for i, s := range w.list(n.List) {
			if i > 0 {
				w.tok(",")
				w.sp()
			}
			sn := w.n(s)
			w.moduleName(sn.A)
			ln, en := w.n(sn.A), w.n(sn.B)
			if ln.Kind != en.Kind || ln.Str != en.Str {
				w.tok("as")
				w.moduleName(sn.B)
			}
		}
		w.tok("}")
		if n.B != NoNode {
			w.sp()
			w.tok("from")
			w.sp()
			w.literal(w.n(n.B))
			w.withClause(n.List2)
		}
		w.tok(";")
	}
}
