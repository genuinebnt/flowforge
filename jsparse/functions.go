package jsparse

// Functions, generators, async functions, methods and arrow functions
// (ECMA-262 15.1-15.6, 15.8, 15.9).

// fnInfo carries what the body parser needs for the retroactive checks
// that depend on a "use strict" directive in the body.
type fnInfo struct {
	names    []boundName
	simple   bool
	unique   bool   // UniqueFormalParameters (methods, arrows): no duplicates ever
	nameTok  *Token // function name, if any
	accessor Op     // OpGet / OpSet for parameter-count checks
}

// parseFunction parses a function declaration or expression starting at
// `function` (after `async` when isAsync).
func (p *parser) parseFunction(start int32, isAsync, decl, defaultExport bool) NodeID {
	if !p.enter() {
		p.leave()
		return p.errNode()
	}
	defer p.leave()
	p.next() // function
	isGen := p.eat(TStar)
	kind := KFunctionExpression
	if decl {
		kind = KFunctionDeclaration
	}
	id := p.newNode(kind, start)
	var nameTok *Token
	if p.at(TIdentifier) {
		t := p.tok
		nameTok = &t
		if !isIdentifierToken(&t) {
			if t.Flags&FlagEscaped != 0 {
				p.syntaxError(CodeEscapedKeyword, "keyword must not contain escapes")
			} else {
				p.unexpected()
			}
			return p.errNode()
		}
		if decl {
			p.checkReserved(&t, true)
		} else {
			p.checkFunctionExprName(&t, isAsync, isGen)
		}
		p.checkBindingIdentifier(t.Value, t.Span(), false)
		if decl {
			p.declare(t.Value, p.functionBindKind(isAsync, isGen), t.Span())
		}
		name := p.ident(t.Value, t.Span())
		p.nd(id).A = name
		p.next()
	} else if decl && !defaultExport {
		p.unexpected()
	}
	if isAsync {
		p.nd(id).Flags |= NAsync
	}
	if isGen {
		p.nd(id).Flags |= NGenerator
	}
	p.parseFunctionRest(id, isAsync, isGen, OpNone, false, nameTok)
	return p.done(id)
}

// checkFunctionExprName applies the reserved-word rules to the name of a
// function expression, whose [Yield]/[Await] are those of the expression
// itself rather than of the enclosing context.
func (p *parser) checkFunctionExprName(t *Token, isAsync, isGen bool) {
	switch t.Kw {
	case KwYield:
		if isGen || p.strict() {
			p.early(CodeInvalidUseOfKeyword, t.Span(), "yield is reserved here")
		}
	case KwAwait:
		if isAsync || p.module {
			p.early(CodeInvalidUseOfKeyword, t.Span(), "await is reserved here")
		}
	default:
		p.checkReserved(t, true)
	}
}

// parseMethod parses the parameters and body of a method, getter, setter
// or constructor; the FunctionExpression starts at `(`.
func (p *parser) parseMethod(isAsync, isGen bool, kind Op, derived bool) NodeID {
	id := p.newNode(KFunctionExpression, p.start())
	if isAsync {
		p.nd(id).Flags |= NAsync
	}
	if isGen {
		p.nd(id).Flags |= NGenerator
	}
	if !p.enter() {
		p.leave()
		return p.done(id)
	}
	p.parseFunctionRest(id, isAsync, isGen, kind, derived, nil)
	p.leave()
	return p.done(id)
}

// parseFunctionRest parses FormalParameters and FunctionBody into id.
// kind is OpNone for plain functions, else OpMethod / OpGet / OpSet /
// OpConstructor.
func (p *parser) parseFunctionRest(id NodeID, isAsync, isGen bool, kind Op, derived bool, nameTok *Token) {
	outer := p.cur()
	ctx := fnCtx{
		async: isAsync, generator: isGen, method: kind != OpNone,
		superCall: kind == OpConstructor && derived, newTarget: true,
		strict: outer.strict, returnOK: true,
	}
	p.pushFn(ctx)
	p.pushScope(scopeFunction | scopeVarBoundary)
	var names []boundName
	saved := p.bound
	p.bound = &names
	p.cur().inParams = true
	params := p.parseFormalParams()
	p.cur().inParams = false
	p.bound = saved
	switch kind {
	case OpGet:
		if params.Len != 0 {
			p.early(CodeAccessorParams, p.nd(id).Span, "getter must not have parameters")
		}
	case OpSet:
		if params.Len != 1 || p.nd(p.ids(params)[0]).Kind == KRestElement {
			p.early(CodeAccessorParams, p.nd(id).Span, "setter must have exactly one parameter")
		}
	}
	info := fnInfo{names: names, simple: p.isSimpleParams(params), unique: kind != OpNone, nameTok: nameTok}
	body := p.parseFunctionBody(&info)
	strict := p.cur().strict
	p.popScope()
	p.popFn()
	n := p.nd(id)
	n.List = params
	n.B = body
	if strict {
		n.Flags |= NStrict
	}
}

