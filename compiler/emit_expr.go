package compiler

import (
	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/compiler/internal/abi"
	"github.com/genuinebnt/flowforge/dsl"
)

// expr emits code that pushes exactly one value: the value of e.
func (x *fnCtx) expr(e dsl.Expr) {
	if e == nil {
		x.c.errorf(nil, "missing expression")
		x.fb.Op(bytecode.OpUnit)
		return
	}
	x.span(e)
	switch e := e.(type) {
	case *dsl.IntLit:
		x.fb.Int(e.Value)
	case *dsl.FloatLit:
		x.fb.Float(e.Value)
	case *dsl.DurationLit:
		x.fb.Duration(e.Nanos)
	case *dsl.BoolLit:
		x.boolConst(e.Value)
	case *dsl.StringLit:
		x.stringLit(e)
	case *dsl.Ident:
		x.ref(e.Ref, e)
	case *dsl.PathExpr:
		x.ref(e.Ref, e)
	case *dsl.TupleExpr:
		x.tuple(e)
	case *dsl.ListExpr:
		x.list(e)
	case *dsl.RecordLit:
		x.recordLit(e)
	case *dsl.Lambda:
		x.pushClosure(e)
	case *dsl.IfExpr:
		x.ifExpr(e)
	case *dsl.MatchExpr:
		x.match(e)
	case *dsl.AgentExpr:
		x.agent(e)
	case *dsl.BlockExpr:
		x.block(e)
	case *dsl.UnaryExpr:
		x.unary(e)
	case *dsl.BinaryExpr:
		x.binary(e)
	case *dsl.CallExpr:
		x.call(e)
	case *dsl.IndexExpr:
		x.expr(e.X)
		x.expr(e.Index)
		x.span(e)
		x.fb.Op(bytecode.OpIndex)
	case *dsl.FieldExpr:
		x.field(e)
	case *dsl.TryExpr:
		x.try(e)
	case *dsl.CastExpr:
		x.cast(e)
	default:
		x.c.errorf(e, "cannot compile expression %T", e)
		x.fb.Op(bytecode.OpUnit)
	}
}

func (x *fnCtx) boolConst(v bool) {
	if v {
		x.fb.Op(bytecode.OpTrue)
	} else {
		x.fb.Op(bytecode.OpFalse)
	}
}

// ref emits the value a name refers to.
func (x *fnCtx) ref(r dsl.Ref, n dsl.Node) {
	switch r.Kind {
	case dsl.RefLocal, dsl.RefCapture:
		x.loadBinding(r.Binding, n)
	case dsl.RefFn, dsl.RefTool, dsl.RefBuiltin, dsl.RefVariant:
		x.itemValue(r, n)
	default:
		x.c.errorf(n, "unresolved name")
		x.fb.Op(bytecode.OpUnit)
	}
}

// stringLit emits a plain string as a constant and an interpolated one as
// its pieces followed by INTERP (spec 8.5).
func (x *fnCtx) stringLit(s *dsl.StringLit) {
	if !s.Interpolated() {
		text := ""
		for _, p := range s.Parts {
			text += p.Lit
		}
		x.fb.String(text)
		return
	}
	pending := 0 // values pushed for the current INTERP
	groups := 0  // INTERP results on the stack
	flush := func() {
		if pending == 0 {
			return
		}
		x.span(s)
		x.fb.Emit(bytecode.OpInterp, int64(pending))
		pending = 0
		groups++
		if groups == 2 {
			x.fb.Op(bytecode.OpConcat)
			groups = 1
		}
	}
	for _, p := range s.Parts {
		if p.X == nil {
			if p.Lit == "" {
				continue
			}
			x.fb.String(p.Lit)
		} else {
			x.expr(p.X)
		}
		pending++
		if pending == 255 {
			flush()
		}
	}
	flush()
	if groups == 0 {
		x.fb.String("")
	}
}

func (x *fnCtx) tuple(e *dsl.TupleExpr) {
	if len(e.Elems) == 0 {
		x.fb.Op(bytecode.OpUnit)
		return
	}
	if len(e.Elems) > 255 {
		x.c.limitf(e, "a tuple has more than 255 elements")
	}
	for _, el := range e.Elems {
		x.expr(el)
	}
	x.span(e)
	x.fb.MakeRec(bytecode.TypeTuple, uint8(min(len(e.Elems), 255)))
}

