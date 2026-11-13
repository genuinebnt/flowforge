package jsparse

// NodeID indexes Program.Nodes. NoNode (-1) means "absent" (ESTree null).
type NodeID int32

// NoNode is the absent node.
const NoNode NodeID = -1

// NodeList is a range of Program.Children.
type NodeList struct {
	Start, Len int32
}

// Kind is an ESTree node type. The names (Kind.String) are the ESTree
// "type" values, so the JSON form is comparable with acorn.
type Kind uint8

// Node kinds.
const (
	KInvalid Kind = iota
	KProgram
	KIdentifier
	KPrivateIdentifier
	KLiteral
	KExpressionStatement
	KBlockStatement
	KStaticBlock
	KEmptyStatement
	KDebuggerStatement
	KWithStatement
	KReturnStatement
	KLabeledStatement
	KBreakStatement
	KContinueStatement
	KIfStatement
	KSwitchStatement
	KSwitchCase
	KThrowStatement
	KTryStatement
	KCatchClause
	KWhileStatement
	KDoWhileStatement
	KForStatement
	KForInStatement
	KForOfStatement
	KFunctionDeclaration
	KVariableDeclaration
	KVariableDeclarator
	KThisExpression
	KArrayExpression
	KObjectExpression
	KProperty
	KFunctionExpression
	KArrowFunctionExpression
	KUnaryExpression
	KUpdateExpression
	KBinaryExpression
	KAssignmentExpression
	KLogicalExpression
	KMemberExpression
	KChainExpression
	KConditionalExpression
	KCallExpression
	KNewExpression
	KSequenceExpression
	KYieldExpression
	KAwaitExpression
	KTemplateLiteral
	KTaggedTemplateExpression
	KTemplateElement
	KObjectPattern
	KArrayPattern
	KRestElement
	KAssignmentPattern
	KClassDeclaration
	KClassExpression
	KClassBody
	KMethodDefinition
	KPropertyDefinition
	KMetaProperty
	KSuper
	KSpreadElement
	KImportDeclaration
	KImportSpecifier
	KImportDefaultSpecifier
	KImportNamespaceSpecifier
	KImportAttribute
	KExportNamedDeclaration
	KExportSpecifier
	KExportDefaultDeclaration
	KExportAllDeclaration
	KImportExpression

	kindCount
)

var kindNames = [kindCount]string{
	"Invalid", "Program", "Identifier", "PrivateIdentifier", "Literal", "ExpressionStatement",
	"BlockStatement", "StaticBlock", "EmptyStatement", "DebuggerStatement", "WithStatement",
	"ReturnStatement", "LabeledStatement", "BreakStatement", "ContinueStatement", "IfStatement",
	"SwitchStatement", "SwitchCase", "ThrowStatement", "TryStatement", "CatchClause", "WhileStatement",
	"DoWhileStatement", "ForStatement", "ForInStatement", "ForOfStatement", "FunctionDeclaration",
	"VariableDeclaration", "VariableDeclarator", "ThisExpression", "ArrayExpression", "ObjectExpression",
	"Property", "FunctionExpression", "ArrowFunctionExpression", "UnaryExpression", "UpdateExpression",
	"BinaryExpression", "AssignmentExpression", "LogicalExpression", "MemberExpression", "ChainExpression",
	"ConditionalExpression", "CallExpression", "NewExpression", "SequenceExpression", "YieldExpression",
	"AwaitExpression", "TemplateLiteral", "TaggedTemplateExpression", "TemplateElement", "ObjectPattern",
	"ArrayPattern", "RestElement", "AssignmentPattern", "ClassDeclaration", "ClassExpression", "ClassBody",
	"MethodDefinition", "PropertyDefinition", "MetaProperty", "Super", "SpreadElement", "ImportDeclaration",
	"ImportSpecifier", "ImportDefaultSpecifier", "ImportNamespaceSpecifier", "ImportAttribute",
	"ExportNamedDeclaration", "ExportSpecifier", "ExportDefaultDeclaration", "ExportAllDeclaration",
	"ImportExpression",
}

// String returns the ESTree type name.
func (k Kind) String() string {
	if k < kindCount {
		return kindNames[k]
	}
	return "Invalid"
}

// Op is the operator or sub-kind of a node: the operator of unary,
// update, binary, logical and assignment expressions; the kind of a
// variable declaration, property, method or literal.
type Op uint8

