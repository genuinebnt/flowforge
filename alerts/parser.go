package alerts

import "strings"

// maxNesting bounds expression nesting (parentheses, lists, calls, unary
// chains) so hostile input cannot exhaust the stack.
const maxNesting = 64

// parser is a recursive-descent / precedence-climbing parser for the rule
// statement and the DSL expression subset of spec 7.3. After a syntax
// error it abandons the current section and resynchronizes at the next
// "->" or clause keyword, so several independent errors are reported.
type parser struct {
	toks   []token
	pos    int
	diag   *diagSink
	failed bool // the current section hit a syntax error
	depth  int
	// quietEOF suppresses errors at end of input after an unterminated
	// string or comment (already reported by the lexer).
	quietEOF bool
}

func isClauseWord(t token) bool {
	if t.kind != tIdent {
		return false
	}
	switch t.text {
	case "throttle", "group_by", "digest", "severity", "quiet_hours":
		return true
	}
	return false
}

func isWordOperator(t token) bool {
	if t.kind == tKeyword {
		return t.text == "in"
	}
	if t.kind != tIdent {
		return false
	}
	switch t.text {
	case "contains", "starts_with", "ends_with", "matches":
		return true
	}
	return false
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) peekAt(n int) token {
	if p.pos+n < len(p.toks) {
		return p.toks[p.pos+n]
	}
	return p.toks[len(p.toks)-1]
}

func (p *parser) advance() token {
	t := p.toks[p.pos]
	if t.kind != tEOF {
		p.pos++
	}
	return t
}

func (p *parser) atOp(text string) bool { return p.peek().is(tOp, text) }

// describe renders a token for messages.
func describe(t token) string {
	switch t.kind {
	case tEOF:
		return "end of input"
	case tString:
		return "string literal"
	case tInt, tFloat, tDuration:
		return "literal " + t.text
	}
	return "`" + t.text + "`"
}

// errorf reports a syntax error at t and marks the section as failed.
func (p *parser) errorf(code string, sp span, label, format string, args ...any) *Diagnostic {
	p.failed = true
	if p.quietEOF && sp.start >= len(p.diag.src) {
		return &Diagnostic{} // swallowed: decorations go nowhere
	}
	return p.diag.add(code, sp, label, format, args...)
}

func (p *parser) expectOp(text, what string) bool {
	if p.failed {
		return false
	}
	if p.atOp(text) {
		p.advance()
		return true
	}
	t := p.peek()
	p.errorf(CodeExpected, t.sp, "expected "+what, "expected `%s`, found %s", text, describe(t))
	return false
}

// parseRule parses alert_rule (spec 15.2). It returns nil only when the
// header is too broken to continue.
func parseRule(toks []token, quietEOF bool, diag *diagSink) *ruleAST {
	p := &parser{toks: toks, diag: diag, quietEOF: quietEOF}
	r := &ruleAST{}
	if t := p.peek(); !t.is(tKeyword, "alert") {
		p.errorf(CodeExpected, t.sp, "expected `alert`", "expected `alert`, found %s", describe(t)).
			Help = "a rule has the form: alert NAME when CONDITION -> action(...) [clauses]"
		return nil
	}
	p.advance()
	name := p.peek()
	if name.kind != tIdent {
		p.errorf(CodeExpected, name.sp, "expected rule name", "expected rule name, found %s", describe(name))
		return nil
	}
	p.advance()
	r.name, r.nameSp = name.text, name.sp
	if t := p.peek(); !t.is(tKeyword, "when") {
		p.errorf(CodeExpected, t.sp, "expected `when`", "expected `when`, found %s", describe(t))
		return nil
	}
	p.advance()

	r.cond = p.parseExpr()
	if !p.failed {
		if t := p.peek(); !t.is(tOp, "->") {
			p.unexpected(t, "expected `->` and an action")
		}
	}
	if p.failed {
		p.syncTo(func(t token) bool { return t.is(tOp, "->") || isClauseWord(t) })
	}
	p.failed = false
	if p.atOp("->") {
		p.advance()
		r.actions = p.parseActions()
		if p.failed {
			p.syncTo(isClauseWord)
		}
	}
	for p.peek().kind != tEOF {
		p.failed = false
		t := p.peek()
		if !isClauseWord(t) {
			p.unexpected(t, "expected a clause").Help = "clauses are throttle, group_by, digest, severity and quiet_hours"
			p.advance()
			p.syncTo(isClauseWord)
			continue
		}
		if c, ok := p.parseClause(); ok {
			r.clauses = append(r.clauses, c)
		} else {
			p.syncTo(isClauseWord)
		}
	}
	return r
}

