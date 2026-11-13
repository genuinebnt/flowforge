package diff

import (
	"bytes"
	"slices"
	"testing"
)

// splitFuzz turns fuzz bytes into two token sequences over a small
// alphabet (so matches are common): bytes before the first 0xff are a,
// the rest b.
func splitFuzz(data []byte, alpha byte) ([]uint32, []uint32) {
	i := bytes.IndexByte(data, 0xff)
	if i < 0 {
		i = len(data) / 2
	} else {
		data = slices.Delete(slices.Clone(data), i, i+1)
	}
	conv := func(p []byte) []uint32 {
		out := make([]uint32, len(p))
		for k, c := range p {
			out[k] = uint32(c % alpha)
		}
		return out
	}
	return conv(data[:i]), conv(data[i:])
}

// FuzzDiffApply asserts DIFF-INV-01..04 and 15 on arbitrary inputs for
// every algorithm: canonical, Apply-correct, invertible, minimal where
// claimed, never below minimal, and Apply-correct under a cap.
func FuzzDiffApply(f *testing.F) {
	f.Add([]byte("ABCABBA\xffCBABAC"), byte(4), uint8(0))
	f.Add([]byte("\xff"), byte(1), uint8(1))
	f.Add([]byte("aaaaabbbbb\xffbbbbbaaaaa"), byte(2), uint8(3))
	f.Fuzz(func(t *testing.T, data []byte, alpha byte, limit uint8) {
		if len(data) > 400 {
			return
		}
		a, b := splitFuzz(data, max(alpha%16, 1))
		minimal := lcsDistance(a, b)
		for _, alg := range Algorithms() {
			res := mustDiff(t, a, b, Options{Algorithm: alg})
			if !equalU32(mustApply(t, a, res.Script), b) {
				t.Fatalf("%s: Apply mismatch", alg)
			}
			d := res.Script.Distance()
			if d < minimal || ((alg == MyersLinearSpace || alg == Myers) && d != minimal) {
				t.Fatalf("%s: distance %d, minimal %d", alg, d, minimal)
			}
			inv, err := Invert(res.Script)
			if err != nil || !equalU32(mustApply(t, b, inv), a) {
				t.Fatalf("%s: Invert failed: %v", alg, err)
			}
			capped := mustDiff(t, a, b, Options{Algorithm: alg, MaxEditDistance: int(limit)})
			if !equalU32(mustApply(t, a, capped.Script), b) {
				t.Fatalf("%s: capped result not Apply-correct", alg)
			}
			gc := mustDiff(t, a, b, Options{Algorithm: alg, GitCompact: true})
			if gc.Script.Distance() != d || !equalU32(mustApply(t, a, gc.Script), b) {
				t.Fatalf("%s: GitCompact changed distance or broke Apply", alg)
			}
		}
	})
}

// FuzzUnifiedRoundTrip asserts DIFF-INV-07 on arbitrary texts and that
// ParseUnified/ApplyUnified never panic on arbitrary patch text.
func FuzzUnifiedRoundTrip(f *testing.F) {
	f.Add([]byte("a\nb\nc\n"), []byte("a\nB\nc"), uint8(3), []byte("@@ -1,2 +1 @@\n-a\n b\n"))
	f.Add([]byte(""), []byte("x\n"), uint8(0), []byte("\\ No newline at end of file\n"))
	f.Fuzz(func(t *testing.T, oldT, newT []byte, ctx uint8, patch []byte) {
		if len(oldT)+len(newT) > 2000 {
			return
		}
		ol, nl := SplitLines(oldT), SplitLines(newT)
		for _, alg := range Algorithms() {
			res, _, err := DiffStrings(ol, nl, Options{Algorithm: alg})
			if err != nil {
				t.Fatal(err)
			}
			text, err := Unified(ol, nl, res.Script, UnifiedOptions{OldName: "a", NewName: "b", Context: int(ctx % 6)})
			if err != nil {
				t.Fatal(err)
			}
			if len(text) == 0 {
				if !slices.Equal(ol, nl) {
					t.Fatalf("%s: empty rendering of a change", alg)
				}
				continue
			}
			p, err := ParseUnified(text)
			if err != nil {
				t.Fatalf("%s: %v\n%s", alg, err, text)
			}
			got, err := ApplyUnified(ol, p)
			if err != nil || !slices.Equal(got, nl) {
				t.Fatalf("%s: round trip failed (%v)\n%s", alg, err, text)
			}
		}
		if p, err := ParseUnified(patch); err == nil {
			_, _ = ApplyUnified(ol, p)
		}
	})
}

// FuzzJSONPatch asserts DIFF-INV-09 on arbitrary JSON pairs and that
// ParsePatch/ApplyPatch never panic on arbitrary patch documents.
func FuzzJSONPatch(f *testing.F) {
	f.Add([]byte(`{"a":[1,2,3],"b":{"c":null}}`), []byte(`{"a":[1,3,4],"b":{"c":true},"d":"x"}`), []byte(`[{"op":"move","from":"/a/0","path":"/z"}]`))
	f.Add([]byte(`[]`), []byte(`[[],{}]`), []byte(`[{"op":"add","path":"/-","value":1}]`))
	f.Fuzz(func(t *testing.T, aText, bText, patch []byte) {
		if len(aText)+len(bText) > 4000 {
			return
		}
		if ops, err := ParsePatch(patch); err == nil {
			if doc, err := ParseJSON(aText); err == nil {
				_, _ = ApplyPatch(doc, ops)
			}
		}
		a, err := ParseJSON(aText)
		if err != nil {
			return
		}
		b, err := ParseJSON(bText)
		if err != nil {
			return
		}
		p := JSONDiff(a, b, JSONOptions{})
		got, err := ApplyPatch(a, p)
		if err != nil || !Equal(got, b) {
			t.Fatalf("round trip failed (%v)\na=%s\nb=%s\npatch=%s", err, aText, bText, MarshalPatch(p))
		}
		back, err := ParsePatch(MarshalPatch(p))
		if err != nil || !bytes.Equal(MarshalPatch(back), MarshalPatch(p)) {
			t.Fatalf("patch encoding round trip failed: %v", err)
		}
	})
}

// FuzzTreeDiff asserts DIFF-INV-11/12 on small trees decoded from bytes
// (byte i: parent choice and label of node i): Zhang–Shasha equals the
// exhaustive Tai-mapping search, is symmetric, and its mapping is valid.
func FuzzTreeDiff(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3}, []byte{0, 0, 1})
	f.Add([]byte{}, []byte{5})
	f.Fuzz(func(t *testing.T, x, y []byte) {
		build := func(p []byte) *ArenaTree {
			tr := &ArenaTree{}
			for i, c := range p[:min(len(p), 7)] {
				parent := NoNode
				if i > 0 {
					parent = NodeID(int(c>>2) % i)
				}
				tr.Add(parent, uint32(c&3))
			}
			return tr
		}
		a, b := build(x), build(y)
		unit := TreeCosts{Insert: 1, Delete: 1, Relabel: 1}
		res := mustTreeDiff(t, a, b, TreeOptions{})
		if want := bruteTreeDistance(a, b, unit); res.Distance != want {
			t.Fatalf("Zhang–Shasha %d, brute force %d", res.Distance, want)
		}
		if back := mustTreeDiff(t, b, a, TreeOptions{}); back.Distance != res.Distance {
			t.Fatalf("asymmetric: %d vs %d", res.Distance, back.Distance)
		}
		checkMapping(t, a, b, res, unit)
	})
}
