package diff

import (
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

// lcsDistance is the minimal distance n + m - 2*LCS(a, b) from the
// package's Reference algorithm (the O(NM) dynamic program), itself checked
// against exhaustive recursion by DIFF-INV-20.
func lcsDistance(a, b []uint32) int {
	r, err := Diff(a, b, Options{Algorithm: Reference})
	if err != nil {
		panic(err) // test inputs are far below MaxReferenceCells
	}
	return r.Script.Distance()
}

// exhaustiveLCS is the independent oracle for Reference: plain recursion
// over the LCS definition (exponential; tiny inputs only).
func exhaustiveLCS(a, b []uint32) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if a[0] == b[0] {
		return 1 + exhaustiveLCS(a[1:], b[1:])
	}
	return max(exhaustiveLCS(a[1:], b), exhaustiveLCS(a, b[1:]))
}

// randSeq draws n tokens from an alphabet of size alpha.
func randSeq(r core.Rng, n, alpha int) []uint32 {
	s := make([]uint32, n)
	for i := range s {
		s[i] = uint32(core.IntN(r, alpha))
	}
	return s
}

// editSeq applies about edits random single-token edits (replace, insert,
// delete, or a moved block) to a copy of s.
func editSeq(r core.Rng, s []uint32, edits, alpha int) []uint32 {
	out := slices.Clone(s)
	for range edits {
		switch core.IntN(r, 4) {
		case 0:
			if len(out) > 0 {
				out[core.IntN(r, len(out))] = uint32(core.IntN(r, alpha))
			}
		case 1:
			i := core.IntN(r, len(out)+1)
			out = slices.Insert(out, i, uint32(core.IntN(r, alpha)))
		case 2:
			if len(out) > 0 {
				i := core.IntN(r, len(out))
				out = slices.Delete(out, i, i+1)
			}
		default:
			if len(out) > 2 {
				i := core.IntN(r, len(out)-1)
				j := i + 1 + core.IntN(r, min(4, len(out)-i-1))
				blk := slices.Clone(out[i:j])
				out = slices.Delete(out, i, j)
				k := core.IntN(r, len(out)+1)
				out = slices.Insert(out, k, blk...)
			}
		}
	}
	return out
}

// randPair returns a related or unrelated pair of small sequences.
func randPair(r core.Rng, maxLen int) ([]uint32, []uint32) {
	alpha := 1 + core.IntN(r, 6)
	if core.IntN(r, 4) == 0 {
		alpha = 1 + core.IntN(r, 40)
	}
	a := randSeq(r, core.IntN(r, maxLen+1), alpha)
	if core.IntN(r, 3) == 0 {
		return a, randSeq(r, core.IntN(r, maxLen+1), alpha)
	}
	return a, editSeq(r, a, core.IntN(r, 6), alpha)
}

func mustDiff(t testing.TB, a, b []uint32, opts Options) Result {
	t.Helper()
	r, err := Diff(a, b, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Script.Validate(); err != nil {
		t.Fatalf("%s: %v (%s)", opts.Algorithm, err, r.Script)
	}
	return r
}

func mustApply(t testing.TB, a []uint32, s EditScript) []uint32 {
	t.Helper()
	out, err := Apply(a, s)
	if err != nil {
		t.Fatalf("Apply: %v (%s)", err, s)
	}
	return out
}

func equalU32(a, b []uint32) bool { return len(a) == len(b) && (len(a) == 0 || slices.Equal(a, b)) }

func linesOf(r core.Rng, n, alpha int) []string {
	words := []string{"a", "b", "c", "{", "}", "", "return x;", "if (y) {", "  z++;", "foo()", "bar"}
	out := make([]string, n)
	for i := range out {
		out[i] = words[core.IntN(r, min(alpha, len(words)))] + "\n"
	}
	if n > 0 && core.IntN(r, 3) == 0 {
		out[n-1] = out[n-1][:len(out[n-1])-1] // no newline at end of file
	}
	return out
}
