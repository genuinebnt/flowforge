package vecindex

import (
	"math"

	"github.com/genuinebnt/flowforge/core"
)

// kmeans clusters n points of dimension d (row-major in data) into k
// centroids with k-means++ seeding followed by at most iters Lloyd
// iterations. Every random choice comes from rng, so the result is a pure
// function of (data, k, iters, rng state). k is clamped to n. The returned
// slice holds k*d centroid coordinates, row-major.
//
// An empty cluster keeps its previous centroid; Lloyd stops early once an
// iteration changes no assignment.
func kmeans(data []float32, n, d, k, iters int, rng core.Rng) []float32 {
	if n <= 0 || d <= 0 || k <= 0 {
		return nil
	}
	k = min(k, n)
	cent := kmeansPlusPlus(data, n, d, k, rng)
	assign := make([]int32, n)
	for i := range assign {
		assign[i] = -1
	}
	sums := make([]float64, k*d)
	counts := make([]int, k)
	for range iters {
		changed := false
		for i := range n {
			c := nearestCentroid(data[i*d:(i+1)*d], cent, k, d)
			if int32(c) != assign[i] {
				assign[i] = int32(c)
				changed = true
			}
		}
		if !changed {
			break
		}
		clear(sums)
		clear(counts)
		for i := range n {
			c := int(assign[i])
			counts[c]++
			row := data[i*d : (i+1)*d]
			s := sums[c*d : (c+1)*d]
			for j, x := range row {
				s[j] += float64(x)
			}
		}
		for c := range k {
			if counts[c] == 0 {
				continue // keep the old centroid
			}
			inv := 1 / float64(counts[c])
			for j := range d {
				cent[c*d+j] = float32(sums[c*d+j] * inv)
			}
		}
	}
	return cent
}

// kmeansPlusPlus picks k initial centroids: the first uniformly, each next
// one with probability proportional to its squared distance from the
// nearest centroid chosen so far (Arthur and Vassilvitskii, 2007).
func kmeansPlusPlus(data []float32, n, d, k int, rng core.Rng) []float32 {
	cent := make([]float32, 0, k*d)
	first := core.IntN(rng, n)
	cent = append(cent, data[first*d:(first+1)*d]...)
	dist := make([]float64, n)
	var total float64
	for i := range n {
		dist[i] = float64(L2Squared(data[i*d:(i+1)*d], cent[:d]))
		total += dist[i]
	}
	for c := 1; c < k; c++ {
		pick := -1
		if total > 0 && !math.IsInf(total, 0) {
			target := core.Float64(rng) * total
			var acc float64
			for i, w := range dist {
				acc += w
				if acc > target && w > 0 {
					pick = i
					break
				}
			}
		}
		if pick < 0 {
			// All remaining mass is zero (duplicate points) or rounding ran
			// past the end: fall back to a uniform choice.
			pick = core.IntN(rng, n)
		}
		row := data[pick*d : (pick+1)*d]
		cent = append(cent, row...)
		newC := cent[c*d : (c+1)*d]
		total = 0
		for i := range n {
			if nd := float64(L2Squared(data[i*d:(i+1)*d], newC)); nd < dist[i] {
				dist[i] = nd
			}
			total += dist[i]
		}
	}
	return cent
}

// nearestCentroid returns the index of the centroid closest to x by squared
// L2, the lowest index on ties.
func nearestCentroid(x, cent []float32, k, d int) int {
	best, bestD := 0, float32(math.Inf(1))
	for c := range k {
		if dd := L2Squared(x, cent[c*d:(c+1)*d]); dd < bestD {
			best, bestD = c, dd
		}
	}
	return best
}
