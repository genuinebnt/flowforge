package sim

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/vm"
)

// effectLog is one effect as the run's log records it.
type effectLog struct {
	path, kind, argsHash, tool string
	scheduledSeq               uint64
	started                    map[int]uint64 // attempt -> seq of effect_started
	terminals                  int
	done, failed               bool
	value                      string // base64 CBOR of a completed result
	result                     json.RawMessage
	failCode, errCode, errMsg  string
	resolved                   bool
}

type decisionLog struct {
	seq      uint64
	path     string
	purpose  string
	decision scope.Decision
}

type compLog struct {
	started  *engine.CompensationStartedPayload
	steps    []engine.CompensationStepPayload
	stepSeqs []uint64
	finished *engine.CompensationFinishedPayload
}

// parsedLog is the part of a run's log the checker needs.
type parsedLog struct {
	effects   map[string]*effectLog
	order     []string
	decisions []decisionLog
	comp      compLog
	observed  []string // observation ids of observation_ingested
	terminal  string   // type of the terminal event, "" if none
	failCode  string
	output    json.RawMessage
	bad       []string // payloads that do not decode
}

func parseLog(evs []eventstore.Event) *parsedLog {
	l := &parsedLog{effects: map[string]*effectLog{}}
	dec := func(e eventstore.Event, v any) bool {
		if err := json.Unmarshal(e.Payload, v); err != nil {
			l.bad = append(l.bad, fmt.Sprintf("seq %d %s: %v", e.Seq, e.Type, err))
			return false
		}
		return true
	}
	get := func(path string) *effectLog {
		if f := l.effects[path]; f != nil {
			return f
		}
		f := &effectLog{path: path, started: map[int]uint64{}}
		l.effects[path] = f
		return f
	}
	for _, e := range evs {
		switch e.Type {
		case engine.EvEffectScheduled:
			var p engine.EffectScheduledPayload
			if dec(e, &p) {
				f := get(p.Path)
				f.kind, f.argsHash, f.tool, f.scheduledSeq = p.Kind, p.ArgsHash, p.Tool, e.Seq
				l.order = append(l.order, p.Path)
			}
		case engine.EvEffectStarted:
			var p engine.EffectStartedPayload
			if dec(e, &p) {
				get(p.Path).started[p.Attempt] = e.Seq
			}
		case engine.EvEffectCompleted:
			var p engine.EffectCompletedPayload
			if dec(e, &p) {
				f := get(p.Path)
				f.terminals++
				f.done, f.value, f.result, f.resolved = true, p.Value, p.Result, p.Resolution != nil
			}
		case engine.EvEffectFailed:
			var p engine.EffectFailedPayload
			if dec(e, &p) && p.Final {
				f := get(p.Path)
				f.terminals++
				f.done, f.failed, f.failCode, f.errCode, f.errMsg = true, true, p.FailCode, p.Error.Code, p.Error.Message
			}
		case engine.EvScopeDecision:
			var p engine.ScopeDecisionPayload
			if dec(e, &p) {
				l.decisions = append(l.decisions, decisionLog{seq: e.Seq, path: p.Path, purpose: p.Purpose, decision: p.Decision})
			}
		case engine.EvObservationIngested:
			var p engine.ObservationIngestedPayload
			if dec(e, &p) {
				l.observed = append(l.observed, p.ObservationID)
			}
		case engine.EvCompensationStarted:
			var p engine.CompensationStartedPayload
			if dec(e, &p) {
				l.comp.started = &p
			}
		case engine.EvCompensationStep:
			var p engine.CompensationStepPayload
			if dec(e, &p) {
				l.comp.steps = append(l.comp.steps, p)
				l.comp.stepSeqs = append(l.comp.stepSeqs, e.Seq)
			}
		case engine.EvCompensationFinished:
			var p engine.CompensationFinishedPayload
			if dec(e, &p) {
				l.comp.finished = &p
			}
		case engine.EvRunSucceeded:
			var p engine.RunSucceededPayload
			if dec(e, &p) {
				l.terminal, l.output = e.Type, p.Output
			}
		case engine.EvRunFailed:
			var p engine.RunFailedPayload
			if dec(e, &p) {
				l.terminal, l.failCode = e.Type, p.Code
			}
		case engine.EvRunCancelled:
			l.terminal = e.Type
		}
	}
	return l
}

