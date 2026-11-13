package bytecode

import "fmt"

// Opcode is one instruction byte (spec 8.4). The numeric values are part of
// the module format and never change.
type Opcode uint8

// Instruction opcodes with the exact values of spec 8.4.
const (
	OpNop        Opcode = 0x00
	OpConst      Opcode = 0x01 // k:u16
	OpUnit       Opcode = 0x02
	OpTrue       Opcode = 0x03
	OpFalse      Opcode = 0x04
	OpPop        Opcode = 0x05
	OpDup        Opcode = 0x06
	OpLoadLocal  Opcode = 0x10 // i:u16
	OpStoreLocal Opcode = 0x11 // i:u16
	OpLoadCap    Opcode = 0x12 // i:u8

	OpAddI Opcode = 0x20
	OpSubI Opcode = 0x21
	OpMulI Opcode = 0x22
	OpDivI Opcode = 0x23
	OpModI Opcode = 0x24
	OpNegI Opcode = 0x25
	OpAddF Opcode = 0x26
	OpSubF Opcode = 0x27
	OpMulF Opcode = 0x28
	OpDivF Opcode = 0x29
	OpNegF Opcode = 0x2A

	OpConcat Opcode = 0x2B

	OpEq  Opcode = 0x30
	OpNe  Opcode = 0x31
	OpLt  Opcode = 0x32
	OpLe  Opcode = 0x33
	OpGt  Opcode = 0x34
	OpGe  Opcode = 0x35
	OpNot Opcode = 0x36

	OpJump        Opcode = 0x40 // off:i32, forward only
	OpJumpIfFalse Opcode = 0x41 // off:i32, forward only
	OpLoop        Opcode = 0x42 // off:i32, backward only

	OpCall        Opcode = 0x50 // fn:u16, argc:u8
	OpCallClosure Opcode = 0x51 // argc:u8
	OpRet         Opcode = 0x52
	OpClosure     Opcode = 0x53 // fn:u16, ncaps:u8

	OpMakeList Opcode = 0x60 // n:u16
	OpMakeMap  Opcode = 0x61 // n:u16
	OpMakeRec  Opcode = 0x62 // type:u16, n:u8
	OpGetField Opcode = 0x63 // f:u8
	OpIndex    Opcode = 0x64
	OpNewList  Opcode = 0x65
	OpListPush Opcode = 0x66
	OpListLen  Opcode = 0x67
	OpListGet  Opcode = 0x68

	OpMakeEnum    Opcode = 0x70 // type:u16, variant:u8, n:u8
	OpTestVariant Opcode = 0x71 // variant:u8
	OpUnpack      Opcode = 0x72 // n:u8
	OpTry         Opcode = 0x73

	OpInterp Opcode = 0x78 // n:u8

	OpBuiltin Opcode = 0x80 // id:u16, argc:u8

	OpEffect   Opcode = 0x90 // kind:u8, argc:u8
	OpPushComp Opcode = 0x91 // fn:u16, ncaps:u8

	OpHalt Opcode = 0xF0
	OpFail Opcode = 0xF1 // code:u16
)

// Operand is the encoding of one instruction operand.
type Operand uint8

// Operand encodings. All multi-byte operands are little-endian.
const (
	U8  Operand = 1
	U16 Operand = 2
	I32 Operand = 4
)

// Width returns the operand's size in bytes.
func (o Operand) Width() int { return int(o) }

// OpInfo describes one opcode: its mnemonic and operand layout.
type OpInfo struct {
	Name     string
	Operands []Operand
	// Size is the total instruction size in bytes, opcode included.
	Size int
}

var opTable [256]*OpInfo

func def(op Opcode, name string, operands ...Operand) {
	size := 1
	for _, o := range operands {
		size += o.Width()
	}
	opTable[op] = &OpInfo{Name: name, Operands: operands, Size: size}
}

func init() {
	def(OpNop, "NOP")
	def(OpConst, "CONST", U16)
	def(OpUnit, "UNIT")
	def(OpTrue, "TRUE")
	def(OpFalse, "FALSE")
	def(OpPop, "POP")
	def(OpDup, "DUP")
	def(OpLoadLocal, "LOAD_LOCAL", U16)
	def(OpStoreLocal, "STORE_LOCAL", U16)
	def(OpLoadCap, "LOAD_CAP", U8)
	def(OpAddI, "ADD_I")
	def(OpSubI, "SUB_I")
	def(OpMulI, "MUL_I")
	def(OpDivI, "DIV_I")
	def(OpModI, "MOD_I")
	def(OpNegI, "NEG_I")
	def(OpAddF, "ADD_F")
	def(OpSubF, "SUB_F")
	def(OpMulF, "MUL_F")
	def(OpDivF, "DIV_F")
	def(OpNegF, "NEG_F")
	def(OpConcat, "CONCAT")
	def(OpEq, "EQ")
	def(OpNe, "NE")
	def(OpLt, "LT")
	def(OpLe, "LE")
	def(OpGt, "GT")
	def(OpGe, "GE")
	def(OpNot, "NOT")
	def(OpJump, "JUMP", I32)
	def(OpJumpIfFalse, "JUMP_IF_FALSE", I32)
	def(OpLoop, "LOOP", I32)
	def(OpCall, "CALL", U16, U8)
	def(OpCallClosure, "CALL_CLOSURE", U8)
	def(OpRet, "RET")
	def(OpClosure, "CLOSURE", U16, U8)
	def(OpMakeList, "MAKE_LIST", U16)
	def(OpMakeMap, "MAKE_MAP", U16)
	def(OpMakeRec, "MAKE_REC", U16, U8)
	def(OpGetField, "GET_FIELD", U8)
	def(OpIndex, "INDEX")
	def(OpNewList, "NEW_LIST")
	def(OpListPush, "LIST_PUSH")
	def(OpListLen, "LIST_LEN")
	def(OpListGet, "LIST_GET")
	def(OpMakeEnum, "MAKE_ENUM", U16, U8, U8)
	def(OpTestVariant, "TEST_VARIANT", U8)
	def(OpUnpack, "UNPACK", U8)
	def(OpTry, "TRY")
	def(OpInterp, "INTERP", U8)
	def(OpBuiltin, "BUILTIN", U16, U8)
	def(OpEffect, "EFFECT", U8, U8)
	def(OpPushComp, "PUSH_COMP", U16, U8)
	def(OpHalt, "HALT")
	def(OpFail, "FAIL", U16)
}

// Info returns the description of op, or nil for an unknown opcode.
func (op Opcode) Info() *OpInfo { return opTable[op] }

// Valid reports whether op is a defined opcode.
func (op Opcode) Valid() bool { return opTable[op] != nil }

// Size returns the encoded instruction size, or 0 for an unknown opcode.
func (op Opcode) Size() int {
	if i := opTable[op]; i != nil {
		return i.Size
	}
	return 0
}

// String returns the mnemonic, or "OP_XX" for an unassigned byte.
func (op Opcode) String() string {
	if i := opTable[op]; i != nil {
		return i.Name
	}
	return fmt.Sprintf("OP_%02X", uint8(op))
}

// IsTerminal reports whether control never falls through op to the next
// instruction (RET, HALT, FAIL and the unconditional jumps).
func (op Opcode) IsTerminal() bool {
	switch op {
	case OpRet, OpHalt, OpFail, OpJump, OpLoop:
		return true
	}
	return false
}

// Opcodes returns every defined opcode in increasing numeric order.
func Opcodes() []Opcode {
	var out []Opcode
	for i := range opTable {
		if opTable[i] != nil {
			out = append(out, Opcode(i))
		}
	}
	return out
}
