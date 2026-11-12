package bytecode

import (
	"crypto/sha256"
	"fmt"
	"math"
)

// Format constants (spec 8.3, 8.7).
const (
	Magic         = "FFBC"
	FormatVersion = 1

	// MaxModuleSize is the largest encoded module accepted (spec 8.7).
	MaxModuleSize = 4 << 20
	// MaxStackDepth is the value stack limit (spec 8.7). The module format
	// has no per-function stack size field, so the verifier computes each
	// function's maximum operand depth and requires it to stay below this.
	MaxStackDepth = 65536
	// MaxStringBytes bounds string constants and run-time strings.
	MaxStringBytes = 1 << 20
	// NoType marks an absent type text reference in TYPES.
	NoType = ^uint32(0)
)

// ConstTag is the tag byte of a CONSTS entry.
type ConstTag uint8

// Constant tags. Int, Duration and Time payloads are 8-byte little-endian
// two's complement; Float is the 8-byte IEEE 754 bit pattern (finite only);
// String is UTF-8.
const (
	ConstInt      ConstTag = 1
	ConstFloat    ConstTag = 2
	ConstString   ConstTag = 3
	ConstDuration ConstTag = 4 // nanoseconds
	ConstTime     ConstTag = 5 // Unix nanoseconds
)

func (t ConstTag) String() string {
	switch t {
	case ConstInt:
		return "int"
	case ConstFloat:
		return "float"
	case ConstString:
		return "string"
	case ConstDuration:
		return "duration"
	case ConstTime:
		return "time"
	}
	return fmt.Sprintf("tag(%d)", uint8(t))
}

// Const is one constant pool entry. Int holds the payload of Int, Duration
// and Time constants.
type Const struct {
	Tag   ConstTag
	Int   int64
	Float float64
	Str   string
}

// IntConst returns an Int constant.
func IntConst(v int64) Const { return Const{Tag: ConstInt, Int: v} }

// FloatConst returns a Float constant.
func FloatConst(v float64) Const { return Const{Tag: ConstFloat, Float: v} }

// StringConst returns a String constant.
func StringConst(s string) Const { return Const{Tag: ConstString, Str: s} }

// DurationConst returns a Duration constant (nanoseconds).
func DurationConst(ns int64) Const { return Const{Tag: ConstDuration, Int: ns} }

// TimeConst returns a Time constant (Unix nanoseconds).
func TimeConst(ns int64) Const { return Const{Tag: ConstTime, Int: ns} }

func (c Const) key() string {
	switch c.Tag {
	case ConstFloat:
		return fmt.Sprintf("%d:%x", c.Tag, math.Float64bits(c.Float))
	case ConstString:
		return fmt.Sprintf("%d:%s", c.Tag, c.Str)
	}
	return fmt.Sprintf("%d:%d", c.Tag, c.Int)
}

func (c Const) String() string {
	switch c.Tag {
	case ConstFloat:
		return fmt.Sprintf("float %v", c.Float)
	case ConstString:
		return fmt.Sprintf("string %q", c.Str)
	}
	return fmt.Sprintf("%s %d", c.Tag, c.Int)
}

// TypeDef is one TYPES entry. Name and the type references are constant
// pool indices; type references point at String constants holding DSL type
// text (for example "Option<List<Fqdn>>") or are NoType.
type TypeDef struct {
	Kind     TypeKind
	Name     uint32
	Fields   []FieldDef   // KindRecord, KindSignature (parameters)
	Variants []VariantDef // KindEnum
	Func     uint16       // KindSignature: described function
	Result   uint32       // KindSignature: result type text
}

// FieldDef is a record field or a signature parameter.
type FieldDef struct {
	Name uint32
	Type uint32
}

// VariantDef is an enum variant; its arity is len(Payload).
type VariantDef struct {
	Name    uint32
	Payload []uint32
}

