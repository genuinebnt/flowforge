package treewalk_test

import (
	"errors"
	"testing"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler/treewalk"
	"github.com/genuinebnt/flowforge/dsl"
	"github.com/genuinebnt/flowforge/vm"
)

func check(t *testing.T, src string) *dsl.Program {
	t.Helper()
	f, d := dsl.Parse("t.ff", []byte(src))
	prog, cd := dsl.Check(f, dsl.Options{})
	if dsl.HasErrors(d) || dsl.HasErrors(cd) {
		t.Fatalf("%v %v", d, cd)
	}
	return prog
}

// The evaluator works without a compiled module: closures get private
// function indices and effects go to the handler with VM paths.
func TestTreeWalk_Standalone(t *testing.T) {
	prog := check(t, `
tool probe(n: Int) -> Int ! passive
workflow w(k: Int) -> List<Int> {
  let add = x => x + k;
  [1, 2] |> par_map(n => add(probe(n)), limit = 2)
}`)
	h := func(s vm.Suspended) vm.EffectResult { return vm.OkResult(vm.Int(s.Args[1].AsInt() * 10)) }
	out, trace, err := treewalk.Run(prog, "w", []vm.Value{vm.Int(5)}, treewalk.Config{}, h)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := out.(vm.Completed)
	if !ok || vm.Display(c.Value) != "[15, 25]" {
		t.Fatalf("outcome %v", out)
	}
	paths := []string{}
	for _, e := range trace {
		paths = append(paths, e.Path+" "+e.Kind.String())
	}
	want := []string{"0 par_map", "0/0/0 tool_call", "0/1/0 tool_call"}
	if len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] || paths[2] != want[2] {
		t.Fatalf("trace %v", paths)
	}
}

func TestTreeWalk_FailuresAndSetup(t *testing.T) {
	prog := check(t, "workflow w(n: Int) -> Int { 10 / n }")
	out, _, err := treewalk.Run(prog, "w", []vm.Value{vm.Int(0)}, treewalk.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := out.(vm.Failed); !ok || f.Code != bytecode.FailDivZero || !f.Span.HasSource {
		t.Fatalf("outcome %+v", out)
	}
	if _, _, err := treewalk.Run(prog, "nope", nil, treewalk.Config{}, nil); !errors.Is(err, treewalk.ErrSetup) {
		t.Fatalf("unknown workflow: %v", err)
	}
	if _, _, err := treewalk.Run(prog, "w", nil, treewalk.Config{}, nil); !errors.Is(err, treewalk.ErrSetup) {
		t.Fatalf("missing input: %v", err)
	}
	if _, err := treewalk.New(nil, treewalk.Config{}); !errors.Is(err, treewalk.ErrSetup) {
		t.Fatal("nil program accepted")
	}
}
