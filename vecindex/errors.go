package vecindex

import "errors"

// Sentinel errors. Every error returned by this package wraps one of these
// (with %w), so callers test with errors.Is. Services map them to gRPC and
// HTTP codes (spec 11.8); this package never does.
var (
	// ErrDimension reports a vector whose length differs from the index
	// dimension.
	ErrDimension = errors.New("vecindex: dimension mismatch")
	// ErrInvalidVector reports a vector with a NaN or infinite component, or
	// a zero vector under the cosine metric (it cannot be normalised).
	ErrInvalidVector = errors.New("vecindex: invalid vector")
	// ErrInvalidParams reports search parameters outside the contract of
	// spec 22.9 (k in 1..100, L >= k, beam width >= 0).
	ErrInvalidParams = errors.New("vecindex: invalid search parameters")
	// ErrInvalidConfig reports an index, graph, PQ or segment configuration
	// that cannot be honoured.
	ErrInvalidConfig = errors.New("vecindex: invalid configuration")
	// ErrInvalidMetric reports an unknown metric.
	ErrInvalidMetric = errors.New("vecindex: invalid metric")
	// ErrEmpty reports an operation that needs at least one vector (PQ
	// training, graph or segment construction) given none.
	ErrEmpty = errors.New("vecindex: no vectors")
	// ErrCorrupt reports a segment file that is structurally invalid:
	// truncated, wrong magic or version, inconsistent sizes, out-of-range
	// neighbour ids.
	ErrCorrupt = errors.New("vecindex: corrupt segment")
	// ErrChecksum reports a segment file whose header, section or
	// whole-file CRC32C does not match (vec_007). It is returned instead of
	// ever answering from damaged data.
	ErrChecksum = errors.New("vecindex: segment checksum mismatch")
	// ErrClosed reports use of an index or segment after Close.
	ErrClosed = errors.New("vecindex: closed")
	// ErrInvalidKey reports a registry key with a zero tenant or program id
	// or an empty model id.
	ErrInvalidKey = errors.New("vecindex: invalid index key")
	// ErrIndexExists reports registering a second index under one key.
	ErrIndexExists = errors.New("vecindex: index already registered")
	// ErrNoIndex reports a lookup of a key with no registered index.
	ErrNoIndex = errors.New("vecindex: no such index")
)
