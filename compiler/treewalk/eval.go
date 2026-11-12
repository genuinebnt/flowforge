package treewalk

import (
	"errors"
	"fmt"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler/internal/abi"
	"github.com/genuinebnt/flowforge/dsl"
	"github.com/genuinebnt/flowforge/vm"
)

// ClosureIndex maps source functions to the function indices of a
// compiled module (compiler.ClosureTable implements it). With it the
// evaluator builds closure values identical to the VM's, so effect
// arguments that contain closures (par_map) hash the same. Without it
// closures get private indices.
type ClosureIndex interface {
	LambdaFunc(l *dsl.Lambda) (uint16, bool)
	FnFunc(fn *dsl.FnDecl) (uint16, bool)
	ToolFunc(t *dsl.Tool) (uint16, bool)
	BuiltinFunc(b *dsl.Builtin) (uint16, bool)
	VariantFunc(v *dsl.Variant) (uint16, bool)
}

// Config configures an evaluation.
type Config struct {
	// VM supplies the limits (string, list, effects, call depth) and the
	// scope snapshot behind scope.allows, exactly as for the VM.
	VM vm.Config
	// Module supplies type names to to_string (optional).
	Module *bytecode.Module
	// Closures maps lambdas and items to module functions (optional).
	Closures ClosureIndex
	// ParMapOrder, when set, gives the order in which the tasks of a
	// par_map are run (a permutation of 0..n-1); results are joined in
	// input order regardless. Nil runs them in input order.
	ParMapOrder func(path vm.Path, n int) []int
}

// ErrSetup reports a misuse of the evaluator (unknown workflow, wrong
// number of inputs); program failures are vm.Failed outcomes.
var ErrSetup = errors.New("treewalk: invalid evaluation request")

// Program is a checked program prepared for evaluation.
type Program struct {
	prog  *dsl.Program
	types *abi.Types
	cfg   Config
	vmcfg vm.Config
	// closures maps closure function indices to their source; private
	// indices are handed out from 0xFFFF downwards.
	closures map[uint16]closureInfo
	lambdaID map[*dsl.Lambda]uint16
	itemID   map[any]uint16
	nextID   uint16
}

// New prepares prog (which must have checked without errors).
func New(prog *dsl.Program, cfg Config) (*Program, error) {
	if prog == nil {
		return nil, fmt.Errorf("%w: nil program", ErrSetup)
	}
	limits := cfg.VM
	if limits.Limits.String == 0 {
		limits.Limits.String = vm.DefaultString
	}
	if limits.Limits.List == 0 {
		limits.Limits.List = vm.DefaultList
	}
	if limits.Limits.Effects == 0 {
		limits.Limits.Effects = vm.DefaultEffects
	}
	if limits.Limits.CallDepth == 0 {
		limits.Limits.CallDepth = vm.DefaultCallDepth
	}
	return &Program{
		prog: prog, types: abi.NewTypes(prog), cfg: cfg, vmcfg: limits,
		closures: map[uint16]closureInfo{}, lambdaID: map[*dsl.Lambda]uint16{},
		itemID: map[any]uint16{}, nextID: 0xFFFF,
	}, nil
}

// Run evaluates the named workflow with args, asking h for every effect
// except par_map, whose tasks it evaluates itself (as vm.RunWithEffects
// does). It returns the final outcome (vm.Completed or vm.Failed) and the
// effect trace in scheduling order.
func Run(prog *dsl.Program, workflow string, args []vm.Value, cfg Config, h vm.Handler) (vm.Outcome, []vm.TraceEntry, error) {
	p, err := New(prog, cfg)
	if err != nil {
		return nil, nil, err
	}
	ev, err := p.Start(workflow, args)
	if err != nil {
		return nil, nil, err
	}
	var trace []vm.TraceEntry
	out := ev.Drive(h, &trace)
	return out, trace, nil
}

