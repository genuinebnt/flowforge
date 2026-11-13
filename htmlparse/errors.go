package htmlparse

import (
	"errors"
	"strconv"
)

// ErrorCode is a stable parse-error code. Tokenizer and input-stream
// errors use the WHATWG names from HTML §13.2.2 ("Parse errors"). The
// standard does not name tree-construction errors, so those use the
// Flowforge names listed below (the html5lib vocabulary where one exists).
// Limit errors are Flowforge codes too. The list is closed: the Rust port
// declares exactly these variants in this order.
type ErrorCode uint8

// Parse-error codes. The String method returns the stable name.
const (
	// --- WHATWG tokenizer / input stream errors (HTML §13.2.2) ---
	ErrAbruptClosingOfEmptyComment ErrorCode = iota
	ErrAbruptDoctypePublicIdentifier
	ErrAbruptDoctypeSystemIdentifier
	ErrAbsenceOfDigitsInNumericCharacterReference
	ErrCDATAInHTMLContent
	ErrCharacterReferenceOutsideUnicodeRange
	ErrControlCharacterInInputStream
	ErrControlCharacterReference
	ErrDisallowedProcessingInstructionTarget
	ErrDuplicateAttribute
	ErrEndTagWithAttributes
	ErrEndTagWithTrailingSolidus
	ErrEOFBeforeTagName
	ErrEOFInCDATA
	ErrEOFInComment
	ErrEOFInDoctype
	ErrEOFInProcessingInstruction
	ErrEOFInScriptHTMLCommentLikeText
	ErrEOFInTag
	ErrIncorrectlyClosedComment
	ErrIncorrectlyOpenedComment
	ErrInvalidCharacterSequenceAfterDoctypeName
	ErrInvalidFirstCharacterOfProcessingInstructionTarget
	ErrInvalidFirstCharacterOfTagName
	ErrInvalidProcessingInstructionTarget
	ErrMissingAttributeValue
	ErrMissingDoctypeName
	ErrMissingDoctypePublicIdentifier
	ErrMissingDoctypeSystemIdentifier
	ErrMissingEndTagName
	ErrMissingQuoteBeforeDoctypePublicIdentifier
	ErrMissingQuoteBeforeDoctypeSystemIdentifier
	ErrMissingSemicolonAfterCharacterReference
	ErrMissingWhitespaceAfterDoctypePublicKeyword
	ErrMissingWhitespaceAfterDoctypeSystemKeyword
	ErrMissingWhitespaceBeforeDoctypeName
	ErrMissingWhitespaceBetweenAttributes
	ErrMissingWhitespaceBetweenDoctypePublicAndSystemIdentifiers
	ErrNestedComment
	ErrNonVoidHTMLElementStartTagWithTrailingSolidus
	ErrNoncharacterCharacterReference
	ErrNoncharacterInInputStream
	ErrNullCharacterReference
	ErrSurrogateCharacterReference
	ErrSurrogateInInputStream
	ErrUnexpectedCharacterAfterDoctypeSystemIdentifier
	ErrUnexpectedCharacterInAttributeName
	ErrUnexpectedCharacterInUnquotedAttributeValue
	ErrUnexpectedEqualsSignBeforeAttributeName
	ErrUnexpectedNullCharacter
	ErrUnexpectedSolidusInTag
	ErrUnknownNamedCharacterReference

	// --- Flowforge tree-construction errors (the spec only says "parse error") ---

	// ErrMissingDoctype: the first token was not a DOCTYPE (initial mode,
	// "anything else"); the document is put in quirks mode.
	ErrMissingDoctype
	// ErrNonConformingDoctype: a DOCTYPE other than <!DOCTYPE html> or the
	// about:legacy-compat form.
	ErrNonConformingDoctype
	// ErrUnexpectedDoctype: a DOCTYPE token after the initial mode.
	ErrUnexpectedDoctype
	// ErrUnexpectedStartTag: a start tag the current insertion mode
	// ignores or treats as misplaced.
	ErrUnexpectedStartTag
	// ErrUnexpectedEndTag: an end tag with no matching open element, or one
	// the insertion mode ignores.
	ErrUnexpectedEndTag
	// ErrUnexpectedCharacter: non-whitespace text where none is allowed
	// (tables, framesets, after the body).
	ErrUnexpectedCharacter
	// ErrUnclosedElement: an element was implicitly closed (the "current
	// node is not X" checks, adoption agency misnesting).
	ErrUnclosedElement
	// ErrEOFInElement: end of file while elements other than the implied
	// ones were still open.
	ErrEOFInElement
	// ErrXMLNSAttributeMismatch: an xmlns or xmlns:xlink attribute on a
	// foreign element disagrees with the element's namespace.
	ErrXMLNSAttributeMismatch

	// --- Flowforge limit errors (Options); parsing continues fail-closed ---

	// ErrNestingDepthLimit: an element would have exceeded
	// Options.MaxDepth; it was inserted childless instead of being opened.
	ErrNestingDepthLimit
	// ErrAttributeLimit: a tag had more than Options.MaxAttributes
	// attributes; the excess attributes were dropped.
	ErrAttributeLimit
	// ErrInputSizeLimit: the input exceeded Options.MaxInputSize; only the
	// prefix was parsed.
	ErrInputSizeLimit

	numErrorCodes
)

