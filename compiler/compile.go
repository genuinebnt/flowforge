package compiler

import (
	"crypto/sha256"
	"fmt"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dsl"
)

// Version is the default compiler version recorded in META. It is part of
// the encoded module, so a different compiler (or version) gives a
// different module hash for the same source.
const Version = "flowforge-go-compiler 0.1.0"

// Tool is one entry of the tool registry the program is compiled against.
type Tool struct {
	// Spec is the declaration the checker sees (signature and effects).
	Spec dsl.ToolSpec
	// SpecSHA256 is the registry hash pinned in the module's TOOLS table.
	// The zero value means "derive it from Spec" (ToolSpecSHA256).
	SpecSHA256 [32]byte
	// Idempotent marks the tool as safe to repeat; tool_call sites of it
	// are annotated as idempotent for the engine.
	Idempotent bool
}

// Options configures Compile.
type Options struct {
	// FileName names the source in diagnostics; "<workflow>.ff" when empty.
	FileName string
	// Policy is the program policy (max_class); the zero value checks
	// internal consistency only.
	Policy dsl.Policy
	// Tools is the tool registry.
	Tools []Tool
	// Workflow names the workflow compiled as the module entry. When empty
	// the file must contain exactly one workflow.
	Workflow string
	// CompilerVersion is recorded in META (Version when empty).
	CompilerVersion string
	// AllFns also compiles every fn of the file, including those the entry
	// does not reach (tooling and tests use it to check whole files).
	AllFns bool
}

// Result is a successfully compiled workflow.
type Result struct {
	// Module is the verified module; Encoded its encoding (spec 8.3) and
	// ModuleSHA256 the module hash (SHA-256 of Encoded).
	Module       *bytecode.Module
	Encoded      []byte
	ModuleSHA256 [32]byte
	// Verified is the verifier's result for Module (the form the VM loads).
	Verified *bytecode.Verified
	// Tools resolves the module's TOOLS entries (registry hashes and
	// effects) for bytecode.Verify.
	Tools bytecode.ToolMap
	// Effects lists the names of the module's declared effect mask: the
	// DSL effects of spec 7.5 in canonical order, then the engine effects
	// scope, approval, alert and plan.
	Effects []string
	// Class is the workflow's activity class.
	Class core.ActivityClass
	// Ops lists the runtime operations (EFFECT kinds) the workflow may
	// perform, in kind order.
	Ops []string
	// Diagnostics are the warnings of a successful compile.
	Diagnostics []dsl.Diagnostic
	// SourceSHA256 and ProgramPolicySHA256 are the META hashes.
	SourceSHA256        [32]byte
	ProgramPolicySHA256 [32]byte
	// Workflow is the entry workflow's name.
	Workflow string
	// Program is the checked program the module was compiled from (the
	// tree-walking evaluator runs it).
	Program *dsl.Program
	// Closures maps closure functions of the module back to the source.
	Closures *ClosureTable
	// Source is the compiled source text.
	Source string
	file   string
}

// Annotations returns the module's ANNOTS section (nil when the workflow
// has no annotations the engine enforces).
func (r *Result) Annotations() *bytecode.Annotations { return r.Module.Annotations }

// Compile parses, checks and compiles the workflow opts.Workflow of src
// into a verified module. On failure it returns nil and the diagnostics
// (errors and warnings, in source order); on success the result carries
// the warnings. Compile never panics: a defect of the compiler itself is
// reported as diagnostic E0699.
func Compile(src string, opts Options) (res *Result, diags []dsl.Diagnostic) {
	file := opts.FileName
	if file == "" {
		file = defaultFileName(src, opts.Workflow)
	}
	defer func() {
		if r := recover(); r != nil {
			res = nil
			diags = append(diags, internalError(file, dsl.Span{}, fmt.Sprintf("panic: %v", r)))
		}
	}()
	f, pdiags := dsl.Parse(file, []byte(src))
	prog, cdiags := dsl.Check(f, dsl.Options{Policy: opts.Policy, Tools: toolSpecs(opts.Tools), FileName: file})
	diags = append(append(diags, pdiags...), cdiags...)
	dsl.SortDiagnostics(diags)
	if dsl.HasErrors(diags) {
		return nil, diags
	}
	wf, d := selectWorkflow(prog, opts.Workflow, file)
	if d != nil {
		return nil, append(diags, *d)
	}
	c := newCompiler(prog, wf, file, src, opts)
	res, cerrs := c.run()
	if len(cerrs) > 0 {
		return nil, append(diags, cerrs...)
	}
	res.Diagnostics = diags
	if res.Diagnostics == nil {
		res.Diagnostics = []dsl.Diagnostic{}
	}
	return res, res.Diagnostics
}

// defaultFileName mirrors dsl.Analyze: "<workflow>.ff", else "main.ff".
func defaultFileName(src, workflow string) string {
	if workflow != "" {
		return workflow + ".ff"
	}
	f, _ := dsl.Parse("main.ff", []byte(src))
	for _, it := range f.Items {
		if w, ok := it.(*dsl.WorkflowDecl); ok && w.Name != nil {
			return w.Name.Name + ".ff"
		}
	}
	return "main.ff"
}

func toolSpecs(tools []Tool) []dsl.ToolSpec {
	out := make([]dsl.ToolSpec, len(tools))
	for i, t := range tools {
		out[i] = t.Spec
	}
	return out
}

// selectWorkflow finds the entry workflow.
func selectWorkflow(prog *dsl.Program, name, file string) (*dsl.WorkflowDecl, *dsl.Diagnostic) {
	if name != "" {
		if w := prog.Workflow(name); w != nil {
			return w.Decl, nil
		}
		d := diagnostic(dsl.CodeUnknownName, file, dsl.Span{}, "not found",
			fmt.Sprintf("workflow `%s` not found", name))
		return nil, &d
	}
	switch len(prog.Workflows) {
	case 1:
		return prog.Workflows[0].Decl, nil
	case 0:
		d := diagnostic(dsl.CodeUnknownName, file, dsl.Span{}, "no workflow", "the source declares no workflow")
		d.Help = "Declare a workflow; it becomes the module entry."
		return nil, &d
	}
	d := diagnostic(dsl.CodeUnknownName, file, prog.Workflows[1].Decl.Name.Span(), "second workflow",
		"the source declares several workflows; choose the entry")
	d.Help = "Set Options.Workflow to the name of the workflow to compile."
	return nil, &d
}

// ToolSpecSHA256 derives a registry hash for a tool spec that does not
// carry one: SHA-256 of "name\x00signature\x00effect,effect", with the
// effects in the given order (not sorted).
//
// PORT: the hash lands in the module's TOOLS table and so in the module
// hash; a Rust compiler compiling against the same registry must derive
// it the same way.
func ToolSpecSHA256(s dsl.ToolSpec) [32]byte {
	h := sha256.New()
	h.Write([]byte(s.Name))
	h.Write([]byte{0})
	h.Write([]byte(s.Signature))
	h.Write([]byte{0})
	for i, e := range s.Effects {
		if i > 0 {
			h.Write([]byte{','})
		}
		h.Write([]byte(e))
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// PolicySHA256 is the META program_policy_sha256 of a policy: SHA-256 of
// its canonical JSON ({"known":...,"max_class":...}).
func PolicySHA256(p dsl.Policy) [32]byte {
	b, err := core.CanonicalJSON(map[string]any{"max_class": p.MaxClass.String(), "known": p.Known})
	if err != nil {
		return sha256.Sum256([]byte(fmt.Sprintf("%v", p)))
	}
	return sha256.Sum256(b)
}
