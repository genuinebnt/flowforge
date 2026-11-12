package dsl

import "fmt"

// Limits enforced by the front end (E0602).
const (
	// MaxSourceBytes is the largest accepted source file (the module limit
	// of spec 8.7).
	MaxSourceBytes = 4 << 20
	// maxNesting bounds syntactic nesting (parentheses, blocks, unary
	// operators, types, patterns).
	maxNesting = 256
	// maxChain bounds left-associative chains (binary operators, pipes,
	// postfix operators), which also nest in the tree.
	maxChain = 4096
)

// Parse lexes and parses one source file. It always returns a File; on
// syntax errors the File holds what could be recovered and the
// diagnostics (in source order) explain the rest. Parse never panics.
func Parse(file string, src []byte) (*File, []Diagnostic) {
	f := &File{Name: file, Src: src}
	if len(src) > MaxSourceBytes {
		sink := newDiagSink(file)
		end := Pos{Line: 1, Col: 1, Offset: 0}
		sink.errorf(CodeTooLarge, Span{Start: end, End: end}, "source too large",
			"program too large: %d bytes exceeds the limit of %d bytes", len(src), MaxSourceBytes)
		return f, sink.diags
	}
	toks, lexDiags := Lex(file, src)
	sink := newDiagSink(file)
	sink.diags = append(sink.diags, lexDiags...)
	p := &parser{toks: toks, sink: sink, file: file}
	f.Items = p.parseItems()
	SortDiagnostics(sink.diags)
	return f, sink.diags
}

// ParseExpr parses a single expression (used for tests and tooling).
func ParseExpr(file string, src []byte) (Expr, []Diagnostic) {
	toks, lexDiags := Lex(file, src)
	sink := newDiagSink(file)
	sink.diags = append(sink.diags, lexDiags...)
	p := &parser{toks: toks, sink: sink, file: file}
	e := p.parseExpr()
	if !p.at(TokEOF) {
		p.errExpected("end of expression")
	}
	SortDiagnostics(sink.diags)
	return e, sink.diags
}

type parser struct {
	toks     []Token
	pos      int
	sink     *diagSink
	file     string
	noRecord bool // record literals are not allowed (if/match/for heads)
	noLambda bool // a bare `x =>` is not a lambda (match guards)
	depth    int
	chain    int
	tooDeep  bool
}

func (p *parser) tok() Token { return p.toks[p.pos] }

func (p *parser) peek(i int) Token {
	if p.pos+i < len(p.toks) {
		return p.toks[p.pos+i]
	}
	return p.toks[len(p.toks)-1]
}

func (p *parser) at(k TokenKind) bool { return p.toks[p.pos].Kind == k }

func (p *parser) advance() Token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

// prevEnd is the end position of the previous token.
func (p *parser) prevEnd() Pos {
	if p.pos == 0 {
		return p.toks[0].Span.Start
	}
	return p.toks[p.pos-1].Span.End
}

func (p *parser) spanFrom(start Pos) Span { return Span{Start: start, End: p.prevEnd()} }

func (p *parser) errCount() int { return len(p.sink.diags) }

// errExpected reports E0101 at the current token.
func (p *parser) errExpected(what string) {
	t := p.tok()
	p.sink.errorOnce(CodeUnexpectedToken, t.Span, "expected "+what,
		"expected %s, found %s", what, t.describe())
}

// expect consumes a token of kind k or reports E0101.
func (p *parser) expect(k TokenKind) (Token, bool) {
	if p.at(k) {
		return p.advance(), true
	}
	p.errExpected(fmt.Sprintf("`%s`", k))
	return p.tok(), false
}

// enter increases the nesting depth; it returns false (after reporting
// E0602 once) when the limit is exceeded.
func (p *parser) enter() bool {
	p.depth++
	if p.depth > maxNesting {
		if !p.tooDeep {
			p.tooDeep = true
			p.sink.errorf(CodeTooLarge, p.tok().Span, "nested too deeply",
				"program too large: nesting exceeds %d levels", maxNesting)
		}
		return false
	}
	return true
}

func (p *parser) leave() { p.depth-- }

// link counts one more link of a left-associative chain.
func (p *parser) link() bool {
	p.chain++
	if p.chain > maxChain {
		if !p.tooDeep {
			p.tooDeep = true
			p.sink.errorf(CodeTooLarge, p.tok().Span, "chain too long",
				"program too large: operator chain exceeds %d operands", maxChain)
		}
		return false
	}
	return true
}

func isItemStart(k TokenKind) bool {
	switch k {
	case TokWorkflow, TokFn, TokTool, TokType, TokEnum, TokUse:
		return true
	}
	return false
}

// ---- Items ----

func (p *parser) parseItems() []Item {
	var items []Item
	for !p.at(TokEOF) {
		start := p.pos
		p.chain = 0
		annots := p.parseAnnotations()
		if it := p.parseItem(annots); it != nil {
			items = append(items, it)
		}
		if p.pos == start {
			p.advance()
		}
	}
	return items
}

// syncItem skips to the next item keyword at brace depth zero.
func (p *parser) syncItem() {
	depth := 0
	for !p.at(TokEOF) {
		switch p.tok().Kind {
		case TokLBrace:
			depth++
		case TokRBrace:
			depth--
			if depth < 0 {
				depth = 0
			}
		case TokHash:
			if depth == 0 && p.peek(1).Kind == TokLBracket {
				return
			}
		default:
			if depth == 0 && isItemStart(p.tok().Kind) {
				return
			}
		}
		p.advance()
	}
}

