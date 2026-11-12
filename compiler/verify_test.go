package compiler_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/dsl"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestCompilerProp_VerifierAcceptsOutput: every program of
// conformance/dsl/check that checks without errors compiles, for each of
// its workflows, into a module the verifier accepts (Compile verifies; a
// rejection is E0699).
func TestCompilerProp_VerifierAcceptsOutput(t *testing.T) {
	dir := testkit.ConformanceDir(t, "dsl/check")
	names, _ := filepath.Glob(filepath.Join(dir, "*.ff"))
	sort.Strings(names)
	compiled := 0
	for _, n := range names {
		src, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		f, pd := dsl.Parse("x.ff", src)
		prog, cd := dsl.Check(f, dsl.Options{})
		if dsl.HasErrors(pd) || dsl.HasErrors(cd) {
			continue
		}
		if len(prog.Workflows) == 0 && len(prog.Fns) > 0 {
			t.Run(strings.TrimSuffix(filepath.Base(n), ".ff")+"/fns", func(t *testing.T) {
				src := string(src) + "\nworkflow compile_all_fns() {}\n"
				res, diags := compiler.Compile(src, compiler.Options{AllFns: true})
				if res == nil {
					t.Fatalf("compile failed: %v", diags)
				}
			})
			compiled++
		}
		for _, w := range prog.Workflows {
			name := w.Decl.Name.Name
			t.Run(strings.TrimSuffix(filepath.Base(n), ".ff")+"/"+name, func(t *testing.T) {
				res, diags := compiler.Compile(string(src), compiler.Options{Workflow: name})
				if res == nil {
					t.Fatalf("compile failed: %v", diags)
				}
			})
			compiled++
		}
	}
	if compiled < 10 {
		t.Fatalf("only %d workflows compiled", compiled)
	}
}