func (p *parser) unexpected(t token, label string) *Diagnostic {
	return p.errorf(CodeUnexpectedToken, t.sp, label, "unexpected %s", describe(t))
}

// syncTo skips tokens until stop matches or input ends.
func (p *parser) syncTo(stop func(token) bool) {
	for p.peek().kind != tEOF && !stop(p.peek()) {
		p.advance()
	}
}

func (p *parser) parseActions() []actionAST {
	var out []actionAST
	for {
		a, ok := p.parseAction()
		if !ok {
			return out
		}
		out = append(out, a)
		if !p.atOp(",") {
			return out
		}
		p.advance()
	}
}

func (p *parser) parseAction() (actionAST, bool) {
	name := p.peek()
	if name.kind != tIdent {
		p.errorf(CodeExpected, name.sp, "expected action", "expected an action such as slack(...), found %s", describe(name))
		return actionAST{}, false
	}
	p.advance()
	a := actionAST{name: name.text, nameSp: name.sp, sp: name.sp}
	if !p.expectOp("(", "argument list") {
		return a, false
	}
	for !p.atOp(")") {
		var arg argAST
		if t := p.peek(); t.kind == tIdent && (p.peekAt(1).is(tOp, "=") || p.peekAt(1).is(tOp, ":")) {
			arg.name, arg.nameSp = t.text, t.sp
			p.advance()
			p.advance()
		}
		arg.val = p.parseExpr()
		if p.failed {
			return a, false
		}
		a.args = append(a.args, arg)
		if !p.atOp(",") {
			break
		}
		p.advance()
	}
	end := p.peek()
	if !p.expectOp(")", "`)`") {
		return a, false
	}
	a.sp = name.sp.to(end.sp)
	return a, true
}

func (p *parser) parseClause() (clauseAST, bool) {
	kw := p.advance()
	c := clauseAST{kind: kw.text, sp: kw.sp}
	switch kw.text {
	case "throttle", "digest":
		t := p.peek()
		if t.kind == tBad {
			p.advance()
			p.failed = true
			return c, false
		}
		if t.kind != tDuration {
			d := p.errorf(CodeExpected, t.sp, "expected a duration", "`%s` needs a duration, found %s", kw.text, describe(t))
			if t.kind == tInt {
				d.Help = "add a unit, for example 1h or 15m"
			}
			return c, false
		}
		p.advance()
		c.duration, c.valSp = t.ival, t.sp
	case "group_by":
		for {
			e := p.parseExpr()
			if p.failed {
				return c, false
			}
			c.exprs = append(c.exprs, e)
			if !p.atOp(",") {
				break
			}
			p.advance()
		}
		c.valSp = c.exprs[0].exprSpan().to(c.exprs[len(c.exprs)-1].exprSpan())
	case "severity":
		if !p.expectOp("=", "`=`") {
			return c, false
		}
		t := p.peek()
		if t.kind != tIdent {
			p.errorf(CodeExpected, t.sp, "expected a severity", "expected a severity name, found %s", describe(t)).
				Help = "severities are Info, Low, Medium, High and Critical"
			return c, false
		}
		p.advance()
		c.ident, c.valSp = t.text, t.sp
	case "quiet_hours":
		t := p.peek()
		if t.kind == tBad {
			p.advance()
			p.failed = true
			return c, false
		}
		if t.kind != tString {
			p.errorf(CodeExpected, t.sp, "expected a string", "`quiet_hours` needs a string such as \"22:00-07:00 Asia/Kolkata\", found %s", describe(t))
			return c, false
		}
		p.advance()
		c.text, c.valSp = t.sval, t.sp
	}
	return c, true
}

// ---- expressions (spec 7.3 precedence) ----

func (p *parser) parseExpr() expr { return p.parsePipe() }

// parsePipe desugars a |> f(x) into f(a, x) and a |> f into f(a).
func (p *parser) parsePipe() expr {
	l := p.parseOr()
	for !p.failed && p.atOp("|>") {
		p.advance()
		name := p.peek()
		if name.kind != tIdent {
			p.errorf(CodeExpected, name.sp, "expected a function", "expected a function name after `|>`, found %s", describe(name))
			return nil
		}
		p.advance()
		call := &callExpr{sp: l.exprSpan().to(name.sp), name: name.text, nameSp: name.sp, args: []expr{l}}
		if p.atOp("(") {
			args, end, ok := p.parseCallArgs()
			if !ok {
				return nil
			}
			call.args = append(call.args, args...)
			call.sp = l.exprSpan().to(end)
		}
		l = call
	}
	return l
}

