package vm

import (
	"fmt"
	"math"

	"github.com/genuinebnt/flowforge/bytecode"
)

type status uint8

const (
	statusNew       status = iota // created, Start not yet called
	statusReady                   // runnable (started, or stopped by OutOfFuel)
	statusSuspended               // waiting for Resume
	statusCompleted
	statusFailed
)

type frame struct {
	fn   uint16
	pc   int     // next instruction (saved return address for callers)
	bp   int     // first local slot
	ret  int     // slot that receives the result
	caps *tupObj // captures of the running closure, nil for plain calls
}

type funcInfo struct {
	code     []byte
	arity    int
	nlocals  int
	ncaps    int
	maxStack int
	spans    []bytecode.Span
}

// VM is one execution of a verified module: the workflow body of a run, a
// par_map child or a compensation. A VM is not safe for concurrent use;
// separate VMs are independent.
type VM struct {
	prog   *bytecode.Verified
	mod    *bytecode.Module
	funcs  []funcInfo
	consts []Value
	cfg    Config
	lim    Limits

	stack  []Value
	sp     int
	frames []frame
	comps  []Value

	status  status
	pending *Suspended
	result  Value
	failure Failed

	child   bool // par_map child or compensation: TRY never fails the run
	prefix  Path
	nextSeq uint64
	effects int

	fuelUsed int64

	heapLive  int64 // live bytes at the last measurement
	heapAlloc int64 // bytes allocated since the last measurement
}

func newVM(prog *bytecode.Verified, cfg Config) (*VM, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	m := prog.Module()
	vm := &VM{prog: prog, mod: m, cfg: cfg, lim: cfg.Limits}
	vm.funcs = make([]funcInfo, len(m.Funcs))
	for i := range m.Funcs {
		f := &m.Funcs[i]
		vm.funcs[i] = funcInfo{
			code: f.Code, arity: int(f.Arity), nlocals: int(f.NLocals), ncaps: int(f.NCaps),
			maxStack: prog.Func(i).MaxStack, spans: f.Spans,
		}
	}
	vm.consts = make([]Value, len(m.Consts))
	for i, c := range m.Consts {
		switch c.Tag {
		case bytecode.ConstInt:
			vm.consts[i] = Int(c.Int)
		case bytecode.ConstDuration:
			vm.consts[i] = Duration(c.Int)
		case bytecode.ConstTime:
			vm.consts[i] = Time(c.Int)
		case bytecode.ConstFloat:
			vm.consts[i] = Float(c.Float)
		case bytecode.ConstString:
			vm.consts[i] = Value{k: KindString, p: &strObj{s: c.Str, konst: int32(i) + 1}}
		}
	}
	vm.frames = make([]frame, 0, 8)
	return vm, nil
}

// New creates the VM for the workflow body of a run. Call Start next.
func New(prog *bytecode.Verified, cfg Config) (*VM, error) {
	return newVM(prog, cfg)
}

// Start pushes the entry frame with the workflow inputs as its arguments.
// The VM then runs with Run.
func (vm *VM) Start(entryArgs []Value) error {
	if vm.status != statusNew {
		return fmt.Errorf("%w: Start called twice", ErrState)
	}
	entry := vm.mod.Entry
	if len(entryArgs) != vm.funcs[entry].arity {
		return fmt.Errorf("%w: entry takes %d, got %d", ErrEntryArgs, vm.funcs[entry].arity, len(entryArgs))
	}
	return vm.enter(entry, nil, entryArgs)
}

