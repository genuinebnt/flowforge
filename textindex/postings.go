package textindex

import (
	"encoding/binary"
	"math"
)

// noMore is the doc id of an exhausted iterator.
const noMore = math.MaxUint32

// docIter iterates sorted local doc ids. Before the first nextDoc or
// advance, docID is undefined.
type docIter interface {
	docID() uint32
	nextDoc() uint32
	// advance moves to the first doc >= target (staying put if the
	// current doc already is) and returns it.
	advance(target uint32) uint32
	cost() int
}

// postIter decodes one postings list (term or trigram) block by block
// with the segment's PostingCodec, using the skip entries to jump over
// blocks (FORMAT.md "Postings"). Decoding is bounds-checked: on malformed
// data the iterator ends (open-time validation makes that unreachable).
type postIter struct {
	pc       PostingCodec
	data     []byte
	base     int // offset of block 0 (after the skip table)
	skipOff  int // offset of the skip table
	nskip    int
	skipSize int
	df       int
	withTF   bool
	posData  []byte
	posBase  int

	i       int // global index of the current posting (-1 before start)
	blk     int // current block (-1 none)
	bstart  int // global index of the block's first posting
	bn      int // postings in the block
	nextOff int // byte offset of the next block
	docs    [postingsBlock]uint32
	tfs     [postingsBlock]uint32

	doc uint32
	tf  uint32
	// Positions are decoded lazily, so term-only queries never touch them.
	// posOff is where the next unread positions start; pending counts the
	// position varints of postings passed without reading them, skipped
	// on the next positions call. A skip jump resets both from the entry.
	posOff  int
	pending int
	posRead bool
	posBuf  []uint32
}

func (it *postIter) reset(pc PostingCodec, data []byte, off, df int, withTF bool, posData []byte, posOff int) {
	nskip := (df - 1) / postingsBlock
	size := triSkipSize
	if withTF {
		size = termSkipSize
	}
	buf := it.posBuf[:0]
	*it = postIter{
		pc: pc, data: data, skipOff: off, base: off + nskip*size, nskip: nskip, skipSize: size,
		df: df, withTF: withTF, posData: posData, posBase: posOff,
		i: -1, blk: -1, nextOff: off + nskip*size, posOff: posOff, posRead: true, posBuf: buf,
	}
}

func (it *postIter) docID() uint32 { return it.doc }
func (it *postIter) cost() int     { return it.df }

func (it *postIter) end() uint32 {
	it.i, it.doc = it.df, noMore
	return noMore
}

// load decodes block k starting at byte off, whose first delta is
// relative to prev.
func (it *postIter) load(k, off int, prev uint32) bool {
	n := min(postingsBlock, it.df-k*postingsBlock)
	if n <= 0 || off < 0 || off > len(it.data) {
		return false
	}
	var tfs []uint32
	if it.withTF {
		tfs = it.tfs[:n]
	}
	used, ok := it.pc.DecodeBlock(it.data[off:], it.docs[:n], tfs)
	if !ok {
		return false
	}
	d := uint64(prev)
	for j := 0; j < n; j++ {
		if (k > 0 || j > 0) && it.docs[j] == 0 {
			return false
		}
		d += uint64(it.docs[j])
		if d >= noMore {
			return false
		}
		it.docs[j] = uint32(d)
		if !it.withTF {
			it.tfs[j] = 1
		} else if it.tfs[j] == 0 {
			return false
		}
	}
	it.blk, it.bstart, it.bn, it.nextOff = k, k*postingsBlock, n, off+used
	return true
}

func (it *postIter) nextDoc() uint32 {
	if it.i >= 0 && !it.posRead {
		it.pending += int(it.tf)
	}
	it.i++
	if it.i >= it.df {
		return it.end()
	}
	if it.blk < 0 || it.i >= it.bstart+it.bn {
		prev := uint32(0)
		if it.blk >= 0 {
			prev = it.docs[it.bn-1]
		}
		if !it.load(it.blk+1, it.nextOff, prev) {
			return it.end()
		}
	}
	j := it.i - it.bstart
	it.doc, it.tf = it.docs[j], it.tfs[j]
	it.posRead = !it.withTF
	return it.doc
}

func (it *postIter) skipEntry(k int) (last uint32, nextPost, nextPos int, ok bool) {
	off := it.skipOff + k*it.skipSize
	if off < 0 || off+it.skipSize > len(it.data) {
		return 0, 0, 0, false
	}
	e := it.data[off : off+it.skipSize]
	last = binary.LittleEndian.Uint32(e)
	nextPost = int(binary.LittleEndian.Uint32(e[4:]))
	if it.withTF {
		nextPos = int(binary.LittleEndian.Uint32(e[8:]))
	}
	return last, nextPost, nextPos, true
}

