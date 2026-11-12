package bytecode

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// ErrBuild is wrapped by every error Builder.Build reports.
var ErrBuild = errors.New("bytecode: build error")

// Builder assembles a module. Constants are deduplicated; functions can be
// declared before their bodies are emitted so that recursion and forward
// calls work. Errors are sticky and reported by Build.
type Builder struct {
	m      Module
	consts map[string]uint32
	funcs  []*FuncBuilder
	err    error
}

// NewBuilder returns an empty builder with format version 1.
func NewBuilder() *Builder {
	return &Builder{m: Module{FormatVersion: FormatVersion}, consts: map[string]uint32{}}
}

func (b *Builder) setErr(format string, args ...any) {
	if b.err == nil {
		b.err = fmt.Errorf("%w: %s", ErrBuild, fmt.Sprintf(format, args...))
	}
}

// Const adds c to the constant pool (or finds an equal existing entry) and
// returns its index.
func (b *Builder) Const(c Const) uint32 {
	k := c.key()
	if i, ok := b.consts[k]; ok {
		return i
	}
	i := uint32(len(b.m.Consts))
	b.m.Consts = append(b.m.Consts, c)
	b.consts[k] = i
	return i
}

// Str adds a String constant and returns its index.
func (b *Builder) Str(s string) uint32 { return b.Const(StringConst(s)) }

func (b *Builder) typeRef(text string) uint32 {
	if text == "" {
		return NoType
	}
	return b.Str(text)
}

// Field is a record field (or signature parameter) given by name and DSL
// type text.
type Field struct {
	Name string
	Type string
}

// Variant is an enum variant given by name and payload type texts.
type Variant struct {
	Name    string
	Payload []string
}

// Record adds a record layout and returns its type id.
func (b *Builder) Record(name string, fields ...Field) TypeID {
	td := TypeDef{Kind: KindRecord, Name: b.Str(name)}
	for _, f := range fields {
		td.Fields = append(td.Fields, FieldDef{Name: b.Str(f.Name), Type: b.typeRef(f.Type)})
	}
	return b.addType(td)
}

// Enum adds an enum layout and returns its type id.
func (b *Builder) Enum(name string, variants ...Variant) TypeID {
	td := TypeDef{Kind: KindEnum, Name: b.Str(name)}
	for _, v := range variants {
		vd := VariantDef{Name: b.Str(v.Name)}
		for _, p := range v.Payload {
			vd.Payload = append(vd.Payload, b.typeRef(p))
		}
		td.Variants = append(td.Variants, vd)
	}
	return b.addType(td)
}

// Signature records the parameter and result types of function fn (the
// compiler emits one for the entry function).
func (b *Builder) Signature(fn uint16, params []Field, result string) {
	td := TypeDef{Kind: KindSignature, Name: b.Str(b.funcName(fn)), Func: fn, Result: b.typeRef(result)}
	for _, p := range params {
		td.Fields = append(td.Fields, FieldDef{Name: b.Str(p.Name), Type: b.typeRef(p.Type)})
	}
	b.addType(td)
}

func (b *Builder) funcName(fn uint16) string {
	if int(fn) < len(b.funcs) {
		return b.funcs[fn].name
	}
	return ""
}

func (b *Builder) addType(td TypeDef) TypeID {
	if len(b.m.Types) >= math.MaxUint16-int(FirstUserType) {
		b.setErr("too many types")
		return 0
	}
	b.m.Types = append(b.m.Types, td)
	return FirstUserType + TypeID(len(b.m.Types)-1)
}

// Tool adds a TOOLS entry and returns its index. Adding the same name twice
// returns the first index (the hash must then agree).
func (b *Builder) Tool(name string, specSHA256 [32]byte) uint16 {
	idx := b.Str(name)
	for i, t := range b.m.Tools {
		if t.Name == idx {
			if t.SpecSHA256 != specSHA256 {
				b.setErr("tool %q added with two different spec hashes", name)
			}
			return uint16(i)
		}
	}
	if len(b.m.Tools) >= math.MaxUint16 {
		b.setErr("too many tools")
		return 0
	}
	b.m.Tools = append(b.m.Tools, Tool{Name: idx, SpecSHA256: specSHA256})
	return uint16(len(b.m.Tools) - 1)
}

