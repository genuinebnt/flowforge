package jsparse

import "sort"

// Options configures Parse.
type Options struct {
	// SourceType selects the Script or Module goal symbol.
	SourceType SourceType
	// Tolerant enables error-tolerant mode: every problem becomes a
	// diagnostic and the parser resynchronizes at the next statement
	// boundary, always returning a best-effort Program and a nil error.
	Tolerant bool
	// Tokens records every input element, trivia included, in
	// Program.Tokens.
	Tokens bool
	// MaxDepth bounds syntactic nesting (statements, expressions and
	// binary-operator chains). 0 means DefaultMaxDepth.
	MaxDepth int
}

// DefaultMaxDepth is the nesting limit used when Options.MaxDepth is 0.
const DefaultMaxDepth = 4000

// Parse parses src (UTF-8) as an ECMAScript 2024 Script or Module.
//
// In strict mode (the default) it returns the first syntax or early error
// as *Error (with the partial Program). In tolerant mode it never returns
// an error; diagnostics are in Program.Diagnostics. Parse never panics on
// any input.
func Parse(src []byte, opts Options) (*Program, error) {
	p := newParser(string(src), opts)
	p.parseProgram()
	prog := p.program()
	if !opts.Tolerant && len(p.diags) > 0 {
		return prog, &Error{Diagnostic: p.diags[0]}
	}
	return prog, nil
}

// ParseScript parses src as a Script in strict mode.
func ParseScript(src []byte) (*Program, error) { return Parse(src, Options{}) }

// ParseModule parses src as a Module in strict mode.
func ParseModule(src []byte) (*Program, error) {
	return Parse(src, Options{SourceType: SourceModule})
}

// parser holds the state of one parse. Errors never unwind with panics:
// a fatal error "halts" the parser, which replaces the current token by
// EOF so every loop terminates and every production returns; tolerant
// mode then resynchronizes at the nearest statement list.
type parser struct {
	src    string
	lex    *Lexer
	opts   Options
	module bool

	tok      Token // current token
	prevEnd  int32 // end of the previously consumed token
	havePeek bool
	peekTok  Token

	nodes    []Node
	children []NodeID
	scratch  []NodeID
	tokens   []Token

	diags    []Diagnostic
	halted   bool
	haltTok  Token
	depth    int
	maxDepth int

	fns       []fnCtx
	scopes    []scope
	classes   []classCtx
	cover     []coverErr
	exports   exportState
	tmplDepth int // open template substitutions, for goal labelling

	// Position of the last `await` identifier reference, used by the
	// async arrow parameter checks.
	lastAwaitIdent int32
	// bound collects the names bound by the parameter list being parsed.
	bound *[]boundName
	// skipping is set while resync discards tokens.
	skipping bool
}

func newParser(src string, opts Options) *parser {
	p := &parser{src: src, opts: opts, module: opts.SourceType == SourceModule}
	p.lex = newLexer(src, p.module)
	p.lex.recordTrivia = opts.Tokens
	p.maxDepth = opts.MaxDepth
	if p.maxDepth <= 0 {
		p.maxDepth = DefaultMaxDepth
	}
	p.lastAwaitIdent = -1
	est := len(src)/6 + 16
	p.nodes = make([]Node, 0, est)
	p.children = make([]NodeID, 0, est/2)
	p.tok = p.lex.Next(GoalDiv)
	p.checkLexError()
	return p
}

func (p *parser) program() *Program {
	prog := &Program{
		Nodes:       p.nodes,
		Children:    p.children,
		Root:        0,
		SourceType:  p.opts.SourceType,
		Source:      p.src,
		Diagnostics: p.diags,
		Comments:    p.lex.comments,
	}
	if p.opts.Tokens {
		all := make([]Token, 0, len(p.tokens)+len(p.lex.trivia))
		all = append(all, p.tokens...)
		all = append(all, p.lex.trivia...)
		sort.SliceStable(all, func(i, j int) bool { return all[i].Start < all[j].Start })
		prog.Tokens = all
	}
	return prog
}

// ---- tokens ----

