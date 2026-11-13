package diff

import (
	"slices"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// DIFF-INV-06: hunks cover every change op exactly once and in order; each
// hunk has at most context equal tokens before its first and after its
// last change (exactly min(context, available)); consecutive hunks are
// separated by more than 2*context equal tokens, so contexts never overlap
// or touch; and inside a hunk no equal run exceeds 2*context.
func TestDiffInv06_Hunks(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		a, b := randPair(r, 60)
		alg := Algorithms()[core.IntN(r, 4)]
		s := mustDiff(t, a, b, Options{Algorithm: alg}).Script
		ctx := core.IntN(r, 5)
		hs := Hunks(s, ctx)
		var changes, covered []Op
		for _, op := range s.Ops {
			if op.Kind != OpEqual {
				changes = append(changes, op)
			}
		}
		for hi, h := range hs {
			if hi > 0 {
				gap := h.OldStart - hs[hi-1].OldEnd
				if gap <= 0 || gap != h.NewStart-hs[hi-1].NewEnd {
					t.Fatalf("hunks %d and %d touch or misalign (gap %d)", hi-1, hi, gap)
				}
			}
			first, last := -1, -1
			for i, op := range h.Ops {
				if op.Kind != OpEqual {
					covered = append(covered, op)
					if first < 0 {
						first = i
					}
					last = i
				} else if i > 0 && i < len(h.Ops)-1 && op.Len() > 2*ctx {
					t.Fatalf("hunk %d keeps an inner equal run of %d > 2*%d", hi, op.Len(), ctx)
				}
			}
			if first < 0 {
				t.Fatalf("hunk %d has no change", hi)
			}
			lead, trail := 0, 0
			if first == 1 {
				lead = h.Ops[0].Len()
			}
			if last == len(h.Ops)-2 {
				trail = h.Ops[len(h.Ops)-1].Len()
			}
			wantLead := min(ctx, h.Ops[first].OldStart)
			wantTrail := min(ctx, s.OldLen-h.Ops[last].OldEnd)
			if first > 1 || last < len(h.Ops)-2 || lead != wantLead || trail != wantTrail {
				t.Fatalf("hunk %d context %d/%d, want %d/%d", hi, lead, trail, wantLead, wantTrail)
			}
			if hi > 0 && h.Ops[first].OldStart-hs[hi-1].Ops[len(hs[hi-1].Ops)-1].OldEnd < 1 {
				t.Fatalf("context of hunk %d overlaps hunk %d", hi, hi-1)
			}
		}
		if !slices.Equal(changes, covered) {
			t.Fatalf("hunks cover %v, script changes %v", covered, changes)
		}
		for hi := 1; hi < len(hs); hi++ {
			prevChange := hs[hi-1].Ops[len(hs[hi-1].Ops)-1]
			if prevChange.Kind == OpEqual {
				prevChange = hs[hi-1].Ops[len(hs[hi-1].Ops)-2]
			}
			nextChange := hs[hi].Ops[0]
			if nextChange.Kind == OpEqual {
				nextChange = hs[hi].Ops[1]
			}
			if eq := nextChange.OldStart - prevChange.OldEnd; eq <= 2*ctx {
				t.Fatalf("hunks %d/%d separated by %d <= 2*%d equal tokens", hi-1, hi, eq, ctx)
			}
		}
	}
}

