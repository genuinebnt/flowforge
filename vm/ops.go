package vm

import (
	"github.com/genuinebnt/flowforge/bytecode"
)

// slowOp executes the allocation-heavy instructions. It returns the next
// pc. The caller has synced the registers into the VM.
func (vm *VM) slowOp(op bytecode.Opcode, code []byte, pc int, st []Value, spp *int) (int, *fault) {
	sp := *spp
	defer func() { *spp = sp }()
	switch op {
	case bytecode.OpMakeList:
		n := rd16(code, pc+1)
		if n > vm.lim.List {
			return 0, faultf(bytecode.FailLimitExceeded, "list over the element limit")
		}
		if f := vm.alloc(listBytes(n)); f != nil {
			return 0, f
		}
		elems := append([]Value(nil), st[sp-n:sp]...)
		clear(st[sp-n : sp])
		sp -= n
		st[sp] = mkList(elems)
		sp++
		return pc + 3, nil

	case bytecode.OpMakeMap:
		n := rd16(code, pc+1)
		if n > vm.lim.List {
			return 0, faultf(bytecode.FailLimitExceeded, "map over the element limit")
		}
		if f := vm.alloc(mapBytes(n)); f != nil {
			return 0, f
		}
		m := &mapObj{index: make(map[string]int, n)}
		base := sp - 2*n
		for i := 0; i < n; i++ {
			k, v := st[base+2*i], st[base+2*i+1]
			ks := keyString(k)
			if j, dup := m.index[ks]; dup {
				m.vals[j] = v // a repeated key keeps its first position
				continue
			}
			m.index[ks] = len(m.keys)
			m.keys = append(m.keys, k)
			m.vals = append(m.vals, v)
		}
		clear(st[base:sp])
		sp = base
		st[sp] = Value{k: KindMap, p: m}
		sp++
		return pc + 3, nil

	case bytecode.OpMakeRec:
		t, n := rd16(code, pc+1), int(code[pc+3])
		if f := vm.alloc(tupBytes(n)); f != nil {
			return 0, f
		}
		fields := append([]Value(nil), st[sp-n:sp]...)
		clear(st[sp-n : sp])
		sp -= n
		st[sp] = Value{k: KindRecord, n: uint64(t), p: &tupObj{vals: fields}}
		sp++
		return pc + 4, nil

	case bytecode.OpGetField:
		r := st[sp-1]
		i := int(code[pc+1])
		if r.k != KindRecord {
			return 0, typeFault("GET_FIELD on " + r.k.String())
		}
		fs := r.Fields()
		if i >= len(fs) {
			return 0, typeFault("GET_FIELD index out of range")
		}
		st[sp-1] = fs[i]
		return pc + 2, nil

	case bytecode.OpIndex, bytecode.OpListGet:
		c, idx := st[sp-2], st[sp-1]
		var r Value
		switch {
		case c.k == KindList && idx.k == KindInt:
			i := int64(idx.n)
			if i < 0 || i >= int64(c.n) {
				return 0, faultf(bytecode.FailIndexOOB, "list index out of range")
			}
			r = c.p.(*listObj).elems[i]
		case c.k == KindMap && op == bytecode.OpIndex:
			v, ok := c.MapGet(idx)
			if !ok {
				return 0, faultf(bytecode.FailIndexOOB, "key not in map")
			}
			r = v
		default:
			return 0, typeFault(op.String() + " on " + c.k.String() + " with " + idx.k.String())
		}
		sp--
		st[sp] = Value{}
		st[sp-1] = r
		return pc + 1, nil

	case bytecode.OpNewList:
		st[sp] = mkList(nil)
		sp++
		return pc + 1, nil

	case bytecode.OpListPush:
		l, v := st[sp-2], st[sp-1]
		if l.k != KindList {
			return 0, typeFault("LIST_PUSH on " + l.k.String())
		}
		r, f := vm.listAppend(l, v)
		if f != nil {
			return 0, f
		}
		sp--
		st[sp] = Value{}
		st[sp-1] = r
		return pc + 1, nil

	case bytecode.OpListLen:
		l := st[sp-1]
		if l.k != KindList && l.k != KindMap {
			return 0, typeFault("LIST_LEN on " + l.k.String())
		}
		st[sp-1] = Int(int64(l.Len()))
		return pc + 1, nil

	case bytecode.OpMakeEnum:
		t, variant, n := rd16(code, pc+1), code[pc+3], int(code[pc+4])
		var payload []Value
		if n > 0 {
			if f := vm.alloc(tupBytes(n)); f != nil {
				return 0, f
			}
			payload = append([]Value(nil), st[sp-n:sp]...)
			clear(st[sp-n : sp])
			sp -= n
		}
		st[sp] = mkEnum(bytecode.TypeID(t), variant, payload)
		sp++
		return pc + 5, nil

	case bytecode.OpTestVariant:
		e := st[sp-1]
		if e.k != KindEnum {
			return 0, typeFault("TEST_VARIANT on " + e.k.String())
		}
		st[sp] = Bool(e.Variant() == code[pc+1])
		sp++
		return pc + 2, nil

	case bytecode.OpUnpack:
		e := st[sp-1]
		n := int(code[pc+1])
		if e.k != KindEnum {
			return 0, typeFault("UNPACK on " + e.k.String())
		}
		fs := e.Fields()
		if len(fs) != n {
			return 0, typeFault("UNPACK arity differs from the payload")
		}
		sp--
		st[sp] = Value{}
		copy(st[sp:], fs)
		sp += n
		return pc + 2, nil

	case bytecode.OpInterp:
		n := int(code[pc+1])
		d := displayer{m: vm.mod, max: vm.lim.String}
		for _, v := range st[sp-n : sp] {
			d.value(v, true)
		}
		if d.overflow {
			return 0, faultf(bytecode.FailLimitExceeded, "string over 1 MiB")
		}
		s := d.sb.String()
		if f := vm.alloc(strBytes(s)); f != nil {
			return 0, f
		}
		clear(st[sp-n : sp])
		sp -= n
		st[sp] = Str(s)
		sp++
		return pc + 2, nil
	}
	return 0, typeFault("unexpected opcode " + op.String())
}

