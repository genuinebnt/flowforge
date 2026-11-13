package jsparse

import (
	"math"
	"sort"
	"strconv"
)

// fieldKind says how a schema field is stored and serialized.
type fieldKind uint8

const (
	fNode  fieldKind = iota // NodeID slot (null when NoNode)
	fList                   // NodeList (slot 0 = List, 1 = List2); holes are null
	fFlag                   // boolean from NodeFlags
	fOp                     // Op.String()
	fFalse                  // constant false
	fNull                   // constant null
)

// field is one ESTree property of a node kind.
type field struct {
	name string
	kind fieldKind
	slot uint8
	flag NodeFlags
	// ifB emits the field only when slot B is present (acorn writes
	// ExportNamedDeclaration.attributes only for re-exports).
	ifB bool
}

func nd(name string, s uint8) field     { return field{name: name, kind: fNode, slot: s} }
func ls(name string, s uint8) field     { return field{name: name, kind: fList, slot: s} }
func fl(name string, f NodeFlags) field { return field{name: name, kind: fFlag, flag: f} }
func op(name string) field              { return field{name: name, kind: fOp} }
func constFalse(name string) field      { return field{name: name, kind: fFalse} }
func constNull(name string) field       { return field{name: name, kind: fNull} }
func fnFields(withID bool, arrow bool) []field {
	f := []field{nd("id", 0), ls("params", 0), nd("body", 1), fl("async", NAsync), fl("expression", NExpression)}
	if arrow {
		f[0] = constNull("id")
		return append(f, constFalse("generator"))
	}
	_ = withID
	return append(f, fl("generator", NGenerator))
}

