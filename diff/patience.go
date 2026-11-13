package diff

// Patience diff (Bram Cohen), following git's xdiff/xpatience.c: lines
// that occur exactly once in both ranges are matched, the longest
// increasing subsequence of those matches (found by patience sorting) are
// the anchors, matches are grown around each anchor, and the gaps are
// diffed recursively. A range whose sides share no line at all is a plain
// replacement; a range with shared but no unique lines falls back to
// Myers. Like git, patience works on the whole input (no prefix/suffix
// trimming), so a line repeated inside a common prefix is not unique.

// patEntry.line2 markers: no occurrence in b yet, or more than one
// occurrence in a or b.
const (
	patNone      = -1
	patNonUnique = -2
)

// patEntry is git's struct entry: one distinct line of a's range.
type patEntry struct {
	line1, line2 int // line2: patNone, patNonUnique or the unique match in b
	prev         int // index of the previous entry in the LIS, -1 none
}

// patience is one step of git's patience_diff over a[lo1:hi1] and
// b[lo2:hi2]; the gaps between anchors are pushed as new work.
func (e *engine) patience(lo1, hi1, lo2, hi2 int) {
	a, b := e.a, e.b
	if lo1 == hi1 || lo2 == hi2 {
		e.bld.del(hi1 - lo1)
		e.bld.ins(hi2 - lo2)
		return
	}
	// fill_hashmap: pass 1 over a, pass 2 over b. Entries keep a's order.
	index := make(map[uint32]int, hi1-lo1)
	entries := make([]patEntry, 0, hi1-lo1)
	for i := lo1; i < hi1; i++ {
		if j, ok := index[a[i]]; ok {
			entries[j].line2 = patNonUnique
			continue
		}
		index[a[i]] = len(entries)
		entries = append(entries, patEntry{line1: i, line2: patNone, prev: -1})
	}
	hasMatches := false
	for i := lo2; i < hi2; i++ {
		j, ok := index[b[i]]
		if !ok {
			continue
		}
		hasMatches = true
		if entries[j].line2 == patNone {
			entries[j].line2 = i
		} else {
			entries[j].line2 = patNonUnique
		}
	}
	if !hasMatches {
		e.bld.del(hi1 - lo1)
		e.bld.ins(hi2 - lo2)
		return
	}
	// find_longest_common_sequence: patience sorting on line2 in a order.
	var seq []int // seq[i] = entry ending the best increasing run of length i+1
	for j := range entries {
		if entries[j].line2 < 0 {
			continue
		}
		// binary_search: largest i with entries[seq[i]].line2 < line2.
		left, right := -1, len(seq)
		for left+1 < right {
			mid := left + (right-left)/2
			if entries[seq[mid]].line2 > entries[j].line2 {
				right = mid
			} else {
				left = mid
			}
		}
		if left >= 0 {
			entries[j].prev = seq[left]
		}
		if left+1 == len(seq) {
			seq = append(seq, j)
		} else {
			seq[left+1] = j
		}
	}
	if len(seq) == 0 {
		e.myers(lo1, hi1, lo2, hi2)
		return
	}
	var anchors []int
	for j := seq[len(seq)-1]; j >= 0; j = entries[j].prev {
		anchors = append(anchors, j)
	}
	for i, k := 0, len(anchors)-1; i < k; i, k = i+1, k-1 {
		anchors[i], anchors[k] = anchors[k], anchors[i]
	}
	// walk_common_sequence.
	var items []work
	line1, line2 := lo1, lo2
	for ai := 0; ; {
		var next1, next2 int
		if ai < len(anchors) {
			next1, next2 = entries[anchors[ai]].line1, entries[anchors[ai]].line2
			for next1 > line1 && next2 > line2 && a[next1-1] == b[next2-1] {
				next1--
				next2--
			}
		} else {
			next1, next2 = hi1, hi2
		}
		eq := 0
		for line1 < next1 && line2 < next2 && a[line1] == b[line2] {
			line1++
			line2++
			eq++
		}
		if eq > 0 {
			items = append(items, work{equal: eq})
		}
		if next1 > line1 || next2 > line2 {
			items = append(items, work{lo1: line1, hi1: next1, lo2: line2, hi2: next2})
		}
		if ai >= len(anchors) {
			break
		}
		last := ai
		for last+1 < len(anchors) &&
			entries[anchors[last+1]].line1 == entries[anchors[last]].line1+1 &&
			entries[anchors[last+1]].line2 == entries[anchors[last]].line2+1 {
			last++
		}
		end1 := entries[anchors[last]].line1 + 1
		end2 := entries[anchors[last]].line2 + 1
		items = append(items, work{equal: end1 - next1})
		line1, line2 = end1, end2
		ai = last + 1
	}
	e.pushOrdered(items)
}

// patienceDiff runs patience over the whole input; the bool reports a
// capped Myers fallback.
func patienceDiff(a, b []uint32, limit int) (EditScript, bool) {
	e := newEngine(a, b, limit)
	e.stack = append(e.stack, work{lo1: 0, hi1: len(a), lo2: 0, hi2: len(b)})
	e.drain(e.patience)
	return e.bld.finish(a, b), e.capped
}
