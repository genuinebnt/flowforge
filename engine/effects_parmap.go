package engine

import (
	"fmt"
	"strings"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/vm"
)

// doParMap performs par_map (spec 8.6, 9.5): one child VM per element with
// hierarchical effect paths parent/task/child, at most limit active at
// once, results joined in input order. With on_error = fail the first
// failure (lowest index among the failed tasks of a round) fails the
// effect and the siblings' open effects are cancelled; with collect every
// element becomes Ok(v) or Err(TaskError).
func (r *runner) doParMap(s vm.Suspended, e *effectState, ph phase) (*stepResult, error) {
	pm, err := vm.ParMapArgs(s)
	if err != nil {
		return nil, r.failEffect(e, ActorEngine, EffectError{Code: "invalid_arguments", Message: err.Error()}, bytecode.FailTypeMismatch)
	}
	tasks := make([]*task, len(pm.Items))
	for i := range pm.Items {
		child, err := pm.NewTask(r.lm.prog, s, i, r.vmcfg)
		if err != nil {
			return nil, r.failEffect(e, ActorEngine, EffectError{Code: "invalid_arguments", Message: err.Error()}, bytecode.FailTypeMismatch)
		}
		tasks[i] = &task{vm: child, out: child.Outcome()}
	}
	limit := int(min(pm.Limit, int64(len(tasks)+1)))
	st, err := r.runTasks(tasks, limit, pm.Collect, ph)
	if err != nil || st.kind != stepFinished {
		return &st, err
	}
	if !pm.Collect {
		for i, t := range tasks {
			f, ok := t.out.(vm.Failed)
			if !ok {
				continue
			}
			extra := r.closeOpenUnder(s.Path.String()+"/", "cancelled", "a sibling task failed", bytecode.FailCancelled)
			return nil, r.failEffect(e, ActorEngine, EffectError{
				Code: f.Code.String(), Message: fmt.Sprintf("par_map task %d failed: %s", i, f.Message),
			}, bytecode.FailEffectFailed, extra...)
		}
	}
	outs := make([]vm.Outcome, len(tasks))
	for i, t := range tasks {
		outs[i] = t.out
	}
	res := pm.Join(outs)
	if res.Failed {
		return nil, r.failEffect(e, ActorEngine, EffectError{Code: res.Code.String(), Message: res.Message}, res.Code)
	}
	return nil, r.completeEffect(e, ActorEngine, res.Value)
}

// closeOpenUnder returns effect_failed events for every open effect whose
// path starts with prefix ("" for all), in scheduling order.
func (r *runner) closeOpenUnder(prefix, code, msg string, fail bytecode.FailCode) []pending {
	var evs []pending
	for _, path := range r.log.order {
		e := r.log.effects[path]
		if e.done || !strings.HasPrefix(path, prefix) {
			continue
		}
		evs = append(evs, ev(EvEffectFailed, ActorEngine, EffectFailedPayload{
			Path: path, Attempt: e.attempt, Final: true, FailCode: fail.String(),
			Error: EffectError{Code: code, Message: msg},
		}))
	}
	return evs
}