var errorCodeNames = [numErrorCodes]string{
	ErrAbruptClosingOfEmptyComment:                               "abrupt-closing-of-empty-comment",
	ErrAbruptDoctypePublicIdentifier:                             "abrupt-doctype-public-identifier",
	ErrAbruptDoctypeSystemIdentifier:                             "abrupt-doctype-system-identifier",
	ErrAbsenceOfDigitsInNumericCharacterReference:                "absence-of-digits-in-numeric-character-reference",
	ErrCDATAInHTMLContent:                                        "cdata-in-html-content",
	ErrCharacterReferenceOutsideUnicodeRange:                     "character-reference-outside-unicode-range",
	ErrControlCharacterInInputStream:                             "control-character-in-input-stream",
	ErrControlCharacterReference:                                 "control-character-reference",
	ErrDisallowedProcessingInstructionTarget:                     "disallowed-processing-instruction-target",
	ErrDuplicateAttribute:                                        "duplicate-attribute",
	ErrEndTagWithAttributes:                                      "end-tag-with-attributes",
	ErrEndTagWithTrailingSolidus:                                 "end-tag-with-trailing-solidus",
	ErrEOFBeforeTagName:                                          "eof-before-tag-name",
	ErrEOFInCDATA:                                                "eof-in-cdata",
	ErrEOFInComment:                                              "eof-in-comment",
	ErrEOFInDoctype:                                              "eof-in-doctype",
	ErrEOFInProcessingInstruction:                                "eof-in-processing-instruction",
	ErrEOFInScriptHTMLCommentLikeText:                            "eof-in-script-html-comment-like-text",
	ErrEOFInTag:                                                  "eof-in-tag",
	ErrIncorrectlyClosedComment:                                  "incorrectly-closed-comment",
	ErrIncorrectlyOpenedComment:                                  "incorrectly-opened-comment",
	ErrInvalidCharacterSequenceAfterDoctypeName:                  "invalid-character-sequence-after-doctype-name",
	ErrInvalidFirstCharacterOfProcessingInstructionTarget:        "invalid-first-character-of-processing-instruction-target",
	ErrInvalidFirstCharacterOfTagName:                            "invalid-first-character-of-tag-name",
	ErrInvalidProcessingInstructionTarget:                        "invalid-processing-instruction-target",
	ErrMissingAttributeValue:                                     "missing-attribute-value",
	ErrMissingDoctypeName:                                        "missing-doctype-name",
	ErrMissingDoctypePublicIdentifier:                            "missing-doctype-public-identifier",
	ErrMissingDoctypeSystemIdentifier:                            "missing-doctype-system-identifier",
	ErrMissingEndTagName:                                         "missing-end-tag-name",
	ErrMissingQuoteBeforeDoctypePublicIdentifier:                 "missing-quote-before-doctype-public-identifier",
	ErrMissingQuoteBeforeDoctypeSystemIdentifier:                 "missing-quote-before-doctype-system-identifier",
	ErrMissingSemicolonAfterCharacterReference:                   "missing-semicolon-after-character-reference",
	ErrMissingWhitespaceAfterDoctypePublicKeyword:                "missing-whitespace-after-doctype-public-keyword",
	ErrMissingWhitespaceAfterDoctypeSystemKeyword:                "missing-whitespace-after-doctype-system-keyword",
	ErrMissingWhitespaceBeforeDoctypeName:                        "missing-whitespace-before-doctype-name",
	ErrMissingWhitespaceBetweenAttributes:                        "missing-whitespace-between-attributes",
	ErrMissingWhitespaceBetweenDoctypePublicAndSystemIdentifiers: "missing-whitespace-between-doctype-public-and-system-identifiers",
	ErrNestedComment:                                             "nested-comment",
	ErrNonVoidHTMLElementStartTagWithTrailingSolidus:             "non-void-html-element-start-tag-with-trailing-solidus",
	ErrNoncharacterCharacterReference:                            "noncharacter-character-reference",
	ErrNoncharacterInInputStream:                                 "noncharacter-in-input-stream",
	ErrNullCharacterReference:                                    "null-character-reference",
	ErrSurrogateCharacterReference:                               "surrogate-character-reference",
	ErrSurrogateInInputStream:                                    "surrogate-in-input-stream",
	ErrUnexpectedCharacterAfterDoctypeSystemIdentifier:           "unexpected-character-after-doctype-system-identifier",
	ErrUnexpectedCharacterInAttributeName:                        "unexpected-character-in-attribute-name",
	ErrUnexpectedCharacterInUnquotedAttributeValue:               "unexpected-character-in-unquoted-attribute-value",
	ErrUnexpectedEqualsSignBeforeAttributeName:                   "unexpected-equals-sign-before-attribute-name",
	ErrUnexpectedNullCharacter:                                   "unexpected-null-character",
	ErrUnexpectedSolidusInTag:                                    "unexpected-solidus-in-tag",
	ErrUnknownNamedCharacterReference:                            "unknown-named-character-reference",

	ErrMissingDoctype:         "missing-doctype",
	ErrNonConformingDoctype:   "non-conforming-doctype",
	ErrUnexpectedDoctype:      "unexpected-doctype",
	ErrUnexpectedStartTag:     "unexpected-start-tag",
	ErrUnexpectedEndTag:       "unexpected-end-tag",
	ErrUnexpectedCharacter:    "unexpected-character",
	ErrUnclosedElement:        "unclosed-element",
	ErrEOFInElement:           "eof-in-element",
	ErrXMLNSAttributeMismatch: "xmlns-attribute-mismatch",

	ErrNestingDepthLimit: "nesting-depth-limit-exceeded",
	ErrAttributeLimit:    "attribute-limit-exceeded",
	ErrInputSizeLimit:    "input-size-limit-exceeded",
}

