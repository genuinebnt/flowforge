package compiler_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/testkit"
	"github.com/genuinebnt/flowforge/vm"
)

// TestCompilerAnnotations_Recon: the spec 7.1 recon workflow's #[timeout]
// and the #[retry] of its `web` statement reach the module: the retry
// covers the par_map effect and the effects of the par_map closure, and
// a VM suspension finds its policy through Suspended.Site.
func TestCompilerAnnotations_Recon(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(testkit.ConformanceDir(t, "vm/run"), "spec_recon", "case.ff"))
	if err != nil {
		t.Fatal(err)
	}
	res, diags := compiler.Compile(string(src), compiler.Options{})
	if res == nil {
		t.Fatal(diags)
	}
	a := res.Annotations()
	if a == nil || a.Timeout != int64(2*time.Hour) {
		t.Fatalf("workflow timeout not recorded: %+v", a)
	}
	// Decoding the encoded module gives the same section.
	m, err := bytecode.Decode(res.Encoded)
	if err != nil || !reflect.DeepEqual(m.Annotations, a) {
		t.Fatalf("ANNOTS does not round-trip: %v\n%+v\n%+v", err, m.Annotations, a)
	}
	kinds := map[bytecode.EffectKind]int{}
	for _, s := range a.Sites {
		if s.RetryMax != 3 || s.BackoffInitial != int64(time.Second) || s.BackoffMax != int64(30*time.Second) {
			t.Errorf("site %+v: wrong retry policy", s)
		}
		kinds[effectAt(res.Module, s)]++
	}
	want := map[bytecode.EffectKind]int{bytecode.EffectParMap: 1, bytecode.EffectScopeCheck: 1, bytecode.EffectToolCall: 1}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("annotated sites %v, want %v", kinds, want)
	}
	// At run time each httpx suspension resolves to the retry policy and
	// the subfinder calls (outside the statement) to none.
	var c vmCase
	for _, x := range loadVMCases(t, "run") {
		if x.name == "spec_recon" {
			c = x
		}
	}
	h := c.sc.handler(res.Module)
	checked := 0
	probe := func(s vm.Suspended) vm.EffectResult {
		site, ok := a.Site(uint16(s.Site.Func), uint32(s.Site.CodeOffset))
		if s.Kind == bytecode.EffectToolCall && s.Tool() == "httpx" {
			if !ok || site.RetryMax != 3 {
				t.Errorf("httpx at %v has no retry policy", s.Path)
			}
			checked++
		}
		if s.Kind == bytecode.EffectToolCall && s.Tool() == "subfinder" && ok {
			t.Errorf("subfinder at %v is annotated", s.Path)
		}
		return h(s)
	}
	d := &vmDriver{prog: res.Verified, cfg: c.sc.vmConfig(), sc: &c.sc, h: probe}
	if _, err := d.run(caseInputs(t, res, c)); err != nil {
		t.Fatal(err)
	}
	if checked != 3 {
		t.Fatalf("checked %d httpx calls, want 3", checked)
	}
}

func effectAt(m *bytecode.Module, s bytecode.SiteAnnotation) bytecode.EffectKind {
	code := m.Funcs[s.Func].Code
	if int(s.CodeOffset)+1 >= len(code) || bytecode.Opcode(code[s.CodeOffset]) != bytecode.OpEffect {
		return 0
	}
	return bytecode.EffectKind(code[s.CodeOffset+1])
}

// TestCompilerAnnotations_All: every annotation the engine enforces is in
// the module: workflow #[timeout], #[effects(max)], #[on_unknown(retry)],
// #[idempotent]; let #[timeout] and #[retry]; #[idempotent] tools and
// registry tools; the section is absent when nothing is annotated.
func TestCompilerAnnotations_All(t *testing.T) {
	src := `
#[idempotent]
tool fetch(u: Url) -> String ! read
tool store(s: String) -> Int ! write
tool lookup(n: Fqdn) -> Option<IpAddress> ! passive

#[timeout(10m), effects(max = 50000), on_unknown(retry), idempotent]
workflow w(u: Url) -> Int {
  let page = fetch(u);
  #[timeout(30s)]
  let n = store(page);
  #[retry(max = 5, backoff = exp(2s, 1m))]
  let m = store(page + "!");
  let ip = lookup("a.example.com" as Fqdn);
  n + m
}
`
	res, diags := compiler.Compile(src, compiler.Options{
		Tools: []compiler.Tool{{Spec: dslTool("lookup", "(Fqdn) -> Option<IpAddress>", "passive"), Idempotent: true}},
	})
	if res == nil {
		t.Fatal(diags)
	}
	a := res.Annotations()
	if a == nil || a.Timeout != int64(10*time.Minute) || a.MaxEffects != 50000 || !a.OnUnknownRetry || !a.Idempotent {
		t.Fatalf("workflow annotations: %+v", a)
	}
	if len(a.Sites) != 4 {
		t.Fatalf("sites: %+v", a.Sites)
	}
	byTool := map[string]bytecode.SiteAnnotation{}
	for _, s := range a.Sites {
		code := res.Module.Funcs[s.Func].Code
		k, _ := res.Module.ConstString(uint32(code[s.CodeOffset-3+1]) | uint32(code[s.CodeOffset-3+2])<<8)
		if _, dup := byTool[k]; dup {
			k += "#2"
		}
		byTool[k] = s
		if !s.Idempotent {
			t.Errorf("site %+v of an idempotent workflow is not idempotent", s)
		}
	}
	if s := byTool["store"]; s.Timeout != int64(30*time.Second) || s.RetryMax != 0 {
		t.Errorf("timeout let: %+v", s)
	}
	if s := byTool["store#2"]; s.RetryMax != 5 || s.BackoffInitial != int64(2*time.Second) || s.BackoffMax != int64(time.Minute) || s.Timeout != 0 {
		t.Errorf("retry let: %+v", s)
	}
	if _, ok := byTool["fetch"]; !ok {
		t.Error("idempotent tool site missing")
	}

	plain, diags := compiler.Compile("workflow p() -> Int { 1 }", compiler.Options{})
	if plain == nil {
		t.Fatal(diags)
	}
	if plain.Annotations() != nil || plain.Module.Flags != 0 {
		t.Fatal("an unannotated workflow must keep the plain module layout")
	}
}
