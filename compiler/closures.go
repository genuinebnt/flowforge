package compiler

import (
	"fmt"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler/internal/abi"
	"github.com/genuinebnt/flowforge/dsl"
)

// ClosureTable records which module function implements each lambda,
// compensation, fn and item wrapper the compiled code turns into a
// closure value. It implements the tree-walker's closure index so that
// both evaluators build identical closure values.
type ClosureTable struct {
	lambdas  map[*dsl.Lambda]uint16
	fns      map[*dsl.FnDecl]uint16
	tools    map[*dsl.Tool]uint16
	builtins map[*dsl.Builtin]uint16
	variants map[*dsl.Variant]uint16
}

func newClosureTable() *ClosureTable {
	return &ClosureTable{
		lambdas: map[*dsl.Lambda]uint16{}, fns: map[*dsl.FnDecl]uint16{}, tools: map[*dsl.Tool]uint16{},
		builtins: map[*dsl.Builtin]uint16{}, variants: map[*dsl.Variant]uint16{},
	}
}

// LambdaFunc returns the function of a lambda or compensation that was
// compiled as a function (lambdas inlined into loops have none).
func (t *ClosureTable) LambdaFunc(l *dsl.Lambda) (uint16, bool) { i, ok := t.lambdas[l]; return i, ok }

// FnFunc returns the function of a user fn.
func (t *ClosureTable) FnFunc(fn *dsl.FnDecl) (uint16, bool) { i, ok := t.fns[fn]; return i, ok }

// ToolFunc returns the wrapper function of a tool used as a value.
func (t *ClosureTable) ToolFunc(tl *dsl.Tool) (uint16, bool) { i, ok := t.tools[tl]; return i, ok }

// BuiltinFunc returns the wrapper function of a library function used as
// a value.
func (t *ClosureTable) BuiltinFunc(b *dsl.Builtin) (uint16, bool) {
	i, ok := t.builtins[b]
	return i, ok
}

// VariantFunc returns the wrapper function of a variant constructor used
// as a value.
func (t *ClosureTable) VariantFunc(v *dsl.Variant) (uint16, bool) {
	i, ok := t.variants[v]
	return i, ok
}

var _ abi.Closures = (*ClosureTable)(nil)

// jobKind says what a queued function body implements.
type jobKind int

const (
	jobFn jobKind = iota
	jobLambda
	jobCompensation
	jobToolWrapper
	jobBuiltinWrapper
	jobVariantWrapper
)

// job is a function declared (its index is fixed) whose body is emitted
// later from the work list.
type job struct {
	kind    jobKind
	fb      *bytecode.FuncBuilder
	fn      *dsl.FnDecl
	lambda  *dsl.Lambda
	tool    *dsl.Tool
	builtin *dsl.Builtin
	variant *dsl.Variant
	// annot is the let-statement annotation in force where a lambda was
	// written; its effects inherit it.
	annot *letAnnot
	// idempotent is inherited from an enclosing #[idempotent] fn.
	idempotent bool
}

func (c *compiler) enqueue(j *job) { c.queue = append(c.queue, j) }

// fnFunc returns (declaring on first use) the function of a user fn.
func (c *compiler) fnFunc(fn *dsl.FnDecl) (uint16, bool) {
	if fb, ok := c.fnFuncs[fn]; ok {
		return fb.Index(), true
	}
	if fn.Func == nil || len(fn.Params) > 255 {
		c.limitf(fn.Name, "fn `%s` has more than 255 parameters", fn.Name.Name)
		return 0, false
	}
	fb := c.b.Func(fn.Name.Name, uint8(len(fn.Params)), 0)
	c.fnFuncs[fn] = fb
	c.closures.fns[fn] = fb.Index()
	c.enqueue(&job{kind: jobFn, fb: fb, fn: fn, idempotent: fn.Attrs.Idempotent})
	return fb.Index(), true
}

// lambdaFunc declares the function of a lambda value created in x.
func (c *compiler) lambdaFunc(x *fnCtx, l *dsl.Lambda) (uint16, bool) {
	if i, ok := c.closures.lambdas[l]; ok {
		return i, true
	}
	ncaps := len(l.Captures)
	arity := len(l.Params)
	kind := jobLambda
	name := fmt.Sprintf("lambda@%d", l.Span().Start.Offset)
	if l.Synthetic {
		kind, ncaps, arity = jobCompensation, ncaps+1, 0
		name = fmt.Sprintf("compensate@%d", l.Span().Start.Offset)
	}
	if ncaps > 255 || arity > 255 {
		c.limitf(l, "lambda captures or takes more than 255 values")
		return 0, false
	}
	fb := c.b.Func(name, uint8(arity), uint8(ncaps))
	c.closures.lambdas[l] = fb.Index()
	c.enqueue(&job{kind: kind, fb: fb, lambda: l, annot: x.annot, idempotent: x.idempotent})
	return fb.Index(), true
}

// toolFunc declares the wrapper of a tool used as a value.
func (c *compiler) toolFunc(t *dsl.Tool, n dsl.Node) (uint16, bool) {
	if i, ok := c.closures.tools[t]; ok {
		return i, true
	}
	if len(t.Params) > 254 {
		c.limitf(n, "tool `%s` has too many parameters", t.Name)
		return 0, false
	}
	fb := c.b.Func("tool:"+t.Name, uint8(len(t.Params)), 0)
	c.closures.tools[t] = fb.Index()
	c.enqueue(&job{kind: jobToolWrapper, fb: fb, tool: t})
	return fb.Index(), true
}

