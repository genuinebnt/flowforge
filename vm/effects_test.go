package vm_test

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"

	bc "github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
	"github.com/genuinebnt/flowforge/vm"
)

// reconProgram is the effectful workflow used by vm_003..vm_005:
//
//	workflow recon(roots: List<Fqdn>) -> (List<String>, Duration) {
//	  let t0 = now();
//	  let found = for r in roots { match scope.check(r) { Ok(p) => [httpx(p)], Err(_) => [] } };
//	  let t1 = now();
//	  emit_alert(Severity::High, "found {len(found)}");
//	  (found, t1 - t0)
//	}
func reconProgram(t testing.TB) *bc.Verified {
	return program(t, 1, func(b *bc.Builder, f *bc.FuncBuilder) {
		t0, t1 := f.Local(), f.Local()
		acc, xs, i, res, out := f.Local(), f.Local(), f.Local(), f.Local(), f.Local()
		f.Effect(bc.EffectNow, 0)
		f.Store(t0)
		f.Op(bc.OpNewList)
		f.Load(0)
		f.Int(0)
		head := f.Here()
		f.Store(i)
		f.Store(xs)
		f.Store(acc)
		f.Load(i)
		f.Load(xs)
		f.Op(bc.OpListLen, bc.OpLt)
		exit := f.NewLabel()
		f.JumpIfFalse(exit)
		f.Load(xs)
		f.Load(i)
		f.Op(bc.OpListGet)
		f.Effect(bc.EffectScopeCheck, 1)
		f.Store(res)
		f.Load(res)
		f.Emit(bc.OpTestVariant, int64(bc.VariantOk))
		denied, next := f.NewLabel(), f.NewLabel()
		f.JumpIfFalse(denied)
		f.Emit(bc.OpUnpack, 1)
		f.ToolCall("httpx", 1)
		f.Store(out)
		f.Load(acc)
		f.Load(out)
		f.Op(bc.OpListPush)
		f.Jump(next)
		f.Bind(denied)
		f.Op(bc.OpPop)
		f.Load(acc)
		f.Bind(next)
		f.Load(xs)
		f.Load(i)
		f.Int(1)
		f.Op(bc.OpAddI)
		f.Loop(head)
		f.Bind(exit)
		f.Effect(bc.EffectNow, 0)
		f.Store(t1)
		f.MakeEnum(bc.TypeSeverity, 3, 0)
		f.String("found ")
		f.Load(acc)
		f.Op(bc.OpListLen)
		f.Emit(bc.OpInterp, 2)
		f.Effect(bc.EffectEmitAlert, 2)
		f.Op(bc.OpPop)
		f.Load(acc)
		f.Load(t1)
		f.Load(t0)
		f.Op(bc.OpSubI)
		f.MakeRec(bc.TypeTuple, 2)
		f.Op(bc.OpRet)
	})
}

func fqdns(t testing.TB, names ...string) vm.Value {
	vs := make([]vm.Value, len(names))
	for i, n := range names {
		v, err := vm.Fqdn(n)
		if err != nil {
			t.Fatal(err)
		}
		vs[i] = v
	}
	return vm.List(vs...)
}

func reconEffects() *vm.ScriptedEffects {
	return &vm.ScriptedEffects{
		ByKind: map[bc.EffectKind]vm.Handler{
			bc.EffectNow: func(s vm.Suspended) vm.EffectResult {
				return vm.OkResult(vm.Time(int64(1_000_000 * (s.Seq + 1))))
			},
			bc.EffectScopeCheck: func(s vm.Suspended) vm.EffectResult {
				target := s.Args[0]
				if strings.Contains(target.Str(), "evil") {
					return vm.OkResult(vm.Err(vm.Record(bc.TypeScopeDenied, vm.Str(target.Str()), vm.Str("explicit_deny"), vm.None())))
				}
				return vm.OkResult(vm.Ok(vm.ScopeProof(target, "dec-"+s.Path.String())))
			},
			bc.EffectEmitAlert: func(vm.Suspended) vm.EffectResult { return vm.OkResult(vm.Unit()) },
		},
		ByTool: map[string]vm.Handler{
			"httpx": func(s vm.Suspended) vm.EffectResult {
				return vm.OkResult(vm.Str("200 " + s.Args[1].ProofTarget().Str()))
			},
		},
	}
}

func reconInput(t testing.TB) vm.Value {
	return fqdns(t, "a.example.com", "evil.example.com", "b.example.com")
}

