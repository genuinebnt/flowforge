package vm_test

import (
	"crypto/sha256"
	"math"

	bc "github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/vm"
)

// progGen generates random, mostly well-formed modules: it tracks the
// abstract stack depth and the written locals so that most output passes
// the verifier, while value kinds are random (run-time type errors are
// expected and fine).
type progGen struct {
	r       core.Rng
	b       *bc.Builder
	helpers []*bc.FuncBuilder // arity 1, ncaps 0
	lambda  *bc.FuncBuilder   // arity 1, ncaps 1
	comp    *bc.FuncBuilder   // arity 0, ncaps 1
	budget  int
}

func (g *progGen) n(k int) int { return core.IntN(g.r, k) }

type genFrame struct {
	f       *bc.FuncBuilder
	depth   int
	locals  []uint16 // readable locals
	hasCaps bool
}

func genModule(r core.Rng) *bc.Module {
	g := &progGen{r: r, b: newBuilder()}
	main := g.b.Func("main", 1, 0)
	g.b.SetEntry(main.Index())
	for i := 0; i < 2; i++ {
		g.helpers = append(g.helpers, g.b.Func("h", 1, 0))
	}
	g.lambda = g.b.Func("lambda", 1, 1)
	g.comp = g.b.Func("comp", 0, 1)
	g.b.Record("R", bc.Field{Name: "a", Type: "Int"}, bc.Field{Name: "b", Type: "String"})

	g.body(&genFrame{f: main, locals: []uint16{0}}, 40)
	for _, h := range g.helpers {
		g.body(&genFrame{f: h, locals: []uint16{0}}, 12)
	}
	g.body(&genFrame{f: g.lambda, locals: []uint16{0}, hasCaps: true}, 12)
	g.body(&genFrame{f: g.comp, hasCaps: true}, 6)
	m, err := g.b.Build()
	if err != nil {
		panic(err)
	}
	return m
}

func (g *progGen) body(fr *genFrame, size int) {
	g.budget = size
	g.block(fr, 0)
	for fr.depth < 1 {
		g.push(fr)
	}
	switch g.n(12) {
	case 0:
		fr.f.Op(bc.OpHalt)
	case 1:
		fr.f.Fail(bc.FailCode(1 + g.n(15)))
	default:
		fr.f.Op(bc.OpRet)
	}
}

func (g *progGen) push(fr *genFrame) {
	f := fr.f
	switch g.n(9) {
	case 0:
		f.Int(int64(g.n(7)) - 2)
	case 1:
		f.String([]string{"a", "b.com", "", "x,y", "10.0.0.1"}[g.n(5)])
	case 2:
		f.Float(float64(g.n(5)) / 2)
	case 3:
		f.Op([]bc.Opcode{bc.OpUnit, bc.OpTrue, bc.OpFalse, bc.OpNewList}[g.n(4)])
	case 4:
		if fr.hasCaps {
			f.Cap(0)
		} else {
			f.Int(1)
		}
	case 5:
		f.Duration(int64(g.n(3)) * 1e9)
	case 6:
		f.MakeEnum(bc.TypeOption, bc.VariantNone, 0)
	default:
		if len(fr.locals) > 0 {
			f.Load(fr.locals[g.n(len(fr.locals))])
		} else {
			f.Int(3)
		}
	}
	fr.depth++
}

var unaryOps = []bc.Opcode{bc.OpNegI, bc.OpNegF, bc.OpNot, bc.OpListLen, bc.OpTry, bc.OpDup, bc.OpPop}
var binaryOps = []bc.Opcode{bc.OpAddI, bc.OpSubI, bc.OpMulI, bc.OpDivI, bc.OpModI, bc.OpAddF, bc.OpMulF, bc.OpDivF,
	bc.OpConcat, bc.OpEq, bc.OpNe, bc.OpLt, bc.OpGe, bc.OpIndex, bc.OpListPush, bc.OpListGet}