// Span maps a code offset to a source range.
type Span struct {
	CodeOffset   uint32
	SourceOffset uint32
	Len          uint32
}

// Func is one FUNCS entry.
type Func struct {
	Name    uint32 // constant index of the function name
	Arity   uint8
	NLocals uint16
	NCaps   uint8
	Code    []byte
	Spans   []Span // sorted by CodeOffset
}

// Tool is one TOOLS entry.
type Tool struct {
	Name       uint32 // constant index of the tool name
	SpecSHA256 [32]byte
}

// Meta is the META section.
type Meta struct {
	CompilerVersion     string
	SourceSHA256        [32]byte
	ProgramPolicySHA256 [32]byte
	WorkflowName        string
}

// Module is a decoded bytecode module (spec 8.3).
type Module struct {
	FormatVersion uint16
	Flags         uint16
	Consts        []Const
	Types         []TypeDef
	Funcs         []Func
	Effects       EffectMask
	Tools         []Tool
	Meta          Meta
	Entry         uint16
}

// Hash returns the module hash: SHA-256 of the encoded module.
func (m *Module) Hash() ([32]byte, error) {
	b, err := m.Encode()
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// ConstString returns the String constant at index i.
func (m *Module) ConstString(i uint32) (string, bool) {
	if uint64(i) >= uint64(len(m.Consts)) || m.Consts[i].Tag != ConstString {
		return "", false
	}
	return m.Consts[i].Str, true
}

func (m *Module) str(i uint32) string {
	s, _ := m.ConstString(i)
	return s
}

func (m *Module) typeText(i uint32) string {
	if i == NoType {
		return ""
	}
	return m.str(i)
}

// FuncName returns the name of function fn ("" if it has none).
func (m *Module) FuncName(fn int) string {
	if fn < 0 || fn >= len(m.Funcs) {
		return ""
	}
	return m.str(m.Funcs[fn].Name)
}

// ToolName returns the name of TOOLS entry i.
func (m *Module) ToolName(i int) string {
	if i < 0 || i >= len(m.Tools) {
		return ""
	}
	return m.str(m.Tools[i].Name)
}

// Layout resolves a type id to its layout: a built-in layout for reserved
// ids, or the module's TYPES entry with names resolved to strings.
func (m *Module) Layout(id TypeID) (*TypeLayout, bool) {
	if id < FirstUserType {
		return BuiltinType(id)
	}
	i := int(id - FirstUserType)
	if i >= len(m.Types) {
		return nil, false
	}
	td := &m.Types[i]
	l := &TypeLayout{ID: id, Kind: td.Kind, Name: m.str(td.Name), Func: td.Func}
	for _, f := range td.Fields {
		l.Fields = append(l.Fields, FieldLayout{Name: m.str(f.Name), Type: m.typeText(f.Type)})
	}
	for _, vd := range td.Variants {
		vl := VariantLayout{Name: m.str(vd.Name)}
		for _, p := range vd.Payload {
			vl.Payload = append(vl.Payload, m.typeText(p))
		}
		l.Variants = append(l.Variants, vl)
	}
	if td.Kind == KindSignature {
		l.Result = m.typeText(td.Result)
	}
	return l, true
}

// LayoutByName finds a value type (record or enum) by name, user types
// first, then built-ins.
func (m *Module) LayoutByName(name string) (*TypeLayout, bool) {
	for i := range m.Types {
		if m.Types[i].Kind != KindSignature && m.str(m.Types[i].Name) == name {
			return m.Layout(FirstUserType + TypeID(i))
		}
	}
	return BuiltinTypeByName(name)
}

// Signature returns the signature layout describing function fn, if the
// module carries one.
func (m *Module) Signature(fn uint16) (*TypeLayout, bool) {
	for i := range m.Types {
		if m.Types[i].Kind == KindSignature && m.Types[i].Func == fn {
			return m.Layout(FirstUserType + TypeID(i))
		}
	}
	return nil, false
}
