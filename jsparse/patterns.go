package jsparse

// Destructuring (13.15.5 assignment patterns, 14.3.3 binding patterns).
// Binding patterns in declarations and parameters are parsed directly;
// assignment patterns and arrow parameters are reinterpreted from the
// expression they were parsed as (cover grammars), in place: the arena
// node changes kind (ObjectExpression → ObjectPattern, ...).

type patMode uint8

const (
	patAssign  patMode = iota // AssignmentPattern / DestructuringAssignmentTarget
	patBinding                // BindingPattern (arrow parameters)
)

// boundName is a name bound by a parameter list, kept for the
// retroactive strict-mode and duplicate checks.
type boundName struct {
	name string
	kw   Keyword
	span Span
}

// toAssignTarget validates and converts the left operand of `=` or the
// head of a for-in/of statement.
func (p *parser) toAssignTarget(id NodeID) {
	n := p.nd(id)
	switch n.Kind {
	case KObjectExpression, KArrayExpression:
		if n.Flags&NParenthesized != 0 {
			p.early(CodeInvalidAssignTarget, n.Span, "invalid destructuring assignment target")
			return
		}
		p.toPattern(id, patAssign, bindNone)
		p.dropCover(n.Span)
	default:
		p.checkSimpleTarget(id)
	}
}

// checkSimpleTarget requires AssignmentTargetType simple: an identifier
// (not eval/arguments in strict code) or a member expression.
func (p *parser) checkSimpleTarget(id NodeID) {
	n := p.nd(id)
	switch n.Kind {
	case KIdentifier:
		if p.strs[n.Str] == "" {
			return // placeholder after an error
		}
		if p.strict() && (p.strs[n.Str] == "eval" || p.strs[n.Str] == "arguments") {
			p.early(CodeStrictEvalArguments, n.Span, "cannot assign to "+p.strs[n.Str]+" in strict mode")
		}
	case KMemberExpression:
	default:
		p.early(CodeInvalidAssignTarget, n.Span, "invalid assignment target")
	}
}

// toPattern reinterprets an expression as a pattern. In binding mode the
// bound identifiers are declared with kind.
func (p *parser) toPattern(id NodeID, mode patMode, kind bindKind) {
	if id == NoNode {
		return
	}
	n := p.nd(id)
	paren := n.Flags&NParenthesized != 0
	bad := func(msg string) {
		code := CodeInvalidAssignTarget
		if mode == patBinding {
			code = CodeInvalidPattern
		}
		p.early(code, n.Span, msg)
	}
	switch n.Kind {
	case KIdentifier:
		if mode == patBinding {
			if paren {
				bad("parenthesized binding")
				return
			}
			p.bindIdentNode(id, kind)
			return
		}
		p.checkSimpleTarget(id)
	case KMemberExpression:
		if mode == patBinding {
			bad("invalid binding pattern")
		}
	case KObjectExpression, KObjectPattern:
		if paren {
			bad("parenthesized pattern")
			return
		}
		n.Kind = KObjectPattern
		props := p.ids(n.List)
		for i, pr := range props {
			pn := p.nd(pr)
			switch pn.Kind {
			case KProperty:
				if pn.Flags&NMethod != 0 || pn.Op != OpInit {
					p.early(CodeInvalidPattern, pn.Span, "invalid destructuring target")
					continue
				}
				p.toPattern(pn.B, mode, kind)
			case KSpreadElement, KRestElement:
				if i != len(props)-1 || pn.Flags&NTrailingComma != 0 {
					p.early(CodeInvalidPattern, pn.Span, "rest element must be last")
				}
				pn.Kind = KRestElement
				arg := p.nd(pn.A)
				ok := arg.Kind == KIdentifier && (mode == patAssign || arg.Flags&NParenthesized == 0)
				if mode == patAssign && arg.Kind == KMemberExpression {
					ok = true
				}
				if !ok {
					p.early(CodeInvalidPattern, arg.Span, "invalid rest element")
					continue
				}
				p.toPattern(pn.A, mode, kind)
			default:
				p.early(CodeInvalidPattern, pn.Span, "invalid destructuring target")
			}
		}
	case KArrayExpression, KArrayPattern:
		if paren {
			bad("parenthesized pattern")
			return
		}
		n.Kind = KArrayPattern
		elems := p.ids(n.List)
		for i, el := range elems {
			if el == NoNode {
				continue
			}
			en := p.nd(el)
			if en.Kind == KSpreadElement || en.Kind == KRestElement {
				if i != len(elems)-1 || en.Flags&NTrailingComma != 0 {
					p.early(CodeInvalidPattern, en.Span, "rest element must be last")
				}
				en.Kind = KRestElement
				p.toRestArgument(en.A, mode, kind)
				continue
			}
			p.toPattern(el, mode, kind)
		}
	case KAssignmentExpression:
		if n.Op != OpAssign || paren {
			bad("invalid destructuring target")
			return
		}
		n.Kind = KAssignmentPattern
		n.Op = OpNone
		p.toPattern(n.A, mode, kind)
	case KAssignmentPattern:
		if paren {
			bad("invalid destructuring target")
			return
		}
		p.toPattern(n.A, mode, kind)
	case KRestElement:
		p.toRestArgument(n.A, mode, kind)
	default:
		bad("invalid destructuring target")
	}
}

