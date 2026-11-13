package vm_test

import (
	"math"
	"testing"
	"time"

	bc "github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/vm"
)

type exprCase struct {
	name string
	emit func(b *bc.Builder, f *bc.FuncBuilder)
	want vm.Value
	fail bc.FailCode
}

func runExprCases(t *testing.T, cases []exprCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := program(t, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
				c.emit(b, f)
				f.Op(bc.OpRet)
			})
			out := runProg(t, p)
			if c.fail != 0 {
				wantFail(t, out, c.fail)
				return
			}
			wantValue(t, out, c.want)
		})
	}
}

func i2(op bc.Opcode, a, b int64) func(*bc.Builder, *bc.FuncBuilder) {
	return func(_ *bc.Builder, f *bc.FuncBuilder) { f.Int(a); f.Int(b); f.Op(op) }
}

func f2(op bc.Opcode, a, b float64) func(*bc.Builder, *bc.FuncBuilder) {
	return func(_ *bc.Builder, f *bc.FuncBuilder) { f.Float(a); f.Float(b); f.Op(op) }
}

func builtin(id bc.BuiltinID, args ...func(f *bc.FuncBuilder)) func(*bc.Builder, *bc.FuncBuilder) {
	return func(_ *bc.Builder, f *bc.FuncBuilder) {
		for _, a := range args {
			a(f)
		}
		f.Builtin(id)
	}
}

func s(v string) func(f *bc.FuncBuilder) { return func(f *bc.FuncBuilder) { f.String(v) } }
func n(v int64) func(f *bc.FuncBuilder)  { return func(f *bc.FuncBuilder) { f.Int(v) } }
func list(xs ...int64) func(f *bc.FuncBuilder) {
	return func(f *bc.FuncBuilder) {
		for _, x := range xs {
			f.Int(x)
		}
		f.Emit(bc.OpMakeList, int64(len(xs)))
	}
}

func mustV(v vm.Value, err error) vm.Value {
	if err != nil {
		panic(err)
	}
	return v
}

// shortCircuitAnd emits a && b.
func shortCircuitAnd(f *bc.FuncBuilder, a, b func()) {
	no, end := f.NewLabel(), f.NewLabel()
	a()
	f.JumpIfFalse(no)
	b()
	f.Jump(end)
	f.Bind(no)
	f.Op(bc.OpFalse)
	f.Bind(end)
}

// shortCircuitOr emits a || b.
func shortCircuitOr(f *bc.FuncBuilder, a, b func()) {
	evalB, end := f.NewLabel(), f.NewLabel()
	a()
	f.JumpIfFalse(evalB)
	f.Op(bc.OpTrue)
	f.Jump(end)
	f.Bind(evalB)
	b()
	f.Bind(end)
}

