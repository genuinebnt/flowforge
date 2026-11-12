package dsl

import (
	"fmt"
	"sort"
)

// Severity of a diagnostic.
type Severity string

// Diagnostic severities (spec 7.7, 11.2).
const (
	SevError   Severity = "error"
	SevWarning Severity = "warning"
	SevHint    Severity = "hint"
)

// Diagnostic codes. The ranges follow spec 7.7; codes not named in the
// spec are Flowforge Go decisions recorded in OPEN_QUESTIONS.md.
const (
	// Lexing (E0001-E0099).
	CodeUnterminatedString  = "E0001"
	CodeInvalidEscape       = "E0002"
	CodeLiteralTooLarge     = "E0003"
	CodeUnknownDurationUnit = "E0004"
	CodeUnexpectedChar      = "E0005"
	CodeUnterminatedComment = "E0006"
	CodeMalformedNumber     = "E0007"
	CodeBadInterpolation    = "E0008"

	// Parsing (E0100-E0199).
	CodeUnexpectedToken = "E0101"
	CodeMissingSemi     = "E0102"
	CodeNonAssocChain   = "E0103"

	// Names (E0200-E0299).
	CodeUnknownName    = "E0201"
	CodeDuplicateBind  = "E0202"
	CodeUnknownTool    = "E0203"
	CodeUnknownType    = "E0204"
	CodeDuplicateItem  = "E0205"
	CodeUnknownVariant = "E0206"

	// Types (E0300-E0399).
	CodeTypeMismatch     = "E0301"
	CodeScopeProofMisuse = "E0302"
	CodeArity            = "E0303"
	CodeCannotInfer      = "E0304"
	CodeNotCallable      = "E0305"
	CodeBadTry           = "E0306"
	CodeBadField         = "E0307"
	CodeBadNamedArg      = "E0308"
	CodeBadCast          = "E0309"
	CodeBadOperand       = "E0310"
	CodeNotDisplayable   = "E0311"
	CodeImmutable        = "E0312"
	CodeBadReturn        = "E0313"
	CodeBadMapKey        = "E0314"

	// Effects and policy (E0400-E0499).
	CodeEffectNotDeclared = "E0401"
	CodeEffectExceedsMax  = "E0402"
	CodeMissingApproval   = "E0403"
	CodeBadCompensate     = "E0404"
	CodeUnknownEffect     = "E0405"

	// Patterns (E0500-E0599).
	CodeNonExhaustive  = "E0501"
	CodeRefutable      = "E0502"
	CodePatternMisuse  = "E0503"
	CodeNoSuchVariant  = "E0504"
	CodePatternArity   = "E0505"
	CodeBadLitPattern  = "E0506"
	CodeBadRecordPat   = "E0507"
	CodeDupPatternBind = "E0202" // same code as a duplicate binding

	// Annotations and limits (E0600-E0699).
	CodeUnknownAnnotation   = "E0601"
	CodeTooLarge            = "E0602"
	CodeMisplacedAnnotation = "E0603"
	CodeBadAnnotationArgs   = "E0604"

	// Warnings.
	WarnNaming      = "W0101"
	WarnOverBroad   = "W0402"
	WarnUnreachable = "W0501"
	WarnDeprecated  = "W0601"
)

// SourceSpan is a labelled span in a named file, the shape used by
// Diagnostic.Primary and Diagnostic.Secondary (spec 7.7).
type SourceSpan struct {
	File  string `json:"file"`
	Start Pos    `json:"start"`
	End   Pos    `json:"end"`
	Label string `json:"label"`
}

// Diagnostic is a compiler message in the JSON shape of spec 7.7.
type Diagnostic struct {
	Code      string       `json:"code"`
	Severity  Severity     `json:"severity"`
	Message   string       `json:"message"`
	Primary   SourceSpan   `json:"primary"`
	Secondary []SourceSpan `json:"secondary"`
	Help      string       `json:"help,omitempty"`
}

// Error implements error so a diagnostic can be wrapped by callers.
func (d Diagnostic) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s %s: %s", d.Primary.File, d.Primary.Start.Line,
		d.Primary.Start.Col, d.Severity, d.Code, d.Message)
}

// HasErrors reports whether any diagnostic is an error.
func HasErrors(ds []Diagnostic) bool {
	for _, d := range ds {
		if d.Severity == SevError {
			return true
		}
	}
	return false
}

// SortDiagnostics orders diagnostics by primary file and start offset,
// keeping the production order of diagnostics at the same position.
func SortDiagnostics(ds []Diagnostic) {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i].Primary, ds[j].Primary
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Start.Offset < b.Start.Offset
	})
}

// diagSink collects diagnostics for one file.
type diagSink struct {
	file  string
	diags []Diagnostic
	// errorsAt suppresses duplicate errors at the same offset with the
	// same code, which recovery can otherwise produce.
	seen map[[2]int]bool
}

func newDiagSink(file string) *diagSink {
	return &diagSink{file: file, seen: map[[2]int]bool{}}
}

func (s *diagSink) span(sp Span, label string) SourceSpan {
	return SourceSpan{File: s.file, Start: sp.Start, End: sp.End, Label: label}
}

// add appends a diagnostic and returns a pointer to it so the caller can
// attach secondary spans or help.
func (s *diagSink) add(code string, sev Severity, sp Span, label, msg string) *Diagnostic {
	s.diags = append(s.diags, Diagnostic{
		Code: code, Severity: sev, Message: msg,
		Primary:   s.span(sp, label),
		Secondary: []SourceSpan{},
	})
	return &s.diags[len(s.diags)-1]
}

func (s *diagSink) errorf(code string, sp Span, label, format string, args ...any) *Diagnostic {
	return s.add(code, SevError, sp, label, fmt.Sprintf(format, args...))
}

func (s *diagSink) warnf(code string, sp Span, label, format string, args ...any) *Diagnostic {
	return s.add(code, SevWarning, sp, label, fmt.Sprintf(format, args...))
}

// errorOnce reports an error unless one with the same code was already
// reported at the same start offset.
func (s *diagSink) errorOnce(code string, sp Span, label, format string, args ...any) *Diagnostic {
	key := [2]int{sp.Start.Offset, codeNum(code)}
	if s.seen[key] {
		return &Diagnostic{}
	}
	s.seen[key] = true
	return s.errorf(code, sp, label, format, args...)
}

func codeNum(code string) int {
	n := 0
	for _, r := range code {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
		}
	}
	if len(code) > 0 && code[0] == 'W' {
		n += 100000
	}
	return n
}

// withSecondary appends a secondary span.
func (d *Diagnostic) withSecondary(file string, sp Span, label string) *Diagnostic {
	d.Secondary = append(d.Secondary, SourceSpan{File: file, Start: sp.Start, End: sp.End, Label: label})
	return d
}

// withHelp sets the help text.
func (d *Diagnostic) withHelp(format string, args ...any) *Diagnostic {
	d.Help = fmt.Sprintf(format, args...)
	return d
}
