package diff

import (
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// DIFF-INV-01: for every algorithm, the script is canonical and
// Apply(a, Diff(a, b)) == b.
func TestDiffInv01_ApplyCorrect(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		a, b := randPair(r, 40)
		for _, alg := range Algorithms() {
			res := mustDiff(t, a, b, Options{Algorithm: alg})
			if got := mustApply(t, a, res.Script); !equalU32(got, b) {
				t.Fatalf("%s: Apply(a, diff) = %v, want %v (a=%v, %s)", alg, got, b, a, res.Script)
			}
			if res.Capped {
				t.Fatalf("%s: capped without a cap", alg)
			}
		}
	}
}

// DIFF-INV-02: Myers and Myers are minimal: their distance equals the
// brute-force n + m - 2*LCS on random small inputs.
func TestDiffInv02_MyersMinimal(t *testing.T) {
	r := testkit.Rng(t)
	for range 2 * testkit.Cases() {
		a, b := randPair(r, 30)
		want := lcsDistance(a, b)
		for _, alg := range []Algorithm{MyersLinearSpace, Myers} {
			if got := mustDiff(t, a, b, Options{Algorithm: alg}).Script.Distance(); got != want {
				t.Fatalf("%s distance %d, brute force %d (a=%v b=%v)", alg, got, want, a, b)
			}
		}
	}
}

// DIFF-INV-03: patience and histogram are Apply-correct heuristics whose
// distance is never below the Myers (minimal) distance.
func TestDiffInv03_HeuristicsBoundedByMyers(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		a, b := randPair(r, 60)
		m := mustDiff(t, a, b, Options{Algorithm: MyersLinearSpace}).Script.Distance()
		for _, alg := range []Algorithm{Patience, Histogram} {
			res := mustDiff(t, a, b, Options{Algorithm: alg})
			if d := res.Script.Distance(); d < m {
				t.Fatalf("%s distance %d below Myers %d", alg, d, m)
			}
			if got := mustApply(t, a, res.Script); !equalU32(got, b) {
				t.Fatalf("%s not Apply-correct", alg)
			}
		}
	}
}

// DIFF-INV-04: Apply(b, Invert(diff(a, b))) == a, Invert is an involution
// and keeps the distance.
func TestDiffInv04_Invert(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		a, b := randPair(r, 40)
		for _, alg := range Algorithms() {
			s := mustDiff(t, a, b, Options{Algorithm: alg}).Script
			inv, err := Invert(s)
			if err != nil {
				t.Fatal(err)
			}
			if err := inv.Validate(); err != nil {
				t.Fatal(err)
			}
			if got := mustApply(t, b, inv); !equalU32(got, a) {
				t.Fatalf("%s: Apply(b, Invert) = %v, want %v", alg, got, a)
			}
			back, err := Invert(inv)
			if err != nil || !back.Equal(s) || inv.Distance() != s.Distance() {
				t.Fatalf("%s: Invert is not an involution: %s vs %s", alg, back, s)
			}
		}
	}
}

// DIFF-INV-05: Compose(diff(a, b), diff(b, c)) is canonical, applies a to
// c, and its distance is at most the sum; composing with an identity
// script changes nothing.
func TestDiffInv05_Compose(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		a, b := randPair(r, 30)
		c := editSeq(r, b, 3, 6)
		s1 := mustDiff(t, a, b, Options{}).Script
		s2 := mustDiff(t, b, c, Options{Algorithm: Histogram}).Script
		s, err := Compose(s1, s2)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Validate(); err != nil {
			t.Fatal(err)
		}
		if got := mustApply(t, a, s); !equalU32(got, c) {
			t.Fatalf("Apply(a, Compose) = %v, want %v", got, c)
		}
		if s.Distance() > s1.Distance()+s2.Distance() {
			t.Fatalf("composed distance %d > %d + %d", s.Distance(), s1.Distance(), s2.Distance())
		}
		id := mustDiff(t, b, b, Options{}).Script
		if s3, err := Compose(s1, id); err != nil || !s3.Equal(s1) {
			t.Fatalf("Compose(s, identity) = %s, want %s (%v)", s3, s1, err)
		}
	}
	// A second script that disagrees with what the first inserted is
	// rejected, not silently applied.
	s1 := mustDiff(t, []uint32{1}, []uint32{2}, Options{}).Script
	s2 := mustDiff(t, []uint32{3}, nil, Options{}).Script
	if _, err := Compose(s1, s2); err == nil {
		t.Fatal("Compose accepted mismatched scripts")
	}
}

// DIFF-INV-20: the Reference algorithm is minimal: its distance equals an
// exhaustive recursion over the LCS definition on tiny inputs, and inputs
// beyond MaxReferenceCells are refused rather than allocated.
func TestDiffInv20_ReferenceExhaustive(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		a, b := randPair(r, 9)
		want := len(a) + len(b) - 2*exhaustiveLCS(a, b)
		res := mustDiff(t, a, b, Options{Algorithm: Reference})
		if res.Script.Distance() != want || !equalU32(mustApply(t, a, res.Script), b) {
			t.Fatalf("Reference distance %d, exhaustive %d (a=%v b=%v)", res.Script.Distance(), want, a, b)
		}
	}
	big := make([]uint32, 1<<13)
	if _, err := Diff(big, append(big, big...), Options{Algorithm: Reference}); err == nil {
		t.Fatal("Reference accepted an input beyond MaxReferenceCells")
	}
}