// block emits a random statement sequence that leaves the depth it found
// plus some extra values.
func (g *progGen) block(fr *genFrame, nest int) {
	f := fr.f
	for g.budget > 0 {
		g.budget--
		switch k := g.n(22); {
		case k < 4 || fr.depth == 0:
			g.push(fr)
		case k < 6:
			op := unaryOps[g.n(len(unaryOps))]
			f.Op(op)
			switch op {
			case bc.OpDup:
				fr.depth++
			case bc.OpPop:
				fr.depth--
			}
		case k < 9 && fr.depth >= 2:
			f.Op(binaryOps[g.n(len(binaryOps))])
			fr.depth--
		case k < 10:
			l := f.Local()
			f.Store(l)
			fr.depth--
			fr.locals = append(fr.locals, l)
		case k < 11:
			ids := []bc.BuiltinID{bc.BLen, bc.BUpper, bc.BToString, bc.BParseInt, bc.BFqdnParse, bc.BReverse, bc.BSort,
				bc.BDedupe, bc.BEnumerate, bc.BHostParse, bc.BScopeAllows, bc.BFlatten, bc.BIntToFloat, bc.BMapKeys}
			ids2 := []bc.BuiltinID{bc.BContains, bc.BSplit, bc.BJoin, bc.BTake, bc.BSkip, bc.BConcat, bc.BZip, bc.BRange,
				bc.BMapGet, bc.BListGetOpt, bc.BSortByKeys, bc.BGroupByKeys, bc.BListAppend}
			if fr.depth >= 2 && g.n(2) == 0 {
				f.Builtin(ids2[g.n(len(ids2))])
				fr.depth--
			} else {
				f.Builtin(ids[g.n(len(ids))])
			}
		case k < 12 && fr.depth >= 2:
			switch g.n(4) {
			case 0:
				f.Emit(bc.OpMakeList, 2)
			case 1:
				f.Emit(bc.OpMakeMap, 1)
			case 2:
				f.MakeRec(bc.TypeTuple, 2)
			default:
				f.MakeRec(bc.FirstUserType, 2)
			}
			fr.depth--
		case k < 13:
			f.Emit(bc.OpInterp, 1)
			if g.n(2) == 0 {
				f.Emit(bc.OpGetField, int64(g.n(2)))
			}
		case k < 14:
			switch g.n(4) {
			case 0:
				f.Effect(bc.EffectNow, 0)
				fr.depth++
			case 1:
				f.ToolCall("httpx", 1)
			case 2:
				f.Effect(bc.EffectScopeCheck, 1)
			default:
				f.MakeEnum(bc.TypeOption, bc.VariantSome, 1)
				f.Emit(bc.OpTestVariant, int64(g.n(2)))
				f.Op(bc.OpPop)
				u := g.n(2) // UNPACK u leaves u values in place of the enum
				f.Emit(bc.OpUnpack, int64(u))
				fr.depth += u - 1
			}
		case k < 15:
			f.Call(g.helpers[g.n(len(g.helpers))].Index(), 1)
		case k < 16:
			f.Closure(g.lambda.Index(), 1)
			g.push(fr)
			f.CallClosure(1)
			fr.depth--
		case k < 17:
			f.PushComp(g.comp.Index(), 1)
			fr.depth--
		case k < 19 && nest < 3:
			// if/else with equal depth on both arms
			els, end := f.NewLabel(), f.NewLabel()
			f.JumpIfFalse(els)
			fr.depth--
			base := fr.depth
			saved := append([]uint16(nil), fr.locals...)
			g.arm(fr, base, nest)
			f.Jump(end)
			f.Bind(els)
			fr.locals = append([]uint16(nil), saved...)
			fr.depth = base
			g.arm(fr, base, nest)
			fr.locals = saved
			f.Bind(end)
		case k < 20 && nest < 2:
			g.loop(fr, nest)
		default:
			g.push(fr)
		}
	}
}

// arm emits a sub-block that ends exactly one value above base.
func (g *progGen) arm(fr *genFrame, base, nest int) {
	save := g.budget
	g.budget = 1 + g.n(4)
	g.block(fr, nest+1)
	g.budget = save
	for fr.depth > base+1 {
		fr.f.Op(bc.OpPop)
		fr.depth--
	}
	for fr.depth < base+1 {
		g.push(fr)
	}
}

// loop emits `for i in 0..k { body }` with the counter on the stack.
func (g *progGen) loop(fr *genFrame, nest int) {
	f := fr.f
	i := f.Local()
	f.Int(0)
	head := f.Here()
	f.Store(i)
	f.Load(i)
	f.Int(int64(1 + g.n(4)))
	f.Op(bc.OpLt)
	exit := f.NewLabel()
	f.JumpIfFalse(exit)
	base := fr.depth
	saved := append([]uint16(nil), fr.locals...)
	fr.locals = append(fr.locals, i)
	save := g.budget
	g.budget = 1 + g.n(5)
	g.block(fr, nest+1)
	g.budget = save
	for fr.depth > base {
		f.Op(bc.OpPop)
		fr.depth--
	}
	for fr.depth < base {
		g.push(fr)
	}
	f.Load(i)
	f.Int(1)
	f.Op(bc.OpAddI)
	f.Loop(head)
	f.Bind(exit)
	fr.locals = saved
}

// genHandler answers effects deterministically from the path.
func genHandler(s vm.Suspended) vm.EffectResult {
	h := sha256.Sum256([]byte(s.Path.String()))
	switch h[0] % 7 {
	case 0:
		return vm.OkResult(vm.Int(int64(h[1])))
	case 1:
		return vm.OkResult(vm.Str("r"))
	case 2:
		return vm.OkResult(vm.Some(vm.Int(1)))
	case 3:
		return vm.OkResult(vm.None())
	case 4:
		return vm.OkResult(vm.Ok(vm.ScopeProof(vm.Str("t"), "d")))
	case 5:
		return vm.FailResult(bc.FailEffectFailed, "scripted")
	}
	return vm.OkResult(vm.List(vm.Int(1), vm.Float(math.Pi)))
}