// NewChild creates a VM that calls closure with args: a par_map element
// (args = [element]) or a compensation (no args). Its effects get paths
// prefix/0, prefix/1, ... For a par_map child of effect P the engine passes
// prefix = P.Child(taskIndex), so effects read P/task/child_seq (spec 8.6).
// In a child, TRY on Err or None returns from the closure instead of
// failing the run.
func NewChild(prog *bytecode.Verified, closure Value, args []Value, prefix Path, cfg Config) (*VM, error) {
	vm, err := newVM(prog, cfg)
	if err != nil {
		return nil, err
	}
	if closure.k != KindClosure || int(closure.n) >= len(vm.funcs) {
		return nil, fmt.Errorf("%w: not a closure of this module", ErrBadValue)
	}
	fi := &vm.funcs[closure.n]
	caps, _ := closure.p.(*tupObj)
	ncaps := 0
	if caps != nil {
		ncaps = len(caps.vals)
	}
	if ncaps != fi.ncaps {
		return nil, fmt.Errorf("%w: closure has %d captures, function needs %d", ErrBadValue, ncaps, fi.ncaps)
	}
	if len(args) != fi.arity {
		return nil, fmt.Errorf("%w: closure takes %d, got %d", ErrEntryArgs, fi.arity, len(args))
	}
	vm.child = true
	vm.prefix = append(Path(nil), prefix...)
	if caps != nil {
		c, err := vm.importValue(closure)
		if err != nil {
			return nil, err
		}
		caps = c.p.(*tupObj)
	}
	if err := vm.enter(uint16(closure.n), caps, args); err != nil {
		return nil, err
	}
	return vm, nil
}

func (vm *VM) enter(fn uint16, caps *tupObj, args []Value) error {
	fi := &vm.funcs[fn]
	imported := make([]Value, len(args))
	for i, a := range args {
		c, err := vm.importValue(a)
		if err != nil {
			return err
		}
		imported[i] = c
	}
	args = imported
	need := fi.nlocals + fi.maxStack
	if need > vm.lim.Stack {
		return fmt.Errorf("%w: entry frame needs %d stack slots", ErrConfig, need)
	}
	vm.stack = make([]Value, max(need, 16))
	copy(vm.stack, args)
	vm.sp = fi.nlocals
	vm.frames = append(vm.frames[:0], frame{fn: fn, caps: caps})
	vm.status = statusReady
	var size int64
	for _, a := range args {
		size += valueSize(a)
	}
	if caps != nil {
		size += valueSize(Value{k: KindClosure, p: caps})
	}
	if size > vm.lim.Heap {
		return fmt.Errorf("%w: arguments exceed the heap limit", ErrBadValue)
	}
	vm.heapAlloc = size
	return nil
}

// Run executes until the VM suspends, completes, fails or uses fuel units
// of fuel. It is an error to call Run on a suspended or finished VM.
func (vm *VM) Run(fuel int64) (Outcome, error) {
	switch vm.status {
	case statusReady:
		return vm.exec(fuel), nil
	case statusSuspended:
		return nil, fmt.Errorf("%w: suspended; call Resume", ErrState)
	case statusNew:
		return nil, fmt.Errorf("%w: not started", ErrState)
	}
	return vm.Outcome(), nil
}

// Resume delivers the result of the pending effect and continues running
// with fuel units of fuel. A failed result fails the run.
func (vm *VM) Resume(res EffectResult, fuel int64) (Outcome, error) {
	if vm.status != statusSuspended {
		return nil, fmt.Errorf("%w: no pending effect", ErrState)
	}
	vm.pending = nil
	if res.Failed {
		code := res.Code
		if !code.Valid() {
			code = bytecode.FailEffectFailed
		}
		return vm.failAt(code, res.Message), nil
	}
	val, err := vm.importValue(res.Value)
	if err != nil {
		return vm.failAt(bytecode.FailEffectFailed, "invalid effect result: "+err.Error()), nil
	}
	if f := vm.alloc(valueSize(val)); f != nil {
		return vm.failAt(f.code, f.msg), nil
	}
	vm.stack[vm.sp] = val
	vm.sp++
	vm.status = statusReady
	return vm.exec(fuel), nil
}

