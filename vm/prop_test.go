package vm_test

import (
	"math"
	"testing"

	bc "github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
	"github.com/genuinebnt/flowforge/vm"
)

type scopeAll struct{}

func (scopeAll) Allows(v vm.Value) bool { return v.Kind() == vm.KindFqdn }

// runChecked drives v with small fuel slices, snapshotting and restoring
// at every stop; it returns the final outcome and the fuel used.
func runChecked(t testing.TB, p *bc.Verified, cfg vm.Config, v *vm.VM, slice func() int64) (vm.Outcome, int64) {
	t.Helper()
	var out vm.Outcome
	var err error
	if s, ok := v.Pending(); ok {
		out = s
	} else if v.Done() {
		return v.Outcome(), v.FuelUsed()
	} else if out, err = v.Run(slice()); err != nil {
		t.Fatal(err)
	}
	for steps := 0; ; steps++ {
		if steps > 1_000_000 {
			t.Fatal("no progress")
		}
		snap, err := v.Snapshot()
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		r, err := vm.Restore(p, snap, cfg)
		if err != nil {
			t.Fatalf("restore of a valid snapshot failed: %v\n%s", err, bc.DisassembleModule(p.Module()))
		}
		v = r
		switch o := out.(type) {
		case vm.OutOfFuel:
			out, err = v.Run(slice())
		case vm.Suspended:
			if o.Kind == bc.EffectParMap {
				out, err = v.Resume(vm.FailResult(bc.FailEffectFailed, "no par_map here"), slice())
			} else {
				out, err = v.Resume(genHandler(o), slice())
			}
		default:
			return out, v.FuelUsed()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestVMProp_VerifiedNeverPanics(t *testing.T) {
	r := testkit.Rng(t)
	accepted := 0
	cfg := vm.Config{FuelLimit: 5000, Scope: scopeAll{}}
	for i := 0; i < testkit.Cases(); i++ {
		m := genModule(r)
		p, err := bc.Verify(m, testTools())
		if err != nil {
			continue
		}
		accepted++
		for _, arg := range []vm.Value{vm.Int(3), ints(1, 2), vm.Str("a.com")} {
			v, err := vm.New(p, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.Start([]vm.Value{arg}); err != nil {
				t.Fatal(err)
			}
			out, _ := runChecked(t, p, cfg, v, func() int64 { return int64(1 + core.IntN(r, 60)) })
			switch out.(type) {
			case vm.Completed, vm.Failed:
			default:
				t.Fatalf("unexpected outcome %#v", out)
			}
			for _, c := range v.Compensations() {
				child, err := vm.NewChild(p, c, nil, vm.Path{9}, cfg)
				if err != nil {
					t.Fatal(err)
				}
				var tr []vm.TraceEntry
				if _, err := vm.Drive(child, genHandler, &tr); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if accepted < testkit.Cases()/2 {
		t.Fatalf("generator too weak: %d of %d modules verified", accepted, testkit.Cases())
	}
}

func TestVMProp_FuelMonotonic(t *testing.T) {
	r := testkit.Rng(t)
	cfg := vm.Config{FuelLimit: 20000, Scope: scopeAll{}}
	for i := 0; i < testkit.Cases(); i++ {
		p, err := bc.Verify(genModule(r), testTools())
		if err != nil {
			continue
		}
		args := []vm.Value{vm.Int(int64(core.IntN(r, 5)))}
		ref, _ := vm.New(p, cfg)
		_ = ref.Start(args)
		var tr []vm.TraceEntry
		want, err := vm.Drive(ref, genHandler, &tr)
		if err != nil {
			t.Fatal(err)
		}
		if f, ok := want.(vm.Failed); ok && f.Code == bc.FailOutOfFuel {
			continue
		}
		used := ref.FuelUsed()
		// Any slicing of the same fuel gives the same outcome and usage.
		v, _ := vm.New(p, cfg)
		_ = v.Start(args)
		got, gotUsed := runChecked(t, p, cfg, v, func() int64 { return int64(core.IntN(r, 80)) })
		if !sameOutcome(got, want) || gotUsed != used {
			t.Fatalf("sliced run: %#v (fuel %d), want %#v (fuel %d)", got, gotUsed, want, used)
		}
		// A smaller total limit fails with out_of_fuel; a larger one changes nothing.
		for _, limit := range []int64{used - 1, used, used + 1 + int64(core.IntN(r, 1000))} {
			if limit < 0 {
				continue
			}
			c := cfg
			c.FuelLimit = limit
			if limit == 0 {
				c.FuelLimit = -1
				continue
			}
			v, _ := vm.New(p, c)
			_ = v.Start(args)
			var tr []vm.TraceEntry
			out, _ := vm.Drive(v, genHandler, &tr)
			if limit < used {
				if f, ok := out.(vm.Failed); !ok || f.Code != bc.FailOutOfFuel {
					t.Fatalf("limit %d < %d: %#v", limit, used, out)
				}
				if v.FuelUsed() > limit {
					t.Fatalf("used %d over limit %d", v.FuelUsed(), limit)
				}
			} else if !sameOutcome(out, want) || v.FuelUsed() != used {
				t.Fatalf("limit %d >= %d: %#v", limit, used, out)
			}
		}
	}
}

func sameOutcome(a, b vm.Outcome) bool {
	switch x := a.(type) {
	case vm.Completed:
		y, ok := b.(vm.Completed)
		return ok && vm.Equal(x.Value, y.Value)
	case vm.Failed:
		y, ok := b.(vm.Failed)
		return ok && x.Code == y.Code && x.Span == y.Span
	}
	return false
}

func FuzzVerifier(f *testing.F) {
	r := core.NewSplitMix64(1)
	for i := 0; i < 20; i++ {
		if b, err := genModule(r).Encode(); err == nil {
			f.Add(b)
		}
	}
	for _, p := range []*bc.Verified{reconProgram(f), compProgram(f), parMapProgram(f, 1)} {
		b, _ := p.Module().Encode()
		f.Add(b)
	}
	cfg := vm.Config{FuelLimit: 3000, Scope: scopeAll{}}
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := bc.Decode(b)
		if err != nil {
			return
		}
		p, err := bc.Verify(m, testTools())
		if err != nil {
			return
		}
		args := make([]vm.Value, m.Funcs[m.Entry].Arity)
		for i := range args {
			args[i] = vm.Int(int64(i))
		}
		v, err := vm.New(p, cfg)
		if err != nil {
			return
		}
		if err := v.Start(args); err != nil {
			return
		}
		runChecked(t, p, cfg, v, func() int64 { return 97 })
	})
}

func FuzzSnapshotDecode(f *testing.F) {
	p := reconProgram(f)
	cfg := vm.Config{FuelLimit: 5000}
	v, _ := vm.New(p, cfg)
	_ = v.Start([]vm.Value{reconInput(f)})
	h := reconEffects()
	out, _ := v.Run(40)
	for i := 0; i < 40; i++ {
		snap, err := v.Snapshot()
		if err != nil {
			f.Fatal(err)
		}
		f.Add(snap)
		switch o := out.(type) {
		case vm.OutOfFuel:
			out, _ = v.Run(40)
			continue
		case vm.Suspended:
			out, _ = v.Resume(h.Handle(o), 40)
			continue
		}
		break
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := vm.Restore(p, b, cfg)
		if err != nil {
			return
		}
		runChecked(t, p, cfg, r, func() int64 { return math.MaxInt64 })
	})
}
