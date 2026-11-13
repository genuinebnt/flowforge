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

// postIter decodes one postings list (term or trigram) with block skips
// (FORMAT.md "Postings"). Decoding is bounds-checked: on malformed data
// the iterator ends (open-time validation makes that unreachable).
type postIter struct {
	data     []byte
	base     int // offset of the first posting (after the skip table)
	skipOff  int // offset of the skip table
	nskip    int
	skipSize int
	df       int
	withTF   bool
	posData  []byte
	posBase  int

	i       int
	off     int
	doc     uint32
	prevDoc uint32
	tf      uint32
	posOff  int
	pending int
	posRead bool
	posBuf  []uint32
}

func (it *postIter) reset(data []byte, off, df int, withTF bool, posData []byte, posOff int) {
	nskip := (df - 1) / postingsBlock
	size := triSkipSize
	if withTF {
		size = termSkipSize
	}
	*it = postIter{
		data: data, skipOff: off, base: off + nskip*size, nskip: nskip, skipSize: size,
		df: df, withTF: withTF, posData: posData, posBase: posOff,
		i: -1, off: off + nskip*size, posOff: posOff, posRead: true, posBuf: it.posBuf[:0],
	}
}

func (it *postIter) docID() uint32 { return it.doc }
func (it *postIter) cost() int     { return it.df }

func (it *postIter) end() uint32 {
	it.i, it.doc = it.df, noMore
	return noMore
}

func (it *postIter) nextDoc() uint32 {
	if it.i >= 0 && !it.posRead {
		it.pending += int(it.tf)
	}
	it.i++
	if it.i >= it.df {
		return it.end()
	}
	d, off, ok := uvarint32(it.data, it.off)
	if !ok {
		return it.end()
	}
	doc := uint64(it.prevDoc) + uint64(d)
	if doc >= noMore || (it.i > 0 && d == 0) {
		return it.end()
	}
	it.off = off
	it.doc, it.prevDoc = uint32(doc), uint32(doc)
	if it.withTF {
		tf, off, ok := uvarint32(it.data, it.off)
		if !ok || tf == 0 {
			return it.end()
		}
		it.tf, it.off, it.posRead = tf, off, false
	} else {
		it.tf = 1
	}
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
	if it.i >= 0 && (it.doc >= target) {
		return it.doc
	}
	if cur := max(it.i, 0) / postingsBlock; cur < it.nskip {
		// Within the current block? Then a linear scan is cheapest.
		if last, _, _, ok := it.skipEntry(cur); !ok || last >= target {
			goto scan
		}
		// smallest block k > cur whose last doc >= target (nskip = last block)
		lo, hi := cur+1, it.nskip
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
		if lo > cur {
			last, np, npos, ok := it.skipEntry(lo - 1)
			if !ok {
				return it.end()
			}
			it.i = lo*postingsBlock - 1
			it.off = it.base + np
			it.prevDoc = last
			it.posOff = it.posBase + npos
			it.pending = 0
			it.posRead = true
		}
	}
scan:
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