// DIFF-INV-07: unified rendering, parsing and strict application round
// trip for every algorithm and context: ApplyUnified(old,
// ParseUnified(Unified(old, new, diff))) == new, and the parsed hunk
// headers equal HunkHeader of Hunks.
func TestDiffInv07_UnifiedRoundTrip(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		oldL := linesOf(r, core.IntN(r, 25), 1+core.IntN(r, 11))
		newL := linesOf(r, core.IntN(r, 25), 1+core.IntN(r, 11))
		if core.IntN(r, 2) == 0 {
			newL = slices.Clone(oldL)
			for range core.IntN(r, 4) {
				if len(newL) > 0 {
					newL[core.IntN(r, len(newL))] = "changed\n"
				}
			}
		}
		alg := Algorithms()[core.IntN(r, 4)]
		res, _, err := DiffStrings(oldL, newL, Options{Algorithm: alg})
		if err != nil {
			t.Fatal(err)
		}
		ctx := core.IntN(r, 5)
		text, err := Unified(oldL, newL, res.Script, UnifiedOptions{OldName: "a/f", NewName: "b/f", Context: ctx})
		if err != nil {
			t.Fatal(err)
		}
		if res.Script.Distance() == 0 {
			if len(text) != 0 {
				t.Fatalf("no-change diff rendered %q", text)
			}
			continue
		}
		p, err := ParseUnified(text)
		if err != nil {
			t.Fatalf("ParseUnified: %v\n%s", err, text)
		}
		got, err := ApplyUnified(oldL, p)
		if err != nil {
			t.Fatalf("ApplyUnified: %v\n%s", err, text)
		}
		if !slices.Equal(got, newL) {
			t.Fatalf("round trip gave %q, want %q\n%s", got, newL, text)
		}
		hs := Hunks(res.Script, ctx)
		if len(hs) != len(p.Hunks) || p.OldName != "a/f" || p.NewName != "b/f" {
			t.Fatalf("parsed %d hunks (%q/%q), rendered %d", len(p.Hunks), p.OldName, p.NewName, len(hs))
		}
		for i, h := range hs {
			ph := p.Hunks[i]
			got := HunkHeader(Hunk{OldStart: start0(ph.OldStart, ph.OldLen), OldEnd: start0(ph.OldStart, ph.OldLen) + ph.OldLen,
				NewStart: start0(ph.NewStart, ph.NewLen), NewEnd: start0(ph.NewStart, ph.NewLen) + ph.NewLen})
			if got != HunkHeader(h) {
				t.Fatalf("hunk %d header %q, want %q", i, got, HunkHeader(h))
			}
		}
		// A patch applied to the wrong base is refused, not misapplied: when
		// a hunk reads old line 1, tampering with it must fail.
		touches := false
		for _, h := range p.Hunks {
			touches = touches || (h.OldLen > 0 && h.OldStart == 1)
		}
		if touches {
			bad := slices.Clone(oldL)
			bad[0] = "\x00tampered\n"
			if _, err := ApplyUnified(bad, p); err == nil {
				t.Fatalf("patch reading line 1 applied to a tampered base\n%s", text)
			}
		}
	}
}

func start0(start, n int) int {
	if n == 0 {
		return start
	}
	return start - 1
}

// DIFF-INV-16: CleanupSemantic is Apply-correct and canonical, never
// shortens a script below the minimal distance, leaves no equality that
// its own elimination rule would remove, and is idempotent.
func TestDiffInv16_SemanticCleanup(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		a, b := randPair(r, 50)
		s := mustDiff(t, a, b, Options{}).Script
		var opts CleanupOptions
		if core.IntN(r, 2) == 0 {
			opts.Score = func(l, rr uint32) int { return int((l*7 + rr*3) % 5) }
		}
		c, err := CleanupSemantic(a, s, opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("%v: %s", err, c)
		}
		if got := mustApply(t, a, c); !equalU32(got, b) {
			t.Fatalf("cleanup broke Apply: %s -> %s", s, c)
		}
		if c.Distance() < s.Distance() {
			t.Fatalf("cleanup distance %d below minimal %d", c.Distance(), s.Distance())
		}
		ops := c.Ops
		for i := 1; i+1 < len(ops); i++ {
			if ops[i].Kind != OpEqual {
				continue
			}
			before, after := regionSize(ops[:i], true), regionSize(ops[i+1:], false)
			if l := ops[i].Len(); l <= before && l <= after {
				t.Fatalf("equality %d (len %d) still eliminable (%d/%d): %s", i, l, before, after, c)
			}
		}
		c2, err := CleanupSemantic(a, c, opts)
		if err != nil || !c2.Equal(c) {
			t.Fatalf("not idempotent: %s -> %s", c, c2)
		}
	}
}

// regionSize returns max(deleted, inserted) of the change region adjacent
// to a cut: the trailing region of ops (fromEnd) or the leading one.
func regionSize(ops []Op, fromEnd bool) int {
	d, n := 0, 0
	for k := range ops {
		i := k
		if fromEnd {
			i = len(ops) - 1 - k
		}
		op := ops[i]
		if op.Kind == OpEqual {
			break
		}
		if op.Kind == OpDelete {
			d += op.Len()
		} else {
			n += op.Len()
		}
	}
	return max(d, n)
}

// DIFF-INV-17: word-level refinement: spans are sorted, disjoint and in
// bounds, and removing them from both lines leaves the same text.
func TestDiffInv17_WordRefinement(t *testing.T) {
	r := testkit.Rng(t)
	words := []string{"foo", "bar", " ", "  ", "(", ")", ".", "x1", "é", "\t", ","}
	gen := func() string {
		var b strings.Builder
		for range core.IntN(r, 15) {
			b.WriteString(words[core.IntN(r, len(words))])
		}
		return b.String()
	}
	for range testkit.Cases() {
		o, n := gen(), gen()
		if core.IntN(r, 2) == 0 {
			n = o + gen()
		}
		os, ns := RefineLine(o, n)
		ko, err := keep(o, os)
		if err != nil {
			t.Fatalf("old spans %v of %q: %v", os, o, err)
		}
		kn, err := keep(n, ns)
		if err != nil {
			t.Fatalf("new spans %v of %q: %v", ns, n, err)
		}
		if ko != kn {
			t.Fatalf("unchanged text differs: %q vs %q (%q -> %q)", ko, kn, o, n)
		}
		if o == n && (len(os) != 0 || len(ns) != 0) {
			t.Fatalf("identical lines got spans")
		}
	}
}