// String returns the stable name of the code, for example
// "unexpected-null-character".
func (c ErrorCode) String() string {
	if c < numErrorCodes {
		return errorCodeNames[c]
	}
	return "error-code-" + strconv.Itoa(int(c))
}

// ErrorCodeByName returns the code with the given stable name.
func ErrorCodeByName(name string) (ErrorCode, bool) {
	for i, n := range errorCodeNames {
		if n == name {
			return ErrorCode(i), true
		}
	}
	return 0, false
}

// IsTokenizerError reports whether c is one of the WHATWG-named
// tokenizer or input-stream errors (as opposed to a tree-construction or
// limit error).
func (c ErrorCode) IsTokenizerError() bool { return c <= ErrUnknownNamedCharacterReference }

// ParseError is one parse error. Offset is a byte offset into the input
// that was given to Tokenize, Parse or ParseFragment (after decoding, never
// into raw transport bytes). Line and Col are 1-based; Col counts UTF-16
// code units (as html5lib and browser developer tools do, so a
// supplementary-plane character counts 2), a CR LF pair is one line
// break, and a lone CR is a line break. For end-of-file errors Offset is
// len(input). The error is located at the input character being consumed
// when the error was detected.
//
// PORT: Col is in UTF-16 code units, not bytes or chars.
type ParseError struct {
	Code   ErrorCode
	Offset int
	Line   int
	Col    int
}