func (p *parser) parseItem(annots []*Annotation) Item {
	var it Item
	switch p.tok().Kind {
	case TokWorkflow:
		it = p.parseWorkflow()
	case TokFn:
		it = p.parseFn()
	case TokTool:
		it = p.parseTool()
	case TokType:
		it = p.parseTypeDecl()
	case TokEnum:
		it = p.parseEnum()
	case TokUse:
		it = p.parseUse()
	default:
		p.errExpected("an item (workflow, fn, tool, type, enum or use)")
		p.advance()
		p.syncItem()
		return nil
	}
	if it == nil {
		return nil
	}
	if s, ok := it.(interface{ setAnnots([]*Annotation) }); ok {
		s.setAnnots(annots)
	}
	return it
}

func (p *parser) parseAnnotations() []*Annotation {
	var out []*Annotation
	for p.at(TokHash) {
		p.advance()
		if _, ok := p.expect(TokLBracket); !ok {
			return out
		}
		for {
			start := p.tok().Span.Start
			name := p.parseIdent("annotation name")
			if name == nil {
				break
			}
			a := &Annotation{Name: name}
			if p.at(TokLParen) {
				a.HasParens = true
				a.Args = p.parseArgs()
			}
			a.Sp = p.spanFrom(start)
			out = append(out, a)
			if !p.at(TokComma) {
				break
			}
			p.advance()
		}
		if _, ok := p.expect(TokRBracket); !ok {
			for !p.at(TokEOF) && !p.at(TokRBracket) && !isItemStart(p.tok().Kind) && !p.at(TokLet) {
				p.advance()
			}
			if p.at(TokRBracket) {
				p.advance()
			}
		}
	}
	return out
}

// parseIdent consumes an identifier or reports E0101.
func (p *parser) parseIdent(what string) *Ident {
	if !p.at(TokIdent) {
		p.errExpected(what)
		return nil
	}
	t := p.advance()
	return &Ident{exprBase: exprBase{Sp: t.Span}, Name: t.Text}
}

func (p *parser) parseGenerics() []*Ident {
	if !p.at(TokLt) {
		return nil
	}
	p.advance()
	var gs []*Ident
	for {
		id := p.parseIdent("type parameter name")
		if id == nil {
			break
		}
		gs = append(gs, id)
		if !p.at(TokComma) {
			break
		}
		p.advance()
	}
	p.expect(TokGt)
	return gs
}

func (p *parser) parseParams() []*Param {
	if _, ok := p.expect(TokLParen); !ok {
		return nil
	}
	var ps []*Param
	for !p.at(TokRParen) && !p.at(TokEOF) {
		start := p.tok().Span.Start
		name := p.parseIdent("parameter name")
		if name == nil {
			break
		}
		prm := &Param{Name: name}
		if _, ok := p.expect(TokColon); ok {
			prm.TypeX = p.parseType()
		}
		prm.Sp = p.spanFrom(start)
		ps = append(ps, prm)
		if !p.at(TokComma) {
			break
		}
		p.advance()
	}
	if _, ok := p.expect(TokRParen); !ok {
		for !p.at(TokEOF) && !p.at(TokRParen) && !p.at(TokLBrace) && !isItemStart(p.tok().Kind) {
			p.advance()
		}
		if p.at(TokRParen) {
			p.advance()
		}
	}
	return ps
}

func (p *parser) parseEffectClause() *EffectClause {
	start := p.tok().Span.Start
	p.advance() // !
	braced := false
	if p.at(TokLBrace) {
		braced = true
		p.advance()
	}
	ec := &EffectClause{}
	for {
		id := p.parseIdent("effect name")
		if id == nil {
			break
		}
		ec.Names = append(ec.Names, id)
		if !p.at(TokComma) {
			break
		}
		p.advance()
	}
	if braced {
		p.expect(TokRBrace)
	}
	ec.Sp = p.spanFrom(start)
	return ec
}

func (p *parser) parseWorkflow() Item {
	start := p.advance().Span.Start
	d := &WorkflowDecl{}
	d.Name = p.parseIdent("workflow name")
	if d.Name == nil {
		p.syncItem()
		return nil
	}
	d.Params = p.parseParams()
	if p.at(TokArrow) {
		p.advance()
		d.Result = p.parseType()
	}
	if p.at(TokBang) {
		d.Effects = p.parseEffectClause()
	}
	d.Body = p.parseBlock()
	d.Sp = p.spanFrom(start)
	return d
}

func (p *parser) parseFn() Item {
	start := p.advance().Span.Start
	d := &FnDecl{}
	d.Name = p.parseIdent("function name")
	if d.Name == nil {
		p.syncItem()
		return nil
	}
	d.Generics = p.parseGenerics()
	d.Params = p.parseParams()
	if p.at(TokArrow) {
		p.advance()
		d.Result = p.parseType()
	}
	if p.at(TokBang) {
		d.Effects = p.parseEffectClause()
	}
	d.Body = p.parseBlock()
	d.Sp = p.spanFrom(start)
	return d
}

func (p *parser) parseTool() Item {
	start := p.advance().Span.Start
	d := &ToolDecl{}
	d.Name = p.parseIdent("tool name")
	if d.Name == nil {
		p.syncItem()
		return nil
	}
	d.Params = p.parseParams()
	if _, ok := p.expect(TokArrow); ok {
		d.Result = p.parseType()
	}
	if p.at(TokBang) {
		d.Effects = p.parseEffectClause()
	} else {
		p.errExpected("`!` and the tool's effects")
	}
	if p.at(TokSemi) {
		p.advance()
	}
	d.Sp = p.spanFrom(start)
	return d
}

