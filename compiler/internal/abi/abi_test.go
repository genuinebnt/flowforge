package abi

import (
	"testing"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/dsl"
)

func TestTypesAndVariants(t *testing.T) {
	f, _ := dsl.Parse("a.ff", []byte("type R = { a: Int }\nenum E { X, Y(Int) }\nworkflow w() {}"))
	prog, diags := dsl.Check(f, dsl.Options{})
	if dsl.HasErrors(diags) {
		t.Fatal(diags)
	}
	ty := NewTypes(prog)
	byName := map[string]*dsl.TypeDef{}
	for _, d := range prog.Types {
		byName[d.Name] = d
	}
	for name, want := range reserved {
		if id, ok := ty.ID(byName[name]); !ok || id != want {
			t.Errorf("%s: id %d, want %d", name, id, want)
		}
	}
	r, _ := ty.ID(byName["R"])
	e, _ := ty.ID(byName["E"])
	if r < bytecode.FirstUserType || e != r+1 || ty.Module[len(ty.Module)-1] != byName["E"] {
		t.Fatalf("user ids R=%d E=%d", r, e)
	}
	opt := byName["Option"]
	if VariantIndex(opt.VariantByName("Some")) != bytecode.VariantSome || VariantIndex(opt.VariantByName("None")) != bytecode.VariantNone {
		t.Fatal("Option variants")
	}
	if VariantIndex(byName["E"].VariantByName("Y")) != 1 {
		t.Fatal("user variants keep declaration order")
	}
}