// SetEffects sets the declared effect mask.
func (b *Builder) SetEffects(m EffectMask) { b.m.Effects = m }

// AddEffects adds bits to the declared effect mask.
func (b *Builder) AddEffects(m EffectMask) { b.m.Effects |= m }

// SetMeta sets the META section.
func (b *Builder) SetMeta(meta Meta) { b.m.Meta = meta }

// SetEntry sets the workflow body function.
func (b *Builder) SetEntry(fn uint16) { b.m.Entry = fn }

// Func declares a function and returns its body builder. Its index is
// fixed immediately (FuncBuilder.Index), so it can be referenced before
// its body is emitted. Locals 0..arity-1 are the arguments.
func (b *Builder) Func(name string, arity, ncaps uint8) *FuncBuilder {
	if len(b.funcs) > math.MaxUint16 {
		b.setErr("too many functions")
	}
	f := &FuncBuilder{
		b: b, index: uint16(len(b.funcs)), name: name, nameIdx: b.Str(name),
		arity: arity, ncaps: ncaps, nlocals: int(arity), pendingSpan: -1,
	}
	b.funcs = append(b.funcs, f)
	return f
}

// Build finalizes all functions (patching jumps) and returns the module.
// The builder must not be used afterwards. Build does not verify.
func (b *Builder) Build() (*Module, error) {
	for _, f := range b.funcs {
		if err := f.finish(); err != nil && b.err == nil {
			b.err = err
		}
	}
	if b.err != nil {
		return nil, b.err
	}
	b.m.Funcs = make([]Func, len(b.funcs))
	for i, f := range b.funcs {
		b.m.Funcs[i] = Func{
			Name: f.nameIdx, Arity: f.arity, NLocals: uint16(f.nlocals), NCaps: f.ncaps,
			Code: f.code, Spans: f.spans,
		}
	}
	m := b.m
	return &m, nil
}

// Label is a jump target inside one function.
type Label int

type fixup struct {
	at    int // offset of the i32 operand
	next  int // offset of the next instruction
	label Label
}

// FuncBuilder emits the code of one function.
type FuncBuilder struct {
	b           *Builder
	index       uint16
	name        string
	nameIdx     uint32
	arity       uint8
	ncaps       uint8
	nlocals     int
	code        []byte
	labels      []int // -1 while unbound
	fixups      []fixup
	spans       []Span
	pendingSpan int64 // packed source offset/len of the next span, -1 if none
	pendingLen  uint32
}

// Index returns the function index.
func (f *FuncBuilder) Index() uint16 { return f.index }

// Offset returns the current code offset.
func (f *FuncBuilder) Offset() int { return len(f.code) }

// Local allocates a fresh local slot and returns its index. See the
// package documentation for the single-assignment rules locals obey.
func (f *FuncBuilder) Local() uint16 {
	if f.nlocals >= math.MaxUint16 {
		f.b.setErr("function %q: too many locals", f.name)
		return 0
	}
	f.nlocals++
	return uint16(f.nlocals - 1)
}

// NumLocals returns the number of locals allocated so far.
func (f *FuncBuilder) NumLocals() int { return f.nlocals }

// Span attaches a source range to the next emitted instruction.
func (f *FuncBuilder) Span(sourceOffset, length uint32) {
	f.pendingSpan = int64(sourceOffset)
	f.pendingLen = length
}

