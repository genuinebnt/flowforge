package bytecode

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Disassemble renders function fn of m as text, one instruction per line:
//
//	0004  CONST 2            ; int 40
//	0007  JUMP_IF_FALSE +5   ; -> 0012
//
// Constant, function, type, builtin, effect and failure operands are
// annotated. Malformed code is rendered up to the first bad byte; it never
// panics.
func Disassemble(m *Module, fn int) string {
	if fn < 0 || fn >= len(m.Funcs) {
		return fmt.Sprintf("; no function %d\n", fn)
	}
	f := &m.Funcs[fn]
	var sb strings.Builder
	fmt.Fprintf(&sb, "fn %d %q arity=%d nlocals=%d ncaps=%d\n", fn, m.FuncName(fn), f.Arity, f.NLocals, f.NCaps)
	code := f.Code
	for pc := 0; pc < len(code); {
		op := Opcode(code[pc])
		info := op.Info()
		if info == nil {
			fmt.Fprintf(&sb, "%04d  .byte 0x%02x\n", pc, code[pc])
			pc++
			continue
		}
		if pc+info.Size > len(code) {
			fmt.Fprintf(&sb, "%04d  %s <truncated>\n", pc, info.Name)
			break
		}
		var ops []int64
		at := pc + 1
		for _, w := range info.Operands {
			switch w {
			case U8:
				ops = append(ops, int64(code[at]))
			case U16:
				ops = append(ops, int64(binary.LittleEndian.Uint16(code[at:])))
			case I32:
				ops = append(ops, int64(int32(binary.LittleEndian.Uint32(code[at:]))))
			}
			at += w.Width()
		}
		next := pc + info.Size
		parts := make([]string, len(ops))
		for i, o := range ops {
			if info.Operands[i] == I32 && o >= 0 {
				parts[i] = fmt.Sprintf("+%d", o)
			} else {
				parts[i] = fmt.Sprint(o)
			}
		}
		line := strings.TrimSpace(info.Name + " " + strings.Join(parts, " "))
		fmt.Fprintf(&sb, "%04d  %-22s", pc, line)
		if c := comment(m, op, ops, next); c != "" {
			sb.WriteString(" ; " + c)
		}
		sb.WriteString("\n")
		pc = next
	}
	return sb.String()
}

func comment(m *Module, op Opcode, ops []int64, next int) string {
	switch op {
	case OpConst:
		if int(ops[0]) < len(m.Consts) {
			return m.Consts[ops[0]].String()
		}
	case OpJump, OpJumpIfFalse, OpLoop:
		return fmt.Sprintf("-> %04d", int64(next)+ops[0])
	case OpCall, OpClosure, OpPushComp:
		return m.FuncName(int(ops[0]))
	case OpMakeRec, OpMakeEnum:
		if l, ok := m.Layout(TypeID(ops[0])); ok {
			if op == OpMakeEnum && int(ops[1]) < len(l.Variants) {
				return l.Name + "::" + l.Variants[ops[1]].Name
			}
			return l.Name
		}
	case OpBuiltin:
		if b, ok := LookupBuiltin(BuiltinID(ops[0])); ok {
			return b.Name
		}
	case OpEffect:
		return EffectKind(ops[0]).String()
	case OpFail:
		return FailCode(ops[0]).String()
	}
	return ""
}

// DisassembleModule renders every function of m.
func DisassembleModule(m *Module) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "; module v%d entry=%d effects=%s\n", m.FormatVersion, m.Entry, m.Effects)
	for i := range m.Funcs {
		sb.WriteString(Disassemble(m, i))
	}
	return sb.String()
}
