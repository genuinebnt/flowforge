package chunkstore

import (
	"errors"
	"fmt"

	"github.com/genuinebnt/flowforge/core"
)

// Code is a stable error or diagnostic code. The set is closed: the Rust
// port declares exactly these values, and fsck reports use them verbatim.
type Code string

// The closed list of chunkstore codes. Decoders check, in this order:
// length (…_TRUNCATED), magic (…_BAD_MAGIC), CRC32C (…_CHECKSUM),
// version and flags (…_UNSUPPORTED), then structure (…_MALFORMED). Because
// the CRC covers every byte except itself and the magic is checked first,
// a single flipped byte always yields …_BAD_MAGIC (in the first four bytes)
// or …_CHECKSUM (anywhere else).
const (
	CodeInvalidConfig Code = "INVALID_CONFIG"
	CodeInvalidName   Code = "INVALID_NAME"
	CodeClosed        Code = "CLOSED"
	CodeTooLarge      Code = "TOO_LARGE"
	CodeBackend       Code = "BACKEND"

	CodeObjectNotFound   Code = "OBJECT_NOT_FOUND"
	CodeManifestNotFound Code = "MANIFEST_NOT_FOUND"
	CodeChunkMissing     Code = "CHUNK_MISSING"
	CodePackMissing      Code = "PACK_MISSING"
	CodeIndexMissing     Code = "INDEX_MISSING"

	CodePackTruncated   Code = "PACK_TRUNCATED"
	CodePackBadMagic    Code = "PACK_BAD_MAGIC"
	CodePackChecksum    Code = "PACK_CHECKSUM"
	CodePackUnsupported Code = "PACK_UNSUPPORTED"
	CodePackMalformed   Code = "PACK_MALFORMED"

	CodeRecordChecksum      Code = "RECORD_CHECKSUM"
	CodeRecordMalformed     Code = "RECORD_MALFORMED"
	CodeChunkDecompress     Code = "CHUNK_DECOMPRESS"
	CodeChunkLengthMismatch Code = "CHUNK_LENGTH_MISMATCH"
	CodeChunkHashMismatch   Code = "CHUNK_HASH_MISMATCH"

	CodeIndexTruncated    Code = "INDEX_TRUNCATED"
	CodeIndexBadMagic     Code = "INDEX_BAD_MAGIC"
	CodeIndexChecksum     Code = "INDEX_CHECKSUM"
	CodeIndexUnsupported  Code = "INDEX_UNSUPPORTED"
	CodeIndexMalformed    Code = "INDEX_MALFORMED"
	CodeIndexPackMismatch Code = "INDEX_PACK_MISMATCH"

	CodeManifestTruncated      Code = "MANIFEST_TRUNCATED"
	CodeManifestBadMagic       Code = "MANIFEST_BAD_MAGIC"
	CodeManifestChecksum       Code = "MANIFEST_CHECKSUM"
	CodeManifestUnsupported    Code = "MANIFEST_UNSUPPORTED"
	CodeManifestMalformed      Code = "MANIFEST_MALFORMED"
	CodeManifestLengthMismatch Code = "MANIFEST_LENGTH_MISMATCH"

	CodeRefTruncated   Code = "REF_TRUNCATED"
	CodeRefBadMagic    Code = "REF_BAD_MAGIC"
	CodeRefChecksum    Code = "REF_CHECKSUM"
	CodeRefUnsupported Code = "REF_UNSUPPORTED"
	CodeRefMalformed   Code = "REF_MALFORMED"

	// Fsck warnings: harmless leftovers that the next GC removes.
	CodeOrphanObject   Code = "ORPHAN_OBJECT"
	CodeDuplicateChunk Code = "DUPLICATE_CHUNK"
)

// Kind maps a code to the transport-neutral core error kind, so services
// can translate chunkstore errors without knowing every code.
func (c Code) Kind() core.Kind {
	switch c {
	case CodeInvalidConfig, CodeInvalidName:
		return core.KindInvalidArgument
	case CodeTooLarge:
		return core.KindResourceExhausted
	case CodeObjectNotFound, CodeManifestNotFound:
		return core.KindNotFound
	case CodeClosed:
		return core.KindFailedPrecondition
	case CodeBackend:
		return core.KindUnavailable
	default:
		// Missing or corrupt data: the store is damaged.
		return core.KindInternal
	}
}

// Error is the error type of every chunkstore operation. Compare with
// errors.Is against the Err* sentinels (which match on Code) or read the
// code with CodeOf.
type Error struct {
	Code   Code
	Object string // backend object name or chunk id, if known
	Detail string
	Err    error // wrapped cause, if any
}

func (e *Error) Error() string {
	s := "chunkstore: " + string(e.Code)
	if e.Object != "" {
		s += " [" + e.Object + "]"
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap returns the wrapped cause.
func (e *Error) Unwrap() error { return e.Err }

// Is reports whether target is an *Error with the same code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// CodeOf returns the chunkstore code carried by err, or "" if none.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func errf(code Code, object, format string, args ...any) *Error {
	return &Error{Code: code, Object: object, Detail: fmt.Sprintf(format, args...)}
}

func wrapErr(code Code, object string, cause error) *Error {
	return &Error{Code: code, Object: object, Err: cause}
}

// Sentinels for errors.Is.
var (
	ErrObjectNotFound   = &Error{Code: CodeObjectNotFound}
	ErrManifestNotFound = &Error{Code: CodeManifestNotFound}
	ErrChunkMissing     = &Error{Code: CodeChunkMissing}
	ErrClosed           = &Error{Code: CodeClosed}
	ErrInvalidName      = &Error{Code: CodeInvalidName}
	ErrInvalidConfig    = &Error{Code: CodeInvalidConfig}
	ErrTooLarge         = &Error{Code: CodeTooLarge}
)