// Operators and sub-kinds.
const (
	OpNone Op = iota
	// Binary.
	OpEq         // ==
	OpNotEq      // !=
	OpStrictEq   // ===
	OpStrictNe   // !==
	OpLt         // <
	OpLe         // <=
	OpGt         // >
	OpGe         // >=
	OpShl        // <<
	OpShr        // >>
	OpUShr       // >>>
	OpAdd        // +
	OpSub        // -
	OpMul        // *
	OpDiv        // /
	OpMod        // %
	OpExp        // **
	OpBitOr      // |
	OpBitXor     // ^
	OpBitAnd     // &
	OpIn         // in
	OpInstanceof // instanceof
	// Logical.
	OpOr       // ||
	OpAnd      // &&
	OpCoalesce // ??
	// Unary.
	OpNeg    // -
	OpPos    // +
	OpNot    // !
	OpBitNot // ~
	OpTypeof // typeof
	OpVoid   // void
	OpDelete // delete
	// Update.
	OpInc // ++
	OpDec // --
	// Assignment.
	OpAssign         // =
	OpAddAssign      // +=
	OpSubAssign      // -=
	OpMulAssign      // *=
	OpDivAssign      // /=
	OpModAssign      // %=
	OpExpAssign      // **=
	OpShlAssign      // <<=
	OpShrAssign      // >>=
	OpUShrAssign     // >>>=
	OpBitOrAssign    // |=
	OpBitXorAssign   // ^=
	OpBitAndAssign   // &=
	OpOrAssign       // ||=
	OpAndAssign      // &&=
	OpCoalesceAssign // ??=
	// VariableDeclaration.kind.
	OpVar
	OpLet
	OpConst
	// Property.kind and MethodDefinition.kind.
	OpInit
	OpGet
	OpSet
	OpMethod
	OpConstructor
	// Literal sub-kinds.
	LitString
	LitNumber
	LitBoolean
	LitNull
	LitRegExp
	LitBigInt

	opCount
)

var opNames = [opCount]string{
	"", "==", "!=", "===", "!==", "<", "<=", ">", ">=", "<<", ">>", ">>>", "+", "-", "*", "/", "%", "**",
	"|", "^", "&", "in", "instanceof", "||", "&&", "??", "-", "+", "!", "~", "typeof", "void", "delete",
	"++", "--", "=", "+=", "-=", "*=", "/=", "%=", "**=", "<<=", ">>=", ">>>=", "|=", "^=", "&=", "||=",
	"&&=", "??=", "var", "let", "const", "init", "get", "set", "method", "constructor",
	"string", "number", "boolean", "null", "regexp", "bigint",
}

// String returns the operator text or sub-kind name used in ESTree.
func (o Op) String() string {
	if o < opCount {
		return opNames[o]
	}
	return ""
}

// NodeFlags are boolean ESTree fields plus parser-internal facts.
type NodeFlags uint32

// Node flags.
const (
	NComputed      NodeFlags = 1 << iota // computed
	NOptional                            // optional (member/call in a chain)
	NShorthand                           // shorthand property
	NMethod                              // method property
	NStatic                              // static class member
	NAsync                               // async function
	NGenerator                           // generator function
	NExpression                          // arrow with expression body
	NPrefix                              // prefix update
	NDelegate                            // yield*
	NAwait                               // for await
	NTail                                // last template element
	NCookedNull                          // template element cooked value is undefined
	NDirective                           // expression statement is a directive (Str holds its raw text)
	NParenthesized                       // written in parentheses (not part of ESTree; printer and cover grammar use it)
	NStrict                              // function or program body is strict mode code
	NTrailingComma                       // spread/rest element followed by a comma (internal; invalid as a rest)
)

// Node is one AST node: 64 bytes and pointer-free (1+1 bytes of kind and
// op, 2 of padding, then 4-byte fields and the 8-byte Num), so the arena
// is one allocation the garbage collector does not scan. Which slots a
// kind uses is defined by the schema table in astjson.go (schemas), which
// also names each slot's ESTree field. Unused NodeID slots are NoNode.
type Node struct {
	Kind  Kind
	Op    Op        // operator or sub-kind (see Op)
	Flags NodeFlags // N* flags
	Span  Span      // source bytes, parentheses excluded
	// A, B, C, D, List and List2 are the child slots; schemas names the
	// ESTree field each one is for a given Kind.
	A     NodeID
	B     NodeID
	C     NodeID
	D     NodeID
	List  NodeList
	List2 NodeList
	// Str is the identifier name, string value, template cooked value,
	// regular expression pattern, BigInt text (prefix kept, separators and
	// the n suffix removed) or directive raw text, as an index into
	// Program.Strings.
	Str StrID
	// Str2 is the template raw value or the regular expression flags.
	Str2 StrID
	// Num is the value of a numeric literal (1 or 0 for booleans).
	Num float64
}

// StrID indexes Program.Strings; 0 is the empty string. Keeping strings
// out of Node makes the arena pointer-free, so the garbage collector never
// scans it (PORT: Rust keeps the same layout with a Vec<String> or an
// atom table).
type StrID uint32

