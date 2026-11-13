package jsparse

// Static semantics (early errors) bookkeeping: function contexts, lexical
// scopes for redeclaration checks (ECMA-262 8.2 / 14.2.1 / 16.1.1 and
// Annex B.3.2-B.3.5), labels, private names, exports and cover-grammar
// errors whose fate depends on later tokens.

// fnCtx is the syntactic context of the innermost function-like body
// (function, arrow, method, class field initializer, static block or
// the program). It models the [Yield], [Await] and [Return] grammar
// parameters plus the "Contains" early errors.
type fnCtx struct {
	top         bool // Script or Module body
	async       bool // [+Await]: await is AwaitExpression
	generator   bool // [+Yield]: yield is YieldExpression
	arrow       bool
	method      bool // super.x allowed
	superCall   bool // super() allowed (derived constructor)
	newTarget   bool // new.target allowed
	fieldInit   bool // class field initializer: arguments forbidden
	staticBlock bool // class static block: await reserved, arguments forbidden
	awaitIdent  bool // await is reserved as an identifier here (async arrow params, field initializers of async contexts)
	inParams    bool // parsing FormalParameters
	strict      bool
	returnOK    bool

	labels    []label
	loopDepth int
	brkDepth  int // loops and switches

	// Positions of the last yield/await expression in this context (-1 none).
	yieldPos int32
	awaitPos int32
}

type label struct {
	name   string
	isLoop bool
	direct bool // labels the statement currently being parsed
}

func (p *parser) cur() *fnCtx { return &p.fns[len(p.fns)-1] }

func (p *parser) pushFn(c fnCtx) {
	c.yieldPos, c.awaitPos = -1, -1
	p.fns = append(p.fns, c)
}

func (p *parser) popFn() { p.fns = p.fns[:len(p.fns)-1] }

func (p *parser) strict() bool { return p.cur().strict }

// argumentsForbidden reports whether `arguments` is an early error here:
// inside a class field initializer or static block, not crossing a
// non-arrow function boundary.
func (p *parser) argumentsForbidden() bool {
	for i := len(p.fns) - 1; i >= 0; i-- {
		c := &p.fns[i]
		if c.fieldInit || c.staticBlock {
			return true
		}
		if !c.arrow {
			return false
		}
	}
	return false
}

// inStaticBlock reports whether the innermost context is a class static
// block (where await is reserved and AwaitExpression forbidden).
func (p *parser) inStaticBlock() bool {
	// Arrow functions are function boundaries for this rule (15.7.1).
	return p.cur().staticBlock
}

// ---- scopes ----

type scopeFlags uint8

const (
	scopeTop         scopeFlags = 1 << iota
	scopeFunction               // function body (functions are var-like here)
	scopeVarBoundary            // var declarations stop here
	scopeSimpleCatch            // catch (identifier)
)

// nameSet is a set of names: a slice while it holds at most 16 (cheaper
// than hashing for the usual block), then a map. It is only ever
// queried, never iterated, so map order cannot reach a diagnostic.
type nameSet struct {
	small []string
	big   map[string]struct{}
}

func (s *nameSet) has(n string) bool {
	if s.big != nil {
		_, ok := s.big[n]
		return ok
	}
	for _, x := range s.small {
		if x == n {
			return true
		}
	}
	return false
}

func (s *nameSet) add(n string) {
	if s.big != nil {
		s.big[n] = struct{}{}
		return
	}
	if len(s.small) < 16 {
		s.small = append(s.small, n)
		return
	}
	s.big = make(map[string]struct{}, 32)
	for _, x := range s.small {
		s.big[x] = struct{}{}
	}
	s.big[n] = struct{}{}
}

func (s *nameSet) reset() {
	s.small = s.small[:0]
	s.big = nil
}

type scope struct {
	flags      scopeFlags
	vars       nameSet
	lex        nameSet
	funcs      nameSet
	catchParam string
}

func (p *parser) pushScope(f scopeFlags) {
	if len(p.scopes) < cap(p.scopes) {
		p.scopes = p.scopes[:len(p.scopes)+1]
		s := &p.scopes[len(p.scopes)-1]
		s.flags = f
		s.vars.reset()
		s.lex.reset()
		s.funcs.reset()
		s.catchParam = ""
		return
	}
	p.scopes = append(p.scopes, scope{flags: f})
}

func (p *parser) popScope() { p.scopes = p.scopes[:len(p.scopes)-1] }

func (p *parser) curScope() *scope { return &p.scopes[len(p.scopes)-1] }

type bindKind uint8

const (
	bindNone        bindKind = iota // no declaration (expression names)
	bindVar                         // var, parameters
	bindLexical                     // let, const, class, strict or generator/async block functions
	bindFunction                    // sloppy-mode plain function declaration
	bindSimpleCatch                 // catch (e)
)

// functionsAsVar reports whether function declarations in s bind like
// var (function bodies and the top level of a script) rather than like
// let (blocks, module top level).
func (p *parser) functionsAsVar(s *scope) bool {
	return s.flags&scopeFunction != 0 || (!p.module && s.flags&scopeTop != 0)
}