// compPrefix returns the path prefix of compensation effects ("N/"), or "".
func (l *parsedLog) compPrefix() string {
	if l.comp.started == nil {
		return ""
	}
	return fmt.Sprintf("%d/", l.comp.started.PathPrefix)
}

// replayer re-executes a run's workflow on a fresh VM, answering every
// effect from the log (spec 8.6), and records which effects the program
// asked for, in order.
type replayer struct {
	prog    *bytecode.Verified
	cfg     vm.Config
	log     *parsedLog
	visited []string
	errs    []string
}

// loadModule decodes and verifies a recorded module.
func loadModule(m *Module) (*bytecode.Verified, error) {
	mod, err := bytecode.Decode(m.Encoded)
	if err != nil {
		return nil, err
	}
	res := bytecode.ToolMap{}
	byName := map[string]engine.ToolSpec{}
	for _, t := range m.Tools {
		byName[t.Name] = t
	}
	for i, t := range mod.Tools {
		name := mod.ToolName(i)
		spec, ok := byName[name]
		if !ok || spec.SpecSHA256 != t.SpecSHA256 {
			return nil, fmt.Errorf("tool %s is not the pinned version", name)
		}
		mask, _ := bytecode.ParseEffect(spec.EffectClass)
		res[name] = bytecode.ToolInfo{SpecSHA256: t.SpecSHA256, Effects: mask}
	}
	return bytecode.Verify(mod, res)
}

func (rp *replayer) recorded(e *effectLog) vm.EffectResult {
	if e.failed {
		code, ok := bytecode.ParseFailCode(e.failCode)
		if !ok {
			code = bytecode.FailEffectFailed
		}
		msg := e.errMsg
		if e.errCode != "" {
			msg = e.errCode + ": " + msg
		}
		return vm.FailResult(code, msg)
	}
	b, err := base64.StdEncoding.DecodeString(e.value)
	if err != nil {
		rp.errs = append(rp.errs, fmt.Sprintf("effect %s: result does not decode: %v", e.path, err))
		return vm.FailResult(bytecode.FailNondeterminismDetected, "bad result")
	}
	v, err := vm.DecodeValue(rp.prog, b)
	if err != nil {
		rp.errs = append(rp.errs, fmt.Sprintf("effect %s: result does not decode: %v", e.path, err))
		return vm.FailResult(bytecode.FailNondeterminismDetected, "bad result")
	}
	return vm.OkResult(v)
}

// drive runs v until it completes, fails, or asks for an effect the log
// has not finished (the frontier where the run stopped).
func (rp *replayer) drive(v *vm.VM) (vm.Outcome, bool) {
	out, err := v.Run(math.MaxInt64)
	for {
		if err != nil {
			rp.errs = append(rp.errs, "vm: "+err.Error())
			return nil, true
		}
		s, ok := out.(vm.Suspended)
		if !ok {
			return out, false
		}
		path := s.Path.String()
		e := rp.log.effects[path]
		if e == nil {
			return s, true
		}
		hash, herr := vm.ArgsHash(s.Args)
		if herr != nil || e.kind != s.Kind.String() || e.argsHash != hex.EncodeToString(hash[:]) {
			rp.errs = append(rp.errs, fmt.Sprintf("effect %s: the log has %s/%s, replay asks for %s/%x", path, e.kind, short(e.argsHash), s.Kind, hash[:6]))
			return s, true
		}
		rp.visited = append(rp.visited, path)
		if s.Kind == bytecode.EffectParMap {
			rp.parMap(s, e)
		}
		if !e.done {
			return s, true
		}
		out, err = v.Resume(rp.recorded(e), math.MaxInt64)
	}
}