// Has reports whether all of f are set.
func (n *Node) Has(f NodeFlags) bool { return n.Flags&f == f }

// SourceType says whether a Program is a Script or a Module.
type SourceType uint8

// Source types.
const (
	SourceScript SourceType = iota
	SourceModule
)

// String returns "script" or "module" (the ESTree sourceType).
func (s SourceType) String() string {
	if s == SourceModule {
		return "module"
	}
	return "script"
}

// Program is a parsed source text: an arena of nodes plus the children
// slice that NodeList ranges index.
type Program struct {
	Nodes    []Node
	Children []NodeID // NodeList ranges index this slice
	// Strings holds the node strings (Node.Str, Node.Str2); Strings[0] is "".
	Strings    []string
	Root       NodeID // the Program node (NoNode only if parsing produced nothing)
	SourceType SourceType
	// Source is the parsed text (a copy of the input bytes).
	Source string
	// Diagnostics holds every reported problem (tolerant mode), or the
	// first one (strict mode, together with the returned error).
	Diagnostics []Diagnostic
	// Comments are all comments, in source order.
	Comments []Comment
	// Tokens holds every input element, trivia included, in source order
	// when Options.Tokens is set.
	Tokens []Token

	lineStarts []int32
}

// Node returns the node with the given id; id must be valid.
func (p *Program) Node(id NodeID) *Node { return &p.Nodes[id] }

// Text returns the Str of node id (its name or string value).
func (p *Program) Text(id NodeID) string { return p.Strings[p.Nodes[id].Str] }

// Text2 returns the Str2 of node id (template raw value, regexp flags).
func (p *Program) Text2(id NodeID) string { return p.Strings[p.Nodes[id].Str2] }

// List returns the ids of a node list.
func (p *Program) List(l NodeList) []NodeID {
	return p.Children[l.Start : l.Start+l.Len]
}

// Position returns the 1-based line and 0-based column (in bytes) of a
// byte offset. Line terminators are LF, CR, CRLF, LS and PS. The line
// table is built on the first call and cached in the Program, so
// concurrent first calls on one Program race; call it once before
// sharing the Program between goroutines.
func (p *Program) Position(off int32) (line, col int) {
	if p.lineStarts == nil {
		p.lineStarts = append(p.lineStarts, 0)
		s := p.Source
		for i := 0; i < len(s); i++ {
			switch c := s[i]; {
			case c == '\n':
				p.lineStarts = append(p.lineStarts, int32(i+1))
			case c == '\r':
				if i+1 < len(s) && s[i+1] == '\n' {
					i++
				}
				p.lineStarts = append(p.lineStarts, int32(i+1))
			case c == 0xE2 && i+2 < len(s) && s[i+1] == 0x80 && (s[i+2] == 0xA8 || s[i+2] == 0xA9):
				p.lineStarts = append(p.lineStarts, int32(i+3))
				i += 2
			}
		}
	}
	lo, hi := 0, len(p.lineStarts)
	for lo+1 < hi {
		mid := (lo + hi) / 2
		if p.lineStarts[mid] <= off {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo + 1, int(off - p.lineStarts[lo])
}

// ChildIDs appends the direct children of id to dst in ESTree field
// order (absent children and array holes skipped) and returns it.
func (p *Program) ChildIDs(dst []NodeID, id NodeID) []NodeID {
	n := &p.Nodes[id]
	for _, f := range schemas[n.Kind] {
		switch f.kind {
		case fNode:
			if c := slot(n, f.slot); c != NoNode {
				dst = append(dst, c)
			}
		case fList:
			l := n.List
			if f.slot == 1 {
				l = n.List2
			}
			for _, c := range p.List(l) {
				if c != NoNode {
					dst = append(dst, c)
				}
			}
		}
	}
	return dst
}

// slot returns NodeID slot s (0-3: A-D) of n.
func slot(n *Node, s uint8) NodeID {
	switch s {
	case 0:
		return n.A
	case 1:
		return n.B
	case 2:
		return n.C
	default:
		return n.D
	}
}

// Walk calls fn for id and every descendant in pre-order (children in
// ESTree field order). If fn returns false the subtree is skipped. The
// traversal uses an explicit stack, so depth is not limited by the Go
// stack.
func (p *Program) Walk(id NodeID, fn func(id NodeID) bool) {
	if id == NoNode {
		return
	}
	stack := []NodeID{id}
	var kids []NodeID
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !fn(cur) {
			continue
		}
		kids = p.ChildIDs(kids[:0], cur)
		for i := len(kids) - 1; i >= 0; i-- {
			stack = append(stack, kids[i])
		}
	}
}
