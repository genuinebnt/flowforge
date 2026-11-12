package vm

import "github.com/genuinebnt/flowforge/bytecode"

// Heap accounting (spec 8.7: live heap 64 MiB, counted by value size).
//
// Every allocation adds its size to heapAlloc. While heapLive + heapAlloc
// stays under the limit nothing else happens; that sum is an upper bound
// of the live heap because memory only grows through allocation. When the
// bound would exceed the limit the VM measures the live heap exactly by
// walking the roots (stack, capture sets of active frames, compensation
// stack, pending effect arguments), counting each heap object once and
// skipping constant-pool strings. To keep measurement amortized it only
// measures when at least 1/8 of the limit was allocated since the last
// measurement; otherwise it fails with limit_exceeded. The effective limit
// is therefore between 7/8 of the limit and the limit (fail closed). Both
// counters are part of the snapshot and the object graph is restored with
// its sharing intact, so the decision is deterministic across restore.

const valueBytes = 32

type fault struct {
	code bytecode.FailCode
	msg  string
}

func faultf(code bytecode.FailCode, msg string) *fault { return &fault{code: code, msg: msg} }

var (
	errOverflow = &fault{bytecode.FailArithOverflow, "integer overflow"}
	errDivZero  = &fault{bytecode.FailDivZero, "division by zero"}
	errNaN      = &fault{bytecode.FailArithNaN, "float result is NaN"}
	errFloatInf = &fault{bytecode.FailArithOverflow, "float result is infinite"}
)

func typeFault(what string) *fault { return &fault{bytecode.FailTypeMismatch, what} }

func strBytes(s string) int64   { return 24 + int64(len(s)) }
func listBytes(n int) int64     { return 24 + int64(n)*valueBytes }
func tupBytes(n int) int64      { return 24 + int64(n)*valueBytes }
func mapBytes(n int) int64      { return 80 + int64(n)*(2*valueBytes+48) }
func proofBytes(d string) int64 { return 48 + int64(len(d)) }

// alloc records n freshly allocated bytes.
func (vm *VM) alloc(n int64) *fault {
	vm.heapAlloc += n
	limit := vm.lim.Heap
	if vm.heapLive+vm.heapAlloc <= limit {
		return nil
	}
	if vm.heapAlloc-n < limit/8 {
		return faultf(bytecode.FailLimitExceeded, "live heap limit exceeded")
	}
	live := vm.measure()
	vm.heapLive, vm.heapAlloc = live, n
	if live+n > limit {
		return faultf(bytecode.FailLimitExceeded, "live heap limit exceeded")
	}
	return nil
}

type sizer struct {
	seen  map[any]struct{}
	total int64
}

func (s *sizer) first(p any) bool {
	if _, ok := s.seen[p]; ok {
		return false
	}
	s.seen[p] = struct{}{}
	return true
}

func (s *sizer) value(v Value) {
	switch o := v.p.(type) {
	case *strObj:
		if o.konst == 0 && s.first(o) {
			s.total += strBytes(o.s)
		}
	case *listObj:
		if s.first(o) {
			s.total += listBytes(len(o.elems))
			for _, e := range o.elems {
				s.value(e)
			}
		}
	case *mapObj:
		if s.first(o) {
			s.total += mapBytes(len(o.keys))
			for i := range o.keys {
				s.value(o.keys[i])
				s.value(o.vals[i])
			}
		}
	case *tupObj:
		s.tup(o)
	case *proofObj:
		if s.first(o) {
			s.total += proofBytes(o.decision)
			s.value(o.target)
		}
	}
}

func (s *sizer) tup(o *tupObj) {
	if o != nil && s.first(o) {
		s.total += tupBytes(len(o.vals))
		for _, e := range o.vals {
			s.value(e)
		}
	}
}

// valueSize is the heap size of v counted on its own.
func valueSize(v Value) int64 {
	if v.p == nil {
		return 0
	}
	s := sizer{seen: map[any]struct{}{}}
	s.value(v)
	return s.total
}

// measure returns the exact live heap size.
func (vm *VM) measure() int64 {
	s := sizer{seen: map[any]struct{}{}}
	for _, v := range vm.stack[:vm.sp] {
		s.value(v)
	}
	for i := range vm.frames {
		s.tup(vm.frames[i].caps)
	}
	for _, c := range vm.comps {
		s.value(c)
	}
	if vm.pending != nil {
		for _, a := range vm.pending.Args {
			s.value(a)
		}
	}
	return s.total
}

// HeapLive returns the live heap size in bytes (measured now).
func (vm *VM) HeapLive() int64 { return vm.measure() }