func TestVM001_Arithmetic(t *testing.T) {
	divByZeroIsZero := func(f *bc.FuncBuilder) func() {
		return func() { f.Int(1); f.Int(0); f.Op(bc.OpDivI); f.Int(0); f.Op(bc.OpEq) }
	}
	t.Run("int", func(t *testing.T) {
		runExprCases(t, []exprCase{
			{"add_mul", func(_ *bc.Builder, f *bc.FuncBuilder) { f.Int(2); f.Int(3); f.Int(4); f.Op(bc.OpMulI, bc.OpAddI) }, vm.Int(14), 0},
			{"sub", i2(bc.OpSubI, 3, 10), vm.Int(-7), 0},
			{"div_truncates", i2(bc.OpDivI, -7, 2), vm.Int(-3), 0},
			{"mod_sign", i2(bc.OpModI, -7, 3), vm.Int(-1), 0},
			{"neg", func(_ *bc.Builder, f *bc.FuncBuilder) { f.Int(5); f.Op(bc.OpNegI) }, vm.Int(-5), 0},
			{"add_overflow", i2(bc.OpAddI, math.MaxInt64, 1), vm.Value{}, bc.FailArithOverflow},
			{"sub_overflow", i2(bc.OpSubI, math.MinInt64, 1), vm.Value{}, bc.FailArithOverflow},
			{"mul_overflow", i2(bc.OpMulI, math.MinInt64, -1), vm.Value{}, bc.FailArithOverflow},
			{"mul_big_ok", i2(bc.OpMulI, 1<<31, 1<<31), vm.Int(1 << 62), 0},
			{"mul_min_ok", i2(bc.OpMulI, math.MinInt64/2, 2), vm.Int(math.MinInt64), 0},
			{"div_overflow", i2(bc.OpDivI, math.MinInt64, -1), vm.Value{}, bc.FailArithOverflow},
			{"mod_min_neg1", i2(bc.OpModI, math.MinInt64, -1), vm.Int(0), 0},
			{"neg_overflow", func(_ *bc.Builder, f *bc.FuncBuilder) { f.Int(math.MinInt64); f.Op(bc.OpNegI) }, vm.Value{}, bc.FailArithOverflow},
			{"div_zero", i2(bc.OpDivI, 1, 0), vm.Value{}, bc.FailDivZero},
			{"mod_zero", i2(bc.OpModI, 1, 0), vm.Value{}, bc.FailDivZero},
			{"type_mismatch", func(_ *bc.Builder, f *bc.FuncBuilder) { f.Int(1); f.String("x"); f.Op(bc.OpAddI) }, vm.Value{}, bc.FailTypeMismatch},
		})
	})
	t.Run("float", func(t *testing.T) {
		runExprCases(t, []exprCase{
			{"mul", f2(bc.OpMulF, 1.5, 2), vm.Float(3), 0},
			{"div", f2(bc.OpDivF, 1, 4), vm.Float(0.25), 0},
			{"neg", func(_ *bc.Builder, f *bc.FuncBuilder) { f.Float(2); f.Op(bc.OpNegF) }, vm.Float(-2), 0},
			{"overflow_to_inf", f2(bc.OpMulF, 1e308, 10), vm.Value{}, bc.FailArithOverflow},
			{"div_zero", f2(bc.OpDivF, 1, 0), vm.Value{}, bc.FailDivZero},
			{"int_to_float", builtin(bc.BIntToFloat, n(3)), vm.Float(3), 0},
			{"float_to_int", func(_ *bc.Builder, f *bc.FuncBuilder) { f.Float(-3.9); f.Builtin(bc.BFloatToInt) }, vm.Int(-3), 0},
			{"float_to_int_overflow", func(_ *bc.Builder, f *bc.FuncBuilder) { f.Float(1e19); f.Builtin(bc.BFloatToInt) }, vm.Value{}, bc.FailArithOverflow},
		})
	})
	t.Run("duration_time", func(t *testing.T) {
		sec := int64(time.Second)
		runExprCases(t, []exprCase{
			{"dur_add", func(_ *bc.Builder, f *bc.FuncBuilder) { f.Duration(sec); f.Duration(2 * sec); f.Op(bc.OpAddI) }, vm.Duration(3 * sec), 0},
			{"dur_mul", func(_ *bc.Builder, f *bc.FuncBuilder) { f.Duration(sec); f.Int(3); f.Op(bc.OpMulI) }, vm.Duration(3 * sec), 0},
			{"time_plus_dur", func(_ *bc.Builder, f *bc.FuncBuilder) {
				f.ConstOp(bc.TimeConst(100))
				f.Duration(sec)
				f.Op(bc.OpAddI)
			}, vm.Time(100 + sec), 0},
			{"time_minus_time", func(_ *bc.Builder, f *bc.FuncBuilder) {
				f.ConstOp(bc.TimeConst(5 * sec))
				f.ConstOp(bc.TimeConst(2 * sec))
				f.Op(bc.OpSubI)
			}, vm.Duration(3 * sec), 0},
			{"time_plus_time", func(_ *bc.Builder, f *bc.FuncBuilder) {
				f.ConstOp(bc.TimeConst(5))
				f.ConstOp(bc.TimeConst(2))
				f.Op(bc.OpAddI)
			}, vm.Value{}, bc.FailTypeMismatch},
			{"interp_duration", func(_ *bc.Builder, f *bc.FuncBuilder) { f.Duration(90 * sec); f.Emit(bc.OpInterp, 1) }, vm.Str("1m30s"), 0},
		})
	})
	t.Run("comparison", func(t *testing.T) {
		runExprCases(t, []exprCase{
			{"lt", i2(bc.OpLt, 3, 5), vm.Bool(true), 0},
			{"ge", i2(bc.OpGe, 3, 5), vm.Bool(false), 0},
			{"le_eq", i2(bc.OpLe, 5, 5), vm.Bool(true), 0},
			{"gt_float", f2(bc.OpGt, 2.5, 1), vm.Bool(true), 0},
			{"string_lt", func(_ *bc.Builder, f *bc.FuncBuilder) { f.String("abc"); f.String("abd"); f.Op(bc.OpLt) }, vm.Bool(true), 0},
			{"list_eq", func(_ *bc.Builder, f *bc.FuncBuilder) { list(1, 2)(f); list(1, 2)(f); f.Op(bc.OpEq) }, vm.Bool(true), 0},
			{"list_ne", func(_ *bc.Builder, f *bc.FuncBuilder) { list(1, 2)(f); list(1, 3)(f); f.Op(bc.OpNe) }, vm.Bool(true), 0},
			{"list_lt", func(_ *bc.Builder, f *bc.FuncBuilder) { list(1, 2)(f); list(1, 2, 0)(f); f.Op(bc.OpLt) }, vm.Bool(true), 0},
			{"float_zero_eq", f2(bc.OpEq, 0, math.Copysign(0, -1)), vm.Bool(true), 0},
			{"mixed_kinds_eq", func(_ *bc.Builder, f *bc.FuncBuilder) { f.Int(1); f.Float(1); f.Op(bc.OpEq) }, vm.Bool(false), 0},
			{"mixed_kinds_lt", func(_ *bc.Builder, f *bc.FuncBuilder) { f.Int(1); f.Float(1); f.Op(bc.OpLt) }, vm.Value{}, bc.FailTypeMismatch},
			{"not", func(_ *bc.Builder, f *bc.FuncBuilder) { f.Op(bc.OpTrue, bc.OpNot) }, vm.Bool(false), 0},
		})
	})
	t.Run("short_circuit", func(t *testing.T) {
		runExprCases(t, []exprCase{
			{"and_skips_rhs", func(_ *bc.Builder, f *bc.FuncBuilder) {
				shortCircuitAnd(f, func() { f.Op(bc.OpFalse) }, divByZeroIsZero(f))
			}, vm.Bool(false), 0},
			{"and_evaluates_rhs", func(_ *bc.Builder, f *bc.FuncBuilder) {
				shortCircuitAnd(f, func() { f.Op(bc.OpTrue) }, divByZeroIsZero(f))
			}, vm.Value{}, bc.FailDivZero},
			{"or_skips_rhs", func(_ *bc.Builder, f *bc.FuncBuilder) {
				shortCircuitOr(f, func() { f.Op(bc.OpTrue) }, divByZeroIsZero(f))
			}, vm.Bool(true), 0},
			{"or_rhs", func(_ *bc.Builder, f *bc.FuncBuilder) {
				shortCircuitOr(f, func() { f.Op(bc.OpFalse) }, func() { f.Op(bc.OpTrue) })
			}, vm.Bool(true), 0},
		})
	})
	t.Run("strings", func(t *testing.T) {
		runExprCases(t, []exprCase{
			{"concat", func(_ *bc.Builder, f *bc.FuncBuilder) { f.String("foo"); f.String("bar"); f.Op(bc.OpConcat) }, vm.Str("foobar"), 0},
			{"interp", func(_ *bc.Builder, f *bc.FuncBuilder) {
				f.String("x=")
				f.Int(5)
				f.String(" ok=")
				f.Op(bc.OpTrue)
				f.String(" f=")
				f.Float(2)
				f.Emit(bc.OpInterp, 6)
			}, vm.Str("x=5 ok=true f=2.0"), 0},
			{"interp_list", func(_ *bc.Builder, f *bc.FuncBuilder) {
				f.Int(1)
				f.String("a")
				f.Emit(bc.OpMakeList, 2)
				f.Emit(bc.OpInterp, 1)
			}, vm.Str(`[1, "a"]`), 0},
			{"len_unicode", builtin(bc.BLen, s("héllo")), vm.Int(5), 0},
			{"upper", builtin(bc.BUpper, s("straße")), vm.Str("STRAßE"), 0},
			{"lower", builtin(bc.BLower, s("ÄBC")), vm.Str("äbc"), 0},
			{"trim", builtin(bc.BTrim, s("  a b \n")), vm.Str("a b"), 0},
			{"split", builtin(bc.BSplit, s("a,b,,c"), s(",")), strs("a", "b", "", "c"), 0},
			{"join", func(_ *bc.Builder, f *bc.FuncBuilder) {
				f.String("a")
				f.String("b")
				f.Emit(bc.OpMakeList, 2)
				f.String("-")
				f.Builtin(bc.BJoin)
			}, vm.Str("a-b"), 0},
			{"replace", builtin(bc.BReplace, s("a.b.c"), s("."), s("::")), vm.Str("a::b::c"), 0},
			{"contains", builtin(bc.BContains, s("hello"), s("ell")), vm.Bool(true), 0},
			{"starts_with", builtin(bc.BStartsWith, s("hello"), s("he")), vm.Bool(true), 0},
			{"ends_with", builtin(bc.BEndsWith, s("hello"), s("lo")), vm.Bool(true), 0},
			{"parse_int", builtin(bc.BParseInt, s("-42")), vm.Some(vm.Int(-42)), 0},
			{"parse_int_bad", builtin(bc.BParseInt, s("4x")), vm.None(), 0},
			{"parse_float", builtin(bc.BParseFloat, s("2.5")), vm.Some(vm.Float(2.5)), 0},
			{"parse_float_inf", builtin(bc.BParseFloat, s("inf")), vm.None(), 0},
			{"to_string", builtin(bc.BToString, n(7)), vm.Str("7"), 0},
			{"fqdn_parse", builtin(bc.BFqdnParse, s("WWW.Example.COM.")), vm.Some(mustV(vm.Fqdn("www.example.com"))), 0},
			{"fqdn_parse_bad", builtin(bc.BFqdnParse, s("a..b")), vm.None(), 0},
			{"ip_parse_mapped", builtin(bc.BIPParse, s("::ffff:1.2.3.4")), vm.Some(mustV(vm.IPAddress("1.2.3.4"))), 0},
			{"cidr_parse", builtin(bc.BCIDRParse, s("10.1.2.3/8")), vm.Some(mustV(vm.Netblock("10.0.0.0/8"))), 0},
			{"url_parse", builtin(bc.BURLParse, s("HTTPS://A.com:443/x/../y#f")), vm.Some(mustV(vm.URL("https://a.com/y"))), 0},
			{"asn_parse", builtin(bc.BASNParse, s("as64500")), vm.Some(mustV(vm.Asn("AS64500"))), 0},
			{"host_parse", builtin(bc.BHostParse, s("10.0.0.1")), vm.Some(mustV(vm.Host("10.0.0.1"))), 0},
			{"scope_allows_without_snapshot", builtin(bc.BScopeAllows, s("a.com")), vm.Bool(false), 0},
			{"base64_encode", builtin(bc.BBase64Encode, s("héllo")), vm.Str("aMOpbGxv"), 0},
			{"base64_decode_lenient", builtin(bc.BBase64Decode, s("aMOp\nbGxv")), vm.Some(vm.Str("héllo")), 0},
			{"base64_decode_url_unpadded", builtin(bc.BBase64Decode, s("Pz8-")), vm.Some(vm.Str("??>")), 0},
			{"base64_decode_bad", builtin(bc.BBase64Decode, s("Zm9vY")), vm.None(), 0},
			{"base64_decode_not_utf8", builtin(bc.BBase64Decode, s("/w==")), vm.None(), 0},
			{"url_encode", builtin(bc.BURLEncode, s("a b/c?d=é&e~*")), vm.Str("a%20b%2Fc%3Fd%3D%C3%A9%26e~*"), 0},
			{"url_decode", builtin(bc.BURLDecode, s("a%20b+%2f%C3%A9")), vm.Some(vm.Str("a b+/é")), 0},
			{"url_decode_bad_escape", builtin(bc.BURLDecode, s("100%")), vm.None(), 0},
			{"url_decode_not_utf8", builtin(bc.BURLDecode, s("%FF")), vm.None(), 0},
			{"hex_encode", builtin(bc.BHexEncode, s("Hi!")), vm.Str("486921"), 0},
			{"hex_decode", builtin(bc.BHexDecode, s("48 69 21")), vm.Some(vm.Str("Hi!")), 0},
			{"hex_decode_odd", builtin(bc.BHexDecode, s("486")), vm.None(), 0},
		})
	})
	t.Run("lists", func(t *testing.T) {
		runExprCases(t, []exprCase{
			{"make_list_len", func(_ *bc.Builder, f *bc.FuncBuilder) { list(1, 2, 3)(f); f.Op(bc.OpListLen) }, vm.Int(3), 0},
			{"index", func(_ *bc.Builder, f *bc.FuncBuilder) { list(1, 2, 3)(f); f.Int(1); f.Op(bc.OpIndex) }, vm.Int(2), 0},
			{"index_oob", func(_ *bc.Builder, f *bc.FuncBuilder) { list(1)(f); f.Int(1); f.Op(bc.OpIndex) }, vm.Value{}, bc.FailIndexOOB},
			{"list_get_negative", func(_ *bc.Builder, f *bc.FuncBuilder) { list(1)(f); f.Int(-1); f.Op(bc.OpListGet) }, vm.Value{}, bc.FailIndexOOB},
			{"push", func(_ *bc.Builder, f *bc.FuncBuilder) {
				f.Op(bc.OpNewList)
				for i := int64(1); i <= 3; i++ {
					f.Int(i)
					f.Op(bc.OpListPush)
				}
			}, ints(1, 2, 3), 0},
			{"push_shared_prefix_copies", func(_ *bc.Builder, f *bc.FuncBuilder) {
				base := f.Local()
				list(1)(f)
				f.Store(base)
				f.Load(base)
				f.Int(2)
				f.Op(bc.OpListPush, bc.OpPop)
				f.Load(base)
				f.Int(3)
				f.Op(bc.OpListPush)
			}, ints(1, 3), 0},
			{"map_index", func(_ *bc.Builder, f *bc.FuncBuilder) {
				f.String("a")
				f.Int(1)
				f.String("b")
				f.Int(2)
				f.Emit(bc.OpMakeMap, 2)
				f.String("b")
				f.Op(bc.OpIndex)
			}, vm.Int(2), 0},
			{"map_get_missing", func(_ *bc.Builder, f *bc.FuncBuilder) {
				f.String("a")
				f.Int(1)
				f.Emit(bc.OpMakeMap, 1)
				f.String("z")
				f.Builtin(bc.BMapGet)
			}, vm.None(), 0},
			{"map_insert_keys", func(_ *bc.Builder, f *bc.FuncBuilder) {
				f.String("b")
				f.Int(1)
				f.Emit(bc.OpMakeMap, 1)
				f.String("a")
				f.Int(2)
				f.Builtin(bc.BMapInsert)
				f.Builtin(bc.BMapKeys)
			}, strs("b", "a"), 0},
			{"sort", builtin(bc.BSort, list(3, 1, 2)), ints(1, 2, 3), 0},
			{"sort_by_keys_stable", builtin(bc.BSortByKeys, list(2, 1, 2, 1), list(10, 20, 30, 40)), ints(20, 40, 10, 30), 0},
			{"dedupe", builtin(bc.BDedupe, list(3, 1, 3, 2, 1)), ints(3, 1, 2), 0},
			{"reverse", builtin(bc.BReverse, list(1, 2, 3)), ints(3, 2, 1), 0},
			{"range", builtin(bc.BRange, n(2), n(5)), ints(2, 3, 4), 0},
			{"range_empty", builtin(bc.BRange, n(5), n(2)), ints(), 0},
			{"take", builtin(bc.BTake, list(1, 2, 3), n(2)), ints(1, 2), 0},
			{"skip", builtin(bc.BSkip, list(1, 2, 3), n(2)), ints(3), 0},
			{"concat", builtin(bc.BConcat, list(1), list(2, 3)), ints(1, 2, 3), 0},
			{"zip", builtin(bc.BZip, list(1, 2), list(3)), vm.List(vm.Tuple(vm.Int(1), vm.Int(3))), 0},
			{"enumerate", builtin(bc.BEnumerate, list(7)), vm.List(vm.Tuple(vm.Int(0), vm.Int(7))), 0},
			{"flatten", func(_ *bc.Builder, f *bc.FuncBuilder) {
				list(1)(f)
				list(2, 3)(f)
				f.Emit(bc.OpMakeList, 2)
				f.Builtin(bc.BFlatten)
			}, ints(1, 2, 3), 0},
			{"contains_list", builtin(bc.BContains, list(1, 2), n(2)), vm.Bool(true), 0},
			{"list_get_opt", builtin(bc.BListGetOpt, list(5), n(0)), vm.Some(vm.Int(5)), 0},
			{"group_by_keys", builtin(bc.BGroupByKeys, list(1, 0, 1), list(10, 20, 30)),
				mustV(vm.Map([]vm.Value{vm.Int(1), vm.Int(0)}, []vm.Value{ints(10, 30), ints(20)})), 0},
			{"map_filter_loop", func(_ *bc.Builder, f *bc.FuncBuilder) {
				f.Int(0)
				f.Int(10)
				f.Builtin(bc.BRange)
				emitMapFilter(f)
			}, ints(0, 6, 12, 18), 0},
		})
	})
}

