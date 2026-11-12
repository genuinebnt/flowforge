package compiler_test

import (
	"fmt"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
	"github.com/genuinebnt/flowforge/vm"
)

// TestVM008_TreeWalkVsVM (vm_008, spec 25.6): every conformance/vm/run
// case evaluated by the tree-walker gives the same final value or failure
// code and the same effect trace (paths, kinds, argument hashes, results)
// as the VM running the compiled module. Fuel is not modeled by the
// tree-walker, so cases that exhaust fuel are compared without the limit.
// The generated-program half also runs as TestCompilerProp_TreeWalkEqualsVM
// with more cases.
func TestVM008_TreeWalkVsVM(t *testing.T) {
	t.Run("generated", func(t *testing.T) {
		r := testkit.Rng(t)
		sc := &script{}
		for i := 0; i < 50; i++ {
			res, src := genCompile(t, r, i)
			d := &vmDriver{prog: res.Verified, sc: sc, h: genHandler}
			want, err := d.run(nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := runTreeWalk(res, sc, vm.Config{}, nil, genHandler)
			if err != nil {
				t.Fatal(err)
			}
			if err := sameRun(want, got); err != nil {
				t.Fatalf("case %d: %v\n%s", i, err, src)
			}
		}
	})
	for _, c := range loadVMCases(t, "run") {
		t.Run(c.name, func(t *testing.T) {
			res := compileCase(t, c)
			args := caseInputs(t, res, c)
			cfg := c.sc.vmConfig()
			cfg.FuelLimit = 0
			h := c.sc.handler(res.Module)
			d := &vmDriver{prog: res.Verified, cfg: cfg, sc: &c.sc, h: h}
			want, err := d.run(args)
			if err != nil {
				t.Fatal(err)
			}
			got, err := runTreeWalk(res, &c.sc, cfg, args, h)
			if err != nil {
				t.Fatal(err)
			}
			if err := sameRun(want, got); err != nil {
				t.Fatalf("tree-walker differs from the VM: %v", err)
			}
		})
	}
}

// sameRun compares two runs: outcome and trace.
func sameRun(vmRun, twRun runOutcome) error {
	if err := sameOutcome(vmRun.out, twRun.out); err != nil {
		return err
	}
	if vmRun.comps != twRun.comps {
		return fmt.Errorf("compensations: vm ran %d, tree-walker %d", vmRun.comps, twRun.comps)
	}
	if len(vmRun.trace) != len(twRun.trace) {
		return fmt.Errorf("trace length: vm %d, tree-walker %d\nvm: %v\ntw: %v", len(vmRun.trace), len(twRun.trace), vmRun.trace, twRun.trace)
	}
	for i := range vmRun.trace {
		a, b := vmRun.trace[i], twRun.trace[i]
		if a.Path != b.Path || a.Kind != b.Kind || a.ArgsHash != b.ArgsHash {
			return fmt.Errorf("effect %d: vm %v, tree-walker %v", i, a, b)
		}
		if err := sameResult(a.Result, b.Result); err != nil {
			return fmt.Errorf("effect %d (%s) result: %v", i, a.Path, err)
		}
	}
	return nil
}

func sameOutcome(a, b vm.Outcome) error {
	switch x := a.(type) {
	case vm.Completed:
		y, ok := b.(vm.Completed)
		if !ok {
			return fmt.Errorf("vm completed with %s, tree-walker gave %v", vm.Display(x.Value), b)
		}
		return sameValue(x.Value, y.Value)
	case vm.Failed:
		y, ok := b.(vm.Failed)
		if !ok {
			return fmt.Errorf("vm failed with %s, tree-walker gave %v", x.Code, b)
		}
		if x.Code != y.Code {
			return fmt.Errorf("vm failed with %s (%s), tree-walker with %s (%s)", x.Code, x.Message, y.Code, y.Message)
		}
		return nil
	}
	return fmt.Errorf("unexpected vm outcome %T", a)
}

func sameResult(a, b vm.EffectResult) error {
	if a.Failed != b.Failed || (a.Failed && a.Code != b.Code) {
		return fmt.Errorf("vm %+v, tree-walker %+v", a, b)
	}
	if a.Failed {
		return nil
	}
	return sameValue(a.Value, b.Value)
}

// sameValue compares structurally and by canonical encoding, which also
// covers stream flags and closure identities.
func sameValue(a, b vm.Value) error {
	if !vm.Equal(a, b) {
		return fmt.Errorf("vm %s, tree-walker %s", vm.Display(a), vm.Display(b))
	}
	ha, err1 := vm.ArgsHash([]vm.Value{a})
	hb, err2 := vm.ArgsHash([]vm.Value{b})
	if err1 != nil || err2 != nil || ha != hb {
		return fmt.Errorf("encodings differ: vm %s, tree-walker %s", vm.Display(a), vm.Display(b))
	}
	return nil
}
