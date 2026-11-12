package dsl

// This file defines the syntax tree. Parse produces it; Check annotates it
// in place with resolved types, effects and symbols (the fields documented
// as "set by Check"). The bytecode compiler lowers the annotated tree.

// Node is implemented by every syntax tree node.
type Node interface {
	// Span returns the node's source range.
	Span() Span
}

// File is one parsed source file.
type File struct {
	Name  string
	Items []Item
	// Src is the source the file was parsed from.
	Src []byte
}

// Item is a top-level declaration.
type Item interface {
	Node
	// ItemName returns the declared name (nil for use declarations).
	ItemName() *Ident
	// Annots returns the annotations written before the item.
	Annots() []*Annotation
	item()
}

// Annotation is #[name] or #[name(args)].
type Annotation struct {
	Sp        Span
	Name      *Ident
	HasParens bool
	Args      []*Arg
}

// Span implements Node.
func (a *Annotation) Span() Span { return a.Sp }

// Attrs holds the validated annotations of an item or let statement
// (set by Check). Zero values mean "not annotated".
type Attrs struct {
	// Timeout is #[timeout(d)] in nanoseconds; 0 if absent.
	Timeout int64
	// RetryMax, BackoffInitial and BackoffMax come from
	// #[retry(max = n, backoff = exp(initial, max))]; RetryMax 0 if absent.
	RetryMax       int64
	BackoffInitial int64
	BackoffMax     int64
	// Idempotent is #[idempotent].
	Idempotent bool
	// Deprecated is #[deprecated("msg")]; HasDeprecated reports presence.
	Deprecated    string
	HasDeprecated bool
	// MaxEffects is #[effects(max = N)]; 0 if absent (engine default).
	MaxEffects int64
	// OnUnknownRetry is #[on_unknown(retry)].
	OnUnknownRetry bool
}

type itemBase struct {
	Sp    Span
	Annot []*Annotation
	Attrs Attrs // set by Check
}

// Span implements Node.
func (b *itemBase) Span() Span { return b.Sp }

// Annots implements Item.
func (b *itemBase) Annots() []*Annotation     { return b.Annot }
func (b *itemBase) item()                     {}
func (b *itemBase) setAnnots(a []*Annotation) { b.Annot = a }

// Param is a declared parameter `name: Type`.
type Param struct {
	Sp      Span
	Name    *Ident
	TypeX   TypeExpr
	Binding *Binding // set by Check (nil for tool parameters)
}

// Span implements Node.
func (p *Param) Span() Span { return p.Sp }

// EffectClause is `! e1, e2`.
type EffectClause struct {
	Sp    Span
	Names []*Ident
	Set   EffectSet // set by Check
}

// Span implements Node.
func (e *EffectClause) Span() Span { return e.Sp }

// WorkflowDecl is `workflow name(params) [-> T] [! effects] { ... }`.
type WorkflowDecl struct {
	itemBase
	Name    *Ident
	Params  []*Param
	Result  TypeExpr      // nil when omitted (Unit)
	Effects *EffectClause // nil when omitted (inferred)
	Body    *BlockExpr
	Func    *FuncInfo // set by Check
}

// ItemName implements Item.
func (d *WorkflowDecl) ItemName() *Ident { return d.Name }

// FnDecl is `fn name<G>(params) [-> T] [! effects] { ... }`.
type FnDecl struct {
	itemBase
	Name     *Ident
	Generics []*Ident
	Params   []*Param
	Result   TypeExpr      // nil when omitted (Unit)
	Effects  *EffectClause // nil means pure
	Body     *BlockExpr
	Func     *FuncInfo // set by Check
}

// ItemName implements Item.
func (d *FnDecl) ItemName() *Ident { return d.Name }

// ToolDecl is `tool name(params) -> T ! effects`, bound to the registry.
type ToolDecl struct {
	itemBase
	Name    *Ident
	Params  []*Param
	Result  TypeExpr
	Effects *EffectClause
	Tool    *Tool // set by Check
}

// ItemName implements Item.
func (d *ToolDecl) ItemName() *Ident { return d.Name }