// toRestArgument converts the argument of an array or parameter rest
// element: any target, but no initializer.
func (p *parser) toRestArgument(id NodeID, mode patMode, kind bindKind) {
	a := p.nd(id)
	if a.Kind == KAssignmentExpression || a.Kind == KAssignmentPattern {
		p.early(CodeInvalidPattern, a.Span, "rest element cannot have an initializer")
		return
	}
	p.toPattern(id, mode, kind)
}

// bindIdentNode declares the identifier node id as a binding.
func (p *parser) bindIdentNode(id NodeID, kind bindKind) {
	n := p.nd(id)
	if p.strs[n.Str] == "" {
		return
	}
	p.checkBindingIdentifier(p.strs[n.Str], n.Span, kind == bindLexical)
	if p.bound != nil {
		*p.bound = append(*p.bound, boundName{name: p.strs[n.Str], kw: lookupKeyword(p.strs[n.Str]), span: n.Span})
	}
	p.declare(p.strs[n.Str], kind, n.Span)
}

// parseBindingTarget parses BindingIdentifier or BindingPattern.
func (p *parser) parseBindingTarget(kind bindKind) NodeID {
	switch p.tok.Kind {
	case TLBracket:
		return p.parseArrayBindingPattern(kind)
	case TLBrace:
		return p.parseObjectBindingPattern(kind)
	}
	return p.parseBindingIdentifier(kind)
}

// parseBindingIdentifier parses BindingIdentifier and declares it.
func (p *parser) parseBindingIdentifier(kind bindKind) NodeID {
	if !isIdentifierToken(&p.tok) {
		if p.tok.Kind == TIdentifier && p.tok.Flags&FlagEscaped != 0 {
			p.syntaxError(CodeEscapedKeyword, "keyword must not contain escapes")
		} else {
			p.unexpected()
		}
		return p.errNode()
	}
	tok := p.tok
	p.checkReserved(&tok, true)
	id := p.ident(tok.Value, tok.Span())
	p.next()
	p.bindIdentNode(id, kind)
	return id
}

// parseBindingElement parses BindingElement: a target with an optional
// Initializer.
func (p *parser) parseBindingElement(kind bindKind) NodeID {
	start := p.start()
	target := p.parseBindingTarget(kind)
	if !p.at(TEq) {
		return target
	}
	p.next()
	init := p.parseAssignment(false, false)
	id := p.newNode(KAssignmentPattern, start)
	n := p.nd(id)
	n.A, n.B = target, init
	return p.done(id)
}

// parseBindingRest parses `...` BindingIdentifier / BindingPattern.
func (p *parser) parseBindingRest(kind bindKind, objectRest bool) NodeID {
	start := p.start()
	p.next()
	var arg NodeID
	if objectRest {
		arg = p.parseBindingIdentifier(kind)
	} else {
		arg = p.parseBindingTarget(kind)
	}
	if p.at(TEq) {
		p.syntaxError(CodeInvalidPattern, "rest element cannot have an initializer")
	}
	id := p.newNode(KRestElement, start)
	p.nd(id).A = arg
	return p.done(id)
}

