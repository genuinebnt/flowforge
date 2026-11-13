package sketch

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// SKETCH-INV-01 (pre-refactor compatibility) is TestSketchConformance's
// vecindex_compat file and TestSketchConformance_Vecindex; this test pins
// that the compat data is present and non-trivial so it cannot pass
// vacuously.
func TestSketchInv01_VecindexCompat(t *testing.T) {
	var found bool
	for _, cf := range testkit.LoadCases[sketchFile](t, "sketch") {
		c := cf.Data
		if c.ID != "sketch_vecindex_compat_001" {
			continue
		}
		found = true
		if len(c.Texts) < 30 || len(c.Sets) < 10 || len(c.Pairs) < 50 {
			t.Fatalf("compat data too small: %d texts, %d sets, %d pairs", len(c.Texts), len(c.Sets), len(c.Pairs))
		}
		var cand int
		for _, p := range c.Pairs {
			if p.LSHCandidate {
				cand++
			}
		}
		if cand == 0 || cand == len(c.Pairs) {
			t.Fatalf("compat pairs do not exercise both LSH outcomes (%d of %d candidates)", cand, len(c.Pairs))
		}
		checkCompat(t, c, DefaultMinHasher())
	}
	if !found {
		t.Fatal("conformance/sketch/vecindex_compat.json missing")
	}
}

// mutate returns a copy of base with each token replaced, with probability
// p, by a fresh token from a large vocabulary.
func mutate(r core.Rng, base []uint32, p float64) []uint32 {
	out := slices.Clone(base)
	for i := range out {
		if core.Float64(r) < p {
			out[i] = uint32(1_000_000 + core.IntN(r, 1_000_000))
		}
	}
	return out
}

func randomSeq(r core.Rng, n, vocab int) []uint32 {
	s := make([]uint32, n)
	for i := range s {
		s[i] = uint32(core.IntN(r, vocab))
	}
	return s
}

// SKETCH-INV-02: on generated near duplicates, SimHash Hamming distance
// tracks the shingle-set similarity: per Charikar, P[bit differs] = θ/π
// with cos θ = |A∩B|/sqrt(|A||B|), so the mean Hamming distance over a
// similarity bucket is within 2.5 bits of the mean 64·θ/π, and bucket
// means strictly increase as Jaccard falls.
func TestSketchInv02_SimHashTracksJaccard(t *testing.T) {
	r := testkit.Rng(t)
	rates := []float64{0.005, 0.02, 0.05, 0.1, 0.2, 0.4}
	pairs := max(testkit.Cases()/5, 40)
	var prevMean float64 = -1
	for _, p := range rates {
		var sumH, sumPred, sumJ float64
		for range pairs {
			a := randomSeq(r, 300, 50_000)
			b := mutate(r, a, p)
			sa, sb := WShingling(a, 3), WShingling(b, 3)
			j := JaccardSorted(sa, sb)
			inter := j * float64(len(sa)+len(sb)) / (1 + j)
			cos := inter / math.Sqrt(float64(len(sa))*float64(len(sb)))
			sumPred += 64 * math.Acos(min(cos, 1)) / math.Pi
			sumH += float64(Hamming(SimHashTokens(a, 3), SimHashTokens(b, 3)))
			sumJ += j
		}
		n := float64(pairs)
		mean, pred := sumH/n, sumPred/n
		t.Logf("edit rate %.3f: mean Jaccard %.3f, mean Hamming %.2f, predicted %.2f", p, sumJ/n, mean, pred)
		if math.Abs(mean-pred) > 2.5 {
			t.Errorf("rate %.3f: mean Hamming %.2f, Charikar prediction %.2f", p, mean, pred)
		}
		if mean <= prevMean {
			t.Errorf("rate %.3f: mean Hamming %.2f not above the previous bucket's %.2f", p, mean, prevMean)
		}
		prevMean = mean
	}
}

// setPair builds two string sets with exactly inter shared elements and
// union u, distinct per call (prefix tag).
func setPair(tag string, inter, u int) ([]string, []string) {
	var a, b []string
	for i := range inter {
		s := fmt.Sprintf("%s.c%d", tag, i)
		a, b = append(a, s), append(b, s)
	}
	for i := range u - inter {
		s := fmt.Sprintf("%s.x%d", tag, i)
		if i%2 == 0 {
			a = append(a, s)
		} else {
			b = append(b, s)
		}
	}
	return a, b
}

