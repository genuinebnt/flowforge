package compiler

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler/internal/abi"
	"github.com/genuinebnt/flowforge/dsl"
)

// compiler holds the state of one Compile call: the module being
// assembled, the function work list and the tables that map source items
// to functions.
type compiler struct {
	prog  *dsl.Program
	wf    *dsl.WorkflowDecl
	file  string
	src   string
	opts  Options
	b     *bytecode.Builder
	types *abi.Types

	registry map[string]Tool // registry tools by name
	tools    bytecode.ToolMap

	closures *ClosureTable
	fnFuncs  map[*dsl.FnDecl]*bytecode.FuncBuilder
	queue    []*job

	mask   bytecode.EffectMask
	ops    bytecode.EffectMask // EFFECT kinds seen, as 1<<kind
	annots bytecode.Annotations
	errs   []dsl.Diagnostic
}

func newCompiler(prog *dsl.Program, wf *dsl.WorkflowDecl, file, src string, opts Options) *compiler {
	c := &compiler{
		prog: prog, wf: wf, file: file, src: src, opts: opts,
		b:        bytecode.NewBuilder(),
		types:    abi.NewTypes(prog),
		registry: map[string]Tool{},
		tools:    bytecode.ToolMap{},
		closures: newClosureTable(),
		fnFuncs:  map[*dsl.FnDecl]*bytecode.FuncBuilder{},
	}
	for _, t := range opts.Tools {
		if _, dup := c.registry[t.Spec.Name]; !dup {
			c.registry[t.Spec.Name] = t
		}
	}
	return c
}

// errorf records a compiler defect at node n.
func (c *compiler) errorf(n dsl.Node, format string, args ...any) {
	var sp dsl.Span
	if n != nil {
		sp = n.Span()
	}
	c.errs = append(c.errs, internalError(c.file, sp, fmt.Sprintf(format, args...)))
}

// limitf records that the program exceeds a module limit at node n.
func (c *compiler) limitf(n dsl.Node, format string, args ...any) {
	var sp dsl.Span
	if n != nil {
		sp = n.Span()
	}
	d := diagnostic(CodeTooLarge, c.file, sp, "too large", fmt.Sprintf(format, args...))
	d.Help = "Split the workflow into smaller functions or reduce literal sizes."
	c.errs = append(c.errs, d)
}

// run compiles the entry workflow and everything it reaches, assembles
// and verifies the module.
func (c *compiler) run() (*Result, []dsl.Diagnostic) {
	c.addTypes()
	if len(c.wf.Params) > 255 {
		c.limitf(c.wf.Name, "workflow `%s` has more than 255 parameters", c.wf.Name.Name)
		return nil, c.errs
	}
	entry := c.b.Func(c.wf.Name.Name, uint8(len(c.wf.Params)), 0)
	c.b.SetEntry(entry.Index())
	c.emitWorkflow(entry)
	if c.opts.AllFns {
		for _, fn := range c.prog.Fns {
			c.fnFunc(fn)
		}
	}
	for len(c.queue) > 0 && len(c.errs) == 0 {
		j := c.queue[0]
		c.queue = c.queue[1:]
		c.emitJob(j)
	}
	if len(c.errs) > 0 {
		return nil, c.errs
	}
	c.addSignature(entry.Index())
	c.mask |= bytecode.EffectMask(c.wf.Func.Inferred)
	c.b.SetEffects(c.mask)
	c.setAnnotations()
	policy := PolicySHA256(c.opts.Policy)
	source := sha256.Sum256([]byte(c.src))
	version := c.opts.CompilerVersion
	if version == "" {
		version = Version
	}
	c.b.SetMeta(bytecode.Meta{CompilerVersion: version, SourceSHA256: source,
		ProgramPolicySHA256: policy, WorkflowName: c.wf.Name.Name})

	mod, err := c.b.Build()
	if err != nil {
		if strings.Contains(err.Error(), "too many") || strings.Contains(err.Error(), "exceeds u16") {
			c.limitf(c.wf.Name, "workflow `%s` exceeds a module limit: %v", c.wf.Name.Name, err)
		} else {
			c.errorf(c.wf.Name, "assembling the module: %v", err)
		}
		return nil, c.errs
	}
	enc, err := mod.Encode()
	if err != nil {
		if errors.Is(err, bytecode.ErrTooLarge) {
			c.limitf(c.wf.Name, "the compiled module exceeds 4 MiB")
		} else {
			c.errorf(c.wf.Name, "encoding the module: %v", err)
		}
		return nil, c.errs
	}
	ver, err := bytecode.Verify(mod, c.tools)
	if err != nil {
		var ve *bytecode.VerifyError
		if errors.As(err, &ve) && errors.Is(err, bytecode.ErrStackOverflow) {
			c.limitf(c.wf.Name, "workflow `%s` needs more than 65,536 stack slots", c.wf.Name.Name)
		} else {
			c.errorf(c.wf.Name, "the bytecode verifier rejected the output: %v", err)
		}
		return nil, c.errs
	}
	var info *dsl.WorkflowInfo
	for _, w := range c.prog.Workflows {
		if w.Decl == c.wf {
			info = w
		}
	}
	res := &Result{
		Module: mod, Encoded: enc, ModuleSHA256: sha256.Sum256(enc), Verified: ver, Tools: c.tools,
		Effects: maskNames(c.mask), Class: c.wf.Func.Inferred.Class(), Ops: c.opNames(),
		SourceSHA256: source, ProgramPolicySHA256: policy, Workflow: c.wf.Name.Name,
		Program: c.prog, Closures: c.closures, Source: c.src, file: c.file,
	}
	if info != nil {
		res.Class = info.Class
	}
	return res, nil
}