// next consumes the current token and lexes the following one under
// InputElementDiv. Positions that admit a RegularExpressionLiteral or a
// TemplateMiddle/TemplateTail re-request the token under the matching
// goal with regoalRegExp / regoalTemplateTail before inspecting it.
func (p *parser) next() {
	if p.halted {
		return
	}
	p.prevEnd = p.tok.End
	if p.opts.Tokens && p.tok.Kind != TEOF {
		p.tokens = append(p.tokens, p.tok)
	}
	if p.havePeek {
		p.tok = p.peekTok
		p.havePeek = false
	} else {
		p.tok = p.lex.Next(GoalDiv)
	}
	p.checkLexError()
}

// peek returns the token after the current one (lexed under
// InputElementDiv, which is the goal at every position where the parser
// peeks: after identifiers and keywords that are not operators).
func (p *parser) peek() *Token {
	if p.halted {
		return &p.tok
	}
	if !p.havePeek {
		p.peekTok = p.lex.Next(GoalDiv)
		p.havePeek = true
	}
	return &p.peekTok
}

// regoal re-lexes the current token under goal. It is only called on a
// token that begins with `/` (RegExp goals) or `}` (TemplateTail goals),
// the only code points where the goals differ.
func (p *parser) regoal(goal Goal) {
	if p.halted {
		return
	}
	p.havePeek = false
	p.lex.rewindTo(&p.tok)
	p.tok = p.lex.Next(goal)
	p.checkLexError()
}

func (p *parser) regoalRegExp() {
	g := GoalRegExp
	if p.tmplDepth > 0 {
		g = GoalRegExpOrTemplateTail
	}
	p.regoal(g)
}

func (p *parser) regoalTemplateTail() {
	g := GoalTemplateTail
	p.regoal(g)
}

// checkLexError reports a lexical error attached to the current token.
// Error tokens (TInvalid) halt the parser; other tokens with an error
// (a malformed escape, a misplaced separator) are still usable.
func (p *parser) checkLexError() {
	t := &p.tok
	if t.Err == CodeNone {
		return
	}
	span := Span{t.ErrAt, t.ErrAt + 1}
	if span.Start < t.Start || span.Start >= t.End {
		span = Span{t.Start, t.End}
	}
	msg := t.Err.String()
	if p.skipping {
		p.diags = append(p.diags, Diagnostic{Code: t.Err, Span: span, Message: msg})
		return
	}
	if t.Kind == TInvalid || t.Err == CodeUnterminatedString || t.Err == CodeUnterminatedTemplate || t.Err == CodeUnterminatedRegExp {
		p.syntaxErrorAt(t.Err, span, msg)
		return
	}
	p.report(t.Err, span, msg)
}

func (p *parser) at(k TokenKind) bool { return p.tok.Kind == k }

// isKw reports whether the current token is keyword kw written without
// escapes.
func (p *parser) isKw(kw Keyword) bool {
	return p.tok.Kind == TIdentifier && p.tok.Kw == kw && p.tok.Flags&FlagEscaped == 0
}

func isKwTok(t *Token, kw Keyword) bool {
	return t.Kind == TIdentifier && t.Kw == kw && t.Flags&FlagEscaped == 0
}

func (p *parser) newlineBefore() bool { return p.tok.Flags&FlagNewlineBefore != 0 }

func (p *parser) eat(k TokenKind) bool {
	if p.tok.Kind == k {
		p.next()
		return true
	}
	return false
}

func (p *parser) expect(k TokenKind) {
	if p.tok.Kind == k {
		p.next()
		return
	}
	p.unexpectedExpected(k)
}

func (p *parser) unexpectedExpected(k TokenKind) {
	if p.tok.Kind == TEOF {
		p.syntaxError(CodeUnexpectedEOF, "expected "+k.String())
		return
	}
	p.syntaxError(CodeExpectedToken, "expected "+k.String())
}

func (p *parser) unexpected() {
	switch p.tok.Kind {
	case TEOF:
		p.syntaxError(CodeUnexpectedEOF, "unexpected end of input")
	case TInvalid:
		p.syntaxError(CodeUnexpectedCharacter, "unexpected character")
	default:
		p.syntaxError(CodeUnexpectedToken, "unexpected token "+p.tokText())
	}
}