// parMap replays the children of a par_map and checks the recorded join.
func (rp *replayer) parMap(s vm.Suspended, e *effectLog) {
	pm, err := vm.ParMapArgs(s)
	if err != nil {
		return
	}
	outs := make([]vm.Outcome, len(pm.Items))
	frontier := false
	for i := range pm.Items {
		child, err := pm.NewTask(rp.prog, s, i, rp.cfg)
		if err != nil {
			rp.errs = append(rp.errs, fmt.Sprintf("par_map %s task %d: %v", e.path, i, err))
			return
		}
		o, f := rp.drive(child)
		if f {
			frontier = true
			o = vm.Failed{Code: bytecode.FailCancelled, Message: "cancelled"}
		}
		outs[i] = o
	}
	if !e.done || e.resolved || (e.failed && (e.errCode == "cancelled" || e.errCode == "timeout")) {
		return // closed by the engine (cancel, deadline), not by its children
	}
	if frontier && pm.Collect {
		rp.errs = append(rp.errs, fmt.Sprintf("par_map %s (collect) completed but a task did not finish", e.path))
		return
	}
	res := pm.Join(outs)
	if res.Failed != e.failed {
		rp.errs = append(rp.errs, fmt.Sprintf("par_map %s: the log records failed=%v, the children join to failed=%v", e.path, e.failed, res.Failed))
		return
	}
	if !res.Failed {
		got, err := vm.EncodeValue(res.Value)
		want, derr := base64.StdEncoding.DecodeString(e.value)
		if err != nil || derr != nil || !bytes.Equal(got, want) {
			rp.errs = append(rp.errs, fmt.Sprintf("par_map %s: recorded result differs from the children's join", e.path))
		}
	}
}

