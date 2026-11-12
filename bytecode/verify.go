package bytecode

import (
	"encoding/binary"
	"fmt"
)

// ToolInfo is what the verifier needs to know about a registered tool.
type ToolInfo struct {
	SpecSHA256 [32]byte
	// Effects are the tool's declared effects (spec 7.5: calling a tool
	// contributes the tool's declared effects).
	Effects EffectMask
}

// ToolResolver looks tools up in the registry at load time.
type ToolResolver interface {
	ResolveTool(name string) (ToolInfo, bool)
}

// ToolMap is an in-memory ToolResolver.
type ToolMap map[string]ToolInfo

// ResolveTool implements ToolResolver.
func (t ToolMap) ResolveTool(name string) (ToolInfo, bool) {
	ti, ok := t[name]
	return ti, ok
}

// Verified is a module that passed Verify, together with the analysis
// results the VM relies on. It is immutable.
type Verified struct {
	mod   *Module
	hash  [32]byte
	funcs []FuncInfo
	reach []bool
	mask  EffectMask
}

// Module returns the verified module. Callers must not modify it.
func (v *Verified) Module() *Module { return v.mod }

// Hash returns the module hash (SHA-256 of the encoded module).
func (v *Verified) Hash() [32]byte { return v.hash }

// Func returns the analysis of function fn.
func (v *Verified) Func(fn int) *FuncInfo { return &v.funcs[fn] }

// Reachable reports whether function fn is reachable from the entry.
func (v *Verified) Reachable(fn int) bool { return v.reach[fn] }

// InferredEffects returns the union of the effects of all reachable EFFECT
// instructions.
func (v *Verified) InferredEffects() EffectMask { return v.mask }

// FuncInfo is the verifier's analysis of one function.
type FuncInfo struct {
	// MaxStack is the largest operand stack depth (locals excluded).
	MaxStack int
	depth    []int32
	resumes  map[uint32]Opcode
}

// DepthAt returns the abstract operand depth before the instruction at
// code offset pc, or -1 if pc is not a reachable instruction boundary.
func (fi *FuncInfo) DepthAt(pc int) int {
	if pc < 0 || pc >= len(fi.depth) {
		return -1
	}
	return int(fi.depth[pc])
}

// ResumeOp reports the opcode of the reachable CALL, CALL_CLOSURE or EFFECT
// instruction that ends at offset pc. Snapshots use it to validate saved
// return addresses.
func (fi *FuncInfo) ResumeOp(pc int) (Opcode, bool) {
	if pc < 0 {
		return 0, false
	}
	op, ok := fi.resumes[uint32(pc)]
	return op, ok
}

// maxVerifyWords bounds the bitset memory of one function's dataflow.
const maxVerifyWords = 1 << 24

