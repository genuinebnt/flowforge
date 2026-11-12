package vm_test

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"

	bc "github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/vm"
)

func sampleValues(t *testing.T) []vm.Value {
	m, _ := vm.Map([]vm.Value{vm.Str("b"), vm.Str("a")}, []vm.Value{vm.Int(1), ints(2)})
	return []vm.Value{
		vm.Unit(), vm.Bool(true), vm.Int(math.MinInt64), vm.Duration(5), vm.Time(-3), vm.Float(-0.5),
		vm.Str("héllo"), ints(1, 2), vm.Stream(vm.Str("x")), m,
		vm.Tuple(vm.Int(1), vm.Str("a")), vm.Some(vm.None()), vm.Err(vm.Str("e")),
		mustV(vm.Fqdn("a.com")), mustV(vm.IPAddress("::1")), mustV(vm.Netblock("10.0.0.0/8")),
		mustV(vm.Asn("AS1")), mustV(vm.URL("https://a.com/x")), vm.EntityRef("ent_1"),
		vm.ScopeProof(mustV(vm.Host("a.com")), "dec-1"),
		vm.Enum(bc.TypeSeverity, 4),
	}
}

func TestVMCodec_ValueRoundTrip(t *testing.T) {
	p := reconProgram(t)
	vals := sampleValues(t)
	for _, v := range vals {
		b, err := vm.EncodeValue(v)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		got, err := vm.DecodeValue(p, b)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if !vm.Equal(got, v) || got.IsStream() != v.IsStream() {
			t.Fatalf("round trip %s -> %s", v, got)
		}
		again, _ := vm.EncodeValue(got)
		if !bytes.Equal(again, b) {
			t.Fatalf("%s: encoding not canonical", v)
		}
	}
	args, err := vm.EncodeArgs(vals)
	if err != nil {
		t.Fatal(err)
	}
	back, err := vm.DecodeArgs(p, args)
	if err != nil || len(back) != len(vals) {
		t.Fatal(err)
	}
	h1, _ := vm.ArgsHash(vals)
	h2, _ := vm.ArgsHash(append(vals[:len(vals)-1:len(vals)-1], vm.Enum(bc.TypeSeverity, 3)))
	if h1 == h2 {
		t.Fatal("hash ignores a changed argument")
	}
	// Sharing does not change the encoding.
	shared := ints(1, 2)
	a, _ := vm.EncodeValue(vm.List(shared, shared))
	b, _ := vm.EncodeValue(vm.List(ints(1, 2), ints(1, 2)))
	if !bytes.Equal(a, b) {
		t.Fatal("encoding depends on sharing")
	}

	t.Run("rejects", func(t *testing.T) {
		cases := map[string][]byte{
			"non_canonical_fqdn": {0x82, 0x0c, 0x65, 'A', '.', 'c', 'o', 'm'},
			"unknown_kind":       {0x81, 0x18, 0x63},
			"bad_type_id":        {0x83, 0x09, 0x19, 0x03, 0xe8, 0x80},
			"closure_bad_fn":     {0x83, 0x0b, 0x18, 0x63, 0x80},
			"trailing":           {0x81, 0x00, 0x00},
		}
		for name, b := range cases {
			if _, err := vm.DecodeValue(p, b); !errors.Is(err, vm.ErrDecode) {
				t.Errorf("%s: got %v", name, err)
			}
		}
	})
}

