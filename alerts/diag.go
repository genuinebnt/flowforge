package alerts

import (
	"fmt"
	"sort"
	"unicode/utf8"
)

// Diagnostic codes of the alert rule language. They follow the layout of
// the DSL ranges (spec 7.7) under an "A" prefix so the two languages never
// share a code:
//
//	A0001-A0099  lexing
//	A0100-A0199  parsing
//	A0200-A0299  names (variables, fields, functions, actions, severities)
//	A0300-A0399  types
//	A0400-A0499  values and limits (regex, durations, quiet hours)
const (
	CodeUnterminatedString  = "A0001"
	CodeInvalidEscape       = "A0002"
	CodeIntegerTooLarge     = "A0003"
	CodeUnknownDurationUnit = "A0004"
	CodeUnexpectedChar      = "A0005"
	CodeUnterminatedComment = "A0006"
	CodeInterpolation       = "A0007"
	CodeInvalidNumber       = "A0008"
	CodeInvalidUTF8         = "A0009"

	CodeUnexpectedToken = "A0101"
	CodeExpected        = "A0102"
	CodeNonAssociative  = "A0103"
	CodeDuplicateClause = "A0104"
	CodeUnsupported     = "A0105"
	CodeTooDeep         = "A0106"
	CodeTooLarge        = "A0107"

	CodeUnknownVariable = "A0201"
	CodeUnknownField    = "A0202"
	CodeUnknownFunction = "A0203"
	CodeUnknownAction   = "A0204"
	CodeUnknownSeverity = "A0205"
	CodeUnknownArgument = "A0206"

	CodeOperandType      = "A0301"
	CodeConditionNotBool = "A0302"
	CodeArity            = "A0303"
	CodeArgumentType     = "A0304"
	CodeGroupByType      = "A0305"
	CodeActionArgument   = "A0306"
	CodeSeverityConflict = "A0307"

	CodeInvalidRegex    = "A0401"
	CodeRegexTooLarge   = "A0402"
	CodeInvalidDuration = "A0403"
	CodeInvalidQuiet    = "A0404"
	CodeUnknownZone     = "A0405"
	CodeInvalidAddress  = "A0406"
)

// DiagError and DiagWarning are the values of Diagnostic.Severity.
const (
	DiagError   = "error"
	DiagWarning = "warning"
)

// Position is a point in the source: 1-based line, 1-based column counted
// in Unicode code points, and 0-based byte offset.
type Position struct {
	Line   int `json:"line"`
	Col    int `json:"col"`
	Offset int `json:"offset"`
}

// Label is a labelled source span (the "primary" and "secondary" objects
// of spec 7.7). End is exclusive.
type Label struct {
	File  string   `json:"file"`
	Start Position `json:"start"`
	End   Position `json:"end"`
	Label string   `json:"label,omitempty"`
}

// Diagnostic is one compiler message in the standard format of spec 7.7.
type Diagnostic struct {
	Code      string  `json:"code"`
	Severity  string  `json:"severity"`
	Message   string  `json:"message"`
	Primary   Label   `json:"primary"`
	Secondary []Label `json:"secondary,omitempty"`
	Help      string  `json:"help,omitempty"`
}

// Error implements error so a diagnostic can travel as one.
func (d Diagnostic) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s %s: %s", d.Primary.File, d.Primary.Start.Line, d.Primary.Start.Col, d.Severity, d.Code, d.Message)
}

// HasErrors reports whether any diagnostic has error severity.
func HasErrors(ds []Diagnostic) bool {
	for _, d := range ds {
		if d.Severity == DiagError {
			return true
		}
	}
	return false
}

// span is a half-open byte range [start, end) in the source.
type span struct{ start, end int }

func (s span) to(o span) span { return span{s.start, o.end} }

// diagSink collects diagnostics and converts byte offsets to positions.
type diagSink struct {
	file  string
	src   string
	lines []int // byte offset of the start of each line
	diags []Diagnostic
}

func newDiagSink(file, src string) *diagSink {
	lines := []int{0}
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			lines = append(lines, i+1)
		}
	}
	return &diagSink{file: file, src: src, lines: lines}
}

// pos converts a byte offset to a 1-based line and column. Columns count
// code points when the line prefix is valid UTF-8 and bytes otherwise.
func (s *diagSink) pos(off int) Position {
	off = max(0, min(off, len(s.src)))
	line := sort.Search(len(s.lines), func(i int) bool { return s.lines[i] > off }) - 1
	start := s.lines[line]
	col := 1
	if utf8.ValidString(s.src[start:off]) {
		col += utf8.RuneCountInString(s.src[start:off])
	} else {
		col += off - start
	}
	return Position{Line: line + 1, Col: col, Offset: off}
}

func (s *diagSink) label(sp span, text string) Label {
	if sp.end < sp.start {
		sp.end = sp.start
	}
	return Label{File: s.file, Start: s.pos(sp.start), End: s.pos(sp.end), Label: text}
}

// add records an error diagnostic and returns it for optional decoration.
func (s *diagSink) add(code string, sp span, label, format string, args ...any) *Diagnostic {
	s.diags = append(s.diags, Diagnostic{
		Code:     code,
		Severity: DiagError,
		Message:  fmt.Sprintf(format, args...),
		Primary:  s.label(sp, label),
	})
	return &s.diags[len(s.diags)-1]
}

// sorted returns the diagnostics in source order (stable for equal spans).
func (s *diagSink) sorted() []Diagnostic {
	out := append([]Diagnostic(nil), s.diags...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Primary.Start.Offset < out[j].Primary.Start.Offset
	})
	return out
}
