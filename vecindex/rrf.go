package vecindex

import (
	"math"
	"slices"

	"github.com/genuinebnt/flowforge/core"
)

// DefaultRRFK is the rank constant of reciprocal rank fusion (spec 22.6).
const DefaultRRFK = 60

// RRF fuses ranked lists with reciprocal rank fusion (spec 22.6):
//
//	score(d) = sum over lists L of 1 / (rrfK + rank_L(d))
//
// where rank starts at 1 and a document absent from a list contributes 0.
// A document repeated within one list counts at its first (best) rank.
// rrfK <= 0 means 60. The result holds every document once, sorted by
// score descending, ties broken by id ascending; Distance is +Inf (RRF has
// no distance). Scores are summed in float64 in list order and reported as
// float32.
//
// PORT: the spec 22.6 example rounds each term before summing, so compare
// scores with tolerance 1e-5 (docs/PORTING_RUST.md 5.9); exact ties, and
// hence the id tie-break, can differ when a port sums in another order.
func RRF(rrfK int, lists ...[]core.UUID) []Hit {
	if rrfK <= 0 {
		rrfK = DefaultRRFK
	}
	score := make(map[core.UUID]float64)
	var order []core.UUID
	for _, l := range lists {
		seen := make(map[core.UUID]struct{}, len(l))
		for i, id := range l {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			if _, ok := score[id]; !ok {
				order = append(order, id)
			}
			score[id] += 1 / float64(rrfK+i+1)
		}
	}
	out := make([]Hit, len(order))
	for i, id := range order {
		out[i] = Hit{ID: id, Distance: float32(math.Inf(1)), Score: float32(score[id])}
	}
	slices.SortFunc(out, func(a, b Hit) int {
		sa, sb := score[a.ID], score[b.ID]
		switch {
		case sa > sb:
			return -1
		case sa < sb:
			return 1
		}
		return compareID(a.ID, b.ID)
	})
	return out
}

// HybridSearch runs the vector half of a hybrid query on idx and fuses it
// with lexical, the already ranked result of the lexical search (the
// trigram index or Postgres full-text, which live outside this package),
// by reciprocal rank fusion (spec 22.6). It returns the top p.K fused hits;
// a hit that came back from the vector search keeps its distance.
func HybridSearch(idx Index, q []float32, p SearchParams, lexical []core.UUID, rrfK int) ([]Hit, error) {
	vec, err := idx.Search(q, p)
	if err != nil {
		return nil, err
	}
	ids := make([]core.UUID, len(vec))
	dist := make(map[core.UUID]float32, len(vec))
	for i, h := range vec {
		ids[i] = h.ID
		dist[h.ID] = h.Distance
	}
	fused := RRF(rrfK, lexical, ids)
	if len(fused) > p.K {
		fused = fused[:p.K]
	}
	for i := range fused {
		if d, ok := dist[fused[i].ID]; ok {
			fused[i].Distance = d
		}
	}
	return fused, nil
}
