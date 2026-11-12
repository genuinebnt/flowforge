package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/engine"
)

// compilerPort adapts the real DSL compiler (compiler.EngineAdapter) to
// the engine's Compiler port, converting the plain structs field by field
// as the services do.
type compilerPort struct{ a compiler.EngineAdapter }

var _ engine.Compiler = compilerPort{}

func (c compilerPort) Check(ctx context.Context, req engine.CompileRequest) (engine.CheckResult, error) {
	r, err := c.Compile(ctx, req)
	return r.CheckResult, err
}

func (c compilerPort) Compile(ctx context.Context, req engine.CompileRequest) (engine.CompileResult, error) {
	areq := compiler.AdapterRequest{Name: req.Name, Source: req.Source}
	if req.Policy != nil {
		areq.Policy = &compiler.AdapterPolicy{MaxClass: req.Policy.MaxClass}
	}
	for _, t := range req.Tools {
		areq.Tools = append(areq.Tools, compiler.AdapterTool{
			Name: t.Name, InputType: t.InputType, OutputType: t.OutputType,
			EffectClass: t.EffectClass, Idempotent: t.Idempotent, SpecSHA256: t.SpecSHA256,
		})
	}
	res, err := c.a.Compile(ctx, areq)
	if err != nil {
		return engine.CompileResult{}, err
	}
	var diags []engine.Diagnostic
	b, err := json.Marshal(res.Diagnostics)
	if err != nil {
		return engine.CompileResult{}, fmt.Errorf("sim: encode diagnostics: %w", err)
	}
	if err := json.Unmarshal(b, &diags); err != nil {
		return engine.CompileResult{}, fmt.Errorf("sim: decode diagnostics: %w", err)
	}
	return engine.CompileResult{
		CheckResult: engine.CheckResult{Diagnostics: diags, Effects: res.Effects, Class: res.Class, OK: res.OK},
		Module:      res.Module,
		Attributes: engine.WorkflowAttributes{
			Timeout: time.Duration(res.Attributes.TimeoutNanos), MaxEffects: res.Attributes.MaxEffects,
			OnUnknownRetry: res.Attributes.OnUnknownRetry,
		},
	}, nil
}
