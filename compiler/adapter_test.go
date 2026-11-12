package compiler_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/core"
)

func TestEngineAdapter(t *testing.T) {
	src := `
type Svc = { status: Int }
#[timeout(5m), effects(max = 200)]
workflow probe_all(hosts: List<Fqdn>) -> Int {
  let ips = hosts |> filter_map(resolve);
  let up = hosts |> filter_map(h => match scope.check(Host::Name(h)) { Ok(p) => httpx(p), Err(_) => None });
  len(ips) + len(up)
}
`
	tools := []compiler.AdapterTool{
		{Name: "resolve", InputType: "Fqdn", OutputType: "Option<IpAddress>", EffectClass: "passive", SpecSHA256: sha256.Sum256([]byte("r1"))},
		{Name: "httpx", InputType: "InScope<Host>", OutputType: "Option<Svc>", EffectClass: "active", Idempotent: true, SpecSHA256: sha256.Sum256([]byte("h1"))},
	}
	a := compiler.EngineAdapter{Version: "adapter-test"}
	req := compiler.AdapterRequest{Name: "probe_all", Source: src, Policy: &compiler.AdapterPolicy{MaxClass: core.ClassActive}, Tools: tools}
	res, err := a.Compile(context.Background(), req)
	if err != nil || !res.OK {
		t.Fatalf("%v %+v", err, res.Diagnostics)
	}
	if !slices.Equal(res.Effects, []string{"passive", "active", "scope"}) || res.Class != core.ClassActive {
		t.Fatalf("effects %v class %v", res.Effects, res.Class)
	}
	if res.Attributes.TimeoutNanos != int64(5*time.Minute) || res.Attributes.MaxEffects != 200 {
		t.Fatalf("attributes %+v", res.Attributes)
	}
	m, err := bytecode.Decode(res.Module)
	if err != nil || m.Meta.CompilerVersion != "adapter-test" || res.ModuleSHA256 != sha256.Sum256(res.Module) {
		t.Fatalf("module: %v", err)
	}
	reg := bytecode.ToolMap{"resolve": {SpecSHA256: tools[0].SpecSHA256, Effects: bytecode.EffPassive},
		"httpx": {SpecSHA256: tools[1].SpecSHA256, Effects: bytecode.EffActive}}
	if _, err := bytecode.Verify(m, reg); err != nil {
		t.Fatalf("engine-side verification: %v", err)
	}
	// The diagnostics JSON has the spec 7.7 shape.
	req.Policy.MaxClass = core.ClassPassive
	chk, err := a.Check(context.Background(), req)
	if err != nil || chk.OK || len(chk.Diagnostics) == 0 || chk.Diagnostics[0].Code != "E0402" {
		t.Fatalf("check: %v %+v", err, chk)
	}
	if chk.Diagnostics[0].Primary.File != "probe_all.ff" {
		t.Fatalf("file %q", chk.Diagnostics[0].Primary.File)
	}
	b, _ := json.Marshal(chk.Diagnostics[0])
	var shape map[string]any
	_ = json.Unmarshal(b, &shape)
	for _, k := range []string{"code", "severity", "message", "primary", "secondary"} {
		if _, ok := shape[k]; !ok {
			t.Errorf("diagnostic JSON lacks %q: %s", k, b)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Compile(ctx, req); err == nil {
		t.Fatal("a cancelled context must be reported")
	}
}