// SKETCH-INV-03: the MinHash estimate is within 5 standard errors
// (sqrt(J(1-J)/k) + 1/k) of the exact Jaccard for every pair, and its mean
// error over all pairs is below 0.01.
func TestSketchInv03_MinHashEstimate(t *testing.T) {
	r := testkit.Rng(t)
	m := DefaultMinHasher()
	k := float64(m.Permutations())
	var bias float64
	n := testkit.Cases()
	for i := range n {
		u := 10 + core.IntN(r, 200)
		inter := core.IntN(r, u+1)
		a, b := setPair(fmt.Sprintf("p%d-%d", i, r.Uint64()), inter, u)
		j := Jaccard(a, b)
		e := EstimateJaccard(m.Signature(a), m.Signature(b))
		if tol := 5*math.Sqrt(j*(1-j)/k) + 1/k; math.Abs(e-j) > tol {
			t.Errorf("pair %d: estimate %.4f, exact %.4f (tolerance %.4f)", i, e, j, tol)
		}
		bias += e - j
	}
	if b := bias / float64(n); math.Abs(b) > 0.01 {
		t.Errorf("mean estimate error %.4f", b)
	}
}

// SKETCH-INV-04: the empirical LSH collision rate of pairs with Jaccard s
// matches 1-(1-s^r)^b within 5 binomial standard errors (at least 0.05).
func TestSketchInv04_LSHCollisionRate(t *testing.T) {
	r := testkit.Rng(t)
	m := DefaultMinHasher()
	trials := max(testkit.Cases()/2, 100)
	for _, s := range []float64{0.2, 0.3, 0.4, 0.5, 0.6, 0.8} {
		const u = 60
		inter := int(math.Round(s * u))
		var hits int
		for i := range trials {
			a, b := setPair(fmt.Sprintf("s%v-%d-%d", s, i, r.Uint64()), inter, u)
			sa, sb := m.Signature(a), m.Signature(b)
			for band := range DefaultLSHBands {
				if BandKey(sa, band, DefaultLSHRows) == BandKey(sb, band, DefaultLSHRows) {
					hits++
					break
				}
			}
		}
		want := CollisionProbability(float64(inter)/u, DefaultLSHBands, DefaultLSHRows)
		got := float64(hits) / float64(trials)
		tol := max(0.05, 5*math.Sqrt(want*(1-want)/float64(trials)))
		t.Logf("s=%.2f: collision rate %.3f, predicted %.3f", s, got, want)
		if math.Abs(got-want) > tol {
			t.Errorf("s=%.2f: collision rate %.3f, predicted %.3f (tolerance %.3f)", s, got, want, tol)
		}
	}
}

// SKETCH-INV-05: WeightedSimHash with every weight 1 equals SimHashHashes;
// term-frequency weights reproduce the multiset SimHash; scaling every
// weight by a positive constant does not change the hash.
func TestSketchInv05_WeightedConsistency(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		ids := randomSeq(r, core.IntN(r, 60), 1+core.IntN(r, 40))
		hs := TokenShingles(ids, 1)
		wf := make([]WeightedFeature, len(hs))
		for i, h := range hs {
			wf[i] = WeightedFeature{Hash: h, Weight: 1}
		}
		if WeightedSimHash(wf) != SimHashHashes(hs) {
			t.Fatalf("unit weights differ from SimHashHashes for %v", ids)
		}
		tf := CountTokens(ids)
		if WeightedSimHashTokens(tf) != SimHashTokens(ids, 1) {
			t.Fatalf("term-frequency weights differ from SimHashTokens(ids, 1) for %v", ids)
		}
		c := int32(1 + core.IntN(r, 1000))
		scaled := slices.Clone(tf)
		for i := range scaled {
			scaled[i].Weight *= c
		}
		if WeightedSimHashTokens(scaled) != WeightedSimHashTokens(tf) {
			t.Fatalf("scaling weights by %d changed the hash", c)
		}
	}
}