func (x *fnCtx) list(e *dsl.ListExpr) {
	if len(e.Elems) <= 65535 {
		for _, el := range e.Elems {
			x.expr(el)
		}
		x.span(e)
		x.fb.Emit(bytecode.OpMakeList, int64(len(e.Elems)))
		return
	}
	x.fb.Op(bytecode.OpNewList)
	for _, el := range e.Elems {
		x.expr(el)
		x.fb.Op(bytecode.OpListPush)
	}
}

// recordLit evaluates the fields in source order and builds the record
// with its fields in declaration order (spec 8.2).
func (x *fnCtx) recordLit(e *dsl.RecordLit) {
	id, ok := x.c.types.ID(e.Def)
	if !ok || e.Def == nil {
		x.c.errorf(e, "record type without an id")
		x.fb.Op(bytecode.OpUnit)
		return
	}
	n := len(e.Def.Fields)
	if n > 255 {
		x.c.limitf(e, "record `%s` has more than 255 fields", e.Def.Name)
		x.fb.Op(bytecode.OpUnit)
		return
	}
	inOrder := len(e.Fields) == n
	for i, f := range e.Fields {
		if f.Index != i {
			inOrder = false
		}
	}
	if inOrder {
		for _, f := range e.Fields {
			x.expr(f.Value)
		}
	} else {
		slots := make([]int, n)
		for i := range slots {
			slots[i] = -1
		}
		for _, f := range e.Fields {
			x.expr(f.Value)
			if f.Index < 0 || f.Index >= n || slots[f.Index] >= 0 {
				x.c.errorf(f.Value, "bad record field index")
				x.fb.Op(bytecode.OpPop)
				continue
			}
			slots[f.Index] = int(x.storeTemp())
		}
		for i, s := range slots {
			if s < 0 {
				x.c.errorf(e, "record field %d missing", i)
				x.fb.Op(bytecode.OpUnit)
				continue
			}
			x.fb.Load(uint16(s))
		}
	}
	x.span(e)
	x.fb.MakeRec(id, uint8(n))
}

func (x *fnCtx) field(e *dsl.FieldExpr) {
	if e.Builtin != nil {
		x.c.errorf(e, "scope.%s used as a value", e.Name.Name)
		x.fb.Op(bytecode.OpUnit)
		return
	}
	x.expr(e.X)
	if e.Index < 0 || e.Index > 255 {
		x.c.errorf(e, "bad field index")
		return
	}
	x.span(e)
	x.fb.Emit(bytecode.OpGetField, int64(e.Index))
}

// makeVariant emits MAKE_ENUM for v with its payload already on the stack.
func (x *fnCtx) makeVariant(v *dsl.Variant, n dsl.Node) {
	id, ok := x.c.types.ID(v.Def)
	if !ok {
		x.c.errorf(n, "enum %s without an id", v.Def.Name)
		for range v.Payload {
			x.fb.Op(bytecode.OpPop)
		}
		x.fb.Op(bytecode.OpUnit)
		return
	}
	x.span(n)
	x.fb.MakeEnum(id, abi.VariantIndex(v), uint8(len(v.Payload)))
}

// optionNone and optionSome build Option values.
func (x *fnCtx) optionNone() { x.fb.MakeEnum(bytecode.TypeOption, bytecode.VariantNone, 0) }
func (x *fnCtx) optionSome() { x.fb.MakeEnum(bytecode.TypeOption, bytecode.VariantSome, 1) }

func (x *fnCtx) ifExpr(e *dsl.IfExpr) {
	elseL, end := x.fb.NewLabel(), x.fb.NewLabel()
	x.expr(e.Cond)
	x.fb.JumpIfFalse(elseL)
	x.block(e.Then)
	x.fb.Jump(end)
	x.fb.Bind(elseL)
	if e.Else != nil {
		x.expr(e.Else)
	} else {
		x.fb.Op(bytecode.OpUnit)
	}
	x.fb.Bind(end)
}