// Verify checks every rule of spec 8.8 and returns the analysis the VM
// needs. tools resolves TOOLS entries; a nil resolver rejects any module
// that lists tools. Errors are *VerifyError wrapping a rule sentinel.
func Verify(m *Module, tools ToolResolver) (*Verified, error) {
	if m.FormatVersion != FormatVersion || m.Flags&^FlagAnnotations != 0 {
		return nil, &VerifyError{Rule: ErrFormatVersionFlag, Func: -1, Offset: -1}
	}
	if len(m.Funcs) == 0 || len(m.Funcs) > 1<<16 || len(m.Types) > int(^uint16(0)-uint16(FirstUserType)) {
		return nil, &VerifyError{Rule: ErrTooManyItems, Func: -1, Offset: -1}
	}
	if err := verifyNames(m); err != nil {
		return nil, err
	}
	if int(m.Entry) >= len(m.Funcs) || m.Funcs[m.Entry].NCaps != 0 {
		return nil, &VerifyError{Rule: ErrEntry, Func: -1, Offset: -1}
	}
	if err := verifyAnnotations(m); err != nil {
		return nil, err
	}
	toolFx, err := verifyTools(m, tools)
	if err != nil {
		return nil, err
	}
	if m.Effects&^EffAll != 0 {
		return nil, &VerifyError{Rule: ErrEffectBits, Func: -1, Offset: -1, Detail: m.Effects.String()}
	}

	v := &Verified{mod: m, funcs: make([]FuncInfo, len(m.Funcs)), reach: make([]bool, len(m.Funcs))}
	fx := make([]EffectMask, len(m.Funcs))
	refs := make([][]uint16, len(m.Funcs))
	for i := range m.Funcs {
		fv := &funcVerifier{m: m, fn: i, f: &m.Funcs[i], toolFx: toolFx}
		if err := fv.run(); err != nil {
			return nil, err
		}
		v.funcs[i] = FuncInfo{MaxStack: fv.maxDepth, depth: fv.depth, resumes: fv.resumes}
		fx[i] = fv.effects
		refs[i] = fv.refs
	}

	// Effects of all reachable functions.
	work := []uint16{m.Entry}
	v.reach[m.Entry] = true
	for len(work) > 0 {
		fn := work[len(work)-1]
		work = work[:len(work)-1]
		v.mask |= fx[fn]
		for _, r := range refs[fn] {
			if !v.reach[r] {
				v.reach[r] = true
				work = append(work, r)
			}
		}
	}
	if v.mask&^m.Effects != 0 {
		return nil, &VerifyError{Rule: ErrEffectMask, Func: -1, Offset: -1,
			Detail: fmt.Sprintf("declared %s, reachable %s", m.Effects, v.mask)}
	}
	h, err := m.Hash()
	if err != nil {
		return nil, fmt.Errorf("bytecode: hashing verified module: %w", err)
	}
	v.hash = h
	return v, nil
}

func verifyNames(m *Module) error {
	bad := func(what string, i int) error {
		return &VerifyError{Rule: ErrNameIndex, Func: -1, Offset: -1, Detail: fmt.Sprintf("%s %d", what, i)}
	}
	isStr := func(i uint32) bool { _, ok := m.ConstString(i); return ok }
	isType := func(i uint32) bool { return i == NoType || isStr(i) }
	for i, f := range m.Funcs {
		if !isStr(f.Name) {
			return bad("function", i)
		}
	}
	for i, t := range m.Tools {
		if !isStr(t.Name) {
			return bad("tool", i)
		}
	}
	for i, t := range m.Types {
		if !isStr(t.Name) {
			return bad("type", i)
		}
		for _, f := range t.Fields {
			if !isStr(f.Name) || !isType(f.Type) {
				return bad("type field of type", i)
			}
		}
		for _, vd := range t.Variants {
			if !isStr(vd.Name) {
				return bad("variant of type", i)
			}
			for _, p := range vd.Payload {
				if !isType(p) {
					return bad("variant payload of type", i)
				}
			}
		}
		if t.Kind == KindSignature {
			if !isType(t.Result) {
				return bad("signature result of type", i)
			}
			if int(t.Func) >= len(m.Funcs) {
				return &VerifyError{Rule: ErrSignature, Func: -1, Offset: -1, Detail: fmt.Sprintf("type %d", i)}
			}
		}
	}
	return nil
}

func verifyTools(m *Module, tools ToolResolver) (map[string]EffectMask, error) {
	out := make(map[string]EffectMask, len(m.Tools))
	for i, t := range m.Tools {
		name := m.str(t.Name)
		if _, dup := out[name]; dup {
			return nil, &VerifyError{Rule: ErrDuplicateTool, Func: -1, Offset: -1, Detail: name}
		}
		var info ToolInfo
		ok := false
		if tools != nil {
			info, ok = tools.ResolveTool(name)
		}
		if !ok {
			return nil, &VerifyError{Rule: ErrToolUnknown, Func: -1, Offset: -1, Detail: name}
		}
		if info.SpecSHA256 != t.SpecSHA256 {
			return nil, &VerifyError{Rule: ErrToolHash, Func: -1, Offset: -1, Detail: fmt.Sprintf("tool %d %q", i, name)}
		}
		out[name] = info.Effects
	}
	return out, nil
}

