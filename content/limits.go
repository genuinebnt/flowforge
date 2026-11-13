package content

// Amplification bounds of spec 26.4 (V2 §12.10). They are identical in the Go and
// Rust implementations; a document that hits one is read partially
// (grade partial), never fatally.
const (
	// MaxResponseBytes is the largest body read: 8 MiB. Longer bodies are
	// cut and the capture is partial.
	MaxResponseBytes = 8 << 20
	// MaxParseDepth is the XML and CSS nesting limit: 64.
	MaxParseDepth = 64
	// MaxFactsPerParser caps the facts (features) one extractor may emit
	// for one document: 20,000.
	MaxFactsPerParser = 20_000
	// MaxInnerDocuments caps the derived documents (inline scripts,
	// styles, handlers, source-map originals) of one capture: 200.
	MaxInnerDocuments = 200
	// MaxHTMLDepth is the HTML open-element limit. HTML is not in the
	// §12.10 list; real pages nest deeper than 64, so htmlparse's own
	// default (512) is kept.
	MaxHTMLDepth = 512
)

// Limits carries the bounds; each zero field means the default constant
// of the same name above.
type Limits struct {
	MaxResponseBytes  int
	MaxParseDepth     int
	MaxFactsPerParser int
	MaxInnerDocuments int
}

// withDefaults fills the zero fields.
func (l Limits) withDefaults() Limits {
	if l.MaxResponseBytes <= 0 {
		l.MaxResponseBytes = MaxResponseBytes
	}
	if l.MaxParseDepth <= 0 {
		l.MaxParseDepth = MaxParseDepth
	}
	if l.MaxFactsPerParser <= 0 {
		l.MaxFactsPerParser = MaxFactsPerParser
	}
	if l.MaxInnerDocuments <= 0 {
		l.MaxInnerDocuments = MaxInnerDocuments
	}
	return l
}

// Grade is how completely a reader read its input (V2 §12.10). The same
// four names exist in cssparse, xmlparse, sourcemap and wellknown; "read
// fine, found nothing" (complete, no facts) is never confused with
// "could not read" (failed).
type Grade string

// Reading grades.
const (
	GradeComplete    Grade = "complete"
	GradePartial     Grade = "partial"
	GradeUnsupported Grade = "unsupported"
	GradeFailed      Grade = "failed"
)

// rank orders grades from best (complete) to worst (failed); an unknown
// grade ranks as failed.
func (g Grade) rank() int {
	switch g {
	case GradeComplete:
		return 0
	case GradePartial:
		return 1
	case GradeUnsupported:
		return 2
	}
	return 3
}

// worseGrade returns the worse of two grades.
func worseGrade(a, b Grade) Grade {
	if b.rank() > a.rank() {
		return b
	}
	return a
}
