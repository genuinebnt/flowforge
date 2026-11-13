package jsparse

// Modules (ECMA-262 16.2): import and export declarations with import
// attributes (`with { type: "json" }`) and string export names.

// parseModuleExportName parses ModuleExportName: IdentifierName or
// StringLiteral (which must be well-formed Unicode).
func (p *parser) parseModuleExportName() (NodeID, string, bool) {
	switch p.tok.Kind {
	case TString:
		t := p.tok
		if !isWellFormed(t.Value) {
			p.early(CodeExport, t.Span(), "export name must be well-formed Unicode")
		}
		id := p.parseLiteral()
		return id, t.Value, true
	case TIdentifier:
		t := p.tok
		id := p.ident(t.Value, t.Span())
		p.next()
		return id, t.Value, false
	}
	p.unexpected()
	return p.errNode(), "", false
}

// isWellFormed reports whether s holds no lone surrogates (WTF-8
// sequences ED A0..BF xx).
func isWellFormed(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF {
			return false
		}
	}
	return true
}

func (p *parser) expectContextual(kw Keyword) {
	if p.isKw(kw) {
		p.next()
		return
	}
	p.unexpected()
}

// parseModuleSpecifier parses FromClause's StringLiteral.
func (p *parser) parseModuleSpecifier() NodeID {
	if !p.at(TString) {
		p.unexpected()
		return p.errNode()
	}
	return p.parseLiteral()
}

// parseWithClause parses an optional WithClause (import attributes).
func (p *parser) parseWithClause() NodeList {
	if !p.isKw(KwWith) {
		return NodeList{Start: int32(len(p.children))}
	}
	p.next()
	p.expect(TLBrace)
	mark := len(p.scratch)
	var keys []string
	for !p.at(TRBrace) && !p.at(TEOF) {
		start := p.start()
		var key NodeID
		var name string
		switch p.tok.Kind {
		case TString:
			name = p.tok.Value
			key = p.parseLiteral()
		case TIdentifier:
			name = p.tok.Value
			key = p.ident(name, p.tok.Span())
			p.next()
		default:
			p.unexpected()
			key = p.errNode()
		}
		for _, k := range keys {
			if k == name {
				p.early(CodeExport, p.nd(key).Span, "duplicate import attribute '"+name+"'")
			}
		}
		keys = append(keys, name)
		p.expect(TColon)
		if !p.at(TString) {
			p.unexpected()
			break
		}
		val := p.parseLiteral()
		id := p.newNode(KImportAttribute, start)
		n := p.nd(id)
		n.A, n.B = key, val
		p.push(p.done(id))
		if !p.at(TRBrace) {
			p.expect(TComma)
		}
	}
	p.expect(TRBrace)
	return p.list(mark)
}

// parseImportBinding parses ImportedBinding (a lexical binding).
func (p *parser) parseImportBinding() NodeID {
	return p.parseBindingIdentifier(bindLexical)
}

// parseImportDeclaration parses ImportDeclaration at `import`.
func (p *parser) parseImportDeclaration() NodeID {
	start := p.start()
	p.next()
	mark := len(p.scratch)
	src := NoNode
	if p.at(TString) {
		src = p.parseModuleSpecifier()
	} else {
		if p.at(TIdentifier) {
			s := p.start()
			local := p.parseImportBinding()
			id := p.newNode(KImportDefaultSpecifier, s)
			p.nd(id).A = local
			p.push(p.done(id))
			if p.eat(TComma) {
				if !p.at(TStar) && !p.at(TLBrace) {
					p.unexpected()
				}
			}
		}
		switch {
		case p.at(TStar):
			s := p.start()
			p.next()
			p.expectContextual(KwAs)
			local := p.parseImportBinding()
			id := p.newNode(KImportNamespaceSpecifier, s)
			p.nd(id).A = local
			p.push(p.done(id))
		case p.at(TLBrace):
			p.next()
			for !p.at(TRBrace) && !p.at(TEOF) {
				s := p.start()
				importedTok := p.tok
				imported, _, isString := p.parseModuleExportName()
				var local NodeID
				if p.isKw(KwAs) {
					p.next()
					local = p.parseImportBinding()
				} else {
					if isString || !isIdentifierToken(&importedTok) {
						p.syntaxErrorAt(CodeUnexpectedToken, p.nd(imported).Span, "expected 'as' after import name")
					}
					p.checkReserved(&importedTok, true)
					local = p.ident(importedTok.Value, importedTok.Span())
					p.bindIdentNode(local, bindLexical)
				}
				id := p.newNode(KImportSpecifier, s)
				n := p.nd(id)
				n.A, n.B = imported, local
				p.push(p.done(id))
				if !p.at(TRBrace) {
					p.expect(TComma)
				}
			}
			p.expect(TRBrace)
		}
		p.expectContextual(KwFrom)
		src = p.parseModuleSpecifier()
	}
	attrs := p.parseWithClause()
	p.semicolon()
	id := p.newNode(KImportDeclaration, start)
	n := p.nd(id)
	n.List = p.list(mark)
	n.A = src
	n.List2 = attrs
	return p.done(id)
}

