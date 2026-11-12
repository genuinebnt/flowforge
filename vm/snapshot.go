package vm

import (
	"bytes"
	"fmt"
	"math"

	"github.com/genuinebnt/flowforge/bytecode"
)

// Snapshot format: deterministic CBOR (spec 8.7) of the array
//
//	[ version=1, module_hash, status, pc, frames, stack, compensation_stack,
//	  next_seq, heap, child, prefix, effects, fuel_used, heap_live,
//	  heap_alloc, pending, result, failure, fuel_credit ]
//
// frames are [fn, pc, bp, ret, caps_ref|null]; pc of the top frame is
// also stored on its own. Heap objects are listed once each, children
// before parents, and values refer to them by index, so sharing is kept
// and every reference points backwards (no cycles can be expressed):
//
//	[0, text]                 string
//	[1, [values...]]          list backing store
//	[2, [k1, v1, ...]]        map
//	[3, [values...]]          record fields, enum payload or captures
//	[4, target, decision_id]  scope proof
//
// Values: scalars as in the argument encoding; String-like [kind, ref]
// where a negative ref -1-k names constant k; List [7, stream, len, ref];
// Map [8, ref]; Record [9, type, ref]; Enum [10, type, variant, ref|null];
// Closure [11, fn, ref|null]; ScopeProof [17, ref].
const snapshotVersion = 1

const (
	objStr   = 0
	objList  = 1
	objMap   = 2
	objTup   = 3
	objProof = 4
)

type snapEncoder struct {
	ids  map[any]uint64
	heap []any
}

func (e *snapEncoder) ref(p any, build func() []any) uint64 {
	if id, ok := e.ids[p]; ok {
		return id
	}
	obj := build()
	id := uint64(len(e.heap))
	e.heap = append(e.heap, obj)
	e.ids[p] = id
	return id
}

func (e *snapEncoder) values(vs []Value) []any {
	out := make([]any, len(vs))
	for i, v := range vs {
		out[i] = e.value(v)
	}
	return out
}

func (e *snapEncoder) tup(t *tupObj) any {
	if t == nil {
		return nil
	}
	return e.ref(t, func() []any { return []any{uint64(objTup), e.values(t.vals)} })
}

func (e *snapEncoder) value(v Value) any {
	k := uint64(v.k)
	switch v.k {
	case KindUnit:
		return []any{k}
	case KindBool:
		return []any{k, v.AsBool()}
	case KindInt, KindDuration, KindTime:
		return []any{k, v.AsInt()}
	case KindFloat:
		return []any{k, v.AsFloat()}
	case KindList:
		l := v.p.(*listObj)
		id := e.ref(l, func() []any { return []any{uint64(objList), e.values(l.elems)} })
		return []any{k, uint64(v.f & flagStream), v.n, id}
	case KindMap:
		m := v.p.(*mapObj)
		id := e.ref(m, func() []any {
			flat := make([]any, 0, 2*len(m.keys))
			for i := range m.keys {
				flat = append(flat, e.value(m.keys[i]), e.value(m.vals[i]))
			}
			return []any{uint64(objMap), flat}
		})
		return []any{k, id}
	case KindRecord:
		t, _ := v.p.(*tupObj)
		return []any{k, uint64(v.TypeID()), e.tup(t)}
	case KindEnum:
		t, _ := v.p.(*tupObj)
		return []any{k, uint64(v.TypeID()), uint64(v.Variant()), e.tup(t)}
	case KindClosure:
		t, _ := v.p.(*tupObj)
		return []any{k, v.n, e.tup(t)}
	case KindScopeProof:
		p := v.p.(*proofObj)
		id := e.ref(p, func() []any { return []any{uint64(objProof), e.value(p.target), p.decision} })
		return []any{k, id}
	}
	s := v.p.(*strObj)
	if s.konst > 0 {
		return []any{k, -int64(s.konst)}
	}
	return []any{k, e.ref(s, func() []any { return []any{uint64(objStr), s.s} })}
}

