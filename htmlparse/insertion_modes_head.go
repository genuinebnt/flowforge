package htmlparse

import "slices"

// Insertion modes before the body: HTML §13.2.6.4.1-6 ("initial",
// "before html", "before head", "in head", "in head noscript", "after
// head") and §13.2.6.4.8 ("text").
//
// Every mode method returns true when the token must be reprocessed; the
// method has already switched b.mode. "Process the token using the rules
// for X" is a direct call to X's method, which returns X's reprocess
// result.

// isStart reports a start tag token whose name is one of the atoms tags.
func isStart(tok *Token, tags ...tagID) bool {
	return tok.Kind == StartTagToken && slices.Contains(tags, tok.atom)
}

// isEnd reports an end tag token whose name is one of the atoms tags.
func isEnd(tok *Token, tags ...tagID) bool {
	return tok.Kind == EndTagToken && slices.Contains(tags, tok.atom)
}

// isSpaceToken reports a character token of ASCII whitespace only.
func isSpaceToken(tok *Token) bool { return tok.Kind == CharacterToken && isAllSpace(tok.Data) }

// quirksPublicPrefixes: a DOCTYPE whose public identifier starts with one
// of these (ASCII case-insensitively) puts the document in quirks mode
// (the prefix list of §13.2.6.4.1, lower-cased).
var quirksPublicPrefixes = []string{
	"+//silmaril//dtd html pro v0r11 19970101//",
	"-//as//dtd html 3.0 aswedit + extensions//",
	"-//advasoft ltd//dtd html 3.0 aswedit + extensions//",
	"-//ietf//dtd html 2.0 level 1//",
	"-//ietf//dtd html 2.0 level 2//",
	"-//ietf//dtd html 2.0 strict level 1//",
	"-//ietf//dtd html 2.0 strict level 2//",
	"-//ietf//dtd html 2.0 strict//",
	"-//ietf//dtd html 2.0//",
	"-//ietf//dtd html 2.1e//",
	"-//ietf//dtd html 3.0//",
	"-//ietf//dtd html 3.2 final//",
	"-//ietf//dtd html 3.2//",
	"-//ietf//dtd html 3//",
	"-//ietf//dtd html level 0//",
	"-//ietf//dtd html level 1//",
	"-//ietf//dtd html level 2//",
	"-//ietf//dtd html level 3//",
	"-//ietf//dtd html strict level 0//",
	"-//ietf//dtd html strict level 1//",
	"-//ietf//dtd html strict level 2//",
	"-//ietf//dtd html strict level 3//",
	"-//ietf//dtd html strict//",
	"-//ietf//dtd html//",
	"-//metrius//dtd metrius presentational//",
	"-//microsoft//dtd internet explorer 2.0 html strict//",
	"-//microsoft//dtd internet explorer 2.0 html//",
	"-//microsoft//dtd internet explorer 2.0 tables//",
	"-//microsoft//dtd internet explorer 3.0 html strict//",
	"-//microsoft//dtd internet explorer 3.0 html//",
	"-//microsoft//dtd internet explorer 3.0 tables//",
	"-//netscape comm. corp.//dtd html//",
	"-//netscape comm. corp.//dtd strict html//",
	"-//o'reilly and associates//dtd html 2.0//",
	"-//o'reilly and associates//dtd html extended 1.0//",
	"-//o'reilly and associates//dtd html extended relaxed 1.0//",
	"-//sq//dtd html 2.0 hotmetal + extensions//",
	"-//softquad software//dtd hotmetal pro 6.0::19990601::extensions to html 4.0//",
	"-//softquad//dtd hotmetal pro 4.0::19971010::extensions to html 4.0//",
	"-//spyglass//dtd html 2.0 extended//",
	"-//sun microsystems corp.//dtd hotjava html//",
	"-//sun microsystems corp.//dtd hotjava strict html//",
	"-//w3c//dtd html 3 1995-03-24//",
	"-//w3c//dtd html 3.2 draft//",
	"-//w3c//dtd html 3.2 final//",
	"-//w3c//dtd html 3.2//",
	"-//w3c//dtd html 3.2s draft//",
	"-//w3c//dtd html 4.0 frameset//",
	"-//w3c//dtd html 4.0 transitional//",
	"-//w3c//dtd html experimental 19960712//",
	"-//w3c//dtd html experimental 970421//",
	"-//w3c//dtd w3 html//",
	"-//w3o//dtd w3 html 3.0//",
	"-//webtechs//dtd mozilla html 2.0//",
	"-//webtechs//dtd mozilla html//",
}