// FieldDecl is a record field `name: Type`.
type FieldDecl struct {
	Sp    Span
	Name  *Ident
	TypeX TypeExpr
}

// Span implements Node.
func (f *FieldDecl) Span() Span { return f.Sp }

// TypeDecl is `type Name<G> = { fields }`.
type TypeDecl struct {
	itemBase
	Name     *Ident
	Generics []*Ident
	Fields   []*FieldDecl
	Def      *TypeDef // set by Check
}

// ItemName implements Item.
func (d *TypeDecl) ItemName() *Ident { return d.Name }

// VariantDecl is an enum variant `Name` or `Name(T1, T2)`.
type VariantDecl struct {
	Sp      Span
	Name    *Ident
	Payload []TypeExpr
}

// Span implements Node.
func (v *VariantDecl) Span() Span { return v.Sp }

// EnumDecl is `enum Name<G> { variants }`.
type EnumDecl struct {
	itemBase
	Name     *Ident
	Generics []*Ident
	Variants []*VariantDecl
	Def      *TypeDef // set by Check
}

// ItemName implements Item.
func (d *EnumDecl) ItemName() *Ident { return d.Name }

// UseDecl is `use a::b;`. It brings the last path segment into scope.
type UseDecl struct {
	itemBase
	Path []*Ident
}

// ItemName implements Item.
func (d *UseDecl) ItemName() *Ident { return d.Path[len(d.Path)-1] }

// ---- Type expressions ----

// TypeExpr is a written type.
type TypeExpr interface {
	Node
	// Resolved returns the type the expression denotes (set by Check).
	Resolved() Type
	typeExpr()
}

type typeExprBase struct {
	Sp  Span
	Res Type
}

// Span implements Node.
func (b *typeExprBase) Span() Span { return b.Sp }

// Resolved implements TypeExpr.
func (b *typeExprBase) Resolved() Type { return b.Res }
func (b *typeExprBase) typeExpr()      {}

// NamedTypeExpr is `Name` or `Name<Args>`.
type NamedTypeExpr struct {
	typeExprBase
	Name *Ident
	Args []TypeExpr
}

// TupleTypeExpr is `()` (unit) or `(A, B, ...)`.
type TupleTypeExpr struct {
	typeExprBase
	Elems []TypeExpr
}

// FuncTypeExpr is `(A, B) -> R`.
type FuncTypeExpr struct {
	typeExprBase
	Params []TypeExpr
	Result TypeExpr
}

// ---- Statements ----

// Stmt is a statement inside a block.
type Stmt interface {
	Node
	stmt()
}

// LetStmt is `[annotations] let pat [: T] = e [compensate c];`.
type LetStmt struct {
	Sp    Span
	Annot []*Annotation
	Pat   Pattern
	TypeX TypeExpr // nil when omitted
	Value Expr
	// Compensate is the compensation as a synthetic one-parameter lambda
	// whose parameter is `it` (spec 7.5, 8.5); nil when absent.
	Compensate *Lambda
	Attrs      Attrs // set by Check
}

// ForStmt is `for pat in iter { body }`.
type ForStmt struct {
	Sp   Span
	Pat  Pattern
	Iter Expr
	Body *BlockExpr
	// ElemType is the type bound to Pat each iteration (set by Check).
	ElemType Type
	// IterKind says what is iterated (set by Check).
	IterKind IterKind
}

// IterKind is what a for loop iterates over.
type IterKind int

// Iteration kinds.
const (
	IterList   IterKind = iota // List<T>
	IterStream                 // Stream<T>
	IterMap                    // Map<K, V>, binding (K, V) tuples in insertion order
)

// RequireStmt is `require approval(args);`. It is sugar for
// `approve(args)?` with failure code approval_denied.
type RequireStmt struct {
	Sp   Span
	Args []*Arg
}

// ReturnStmt is `return [e];`.
type ReturnStmt struct {
	Sp    Span
	Value Expr // nil for a bare return
}

// ExprStmt is an expression evaluated for its effects.
type ExprStmt struct {
	Sp Span
	X  Expr
}

