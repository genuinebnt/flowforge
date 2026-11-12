package compiler_test

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dsl"
	"github.com/genuinebnt/flowforge/testkit"
	"github.com/genuinebnt/flowforge/vm"
)

// TestCompiler_AgentWorkflowCompiles: the spec 7.1 triage workflow (agent
// step) compiles; its module uses agent_call and execute_plan, which the
// engine rejects at registration (DECISIONS D1).
func TestCompiler_AgentWorkflowCompiles(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(testkit.ConformanceDir(t, "dsl/check"), "spec_triage.ff"))
	if err != nil {
		t.Fatal(err)
	}
	res, diags := compiler.Compile(string(src), compiler.Options{})
	if res == nil {
		t.Fatal(diags)
	}
	for _, op := range []string{"agent_call", "execute_plan"} {
		if !slices.Contains(res.Ops, op) {
			t.Errorf("ops %v lack %s", res.Ops, op)
		}
	}
	if !slices.Contains(res.Effects, "llm") || !slices.Contains(res.Effects, "plan") {
		t.Errorf("effects %v lack llm and plan", res.Effects)
	}
}

// TestCompiler_ModuleMetadata: META, TOOLS, EFFECTS and the entry
// signature are filled in, and compilation is deterministic.
func TestCompiler_ModuleMetadata(t *testing.T) {
	src := `
tool resolve(name: Fqdn) -> Option<IpAddress> ! passive
workflow look(names: List<Fqdn>, n: Int) -> Int {
  let ips = names |> filter_map(resolve);
  emit_alert(Severity::Info, "found {len(ips)}");
  len(ips) + n
}
`
	policy := dsl.Policy{MaxClass: core.ClassActive, Known: true}
	reg := []compiler.Tool{{Spec: dslTool("resolve", "(Fqdn) -> Option<IpAddress>", "passive"), SpecSHA256: sha256.Sum256([]byte("resolve v3"))}}
	opts := compiler.Options{Policy: policy, Tools: reg, CompilerVersion: "test 1"}
	res, diags := compiler.Compile(src, opts)
	if res == nil {
		t.Fatal(diags)
	}
	m := res.Module
	if m.Meta.CompilerVersion != "test 1" || m.Meta.WorkflowName != "look" ||
		m.Meta.SourceSHA256 != sha256.Sum256([]byte(src)) || m.Meta.ProgramPolicySHA256 != compiler.PolicySHA256(policy) {
		t.Fatalf("META: %+v", m.Meta)
	}
	if len(m.Tools) != 1 || m.ToolName(0) != "resolve" || m.Tools[0].SpecSHA256 != reg[0].SpecSHA256 {
		t.Fatalf("TOOLS: %+v", m.Tools)
	}
	if want := bytecode.EffPassive | bytecode.EffAlert; m.Effects != want {
		t.Fatalf("EFFECTS %s, want %s", m.Effects, want)
	}
	if !slices.Equal(res.Effects, []string{"passive", "alert"}) || res.Class != core.ClassPassive {
		t.Fatalf("effects %v class %v", res.Effects, res.Class)
	}
	sig, ok := m.Signature(m.Entry)
	if !ok || len(sig.Fields) != 2 || sig.Fields[0].Type != "List<Fqdn>" || sig.Result != "Int" {
		t.Fatalf("signature %+v", sig)
	}
	if res.ModuleSHA256 != sha256.Sum256(res.Encoded) {
		t.Fatal("module hash")
	}
	again, _ := compiler.Compile(src, opts)
	if !bytes.Equal(again.Encoded, res.Encoded) {
		t.Fatal("compilation is not deterministic")
	}
	// The registry hash is what the verifier checks at load time.
	if _, err := bytecode.Verify(m, bytecode.ToolMap{"resolve": {SpecSHA256: sha256.Sum256([]byte("resolve v2")), Effects: bytecode.EffPassive}}); err == nil {
		t.Fatal("a changed tool hash must be rejected")
	}
}

// TestCompiler_Diagnostics: front-end errors, an unknown entry and an
// ambiguous entry are diagnostics, not panics.
func TestCompiler_Diagnostics(t *testing.T) {
	cases := []struct {
		name, src, workflow, code string
	}{
		{"type_error", "workflow w() -> Int { \"x\" }", "", dsl.CodeTypeMismatch},
		{"syntax", "workflow w( -> Int { 1 }", "", dsl.CodeUnexpectedToken},
		{"unknown_entry", "workflow w() -> Int { 1 }", "v", dsl.CodeUnknownName},
		{"two_workflows", "workflow a() {}\nworkflow b() {}", "", dsl.CodeUnknownName},
		{"no_workflow", "fn f() -> Int { 1 }", "", dsl.CodeUnknownName},
		{"policy", "tool scan(h: InScope<Host>) -> Int ! intrusive\nworkflow w(h: InScope<Host>) -> Int { scan(h) }", "", dsl.CodeEffectExceedsMax},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, diags := compiler.Compile(c.src, compiler.Options{Workflow: c.workflow, Policy: dsl.Policy{MaxClass: core.ClassPassive, Known: true}})
			if res != nil {
				t.Fatal("compiled")
			}
			found := false
			for _, d := range diags {
				found = found || d.Code == c.code
			}
			if !found {
				t.Fatalf("diagnostics %v lack %s", diags, c.code)
			}
		})
	}
	// Choosing among several workflows.
	res, diags := compiler.Compile("workflow a() -> Int { 1 }\nworkflow b() -> Int { 2 }", compiler.Options{Workflow: "b"})
	if res == nil || res.Module.Meta.WorkflowName != "b" {
		t.Fatal(diags)
	}
}

// TestCompiler_FailureSpans: a VM failure's span maps back to the source
// expression that failed.
func TestCompiler_FailureSpans(t *testing.T) {
	src := "fn ratio(a: Int, b: Int) -> Int {\n  let q = a / b;\n  q\n}\nworkflow w() -> Int { ratio(1, 0) }\n"
	res, diags := compiler.Compile(src, compiler.Options{FileName: "s.ff"})
	if res == nil {
		t.Fatal(diags)
	}
	out, _, err := vm.RunWithEffects(res.Verified, vm.Config{}, nil, func(vm.Suspended) vm.EffectResult { return vm.EffectResult{} })
	if err != nil {
		t.Fatal(err)
	}
	f, ok := out.(vm.Failed)
	if !ok || f.Code != bytecode.FailDivZero || !f.Span.HasSource {
		t.Fatalf("outcome %+v", out)
	}
	sp := res.SourceSpan(f.Span.SourceOffset, f.Span.Len, "here")
	if got := src[sp.Start.Offset:sp.End.Offset]; got != "a / b" || sp.Start.Line != 2 || sp.Start.Col != 11 || sp.File != "s.ff" {
		t.Fatalf("span %+v covers %q", sp, got)
	}
}