func TestVMJSON_Conversion(t *testing.T) {
	b := newBuilder()
	svc := b.Record("HttpService", bc.Field{Name: "url", Type: "Url"}, bc.Field{Name: "status", Type: "Int"},
		bc.Field{Name: "title", Type: "Option<String>"}, bc.Field{Name: "tags", Type: "List<String>"})
	color := b.Enum("Color", bc.Variant{Name: "Red"}, bc.Variant{Name: "Rgb", Payload: []string{"Int", "Int", "Int"}})
	f := b.Func("main", 2, 0)
	b.SetEntry(f.Index())
	b.Signature(f.Index(), []bc.Field{{Name: "roots", Type: "List<Fqdn>"}, {Name: "limit", Type: "Option<Int>"}}, "Report")
	f.Load(0)
	f.Op(bc.OpRet)
	p := build(t, b)
	m := p.Module()

	v := vm.Record(svc, mustV(vm.URL("https://a.com/")), vm.Int(200), vm.None(), strs("x"))
	j, err := vm.ToJSON(m, v, vm.JSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(j), `{"status":200,"tags":["x"],"title":null,"url":"https://a.com/"}`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	typ, err := vm.ParseType(m, "List<HttpService>")
	if err != nil {
		t.Fatal(err)
	}
	back, err := vm.FromJSON(m, typ, []byte(`[{"url":"HTTPS://A.com:443","status":200,"tags":["x"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if !vm.Equal(back, vm.List(v)) {
		t.Fatalf("FromJSON gave %s", back)
	}
	for _, c := range []struct {
		typ  string
		v    vm.Value
		json string
	}{
		{"Color", vm.Enum(color, 0), `{"variant":"Red"}`},
		{"Color", vm.Enum(color, 1, vm.Int(1), vm.Int(2), vm.Int(3)), `{"values":[1,2,3],"variant":"Rgb"}`},
		{"Result<Int, String>", vm.Err(vm.Str("no")), `{"err":"no"}`},
		{"Option<Duration>", vm.Some(vm.Duration(90e9)), `"1m30s"`},
		{"(Int, Bool)", vm.Tuple(vm.Int(1), vm.Bool(false)), `[1,false]`},
		{"Map<String, Int>", mustV(vm.Map([]vm.Value{vm.Str("a")}, []vm.Value{vm.Int(1)})), `{"a":1}`},
		{"Map<Int, Int>", mustV(vm.Map([]vm.Value{vm.Int(2)}, []vm.Value{vm.Int(1)})), `[[2,1]]`},
		{"Host", mustV(vm.Host("10.0.0.1")), `{"values":["10.0.0.1"],"variant":"Ip"}`},
		{"Time", vm.Time(1_500_000_000), `"1970-01-01T00:00:01.5Z"`},
		{"Float", vm.Float(2), `2`},
	} {
		got, err := vm.ToJSON(m, c.v, vm.JSONOptions{})
		if err != nil || string(got) != c.json {
			t.Errorf("ToJSON(%s) = %s, %v; want %s", c.v, got, err, c.json)
		}
		ty, err := vm.ParseType(m, c.typ)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := vm.FromJSON(m, ty, []byte(c.json))
		if err != nil || !vm.Equal(dec, c.v) {
			t.Errorf("FromJSON(%s, %s) = %s, %v", c.typ, c.json, dec, err)
		}
	}
	if _, err := vm.ToJSON(m, vm.ScopeProof(vm.Str("a"), "d"), vm.JSONOptions{}); !errors.Is(err, vm.ErrProofInOutput) {
		t.Fatalf("proof in output: %v", err)
	}
	if j, err := vm.ToJSON(m, vm.ScopeProof(vm.Str("a"), "d"), vm.JSONOptions{UnwrapProofs: true}); err != nil || string(j) != `"a"` {
		t.Fatalf("unwrapped proof: %s %v", j, err)
	}
	if _, err := vm.EncodeOutput(p, vm.List(vm.ScopeProof(vm.Str("a"), "d"))); !errors.Is(err, vm.ErrProofInOutput) {
		t.Fatal("nested proof accepted in output")
	}
	ins, err := vm.DecodeInputs(p, []byte(`{"roots":["Example.COM"]}`))
	if err != nil || len(ins) != 2 || !vm.Equal(ins[0], fqdns(t, "example.com")) || !vm.Equal(ins[1], vm.None()) {
		t.Fatalf("inputs %v %v", ins, err)
	}
	for _, bad := range []string{`{"roots":["a..b"]}`, `{"roots":[], "extra":1}`, `{}`, `[]`} {
		if _, err := vm.DecodeInputs(p, []byte(bad)); err == nil {
			t.Errorf("accepted inputs %s", bad)
		}
	}
	for _, bad := range []string{"List", "Nope", "(Int) -> Int", "Map<Int>", "InScope<Fqdn"} {
		if _, err := vm.ParseType(m, bad); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
	if ty, _ := vm.ParseType(m, "InScope<Fqdn>"); true {
		if _, err := vm.FromJSON(m, ty, []byte(`"a.com"`)); err == nil {
			t.Error("decoded a proof from JSON")
		}
	}
}

func TestVMLimits(t *testing.T) {
	doubling := func(t *testing.T, seed func(f *bc.FuncBuilder), op func(f *bc.FuncBuilder)) *bc.Verified {
		return program(t, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			seed(f)
			h := f.Here()
			x := f.Local()
			f.Store(x)
			f.Load(x)
			f.Load(x)
			op(f)
			f.Loop(h)
			f.Op(bc.OpRet) // unreachable
		})
	}
	t.Run("string", func(t *testing.T) {
		p := doubling(t, func(f *bc.FuncBuilder) { f.String("ab") }, func(f *bc.FuncBuilder) { f.Op(bc.OpConcat) })
		wantFail(t, runProg(t, p), bc.FailLimitExceeded)
	})
	t.Run("list", func(t *testing.T) {
		p := doubling(t, func(f *bc.FuncBuilder) { list(1)(f) }, func(f *bc.FuncBuilder) { f.Builtin(bc.BConcat) })
		wantFail(t, runProg(t, p), bc.FailLimitExceeded)
	})
	t.Run("heap", func(t *testing.T) {
		// Keep every intermediate list alive in a growing list of lists.
		p := program(t, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			f.Op(bc.OpNewList)
			h := f.Here()
			acc := f.Local()
			f.Store(acc)
			f.Load(acc)
			f.Int(0)
			f.Int(1000)
			f.Builtin(bc.BRange)
			f.Op(bc.OpListPush)
			f.Loop(h)
			f.Op(bc.OpRet)
		})
		out, _, _ := vm.RunWithEffects(p, vm.Config{Limits: vm.Limits{Heap: 1 << 20}}, nil, noEffects)
		wantFail(t, out, bc.FailLimitExceeded)
	})
	t.Run("garbage_is_not_live", func(t *testing.T) {
		// Allocating far more than the heap limit is fine when it dies.
		p := program(t, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			f.Int(0)
			h := f.Here()
			i := f.Local()
			f.Store(i)
			f.Int(0)
			f.Int(1000)
			f.Builtin(bc.BRange)
			f.Op(bc.OpPop)
			f.Load(i)
			f.Int(2000)
			f.Op(bc.OpLt)
			exit := f.NewLabel()
			f.JumpIfFalse(exit)
			f.Load(i)
			f.Int(1)
			f.Op(bc.OpAddI)
			f.Loop(h)
			f.Bind(exit)
			f.Load(i)
			f.Op(bc.OpRet)
		})
		out, _, _ := vm.RunWithEffects(p, vm.Config{Limits: vm.Limits{Heap: 1 << 20}}, nil, noEffects)
		wantValue(t, out, vm.Int(2000))
	})
	t.Run("effects", func(t *testing.T) {
		p := program(t, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			for i := 0; i < 3; i++ {
				f.Effect(bc.EffectNow, 0)
				f.Op(bc.OpPop)
			}
			f.Op(bc.OpUnit, bc.OpRet)
		})
		h := func(vm.Suspended) vm.EffectResult { return vm.OkResult(vm.Time(1)) }
		out, _, _ := vm.RunWithEffects(p, vm.Config{Limits: vm.Limits{Effects: 2}}, nil, h)
		wantFail(t, out, bc.FailLimitExceeded)
		out, _, _ = vm.RunWithEffects(p, vm.Config{Limits: vm.Limits{Effects: 3}}, nil, h)
		wantValue(t, out, vm.Unit())
	})
	t.Run("stack", func(t *testing.T) {
		b := newBuilder()
		fib := addFib(b)
		f := b.Func("main", 0, 0)
		b.SetEntry(f.Index())
		f.Int(30)
		f.Call(fib, 1)
		f.Op(bc.OpRet)
		out, _, _ := vm.RunWithEffects(build(t, b), vm.Config{Limits: vm.Limits{Stack: 20}}, nil, noEffects)
		wantFail(t, out, bc.FailLimitExceeded)
	})
	t.Run("config", func(t *testing.T) {
		p := reconProgram(t)
		if _, err := vm.New(p, vm.Config{Limits: vm.Limits{Effects: vm.MaxEffectsOverride + 1}}); !errors.Is(err, vm.ErrConfig) {
			t.Fatal("accepted an effects limit over 1,000,000")
		}
	})
}

func TestVM_SelfReferencePushStaysAcyclic(t *testing.T) {
	// let l = []; let m = l.push(l); m.push(m) -- snapshots must still work.
	p := program(t, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
		l, m := f.Local(), f.Local()
		f.Op(bc.OpNewList)
		f.Store(l)
		f.Load(l)
		f.Load(l)
		f.Op(bc.OpListPush)
		f.Store(m)
		f.Load(m)
		f.Emit(bc.OpMakeList, 1)
		f.Load(m)
		f.Op(bc.OpListPush)
		f.Effect(bc.EffectNow, 0)
		f.Op(bc.OpPop, bc.OpRet)
	})
	v, _ := vm.New(p, vm.Config{})
	_ = v.Start(nil)
	out, _ := v.Run(math.MaxInt64)
	if _, ok := out.(vm.Suspended); !ok {
		t.Fatalf("%#v", out)
	}
	snap, err := v.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	r, err := vm.Restore(p, snap, vm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	out, _ = r.Resume(vm.OkResult(vm.Time(0)), math.MaxInt64)
	want := vm.List(vm.List(vm.List()), vm.List(vm.List()))
	wantValue(t, out, want)
}

type allowCom struct{}

func (allowCom) Allows(v vm.Value) bool { return strings.HasSuffix(v.Str(), ".com") }

func TestVM_ScopeAllowsAndCancel(t *testing.T) {
	p := program(t, 1, func(b *bc.Builder, f *bc.FuncBuilder) {
		f.Load(0)
		f.Builtin(bc.BHostParse)
		f.Op(bc.OpTry)
		f.Builtin(bc.BScopeAllows)
		f.Op(bc.OpRet)
	})
	for in, want := range map[string]bool{"a.com": true, "a.org": false} {
		out, _, _ := vm.RunWithEffects(p, vm.Config{Scope: allowCom{}}, []vm.Value{vm.Str(in)}, noEffects)
		wantValue(t, out, vm.Bool(want))
	}
	v, _ := vm.New(reconProgram(t), vm.Config{})
	_ = v.Start([]vm.Value{reconInput(t)})
	_, _ = v.Run(math.MaxInt64)
	wantFail(t, v.Cancel(bc.FailCancelled, "user"), bc.FailCancelled)
	if _, err := v.Resume(vm.OkResult(vm.Unit()), 1); !errors.Is(err, vm.ErrState) {
		t.Fatal("resumed a cancelled VM")
	}
}