func (p *parser) parseTypeDecl() Item {
	start := p.advance().Span.Start
	d := &TypeDecl{}
	d.Name = p.parseIdent("type name")
	if d.Name == nil {
		p.syncItem()
		return nil
	}
	d.Generics = p.parseGenerics()
	if _, ok := p.expect(TokAssign); !ok {
		p.syncItem()
		return nil
	}
	if _, ok := p.expect(TokLBrace); !ok {
		p.syncItem()
		return nil
	}
	for !p.at(TokRBrace) && !p.at(TokEOF) {
		fs := p.tok().Span.Start
		name := p.parseIdent("field name")
		if name == nil {
			break
		}
		fd := &FieldDecl{Name: name}
		if _, ok := p.expect(TokColon); ok {
			fd.TypeX = p.parseType()
		}
		fd.Sp = p.spanFrom(fs)
		d.Fields = append(d.Fields, fd)
		if !p.at(TokComma) {
			break
		}
		p.advance()
	}
	if _, ok := p.expect(TokRBrace); !ok {
		p.syncItem()
	}
	if p.at(TokSemi) {
		p.advance()
	}
	d.Sp = p.spanFrom(start)
	return d
}

func (p *parser) parseEnum() Item {
	start := p.advance().Span.Start
	d := &EnumDecl{}
	d.Name = p.parseIdent("enum name")
	if d.Name == nil {
		p.syncItem()
		return nil
	}
	d.Generics = p.parseGenerics()
	if _, ok := p.expect(TokLBrace); !ok {
		p.syncItem()
		return nil
	}
	for !p.at(TokRBrace) && !p.at(TokEOF) {
		vs := p.tok().Span.Start
		name := p.parseIdent("variant name")
		if name == nil {
			break
		}
		v := &VariantDecl{Name: name}
		if p.at(TokLParen) {
			p.advance()
			for !p.at(TokRParen) && !p.at(TokEOF) {
				v.Payload = append(v.Payload, p.parseType())
				if !p.at(TokComma) {
					break
				}
				p.advance()
			}
			p.expect(TokRParen)
		}
		v.Sp = p.spanFrom(vs)
		d.Variants = append(d.Variants, v)
		if !p.at(TokComma) {
			break
		}
		p.advance()
	}
	if _, ok := p.expect(TokRBrace); !ok {
		p.syncItem()
	}
	d.Sp = p.spanFrom(start)
	return d
}

func (p *parser) parseUse() Item {
	start := p.advance().Span.Start
	d := &UseDecl{}
	for {
		id := p.parseIdent("path segment")
		if id == nil {
			break
		}
		d.Path = append(d.Path, id)
		if !p.at(TokColons) {
			break
		}
		p.advance()
	}
	if len(d.Path) == 0 {
		p.syncItem()
		return nil
	}
	if _, ok := p.expect(TokSemi); !ok {
		p.syncItem()
	}
	d.Sp = p.spanFrom(start)
	return d
}

// ---- Types ----

func (p *parser) parseType() TypeExpr {
	start := p.tok().Span.Start
	if !p.enter() {
		p.leave()
		return &NamedTypeExpr{typeExprBase: typeExprBase{Sp: p.tok().Span}, Name: &Ident{Name: "_"}}
	}
	defer p.leave()
	switch p.tok().Kind {
	case TokIdent:
		name := p.parseIdent("type")
		t := &NamedTypeExpr{Name: name}
		if p.at(TokLt) {
			p.advance()
			for !p.at(TokGt) && !p.at(TokEOF) {
				t.Args = append(t.Args, p.parseType())
				if !p.at(TokComma) {
					break
				}
				p.advance()
			}
			p.expect(TokGt)
		}
		t.Sp = p.spanFrom(start)
		return t
	case TokLParen:
		p.advance()
		var elems []TypeExpr
		for !p.at(TokRParen) && !p.at(TokEOF) {
			elems = append(elems, p.parseType())
			if !p.at(TokComma) {
				break
			}
			p.advance()
		}
		p.expect(TokRParen)
		if p.at(TokArrow) {
			p.advance()
			res := p.parseType()
			ft := &FuncTypeExpr{Params: elems, Result: res}
			ft.Sp = p.spanFrom(start)
			return ft
		}
		if len(elems) == 1 {
			return elems[0] // parenthesized type
		}
		tt := &TupleTypeExpr{Elems: elems}
		tt.Sp = p.spanFrom(start)
		return tt
	}
	p.errExpected("a type")
	return &NamedTypeExpr{typeExprBase: typeExprBase{Sp: p.tok().Span}, Name: &Ident{exprBase: exprBase{Sp: p.tok().Span}, Name: "_"}}
}

// ---- Blocks and statements ----

// syncStmt skips to the end of a broken statement: past the next `;`, or
// up to a `}` or item keyword at the current depth.
func (p *parser) syncStmt() {
	depth := 0
	for !p.at(TokEOF) {
		switch p.tok().Kind {
		case TokSemi:
			if depth == 0 {
				p.advance()
				return
			}
		case TokLBrace, TokLParen, TokLBracket:
			depth++
		case TokRParen, TokRBracket:
			if depth > 0 {
				depth--
			}
		case TokRBrace:
			if depth == 0 {
				return
			}
			depth--
		default:
			if isItemStart(p.tok().Kind) {
				return
			}
		}
		p.advance()
	}
}

