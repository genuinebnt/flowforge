package compiler_test

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/compiler/treewalk"
	"github.com/genuinebnt/flowforge/dsl"
	"github.com/genuinebnt/flowforge/vm"
)

// script is effects.json of a conformance/vm case: the run's inputs,
// registry, limits, and the scripted effect results.
type script struct {
	Workflow     string            `json:"workflow,omitempty"`
	Inputs       json.RawMessage   `json:"inputs,omitempty"`
	Tools        []dsl.ToolSpec    `json:"tools,omitempty"`
	ScopeAllows  []string          `json:"scope_allows,omitempty"`
	FuelSlice    int64             `json:"fuel_slice,omitempty"`
	FuelLimit    int64             `json:"fuel_limit,omitempty"`
	ParMapOrder  map[string][]int  `json:"par_map_order,omitempty"`
	NoCompensate bool              `json:"no_compensate,omitempty"`
	Results      map[string]result `json:"results,omitempty"`
	ByTool       map[string]result `json:"by_tool,omitempty"`
	ByKind       map[string]result `json:"by_kind,omitempty"`
	// Tamper names a trace path whose recorded args_hash a replay case
	// corrupts (conformance/vm/replay).
	Tamper string `json:"tamper,omitempty"`
}

// result is one scripted effect result:
//
//	{"type": "T", "value": json}        a value decoded with vm.FromJSON
//	{"scope": "allow", "decision": id}  Ok(InScope(target))
//	{"scope": "deny", "reason": r, "decision": id}  Err(ScopeDenied)
//	{"approval": "approve", "by": who}  Ok(Approval)
//	{"approval": "deny", "by": who, "reason": r}  Err(Denied)
//	{"fail": code, "message": m}        an effect failure
type result struct {
	Type     string          `json:"type,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
	Scope    string          `json:"scope,omitempty"`
	Approval string          `json:"approval,omitempty"`
	Decision string          `json:"decision,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	By       string          `json:"by,omitempty"`
	Fail     string          `json:"fail,omitempty"`
	Message  string          `json:"message,omitempty"`
}

// scopeSet is a vm.ScopeSnapshot allowing targets by display text.
type scopeSet map[string]bool

func (s scopeSet) Allows(v vm.Value) bool { return s[vm.Display(v)] }

func (sc *script) vmConfig() vm.Config {
	cfg := vm.Config{FuelLimit: sc.FuelLimit}
	if len(sc.ScopeAllows) > 0 {
		set := scopeSet{}
		for _, s := range sc.ScopeAllows {
			set[s] = true
		}
		cfg.Scope = set
	}
	return cfg
}

func (sc *script) registry() []compiler.Tool {
	out := make([]compiler.Tool, len(sc.Tools))
	for i, t := range sc.Tools {
		out[i] = compiler.Tool{Spec: t}
	}
	return out
}

// handler answers effects from the script: by path, then tool, then kind.
// sleep, log and emit_alert default to (); anything else unscripted fails
// with effect_failed.
func (sc *script) handler(m *bytecode.Module) vm.Handler {
	return func(s vm.Suspended) vm.EffectResult {
		r, ok := sc.Results[s.Path.String()]
		if !ok && s.Kind == bytecode.EffectToolCall {
			r, ok = sc.ByTool[s.Tool()]
		}
		if !ok {
			r, ok = sc.ByKind[s.Kind.String()]
		}
		if !ok {
			switch s.Kind {
			case bytecode.EffectSleep, bytecode.EffectLog, bytecode.EffectEmitAlert:
				return vm.OkResult(vm.Unit())
			}
			return vm.FailResult(bytecode.FailEffectFailed, "no scripted result for effect "+s.Path.String())
		}
		return r.effectResult(m, s)
	}
}