func (p *parser) parseOr() expr {
	l := p.parseAnd()
	for !p.failed && p.atOp("||") {
		op := p.advance()
		r := p.parseAnd()
		if p.failed {
			return nil
		}
		l = &binaryExpr{sp: l.exprSpan().to(r.exprSpan()), op: "||", opSp: op.sp, l: l, r: r}
	}
	return l
}

func (p *parser) parseAnd() expr {
	l := p.parseEquality()
	for !p.failed && p.atOp("&&") {
		op := p.advance()
		r := p.parseEquality()
		if p.failed {
			return nil
		}
		l = &binaryExpr{sp: l.exprSpan().to(r.exprSpan()), op: "&&", opSp: op.sp, l: l, r: r}
	}
	return l
}

func isEqualityOp(t token) bool { return t.is(tOp, "==") || t.is(tOp, "!=") }

func isRelationalOp(t token) bool {
	if t.kind == tOp {
		switch t.text {
		case "<", "<=", ">", ">=":
			return true
		}
	}
	return isWordOperator(t)
}

// nonAssoc parses operand [op operand] and rejects a second operator of
// the same level (spec 7.3: levels 4, 5 and 6 are non-associative).
func (p *parser) nonAssoc(operand func() expr, isOp func(token) bool) expr {
	l := operand()
	if p.failed || !isOp(p.peek()) {
		return l
	}
	op := p.advance()
	r := operand()
	if p.failed {
		return nil
	}
	if t := p.peek(); isOp(t) {
		p.errorf(CodeNonAssociative, t.sp, "second operator", "operator %s cannot be chained with %s", describe(t), describe(op)).
			Help = "add parentheses to make the grouping explicit"
		return nil
	}
	return &binaryExpr{sp: l.exprSpan().to(r.exprSpan()), op: op.text, opSp: op.sp, l: l, r: r}
}

func (p *parser) parseEquality() expr { return p.nonAssoc(p.parseRelational, isEqualityOp) }

func (p *parser) parseRelational() expr { return p.nonAssoc(p.parseRange, isRelationalOp) }

func (p *parser) parseRange() expr {
	e := p.nonAssoc(p.parseAdditive, func(t token) bool { return t.is(tOp, "..") })
	if b, ok := e.(*binaryExpr); ok && b.op == ".." {
		return &rangeExpr{sp: b.sp, lo: b.l, hi: b.r}
	}
	return e
}

func (p *parser) leftAssoc(operand func() expr, ops ...string) expr {
	l := operand()
	for !p.failed {
		t := p.peek()
		if t.kind != tOp || !containsString(ops, t.text) {
			return l
		}
		p.advance()
		r := operand()
		if p.failed {
			return nil
		}
		l = &binaryExpr{sp: l.exprSpan().to(r.exprSpan()), op: t.text, opSp: t.sp, l: l, r: r}
	}
	return l
}

func (p *parser) parseAdditive() expr { return p.leftAssoc(p.parseMultiplicative, "+", "-") }

func (p *parser) parseMultiplicative() expr { return p.leftAssoc(p.parseUnary, "*", "/", "%") }

func (p *parser) parseUnary() expr {
	if p.failed {
		return nil
	}
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxNesting {
		t := p.peek()
		p.errorf(CodeTooDeep, t.sp, "nesting limit reached", "expression nested more than %d levels deep", maxNesting)
		return nil
	}
	if t := p.peek(); t.is(tOp, "-") || t.is(tOp, "!") {
		p.advance()
		x := p.parseUnary()
		if p.failed {
			return nil
		}
		return &unaryExpr{sp: t.sp.to(x.exprSpan()), op: t.text, x: x}
	}
	e := p.parsePrimary()
	if p.failed {
		return nil
	}
	if t := p.peek(); t.is(tOp, "(") || t.is(tOp, "[") || t.is(tOp, "?") || t.is(tOp, ".") || (t.kind == tIdent && t.text == "as") {
		p.errorf(CodeUnsupported, t.sp, "not supported here", "%s is not supported in alert rules", describe(t)).
			Help = "alert conditions support literals, variable paths, operators and built-in function calls"
		return nil
	}
	return e
}

