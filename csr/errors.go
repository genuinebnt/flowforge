package csr

import "errors"

// Sentinel errors. Every error returned by this package wraps one of them,
// so callers can classify failures with errors.Is.
var (
	// ErrInvalidInput reports bad arguments to Build, an algorithm or an
	// overlay operation (unknown label index, NaN confidence, ...).
	ErrInvalidInput = errors.New("csr: invalid input")
	// ErrNodeRange reports a node index that is not below the node count.
	ErrNodeRange = errors.New("csr: node index out of range")
	// ErrFormat reports a snapshot file whose structure is malformed:
	// truncated, misaligned or overlapping sections, inconsistent arrays.
	ErrFormat = errors.New("csr: malformed snapshot")
	// ErrChecksum reports a snapshot whose sha256 footer or a section
	// crc32c does not match its contents.
	ErrChecksum = errors.New("csr: snapshot checksum mismatch")
	// ErrUnsupported reports a snapshot with an unknown version or header
	// flags this implementation does not understand.
	ErrUnsupported = errors.New("csr: unsupported snapshot version or flags")
	// ErrUnknownNode reports a change event that references an entity
	// neither the snapshot nor the overlay knows.
	ErrUnknownNode = errors.New("csr: unknown node")
	// ErrSeqOrder reports a change event whose seq is not greater than the
	// last one applied to an overlay.
	ErrSeqOrder = errors.New("csr: change event out of order")
	// ErrStaleSnapshot reports an attempt to publish a snapshot older than
	// the one currently held.
	ErrStaleSnapshot = errors.New("csr: snapshot older than current")
)
