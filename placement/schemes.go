package placement

import (
	"errors"
	"fmt"
)

// Scheme names a Placement implementation (spec 23.3). Both candidates stay
// selectable (docs/CONVENTIONS.md "Pluggable algorithms"; the measured
// comparison is BenchmarkPlcAlgo and docs/ALGORITHMS.md).
type Scheme string

// Placement schemes.
const (
	// SchemeRendezvous is highest-random-weight hashing: no state, minimal
	// movement on membership change, O(workers) per lookup. The default.
	SchemeRendezvous Scheme = "rendezvous"
	// SchemeConsistent is a consistent-hash ring with virtual nodes:
	// O(log(workers*vnodes)) per lookup after a ring build per membership.
	SchemeConsistent Scheme = "consistent"
)

// DefaultScheme is the scheme the engine's placement uses.
const DefaultScheme = SchemeRendezvous

// ErrUnknownScheme reports a Scheme that New does not know.
var ErrUnknownScheme = errors.New("placement: unknown scheme")

// Schemes returns every Scheme New accepts, default first.
func Schemes() []Scheme { return []Scheme{SchemeRendezvous, SchemeConsistent} }

// New returns the Placement of scheme s (empty: DefaultScheme); cfg applies
// to SchemeConsistent only.
func New(s Scheme, cfg ConsistentConfig) (Placement, error) {
	switch s {
	case "", SchemeRendezvous:
		return Rendezvous{}, nil
	case SchemeConsistent:
		return NewConsistentHash(cfg), nil
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownScheme, s)
}
