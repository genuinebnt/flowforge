package vm

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/bytecode"
)

// builtinInputSize is the n of the BUILTIN fuel rule (1 + n/16): the total
// element or byte count of the arguments. range, whose input is two
// integers, is charged for the elements it produces.
func builtinInputSize(args []Value) int64 {
	var n int64
	for _, a := range args {
		switch a.k {
		case KindList:
			n += int64(a.n)
		case KindMap:
			n += int64(a.Len())
		default:
			if a.k.isStringy() {
				n += int64(a.Len())
			}
		}
	}
	return n
}

func rangeSize(args []Value) int64 {
	if len(args) == 2 && args[0].k == KindInt && args[1].k == KindInt {
		a, b := int64(args[0].n), int64(args[1].n)
		if b > a {
			if d := uint64(b) - uint64(a); d < 1<<62 {
				return int64(d)
			}
			return 1 << 62
		}
	}
	return 0
}

func (vm *VM) newStr(s string) (Value, *fault) {
	if len(s) > vm.lim.String {
		return Value{}, faultf(bytecode.FailLimitExceeded, "string over 1 MiB")
	}
	if f := vm.alloc(strBytes(s)); f != nil {
		return Value{}, f
	}
	return Str(s), nil
}

func (vm *VM) newList(elems []Value) (Value, *fault) {
	if len(elems) > vm.lim.List {
		return Value{}, faultf(bytecode.FailLimitExceeded, "list over the element limit")
	}
	if f := vm.alloc(listBytes(len(elems))); f != nil {
		return Value{}, f
	}
	return mkList(elems), nil
}

func (vm *VM) checkLen(n int) *fault {
	if n > vm.lim.List {
		return faultf(bytecode.FailLimitExceeded, "list over the element limit")
	}
	return nil
}

func (vm *VM) some(v Value) (Value, *fault) {
	if f := vm.alloc(tupBytes(1)); f != nil {
		return Value{}, f
	}
	return Some(v), nil
}

func (vm *VM) tuple(a, b Value) Value {
	return Value{k: KindRecord, n: uint64(bytecode.TypeTuple), p: &tupObj{vals: []Value{a, b}}}
}

func argErr(name string, args []Value) *fault {
	kinds := make([]string, len(args))
	for i, a := range args {
		kinds[i] = a.k.String()
	}
	return typeFault(name + "(" + strings.Join(kinds, ", ") + ")")
}