func (p *parser) parseArrayBindingPattern(kind bindKind) NodeID {
	start := p.start()
	p.next()
	mark := len(p.scratch)
	for !p.at(TRBracket) && !p.at(TEOF) {
		if p.at(TComma) {
			p.next()
			p.push(NoNode)
			continue
		}
		if p.at(TEllipsis) {
			p.push(p.parseBindingRest(kind, false))
			if !p.at(TRBracket) {
				p.syntaxError(CodeInvalidPattern, "rest element must be last")
			}
			break
		}
		p.push(p.parseBindingElement(kind))
		if !p.at(TRBracket) {
			p.expect(TComma)
		}
	}
	p.expect(TRBracket)
	id := p.newNode(KArrayPattern, start)
	p.nd(id).List = p.list(mark)
	return p.done(id)
}

func (p *parser) parseObjectBindingPattern(kind bindKind) NodeID {
	start := p.start()
	p.next()
	mark := len(p.scratch)
	for !p.at(TRBrace) && !p.at(TEOF) {
		if p.at(TEllipsis) {
			p.push(p.parseBindingRest(kind, true))
			if !p.at(TRBrace) {
				p.syntaxError(CodeInvalidPattern, "rest element must be last")
			}
			break
		}
		pstart := p.start()
		key := p.parsePropertyKey(false)
		prop := p.newNode(KProperty, pstart)
		p.nd(prop).Op = OpInit
		p.nd(prop).A = key.id
		if key.computed {
			p.nd(prop).Flags |= NComputed
		}
		if p.at(TColon) {
			p.next()
			v := p.parseBindingElement(kind)
			p.nd(prop).B = v
		} else {
			if !key.isIdent || !isIdentifierToken(&key.identTok) {
				if key.isIdent && key.identTok.Flags&FlagEscaped != 0 {
					p.syntaxErrorAt(CodeEscapedKeyword, p.nd(key.id).Span, "keyword must not contain escapes")
				} else {
					p.syntaxErrorAt(CodeUnexpectedToken, p.nd(key.id).Span, "expected binding identifier")
				}
				p.push(p.done(prop))
				break
			}
			tok := key.identTok
			p.checkReserved(&tok, true)
			v := p.ident(tok.Value, tok.Span())
			p.bindIdentNode(v, kind)
			p.nd(prop).Flags |= NShorthand
			if p.at(TEq) {
				p.next()
				init := p.parseAssignment(false, false)
				ap := p.newNode(KAssignmentPattern, tok.Start)
				p.nd(ap).A, p.nd(ap).B = v, init
				v = p.done(ap)
			}
			p.nd(prop).B = v
		}
		p.push(p.done(prop))
		if !p.at(TRBrace) {
			p.expect(TComma)
		}
	}
	p.expect(TRBrace)
	id := p.newNode(KObjectPattern, start)
	p.nd(id).List = p.list(mark)
	return p.done(id)
}

// isSimpleParams reports whether every parameter is a plain identifier
// (IsSimpleParameterList).
func (p *parser) isSimpleParams(params NodeList) bool {
	for _, id := range p.ids(params) {
		if p.nd(id).Kind != KIdentifier {
			return false
		}
	}
	return true
}

// boundNamesOf appends the names bound by a pattern (BoundNames).
func (p *parser) boundNamesOf(dst []boundName, id NodeID) []boundName {
	if id == NoNode {
		return dst
	}
	n := p.nd(id)
	switch n.Kind {
	case KIdentifier:
		if p.strs[n.Str] != "" {
			dst = append(dst, boundName{name: p.strs[n.Str], kw: lookupKeyword(p.strs[n.Str]), span: n.Span})
		}
	case KObjectPattern:
		for _, c := range p.ids(n.List) {
			cn := p.nd(c)
			if cn.Kind == KRestElement {
				dst = p.boundNamesOf(dst, cn.A)
			} else {
				dst = p.boundNamesOf(dst, cn.B)
			}
		}
	case KArrayPattern:
		for _, c := range p.ids(n.List) {
			dst = p.boundNamesOf(dst, c)
		}
	case KAssignmentPattern:
		dst = p.boundNamesOf(dst, n.A)
	case KRestElement:
		dst = p.boundNamesOf(dst, n.A)
	case KVariableDeclaration:
		for _, d := range p.ids(n.List) {
			dst = p.boundNamesOf(dst, p.nd(d).A)
		}
	case KFunctionDeclaration, KClassDeclaration:
		dst = p.boundNamesOf(dst, n.A)
	}
	return dst
}