// declare records a binding and reports redeclarations (the
// LexicallyDeclaredNames / VarDeclaredNames early errors of 14.2.1,
// 15.2.1 and 16.1.1). A var is added to every scope up to its function
// (or the top), so a later lexical declaration in an enclosing block
// sees it.
func (p *parser) declare(name string, kind bindKind, span Span) {
	if kind == bindNone || len(p.scopes) == 0 {
		return
	}
	redeclared := false
	s := p.curScope()
	switch kind {
	case bindLexical:
		redeclared = s.lex.has(name) || s.funcs.has(name) || s.vars.has(name)
		s.lex.add(name)
	case bindSimpleCatch:
		s.lex.add(name)
		s.catchParam = name
	case bindFunction:
		if p.functionsAsVar(s) {
			redeclared = s.lex.has(name)
		} else {
			redeclared = s.lex.has(name) || s.vars.has(name)
		}
		s.funcs.add(name)
	case bindVar:
		for i := len(p.scopes) - 1; i >= 0; i-- {
			sc := &p.scopes[i]
			// B.3.4: var may redeclare a simple catch parameter (since
			// ES2020 also in for-of heads).
			catchOK := sc.flags&scopeSimpleCatch != 0 && sc.catchParam == name
			if (sc.lex.has(name) && !catchOK) || (!p.functionsAsVar(sc) && sc.funcs.has(name)) {
				redeclared = true
				break
			}
			sc.vars.add(name)
			if sc.flags&scopeVarBoundary != 0 {
				break
			}
		}
	}
	if redeclared {
		p.early(CodeRedeclaration, span, "identifier '"+name+"' has already been declared")
	}
}

// functionBindKind is the binding kind of a function declaration
// (B.3.2: sloppy plain functions in blocks may be redeclared).
func (p *parser) functionBindKind(async, generator bool) bindKind {
	if p.strict() || async || generator {
		if p.functionsAsVar(p.curScope()) {
			return bindVar
		}
		return bindLexical
	}
	return bindFunction
}

// ---- labels ----

func (p *parser) findLabel(name string) *label {
	c := p.cur()
	for i := len(c.labels) - 1; i >= 0; i-- {
		if c.labels[i].name == name {
			return &c.labels[i]
		}
	}
	return nil
}

// markLoopLabels turns the labels directly attached to the statement
// being parsed into iteration labels (continue targets).
func (p *parser) markLoopLabels() {
	c := p.cur()
	for i := len(c.labels) - 1; i >= 0 && c.labels[i].direct; i-- {
		c.labels[i].isLoop = true
	}
}

// clearDirectLabels ends the "direct" status of pending labels once the
// labelled statement's own parsing has started.
func (p *parser) clearDirectLabels() {
	c := p.cur()
	for i := len(c.labels) - 1; i >= 0 && c.labels[i].direct; i-- {
		c.labels[i].direct = false
	}
}

// ---- private names ----

type privateKind uint8

const (
	privField privateKind = iota
	privMethod
	privGetter
	privSetter
	privAccessorPair
)

type privateDecl struct {
	name     string
	kind     privateKind
	isStatic bool
}

type privateUse struct {
	name string
	span Span
}

type classCtx struct {
	declared []privateDecl
	used     []privateUse
}

// declarePrivate records a private name declared in the innermost class
// (15.7.1): a getter and setter of the same staticness form one pair;
// any other repeat is an error, as is #constructor.
func (p *parser) declarePrivate(name string, kind privateKind, isStatic bool, span Span) {
	if name == "constructor" {
		p.early(CodePrivateName, span, "#constructor is not allowed")
		return
	}
	c := &p.classes[len(p.classes)-1]
	for i := range c.declared {
		d := &c.declared[i]
		if d.name != name {
			continue
		}
		if d.isStatic == isStatic && ((d.kind == privGetter && kind == privSetter) || (d.kind == privSetter && kind == privGetter)) {
			d.kind = privAccessorPair
			return
		}
		p.early(CodePrivateName, span, "duplicate private name #"+name)
		return
	}
	c.declared = append(c.declared, privateDecl{name: name, kind: kind, isStatic: isStatic})
}

// usePrivate records a private name reference; it is resolved when the
// class body ends (exitClass), because a name may be declared after its
// first use.
func (p *parser) usePrivate(name string, span Span) {
	if len(p.classes) == 0 {
		p.early(CodePrivateName, span, "private name #"+name+" outside a class")
		return
	}
	c := &p.classes[len(p.classes)-1]
	c.used = append(c.used, privateUse{name: name, span: span})
}

// exitClass resolves the private names used in the class body; names not
// declared here are resolved by an enclosing class or reported.
func (p *parser) exitClass() {
	c := p.classes[len(p.classes)-1]
	p.classes = p.classes[:len(p.classes)-1]
	for _, u := range c.used {
		found := false
		for _, d := range c.declared {
			if d.name == u.name {
				found = true
				break
			}
		}
		if found {
			continue
		}
		if len(p.classes) > 0 {
			outer := &p.classes[len(p.classes)-1]
			outer.used = append(outer.used, u)
		} else {
			p.early(CodePrivateName, u.span, "undeclared private name #"+u.name)
		}
	}
}