// schemas maps each kind to its ESTree fields in ESTree order. It is the
// single source of truth for slot meaning: the JSON writer, ChildIDs, the
// printer and the arena-integrity invariant all read it. Literal,
// TemplateElement, ExpressionStatement.directive and
// Program.sourceType are serialized specially (see writeNode).
var schemas = [kindCount][]field{
	KProgram:                  {ls("body", 0)},
	KIdentifier:               {},
	KPrivateIdentifier:        {},
	KLiteral:                  {},
	KExpressionStatement:      {nd("expression", 0)},
	KBlockStatement:           {ls("body", 0)},
	KStaticBlock:              {ls("body", 0)},
	KEmptyStatement:           {},
	KDebuggerStatement:        {},
	KWithStatement:            {nd("object", 0), nd("body", 1)},
	KReturnStatement:          {nd("argument", 0)},
	KLabeledStatement:         {nd("label", 0), nd("body", 1)},
	KBreakStatement:           {nd("label", 0)},
	KContinueStatement:        {nd("label", 0)},
	KIfStatement:              {nd("test", 0), nd("consequent", 1), nd("alternate", 2)},
	KSwitchStatement:          {nd("discriminant", 0), ls("cases", 0)},
	KSwitchCase:               {nd("test", 0), ls("consequent", 0)},
	KThrowStatement:           {nd("argument", 0)},
	KTryStatement:             {nd("block", 0), nd("handler", 1), nd("finalizer", 2)},
	KCatchClause:              {nd("param", 0), nd("body", 1)},
	KWhileStatement:           {nd("test", 0), nd("body", 1)},
	KDoWhileStatement:         {nd("body", 0), nd("test", 1)},
	KForStatement:             {nd("init", 0), nd("test", 1), nd("update", 2), nd("body", 3)},
	KForInStatement:           {nd("left", 0), nd("right", 1), nd("body", 2)},
	KForOfStatement:           {nd("left", 0), nd("right", 1), nd("body", 2), fl("await", NAwait)},
	KFunctionDeclaration:      fnFields(true, false),
	KVariableDeclaration:      {ls("declarations", 0), op("kind")},
	KVariableDeclarator:       {nd("id", 0), nd("init", 1)},
	KThisExpression:           {},
	KArrayExpression:          {ls("elements", 0)},
	KObjectExpression:         {ls("properties", 0)},
	KProperty:                 {nd("key", 0), nd("value", 1), op("kind"), fl("method", NMethod), fl("shorthand", NShorthand), fl("computed", NComputed)},
	KFunctionExpression:       fnFields(true, false),
	KArrowFunctionExpression:  fnFields(false, true),
	KUnaryExpression:          {op("operator"), field{name: "prefix", kind: fFlag, flag: 0}, nd("argument", 0)},
	KUpdateExpression:         {op("operator"), fl("prefix", NPrefix), nd("argument", 0)},
	KBinaryExpression:         {op("operator"), nd("left", 0), nd("right", 1)},
	KAssignmentExpression:     {op("operator"), nd("left", 0), nd("right", 1)},
	KLogicalExpression:        {op("operator"), nd("left", 0), nd("right", 1)},
	KMemberExpression:         {nd("object", 0), nd("property", 1), fl("computed", NComputed), fl("optional", NOptional)},
	KChainExpression:          {nd("expression", 0)},
	KConditionalExpression:    {nd("test", 0), nd("consequent", 1), nd("alternate", 2)},
	KCallExpression:           {nd("callee", 0), ls("arguments", 0), fl("optional", NOptional)},
	KNewExpression:            {nd("callee", 0), ls("arguments", 0)},
	KSequenceExpression:       {ls("expressions", 0)},
	KYieldExpression:          {nd("argument", 0), fl("delegate", NDelegate)},
	KAwaitExpression:          {nd("argument", 0)},
	KTemplateLiteral:          {ls("quasis", 0), ls("expressions", 1)},
	KTaggedTemplateExpression: {nd("tag", 0), nd("quasi", 1)},
	KTemplateElement:          {fl("tail", NTail)},
	KObjectPattern:            {ls("properties", 0)},
	KArrayPattern:             {ls("elements", 0)},
	KRestElement:              {nd("argument", 0)},
	KAssignmentPattern:        {nd("left", 0), nd("right", 1)},
	KClassDeclaration:         {nd("id", 0), nd("superClass", 1), nd("body", 2)},
	KClassExpression:          {nd("id", 0), nd("superClass", 1), nd("body", 2)},
	KClassBody:                {ls("body", 0)},
	KMethodDefinition:         {nd("key", 0), nd("value", 1), op("kind"), fl("computed", NComputed), fl("static", NStatic)},
	KPropertyDefinition:       {nd("key", 0), nd("value", 1), fl("computed", NComputed), fl("static", NStatic)},
	KMetaProperty:             {nd("meta", 0), nd("property", 1)},
	KSuper:                    {},
	KSpreadElement:            {nd("argument", 0)},
	KImportDeclaration:        {ls("specifiers", 0), nd("source", 0), ls("attributes", 1)},
	KImportSpecifier:          {nd("imported", 0), nd("local", 1)},
	KImportDefaultSpecifier:   {nd("local", 0)},
	KImportNamespaceSpecifier: {nd("local", 0)},
	KImportAttribute:          {nd("key", 0), nd("value", 1)},
	KExportNamedDeclaration:   {nd("declaration", 0), ls("specifiers", 0), nd("source", 1), field{name: "attributes", kind: fList, slot: 1, ifB: true}},
	KExportSpecifier:          {nd("local", 0), nd("exported", 1)},
	KExportDefaultDeclaration: {nd("declaration", 0)},
	KExportAllDeclaration:     {nd("exported", 0), nd("source", 1), ls("attributes", 1)},
	KImportExpression:         {nd("source", 0), nd("options", 1)},
}

// jsonKey is a serialized property: either a schema field or one of the
// special properties of writeNode.
type jsonKey struct {
	name string
	f    int // index into the schema, or a special key below
}

const (
	keyType = -1 - iota
	keyName
	keyValue
	keyRaw
	keyRegex
	keyBigint
	keyDirective
	keySourceType
	keyStart
	keyEnd
)

// sortedKeys holds, per kind, the serialized keys in byte order of their
// names: canonical JSON has sorted keys, like core.CanonicalJSON.
var sortedKeys = func() (out [kindCount][]jsonKey) {
	for k := Kind(0); k < kindCount; k++ {
		keys := []jsonKey{{"type", keyType}, {"start", keyStart}, {"end", keyEnd}}
		for i, f := range schemas[k] {
			keys = append(keys, jsonKey{f.name, i})
		}
		switch k {
		case KIdentifier, KPrivateIdentifier:
			keys = append(keys, jsonKey{"name", keyName})
		case KLiteral:
			keys = append(keys, jsonKey{"value", keyValue}, jsonKey{"raw", keyRaw}, jsonKey{"regex", keyRegex}, jsonKey{"bigint", keyBigint})
		case KTemplateElement:
			keys = append(keys, jsonKey{"value", keyValue})
		case KExpressionStatement:
			keys = append(keys, jsonKey{"directive", keyDirective})
		case KProgram:
			keys = append(keys, jsonKey{"sourceType", keySourceType})
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].name < keys[j].name })
		out[k] = keys
	}
	return out
}()