// replayRun checks inv_05 for one run and returns the number of
// compensations the replayed VM had registered when it stopped (for
// inv_07).
func replayRun(h *History, r *RunRecord, l *parsedLog) (comps int, errs []string) {
	var mod *Module
	for i := range h.Modules {
		if h.Modules[i].VersionID == r.WorkflowVersion {
			mod = &h.Modules[i]
		}
	}
	if mod == nil {
		return 0, []string{"workflow version " + r.WorkflowVersion + " has no recorded module"}
	}
	prog, err := loadModule(mod)
	if err != nil {
		return 0, []string{"module: " + err.Error()}
	}
	var started bool
	for _, e := range r.Log {
		if e.Type == engine.EvRunStarted {
			started = true
		}
	}
	if !started {
		return 0, nil // cancelled before it started
	}
	inputs, err := vm.DecodeInputs(prog, r.Input)
	if err != nil {
		return 0, []string{"inputs: " + err.Error()}
	}
	rp := &replayer{prog: prog, cfg: vm.Config{FuelLimit: vm.DefaultFuelLimit}, log: l}
	if an := prog.Module().Annotations; an != nil && an.MaxEffects > 0 {
		rp.cfg.Limits.Effects = int(an.MaxEffects)
	}
	v, err := vm.New(prog, rp.cfg)
	if err == nil {
		err = v.Start(inputs)
	}
	if err != nil {
		return 0, []string{"vm: " + err.Error()}
	}
	out, frontier := rp.drive(v)
	comps = len(v.Compensations())
	if len(rp.errs) > 0 {
		return comps, rp.errs
	}
	engineStop := l.terminal == engine.EvRunCancelled || l.terminal == "" ||
		(l.terminal == engine.EvRunFailed && (l.failCode == bytecode.FailTimeout.String() ||
			l.failCode == bytecode.FailNondeterminismDetected.String() || l.failCode == bytecode.FailBudgetExceeded.String()))
	switch o := out.(type) {
	case vm.Completed:
		switch l.terminal {
		case engine.EvRunSucceeded:
			got, err := vm.EncodeOutput(prog, o.Value)
			if err != nil || !jsonEqual(got, l.output) {
				errs = append(errs, fmt.Sprintf("replay output %s, the log's %s", got, l.output))
			}
		case engine.EvRunCancelled, "":
		default:
			errs = append(errs, "replay completed, the run ended with "+l.terminal+" "+l.failCode)
		}
	case vm.Failed:
		if !engineStop && !(l.terminal == engine.EvRunFailed && l.failCode == o.Code.String()) {
			errs = append(errs, fmt.Sprintf("replay failed with %s, the run ended with %s %s", o.Code, l.terminal, l.failCode))
		}
	default:
		if frontier && !engineStop {
			errs = append(errs, fmt.Sprintf("replay stopped at an unfinished effect, the run ended with %s %s", l.terminal, l.failCode))
		}
	}
	// Effect order: per VM (the body, each par_map task), the effects
	// replay asked for must be the log's, in the log's order.
	cp := l.compPrefix()
	errs = append(errs, compareOrder(rp.visited, l.order, cp, l.terminal == engine.EvRunSucceeded)...)

	// Compensations replay as child VMs N/k with their recorded effects.
	if c := l.comp.started; c != nil {
		list := v.Compensations()
		for k, step := range l.comp.steps {
			if k >= len(list) {
				break
			}
			child, err := vm.NewChild(prog, list[k], nil, vm.Path{c.PathPrefix, uint32(k)}, rp.cfg)
			if err != nil {
				errs = append(errs, fmt.Sprintf("compensation %d: %v", k, err))
				continue
			}
			o, f := rp.drive(child)
			_, failed := o.(vm.Failed)
			if f && step.Result == "ok" {
				errs = append(errs, fmt.Sprintf("compensation %d recorded ok but replay stops at an unfinished effect", k))
			} else if !f && failed != (step.Result != "ok") {
				errs = append(errs, fmt.Sprintf("compensation %d recorded %s, replay failed=%v", k, step.Result, failed))
			}
		}
		errs = append(errs, rp.errs...)
	}
	return comps, errs
}

// compareOrder checks that, per group of sibling effects, the paths
// replay visited are the log's paths in the log's order; when the run
// completed normally they must be all of them.
func compareOrder(visited, logged []string, compPrefix string, complete bool) []string {
	group := func(p string) string {
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			return p[:i]
		}
		return ""
	}
	byGroup := func(ps []string) map[string][]string {
		m := map[string][]string{}
		for _, p := range ps {
			if compPrefix != "" && strings.HasPrefix(p, compPrefix) {
				continue
			}
			m[group(p)] = append(m[group(p)], p)
		}
		return m
	}
	v, l := byGroup(visited), byGroup(logged)
	var errs []string
	for g, lp := range l {
		vp := v[g]
		if len(vp) > len(lp) || strings.Join(vp, ",") != strings.Join(lp[:len(vp)], ",") {
			errs = append(errs, fmt.Sprintf("effects under %q: replay %v, log %v", g, vp, lp))
		} else if complete && len(vp) != len(lp) {
			errs = append(errs, fmt.Sprintf("effects under %q: the log has %v, replay asked only for %v", g, lp, vp))
		}
	}
	for g, vp := range v {
		if _, ok := l[g]; !ok {
			errs = append(errs, fmt.Sprintf("effects under %q: replay %v, none logged", g, vp))
		}
	}
	return errs
}

func jsonEqual(a, b []byte) bool {
	ca, err1 := core.CanonicalJSON(json.RawMessage(a))
	cb, err2 := core.CanonicalJSON(json.RawMessage(b))
	return err1 == nil && err2 == nil && bytes.Equal(ca, cb)
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
