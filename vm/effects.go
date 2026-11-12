package vm

import (
	"fmt"

	"github.com/genuinebnt/flowforge/bytecode"
)

// ParMap is the decoded argument list of an EFFECT par_map (spec 8.5, 8.6).
type ParMap struct {
	Items   []Value // shared, read-only
	Fn      Value   // closure taking one argument
	Limit   int64   // concurrency limit (at least 1)
	Collect bool    // on_error = collect
	Stream  bool    // the input was a Stream; the joined result is too
}

// ParMapArgs decodes a par_map suspension.
func ParMapArgs(s Suspended) (ParMap, error) {
	if s.Kind != bytecode.EffectParMap || len(s.Args) != 4 {
		return ParMap{}, fmt.Errorf("%w: not a par_map effect", ErrBadValue)
	}
	list, fn, limit, mode := s.Args[0], s.Args[1], s.Args[2], s.Args[3]
	if list.k != KindList || fn.k != KindClosure || limit.k != KindInt || mode.k != KindInt {
		return ParMap{}, fmt.Errorf("%w: par_map arguments are (List, Closure, Int, Int)", ErrBadValue)
	}
	if m := mode.AsInt(); m != bytecode.ParMapFail && m != bytecode.ParMapCollect {
		return ParMap{}, fmt.Errorf("%w: par_map on_error %d", ErrBadValue, m)
	}
	return ParMap{
		Items: list.Elems(), Fn: fn, Limit: max(limit.AsInt(), 1),
		Collect: mode.AsInt() == bytecode.ParMapCollect, Stream: list.IsStream(),
	}, nil
}

// ChildPath returns the effect path prefix of par_map task i: the par_map
// effect's own path extended with the task index. A child VM created with
// this prefix numbers its effects parent_seq/task_index/child_seq.
func (p ParMap) ChildPath(parent Path, task int) Path { return parent.Child(uint32(task)) }

// NewTask creates the child VM for task i of a par_map suspended at
// parent.
func (p ParMap) NewTask(prog *bytecode.Verified, parent Suspended, task int, cfg Config) (*VM, error) {
	if task < 0 || task >= len(p.Items) {
		return nil, fmt.Errorf("%w: task %d of %d", ErrBadValue, task, len(p.Items))
	}
	return NewChild(prog, p.Fn, []Value{p.Items[task]}, p.ChildPath(parent.Path, task), cfg)
}

// Join builds the par_map effect result from the task outcomes, given in
// input order (each Completed or Failed), regardless of the order in which
// they finished. With on_error = fail the first failing task in input
// order fails the effect (the engine should have cancelled the siblings
// already); with collect each element becomes Ok(value) or
// Err(TaskError { code, message }).
func (p ParMap) Join(outcomes []Outcome) EffectResult {
	if len(outcomes) != len(p.Items) {
		return FailResult(bytecode.FailEffectFailed, fmt.Sprintf("par_map joined %d of %d tasks", len(outcomes), len(p.Items)))
	}
	elems := make([]Value, len(outcomes))
	for i, o := range outcomes {
		switch o := o.(type) {
		case Completed:
			if p.Collect {
				elems[i] = Ok(o.Value)
			} else {
				elems[i] = o.Value
			}
		case Failed:
			if !p.Collect {
				return FailResult(bytecode.FailEffectFailed, fmt.Sprintf("par_map task %d failed: %s: %s", i, o.Code, o.Message))
			}
			elems[i] = Err(Record(bytecode.TypeTaskError, Str(o.Code.String()), Str(o.Message)))
		default:
			return FailResult(bytecode.FailEffectFailed, fmt.Sprintf("par_map task %d did not finish", i))
		}
	}
	v := mkList(elems)
	if p.Stream {
		v.f = flagStream
	}
	return OkResult(v)
}
