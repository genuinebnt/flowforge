package jsparse

import (
	"strconv"
	"strings"
)

// Extraction helpers for the content package (V2_DESIGN §5 feature
// extraction). They are syntactic and scope-free: they report what the
// source spells, never what it evaluates to. Classification (is this an
// API endpoint? a route table?) belongs to the caller.

// StringKind says how a string-valued node was written.
type StringKind uint8

// String kinds.
const (
	StringLiteral    StringKind = iota // "..." or '...'
	TemplateLiteral                    // `...${x}...`, untagged
	TaggedTemplate                     // tag`...`
)

// StringUse is one string or template literal with its syntactic context.
type StringUse struct {
	Node NodeID
	Kind StringKind
	Span Span
	// Value is the cooked string. For templates the substitutions are
	// replaced by ExprPlaceholder; Parts holds the cooked quasis (an
	// undefined cooked part, possible in tagged templates, is "").
	Value string
	Parts []string
	// CalleePath is the callee of the call or `new` that has this literal
	// in an argument (directly, inside an object/array literal, a
	// conditional or a string concatenation), or the tag of a tagged
	// template: "fetch", "axios.get", "$.ajax", "xhr.open". Empty if none.
	CalleePath string
	// ReceiverType rewrites the first segment of CalleePath when it is a
	// variable initialized with `new C(...)` anywhere in the program
	// (flow-insensitive): `var x = new XMLHttpRequest(); x.open(...)`
	// gives "XMLHttpRequest.open". Otherwise it equals CalleePath.
	ReceiverType string
	// ArgIndex is the position of the argument that contains the literal,
	// directly or nested (-1 when not inside an argument).
	ArgIndex int
	// PropertyKey is the key when the literal is a property value
	// ({url: "/api"}), possibly inside an argument.
	PropertyKey string
}

// ExprPlaceholder stands for a template substitution in StringUse.Value.
const ExprPlaceholder = "${}"

// extractor holds the parent links needed by the helpers.
type extractor struct {
	p      *Program
	parent []NodeID
	ctor   map[string]string // variable name → constructor name (`new C`)
}

func newExtractor(p *Program) *extractor {
	e := &extractor{p: p, parent: make([]NodeID, len(p.Nodes)), ctor: map[string]string{}}
	for i := range e.parent {
		e.parent[i] = NoNode
	}
	var kids []NodeID
	for i := range p.Nodes {
		kids = p.ChildIDs(kids[:0], NodeID(i))
		for _, c := range kids {
			e.parent[c] = NodeID(i)
		}
		n := &p.Nodes[i]
		if n.Kind == KVariableDeclarator && n.A != NoNode && n.B != NoNode {
			id, init := &p.Nodes[n.A], &p.Nodes[n.B]
			if id.Kind == KIdentifier && init.Kind == KNewExpression {
				if path := e.calleePath(init.A); path != "" {
					e.ctor[id.Str] = path
				}
			}
		}
	}
	return e
}

// StringLiterals returns every string and template literal in source
// order with its call context. Property keys, computed member names and
// module specifiers (see ModuleRefs) are not included.
func (p *Program) StringLiterals() []StringUse {
	e := newExtractor(p)
	var out []StringUse
	p.Walk(p.Root, func(id NodeID) bool {
		n := &p.Nodes[id]
		switch n.Kind {
		case KLiteral:
			if n.Op != LitString || e.isKeyOrSpecifier(id) {
				return true
			}
			u := StringUse{Node: id, Kind: StringLiteral, Span: n.Span, Value: n.Str}
			e.context(&u, id)
			out = append(out, u)
		case KTemplateLiteral:
			u := StringUse{Node: id, Kind: TemplateLiteral, Span: n.Span}
			anchor := id
			if par := e.parent[id]; par != NoNode && p.Nodes[par].Kind == KTaggedTemplateExpression {
				u.Kind = TaggedTemplate
				anchor = par
			}
			var b strings.Builder
			for i, q := range p.List(n.List) {
				if i > 0 {
					b.WriteString(ExprPlaceholder)
				}
				b.WriteString(p.Nodes[q].Str)
				u.Parts = append(u.Parts, p.Nodes[q].Str)
			}
			u.Value = b.String()
			if u.Kind == TaggedTemplate {
				e.setCallee(&u, e.p.Nodes[anchor].A)
				u.ArgIndex = -1
			} else {
				e.context(&u, anchor)
			}
			out = append(out, u)
		}
		return true
	})
	return out
}

