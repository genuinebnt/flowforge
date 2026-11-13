package compiler_test

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dsl"
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

// TestRegress_G_CORE_09_AdapterProhibited: the engine adapter dropped the
// program's activity.prohibited list, so a workflow calling a prohibited
// tool compiled. The list now reaches the policy check (E0402 at the
// tool's first use) and the META policy hash; unrelated entries change
// neither the outcome nor, when empty, the hash.
func TestRegress_G_CORE_09_AdapterProhibited(t *testing.T) {
	src := `
type Svc = { status: Int }
workflow probe(h: Fqdn) -> Int {
  let ip = resolve(h);
  match scope.check(Host::Name(h)) { Ok(p) => match httpx(p) { Some(s) => s.status, None => 0 }, Err(_) => 0 }
}
`
	tools := []compiler.AdapterTool{
		{Name: "resolve", InputType: "Fqdn", OutputType: "Option<IpAddress>", EffectClass: "passive", SpecSHA256: sha256.Sum256([]byte("r1"))},
		{Name: "httpx", InputType: "InScope<Host>", OutputType: "Option<Svc>", EffectClass: "active", Idempotent: true, SpecSHA256: sha256.Sum256([]byte("h1"))},
	}
	a := compiler.EngineAdapter{}
	compile := func(prohibited ...string) compiler.AdapterCompileResult {
		t.Helper()
		res, err := a.Compile(context.Background(), compiler.AdapterRequest{Name: "probe", Source: src,
			Policy: &compiler.AdapterPolicy{MaxClass: core.ClassIntrusive, Prohibited: prohibited}, Tools: tools})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for _, p := range [][]string{{"httpx"}, {"Active"}, {"passive"}, {"dos", "resolve"}} {
		res := compile(p...)
		if res.OK || len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != "E0402" || !strings.Contains(res.Diagnostics[0].Message, "prohibited") {
			t.Fatalf("prohibited %q: ok=%v %+v, want one E0402", p, res.OK, res.Diagnostics)
		}
	}
	base := compile()
	other := compile("dos", "social_engineering", "")
	if !base.OK || !other.OK {
		t.Fatalf("allowed tools rejected: %+v %+v", base.Diagnostics, other.Diagnostics)
	}
	mb, _ := bytecode.Decode(base.Module)
	mo, _ := bytecode.Decode(other.Module)
	if mb.Meta.ProgramPolicySHA256 != compiler.PolicySHA256(dsl.Policy{MaxClass: core.ClassIntrusive, Known: true}) {
		t.Fatal("an empty prohibited list changed the policy hash")
	}
	if mb.Meta.ProgramPolicySHA256 == mo.Meta.ProgramPolicySHA256 {
		t.Fatal("the policy hash ignores activity.prohibited")
	}
	if compiler.PolicySHA256(dsl.Policy{Known: true, Prohibited: []string{"B", " a"}}) != compiler.PolicySHA256(dsl.Policy{Known: true, Prohibited: []string{"a", "b", "a"}}) {
		t.Fatal("the policy hash depends on prohibited order, case or duplicates")
	}
}