func (r result) effectResult(m *bytecode.Module, s vm.Suspended) vm.EffectResult {
	if r.Fail != "" {
		code, ok := bytecode.ParseFailCode(r.Fail)
		if !ok {
			code = bytecode.FailEffectFailed
		}
		return vm.FailResult(code, r.Message)
	}
	var target vm.Value
	if len(s.Args) > 0 {
		target = s.Args[0]
	}
	switch r.Scope {
	case "allow":
		return vm.OkResult(vm.Ok(vm.ScopeProof(target, r.Decision)))
	case "deny":
		return vm.OkResult(vm.Err(vm.Record(bytecode.TypeScopeDenied, vm.Str(vm.Display(target)),
			vm.Str(r.Reason), vm.None(), vm.Str(r.Decision))))
	}
	role := ""
	if len(s.Args) > 0 {
		role = s.Args[0].Str()
	}
	switch r.Approval {
	case "approve":
		return vm.OkResult(vm.Ok(vm.Record(bytecode.TypeApproval, vm.Str("ap-"+s.Path.String()),
			vm.Str(role), vm.Str(r.By), vm.Time(1_700_000_000_000_000_000))))
	case "deny":
		return vm.OkResult(vm.Err(vm.Record(bytecode.TypeDenied, vm.Str("ap-"+s.Path.String()),
			vm.Str(role), vm.Str(r.Reason))))
	}
	t, err := vm.ParseType(m, r.Type)
	if err != nil {
		return vm.FailResult(bytecode.FailTypeMismatch, "script: "+err.Error())
	}
	raw := r.Value
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	v, err := vm.FromJSON(m, t, raw)
	if err != nil {
		return vm.FailResult(bytecode.FailTypeMismatch, "script: "+err.Error())
	}
	return vm.OkResult(v)
}

// parMapOrder returns the scripted task order of the par_map at path.
func (sc *script) parMapOrder(path vm.Path, n int) []int {
	if o, ok := sc.ParMapOrder[path.String()]; ok && len(o) == n {
		return o
	}
	return nil
}

// runOutcome is a finished run: the outcome and the full effect trace
// (compensations included).
type runOutcome struct {
	out   vm.Outcome
	trace []vm.TraceEntry
	comps int
}

// vmDriver runs the compiled module the way the engine would: fuel
// slices, par_map tasks in the scripted completion order joined in input
// order, and on failure the compensations in reverse registration order
// with effect prefix [next_seq, k] (vm OPEN_QUESTIONS 1).
type vmDriver struct {
	prog  *bytecode.Verified
	cfg   vm.Config
	sc    *script
	h     vm.Handler
	slice int64
	// args records the display of every effect's arguments by path.
	args map[string]string
}

func (d *vmDriver) run(args []vm.Value) (runOutcome, error) {
	v, err := vm.New(d.prog, d.cfg)
	if err != nil {
		return runOutcome{}, err
	}
	if err := v.Start(args); err != nil {
		return runOutcome{}, err
	}
	var ro runOutcome
	out, err := d.drive(v, &ro.trace)
	if err != nil {
		return runOutcome{}, err
	}
	ro.out = out
	if _, failed := out.(vm.Failed); failed && !d.sc.NoCompensate {
		for k, c := range v.Compensations() {
			child, err := vm.NewChild(d.prog, c, nil, vm.Path{uint32(v.NextSeq()), uint32(k)}, d.cfg)
			if err != nil {
				return runOutcome{}, err
			}
			if _, err := d.drive(child, &ro.trace); err != nil {
				return runOutcome{}, err
			}
			ro.comps++
		}
	}
	return ro, nil
}

func (d *vmDriver) drive(v *vm.VM, trace *[]vm.TraceEntry) (vm.Outcome, error) {
	slice := d.slice
	if slice <= 0 {
		slice = math.MaxInt64
	}
	out, err := v.Run(slice)
	for err == nil {
		switch o := out.(type) {
		case vm.OutOfFuel:
			out, err = v.Run(slice)
			continue
		case vm.Suspended:
			hash, herr := vm.ArgsHash(o.Args)
			if herr != nil {
				return nil, herr
			}
			if d.args != nil {
				d.args[o.Path.String()] = displayArgs(d.prog.Module(), o.Args)
			}
			idx := len(*trace)
			*trace = append(*trace, vm.TraceEntry{Path: o.Path.String(), Kind: o.Kind, ArgsHash: hash})
			var res vm.EffectResult
			if o.Kind == bytecode.EffectParMap {
				if res, err = d.parMap(v, o, trace); err != nil {
					return nil, err
				}
			} else {
				res = d.h(o)
			}
			(*trace)[idx].Result = res
			out, err = v.Resume(res, slice)
			continue
		}
		return out, nil
	}
	return nil, err
}