// Outcome returns the current state as an outcome: the pending effect, the
// final value or failure, or OutOfFuel for a runnable VM.
func (vm *VM) Outcome() Outcome {
	switch vm.status {
	case statusSuspended:
		return *vm.pending
	case statusCompleted:
		return Completed{Value: vm.result}
	case statusFailed:
		return vm.failure
	}
	return OutOfFuel{}
}

// Pending returns the effect the VM is suspended on.
func (vm *VM) Pending() (Suspended, bool) {
	if vm.status != statusSuspended {
		return Suspended{}, false
	}
	return *vm.pending, true
}

// Done reports whether the VM completed or failed.
func (vm *VM) Done() bool { return vm.status == statusCompleted || vm.status == statusFailed }

// FuelUsed returns the fuel consumed so far.
func (vm *VM) FuelUsed() int64 { return vm.fuelUsed }

// NextSeq returns the sequence number the next effect will get.
func (vm *VM) NextSeq() uint64 { return vm.nextSeq }

// Prefix returns the effect path prefix of a child VM (empty for a root).
func (vm *VM) Prefix() Path { return append(Path(nil), vm.prefix...) }

// Program returns the verified module the VM runs.
func (vm *VM) Program() *bytecode.Verified { return vm.prog }

// Compensations returns the registered compensation closures in the order
// they must run: most recently registered first (spec 7.5, 9.6). Each takes
// no arguments; the engine runs each with NewChild.
func (vm *VM) Compensations() []Value {
	out := make([]Value, len(vm.comps))
	for i, c := range vm.comps {
		out[len(vm.comps)-1-i] = c
	}
	return out
}

// Cancel fails a VM that has not finished with the given code (usually
// cancelled or timeout), dropping any pending effect. The compensation
// stack stays available.
func (vm *VM) Cancel(code bytecode.FailCode, msg string) Outcome {
	if vm.Done() {
		return vm.Outcome()
	}
	if !code.Valid() {
		code = bytecode.FailCancelled
	}
	vm.pending = nil
	return vm.failAt(code, msg)
}

// failAt fails the VM at the current instruction of the top frame.
func (vm *VM) failAt(code bytecode.FailCode, msg string) Failed {
	sp := Span{Func: -1, CodeOffset: -1}
	if n := len(vm.frames); n > 0 {
		fr := &vm.frames[n-1]
		sp = vm.span(int(fr.fn), fr.pc)
	}
	return vm.failSpan(code, msg, sp)
}

func (vm *VM) failSpan(code bytecode.FailCode, msg string, sp Span) Failed {
	if msg == "" {
		msg = code.String()
	}
	vm.failure = Failed{Code: code, Message: msg, Span: sp}
	vm.status = statusFailed
	vm.pending = nil
	return vm.failure
}

