package compiler_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/dsl"
	"github.com/genuinebnt/flowforge/testkit"
	"github.com/genuinebnt/flowforge/vm"
)

// FuzzCompile (spec 25.5): for arbitrary source, Compile returns either
// diagnostics or a module that verifies; it never panics and never
// reports an internal error (E0699). Accepted modules also run on the VM
// (bounded fuel, every effect failing) without a VM error.
func FuzzCompile(f *testing.F) {
	for _, suite := range []string{"dsl/check", "dsl/parse"} {
		names, _ := filepath.Glob(filepath.Join(testkit.ConformanceDir(f, suite), "*.ff"))
		for _, n := range names {
			if b, err := os.ReadFile(n); err == nil {
				f.Add(string(b))
			}
		}
	}
	runs, _ := filepath.Glob(filepath.Join(testkit.ConformanceDir(f, "vm/run"), "*", "case.ff"))
	for _, n := range runs {
		if b, err := os.ReadFile(n); err == nil {
			f.Add(string(b))
		}
	}
	f.Fuzz(func(t *testing.T, src string) {
		file, _ := dsl.Parse("fuzz.ff", []byte(src))
		var workflows []string
		for _, it := range file.Items {
			if w, ok := it.(*dsl.WorkflowDecl); ok && w.Name != nil {
				workflows = append(workflows, w.Name.Name)
			}
		}
		if len(workflows) == 0 {
			workflows = []string{""}
		}
		for _, w := range workflows {
			res, diags := compiler.Compile(src, compiler.Options{Workflow: w, AllFns: true})
			for _, d := range diags {
				if d.Code == compiler.CodeInternal {
					t.Fatalf("internal compiler error: %v\n%s", d, src)
				}
			}
			if res == nil {
				if !dsl.HasErrors(diags) {
					t.Fatalf("no module and no error\n%s", src)
				}
				continue
			}
			if _, err := bytecode.Verify(res.Module, res.Tools); err != nil {
				t.Fatalf("verify: %v\n%s", err, src)
			}
			if res.Module.Funcs[res.Module.Entry].Arity != 0 {
				continue
			}
			_, _, err := vm.RunWithEffects(res.Verified, vm.Config{FuelLimit: 20_000}, nil,
				func(vm.Suspended) vm.EffectResult { return vm.FailResult(bytecode.FailEffectFailed, "fuzz") })
			if err != nil {
				t.Fatalf("vm: %v\n%s", err, src)
			}
		}
	})
}