// Error implements error.
func (e ParseError) Error() string {
	return "htmlparse: " + strconv.Itoa(e.Line) + ":" + strconv.Itoa(e.Col) + ": " + e.Code.String()
}

// ErrInvalidNode is returned by the Document mutation helpers for an
// out-of-range NodeID or a node of the wrong kind.
var ErrInvalidNode = errors.New("htmlparse: invalid node")

// ErrHierarchy is returned by the Document mutation helpers when an
// operation would make a node its own ancestor.
var ErrHierarchy = errors.New("htmlparse: hierarchy violation")

// fillPositions computes Line and Col for every error in errs from the
// input. It runs in O(len(input) + len(errs) log len(errs)).
func fillPositions(input []byte, errs []ParseError) {
	if len(errs) == 0 {
		return
	}
	// Errors are almost always in offset order already; sort indices
	// stably by offset so the sweep is a single pass.
	idx := make([]int, len(errs))
	for i := range idx {
		idx[i] = i
	}
	sorted := true
	for i := 1; i < len(errs); i++ {
		if errs[i].Offset < errs[i-1].Offset {
			sorted = false
			break
		}
	}
	if !sorted {
		stableSortByOffset(idx, errs)
	}
	line, col := 1, 1
	pos := 0
	for _, i := range idx {
		target := errs[i].Offset
		if target > len(input) {
			target = len(input)
		}
		for pos < target {
			b := input[pos]
			switch {
			case b == '\n':
				line++
				col = 1
				pos++
			case b == '\r':
				line++
				col = 1
				pos++
				if pos < len(input) && input[pos] == '\n' {
					if pos == target {
						// The offset points at the LF of a CR LF pair:
						// it belongs to the same line break.
						break
					}
					pos++
				}
			case b < 0x80:
				col++
				pos++
			default:
				r, w := decodeRune(input[pos:])
				col++
				if r >= 0x10000 {
					col++ // two UTF-16 code units
				}
				pos += w
			}
		}
		errs[i].Line = line
		errs[i].Col = col
	}
}

// stableSortByOffset is a bottom-up merge sort of idx keyed by
// errs[idx[k]].Offset.
//
// PORT: written out so the order of equal offsets (emission order) does
// not depend on a library's stability guarantee; Rust's stable sort_by
// gives the same result.
func stableSortByOffset(idx []int, errs []ParseError) {
	if len(idx) < 2 {
		return
	}
	tmp := make([]int, len(idx))
	for width := 1; width < len(idx); width *= 2 {
		for lo := 0; lo < len(idx); lo += 2 * width {
			mid := min(lo+width, len(idx))
			hi := min(lo+2*width, len(idx))
			i, j, k := lo, mid, lo
			for i < mid && j < hi {
				if errs[idx[j]].Offset < errs[idx[i]].Offset {
					tmp[k] = idx[j]
					j++
				} else {
					tmp[k] = idx[i]
					i++
				}
				k++
			}
			for i < mid {
				tmp[k] = idx[i]
				i++
				k++
			}
			for j < hi {
				tmp[k] = idx[j]
				j++
				k++
			}
		}
		copy(idx, tmp)
	}
}