func (vm *VM) span(fn, pc int) Span {
	s := Span{Func: fn, CodeOffset: pc}
	spans := vm.funcs[fn].spans
	lo, hi := 0, len(spans)
	for lo < hi {
		mid := (lo + hi) / 2
		if int(spans[mid].CodeOffset) <= pc {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo > 0 {
		e := spans[lo-1]
		s.SourceOffset, s.Len, s.HasSource = e.SourceOffset, e.Len, true
	}
	return s
}

// importValue validates a value that enters the VM from outside (entry
// arguments, closure captures of a child, effect results) and returns a
// fresh, unshared copy: finite floats, known type ids with matching arity,
// closures of this module, and the size limits. Copying makes the heap
// accounting independent of how the caller built the value, so a live run
// and its replay (which decodes recorded bytes) account identically.
func (vm *VM) importValue(v Value) (Value, error) {
	return vm.importDepth(v, 0)
}

const maxValueDepth = 10_000

func (vm *VM) importSlice(vs []Value, depth int) ([]Value, error) {
	if len(vs) == 0 {
		return nil, nil
	}
	out := make([]Value, len(vs))
	for i, e := range vs {
		c, err := vm.importDepth(e, depth+1)
		if err != nil {
			return nil, err
		}
		out[i] = c
	}
	return out, nil
}

func (vm *VM) importDepth(v Value, depth int) (Value, error) {
	if depth > maxValueDepth {
		return Value{}, fmt.Errorf("%w: nested too deeply", ErrBadValue)
	}
	if v.k.isStringy() {
		s, ok := v.p.(*strObj)
		if !ok {
			return Value{}, fmt.Errorf("%w: %s without text", ErrBadValue, v.k)
		}
		if len(s.s) > vm.lim.String {
			return Value{}, fmt.Errorf("%w: string over limit", ErrBadValue)
		}
		return Value{k: v.k, p: &strObj{s: s.s}}, nil
	}
	switch v.k {
	case KindUnit, KindBool, KindInt, KindDuration, KindTime:
		return v, nil
	case KindFloat:
		if f := v.AsFloat(); math.IsNaN(f) || math.IsInf(f, 0) {
			return Value{}, fmt.Errorf("%w: non-finite float", ErrBadValue)
		}
		return v, nil
	case KindList:
		if v.Len() > vm.lim.List {
			return Value{}, fmt.Errorf("%w: list over limit", ErrBadValue)
		}
		elems, err := vm.importSlice(v.Elems(), depth)
		if err != nil {
			return Value{}, err
		}
		out := mkList(elems)
		out.f = v.f & flagStream
		return out, nil
	case KindMap:
		if v.Len() > vm.lim.List {
			return Value{}, fmt.Errorf("%w: map over limit", ErrBadValue)
		}
		m := v.p.(*mapObj)
		keys, err := vm.importSlice(m.keys, depth)
		if err != nil {
			return Value{}, err
		}
		vals, err := vm.importSlice(m.vals, depth)
		if err != nil {
			return Value{}, err
		}
		return Map(keys, vals)
	case KindRecord, KindEnum:
		l, ok := vm.mod.Layout(v.TypeID())
		if !ok {
			return Value{}, fmt.Errorf("%w: unknown type id %d", ErrBadValue, v.TypeID())
		}
		n := len(v.Fields())
		if v.k == KindRecord {
			if l.Kind != bytecode.KindRecord || (!l.Variadic && len(l.Fields) != n) {
				return Value{}, fmt.Errorf("%w: record does not match type %s", ErrBadValue, l.Name)
			}
		} else if l.Kind != bytecode.KindEnum || l.Arity(int(v.Variant())) != n {
			return Value{}, fmt.Errorf("%w: enum does not match type %s", ErrBadValue, l.Name)
		}
		fs, err := vm.importSlice(v.Fields(), depth)
		if err != nil {
			return Value{}, err
		}
		out := Value{k: v.k, n: v.n}
		if v.k == KindRecord || len(fs) > 0 {
			out.p = &tupObj{vals: fs}
		}
		return out, nil
	case KindClosure:
		if int(v.n) >= len(vm.funcs) || len(v.Fields()) != vm.funcs[v.n].ncaps {
			return Value{}, fmt.Errorf("%w: closure does not match this module", ErrBadValue)
		}
		fs, err := vm.importSlice(v.Fields(), depth)
		if err != nil {
			return Value{}, err
		}
		out := Value{k: KindClosure, n: v.n}
		if len(fs) > 0 {
			out.p = &tupObj{vals: fs}
		}
		return out, nil
	case KindScopeProof:
		p, ok := v.p.(*proofObj)
		if !ok {
			return Value{}, fmt.Errorf("%w: proof without payload", ErrBadValue)
		}
		t, err := vm.importDepth(p.target, depth+1)
		if err != nil {
			return Value{}, err
		}
		return Value{k: KindScopeProof, p: &proofObj{target: t, decision: p.decision}}, nil
	}
	return Value{}, fmt.Errorf("%w: unknown kind %d", ErrBadValue, v.k)
}