func keep(s string, spans []Span) (string, error) {
	var b strings.Builder
	pos := 0
	for _, sp := range spans {
		if sp.Start < pos || sp.End <= sp.Start || sp.End > len(s) {
			return "", errBadSpan
		}
		b.WriteString(s[pos:sp.Start])
		pos = sp.End
	}
	b.WriteString(s[pos:])
	return b.String(), nil
}

var errBadSpan = ErrInvalidScript

// DIFF-INV-18: the side-by-side form reconstructs each hunk: its old-side
// rows are exactly the old lines of the hunk range in order (same for the
// new side), line numbers are consecutive, and change rows carry valid
// spans.
func TestDiffInv18_SideBySide(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() / 2 {
		oldL := linesOf(r, core.IntN(r, 20), 11)
		newL := linesOf(r, core.IntN(r, 20), 11)
		res, _, err := DiffStrings(oldL, newL, Options{Algorithm: Histogram})
		if err != nil {
			t.Fatal(err)
		}
		ctx := core.IntN(r, 4)
		sbs, err := SideBySideOf(oldL, newL, res.Script, ctx)
		if err != nil {
			t.Fatal(err)
		}
		hs := Hunks(res.Script, ctx)
		if len(hs) != len(sbs.Hunks) {
			t.Fatalf("%d side-by-side hunks, %d hunks", len(sbs.Hunks), len(hs))
		}
		for i, h := range hs {
			var o, n []string
			on, nn := h.OldStart+1, h.NewStart+1
			for _, row := range sbs.Hunks[i].Rows {
				if row.OldNo != 0 {
					if row.OldNo != on {
						t.Fatalf("old line number %d, want %d", row.OldNo, on)
					}
					o = append(o, row.Old)
					on++
				}
				if row.NewNo != 0 {
					if row.NewNo != nn {
						t.Fatalf("new line number %d, want %d", row.NewNo, nn)
					}
					n = append(n, row.New)
					nn++
				}
				if row.Kind == RowChange {
					if _, err := keep(row.Old, row.OldSpans); err != nil {
						t.Fatal(err)
					}
					if _, err := keep(row.New, row.NewSpans); err != nil {
						t.Fatal(err)
					}
				}
			}
			if !slices.Equal(o, oldL[h.OldStart:h.OldEnd]) || !slices.Equal(n, newL[h.NewStart:h.NewEnd]) {
				t.Fatalf("hunk %d rows do not reconstruct its lines", i)
			}
			if sbs.Hunks[i].Header != HunkHeader(h) {
				t.Fatalf("header %q, want %q", sbs.Hunks[i].Header, HunkHeader(h))
			}
		}
	}
}

// DIFF-INV-19: SplitLines and SplitWords tile their input exactly, and the
// interner assigns dense ids in first-seen order that are equal exactly
// when the strings are equal.
func TestDiffInv19_SplitAndIntern(t *testing.T) {
	r := testkit.Rng(t)
	alphabet := []string{"a", "b", " ", "\n", "\r\n", "é", "\xff", "_", "9", ".", "\t"}
	for range testkit.Cases() {
		var sb strings.Builder
		for range core.IntN(r, 40) {
			sb.WriteString(alphabet[core.IntN(r, len(alphabet))])
		}
		s := sb.String()
		lines, words := SplitLines([]byte(s)), SplitWords(s)
		if strings.Join(lines, "") != s || strings.Join(words, "") != s {
			t.Fatalf("split does not tile %q: %q / %q", s, lines, words)
		}
		for i, l := range lines {
			if l == "" || (i < len(lines)-1 && !strings.HasSuffix(l, "\n")) || strings.Contains(strings.TrimSuffix(l, "\n"), "\n") {
				t.Fatalf("bad line %q in %q", l, lines)
			}
		}
		in := NewInterner()
		ids := in.InternAll(words)
		next := uint32(0)
		for i, id := range ids {
			if id > next {
				t.Fatalf("id %d skips ahead of %d", id, next)
			}
			if id == next {
				next++
			}
			for j := range i {
				if (ids[j] == id) != (words[j] == words[i]) {
					t.Fatalf("ids %d/%d disagree with strings %q/%q", ids[j], id, words[j], words[i])
				}
			}
			if v, ok := in.Value(id); !ok || v != words[i] {
				t.Fatalf("Value(%d) = %q", id, v)
			}
		}
		if in.Len() != int(next) {
			t.Fatalf("Len %d, want %d", in.Len(), next)
		}
	}
}
