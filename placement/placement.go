package placement

import (
	"slices"
	"time"
)

// WorkerID identifies a worker. It is compared byte-wise for tie-breaking,
// so every node must spell the same worker the same way.
//
// PORT: Go string comparison is byte order of the UTF-8 bytes; Rust's
// String/&str Ord is the same, so derive Ord on a String newtype.
type WorkerID string

// Placement is the work-distribution port of spec section 24.4. Both methods
// are deterministic functions of their arguments and must not retain or
// modify live.
type Placement interface {
	// Owner returns the preferred owner of key among live, or false when
	// live is empty.
	Owner(key []byte, live []WorkerID) (WorkerID, bool)
	// Ranking returns the distinct live workers in preference order for
	// claiming key: the owner first. It returns an empty slice when live is
	// empty.
	Ranking(key []byte, live []WorkerID) []WorkerID
}

// Rank returns the position of self in p's ranking of key, 0 for the
// preferred owner. It returns false when self is not live.
func Rank(p Placement, key []byte, live []WorkerID, self WorkerID) (int, bool) {
	i := slices.Index(p.Ranking(key, live), self)
	return i, i >= 0
}

// ClaimDelay returns how long self should wait before trying to acquire the
// lease for key: rank times step (spec 23.3, "other workers delay their
// attempts by their rank in the hash order"). It returns false when self is
// not live and should not try at all.
func ClaimDelay(p Placement, key []byte, live []WorkerID, self WorkerID, step time.Duration) (time.Duration, bool) {
	r, ok := Rank(p, key, live, self)
	if !ok {
		return 0, false
	}
	return time.Duration(r) * step, true
}

// distinct returns the distinct ids of live, sorted ascending. It never
// modifies live.
func distinct(live []WorkerID) []WorkerID {
	out := slices.Clone(live)
	slices.Sort(out)
	return slices.Compact(out)
}