// SKETCH-INV-06: WShingling is the exact set of w-windows: its size and
// Jaccard equal those computed on the windows themselves, and
// TokenShingles has max(n-k+1, min(n,1)) elements in sequence order.
func TestSketchInv06_ShinglingExact(t *testing.T) {
	r := testkit.Rng(t)
	windows := func(ids []uint32, w int) map[string]struct{} {
		out := map[string]struct{}{}
		if len(ids) == 0 {
			return out
		}
		if len(ids) <= w {
			out[fmt.Sprint(ids)] = struct{}{}
			return out
		}
		for i := 0; i+w <= len(ids); i++ {
			out[fmt.Sprint(ids[i:i+w])] = struct{}{}
		}
		return out
	}
	for range testkit.Cases() {
		w := 1 + core.IntN(r, 5)
		vocab := 1 + core.IntN(r, 8)
		a := randomSeq(r, core.IntN(r, 30), vocab)
		b := randomSeq(r, core.IntN(r, 30), vocab)
		ts := TokenShingles(a, w)
		wantLen := 0
		if len(a) > 0 {
			wantLen = max(len(a)-w+1, 1)
		}
		if len(ts) != wantLen {
			t.Fatalf("len(TokenShingles(%v, %d)) = %d, want %d", a, w, len(ts), wantLen)
		}
		wa, wb := windows(a, w), windows(b, w)
		sa, sb := WShingling(a, w), WShingling(b, w)
		if len(sa) != len(wa) || !sort.SliceIsSorted(sa, func(i, j int) bool { return sa[i] < sa[j] }) {
			t.Fatalf("WShingling(%v, %d) has %d elements, want %d sorted", a, w, len(sa), len(wa))
		}
		inter := 0
		for k := range wa {
			if _, ok := wb[k]; ok {
				inter++
			}
		}
		want := 1.0
		if u := len(wa) + len(wb) - inter; u > 0 {
			want = float64(inter) / float64(u)
		}
		if got := JaccardSorted(sa, sb); math.Abs(got-want) > 1e-12 {
			t.Fatalf("JaccardSorted = %v, window Jaccard %v", got, want)
		}
	}
}

// SKETCH-INV-07: SimHashIndex.Near returns exactly the stored ids within
// distance 3 (the pigeonhole argument makes multi-index hashing exact),
// sorted by distance then id, after any sequence of adds and removes.
func TestSketchInv07_SimHashIndexExact(t *testing.T) {
	r := testkit.Rng(t)
	for range max(testkit.Cases()/10, 20) {
		x := NewSimHashIndex[uint32](func(a, b uint32) int { return int(int64(a) - int64(b)) })
		ref := map[uint32]uint64{}
		center := r.Uint64()
		for range 200 {
			id := uint32(core.IntN(r, 64))
			if core.IntN(r, 5) == 0 {
				x.Remove(id)
				delete(ref, id)
				continue
			}
			h := center
			for range core.IntN(r, 7) {
				h ^= 1 << core.IntN(r, 64)
			}
			x.Add(id, h)
			ref[id] = h
		}
		q := center
		for range core.IntN(r, 3) {
			q ^= 1 << core.IntN(r, 64)
		}
		var want []Match[uint32]
		for id, h := range ref {
			if d := Hamming(q, h); d <= NearDuplicateDistance {
				want = append(want, Match[uint32]{ID: id, Distance: d})
			}
		}
		slices.SortFunc(want, func(a, b Match[uint32]) int {
			if a.Distance != b.Distance {
				return a.Distance - b.Distance
			}
			return int(int64(a.ID) - int64(b.ID))
		})
		if got := x.Near(q); !slices.Equal(got, want) || x.Len() != len(ref) {
			t.Fatalf("Near = %v, brute force %v (len %d vs %d)", got, want, x.Len(), len(ref))
		}
	}
}

// SKETCH-INV-08: LSH.Candidates returns exactly the stored ids whose
// signature agrees with the query on every row of at least one band.
func TestSketchInv08_LSHCandidatesExact(t *testing.T) {
	r := testkit.Rng(t)
	for range max(testkit.Cases()/10, 20) {
		bands, rows := 1+core.IntN(r, 6), 1+core.IntN(r, 4)
		l, err := NewLSH[uint32](bands, rows, func(a, b uint32) int { return int(int64(a) - int64(b)) })
		if err != nil {
			t.Fatal(err)
		}
		gen := func() []uint64 {
			s := make([]uint64, bands*rows)
			for i := range s {
				s[i] = uint64(core.IntN(r, 3)) // small alphabet: many band matches
			}
			return s
		}
		ref := map[uint32][]uint64{}
		for range 40 {
			id := uint32(core.IntN(r, 20))
			if core.IntN(r, 4) == 0 {
				l.Remove(id)
				delete(ref, id)
				continue
			}
			s := gen()
			if err := l.Add(id, s); err != nil {
				t.Fatal(err)
			}
			ref[id] = s
		}
		q := gen()
		var want []uint32
		for id, s := range ref {
			for b := range bands {
				if slices.Equal(s[b*rows:(b+1)*rows], q[b*rows:(b+1)*rows]) {
					want = append(want, id)
					break
				}
			}
		}
		slices.Sort(want)
		got, err := l.Candidates(q)
		if err != nil {
			t.Fatal(err)
		}
		if !equalU32(got, want) || l.Len() != len(ref) {
			t.Fatalf("Candidates = %v, brute force %v", got, want)
		}
	}
}

