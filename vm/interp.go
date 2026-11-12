package vm

import (
	"encoding/binary"

	"github.com/genuinebnt/flowforge/bytecode"
)

// Fuel costs (spec 8.7).
const (
	costInstr  = 1
	costLoop   = 5
	costEffect = 50
)

var opCost = func() (t [256]int64) {
	for i := range t {
		t[i] = costInstr
	}
	t[bytecode.OpLoop] = costLoop
	t[bytecode.OpEffect] = costEffect
	return t
}()

func rd16(code []byte, i int) int { return int(binary.LittleEndian.Uint16(code[i:])) }
func rdI32(code []byte, i int) int {
	return int(int32(binary.LittleEndian.Uint32(code[i:])))
}

// exec runs the interpreter loop with a fuel slice. The verifier
// guarantees that operand depth never underflows or exceeds the frame's
// computed maximum, that jumps land on instruction boundaries and that
// every index operand is in range, so the loop only checks dynamic
// properties: value kinds, arithmetic, limits and fuel.
func (vm *VM) exec(slice int64) Outcome {
	if slice < 0 {
		slice = 0
	}
	budget := slice
	capped := false
	if rem := vm.cfg.FuelLimit - vm.fuelUsed; budget >= rem {
		budget, capped = rem, true
	}
	left := budget

	fr := &vm.frames[len(vm.frames)-1]
	fi := &vm.funcs[fr.fn]
	code := fi.code
	pc := fr.pc
	st := vm.stack
	sp := vm.sp
	var ipc int // offset of the executing instruction
	var f *fault

	// sync stores the cached registers back into the VM.
	sync := func() {
		fr.pc = pc
		vm.sp = sp
		vm.fuelUsed += budget - left
		budget = left
	}
	stop := func() Outcome {
		// The instruction at pc was not executed.
		pc = ipc
		sync()
		if capped {
			return vm.failSpan(bytecode.FailOutOfFuel, "fuel limit exhausted", vm.span(int(fr.fn), ipc))
		}
		return OutOfFuel{}
	}
	fail := func(f *fault) Outcome {
		pc = ipc
		sync()
		return vm.failSpan(f.code, f.msg, vm.span(int(fr.fn), ipc))
	}

	for {
		ipc = pc
		op := bytecode.Opcode(code[pc])
		if c := opCost[op]; left < c {
			return stop()
		} else {
			left -= c
		}
		switch op {
		case bytecode.OpNop:
			pc++
		case bytecode.OpConst:
			st[sp] = vm.consts[rd16(code, pc+1)]
			sp++
			pc += 3
		case bytecode.OpUnit:
			st[sp] = Value{}
			sp++
			pc++
		case bytecode.OpTrue:
			st[sp] = Value{k: KindBool, n: 1}
			sp++
			pc++
		case bytecode.OpFalse:
			st[sp] = Value{k: KindBool}
			sp++
			pc++
		case bytecode.OpPop:
			sp--
			st[sp] = Value{}
			pc++
		case bytecode.OpDup:
			st[sp] = st[sp-1]
			sp++
			pc++
		case bytecode.OpLoadLocal:
			st[sp] = st[fr.bp+rd16(code, pc+1)]
			sp++
			pc += 3
		case bytecode.OpStoreLocal:
			sp--
			st[fr.bp+rd16(code, pc+1)] = st[sp]
			st[sp] = Value{}
			pc += 3
		case bytecode.OpLoadCap:
			st[sp] = fr.caps.vals[code[pc+1]]
			sp++
			pc += 2

		case bytecode.OpAddI, bytecode.OpSubI, bytecode.OpMulI, bytecode.OpDivI, bytecode.OpModI:
			a, b := &st[sp-2], &st[sp-1]
			if a.k == KindInt && b.k == KindInt && op == bytecode.OpAddI {
				x, y := int64(a.n), int64(b.n)
				r := x + y
				if (r > x) != (y > 0) {
					return fail(errOverflow)
				}
				a.n = uint64(r)
			} else if a.k == KindInt && b.k == KindInt && op == bytecode.OpSubI {
				x, y := int64(a.n), int64(b.n)
				r := x - y
				if (r < x) != (y > 0) {
					return fail(errOverflow)
				}
				a.n = uint64(r)
			} else {
				r, f := intArith(op, *a, *b)
				if f != nil {
					return fail(f)
				}
				*a = r
			}
			sp--
			st[sp] = Value{}
			pc++
		case bytecode.OpNegI:
			r, f := negInt(st[sp-1])
			if f != nil {
				return fail(f)
			}
			st[sp-1] = r
			pc++
		case bytecode.OpAddF, bytecode.OpSubF, bytecode.OpMulF, bytecode.OpDivF:
			r, f := floatArith(op, st[sp-2], st[sp-1])
			if f != nil {
				return fail(f)
			}
			sp--
			st[sp-1] = r
			st[sp] = Value{}
			pc++
		case bytecode.OpNegF:
			a := st[sp-1]
			if a.k != KindFloat {
				return fail(typeFault("NEG_F on " + a.k.String()))
			}
			st[sp-1] = Float(-a.AsFloat())
			pc++
		case bytecode.OpConcat:
			a, b := st[sp-2], st[sp-1]
			if a.k != KindString || b.k != KindString {
				return fail(typeFault("CONCAT on " + a.k.String() + " and " + b.k.String()))
			}
			s := a.Str() + b.Str()
			if len(s) > vm.lim.String {
				return fail(faultf(bytecode.FailLimitExceeded, "string over 1 MiB"))
			}
			sync()
			if f = vm.alloc(strBytes(s)); f != nil {
				return fail(f)
			}
			sp--
			st[sp-1] = Str(s)
			st[sp] = Value{}
			pc++

		case bytecode.OpEq, bytecode.OpNe:
			a, b := &st[sp-2], &st[sp-1]
			var eq bool
			if a.k == b.k && a.k <= KindTime {
				eq = a.n == b.n
			} else {
				eq = Equal(*a, *b)
			}
			*a = Bool(eq == (op == bytecode.OpEq))
			sp--
			st[sp] = Value{}
			pc++
		case bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe:
			a, b := &st[sp-2], &st[sp-1]
			var r bool
			if a.k == KindInt && b.k == KindInt {
				x, y := int64(a.n), int64(b.n)
				switch op {
				case bytecode.OpLt:
					r = x < y
				case bytecode.OpLe:
					r = x <= y
				case bytecode.OpGt:
					r = x > y
				default:
					r = x >= y
				}
			} else {
				r, f = compareOp(op, *a, *b)
				if f != nil {
					return fail(f)
				}
			}
			*a = Bool(r)
			sp--
			st[sp] = Value{}
			pc++
		case bytecode.OpNot:
			a := &st[sp-1]
			if a.k != KindBool {
				return fail(typeFault("NOT on " + a.k.String()))
			}
			a.n ^= 1
			pc++

		case bytecode.OpJump:
			pc += 5 + rdI32(code, pc+1)
		case bytecode.OpJumpIfFalse:
			sp--
			c := st[sp]
			st[sp] = Value{}
			if c.k != KindBool {
				return fail(typeFault("JUMP_IF_FALSE on " + c.k.String()))
			}
			if c.n == 0 {
				pc += 5 + rdI32(code, pc+1)
			} else {
				pc += 5
			}
		case bytecode.OpLoop:
			pc += 5 + rdI32(code, pc+1)

		case bytecode.OpCall, bytecode.OpCallClosure:
			var fn, argc, ret int
			var caps *tupObj
			if op == bytecode.OpCall {
				fn, argc = rd16(code, pc+1), int(code[pc+3])
				pc += 4
				ret = sp - argc
			} else {
				argc = int(code[pc+1])
				pc += 2
				ret = sp - argc - 1
				c := st[ret]
				if c.k != KindClosure {
					return fail(typeFault("CALL_CLOSURE on " + c.k.String()))
				}
				fn = int(c.n)
				if vm.funcs[fn].arity != argc {
					return fail(typeFault("closure called with the wrong number of arguments"))
				}
				caps, _ = c.p.(*tupObj)
			}
			if len(vm.frames) >= vm.lim.CallDepth {
				return fail(faultf(bytecode.FailLimitExceeded, "call depth limit exceeded"))
			}
			callee := &vm.funcs[fn]
			bp := sp - argc
			need := bp + callee.nlocals + callee.maxStack
			if need > vm.lim.Stack {
				return fail(faultf(bytecode.FailLimitExceeded, "value stack limit exceeded"))
			}
			if need > len(st) {
				grown := make([]Value, min(max(need, 2*len(st)), vm.lim.Stack))
				copy(grown, st[:sp])
				st = grown
				vm.stack = st
			}
			for i := bp + argc; i < bp+callee.nlocals; i++ {
				st[i] = Value{}
			}
			fr.pc = pc
			vm.frames = append(vm.frames, frame{fn: uint16(fn), bp: bp, ret: ret, caps: caps})
			fr = &vm.frames[len(vm.frames)-1]
			fi = callee
			code = fi.code
			pc = 0
			sp = bp + callee.nlocals

		case bytecode.OpRet:
			if done := vm.ret(st, &sp); done {
				pc++
				sync()
				return Completed{Value: vm.result}
			}
			fr = &vm.frames[len(vm.frames)-1]
			fi = &vm.funcs[fr.fn]
			code = fi.code
			pc = fr.pc

		case bytecode.OpTry:
			v := st[sp-1]
			if v.k != KindEnum || (v.TypeID() != bytecode.TypeOption && v.TypeID() != bytecode.TypeResult) {
				return fail(typeFault("TRY on " + v.k.String()))
			}
			failed := (v.TypeID() == bytecode.TypeOption && v.Variant() == bytecode.VariantNone) ||
				(v.TypeID() == bytecode.TypeResult && v.Variant() == bytecode.VariantErr)
			if !failed {
				fs := v.Fields()
				if len(fs) != 1 {
					return fail(typeFault("TRY payload"))
				}
				st[sp-1] = fs[0]
				pc++
				continue
			}
			if !vm.child && len(vm.frames) == 1 {
				return fail(tryFault(v))
			}
			if done := vm.ret(st, &sp); done {
				pc++
				sync()
				return Completed{Value: vm.result}
			}
			fr = &vm.frames[len(vm.frames)-1]
			fi = &vm.funcs[fr.fn]
			code = fi.code
			pc = fr.pc

		case bytecode.OpClosure, bytecode.OpPushComp:
			fn, n := rd16(code, pc+1), int(code[pc+3])
			c := Value{k: KindClosure, n: uint64(fn)}
			if n > 0 {
				c.p = &tupObj{vals: append([]Value(nil), st[sp-n:sp]...)}
				clear(st[sp-n : sp])
				sp -= n
				sync()
				if f = vm.alloc(tupBytes(n)); f != nil {
					return fail(f)
				}
			}
			if op == bytecode.OpClosure {
				st[sp] = c
				sp++
			} else {
				vm.comps = append(vm.comps, c)
			}
			pc += 4

		case bytecode.OpHalt:
			vm.result = st[sp-1]
			vm.status = statusCompleted
			pc++
			sync()
			return Completed{Value: vm.result}
		case bytecode.OpFail:
			c := bytecode.FailCode(rd16(code, pc+1))
			return fail(faultf(c, c.String()))

		case bytecode.OpEffect:
			kind, argc := bytecode.EffectKind(code[pc+1]), int(code[pc+2])
			if vm.effects >= vm.lim.Effects {
				return fail(faultf(bytecode.FailLimitExceeded, "effect limit exceeded"))
			}
			args := make([]Value, argc)
			copy(args, st[sp-argc:sp])
			clear(st[sp-argc : sp])
			sp -= argc
			if kind == bytecode.EffectToolCall {
				tool := args[argc-1]
				copy(args[1:], args[:argc-1])
				args[0] = tool
			}
			seq := vm.nextSeq
			vm.nextSeq++
			vm.effects++
			pc += 3
			vm.pending = &Suspended{Path: vm.prefix.Child(uint32(seq)), Seq: seq, Kind: kind, Args: args}
			vm.status = statusSuspended
			sync()
			return *vm.pending

		case bytecode.OpBuiltin:
			id, argc := bytecode.BuiltinID(rd16(code, pc+1)), int(code[pc+3])
			args := st[sp-argc : sp]
			n := builtinInputSize(args)
			if id == bytecode.BRange {
				n += rangeSize(args)
			}
			extra := n / 16
			if left < extra {
				left += costInstr
				return stop()
			}
			left -= extra
			sync()
			r, f := vm.callBuiltin(id, args)
			if f != nil {
				return fail(f)
			}
			clear(st[sp-argc : sp])
			sp -= argc
			st[sp] = r
			sp++
			pc += 4

		default:
			sync()
			r, f := vm.slowOp(op, code, pc, st, &sp)
			if f != nil {
				return fail(f)
			}
			pc = r
		}
	}
}