// isKeyOrSpecifier reports literals that are property keys, import
// attribute parts or module specifiers rather than values.
func (e *extractor) isKeyOrSpecifier(id NodeID) bool {
	par := e.parent[id]
	if par == NoNode {
		return false
	}
	pn := &e.p.Nodes[par]
	switch pn.Kind {
	case KProperty, KMethodDefinition, KPropertyDefinition:
		return pn.A == id && pn.Flags&NComputed == 0
	case KMemberExpression:
		return pn.B == id // obj["name"] is an access, reported via CalleePath
	case KImportDeclaration, KExportAllDeclaration, KExportNamedDeclaration, KImportAttribute,
		KImportSpecifier, KExportSpecifier:
		return true
	}
	return false
}

// context fills CalleePath, ArgIndex and PropertyKey by walking up from id
// through object/array literals and properties to an enclosing call.
func (e *extractor) context(u *StringUse, id NodeID) {
	u.ArgIndex = -1
	cur := id
	for depth := 0; depth < 8; depth++ {
		par := e.parent[cur]
		if par == NoNode {
			return
		}
		pn := &e.p.Nodes[par]
		switch pn.Kind {
		case KProperty:
			if pn.B == cur && u.PropertyKey == "" {
				u.PropertyKey = e.keyName(pn.A, pn.Flags&NComputed != 0)
			}
		case KObjectExpression, KArrayExpression, KSpreadElement, KConditionalExpression, KLogicalExpression:
		case KBinaryExpression:
			if pn.Op != OpAdd { // string concatenation keeps the context
				return
			}
		case KCallExpression, KNewExpression:
			if pn.A == cur {
				return // the literal is (part of) the callee
			}
			for i, a := range e.p.List(pn.List) {
				if a == cur {
					u.ArgIndex = i
				}
			}
			e.setCallee(u, pn.A)
			return
		default:
			return
		}
		cur = par
	}
}

// setCallee records the callee path and its constructor-resolved form.
func (e *extractor) setCallee(u *StringUse, callee NodeID) {
	u.CalleePath = e.calleePath(callee)
	u.ReceiverType = u.CalleePath
	if first, rest, ok := strings.Cut(u.CalleePath, "."); ok {
		if c, ok := e.ctor[first]; ok {
			u.ReceiverType = c + "." + rest
		}
	}
}

func (e *extractor) keyName(key NodeID, computed bool) string {
	k := &e.p.Nodes[key]
	if computed {
		if k.Kind == KLiteral && k.Op == LitString {
			return k.Str
		}
		return "[]"
	}
	switch k.Kind {
	case KIdentifier, KPrivateIdentifier:
		return k.Str
	case KLiteral:
		if k.Op == LitNumber {
			return NumberToString(k.Num)
		}
		return k.Str
	}
	return ""
}

// calleePath renders a callee as a dotted path: identifiers, `this`,
// member names (computed string keys included), `new C()` receivers as C,
// and "?" for anything else.
func (e *extractor) calleePath(id NodeID) string {
	var parts []string
	for depth := 0; depth < 16 && id != NoNode; depth++ {
		n := &e.p.Nodes[id]
		switch n.Kind {
		case KIdentifier:
			parts = append(parts, n.Str)
			id = NoNode
		case KThisExpression:
			parts = append(parts, "this")
			id = NoNode
		case KSuper:
			parts = append(parts, "super")
			id = NoNode
		case KMemberExpression:
			parts = append(parts, e.keyName(n.B, n.Flags&NComputed != 0))
			id = n.A
		case KChainExpression:
			id = n.A
		case KNewExpression:
			id = n.A
		case KCallExpression:
			parts = append(parts, "()")
			id = n.A
		case KImportExpression:
			parts = append(parts, "import()")
			id = NoNode
		default:
			parts = append(parts, "?")
			id = NoNode
		}
	}
	var b strings.Builder
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == "()" {
			b.WriteString("()")
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(parts[i])
	}
	return b.String()
}

// ModuleRef is a module specifier the program loads.
type ModuleRef struct {
	Node NodeID
	Span Span
	// Specifier is the specifier text; for import() with a template
	// literal the substitutions are ExprPlaceholder.
	Specifier string
	// Dynamic is true for import(); Static for import/export ... from.
	Dynamic bool
	// Literal is false when an import() argument is not a string or
	// template literal (Specifier is then empty).
	Literal bool
}