// agent emits EFFECT agent_call with arguments (planner name, goal,
// context, tool names, budget); absent arguments are ().
func (x *fnCtx) agent(e *dsl.AgentExpr) {
	slots := [4]int{-1, -1, -1, -1}
	for _, a := range e.Args {
		if a.Value == nil {
			continue
		}
		if a.Param == 2 {
			continue // the tool list is static
		}
		x.expr(a.Value)
		if a.Param < 0 || a.Param > 3 || slots[a.Param] >= 0 {
			x.fb.Op(bytecode.OpPop)
			continue
		}
		slots[a.Param] = int(x.storeTemp())
	}
	name := ""
	if e.Name != nil {
		name = e.Name.Name
	}
	x.fb.String(name)
	for i := 0; i < 4; i++ {
		switch {
		case i == 2:
			for _, t := range e.Tools {
				x.fb.String(t.Name)
			}
			x.fb.Emit(bytecode.OpMakeList, int64(len(e.Tools)))
		case slots[i] >= 0:
			x.fb.Load(uint16(slots[i]))
		default:
			x.fb.Op(bytecode.OpUnit)
		}
	}
	x.effect(bytecode.EffectAgentCall, 5, nil, e)
}

func (x *fnCtx) unary(e *dsl.UnaryExpr) {
	x.expr(e.X)
	x.span(e)
	switch e.Op {
	case dsl.OpNot:
		x.fb.Op(bytecode.OpNot)
	default:
		if isPrim(e.X.Type(), dsl.PrimFloat) {
			x.fb.Op(bytecode.OpNegF)
		} else {
			x.fb.Op(bytecode.OpNegI)
		}
	}
}

func isPrim(t dsl.Type, k dsl.PrimKind) bool {
	p, ok := t.(*dsl.PrimType)
	return ok && p.Kind == k
}

func (x *fnCtx) binary(e *dsl.BinaryExpr) {
	switch e.Op {
	case dsl.OpAnd:
		// a && b: if a is false the result is false without evaluating b.
		short, end := x.fb.NewLabel(), x.fb.NewLabel()
		x.expr(e.X)
		x.fb.JumpIfFalse(short)
		x.expr(e.Y)
		x.fb.Jump(end)
		x.fb.Bind(short)
		x.fb.Op(bytecode.OpFalse)
		x.fb.Bind(end)
		return
	case dsl.OpOr:
		rhs, end := x.fb.NewLabel(), x.fb.NewLabel()
		x.expr(e.X)
		x.fb.JumpIfFalse(rhs)
		x.fb.Op(bytecode.OpTrue)
		x.fb.Jump(end)
		x.fb.Bind(rhs)
		x.expr(e.Y)
		x.fb.Bind(end)
		return
	case dsl.OpIn:
		// x in xs is contains(xs, x); x is evaluated first.
		x.expr(e.X)
		t := x.storeTemp()
		x.expr(e.Y)
		x.fb.Load(t)
		x.span(e)
		x.fb.Builtin(bytecode.BContains)
		return
	}
	x.expr(e.X)
	x.expr(e.Y)
	x.span(e)
	if op, ok := compareOps[e.Op]; ok {
		x.fb.Op(op)
		return
	}
	if e.Op == dsl.OpRange {
		x.fb.Builtin(bytecode.BRange)
		return
	}
	x.fb.Op(arithOp(e.Op, e.X.Type(), e.Y.Type()))
}

var compareOps = map[dsl.BinaryOp]bytecode.Opcode{
	dsl.OpEq: bytecode.OpEq, dsl.OpNe: bytecode.OpNe, dsl.OpLt: bytecode.OpLt,
	dsl.OpLe: bytecode.OpLe, dsl.OpGt: bytecode.OpGt, dsl.OpGe: bytecode.OpGe,
}

// arithOp selects the instruction for an arithmetic operator by operand
// type: Float uses the _F forms, String + is CONCAT, and Int, Duration
// and Time use the checked _I forms (the VM's integer arithmetic accepts
// exactly the Duration and Time combinations the checker allows).
func arithOp(op dsl.BinaryOp, a, b dsl.Type) bytecode.Opcode {
	float := isPrim(a, dsl.PrimFloat) || isPrim(b, dsl.PrimFloat)
	if op == dsl.OpAdd && isPrim(a, dsl.PrimString) {
		return bytecode.OpConcat
	}
	switch op {
	case dsl.OpAdd:
		if float {
			return bytecode.OpAddF
		}
		return bytecode.OpAddI
	case dsl.OpSub:
		if float {
			return bytecode.OpSubF
		}
		return bytecode.OpSubI
	case dsl.OpMul:
		if float {
			return bytecode.OpMulF
		}
		return bytecode.OpMulI
	case dsl.OpDiv:
		if float {
			return bytecode.OpDivF
		}
		return bytecode.OpDivI
	}
	return bytecode.OpModI
}

