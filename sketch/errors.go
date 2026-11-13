package sketch

import "errors"

// Sentinel errors. Every error returned by this package wraps one of these
// (with %w). Their texts are stable codes.
var (
	// ErrInvalidConfig reports a MinHash or LSH configuration that cannot be
	// honoured (no permutations, no bands or rows, nil comparator).
	ErrInvalidConfig = errors.New("sketch: invalid configuration")
	// ErrSignatureLength reports a signature whose length is not the
	// index's bands*rows.
	ErrSignatureLength = errors.New("sketch: signature length mismatch")
)
