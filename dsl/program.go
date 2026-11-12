package dsl

import (
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

// registryFile is the file name used for diagnostics in tool
// declarations injected through Options.
const registryFile = "<registry>"

// Policy is the program policy the checker enforces at compile time
// (spec 7.5). With Known false the checker checks only internal
// consistency and skips E0402 and E0403.
type Policy struct {
	// MaxClass is the program's max_class.
	MaxClass core.ActivityClass `json:"max_class"`
	// Known reports that the program's policy is available.
	Known bool `json:"known"`
}

// ToolSpec is a tool declaration generated from the tool registry
// (spec 6.2, 16.2).
type ToolSpec struct {
	Name string `json:"name"`
	// Signature is a function type, e.g. "(InScope<Host>) -> Option<HttpService>".
	Signature string `json:"signature"`
	// ParamNames names the parameters; missing names default to arg0, arg1, ...
	ParamNames []string `json:"param_names,omitempty"`
	// Effects lists effect names, e.g. ["active"].
	Effects []string `json:"effects"`
}

// Options configures Check and Analyze.
type Options struct {
	Policy Policy
	// Tools are registry tool declarations in structured form.
	Tools []ToolSpec
	// ToolSource holds registry tool declarations as source text
	// (`tool name(p: T) -> R ! e` items). Types they mention resolve
	// against the program's types and the prelude.
	ToolSource string
	// FileName names the source in diagnostics (Analyze only). When empty,
	// Analyze uses "<first workflow>.ff", or "main.ff".
	FileName string
}

// WorkflowInfo summarizes a checked workflow.
type WorkflowInfo struct {
	Decl *WorkflowDecl
	// Effects is the inferred effect set (spec 7.5).
	Effects EffectSet
	// Class is the inferred activity class.
	Class core.ActivityClass
	// Ops are the runtime effect kinds the workflow may perform.
	Ops OpSet
	// Tools are the tools the workflow may call, in first-use order.
	Tools []*Tool
}

// Program is the result of checking a file: the annotated syntax tree
// plus tables a compiler needs. It is only meaningful when the
// accompanying diagnostics contain no errors.
type Program struct {
	File *File
	// Registry holds the tool declarations injected through Options.
	Registry *File
	// Types lists every TypeDef in ID order (builtins, then user types).
	Types []*TypeDef
	// Tools lists every declared tool (registry first, then source).
	Tools []*Tool
	// Fns and Workflows list the declarations in source order.
	Fns       []*FnDecl
	Workflows []*WorkflowInfo
	// Effects and Class are the union over all workflows.
	Effects EffectSet
	Class   core.ActivityClass
	// NumBindings is one more than the largest Binding.ID.
	NumBindings int
}

// Workflow returns the named workflow or nil.
func (p *Program) Workflow(name string) *WorkflowInfo {
	for _, w := range p.Workflows {
		if w.Decl.Name.Name == name {
			return w
		}
	}
	return nil
}

// Analysis is the CheckWorkflowResponse-shaped result of Analyze
// (spec 11.4, 12.6).
type Analysis struct {
	Diagnostics     []Diagnostic       `json:"diagnostics"`
	InferredEffects []string           `json:"inferred_effects"`
	InferredClass   core.ActivityClass `json:"inferred_class"`
	OK              bool               `json:"ok"`
}

// Analyze parses and checks src and summarizes the result. ok is false
// when any error was reported.
func Analyze(src string, opts Options) Analysis {
	name := opts.FileName
	placeholder := name == ""
	if placeholder {
		name = "\x00source"
	}
	f, diags := Parse(name, []byte(src))
	prog, cdiags := Check(f, opts)
	diags = append(diags, cdiags...)
	SortDiagnostics(diags)
	if placeholder {
		real := "main.ff"
		for _, it := range f.Items {
			if w, ok := it.(*WorkflowDecl); ok {
				real = w.Name.Name + ".ff"
				break
			}
		}
		for i := range diags {
			if diags[i].Primary.File == name {
				diags[i].Primary.File = real
			}
			for j := range diags[i].Secondary {
				if diags[i].Secondary[j].File == name {
					diags[i].Secondary[j].File = real
				}
			}
		}
	}
	if diags == nil {
		diags = []Diagnostic{}
	}
	return Analysis{
		Diagnostics:     diags,
		InferredEffects: prog.Effects.Names(),
		InferredClass:   prog.Class,
		OK:              !HasErrors(diags),
	}
}

// Check resolves names, infers and checks types and effects, checks scope
// typing, annotations and (when opts.Policy.Known) the program policy.
// It annotates f in place (see the "set by Check" fields of the syntax
// tree), so a File must not be checked concurrently or twice. Check
// never panics; diagnostics are returned in source order.
func Check(f *File, opts Options) (*Program, []Diagnostic) {
	c := newChecker(f, opts)
	prog := c.run()
	diags := append([]Diagnostic{}, c.mainSink.diags...)
	diags = append(diags, c.regSink.diags...)
	SortDiagnostics(diags)
	return prog, diags
}

// checker holds the state of one Check call.
type checker struct {
	file     *File
	opts     Options
	mainSink *diagSink
	regSink  *diagSink
	sink     *diagSink // the sink for the item being checked

	types     map[string]*TypeDef
	typeDecls map[string]itemSym
	values    map[string]itemSym
	userTypes []*TypeDef
	tools     []*Tool
	// shadowedTools pairs a registry tool with a source redeclaration.
	shadowedTools [][2]*Tool

	typeParams map[string]*TypeParam
	scope      *scope
	fn         *fnCtx

	nextVar     int
	nextParam   int
	nextBinding int
	// opEvents counts runtime operations recorded so far; comparing it
	// before and after checking an expression tells whether the
	// expression performs any.
	opEvents int
	// curSpan is the callee span of the call being checked (for
	// deprecation warnings on tools).
	curSpan *Span
	// deferred checks run when the current item's body is checked.
	deferred []func()

	// fnCalls records the user fns each item calls, for summaries.
	fnCalls map[*FuncInfo][]*FnDecl
	// toolUses records the tools each item uses directly.
	toolUses map[*FuncInfo][]*Tool
	// directOps records the runtime ops each item performs directly.
	directOps map[*FuncInfo]OpSet
}

func newChecker(f *File, opts Options) *checker {
	c := &checker{
		file:      f,
		opts:      opts,
		mainSink:  newDiagSink(f.Name),
		regSink:   newDiagSink(registryFile),
		types:     map[string]*TypeDef{},
		typeDecls: map[string]itemSym{},
		values:    map[string]itemSym{},
		fnCalls:   map[*FuncInfo][]*FnDecl{},
		toolUses:  map[*FuncInfo][]*Tool{},
		directOps: map[*FuncInfo]OpSet{},
	}
	c.sink = c.mainSink
	return c
}

func (c *checker) sinkFor(file string) *diagSink {
	if file == registryFile {
		return c.regSink
	}
	return c.mainSink
}

// registry builds the injected tool declarations.
func (c *checker) registry() *File {
	reg := &File{Name: registryFile}
	if strings.TrimSpace(c.opts.ToolSource) != "" {
		rf, diags := Parse(registryFile, []byte(c.opts.ToolSource))
		c.regSink.diags = append(c.regSink.diags, diags...)
		for _, it := range rf.Items {
			if _, ok := it.(*ToolDecl); ok {
				reg.Items = append(reg.Items, it)
			} else {
				c.regSink.errorf(CodeUnexpectedToken, it.Span(), "not a tool declaration",
					"the tool registry may only contain tool declarations")
			}
		}
	}
	for _, ts := range c.opts.Tools {
		if d := c.toolFromSpec(ts); d != nil {
			reg.Items = append(reg.Items, d)
		}
	}
	return reg
}

// toolFromSpec converts a structured registry entry into a ToolDecl.
func (c *checker) toolFromSpec(ts ToolSpec) *ToolDecl {
	toks, diags := Lex(registryFile, []byte(ts.Signature))
	sink := newDiagSink(registryFile)
	sink.diags = append(sink.diags, diags...)
	p := &parser{toks: toks, sink: sink, file: registryFile}
	tx := p.parseType()
	ft, ok := tx.(*FuncTypeExpr)
	if !ok || len(sink.diags) > 0 || !p.at(TokEOF) || !IsIdentName(ts.Name) {
		c.regSink.diags = append(c.regSink.diags, sink.diags...)
		c.regSink.errorf(CodeUnexpectedToken, Span{}, "invalid tool specification",
			"registry tool %q has an invalid signature %q", ts.Name, ts.Signature).
			withHelp("Signatures look like (Fqdn) -> Option<IpAddress>.")
		return nil
	}
	d := &ToolDecl{Name: &Ident{Name: ts.Name}, Result: ft.Result}
	for i, pt := range ft.Params {
		name := "arg" + itoa(i)
		if i < len(ts.ParamNames) && IsIdentName(ts.ParamNames[i]) {
			name = ts.ParamNames[i]
		}
		d.Params = append(d.Params, &Param{Name: &Ident{Name: name}, TypeX: pt})
	}
	ec := &EffectClause{}
	for _, e := range ts.Effects {
		ec.Names = append(ec.Names, &Ident{Name: e})
	}
	d.Effects = ec
	return d
}

// IsIdentName reports whether s is a valid identifier and not a keyword.
func IsIdentName(s string) bool {
	if s == "" || !isIdentStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isIdentChar(s[i]) {
			return false
		}
	}
	return !IsKeyword(s)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// run performs all checking phases.
func (c *checker) run() *Program {
	reg := c.registry()
	prog := &Program{File: c.file, Registry: reg}

	// Phase 1: names.
	c.sink = c.regSink
	c.collect(reg.Items, registryFile)
	c.sink = c.mainSink
	c.collect(c.file.Items, c.file.Name)

	// Phase 2: type bodies, signatures, uses.
	c.resolveTypeBodies(c.file.Items)
	c.resolveSignatures(reg.Items, registryFile)
	c.resolveSignatures(c.file.Items, c.file.Name)
	c.checkRegistryAgreement()
	c.resolveUses(c.file.Items)

	// Phase 3: annotations on items.
	for _, it := range c.file.Items {
		c.checkItemAnnotations(it)
	}

	// Phase 4: bodies.
	for _, it := range c.file.Items {
		switch d := it.(type) {
		case *FnDecl:
			c.checkBody(d.Params, d.Body, d.Func, false, d.Generics)
			prog.Fns = append(prog.Fns, d)
		case *WorkflowDecl:
			c.checkBody(d.Params, d.Body, d.Func, true, nil)
		}
	}

	// Phase 5: effects, summaries, policy.
	c.summarize()
	for _, it := range c.file.Items {
		switch d := it.(type) {
		case *FnDecl:
			c.checkDeclaredEffects(d.Name, d.Effects, d.Func, d.Body, "function")
		case *WorkflowDecl:
			c.checkDeclaredEffects(d.Name, d.Effects, d.Func, d.Body, "workflow")
			w := &WorkflowInfo{Decl: d, Effects: d.Func.Inferred, Class: d.Func.Inferred.Class(),
				Ops: d.Func.Ops, Tools: d.Func.Tools}
			prog.Workflows = append(prog.Workflows, w)
			prog.Effects |= w.Effects
		}
	}
	if c.opts.Policy.Known {
		c.checkPolicy()
	}
	prog.Class = prog.Effects.Class()
	prog.Types = append(append([]*TypeDef{}, builtinTypeDefs...), c.userTypes...)
	prog.Tools = c.tools
	prog.NumBindings = c.nextBinding
	return prog
}