func (it *postIter) advance(target uint32) uint32 {
	if it.i >= 0 && it.doc >= target {
		return it.doc
	}
	if it.i >= it.df {
		return noMore
	}
	// Inside the decoded block: step (no decoding needed).
	inBlock := it.blk >= 0 && it.docs[it.bn-1] >= target
	if !inBlock && it.blk < it.nskip {
		// First block k > blk whose last doc >= target (the last block,
		// index nskip, has no skip entry and bounds the search).
		lo, hi := it.blk+1, it.nskip
		for lo < hi {
			mid := int(uint(lo+hi) >> 1)
			last, _, _, ok := it.skipEntry(mid)
			if !ok {
				return it.end()
			}
			if last >= target {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		if lo > it.blk+1 {
			last, np, npos, ok := it.skipEntry(lo - 1)
			if !ok || !it.load(lo, it.base+np, last) {
				return it.end()
			}
			it.i = it.bstart - 1
			it.posOff = it.posBase + npos
			it.pending = 0
			it.posRead = true
		}
	}
	for it.nextDoc() < target {
	}
	return it.doc
}

// positions returns the positions of the current doc (valid until the
// next call). It may be called more than once per doc.
func (it *postIter) positions() []uint32 {
	if it.posRead {
		return it.posBuf
	}
	off, ok := skipUvarints(it.posData, it.posOff, it.pending)
	it.pending = 0
	it.posBuf = it.posBuf[:0]
	p := uint32(0)
	for k := uint32(0); ok && k < it.tf; k++ {
		var v uint32
		v, off, ok = uvarint32(it.posData, off)
		if k == 0 {
			p = v
		} else {
			p += v
		}
		if ok {
			it.posBuf = append(it.posBuf, p)
		}
	}
	it.posOff = off
	it.posRead = true
	return it.posBuf
}

// sliceIter iterates a sorted slice; advance gallops (exponential then
// binary search).
type sliceIter struct {
	docs []uint32
	i    int
}

func newSliceIter(d []uint32) *sliceIter { return &sliceIter{docs: d, i: -1} }

func (s *sliceIter) docID() uint32 {
	if s.i < 0 || s.i >= len(s.docs) {
		return noMore
	}
	return s.docs[s.i]
}
func (s *sliceIter) cost() int { return len(s.docs) }
func (s *sliceIter) nextDoc() uint32 {
	s.i++
	return s.docID()
}
func (s *sliceIter) advance(target uint32) uint32 {
	if s.i >= 0 && s.docID() >= target {
		return s.docID()
	}
	lo := max(s.i, 0)
	step := 1
	hi := lo
	for hi < len(s.docs) && s.docs[hi] < target {
		lo = hi + 1
		hi += step
		step *= 2
	}
	hi = min(hi, len(s.docs))
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if s.docs[mid] < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	s.i = lo
	return s.docID()
}

// allIter iterates 0..n-1.
type allIter struct {
	n   uint32
	doc uint32
	ok  bool
}

func (a *allIter) docID() uint32 {
	if !a.ok || a.doc >= a.n {
		return noMore
	}
	return a.doc
}
func (a *allIter) cost() int { return int(a.n) }
func (a *allIter) nextDoc() uint32 {
	if !a.ok {
		a.ok = true
		a.doc = 0
	} else if a.doc < a.n {
		a.doc++
	}
	return a.docID()
}
func (a *allIter) advance(t uint32) uint32 {
	if a.ok && a.docID() >= t {
		return a.docID()
	}
	a.ok = true
	a.doc = max(a.doc, min(t, a.n))
	return a.docID()
}

// andIter is a leapfrog intersection: the cheapest iterator leads, the
// others advance (skips and galloping) to its doc.
type andIter struct {
	its     []docIter // sorted by cost ascending
	doc     uint32
	started bool
}

func newAndIter(its []docIter) docIter {
	if len(its) == 1 {
		return its[0]
	}
	s := append([]docIter(nil), its...)
	for i := 1; i < len(s); i++ { // insertion sort by cost (stable)
		for j := i; j > 0 && s[j].cost() < s[j-1].cost(); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return &andIter{its: s}
}

func (a *andIter) docID() uint32 { return a.doc }
func (a *andIter) cost() int     { return a.its[0].cost() }
func (a *andIter) nextDoc() uint32 {
	a.started = true
	return a.align(a.its[0].nextDoc())
}
func (a *andIter) advance(t uint32) uint32 {
	if a.started && a.doc >= t {
		return a.doc
	}
	a.started = true
	return a.align(a.its[0].advance(t))
}

func (a *andIter) align(d uint32) uint32 {
outer:
	for d != noMore {
		for _, it := range a.its[1:] {
			x := it.advance(d)
			if x > d {
				d = a.its[0].advance(x)
				continue outer
			}
		}
		break
	}
	a.doc = d
	return d
}

// orIter is a union of iterators.
type orIter struct {
	its     []docIter
	doc     uint32
	started bool
	c       int
}

func newOrIter(its []docIter) docIter {
	if len(its) == 1 {
		return its[0]
	}
	c := 0
	for _, it := range its {
		c += it.cost()
	}
	return &orIter{its: its, c: c}
}

func (o *orIter) docID() uint32 { return o.doc }
func (o *orIter) cost() int     { return o.c }
func (o *orIter) nextDoc() uint32 {
	if !o.started {
		o.started = true
		for _, it := range o.its {
			it.nextDoc()
		}
		return o.min()
	}
	for _, it := range o.its {
		if it.docID() == o.doc {
			it.nextDoc()
		}
	}
	return o.min()
}
func (o *orIter) advance(t uint32) uint32 {
	if o.started && o.doc >= t {
		return o.doc
	}
	o.started = true
	for _, it := range o.its {
		it.advance(t)
	}
	return o.min()
}
func (o *orIter) min() uint32 {
	m := uint32(noMore)
	for _, it := range o.its {
		m = min(m, it.docID())
	}
	o.doc = m
	return m
}
