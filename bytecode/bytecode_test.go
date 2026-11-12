package bytecode_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	bc "github.com/genuinebnt/flowforge/bytecode"
)

var toolHash = sha256.Sum256([]byte("httpx spec v1"))

func tools() bc.ToolMap {
	return bc.ToolMap{"httpx": {SpecSHA256: toolHash, Effects: bc.EffActive}}
}

// sampleModule exercises every section of the format.
func sampleModule(t testing.TB) *bc.Module {
	t.Helper()
	b := bc.NewBuilder()
	rec := b.Record("Report", bc.Field{Name: "new_assets", Type: "Int"}, bc.Field{Name: "title", Type: "Option<String>"})
	en := b.Enum("Color", bc.Variant{Name: "Red"}, bc.Variant{Name: "Rgb", Payload: []string{"Int", "Int", "Int"}})
	b.Tool("httpx", toolHash)
	b.SetEffects(bc.EffActive | bc.EffTime)
	b.SetMeta(bc.Meta{CompilerVersion: "ffc 0.1.0", SourceSHA256: sha256.Sum256([]byte("src")),
		ProgramPolicySHA256: sha256.Sum256([]byte("pol")), WorkflowName: "recon"})

	main := b.Func("main", 1, 0)
	helper := b.Func("helper", 1, 0)
	b.Signature(main.Index(), []bc.Field{{Name: "n", Type: "Int"}}, "Report")
	b.SetEntry(main.Index())

	main.Span(10, 5)
	main.Load(0)
	main.Call(helper.Index(), 1)
	x := main.Local()
	main.Store(x)
	main.Load(x)
	main.Int(0)
	main.Op(bc.OpGt)
	els := main.NewLabel()
	end := main.NewLabel()
	main.JumpIfFalse(els)
	main.Load(x)
	main.ToolCall("httpx", 1)
	main.Op(bc.OpPop)
	main.Jump(end)
	main.Bind(els)
	main.Effect(bc.EffectNow, 0)
	main.Op(bc.OpPop)
	main.Bind(end)
	main.Load(x)
	main.Emit(bc.OpUnit)
	main.MakeRec(rec, 2)
	main.Float(2.5)
	main.Op(bc.OpPop)
	main.Int(1)
	main.Int(2)
	main.Int(3)
	main.MakeEnum(en, 1, 3)
	main.Op(bc.OpPop)
	main.Duration(int64(1e9))
	main.Op(bc.OpPop)
	main.Span(30, 2)
	main.Op(bc.OpRet)

	helper.Load(0)
	helper.Int(2)
	helper.Op(bc.OpMulI, bc.OpRet)
	m, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBC001_RoundTrip(t *testing.T) {
	m := sampleModule(t)
	if _, err := bc.Verify(m, tools()); err != nil {
		t.Fatalf("sample does not verify: %v\n%s", err, bc.DisassembleModule(m))
	}
	enc, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("encode_then_decode", func(t *testing.T) {
		got, err := bc.Decode(enc)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, m) {
			t.Fatalf("decoded module differs\n got %+v\nwant %+v", got, m)
		}
	})
	t.Run("decode_then_encode", func(t *testing.T) {
		got, err := bc.Decode(enc)
		if err != nil {
			t.Fatal(err)
		}
		again, err := got.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(again, enc) {
			t.Fatal("re-encoding changed the bytes")
		}
	})
	t.Run("hash", func(t *testing.T) {
		h, err := m.Hash()
		if err != nil {
			t.Fatal(err)
		}
		if h != sha256.Sum256(enc) {
			t.Fatal("Hash is not sha256 of the encoding")
		}
	})
	t.Run("rejects_malformed", func(t *testing.T) {
		cases := map[string]struct {
			b    []byte
			want error
		}{
			"magic":     {append([]byte("XXBC"), enc[4:]...), bc.ErrBadMagic},
			"version":   {append(append([]byte("FFBC"), 2, 0), enc[6:]...), bc.ErrVersion},
			"truncated": {enc[:len(enc)-1], bc.ErrTruncated},
			"trailing":  {append(append([]byte(nil), enc...), 0), bc.ErrTrailing},
			"too_large": {make([]byte, bc.MaxModuleSize+1), bc.ErrTooLarge},
		}
		for name, c := range cases {
			if _, err := bc.Decode(c.b); !errors.Is(err, c.want) {
				t.Errorf("%s: got %v, want %v", name, err, c.want)
			}
		}
	})
	t.Run("disassemble", func(t *testing.T) {
		d := bc.DisassembleModule(m)
		for _, want := range []string{"CALL 1 1", "; helper", "EFFECT 1 2", "tool_call", "MAKE_ENUM", "Color::Rgb", "-> "} {
			if !strings.Contains(d, want) {
				t.Errorf("disassembly lacks %q:\n%s", want, d)
			}
		}
	})
}