// bitset is a fixed-size set of local indices.
type bitset []uint64

func newBitset(n int) bitset    { return make(bitset, (n+63)/64) }
func (b bitset) has(i int) bool { return b[i>>6]&(1<<(uint(i)&63)) != 0 }
func (b bitset) set(i int)      { b[i>>6] |= 1 << (uint(i) & 63) }
func (b bitset) clone() bitset  { return append(bitset(nil), b...) }
func (b bitset) and(o bitset) {
	for i := range b {
		b[i] &= o[i]
	}
}
func (b bitset) or(o bitset) {
	for i := range b {
		b[i] |= o[i]
	}
}

// absState is the abstract machine state before an instruction: operand
// depth, locals written on every path (must) and on some path (may).
type absState struct {
	depth int
	must  bitset
	may   bitset
}

func (s *absState) clone() *absState {
	return &absState{depth: s.depth, must: s.must.clone(), may: s.may.clone()}
}

type funcVerifier struct {
	m      *Module
	fn     int
	f      *Func
	toolFx map[string]EffectMask

	boundary []bool
	target   []bool
	depth    []int32
	resumes  map[uint32]Opcode
	maxDepth int
	effects  EffectMask
	refs     []uint16
}

func (fv *funcVerifier) errAt(rule error, pc int, format string, args ...any) error {
	return &VerifyError{Rule: rule, Func: fv.fn, Offset: pc, Detail: fmt.Sprintf(format, args...)}
}

func u16at(code []byte, i int) int { return int(binary.LittleEndian.Uint16(code[i:])) }
func i32at(code []byte, i int) int { return int(int32(binary.LittleEndian.Uint32(code[i:]))) }

func (fv *funcVerifier) run() error {
	f, code := fv.f, fv.f.Code
	if int(f.NLocals) < int(f.Arity) {
		return fv.errAt(ErrLocalsArity, -1, "nlocals %d < arity %d", f.NLocals, f.Arity)
	}
	if len(code) == 0 {
		return fv.errAt(ErrNoTerminator, 0, "empty function")
	}
	// Pass 1: instruction boundaries.
	fv.boundary = make([]bool, len(code))
	last := -1
	for pc := 0; pc < len(code); {
		op := Opcode(code[pc])
		if !op.Valid() {
			return fv.errAt(ErrUnknownOpcode, pc, "0x%02x", code[pc])
		}
		if pc+op.Size() > len(code) {
			return fv.errAt(ErrTruncatedInstr, pc, "%s", op)
		}
		fv.boundary[pc] = true
		last = pc
		pc += op.Size()
	}
	switch Opcode(code[last]) {
	case OpRet, OpHalt, OpFail:
	default:
		return fv.errAt(ErrNoTerminator, last, "last instruction is %s", Opcode(code[last]))
	}
	// Pass 2: static operand checks for every instruction.
	fv.target = make([]bool, len(code))
	prev := -1
	for pc := 0; pc < len(code); pc += Opcode(code[pc]).Size() {
		if err := fv.static(pc, prev); err != nil {
			return err
		}
		prev = pc
	}
	// Pass 3: forward dataflow.
	return fv.dataflow()
}

