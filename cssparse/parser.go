package cssparse

// NodeID indexes Document.Nodes. NoNode (-1) means "none".
type NodeID int32

// NoNode is the null NodeID.
const NoNode NodeID = -1

// NodeKind is the type of a tree node (CSS Syntax 3 §5, "Parsing").
type NodeKind uint8

// Node kinds.
const (
	NodeInvalid NodeKind = iota
	// Roots: one per "parse a ..." entry point.
	NodeStylesheet         // §5.3.3 parse a stylesheet
	NodeRuleList           // §5.3.4 parse a list of rules
	NodeDeclarationList    // §5.3.8 parse a list of declarations (2021 CR)
	NodeBlockContents      // "parse a block's contents" (current ED, with nesting)
	NodeComponentValueList // §5.3.10 parse a list of component values; also the one-item parsers
	// Rules and declarations.
	NodeQualifiedRule // children: prelude; Block: the {} block
	NodeAtRule        // Token: the at-keyword; children: prelude; Block: {} block or NoNode
	NodeDeclaration   // Token: the name ident; children: value; Important flag
	// Component values.
	NodeSimpleBlock // Token: the opening bracket; CloseToken: its mirror or -1 at EOF
	NodeFunction    // Token: the function token; CloseToken: ")" or -1 at EOF
	NodePreserved   // Token: any other token
	// NodeError marks input the algorithms threw away (an invalid rule or
	// declaration, or a block elided by the depth limit). Its children are
	// the discarded component values; Error says why.
	NodeError
)

var nodeKindNames = [...]string{
	"invalid", "stylesheet", "rule-list", "declaration-list", "block-contents",
	"component-value-list", "qualified-rule", "at-rule", "declaration",
	"simple-block", "function", "preserved", "error",
}

// String returns the stable name of k.
func (k NodeKind) String() string {
	if int(k) < len(nodeKindNames) {
		return nodeKindNames[k]
	}
	return "node?"
}

// Node is one arena node. Children form a singly linked list
// (FirstChild, NextSibling); LastChild makes appends O(1).
type Node struct {
	Kind      NodeKind
	Error     DiagCode // NodeError only
	Important bool     // NodeDeclaration: "!important" was present (and removed from the value)
	Parent    NodeID
	// FirstChild, LastChild and NextSibling link the children: the
	// prelude of a rule, the value of a declaration, the contents of a
	// block or function, or the items of a root.
	FirstChild, LastChild, NextSibling NodeID
	// Block is the {} block of a rule (a DiagDepthLimit NodeError when the
	// depth limit elided it). For a NodeBlockContents root made
	// by ParseBlockContents it is the block that was re-parsed.
	Block NodeID
	// Token is the token index of the node's defining token, or -1.
	Token int32
	// CloseToken is the closing token of a block or function, or -1.
	CloseToken int32
	// Start and End are the node's byte span in Document.Src.
	Start, End int32
}

// Document is a parsed CSS source: the token list, the node arena and the
// diagnostics. Nodes not reachable from Root can exist (the name and colon
// of a declaration, for example); walk from Root.
type Document struct {
	Src    string
	Tokens []Token
	Nodes  []Node
	Diags  []Diagnostic
	Root   NodeID
	opts   Options
}

// Grade reports whether a limit truncated the document (GradePartial) or
// not (GradeComplete). CSS parsing never fails.
func (d *Document) Grade() Grade { return gradeOf(d.Diags) }

// Node returns a pointer to node id.
func (d *Document) Node(id NodeID) *Node { return &d.Nodes[id] }

// Children returns the child ids of id in order.
func (d *Document) Children(id NodeID) []NodeID {
	var out []NodeID
	for c := d.Nodes[id].FirstChild; c != NoNode; c = d.Nodes[c].NextSibling {
		out = append(out, c)
	}
	return out
}

// Name returns the name of an at-rule, function or declaration, or the
// token value of a preserved token.
func (d *Document) Name(id NodeID) string {
	n := &d.Nodes[id]
	if n.Token < 0 {
		return ""
	}
	return d.Tokens[n.Token].Value
}

// TokenOf returns the defining token of id, or nil.
func (d *Document) TokenOf(id NodeID) *Token {
	n := &d.Nodes[id]
	if n.Token < 0 {
		return nil
	}
	return &d.Tokens[n.Token]
}

