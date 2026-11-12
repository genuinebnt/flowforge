package sim

import (
	"context"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
)

func TestTmpCompile(t *testing.T) {
	var tools []engine.ToolSpec
	for _, s := range toolSpecs() {
		s.SpecSHA256 = s.ComputeSHA256()
		tools = append(tools, s)
	}
	for _, w := range workflowSources {
		res, err := compilerPort{}.Compile(context.Background(), engine.CompileRequest{Name: w.name, Source: w.src, Tools: tools, Policy: &engine.ProgramPolicy{MaxClass: core.ClassIntrusive}})
		if err != nil || !res.OK {
			t.Errorf("%s: %v %+v", w.name, err, res.Diagnostics)
			continue
		}
		t.Logf("%s ok effects=%v class=%v", w.name, res.Effects, res.Class)
	}
}