// try lowers `e?` (spec 8.5). TRY is used where its VM semantics match the
// checker's TryMode: in the workflow body a failing TRY fails the run
// (approval_denied for a Denied error, else unwrap_failed), elsewhere it
// returns the Err or None. The other combinations are lowered explicitly:
//
//	TEST_VARIANT ok; JUMP_IF_FALSE fail; UNPACK 1; JUMP end
//	fail: FAIL code      (fail the run outside the workflow body)
//	fail: RET            (return Err/None from the workflow body)
func (x *fnCtx) try(e *dsl.TryExpr) {
	x.expr(e.X)
	useTry := (e.Mode == dsl.TryFailRun) == x.entry
	if useTry {
		x.span(e)
		x.fb.Op(bytecode.OpTry)
		return
	}
	code := bytecode.FailUnwrapFailed
	if !e.OnOption && resultErrIsDenied(e.X.Type()) {
		code = bytecode.FailApprovalDenied
	}
	x.unwrapOrElse(abi.OkVariant(e.OnOption), e, func() {
		if e.Mode == dsl.TryFailRun {
			x.fb.Fail(code)
		} else {
			x.fb.Op(bytecode.OpRet)
		}
	})
}

// unwrapOrElse replaces the enum on top of the stack by its single payload
// when it is variant ok, and otherwise runs orElse with the enum on the
// stack; orElse must not fall through.
func (x *fnCtx) unwrapOrElse(ok uint8, n dsl.Node, orElse func()) {
	fail, end := x.fb.NewLabel(), x.fb.NewLabel()
	x.span(n)
	x.fb.Emit(bytecode.OpTestVariant, int64(ok))
	x.fb.JumpIfFalse(fail)
	x.fb.Emit(bytecode.OpUnpack, 1)
	x.fb.Jump(end)
	x.fb.Bind(fail)
	x.span(n)
	orElse()
	x.fb.Bind(end)
}

func resultErrIsDenied(t dsl.Type) bool {
	n, ok := t.(*dsl.NamedType)
	return ok && n.Def.Builtin && n.Def.Name == "Result" && len(n.Args) == 2 && abi.IsDenied(n.Args[1])
}

// cast lowers `x as T` by CastExpr.Kind. Int, Duration and Time share the
// i64 representation; the conversions go through the VM's checked integer
// arithmetic with unit constants.
func (x *fnCtx) cast(e *dsl.CastExpr) {
	x.expr(e.X)
	x.span(e)
	switch e.Kind {
	case dsl.CastNone:
	case dsl.CastIntToFloat:
		x.fb.Builtin(bytecode.BIntToFloat)
	case dsl.CastFloatToInt:
		x.fb.Builtin(bytecode.BFloatToInt)
	case dsl.CastIntToDuration:
		x.fb.Duration(1)
		x.fb.Op(bytecode.OpMulI) // Int * Duration -> Duration
	case dsl.CastDurationToInt:
		x.fb.Duration(1)
		x.fb.Op(bytecode.OpDivI) // Duration / Duration -> Int
	case dsl.CastIntToTime:
		x.fb.Duration(1)
		x.fb.Op(bytecode.OpMulI)
		x.fb.ConstOp(bytecode.TimeConst(0))
		x.fb.Op(bytecode.OpAddI) // Duration + Time -> Time
	case dsl.CastTimeToInt:
		x.fb.ConstOp(bytecode.TimeConst(0))
		x.fb.Op(bytecode.OpSubI) // Time - Time -> Duration
		x.fb.Duration(1)
		x.fb.Op(bytecode.OpDivI)
	case dsl.CastToString:
		x.fb.Builtin(bytecode.BToString)
	case dsl.CastParseDomain:
		var def *dsl.TypeDef
		if n, ok := e.Type().(*dsl.NamedType); ok {
			def = n.Def
		}
		id, ok := abi.DomainParser(def)
		if !ok {
			x.c.errorf(e, "no parser for %v", e.Type())
			return
		}
		x.fb.Builtin(id)
		x.unwrapOrElse(bytecode.VariantSome, e, func() { x.fb.Fail(bytecode.FailUnwrapFailed) })
	default:
		x.c.errorf(e, "unknown cast kind %d", e.Kind)
	}
}