func (fv *funcVerifier) static(pc, prev int) error {
	m, f, code := fv.m, fv.f, fv.f.Code
	op := Opcode(code[pc])
	next := pc + op.Size()
	switch op {
	case OpConst:
		if k := u16at(code, pc+1); k >= len(m.Consts) {
			return fv.errAt(ErrConstIndex, pc, "constant %d of %d", k, len(m.Consts))
		}
	case OpLoadLocal, OpStoreLocal:
		if i := u16at(code, pc+1); i >= int(f.NLocals) {
			return fv.errAt(ErrLocalIndex, pc, "local %d of %d", i, f.NLocals)
		}
	case OpLoadCap:
		if i := int(code[pc+1]); i >= int(f.NCaps) {
			return fv.errAt(ErrCapIndex, pc, "capture %d of %d", i, f.NCaps)
		}
	case OpJump, OpJumpIfFalse, OpLoop:
		t := next + i32at(code, pc+1)
		if t < 0 || t >= len(code) {
			return fv.errAt(ErrJumpOutOfRange, pc, "target %d", t)
		}
		if !fv.boundary[t] {
			return fv.errAt(ErrJumpMisaligned, pc, "target %d", t)
		}
		if op == OpLoop {
			if t > pc {
				return fv.errAt(ErrLoopForward, pc, "target %d", t)
			}
		} else if t < next {
			return fv.errAt(ErrBackwardJump, pc, "target %d", t)
		}
		fv.target[t] = true
	case OpCall:
		fn, argc := u16at(code, pc+1), int(code[pc+3])
		if fn >= len(m.Funcs) {
			return fv.errAt(ErrFuncIndex, pc, "function %d", fn)
		}
		if int(m.Funcs[fn].Arity) != argc {
			return fv.errAt(ErrCallArity, pc, "argc %d, arity %d", argc, m.Funcs[fn].Arity)
		}
		if m.Funcs[fn].NCaps != 0 {
			return fv.errAt(ErrCallCaptures, pc, "function %d", fn)
		}
	case OpClosure:
		fn, n := u16at(code, pc+1), int(code[pc+3])
		if fn >= len(m.Funcs) {
			return fv.errAt(ErrFuncIndex, pc, "function %d", fn)
		}
		if int(m.Funcs[fn].NCaps) != n {
			return fv.errAt(ErrClosureCaps, pc, "ncaps %d, function has %d", n, m.Funcs[fn].NCaps)
		}
	case OpPushComp:
		fn, n := u16at(code, pc+1), int(code[pc+3])
		if fn >= len(m.Funcs) {
			return fv.errAt(ErrFuncIndex, pc, "function %d", fn)
		}
		if int(m.Funcs[fn].NCaps) != n || m.Funcs[fn].Arity != 0 {
			return fv.errAt(ErrCompSignature, pc, "function %d", fn)
		}
	case OpMakeRec:
		t, n := TypeID(u16at(code, pc+1)), int(code[pc+3])
		l, ok := m.Layout(t)
		if !ok || l.Kind != KindRecord {
			return fv.errAt(ErrTypeIndex, pc, "type %d", t)
		}
		if !l.Variadic && len(l.Fields) != n {
			return fv.errAt(ErrFieldCount, pc, "n %d, record has %d fields", n, len(l.Fields))
		}
	case OpMakeEnum:
		t, vi, n := TypeID(u16at(code, pc+1)), int(code[pc+3]), int(code[pc+4])
		l, ok := m.Layout(t)
		if !ok || l.Kind != KindEnum {
			return fv.errAt(ErrTypeIndex, pc, "type %d", t)
		}
		if vi >= len(l.Variants) {
			return fv.errAt(ErrVariantIndex, pc, "variant %d of %d", vi, len(l.Variants))
		}
		if l.Arity(vi) != n {
			return fv.errAt(ErrPayloadArity, pc, "n %d, variant arity %d", n, l.Arity(vi))
		}
	case OpBuiltin:
		id, argc := BuiltinID(u16at(code, pc+1)), int(code[pc+3])
		b, ok := LookupBuiltin(id)
		if !ok {
			return fv.errAt(ErrUnknownBuiltin, pc, "builtin %d", id)
		}
		if b.Arity != argc {
			return fv.errAt(ErrBuiltinArity, pc, "%s: argc %d, arity %d", b.Name, argc, b.Arity)
		}
	case OpEffect:
		kind, argc := EffectKind(code[pc+1]), int(code[pc+2])
		if !kind.Valid() {
			return fv.errAt(ErrUnknownEffect, pc, "kind %d", kind)
		}
		if want := kind.Argc(); (want >= 0 && argc != want) || (want < 0 && argc < 1) {
			return fv.errAt(ErrEffectArgc, pc, "%s with %d arguments", kind, argc)
		}
		if kind == EffectToolCall {
			if prev < 0 || Opcode(code[prev]) != OpConst {
				return fv.errAt(ErrToolCallTarget, pc, "no CONST before tool_call")
			}
			name, ok := m.ConstString(uint32(u16at(code, prev+1)))
			if !ok {
				return fv.errAt(ErrToolCallTarget, pc, "constant is not a string")
			}
			if _, ok := fv.toolFx[name]; !ok {
				return fv.errAt(ErrToolCallTarget, pc, "tool %q not in TOOLS", name)
			}
		}
	case OpFail:
		if c := FailCode(u16at(code, pc+1)); !c.Valid() {
			return fv.errAt(ErrFailCode, pc, "code %d", c)
		}
	}
	return nil
}