func (p *parser) parseBlock() *BlockExpr {
	start := p.tok().Span.Start
	b := &BlockExpr{}
	if _, ok := p.expect(TokLBrace); !ok {
		b.Sp = p.spanFrom(start)
		return b
	}
	if !p.enter() {
		p.leave()
		p.skipBalanced()
		b.Sp = p.spanFrom(start)
		return b
	}
	defer p.leave()
	saveNR, saveNL, saveChain := p.noRecord, p.noLambda, p.chain
	p.noRecord, p.noLambda = false, false
	defer func() { p.noRecord, p.noLambda, p.chain = saveNR, saveNL, saveChain }()
	for !p.at(TokRBrace) && !p.at(TokEOF) {
		if isItemStart(p.tok().Kind) {
			break // unterminated block; let the item parser take over
		}
		p.chain = saveChain
		before := p.pos
		errs := p.errCount()
		stmt, tail := p.parseStmt()
		if tail != nil {
			b.Tail = tail
			break
		}
		if stmt != nil {
			b.Stmts = append(b.Stmts, stmt)
		}
		if p.errCount() > errs && !p.at(TokRBrace) {
			p.syncStmt()
		}
		if p.pos == before {
			p.errExpected("a statement")
			p.advance()
		}
	}
	p.expect(TokRBrace)
	b.Sp = p.spanFrom(start)
	return b
}

// skipBalanced skips a brace-balanced region after an opening brace.
func (p *parser) skipBalanced() {
	depth := 1
	for !p.at(TokEOF) && depth > 0 {
		switch p.tok().Kind {
		case TokLBrace:
			depth++
		case TokRBrace:
			depth--
		}
		p.advance()
	}
}

// expectSemi consumes `;` or reports E0102 just after the previous token.
func (p *parser) expectSemi(what string) {
	if p.at(TokSemi) {
		p.advance()
		return
	}
	end := p.prevEnd()
	p.sink.errorOnce(CodeMissingSemi, Span{Start: end, End: end}, "expected `;`",
		"missing semicolon after %s", what).withHelp("Add `;` here.")
}

// parseStmt parses one statement. When the block ends with an expression
// without a semicolon it returns that expression as tail instead.
func (p *parser) parseStmt() (Stmt, Expr) {
	start := p.tok().Span.Start
	var annots []*Annotation
	if p.at(TokHash) {
		annots = p.parseAnnotations()
		if !p.at(TokLet) {
			for _, a := range annots {
				p.sink.errorf(CodeMisplacedAnnotation, a.Sp, "not allowed here",
					"annotation `%s` is only allowed on items and let statements", a.Name.Name)
			}
			annots = nil
		}
	}
	switch p.tok().Kind {
	case TokSemi:
		p.advance()
		return nil, nil
	case TokLet:
		return p.parseLet(start, annots), nil
	case TokFor:
		return p.parseFor(), nil
	case TokRequire:
		return p.parseRequire(), nil
	case TokReturn:
		p.advance()
		s := &ReturnStmt{}
		if !p.at(TokSemi) && !p.at(TokRBrace) {
			s.Value = p.parseExpr()
		}
		p.expectSemi("return statement")
		s.Sp = p.spanFrom(start)
		return s, nil
	case TokIdent:
		if p.peek(1).Kind == TokAssign {
			target := p.parseIdent("name")
			p.advance() // =
			s := &AssignStmt{Target: target, Value: p.parseExpr()}
			p.expectSemi("assignment")
			s.Sp = p.spanFrom(start)
			return s, nil
		}
	}
	errs := p.errCount()
	var x Expr
	blockLike := p.at(TokIf) || p.at(TokMatch) || p.at(TokLBrace)
	if blockLike {
		x = p.parsePrimary()
	} else {
		x = p.parseExpr()
	}
	switch {
	case p.at(TokSemi):
		p.advance()
		return &ExprStmt{Sp: p.spanFrom(start), X: x}, nil
	case p.at(TokRBrace):
		return nil, x
	case blockLike:
		return &ExprStmt{Sp: x.Span(), X: x}, nil
	}
	if p.errCount() == errs {
		p.expectSemi("expression statement")
	}
	return &ExprStmt{Sp: p.spanFrom(start), X: x}, nil
}

func (p *parser) parseLet(start Pos, annots []*Annotation) Stmt {
	p.advance() // let
	s := &LetStmt{Annot: annots}
	s.Pat = p.parsePattern()
	if p.at(TokColon) {
		p.advance()
		s.TypeX = p.parseType()
	}
	if _, ok := p.expect(TokAssign); ok {
		s.Value = p.parseExpr()
	} else {
		s.Value = &BadExpr{exprBase{Sp: p.tok().Span}}
	}
	if p.at(TokCompensate) {
		cs := p.advance().Span.Start
		body := p.parseExpr()
		it := &Ident{exprBase: exprBase{Sp: Span{Start: cs, End: cs}}, Name: "it"}
		s.Compensate = &Lambda{Params: []*LambdaParam{{Name: it}}, Body: body, Synthetic: true}
		s.Compensate.Sp = body.Span()
	}
	p.expectSemi("let statement")
	s.Sp = p.spanFrom(start)
	return s
}

func (p *parser) parseFor() Stmt {
	start := p.advance().Span.Start
	s := &ForStmt{}
	s.Pat = p.parsePattern()
	p.expect(TokIn)
	save := p.noRecord
	p.noRecord = true
	s.Iter = p.parseExpr()
	p.noRecord = save
	s.Body = p.parseBlock()
	s.Sp = p.spanFrom(start)
	return s
}

