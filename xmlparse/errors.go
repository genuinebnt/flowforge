package xmlparse

import (
	"fmt"
	"strconv"
)

// ErrorCode is a stable well-formedness or policy error code. XML
// well-formedness errors are fatal (XML 1.0 §1.2 "fatal error"): the
// decoder stops at the first one.
type ErrorCode uint8

// Error codes. The String forms are stable and shared with the Rust port
// and the conformance data.
const (
	ErrNone ErrorCode = iota
	// EOF-class errors: detected only at the end of input; Offset is the
	// input length.
	ErrUnexpectedEOF // input ended inside markup or with open elements
	ErrNoRootElement // input ended before any element
	// Character-level errors.
	ErrInvalidUTF8 // a byte sequence that is not UTF-8
	ErrInvalidChar // a code point outside the Char production [2]
	// Grammar errors.
	ErrUnexpectedChar      // a character the grammar does not allow here
	ErrInvalidName         // a Name [5] that does not start with a NameStartChar, or a colon where none is allowed
	ErrInvalidQName        // a QName (Namespaces §4) with an empty part or two colons
	ErrTextOutsideRoot     // character data or a reference before or after the root element
	ErrMultipleRoots       // a second top-level element
	ErrUnexpectedEndTag    // an end tag with no open element
	ErrMismatchedEndTag    // WFC: Element Type Match [39]
	ErrDuplicateAttribute  // WFC: Unique Att Spec [40]; Namespaces §6.3 for expanded names
	ErrLtInAttribute       // WFC: No < in Attribute Values [41]
	ErrInvalidReference    // malformed Reference [67]
	ErrUndefinedEntity     // WFC: Entity Declared [68] (only the five predefined entities exist)
	ErrInvalidCharRef      // WFC: Legal Character [66]
	ErrDoubleHyphenComment // "--" inside a Comment [15]
	ErrCDataEndInContent   // "]]>" in CharData [14]
	ErrReservedPITarget    // PITarget [17] matching "xml" outside the XML declaration
	ErrInvalidXMLDecl      // malformed XMLDecl [23]
	ErrMisplacedDoctype    // doctypedecl [28] after the root, twice, or in content
	ErrUnboundPrefix       // Namespaces §5 "Namespace Constraint: Prefix Declared"
	ErrReservedPrefix      // Namespaces §3 constraints on xml and xmlns
	ErrInvalidNamespaceDecl
	// Policy errors (grade "unsupported").
	ErrUnsupportedEncoding // encoding other than UTF-8 / UTF-16 with BOM
	ErrDTDInternalSubset   // <!DOCTYPE with an internal subset: refused (XXE / entity expansion)
	ErrDTDExternalID       // <!DOCTYPE with SYSTEM or PUBLIC: refused (XXE)
	// Limit errors (grade "partial").
	ErrDepthLimit    // element nesting deeper than Options.MaxDepth
	ErrInputTooLarge // input longer than Options.MaxInputBytes

	errorCodeCount
)

var errorNames = [...]string{
	"none", "unexpected_eof", "no_root_element", "invalid_utf8", "invalid_char",
	"unexpected_char", "invalid_name", "invalid_qname", "text_outside_root",
	"multiple_roots", "unexpected_end_tag", "mismatched_end_tag",
	"duplicate_attribute", "lt_in_attribute", "invalid_reference",
	"undefined_entity", "invalid_char_ref", "double_hyphen_in_comment",
	"cdata_end_in_content", "reserved_pi_target", "invalid_xml_decl",
	"misplaced_doctype", "unbound_prefix", "reserved_prefix",
	"invalid_namespace_decl", "unsupported_encoding", "dtd_internal_subset",
	"dtd_external_id", "depth_limit", "input_too_large",
}

// String returns the stable name of c.
func (c ErrorCode) String() string {
	if int(c) < len(errorNames) {
		return errorNames[c]
	}
	return "error(" + strconv.Itoa(int(c)) + ")"
}

// EOFClass reports whether c can only be detected at the end of input.
func (c ErrorCode) EOFClass() bool { return c == ErrUnexpectedEOF || c == ErrNoRootElement }

// Grade maps the code to the reading grade of a result that stopped
// with it.
func (c ErrorCode) Grade() Grade {
	switch c {
	case ErrNone:
		return GradeComplete
	case ErrDepthLimit, ErrInputTooLarge:
		return GradePartial
	case ErrUnsupportedEncoding, ErrDTDInternalSubset, ErrDTDExternalID:
		return GradeUnsupported
	}
	return GradeFailed
}

// SyntaxError is the first error found. Offset is the first offending
// byte, at character granularity: input[:Offset] can still be extended to
// a well-formed document, input[:Offset+w] cannot, where w is the length
// of the UTF-8 character starting at Offset (1 for a byte that starts no
// valid sequence), and what follows does not change the error
// (XML-INV-02). For invalid UTF-8 it is the first byte that makes the
// sequence invalid. EOF-class errors carry Offset = input length. For UTF-16 input, offsets index the transcoded
// UTF-8 buffer (Document.UTF16 is set).
type SyntaxError struct {
	Code   ErrorCode
	Offset int
	// Line and Column are 1-based; Column counts bytes. CR LF and CR
	// count as one line break.
	Line, Column int
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("xmlparse: %s at line %d, column %d (offset %d)", e.Code, e.Line, e.Column, e.Offset)
}

// Grade says how completely a result reflects its input. The same four
// grades exist in cssparse, sourcemap and wellknown.
type Grade uint8

const (
	// GradeComplete: the whole document was read and is well-formed.
	GradeComplete Grade = iota
	// GradePartial: a limit (depth, input size) stopped the read; what was
	// read before it is returned.
	GradePartial
	// GradeUnsupported: a refused feature (DOCTYPE subset or external ID,
	// unsupported encoding) stopped the read.
	GradeUnsupported
	// GradeFailed: the document is not well-formed.
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

// GradeOf returns the grade of a result that ended with err (nil, or a
// *SyntaxError).
func GradeOf(err error) Grade {
	if err == nil {
		return GradeComplete
	}
	if se, ok := err.(*SyntaxError); ok {
		return se.Code.Grade()
	}
	return GradeFailed
}
