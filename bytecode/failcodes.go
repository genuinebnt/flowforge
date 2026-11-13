package bytecode

import "fmt"

// FailCode is a run failure code (spec 8.7). The numbering is part of the
// module format: it is the operand of FAIL.
type FailCode uint16

// Failure codes of spec 8.7, numbered in the order the spec lists them.
// FailTypeMismatch is an extension (see OPEN_QUESTIONS.md): the verifier
// checks stack shape, not static types, so a module that was not produced
// by a correct compiler can still apply an operation to the wrong kind of
// value; the VM fails the run with this code instead of misbehaving.
const (
	FailArithOverflow          FailCode = 1
	FailArithNaN               FailCode = 2
	FailDivZero                FailCode = 3
	FailIndexOOB               FailCode = 4
	FailMatchFailed            FailCode = 5
	FailUnwrapFailed           FailCode = 6
	FailApprovalDenied         FailCode = 7
	FailScopeDenied            FailCode = 8
	FailEffectFailed           FailCode = 9
	FailTimeout                FailCode = 10
	FailBudgetExceeded         FailCode = 11
	FailOutOfFuel              FailCode = 12
	FailNondeterminismDetected FailCode = 13
	FailLimitExceeded          FailCode = 14
	FailCancelled              FailCode = 15
	FailTypeMismatch           FailCode = 16
)

var failNames = [...]string{
	FailArithOverflow:          "arith_overflow",
	FailArithNaN:               "arith_nan",
	FailDivZero:                "div_zero",
	FailIndexOOB:               "index_oob",
	FailMatchFailed:            "match_failed",
	FailUnwrapFailed:           "unwrap_failed",
	FailApprovalDenied:         "approval_denied",
	FailScopeDenied:            "scope_denied",
	FailEffectFailed:           "effect_failed",
	FailTimeout:                "timeout",
	FailBudgetExceeded:         "budget_exceeded",
	FailOutOfFuel:              "out_of_fuel",
	FailNondeterminismDetected: "nondeterminism_detected",
	FailLimitExceeded:          "limit_exceeded",
	FailCancelled:              "cancelled",
	FailTypeMismatch:           "type_mismatch",
}

// Valid reports whether c is a known failure code.
func (c FailCode) Valid() bool { return c != 0 && int(c) < len(failNames) }

// String returns the snake_case failure name, or "fail(N)".
func (c FailCode) String() string {
	if c.Valid() {
		return failNames[c]
	}
	return fmt.Sprintf("fail(%d)", uint16(c))
}

// ParseFailCode maps a code name back to its number.
func ParseFailCode(s string) (FailCode, bool) {
	for i, n := range failNames {
		if n != "" && n == s {
			return FailCode(i), true
		}
	}
	return 0, false
}