// stackEffect returns how many values op pops and pushes.
func stackEffect(code []byte, pc int) (pop, push int) {
	switch op := Opcode(code[pc]); op {
	case OpNop, OpJump, OpLoop:
		return 0, 0
	case OpConst, OpUnit, OpTrue, OpFalse, OpLoadLocal, OpLoadCap, OpNewList:
		return 0, 1
	case OpPop, OpStoreLocal, OpJumpIfFalse:
		return 1, 0
	case OpDup:
		return 1, 2
	case OpAddI, OpSubI, OpMulI, OpDivI, OpModI, OpAddF, OpSubF, OpMulF, OpDivF, OpConcat,
		OpEq, OpNe, OpLt, OpLe, OpGt, OpGe, OpIndex, OpListPush, OpListGet:
		return 2, 1
	case OpNegI, OpNegF, OpNot, OpGetField, OpListLen, OpTry:
		return 1, 1
	case OpCall:
		return int(code[pc+3]), 1
	case OpCallClosure:
		return int(code[pc+1]) + 1, 1
	case OpRet, OpHalt:
		return 1, 0
	case OpClosure:
		return int(code[pc+3]), 1
	case OpMakeList:
		return u16at(code, pc+1), 1
	case OpMakeMap:
		return 2 * u16at(code, pc+1), 1
	case OpMakeRec:
		return int(code[pc+3]), 1
	case OpMakeEnum:
		return int(code[pc+4]), 1
	case OpTestVariant:
		return 1, 2
	case OpUnpack:
		return 1, int(code[pc+1])
	case OpInterp:
		return int(code[pc+1]), 1
	case OpBuiltin:
		return int(code[pc+3]), 1
	case OpEffect:
		return int(code[pc+2]), 1
	case OpPushComp:
		return int(code[pc+3]), 0
	case OpFail:
		return 0, 0
	}
	return 0, 0
}