func (p *parser) tokText() string {
	t := &p.tok
	if t.End-t.Start > 32 {
		return p.src[t.Start:t.Start+32] + "..."
	}
	return p.src[t.Start:t.End]
}

// ---- diagnostics ----

// report records a non-fatal diagnostic (early errors, usable-token
// lexical errors). In strict mode the first one halts the parse.
func (p *parser) report(c Code, span Span, msg string) {
	if p.halted {
		return
	}
	p.diags = append(p.diags, Diagnostic{Code: c, Span: span, Message: msg})
	if !p.opts.Tolerant {
		p.halt()
	}
}

// early records an early error at span.
func (p *parser) early(c Code, span Span, msg string) { p.report(c, span, msg) }

// syntaxError records a fatal error at the current token and halts.
func (p *parser) syntaxError(c Code, msg string) {
	p.syntaxErrorAt(c, Span{p.tok.Start, p.tok.End}, msg)
}

func (p *parser) syntaxErrorAt(c Code, span Span, msg string) {
	if p.halted {
		return
	}
	p.diags = append(p.diags, Diagnostic{Code: c, Span: span, Message: msg})
	p.halt()
}

// halt stops parsing: the current token becomes EOF. Tolerant mode
// resumes in resync.
func (p *parser) halt() {
	if p.halted {
		return
	}
	p.haltTok = p.tok
	p.halted = true
	p.havePeek = false
	p.tok = Token{Kind: TEOF, Start: p.tok.Start, End: p.tok.Start}
}

// ---- nodes ----

func (p *parser) newNode(k Kind, start int32) NodeID {
	p.nodes = append(p.nodes, Node{Kind: k, Span: Span{start, start}, A: NoNode, B: NoNode, C: NoNode, D: NoNode})
	return NodeID(len(p.nodes) - 1)
}

// done sets the end of id to the end of the last consumed token.
func (p *parser) done(id NodeID) NodeID {
	end := p.prevEnd
	if end < p.nodes[id].Span.Start {
		end = p.nodes[id].Span.Start
	}
	p.nodes[id].Span.End = end
	return id
}

func (p *parser) nd(id NodeID) *Node { return &p.nodes[id] }

func (p *parser) start() int32 { return p.tok.Start }

// push appends id to the list being built.
func (p *parser) push(id NodeID) { p.scratch = append(p.scratch, id) }

// list moves scratch[mark:] into the children slice.
func (p *parser) list(mark int) NodeList {
	items := p.scratch[mark:]
	l := NodeList{Start: int32(len(p.children)), Len: int32(len(items))}
	p.children = append(p.children, items...)
	p.scratch = p.scratch[:mark]
	return l
}

func (p *parser) listOf(ids ...NodeID) NodeList {
	l := NodeList{Start: int32(len(p.children)), Len: int32(len(ids))}
	p.children = append(p.children, ids...)
	return l
}

func (p *parser) ids(l NodeList) []NodeID { return p.children[l.Start : l.Start+l.Len] }

func (p *parser) ident(name string, span Span) NodeID {
	id := p.newNode(KIdentifier, span.Start)
	n := p.nd(id)
	n.Span.End = span.End
	n.Str = name
	return id
}

// enter / leave bound the nesting depth.
func (p *parser) enter() bool {
	p.depth++
	if p.depth > p.maxDepth {
		p.syntaxError(CodeTooDeep, "nesting too deep")
		return false
	}
	return true
}

func (p *parser) leave() { p.depth-- }

// ---- program ----

func (p *parser) parseProgram() {
	root := p.newNode(KProgram, 0)
	p.pushFn(fnCtx{top: true, async: p.module, strict: p.module})
	p.pushScope(scopeTop | scopeVarBoundary)
	mark := len(p.scratch)
	p.parseDirectives(nil)
	p.parseStatementList(TEOF, true)
	body := p.list(mark)
	if p.module {
		p.checkExports()
	}
	p.popScope()
	strict := p.cur().strict
	p.popFn()
	n := p.nd(root)
	n.List = body
	n.Span.End = int32(len(p.src))
	if strict {
		n.Flags |= NStrict
	}
}