// builtinFunc declares the wrapper of a library function used as a value.
func (c *compiler) builtinFunc(b *dsl.Builtin, n dsl.Node) (uint16, bool) {
	if i, ok := c.closures.builtins[b]; ok {
		return i, true
	}
	if len(b.Overloads) != 1 {
		c.errorf(n, "builtin %s used as a value has %d overloads", b.Name, len(b.Overloads))
		return 0, false
	}
	fb := c.b.Func("builtin:"+b.Name, uint8(len(b.Overloads[0].Params)), 0)
	c.closures.builtins[b] = fb.Index()
	c.enqueue(&job{kind: jobBuiltinWrapper, fb: fb, builtin: b})
	return fb.Index(), true
}

// variantFunc declares the wrapper of a variant constructor used as a value.
func (c *compiler) variantFunc(v *dsl.Variant, n dsl.Node) (uint16, bool) {
	if i, ok := c.closures.variants[v]; ok {
		return i, true
	}
	if len(v.Payload) > 255 {
		c.limitf(n, "variant `%s` has too many values", v.QualifiedName())
		return 0, false
	}
	fb := c.b.Func("variant:"+v.QualifiedName(), uint8(len(v.Payload)), 0)
	c.closures.variants[v] = fb.Index()
	c.enqueue(&job{kind: jobVariantWrapper, fb: fb, variant: v})
	return fb.Index(), true
}

// emitWorkflow emits the entry function: parameters are locals 0..n-1.
func (c *compiler) emitWorkflow(fb *bytecode.FuncBuilder) {
	x := newFnCtx(c, fb, true)
	x.idempotent = c.wf.Attrs.Idempotent
	for i, p := range c.wf.Params {
		if p.Binding != nil {
			x.locals[p.Binding] = uint16(i)
		}
	}
	x.block(c.wf.Body)
	x.fb.Op(bytecode.OpHalt)
}

// emitJob emits a queued function body.
func (c *compiler) emitJob(j *job) {
	x := newFnCtx(c, j.fb, false)
	x.annot, x.idempotent = j.annot, j.idempotent
	switch j.kind {
	case jobFn:
		for i, p := range j.fn.Params {
			if p.Binding != nil {
				x.locals[p.Binding] = uint16(i)
			}
		}
		x.block(j.fn.Body)
		x.fb.Op(bytecode.OpRet)
	case jobLambda:
		for i, p := range j.lambda.Params {
			if p.Binding != nil {
				x.locals[p.Binding] = uint16(i)
			}
		}
		for i, cp := range j.lambda.Captures {
			x.caps[cp.Binding] = uint8(i)
		}
		x.expr(j.lambda.Body)
		x.fb.Op(bytecode.OpRet)
	case jobCompensation:
		if len(j.lambda.Params) == 1 && j.lambda.Params[0].Binding != nil {
			x.caps[j.lambda.Params[0].Binding] = 0
		}
		for i, cp := range j.lambda.Captures {
			x.caps[cp.Binding] = uint8(i + 1)
		}
		x.expr(j.lambda.Body)
		x.fb.Op(bytecode.OpRet)
	case jobToolWrapper:
		for i := range j.tool.Params {
			x.fb.Load(uint16(i))
		}
		x.toolCall(j.tool, len(j.tool.Params), nil)
		x.fb.Op(bytecode.OpRet)
	case jobBuiltinWrapper:
		n := len(j.builtin.Overloads[0].Params)
		for i := 0; i < n; i++ {
			x.fb.Load(uint16(i))
		}
		x.builtinOnStack(j.builtin, n, nil)
		x.fb.Op(bytecode.OpRet)
	case jobVariantWrapper:
		for i := range j.variant.Payload {
			x.fb.Load(uint16(i))
		}
		x.makeVariant(j.variant, nil)
		x.fb.Op(bytecode.OpRet)
	}
}

// pushClosure emits code that leaves a closure value for l (a lambda or
// a compensation) on the stack: its captures, then CLOSURE.
func (x *fnCtx) pushClosure(l *dsl.Lambda) {
	fn, ok := x.c.lambdaFunc(x, l)
	if !ok {
		x.fb.Op(bytecode.OpUnit)
		return
	}
	for _, cp := range l.Captures {
		x.loadBinding(cp.Binding, l)
	}
	x.fb.Closure(fn, uint8(len(l.Captures)))
}

// itemValue emits the value of a fn, tool, library function or variant
// constructor used as a value.
func (x *fnCtx) itemValue(ref dsl.Ref, n dsl.Node) {
	var fn uint16
	ok := false
	switch ref.Kind {
	case dsl.RefFn:
		fn, ok = x.c.fnFunc(ref.Fn)
	case dsl.RefTool:
		fn, ok = x.c.toolFunc(ref.Tool, n)
	case dsl.RefBuiltin:
		fn, ok = x.c.builtinFunc(ref.Builtin, n)
	case dsl.RefVariant:
		if len(ref.Variant.Payload) == 0 {
			x.makeVariant(ref.Variant, n)
			return
		}
		fn, ok = x.c.variantFunc(ref.Variant, n)
	default:
		x.c.errorf(n, "reference of kind %d is not a value", ref.Kind)
	}
	if !ok {
		x.fb.Op(bytecode.OpUnit)
		return
	}
	x.fb.Closure(fn, 0)
}
