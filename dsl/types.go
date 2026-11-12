package dsl

import (
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

// Type is a checked type (spec 7.4). After Check every expression type is
// "zonked": inference variables are replaced by what they resolved to.
// Implementations: *PrimType, *NamedType, *TupleType, *FuncType,
// *TypeParam and, only while checking or when inference failed, *TypeVar.
type Type interface {
	String() string
	isType()
}

// PrimKind enumerates the primitive types.
type PrimKind int

// Primitive kinds.
const (
	PrimInt PrimKind = iota
	PrimFloat
	PrimBool
	PrimString
	PrimDuration
	PrimTime
	PrimUnit
	// PrimNever is the type of expressions that do not complete normally
	// (a block ending in return). It unifies with every type.
	PrimNever
	// PrimError is the type of expressions that failed to check. It
	// unifies with every type so one error is reported only once.
	PrimError
)

var primNames = [...]string{"Int", "Float", "Bool", "String", "Duration", "Time", "()", "Never", "{error}"}

// PrimType is a primitive type. Use the shared values TInt, TFloat, ...
type PrimType struct{ Kind PrimKind }

func (t *PrimType) String() string { return primNames[t.Kind] }
func (*PrimType) isType()          {}

// Shared primitive types.
var (
	TInt      = &PrimType{PrimInt}
	TFloat    = &PrimType{PrimFloat}
	TBool     = &PrimType{PrimBool}
	TString   = &PrimType{PrimString}
	TDuration = &PrimType{PrimDuration}
	TTime     = &PrimType{PrimTime}
	TUnit     = &PrimType{PrimUnit}
	TNever    = &PrimType{PrimNever}
	TError    = &PrimType{PrimError}
)

// TypeDefKind classifies named types.
type TypeDefKind int

// Named type kinds.
const (
	DefRecord TypeDefKind = iota // record with named fields
	DefEnum                      // algebraic data type
	DefScalar                    // validated domain scalar (Fqdn, Url, ...)
	DefOpaque                    // builtin with no visible structure (List, Map, Plan, InScope, ...)
)

// TypeDef is a record, enum, domain scalar or builtin type constructor.
// IDs are dense and stable for a given program: builtins first in prelude
// order, then user declarations in source order. The bytecode TYPES
// section can use them directly.
type TypeDef struct {
	ID       int
	Name     string
	Kind     TypeDefKind
	Params   []*TypeParam
	Fields   []*Field   // DefRecord
	Variants []*Variant // DefEnum
	Builtin  bool
	// Display reports whether values can be interpolated into strings.
	Display bool
	// Decl is the declaring item for user types (*TypeDecl or *EnumDecl).
	Decl Item
}

// Field is a record field.
type Field struct {
	Name  string
	Type  Type
	Index int
}

// Variant is an enum variant. Index is its position in the enum.
type Variant struct {
	Def     *TypeDef
	Name    string
	Index   int
	Payload []Type
	Decl    *VariantDecl // nil for builtin enums
}

// QualifiedName returns Enum::Variant.
func (v *Variant) QualifiedName() string { return v.Def.Name + "::" + v.Name }

// FieldByName returns the named field or nil.
func (d *TypeDef) FieldByName(name string) *Field {
	for _, f := range d.Fields {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// VariantByName returns the named variant or nil.
func (d *TypeDef) VariantByName(name string) *Variant {
	for _, v := range d.Variants {
		if v.Name == name {
			return v
		}
	}
	return nil
}

// NamedType is an application of a TypeDef to type arguments.
type NamedType struct {
	Def  *TypeDef
	Args []Type
}

func (t *NamedType) String() string {
	if len(t.Args) == 0 {
		return t.Def.Name
	}
	parts := make([]string, len(t.Args))
	for i, a := range t.Args {
		parts[i] = a.String()
	}
	return t.Def.Name + "<" + strings.Join(parts, ", ") + ">"
}
func (*NamedType) isType() {}

// TupleType is a tuple with two or more elements (the empty tuple is
// TUnit).
type TupleType struct{ Elems []Type }

func (t *TupleType) String() string {
	parts := make([]string, len(t.Elems))
	for i, a := range t.Elems {
		parts[i] = a.String()
	}
	return "(" + strings.Join(parts, ", ") + ")"
}
func (*TupleType) isType() {}

// FuncType is a function type. Effects is the latent effect set: what
// calling the function may do. Function types written in source are
// pure (Effects empty); lambdas, tools and fns referenced as values carry
// their effects. Open marks parameters of effect-polymorphic builtins
// (map, filter, ...), which accept functions with any effects and
// propagate them to the call.
type FuncType struct {
	Params  []Type
	Result  Type
	Effects EffectSet
	Open    bool
}

func (t *FuncType) String() string {
	parts := make([]string, len(t.Params))
	for i, a := range t.Params {
		parts[i] = a.String()
	}
	s := "(" + strings.Join(parts, ", ") + ") -> " + t.Result.String()
	if t.Effects != 0 {
		s += " ! " + strings.Join(t.Effects.Names(), ", ")
	}
	return s
}
func (*FuncType) isType() {}

// TypeParam is a generic parameter of a declaration (rigid inside the
// declaration's body).
type TypeParam struct {
	Name string
	ID   int
}

func (t *TypeParam) String() string { return t.Name }
func (*TypeParam) isType()          {}

// TypeVar is an inference variable. Ref is non-nil once solved.
type TypeVar struct {
	ID  int
	Ref Type
}

func (t *TypeVar) String() string {
	if t.Ref != nil {
		return t.Ref.String()
	}
	return "_"
}
func (*TypeVar) isType() {}

// prune follows solved inference variables.
func prune(t Type) Type {
	for {
		v, ok := t.(*TypeVar)
		if !ok || v.Ref == nil {
			return t
		}
		t = v.Ref
	}
}

// ---- Effects ----

// Effect is one static effect kind (spec 7.5).
type Effect uint16

// Effect kinds in canonical order.
const (
	EffRead Effect = 1 << iota
	EffPassive
	EffActive
	EffIntrusive
	EffWrite
	EffFinancialWrite
	EffLLM
	EffTime
)

var effectNames = []struct {
	e    Effect
	name string
}{
	{EffRead, "read"}, {EffPassive, "passive"}, {EffActive, "active"},
	{EffIntrusive, "intrusive"}, {EffWrite, "write"},
	{EffFinancialWrite, "financial_write"}, {EffLLM, "llm"}, {EffTime, "time"},
}

// EffectSet is a set of effects.
type EffectSet uint16

// ParseEffect parses an effect name.
func ParseEffect(name string) (Effect, bool) {
	for _, e := range effectNames {
		if e.name == name {
			return e.e, true
		}
	}
	return 0, false
}

// Has reports whether s contains e.
func (s EffectSet) Has(e Effect) bool { return s&EffectSet(e) != 0 }

// Names returns the effect names in canonical order (read, passive,
// active, intrusive, write, financial_write, llm, time).
func (s EffectSet) Names() []string {
	out := []string{}
	for _, e := range effectNames {
		if s.Has(e.e) {
			out = append(out, e.name)
		}
	}
	return out
}

// String formats the set as a comma-separated list.
func (s EffectSet) String() string { return strings.Join(s.Names(), ", ") }

// Class returns the highest activity class among passive, active and
// intrusive in s (core.ClassNone when none is present).
func (s EffectSet) Class() core.ActivityClass {
	switch {
	case s.Has(EffIntrusive):
		return core.ClassIntrusive
	case s.Has(EffActive):
		return core.ClassActive
	case s.Has(EffPassive):
		return core.ClassPassive
	}
	return core.ClassNone
}

// effectClass returns the activity class of a single effect.
func effectClass(e Effect) core.ActivityClass {
	return EffectSet(e).Class()
}

// gatedEffects need a preceding approval under a known policy (E0403).
const gatedEffects = EffectSet(EffFinancialWrite) | EffectSet(EffIntrusive)

// each calls fn for every effect in s in canonical order.
func (s EffectSet) each(fn func(Effect, string)) {
	for _, e := range effectNames {
		if s.Has(e.e) {
			fn(e.e, e.name)
		}
	}
}

// ---- Runtime operations ----

// OpKind is a runtime effect kind of the EFFECT instruction (spec 8.4).
// The values are the instruction's kind operand.
type OpKind uint8

// Runtime effect kinds (spec 8.4).
const (
	OpNone            OpKind = 0
	OpToolCall        OpKind = 1
	OpScopeCheck      OpKind = 2
	OpApproval        OpKind = 3
	OpSleep           OpKind = 4
	OpAgentCall       OpKind = 5
	OpExecutePlan     OpKind = 6
	OpEmitAlert       OpKind = 7
	OpNow             OpKind = 8
	OpLog             OpKind = 9
	OpParMap          OpKind = 10
	OpEmbed           OpKind = 11
	OpSimilarityQuery OpKind = 12
)

var opNames = [...]string{"", "tool_call", "scope_check", "approval", "sleep", "agent_call",
	"execute_plan", "emit_alert", "now", "log", "par_map", "embed", "similarity_query"}

func (o OpKind) String() string {
	if int(o) < len(opNames) {
		return opNames[o]
	}
	return "op?"
}

// OpSet is a set of runtime effect kinds.
type OpSet uint16

// Has reports whether s contains o.
func (s OpSet) Has(o OpKind) bool { return o != OpNone && s&(1<<o) != 0 }

func (s OpSet) with(o OpKind) OpSet {
	if o == OpNone {
		return s
	}
	return s | 1<<o
}

// Names returns the operation names in kind order.
func (s OpSet) Names() []string {
	out := []string{}
	for o := OpToolCall; o <= OpSimilarityQuery; o++ {
		if s.Has(o) {
			out = append(out, o.String())
		}
	}
	return out
}

// ---- Bindings, frames and references ----

// BindingKind classifies local bindings.
type BindingKind int

// Binding kinds.
const (
	BindParam       BindingKind = iota // fn or workflow parameter
	BindLet                            // let statement
	BindPattern                        // pattern variable in match, let, or for
	BindLambdaParam                    // lambda parameter
	BindIt                             // `it` in a compensation
)

// Binding is one immutable local variable. ID is unique within a Program;
// Slot is the variable's local index within its Frame (parameters take
// slots 0..n-1, then locals in declaration order; slots are never reused).
type Binding struct {
	ID    int
	Name  string
	Kind  BindingKind
	Type  Type
	Frame *Frame
	Slot  int
	Decl  Span
}

// Frame is the local-variable frame of a fn, workflow, lambda or
// compensation.
type Frame struct {
	// Bindings lists every binding owned by the frame in slot order.
	Bindings []*Binding
	// Parent is the lexically enclosing frame (nil for items).
	Parent *Frame
	// Lambda is the lambda owning the frame (nil for items).
	Lambda *Lambda
}

// NumSlots is the number of local slots the frame needs.
func (f *Frame) NumSlots() int { return len(f.Bindings) }

// Capture is one value a lambda captures. Outer says where the creating
// frame finds the value: Outer < 0 means the creating frame's local slot
// Binding.Slot; Outer >= 0 is an index into the creating lambda's own
// Captures.
type Capture struct {
	Binding *Binding
	Outer   int
}

// RefKind classifies what a name refers to.
type RefKind int

// Reference kinds.
const (
	RefNone      RefKind = iota // unresolved
	RefLocal                    // a binding of the current frame
	RefCapture                  // a binding captured by the current lambda
	RefFn                       // a user fn
	RefWorkflow                 // a workflow (not callable)
	RefTool                     // a tool
	RefBuiltin                  // a standard-library function
	RefVariant                  // an enum variant constructor or unit variant
	RefNamespace                // the builtin `scope` namespace
)

// Ref is the resolution of a name (set by Check).
type Ref struct {
	Kind    RefKind
	Binding *Binding // RefLocal, RefCapture
	Capture int      // RefCapture: index into the enclosing Lambda.Captures
	Fn      *FnDecl
	Flow    *WorkflowDecl
	Tool    *Tool
	Builtin *Builtin
	Variant *Variant
}

// FuncInfo is the checked signature and analysis of a fn or workflow.
type FuncInfo struct {
	Name     string
	Params   []Type
	Result   Type
	Generics []*TypeParam
	// Declared is the effect set after `!` (empty for a fn without one).
	Declared    EffectSet
	HasDeclared bool
	// Inferred is the union of the body's effects.
	Inferred EffectSet
	// Ops are the runtime effect kinds the body may perform, including
	// through called fns.
	Ops OpSet
	// Tools are the tools the body may call, directly or through fns, in
	// first-use order.
	Tools []*Tool
	Frame *Frame
}

// Tool is a registered tool's checked declaration.
type Tool struct {
	Name       string
	ParamNames []string
	Params     []Type
	Result     Type
	Effects    EffectSet
	Decl       *ToolDecl
	// FromRegistry reports that the declaration came from Options.Tools.
	FromRegistry bool
}

// Class returns the tool's activity class.
func (t *Tool) Class() core.ActivityClass { return t.Effects.Class() }

// funcType returns the tool's type as a function value.
func (t *Tool) funcType() *FuncType {
	return &FuncType{Params: t.Params, Result: t.Result, Effects: t.Effects}
}

// Signature formats the tool as in a planner request (spec 16.2), e.g.
// "(Fqdn) -> Option<IpAddress>".
func (t *Tool) Signature() string {
	return (&FuncType{Params: t.Params, Result: t.Result}).String()
}

// ---- Type utilities ----

// containsInScope reports whether t mentions InScope anywhere.
func containsInScope(t Type) bool {
	switch t := prune(t).(type) {
	case *NamedType:
		if t.Def == defInScope {
			return true
		}
		for _, a := range t.Args {
			if containsInScope(a) {
				return true
			}
		}
	case *TupleType:
		for _, a := range t.Elems {
			if containsInScope(a) {
				return true
			}
		}
	case *FuncType:
		for _, a := range t.Params {
			if containsInScope(a) {
				return true
			}
		}
		return containsInScope(t.Result)
	}
	return false
}

// subst replaces type parameters according to m.
func subst(t Type, m map[*TypeParam]Type) Type {
	if len(m) == 0 {
		return t
	}
	switch tt := prune(t).(type) {
	case *TypeParam:
		if r, ok := m[tt]; ok {
			return r
		}
		return tt
	case *NamedType:
		if len(tt.Args) == 0 {
			return tt
		}
		args := make([]Type, len(tt.Args))
		for i, a := range tt.Args {
			args[i] = subst(a, m)
		}
		return &NamedType{Def: tt.Def, Args: args}
	case *TupleType:
		el := make([]Type, len(tt.Elems))
		for i, a := range tt.Elems {
			el[i] = subst(a, m)
		}
		return &TupleType{Elems: el}
	case *FuncType:
		ps := make([]Type, len(tt.Params))
		for i, a := range tt.Params {
			ps[i] = subst(a, m)
		}
		return &FuncType{Params: ps, Result: subst(tt.Result, m), Effects: tt.Effects, Open: tt.Open}
	default:
		return tt
	}
}

// zonk replaces solved variables throughout t.
func zonk(t Type) Type {
	if t == nil {
		return nil
	}
	switch tt := prune(t).(type) {
	case *NamedType:
		if len(tt.Args) == 0 {
			return tt
		}
		args := make([]Type, len(tt.Args))
		for i, a := range tt.Args {
			args[i] = zonk(a)
		}
		return &NamedType{Def: tt.Def, Args: args}
	case *TupleType:
		el := make([]Type, len(tt.Elems))
		for i, a := range tt.Elems {
			el[i] = zonk(a)
		}
		return &TupleType{Elems: el}
	case *FuncType:
		ps := make([]Type, len(tt.Params))
		for i, a := range tt.Params {
			ps[i] = zonk(a)
		}
		return &FuncType{Params: ps, Result: zonk(tt.Result), Effects: tt.Effects, Open: tt.Open}
	default:
		return tt
	}
}

// hasVars reports whether t contains an unsolved inference variable and
// returns the first one.
func hasVars(t Type) (*TypeVar, bool) {
	switch tt := prune(t).(type) {
	case *TypeVar:
		return tt, true
	case *NamedType:
		for _, a := range tt.Args {
			if v, ok := hasVars(a); ok {
				return v, true
			}
		}
	case *TupleType:
		for _, a := range tt.Elems {
			if v, ok := hasVars(a); ok {
				return v, true
			}
		}
	case *FuncType:
		for _, a := range tt.Params {
			if v, ok := hasVars(a); ok {
				return v, true
			}
		}
		return hasVars(tt.Result)
	}
	return nil, false
}

// occurs reports whether v occurs in t.
func occurs(v *TypeVar, t Type) bool {
	switch tt := prune(t).(type) {
	case *TypeVar:
		return tt == v
	case *NamedType:
		for _, a := range tt.Args {
			if occurs(v, a) {
				return true
			}
		}
	case *TupleType:
		for _, a := range tt.Elems {
			if occurs(v, a) {
				return true
			}
		}
	case *FuncType:
		for _, a := range tt.Params {
			if occurs(v, a) {
				return true
			}
		}
		return occurs(v, tt.Result)
	}
	return false
}

// unify makes a and b equal by solving inference variables. There is no
// subtyping and no coercion (spec 7.5). Never unifies with anything.
func unify(a, b Type) bool {
	a, b = prune(a), prune(b)
	if a == b {
		return true
	}
	if va, ok := a.(*TypeVar); ok {
		if vb, ok := b.(*TypeVar); ok && va == vb {
			return true
		}
		if isWild(b) {
			return true
		}
		if occurs(va, b) {
			return false
		}
		va.Ref = b
		return true
	}
	if _, ok := b.(*TypeVar); ok {
		return unify(b, a)
	}
	switch at := a.(type) {
	case *PrimType:
		if isWild(at) || isWild(b) {
			return true
		}
		bt, ok := b.(*PrimType)
		return ok && bt.Kind == at.Kind
	case *NamedType:
		if isWild(b) {
			return true
		}
		bt, ok := b.(*NamedType)
		if !ok || bt.Def != at.Def || len(bt.Args) != len(at.Args) {
			return false
		}
		for i := range at.Args {
			if !unify(at.Args[i], bt.Args[i]) {
				return false
			}
		}
		return true
	case *TupleType:
		if isWild(b) {
			return true
		}
		bt, ok := b.(*TupleType)
		if !ok || len(bt.Elems) != len(at.Elems) {
			return false
		}
		for i := range at.Elems {
			if !unify(at.Elems[i], bt.Elems[i]) {
				return false
			}
		}
		return true
	case *FuncType:
		if isWild(b) {
			return true
		}
		bt, ok := b.(*FuncType)
		if !ok || len(bt.Params) != len(at.Params) {
			return false
		}
		for i := range at.Params {
			if !unify(at.Params[i], bt.Params[i]) {
				return false
			}
		}
		return unify(at.Result, bt.Result)
	case *TypeParam:
		if isWild(b) {
			return true
		}
		return a == b
	}
	return false
}

// isWild reports whether t is Never or the error type, which unify with
// every type.
func isWild(t Type) bool {
	p, ok := prune(t).(*PrimType)
	return ok && (p.Kind == PrimNever || p.Kind == PrimError)
}

// hasError reports whether t mentions the error type.
func hasError(t Type) bool {
	switch tt := prune(t).(type) {
	case *PrimType:
		return tt.Kind == PrimError
	case *NamedType:
		for _, a := range tt.Args {
			if hasError(a) {
				return true
			}
		}
	case *TupleType:
		for _, a := range tt.Elems {
			if hasError(a) {
				return true
			}
		}
	case *FuncType:
		for _, a := range tt.Params {
			if hasError(a) {
				return true
			}
		}
		return hasError(tt.Result)
	}
	return false
}

// isNever reports whether t is the Never type.
func isNever(t Type) bool {
	p, ok := prune(t).(*PrimType)
	return ok && p.Kind == PrimNever
}

// namedOf returns t as a *NamedType of def, or nil.
func namedOf(t Type, def *TypeDef) *NamedType {
	n, ok := prune(t).(*NamedType)
	if ok && n.Def == def {
		return n
	}
	return nil
}

// isPrim reports whether t is the primitive kind k.
func isPrim(t Type, k PrimKind) bool {
	p, ok := prune(t).(*PrimType)
	return ok && p.Kind == k
}

// displayable reports whether values of t can be interpolated.
func displayable(t Type) bool {
	switch tt := prune(t).(type) {
	case *PrimType:
		return tt.Kind != PrimUnit
	case *NamedType:
		return tt.Def.Display
	}
	return false
}

// equatable reports whether values of t can be compared with == (no
// function values).
func equatable(t Type) bool {
	switch tt := prune(t).(type) {
	case *FuncType:
		return false
	case *NamedType:
		for _, a := range tt.Args {
			if !equatable(a) {
				return false
			}
		}
		if tt.Def.Kind == DefRecord {
			for _, f := range tt.Def.Fields {
				if _, isFn := f.Type.(*FuncType); isFn {
					return false
				}
			}
		}
	case *TupleType:
		for _, a := range tt.Elems {
			if !equatable(a) {
				return false
			}
		}
	}
	return true
}

// ordered reports whether t supports < <= > >=.
func ordered(t Type) bool {
	p, ok := prune(t).(*PrimType)
	if !ok {
		n, ok := prune(t).(*NamedType)
		return ok && n.Def.Kind == DefScalar
	}
	switch p.Kind {
	case PrimInt, PrimFloat, PrimString, PrimDuration, PrimTime:
		return true
	}
	return false
}
