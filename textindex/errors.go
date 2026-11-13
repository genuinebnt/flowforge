package textindex

import (
	"errors"
	"fmt"
)

// Sentinel errors. Every error returned by this package wraps one of these
// (with %w), so callers test with errors.Is. Services map them to gRPC and
// HTTP codes; this package never does.
var (
	// ErrQuery reports a query that cannot be parsed, lowered or executed.
	// The concrete error is a *QueryError carrying a stable Code and a Span.
	ErrQuery = errors.New("textindex: invalid query")
	// ErrInvalidSchema reports a schema with duplicate, reserved, empty or
	// too many field names.
	ErrInvalidSchema = errors.New("textindex: invalid schema")
	// ErrInvalidDocument reports a document whose fields are not declared
	// by the schema or that repeats a field.
	ErrInvalidDocument = errors.New("textindex: invalid document")
	// ErrDuplicateID reports Add of an id that is already live or pending.
	ErrDuplicateID = errors.New("textindex: duplicate document id")
	// ErrInvalidConfig reports an index configuration that cannot be
	// honoured.
	ErrInvalidConfig = errors.New("textindex: invalid configuration")
	// ErrCorrupt reports a segment, tombstone or commit file that is
	// structurally invalid (truncated, wrong magic or version, out-of-range
	// offsets, unsorted keys, ...).
	ErrCorrupt = errors.New("textindex: corrupt file")
	// ErrChecksum reports a file whose section or footer CRC32C does not
	// match. It is returned instead of ever answering from damaged data.
	ErrChecksum = errors.New("textindex: checksum mismatch")
	// ErrSchemaMismatch reports a segment written with a different schema
	// than the index that opens it.
	ErrSchemaMismatch = errors.New("textindex: segment schema mismatch")
	// ErrClosed reports use of an index after Close.
	ErrClosed = errors.New("textindex: closed")
)

// ErrorCode is a stable, language-neutral query error code. The closed list
// below is part of the query-language contract (README "Errors").
type ErrorCode string

// Query error codes. Parse-time codes come from Parse; lowering and
// execution codes from Index.Search.
const (
	CodeEmptyQuery         ErrorCode = "EMPTY_QUERY"         // no clause at all
	CodeQueryTooLong       ErrorCode = "QUERY_TOO_LONG"      // more than MaxQueryBytes
	CodeInvalidUTF8        ErrorCode = "INVALID_UTF8"        // query is not valid UTF-8
	CodeUnexpectedToken    ErrorCode = "UNEXPECTED_TOKEN"    // token not allowed here
	CodeUnbalancedParen    ErrorCode = "UNBALANCED_PAREN"    // "(" without ")"
	CodeExpectedTerm       ErrorCode = "EXPECTED_TERM"       // operand missing after -, AND, OR, (
	CodeExpectedValue      ErrorCode = "EXPECTED_VALUE"      // "field:" without a value
	CodeUnterminatedPhrase ErrorCode = "UNTERMINATED_PHRASE" // '"' without closing '"'
	CodeUnterminatedRegex  ErrorCode = "UNTERMINATED_REGEX"  // '/' without closing '/'
	CodeEmptyRegex         ErrorCode = "EMPTY_REGEX"         // "//"
	CodeBadRegexFlag       ErrorCode = "BAD_REGEX_FLAG"      // flag other than i, m, s or repeated
	CodeBadRegex           ErrorCode = "BAD_REGEX"           // RE2 syntax error
	CodeBadTime            ErrorCode = "BAD_TIME"            // since:/until: value not a duration or date
	CodeTooDeep            ErrorCode = "NESTING_TOO_DEEP"    // more than MaxQueryDepth nested groups/negations
	CodeTooManyClauses     ErrorCode = "TOO_MANY_CLAUSES"    // more than MaxQueryClauses leaves
	CodeUnknownField       ErrorCode = "UNKNOWN_FIELD"       // field not in the schema
	CodeFieldType          ErrorCode = "FIELD_TYPE"          // operator not supported on the field's type
	CodeRegexNoTrigrams    ErrorCode = "REGEX_NO_TRIGRAMS"   // regex without trigrams and filters do not cap candidates
)

// Span is a half-open byte range [Start, End) into the query text.
type Span struct {
	Start int
	End   int
}

// QueryError is the error type of the query language. It wraps ErrQuery.
type QueryError struct {
	Code ErrorCode
	Span Span
	Msg  string
}

func (e *QueryError) Error() string {
	return fmt.Sprintf("textindex: %s at [%d,%d): %s", e.Code, e.Span.Start, e.Span.End, e.Msg)
}

// Unwrap returns ErrQuery.
func (e *QueryError) Unwrap() error { return ErrQuery }

func qerr(code ErrorCode, start, end int, format string, args ...any) *QueryError {
	return &QueryError{Code: code, Span: Span{start, end}, Msg: fmt.Sprintf(format, args...)}
}

func corruptf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, args...)...)
}
