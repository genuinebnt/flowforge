package placement

import (
	"cmp"
	"slices"
)

// Rendezvous is highest-random-weight hashing (spec 23.3). The zero value is
// ready to use and safe for concurrent use; it holds no state.
type Rendezvous struct {
	// score overrides the scoring function in tests (to force ties).
	score func(keyState uint64, w WorkerID) uint64
}

var _ Placement = Rendezvous{}

func (r Rendezvous) scoreOf(ks uint64, w WorkerID) uint64 {
	if r.score != nil {
		return r.score(ks, w)
	}
	return fmix64(fnvString(ks, string(w)))
}

// Owner implements Placement: argmax over live of Hash64(key, w), ties
// broken by the smaller worker id. It runs in O(len(live)).
func (r Rendezvous) Owner(key []byte, live []WorkerID) (WorkerID, bool) {
	if len(live) == 0 {
		return "", false
	}
	ks := keyState(key)
	best := live[0]
	bestScore := r.scoreOf(ks, best)
	for _, w := range live[1:] {
		s := r.scoreOf(ks, w)
		if s > bestScore || (s == bestScore && w < best) {
			best, bestScore = w, s
		}
	}
	return best, true
}

// Ranking implements Placement: the distinct live workers ordered by score
// descending, then id ascending.
func (r Rendezvous) Ranking(key []byte, live []WorkerID) []WorkerID {
	ws := distinct(live)
	if len(ws) == 0 {
		return []WorkerID{}
	}
	ks := keyState(key)
	type scored struct {
		w WorkerID
		s uint64
	}
	sc := make([]scored, len(ws))
	for i, w := range ws {
		sc[i] = scored{w, r.scoreOf(ks, w)}
	}
	slices.SortFunc(sc, func(a, b scored) int {
		if c := cmp.Compare(b.s, a.s); c != 0 {
			return c
		}
		return cmp.Compare(a.w, b.w)
	})
	out := make([]WorkerID, len(sc))
	for i, s := range sc {
		out[i] = s.w
	}
	return out
}