// VerifyCase is one bc_002 case: a module and the rule it violates ("" =
// accepted). Shared with the conformance generator.
type VerifyCase struct {
	Name  string
	Build func(t testing.TB) *bc.Module
	Rule  error
}

// one builds a module with a single entry function whose body is emitted
// by body.
func one(arity uint8, ncaps uint8, body func(b *bc.Builder, f *bc.FuncBuilder)) func(t testing.TB) *bc.Module {
	return func(t testing.TB) *bc.Module {
		t.Helper()
		b := bc.NewBuilder()
		b.Tool("httpx", toolHash)
		b.SetEffects(bc.EffAll)
		f := b.Func("main", arity, ncaps)
		body(b, f)
		m, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
}

func mutate(base func(t testing.TB) *bc.Module, fn func(m *bc.Module)) func(t testing.TB) *bc.Module {
	return func(t testing.TB) *bc.Module {
		m := base(t)
		fn(m)
		return m
	}
}

func ret(f *bc.FuncBuilder) { f.Op(bc.OpUnit, bc.OpRet) }

func verifyCases() []VerifyCase {
	okBody := one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { ret(f) })
	return []VerifyCase{
		{"accept_minimal", okBody, nil},
		{"accept_sample", sampleModule, nil},
		{"accept_loop", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			// for-loop shape: loop-carried values on the stack, re-bound to
			// fresh locals at the top of every iteration.
			xs, i, x := f.Local(), f.Local(), f.Local()
			f.Int(10)
			f.Int(0)
			head := f.Here()
			f.Store(i)
			f.Store(xs)
			f.Load(i)
			f.Load(xs)
			f.Op(bc.OpLt)
			exit := f.NewLabel()
			f.JumpIfFalse(exit)
			f.Load(i)
			f.Store(x)
			f.Load(xs)
			f.Load(x)
			f.Int(1)
			f.Op(bc.OpAddI)
			f.Loop(head)
			f.Bind(exit)
			ret(f)
		}), nil},

		{"bad_jump_target", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Emit(bc.OpJump, 100); ret(f) }), bc.ErrJumpOutOfRange},
		{"jump_misaligned", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			f.Emit(bc.OpJump, 1)
			f.Int(1)
			f.Op(bc.OpRet)
		}), bc.ErrJumpMisaligned},
		{"backward_jump", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Op(bc.OpNop); f.Emit(bc.OpJump, -6); ret(f) }), bc.ErrBackwardJump},
		{"loop_forward", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Emit(bc.OpLoop, 0); ret(f) }), bc.ErrLoopForward},
		{"loop_header_unreachable", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			over := f.NewLabel()
			f.Jump(over)
			h := f.Here()
			f.Op(bc.OpNop)
			f.Bind(over)
			f.Loop(h)
			ret(f)
		}), bc.ErrLoopHeader},
		{"stack_underflow", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Op(bc.OpPop); ret(f) }), bc.ErrStackUnderflow},
		{"stack_mismatch_at_join", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			l := f.NewLabel()
			f.Op(bc.OpTrue)
			f.JumpIfFalse(l)
			f.Op(bc.OpUnit)
			f.Bind(l)
			ret(f)
		}), bc.ErrStackMismatch},
		{"stack_mismatch_at_loop", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			h := f.Here()
			f.Op(bc.OpUnit)
			f.Loop(h)
			ret(f)
		}), bc.ErrStackMismatch},
		{"stack_overflow", mutate(one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Op(bc.OpUnit, bc.OpUnit, bc.OpPop, bc.OpRet) }),
			func(m *bc.Module) { m.Funcs[0].NLocals = math.MaxUint16 }), bc.ErrStackOverflow},
		{"local_index", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Load(3); f.Op(bc.OpRet) }), bc.ErrLocalIndex},
		{"local_read_before_write", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			x := f.Local()
			l := f.NewLabel()
			f.Op(bc.OpTrue)
			f.JumpIfFalse(l)
			f.Int(1)
			f.Store(x)
			f.Bind(l)
			f.Load(x)
			f.Op(bc.OpRet)
		}), bc.ErrLocalUnset},
		{"local_assigned_twice", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			x := f.Local()
			f.Int(1)
			f.Store(x)
			f.Int(2)
			f.Store(x)
			ret(f)
		}), bc.ErrLocalReassigned},
		{"local_reassigned_across_loop", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			i := f.Local()
			f.Int(0)
			f.Store(i)
			h := f.Here()
			f.Load(i)
			f.Store(i)
			f.Loop(h)
			ret(f)
		}), bc.ErrLocalReassigned},
		{"locals_below_arity", mutate(okBody, func(m *bc.Module) { m.Funcs[0].Arity = 2 }), bc.ErrLocalsArity},
		{"unknown_opcode_byte", mutate(okBody, func(m *bc.Module) { m.Funcs[0].Code = []byte{0xEE, 0x52} }), bc.ErrUnknownOpcode},
		{"truncated_instruction", mutate(okBody, func(m *bc.Module) { m.Funcs[0].Code = []byte{0x02, 0x52, 0x01, 0x00} }), bc.ErrTruncatedInstr},
		{"no_terminator", mutate(okBody, func(m *bc.Module) { m.Funcs[0].Code = []byte{0x02} }), bc.ErrNoTerminator},
		{"empty_function", mutate(okBody, func(m *bc.Module) { m.Funcs[0].Code = nil; m.Funcs[0].Spans = nil }), bc.ErrNoTerminator},
		{"const_index", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Emit(bc.OpConst, 999); f.Op(bc.OpRet) }), bc.ErrConstIndex},
		{"cap_index", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Cap(0); f.Op(bc.OpRet) }), bc.ErrCapIndex},
		{"func_index", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Call(9, 0); f.Op(bc.OpRet) }), bc.ErrFuncIndex},
		{"call_arity", one(1, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Load(0); f.Load(0); f.Call(0, 2); f.Op(bc.OpRet) }), bc.ErrCallArity},
		{"call_needs_captures", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			g := b.Func("g", 0, 1)
			g.Cap(0)
			g.Op(bc.OpRet)
			f.Call(g.Index(), 0)
			f.Op(bc.OpRet)
		}), bc.ErrCallCaptures},
		{"closure_caps", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			g := b.Func("g", 0, 1)
			g.Cap(0)
			g.Op(bc.OpRet)
			f.Closure(g.Index(), 0)
			f.Op(bc.OpRet)
		}), bc.ErrClosureCaps},
		{"comp_signature", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			g := b.Func("g", 1, 0)
			ret(g)
			f.PushComp(g.Index(), 0)
			ret(f)
		}), bc.ErrCompSignature},
		{"type_index", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.MakeRec(20, 0); f.Op(bc.OpRet) }), bc.ErrTypeIndex},
		{"field_count", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			r := b.Record("R", bc.Field{Name: "a", Type: "Int"})
			f.MakeRec(r, 0)
			f.Op(bc.OpRet)
		}), bc.ErrFieldCount},
		{"variant_index", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.MakeEnum(bc.TypeOption, 2, 0); f.Op(bc.OpRet) }), bc.ErrVariantIndex},
		{"payload_arity", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.MakeEnum(bc.TypeOption, bc.VariantSome, 0); f.Op(bc.OpRet) }), bc.ErrPayloadArity},
		{"unknown_effect", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Effect(40, 0); f.Op(bc.OpRet) }), bc.ErrUnknownEffect},
		{"effect_argc", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Op(bc.OpUnit); f.Effect(bc.EffectNow, 1); f.Op(bc.OpRet) }), bc.ErrEffectArgc},
		{"tool_call_without_tool", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Op(bc.OpUnit); f.Effect(bc.EffectToolCall, 1); f.Op(bc.OpRet) }), bc.ErrToolCallTarget},
		{"tool_call_unlisted", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.String("nmap"); f.Effect(bc.EffectToolCall, 1); f.Op(bc.OpRet) }), bc.ErrToolCallTarget},
		{"unknown_builtin", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Emit(bc.OpBuiltin, 9999, 0); f.Op(bc.OpRet) }), bc.ErrUnknownBuiltin},
		{"builtin_arity", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Op(bc.OpUnit); f.Emit(bc.OpBuiltin, int64(bc.BReplace), 1); f.Op(bc.OpRet) }), bc.ErrBuiltinArity},
		{"fail_code", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { f.Fail(999) }), bc.ErrFailCode},
		{"name_index", mutate(okBody, func(m *bc.Module) { m.Funcs[0].Name = 77 }), bc.ErrNameIndex},
		{"entry_missing", mutate(okBody, func(m *bc.Module) { m.Entry = 5 }), bc.ErrEntry},
		{"signature_func", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { b.Signature(9, nil, "Int"); ret(f) }), bc.ErrSignature},
		{"no_functions", mutate(okBody, func(m *bc.Module) { m.Funcs = nil }), bc.ErrTooManyItems},
		{"duplicate_tool", mutate(okBody, func(m *bc.Module) { m.Tools = append(m.Tools, m.Tools[0]) }), bc.ErrDuplicateTool},
		{"tool_unknown", one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) { b.Tool("nmap", toolHash); ret(f) }), bc.ErrToolUnknown},
		{"tool_hash_mismatch", mutate(okBody, func(m *bc.Module) { m.Tools[0].SpecSHA256[0] ^= 1 }), bc.ErrToolHash},
		{"effect_bits", mutate(okBody, func(m *bc.Module) { m.Effects = 1 << 15 }), bc.ErrEffectBits},
		{"effect_mask", mutate(one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			f.Op(bc.OpUnit)
			f.ToolCall("httpx", 1)
			f.Op(bc.OpRet)
		}), func(m *bc.Module) { m.Effects = bc.EffPassive }), bc.ErrEffectMask},
		{"effect_mask_through_closure", mutate(one(0, 0, func(b *bc.Builder, f *bc.FuncBuilder) {
			g := b.Func("g", 0, 0)
			g.Effect(bc.EffectNow, 0)
			g.Op(bc.OpRet)
			f.Closure(g.Index(), 0)
			f.Op(bc.OpRet)
		}), func(m *bc.Module) { m.Effects = 0 }), bc.ErrEffectMask},
		{"format_flags", mutate(okBody, func(m *bc.Module) { m.Flags = 1 }), bc.ErrFormatVersionFlag},
	}
}