// ---- exports ----

type exportState struct {
	names   nameSet
	pending []privateUse // local names of `export { x }` to resolve at the end
}

// addExportName records an ExportedName; a module may export each name
// once (16.2.1.1).
func (p *parser) addExportName(name string, span Span) {
	if p.exports.names.has(name) {
		p.early(CodeExport, span, "duplicate export '"+name+"'")
		return
	}
	p.exports.names.add(name)
}

// checkExports resolves `export { x }` local names against the module's
// top-level declarations once the whole module is parsed (they may be
// declared after the export).
func (p *parser) checkExports() {
	top := &p.scopes[0]
	for _, u := range p.exports.pending {
		if !top.vars.has(u.name) && !top.lex.has(u.name) && !top.funcs.has(u.name) {
			p.early(CodeExport, u.span, "export '"+u.name+"' is not defined")
		}
	}
}

// ---- cover grammar ----

// coverErr is an error that is cancelled if the expression containing it
// is reinterpreted as a pattern: CoverInitializedName and duplicate
// __proto__ (both 13.2.5.1; the __proto__ rule moved out of Annex B in
// ES2022 and, like the other, does not apply to an assignment pattern).
type coverErr struct {
	code Code
	span Span
}

// flushCover reports pending cover errors that start at or after from:
// the expression they belong to will not become a pattern.
func (p *parser) flushCover(from int32) {
	i := len(p.cover)
	for i > 0 && p.cover[i-1].span.Start >= from {
		i--
	}
	for _, e := range p.cover[i:] {
		if e.code == CodeInvalidCover {
			p.syntaxErrorAt(e.code, e.span, "shorthand property initializer is only valid in a destructuring pattern")
		} else {
			p.early(e.code, e.span, "duplicate __proto__ property")
		}
	}
	p.cover = p.cover[:i]
}

// dropCover cancels pending cover errors inside span (the expression was
// converted to a pattern).
func (p *parser) dropCover(span Span) {
	i := len(p.cover)
	for i > 0 && p.cover[i-1].span.Start >= span.Start {
		i--
	}
	p.cover = p.cover[:i]
}

// ---- identifier checks ----

// checkIdentifierReference applies the early errors of 13.1.1 to an
// identifier used as an IdentifierReference.
func (p *parser) checkIdentifierReference(t *Token) {
	p.checkReserved(t, false)
}

// checkLabelIdentifier applies them to a LabelIdentifier (which is not an
// IdentifierReference: `arguments:` is fine in a field initializer).
func (p *parser) checkLabelIdentifier(t *Token) {
	p.checkReserved(t, true)
}

// checkBindingIdentifier applies the BindingIdentifier early errors.
func (p *parser) checkBindingIdentifier(name string, span Span, lexical bool) {
	if p.strict() && (name == "eval" || name == "arguments") {
		p.early(CodeStrictEvalArguments, span, "cannot bind '"+name+"' in strict mode")
	}
	if lexical && name == "let" {
		p.early(CodeLetName, span, "let cannot be a lexically bound name")
	}
}

// checkReserved reports an identifier token that is a reserved word in
// the current context.
func (p *parser) checkReserved(t *Token, binding bool) {
	kw := t.Kw
	span := Span{t.Start, t.End}
	if kw == KwNone {
		return
	}
	c := p.cur()
	switch {
	case kw == KwYield:
		if c.generator || p.strict() {
			p.early(CodeInvalidUseOfKeyword, span, "yield is reserved here")
		}
		if c.inParams && c.generator {
			p.early(CodeYieldAwaitInParams, span, "yield in generator parameters")
		}
	case kw == KwAwait:
		if c.async || p.module || p.inStaticBlock() || c.awaitIdent {
			p.early(CodeInvalidUseOfKeyword, span, "await is reserved here")
		}
		p.lastAwaitIdent = t.Start
	case kw.isReserved():
		if t.Flags&FlagEscaped != 0 {
			p.early(CodeEscapedKeyword, span, "keyword must not contain escapes")
		} else {
			p.early(CodeReservedWord, span, "unexpected reserved word")
		}
	case kw.isStrictReserved():
		if p.strict() {
			p.early(CodeReservedWord, span, "reserved word in strict mode")
		}
	case kw == KwArguments:
		if !binding && p.argumentsForbidden() {
			p.early(CodeClassField, span, "arguments is not allowed in class field initializer or static block")
		}
	}
}

// isIdentifierToken reports whether t can be an Identifier (an
// IdentifierName that is not a ReservedWord, with yield and await
// decided by context in checkReserved).
func isIdentifierToken(t *Token) bool {
	if t.Kind != TIdentifier {
		return false
	}
	return !t.Kw.isReserved() || t.Kw == KwYield || t.Kw == KwAwait
}