// tryFault picks the failure code when TRY fails in the workflow body: an
// Err carrying the built-in Denied record (an approval denial, spec 8.5
// "require approval") fails with approval_denied; everything else with
// unwrap_failed (spec 7.5).
func tryFault(v Value) *fault {
	if v.TypeID() == bytecode.TypeResult {
		if fs := v.Fields(); len(fs) == 1 && fs[0].k == KindRecord && fs[0].TypeID() == bytecode.TypeDenied {
			return faultf(bytecode.FailApprovalDenied, "approval denied")
		}
		return faultf(bytecode.FailUnwrapFailed, "? on Err in the workflow body")
	}
	return faultf(bytecode.FailUnwrapFailed, "? on None in the workflow body")
}

// ret pops the current frame, moving the top of stack into the caller's
// result slot. It reports whether the bottom frame returned.
func (vm *VM) ret(st []Value, sp *int) bool {
	fr := &vm.frames[len(vm.frames)-1]
	result := st[*sp-1]
	clear(st[fr.ret:*sp])
	ret := fr.ret
	vm.frames = vm.frames[:len(vm.frames)-1]
	if len(vm.frames) == 0 {
		*sp = 0
		vm.result = result
		vm.status = statusCompleted
		return true
	}
	st[ret] = result
	*sp = ret + 1
	return false
}