func TestBC002_VerifierRejects(t *testing.T) {
	for _, c := range verifyCases() {
		t.Run(c.Name, func(t *testing.T) {
			m := c.Build(t)
			_, err := bc.Verify(m, tools())
			if c.Rule == nil {
				if err != nil {
					t.Fatalf("rejected: %v\n%s", err, bc.DisassembleModule(m))
				}
				return
			}
			if !errors.Is(err, c.Rule) {
				t.Fatalf("got %v, want %v\n%s", err, c.Rule, bc.DisassembleModule(m))
			}
			var ve *bc.VerifyError
			if !errors.As(err, &ve) {
				t.Fatalf("error %T is not a *VerifyError", err)
			}
			if bc.RuleID(err) == "" {
				t.Fatal("rule has no id")
			}
		})
	}
	// The six cases spec 25.3 names map to six distinct errors.
	required := []error{bc.ErrJumpOutOfRange, bc.ErrStackUnderflow, bc.ErrStackMismatch, bc.ErrLocalReassigned, bc.ErrUnknownOpcode, bc.ErrConstIndex}
	ids := map[string]bool{}
	for _, e := range required {
		ids[bc.RuleID(e)] = true
	}
	if len(ids) != len(required) {
		t.Fatal("required bc_002 rules are not distinct")
	}
	all := map[string]bool{}
	for _, id := range bc.RuleIDs() {
		if all[id] {
			t.Fatalf("duplicate rule id %s", id)
		}
		all[id] = true
	}
}

