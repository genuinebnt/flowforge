package vm_test

import (
	"crypto/sha256"
	"math"
	"testing"

	bc "github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/vm"
)

var (
	httpxHash = sha256.Sum256([]byte("httpx spec"))
	undoHash  = sha256.Sum256([]byte("undo spec"))
	postHash  = sha256.Sum256([]byte("post spec"))
)

func testTools() bc.ToolMap {
	return bc.ToolMap{
		"httpx": {SpecSHA256: httpxHash, Effects: bc.EffActive},
		"post":  {SpecSHA256: postHash, Effects: bc.EffFinancialWrite},
		"undo":  {SpecSHA256: undoHash, Effects: bc.EffFinancialWrite},
	}
}

// newBuilder returns a builder with the test tools registered and every
// effect declared.
func newBuilder() *bc.Builder {
	b := bc.NewBuilder()
	b.Tool("httpx", httpxHash)
	b.Tool("post", postHash)
	b.Tool("undo", undoHash)
	b.SetEffects(bc.EffAll)
	return b
}

func build(t testing.TB, b *bc.Builder) *bc.Verified {
	t.Helper()
	m, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	p, err := bc.Verify(m, testTools())
	if err != nil {
		t.Fatalf("verify: %v\n%s", err, bc.DisassembleModule(m))
	}
	return p
}

// program builds a module whose entry is body with the given arity.
func program(t testing.TB, arity uint8, body func(b *bc.Builder, f *bc.FuncBuilder)) *bc.Verified {
	t.Helper()
	b := newBuilder()
	f := b.Func("main", arity, 0)
	b.SetEntry(f.Index())
	body(b, f)
	return build(t, b)
}

func noEffects(s vm.Suspended) vm.EffectResult {
	return vm.FailResult(bc.FailEffectFailed, "unexpected effect")
}

func runProg(t testing.TB, p *bc.Verified, args ...vm.Value) vm.Outcome {
	t.Helper()
	out, _, err := vm.RunWithEffects(p, vm.Config{}, args, noEffects)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func wantValue(t testing.TB, out vm.Outcome, want vm.Value) {
	t.Helper()
	c, ok := out.(vm.Completed)
	if !ok {
		t.Fatalf("outcome %#v, want Completed(%s)", out, want)
	}
	if !vm.Equal(c.Value, want) {
		t.Fatalf("got %s, want %s", c.Value, want)
	}
}

func wantFail(t testing.TB, out vm.Outcome, code bc.FailCode) {
	t.Helper()
	f, ok := out.(vm.Failed)
	if !ok || f.Code != code {
		t.Fatalf("outcome %#v, want Failed(%s)", out, code)
	}
}

func ints(xs ...int64) vm.Value {
	vs := make([]vm.Value, len(xs))
	for i, x := range xs {
		vs[i] = vm.Int(x)
	}
	return vm.List(vs...)
}

func strs(xs ...string) vm.Value {
	vs := make([]vm.Value, len(xs))
	for i, x := range xs {
		vs[i] = vm.Str(x)
	}
	return vm.List(vs...)
}

// addFib adds fn fib(n) = n < 2 ? n : fib(n-1) + fib(n-2).
func addFib(b *bc.Builder) uint16 {
	f := b.Func("fib", 1, 0)
	rec := f.NewLabel()
	f.Load(0)
	f.Int(2)
	f.Op(bc.OpLt)
	f.JumpIfFalse(rec)
	f.Load(0)
	f.Op(bc.OpRet)
	f.Bind(rec)
	f.Load(0)
	f.Int(1)
	f.Op(bc.OpSubI)
	f.Call(f.Index(), 1)
	f.Load(0)
	f.Int(2)
	f.Op(bc.OpSubI)
	f.Call(f.Index(), 1)
	f.Op(bc.OpAddI, bc.OpRet)
	return f.Index()
}

// emitMapFilter emits, for the list on top of the stack,
// [x * 2 for x in xs if x % 3 == 0] using the loop pattern a compiler must
// follow: loop-carried values (accumulator, list, index) travel on the
// stack and are re-bound to fresh locals at the top of every iteration.
func emitMapFilter(f *bc.FuncBuilder) {
	acc, xs, i, x := f.Local(), f.Local(), f.Local(), f.Local()
	tmp := f.Local()
	f.Store(tmp)
	f.Op(bc.OpNewList)
	f.Load(tmp)
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
	f.Store(x)
	f.Load(x)
	f.Int(3)
	f.Op(bc.OpModI)
	f.Int(0)
	f.Op(bc.OpEq)
	skip, next := f.NewLabel(), f.NewLabel()
	f.JumpIfFalse(skip)
	f.Load(acc)
	f.Load(x)
	f.Int(2)
	f.Op(bc.OpMulI, bc.OpListPush)
	f.Jump(next)
	f.Bind(skip)
	f.Load(acc)
	f.Bind(next)
	f.Load(xs)
	f.Load(i)
	f.Int(1)
	f.Op(bc.OpAddI)
	f.Loop(head)
	f.Bind(exit)
	f.Load(acc)
}

// emitSumLoop emits sum over i in 0..n of (i*i) % 7, n in local nLocal.
func emitSumLoop(f *bc.FuncBuilder, nLocal uint16) {
	s, i := f.Local(), f.Local()
	f.Int(0)
	f.Int(0)
	head := f.Here()
	f.Store(i)
	f.Store(s)
	f.Load(i)
	f.Load(nLocal)
	f.Op(bc.OpLt)
	exit := f.NewLabel()
	f.JumpIfFalse(exit)
	f.Load(s)
	f.Load(i)
	f.Load(i)
	f.Op(bc.OpMulI)
	f.Int(7)
	f.Op(bc.OpModI, bc.OpAddI)
	f.Load(i)
	f.Int(1)
	f.Op(bc.OpAddI)
	f.Loop(head)
	f.Bind(exit)
	f.Load(s)
}

var bigLimits = vm.Config{FuelLimit: math.MaxInt64 / 2, Limits: vm.Limits{Heap: 1 << 34, Effects: vm.MaxEffectsOverride}}