// addTypes emits a TYPES entry for every record and enum that has no
// reserved id, in the order abi.Types assigned the ids.
func (c *compiler) addTypes() {
	for _, d := range c.types.Module {
		want, _ := c.types.ID(d)
		var got bytecode.TypeID
		switch d.Kind {
		case dsl.DefRecord:
			if len(d.Fields) > 255 {
				c.limitf(declNode(d), "record `%s` has more than 255 fields", d.Name)
			}
			fs := make([]bytecode.Field, len(d.Fields))
			for i, f := range d.Fields {
				fs[i] = bytecode.Field{Name: f.Name, Type: abi.TypeText(f.Type)}
			}
			got = c.b.Record(d.Name, fs...)
		case dsl.DefEnum:
			if len(d.Variants) > 256 {
				c.limitf(declNode(d), "enum `%s` has more than 256 variants", d.Name)
			}
			vs := make([]bytecode.Variant, len(d.Variants))
			for i, v := range d.Variants {
				if len(v.Payload) > 255 {
					c.limitf(declNode(d), "variant `%s` has more than 255 values", v.QualifiedName())
				}
				ps := make([]string, len(v.Payload))
				for j, p := range v.Payload {
					ps[j] = abi.TypeText(p)
				}
				vs[i] = bytecode.Variant{Name: v.Name, Payload: ps}
			}
			got = c.b.Enum(d.Name, vs...)
		}
		if got != want {
			c.errorf(declNode(d), "type `%s` got id %d, expected %d", d.Name, got, want)
		}
	}
}

func declNode(d *dsl.TypeDef) dsl.Node {
	if d.Decl != nil {
		return d.Decl
	}
	return nil
}

// addSignature records the entry's parameter and result types so the
// engine can decode inputs and encode outputs.
func (c *compiler) addSignature(entry uint16) {
	params := make([]bytecode.Field, len(c.wf.Params))
	for i, p := range c.wf.Params {
		t := ""
		if i < len(c.wf.Func.Params) {
			t = abi.TypeText(c.wf.Func.Params[i])
		}
		params[i] = bytecode.Field{Name: p.Name.Name, Type: t}
	}
	c.b.Signature(entry, params, abi.TypeText(c.wf.Func.Result))
}

// useTool adds a TOOLS entry for t (once) and returns its name.
func (c *compiler) useTool(t *dsl.Tool) string {
	if _, ok := c.tools[t.Name]; ok {
		return t.Name
	}
	hash := c.toolHash(t)
	c.b.Tool(t.Name, hash)
	c.tools[t.Name] = bytecode.ToolInfo{SpecSHA256: hash, Effects: bytecode.EffectMask(t.Effects)}
	return t.Name
}

func (c *compiler) toolHash(t *dsl.Tool) [32]byte {
	if r, ok := c.registry[t.Name]; ok && t.FromRegistry {
		if r.SpecSHA256 != ([32]byte{}) {
			return r.SpecSHA256
		}
		return ToolSpecSHA256(r.Spec)
	}
	return ToolSpecSHA256(sourceToolSpec(t))
}

// sourceToolSpec renders a tool declared in source as a registry spec.
func sourceToolSpec(t *dsl.Tool) dsl.ToolSpec {
	return dsl.ToolSpec{Name: t.Name, Signature: t.Signature(), ParamNames: t.ParamNames, Effects: t.Effects.Names()}
}

// toolIdempotent reports whether calls of t are annotated idempotent.
func (c *compiler) toolIdempotent(t *dsl.Tool) bool {
	if t.Decl != nil && t.Decl.Attrs.Idempotent {
		return true
	}
	r, ok := c.registry[t.Name]
	return ok && t.FromRegistry && r.Idempotent
}

func (c *compiler) opNames() []string {
	var out []string
	for k := bytecode.EffectToolCall; k <= bytecode.EffectSimilarityQuery; k++ {
		if c.ops&(1<<k) != 0 {
			out = append(out, k.String())
		}
	}
	return out
}

// maskNames lists the names of the bits of m in mask order.
func maskNames(m bytecode.EffectMask) []string {
	out := []string{}
	for i := 0; i < 12; i++ {
		bit := bytecode.EffectMask(1) << i
		if m&bit == 0 {
			continue
		}
		s := bit.String()
		out = append(out, strings.Trim(s, "{}"))
	}
	return out
}
