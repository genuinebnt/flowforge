package compiler_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/vm"
)

var vmBenchPrograms = []struct{ name, src string }{
	{"fib30", "fn fib(n: Int) -> Int { if n < 2 { n } else { fib(n - 1) + fib(n - 2) } }\nworkflow main() -> Int { fib(30) }"},
	{"loop_arith", "workflow main() -> Int { fold(0..1000000, 0, (acc, i) => acc + i * 3 % 7 - 1) }"},
	{"map_filter_1M", "workflow main() -> Int { (0..1000000) |> map(x => x * 2) |> filter(x => x % 3 == 0) |> len }"},
	{"string_build", `workflow main() -> Int { len(fold(0..20000, "", (s, i) => if i % 100 == 0 { "" } else { s + "{i}," })) }`},
}

// BenchmarkVM01_TreeWalkVsVM (b_vm_01): the same programs on the bytecode
// VM and on the tree-walking evaluator. ns/op is one VM run plus one
// tree-walker run; the treewalk/vm metric is the ratio of their times
// (spec 25.8: the VM must be faster).
func BenchmarkVM01_TreeWalkVsVM(b *testing.B) {
	for _, p := range vmBenchPrograms {
		b.Run(p.name, func(b *testing.B) {
			res, diags := compiler.Compile(p.src, compiler.Options{})
			if res == nil {
				b.Fatal(diags)
			}
			cfg := vm.Config{FuelLimit: 1 << 40, Limits: vm.Limits{Heap: 1 << 30}}
			none := func(vm.Suspended) vm.EffectResult { return vm.EffectResult{} }
			var vmTime, twTime time.Duration
			b.ReportAllocs()
			for b.Loop() {
				t0 := time.Now()
				out, _, err := vm.RunWithEffects(res.Verified, cfg, nil, none)
				vmTime += time.Since(t0)
				if _, ok := out.(vm.Completed); !ok || err != nil {
					b.Fatalf("vm: %v %v", out, err)
				}
				t1 := time.Now()
				tw, _, err := runTreeWalkPlain(res, cfg, none)
				twTime += time.Since(t1)
				if err != nil || sameOutcome(out, tw) != nil {
					b.Fatalf("tree-walker: %v %v", tw, err)
				}
			}
			b.ReportMetric(float64(twTime)/float64(vmTime), "treewalk/vm")
			b.ReportMetric(float64(vmTime.Nanoseconds())/float64(b.N), "vm-ns/run")
			b.ReportMetric(float64(twTime.Nanoseconds())/float64(b.N), "treewalk-ns/run")
		})
	}
}

func runTreeWalkPlain(res *compiler.Result, cfg vm.Config, h vm.Handler) (vm.Outcome, []vm.TraceEntry, error) {
	ro, err := runTreeWalk(res, &script{}, cfg, nil, h)
	return ro.out, ro.trace, err
}

// genSource returns a generated program of at least size bytes.
func genSource(size int) string {
	g := &progGen{r: core.NewSplitMix64(uint64(size))}
	var b strings.Builder
	b.WriteString(genPrelude)
	for b.Len() < size {
		b.WriteString(g.fn())
		if len(g.fns) > 30 {
			g.fns = g.fns[1:]
		}
	}
	b.WriteString("workflow main() -> Int { 0 }\n")
	return b.String()
}

// BenchmarkCompile (b_dsl_01 for the code generator): parse, check,
// compile and verify a 10 KB and a 1 MB generated program.
func BenchmarkCompile(b *testing.B) {
	for _, size := range []int{10 << 10, 1 << 20} {
		src := genSource(size)
		b.Run(fmt.Sprintf("%dKB", size>>10), func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				res, diags := compiler.Compile(src, compiler.Options{AllFns: true})
				if res == nil {
					b.Fatal(diags[len(diags)-1])
				}
			}
		})
	}
}