// Snapshot serializes the complete VM state. Restoring it and continuing
// behaves exactly like continuing the original VM.
func (vm *VM) Snapshot() ([]byte, error) {
	e := &snapEncoder{ids: map[any]uint64{}, heap: []any{}}
	h := vm.prog.Hash()
	frames := make([]any, len(vm.frames))
	pc := uint64(0)
	live := vm.status == statusReady || vm.status == statusSuspended
	if !live {
		frames = frames[:0]
	}
	for i := range frames {
		fr := &vm.frames[i]
		frames[i] = []any{uint64(fr.fn), uint64(fr.pc), uint64(fr.bp), uint64(fr.ret), e.tup(fr.caps)}
		pc = uint64(fr.pc)
	}
	var stack []any
	if live {
		stack = e.values(vm.stack[:vm.sp])
	} else {
		stack = []any{}
	}
	comps := e.values(vm.comps)
	var pending, result, failure any
	if vm.status == statusSuspended {
		pending = []any{vm.pending.Seq, uint64(vm.pending.Kind), e.values(vm.pending.Args)}
	}
	if vm.status == statusCompleted {
		result = e.value(vm.result)
	}
	if vm.status == statusFailed {
		f := vm.failure
		failure = []any{uint64(f.Code), f.Message, int64(f.Span.Func), int64(f.Span.CodeOffset),
			uint64(f.Span.SourceOffset), uint64(f.Span.Len), f.Span.HasSource}
	}
	prefix := make([]any, len(vm.prefix))
	for i, c := range vm.prefix {
		prefix[i] = uint64(c)
	}
	doc := []any{
		uint64(snapshotVersion), h[:], uint64(vm.status), pc, frames, stack, comps,
		vm.nextSeq, e.heap, vm.child, prefix, uint64(vm.effects), uint64(vm.fuelUsed),
		uint64(vm.heapLive), uint64(vm.heapAlloc), pending, result, failure, uint64(vm.credit),
	}
	b, err := encMode.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("vm: encoding snapshot: %w", err)
	}
	return b, nil
}

type snapDecoder struct {
	vm   *VM
	heap []any // decoded objects: *strObj, *listObj, *mapObj, *tupObj, *proofObj
}

func snapErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSnapshot, fmt.Sprintf(format, args...))
}

func (d *snapDecoder) obj(x any, limit int) (any, error) {
	id, ok := asUint(x, uint64(limit))
	if !ok || id >= uint64(limit) {
		return nil, snapErr("bad heap reference")
	}
	return d.heap[id], nil
}

func (d *snapDecoder) tupRef(x any, limit int) (*tupObj, error) {
	if x == nil {
		return nil, nil
	}
	o, err := d.obj(x, limit)
	if err != nil {
		return nil, err
	}
	t, ok := o.(*tupObj)
	if !ok {
		return nil, snapErr("reference is not a field tuple")
	}
	return t, nil
}

