package htmlparse

import "strings"

// Foreign content: HTML §13.2.6.5 "The rules for parsing tokens in
// foreign content", and the SVG/MathML/foreign attribute and tag name
// adjustments of §13.2.6.1.

// foreignCharacters handles a character token in foreign content: NUL
// becomes U+FFFD (a parse error), and non-whitespace clears frameset-ok.
func (b *treeBuilder) foreignCharacters(tok *Token) {
	s := tok.Data
	if strings.IndexByte(s, 0) >= 0 {
		var sb strings.Builder
		for i := 0; i < len(s); i++ {
			if s[i] == 0 {
				b.parseError(ErrUnexpectedNullCharacter)
				sb.WriteString("�")
				continue
			}
			sb.WriteByte(s[i])
		}
		s = sb.String()
	}
	for i := 0; i < len(tok.Data); i++ {
		if c := charClass(tok.Data[i]); c == 0 {
			b.framesetOK = false
			break
		}
	}
	b.insertCharacters(s)
}

// breakoutTag reports the start tags that break out of foreign content.
func breakoutTag(tok *Token) bool {
	switch tok.atom {
	case tagB, tagBig, tagBlockquote, tagBody, tagBr, tagCenter, tagCode, tagDd, tagDiv,
		tagDl, tagDt, tagEm, tagEmbed, tagH1, tagH2, tagH3, tagH4, tagH5, tagH6, tagHead,
		tagHr, tagI, tagImg, tagLi, tagListing, tagMenu, tagMeta, tagNobr, tagOl, tagP,
		tagPre, tagRuby, tagS, tagSmall, tagSpan, tagStrong, tagStrike, tagSub, tagSup,
		tagTable, tagTt, tagU, tagUl, tagVar:
		return true
	case tagFont:
		for _, a := range tok.Attrs {
			if a.Name == "color" || a.Name == "face" || a.Name == "size" {
				return true
			}
		}
	}
	return false
}

// foreignContent applies the rules for parsing tokens in foreign content
// to tok. It returns true when the token must be reprocessed by the
// dispatcher.
func (b *treeBuilder) foreignContent(tok *Token) bool {
	switch tok.Kind {
	case CharacterToken:
		b.foreignCharacters(tok)
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
		if breakoutTag(tok) {
			return b.foreignBreakout(tok)
		}
		acn := b.adjustedCurrent()
		ns := acn.ns
		switch ns {
		case MathMLNamespace:
			adjustMathMLAttributes(tok)
		case SVGNamespace:
			if fixed, ok := svgTagNameFixups[tok.Name]; ok {
				tok.Name = fixed
			}
			adjustSVGAttributes(tok)
		}
		adjustForeignAttributes(tok)
		_, pushed := b.insertElement(tok, ns)
		if tok.SelfClosing {
			// For an SVG script this also is "act as described in the
			// steps for a script end tag": pop it (scripts never run).
			if pushed {
				b.pop()
			}
			b.ack = true
		}
		return false
	case EndTagToken:
		if tok.atom == tagBr || tok.atom == tagP {
			return b.foreignBreakout(tok)
		}
		if c := b.current(); tok.Name == "script" && c.ns == SVGNamespace && b.doc.Nodes[c.id].Name == "script" {
			b.pop()
			return false
		}
		return b.foreignEndTag(tok)
	}
	return false
}

// foreignBreakout pops until an HTML element or integration point is the
// current node and asks for the token to be reprocessed in the current
// insertion mode.
func (b *treeBuilder) foreignBreakout(tok *Token) bool {
	b.parseError(ErrUnexpectedStartTag)
	for len(b.stack) > 1 {
		c := b.current()
		if c.ns == HTMLNamespace || isMathMLTextIntegrationPoint(c) || isHTMLIntegrationPoint(c) {
			break
		}
		b.pop()
	}
	return b.processInMode(b.mode, tok)
}

// foreignEndTag is the "any other end tag" entry of the foreign rules.
func (b *treeBuilder) foreignEndTag(tok *Token) bool {
	i := len(b.stack) - 1
	if i < 0 {
		return false
	}
	if lowerASCII(b.doc.Nodes[b.stack[i].id].Name) != tok.Name {
		b.parseError(ErrUnexpectedEndTag)
	}
	for ; i > 0; i-- {
		node := b.stack[i]
		if lowerASCII(b.doc.Nodes[node.id].Name) == tok.Name {
			b.stack = b.stack[:i]
			return false
		}
		if b.stack[i-1].ns == HTMLNamespace {
			// Process the token with the current insertion mode's rules
			// (not the dispatcher); a "reprocess" they request goes back
			// through the dispatcher in the caller's loop.
			return b.processInMode(b.mode, tok)
		}
	}
	return false
}