// AssignStmt is `name = e;`. Bindings are immutable, so Check always
// rejects it (E0312); the parser accepts it to give a precise message.
type AssignStmt struct {
	Sp     Span
	Target *Ident
	Value  Expr
}

// Span implements Node for the statement types.
func (s *LetStmt) Span() Span     { return s.Sp }
func (s *ForStmt) Span() Span     { return s.Sp }
func (s *RequireStmt) Span() Span { return s.Sp }
func (s *ReturnStmt) Span() Span  { return s.Sp }
func (s *ExprStmt) Span() Span    { return s.Sp }
func (s *AssignStmt) Span() Span  { return s.Sp }
func (*LetStmt) stmt()            {}
func (*ForStmt) stmt()            {}
func (*RequireStmt) stmt()        {}
func (*ReturnStmt) stmt()         {}
func (*ExprStmt) stmt()           {}
func (*AssignStmt) stmt()         {}

// ---- Expressions ----

// Expr is an expression. After Check, Type returns its zonked type and
// Effects its effect set (the union of everything evaluating it may do,
// excluding the latent effects of lambdas it creates).
type Expr interface {
	Node
	Type() Type
	Effects() EffectSet
	base() *exprBase
	expr()
}

type exprBase struct {
	Sp  Span
	T   Type
	Eff EffectSet
}

// Span implements Node.
func (b *exprBase) Span() Span { return b.Sp }

// Type returns the checked type (nil before Check).
func (b *exprBase) Type() Type { return b.T }

// Effects returns the checked effect set.
func (b *exprBase) Effects() EffectSet { return b.Eff }
func (b *exprBase) base() *exprBase    { return b }
func (b *exprBase) expr()              {}

// IntLit is an integer literal.
type IntLit struct {
	exprBase
	Value int64
}

// FloatLit is a float literal.
type FloatLit struct {
	exprBase
	Value float64
}

// DurationLit is a duration literal, in nanoseconds.
type DurationLit struct {
	exprBase
	Nanos int64
}

// BoolLit is true or false.
type BoolLit struct {
	exprBase
	Value bool
}

// StrPart is a literal piece (X == nil) or an interpolated expression.
type StrPart struct {
	Lit string
	X   Expr
}

// StringLit is a string literal; Parts alternate literal text and
// interpolations (lowered to INTERP when any X is present).
type StringLit struct {
	exprBase
	Parts []StrPart
}

// Interpolated reports whether the literal contains interpolations.
func (s *StringLit) Interpolated() bool {
	for _, p := range s.Parts {
		if p.X != nil {
			return true
		}
	}
	return false
}

// Ident is a name use. Ref is set by Check.
type Ident struct {
	exprBase
	Name string
	Ref  Ref
}

// PathExpr is `A::B` (an enum variant or a used item).
type PathExpr struct {
	exprBase
	Segs []*Ident
	Ref  Ref // set by Check
}

// TupleExpr is `()` (unit) or `(a, b, ...)`.
type TupleExpr struct {
	exprBase
	Elems []Expr
}

// ListExpr is `[a, b, ...]`.
type ListExpr struct {
	exprBase
	Elems []Expr
}

// FieldInit is `name: e` in a record literal. Index is the field's
// declaration index (set by Check).
type FieldInit struct {
	Name  *Ident
	Value Expr
	Index int
}

// RecordLit is `Path { f: e, ... }` (`f = e` is accepted too).
type RecordLit struct {
	exprBase
	Path   []*Ident
	Fields []*FieldInit
	Def    *TypeDef // set by Check
}

// LambdaParam is one lambda parameter.
type LambdaParam struct {
	Name    *Ident
	Binding *Binding // set by Check
}

// Lambda is `x => e` or `(a, b) => e`. Compensations are represented as
// synthetic lambdas with the single parameter `it`.
type Lambda struct {
	exprBase
	Params []*LambdaParam
	Body   Expr
	// Synthetic marks the compensation lambda of a let statement.
	Synthetic bool
	// Frame holds the lambda's own slots (set by Check).
	Frame *Frame
	// Captures lists the outer bindings the lambda reads, in first-use
	// order (set by Check). Captures are by value.
	Captures []*Capture
}

