package compiler_test

import (
	"testing"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/dsl"
)

// notLowered lists the native builtins no DSL construct lowers to: the
// language has no plain sort (sort_by lowers to sort_by_keys), no map
// literal or update (map_insert), no Host parsing cast (Host is an enum)
// and no list append operator (list_append).
var notLowered = map[bytecode.BuiltinID]bool{
	bytecode.BSort: true, bytecode.BMapInsert: true, bytecode.BHostParse: true, bytecode.BListAppend: true,
}

// TestCompilerCoverage_BuiltinsAndEffects: the conformance/vm/run cases
// exercise every standard-library function, every EFFECT kind of spec 8.4
// and every native builtin the compiler can emit.
func TestCompilerCoverage_BuiltinsAndEffects(t *testing.T) {
	usedLib := map[string]bool{}
	usedKinds := map[bytecode.EffectKind]bool{}
	usedNative := map[bytecode.BuiltinID]bool{}
	for _, c := range loadVMCases(t, "run") {
		res := compileCase(t, c)
		for _, it := range res.Program.File.Items {
			dsl.Walk(it, func(n dsl.Node) bool {
				switch x := n.(type) {
				case *dsl.CallExpr:
					if x.Builtin != nil {
						usedLib[x.Builtin.Name] = true
					}
				case *dsl.Ident:
					if x.Ref.Kind == dsl.RefBuiltin && x.Ref.Builtin != nil {
						usedLib[x.Ref.Builtin.Name] = true
					}
				}
				return true
			})
		}
		for _, f := range res.Module.Funcs {
			for pc := 0; pc < len(f.Code); pc += bytecode.Opcode(f.Code[pc]).Size() {
				switch bytecode.Opcode(f.Code[pc]) {
				case bytecode.OpEffect:
					usedKinds[bytecode.EffectKind(f.Code[pc+1])] = true
				case bytecode.OpBuiltin:
					usedNative[bytecode.BuiltinID(uint16(f.Code[pc+1])|uint16(f.Code[pc+2])<<8)] = true
				}
			}
		}
	}
	for _, b := range dsl.Builtins() {
		if !usedLib[b.Name] {
			t.Errorf("library function %s is not exercised by conformance/vm/run", b.Name)
		}
	}
	for k := bytecode.EffectToolCall; k <= bytecode.EffectSimilarityQuery; k++ {
		if !usedKinds[k] {
			t.Errorf("effect kind %s is not exercised", k)
		}
	}
	for _, b := range bytecode.Builtins() {
		if !usedNative[b.ID] && !notLowered[b.ID] {
			t.Errorf("native builtin %s is not exercised", b.Name)
		}
		if usedNative[b.ID] && notLowered[b.ID] {
			t.Errorf("native builtin %s is emitted but listed as not lowered", b.Name)
		}
	}
}