// ModuleRefs returns static import/export sources and dynamic import()
// specifiers in source order.
func (p *Program) ModuleRefs() []ModuleRef {
	var out []ModuleRef
	p.Walk(p.Root, func(id NodeID) bool {
		n := &p.Nodes[id]
		switch n.Kind {
		case KImportDeclaration:
			out = append(out, p.moduleRef(n.A, false))
		case KExportAllDeclaration, KExportNamedDeclaration:
			if n.B != NoNode {
				out = append(out, p.moduleRef(n.B, false))
			}
		case KImportExpression:
			r := p.moduleRef(n.A, true)
			r.Node = id
			out = append(out, r)
		}
		return true
	})
	return out
}

func (p *Program) moduleRef(src NodeID, dynamic bool) ModuleRef {
	r := ModuleRef{Node: src, Dynamic: dynamic}
	if src == NoNode {
		return r
	}
	n := &p.Nodes[src]
	r.Span = n.Span
	switch {
	case n.Kind == KLiteral && n.Op == LitString:
		r.Specifier, r.Literal = n.Str, true
	case n.Kind == KTemplateLiteral:
		var b strings.Builder
		for i, q := range p.List(n.List) {
			if i > 0 {
				b.WriteString(ExprPlaceholder)
			}
			b.WriteString(p.Nodes[q].Str)
		}
		r.Specifier, r.Literal = b.String(), true
	}
	return r
}

// SourceMappingURL returns the URL of the last `//# sourceMappingURL=`
// (or legacy `//@`, or the /*# ... */ form) comment, as source maps
// specify (ECMA-426 §11.1.2.1, "extracting a source map URL from
// JavaScript"): only a comment after which nothing but white space,
// line terminators and other comments follows counts.
func (p *Program) SourceMappingURL() (string, Span, bool) {
	last := int32(0)
	for _, t := range p.Tokens {
		if !t.Kind.IsTrivia() {
			last = t.End
		}
	}
	if p.Tokens == nil {
		// Without recorded tokens, the end of the last statement bounds it.
		if p.Root != NoNode && p.Nodes[p.Root].List.Len > 0 {
			body := p.List(p.Nodes[p.Root].List)
			last = p.Nodes[body[len(body)-1]].Span.End
		}
	}
	for i := len(p.Comments) - 1; i >= 0; i-- {
		c := p.Comments[i]
		if c.Span.Start < last {
			break
		}
		text := p.Source[c.Span.Start:c.Span.End]
		var body string
		switch c.Kind {
		case TLineComment:
			body = text[2:]
		case TBlockComment:
			body = strings.TrimSuffix(text[2:], "*/")
		default:
			continue
		}
		if len(body) < 1 || (body[0] != '#' && body[0] != '@') {
			continue
		}
		rest := strings.TrimLeft(body[1:], " \t")
		v, ok := strings.CutPrefix(rest, "sourceMappingURL=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if end := strings.IndexAny(v, " \t\"'"); end >= 0 {
			v = v[:end]
		}
		if v == "" {
			continue
		}
		return v, c.Span, true
	}
	return "", Span{}, false
}

// ValueKind classifies a literal-table value.
type ValueKind uint8

// Value kinds.
const (
	ValueOther ValueKind = iota
	ValueString
	ValueTemplate
	ValueNumber
	ValueBool
	ValueNull
	ValueIdentifier // a reference, e.g. a component name
	ValueFunction   // function, arrow or method
	ValueCall       // e.g. () => import("./x") bodies, lazy(...)
	ValueRegExp
)

// TableEntry is one leaf of a literal table.
type TableEntry struct {
	// Path locates the leaf: ".key", "[3]", nested as "[0].children[1].path".
	Path  string
	Kind  ValueKind
	Value string // string/template/number/bool text, identifier or callee path
	Node  NodeID
}

// LiteralTable is an outermost object or array literal flattened into its
// leaves.
type LiteralTable struct {
	Node NodeID
	Span Span
	// Context names where the literal goes: the declared variable,
	// assignment target, property key or callee path (+ "#arg<n>").
	Context string
	Entries []TableEntry
	// Truncated is set when the table had more than maxEntries leaves.
	Truncated bool
}