func TestVM003_FuelExhaustion(t *testing.T) {
	// A small pure program: fib(7) + len(map_filter(0..12)) + len("{n}").
	b := newBuilder()
	fib := addFib(b)
	f := b.Func("main", 0, 0)
	b.SetEntry(f.Index())
	f.Int(7)
	f.Call(fib, 1)
	f.Int(0)
	f.Int(12)
	f.Builtin(bc.BRange)
	emitMapFilter(f)
	f.Op(bc.OpListLen, bc.OpAddI)
	f.Op(bc.OpDup)
	f.Emit(bc.OpInterp, 1)
	f.Builtin(bc.BLen)
	f.Op(bc.OpAddI, bc.OpRet)
	p := build(t, b)

	ref, err := vm.New(p, vm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_ = ref.Start(nil)
	out, _ := ref.Run(math.MaxInt64)
	wantValue(t, out, vm.Int(13+4+2))
	total := ref.FuelUsed()

	for k := int64(0); k <= total; k++ {
		v, _ := vm.New(p, vm.Config{})
		_ = v.Start(nil)
		out, err := v.Run(k)
		if err != nil {
			t.Fatal(err)
		}
		if k < total {
			if _, ok := out.(vm.OutOfFuel); !ok {
				t.Fatalf("fuel %d of %d: got %#v, want OutOfFuel", k, total, out)
			}
			if v.FuelUsed() > k {
				t.Fatalf("fuel %d: used %d", k, v.FuelUsed())
			}
			out, _ = v.Run(math.MaxInt64)
		}
		wantValue(t, out, vm.Int(19))
		if v.FuelUsed() != total {
			t.Fatalf("fuel %d: total %d, want %d", k, v.FuelUsed(), total)
		}
	}
	t.Run("single_steps", func(t *testing.T) {
		v, _ := vm.New(p, vm.Config{})
		_ = v.Start(nil)
		steps := 0
		for {
			out, _ := v.Run(1)
			if _, ok := out.(vm.OutOfFuel); !ok {
				wantValue(t, out, vm.Int(19))
				break
			}
			steps++
			if steps > int(total)*2 {
				t.Fatal("no progress")
			}
		}
		if v.FuelUsed() != total {
			t.Fatalf("total %d, want %d", v.FuelUsed(), total)
		}
	})
	t.Run("run_limit_fails", func(t *testing.T) {
		v, _ := vm.New(p, vm.Config{FuelLimit: total - 1})
		_ = v.Start(nil)
		out, _ := v.Run(math.MaxInt64)
		wantFail(t, out, bc.FailOutOfFuel)
	})
	t.Run("with_effects", func(t *testing.T) {
		rp := reconProgram(t)
		want, _, _ := vm.RunWithEffects(rp, vm.Config{}, []vm.Value{reconInput(t)}, reconEffects().Handle)
		for _, slice := range []int64{1, 3, 7, 49, 50, 51, 200} {
			v, _ := vm.New(rp, vm.Config{})
			_ = v.Start([]vm.Value{reconInput(t)})
			h := reconEffects()
			out, _ := v.Run(slice)
			for i := 0; ; i++ {
				if i > 100000 {
					t.Fatal("no progress")
				}
				switch o := out.(type) {
				case vm.OutOfFuel:
					out, _ = v.Run(slice)
					continue
				case vm.Suspended:
					out, _ = v.Resume(h.Handle(o), slice)
					continue
				}
				break
			}
			if !vm.Equal(out.(vm.Completed).Value, want.(vm.Completed).Value) {
				t.Fatalf("slice %d: %s != %s", slice, out.(vm.Completed).Value, want.(vm.Completed).Value)
			}
		}
	})
}

func TestVM004_Replay(t *testing.T) {
	p := reconProgram(t)
	live := reconEffects()
	out, trace, err := vm.RunWithEffects(p, vm.Config{}, []vm.Value{reconInput(t)}, live.Handle)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := out.(vm.Completed)
	if !ok {
		t.Fatalf("live run: %#v", out)
	}
	// now, 3 scope checks, 2 tool calls (one target denied), now, alert.
	if got := len(trace); got != 8 {
		t.Fatalf("trace has %d effects:\n%v", got, trace)
	}
	paths := make([]string, len(trace))
	for i, e := range trace {
		paths[i] = e.Path
		if e.Path != fmt.Sprint(i) {
			t.Fatalf("effect %d has path %s", i, e.Path)
		}
	}
	if s := fmt.Sprint(c.Value); !strings.Contains(s, "200 a.example.com") || !strings.Contains(s, "200 b.example.com") {
		t.Fatalf("unexpected result %s", s)
	}

	r := vm.NewReplayer(trace, nil)
	out2, trace2, err := vm.RunWithEffects(p, vm.Config{}, []vm.Value{reconInput(t)}, r.Handle)
	if err != nil {
		t.Fatal(err)
	}
	wantValue(t, out2, c.Value)
	if r.Executed != 0 || r.Replayed != len(trace) {
		t.Fatalf("replayed %d executed %d of %d", r.Replayed, r.Executed, len(trace))
	}
	for i := range trace {
		if trace2[i].Path != trace[i].Path || trace2[i].ArgsHash != trace[i].ArgsHash {
			t.Fatalf("effect %d differs on replay", i)
		}
	}

	t.Run("prefix_then_live_frontier", func(t *testing.T) {
		fresh := reconEffects()
		r := vm.NewReplayer(trace[:3], fresh.Handle)
		out, _, _ := vm.RunWithEffects(p, vm.Config{}, []vm.Value{reconInput(t)}, r.Handle)
		wantValue(t, out, c.Value)
		if r.Replayed != 3 || r.Executed != len(trace)-3 || len(fresh.Calls) != len(trace)-3 {
			t.Fatalf("replayed %d executed %d", r.Replayed, r.Executed)
		}
	})
	t.Run("nondeterminism_detected", func(t *testing.T) {
		r := vm.NewReplayer(trace, nil)
		other := fqdns(t, "a.example.com", "c.example.com", "b.example.com")
		out, _, _ := vm.RunWithEffects(p, vm.Config{}, []vm.Value{other}, r.Handle)
		wantFail(t, out, bc.FailNondeterminismDetected)
	})
}

// driveSnapshotAt runs p to completion, snapshotting, dropping and
// restoring the VM when it is suspended on effect number crashAt.
func driveSnapshotAt(t *testing.T, p *bc.Verified, args []vm.Value, crashAt int) (vm.Value, []string) {
	t.Helper()
	h := reconEffects()
	v, _ := vm.New(p, vm.Config{})
	if err := v.Start(args); err != nil {
		t.Fatal(err)
	}
	var order []string
	out, _ := v.Run(math.MaxInt64)
	for n := 0; ; n++ {
		s, ok := out.(vm.Suspended)
		if !ok {
			break
		}
		if n == crashAt {
			snap, err := v.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			v = nil // crash: the VM is gone
			restored, err := vm.Restore(p, snap, vm.Config{})
			if err != nil {
				t.Fatalf("restore at effect %d: %v", n, err)
			}
			again, err := restored.Snapshot()
			if err != nil || !bytes.Equal(again, snap) {
				t.Fatalf("snapshot of a restored VM differs (%v)", err)
			}
			v = restored
			s, _ = v.Pending()
		}
		h2, _ := vm.ArgsHash(s.Args)
		order = append(order, fmt.Sprintf("%s:%s:%x", s.Path, s.Kind, h2[:4]))
		out, _ = v.Resume(h.Handle(s), math.MaxInt64)
	}
	c, ok := out.(vm.Completed)
	if !ok {
		t.Fatalf("crash at %d: %#v", crashAt, out)
	}
	return c.Value, order
}

func TestVM005_CrashAtEveryEffect(t *testing.T) {
	p := reconProgram(t)
	args := []vm.Value{reconInput(t)}
	want, wantOrder := driveSnapshotAt(t, p, args, -1)
	for k := 0; k < len(wantOrder); k++ {
		got, order := driveSnapshotAt(t, p, args, k)
		if !vm.Equal(got, want) {
			t.Fatalf("crash at %d: result %s, want %s", k, got, want)
		}
		if strings.Join(order, ",") != strings.Join(wantOrder, ",") {
			t.Fatalf("crash at %d: effect order\n%v\nwant\n%v", k, order, wantOrder)
		}
	}
	t.Run("snapshot_at_every_fuel_stop", func(t *testing.T) {
		h := reconEffects()
		v, _ := vm.New(p, vm.Config{})
		_ = v.Start(args)
		out, _ := v.Run(13)
		for i := 0; i < 100000; i++ {
			snap, err := v.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if v, err = vm.Restore(p, snap, vm.Config{}); err != nil {
				t.Fatalf("restore: %v", err)
			}
			switch o := out.(type) {
			case vm.OutOfFuel:
				out, _ = v.Run(13)
				continue
			case vm.Suspended:
				out, _ = v.Resume(h.Handle(o), 13)
				continue
			}
			break
		}
		wantValue(t, out, want)
	})
	t.Run("restore_completed", func(t *testing.T) {
		v, _ := vm.New(p, vm.Config{})
		_ = v.Start(args)
		var trace []vm.TraceEntry
		if _, err := vm.Drive(v, reconEffects().Handle, &trace); err != nil {
			t.Fatal(err)
		}
		snap, _ := v.Snapshot()
		r, err := vm.Restore(p, snap, vm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		wantValue(t, r.Outcome(), want)
	})
	t.Run("wrong_module", func(t *testing.T) {
		v, _ := vm.New(p, vm.Config{})
		_ = v.Start(args)
		_, _ = v.Run(math.MaxInt64)
		snap, _ := v.Snapshot()
		other := program(t, 0, func(_ *bc.Builder, f *bc.FuncBuilder) { f.Op(bc.OpUnit, bc.OpRet) })
		if _, err := vm.Restore(other, snap, vm.Config{}); err == nil {
			t.Fatal("restored a snapshot into another module")
		}
	})
}

//	parMapProgram: workflow(xs: List<Fqdn>) -> List<String> {
//	  xs |> par_map(n => "p:" + httpx(n)?, limit = 2) }
//
// where httpx returns Option<String>; ? returns None from the closure.
func parMapProgram(t testing.TB, mode int64) *bc.Verified {
	b := newBuilder()
	probe := b.Func("probe", 1, 1)
	probe.Load(0)
	probe.ToolCall("httpx", 1)
	probe.Op(bc.OpTry)
	probe.Store(probe.Local())
	probe.Cap(0)
	probe.Load(1)
	probe.Op(bc.OpConcat, bc.OpRet)
	f := b.Func("main", 1, 0)
	b.SetEntry(f.Index())
	f.Load(0)
	f.String("p:")
	f.Closure(probe.Index(), 1)
	f.Int(2)
	f.Int(mode)
	f.Effect(bc.EffectParMap, 4)
	f.Op(bc.OpRet)
	return build(t, b)
}

func TestVM006_ParMapShuffled(t *testing.T) {
	names := []string{"a.com", "b.com", "c.com", "d.com", "e.com", "f.com"}
	rng := testkit.Rng(t)
	for _, mode := range []int64{bc.ParMapFail, bc.ParMapCollect} {
		p := parMapProgram(t, mode)
		for round := 0; round < 20; round++ {
			v, _ := vm.New(p, vm.Config{})
			_ = v.Start([]vm.Value{fqdns(t, names...)})
			out, _ := v.Run(math.MaxInt64)
			s, ok := out.(vm.Suspended)
			if !ok || s.Kind != bc.EffectParMap || s.Path.String() != "0" {
				t.Fatalf("want par_map at path 0, got %#v", out)
			}
			pm, err := vm.ParMapArgs(s)
			if err != nil || pm.Limit != 2 || len(pm.Items) != len(names) {
				t.Fatalf("par_map args: %+v %v", pm, err)
			}
			tasks := make([]*vm.VM, len(names))
			pend := make([]vm.Suspended, len(names))
			for i := range names {
				if tasks[i], err = pm.NewTask(p, s, i, vm.Config{}); err != nil {
					t.Fatal(err)
				}
				o, _ := tasks[i].Run(math.MaxInt64)
				cs, ok := o.(vm.Suspended)
				if !ok || cs.Path.String() != fmt.Sprintf("0/%d/0", i) || cs.Tool() != "httpx" {
					t.Fatalf("task %d: %#v", i, o)
				}
				pend[i] = cs
			}
			order := make([]int, len(names))
			for i := range order {
				order[i] = i
			}
			core.Shuffle(rng, len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			outs := make([]vm.Outcome, len(names))
			for _, i := range order {
				res := vm.OkResult(vm.Some(vm.Str("r" + names[i])))
				if names[i] == "c.com" {
					res = vm.OkResult(vm.None()) // ? returns None from the closure
				}
				if names[i] == "e.com" && mode == bc.ParMapCollect {
					res = vm.FailResult(bc.FailEffectFailed, "tool crashed")
				}
				outs[i], _ = tasks[i].Resume(res, math.MaxInt64)
			}
			out, _ = v.Resume(pm.Join(outs), math.MaxInt64)
			c, ok := out.(vm.Completed)
			if !ok {
				t.Fatalf("parent: %#v", out)
			}
			got := c.Value.Elems()
			for i, n := range names {
				var want vm.Value
				switch {
				case n == "c.com":
					want = vm.None()
				case n == "e.com" && mode == bc.ParMapCollect:
					want = vm.Err(vm.Record(bc.TypeTaskError, vm.Str("effect_failed"), vm.Str("tool crashed")))
				default:
					want = vm.Str("p:r" + n)
				}
				if mode == bc.ParMapCollect && n != "e.com" {
					want = vm.Ok(want)
				}
				if !vm.Equal(got[i], want) {
					t.Fatalf("element %d = %s, want %s", i, got[i], want)
				}
			}
		}
	}
	t.Run("fail_mode_failure", func(t *testing.T) {
		p := parMapProgram(t, bc.ParMapFail)
		h := &vm.ScriptedEffects{ByTool: map[string]vm.Handler{"httpx": func(s vm.Suspended) vm.EffectResult {
			if s.Path.String() == "0/1/0" {
				return vm.FailResult(bc.FailEffectFailed, "boom")
			}
			return vm.OkResult(vm.Some(vm.Str("ok")))
		}}}
		out, trace, err := vm.RunWithEffects(p, vm.Config{}, []vm.Value{fqdns(t, names...)}, h.Handle)
		if err != nil {
			t.Fatal(err)
		}
		wantFail(t, out, bc.FailEffectFailed)
		if trace[0].Path != "0" || trace[1].Path != "0/0/0" || trace[2].Path != "0/1/0" || len(trace) != 3 {
			t.Fatalf("trace %v", trace)
		}
	})
}

// compProgram: three saga steps `let x_k = post(k) compensate undo(it)`,
// then returns the list of ids.
func compProgram(t testing.TB) *bc.Verified {
	b := newBuilder()
	undo := b.Func("undo_step", 0, 1)
	undo.Cap(0)
	undo.ToolCall("undo", 1)
	undo.Op(bc.OpRet)
	f := b.Func("main", 0, 0)
	b.SetEntry(f.Index())
	var xs []uint16
	for k := int64(1); k <= 3; k++ {
		x := f.Local()
		xs = append(xs, x)
		f.Int(k)
		f.ToolCall("post", 1)
		f.Store(x)
		f.Load(x)
		f.PushComp(undo.Index(), 1)
	}
	for _, x := range xs {
		f.Load(x)
	}
	f.Emit(bc.OpMakeList, 3)
	f.Op(bc.OpRet)
	return build(t, b)
}

func TestVM007_CompensationOrder(t *testing.T) {
	p := compProgram(t)
	for failAt := 1; failAt <= 4; failAt++ {
		t.Run(fmt.Sprintf("fail_at_step_%d", failAt), func(t *testing.T) {
			step := 0
			h := &vm.ScriptedEffects{ByTool: map[string]vm.Handler{
				"post": func(s vm.Suspended) vm.EffectResult {
					step++
					if step == failAt {
						return vm.FailResult(bc.FailEffectFailed, "ledger down")
					}
					return vm.OkResult(vm.Str(fmt.Sprintf("id%d", s.Args[1].AsInt())))
				},
			}}
			v, _ := vm.New(p, vm.Config{})
			_ = v.Start(nil)
			var trace []vm.TraceEntry
			out, err := vm.Drive(v, h.Handle, &trace)
			if err != nil {
				t.Fatal(err)
			}
			if failAt == 4 {
				wantValue(t, out, strs("id1", "id2", "id3"))
				return
			}
			wantFail(t, out, bc.FailEffectFailed)
			comps := v.Compensations()
			if len(comps) != failAt-1 {
				t.Fatalf("%d compensations registered, want %d", len(comps), failAt-1)
			}
			var undone []string
			undo := &vm.ScriptedEffects{ByTool: map[string]vm.Handler{"undo": func(s vm.Suspended) vm.EffectResult {
				undone = append(undone, s.Args[1].Str()+"@"+s.Path.String())
				return vm.OkResult(vm.Unit())
			}}}
			base := v.NextSeq()
			for k, c := range comps {
				child, err := vm.NewChild(p, c, nil, vm.Path{uint32(base), uint32(k)}, vm.Config{})
				if err != nil {
					t.Fatal(err)
				}
				var ct []vm.TraceEntry
				o, err := vm.Drive(child, undo.Handle, &ct)
				if err != nil {
					t.Fatal(err)
				}
				wantValue(t, o, vm.Unit())
			}
			var want []string
			for k, i := 0, failAt-1; i >= 1; i, k = i-1, k+1 {
				want = append(want, fmt.Sprintf("id%d@%d/%d/0", i, base, k))
			}
			if strings.Join(undone, ",") != strings.Join(want, ",") {
				t.Fatalf("undo order %v, want %v", undone, want)
			}
		})
	}
}