func (p *parser) parsePrimary() expr {
	t := p.peek()
	switch t.kind {
	case tInt:
		p.advance()
		return &litExpr{sp: t.sp, val: intVal(t.ival)}
	case tFloat:
		p.advance()
		return &litExpr{sp: t.sp, val: floatVal(t.fval)}
	case tDuration:
		p.advance()
		return &litExpr{sp: t.sp, val: durationVal(t.ival)}
	case tString:
		p.advance()
		return &litExpr{sp: t.sp, val: stringVal(t.sval)}
	case tBad:
		p.advance()
		return &badExpr{sp: t.sp}
	case tKeyword:
		switch t.text {
		case "true", "false":
			p.advance()
			return &litExpr{sp: t.sp, val: boolVal(t.text == "true")}
		case "in":
			if p.peekAt(1).is(tOp, "(") {
				return p.parseCall()
			}
		}
	case tIdent:
		if p.peekAt(1).is(tOp, "(") {
			return p.parseCall()
		}
		return p.parsePath()
	case tOp:
		switch t.text {
		case "(":
			p.advance()
			e := p.parseExpr()
			if p.failed {
				return nil
			}
			if p.atOp(",") {
				p.errorf(CodeUnsupported, p.peek().sp, "tuple", "tuples are not supported in alert rules")
				return nil
			}
			end := p.peek()
			if !p.expectOp(")", "`)`") {
				return nil
			}
			return withSpan(e, t.sp.to(end.sp))
		case "[":
			return p.parseList()
		}
	}
	p.unexpected(t, "expected an expression")
	return nil
}

// withSpan widens the span of a parenthesized expression so diagnostics
// cover the parentheses.
func withSpan(e expr, sp span) expr {
	switch n := e.(type) {
	case *binaryExpr:
		c := *n
		c.sp = sp
		return &c
	case *unaryExpr:
		c := *n
		c.sp = sp
		return &c
	}
	return e
}

func (p *parser) parsePath() expr {
	first := p.advance()
	e := &pathExpr{sp: first.sp, parts: []string{first.text}, spans: []span{first.sp}}
	for p.atOp(".") && p.peekAt(1).kind == tIdent {
		p.advance()
		f := p.advance()
		e.parts = append(e.parts, f.text)
		e.spans = append(e.spans, f.sp)
		e.sp = e.sp.to(f.sp)
	}
	return e
}

func (p *parser) parseCall() expr {
	name := p.advance()
	args, end, ok := p.parseCallArgs()
	if !ok {
		return nil
	}
	return &callExpr{sp: name.sp.to(end), name: name.text, nameSp: name.sp, args: args}
}

// parseCallArgs parses "(" [expr {"," expr}] ")" and returns the span of
// the closing parenthesis.
func (p *parser) parseCallArgs() ([]expr, span, bool) {
	if !p.expectOp("(", "`(`") {
		return nil, span{}, false
	}
	var args []expr
	for !p.atOp(")") {
		if t := p.peek(); t.kind == tIdent && (p.peekAt(1).is(tOp, "=") || p.peekAt(1).is(tOp, ":")) {
			p.errorf(CodeUnsupported, t.sp, "named argument", "built-in functions take positional arguments only")
			return nil, span{}, false
		}
		a := p.parseExpr()
		if p.failed {
			return nil, span{}, false
		}
		args = append(args, a)
		if !p.atOp(",") {
			break
		}
		p.advance()
	}
	end := p.peek()
	if !p.expectOp(")", "`)`") {
		return nil, span{}, false
	}
	return args, end.sp, true
}

func (p *parser) parseList() expr {
	open := p.advance()
	l := &listExpr{sp: open.sp}
	for !p.atOp("]") {
		e := p.parseExpr()
		if p.failed {
			return nil
		}
		l.elems = append(l.elems, e)
		if !p.atOp(",") {
			break
		}
		p.advance()
	}
	end := p.peek()
	if !p.expectOp("]", "`]`") {
		return nil
	}
	l.sp = open.sp.to(end.sp)
	return l
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// trimmedText returns the source text of sp with surrounding space
// removed (used to name group_by keys in payloads).
func trimmedText(src string, sp span) string {
	if sp.start < 0 || sp.end > len(src) || sp.start > sp.end {
		return ""
	}
	return strings.TrimSpace(src[sp.start:sp.end])
}