// Eval is one evaluation: a workflow run, a par_map task or a
// compensation. It mirrors the VM's per-run state: the effect counter and
// path prefix, the compensation stack and the call depth.
type Eval struct {
	p      *Program
	prefix vm.Path
	seq    uint64
	comps  []vm.Value
	depth  int
	run    func(ev *Eval) (vm.Value, error)
	h      vm.Handler
	trace  *[]vm.TraceEntry
	done   bool
	out    vm.Outcome
}

// Start prepares the run of workflow with args.
func (p *Program) Start(workflow string, args []vm.Value) (*Eval, error) {
	w := p.prog.Workflow(workflow)
	if w == nil {
		return nil, fmt.Errorf("%w: no workflow %q", ErrSetup, workflow)
	}
	d := w.Decl
	if len(args) != len(d.Params) {
		return nil, fmt.Errorf("%w: workflow %s takes %d inputs, got %d", ErrSetup, workflow, len(d.Params), len(args))
	}
	ev := &Eval{p: p, depth: 1}
	ev.run = func(ev *Eval) (vm.Value, error) {
		env := newEnv()
		for i, prm := range d.Params {
			env.set(prm.Binding, args[i])
		}
		v, err := ev.block(d.Body, env)
		var r *returnSignal
		if errors.As(err, &r) {
			return r.v, nil
		}
		return v, err
	}
	return ev, nil
}

// child prepares a par_map task or compensation: a call of closure c.
func (p *Program) child(c vm.Value, args []vm.Value, prefix vm.Path) *Eval {
	ev := &Eval{p: p, prefix: append(vm.Path(nil), prefix...), depth: 0}
	ev.run = func(ev *Eval) (vm.Value, error) { return ev.callValue(c, args, nil) }
	return ev
}

// NewChild prepares the evaluation of closure c (a compensation, which
// takes no arguments, or a par_map function) with effect paths under
// prefix, as vm.NewChild does.
func (p *Program) NewChild(c vm.Value, args []vm.Value, prefix vm.Path) *Eval {
	return p.child(c, args, prefix)
}

// Drive evaluates to completion, asking h for effects and appending every
// effect to trace. It returns vm.Completed or vm.Failed.
func (ev *Eval) Drive(h vm.Handler, trace *[]vm.TraceEntry) vm.Outcome {
	if ev.done {
		return ev.out
	}
	ev.h, ev.trace = h, trace
	v, err := ev.run(ev)
	ev.done = true
	if err != nil {
		var f *failSignal
		if errors.As(err, &f) {
			ev.out = vm.Failed{Code: f.code, Message: f.msg, Span: f.span}
		} else {
			ev.out = vm.Failed{Code: bytecode.FailTypeMismatch, Message: err.Error(), Span: vm.Span{Func: -1, CodeOffset: -1}}
		}
		return ev.out
	}
	ev.out = vm.Completed{Value: v}
	return ev.out
}

// Compensations returns the registered compensations in run order (most
// recent first), as vm.VM.Compensations does.
func (ev *Eval) Compensations() []vm.Value {
	out := make([]vm.Value, len(ev.comps))
	for i, c := range ev.comps {
		out[len(ev.comps)-1-i] = c
	}
	return out
}

// NextSeq returns the sequence number the next effect would get.
func (ev *Eval) NextSeq() uint64 { return ev.seq }

// Program returns the program the evaluation belongs to.
func (ev *Eval) Program() *Program { return ev.p }

// ---- control signals ----

// failSignal fails the evaluation with a VM failure code.
type failSignal struct {
	code bytecode.FailCode
	msg  string
	span vm.Span
}

func (f *failSignal) Error() string { return f.code.String() + ": " + f.msg }

// returnSignal returns from the enclosing fn or workflow body.
type returnSignal struct{ v vm.Value }

func (r *returnSignal) Error() string { return "return" }

func fail(code bytecode.FailCode, msg string, n dsl.Node) error {
	sp := vm.Span{Func: -1, CodeOffset: -1}
	if n != nil {
		s := n.Span()
		if s.Start.Offset >= 0 && s.End.Offset >= s.Start.Offset {
			sp.SourceOffset, sp.Len, sp.HasSource = uint32(s.Start.Offset), uint32(s.End.Offset-s.Start.Offset), true
		}
	}
	return &failSignal{code: code, msg: msg, span: sp}
}