func (p *parser) parseRequire() Stmt {
	start := p.advance().Span.Start
	s := &RequireStmt{}
	if _, ok := p.expect(TokApproval); ok {
		if p.at(TokLParen) {
			s.Args = p.parseArgs()
		} else {
			p.errExpected("`(`")
		}
	}
	p.expectSemi("require statement")
	s.Sp = p.spanFrom(start)
	return s
}

// ---- Expressions ----

func (p *parser) bad() Expr { return &BadExpr{exprBase{Sp: p.tok().Span}} }

func (p *parser) parseExpr() Expr { return p.parsePipe() }

func (p *parser) parsePipe() Expr {
	x := p.parseOr()
	for p.at(TokPipe) {
		if !p.link() {
			return x
		}
		p.advance()
		rhs := p.parseOr()
		x = desugarPipe(x, rhs)
	}
	return x
}

// desugarPipe turns `a |> f(x)` into f(a, x) and `a |> f` into f(a).
func desugarPipe(a, rhs Expr) Expr {
	sp := a.Span().Join(rhs.Span())
	if c, ok := rhs.(*CallExpr); ok && c.Pipe == PipeNone && !c.paren {
		args := make([]*Arg, 0, len(c.Args)+1)
		args = append(args, &Arg{Value: a, Param: -1})
		args = append(args, c.Args...)
		return &CallExpr{exprBase: exprBase{Sp: sp}, Fn: c.Fn, Args: args, Pipe: PipeCall}
	}
	return &CallExpr{exprBase: exprBase{Sp: sp}, Fn: rhs, Args: []*Arg{{Value: a, Param: -1}}, Pipe: PipeBare}
}

func (p *parser) binary(op BinaryOp, x, y Expr) Expr {
	return &BinaryExpr{exprBase: exprBase{Sp: x.Span().Join(y.Span())}, Op: op, X: x, Y: y}
}

func (p *parser) parseOr() Expr {
	x := p.parseAnd()
	for p.at(TokOrOr) {
		if !p.link() {
			return x
		}
		p.advance()
		x = p.binary(OpOr, x, p.parseAnd())
	}
	return x
}

func (p *parser) parseAnd() Expr {
	x := p.parseEquality()
	for p.at(TokAndAnd) {
		if !p.link() {
			return x
		}
		p.advance()
		x = p.binary(OpAnd, x, p.parseEquality())
	}
	return x
}

var equalityOps = map[TokenKind]BinaryOp{TokEq: OpEq, TokNe: OpNe}
var relationalOps = map[TokenKind]BinaryOp{TokLt: OpLt, TokLe: OpLe, TokGt: OpGt, TokGe: OpGe, TokIn: OpIn}

// nonAssoc parses one non-associative level; a chain is reported as E0103
// and then parsed left-associatively for recovery.
func (p *parser) nonAssoc(ops map[TokenKind]BinaryOp, next func() Expr, level string) Expr {
	x := next()
	op, ok := ops[p.tok().Kind]
	if !ok {
		return x
	}
	p.advance()
	x = p.binary(op, x, next())
	for {
		op, ok := ops[p.tok().Kind]
		if !ok || !p.link() {
			return x
		}
		t := p.advance()
		p.sink.errorOnce(CodeNonAssocChain, t.Span, "second operator here",
			"%s operators cannot be chained", level).
			withHelp("Use parentheses to make the grouping explicit, or combine comparisons with &&.")
		x = p.binary(op, x, next())
	}
}

func (p *parser) parseEquality() Expr {
	return p.nonAssoc(equalityOps, p.parseRelational, "equality")
}

func (p *parser) parseRelational() Expr {
	return p.nonAssoc(relationalOps, p.parseRange, "comparison")
}

func (p *parser) parseRange() Expr {
	return p.nonAssoc(map[TokenKind]BinaryOp{TokDotDot: OpRange}, p.parseAdditive, "range")
}

func (p *parser) parseAdditive() Expr {
	x := p.parseMultiplicative()
	for p.at(TokPlus) || p.at(TokMinus) {
		if !p.link() {
			return x
		}
		op := OpAdd
		if p.advance().Kind == TokMinus {
			op = OpSub
		}
		x = p.binary(op, x, p.parseMultiplicative())
	}
	return x
}

func (p *parser) parseMultiplicative() Expr {
	x := p.parseUnary()
	for p.at(TokStar) || p.at(TokSlash) || p.at(TokPercent) {
		if !p.link() {
			return x
		}
		op := map[TokenKind]BinaryOp{TokStar: OpMul, TokSlash: OpDiv, TokPercent: OpRem}[p.advance().Kind]
		x = p.binary(op, x, p.parseUnary())
	}
	return x
}

