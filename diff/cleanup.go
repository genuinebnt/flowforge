package diff

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Semantic cleanup after diff-match-patch's diff_cleanupSemantic (Neil
// Fraser), on token sequences instead of characters. A shortest script is
// often unreadable ("delete 'm', keep 'a', insert 'z'" inside a renamed
// word); the cleanup trades minimality for fewer, larger changes:
//
//  1. Elimination: an equality no longer than the changes on both of its
//     sides (max of deletions and insertions, in tokens) becomes a delete
//     plus an insert, re-examining the previous equality afterwards.
//  2. Merge (diff_cleanupMerge): adjacent edits are merged, common
//     prefixes and suffixes of a delete/insert pair become equalities, and
//     a single edit between two equalities is slid over one of them when
//     that removes the equality.
//  3. Lossless shift (diff_cleanupSemanticLossless): a single edit between
//     two equalities slides to the position with the best boundary score.
//  4. Overlap: when a deletion ends with a prefix of the following
//     insertion (or the reverse) and the overlap is at least half of
//     either, the overlap becomes an equality.
//
// The four steps (plus a final merge) repeat until a whole round changes
// nothing, so the result is a fixpoint and CleanupSemantic is idempotent.
// On diff-match-patch's own test vectors a single round already gives its
// published results (conformance/diff/cleanup.json).

// CleanupOptions configures CleanupSemantic.
type CleanupOptions struct {
	// Score rates the boundary between the non-empty token runs left and
	// right for the lossless shift (higher is better), like
	// diff_cleanupSemanticScore. An empty side always scores 6 (the edge
	// score) without a call. Nil scores every inner boundary 0: an edit then
	// moves to where a neighbouring equality disappears (the edge score
	// wins) or, failing that, as far right (down) as it can slide.
	Score func(left, right []uint32) int
}

// seg is one run of the token-level diff list.
type seg struct {
	kind OpKind
	toks []uint32
}

// CleanupSemantic returns a cleaned-up script equivalent to s (it applies
// old to the same new sequence). old is the sequence s was computed from;
// it supplies the tokens of Equal ops.
func CleanupSemantic(old []uint32, s EditScript, opts CleanupOptions) (EditScript, error) {
	segs, err := toSegs(old, s)
	if err != nil {
		return EditScript{}, err
	}
	return fromSegs(cleanupSemanticSegs(segs, opts.Score), s.OldLen, s.NewLen), nil
}

// cleanupSemanticSegs runs cleanup rounds until a fixpoint.
func cleanupSemanticSegs(segs []seg, score func(l, r []uint32) int) []seg {
	// Every changing round removes or merges segments or shifts an edit to
	// a strictly better boundary, so a fixpoint comes within a few rounds;
	// the bound only guards against a scorer that is not a total order.
	for iter := 0; iter < len(segs)+8; iter++ {
		before := cloneSegs(segs)
		segs = cleanupEliminate(segs)
		segs = cleanupMerge(segs)
		segs = cleanupLossless(segs, score)
		segs = cleanupOverlap(segs)
		segs = cleanupMerge(segs)
		if segsEqual(segs, before) {
			break
		}
	}
	return segs
}

// toSegs expands s into runs carrying their tokens, checking it against old.
func toSegs(old []uint32, s EditScript) ([]seg, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if len(old) != s.OldLen {
		return nil, fmt.Errorf("%w: old has %d tokens, script expects %d", ErrMismatch, len(old), s.OldLen)
	}
	var out []seg
	d, n := 0, 0
	for _, op := range s.Ops {
		k := op.Len()
		switch op.Kind {
		case OpEqual:
			out = append(out, seg{OpEqual, slices.Clone(old[op.OldStart:op.OldEnd])})
		case OpDelete:
			if !slices.Equal(old[op.OldStart:op.OldEnd], s.Deleted[d:d+k]) {
				return nil, fmt.Errorf("%w: deleted tokens at %d differ", ErrMismatch, op.OldStart)
			}
			out = append(out, seg{OpDelete, slices.Clone(s.Deleted[d : d+k])})
			d += k
		case OpInsert:
			out = append(out, seg{OpInsert, slices.Clone(s.Inserted[n : n+k])})
			n += k
		}
	}
	return out, nil
}