// Emit appends one instruction. Operands are given as plain integers in
// the order of spec 8.4; their count and range are checked.
func (f *FuncBuilder) Emit(op Opcode, operands ...int64) {
	info := op.Info()
	if info == nil {
		f.b.setErr("function %q: unknown opcode 0x%02x", f.name, uint8(op))
		return
	}
	if len(operands) != len(info.Operands) {
		f.b.setErr("function %q: %s takes %d operands, got %d", f.name, info.Name, len(info.Operands), len(operands))
		return
	}
	if f.pendingSpan >= 0 {
		off := uint32(len(f.code))
		if n := len(f.spans); n > 0 && f.spans[n-1].CodeOffset == off {
			f.spans = f.spans[:n-1]
		}
		f.spans = append(f.spans, Span{CodeOffset: off, SourceOffset: uint32(f.pendingSpan), Len: f.pendingLen})
		f.pendingSpan = -1
	}
	f.code = append(f.code, byte(op))
	for i, w := range info.Operands {
		v := operands[i]
		switch w {
		case U8:
			if v < 0 || v > math.MaxUint8 {
				f.b.setErr("function %q: %s operand %d out of u8 range", f.name, info.Name, v)
			}
			f.code = append(f.code, byte(v))
		case U16:
			if v < 0 || v > math.MaxUint16 {
				f.b.setErr("function %q: %s operand %d out of u16 range", f.name, info.Name, v)
			}
			f.code = binary.LittleEndian.AppendUint16(f.code, uint16(v))
		case I32:
			if v < math.MinInt32 || v > math.MaxInt32 {
				f.b.setErr("function %q: %s operand %d out of i32 range", f.name, info.Name, v)
			}
			f.code = binary.LittleEndian.AppendUint32(f.code, uint32(int32(v)))
		}
	}
}

// Op emits an instruction without operands.
func (f *FuncBuilder) Op(ops ...Opcode) {
	for _, op := range ops {
		f.Emit(op)
	}
}

// ConstOp emits CONST for c, adding it to the pool.
func (f *FuncBuilder) ConstOp(c Const) {
	k := f.b.Const(c)
	if k > math.MaxUint16 {
		f.b.setErr("function %q: constant index %d exceeds u16", f.name, k)
	}
	f.Emit(OpConst, int64(k))
}

// Int emits CONST of an Int.
func (f *FuncBuilder) Int(v int64) { f.ConstOp(IntConst(v)) }

// Float emits CONST of a Float.
func (f *FuncBuilder) Float(v float64) { f.ConstOp(FloatConst(v)) }

// String emits CONST of a String.
func (f *FuncBuilder) String(s string) { f.ConstOp(StringConst(s)) }

// Duration emits CONST of a Duration in nanoseconds.
func (f *FuncBuilder) Duration(ns int64) { f.ConstOp(DurationConst(ns)) }

// Load emits LOAD_LOCAL i.
func (f *FuncBuilder) Load(i uint16) { f.Emit(OpLoadLocal, int64(i)) }

// Store emits STORE_LOCAL i.
func (f *FuncBuilder) Store(i uint16) { f.Emit(OpStoreLocal, int64(i)) }

// Cap emits LOAD_CAP i.
func (f *FuncBuilder) Cap(i uint8) { f.Emit(OpLoadCap, int64(i)) }

// Call emits CALL fn argc.
func (f *FuncBuilder) Call(fn uint16, argc uint8) { f.Emit(OpCall, int64(fn), int64(argc)) }

// CallClosure emits CALL_CLOSURE argc.
func (f *FuncBuilder) CallClosure(argc uint8) { f.Emit(OpCallClosure, int64(argc)) }

// Closure emits CLOSURE fn ncaps.
func (f *FuncBuilder) Closure(fn uint16, ncaps uint8) { f.Emit(OpClosure, int64(fn), int64(ncaps)) }

// Builtin emits BUILTIN id argc, using the table arity.
func (f *FuncBuilder) Builtin(id BuiltinID) {
	bi, ok := LookupBuiltin(id)
	if !ok {
		f.b.setErr("function %q: unknown builtin %d", f.name, id)
		return
	}
	f.Emit(OpBuiltin, int64(id), int64(bi.Arity))
}

