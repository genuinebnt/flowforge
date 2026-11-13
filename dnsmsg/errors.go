package dnsmsg

import "strconv"

// Code is a stable error or diagnostic code. The list is closed; the Rust
// port uses the same strings.
type Code string

// Error and diagnostic codes.
const (
	// CodeShortMessage: the input ends inside a header, question, record or
	// record data field.
	CodeShortMessage Code = "short_message"
	// CodeLabelTooLong: a label is longer than 63 octets.
	CodeLabelTooLong Code = "label_too_long"
	// CodeNameTooLong: a name is longer than 255 octets in wire form.
	CodeNameTooLong Code = "name_too_long"
	// CodeBadLabelType: a length octet starts with the reserved bits 01 or
	// 10 (RFC 6891 §5 retired extended label types).
	CodeBadLabelType Code = "bad_label_type"
	// CodePointerOutOfBounds: a compression pointer points past the end of
	// the message.
	CodePointerOutOfBounds Code = "pointer_out_of_bounds"
	// CodePointerLoop: a compression pointer does not point strictly before
	// the last pointer followed, the name runs into that pointer (or past
	// the message end) after a jump, or it follows more than
	// MaxPointerHops pointers.
	CodePointerLoop Code = "pointer_loop"
	// CodeBadRData: record data of a known type is malformed.
	CodeBadRData Code = "bad_rdata"
	// CodeRDLengthMismatch: the record data of a known type does not fill
	// exactly RDLENGTH octets.
	CodeRDLengthMismatch Code = "rdlength_mismatch"
	// CodeTrailingData: octets follow the last record (strict mode).
	CodeTrailingData Code = "trailing_data"
	// CodeBadOPT: an OPT record outside the additional section, with a
	// non-root owner, or more than one OPT record (strict mode).
	CodeBadOPT Code = "bad_opt"
	// CodeBadNameText: a presentation-form name does not parse (encoding).
	CodeBadNameText Code = "bad_name_text"
	// CodeStringTooLong: a TXT character-string or CAA tag is longer than
	// 255 octets, or a CAA tag is empty (encoding).
	CodeStringTooLong Code = "string_too_long"
	// CodeKindMismatch: an RData's kind does not fit its record type, or
	// an address has the wrong family (encoding).
	CodeKindMismatch Code = "rdata_kind_mismatch"
	// CodeMessageTooLarge: the encoded message exceeds 65535 octets or the
	// size limit given to EncodeTruncated.
	CodeMessageTooLarge Code = "message_too_large"
	// CodeTooManyRecords: a section holds more than 65535 entries
	// (encoding).
	CodeTooManyRecords Code = "too_many_records"
)

// Error is a decoding or encoding failure: a Code and the byte offset at
// which it was detected (-1 when the failure has no position).
type Error struct {
	Code   Code
	Offset int
}

// Error returns "dnsmsg: <code>" and the offset when there is one.
func (e *Error) Error() string {
	if e.Offset < 0 {
		return "dnsmsg: " + string(e.Code)
	}
	return "dnsmsg: " + string(e.Code) + " at offset " + strconv.Itoa(e.Offset)
}

// Is reports whether target is an *Error with the same Code, so
// errors.Is(err, &dnsmsg.Error{Code: dnsmsg.CodePointerLoop}) works.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

func fail(c Code, off int) error { return &Error{Code: c, Offset: off} }

// CodeOf returns the Code of err, or "" when err is not an *Error.
func CodeOf(err error) Code {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}

// Diagnostic is a problem lenient decoding recovered from: the code strict
// mode would have failed with and the offset of the record (bad_opt), its
// data (bad_rdata, rdlength_mismatch and name codes) or the first trailing
// octet.
type Diagnostic struct {
	Code   Code
	Offset int
}