// JSONOptions selects optional properties of the canonical JSON form.
type JSONOptions struct {
	// Positions adds "start" and "end" (byte offsets).
	Positions bool
	// Raw adds Literal "raw" (the source text of the literal).
	Raw bool
}

// JSON returns the canonical ESTree JSON of the whole program: object keys
// sorted, no insignificant white space, positions and raw text omitted
// unless requested. Two programs have equal ASTs exactly when their JSON
// is byte-equal. The schema is documented in conformance/jsparse/README.md.
func (p *Program) JSON(opts JSONOptions) []byte {
	return p.NodeJSON(p.Root, opts)
}

// NodeJSON returns the canonical JSON of the subtree rooted at id.
func (p *Program) NodeJSON(id NodeID, opts JSONOptions) []byte {
	w := jsonWriter{p: p, opts: opts, buf: make([]byte, 0, 256)}
	w.node(id)
	return w.buf
}

type jsonWriter struct {
	p    *Program
	opts JSONOptions
	buf  []byte
}

func (w *jsonWriter) node(id NodeID) {
	if id == NoNode {
		w.buf = append(w.buf, "null"...)
		return
	}
	n := &w.p.Nodes[id]
	w.buf = append(w.buf, '{')
	first := true
	sep := func(name string) {
		if !first {
			w.buf = append(w.buf, ',')
		}
		first = false
		w.buf = appendJSONString(w.buf, name)
		w.buf = append(w.buf, ':')
	}
	schema := schemas[n.Kind]
	for _, k := range sortedKeys[n.Kind] {
		switch k.f {
		case keyType:
			sep(k.name)
			w.buf = appendJSONString(w.buf, n.Kind.String())
		case keyStart:
			if w.opts.Positions {
				sep(k.name)
				w.buf = strconv.AppendInt(w.buf, int64(n.Span.Start), 10)
			}
		case keyEnd:
			if w.opts.Positions {
				sep(k.name)
				w.buf = strconv.AppendInt(w.buf, int64(n.Span.End), 10)
			}
		case keyName:
			sep(k.name)
			w.buf = appendJSONString(w.buf, n.Str)
		case keyValue:
			sep(k.name)
			w.value(n)
		case keyRaw:
			if w.opts.Raw {
				sep(k.name)
				w.buf = appendJSONString(w.buf, w.p.Source[n.Span.Start:n.Span.End])
			}
		case keyRegex:
			if n.Op == LitRegExp {
				sep(k.name)
				w.buf = append(w.buf, `{"flags":`...)
				w.buf = appendJSONString(w.buf, n.Str2)
				w.buf = append(w.buf, `,"pattern":`...)
				w.buf = appendJSONString(w.buf, n.Str)
				w.buf = append(w.buf, '}')
			}
		case keyBigint:
			if n.Op == LitBigInt {
				sep(k.name)
				w.buf = appendJSONString(w.buf, n.Str)
			}
		case keyDirective:
			if n.Flags&NDirective != 0 {
				sep(k.name)
				w.buf = appendJSONString(w.buf, n.Str)
			}
		case keySourceType:
			sep(k.name)
			w.buf = appendJSONString(w.buf, w.p.SourceType.String())
		default:
			f := schema[k.f]
			if f.ifB && n.B == NoNode {
				continue
			}
			sep(k.name)
			switch f.kind {
			case fNode:
				w.node(slot(n, f.slot))
			case fList:
				l := n.List
				if f.slot == 1 {
					l = n.List2
				}
				w.buf = append(w.buf, '[')
				for i, c := range w.p.List(l) {
					if i > 0 {
						w.buf = append(w.buf, ',')
					}
					w.node(c)
				}
				w.buf = append(w.buf, ']')
			case fFlag:
				// A zero flag mask means "always true" (UnaryExpression.prefix).
				w.buf = strconv.AppendBool(w.buf, f.flag == 0 || n.Flags&f.flag != 0)
			case fOp:
				w.buf = appendJSONString(w.buf, n.Op.String())
			case fFalse:
				w.buf = append(w.buf, "false"...)
			case fNull:
				w.buf = append(w.buf, "null"...)
			}
		}
	}
	w.buf = append(w.buf, '}')
}

