package diff

// Histogram diff, following git's xdiff/xhistogram.c (itself after JGit's
// HistogramDiff). For a range, every line of a is counted; scanning b, each
// line present in a is extended to the longest common region around each
// of its occurrences, and the region whose rarest line is rarest (ties:
// the longer region) becomes the split point. Lines occurring more than
// maxChainLength (64) times are never anchors; if a range has common lines
// but all are that frequent, it falls back to Myers. A range with no common
// line is a replacement. Like git, histogram works on the whole input (no
// prefix/suffix trimming), and git's hash-bucket chain limit has no
// counterpart here because records are keyed by exact token id.

const histMaxChain = 64

type histRecord struct {
	ptr int // first occurrence in a (index), chained through next
	cnt int
}

type histIndex struct {
	recs    []histRecord
	byToken map[uint32]int
	lineRec []int // per a index - lo1: record index
	next    []int // per a index - lo1: next occurrence (absolute), -1 none
	cnt     int   // best (lowest) occurrence count so far
	common  bool
}

type region struct{ begin1, end1, begin2, end2 int } // inclusive ends

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

// findLCS is find_lcs: scanA, then try_lcs from every b position.
func (e *engine) findLCS(lo1, hi1, lo2, hi2 int) (lcs region, found, fallBack bool) {
	a, b := e.a, e.b
	x := histIndex{
		byToken: make(map[uint32]int, hi1-lo1),
		lineRec: make([]int, hi1-lo1),
		next:    make([]int, hi1-lo1),
	}
	// scanA: from the last line to the first, so each record's ptr ends at
	// the first occurrence and next[] chains occurrences in ascending order.
	for ptr := hi1 - 1; ptr >= lo1; ptr-- {
		if r, ok := x.byToken[a[ptr]]; ok {
			x.next[ptr-lo1] = x.recs[r].ptr
			x.recs[r].ptr = ptr
			x.recs[r].cnt++
			x.lineRec[ptr-lo1] = r
			continue
		}
		x.byToken[a[ptr]] = len(x.recs)
		x.lineRec[ptr-lo1] = len(x.recs)
		x.next[ptr-lo1] = -1
		x.recs = append(x.recs, histRecord{ptr: ptr, cnt: 1})
	}
	x.cnt = histMaxChain + 1
	cntAt := func(i int) int { return x.recs[x.lineRec[i-lo1]].cnt }
	for bPtr := lo2; bPtr < hi2; {
		bNext := bPtr + 1
		r, ok := x.byToken[b[bPtr]]
		if !ok {
			bPtr = bNext
			continue
		}
		rec := x.recs[r]
		if rec.cnt > x.cnt {
			x.common = true
			bPtr = bNext
			continue
		}
		x.common = true
		as := rec.ptr
		for {
			np := x.next[as-lo1]
			bs, ae, be, rc := bPtr, as, bPtr, rec.cnt
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
			if lcs.end1-lcs.begin1 < ae-as || rc < x.cnt {
				lcs = region{begin1: as, end1: ae, begin2: bs, end2: be}
				x.cnt = rc
				found = true
			}
			if np < 0 {
				break
			}
			for np >= 0 && np <= ae {
				np = x.next[np-lo1]
			}
			if np < 0 {
				break
			}
			as = np
		}
		bPtr = bNext
	}
	if x.common && x.cnt > histMaxChain {
		return lcs, found, true
	}
	return lcs, found, false
}

func histogramDiff(a, b []uint32, limit int) (EditScript, bool) {
	e := newEngine(a, b, limit)
	e.stack = append(e.stack, work{lo1: 0, hi1: len(a), lo2: 0, hi2: len(b)})
	e.drain(e.histogram)
	return e.bld.finish(a, b), e.capped
}