func (vm *VM) callBuiltin(id bytecode.BuiltinID, a []Value) (Value, *fault) {
	bi, _ := bytecode.LookupBuiltin(id)
	bad := func() (Value, *fault) { return Value{}, argErr(bi.Name, a) }
	str1 := len(a) >= 1 && a[0].k == KindString
	str2 := str1 && len(a) >= 2 && a[1].k == KindString
	list1 := len(a) >= 1 && a[0].k == KindList

	switch id {
	case bytecode.BLen:
		switch {
		case str1:
			return Int(int64(utf8.RuneCountInString(a[0].Str()))), nil
		case list1, a[0].k == KindMap:
			return Int(int64(a[0].Len())), nil
		}
	case bytecode.BContains:
		switch {
		case str2:
			return Bool(strings.Contains(a[0].Str(), a[1].Str())), nil
		case list1:
			for _, e := range a[0].Elems() {
				if Equal(e, a[1]) {
					return Bool(true), nil
				}
			}
			return Bool(false), nil
		case a[0].k == KindMap:
			_, ok := a[0].MapGet(a[1])
			return Bool(ok), nil
		}
	case bytecode.BStartsWith:
		if str2 {
			return Bool(strings.HasPrefix(a[0].Str(), a[1].Str())), nil
		}
	case bytecode.BEndsWith:
		if str2 {
			return Bool(strings.HasSuffix(a[0].Str(), a[1].Str())), nil
		}
	case bytecode.BSplit:
		if str2 {
			s, sep := a[0].Str(), a[1].Str()
			n := utf8.RuneCountInString(s)
			if sep != "" {
				n = strings.Count(s, sep) + 1
			}
			if f := vm.checkLen(n); f != nil {
				return Value{}, f
			}
			parts := strings.Split(s, sep)
			elems := make([]Value, len(parts))
			var size int64
			for i, p := range parts {
				elems[i] = Str(p)
				size += strBytes(p)
			}
			if f := vm.alloc(size); f != nil {
				return Value{}, f
			}
			return vm.newList(elems)
		}
	case bytecode.BJoin:
		if list1 && len(a) == 2 && a[1].k == KindString {
			sep := a[1].Str()
			total := 0
			for i, e := range a[0].Elems() {
				if e.k != KindString {
					return bad()
				}
				if i > 0 {
					total += len(sep)
				}
				total += e.Len()
				if total > vm.lim.String {
					return Value{}, faultf(bytecode.FailLimitExceeded, "string over 1 MiB")
				}
			}
			var sb strings.Builder
			sb.Grow(total)
			for i, e := range a[0].Elems() {
				if i > 0 {
					sb.WriteString(sep)
				}
				sb.WriteString(e.Str())
			}
			return vm.newStr(sb.String())
		}
	case bytecode.BTrim:
		if str1 {
			return vm.newStr(strings.TrimSpace(a[0].Str()))
		}
	case bytecode.BLower, bytecode.BUpper:
		if str1 {
			s := a[0].Str()
			if id == bytecode.BLower {
				return vm.newStr(strings.ToLower(s))
			}
			return vm.newStr(strings.ToUpper(s))
		}
	case bytecode.BReplace:
		if str2 && a[2].k == KindString {
			s, from, to := a[0].Str(), a[1].Str(), a[2].Str()
			count := strings.Count(s, from)
			if size := int64(len(s)) + int64(count)*(int64(len(to))-int64(len(from))); size > int64(vm.lim.String) {
				return Value{}, faultf(bytecode.FailLimitExceeded, "string over 1 MiB")
			}
			return vm.newStr(strings.ReplaceAll(s, from, to))
		}
	case bytecode.BParseInt:
		if str1 {
			n, err := strconv.ParseInt(a[0].Str(), 10, 64)
			if err != nil {
				return None(), nil
			}
			return vm.some(Int(n))
		}
	case bytecode.BParseFloat:
		if str1 {
			s := a[0].Str()
			x, err := strconv.ParseFloat(s, 64)
			if err != nil || math.IsNaN(x) || math.IsInf(x, 0) || strings.ContainsAny(s, "_xXpP") {
				return None(), nil
			}
			return vm.some(Float(x))
		}
	case bytecode.BToString:
		if a[0].k == KindString {
			return a[0], nil
		}
		d := displayer{m: vm.mod, max: vm.lim.String}
		d.value(a[0], true)
		if d.overflow {
			return Value{}, faultf(bytecode.FailLimitExceeded, "string over 1 MiB")
		}
		return vm.newStr(d.sb.String())
	case bytecode.BTake, bytecode.BSkip:
		if list1 && a[1].k == KindInt {
			n, k := int64(a[0].n), int64(a[1].n)
			k = min(max(k, 0), n)
			if id == bytecode.BTake {
				// A prefix shares the backing store; no copy needed.
				return Value{k: KindList, f: a[0].f, n: uint64(k), p: a[0].p}, nil
			}
			r, f := vm.newList(append([]Value(nil), a[0].Elems()[k:]...))
			r.f = a[0].f
			return r, f
		}
	case bytecode.BReverse:
		if list1 {
			elems := append([]Value(nil), a[0].Elems()...)
			slices.Reverse(elems)
			r, f := vm.newList(elems)
			r.f = a[0].f
			return r, f
		}
	case bytecode.BConcat:
		switch {
		case str2:
			if a[0].Len()+a[1].Len() > vm.lim.String {
				return Value{}, faultf(bytecode.FailLimitExceeded, "string over 1 MiB")
			}
			return vm.newStr(a[0].Str() + a[1].Str())
		case list1 && a[1].k == KindList:
			if f := vm.checkLen(a[0].Len() + a[1].Len()); f != nil {
				return Value{}, f
			}
			elems := make([]Value, 0, a[0].Len()+a[1].Len())
			elems = append(append(elems, a[0].Elems()...), a[1].Elems()...)
			r, f := vm.newList(elems)
			r.f = a[0].f
			return r, f
		}
	case bytecode.BZip:
		if list1 && a[1].k == KindList {
			x, y := a[0].Elems(), a[1].Elems()
			n := min(len(x), len(y))
			if f := vm.alloc(int64(n) * tupBytes(2)); f != nil {
				return Value{}, f
			}
			elems := make([]Value, n)
			for i := range elems {
				elems[i] = vm.tuple(x[i], y[i])
			}
			return vm.newList(elems)
		}
	case bytecode.BEnumerate:
		if list1 {
			x := a[0].Elems()
			if f := vm.alloc(int64(len(x)) * tupBytes(2)); f != nil {
				return Value{}, f
			}
			elems := make([]Value, len(x))
			for i := range elems {
				elems[i] = vm.tuple(Int(int64(i)), x[i])
			}
			return vm.newList(elems)
		}
	case bytecode.BDedupe:
		if list1 {
			seen := make(map[string]struct{}, a[0].Len())
			var elems []Value
			for _, e := range a[0].Elems() {
				k := keyString(e)
				if _, ok := seen[k]; ok {
					continue
				}
				seen[k] = struct{}{}
				elems = append(elems, e)
			}
			r, f := vm.newList(elems)
			r.f = a[0].f
			return r, f
		}
	case bytecode.BSort:
		if list1 {
			elems := append([]Value(nil), a[0].Elems()...)
			slices.SortStableFunc(elems, Compare)
			r, f := vm.newList(elems)
			r.f = a[0].f
			return r, f
		}
	case bytecode.BSortByKeys:
		if list1 && a[1].k == KindList && a[0].Len() == a[1].Len() {
			keys, xs := a[0].Elems(), a[1].Elems()
			idx := make([]int, len(xs))
			for i := range idx {
				idx[i] = i
			}
			slices.SortStableFunc(idx, func(i, j int) int { return Compare(keys[i], keys[j]) })
			elems := make([]Value, len(xs))
			for i, j := range idx {
				elems[i] = xs[j]
			}
			r, f := vm.newList(elems)
			r.f = a[1].f
			return r, f
		}
	case bytecode.BGroupByKeys:
		if list1 && a[1].k == KindList && a[0].Len() == a[1].Len() {
			keys, xs := a[0].Elems(), a[1].Elems()
			m := &mapObj{index: map[string]int{}}
			var groups [][]Value
			for i, k := range keys {
				ks := keyString(k)
				j, ok := m.index[ks]
				if !ok {
					j = len(m.keys)
					m.index[ks] = j
					m.keys = append(m.keys, k)
					groups = append(groups, nil)
				}
				groups[j] = append(groups[j], xs[i])
			}
			m.vals = make([]Value, len(groups))
			for i, g := range groups {
				m.vals[i] = mkList(g)
			}
			if f := vm.alloc(mapBytes(len(m.keys)) + int64(len(groups))*24 + listBytes(len(xs))); f != nil {
				return Value{}, f
			}
			return Value{k: KindMap, p: m}, nil
		}
	case bytecode.BMapGet:
		if a[0].k == KindMap {
			if v, ok := a[0].MapGet(a[1]); ok {
				return vm.some(v)
			}
			return None(), nil
		}
	case bytecode.BMapInsert:
		if a[0].k == KindMap {
			old := a[0].p.(*mapObj)
			ks := keyString(a[1])
			j, exists := old.index[ks]
			n := len(old.keys)
			if !exists {
				if f := vm.checkLen(n + 1); f != nil {
					return Value{}, f
				}
				n++
			}
			if f := vm.alloc(mapBytes(n)); f != nil {
				return Value{}, f
			}
			m := &mapObj{keys: make([]Value, len(old.keys), n), vals: make([]Value, len(old.vals), n), index: make(map[string]int, n)}
			copy(m.keys, old.keys)
			copy(m.vals, old.vals)
			for k, i := range old.index {
				m.index[k] = i
			}
			if exists {
				m.vals[j] = a[2]
			} else {
				m.index[ks] = len(m.keys)
				m.keys = append(m.keys, a[1])
				m.vals = append(m.vals, a[2])
			}
			return Value{k: KindMap, p: m}, nil
		}
	case bytecode.BMapKeys, bytecode.BMapValues:
		if a[0].k == KindMap {
			m := a[0].p.(*mapObj)
			src := m.keys
			if id == bytecode.BMapValues {
				src = m.vals
			}
			return vm.newList(append([]Value(nil), src...))
		}
	case bytecode.BListGetOpt:
		if list1 && a[1].k == KindInt {
			i := int64(a[1].n)
			if i < 0 || i >= int64(a[0].n) {
				return None(), nil
			}
			return vm.some(a[0].Elems()[i])
		}
	case bytecode.BRange:
		if a[0].k == KindInt && a[1].k == KindInt {
			n := rangeSize(a)
			if n > int64(vm.lim.List) {
				return Value{}, faultf(bytecode.FailLimitExceeded, "list over the element limit")
			}
			lo := int64(a[0].n)
			elems := make([]Value, n)
			for i := range elems {
				elems[i] = Int(lo + int64(i))
			}
			return vm.newList(elems)
		}
	case bytecode.BFqdnParse, bytecode.BIPParse, bytecode.BURLParse, bytecode.BCIDRParse, bytecode.BASNParse, bytecode.BHostParse:
		if str1 {
			var v Value
			var err error
			switch id {
			case bytecode.BFqdnParse:
				v, err = Fqdn(a[0].Str())
			case bytecode.BIPParse:
				v, err = IPAddress(a[0].Str())
			case bytecode.BURLParse:
				v, err = URL(a[0].Str())
			case bytecode.BCIDRParse:
				v, err = Netblock(a[0].Str())
			case bytecode.BASNParse:
				v, err = Asn(a[0].Str())
			default:
				v, err = Host(a[0].Str())
			}
			if err != nil {
				return None(), nil
			}
			if f := vm.alloc(valueSize(v)); f != nil {
				return Value{}, f
			}
			return vm.some(v)
		}
	case bytecode.BScopeAllows:
		if vm.cfg.Scope == nil {
			return Bool(false), nil
		}
		t := a[0]
		if t.k == KindScopeProof {
			t = t.ProofTarget()
		}
		if t.k == KindEnum && t.TypeID() == bytecode.TypeHost && len(t.Fields()) == 1 {
			t = t.Fields()[0]
		}
		return Bool(vm.cfg.Scope.Allows(t)), nil
	case bytecode.BFlatten:
		if list1 {
			total := 0
			for _, e := range a[0].Elems() {
				if e.k != KindList {
					return bad()
				}
				total += e.Len()
			}
			if f := vm.checkLen(total); f != nil {
				return Value{}, f
			}
			elems := make([]Value, 0, total)
			for _, e := range a[0].Elems() {
				elems = append(elems, e.Elems()...)
			}
			return vm.newList(elems)
		}
	case bytecode.BIntToFloat:
		if a[0].k == KindInt {
			return Float(float64(a[0].AsInt())), nil
		}
	case bytecode.BFloatToInt:
		if a[0].k == KindFloat {
			x := math.Trunc(a[0].AsFloat())
			if x < -9.223372036854775808e18 || x >= 9.223372036854775808e18 {
				return Value{}, errOverflow
			}
			return Int(int64(x)), nil
		}
	case bytecode.BListAppend:
		if list1 {
			return vm.listAppend(a[0], a[1])
		}
	}
	return bad()
}