// IfExpr is `if c { } [else ...]`. Else is nil, *IfExpr or *BlockExpr.
type IfExpr struct {
	exprBase
	Cond Expr
	Then *BlockExpr
	Else Expr
}

// MatchArm is `pat [if guard] => body`.
type MatchArm struct {
	Sp    Span
	Pat   Pattern
	Guard Expr
	Body  Expr
}

// MatchExpr is `match e { arms }`.
type MatchExpr struct {
	exprBase
	Scrutinee Expr
	Arms      []*MatchArm
}

// AgentExpr is `agent name(named args)`; it has type Plan<T>.
type AgentExpr struct {
	exprBase
	Name *Ident
	Args []*Arg
	// Tools are the tools the plan may use, from the tools argument
	// (set by Check).
	Tools []*Tool
}

// BlockExpr is `{ stmts [tail] }`.
type BlockExpr struct {
	exprBase
	Stmts []Stmt
	Tail  Expr // nil when the block's value is ()
}

// UnaryOp is a prefix operator.
type UnaryOp int

// Unary operators.
const (
	OpNeg UnaryOp = iota // -
	OpNot                // !
)

// UnaryExpr is `-x` or `!x`.
type UnaryExpr struct {
	exprBase
	Op UnaryOp
	X  Expr
}

// BinaryOp is an infix operator (|> is desugared and never appears).
type BinaryOp int

// Binary operators.
const (
	OpOr BinaryOp = iota
	OpAnd
	OpEq
	OpNe
	OpLt
	OpLe
	OpGt
	OpGe
	OpIn
	OpRange
	OpAdd
	OpSub
	OpMul
	OpDiv
	OpRem
)

var binaryOpText = [...]string{"||", "&&", "==", "!=", "<", "<=", ">", ">=", "in", "..", "+", "-", "*", "/", "%"}

// String returns the operator as written.
func (o BinaryOp) String() string { return binaryOpText[o] }

// precedence returns the level of o in the table of spec 7.3.
func (o BinaryOp) precedence() int {
	switch o {
	case OpOr:
		return 2
	case OpAnd:
		return 3
	case OpEq, OpNe:
		return 4
	case OpLt, OpLe, OpGt, OpGe, OpIn:
		return 5
	case OpRange:
		return 6
	case OpAdd, OpSub:
		return 7
	default:
		return 8
	}
}

// BinaryExpr is `x op y`.
type BinaryExpr struct {
	exprBase
	Op   BinaryOp
	X, Y Expr
}

// PipeForm records how a call was written, for pretty-printing.
type PipeForm int

// Pipe forms.
const (
	PipeNone PipeForm = iota // f(a, x)
	PipeBare                 // a |> f        == f(a)
	PipeCall                 // a |> f(x)     == f(a, x)
)

// Arg is a call or annotation argument, positional (Name == nil) or named.
type Arg struct {
	Name  *Ident
	Value Expr
	// Param is the index of the callee parameter the argument binds to
	// (set by Check; -1 when unresolved).
	Param int
}

// CallKind says what a call invokes (set by Check).
type CallKind int

// Call kinds.
const (
	CallUnknown CallKind = iota // unresolved (an error was reported)
	CallBuiltin                 // standard-library function (Builtin)
	CallTool                    // registered tool: EFFECT tool_call
	CallFn                      // user fn (Fn)
	CallVariant                 // enum variant constructor (Variant)
	CallClosure                 // any other function value
)

// CallExpr is a call. Pipes are desugared: `a |> f(x)` is
// CallExpr{Fn: f, Args: [a, x], Pipe: PipeCall}.
type CallExpr struct {
	exprBase
	Fn   Expr
	Args []*Arg
	Pipe PipeForm
	// paren records that the call was written in parentheses, which
	// stops a following pipe from inserting its argument.
	paren bool

	// Set by Check.
	Kind    CallKind
	Builtin *Builtin
	Tool    *Tool
	FnDecl  *FnDecl
	Variant *Variant
	// Own is the effect set the call itself performs: the tool's or
	// fn's declared effects, the builtin's effects, or the latent effects
	// of a called closure. Effects() additionally includes the arguments'
	// effects and, for higher-order builtins, the latent effects of the
	// function arguments.
	Own EffectSet
	// TypeArgs are the instantiated type parameters of a generic callee,
	// in declaration order.
	TypeArgs []Type
}