// bug reports an evaluator defect (a tree the checker should not accept).
func bug(n dsl.Node, format string, args ...any) error {
	return fail(bytecode.FailTypeMismatch, "treewalk: "+fmt.Sprintf(format, args...), n)
}

// ---- environments ----

// env holds the bindings visible in one fn, workflow or closure call.
// Bindings are immutable and unique, so one flat map per call suffices.
type env struct{ vars map[*dsl.Binding]vm.Value }

func newEnv() *env { return &env{vars: map[*dsl.Binding]vm.Value{}} }

func (e *env) set(b *dsl.Binding, v vm.Value) {
	if b != nil {
		e.vars[b] = v
	}
}

func (e *env) get(b *dsl.Binding, n dsl.Node) (vm.Value, error) {
	v, ok := e.vars[b]
	if !ok || b == nil {
		return vm.Value{}, bug(n, "unbound variable")
	}
	return v, nil
}

// ---- effects ----

// effect performs one effect: it numbers it, records it in the trace and
// asks the handler (par_map is evaluated here).
func (ev *Eval) effect(kind bytecode.EffectKind, args []vm.Value, n dsl.Node) (vm.Value, error) {
	if int(ev.seq) >= ev.p.vmcfg.Limits.Effects {
		return vm.Value{}, fail(bytecode.FailLimitExceeded, "effect limit exceeded", n)
	}
	seq := ev.seq
	ev.seq++
	s := vm.Suspended{Path: ev.prefix.Child(uint32(seq)), Seq: seq, Kind: kind, Args: args}
	hash, err := vm.ArgsHash(args)
	if err != nil {
		return vm.Value{}, bug(n, "effect arguments: %v", err)
	}
	idx := len(*ev.trace)
	*ev.trace = append(*ev.trace, vm.TraceEntry{Path: s.Path.String(), Kind: kind, ArgsHash: hash})
	var res vm.EffectResult
	if kind == bytecode.EffectParMap {
		res = ev.parMap(s)
	} else {
		res = ev.h(s)
	}
	(*ev.trace)[idx].Result = res
	if res.Failed {
		code := res.Code
		if !code.Valid() {
			code = bytecode.FailEffectFailed
		}
		msg := res.Message
		if msg == "" {
			msg = code.String()
		}
		return vm.Value{}, fail(code, msg, n)
	}
	return res.Value, nil
}

// parMap runs one child evaluation per element and joins the outcomes in
// input order (spec 8.6), with fail-fast cancellation like
// vm.RunWithEffects.
func (ev *Eval) parMap(s vm.Suspended) vm.EffectResult {
	pm, err := vm.ParMapArgs(s)
	if err != nil {
		return vm.FailResult(bytecode.FailTypeMismatch, err.Error())
	}
	order := make([]int, len(pm.Items))
	for i := range order {
		order[i] = i
	}
	if f := ev.p.cfg.ParMapOrder; f != nil {
		if o := f(s.Path, len(pm.Items)); validPermutation(o, len(pm.Items)) {
			order = o
		}
	}
	outs := make([]vm.Outcome, len(pm.Items))
	for _, i := range order {
		child := ev.p.child(pm.Fn, []vm.Value{pm.Items[i]}, pm.ChildPath(s.Path, i))
		o := child.Drive(ev.h, ev.trace)
		outs[i] = o
		if _, failed := o.(vm.Failed); failed && !pm.Collect {
			break
		}
	}
	for i, o := range outs {
		if o == nil {
			outs[i] = vm.Failed{Code: bytecode.FailCancelled, Message: "cancelled"}
		}
	}
	return pm.Join(outs)
}

func validPermutation(o []int, n int) bool {
	if len(o) != n {
		return false
	}
	seen := make([]bool, n)
	for _, i := range o {
		if i < 0 || i >= n || seen[i] {
			return false
		}
		seen[i] = true
	}
	return true
}