type parser struct {
	d        *Document
	pos, end int32
	// base is the nesting depth of the tokens being parsed (non-zero
	// when re-parsing a nested block's contents).
	base     int
	maxDepth int
	scratch  []NodeID
	stack    []NodeID
	elide    []TokenKind
}

func newDocument(src []byte, opts Options) (*Document, *parser) {
	t := tokenizeString(string(src), opts)
	d := &Document{Src: t.src, Tokens: t.toks, Diags: t.diags, opts: opts,
		Nodes: make([]Node, 0, len(t.toks)/2+4)}
	p := &parser{d: d, end: int32(len(t.toks)), maxDepth: opts.maxDepth()}
	return d, p
}

func (p *parser) newNode(kind NodeKind, tok int32) NodeID {
	n := Node{Kind: kind, Parent: NoNode, FirstChild: NoNode, LastChild: NoNode,
		NextSibling: NoNode, Block: NoNode, Token: tok, CloseToken: -1}
	if tok >= 0 {
		n.Start, n.End = p.d.Tokens[tok].Start, p.d.Tokens[tok].End
	} else {
		n.Start, n.End = p.here(), p.here()
	}
	p.d.Nodes = append(p.d.Nodes, n)
	return NodeID(len(p.d.Nodes) - 1)
}

// here is the byte offset of the next unconsumed token.
func (p *parser) here() int32 {
	if p.pos < int32(len(p.d.Tokens)) {
		return p.d.Tokens[p.pos].Start
	}
	return int32(len(p.d.Src))
}

func (p *parser) appendChild(parent, child NodeID) {
	nodes := p.d.Nodes
	nodes[child].Parent = parent
	nodes[child].NextSibling = NoNode
	if nodes[parent].LastChild == NoNode {
		nodes[parent].FirstChild = child
		if nodes[parent].Token < 0 && !isRootKind(nodes[parent].Kind) {
			nodes[parent].Start, nodes[parent].End = nodes[child].Start, nodes[child].End
		}
	} else {
		nodes[nodes[parent].LastChild].NextSibling = child
	}
	nodes[parent].LastChild = child
	if nodes[child].End > nodes[parent].End {
		nodes[parent].End = nodes[child].End
	}
	if nodes[child].Start < nodes[parent].Start {
		nodes[parent].Start = nodes[child].Start
	}
}

func isRootKind(k NodeKind) bool { return k >= NodeStylesheet && k <= NodeComponentValueList }

func (p *parser) link(parent NodeID, ids []NodeID) {
	for _, c := range ids {
		p.appendChild(parent, c)
	}
}

func (p *parser) diag(code DiagCode, start, end int32) {
	p.d.Diags = append(p.d.Diags, Diagnostic{Code: code, Start: start, End: end})
}

// errorNode wraps the discarded component values ids.
func (p *parser) errorNode(code DiagCode, ids []NodeID) NodeID {
	e := p.newNode(NodeError, -1)
	p.d.Nodes[e].Error = code
	p.link(e, ids)
	n := &p.d.Nodes[e]
	p.diag(code, n.Start, n.End)
	return e
}

// peek returns the index of the next non-comment token, or -1 at EOF.
func (p *parser) peek() int32 {
	for p.pos < p.end && p.d.Tokens[p.pos].Kind == TokenComment {
		p.pos++
	}
	if p.pos >= p.end {
		return -1
	}
	return p.pos
}

func (p *parser) kind(i int32) TokenKind {
	if i < 0 {
		return TokenInvalid
	}
	return p.d.Tokens[i].Kind
}

func (p *parser) skipWhitespace() {
	for p.kind(p.peek()) == TokenWhitespace {
		p.pos++
	}
}

// mirror returns the closing token kind for an opening one, or
// TokenInvalid.
func mirror(k TokenKind) TokenKind {
	switch k {
	case TokenOpenCurly:
		return TokenCloseCurly
	case TokenOpenSquare:
		return TokenCloseSquare
	case TokenOpenParen, TokenFunction:
		return TokenCloseParen
	}
	return TokenInvalid
}

func (p *parser) openNode(i int32) NodeID {
	if p.d.Tokens[i].Kind == TokenFunction {
		return p.newNode(NodeFunction, i)
	}
	return p.newNode(NodeSimpleBlock, i)
}