// appendBudget bounds the reachability check of an in-place append.
const appendBudget = 1024

// mayReach reports whether v may reference the backing b. It answers true
// when it runs out of budget, which makes the caller copy.
func mayReach(v Value, b *listObj, budget *int) bool {
	var vals []Value
	switch o := v.p.(type) {
	case *listObj:
		if o == b {
			return true
		}
		vals = o.elems
	case *tupObj:
		if o == nil {
			return false
		}
		vals = o.vals
	case *mapObj:
		*budget -= len(o.keys)
		if *budget < 0 {
			return true
		}
		for _, k := range o.keys {
			if mayReach(k, b, budget) {
				return true
			}
		}
		vals = o.vals
	case *proofObj:
		return mayReach(o.target, b, budget)
	default:
		return false
	}
	*budget -= 1 + len(vals)
	if *budget < 0 {
		return true
	}
	for _, e := range vals {
		if e.p != nil && mayReach(e, b, budget) {
			return true
		}
	}
	return false
}

// listAppend returns l with v appended. When l is the longest prefix of
// its backing store and v does not reference that store, the store is
// extended in place (amortized O(1)); otherwise the prefix is copied.
// The reference check keeps the object graph acyclic, which snapshots and
// heap accounting rely on.
func (vm *VM) listAppend(l, v Value) (Value, *fault) {
	n := int(l.n)
	if n >= vm.lim.List {
		return Value{}, faultf(bytecode.FailLimitExceeded, "list over the element limit")
	}
	b := l.p.(*listObj)
	budget := appendBudget
	if len(b.elems) == n && (v.p == nil || !mayReach(v, b, &budget)) {
		if f := vm.alloc(valueBytes); f != nil {
			return Value{}, f
		}
		b.elems = append(b.elems, v)
		return Value{k: KindList, f: l.f, n: uint64(n + 1), p: b}, nil
	}
	if f := vm.alloc(listBytes(n + 1)); f != nil {
		return Value{}, f
	}
	elems := make([]Value, n+1, max(n+1, 2*n))
	copy(elems, b.elems[:n])
	elems[n] = v
	return Value{k: KindList, f: l.f, n: uint64(n + 1), p: &listObj{elems: elems}}, nil
}