func (p *parser) parseUnary() Expr {
	if !p.enter() {
		p.leave()
		b := p.bad()
		p.advance()
		return b
	}
	defer p.leave()
	if p.at(TokMinus) || p.at(TokBang) {
		t := p.advance()
		op := OpNeg
		if t.Kind == TokBang {
			op = OpNot
		}
		x := p.parseUnary()
		return &UnaryExpr{exprBase: exprBase{Sp: Span{Start: t.Span.Start, End: x.Span().End}}, Op: op, X: x}
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() Expr {
	x := p.parsePrimary()
	for {
		start := x.Span().Start
		switch p.tok().Kind {
		case TokLParen:
			if !p.link() {
				return x
			}
			args := p.parseArgs()
			x = &CallExpr{exprBase: exprBase{Sp: p.spanFrom(start)}, Fn: x, Args: args}
		case TokLBracket:
			if !p.link() {
				return x
			}
			p.advance()
			save := p.noRecord
			p.noRecord = false
			idx := p.parseExpr()
			p.noRecord = save
			p.expect(TokRBracket)
			x = &IndexExpr{exprBase: exprBase{Sp: p.spanFrom(start)}, X: x, Index: idx}
		case TokDot:
			if !p.link() {
				return x
			}
			p.advance()
			name := p.parseIdent("field name")
			if name == nil {
				return x
			}
			x = &FieldExpr{exprBase: exprBase{Sp: p.spanFrom(start)}, X: x, Name: name, Index: -1}
		case TokQuestion:
			if !p.link() {
				return x
			}
			p.advance()
			x = &TryExpr{exprBase: exprBase{Sp: p.spanFrom(start)}, X: x}
		case TokAs:
			if !p.link() {
				return x
			}
			p.advance()
			to := p.parseType()
			x = &CastExpr{exprBase: exprBase{Sp: p.spanFrom(start)}, X: x, To: to}
		default:
			return x
		}
	}
}

// parseArgs parses `( [arg {, arg} [,]] )`.
func (p *parser) parseArgs() []*Arg {
	p.advance() // (
	save, saveNL, chain := p.noRecord, p.noLambda, p.chain
	p.noRecord, p.noLambda = false, false
	defer func() { p.noRecord, p.noLambda, p.chain = save, saveNL, chain }()
	var args []*Arg
	for !p.at(TokRParen) && !p.at(TokEOF) {
		p.chain = chain
		a := &Arg{Param: -1}
		if p.at(TokIdent) && (p.peek(1).Kind == TokAssign || p.peek(1).Kind == TokColon) {
			a.Name = p.parseIdent("argument name")
			p.advance()
		}
		a.Value = p.parseExpr()
		args = append(args, a)
		if !p.at(TokComma) {
			break
		}
		p.advance()
	}
	if _, ok := p.expect(TokRParen); !ok {
		p.skipTo(TokRParen)
	}
	return args
}

// skipTo skips to and past the next token of kind k at the current
// nesting, stopping early at `;`, `}` or an item keyword.
func (p *parser) skipTo(k TokenKind) {
	depth := 0
	for !p.at(TokEOF) {
		t := p.tok().Kind
		if depth == 0 {
			if t == k {
				p.advance()
				return
			}
			if t == TokSemi || t == TokRBrace || isItemStart(t) {
				return
			}
		}
		switch t {
		case TokLParen, TokLBracket, TokLBrace:
			depth++
		case TokRParen, TokRBracket, TokRBrace:
			if depth > 0 {
				depth--
			}
		}
		p.advance()
	}
}

// lambdaAhead reports whether a parenthesized lambda parameter list starts
// at the current `(`.
func (p *parser) lambdaAhead() bool {
	i := 1
	if p.peek(i).Kind == TokRParen {
		return p.peek(i+1).Kind == TokFatArrow
	}
	for {
		if p.peek(i).Kind != TokIdent {
			return false
		}
		i++
		switch p.peek(i).Kind {
		case TokComma:
			i++
		case TokRParen:
			return p.peek(i+1).Kind == TokFatArrow
		default:
			return false
		}
	}
}

// recordAhead reports whether the `{` at the current position starts a
// record literal body.
func (p *parser) recordAhead() bool {
	if p.noRecord || !p.at(TokLBrace) {
		return false
	}
	n := p.peek(1).Kind
	if n == TokRBrace {
		return true
	}
	return n == TokIdent && (p.peek(2).Kind == TokColon || p.peek(2).Kind == TokAssign)
}

func (p *parser) parseLambda() Expr {
	start := p.tok().Span.Start
	l := &Lambda{}
	if p.at(TokIdent) {
		l.Params = []*LambdaParam{{Name: p.parseIdent("parameter")}}
	} else {
		p.advance() // (
		for p.at(TokIdent) {
			l.Params = append(l.Params, &LambdaParam{Name: p.parseIdent("parameter")})
			if !p.at(TokComma) {
				break
			}
			p.advance()
		}
		p.expect(TokRParen)
	}
	p.expect(TokFatArrow)
	l.Body = p.parseExpr()
	l.Sp = Span{Start: start, End: l.Body.Span().End}
	return l
}

func (p *parser) parsePrimary() Expr {
	t := p.tok()
	start := t.Span.Start
	switch t.Kind {
	case TokInt:
		p.advance()
		return &IntLit{exprBase: exprBase{Sp: t.Span}, Value: t.Int}
	case TokFloat:
		p.advance()
		return &FloatLit{exprBase: exprBase{Sp: t.Span}, Value: t.Float}
	case TokDuration:
		p.advance()
		return &DurationLit{exprBase: exprBase{Sp: t.Span}, Nanos: t.Int}
	case TokTrue, TokFalse:
		p.advance()
		return &BoolLit{exprBase: exprBase{Sp: t.Span}, Value: t.Kind == TokTrue}
	case TokString:
		p.advance()
		return p.stringLit(t)
	case TokIdent:
		if p.peek(1).Kind == TokFatArrow && !p.noLambda {
			return p.parseLambda()
		}
		id := p.parseIdent("expression")
		var x Expr = id
		segs := []*Ident{id}
		for p.at(TokColons) {
			p.advance()
			seg := p.parseIdent("path segment")
			if seg == nil {
				break
			}
			segs = append(segs, seg)
		}
		if len(segs) > 1 {
			x = &PathExpr{exprBase: exprBase{Sp: p.spanFrom(start)}, Segs: segs}
		}
		if p.recordAhead() {
			return p.parseRecordLit(start, segs)
		}
		return x
	case TokLParen:
		if !p.noLambda && p.lambdaAhead() {
			return p.parseLambda()
		}
		p.advance()
		save, saveNL, chain := p.noRecord, p.noLambda, p.chain
		p.noRecord, p.noLambda = false, false
		var elems []Expr
		trailingComma := false
		for !p.at(TokRParen) && !p.at(TokEOF) {
			p.chain = chain
			elems = append(elems, p.parseExpr())
			trailingComma = false
			if !p.at(TokComma) {
				break
			}
			p.advance()
			trailingComma = true
		}
		p.noRecord, p.noLambda, p.chain = save, saveNL, chain
		if _, ok := p.expect(TokRParen); !ok {
			p.skipTo(TokRParen)
		}
		if len(elems) == 1 && !trailingComma {
			// A parenthesized call is not re-desugared by a pipe:
			// `a |> (f(x))` calls the result of f(x) with a.
			if c, ok := elems[0].(*CallExpr); ok {
				c.paren = true
			}
			return elems[0] // parenthesized expression
		}
		if len(elems) == 1 {
			// `(x,)` is not a tuple form of this language.
			return elems[0]
		}
		return &TupleExpr{exprBase: exprBase{Sp: p.spanFrom(start)}, Elems: elems}
	case TokLBracket:
		p.advance()
		save, saveNL, chain := p.noRecord, p.noLambda, p.chain
		p.noRecord, p.noLambda = false, false
		l := &ListExpr{}
		for !p.at(TokRBracket) && !p.at(TokEOF) {
			p.chain = chain
			l.Elems = append(l.Elems, p.parseExpr())
			if !p.at(TokComma) {
				break
			}
			p.advance()
		}
		p.noRecord, p.noLambda, p.chain = save, saveNL, chain
		if _, ok := p.expect(TokRBracket); !ok {
			p.skipTo(TokRBracket)
		}
		l.Sp = p.spanFrom(start)
		return l
	case TokIf:
		return p.parseIf()
	case TokMatch:
		return p.parseMatch()
	case TokAgent:
		return p.parseAgent()
	case TokLBrace:
		return p.parseBlock()
	}
	p.errExpected("an expression")
	return p.bad()
}

func (p *parser) parseRecordLit(start Pos, path []*Ident) Expr {
	p.advance() // {
	save, saveNL, chain := p.noRecord, p.noLambda, p.chain
	p.noRecord, p.noLambda = false, false
	r := &RecordLit{Path: path}
	for !p.at(TokRBrace) && !p.at(TokEOF) {
		p.chain = chain
		name := p.parseIdent("field name")
		if name == nil {
			break
		}
		if p.at(TokColon) || p.at(TokAssign) {
			p.advance()
		} else {
			p.errExpected("`:`")
		}
		r.Fields = append(r.Fields, &FieldInit{Name: name, Value: p.parseExpr(), Index: -1})
		if !p.at(TokComma) {
			break
		}
		p.advance()
	}
	p.noRecord, p.noLambda, p.chain = save, saveNL, chain
	if _, ok := p.expect(TokRBrace); !ok {
		p.skipTo(TokRBrace)
	}
	r.Sp = p.spanFrom(start)
	return r
}

func (p *parser) parseIf() Expr {
	start := p.advance().Span.Start
	e := &IfExpr{}
	save := p.noRecord
	p.noRecord = true
	e.Cond = p.parseExpr()
	p.noRecord = save
	e.Then = p.parseBlock()
	if p.at(TokElse) {
		p.advance()
		if p.at(TokIf) {
			e.Else = p.parseIf()
		} else {
			e.Else = p.parseBlock()
		}
	}
	e.Sp = p.spanFrom(start)
	return e
}

func (p *parser) parseMatch() Expr {
	start := p.advance().Span.Start
	m := &MatchExpr{}
	save := p.noRecord
	p.noRecord = true
	m.Scrutinee = p.parseExpr()
	p.noRecord = save
	if _, ok := p.expect(TokLBrace); !ok {
		m.Sp = p.spanFrom(start)
		return m
	}
	saveNR := p.noRecord
	p.noRecord = false
	chain := p.chain
	for !p.at(TokRBrace) && !p.at(TokEOF) {
		p.chain = chain
		as := p.tok().Span.Start
		errs := p.errCount()
		arm := &MatchArm{Pat: p.parsePattern()}
		if p.at(TokIf) {
			p.advance()
			p.noLambda = true
			arm.Guard = p.parseExpr()
			p.noLambda = false
		}
		if _, ok := p.expect(TokFatArrow); ok {
			arm.Body = p.parseExpr()
		} else {
			arm.Body = p.bad()
		}
		arm.Sp = p.spanFrom(as)
		m.Arms = append(m.Arms, arm)
		if p.at(TokComma) {
			p.advance()
			continue
		}
		if p.at(TokRBrace) {
			break
		}
		if _, ok := arm.Body.(*BlockExpr); ok {
			continue // a block-bodied arm may omit the comma
		}
		if p.errCount() == errs {
			p.errExpected("`,` or `}` after match arm")
		}
		// Recover: skip to the next comma or the closing brace.
		depth := 0
		for !p.at(TokEOF) {
			k := p.tok().Kind
			if depth == 0 && (k == TokComma || k == TokRBrace) {
				break
			}
			if isItemStart(k) {
				break
			}
			switch k {
			case TokLParen, TokLBracket, TokLBrace:
				depth++
			case TokRParen, TokRBracket, TokRBrace:
				depth--
			}
			p.advance()
		}
		if p.at(TokComma) {
			p.advance()
		}
		if isItemStart(p.tok().Kind) {
			break
		}
	}
	p.noRecord = saveNR
	p.expect(TokRBrace)
	m.Sp = p.spanFrom(start)
	return m
}

func (p *parser) parseAgent() Expr {
	start := p.advance().Span.Start
	a := &AgentExpr{}
	a.Name = p.parseIdent("agent name")
	if p.at(TokLParen) {
		a.Args = p.parseArgs()
		for _, arg := range a.Args {
			if arg.Name == nil {
				p.sink.errorOnce(CodeUnexpectedToken, arg.Value.Span(), "expected `name = value`",
					"agent arguments must be named")
			}
		}
	} else {
		p.errExpected("`(`")
	}
	a.Sp = p.spanFrom(start)
	return a
}

// stringLit builds a StringLit, parsing interpolated expressions.
func (p *parser) stringLit(t Token) Expr {
	s := &StringLit{exprBase: exprBase{Sp: t.Span}}
	for _, part := range t.Parts {
		if !part.Interp {
			s.Parts = append(s.Parts, StrPart{Lit: part.Lit})
			continue
		}
		if len(part.Tokens) <= 1 {
			s.Parts = append(s.Parts, StrPart{X: &BadExpr{exprBase{Sp: part.Span}}})
			continue
		}
		sub := &parser{toks: part.Tokens, sink: p.sink, file: p.file, depth: p.depth}
		if !sub.enter() {
			s.Parts = append(s.Parts, StrPart{X: &BadExpr{exprBase{Sp: part.Span}}})
			continue
		}
		x := sub.parseExpr()
		if !sub.at(TokEOF) {
			sub.errExpected("`}` to end the interpolation")
		}
		if sub.tooDeep {
			p.tooDeep = true
		}
		s.Parts = append(s.Parts, StrPart{X: x})
	}
	return s
}

// ---- Patterns ----

func (p *parser) parsePattern() Pattern {
	start := p.tok().Span.Start
	if !p.enter() {
		p.leave()
		sp := p.tok().Span
		p.advance()
		return &WildcardPat{patBase{Sp: sp}}
	}
	defer p.leave()
	t := p.tok()
	switch t.Kind {
	case TokIdent:
		if t.Text == "_" {
			p.advance()
			return &WildcardPat{patBase{Sp: t.Span}}
		}
		segs := []*Ident{p.parseIdent("pattern")}
		for p.at(TokColons) {
			p.advance()
			seg := p.parseIdent("path segment")
			if seg == nil {
				break
			}
			segs = append(segs, seg)
		}
		switch {
		case p.at(TokLParen):
			p.advance()
			vp := &VariantPat{Path: segs, HasParens: true}
			for !p.at(TokRParen) && !p.at(TokEOF) {
				vp.Args = append(vp.Args, p.parsePattern())
				if !p.at(TokComma) {
					break
				}
				p.advance()
			}
			p.expect(TokRParen)
			vp.Sp = p.spanFrom(start)
			return vp
		case p.at(TokLBrace) && (p.peek(1).Kind == TokRBrace || p.peek(1).Kind == TokIdent):
			p.advance()
			rp := &RecordPat{Path: segs}
			for !p.at(TokRBrace) && !p.at(TokEOF) {
				name := p.parseIdent("field name")
				if name == nil {
					break
				}
				fp := &FieldPat{Name: name, Index: -1}
				if p.at(TokColon) {
					p.advance()
					fp.Pat = p.parsePattern()
				}
				rp.Fields = append(rp.Fields, fp)
				if !p.at(TokComma) {
					break
				}
				p.advance()
			}
			p.expect(TokRBrace)
			rp.Sp = p.spanFrom(start)
			return rp
		case len(segs) > 1:
			return &VariantPat{patBase: patBase{Sp: p.spanFrom(start)}, Path: segs}
		}
		return &BindPat{patBase: patBase{Sp: t.Span}, Name: segs[0]}
	case TokInt, TokFloat, TokDuration, TokString, TokTrue, TokFalse:
		lit := p.parsePrimary()
		if s, ok := lit.(*StringLit); ok && s.Interpolated() {
			p.sink.errorf(CodeUnexpectedToken, s.Span(), "interpolation in pattern",
				"string patterns cannot contain interpolations")
		}
		return &LitPat{patBase: patBase{Sp: lit.Span()}, Lit: lit}
	case TokMinus:
		n := p.peek(1)
		if n.Kind == TokInt || n.Kind == TokFloat {
			p.advance()
			p.advance()
			sp := p.spanFrom(start)
			if n.Kind == TokInt {
				return &LitPat{patBase: patBase{Sp: sp}, Lit: &IntLit{exprBase: exprBase{Sp: sp}, Value: -n.Int}}
			}
			return &LitPat{patBase: patBase{Sp: sp}, Lit: &FloatLit{exprBase: exprBase{Sp: sp}, Value: -n.Float}}
		}
	case TokLParen:
		p.advance()
		var elems []Pattern
		for !p.at(TokRParen) && !p.at(TokEOF) {
			elems = append(elems, p.parsePattern())
			if !p.at(TokComma) {
				break
			}
			p.advance()
		}
		p.expect(TokRParen)
		if len(elems) == 1 {
			return elems[0]
		}
		return &TuplePat{patBase: patBase{Sp: p.spanFrom(start)}, Elems: elems}
	}
	p.errExpected("a pattern")
	sp := p.tok().Span
	if !p.at(TokFatArrow) && !p.at(TokRBrace) && !p.at(TokEOF) && !p.at(TokAssign) {
		p.advance()
	}
	return &WildcardPat{patBase{Sp: sp}}
}