// consumeComponentValue is §5.4.7 "Consume a component value", with
// §5.4.8 "consume a simple block" and §5.4.9 "consume a function" done
// iteratively over an explicit stack so nesting cannot overflow the call
// stack (a Rust port must not recurse either). The next token must exist.
func (p *parser) consumeComponentValue() NodeID {
	i := p.peek()
	p.pos++
	if mirror(p.kind(i)) == TokenInvalid {
		return p.newNode(NodePreserved, i)
	}
	if p.base+1 > p.maxDepth {
		return p.elideBlock(i)
	}
	root := p.openNode(i)
	stack := append(p.stack[:0], root)
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		j := p.peek()
		if j < 0 {
			p.diag(DiagEOFInBlock, p.d.Nodes[stack[0]].Start, int32(len(p.d.Src)))
			break
		}
		p.pos++
		k := p.kind(j)
		if k == mirror(p.kind(p.d.Nodes[top].Token)) {
			p.d.Nodes[top].CloseToken = j
			p.d.Nodes[top].End = p.d.Tokens[j].End
			stack = stack[:len(stack)-1]
			if len(stack) > 0 {
				p.appendChild(stack[len(stack)-1], top)
			}
			continue
		}
		if mirror(k) != TokenInvalid {
			if p.base+len(stack)+1 > p.maxDepth {
				e := p.elideBlock(j)
				p.appendChild(top, e)
				continue
			}
			stack = append(stack, p.openNode(j))
			continue
		}
		p.appendChild(top, p.newNode(NodePreserved, j))
	}
	// At EOF, attach the still-open blocks to their parents.
	for len(stack) > 1 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		p.appendChild(stack[len(stack)-1], top)
	}
	p.stack = stack[:0]
	return root
}

// elideBlock skips the block opened by token i (already consumed) up to
// its matching close, returning one DiagDepthLimit error node.
func (p *parser) elideBlock(i int32) NodeID {
	e := p.newNode(NodeError, i)
	p.d.Nodes[e].Error = DiagDepthLimit
	want := append(p.elide[:0], mirror(p.kind(i)))
	for len(want) > 0 {
		j := p.peek()
		if j < 0 {
			break
		}
		p.pos++
		k := p.kind(j)
		p.d.Nodes[e].End = p.d.Tokens[j].End
		if k == want[len(want)-1] {
			want = want[:len(want)-1]
		} else if m := mirror(k); m != TokenInvalid {
			want = append(want, m)
		}
	}
	p.elide = want[:0]
	n := &p.d.Nodes[e]
	p.diag(DiagDepthLimit, n.Start, n.End)
	return e
}

// consumeListOfRules is §5.4.1 "Consume a list of rules" (2021 CR).
func (p *parser) consumeListOfRules(root NodeID, topLevel bool) {
	for {
		i := p.peek()
		switch p.kind(i) {
		case TokenInvalid:
			return
		case TokenWhitespace:
			p.pos++
		case TokenCDO, TokenCDC:
			if topLevel {
				p.pos++
				continue
			}
			p.appendChild(root, p.consumeQualifiedRule(false, false))
		case TokenAtKeyword:
			p.appendChild(root, p.consumeAtRule(false))
		default:
			p.appendChild(root, p.consumeQualifiedRule(false, false))
		}
	}
}

// consumeAtRule is §5.4.2 "Consume an at-rule". With nested, a "}" ends
// the rule without being consumed (current ED).
func (p *parser) consumeAtRule(nested bool) NodeID {
	i := p.peek()
	p.pos++
	rule := p.newNode(NodeAtRule, i)
	mark := len(p.scratch)
	defer func() { p.scratch = p.scratch[:mark] }()
	for {
		j := p.peek()
		switch p.kind(j) {
		case TokenInvalid:
			p.diag(DiagEOFInAtRule, p.d.Nodes[rule].Start, int32(len(p.d.Src)))
			p.link(rule, p.scratch[mark:])
			return rule
		case TokenSemicolon:
			p.pos++
			p.link(rule, p.scratch[mark:])
			p.d.Nodes[rule].End = p.d.Tokens[j].End
			return rule
		case TokenCloseCurly:
			if nested {
				p.link(rule, p.scratch[mark:])
				return rule
			}
		case TokenOpenCurly:
			block := p.consumeComponentValue()
			p.link(rule, p.scratch[mark:])
			p.setBlock(rule, block)
			return rule
		}
		v := p.consumeComponentValue()
		p.scratch = append(p.scratch, v)
	}
}

