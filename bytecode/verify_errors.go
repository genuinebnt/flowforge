package bytecode

import (
	"errors"
	"fmt"
)

// Verifier rule violations (spec 8.8). Every rule has its own sentinel so
// that callers and tests (bc_002) can tell them apart with errors.Is.
var (
	// Instruction decoding.
	ErrUnknownOpcode     = errors.New("bytecode: unknown opcode")
	ErrTruncatedInstr    = errors.New("bytecode: instruction operands run past the end of the code")
	ErrNoTerminator      = errors.New("bytecode: function does not end in RET, HALT or FAIL")
	ErrJumpOutOfRange    = errors.New("bytecode: jump target outside the function")
	ErrJumpMisaligned    = errors.New("bytecode: jump target not on an instruction boundary")
	ErrBackwardJump      = errors.New("bytecode: JUMP or JUMP_IF_FALSE jumps backward")
	ErrLoopForward       = errors.New("bytecode: LOOP jumps forward")
	ErrLoopHeader        = errors.New("bytecode: LOOP target is not reachable by forward control flow")
	ErrStackUnderflow    = errors.New("bytecode: stack underflow")
	ErrStackMismatch     = errors.New("bytecode: stack depth differs between paths at a join")
	ErrStackOverflow     = errors.New("bytecode: stack depth exceeds the 65,536 value limit")
	ErrLocalIndex        = errors.New("bytecode: local index out of range")
	ErrLocalUnset        = errors.New("bytecode: local read before it is written on some path")
	ErrLocalReassigned   = errors.New("bytecode: local written more than once")
	ErrLocalsArity       = errors.New("bytecode: nlocals is smaller than arity")
	ErrCapIndex          = errors.New("bytecode: capture index out of range")
	ErrFuncIndex         = errors.New("bytecode: function index out of range")
	ErrCallArity         = errors.New("bytecode: CALL argument count differs from the callee's arity")
	ErrCallCaptures      = errors.New("bytecode: CALL targets a function that needs captures")
	ErrClosureCaps       = errors.New("bytecode: CLOSURE capture count differs from the function's ncaps")
	ErrCompSignature     = errors.New("bytecode: PUSH_COMP function must take no arguments and match ncaps")
	ErrConstIndex        = errors.New("bytecode: constant index out of range")
	ErrTypeIndex         = errors.New("bytecode: type index does not name a record or enum")
	ErrFieldCount        = errors.New("bytecode: MAKE_REC field count differs from the record layout")
	ErrVariantIndex      = errors.New("bytecode: MAKE_ENUM variant out of range")
	ErrPayloadArity      = errors.New("bytecode: MAKE_ENUM payload count differs from the variant arity")
	ErrUnknownEffect     = errors.New("bytecode: unknown effect kind")
	ErrEffectArgc        = errors.New("bytecode: wrong argument count for effect kind")
	ErrToolCallTarget    = errors.New("bytecode: tool_call is not immediately preceded by CONST of a tool name in TOOLS")
	ErrUnknownBuiltin    = errors.New("bytecode: unknown builtin")
	ErrBuiltinArity      = errors.New("bytecode: builtin argument count differs from its arity")
	ErrFailCode          = errors.New("bytecode: unknown failure code")
	ErrTooComplex        = errors.New("bytecode: function too large to verify")
	ErrNameIndex         = errors.New("bytecode: name or type text index is not a String constant")
	ErrEntry             = errors.New("bytecode: entry function missing or takes captures")
	ErrSignature         = errors.New("bytecode: signature names a missing function")
	ErrTooManyItems      = errors.New("bytecode: too many functions or types")
	ErrDuplicateTool     = errors.New("bytecode: tool listed twice in TOOLS")
	ErrToolUnknown       = errors.New("bytecode: tool not found in the registry")
	ErrToolHash          = errors.New("bytecode: tool spec_sha256 does not match the registry")
	ErrEffectBits        = errors.New("bytecode: effect mask has undefined bits")
	ErrEffectMask        = errors.New("bytecode: declared effect mask is not a superset of the reachable effects")
	ErrFormatVersionFlag = errors.New("bytecode: unsupported format version or flags")
)

// VerifyError locates a rule violation. Unwrap returns the rule sentinel.
type VerifyError struct {
	Rule   error
	Func   int // -1 for module-level rules
	Offset int // code offset, -1 when not applicable
	Detail string
}

func (e *VerifyError) Error() string {
	s := e.Rule.Error()
	if e.Func >= 0 {
		s += fmt.Sprintf(" (function %d", e.Func)
		if e.Offset >= 0 {
			s += fmt.Sprintf(", offset %d", e.Offset)
		}
		s += ")"
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// Unwrap returns the violated rule.
func (e *VerifyError) Unwrap() error { return e.Rule }

var ruleIDs = []struct {
	id  string
	err error
}{
	{"unknown_opcode", ErrUnknownOpcode},
	{"truncated_instruction", ErrTruncatedInstr},
	{"no_terminator", ErrNoTerminator},
	{"jump_out_of_range", ErrJumpOutOfRange},
	{"jump_misaligned", ErrJumpMisaligned},
	{"backward_jump", ErrBackwardJump},
	{"loop_forward", ErrLoopForward},
	{"loop_header", ErrLoopHeader},
	{"stack_underflow", ErrStackUnderflow},
	{"stack_mismatch", ErrStackMismatch},
	{"stack_overflow", ErrStackOverflow},
	{"local_index", ErrLocalIndex},
	{"local_unset", ErrLocalUnset},
	{"local_reassigned", ErrLocalReassigned},
	{"locals_arity", ErrLocalsArity},
	{"cap_index", ErrCapIndex},
	{"func_index", ErrFuncIndex},
	{"call_arity", ErrCallArity},
	{"call_captures", ErrCallCaptures},
	{"closure_caps", ErrClosureCaps},
	{"comp_signature", ErrCompSignature},
	{"const_index", ErrConstIndex},
	{"type_index", ErrTypeIndex},
	{"field_count", ErrFieldCount},
	{"variant_index", ErrVariantIndex},
	{"payload_arity", ErrPayloadArity},
	{"unknown_effect", ErrUnknownEffect},
	{"effect_argc", ErrEffectArgc},
	{"tool_call_target", ErrToolCallTarget},
	{"unknown_builtin", ErrUnknownBuiltin},
	{"builtin_arity", ErrBuiltinArity},
	{"fail_code", ErrFailCode},
	{"too_complex", ErrTooComplex},
	{"name_index", ErrNameIndex},
	{"entry", ErrEntry},
	{"signature", ErrSignature},
	{"too_many_items", ErrTooManyItems},
	{"duplicate_tool", ErrDuplicateTool},
	{"tool_unknown", ErrToolUnknown},
	{"tool_hash", ErrToolHash},
	{"effect_bits", ErrEffectBits},
	{"effect_mask", ErrEffectMask},
	{"format_version_flags", ErrFormatVersionFlag},
}

// RuleID returns the stable snake_case id of the verifier rule err
// violates ("stack_underflow", ...), or "" when err is not a rule
// violation. Conformance data and diagnostics use these ids.
func RuleID(err error) string {
	for _, r := range ruleIDs {
		if errors.Is(err, r.err) {
			return r.id
		}
	}
	return ""
}

// RuleIDs lists every rule id in a stable order.
func RuleIDs() []string {
	out := make([]string, len(ruleIDs))
	for i, r := range ruleIDs {
		out[i] = r.id
	}
	return out
}