// svgTagNameFixups is the SVG tag name case table of §13.2.6.5.
//
// PORT: this and svgAttrFixups are lookup-only maps (a Rust match or phf
// map); nothing iterates them.
var svgTagNameFixups = map[string]string{
	"altglyph": "altGlyph", "altglyphdef": "altGlyphDef", "altglyphitem": "altGlyphItem",
	"animatecolor": "animateColor", "animatemotion": "animateMotion", "animatetransform": "animateTransform",
	"clippath": "clipPath", "feblend": "feBlend", "fecolormatrix": "feColorMatrix",
	"fecomponenttransfer": "feComponentTransfer", "fecomposite": "feComposite",
	"feconvolvematrix": "feConvolveMatrix", "fediffuselighting": "feDiffuseLighting",
	"fedisplacementmap": "feDisplacementMap", "fedistantlight": "feDistantLight",
	"fedropshadow": "feDropShadow", "feflood": "feFlood", "fefunca": "feFuncA", "fefuncb": "feFuncB",
	"fefuncg": "feFuncG", "fefuncr": "feFuncR", "fegaussianblur": "feGaussianBlur", "feimage": "feImage",
	"femerge": "feMerge", "femergenode": "feMergeNode", "femorphology": "feMorphology",
	"feoffset": "feOffset", "fepointlight": "fePointLight", "fespecularlighting": "feSpecularLighting",
	"fespotlight": "feSpotLight", "fetile": "feTile", "feturbulence": "feTurbulence",
	"foreignobject": "foreignObject", "glyphref": "glyphRef", "lineargradient": "linearGradient",
	"radialgradient": "radialGradient", "textpath": "textPath",
}

// svgAttrFixups is the "adjust SVG attributes" table of §13.2.6.1.
var svgAttrFixups = map[string]string{
	"attributename": "attributeName", "attributetype": "attributeType", "basefrequency": "baseFrequency",
	"baseprofile": "baseProfile", "calcmode": "calcMode", "clippathunits": "clipPathUnits",
	"diffuseconstant": "diffuseConstant", "edgemode": "edgeMode", "filterunits": "filterUnits",
	"glyphref": "glyphRef", "gradienttransform": "gradientTransform", "gradientunits": "gradientUnits",
	"kernelmatrix": "kernelMatrix", "kernelunitlength": "kernelUnitLength", "keypoints": "keyPoints",
	"keysplines": "keySplines", "keytimes": "keyTimes", "lengthadjust": "lengthAdjust",
	"limitingconeangle": "limitingConeAngle", "markerheight": "markerHeight", "markerunits": "markerUnits",
	"markerwidth": "markerWidth", "maskcontentunits": "maskContentUnits", "maskunits": "maskUnits",
	"numoctaves": "numOctaves", "pathlength": "pathLength", "patterncontentunits": "patternContentUnits",
	"patterntransform": "patternTransform", "patternunits": "patternUnits", "pointsatx": "pointsAtX",
	"pointsaty": "pointsAtY", "pointsatz": "pointsAtZ", "preservealpha": "preserveAlpha",
	"preserveaspectratio": "preserveAspectRatio", "primitiveunits": "primitiveUnits", "refx": "refX",
	"refy": "refY", "repeatcount": "repeatCount", "repeatdur": "repeatDur",
	"requiredextensions": "requiredExtensions", "requiredfeatures": "requiredFeatures",
	"specularconstant": "specularConstant", "specularexponent": "specularExponent",
	"spreadmethod": "spreadMethod", "startoffset": "startOffset", "stddeviation": "stdDeviation",
	"stitchtiles": "stitchTiles", "surfacescale": "surfaceScale", "systemlanguage": "systemLanguage",
	"tablevalues": "tableValues", "targetx": "targetX", "targety": "targetY", "textlength": "textLength",
	"viewbox": "viewBox", "viewtarget": "viewTarget", "xchannelselector": "xChannelSelector",
	"ychannelselector": "yChannelSelector", "zoomandpan": "zoomAndPan",
}

// adjustMathMLAttributes is "adjust MathML attributes".
func adjustMathMLAttributes(tok *Token) {
	for i := range tok.Attrs {
		if tok.Attrs[i].Name == "definitionurl" {
			tok.Attrs[i].Name = "definitionURL"
		}
	}
}

// adjustSVGAttributes is "adjust SVG attributes".
func adjustSVGAttributes(tok *Token) {
	for i := range tok.Attrs {
		if fixed, ok := svgAttrFixups[tok.Attrs[i].Name]; ok {
			tok.Attrs[i].Name = fixed
		}
	}
}

// adjustForeignAttributes is "adjust foreign attributes": the xlink:,
// xml: and xmlns attributes become namespaced attributes.
func adjustForeignAttributes(tok *Token) {
	for i := range tok.Attrs {
		a := &tok.Attrs[i]
		switch a.Name {
		case "xlink:actuate", "xlink:arcrole", "xlink:href", "xlink:role", "xlink:show", "xlink:title", "xlink:type":
			a.Namespace, a.Name = XLinkNamespace, a.Name[len("xlink:"):]
		case "xml:lang", "xml:space":
			a.Namespace, a.Name = XMLNamespace, a.Name[len("xml:"):]
		case "xmlns":
			a.Namespace = XMLNSNamespace
		case "xmlns:xlink":
			a.Namespace, a.Name = XMLNSNamespace, "xlink"
		}
	}
}

// checkXMLNS reports the parse errors of "create an element for a token"
// for xmlns / xmlns:xlink attributes that disagree with the namespace.
func (b *treeBuilder) checkXMLNS(id NodeID, ns Namespace) {
	for _, a := range b.doc.Nodes[id].Attrs {
		if a.Namespace != XMLNSNamespace {
			continue
		}
		if (a.Name == "xmlns" && a.Value != ns.URI()) || (a.Name == "xlink" && a.Value != XLinkNamespace.URI()) {
			b.parseError(ErrXMLNSAttributeMismatch)
		}
	}
}