func equalU32(a, b []uint32) bool { return len(a) == len(b) && (len(a) == 0 || slices.Equal(a, b)) }

// SKETCH-INV-09: sketches are functions of the (multi)set: SimHash ignores
// feature order, MinHash ignores order and duplicates, and
// SignatureHashes over FNV1a64 values equals Signature.
func TestSketchInv09_OrderIndependence(t *testing.T) {
	r := testkit.Rng(t)
	m := DefaultMinHasher()
	for range testkit.Cases() / 4 {
		var es []string
		for range core.IntN(r, 30) {
			es = append(es, fmt.Sprintf("e%d", core.IntN(r, 40)))
		}
		perm := slices.Clone(es)
		core.Shuffle(r, len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
		dup := append(slices.Clone(perm), perm...)
		if SimHash(es) != SimHash(perm) {
			t.Fatal("SimHash depends on order")
		}
		sig := m.Signature(es)
		if !slices.Equal(sig, m.Signature(dup)) {
			t.Fatal("Signature depends on order or duplicates")
		}
		hs := make([]uint64, len(es))
		for i, e := range es {
			hs[i] = FNV1a64(e)
		}
		if !slices.Equal(sig, m.SignatureHashes(hs)) {
			t.Fatal("SignatureHashes(FNV1a64) != Signature")
		}
	}
}

// SKETCH-INV-10: MulAddMod61 equals a schoolbook 128-bit reference.
func TestSketchInv10_MulAddMod61(t *testing.T) {
	r := testkit.Rng(t)
	for range 10 * testkit.Cases() {
		a, x, b := r.Uint64()%Mersenne61, r.Uint64()%Mersenne61, r.Uint64()%Mersenne61
		hi, lo := mulBig(a, x)
		want := addMod(modBig(hi, lo), b)
		if got := MulAddMod61(a, x, b); got != want {
			t.Fatalf("MulAddMod61(%d, %d, %d) = %d, want %d", a, x, b, got, want)
		}
	}
	for _, v := range [][3]uint64{{Mersenne61 - 1, Mersenne61 - 1, Mersenne61 - 1}, {0, 0, 0}, {1, Mersenne61 - 1, 1}} {
		hi, lo := mulBig(v[0], v[1])
		if got, want := MulAddMod61(v[0], v[1], v[2]), addMod(modBig(hi, lo), v[2]); got != want {
			t.Fatalf("MulAddMod61(%v) = %d, want %d", v, got, want)
		}
	}
}
// mulAddMod61 (schoolbook 128-bit remainder).
func mulBig(a, b uint64) (uint64, uint64) {
	const mask = 1<<32 - 1
	a0, a1, b0, b1 := a&mask, a>>32, b&mask, b>>32
	p00, p01, p10, p11 := a0*b0, a0*b1, a1*b0, a1*b1
	mid := p01&mask + p10&mask + p00>>32
	lo := (mid << 32) | (p00 & mask)
	hi := p11 + p01>>32 + p10>>32 + mid>>32
	return hi, lo
}

func modBig(hi, lo uint64) uint64 {
	var r uint64
	for i := 127; i >= 0; i-- {
		var bit uint64
		if i >= 64 {
			bit = (hi >> (i - 64)) & 1
		} else {
			bit = (lo >> i) & 1
		}
		r = r<<1 | bit
		if r >= Mersenne61 {
			r -= Mersenne61
		}
	}
	return r
}

func addMod(a, b uint64) uint64 {
	s := a + b
	if s >= Mersenne61 {
		s -= Mersenne61
	}
	return s
}

// TestSketchErrors pins the configuration and length errors.
func TestSketchErrors(t *testing.T) {
	if _, err := NewMinHasher(0, 1); err == nil {
		t.Fatal("NewMinHasher(0) accepted")
	}
	if _, err := NewLSH[uint32](0, 4, func(a, b uint32) int { return 0 }); err == nil {
		t.Fatal("NewLSH(0, 4) accepted")
	}
	l, _ := NewLSH[uint32](2, 2, func(a, b uint32) int { return int(int64(a) - int64(b)) })
	if err := l.Add(1, []uint64{1, 2, 3}); err == nil {
		t.Fatal("short signature accepted")
	}
	if _, err := l.BandKeys([]uint64{1}); err == nil {
		t.Fatal("short signature accepted by BandKeys")
	}
}