func (fv *funcVerifier) dataflow() error {
	f, code := fv.f, fv.f.Code
	nl := int(f.NLocals)
	words := (nl + 63) / 64
	targets := 0
	for _, t := range fv.target {
		if t {
			targets++
		}
	}
	if (targets+2)*2*words > maxVerifyWords {
		return fv.errAt(ErrTooComplex, -1, "%d join points x %d locals", targets, nl)
	}
	fv.depth = make([]int32, len(code))
	for i := range fv.depth {
		fv.depth[i] = -1
	}
	fv.resumes = map[uint32]Opcode{}
	pending := make(map[int]*absState)
	cur := &absState{must: newBitset(nl), may: newBitset(nl)}
	for i := 0; i < int(f.Arity); i++ {
		cur.must.set(i)
		cur.may.set(i)
	}
	seenRef := map[uint16]bool{}
	addRef := func(fn int) {
		if !seenRef[uint16(fn)] {
			seenRef[uint16(fn)] = true
			fv.refs = append(fv.refs, uint16(fn))
		}
	}

	for pc := 0; pc < len(code); pc += Opcode(code[pc]).Size() {
		if p, ok := pending[pc]; ok {
			delete(pending, pc)
			if cur == nil {
				cur = p
			} else {
				if cur.depth != p.depth {
					return fv.errAt(ErrStackMismatch, pc, "depths %d and %d", cur.depth, p.depth)
				}
				cur.must.and(p.must)
				cur.may.or(p.may)
			}
		}
		if cur == nil {
			continue // unreachable
		}
		fv.depth[pc] = int32(cur.depth)
		op := Opcode(code[pc])
		next := pc + op.Size()

		switch op {
		case OpLoadLocal:
			if i := u16at(code, pc+1); !cur.must.has(i) {
				return fv.errAt(ErrLocalUnset, pc, "local %d", i)
			}
		case OpStoreLocal:
			i := u16at(code, pc+1)
			if cur.may.has(i) {
				return fv.errAt(ErrLocalReassigned, pc, "local %d", i)
			}
		case OpEffect:
			kind := EffectKind(code[pc+1])
			if kind == EffectToolCall {
				if fv.target[pc] {
					return fv.errAt(ErrToolCallTarget, pc, "tool_call is a jump target")
				}
				name, _ := fv.m.ConstString(uint32(u16at(code, pc-3+1)))
				fv.effects |= fv.toolFx[name]
			} else {
				fv.effects |= kind.Mask()
			}
		case OpCall, OpClosure, OpPushComp:
			addRef(u16at(code, pc+1))
		}

		pop, push := stackEffect(code, pc)
		if cur.depth < pop {
			return fv.errAt(ErrStackUnderflow, pc, "%s needs %d values, depth %d", op, pop, cur.depth)
		}
		cur.depth += push - pop
		if cur.depth > fv.maxDepth {
			fv.maxDepth = cur.depth
		}
		if cur.depth+nl > MaxStackDepth {
			return fv.errAt(ErrStackOverflow, pc, "depth %d with %d locals", cur.depth, nl)
		}
		if op == OpStoreLocal {
			i := u16at(code, pc+1)
			cur.must.set(i)
			cur.may.set(i)
		}
		switch op {
		case OpCall, OpCallClosure, OpEffect:
			fv.resumes[uint32(next)] = op
		}

		switch op {
		case OpJump, OpJumpIfFalse:
			t := next + i32at(code, pc+1)
			st := cur
			if op == OpJumpIfFalse {
				st = cur.clone()
			} else {
				cur = nil
			}
			if p, ok := pending[t]; ok {
				if p.depth != st.depth {
					return fv.errAt(ErrStackMismatch, t, "depths %d and %d", p.depth, st.depth)
				}
				p.must.and(st.must)
				p.may.or(st.may)
			} else {
				pending[t] = st
			}
		case OpLoop:
			t := next + i32at(code, pc+1)
			if fv.depth[t] < 0 {
				return fv.errAt(ErrLoopHeader, pc, "target %d", t)
			}
			if int(fv.depth[t]) != cur.depth {
				return fv.errAt(ErrStackMismatch, t, "loop header depth %d, back edge %d", fv.depth[t], cur.depth)
			}
			// The back edge contributes nothing to the header state: locals
			// first written inside the loop are fresh in every iteration.
			cur = nil
		case OpRet, OpHalt, OpFail:
			cur = nil
		}
	}
	if cur != nil || len(pending) > 0 {
		// Unreachable: the last instruction is terminal and every jump
		// target lies inside the code.
		return fv.errAt(ErrNoTerminator, len(code), "control falls off the end")
	}
	return nil
}
