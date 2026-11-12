package vm_test

import (
	"errors"
	"testing"

	bc "github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/vm"
)

// TestVM_SuspendedSite: a suspension reports the EFFECT's function and
// offset, also after a snapshot round trip, so the engine can look up the
// module's per-site annotations.
func TestVM_SuspendedSite(t *testing.T) {
	prog := program(t, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
		f.Op(bc.OpUnit, bc.OpPop)
		f.Span(40, 5)
		f.Effect(bc.EffectNow, 0)
		f.Op(bc.OpRet)
	})
	v, err := vm.New(prog, vm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Start(nil); err != nil {
		t.Fatal(err)
	}
	out, _ := v.Run(1000)
	s, ok := out.(vm.Suspended)
	if !ok || s.Site.Func != 0 || s.Site.CodeOffset != 2 || !s.Site.HasSource || s.Site.SourceOffset != 40 {
		t.Fatalf("site %+v", out)
	}
	snap, err := v.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	r, err := vm.Restore(prog, snap, vm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := r.Pending(); !ok || p.Site != s.Site {
		t.Fatalf("restored site %+v, want %+v", p.Site, s.Site)
	}
}

// TestVM_CallBuiltin: the native builtins are callable outside a run with
// the BUILTIN instruction's results and failures.
func TestVM_CallBuiltin(t *testing.T) {
	r, err := vm.CallBuiltin(nil, vm.Config{}, bc.BJoin, []vm.Value{vm.List(vm.Str("a"), vm.Str("b")), vm.Str("-")})
	if err != nil || r.Str() != "a-b" {
		t.Fatalf("join: %v %v", r, err)
	}
	_, err = vm.CallBuiltin(nil, vm.Config{}, bc.BFloatToInt, []vm.Value{vm.Float(1e30)})
	var f vm.Failed
	if !errors.As(err, &f) || f.Code != bc.FailArithOverflow {
		t.Fatalf("float_to_int overflow: %v", err)
	}
	if _, err := vm.CallBuiltin(nil, vm.Config{}, bc.BJoin, nil); err == nil {
		t.Fatal("wrong arity accepted")
	}
	c := vm.NewClosure(3, []vm.Value{vm.Int(1)})
	if c.Kind() != vm.KindClosure || c.FuncIndex() != 3 || len(c.Fields()) != 1 {
		t.Fatalf("closure %v", c)
	}
	if vm.NewClosure(3, nil).Fields() != nil {
		t.Fatal("a closure without captures has no capture tuple")
	}
}
