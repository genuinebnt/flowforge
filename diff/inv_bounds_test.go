package diff

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// DIFF-INV-13: determinism: the same inputs give byte-identical outputs
// (scripts, unified text, side-by-side JSON, JSON patches, tree mappings)
// across repeated runs with fresh interners, whatever Go's map iteration
// order does.
func TestDiffInv13_Determinism(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() / 10 {
		oldL := linesOf(r, core.IntN(r, 30), 11)
		newL := linesOf(r, core.IntN(r, 30), 11)
		ja, jb := randJSON(r, 4), randJSON(r, 4)
		ta, tb := randTree(r, core.IntN(r, 15), 3), randTree(r, core.IntN(r, 15), 3)
		var first []byte
		for run := range 4 {
			var out bytes.Buffer
			for _, alg := range Algorithms() {
				res, _, err := DiffStrings(oldL, newL, Options{Algorithm: alg})
				if err != nil {
					t.Fatal(err)
				}
				u, err := Unified(oldL, newL, res.Script, UnifiedOptions{Context: 3})
				if err != nil {
					t.Fatal(err)
				}
				sbs, err := SideBySideOf(oldL, newL, res.Script, 2)
				if err != nil {
					t.Fatal(err)
				}
				sj, err := MarshalSideBySide(sbs)
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&out, "%s|%v|%v|%s|%s\n", res.Script, res.Script.Deleted, res.Script.Inserted, u, sj)
			}
			out.Write(MarshalPatch(JSONDiff(ja, jb, JSONOptions{})))
			tr := mustTreeDiff(t, ta, tb, TreeOptions{})
			fmt.Fprintf(&out, "%+v", tr)
			if run == 0 {
				first = out.Bytes()
			} else if !bytes.Equal(first, out.Bytes()) {
				t.Fatalf("run %d differs from run 0", run)
			}
		}
	}
}