// parseExportDeclaration parses ExportDeclaration at `export`.
func (p *parser) parseExportDeclaration() NodeID {
	start := p.start()
	p.next()
	switch {
	case p.at(TStar):
		p.next()
		exported := NoNode
		if p.isKw(KwAs) {
			p.next()
			var name string
			exported, name, _ = p.parseModuleExportName()
			p.addExportName(name, p.nd(exported).Span)
		}
		p.expectContextual(KwFrom)
		src := p.parseModuleSpecifier()
		attrs := p.parseWithClause()
		p.semicolon()
		id := p.newNode(KExportAllDeclaration, start)
		n := p.nd(id)
		n.A, n.B = exported, src
		n.List2 = attrs
		return p.done(id)

	case p.isKw(KwDefault):
		p.addExportName("default", p.tok.Span())
		p.next()
		dstart := p.start()
		var decl NodeID
		switch {
		case p.isKw(KwFunction):
			decl = p.parseFunction(dstart, false, true, true)
		case p.isKw(KwAsync) && isKwTok(p.peek(), KwFunction) && p.peek().Flags&FlagNewlineBefore == 0:
			p.next()
			decl = p.parseFunction(dstart, true, true, true)
		case p.isKw(KwClass):
			decl = p.parseClass(dstart, true, true)
		default:
			decl = p.parseAssignment(false, false)
			p.semicolon()
		}
		id := p.newNode(KExportDefaultDeclaration, start)
		p.nd(id).A = decl
		return p.done(id)

	case p.at(TLBrace):
		p.next()
		mark := len(p.scratch)
		type localRef struct {
			tok      Token
			isString bool
		}
		var locals []localRef
		for !p.at(TRBrace) && !p.at(TEOF) {
			s := p.start()
			lt := p.tok
			local, _, isString := p.parseModuleExportName()
			var exported NodeID
			var ename string
			if p.isKw(KwAs) {
				p.next()
				exported, ename, _ = p.parseModuleExportName()
			} else {
				ename = lt.Value
				if isString {
					exported = p.newNode(KLiteral, lt.Start)
					en := p.nd(exported)
					en.Op = LitString
					en.Str = lt.Value
					en.Span.End = lt.End
				} else {
					exported = p.ident(lt.Value, lt.Span())
				}
			}
			p.addExportName(ename, p.nd(exported).Span)
			locals = append(locals, localRef{tok: lt, isString: isString})
			id := p.newNode(KExportSpecifier, s)
			n := p.nd(id)
			n.A, n.B = local, exported
			p.push(p.done(id))
			if !p.at(TRBrace) {
				p.expect(TComma)
			}
		}
		p.expect(TRBrace)
		src := NoNode
		attrs := NodeList{Start: int32(len(p.children))}
		if p.isKw(KwFrom) {
			p.next()
			src = p.parseModuleSpecifier()
			attrs = p.parseWithClause()
		} else {
			for _, l := range locals {
				t := l.tok
				if l.isString || !isIdentifierToken(&t) {
					p.early(CodeExport, t.Span(), "exported binding must be an identifier")
					continue
				}
				p.checkReserved(&t, false)
				p.exports.pending = append(p.exports.pending, privateUse{name: t.Value, span: t.Span()})
			}
		}
		p.semicolon()
		id := p.newNode(KExportNamedDeclaration, start)
		n := p.nd(id)
		n.List = p.list(mark)
		n.B = src
		n.List2 = attrs
		return p.done(id)
	}
	// export Declaration / VariableStatement
	dstart := p.start()
	var decl NodeID
	switch {
	case p.isKw(KwVar):
		decl = p.parseVarDecl(OpVar, false, false)
		p.semicolon()
		p.done(decl)
	case p.isKw(KwLet):
		decl = p.parseVarDecl(OpLet, false, false)
		p.semicolon()
		p.done(decl)
	case p.isKw(KwConst):
		decl = p.parseVarDecl(OpConst, false, false)
		p.semicolon()
		p.done(decl)
	case p.isKw(KwFunction):
		decl = p.parseFunction(dstart, false, true, false)
	case p.isKw(KwAsync) && isKwTok(p.peek(), KwFunction) && p.peek().Flags&FlagNewlineBefore == 0:
		p.next()
		decl = p.parseFunction(dstart, true, true, false)
	case p.isKw(KwClass):
		decl = p.parseClass(dstart, true, false)
	default:
		p.unexpected()
		return p.errNode()
	}
	for _, b := range p.boundNamesOf(nil, decl) {
		p.addExportName(b.name, b.span)
	}
	id := p.newNode(KExportNamedDeclaration, start)
	n := p.nd(id)
	n.A = decl
	n.List = NodeList{Start: int32(len(p.children))}
	n.List2 = n.List
	return p.done(id)
}