func TestVM002_ClosuresRecursionMatch(t *testing.T) {
	t.Run("closure_captures_by_value", func(t *testing.T) {
		b := newBuilder()
		adder := b.Func("adder", 1, 1)
		adder.Cap(0)
		adder.Load(0)
		adder.Op(bc.OpAddI, bc.OpRet)
		f := b.Func("main", 0, 0)
		b.SetEntry(f.Index())
		k := f.Local()
		f.Int(10)
		f.Store(k)
		f.Load(k)
		f.Closure(adder.Index(), 1)
		f.Int(5)
		f.CallClosure(1)
		f.Op(bc.OpRet)
		wantValue(t, runProg(t, build(t, b)), vm.Int(15))
	})
	t.Run("recursion_fib", func(t *testing.T) {
		b := newBuilder()
		fib := addFib(b)
		f := b.Func("main", 1, 0)
		b.SetEntry(f.Index())
		f.Load(0)
		f.Call(fib, 1)
		f.Op(bc.OpRet)
		wantValue(t, runProg(t, build(t, b), vm.Int(20)), vm.Int(6765))
	})
	t.Run("call_depth_limit", func(t *testing.T) {
		b := newBuilder()
		f := b.Func("main", 0, 0)
		b.SetEntry(f.Index())
		f.Call(f.Index(), 0)
		f.Op(bc.OpRet)
		wantFail(t, runProg(t, build(t, b)), bc.FailLimitExceeded)
	})
	t.Run("pattern_match", func(t *testing.T) {
		// fn classify(o: Option<Int>) -> String {
		//   match o { Some(x) if x > 10 => "big", Some(_) => "small", None => "none" } }
		b := newBuilder()
		f := b.Func("classify", 1, 0)
		b.SetEntry(f.Index())
		x1, x2 := f.Local(), f.Local()
		end, fail1, arm2, fail2, fail3 := f.NewLabel(), f.NewLabel(), f.NewLabel(), f.NewLabel(), f.NewLabel()
		f.Load(0)
		f.Emit(bc.OpTestVariant, int64(bc.VariantSome))
		f.JumpIfFalse(fail1)
		f.Emit(bc.OpUnpack, 1)
		f.Store(x1)
		f.Load(x1)
		f.Int(10)
		f.Op(bc.OpGt)
		f.JumpIfFalse(arm2) // guard failed
		f.String("big")
		f.Jump(end)
		f.Bind(fail1)
		f.Op(bc.OpPop)
		f.Bind(arm2)
		f.Load(0)
		f.Emit(bc.OpTestVariant, int64(bc.VariantSome))
		f.JumpIfFalse(fail2)
		f.Emit(bc.OpUnpack, 1)
		f.Store(x2)
		f.String("small")
		f.Jump(end)
		f.Bind(fail2)
		f.Op(bc.OpPop)
		f.Load(0)
		f.Emit(bc.OpTestVariant, int64(bc.VariantNone))
		f.JumpIfFalse(fail3)
		f.Emit(bc.OpUnpack, 0)
		f.String("none")
		f.Jump(end)
		f.Bind(fail3)
		f.Op(bc.OpPop)
		f.Fail(bc.FailMatchFailed)
		f.Bind(end)
		f.Op(bc.OpRet)
		p := build(t, b)
		for _, c := range []struct {
			in   vm.Value
			want string
		}{{vm.Some(vm.Int(11)), "big"}, {vm.Some(vm.Int(3)), "small"}, {vm.None(), "none"}} {
			wantValue(t, runProg(t, p, c.in), vm.Str(c.want))
		}
	})
	t.Run("records", func(t *testing.T) {
		b := newBuilder()
		r := b.Record("Report", bc.Field{Name: "a", Type: "Int"}, bc.Field{Name: "b", Type: "String"})
		f := b.Func("main", 0, 0)
		b.SetEntry(f.Index())
		f.Int(1)
		f.String("x")
		f.MakeRec(r, 2)
		f.Emit(bc.OpGetField, 1)
		f.Op(bc.OpRet)
		wantValue(t, runProg(t, build(t, b)), vm.Str("x"))
	})

	// fn double_first(xs: List<Int>) -> Option<Int> { Some(first(xs)? * 2) }
	tryProgram := func(t *testing.T) *bc.Verified {
		b := newBuilder()
		g := b.Func("double_first", 1, 0)
		g.Load(0)
		g.Int(0)
		g.Builtin(bc.BListGetOpt)
		g.Op(bc.OpTry)
		g.Int(2)
		g.Op(bc.OpMulI)
		g.MakeEnum(bc.TypeOption, bc.VariantSome, 1)
		g.Op(bc.OpRet)
		f := b.Func("main", 1, 0)
		b.SetEntry(f.Index())
		f.Load(0)
		f.Call(g.Index(), 1)
		f.Op(bc.OpRet)
		return build(t, b)
	}
	t.Run("try_early_return", func(t *testing.T) {
		p := tryProgram(t)
		wantValue(t, runProg(t, p, ints(4)), vm.Some(vm.Int(8)))
		wantValue(t, runProg(t, p, ints()), vm.None())
	})
	t.Run("try_in_workflow_body", func(t *testing.T) {
		p := program(t, 1, func(b *bc.Builder, f *bc.FuncBuilder) {
			f.Load(0)
			f.Op(bc.OpTry, bc.OpRet)
		})
		wantValue(t, runProg(t, p, vm.Ok(vm.Int(1))), vm.Int(1))
		wantFail(t, runProg(t, p, vm.None()), bc.FailUnwrapFailed)
		wantFail(t, runProg(t, p, vm.Err(vm.Str("e"))), bc.FailUnwrapFailed)
		denied := vm.Record(bc.TypeDenied, vm.Str("ap1"), vm.Str("bob"), vm.Str("no"))
		wantFail(t, runProg(t, p, vm.Err(denied)), bc.FailApprovalDenied)
	})
	t.Run("require_approval", func(t *testing.T) {
		// require approval(role, reason, timeout) lowers to EFFECT approval; TRY.
		p := program(t, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			f.String("finance_ap")
			f.String("post")
			f.Duration(int64(24 * time.Hour))
			f.Effect(bc.EffectApproval, 3)
			f.Op(bc.OpTry, bc.OpPop)
			f.String("posted")
			f.Op(bc.OpRet)
		})
		approve := func(ok bool) vm.Handler {
			return func(s vm.Suspended) vm.EffectResult {
				if ok {
					return vm.OkResult(vm.Ok(vm.Record(bc.TypeApproval, vm.Str("ap1"), vm.Str("finance"), vm.Str("alice"), vm.Time(0))))
				}
				return vm.OkResult(vm.Err(vm.Record(bc.TypeDenied, vm.Str("ap1"), vm.Str("alice"), vm.Str("no"))))
			}
		}
		out, _, err := vm.RunWithEffects(p, vm.Config{}, nil, approve(true))
		if err != nil {
			t.Fatal(err)
		}
		wantValue(t, out, vm.Str("posted"))
		out, _, _ = vm.RunWithEffects(p, vm.Config{}, nil, approve(false))
		wantFail(t, out, bc.FailApprovalDenied)
	})
}