// allocBytes returns the bytes allocated by f.
func allocBytes(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// DIFF-INV-14: linear-space Myers allocates O(N+M) bytes: at most 96 bytes
// per input token (two frontier arrays plus the script), growing linearly
// (4x input with 4x edits stays within 5x bytes), while the greedy
// variant's trace grows quadratically in D.
func TestDiffInv14_LinearSpaceMemory(t *testing.T) {
	r := core.NewSplitMix64(14)
	measure := func(n int, alg Algorithm) (uint64, int) {
		a := randSeq(r, n, 1<<20)
		b := editSeq(r, a, n/50, 1<<20)
		var d int
		bytes := allocBytes(func() {
			res, err := Diff(a, b, Options{Algorithm: alg})
			if err != nil {
				t.Fatal(err)
			}
			d = res.Script.Distance()
		})
		return bytes, d
	}
	small, d1 := measure(10_000, Myers)
	large, d2 := measure(40_000, Myers)
	t.Logf("linear Myers: %d B for 2x10k tokens (D=%d), %d B for 2x40k (D=%d)", small, d1, large, d2)
	if per := float64(large) / 80_000; per > 96 {
		t.Errorf("linear Myers allocates %.1f B per token", per)
	}
	if ratio := float64(large) / float64(small); ratio > 5 {
		t.Errorf("allocation grew %.2fx for 4x input", ratio)
	}
	g1, _ := measure(10_000, MyersGreedy)
	g2, _ := measure(40_000, MyersGreedy)
	t.Logf("greedy Myers: %d B, %d B", g1, g2)
	if float64(g2)/float64(g1) < 8 {
		t.Errorf("greedy trace should grow quadratically in D (got %.2fx)", float64(g2)/float64(g1))
	}
}

// DIFF-INV-15: caps are explicit and safe: a Myers search over the cap is
// flagged Capped and still Apply-correct (common prefix and suffix kept,
// distance >= the true one); a cap at or above the true distance changes
// nothing; patience and histogram stay Apply-correct under any cap; and
// BoundedDiff reports the level and reason it used, with a script that
// applies at that level.
func TestDiffInv15_CapsAndFallbacks(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		a, b := randPair(r, 40)
		want := lcsDistance(a, b)
		for _, alg := range []Algorithm{Myers, MyersGreedy} {
			unc := mustDiff(t, a, b, Options{Algorithm: alg})
			if want > 1 {
				c := mustDiff(t, a, b, Options{Algorithm: alg, MaxEditDistance: want - 1})
				if !c.Capped || c.Script.Distance() < want || !equalU32(mustApply(t, a, c.Script), b) {
					t.Fatalf("%s cap %d (D=%d): capped=%v distance=%d", alg, want-1, want, c.Capped, c.Script.Distance())
				}
				pre := commonPrefix(a, b)
				if pre > 0 && (c.Script.Ops[0].Kind != OpEqual || c.Script.Ops[0].Len() < pre) {
					t.Fatalf("%s capped script lost the common prefix", alg)
				}
			}
			if want > 0 {
				c := mustDiff(t, a, b, Options{Algorithm: alg, MaxEditDistance: want + core.IntN(r, 3)})
				if c.Capped || !c.Script.Equal(unc.Script) {
					t.Fatalf("%s: cap >= D changed the result", alg)
				}
			}
		}
		for _, alg := range []Algorithm{Patience, Histogram} {
			c := mustDiff(t, a, b, Options{Algorithm: alg, MaxEditDistance: 1 + core.IntN(r, 3)})
			if !equalU32(mustApply(t, a, c.Script), b) {
				t.Fatalf("%s capped result not Apply-correct", alg)
			}
		}
	}
	// BoundedDiff levels.
	for range testkit.Cases() / 4 {
		oldT := strings.Join(linesOf(r, core.IntN(r, 30), 11), "")
		newT := strings.Join(linesOf(r, core.IntN(r, 30), 11), "")
		type tc struct {
			b        Bounds
			level    Level
			fallback Fallback
		}
		size := max(len(oldT), len(newT))
		cases := []tc{{Bounds{}, LevelTokens, FallbackNone}}
		if size > 0 {
			cases = append(cases, tc{Bounds{MaxInputBytes: max(size-1, 1), MaxLineInputBytes: size}, LevelLines, FallbackInputSize})
			if size > 1 {
				cases = append(cases, tc{Bounds{MaxInputBytes: 1, MaxLineInputBytes: 1}, LevelHash, FallbackInputSize})
			}
		}
		words := NewInterner()
		dw := lcsDistance(words.InternAll(SplitWords(oldT)), words.InternAll(SplitWords(newT)))
		if dw > 1 {
			cases = append(cases, tc{Bounds{MaxEditDistance: dw - 1}, LevelLines, FallbackEditDistance})
		}
		for _, c := range cases {
			if size == 1 && c.level == LevelLines && c.fallback == FallbackInputSize {
				continue
			}
			res, err := BoundedDiff([]byte(oldT), []byte(newT), c.b)
			if err != nil {
				t.Fatal(err)
			}
			if res.Level != c.level || res.Fallback != c.fallback {
				t.Fatalf("bounds %+v: level %s/%s, want %s/%s", c.b, res.Level, res.Fallback, c.level, c.fallback)
			}
			if res.OldHash != sha256.Sum256([]byte(oldT)) || res.NewHash != sha256.Sum256([]byte(newT)) || res.Same != (oldT == newT) {
				t.Fatal("hashes or Same wrong")
			}
			if res.Level == LevelHash {
				if res.Script.Ops != nil || res.Old != nil {
					t.Fatal("hash level carries a script")
				}
				continue
			}
			got := mustApply(t, res.Old, res.Script)
			if !equalU32(got, res.New) || strings.Join(res.Units.Values(res.New), "") != newT || strings.Join(res.Units.Values(res.Old), "") != oldT {
				t.Fatalf("level %s script does not reproduce the new text", res.Level)
			}
		}
	}
	if _, err := BoundedDiff(nil, nil, Bounds{MaxEditDistance: -1}); err == nil {
		t.Fatal("negative bound accepted")
	}
	if _, err := Diff(nil, nil, Options{MaxEditDistance: -1}); err == nil {
		t.Fatal("negative cap accepted")
	}
	if _, err := Diff(nil, nil, Options{Algorithm: 99}); err == nil {
		t.Fatal("unknown algorithm accepted")
	}
}
