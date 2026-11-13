package htmlparse

import "strings"

// The "in body" insertion mode, HTML §13.2.6.4.7, including the
// customizable-<select> rules (select, option, optgroup, hr, input) that
// replaced the "in select" insertion modes.

// inBodyMode is the "in body" insertion mode (§13.2.6.4.7).
func (b *treeBuilder) inBodyMode(tok *Token) bool {
	switch tok.Kind {
	case CharacterToken:
		b.inBodyCharacters(tok.Data)
		return false
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
		return b.inBodyStartTag(tok)
	case EndTagToken:
		return b.inBodyEndTag(tok)
	case EOFToken:
		if len(b.templateModes) > 0 {
			return b.inTemplateMode(tok)
		}
		b.checkUnclosedAtEnd()
		return false
	}
	return false
}

// inBodyCharacters handles any mix of character tokens in "in body": NULs
// are parse errors and ignored, the rest is inserted after reconstructing
// the active formatting elements, and non-whitespace clears frameset-ok.
// Reconstructing once for the whole run is what per-character processing
// does too: after the first character nothing is left to reconstruct.
func (b *treeBuilder) inBodyCharacters(s string) {
	if strings.IndexByte(s, 0) >= 0 {
		var sb strings.Builder
		for i := 0; i < len(s); i++ {
			if s[i] == 0 {
				b.parseError(ErrUnexpectedNullCharacter)
				continue
			}
			sb.WriteByte(s[i])
		}
		s = sb.String()
	}
	if s == "" {
		return
	}
	b.reconstructFormatting()
	b.insertCharacters(s)
	if b.framesetOK && !isAllSpace(s) {
		b.framesetOK = false
	}
}

// checkUnclosedAtEnd reports one parse error if an element other than
// dd, dt, li, optgroup, option, p, rb, rp, rt, rtc, tbody, td, tfoot, th,
// thead, tr, body and html (all HTML) is still open: the check of "in
// body" EOF and of the body and html end tags.
func (b *treeBuilder) checkUnclosedAtEnd() {
	for _, e := range b.stack {
		if e.ns != HTMLNamespace {
			b.parseError(ErrEOFInElement)
			return
		}
		switch e.tag {
		case tagDd, tagDt, tagLi, tagOptgroup, tagOption, tagP, tagRb, tagRp, tagRt, tagRtc,
			tagTbody, tagTd, tagTfoot, tagTh, tagThead, tagTr, tagBody, tagHtml:
		default:
			b.parseError(ErrEOFInElement)
			return
		}
	}
}

// mergeAttrs adds attributes of tok missing on element id (the "html" and
// "body" start tag rules); existing values are kept.
func (b *treeBuilder) mergeAttrs(id NodeID, tok *Token) {
	n := &b.doc.Nodes[id]
outer:
	for _, a := range tok.Attrs {
		for _, x := range n.Attrs {
			if x.Namespace == a.Namespace && x.Name == a.Name {
				continue outer
			}
		}
		n.Attrs = append(n.Attrs, a)
	}
}

// closePElement is "close a p element" (§13.2.6.4.7).
func (b *treeBuilder) closePElement() {
	b.generateImpliedEndTags(tagP)
	if !b.current().is(tagP) {
		b.parseError(ErrUnclosedElement)
	}
	b.popUntil(tagP)
}

// closePInButtonScope is the recurring "if the stack of open elements has
// a p element in button scope, then close a p element".
func (b *treeBuilder) closePInButtonScope() {
	if b.inScope(scopeButton, tagP) {
		b.closePElement()
	}
}