// Effect emits EFFECT kind argc.
func (f *FuncBuilder) Effect(kind EffectKind, argc uint8) {
	f.Emit(OpEffect, int64(kind), int64(argc))
}

// ToolCall emits the tool_call sequence for nInputs inputs already on the
// stack: CONST tool_name; EFFECT tool_call (nInputs+1). The tool must be in
// the TOOLS table (Builder.Tool).
func (f *FuncBuilder) ToolCall(tool string, nInputs uint8) {
	if nInputs == math.MaxUint8 {
		f.b.setErr("function %q: too many tool inputs", f.name)
		return
	}
	f.String(tool)
	f.Effect(EffectToolCall, nInputs+1)
}

// Fail emits FAIL code.
func (f *FuncBuilder) Fail(code FailCode) { f.Emit(OpFail, int64(code)) }

// MakeRec emits MAKE_REC type n.
func (f *FuncBuilder) MakeRec(t TypeID, n uint8) { f.Emit(OpMakeRec, int64(t), int64(n)) }

// MakeEnum emits MAKE_ENUM type variant n.
func (f *FuncBuilder) MakeEnum(t TypeID, variant, n uint8) {
	f.Emit(OpMakeEnum, int64(t), int64(variant), int64(n))
}

// PushComp emits PUSH_COMP fn ncaps.
func (f *FuncBuilder) PushComp(fn uint16, ncaps uint8) { f.Emit(OpPushComp, int64(fn), int64(ncaps)) }

// NewLabel creates an unbound label.
func (f *FuncBuilder) NewLabel() Label {
	f.labels = append(f.labels, -1)
	return Label(len(f.labels) - 1)
}

// Bind binds l to the current offset.
func (f *FuncBuilder) Bind(l Label) {
	if int(l) < 0 || int(l) >= len(f.labels) {
		f.b.setErr("function %q: unknown label %d", f.name, l)
		return
	}
	if f.labels[l] >= 0 {
		f.b.setErr("function %q: label %d bound twice", f.name, l)
		return
	}
	f.labels[l] = len(f.code)
}

// Here creates a label bound to the current offset (a loop header).
func (f *FuncBuilder) Here() Label {
	l := f.NewLabel()
	f.Bind(l)
	return l
}

func (f *FuncBuilder) jump(op Opcode, l Label) {
	if int(l) < 0 || int(l) >= len(f.labels) {
		f.b.setErr("function %q: unknown label %d", f.name, l)
		return
	}
	at := len(f.code) + 1
	f.Emit(op, 0)
	f.fixups = append(f.fixups, fixup{at: at, next: len(f.code), label: l})
}

// Jump emits a forward JUMP to l.
func (f *FuncBuilder) Jump(l Label) { f.jump(OpJump, l) }

// JumpIfFalse emits a forward JUMP_IF_FALSE to l.
func (f *FuncBuilder) JumpIfFalse(l Label) { f.jump(OpJumpIfFalse, l) }

// Loop emits a backward LOOP to l, which must already be bound.
func (f *FuncBuilder) Loop(l Label) {
	if int(l) < 0 || int(l) >= len(f.labels) || f.labels[l] < 0 {
		f.b.setErr("function %q: LOOP to an unbound label", f.name)
		return
	}
	f.jump(OpLoop, l)
}

func (f *FuncBuilder) finish() error {
	for _, fx := range f.fixups {
		target := f.labels[fx.label]
		if target < 0 {
			return fmt.Errorf("%w: function %q: label %d never bound", ErrBuild, f.name, fx.label)
		}
		off := int64(target) - int64(fx.next)
		if off < math.MinInt32 || off > math.MaxInt32 {
			return fmt.Errorf("%w: function %q: jump too far", ErrBuild, f.name)
		}
		binary.LittleEndian.PutUint32(f.code[fx.at:], uint32(int32(off)))
	}
	f.fixups = nil
	return nil
}