// parseFormalParams parses ( FormalParameters ).
func (p *parser) parseFormalParams() NodeList {
	p.expect(TLParen)
	mark := len(p.scratch)
	for !p.at(TRParen) && !p.at(TEOF) {
		if p.at(TEllipsis) {
			p.push(p.parseBindingRest(bindVar, false))
			if !p.at(TRParen) {
				p.syntaxError(CodeInvalidPattern, "rest parameter must be last")
			}
			break
		}
		p.push(p.parseBindingElement(bindVar))
		if !p.at(TRParen) {
			p.expect(TComma)
		}
	}
	p.expect(TRParen)
	return p.list(mark)
}

// parseFunctionBody parses { FunctionBody } with its directive prologue
// and the checks that depend on it.
func (p *parser) parseFunctionBody(info *fnInfo) NodeID {
	start := p.start()
	p.expect(TLBrace)
	id := p.newNode(KBlockStatement, start)
	mark := len(p.scratch)
	wasStrict := p.strict()
	useStrict := p.parseDirectives(info)
	if useStrict && !info.simple {
		p.early(CodeNonSimpleUseStrict, Span{start, start + 1}, "\"use strict\" not allowed in function with non-simple parameters")
	}
	_ = wasStrict
	p.parseStatementList(TRBrace, false)
	p.expect(TRBrace)
	p.checkParamDuplicates(info)
	p.nd(id).List = p.list(mark)
	return p.done(id)
}

// strictParamChecks applies the strict-mode rules to parameters and the
// function name once a body turns out to be strict.
func (p *parser) strictParamChecks(info *fnInfo) {
	for _, b := range info.names {
		switch {
		case b.name == "eval" || b.name == "arguments":
			p.early(CodeStrictEvalArguments, b.span, "cannot bind '"+b.name+"' in strict mode")
		case b.kw.isStrictReserved() || b.kw == KwYield:
			p.early(CodeReservedWord, b.span, "reserved word in strict mode")
		}
	}
	if t := info.nameTok; t != nil {
		switch {
		case t.Value == "eval" || t.Value == "arguments":
			p.early(CodeStrictEvalArguments, t.Span(), "cannot bind '"+t.Value+"' in strict mode")
		case t.Kw.isStrictReserved() || t.Kw == KwYield:
			p.early(CodeReservedWord, t.Span(), "reserved word in strict mode")
		}
	}
}

// checkParamDuplicates reports duplicate parameter names unless the list
// is a sloppy-mode simple list of a plain function.
func (p *parser) checkParamDuplicates(info *fnInfo) {
	if !p.strict() && info.simple && !info.unique {
		return
	}
	names := info.names
	for i := 1; i < len(names); i++ {
		for j := 0; j < i; j++ {
			if names[i].name == names[j].name {
				p.early(CodeDuplicateParam, names[i].span, "duplicate parameter '"+names[i].name+"'")
				return
			}
		}
		if i > 64 {
			// Long lists: switch to a set to stay linear.
			seen := make(map[string]bool, len(names))
			for _, b := range names {
				if seen[b.name] {
					p.early(CodeDuplicateParam, b.span, "duplicate parameter '"+b.name+"'")
					return
				}
				seen[b.name] = true
			}
			return
		}
	}
}

// parseArrowFromParams parses `=> ConciseBody` for parameters already
// parsed as expressions (or identifiers) in params, converting them to
// binding patterns in the arrow's own scope.
func (p *parser) parseArrowFromParams(start int32, params NodeList, isAsync bool, paramsStart int32, _ bool) NodeID {
	return p.parseArrow(start, params, isAsync, paramsStart, false)
}

func (p *parser) parseArrow(start int32, params NodeList, isAsync bool, paramsStart int32, noIn bool) NodeID {
	if !p.enter() {
		p.leave()
		return p.errNode()
	}
	defer p.leave()
	outer := p.cur()
	p.pushFn(fnCtx{arrow: true, async: isAsync, strict: outer.strict, returnOK: true})
	p.pushScope(scopeFunction | scopeVarBoundary)
	var names []boundName
	saved := p.bound
	p.bound = &names
	for _, prm := range p.ids(params) {
		p.toPattern(prm, patBinding, bindVar)
	}
	p.bound = saved
	p.dropCover(Span{paramsStart, p.prevEnd})
	if isAsync {
		for _, b := range names {
			if b.name == "await" {
				p.early(CodeInvalidUseOfKeyword, b.span, "await is reserved in async arrow parameters")
			}
		}
	}
	p.expect(TArrow)
	id := p.newNode(KArrowFunctionExpression, start)
	info := fnInfo{names: names, simple: p.isSimpleParams(params), unique: true}
	var body NodeID
	expr := false
	if p.at(TLBrace) {
		body = p.parseFunctionBody(&info)
	} else {
		body = p.parseAssignment(noIn, false)
		expr = true
		p.checkParamDuplicates(&info)
	}
	strict := p.cur().strict
	p.popScope()
	p.popFn()
	n := p.nd(id)
	n.List = params
	n.B = body
	if isAsync {
		n.Flags |= NAsync
	}
	if expr {
		n.Flags |= NExpression
	}
	if strict {
		n.Flags |= NStrict
	}
	return p.done(id)
}
