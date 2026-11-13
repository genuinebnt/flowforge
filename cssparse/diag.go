package cssparse

import "strconv"

// DiagCode is a stable diagnostic code. CSS has no fatal errors: every
// parse error is recorded and parsing continues with the recovery the
// spec prescribes (V2_DESIGN §1.8).
type DiagCode uint8

// Diagnostic codes. The String forms are stable and shared with the Rust
// port and the conformance data.
const (
	DiagNone               DiagCode = iota
	DiagEOFInComment                // §4.3.2: comment not closed before EOF
	DiagEOFInString                 // §4.3.5: string not closed before EOF
	DiagNewlineInString             // §4.3.5: newline in string (bad-string)
	DiagEOFInURL                    // §4.3.6: url( not closed before EOF
	DiagBadURL                      // §4.3.6: quote, "(", non-printable or bad escape in url( (bad-url)
	DiagEOFInEscape                 // §4.3.7: "\" immediately before EOF
	DiagInvalidEscape               // §4.3.1: "\" followed by a newline outside a string
	DiagInvalidRule                 // §5.4.3: qualified rule ended by EOF or stop token
	DiagInvalidDeclaration          // §5.4.6: declaration without name or colon
	DiagEOFInAtRule                 // §5.4.2: at-rule ended by EOF
	DiagEOFInBlock                  // §5.4.8/§5.4.9: block or function not closed before EOF
	DiagEmpty                       // §5.3: "parse a ..." found no input
	DiagExtraInput                  // §5.3: "parse a ..." found trailing input
	DiagUnexpectedClose             // a "}" where a block's contents expected a rule or declaration
	DiagDepthLimit                  // block nesting deeper than Options.MaxDepth; the excess was elided
	DiagInputTooLarge               // input longer than Options.MaxInputBytes; the rest was not read
	DiagLimitExceeded               // more items than Options.MaxURLs; the rest were not extracted

	diagCodeCount
)

var diagNames = [...]string{
	"none", "eof_in_comment", "eof_in_string", "newline_in_string", "eof_in_url",
	"bad_url", "eof_in_escape", "invalid_escape", "invalid_rule",
	"invalid_declaration", "eof_in_at_rule", "eof_in_block", "empty",
	"extra_input", "unexpected_close", "depth_limit", "input_too_large",
	"limit_exceeded",
}

// IsLimit reports whether c records a resource limit being hit, which
// makes a result Partial (CSS-INV-08).
func (c DiagCode) IsLimit() bool {
	return c == DiagDepthLimit || c == DiagInputTooLarge || c == DiagLimitExceeded
}

// String returns the stable name of c.
func (c DiagCode) String() string {
	if int(c) < len(diagNames) {
		return diagNames[c]
	}
	return "diag(" + strconv.Itoa(int(c)) + ")"
}

// Diagnostic is one parse error with the byte span it concerns.
type Diagnostic struct {
	Code       DiagCode
	Start, End int32
}

// Grade says how completely a result reflects its input, so callers can
// tell "read fine, found nothing" from "could not read". The same four
// grades, with the same names, exist in xmlparse, sourcemap and wellknown.
type Grade uint8

const (
	// GradeComplete: the whole input was read; recoverable CSS parse
	// errors were handled by the spec's recovery rules.
	GradeComplete Grade = iota
	// GradePartial: a limit (input size, nesting depth, item count)
	// truncated the read.
	GradePartial
	// GradeUnsupported: the input uses a feature this package refuses.
	// cssparse never produces it; it exists for parity with the other
	// parsers.
	GradeUnsupported
	// GradeFailed: the input could not be read. cssparse never produces
	// it (CSS parsing is total).
	GradeFailed
)

var gradeNames = [...]string{"complete", "partial", "unsupported", "failed"}

// String returns the stable name of g.
func (g Grade) String() string {
	if int(g) < len(gradeNames) {
		return gradeNames[g]
	}
	return "grade(" + strconv.Itoa(int(g)) + ")"
}

// gradeOf derives the grade from diagnostics.
func gradeOf(diags []Diagnostic) Grade {
	for _, d := range diags {
		if d.Code.IsLimit() {
			return GradePartial
		}
	}
	return GradeComplete
}