// fromSegs rebuilds a canonical script from runs.
func fromSegs(segs []seg, oldLen, newLen int) EditScript {
	var b builder
	b.carry = true
	for _, sg := range segs {
		switch sg.kind {
		case OpEqual:
			b.equal(len(sg.toks))
		case OpDelete:
			b.delTokens(sg.toks)
		case OpInsert:
			b.insTokens(sg.toks)
		}
	}
	return b.finishOps(oldLen, newLen)
}

func cloneSegs(segs []seg) []seg {
	out := make([]seg, len(segs))
	for i, s := range segs {
		out[i] = seg{s.kind, slices.Clone(s.toks)}
	}
	return out
}

func segsEqual(a, b []seg) bool {
	return slices.EqualFunc(a, b, func(x, y seg) bool { return x.kind == y.kind && slices.Equal(x.toks, y.toks) })
}

func cat(parts ...[]uint32) []uint32 {
	var out []uint32
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// cleanupEliminate is the elimination pass of diff_cleanupSemantic.
func cleanupEliminate(segs []seg) []seg {
	var equalities []int
	ins1, del1, ins2, del2 := 0, 0, 0, 0
	lastEq := -1 // index of the last equality, -1 none
	for p := 0; p < len(segs); p++ {
		if segs[p].kind == OpEqual {
			equalities = append(equalities, p)
			ins1, del1, ins2, del2 = ins2, del2, 0, 0
			lastEq = p
			continue
		}
		if segs[p].kind == OpInsert {
			ins2 += len(segs[p].toks)
		} else {
			del2 += len(segs[p].toks)
		}
		if lastEq >= 0 {
			l := len(segs[lastEq].toks)
			if l > 0 && l <= max(ins1, del1) && l <= max(ins2, del2) {
				e := equalities[len(equalities)-1]
				dup := seg{OpDelete, slices.Clone(segs[e].toks)}
				segs[e].kind = OpInsert
				segs = slices.Insert(segs, e, dup)
				equalities = equalities[:len(equalities)-1]
				if len(equalities) > 0 {
					equalities = equalities[:len(equalities)-1]
				}
				if len(equalities) > 0 {
					p = equalities[len(equalities)-1]
				} else {
					p = -1
				}
				ins1, del1, ins2, del2 = 0, 0, 0, 0
				lastEq = -1
			}
		}
	}
	return segs
}

// cleanupMerge is diff_cleanupMerge.
func cleanupMerge(segs []seg) []seg {
	for {
		segs = mergePass(segs)
		var changed bool
		segs, changed = shiftPass(segs)
		if !changed {
			return segs
		}
	}
}

// mergePass is the first loop of diff_cleanupMerge: adjacent edits are
// joined into one delete and one insert, and their common prefix and
// suffix move into the surrounding equalities.
func mergePass(in []seg) []seg {
	var out []seg
	var del, ins []uint32
	nd, ni := 0, 0
	flush := func(next []uint32) []uint32 {
		// Returns tokens to prepend to the following equality.
		var suffix []uint32
		if nd+ni > 0 {
			if nd > 0 && ni > 0 {
				if c := commonPrefix(ins, del); c > 0 {
					pre := slices.Clone(ins[:c])
					if l := len(out); l > 0 && out[l-1].kind == OpEqual {
						out[l-1].toks = append(out[l-1].toks, pre...)
					} else {
						out = append(out, seg{OpEqual, pre})
					}
					ins, del = ins[c:], del[c:]
				}
				if c := commonSuffix(ins, del); c > 0 {
					suffix = slices.Clone(ins[len(ins)-c:])
					ins, del = ins[:len(ins)-c], del[:len(del)-c]
				}
			}
			if len(del) > 0 {
				out = append(out, seg{OpDelete, slices.Clone(del)})
			}
			if len(ins) > 0 {
				out = append(out, seg{OpInsert, slices.Clone(ins)})
			}
		}
		del, ins, nd, ni = nil, nil, 0, 0
		return suffix
	}
	for _, s := range in {
		switch s.kind {
		case OpInsert:
			ins = append(ins, s.toks...)
			ni++
		case OpDelete:
			del = append(del, s.toks...)
			nd++
		case OpEqual:
			suffix := flush(s.toks)
			toks := cat(suffix, s.toks)
			if len(toks) == 0 {
				continue
			}
			if l := len(out); l > 0 && out[l-1].kind == OpEqual {
				out[l-1].toks = append(out[l-1].toks, toks...)
			} else {
				out = append(out, seg{OpEqual, toks})
			}
		}
	}
	if suffix := flush(nil); len(suffix) > 0 {
		out = append(out, seg{OpEqual, suffix})
	}
	return out
}

// shiftPass slides single edits over a neighbouring equality when that
// eliminates it: A<ins>BA</ins>C -> <ins>AB</ins>AC.
func shiftPass(segs []seg) ([]seg, bool) {
	changed := false
	for p := 1; p < len(segs)-1; p++ {
		prev, cur, next := segs[p-1], segs[p], segs[p+1]
		if prev.kind != OpEqual || next.kind != OpEqual || cur.kind == OpEqual {
			continue
		}
		switch {
		case hasSuffix(cur.toks, prev.toks):
			segs[p].toks = cat(prev.toks, cur.toks[:len(cur.toks)-len(prev.toks)])
			segs[p+1].toks = cat(prev.toks, next.toks)
			segs = slices.Delete(segs, p-1, p)
			changed = true
		case hasPrefix(cur.toks, next.toks):
			segs[p-1].toks = cat(prev.toks, next.toks)
			segs[p].toks = cat(cur.toks[len(next.toks):], next.toks)
			segs = slices.Delete(segs, p+1, p+2)
			changed = true
		}
	}
	return segs, changed
}

// cleanupLossless is diff_cleanupSemanticLossless.
func cleanupLossless(segs []seg, score func(l, r []uint32) int) []seg {
	sc := func(left, right []uint32) int {
		if len(left) == 0 || len(right) == 0 {
			return 6
		}
		if score == nil {
			return 0
		}
		return score(left, right)
	}
	for p := 1; p < len(segs)-1; p++ {
		if segs[p-1].kind != OpEqual || segs[p+1].kind != OpEqual || segs[p].kind == OpEqual {
			continue
		}
		eq1, edit, eq2 := slices.Clone(segs[p-1].toks), slices.Clone(segs[p].toks), slices.Clone(segs[p+1].toks)
		if c := commonSuffix(eq1, edit); c > 0 {
			common := slices.Clone(edit[len(edit)-c:])
			eq1 = eq1[:len(eq1)-c]
			edit = cat(common, edit[:len(edit)-c])
			eq2 = cat(common, eq2)
		}
		best1, bestEdit, best2 := eq1, edit, eq2
		bestScore := sc(eq1, edit) + sc(edit, eq2)
		for len(edit) > 0 && len(eq2) > 0 && edit[0] == eq2[0] {
			eq1 = cat(eq1, edit[:1])
			edit = cat(edit[1:], eq2[:1])
			eq2 = eq2[1:]
			if s := sc(eq1, edit) + sc(edit, eq2); s >= bestScore {
				bestScore, best1, bestEdit, best2 = s, eq1, edit, eq2
			}
		}
		if !slices.Equal(segs[p-1].toks, best1) {
			segs[p].toks = bestEdit
			segs[p-1].toks = best1
			segs[p+1].toks = best2
			if len(best2) == 0 {
				segs = slices.Delete(segs, p+1, p+2)
			}
			if len(best1) == 0 {
				segs = slices.Delete(segs, p-1, p)
				p--
			}
		}
	}
	return segs
}

// cleanupOverlap factors deletion/insertion overlaps into equalities.
func cleanupOverlap(segs []seg) []seg {
	for p := 1; p < len(segs); p++ {
		if segs[p-1].kind != OpDelete || segs[p].kind != OpInsert {
			continue
		}
		del, ins := segs[p-1].toks, segs[p].toks
		o1, o2 := commonOverlap(del, ins), commonOverlap(ins, del)
		if o1 >= o2 {
			if o1 > 0 && (2*o1 >= len(del) || 2*o1 >= len(ins)) {
				eq := seg{OpEqual, slices.Clone(ins[:o1])}
				segs[p-1].toks = slices.Clone(del[:len(del)-o1])
				segs[p].toks = slices.Clone(ins[o1:])
				segs = slices.Insert(segs, p, eq)
				p++
			}
		} else if 2*o2 >= len(del) || 2*o2 >= len(ins) {
			eq := seg{OpEqual, slices.Clone(del[:o2])}
			segs[p-1] = seg{OpInsert, slices.Clone(ins[:len(ins)-o2])}
			segs[p] = seg{OpDelete, slices.Clone(del[o2:])}
			segs = slices.Insert(segs, p, eq)
			p++
		}
		p++
	}
	return slices.DeleteFunc(segs, func(s seg) bool { return len(s.toks) == 0 })
}

func commonPrefix(a, b []uint32) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func commonSuffix(a, b []uint32) int {
	n := 0
	for n < len(a) && n < len(b) && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	return n
}

func hasPrefix(s, p []uint32) bool { return len(p) <= len(s) && slices.Equal(s[:len(p)], p) }
func hasSuffix(s, p []uint32) bool { return len(p) <= len(s) && slices.Equal(s[len(s)-len(p):], p) }

// commonOverlap returns the length of the longest suffix of a that is a
// prefix of b.
func commonOverlap(a, b []uint32) int {
	for k := min(len(a), len(b)); k > 0; k-- {
		if slices.Equal(a[len(a)-k:], b[:k]) {
			return k
		}
	}
	return 0
}

// LineScorer returns a boundary score for interned lines: 5 next to a
// blank (white-space only) line, 4 at any other line boundary. With it the
// lossless shift moves changes to start or end at blank lines.
func LineScorer(in *Interner) func(left, right []uint32) int {
	return func(left, right []uint32) int {
		l, _ := in.Value(left[len(left)-1])
		r, _ := in.Value(right[0])
		if strings.TrimSpace(l) == "" || strings.TrimSpace(r) == "" {
			return 5
		}
		return 4
	}
}

// TextScorer returns diff-match-patch's diff_cleanupSemanticScore for
// interned text tokens (characters or SplitWords words), computed on the
// text of the last three tokens of left and the first four of right: 5 at
// a blank line, 4 at a line break, 3 after the end of a sentence, 2 at
// white space, 1 at other punctuation, 0 inside a word.
func TextScorer(in *Interner) func(left, right []uint32) int {
	return func(left, right []uint32) int {
		var lb, rb strings.Builder
		for _, id := range left[max(0, len(left)-3):] {
			v, _ := in.Value(id)
			lb.WriteString(v)
		}
		for _, id := range right[:min(4, len(right))] {
			v, _ := in.Value(id)
			rb.WriteString(v)
		}
		one, two := lb.String(), rb.String()
		if one == "" || two == "" {
			return 6
		}
		c1, _ := utf8.DecodeLastRuneInString(one)
		c2, _ := utf8.DecodeRuneInString(two)
		nonAlnum1 := !unicode.IsLetter(c1) && !unicode.IsDigit(c1)
		nonAlnum2 := !unicode.IsLetter(c2) && !unicode.IsDigit(c2)
		ws1 := nonAlnum1 && unicode.IsSpace(c1)
		ws2 := nonAlnum2 && unicode.IsSpace(c2)
		lb1 := ws1 && (c1 == '\r' || c1 == '\n')
		lb2 := ws2 && (c2 == '\r' || c2 == '\n')
		blank1 := lb1 && (strings.HasSuffix(one, "\n\n") || strings.HasSuffix(one, "\n\r\n"))
		blank2 := lb2 && (strings.HasPrefix(two, "\n\n") || strings.HasPrefix(two, "\n\r\n") ||
			strings.HasPrefix(two, "\r\n\n") || strings.HasPrefix(two, "\r\n\r\n"))
		switch {
		case blank1 || blank2:
			return 5
		case lb1 || lb2:
			return 4
		case nonAlnum1 && !ws1 && ws2:
			return 3
		case ws1 || ws2:
			return 2
		case nonAlnum1 || nonAlnum2:
			return 1
		}
		return 0
	}
}