// inBodyStartTag is the start tag part of "in body", one case per entry
// of the standard, in its order.
func (b *treeBuilder) inBodyStartTag(tok *Token) bool {
	switch tok.atom {
	case tagHtml:
		b.parseError(ErrUnexpectedStartTag)
		if !b.templateOnStack() && len(b.stack) > 0 {
			b.mergeAttrs(b.stack[0].id, tok)
		}
		return false
	case tagBase, tagBasefont, tagBgsound, tagLink, tagMeta, tagNoframes, tagScript, tagStyle, tagTemplate, tagTitle:
		return b.inHeadMode(tok)
	case tagBody:
		b.parseError(ErrUnexpectedStartTag)
		if len(b.stack) < 2 || !b.stack[1].is(tagBody) || b.templateOnStack() {
			return false
		}
		b.framesetOK = false
		b.mergeAttrs(b.stack[1].id, tok)
		return false
	case tagFrameset:
		b.parseError(ErrUnexpectedStartTag)
		if len(b.stack) < 2 || !b.stack[1].is(tagBody) || !b.framesetOK {
			return false
		}
		// Remove the body from its parent and pop everything above the
		// root html element.
		b.doc.detach(b.stack[1].id)
		b.stack = b.stack[:1]
		b.insertHTML(tok)
		b.mode = modeInFrameset
		return false
	case tagAddress, tagArticle, tagAside, tagBlockquote, tagCenter, tagDetails, tagDialog,
		tagDir, tagDiv, tagDl, tagFieldset, tagFigcaption, tagFigure, tagFooter, tagHeader,
		tagHgroup, tagMain, tagMenu, tagNav, tagOl, tagP, tagSearch, tagSection, tagSummary, tagUl:
		b.closePInButtonScope()
		b.insertHTML(tok)
		return false
	case tagH1, tagH2, tagH3, tagH4, tagH5, tagH6:
		b.closePInButtonScope()
		if c := b.current(); c.ns == HTMLNamespace && isHeading(c.tag) {
			b.parseError(ErrUnexpectedStartTag)
			b.pop()
		}
		b.insertHTML(tok)
		return false
	case tagPre, tagListing:
		b.closePInButtonScope()
		b.insertHTML(tok)
		b.ignoreLF = true
		b.framesetOK = false
		return false
	case tagForm:
		parsingTemplate := b.parsingTemplateContents()
		if b.form != None && !parsingTemplate {
			b.parseError(ErrUnexpectedStartTag)
			return false
		}
		b.closePInButtonScope()
		id, _ := b.insertHTML(tok)
		if !parsingTemplate {
			b.form = id
		}
		return false
	case tagLi:
		// Close an open li unless a special element other than address,
		// div or p is in the way.
		b.framesetOK = false
		for i := len(b.stack) - 1; i >= 0; i-- {
			node := b.stack[i]
			if node.is(tagLi) {
				b.generateImpliedEndTags(tagLi)
				if !b.current().is(tagLi) {
					b.parseError(ErrUnclosedElement)
				}
				b.popUntil(tagLi)
				break
			}
			if isSpecial(node) && !node.is(tagAddress) && !node.is(tagDiv) && !node.is(tagP) {
				break
			}
		}
		b.closePInButtonScope()
		b.insertHTML(tok)
		return false
	case tagDd, tagDt:
		b.framesetOK = false
		for i := len(b.stack) - 1; i >= 0; i-- {
			node := b.stack[i]
			if node.is(tagDd) || node.is(tagDt) {
				b.generateImpliedEndTags(node.tag)
				if !b.current().is(node.tag) {
					b.parseError(ErrUnclosedElement)
				}
				b.popUntil(node.tag)
				break
			}
			if isSpecial(node) && !node.is(tagAddress) && !node.is(tagDiv) && !node.is(tagP) {
				break
			}
		}
		b.closePInButtonScope()
		b.insertHTML(tok)
		return false
	case tagPlaintext:
		b.closePInButtonScope()
		b.insertHTML(tok)
		b.tz.SetState(PLAINTEXTState)
		return false
	case tagButton:
		if b.inScope(scopeDefault, tagButton) {
			b.parseError(ErrUnexpectedStartTag)
			b.generateImpliedEndTags()
			b.popUntil(tagButton)
		}
		b.reconstructFormatting()
		b.insertHTML(tok)
		b.framesetOK = false
		return false
	case tagA:
		// An a in the list after the last marker: run the adoption agency
		// for this token, then make sure that a is gone from the list and
		// the stack (the algorithm may have left it).
		for i := len(b.afe) - 1; i >= 0 && b.afe[i].id != None; i-- {
			if b.afe[i].tag == tagA {
				existing := b.afe[i].id
				b.parseError(ErrUnexpectedStartTag)
				b.adoptionAgency(tok)
				if j := b.afeIndex(existing); j >= 0 {
					b.removeAFE(j)
				}
				b.removeFromStack(existing)
				break
			}
		}
		b.reconstructFormatting()
		b.insertFormatting(tok)
		return false
	case tagB, tagBig, tagCode, tagEm, tagFont, tagI, tagS, tagSmall, tagStrike, tagStrong, tagTt, tagU:
		b.reconstructFormatting()
		b.insertFormatting(tok)
		return false
	case tagNobr:
		b.reconstructFormatting()
		if b.inScope(scopeDefault, tagNobr) {
			b.parseError(ErrUnexpectedStartTag)
			b.adoptionAgency(tok)
			b.reconstructFormatting()
		}
		b.insertFormatting(tok)
		return false
	case tagApplet, tagMarquee, tagObject:
		b.reconstructFormatting()
		b.insertHTML(tok)
		b.insertMarker()
		b.framesetOK = false
		return false
	case tagTable:
		if b.doc.Mode != Quirks {
			b.closePInButtonScope()
		}
		b.insertHTML(tok)
		b.framesetOK = false
		b.mode = modeInTable
		return false
	case tagArea, tagBr, tagEmbed, tagImg, tagKeygen, tagWbr:
		b.reconstructFormatting()
		b.insertVoid(tok)
		b.framesetOK = false
		return false
	case tagInput:
		// The 2025 select rules: an input closes an open select.
		if b.fragment && b.ctx.is(tagSelect) {
			b.parseError(ErrUnexpectedStartTag)
			return false
		}
		if b.inScope(scopeDefault, tagSelect) {
			b.parseError(ErrUnexpectedStartTag)
			b.popUntil(tagSelect)
		}
		b.reconstructFormatting()
		b.insertVoid(tok)
		if !isHiddenInput(tok) {
			b.framesetOK = false
		}
		return false
	case tagParam, tagSource, tagTrack:
		b.insertVoid(tok)
		return false
	case tagHr:
		b.closePInButtonScope()
		if b.inScope(scopeDefault, tagSelect) {
			b.generateImpliedEndTags()
			if b.inScope(scopeDefault, tagOption) || b.inScope(scopeDefault, tagOptgroup) {
				b.parseError(ErrUnexpectedStartTag)
			}
		}
		b.insertVoid(tok)
		b.framesetOK = false
		return false
	case tagImage:
		// "Change the token's tag name to img and reprocess it. (Don't
		// ask.)"
		b.parseError(ErrUnexpectedStartTag)
		tok.Name, tok.atom = "img", tagImg
		return true
	case tagTextarea:
		// Like startText, plus the leading-LF rule and frameset-ok.
		_, b.textPushed = b.insertHTML(tok)
		b.ignoreLF = true
		b.tz.SetState(RCDATAState)
		b.origMode = b.mode
		b.framesetOK = false
		b.mode = modeText
		return false
	case tagXmp:
		b.closePInButtonScope()
		b.reconstructFormatting()
		b.framesetOK = false
		b.startText(tok, RAWTEXTState)
		return false
	case tagIframe:
		b.framesetOK = false
		b.startText(tok, RAWTEXTState)
		return false
	case tagNoembed:
		b.startText(tok, RAWTEXTState)
		return false
	case tagNoscript:
		if b.scripting {
			b.startText(tok, RAWTEXTState)
			return false
		}
		// Scripting off: noscript is an ordinary element (falls out of
		// the switch to "any other start tag").
	case tagSelect:
		switch {
		case b.fragment && b.ctx.is(tagSelect):
			b.parseError(ErrUnexpectedStartTag)
		case b.inScope(scopeDefault, tagSelect):
			b.parseError(ErrUnexpectedStartTag)
			b.popUntil(tagSelect)
		default:
			b.reconstructFormatting()
			b.insertHTML(tok)
			b.framesetOK = false
		}
		return false
	case tagOption:
		if b.inScope(scopeDefault, tagSelect) {
			b.generateImpliedEndTags(tagOptgroup)
			if b.inScope(scopeDefault, tagOption) {
				b.parseError(ErrUnexpectedStartTag)
			}
		} else if b.current().is(tagOption) {
			b.pop()
		}
		b.reconstructFormatting()
		b.insertHTML(tok)
		return false
	case tagOptgroup:
		if b.inScope(scopeDefault, tagSelect) {
			b.generateImpliedEndTags()
			if b.inScope(scopeDefault, tagOption) || b.inScope(scopeDefault, tagOptgroup) {
				b.parseError(ErrUnexpectedStartTag)
			}
		} else if b.current().is(tagOption) {
			b.pop()
		}
		b.reconstructFormatting()
		b.insertHTML(tok)
		return false
	case tagRb, tagRtc:
		if b.inScope(scopeDefault, tagRuby) {
			b.generateImpliedEndTags()
			if !b.current().is(tagRuby) {
				b.parseError(ErrUnclosedElement)
			}
		}
		b.insertHTML(tok)
		return false
	case tagRp, tagRt:
		if b.inScope(scopeDefault, tagRuby) {
			b.generateImpliedEndTags(tagRtc)
			if c := b.current(); !c.is(tagRtc) && !c.is(tagRuby) {
				b.parseError(ErrUnclosedElement)
			}
		}
		b.insertHTML(tok)
		return false
	case tagMath:
		b.reconstructFormatting()
		adjustMathMLAttributes(tok)
		adjustForeignAttributes(tok)
		b.insertForeignStart(tok, MathMLNamespace)
		return false
	case tagSvg:
		b.reconstructFormatting()
		adjustSVGAttributes(tok)
		adjustForeignAttributes(tok)
		b.insertForeignStart(tok, SVGNamespace)
		return false
	case tagCaption, tagCol, tagColgroup, tagFrame, tagHead, tagTbody, tagTd, tagTfoot, tagTh, tagThead, tagTr:
		b.parseError(ErrUnexpectedStartTag)
		return false
	}
	// Every case above that does not return falls out here.
	// Any other start tag.
	b.reconstructFormatting()
	b.insertHTML(tok)
	return false
}