func (p *parser) setBlock(rule, block NodeID) {
	p.d.Nodes[rule].Block = block
	p.d.Nodes[block].Parent = rule
	if e := p.d.Nodes[block].End; e > p.d.Nodes[rule].End {
		p.d.Nodes[rule].End = e
	}
}

// consumeQualifiedRule is §5.4.3 "Consume a qualified rule". It returns
// the rule, or an error node holding the discarded prelude. nested and
// stopSemicolon select the current-ED behaviour used inside blocks.
func (p *parser) consumeQualifiedRule(nested, stopSemicolon bool) NodeID {
	mark := len(p.scratch)
	defer func() { p.scratch = p.scratch[:mark] }()
	for {
		i := p.peek()
		switch k := p.kind(i); {
		case k == TokenInvalid,
			stopSemicolon && k == TokenSemicolon,
			nested && k == TokenCloseCurly:
			return p.errorNode(DiagInvalidRule, p.scratch[mark:])
		case k == TokenOpenCurly:
			rule := p.newNode(NodeQualifiedRule, -1)
			block := p.consumeComponentValue()
			p.link(rule, p.scratch[mark:])
			if p.d.Nodes[rule].FirstChild == NoNode {
				p.d.Nodes[rule].Start = p.d.Nodes[block].Start
			}
			p.setBlock(rule, block)
			return rule
		}
		v := p.consumeComponentValue()
		p.scratch = append(p.scratch, v)
	}
}

// consumeDeclarationList is §5.4.5 "Consume a list of declarations"
// (2021 CR): declarations and at-rules, no nested rules.
func (p *parser) consumeDeclarationList(root NodeID) {
	for {
		i := p.peek()
		switch p.kind(i) {
		case TokenInvalid:
			return
		case TokenWhitespace, TokenSemicolon:
			p.pos++
		case TokenAtKeyword:
			p.appendChild(root, p.consumeAtRule(false))
		case TokenIdent:
			mark := len(p.scratch)
			p.collectUntilSemicolon(false)
			p.appendChild(root, p.declarationFrom(p.scratch[mark:], false))
			p.scratch = p.scratch[:mark]
		default:
			mark := len(p.scratch)
			p.collectUntilSemicolon(false)
			p.appendChild(root, p.errorNode(DiagInvalidDeclaration, p.scratch[mark:]))
			p.scratch = p.scratch[:mark]
		}
	}
}

// collectUntilSemicolon appends component values to scratch up to (not
// including) the next top-level ";" or EOF (and "}" when nested).
func (p *parser) collectUntilSemicolon(nested bool) {
	for {
		k := p.kind(p.peek())
		if k == TokenInvalid || k == TokenSemicolon || (nested && k == TokenCloseCurly) {
			return
		}
		v := p.consumeComponentValue()
		p.scratch = append(p.scratch, v)
	}
}

func (p *parser) preservedKind(id NodeID) TokenKind {
	n := &p.d.Nodes[id]
	if n.Kind != NodePreserved {
		return TokenInvalid
	}
	return p.d.Tokens[n.Token].Kind
}

// declarationFrom is §5.4.6 "Consume a declaration" over the component
// values ids (ids[0] is the name ident). With ed, the current ED's rule
// that a {} block must be a non-custom property's whole value applies,
// and failure returns NoNode instead of an error node.
func (p *parser) declarationFrom(ids []NodeID, ed bool) NodeID {
	fail := func() NodeID {
		if ed {
			return NoNode
		}
		return p.errorNode(DiagInvalidDeclaration, ids)
	}
	if len(ids) == 0 || p.preservedKind(ids[0]) != TokenIdent {
		return fail()
	}
	k := 1
	for k < len(ids) && p.preservedKind(ids[k]) == TokenWhitespace {
		k++
	}
	if k >= len(ids) || p.preservedKind(ids[k]) != TokenColon {
		return fail()
	}
	value := ids[k+1:]
	important := false
	last, prev := -1, -1
	for j := len(value) - 1; j >= 0; j-- {
		if p.preservedKind(value[j]) == TokenWhitespace {
			continue
		}
		if last < 0 {
			last = j
			continue
		}
		prev = j
		break
	}
	if prev >= 0 && p.preservedKind(value[prev]) == TokenDelim && p.d.TokenOf(value[prev]).Value == "!" &&
		p.preservedKind(value[last]) == TokenIdent && asciiEqualFold(p.d.TokenOf(value[last]).Value, "important") {
		value = value[:prev]
		important = true
	}
	name := p.d.Nodes[ids[0]].Token
	if ed && !hasPrefix(p.d.Tokens[name].Value, "--") {
		curly, other := false, false
		for _, v := range value {
			switch {
			case p.d.Nodes[v].Kind == NodeSimpleBlock && p.kind(p.d.Nodes[v].Token) == TokenOpenCurly:
				curly = true
			case p.preservedKind(v) != TokenWhitespace:
				other = true
			}
		}
		if curly && other {
			return NoNode
		}
	}
	decl := p.newNode(NodeDeclaration, name)
	p.d.Nodes[decl].Important = important
	p.link(decl, value)
	p.d.Nodes[decl].Start = p.d.Nodes[ids[0]].Start
	if e := p.d.Nodes[ids[len(ids)-1]].End; e > p.d.Nodes[decl].End {
		p.d.Nodes[decl].End = e
	}
	return decl
}