func (w *jsonWriter) value(n *Node) {
	if n.Kind == KTemplateElement {
		w.buf = append(w.buf, `{"cooked":`...)
		if n.Flags&NCookedNull != 0 {
			w.buf = append(w.buf, "null"...)
		} else {
			w.buf = appendJSONString(w.buf, n.Str)
		}
		w.buf = append(w.buf, `,"raw":`...)
		w.buf = appendJSONString(w.buf, n.Str2)
		w.buf = append(w.buf, '}')
		return
	}
	switch n.Op {
	case LitString:
		w.buf = appendJSONString(w.buf, n.Str)
	case LitNumber:
		w.buf = appendJSONNumber(w.buf, n.Num)
	case LitBoolean:
		w.buf = strconv.AppendBool(w.buf, n.Num != 0)
	default: // null, regexp and bigint values are not JSON-representable
		w.buf = append(w.buf, "null"...)
	}
}

// appendJSONNumber writes f the way JSON.stringify does (Number::toString,
// 6.1.6.1.20; non-finite values become null).
func appendJSONNumber(b []byte, f float64) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return append(b, "null"...)
	}
	return append(b, NumberToString(f)...)
}

// NumberToString implements Number::toString(x) for radix 10.
func NumberToString(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if f == 0 {
		return "0"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	sign := ""
	if f < 0 {
		sign = "-"
		f = -f
	}
	// Shortest round-tripping digits d1...dk and exponent n such that
	// f = 0.d1...dk × 10^n.
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddddde±XX
	mant, expPart := e, ""
	for i := 0; i < len(e); i++ {
		if e[i] == 'e' {
			mant, expPart = e[:i], e[i+1:]
			break
		}
	}
	digits := make([]byte, 0, len(mant))
	for i := 0; i < len(mant); i++ {
		if mant[i] != '.' {
			digits = append(digits, mant[i])
		}
	}
	x, _ := strconv.Atoi(expPart)
	n := x + 1
	k := len(digits)
	var out []byte
	switch {
	case k <= n && n <= 21:
		out = append(out, digits...)
		for i := 0; i < n-k; i++ {
			out = append(out, '0')
		}
	case 0 < n && n <= 21:
		out = append(out, digits[:n]...)
		out = append(out, '.')
		out = append(out, digits[n:]...)
	case -6 < n && n <= 0:
		out = append(out, '0', '.')
		for i := 0; i < -n; i++ {
			out = append(out, '0')
		}
		out = append(out, digits...)
	default:
		out = append(out, digits[0])
		if k > 1 {
			out = append(out, '.')
			out = append(out, digits[1:]...)
		}
		out = append(out, 'e')
		if n-1 >= 0 {
			out = append(out, '+')
		}
		out = strconv.AppendInt(out, int64(n-1), 10)
	}
	return sign + string(out)
}

// appendJSONString writes s as a JSON string like JSON.stringify: control
// characters escaped, lone surrogates (WTF-8) as \uXXXX, invalid UTF-8
// bytes as U+FFFD.
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x80 {
			switch {
			case c == '"':
				b = append(b, '\\', '"')
			case c == '\\':
				b = append(b, '\\', '\\')
			case c == '\n':
				b = append(b, '\\', 'n')
			case c == '\r':
				b = append(b, '\\', 'r')
			case c == '\t':
				b = append(b, '\\', 't')
			case c == '\b':
				b = append(b, '\\', 'b')
			case c == '\f':
				b = append(b, '\\', 'f')
			case c < 0x20:
				b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&15])
			default:
				b = append(b, c)
			}
			i++
			continue
		}
		r, n := decodeRuneSlow(s, i)
		switch {
		case r >= 0xD800 && r <= 0xDFFF:
			b = append(b, '\\', 'u', hexDigits[r>>12], hexDigits[(r>>8)&15], hexDigits[(r>>4)&15], hexDigits[r&15])
		case r == 0xFFFD && n == 1:
			b = append(b, "�"...)
		default:
			b = append(b, s[i:i+n]...)
		}
		i += n
	}
	return append(b, '"')
}

const hexDigits = "0123456789abcdef"