// doctypeMode determines the document mode from a DOCTYPE token
// ("initial" insertion mode, §13.2.6.4.1). This parser never parses an
// iframe srcdoc document, so the spec's srcdoc exemption does not apply.
// The HTML 4.01 Frameset/Transitional prefixes depend on whether the
// system identifier is missing (quirks) or present (limited quirks); an
// empty quoted system identifier ("") is present.
func doctypeMode(tok *Token) QuirksMode {
	pub, sys := tok.PublicID, tok.SystemID
	switch {
	case tok.ForceQuirks, tok.Name != "html",
		equalFoldASCII(pub, "-//W3O//DTD W3 HTML Strict 3.0//EN//"),
		equalFoldASCII(pub, "-/W3C/DTD HTML 4.0 Transitional/EN"),
		equalFoldASCII(pub, "HTML"),
		equalFoldASCII(sys, "http://www.ibm.com/data/dtd/v11/ibmxhtml1-transitional.dtd"):
		return Quirks
	}
	for _, p := range quirksPublicPrefixes {
		if hasPrefixFoldASCII(pub, p) {
			return Quirks
		}
	}
	sysMissing := !tok.HasSystemID
	html401 := hasPrefixFoldASCII(pub, "-//W3C//DTD HTML 4.01 Frameset//") ||
		hasPrefixFoldASCII(pub, "-//W3C//DTD HTML 4.01 Transitional//")
	if sysMissing && html401 {
		return Quirks
	}
	if hasPrefixFoldASCII(pub, "-//W3C//DTD XHTML 1.0 Frameset//") ||
		hasPrefixFoldASCII(pub, "-//W3C//DTD XHTML 1.0 Transitional//") ||
		(!sysMissing && html401) {
		return LimitedQuirks
	}
	return NoQuirks
}

// initialMode is the "initial" insertion mode (§13.2.6.4.1). A missing
// DOCTYPE puts the document in quirks mode.
func (b *treeBuilder) initialMode(tok *Token) bool {
	switch tok.Kind {
	case CharacterToken:
		if isAllSpace(tok.Data) {
			return false
		}
	case CommentToken:
		b.insertComment(tok, b.doc.Root)
		return false
	case ProcessingInstructionToken:
		b.insertPI(tok, b.doc.Root)
		return false
	case DoctypeToken:
		if tok.Name != "html" || tok.HasPublicID || (tok.HasSystemID && tok.SystemID != "about:legacy-compat") {
			b.parseError(ErrNonConformingDoctype)
		}
		id := b.doc.NewNode(DoctypeNode)
		n := &b.doc.Nodes[id]
		n.Name, n.PublicID, n.SystemID, n.Span = tok.Name, tok.PublicID, tok.SystemID, tok.Span
		b.doc.insertBefore(b.doc.Root, id, None)
		b.doc.Mode = doctypeMode(tok)
		b.mode = modeBeforeHTML
		return false
	}
	b.parseError(ErrMissingDoctype)
	b.doc.Mode = Quirks
	b.mode = modeBeforeHTML
	return true
}

// beforeHTMLMode is the "before html" insertion mode (§13.2.6.4.2).
func (b *treeBuilder) beforeHTMLMode(tok *Token) bool {
	switch tok.Kind {
	case DoctypeToken:
		b.parseError(ErrUnexpectedDoctype)
		return false
	case CommentToken:
		b.insertComment(tok, b.doc.Root)
		return false
	case ProcessingInstructionToken:
		b.insertPI(tok, b.doc.Root)
		return false
	case CharacterToken:
		if isAllSpace(tok.Data) {
			return false
		}
	case StartTagToken:
		if tok.atom == tagHtml {
			id := b.createElement("html", tagHtml, HTMLNamespace, tok.Attrs, tok.Span)
			b.doc.insertBefore(b.doc.Root, id, None)
			b.push(stackEntry{id: id, tag: tagHtml, ns: HTMLNamespace})
			b.mode = modeBeforeHead
			return false
		}
	case EndTagToken:
		if !isEnd(tok, tagHead, tagBody, tagHtml, tagBr) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
	}
	at := tok.Span.Start
	id := b.createElement("html", tagHtml, HTMLNamespace, nil, Span{at, at})
	b.doc.insertBefore(b.doc.Root, id, None)
	b.push(stackEntry{id: id, tag: tagHtml, ns: HTMLNamespace})
	b.mode = modeBeforeHead
	return true
}