func hasPrefix(s, pre string) bool { return len(s) >= len(pre) && s[:len(pre)] == pre }

// consumeBlockContents is the current ED's "consume a block's contents":
// declarations, at-rules and nested qualified rules.
func (p *parser) consumeBlockContents(root NodeID) {
	for {
		i := p.peek()
		switch p.kind(i) {
		case TokenInvalid:
			return
		case TokenWhitespace, TokenSemicolon:
			p.pos++
		case TokenCloseCurly:
			// Only reachable when parsing a source text directly: a stray
			// "}" is discarded so the loop always advances.
			p.pos++
			p.appendChild(root, p.errorNode(DiagUnexpectedClose, []NodeID{p.newNode(NodePreserved, i)}))
		case TokenAtKeyword:
			p.appendChild(root, p.consumeAtRule(true))
		default:
			if p.kind(i) == TokenIdent {
				save, nodes, diags := p.pos, len(p.d.Nodes), len(p.d.Diags)
				mark := len(p.scratch)
				p.collectUntilSemicolon(true)
				decl := p.declarationFrom(p.scratch[mark:], true)
				p.scratch = p.scratch[:mark]
				if decl != NoNode {
					p.appendChild(root, decl)
					continue
				}
				// Restore the mark: drop the nodes and diagnostics of the
				// failed attempt and re-read the tokens as a rule.
				p.pos, p.d.Nodes, p.d.Diags = save, p.d.Nodes[:nodes], p.d.Diags[:diags]
			}
			p.appendChild(root, p.consumeQualifiedRule(true, true))
		}
	}
}

func (p *parser) rootNode(kind NodeKind) NodeID {
	r := p.newNode(kind, -1)
	p.d.Nodes[r].Start = 0
	p.d.Nodes[r].End = int32(len(p.d.Src))
	p.d.Root = r
	return r
}

// ParseStylesheet is §5.3.3 "Parse a stylesheet". Rule blocks are kept as
// raw {} blocks; ParseBlockContents parses one on demand.
func ParseStylesheet(src []byte, opts Options) *Document {
	d, p := newDocument(src, opts)
	p.consumeListOfRules(p.rootNode(NodeStylesheet), true)
	return d
}

// ParseRuleList is §5.3.4 "Parse a list of rules" (CDO/CDC not skipped).
func ParseRuleList(src []byte, opts Options) *Document {
	d, p := newDocument(src, opts)
	p.consumeListOfRules(p.rootNode(NodeRuleList), false)
	return d
}

// ParseDeclarationList is §5.3.8 "Parse a list of declarations" (2021 CR,
// as in a style attribute).
func ParseDeclarationList(src []byte, opts Options) *Document {
	d, p := newDocument(src, opts)
	p.consumeDeclarationList(p.rootNode(NodeDeclarationList))
	return d
}

// ParseBlockContentsSource parses src with the current ED's "parse a
// block's contents" (declarations, at-rules and nested rules).
func ParseBlockContentsSource(src []byte, opts Options) *Document {
	d, p := newDocument(src, opts)
	p.consumeBlockContents(p.rootNode(NodeBlockContents))
	return d
}

// ParseComponentValues is §5.3.10 "Parse a list of component values".
func ParseComponentValues(src []byte, opts Options) *Document {
	d, p := newDocument(src, opts)
	root := p.rootNode(NodeComponentValueList)
	for p.peek() >= 0 {
		p.appendChild(root, p.consumeComponentValue())
	}
	p.d.Nodes[root].Start, p.d.Nodes[root].End = 0, int32(len(d.Src))
	return d
}

