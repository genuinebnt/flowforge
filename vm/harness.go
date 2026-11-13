package vm

import (
	"encoding/hex"
	"fmt"
	"math"

	"github.com/genuinebnt/flowforge/bytecode"
)

// Handler performs (or replays) one effect.
type Handler func(Suspended) EffectResult

// TraceEntry is one effect as the engine would log it: the path, kind and
// argument hash of effect_scheduled and the result of effect_completed or
// effect_failed.
type TraceEntry struct {
	Path     string
	Kind     bytecode.EffectKind
	ArgsHash [32]byte
	Result   EffectResult
}

// String renders one trace line for test failure messages: path, kind,
// the first four bytes of the args hash in hex, and "ok <value>" or
// "failed <code>".
func (e TraceEntry) String() string {
	r := "ok " + Display(e.Result.Value)
	if e.Result.Failed {
		r = "failed " + e.Result.Code.String()
	}
	return fmt.Sprintf("%s %s %s %s", e.Path, e.Kind, hex.EncodeToString(e.ArgsHash[:4]), r)
}

// RunWithEffects runs the workflow body of prog with args to completion,
// asking h for every effect except par_map, which it executes itself by
// running one child VM per element (sequentially, in input order) and
// joining the results. It returns the final outcome (Completed or Failed)
// and the trace of all effects, children included, in the order they were
// scheduled. It is the reference driver for tests, replay checks and the
// tree-walker differential test.
func RunWithEffects(prog *bytecode.Verified, cfg Config, args []Value, h Handler) (Outcome, []TraceEntry, error) {
	v, err := New(prog, cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := v.Start(args); err != nil {
		return nil, nil, err
	}
	var trace []TraceEntry
	out, err := Drive(v, h, &trace)
	return out, trace, err
}

// Drive continues v (fresh, stopped by fuel, or suspended on an effect
// whose result h provides) until it completes or fails, appending every
// effect to trace.
func Drive(v *VM, h Handler, trace *[]TraceEntry) (Outcome, error) {
	var out Outcome
	var err error
	if s, ok := v.Pending(); ok {
		out = s
	} else if out, err = v.Run(math.MaxInt64); err != nil {
		return nil, err
	}
	for {
		s, ok := out.(Suspended)
		if !ok {
			return out, nil
		}
		hash, err := ArgsHash(s.Args)
		if err != nil {
			return nil, err
		}
		idx := len(*trace)
		*trace = append(*trace, TraceEntry{Path: s.Path.String(), Kind: s.Kind, ArgsHash: hash})
		var res EffectResult
		if s.Kind == bytecode.EffectParMap {
			res, err = runParMap(v, s, h, trace)
			if err != nil {
				return nil, err
			}
		} else {
			res = h(s)
		}
		(*trace)[idx].Result = res
		if out, err = v.Resume(res, math.MaxInt64); err != nil {
			return nil, err
		}
	}
}

func runParMap(parent *VM, s Suspended, h Handler, trace *[]TraceEntry) (EffectResult, error) {
	pm, err := ParMapArgs(s)
	if err != nil {
		return FailResult(bytecode.FailTypeMismatch, err.Error()), nil
	}
	outs := make([]Outcome, len(pm.Items))
	for i := range pm.Items {
		child, err := pm.NewTask(parent.prog, s, i, parent.cfg)
		if err != nil {
			return FailResult(bytecode.FailEffectFailed, err.Error()), nil
		}
		o, err := Drive(child, h, trace)
		if err != nil {
			return EffectResult{}, err
		}
		outs[i] = o
		if _, failed := o.(Failed); failed && !pm.Collect {
			break // fail fast: later siblings are cancelled
		}
	}
	for i, o := range outs {
		if o == nil {
			outs[i] = Failed{Code: bytecode.FailCancelled, Message: "cancelled"}
		}
	}
	return pm.Join(outs), nil
}

// ScriptedEffects is the in-memory effect double of spec 24.3: it answers
// effects from scripted results by path, by tool name or by kind, in that
// order, and records every call. An effect with no script fails with
// effect_failed.
type ScriptedEffects struct {
	ByPath map[string]EffectResult
	ByTool map[string]Handler
	ByKind map[bytecode.EffectKind]Handler
	// Calls records every effect handled, in order.
	Calls []Suspended
}

// Handle implements Handler.
func (s *ScriptedEffects) Handle(e Suspended) EffectResult {
	s.Calls = append(s.Calls, e)
	if r, ok := s.ByPath[e.Path.String()]; ok {
		return r
	}
	if e.Kind == bytecode.EffectToolCall {
		if f, ok := s.ByTool[e.Tool()]; ok {
			return f(e)
		}
	}
	if f, ok := s.ByKind[e.Kind]; ok {
		return f(e)
	}
	return FailResult(bytecode.FailEffectFailed, "no scripted result for effect "+e.Path.String())
}

// Replayer answers effects from a recorded trace (spec 8.6): an effect
// whose path is in the trace is resumed with the recorded result if its
// kind and argument hash match, or fails the run with
// nondeterminism_detected if they differ. Effects past the end of the
// trace (the live frontier) go to Live, or fail when Live is nil.
type Replayer struct {
	entries map[string]TraceEntry
	Live    Handler
	// Replayed and Executed count effects answered from the trace and
	// passed to Live.
	Replayed, Executed int
}

// NewReplayer indexes trace by path.
func NewReplayer(trace []TraceEntry, live Handler) *Replayer {
	r := &Replayer{entries: make(map[string]TraceEntry, len(trace)), Live: live}
	for _, e := range trace {
		r.entries[e.Path] = e
	}
	return r
}

// Handle implements Handler.
func (r *Replayer) Handle(s Suspended) EffectResult {
	if e, ok := r.entries[s.Path.String()]; ok {
		h, err := ArgsHash(s.Args)
		if err != nil || h != e.ArgsHash || e.Kind != s.Kind {
			return FailResult(bytecode.FailNondeterminismDetected, "effect "+s.Path.String()+" differs from the log")
		}
		r.Replayed++
		return e.Result
	}
	if r.Live == nil {
		return FailResult(bytecode.FailEffectFailed, "effect "+s.Path.String()+" is not in the log")
	}
	r.Executed++
	return r.Live(s)
}
