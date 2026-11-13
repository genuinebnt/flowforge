package textindex

import "slices"

// DefaultRRFK is the reciprocal rank fusion constant, the same as
// vecindex.DefaultRRFK (spec 22.6).
const DefaultRRFK = 60

// FusedHit is one result of reciprocal rank fusion.
type FusedHit struct {
	ID    uint64
	Score float64
	// Ranks holds the 1-based rank of the document in each input list,
	// 0 where it is absent.
	Ranks []int
}

// RRF fuses ranked id lists (vecindex.RRF semantics on uint64 ids):
//
//	score(d) = sum over lists L of 1 / (k + rank_L(d))
//
// with ranks from 1, lists added in argument order, a repeated id counting
// at its first rank, k <= 0 meaning 60, and ties broken by id ascending.
func RRF(k int, lists ...[]uint64) []FusedHit {
	if k <= 0 {
		k = DefaultRRFK
	}
	idx := map[uint64]int{}
	var out []FusedHit
	for li, l := range lists {
		for r, id := range l {
			i, ok := idx[id]
			if !ok {
				i = len(out)
				idx[id] = i
				out = append(out, FusedHit{ID: id, Ranks: make([]int, len(lists))})
			}
			if out[i].Ranks[li] != 0 {
				continue
			}
			out[i].Ranks[li] = r + 1
			out[i].Score += 1 / float64(k+r+1)
		}
	}
	slices.SortFunc(out, func(a, b FusedHit) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return out
}

// Hybrid fuses a lexical result with an externally ranked list (for
// example vecindex search results mapped to document ids) and returns the
// top n (all when n <= 0). Ranks[0] is the lexical rank, Ranks[1] the
// vector rank.
func Hybrid(lexical []Hit, vector []uint64, k, n int) []FusedHit {
	ids := make([]uint64, len(lexical))
	for i, h := range lexical {
		ids[i] = h.ID
	}
	out := RRF(k, ids, vector)
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}