// insertForeignStart inserts a math/svg element from "in body" and pops
// it again (acknowledging the flag) when the tag is self-closing.
func (b *treeBuilder) insertForeignStart(tok *Token, ns Namespace) {
	_, pushed := b.insertElement(tok, ns)
	if tok.SelfClosing {
		if pushed {
			b.pop()
		}
		b.ack = true
	}
}

// insertFormatting inserts a formatting element and pushes it onto the
// list of active formatting elements, unless the depth limit kept it from
// being opened: listed, it would be re-created by later reconstructions
// although it never held content (OPEN_QUESTIONS.md "Depth limit").
func (b *treeBuilder) insertFormatting(tok *Token) {
	id, pushed := b.insertHTML(tok)
	if pushed {
		b.pushFormatting(id, tok.atom)
	}
}

// removeAFE removes entry i of the list of active formatting elements.
func (b *treeBuilder) removeAFE(i int) {
	b.afe = append(b.afe[:i], b.afe[i+1:]...)
}

func isHeading(t tagID) bool {
	switch t {
	case tagH1, tagH2, tagH3, tagH4, tagH5, tagH6:
		return true
	}
	return false
}

// isHiddenInput reports a type attribute that is an ASCII
// case-insensitive match for "hidden" (the frameset-ok and "in table"
// input rules). Only the first type attribute counts; the tokenizer
// already dropped duplicates.
func isHiddenInput(tok *Token) bool {
	for _, a := range tok.Attrs {
		if a.Namespace == NoNamespace && a.Name == "type" {
			return equalFoldASCII(a.Value, "hidden")
		}
	}
	return false
}