func (d *snapDecoder) values(x any, limit int) ([]Value, error) {
	xs, ok := x.([]any)
	if !ok {
		return nil, snapErr("expected an array of values")
	}
	if len(xs) == 0 {
		return nil, nil
	}
	out := make([]Value, len(xs))
	for i, e := range xs {
		v, err := d.value(e, limit)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// value decodes a snapshot value whose heap references must be < limit.
func (d *snapDecoder) value(x any, limit int) (Value, error) {
	arr, ok := x.([]any)
	if !ok || len(arr) == 0 {
		return Value{}, snapErr("value is not a tagged array")
	}
	kn, ok := asUint(arr[0], numKinds-1)
	if !ok {
		return Value{}, snapErr("bad kind")
	}
	k := Kind(kn)
	wantLen := 2
	switch k {
	case KindUnit:
		wantLen = 1
	case KindRecord, KindClosure:
		wantLen = 3
	case KindList, KindEnum:
		wantLen = 4
	}
	if len(arr) != wantLen {
		return Value{}, snapErr("%s with %d fields", k, len(arr))
	}
	m := d.vm.mod
	switch k {
	case KindUnit, KindBool, KindInt, KindDuration, KindTime, KindFloat:
		td := treeDecoder{mod: m}
		v, err := td.value(x, 0)
		if err != nil {
			return Value{}, fmt.Errorf("%w: %v", ErrSnapshot, err)
		}
		return v, nil
	case KindList:
		flag, ok1 := asUint(arr[1], 1)
		n, ok2 := asUint(arr[2], uint64(d.vm.lim.List))
		o, err := d.obj(arr[3], limit)
		if err != nil {
			return Value{}, err
		}
		l, ok3 := o.(*listObj)
		if !ok1 || !ok2 || !ok3 || n > uint64(len(l.elems)) {
			return Value{}, snapErr("bad List")
		}
		return Value{k: KindList, f: uint8(flag), n: n, p: l}, nil
	case KindMap:
		o, err := d.obj(arr[1], limit)
		if err != nil {
			return Value{}, err
		}
		mo, ok := o.(*mapObj)
		if !ok {
			return Value{}, snapErr("bad Map")
		}
		return Value{k: KindMap, p: mo}, nil
	case KindRecord, KindEnum, KindClosure:
		id, ok := asUint(arr[1], math.MaxUint16)
		if !ok {
			return Value{}, snapErr("bad %s header", k)
		}
		var variant uint64
		if k == KindEnum {
			if variant, ok = asUint(arr[2], math.MaxUint8); !ok {
				return Value{}, snapErr("bad variant")
			}
		}
		t, err := d.tupRef(arr[len(arr)-1], limit)
		if err != nil {
			return Value{}, err
		}
		n := 0
		if t != nil {
			n = len(t.vals)
		}
		if k == KindRecord && t == nil {
			return Value{}, snapErr("record without fields object")
		}
		if (k == KindEnum || k == KindClosure) && t != nil && n == 0 {
			return Value{}, snapErr("empty payload object")
		}
		if err := checkShape(m, k, id, variant, n); err != nil {
			return Value{}, fmt.Errorf("%w: %v", ErrSnapshot, err)
		}
		v := Value{k: k, n: id}
		if k == KindEnum {
			v.n = id<<8 | variant
		}
		if t != nil {
			v.p = t
		}
		return v, nil
	case KindScopeProof:
		o, err := d.obj(arr[1], limit)
		if err != nil {
			return Value{}, err
		}
		p, ok := o.(*proofObj)
		if !ok {
			return Value{}, snapErr("bad ScopeProof")
		}
		return Value{k: KindScopeProof, p: p}, nil
	}
	if !k.isStringy() {
		return Value{}, snapErr("bad kind %d", k)
	}
	if ci, ok := arr[1].(int64); ok {
		ki := -ci - 1
		if ci >= 0 || ki >= int64(len(d.vm.consts)) || d.vm.consts[ki].k != KindString {
			return Value{}, snapErr("bad constant reference")
		}
		if k != KindString {
			return Value{}, snapErr("constant reference for %s", k)
		}
		return d.vm.consts[ki], nil
	}
	o, err := d.obj(arr[1], limit)
	if err != nil {
		return Value{}, err
	}
	s, ok := o.(*strObj)
	if !ok {
		return Value{}, snapErr("bad %s reference", k)
	}
	if k.IsDomain() {
		if c, err := canonicalDomain(k, s.s); err != nil || c != s.s {
			return Value{}, snapErr("%s %q is not canonical", k, s.s)
		}
	}
	return Value{k: k, p: s}, nil
}

func (d *snapDecoder) object(x any, id int) (any, error) {
	arr, ok := x.([]any)
	if !ok || len(arr) < 2 {
		return nil, snapErr("bad heap object %d", id)
	}
	tag, ok := asUint(arr[0], objProof)
	if !ok {
		return nil, snapErr("bad heap object tag")
	}
	switch tag {
	case objStr:
		s, ok := asText(arr[1])
		if !ok || len(arr) != 2 || len(s) > d.vm.lim.String {
			return nil, snapErr("bad string object")
		}
		return &strObj{s: s}, nil
	case objList, objTup:
		vs, err := d.values(arr[1], id)
		if err != nil {
			return nil, err
		}
		if len(arr) != 2 || len(vs) > d.vm.lim.List {
			return nil, snapErr("bad list object")
		}
		if tag == objList {
			return &listObj{elems: vs}, nil
		}
		if len(vs) > math.MaxUint8 {
			return nil, snapErr("field tuple too long")
		}
		return &tupObj{vals: vs}, nil
	case objMap:
		flat, err := d.values(arr[1], id)
		if err != nil {
			return nil, err
		}
		if len(arr) != 2 || len(flat)%2 != 0 || len(flat)/2 > d.vm.lim.List {
			return nil, snapErr("bad map object")
		}
		m := &mapObj{index: make(map[string]int, len(flat)/2)}
		for i := 0; i < len(flat); i += 2 {
			ks := keyString(flat[i])
			if _, dup := m.index[ks]; dup {
				return nil, snapErr("duplicate map key")
			}
			m.index[ks] = len(m.keys)
			m.keys = append(m.keys, flat[i])
			m.vals = append(m.vals, flat[i+1])
		}
		return m, nil
	case objProof:
		if len(arr) != 3 {
			return nil, snapErr("bad proof object")
		}
		t, err := d.value(arr[1], id)
		if err != nil {
			return nil, err
		}
		dec, ok := asText(arr[2])
		if !ok {
			return nil, snapErr("bad decision id")
		}
		return &proofObj{target: t, decision: dec}, nil
	}
	return nil, snapErr("bad heap object tag %d", tag)
}

// Restore rebuilds a VM from a snapshot of a VM running prog. Every field
// is validated: heap references, value shapes against the module, frame
// layout against the verifier's stack-depth analysis and limits, so a
// corrupt or hostile snapshot is rejected rather than run.
func Restore(prog *bytecode.Verified, data []byte, cfg Config) (*VM, error) {
	vm, err := newVM(prog, cfg)
	if err != nil {
		return nil, err
	}
	var doc []any
	if err := snapDecMode.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSnapshot, err)
	}
	if !bytes.Equal(mustReencode(doc), data) {
		return nil, snapErr("not canonical CBOR")
	}
	if len(doc) != 19 {
		return nil, snapErr("expected 19 fields, got %d", len(doc))
	}
	if v, ok := asUint(doc[0], math.MaxUint16); !ok || v != snapshotVersion {
		return nil, snapErr("unsupported version")
	}
	h := prog.Hash()
	if hb, ok := doc[1].([]byte); !ok || !bytes.Equal(hb, h[:]) {
		return nil, ErrModuleMismatch
	}
	st, ok := asUint(doc[2], uint64(statusFailed))
	if !ok {
		return nil, snapErr("bad status")
	}
	vm.status = status(st)
	d := &snapDecoder{vm: vm}

	heapArr, ok := doc[8].([]any)
	if !ok {
		return nil, snapErr("bad heap")
	}
	d.heap = make([]any, len(heapArr))
	for i, x := range heapArr {
		o, err := d.object(x, i)
		if err != nil {
			return nil, err
		}
		d.heap[i] = o
	}
	all := len(d.heap)

	child, ok := doc[9].(bool)
	if !ok {
		return nil, snapErr("bad child flag")
	}
	vm.child = child
	prefix, ok := doc[10].([]any)
	if !ok || (!child && len(prefix) != 0) || len(prefix) > 64 {
		return nil, snapErr("bad prefix")
	}
	for _, c := range prefix {
		u, ok := asUint(c, math.MaxUint32)
		if !ok {
			return nil, snapErr("bad prefix component")
		}
		vm.prefix = append(vm.prefix, uint32(u))
	}
	nums := [6]uint64{}
	for i, idx := range []int{7, 11, 12, 13, 14, 18} {
		u, ok := asUint(doc[idx], math.MaxInt64)
		if !ok {
			return nil, snapErr("bad counter %d", idx)
		}
		nums[i] = u
	}
	vm.nextSeq, vm.effects, vm.fuelUsed = nums[0], int(min(nums[1], math.MaxInt32)), int64(nums[2])
	vm.heapLive, vm.heapAlloc, vm.credit = int64(nums[3]), int64(nums[4]), int64(nums[5])
	if vm.credit != 0 && vm.status != statusReady {
		return nil, snapErr("fuel credit on a VM that is not runnable")
	}
	if vm.effects > vm.lim.Effects || uint64(vm.effects) > vm.nextSeq {
		return nil, snapErr("effect counter over the limit")
	}

	stack, err := d.values(doc[5], all)
	if err != nil {
		return nil, err
	}
	if vm.comps, err = d.values(doc[6], all); err != nil {
		return nil, err
	}
	for _, c := range vm.comps {
		if c.k != KindClosure || vm.funcs[c.n].arity != 0 {
			return nil, snapErr("compensation is not a zero-argument closure")
		}
	}

	switch vm.status {
	case statusNew:
		if len(stack) != 0 || !isEmpty(doc[4]) || vm.child {
			return nil, snapErr("new VM with state")
		}
	case statusReady, statusSuspended:
		if err := d.frames(doc[4], doc[3], stack, all); err != nil {
			return nil, err
		}
	case statusCompleted:
		if vm.result, err = d.value(doc[16], all); err != nil {
			return nil, err
		}
	case statusFailed:
		if err := d.failure(doc[17]); err != nil {
			return nil, err
		}
	}
	if vm.status == statusSuspended {
		if err := d.pendingEffect(doc[15], all); err != nil {
			return nil, err
		}
	} else if doc[15] != nil {
		return nil, snapErr("pending effect on a VM that is not suspended")
	}
	return vm, nil
}

