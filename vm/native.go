package vm

import (
	"fmt"

	"github.com/genuinebnt/flowforge/bytecode"
)

// CallBuiltin applies the pure native function id to args outside a run,
// with the same semantics, limits and failure codes as the BUILTIN
// instruction (fuel is not charged). m supplies type names for to_string
// and may be nil; cfg supplies the limits and the scope snapshot of
// scope_allows. A run-time failure is returned as a Failed error. It
// exists for evaluators that must agree with the VM, such as the
// compiler's tree-walking reference evaluator.
func CallBuiltin(m *bytecode.Module, cfg Config, id bytecode.BuiltinID, args []Value) (Value, error) {
	b, ok := bytecode.LookupBuiltin(id)
	if !ok {
		return Value{}, fmt.Errorf("%w: unknown builtin %d", ErrBadValue, id)
	}
	if len(args) != b.Arity {
		return Value{}, fmt.Errorf("%w: %s takes %d arguments, got %d", ErrBadValue, b.Name, b.Arity, len(args))
	}
	cfg, err := cfg.normalized()
	if err != nil {
		return Value{}, err
	}
	v := &VM{mod: m, cfg: cfg, lim: cfg.Limits}
	r, f := v.callBuiltin(id, args)
	if f != nil {
		return Value{}, Failed{Code: f.code, Message: f.msg, Span: Span{Func: -1, CodeOffset: -1}}
	}
	return r, nil
}

// NewClosure returns the closure value CLOSURE fn len(caps) would build
// from caps. It lets reference evaluators produce closure values that are
// identical (in equality, display and argument hashes) to the VM's. A VM
// accepts it as an input only if fn names a function of its module with
// that many captures.
func NewClosure(fn uint16, caps []Value) Value {
	c := Value{k: KindClosure, n: uint64(fn)}
	if len(caps) > 0 {
		c.p = &tupObj{vals: append([]Value(nil), caps...)}
	}
	return c
}