// inBodyEndTag is the end tag part of "in body".
func (b *treeBuilder) inBodyEndTag(tok *Token) bool {
	switch tok.atom {
	case tagTemplate:
		return b.inHeadMode(tok)
	case tagBody, tagHtml:
		if !b.inScope(scopeDefault, tagBody) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		b.checkUnclosedAtEnd()
		b.mode = modeAfterBody
		return tok.atom == tagHtml
	case tagAddress, tagArticle, tagAside, tagBlockquote, tagButton, tagCenter, tagDetails,
		tagDialog, tagDir, tagDiv, tagDl, tagFieldset, tagFigcaption, tagFigure, tagFooter,
		tagHeader, tagHgroup, tagListing, tagMain, tagMenu, tagNav, tagOl, tagPre, tagSearch,
		tagSection, tagSelect, tagSummary, tagUl:
		if !b.inScope(scopeDefault, tok.atom) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		b.generateImpliedEndTags()
		if !b.current().is(tok.atom) {
			b.parseError(ErrUnclosedElement)
		}
		b.popUntil(tok.atom)
		return false
	case tagForm:
		// Outside template contents the form element pointer decides
		// (the form may be misnested and is removed from wherever it is
		// on the stack); inside, the nearest form in scope is closed.
		if !b.parsingTemplateContents() {
			node := b.form
			b.form = None
			if node == None || !b.nodeInScope(node) {
				b.parseError(ErrUnexpectedEndTag)
				return false
			}
			b.generateImpliedEndTags()
			if b.current().id != node {
				b.parseError(ErrUnclosedElement)
			}
			b.removeFromStack(node)
			return false
		}
		if !b.inScope(scopeDefault, tagForm) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		b.generateImpliedEndTags()
		if !b.current().is(tagForm) {
			b.parseError(ErrUnclosedElement)
		}
		b.popUntil(tagForm)
		return false
	case tagP:
		if !b.inScope(scopeButton, tagP) {
			// Insert an empty p and close it at once. At the depth limit
			// the p is inserted but not opened; closing would then pop up
			// to some p outside button scope, so stop here.
			b.parseError(ErrUnexpectedEndTag)
			if _, pushed := b.insertImplied(tagP); !pushed {
				return false
			}
		}
		b.closePElement()
		return false
	case tagLi:
		if !b.inScope(scopeListItem, tagLi) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		b.generateImpliedEndTags(tagLi)
		if !b.current().is(tagLi) {
			b.parseError(ErrUnclosedElement)
		}
		b.popUntil(tagLi)
		return false
	case tagDd, tagDt:
		if !b.inScope(scopeDefault, tok.atom) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		b.generateImpliedEndTags(tok.atom)
		if !b.current().is(tok.atom) {
			b.parseError(ErrUnclosedElement)
		}
		b.popUntil(tok.atom)
		return false
	case tagH1, tagH2, tagH3, tagH4, tagH5, tagH6:
		if !b.inScope(scopeDefault, tagH1, tagH2, tagH3, tagH4, tagH5, tagH6) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		b.generateImpliedEndTags()
		if !b.current().is(tok.atom) {
			b.parseError(ErrUnclosedElement)
		}
		b.popUntil(tagH1, tagH2, tagH3, tagH4, tagH5, tagH6)
		return false
	case tagA, tagB, tagBig, tagCode, tagEm, tagFont, tagI, tagNobr, tagS, tagSmall, tagStrike, tagStrong, tagTt, tagU:
		b.adoptionAgency(tok)
		return false
	case tagApplet, tagMarquee, tagObject:
		if !b.inScope(scopeDefault, tok.atom) {
			b.parseError(ErrUnexpectedEndTag)
			return false
		}
		b.generateImpliedEndTags()
		if !b.current().is(tok.atom) {
			b.parseError(ErrUnclosedElement)
		}
		b.popUntil(tok.atom)
		b.clearToLastMarker()
		return false
	case tagBr:
		// </br> acts as <br> with no attributes.
		b.parseError(ErrUnexpectedEndTag)
		br := Token{Kind: StartTagToken, Name: "br", atom: tagBr, Span: tok.Span}
		b.reconstructFormatting()
		b.insertVoid(&br)
		b.framesetOK = false
		return false
	}
	b.anyOtherEndTag(tok)
	return false
}

// anyOtherEndTag is the "any other end tag" entry of "in body": walk down
// the stack to an HTML element with the token's name and pop through it,
// unless a special element comes first (then the token is ignored). The
// loop stops above the root, which is special anyway.
func (b *treeBuilder) anyOtherEndTag(tok *Token) {
	for i := len(b.stack) - 1; i > 0; i-- {
		node := b.stack[i]
		if node.ns == HTMLNamespace && b.sameName(node, tok) {
			b.generateImpliedEndTagsExceptName(tok.Name)
			if b.current().id != node.id {
				b.parseError(ErrUnclosedElement)
			}
			b.stack = b.stack[:i]
			return
		}
		if isSpecial(node) {
			b.parseError(ErrUnexpectedEndTag)
			return
		}
	}
	b.parseError(ErrUnexpectedEndTag)
}

// sameName reports whether stack entry e has the token's tag name:
// atoms compare directly, and two non-atom names compare as strings.
func (b *treeBuilder) sameName(e stackEntry, tok *Token) bool {
	if tok.atom != tagUnknown || e.tag != tagUnknown {
		return e.tag == tok.atom && tok.atom != tagUnknown
	}
	return b.doc.Nodes[e.id].Name == tok.Name
}
