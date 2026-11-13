package sketch

import (
	"math/bits"
	"slices"
)

const (
	// NearDuplicateDistance is the SimHash Hamming distance at or below
	// which two documents are near duplicates, and the largest distance
	// SimHashIndex finds exactly.
	NearDuplicateDistance = 3
	// simBlocks is the number of 16-bit blocks of the multi-index; it must
	// be NearDuplicateDistance+1 for the pigeonhole argument.
	simBlocks = NearDuplicateDistance + 1
)

// SimHash is the 64-bit SimHash of a multiset of features: every
// occurrence of a feature adds +1 to each bit position where its FNV-1a
// hash has a one and -1 where it has a zero; bit i of the result is set
// when position i ends strictly positive. No features give 0.
func SimHash(features []string) uint64 {
	var acc [64]int64
	for _, f := range features {
		vote(&acc, FNV1a64(f), 1)
	}
	return fold(&acc)
}

// SimHashHashes is SimHash over features given as precomputed 64-bit
// hashes (for example TokenShingles output).
func SimHashHashes(hashes []uint64) uint64 {
	var acc [64]int64
	for _, h := range hashes {
		vote(&acc, h, 1)
	}
	return fold(&acc)
}

// SimHashText is SimHash over the 3-token shingles of Tokens(text).
func SimHashText(text string) uint64 {
	return SimHash(Shingles(Tokens(text), DefaultShingleWidth))
}

// SimHashTokens is SimHashHashes over the k-shingles of a token-id
// sequence.
func SimHashTokens(ids []uint32, k int) uint64 { return SimHashHashes(TokenShingles(ids, k)) }

// WeightedFeature is one hashed feature with an integer weight.
type WeightedFeature struct {
	Hash   uint64
	Weight int32
}

// WeightedToken is one token id with an integer weight (for example its
// term frequency, or a per-kind importance).
type WeightedToken struct {
	Token  uint32
	Weight int32
}

// WeightedSimHash is the weighted SimHash: each feature adds +Weight to the
// bit positions where its hash has a one and -Weight where it has a zero
// (64-bit accumulators, so no overflow below 2^32 features); bit i is set
// when position i ends strictly positive. With every weight 1 it equals
// SimHashHashes over the same hashes. A negative weight votes against its
// hash's bits.
func WeightedSimHash(features []WeightedFeature) uint64 {
	var acc [64]int64
	for _, f := range features {
		vote(&acc, f.Hash, int64(f.Weight))
	}
	return fold(&acc)
}

// WeightedSimHashTokens is WeightedSimHash over (TokenHash(token), weight).
func WeightedSimHashTokens(tokens []WeightedToken) uint64 {
	var acc [64]int64
	for _, t := range tokens {
		vote(&acc, TokenHash(t.Token), int64(t.Weight))
	}
	return fold(&acc)
}

// CountTokens returns the term frequencies of ids as weighted tokens,
// sorted by token id. WeightedSimHashTokens(CountTokens(ids)) equals
// SimHashTokens(ids, 1). A count above MaxInt32 saturates.
func CountTokens(ids []uint32) []WeightedToken {
	s := slices.Clone(ids)
	slices.Sort(s)
	var out []WeightedToken
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && s[j] == s[i] {
			j++
		}
		n := j - i
		if n > 1<<31-1 {
			n = 1<<31 - 1
		}
		out = append(out, WeightedToken{Token: s[i], Weight: int32(n)})
		i = j
	}
	return out
}

// vote adds w to every counter whose bit is set in h and subtracts it from
// the others (Charikar 2002).
func vote(acc *[64]int64, h uint64, w int64) {
	for i := range acc {
		if h&(1<<i) != 0 {
			acc[i] += w
		} else {
			acc[i] -= w
		}
	}
}

// fold sets bit i iff counter i is strictly positive, so a tie (0) gives a
// zero bit.
func fold(acc *[64]int64) uint64 {
	var out uint64
	for i, v := range acc {
		if v > 0 {
			out |= 1 << i
		}
	}
	return out
}

// Hamming returns the number of differing bits.
func Hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// Match is one near-duplicate found by SimHashIndex.
type Match[ID comparable] struct {
	ID       ID
	Distance int
}

// SimHashIndex finds near duplicates (Hamming distance <= 3) with
// multi-index hashing: the 64 bits are split into four 16-bit blocks, and
// by the pigeonhole principle any pair within distance 3 agrees exactly on
// at least one block, so a lookup probes four hash tables and verifies the
// candidates with a popcount. Ids are ordered by the comparator given to
// NewSimHashIndex. It is not safe for concurrent use.
//
// PORT: the tables are Go maps, but their iteration order never reaches an
// output: Near sorts its result.
type SimHashIndex[ID comparable] struct {
	cmp    func(a, b ID) int
	tables [simBlocks]map[uint16][]ID
	hashes map[ID]uint64
}

// NewSimHashIndex returns an empty index; cmp orders ids for Near's ties
// and must be a total order.
func NewSimHashIndex[ID comparable](cmp func(a, b ID) int) *SimHashIndex[ID] {
	x := &SimHashIndex[ID]{cmp: cmp, hashes: make(map[ID]uint64)}
	for i := range x.tables {
		x.tables[i] = make(map[uint16][]ID)
	}
	return x
}

// simBlock returns block i of h, block 0 being the low 16 bits.
func simBlock(h uint64, i int) uint16 { return uint16(h >> (16 * i)) }

// Add records id's hash, replacing an earlier one.
func (x *SimHashIndex[ID]) Add(id ID, h uint64) {
	x.Remove(id)
	x.hashes[id] = h
	for i := range x.tables {
		b := simBlock(h, i)
		x.tables[i][b] = append(x.tables[i][b], id)
	}
}

// Remove forgets id.
func (x *SimHashIndex[ID]) Remove(id ID) {
	h, ok := x.hashes[id]
	if !ok {
		return
	}
	delete(x.hashes, id)
	for i := range x.tables {
		b := simBlock(h, i)
		l := slices.DeleteFunc(x.tables[i][b], func(u ID) bool { return u == id })
		if len(l) == 0 {
			delete(x.tables[i], b)
		} else {
			x.tables[i][b] = l
		}
	}
}

// Len returns the number of stored hashes.
func (x *SimHashIndex[ID]) Len() int { return len(x.hashes) }

// Hash returns id's stored hash.
func (x *SimHashIndex[ID]) Hash(id ID) (uint64, bool) {
	h, ok := x.hashes[id]
	return h, ok
}

// Near returns every stored id within NearDuplicateDistance of h, sorted
// by distance then id.
func (x *SimHashIndex[ID]) Near(h uint64) []Match[ID] {
	seen := make(map[ID]struct{})
	var out []Match[ID]
	for i := range x.tables {
		for _, id := range x.tables[i][simBlock(h, i)] {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			if d := Hamming(h, x.hashes[id]); d <= NearDuplicateDistance {
				out = append(out, Match[ID]{ID: id, Distance: d})
			}
		}
	}
	slices.SortFunc(out, func(a, b Match[ID]) int {
		if a.Distance != b.Distance {
			return a.Distance - b.Distance
		}
		return x.cmp(a.ID, b.ID)
	})
	return out
}
