package diff

// Histogram diff, following git's xdiff/xhistogram.c (itself after JGit's
// HistogramDiff). For a range, every line of a is counted; scanning b, each
// line present in a is extended to the longest common region around each
// of its occurrences, and a region replaces the current best when it is
// longer or its rarest line is rarer (git's order-dependent rule); the
// best region splits the range. Lines occurring more than histMaxChain
// (git's max_chain_length, 64) times are never anchors; if a range has
// common lines but all are that frequent, it falls back to Myers. A range
// with no common line is a replacement. Like git, histogram works on the
// whole input (no prefix/suffix trimming), and git's hash-bucket chain
// limit has no counterpart here because records are keyed by exact token
// id.

// histMaxChain is git's max_chain_length (xhistogram.c).
const histMaxChain = 64

// histRecord is git's struct record: one distinct line of the range.
type histRecord struct {
	ptr int // first occurrence in a (index), chained through next
	cnt int // occurrences in a[lo1:hi1]
}

// histIndex is the per-range histogram. Its arrays are allocated once per
// diff and reused by every range: records are looked up by dense token id
// through tokRec, valid only while tokStamp equals the current epoch, so
// starting a range costs O(1) instead of clearing a table.
type histIndex struct {
	recs     []histRecord
	tokRec   []int32  // dense token -> record index (when stamped)
	tokStamp []uint32 // dense token -> epoch that wrote tokRec
	epoch    uint32
	// PORT: int32 positions, as git uses 32-bit line numbers: inputs are
	// limited to 2^31-1 tokens per side.
	lineRec []int32 // a index -> record index (absolute positions)
	next    []int32 // a index -> next occurrence (absolute), -1 none
}

// region is a common run a[begin1:end1+1] == b[begin2:end2+1] (inclusive
// ends, as in git).
type region struct{ begin1, end1, begin2, end2 int }

// histogram is one step of git's histogram_diff over a[lo1:hi1] and
// b[lo2:hi2]: split at the best region, or fall back.
func (e *engine) histogram(lo1, hi1, lo2, hi2 int) {
	if lo1 >= hi1 && lo2 >= hi2 {
		return
	}
	if lo1 == hi1 {
		e.bld.ins(hi2 - lo2)
		return
	}
	if lo2 == hi2 {
		e.bld.del(hi1 - lo1)
		return
	}
	lcs, found, fallBack := e.findLCS(lo1, hi1, lo2, hi2)
	switch {
	case fallBack:
		e.myers(lo1, hi1, lo2, hi2)
	case !found:
		e.bld.del(hi1 - lo1)
		e.bld.ins(hi2 - lo2)
	default:
		e.pushOrdered([]work{
			{lo1: lo1, hi1: lcs.begin1, lo2: lo2, hi2: lcs.begin2},
			{equal: lcs.end1 - lcs.begin1 + 1},
			{lo1: lcs.end1 + 1, hi1: hi1, lo2: lcs.end2 + 1, hi2: hi2},
		})
	}
}

// findLCS is git's find_lcs: scanA, then try_lcs from every b position.
func (e *engine) findLCS(lo1, hi1, lo2, hi2 int) (lcs region, found, fallBack bool) {
	a, b, x := e.a, e.b, e.hist
	x.epoch++
	x.recs = x.recs[:0]
	// scanA: from the last line to the first, so each record's ptr ends at
	// the first occurrence and next[] chains occurrences in ascending order.
	for ptr := hi1 - 1; ptr >= lo1; ptr-- {
		tok := a[ptr]
		if x.tokStamp[tok] == x.epoch {
			r := x.tokRec[tok]
			x.next[ptr] = int32(x.recs[r].ptr)
			x.recs[r].ptr = ptr
			x.recs[r].cnt++
			x.lineRec[ptr] = r
			continue
		}
		x.tokStamp[tok], x.tokRec[tok] = x.epoch, int32(len(x.recs))
		x.lineRec[ptr] = int32(len(x.recs))
		x.next[ptr] = -1
		x.recs = append(x.recs, histRecord{ptr: ptr, cnt: 1})
	}
	// index.cnt starts at max_chain_length+1: records seen more often than
	// the best region's rarest line are skipped.
	best := histMaxChain + 1
	common := false
	cntAt := func(i int) int { return x.recs[x.lineRec[i]].cnt }
	for bPtr := lo2; bPtr < hi2; {
		bNext := bPtr + 1
		tok := b[bPtr]
		if x.tokStamp[tok] != x.epoch {
			bPtr = bNext
			continue
		}
		rec := x.recs[x.tokRec[tok]]
		common = true
		if rec.cnt > best {
			bPtr = bNext
			continue
		}
		as := rec.ptr
		for {
			np := int(x.next[as])
			bs, ae, be, rc := bPtr, as, bPtr, rec.cnt
			// Grow the match around (as, bPtr), tracking the rarest line.
			for lo1 < as && lo2 < bs && a[as-1] == b[bs-1] {
				as--
				bs--
				if rc > 1 {
					rc = min(rc, cntAt(as))
				}
			}
			for ae < hi1-1 && be < hi2-1 && a[ae+1] == b[be+1] {
				ae++
				be++
				if rc > 1 {
					rc = min(rc, cntAt(ae))
				}
			}
			if bNext <= be {
				bNext = be + 1
			}
			// git keeps a longer region, or one whose rarest line is rarer.
			if lcs.end1-lcs.begin1 < ae-as || rc < best {
				lcs = region{begin1: as, end1: ae, begin2: bs, end2: be}
				best = rc
				found = true
			}
			if np < 0 {
				break
			}
			// Skip occurrences inside the region just found.
			for np >= 0 && np <= ae {
				np = int(x.next[np])
			}
			if np < 0 {
				break
			}
			as = np
		}
		bPtr = bNext
	}
	// Common lines exist but every one is too frequent to anchor on.
	if common && best > histMaxChain {
		return lcs, found, true
	}
	return lcs, found, false
}

// histogramDiff remaps tokens to dense ids (so the per-range index can use
// arrays) and runs histogram over the whole input.
func histogramDiff(a, b []uint32, limit int) (EditScript, bool) {
	da, db, k := denseTokens(a, b)
	e := newEngine(da, db, limit)
	e.hist = &histIndex{
		tokRec:   make([]int32, k),
		tokStamp: make([]uint32, k),
		lineRec:  make([]int32, len(a)),
		next:     make([]int32, len(a)),
	}
	e.stack = append(e.stack, work{lo1: 0, hi1: len(a), lo2: 0, hi2: len(b)})
	e.drain(e.histogram)
	return e.bld.finish(a, b), e.capped
}

// denseTokens renumbers the tokens of a and b as 0..k-1 in first-seen
// order; equality is preserved exactly. PORT: the map is only looked up,
// never iterated, so its order cannot reach the output.
func denseTokens(a, b []uint32) (da, db []uint32, k int) {
	ids := make(map[uint32]uint32, len(a))
	conv := func(s []uint32) []uint32 {
		out := make([]uint32, len(s))
		for i, t := range s {
			id, ok := ids[t]
			if !ok {
				id = uint32(len(ids))
				ids[t] = id
			}
			out[i] = id
		}
		return out
	}
	da, db = conv(a), conv(b)
	return da, db, len(ids)
}