// IndexExpr is `x[i]`.
type IndexExpr struct {
	exprBase
	X, Index Expr
}

// FieldExpr is `x.name`. On a record, Index is the field index. When X
// names the builtin `scope` namespace, Builtin is the selected function.
type FieldExpr struct {
	exprBase
	X       Expr
	Name    *Ident
	Index   int      // set by Check
	Builtin *Builtin // set by Check for scope.check / scope.allows
}

// TryMode says what a failing `?` does (set by Check).
type TryMode int

// Try modes.
const (
	TryFailRun    TryMode = iota // fail the run with code unwrap_failed
	TryReturnErr                 // return Err(e) from the enclosing fn/workflow
	TryReturnNone                // return None from the enclosing fn/workflow
)

// TryExpr is `x?`.
type TryExpr struct {
	exprBase
	X Expr
	// Mode is set by Check; OnOption reports an Option operand.
	Mode     TryMode
	OnOption bool
}

// CastKind is the conversion an `as` expression performs (set by Check).
type CastKind int

// Cast kinds.
const (
	CastNone          CastKind = iota // identity
	CastIntToFloat                    // Int as Float
	CastFloatToInt                    // Float as Int (truncates; fails on overflow)
	CastIntToDuration                 // Int (nanoseconds) as Duration
	CastDurationToInt                 // Duration as Int (nanoseconds)
	CastIntToTime                     // Int (Unix nanoseconds) as Time
	CastTimeToInt                     // Time as Int
	CastToString                      // domain scalar as String
	CastParseDomain                   // String as domain scalar (validated; fails run if invalid)
)

// CastExpr is `x as T`.
type CastExpr struct {
	exprBase
	X    Expr
	To   TypeExpr
	Kind CastKind
}

// BadExpr stands in for an expression that failed to parse.
type BadExpr struct {
	exprBase
}

// ---- Patterns ----

// Pattern is a match, let or for pattern. After Check, Type returns the
// type of the value it matches.
type Pattern interface {
	Node
	Type() Type
	pat() *patBase
}

type patBase struct {
	Sp Span
	T  Type
}

// Span implements Node.
func (b *patBase) Span() Span { return b.Sp }

// Type returns the type of the matched value (nil before Check).
func (b *patBase) Type() Type    { return b.T }
func (b *patBase) pat() *patBase { return b }

// WildcardPat is `_`.
type WildcardPat struct{ patBase }

// BindPat is a bare identifier. Check resolves it either to a new binding
// (Binding) or, when the name is a unit enum variant in scope such as
// None, to that variant (Variant); exactly one is non-nil after Check.
type BindPat struct {
	patBase
	Name    *Ident
	Binding *Binding
	Variant *Variant
}

// LitPat is a literal pattern; Lit is an *IntLit, *FloatLit,
// *DurationLit, *StringLit (no interpolation) or *BoolLit.
type LitPat struct {
	patBase
	Lit Expr
}

// VariantPat is `Path` or `Path(p1, p2)` for an enum variant.
type VariantPat struct {
	patBase
	Path      []*Ident
	HasParens bool
	Args      []Pattern
	Variant   *Variant // set by Check
}

// TuplePat is `(p1, p2, ...)`.
type TuplePat struct {
	patBase
	Elems []Pattern
}

// FieldPat is `name` (shorthand binding) or `name: pat` in a record pattern.
type FieldPat struct {
	Name  *Ident
	Pat   Pattern // nil for shorthand; Check fills ShortBinding
	Index int     // field index (set by Check)
	// ShortBinding is the binding introduced by the shorthand form.
	ShortBinding *Binding
}

