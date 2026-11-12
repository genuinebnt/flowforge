package compiler

import (
	"context"
	"strings"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dsl"
)

// EngineAdapter exposes Compile in the shape of the engine's Compiler port
// (engine.Compiler: Check and Compile on a request with the workflow name,
// source, optional program policy and the registered tools). It uses only
// plain structs, so this package does not import the engine; services
// wire it by converting the request and result field by field (the
// diagnostic JSON is identical, spec 7.7).
type EngineAdapter struct {
	// Version is recorded in META (compiler.Version when empty).
	Version string
}

// AdapterTool is a registered tool as the engine describes it.
type AdapterTool struct {
	Name string `json:"name"`
	// InputType is the input type text: one type for a one-input tool, a
	// tuple "(A, B)" for several, "()" for none.
	InputType  string `json:"input_type"`
	OutputType string `json:"output_type"`
	// EffectClass is the tool's declared effect (passive, active, read, ...).
	EffectClass string `json:"effect_class"`
	Idempotent  bool   `json:"idempotent"`
	// SpecSHA256 is the registry hash pinned in TOOLS.
	SpecSHA256 [32]byte `json:"-"`
}

// AdapterPolicy is the part of a program policy the compiler checks.
type AdapterPolicy struct {
	MaxClass core.ActivityClass `json:"max_class"`
}

// AdapterRequest is the input of Check and Compile.
type AdapterRequest struct {
	// Name is the workflow to compile; diagnostics use "<name>.ff".
	Name   string
	Source string
	// Policy is nil when no program is given (internal consistency only).
	Policy *AdapterPolicy
	Tools  []AdapterTool
}

// AdapterCheckResult is the front-end outcome (proto CheckWorkflowResponse).
type AdapterCheckResult struct {
	Diagnostics []dsl.Diagnostic `json:"diagnostics"`
	// Effects are the module's effect names: the DSL effects of spec 7.5,
	// then the engine effects scope, approval, alert and plan.
	Effects []string           `json:"inferred_effects"`
	Class   core.ActivityClass `json:"inferred_class"`
	OK      bool               `json:"ok"`
}

// AdapterAttributes are the workflow-level annotations, also present in
// the module's ANNOTS section with the per-effect policies.
type AdapterAttributes struct {
	TimeoutNanos   int64 `json:"timeout_ns,omitempty"`
	MaxEffects     int   `json:"max_effects,omitempty"`
	OnUnknownRetry bool  `json:"on_unknown_retry,omitempty"`
	Idempotent     bool  `json:"idempotent,omitempty"`
}

// AdapterCompileResult is a check result plus, when OK, the module.
type AdapterCompileResult struct {
	AdapterCheckResult
	Module       []byte            `json:"-"`
	ModuleSHA256 [32]byte          `json:"-"`
	Attributes   AdapterAttributes `json:"attributes"`
}

// Check compiles without returning the module. The error result is
// reserved for failures of the compiler itself (never for bad source) and
// for a cancelled context.
func (a EngineAdapter) Check(ctx context.Context, req AdapterRequest) (AdapterCheckResult, error) {
	r, err := a.Compile(ctx, req)
	return r.AdapterCheckResult, err
}

// Compile compiles req.Name of req.Source into a verified module.
func (a EngineAdapter) Compile(ctx context.Context, req AdapterRequest) (AdapterCompileResult, error) {
	if err := ctx.Err(); err != nil {
		return AdapterCompileResult{}, err
	}
	opts := Options{Workflow: req.Name, CompilerVersion: a.Version}
	if req.Name != "" {
		opts.FileName = req.Name + ".ff"
	}
	if req.Policy != nil {
		opts.Policy = dsl.Policy{MaxClass: req.Policy.MaxClass, Known: true}
	}
	for _, t := range req.Tools {
		opts.Tools = append(opts.Tools, Tool{Spec: t.toolSpec(), SpecSHA256: t.SpecSHA256, Idempotent: t.Idempotent})
	}
	res, diags := Compile(req.Source, opts)
	if diags == nil {
		diags = []dsl.Diagnostic{}
	}
	out := AdapterCompileResult{AdapterCheckResult: AdapterCheckResult{Diagnostics: diags, Effects: []string{}}}
	if res == nil {
		// Still report what the front end inferred, as CheckWorkflow does.
		an := dsl.Analyze(req.Source, dsl.Options{Policy: opts.Policy, Tools: toolSpecs(opts.Tools), FileName: opts.FileName})
		out.Effects, out.Class = an.InferredEffects, an.InferredClass
		return out, nil
	}
	out.OK = true
	out.Effects, out.Class = res.Effects, res.Class
	out.Module, out.ModuleSHA256 = res.Encoded, res.ModuleSHA256
	if an := res.Annotations(); an != nil {
		out.Attributes = AdapterAttributes{TimeoutNanos: an.Timeout, MaxEffects: int(an.MaxEffects),
			OnUnknownRetry: an.OnUnknownRetry, Idempotent: an.Idempotent}
	}
	return out, nil
}

// toolSpec turns the engine's description into a checker declaration.
func (t AdapterTool) toolSpec() dsl.ToolSpec {
	in := strings.TrimSpace(t.InputType)
	sig := "(" + in + ") -> " + t.OutputType
	if strings.HasPrefix(in, "(") {
		sig = in + " -> " + t.OutputType // "()" or a tuple of inputs
	}
	var effects []string
	if t.EffectClass != "" {
		effects = []string{t.EffectClass}
	}
	return dsl.ToolSpec{Name: t.Name, Signature: sig, Effects: effects}
}
