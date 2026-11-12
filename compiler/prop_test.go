package compiler_test

import (
	"testing"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
	"github.com/genuinebnt/flowforge/vm"
)

// genHandler answers the effects of generated programs deterministically:
// probe(n) is (3n mod 17) + 1 and fails for n = 13; log returns ().
func genHandler(s vm.Suspended) vm.EffectResult {
	if s.Kind == bytecode.EffectToolCall && len(s.Args) == 2 {
		n := s.Args[1].AsInt()
		if n == 13 {
			return vm.FailResult(bytecode.FailEffectFailed, "probe 13")
		}
		m := (3*n)%17 + 1
		if m < 0 {
			m = -m
		}
		return vm.OkResult(vm.Int(m))
	}
	return vm.OkResult(vm.Unit())
}

// genCompile generates and compiles one program; a compile failure is a
// generator or compiler bug.
func genCompile(t *testing.T, r core.Rng, i int) (*compiler.Result, string) {
	t.Helper()
	g := &progGen{r: r}
	src := g.program(1 + g.intn(4))
	res, diags := compiler.Compile(src, compiler.Options{FileName: "gen.ff"})
	if res == nil {
		t.Fatalf("case %d: generated program does not compile: %v\n%s", i, diags, src)
	}
	return res, src
}

func genCases() int { return max(testkit.Cases()/4, 50) }

// TestCompilerProp_GeneratedVerify: generated well-typed programs compile
// and verify (Compile runs bytecode.Verify; the result is re-verified).
func TestCompilerProp_GeneratedVerify(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < genCases(); i++ {
		res, src := genCompile(t, r, i)
		if _, err := bytecode.Verify(res.Module, res.Tools); err != nil {
			t.Fatalf("case %d: %v\n%s", i, err, src)
		}
	}
}

// TestCompilerProp_TreeWalkEqualsVM (spec 25.4 ff-dsl): for generated
// programs the tree-walker and the VM agree on the final value or failure
// code and on the effect trace.
func TestCompilerProp_TreeWalkEqualsVM(t *testing.T) {
	r := testkit.Rng(t)
	sc := &script{}
	for i := 0; i < genCases(); i++ {
		res, src := genCompile(t, r, i)
		d := &vmDriver{prog: res.Verified, sc: sc, h: genHandler}
		want, err := d.run(nil)
		if err != nil {
			t.Fatalf("case %d: vm: %v\n%s", i, err, src)
		}
		got, err := runTreeWalk(res, sc, vm.Config{}, nil, genHandler)
		if err != nil {
			t.Fatalf("case %d: tree-walker: %v\n%s", i, err, src)
		}
		if err := sameRun(want, got); err != nil {
			t.Fatalf("case %d: %v\n%s", i, err, src)
		}
	}
}