func isEmpty(x any) bool {
	a, ok := x.([]any)
	return ok && len(a) == 0
}

func (d *snapDecoder) pendingEffect(x any, all int) error {
	vm := d.vm
	arr, ok := x.([]any)
	if !ok || len(arr) != 3 {
		return snapErr("bad pending effect")
	}
	seq, ok1 := asUint(arr[0], math.MaxUint32)
	kind, ok2 := asUint(arr[1], math.MaxUint8)
	if !ok1 || !ok2 || !bytecode.EffectKind(kind).Valid() || seq+1 != vm.nextSeq {
		return snapErr("bad pending effect header")
	}
	args, err := d.values(arr[2], all)
	if err != nil {
		return err
	}
	if args == nil {
		args = []Value{}
	}
	vm.pending = &Suspended{Path: vm.prefix.Child(uint32(seq)), Seq: seq, Kind: bytecode.EffectKind(kind), Args: args}
	if n := len(vm.frames); n > 0 {
		// The saved pc of the top frame is just past the EFFECT (3 bytes).
		fr := vm.frames[n-1]
		vm.pending.Site = vm.span(int(fr.fn), fr.pc-3)
	}
	return nil
}

func (d *snapDecoder) failure(x any) error {
	arr, ok := x.([]any)
	if !ok || len(arr) != 7 {
		return snapErr("bad failure")
	}
	code, ok1 := asUint(arr[0], math.MaxUint16)
	msg, ok2 := asText(arr[1])
	fn, ok3 := asInt(arr[2])
	off, ok4 := asInt(arr[3])
	so, ok5 := asUint(arr[4], math.MaxUint32)
	ln, ok6 := asUint(arr[5], math.MaxUint32)
	hs, ok7 := arr[6].(bool)
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && ok7) || !bytecode.FailCode(code).Valid() ||
		fn < -1 || fn >= int64(len(d.vm.funcs)) || off < -1 || off > math.MaxInt32 {
		return snapErr("bad failure fields")
	}
	d.vm.failure = Failed{Code: bytecode.FailCode(code), Message: msg,
		Span: Span{Func: int(fn), CodeOffset: int(off), SourceOffset: uint32(so), Len: uint32(ln), HasSource: hs}}
	return nil
}