func (d *vmDriver) parMap(parent *vm.VM, s vm.Suspended, trace *[]vm.TraceEntry) (vm.EffectResult, error) {
	pm, err := vm.ParMapArgs(s)
	if err != nil {
		return vm.FailResult(bytecode.FailTypeMismatch, err.Error()), nil
	}
	order := make([]int, len(pm.Items))
	for i := range order {
		order[i] = i
	}
	if o := d.sc.parMapOrder(s.Path, len(pm.Items)); o != nil {
		order = o
	}
	outs := make([]vm.Outcome, len(pm.Items))
	for _, i := range order {
		child, err := pm.NewTask(parent.Program(), s, i, d.cfg)
		if err != nil {
			return vm.FailResult(bytecode.FailEffectFailed, err.Error()), nil
		}
		o, err := d.drive(child, trace)
		if err != nil {
			return vm.EffectResult{}, err
		}
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
	return pm.Join(outs), nil
}

// runTreeWalk evaluates the same program with the tree-walker.
func runTreeWalk(res *compiler.Result, sc *script, cfg vm.Config, args []vm.Value, h vm.Handler) (runOutcome, error) {
	p, err := treewalk.New(res.Program, treewalk.Config{VM: cfg, Module: res.Module, Closures: res.Closures,
		ParMapOrder: sc.parMapOrder})
	if err != nil {
		return runOutcome{}, err
	}
	ev, err := p.Start(res.Workflow, args)
	if err != nil {
		return runOutcome{}, err
	}
	var ro runOutcome
	ro.out = ev.Drive(h, &ro.trace)
	if _, failed := ro.out.(vm.Failed); failed && !sc.NoCompensate {
		for k, c := range ev.Compensations() {
			p.NewChild(c, nil, vm.Path{uint32(ev.NextSeq()), uint32(k)}).Drive(h, &ro.trace)
			ro.comps++
		}
	}
	return ro, nil
}

// ---- golden form ----

// golden is expected.json of a conformance/vm/run case.
type golden struct {
	Value         json.RawMessage `json:"value,omitempty"`
	Display       string          `json:"display,omitempty"`
	Failure       string          `json:"failure,omitempty"`
	Trace         []goldenEffect  `json:"trace"`
	Compensations int             `json:"compensations,omitempty"`
}

type goldenEffect struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Args     string `json:"args"`
	ArgsHash string `json:"args_hash"`
	Result   string `json:"result"`
}

// toGolden renders a run; args are the VM's display of the arguments.
func toGolden(m *bytecode.Module, ro runOutcome, argsText map[string]string) (golden, error) {
	g := golden{Trace: []goldenEffect{}, Compensations: ro.comps}
	switch o := ro.out.(type) {
	case vm.Completed:
		b, err := vm.ToJSON(m, o.Value, vm.JSONOptions{})
		if err != nil {
			g.Value = json.RawMessage(strconv.Quote("<" + err.Error() + ">"))
		} else {
			g.Value = b
		}
		g.Display = vm.DisplayIn(m, o.Value)
	case vm.Failed:
		g.Failure = o.Code.String()
	default:
		return g, fmt.Errorf("unexpected outcome %T", ro.out)
	}
	for _, e := range ro.trace {
		r := "ok " + vm.DisplayIn(m, e.Result.Value)
		if e.Result.Failed {
			r = "failed " + e.Result.Code.String()
		}
		g.Trace = append(g.Trace, goldenEffect{Path: e.Path, Kind: e.Kind.String(), Args: argsText[e.Path],
			ArgsHash: hex.EncodeToString(e.ArgsHash[:]), Result: r})
	}
	return g, nil
}

func displayArgs(m *bytecode.Module, args []vm.Value) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = vm.DisplayIn(m, a)
	}
	b, _ := json.Marshal(parts)
	return string(b)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dslTool(name, sig string, effects ...string) dsl.ToolSpec {
	return dsl.ToolSpec{Name: name, Signature: sig, Effects: effects}
}