// ParseRule is §5.3.5 "Parse a rule": Root has exactly one child, the rule
// or an error node (DiagEmpty, DiagInvalidRule or DiagExtraInput).
func ParseRule(src []byte, opts Options) *Document {
	d, p := newDocument(src, opts)
	root := p.rootNode(NodeComponentValueList)
	p.skipWhitespace()
	var r NodeID
	switch p.kind(p.peek()) {
	case TokenInvalid:
		r = p.errorNode(DiagEmpty, nil)
	case TokenAtKeyword:
		r = p.consumeAtRule(false)
	default:
		r = p.consumeQualifiedRule(false, false)
	}
	p.appendChild(root, p.extraInput(r))
	return d
}

// ParseDeclaration is §5.3.6 "Parse a declaration": Root has exactly one
// child, the declaration or an error node.
func ParseDeclaration(src []byte, opts Options) *Document {
	d, p := newDocument(src, opts)
	root := p.rootNode(NodeComponentValueList)
	p.skipWhitespace()
	mark := len(p.scratch)
	empty := p.peek() < 0
	for p.peek() >= 0 {
		v := p.consumeComponentValue()
		p.scratch = append(p.scratch, v)
	}
	var r NodeID
	if empty {
		r = p.errorNode(DiagEmpty, nil)
	} else {
		r = p.declarationFrom(p.scratch[mark:], false)
	}
	p.scratch = p.scratch[:mark]
	p.appendChild(root, r)
	return d
}

// ParseComponentValue is §5.3.9 "Parse a component value": Root has
// exactly one child, the value or an error node.
func ParseComponentValue(src []byte, opts Options) *Document {
	d, p := newDocument(src, opts)
	root := p.rootNode(NodeComponentValueList)
	p.skipWhitespace()
	var r NodeID
	if p.peek() < 0 {
		r = p.errorNode(DiagEmpty, nil)
	} else {
		r = p.consumeComponentValue()
	}
	p.appendChild(root, p.extraInput(r))
	return d
}

// extraInput implements the trailing check of §5.3.5 and §5.3.9: after
// whitespace, anything left turns the result into DiagExtraInput (holding
// the result and the rest).
func (p *parser) extraInput(r NodeID) NodeID {
	p.skipWhitespace()
	if p.peek() < 0 {
		return r
	}
	mark := len(p.scratch)
	p.scratch = append(p.scratch, r)
	for p.peek() >= 0 {
		v := p.consumeComponentValue()
		p.scratch = append(p.scratch, v)
	}
	e := p.errorNode(DiagExtraInput, p.scratch[mark:])
	p.scratch = p.scratch[:mark]
	return e
}

// depth returns the number of blocks and functions enclosing (and
// including) id, following re-parse roots back to their source block.
func (d *Document) depth(id NodeID) int {
	n := 0
	for id != NoNode {
		nd := &d.Nodes[id]
		switch nd.Kind {
		case NodeSimpleBlock, NodeFunction:
			n++
			id = nd.Parent
		case NodeBlockContents:
			id = nd.Block
		default:
			id = nd.Parent
		}
	}
	return n
}

// ParseBlockContents parses the contents of the {} block node block with
// the current ED's "consume a block's contents" and returns a new
// NodeBlockContents root (its Block field names the source block). The
// new nodes are appended to d.Nodes; d.Root is unchanged. A block elided
// by the depth limit (a NodeError) yields an empty root.
func (d *Document) ParseBlockContents(block NodeID) NodeID {
	b := &d.Nodes[block]
	if b.Kind != NodeSimpleBlock {
		r := NodeID(len(d.Nodes))
		d.Nodes = append(d.Nodes, Node{Kind: NodeBlockContents, Parent: NoNode, FirstChild: NoNode,
			LastChild: NoNode, NextSibling: NoNode, Block: block, Token: -1, CloseToken: -1,
			Start: b.Start, End: b.Start})
		return r
	}
	end := b.CloseToken
	if end < 0 {
		end = int32(len(d.Tokens))
	}
	p := &parser{d: d, pos: b.Token + 1, end: end, base: d.depth(block), maxDepth: d.opts.maxDepth()}
	r := p.newNode(NodeBlockContents, -1)
	d.Nodes[r].Block = block
	d.Nodes[r].Start = d.Nodes[block].Start
	d.Nodes[r].End = d.Nodes[block].Start
	p.consumeBlockContents(r)
	return r
}