// frames validates the call stack against the verifier's analysis.
func (d *snapDecoder) frames(x, topPC any, stack []Value, all int) error {
	vm := d.vm
	arr, ok := x.([]any)
	if !ok || len(arr) == 0 || len(arr) > vm.lim.CallDepth {
		return snapErr("bad frame count")
	}
	if len(stack) > vm.lim.Stack {
		return snapErr("stack over the limit")
	}
	for i, fx := range arr {
		fa, ok := fx.([]any)
		if !ok || len(fa) != 5 {
			return snapErr("bad frame %d", i)
		}
		var nums [4]uint64
		for j := 0; j < 4; j++ {
			if nums[j], ok = asUint(fa[j], math.MaxInt32); !ok {
				return snapErr("bad frame %d field %d", i, j)
			}
		}
		if nums[0] >= uint64(len(vm.funcs)) {
			return snapErr("frame %d: bad function", i)
		}
		caps, err := d.tupRef(fa[4], all)
		if err != nil {
			return err
		}
		vm.frames = append(vm.frames, frame{fn: uint16(nums[0]), pc: int(nums[1]), bp: int(nums[2]), ret: int(nums[3]), caps: caps})
	}
	top := &vm.frames[len(vm.frames)-1]
	if pc, ok := asUint(topPC, math.MaxInt32); !ok || int(pc) != top.pc {
		return snapErr("pc differs from the top frame")
	}
	for i := range vm.frames {
		fr := &vm.frames[i]
		fi := &vm.funcs[fr.fn]
		info := vm.prog.Func(int(fr.fn))
		ncaps := 0
		if fr.caps != nil {
			ncaps = len(fr.caps.vals)
		}
		if ncaps != fi.ncaps {
			return snapErr("frame %d: capture count", i)
		}
		if fr.bp+fi.nlocals+fi.maxStack > vm.lim.Stack || fr.bp+fi.nlocals > len(stack) {
			return snapErr("frame %d: stack layout", i)
		}
		if i == 0 {
			if fr.bp != 0 || fr.ret != 0 || (!vm.child && fr.fn != vm.mod.Entry) {
				return snapErr("bad bottom frame")
			}
		} else {
			caller := &vm.frames[i-1]
			cfi := &vm.funcs[caller.fn]
			op, ok := vm.prog.Func(int(caller.fn)).ResumeOp(caller.pc)
			base := caller.bp + cfi.nlocals
			switch {
			case ok && op == bytecode.OpCall:
				callFn := rd16(cfi.code, caller.pc-3)
				if fr.ret != fr.bp || int(fr.fn) != callFn || fr.caps != nil {
					return snapErr("frame %d: does not match its CALL", i)
				}
			case ok && op == bytecode.OpCallClosure:
				if fr.ret != fr.bp-1 || fi.arity != int(cfi.code[caller.pc-1]) {
					return snapErr("frame %d: does not match its CALL_CLOSURE", i)
				}
			default:
				return snapErr("frame %d: caller is not at a call", i)
			}
			if fr.ret < base || fr.ret-base != vm.prog.Func(int(caller.fn)).DepthAt(caller.pc)-1 {
				return snapErr("frame %d: caller stack depth", i)
			}
		}
		if i == len(vm.frames)-1 {
			depth := info.DepthAt(fr.pc)
			if vm.status == statusSuspended {
				if op, ok := info.ResumeOp(fr.pc); !ok || op != bytecode.OpEffect {
					return snapErr("suspended VM is not after an EFFECT")
				}
				depth--
			}
			if depth < 0 || len(stack)-(fr.bp+fi.nlocals) != depth {
				return snapErr("top frame stack depth")
			}
		}
	}
	vm.stack = make([]Value, max(len(stack), top.bp+vm.funcs[top.fn].nlocals+vm.funcs[top.fn].maxStack, 16))
	copy(vm.stack, stack)
	vm.sp = len(stack)
	return nil
}

var snapDecMode = mustDecMode(16)

func mustReencode(doc []any) []byte {
	b, err := encMode.Marshal(doc)
	if err != nil {
		return nil
	}
	return b
}
