package vm_test

import (
	"fmt"
	"math"
	"testing"

	bc "github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/vm"
)

func runPure(b *testing.B, p *bc.Verified, cfg vm.Config, args ...vm.Value) (vm.Value, int64) {
	v, err := vm.New(p, cfg)
	if err != nil {
		b.Fatal(err)
	}
	if err := v.Start(args); err != nil {
		b.Fatal(err)
	}
	out, err := v.Run(math.MaxInt64)
	if err != nil {
		b.Fatal(err)
	}
	c, ok := out.(vm.Completed)
	if !ok {
		b.Fatalf("%#v", out)
	}
	return c.Value, v.FuelUsed()
}

func BenchmarkVM01_Fib30(b *testing.B) {
	bl := newBuilder()
	fib := addFib(bl)
	f := bl.Func("main", 1, 0)
	bl.SetEntry(f.Index())
	f.Load(0)
	f.Call(fib, 1)
	f.Op(bc.OpRet)
	p := build(b, bl)
	b.ReportAllocs()
	var fuel int64
	for b.Loop() {
		v, used := runPure(b, p, bigLimits, vm.Int(30))
		if v.AsInt() != 832040 {
			b.Fatal(v)
		}
		fuel = used
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(int64(b.N)*fuel), "ns/instr")
}

func loopProgram(t testing.TB) *bc.Verified {
	return program(t, 1, func(_ *bc.Builder, f *bc.FuncBuilder) {
		emitSumLoop(f, 0)
		f.Op(bc.OpRet)
	})
}

func BenchmarkVM01_Loop(b *testing.B) {
	p := loopProgram(b)
	const n = 1_000_000
	// No allocation per instruction: the allocation count of the whole run
	// does not depend on the iteration count.
	small := testing.AllocsPerRun(3, func() { runPure(b, p, bigLimits, vm.Int(10)) })
	large := testing.AllocsPerRun(3, func() { runPure(b, p, bigLimits, vm.Int(100_000)) })
	if large != small {
		b.Fatalf("allocations grow with iterations: %v vs %v", small, large)
	}
	b.ReportAllocs()
	var fuel int64
	for b.Loop() {
		_, fuel = runPure(b, p, bigLimits, vm.Int(n))
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(int64(b.N)*fuel), "ns/fuel")
	b.ReportMetric(float64(b.N)*n/b.Elapsed().Seconds(), "iter/s")
}

func BenchmarkVM01_ListMapFilter(b *testing.B) {
	const n = 1_000_000
	p := program(b, 0, func(_ *bc.Builder, f *bc.FuncBuilder) {
		f.Int(0)
		f.Int(n)
		f.Builtin(bc.BRange)
		emitMapFilter(f)
		f.Op(bc.OpListLen, bc.OpRet)
	})
	b.ReportAllocs()
	var fuel int64
	for b.Loop() {
		v, used := runPure(b, p, bigLimits)
		if v.AsInt() != (n+2)/3 {
			b.Fatal(v)
		}
		fuel = used
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(int64(b.N)*fuel), "ns/fuel")
}

func BenchmarkVM01_StringBuilding(b *testing.B) {
	// join([ "item {i}" for i in 0..10000 ], ",")
	p := program(b, 0, func(_ *bc.Builder, f *bc.FuncBuilder) {
		acc, i := f.Local(), f.Local()
		f.Op(bc.OpNewList)
		f.Int(0)
		h := f.Here()
		f.Store(i)
		f.Store(acc)
		f.Load(i)
		f.Int(10_000)
		f.Op(bc.OpLt)
		exit := f.NewLabel()
		f.JumpIfFalse(exit)
		f.Load(acc)
		f.String("item ")
		f.Load(i)
		f.Emit(bc.OpInterp, 2)
		f.Op(bc.OpListPush)
		f.Load(i)
		f.Int(1)
		f.Op(bc.OpAddI)
		f.Loop(h)
		f.Bind(exit)
		f.Load(acc)
		f.String(",")
		f.Builtin(bc.BJoin)
		f.Builtin(bc.BLen)
		f.Op(bc.OpRet)
	})
	b.ReportAllocs()
	for b.Loop() {
		runPure(b, p, bigLimits)
	}
}

// suspendedWith returns a VM suspended on now() holding n live list
// elements (and n strings).
func suspendedWith(b *testing.B, n int) (*bc.Verified, *vm.VM) {
	p := program(b, 0, func(_ *bc.Builder, f *bc.FuncBuilder) {
		f.Int(0)
		f.Int(int64(n))
		f.Builtin(bc.BRange)
		f.Op(bc.OpDup)
		f.Emit(bc.OpInterp, 1)
		f.Effect(bc.EffectNow, 0)
		f.Op(bc.OpPop, bc.OpPop, bc.OpRet)
	})
	v, err := vm.New(p, bigLimits)
	if err != nil {
		b.Fatal(err)
	}
	_ = v.Start(nil)
	if out, _ := v.Run(math.MaxInt64); out == nil {
		b.Fatal("no outcome")
	}
	return p, v
}

func BenchmarkVM02_Snapshot(b *testing.B) {
	for _, n := range []int{10, 1000, 100_000} {
		b.Run(fmt.Sprintf("snapshot/%d", n), func(b *testing.B) {
			_, v := suspendedWith(b, n)
			var size int
			for b.Loop() {
				s, err := v.Snapshot()
				if err != nil {
					b.Fatal(err)
				}
				size = len(s)
			}
			b.ReportMetric(float64(size), "bytes")
		})
		b.Run(fmt.Sprintf("restore_resume/%d", n), func(b *testing.B) {
			p, v := suspendedWith(b, n)
			s, _ := v.Snapshot()
			for b.Loop() {
				r, err := vm.Restore(p, s, bigLimits)
				if err != nil {
					b.Fatal(err)
				}
				if out, _ := r.Resume(vm.OkResult(vm.Time(0)), math.MaxInt64); out == nil {
					b.Fatal("no outcome")
				}
			}
		})
	}
}

func BenchmarkVM03_Replay(b *testing.B) {
	for _, n := range []int{10_000, 100_000} {
		p := program(b, 0, func(_ *bc.Builder, f *bc.FuncBuilder) {
			i := f.Local()
			f.Int(0)
			h := f.Here()
			f.Store(i)
			f.Load(i)
			f.Int(int64(n))
			f.Op(bc.OpLt)
			exit := f.NewLabel()
			f.JumpIfFalse(exit)
			f.Load(i)
			f.ToolCall("httpx", 1)
			f.Op(bc.OpPop)
			f.Load(i)
			f.Int(1)
			f.Op(bc.OpAddI)
			f.Loop(h)
			f.Bind(exit)
			f.Op(bc.OpUnit, bc.OpRet)
		})
		live := func(s vm.Suspended) vm.EffectResult { return vm.OkResult(vm.Str("ok")) }
		_, trace, err := vm.RunWithEffects(p, bigLimits, nil, live)
		if err != nil || len(trace) != n {
			b.Fatal(err, len(trace))
		}
		b.Run(fmt.Sprintf("replay/%d", n), func(b *testing.B) {
			for b.Loop() {
				r := vm.NewReplayer(trace, nil)
				out, _, _ := vm.RunWithEffects(p, bigLimits, nil, r.Handle)
				if _, ok := out.(vm.Completed); !ok || r.Replayed != n {
					b.Fatalf("%#v", out)
				}
			}
			b.ReportMetric(float64(b.N)*float64(n)/b.Elapsed().Seconds(), "effects/s")
		})
	}
}