// RecordPat is `Path { f1, f2: p, ... }`; unlisted fields are ignored.
type RecordPat struct {
	patBase
	Path   []*Ident
	Fields []*FieldPat
	Def    *TypeDef // set by Check
}

// Walk calls fn for n and every node below it in source order. If fn
// returns false the children of that node are skipped.
func Walk(n Node, fn func(Node) bool) {
	if n == nil || !fn(n) {
		return
	}
	walkChildren(n, fn)
}

func walkArgs(args []*Arg, fn func(Node) bool) {
	for _, a := range args {
		if a.Value != nil {
			Walk(a.Value, fn)
		}
	}
}

func walkChildren(n Node, fn func(Node) bool) {
	switch n := n.(type) {
	case *WorkflowDecl:
		if n.Body != nil {
			Walk(n.Body, fn)
		}
	case *FnDecl:
		if n.Body != nil {
			Walk(n.Body, fn)
		}
	case *BlockExpr:
		for _, s := range n.Stmts {
			Walk(s, fn)
		}
		if n.Tail != nil {
			Walk(n.Tail, fn)
		}
	case *LetStmt:
		Walk(n.Pat, fn)
		Walk(n.Value, fn)
		if n.Compensate != nil {
			Walk(n.Compensate, fn)
		}
	case *ForStmt:
		Walk(n.Pat, fn)
		Walk(n.Iter, fn)
		Walk(n.Body, fn)
	case *RequireStmt:
		walkArgs(n.Args, fn)
	case *ReturnStmt:
		if n.Value != nil {
			Walk(n.Value, fn)
		}
	case *ExprStmt:
		Walk(n.X, fn)
	case *AssignStmt:
		Walk(n.Value, fn)
	case *StringLit:
		for _, p := range n.Parts {
			if p.X != nil {
				Walk(p.X, fn)
			}
		}
	case *TupleExpr:
		for _, e := range n.Elems {
			Walk(e, fn)
		}
	case *ListExpr:
		for _, e := range n.Elems {
			Walk(e, fn)
		}
	case *RecordLit:
		for _, f := range n.Fields {
			Walk(f.Value, fn)
		}
	case *Lambda:
		Walk(n.Body, fn)
	case *IfExpr:
		Walk(n.Cond, fn)
		Walk(n.Then, fn)
		if n.Else != nil {
			Walk(n.Else, fn)
		}
	case *MatchExpr:
		Walk(n.Scrutinee, fn)
		for _, a := range n.Arms {
			Walk(a.Pat, fn)
			if a.Guard != nil {
				Walk(a.Guard, fn)
			}
			Walk(a.Body, fn)
		}
	case *AgentExpr:
		walkArgs(n.Args, fn)
	case *UnaryExpr:
		Walk(n.X, fn)
	case *BinaryExpr:
		Walk(n.X, fn)
		Walk(n.Y, fn)
	case *CallExpr:
		// Source order: for a pipe the first argument precedes the callee.
		if n.Pipe != PipeNone && len(n.Args) > 0 {
			Walk(n.Args[0].Value, fn)
			Walk(n.Fn, fn)
			walkArgs(n.Args[1:], fn)
		} else {
			Walk(n.Fn, fn)
			walkArgs(n.Args, fn)
		}
	case *IndexExpr:
		Walk(n.X, fn)
		Walk(n.Index, fn)
	case *FieldExpr:
		Walk(n.X, fn)
	case *TryExpr:
		Walk(n.X, fn)
	case *CastExpr:
		Walk(n.X, fn)
	case *VariantPat:
		for _, p := range n.Args {
			Walk(p, fn)
		}
	case *TuplePat:
		for _, p := range n.Elems {
			Walk(p, fn)
		}
	case *RecordPat:
		for _, f := range n.Fields {
			if f.Pat != nil {
				Walk(f.Pat, fn)
			}
		}
	}
}

// isBlockLike reports whether e ends with a block, so that it may stand
// as a statement without a semicolon.
func isBlockLike(e Expr) bool {
	switch e.(type) {
	case *IfExpr, *MatchExpr, *BlockExpr:
		return true
	}
	return false
}
