package codec

import (
	"errors"
	"strconv"
)

// Sentinel errors. Every error this package returns is an *Error whose
// Unwrap gives one of these, so callers test with errors.Is. The texts are
// stable and identical in the Rust port.
var (
	// ErrInvalid: the input is not a valid encoding (a byte outside the
	// alphabet, misplaced padding, non-zero trailing bits, a truncated
	// quantum or escape, or a non-canonical form in strict mode).
	ErrInvalid = errors.New("codec: invalid encoding")
	// ErrCorrupt: a compressed stream has a bad header, a malformed body
	// or a checksum mismatch.
	ErrCorrupt = errors.New("codec: corrupt stream")
	// ErrOutputLimit: decoding stopped at the output cap (MaxOutput, the
	// ratio guard, or a layer budget). The bytes returned with it are a
	// prefix of the full output.
	ErrOutputLimit = errors.New("codec: output limit")
	// ErrUnsupported: the requested kind cannot be decoded by this
	// function (for example Inflate of a text encoding, or a zlib stream
	// that needs a preset dictionary).
	ErrUnsupported = errors.New("codec: unsupported")
	// ErrUnknownAlgorithm: a configuration names an algorithm candidate
	// that does not exist.
	ErrUnknownAlgorithm = errors.New("codec: unknown algorithm")
)

// Code is the closed list of error codes. Codes are part of the format
// contract: conformance data names them, and the Rust port returns the
// same code for the same input.
type Code uint8

// Error codes. The comment after each names its sentinel.
const (
	// CodeInvalidByte: a byte that the grammar does not allow at this
	// position (outside the alphabet, white space in strict mode, a bare
	// CR or LF in strict quoted-printable). ErrInvalid.
	CodeInvalidByte Code = iota + 1
	// CodeBadPadding: '=' where none may appear, missing padding in
	// strict padded mode, or data after the padding. ErrInvalid.
	CodeBadPadding
	// CodeTrailingBits: the bits after the last whole byte of the final
	// quantum are not zero (strict mode, RFC 4648 §3.5). ErrInvalid.
	CodeTrailingBits
	// CodeTruncated: the input ends inside a quantum or an escape at a
	// length no encoder produces. ErrInvalid.
	CodeTruncated
	// CodeInvalidEscape: a malformed escape (%XY, =XY, \xHH, \uHHHH,
	// \u{...}) or one that strict mode forbids. ErrInvalid.
	CodeInvalidEscape
	// CodeNonCanonical: the input decodes, but is not the one form strict
	// mode accepts (lower-case percent escapes, an escaped byte that needs
	// no escape, trailing white space on a quoted-printable line, ...).
	// ErrInvalid.
	CodeNonCanonical
	// CodeLineTooLong: a quoted-printable line is longer than 76
	// characters (RFC 2045 §6.7 rule 5, strict mode). ErrInvalid.
	CodeLineTooLong
	// CodeBadHeader: a compressed stream does not start with a valid
	// header for its format. ErrCorrupt.
	CodeBadHeader
	// CodeCorrupt: a compressed stream is malformed after its header,
	// ends early, or fails its checksum. ErrCorrupt.
	CodeCorrupt
	// CodeOutputLimit: the output reached MaxOutput (or the remaining
	// DecodeLayers budget). ErrOutputLimit.
	CodeOutputLimit
	// CodeRatioLimit: the output reached MaxRatio times the input size
	// (decompression-bomb guard). ErrOutputLimit.
	CodeRatioLimit
	// CodeUnsupported: the kind or stream feature is not supported here.
	// ErrUnsupported.
	CodeUnsupported
)

var codeNames = [...]string{
	CodeInvalidByte:   "invalid_byte",
	CodeBadPadding:    "bad_padding",
	CodeTrailingBits:  "trailing_bits",
	CodeTruncated:     "truncated",
	CodeInvalidEscape: "invalid_escape",
	CodeNonCanonical:  "non_canonical",
	CodeLineTooLong:   "line_too_long",
	CodeBadHeader:     "bad_header",
	CodeCorrupt:       "corrupt",
	CodeOutputLimit:   "output_limit",
	CodeRatioLimit:    "ratio_limit",
	CodeUnsupported:   "unsupported",
}

// Codes returns every error code in numeric order.
func Codes() []Code {
	out := make([]Code, 0, len(codeNames)-1)
	for c := CodeInvalidByte; int(c) < len(codeNames); c++ {
		out = append(out, c)
	}
	return out
}

// String returns the stable snake_case name of c ("invalid_byte").
func (c Code) String() string {
	if c == 0 || int(c) >= len(codeNames) {
		return "code(" + strconv.Itoa(int(c)) + ")"
	}
	return codeNames[c]
}

// Sentinel returns the sentinel error c unwraps to.
func (c Code) Sentinel() error {
	switch c {
	case CodeBadHeader, CodeCorrupt:
		return ErrCorrupt
	case CodeOutputLimit, CodeRatioLimit:
		return ErrOutputLimit
	case CodeUnsupported:
		return ErrUnsupported
	}
	return ErrInvalid
}

// Error is the error value of every decoder. Offset is a byte offset into
// the decoder's input for text encodings (where the problem was found;
// len(input) when the input ended early). For compressed streams it is the
// number of output bytes returned with the error: the cap for the limit
// codes (except a zstd frame refused for its window size, which stops
// before the cap); for CodeCorrupt it depends on how far the decompressor
// got before it noticed, which is implementation-defined
// (OPEN_QUESTIONS.md), so only Code is portable there.
type Error struct {
	Code   Code
	Offset int
}

// Error renders the sentinel text, the code and the offset, for example
// "codec: output limit (ratio_limit at byte 6400)".
func (e *Error) Error() string {
	return e.Code.Sentinel().Error() + " (" + e.Code.String() + " at byte " + strconv.Itoa(e.Offset) + ")"
}

// Unwrap returns the sentinel of e.Code.
func (e *Error) Unwrap() error { return e.Code.Sentinel() }

func errAt(c Code, off int) error { return &Error{Code: c, Offset: off} }

// CodeOf returns the code of an error returned by this package, or 0.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}
