package htmlparse

// Options configures Parse, ParseFragment and ParseRaw.
type Options struct {
	// Scripting sets the scripting flag. It is off by default: Flowforge
	// never executes script, so noscript content parses as markup. With
	// Scripting on, noscript is raw text (as in a browser with script
	// enabled); scripts are still never run.
	Scripting bool
	// MaxDepth limits the stack of open elements (0: DefaultMaxDepth,
	// negative: unlimited). An element that would exceed it is inserted
	// without being opened (ErrNestingDepthLimit).
	MaxDepth int
	// MaxAttributes limits attributes per tag (0: DefaultMaxAttributes,
	// negative: unlimited); extra attributes are dropped
	// (ErrAttributeLimit).
	MaxAttributes int
	// MaxInputSize limits the input in bytes (0: DefaultMaxInputSize,
	// negative: unlimited); only the prefix is parsed (ErrInputSizeLimit).
	MaxInputSize int
}

func (o Options) tokenizeOptions() TokenizeOptions {
	return TokenizeOptions{MaxAttributes: o.MaxAttributes, MaxInputSize: o.MaxInputSize}
}

// Parse parses a complete HTML document from UTF-8 input (use Decode or
// ParseRaw for other encodings) and returns the arena DOM and the parse
// errors in input order. Parsing is total: every byte string yields a
// document.
func Parse(input []byte, opts Options) (*Document, []ParseError) {
	doc, errs, _ := parseDocument(input, opts)
	return doc, errs
}

func parseDocument(input []byte, opts Options) (*Document, []ParseError, *treeBuilder) {
	doc := &Document{Nodes: make([]Node, 0, nodeCapacityHint(input))}
	doc.Root = doc.NewNode(DocumentNode)
	tz := NewTokenizer(input, opts.tokenizeOptions())
	b := newTreeBuilder(doc, tz, opts)
	b.run()
	return doc, collectErrors(tz, b), b
}

// collectErrors merges tokenizer and tree-construction errors by offset
// (stable, so errors at the same offset keep their emission order) and
// fills in line and column.
func collectErrors(tz *Tokenizer, b *treeBuilder) []ParseError {
	errs := make([]ParseError, 0, len(tz.errs)+len(b.errs))
	i, j := 0, 0
	for i < len(tz.errs) || j < len(b.errs) {
		if j >= len(b.errs) || (i < len(tz.errs) && tz.errs[i].Offset <= b.errs[j].Offset) {
			errs = append(errs, tz.errs[i])
			i++
		} else {
			errs = append(errs, b.errs[j])
			j++
		}
	}
	fillPositions(tz.in, errs)
	return errs
}

// noteMetaCharset records the encoding a <meta> in head declares (the
// "in head" meta rule's "change the encoding"); ParseRaw acts on it when
// the confidence was tentative.
func (b *treeBuilder) noteMetaCharset(tok *Token) {
	if b.metaCharset != "" {
		return
	}
	var httpEquiv, content string
	var hasHTTPEquiv, hasContent bool
	for _, a := range tok.Attrs {
		switch a.Name {
		case "charset":
			if e, ok := LookupEncoding(a.Value); ok {
				b.metaCharset = string(e)
				return
			}
		case "http-equiv":
			httpEquiv, hasHTTPEquiv = a.Value, true
		case "content":
			content, hasContent = a.Value, true
		}
	}
	if hasHTTPEquiv && hasContent && equalFoldASCII(httpEquiv, "content-type") {
		if e, ok := extractCharsetFromContent(content); ok {
			b.metaCharset = string(e)
		}
	}
}

// ParseRaw decodes raw (Decode, with the transport charset or
// Content-Type value transportCharset), parses it and, if the confidence
// was tentative and a <meta> in head declares a different encoding,
// re-decodes with that encoding (confidence certain) and parses again —
// the "change the encoding while parsing" algorithm (§13.2.3.4) done as
// a second pass. It returns the encoding finally used. Error offsets
// refer to the decoded UTF-8 text, not to raw.
func ParseRaw(raw []byte, transportCharset string, opts Options) (*Document, Encoding, []ParseError) {
	text, enc, conf := Decode(raw, transportCharset)
	doc, errs, b := parseDocument(text, opts)
	meta := b.metaCharset
	if conf != ConfidenceTentative || meta == "" || enc == EncodingUTF16LE || enc == EncodingUTF16BE {
		return doc, enc, errs
	}
	next := Encoding(meta)
	switch next {
	case EncodingUTF16LE, EncodingUTF16BE:
		next = EncodingUTF8
	case EncodingXUserDefined:
		next = EncodingWindows1252
	}
	if next == enc {
		return doc, enc, errs
	}
	doc, errs, _ = parseDocument(DecodeAs(raw, next), opts)
	return doc, next, errs
}
