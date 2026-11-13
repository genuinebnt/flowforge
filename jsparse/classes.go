package jsparse

// Classes (ECMA-262 15.7): heritage, methods, accessors, fields, private
// names and static blocks. All parts of a class are strict mode code.

// parseClass parses ClassDeclaration or ClassExpression at `class`.
func (p *parser) parseClass(start int32, decl, defaultExport bool) NodeID {
	if !p.enter() {
		p.leave()
		return p.errNode()
	}
	defer p.leave()
	p.next() // class
	c := p.cur()
	savedStrict := c.strict
	c.strict = true
	kind := KClassExpression
	if decl {
		kind = KClassDeclaration
	}
	id := p.newNode(kind, start)
	if p.at(TIdentifier) && !p.isKw(KwExtends) {
		t := p.tok
		if !isIdentifierToken(&t) {
			p.unexpected()
		} else {
			p.checkReserved(&t, true)
			p.checkBindingIdentifier(t.Value, t.Span(), true)
			if decl {
				p.declare(t.Value, bindLexical, t.Span())
			}
			name := p.ident(t.Value, t.Span())
			p.nd(id).A = name
			p.next()
		}
	} else if decl && !defaultExport {
		p.unexpected()
	}
	if p.isKw(KwExtends) {
		p.next()
		hstart := p.start()
		sup := p.parseLHS(false)
		if p.isBareArrow(sup) {
			p.syntaxErrorAt(CodeUnexpectedToken, p.nd(sup).Span, "arrow function is not allowed here")
		}
		p.flushCover(hstart)
		p.nd(id).B = sup
	}
	body := p.parseClassBody(p.nd(id).B != NoNode)
	p.nd(id).C = body
	p.cur().strict = savedStrict
	return p.done(id)
}

func (p *parser) parseClassBody(derived bool) NodeID {
	start := p.start()
	p.expect(TLBrace)
	p.classes = append(p.classes, classCtx{})
	mark := len(p.scratch)
	hasCtor := false
	for !p.at(TRBrace) && !p.at(TEOF) {
		if p.eat(TSemicolon) {
			continue
		}
		p.push(p.parseClassElement(derived, &hasCtor))
	}
	p.expect(TRBrace)
	p.exitClass()
	id := p.newNode(KClassBody, start)
	p.nd(id).List = p.list(mark)
	return p.done(id)
}

// isClassModifierEnd reports whether t after `static`, `async`, `get` or
// `set` makes that word the element name itself.
func isClassModifierEnd(t *Token) bool {
	switch t.Kind {
	case TLParen, TEq, TSemicolon, TRBrace, TEOF:
		return true
	}
	return false
}

// parseClassElement parses ClassElement (method, accessor, field or
// static block).
func (p *parser) parseClassElement(derived bool, hasCtor *bool) NodeID {
	start := p.start()
	isStatic := false
	if p.isKw(KwStatic) {
		pk := p.peek()
		if pk.Kind == TLBrace {
			p.next()
			return p.parseStaticBlock(start)
		}
		if !isClassModifierEnd(pk) {
			isStatic = true
			p.next()
		}
	}
	isAsync, isGen := false, false
	kind := OpMethod
	if p.isKw(KwAsync) {
		if pk := p.peek(); !isClassModifierEnd(pk) && pk.Flags&FlagNewlineBefore == 0 {
			isAsync = true
			p.next()
		}
	}
	if p.at(TStar) {
		isGen = true
		p.next()
	}
	if !isAsync && !isGen && (p.isKw(KwGet) || p.isKw(KwSet)) {
		if pk := p.peek(); !isClassModifierEnd(pk) && pk.Kind != TStar {
			kind = OpGet
			if p.isKw(KwSet) {
				kind = OpSet
			}
			p.next()
		}
	}
	keyStart := p.start()
	key := p.parsePropertyKey(true)
	plainName := !key.computed && !key.private && key.named
	if p.at(TLParen) {
		if plainName && key.name == "constructor" && !isStatic {
			if kind != OpMethod || isAsync || isGen {
				p.early(CodeClassConstructor, Span{keyStart, p.prevEnd}, "class constructor may not be an accessor, generator or async")
			} else {
				if *hasCtor {
					p.early(CodeClassConstructor, Span{keyStart, p.prevEnd}, "a class may only have one constructor")
				}
				*hasCtor = true
				kind = OpConstructor
			}
		}
		if plainName && isStatic && key.name == "prototype" {
			p.early(CodeStaticPrototype, Span{keyStart, p.prevEnd}, "classes may not have a static property named prototype")
		}
		if key.private {
			pk := privMethod
			switch kind {
			case OpGet:
				pk = privGetter
			case OpSet:
				pk = privSetter
			}
			p.declarePrivate(key.name, pk, isStatic, p.nd(key.id).Span)
		}
		fn := p.parseMethod(isAsync, isGen, kind, derived)
		id := p.newNode(KMethodDefinition, start)
		n := p.nd(id)
		n.A, n.B = key.id, fn
		n.Op = kind
		if key.computed {
			n.Flags |= NComputed
		}
		if isStatic {
			n.Flags |= NStatic
		}
		return p.done(id)
	}
	if isAsync || isGen || kind != OpMethod {
		p.unexpected()
		return p.errNode()
	}
	if plainName && (key.name == "constructor") {
		p.early(CodeClassField, p.nd(key.id).Span, "classes may not have a field named constructor")
	}
	if plainName && isStatic && key.name == "prototype" {
		p.early(CodeStaticPrototype, p.nd(key.id).Span, "classes may not have a static field named prototype")
	}
	if key.private {
		p.declarePrivate(key.name, privField, isStatic, p.nd(key.id).Span)
	}
	value := NoNode
	if p.eat(TEq) {
		outer := p.cur()
		p.pushFn(fnCtx{method: true, newTarget: true, fieldInit: true, strict: true,
			awaitIdent: outer.async || outer.awaitIdent})
		value = p.parseAssignment(false, false)
		p.popFn()
	}
	p.semicolon()
	id := p.newNode(KPropertyDefinition, start)
	n := p.nd(id)
	n.A, n.B = key.id, value
	if key.computed {
		n.Flags |= NComputed
	}
	if isStatic {
		n.Flags |= NStatic
	}
	return p.done(id)
}

// parseStaticBlock parses ClassStaticBlock at `{`.
func (p *parser) parseStaticBlock(start int32) NodeID {
	p.expect(TLBrace)
	p.pushFn(fnCtx{method: true, newTarget: true, staticBlock: true, strict: true})
	p.pushScope(scopeFunction | scopeVarBoundary)
	mark := len(p.scratch)
	p.parseStatementList(TRBrace, false)
	p.popScope()
	p.popFn()
	p.expect(TRBrace)
	id := p.newNode(KStaticBlock, start)
	p.nd(id).List = p.list(mark)
	return p.done(id)
}