func TestBC_OpcodeTable(t *testing.T) {
	// Exact values and operand widths of spec 8.4.
	want := map[bc.Opcode]int{
		0x00: 1, 0x01: 3, 0x02: 1, 0x03: 1, 0x04: 1, 0x05: 1, 0x06: 1, 0x10: 3, 0x11: 3, 0x12: 2,
		0x20: 1, 0x25: 1, 0x26: 1, 0x2A: 1, 0x2B: 1, 0x30: 1, 0x35: 1, 0x36: 1, 0x40: 5, 0x41: 5, 0x42: 5,
		0x50: 4, 0x51: 2, 0x52: 1, 0x53: 4, 0x60: 3, 0x61: 3, 0x62: 4, 0x63: 2, 0x64: 1, 0x65: 1, 0x66: 1,
		0x67: 1, 0x68: 1, 0x70: 5, 0x71: 2, 0x72: 2, 0x73: 1, 0x78: 2, 0x80: 4, 0x90: 3, 0x91: 4, 0xF0: 1, 0xF1: 3,
	}
	for op, size := range want {
		if op.Size() != size {
			t.Errorf("%s: size %d, want %d", op, op.Size(), size)
		}
	}
	if n := len(bc.Opcodes()); n != 55 {
		t.Errorf("%d opcodes defined, want 55", n)
	}
	for i := 1; i <= 12; i++ {
		if !bc.EffectKind(i).Valid() {
			t.Errorf("effect kind %d missing", i)
		}
	}
	if bc.EffectKind(13).Valid() || bc.EffectKind(0).Valid() {
		t.Error("unexpected effect kinds")
	}
	for i := 1; i <= 15; i++ {
		if !bc.FailCode(i).Valid() {
			t.Errorf("failure code %d missing", i)
		}
	}
	if c, _ := bc.ParseFailCode("cancelled"); c != 15 {
		t.Error("failure numbering changed")
	}
	for i, b := range bc.Builtins() {
		if int(b.ID) != i {
			t.Fatalf("builtin %s has id %d at position %d", b.Name, b.ID, i)
		}
	}
}