// LiteralTables returns every object or array literal that is not nested
// in another literal and has at least minEntries leaves (each with at
// most maxEntries leaves recorded). Route tables (`[{path: "/a",
// component: A}, ...]`, `{"/users/:id": handler}`) are among them; the
// content package classifies them by key names and value shapes.
func (p *Program) LiteralTables(minEntries, maxEntries int) []LiteralTable {
	e := newExtractor(p)
	var out []LiteralTable
	p.Walk(p.Root, func(id NodeID) bool {
		n := &p.Nodes[id]
		if n.Kind != KObjectExpression && n.Kind != KArrayExpression {
			return true
		}
		t := LiteralTable{Node: id, Span: n.Span, Context: e.tableContext(id)}
		e.flatten(&t, id, "", maxEntries, 0)
		if len(t.Entries) >= minEntries {
			out = append(out, t)
		}
		return false // nested literals belong to this table
	})
	return out
}

func (e *extractor) flatten(t *LiteralTable, id NodeID, path string, max, depth int) {
	n := &e.p.Nodes[id]
	add := func(p string, k ValueKind, v string, node NodeID) {
		if len(t.Entries) >= max {
			t.Truncated = true
			return
		}
		t.Entries = append(t.Entries, TableEntry{Path: p, Kind: k, Value: v, Node: node})
	}
	if depth > 32 {
		t.Truncated = true
		return
	}
	switch n.Kind {
	case KObjectExpression:
		for _, pr := range e.p.List(n.List) {
			pn := &e.p.Nodes[pr]
			if pn.Kind != KProperty {
				add(path+"...", ValueOther, "", pr)
				continue
			}
			key := e.keyName(pn.A, pn.Flags&NComputed != 0)
			sub := path + "." + key
			if pn.Flags&NMethod != 0 || pn.Op != OpInit {
				add(sub, ValueFunction, "", pn.B)
				continue
			}
			e.flatten(t, pn.B, sub, max, depth+1)
		}
	case KArrayExpression:
		for i, el := range e.p.List(n.List) {
			if el == NoNode {
				continue
			}
			e.flatten(t, el, path+"["+strconv.Itoa(i)+"]", max, depth+1)
		}
	case KLiteral:
		switch n.Op {
		case LitString:
			add(path, ValueString, n.Str, id)
		case LitNumber:
			add(path, ValueNumber, NumberToString(n.Num), id)
		case LitBoolean:
			add(path, ValueBool, strconv.FormatBool(n.Num != 0), id)
		case LitNull:
			add(path, ValueNull, "null", id)
		case LitRegExp:
			add(path, ValueRegExp, "/"+n.Str+"/"+n.Str2, id)
		default:
			add(path, ValueOther, n.Str, id)
		}
	case KTemplateLiteral:
		var b strings.Builder
		for i, q := range e.p.List(n.List) {
			if i > 0 {
				b.WriteString(ExprPlaceholder)
			}
			b.WriteString(e.p.Nodes[q].Str)
		}
		add(path, ValueTemplate, b.String(), id)
	case KIdentifier, KMemberExpression:
		add(path, ValueIdentifier, e.calleePath(id), id)
	case KFunctionExpression, KArrowFunctionExpression, KClassExpression:
		add(path, ValueFunction, "", id)
	case KCallExpression, KNewExpression, KImportExpression:
		add(path, ValueCall, e.calleePath(id), id)
	default:
		add(path, ValueOther, "", id)
	}
}

// tableContext names where a literal is stored or passed.
func (e *extractor) tableContext(id NodeID) string {
	par := e.parent[id]
	if par == NoNode {
		return ""
	}
	pn := &e.p.Nodes[par]
	switch pn.Kind {
	case KVariableDeclarator:
		if pn.A != NoNode && e.p.Nodes[pn.A].Kind == KIdentifier {
			return e.p.Nodes[pn.A].Str
		}
	case KAssignmentExpression:
		return e.calleePath(pn.A)
	case KCallExpression, KNewExpression:
		for i, a := range e.p.List(pn.List) {
			if a == id {
				return e.calleePath(pn.A) + "#arg" + strconv.Itoa(i)
			}
		}
	case KExportDefaultDeclaration:
		return "export default"
	case KReturnStatement:
		return "return"
	}
	return ""
}
