package compiler_test

import (
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/vm"
)

// TestRegress_G_LANG_03_ParseIntDoc: the builtin table documented `_`
// separators for parse_int, which the VM rejects. The doc now matches the
// VM: sign and decimal digits only, None otherwise.
func TestRegress_G_LANG_03_ParseIntDoc(t *testing.T) {
	b, ok := bytecode.BuiltinByName("parse_int")
	if !ok || !strings.Contains(b.Doc, "no _") {
		t.Fatalf("parse_int doc %q does not state that _ is rejected", b.Doc)
	}
	for src, want := range map[string]string{`"1_000"`: "None", `"-42"`: "Some(-42)", `"+7"`: "Some(7)", `" 1"`: "None", `"0x10"`: "None"} {
		res, diags := compiler.Compile("workflow main() -> Option<Int> { parse_int("+src+") }", compiler.Options{})
		if res == nil {
			t.Fatal(diags)
		}
		out, _, err := vm.RunWithEffects(res.Verified, vm.Config{FuelLimit: 1 << 20}, nil, func(vm.Suspended) vm.EffectResult { return vm.EffectResult{} })
		c, ok := out.(vm.Completed)
		if err != nil || !ok {
			t.Fatalf("parse_int(%s): %v %v", src, out, err)
		}
		if got := c.Value.String(); got != want {
			t.Errorf("parse_int(%s) = %s, want %s", src, got, want)
		}
	}
}
