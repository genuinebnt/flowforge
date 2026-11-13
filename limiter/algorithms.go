package limiter

import (
	"errors"
	"fmt"
)

// Algorithm names a keyed Limiter implementation of spec 14.2. Every
// benchmarked candidate stays selectable (docs/CONVENTIONS.md "Pluggable
// algorithms"; docs/ALGORITHMS.md has the measurements and the choice).
type Algorithm string

// The keyed algorithms. The sliding window counter is an approximation
// (14.2: within one window of the exact log), the CRDT limiter a
// multi-node G-counter run here with one node; the others are exact.
const (
	AlgoGCRA                 Algorithm = "gcra"         // one mutex
	AlgoGCRAAtomic           Algorithm = "gcra_atomic"  // sharded, lock-free per key (the default)
	AlgoLeakyBucket          Algorithm = "leaky_bucket" // GCRA's equivalent as a meter
	AlgoTokenBucket          Algorithm = "token_bucket"
	AlgoSlidingWindowCounter Algorithm = "sliding_window_counter"
	AlgoSlidingWindowLog     Algorithm = "sliding_window_log"
	AlgoCRDT                 Algorithm = "crdt"
)

// DefaultAlgorithm is what the services use for in-process limits: the
// sharded atomic GCRA, exact and fastest under contention (b_lim_02).
const DefaultAlgorithm = AlgoGCRAAtomic

// ErrUnknownAlgorithm reports an Algorithm that New does not know.
var ErrUnknownAlgorithm = errors.New("limiter: unknown algorithm")

// Algorithms returns every Algorithm New accepts, default first.
func Algorithms() []Algorithm {
	return []Algorithm{AlgoGCRAAtomic, AlgoGCRA, AlgoLeakyBucket, AlgoTokenBucket,
		AlgoSlidingWindowCounter, AlgoSlidingWindowLog, AlgoCRDT}
}

// New returns a limiter of algorithm a configured with rules; the empty
// Algorithm means DefaultAlgorithm. The CRDT limiter runs as node "local".
func New(a Algorithm, rules ...Rule) (Limiter, error) {
	switch a {
	case "", AlgoGCRAAtomic:
		return NewAtomicGCRA(rules...)
	case AlgoGCRA:
		return NewGCRA(rules...)
	case AlgoLeakyBucket:
		return NewLeakyBucket(rules...)
	case AlgoTokenBucket:
		return NewTokenBucket(rules...)
	case AlgoSlidingWindowCounter:
		return NewSlidingWindowCounter(rules...)
	case AlgoSlidingWindowLog:
		return NewSlidingWindowLog(rules...)
	case AlgoCRDT:
		return NewCRDTLimiter("local", rules...)
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownAlgorithm, a)
}