// beforeHeadMode is the "before head" insertion mode (§13.2.6.4.3).
func (b *treeBuilder) beforeHeadMode(tok *Token) bool {
	switch tok.Kind {
	case CharacterToken:
		if isAllSpace(tok.Data) {
			return false
		}
	case CommentToken:
		b.insertComment(tok, None)
		return false
	case ProcessingInstructionToken:
		b.insertPI(tok, None)
		return false
	case DoctypeToken:
		b.parseError(ErrUnexpectedDoctype)
		return false
	case StartTagToken:
		switch tok.atom {
		case tagHtml:
			return b.inBodyMode(tok)
		case tagHead:
			b.head, _ = b.insertHTML(tok)
			b.mode = modeInHead
			return false
		}
	case EndTagToken:
		if !isEnd(tok, tagHead, tagBody, tagHtml, tagBr) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
	}
	b.head, _ = b.insertImplied(tagHead)
	b.mode = modeInHead
	return true
}

// inHeadMode is the "in head" insertion mode (§13.2.6.4.4).
func (b *treeBuilder) inHeadMode(tok *Token) bool {
	switch tok.Kind {
	case CharacterToken:
		if isAllSpace(tok.Data) {
			b.insertCharacters(tok.Data)
			return false
		}
	case CommentToken:
		b.insertComment(tok, None)
		return false
	case ProcessingInstructionToken:
		b.insertPI(tok, None)
		return false
	case DoctypeToken:
		b.parseError(ErrUnexpectedDoctype)
		return false
	case StartTagToken:
		switch tok.atom {
		case tagHtml:
			return b.inBodyMode(tok)
		case tagBase, tagBasefont, tagBgsound, tagLink:
			b.insertVoid(tok)
			return false
		case tagMeta:
			b.insertVoid(tok)
			// The "change the encoding" step; ParseRaw acts on it after
			// the parse.
			b.noteMetaCharset(tok)
			return false
		case tagTitle:
			b.startText(tok, RCDATAState)
			return false
		case tagNoscript:
			if b.scripting {
				b.startText(tok, RAWTEXTState)
				return false
			}
			b.insertHTML(tok)
			b.mode = modeInHeadNoscript
			return false
		case tagNoframes, tagStyle:
			b.startText(tok, RAWTEXTState)
			return false
		case tagScript:
			b.startText(tok, ScriptDataState)
			return false
		case tagTemplate:
			// The spec inserts the element first; the marker, flag and
			// mode stacks do not affect the insertion, so the order does
			// not matter. Declarative shadow roots are not supported, so
			// the shadowrootmode steps never apply (OPEN_QUESTIONS.md).
			b.insertMarker()
			b.framesetOK = false
			b.mode = modeInTemplate
			b.pushTemplateMode(modeInTemplate)
			b.insertHTML(tok)
			return false
		case tagHead:
			b.parseError(ErrUnexpectedStartTag)
			return false
		}
	case EndTagToken:
		switch tok.atom {
		case tagHead:
			b.pop()
			b.mode = modeAfterHead
			return false
		case tagBody, tagHtml, tagBr:
			// Anything else.
		case tagTemplate:
			b.endTemplate()
			return false
		default:
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
	}
	b.pop()
	b.mode = modeAfterHead
	return true
}

// endTemplate handles an end tag whose tag name is "template" in the "in
// head" insertion mode: close everything up to the template, drop the
// formatting elements opened inside it, pop its template insertion mode
// and reset the insertion mode.
func (b *treeBuilder) endTemplate() {
	if !b.templateOnStack() {
		b.parseError(ErrUnexpectedEndTag)
		return
	}
	b.generateAllImpliedEndTagsThoroughly()
	if !b.current().is(tagTemplate) {
		b.parseError(ErrUnclosedElement)
	}
	b.popUntil(tagTemplate)
	b.clearToLastMarker()
	b.popTemplateMode()
	b.resetInsertionMode()
}

// inHeadNoscriptMode is the "in head noscript" insertion mode
// (§13.2.6.4.5), used when scripting is disabled. Character tokens arrive
// split into whitespace / NUL / other segments (processCharacters), so
// leading whitespace stays in head and the rest closes the noscript.
func (b *treeBuilder) inHeadNoscriptMode(tok *Token) bool {
	switch tok.Kind {
	case DoctypeToken:
		b.parseError(ErrUnexpectedDoctype)
		return false
	case StartTagToken:
		switch tok.atom {
		case tagHtml:
			return b.inBodyMode(tok)
		case tagBasefont, tagBgsound, tagLink, tagMeta, tagNoframes, tagStyle:
			return b.inHeadMode(tok)
		case tagHead, tagNoscript:
			b.parseError(ErrUnexpectedStartTag)
			return false
		}
	case EndTagToken:
		switch tok.atom {
		case tagNoscript:
			b.pop()
			b.mode = modeInHead
			return false
		case tagBr:
		default:
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
	case CharacterToken:
		if isAllSpace(tok.Data) {
			return b.inHeadMode(tok)
		}
	case CommentToken, ProcessingInstructionToken:
		return b.inHeadMode(tok)
	}
	b.parseError(ErrUnexpectedStartTag)
	b.pop()
	b.mode = modeInHead
	return true
}

// afterHeadMode is the "after head" insertion mode (§13.2.6.4.6).
func (b *treeBuilder) afterHeadMode(tok *Token) bool {
	switch tok.Kind {
	case CharacterToken:
		if isAllSpace(tok.Data) {
			b.insertCharacters(tok.Data)
			return false
		}
	case CommentToken:
		b.insertComment(tok, None)
		return false
	case ProcessingInstructionToken:
		b.insertPI(tok, None)
		return false
	case DoctypeToken:
		b.parseError(ErrUnexpectedDoctype)
		return false
	case StartTagToken:
		switch tok.atom {
		case tagHtml:
			return b.inBodyMode(tok)
		case tagBody:
			b.insertHTML(tok)
			b.framesetOK = false
			b.mode = modeInBody
			return false
		case tagFrameset:
			b.insertHTML(tok)
			b.mode = modeInFrameset
			return false
		case tagBase, tagBasefont, tagBgsound, tagLink, tagMeta, tagNoframes, tagScript, tagStyle, tagTemplate, tagTitle:
			b.parseError(ErrUnexpectedStartTag)
			if b.head == None {
				return false
			}
			// Push head, process with "in head", remove head again (it
			// need not be the current node by then). The stack is just
			// [html] in this mode, so the push cannot exceed the depth
			// limit.
			b.push(stackEntry{id: b.head, tag: tagHead, ns: HTMLNamespace})
			again := b.inHeadMode(tok)
			b.removeFromStack(b.head)
			return again
		case tagHead:
			b.parseError(ErrUnexpectedStartTag)
			return false
		}
	case EndTagToken:
		switch tok.atom {
		case tagTemplate:
			return b.inHeadMode(tok)
		case tagBody, tagHtml, tagBr:
		default:
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
	}
	b.insertImplied(tagBody)
	// frameset-ok goes back to "ok" with the implied body: a template in
	// head set it to "not ok", and the expected trees keep a later
	// frameset (template.dat,
	// "<head><template></template></head><p><frameset>").
	b.framesetOK = true
	b.mode = modeInBody
	return true
}

// textMode is the "text" insertion mode (§13.2.6.4.8). Scripts are never
// executed, so a script end tag only pops the element. Start tags cannot
// occur: the tokenizer is in RCDATA, RAWTEXT or script data until the
// matching end tag.
func (b *treeBuilder) textMode(tok *Token) bool {
	switch tok.Kind {
	case CharacterToken:
		b.insertCharacters(tok.Data)
		return false
	case EOFToken:
		b.parseError(ErrEOFInElement)
		if b.textPushed {
			b.pop()
		}
		b.mode = b.origMode
		return true
	case EndTagToken:
		if b.textPushed {
			b.pop()
		}
		b.mode = b.origMode
		return false
	}
	return false
}
