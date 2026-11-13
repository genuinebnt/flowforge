package textindex

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
)

type storedBlock struct {
	first, count, off, clen, rlen uint32
}

// Segment is an opened, fully validated FFTX segment. It is immutable and
// safe for concurrent readers.
type Segment struct {
	data    []byte
	fileCRC uint32
	n       int
	maxGen  uint64
	schema  Schema
	sumLen  []uint64

	ids, gens []uint64
	norms     [][]uint32
	kwVals    [][]string
	kwOrds    [][]uint32
	numHas    []bitmap
	numVals   [][]int64

	termCount int
	blockOffs []uint32
	blocks    []byte
	postings  []byte
	positions []byte

	triCount int
	triEnts  []byte
	triData  []byte

	stored     []storedBlock
	storedData []byte
	codec      *codec
}

// OpenSegment validates and opens segment bytes written for schema s. It
// verifies every CRC32C and every structural rule of FORMAT.md, so a
// damaged or crafted input fails with ErrChecksum, ErrCorrupt or
// ErrSchemaMismatch and never causes an out-of-bounds read. data is
// retained.
func OpenSegment(data []byte, s Schema) (*Segment, error) {
	c, err := newCodec()
	if err != nil {
		return nil, err
	}
	return openSegment(data, s, c)
}

// DocCount is the number of documents in the segment (deleted or not).
func (sg *Segment) DocCount() int { return sg.n }

// MaxGen is the largest generation stored in the segment.
func (sg *Segment) MaxGen() uint64 { return sg.maxGen }

// Size is the segment size in bytes.
func (sg *Segment) Size() int { return len(sg.data) }

// FileCRC is the footer CRC32C of the segment.
func (sg *Segment) FileCRC() uint32 { return sg.fileCRC }

func openSegment(data []byte, s Schema, c *codec) (*Segment, error) {
	minSize := segHeaderSize + 4 + numSections*dirEntrySize + segFooterSize
	if len(data) < minSize {
		return nil, corruptf("segment of %d bytes is shorter than %d", len(data), minSize)
	}
	if string(data[:4]) != segMagic {
		return nil, corruptf("bad segment magic")
	}
	if v := binary.LittleEndian.Uint16(data[4:]); v != segVersion {
		return nil, corruptf("unsupported segment version %d", v)
	}
	if f := binary.LittleEndian.Uint16(data[6:]); f != 0 {
		return nil, corruptf("unknown segment flags %#x", f)
	}
	if string(data[len(data)-4:]) != segEndMagic {
		return nil, corruptf("bad segment end magic")
	}
	fileCRC := binary.LittleEndian.Uint32(data[len(data)-8:])
	if crc32c(data[:len(data)-8]) != fileCRC {
		return nil, fmt.Errorf("%w: segment footer", ErrChecksum)
	}
	dirOff := binary.LittleEndian.Uint64(data[len(data)-16:])
	if dirOff != uint64(len(data)-segFooterSize-4-numSections*dirEntrySize) {
		return nil, corruptf("directory offset %d inconsistent with file size", dirOff)
	}
	r := newRd(data[dirOff : len(data)-segFooterSize])
	if r.u32() != numSections {
		return nil, corruptf("section count")
	}
	var sec [numSections + 1][]byte
	next := uint64(segHeaderSize)
	for id := 1; id <= numSections; id++ {
		gotID, off, n, crc := r.u32(), r.u64(), r.u64(), r.u32()
		if gotID != uint32(id) || off != next || n > dirOff-off {
			return nil, corruptf("section %d extent", id)
		}
		b := data[off : off+n]
		if crc32c(b) != crc {
			return nil, fmt.Errorf("%w: section %d", ErrChecksum, id)
		}
		sec[id] = b
		next = off + n
	}
	if next != dirOff {
		return nil, corruptf("sections do not end at the directory")
	}
	sg := &Segment{data: data, fileCRC: fileCRC, schema: s, codec: c}
	steps := []func([]byte) error{
		nil, sg.parseMeta, sg.parseIDs, sg.parseNorms, sg.parseKeywords, sg.parseNumerics,
		sg.parseTermDict, nil, nil, sg.parseTrigrams, sg.parseStored,
	}
	sg.postings, sg.positions = sec[secPostings], sec[secPositions]
	for id := 1; id <= numSections; id++ {
		if steps[id] == nil {
			continue
		}
		if err := steps[id](sec[id]); err != nil {
			return nil, err
		}
	}
	return sg, nil
}

func (sg *Segment) parseMeta(b []byte) error {
	r := newRd(b)
	n := r.u32()
	sg.maxGen = r.u64()
	if n > 1<<28 {
		return corruptf("doc count %d", n)
	}
	sg.n = int(n)
	var got Schema
	readNames := func(withSum bool) []string {
		cnt := int(r.u8())
		var out []string
		for i := 0; i < cnt && r.ok; i++ {
			l := int(r.u8())
			out = append(out, string(r.bytes(l)))
			if withSum {
				sg.sumLen = append(sg.sumLen, r.u64())
			}
		}
		return out
	}
	got.Text = readNames(true)
	got.Keyword = readNames(false)
	got.Numeric = readNames(false)
	got.Stored = readNames(false)
	if !r.done() {
		return corruptf("meta section")
	}
	if !got.Equal(sg.schema) {
		return fmt.Errorf("%w: segment has %v, index has %v", ErrSchemaMismatch, got, sg.schema)
	}
	return nil
}

func (sg *Segment) parseIDs(b []byte) error {
	if len(b) != 16*sg.n {
		return corruptf("ids section size")
	}
	sg.ids = make([]uint64, sg.n)
	sg.gens = make([]uint64, sg.n)
	for i := range sg.n {
		sg.ids[i] = binary.LittleEndian.Uint64(b[8*i:])
		sg.gens[i] = binary.LittleEndian.Uint64(b[8*(sg.n+i):])
		if i > 0 && sg.ids[i] <= sg.ids[i-1] {
			return corruptf("ids not strictly increasing")
		}
		if sg.gens[i] > sg.maxGen {
			return corruptf("generation above max")
		}
	}
	return nil
}

func (sg *Segment) parseNorms(b []byte) error {
	nt := len(sg.schema.Text)
	if len(b) != 4*sg.n*nt {
		return corruptf("norms section size")
	}
	sg.norms = make([][]uint32, nt)
	for f := range nt {
		sg.norms[f] = make([]uint32, sg.n)
		var sum uint64
		for d := range sg.n {
			v := binary.LittleEndian.Uint32(b[4*(f*sg.n+d):])
			sg.norms[f][d] = v
			sum += uint64(v)
		}
		if sum != sg.sumLen[f] {
			return corruptf("norms of field %d do not add up", f)
		}
	}
	return nil
}

func (sg *Segment) parseKeywords(b []byte) error {
	r := newRd(b)
	for range sg.schema.Keyword {
		cnt := int(r.u32())
		if cnt > sg.n {
			return corruptf("keyword value count")
		}
		vals := make([]string, 0, cnt)
		for i := 0; i < cnt && r.ok; i++ {
			l := int(r.uvarint())
			v := string(r.bytes(l))
			if !r.ok || v == "" || (i > 0 && v <= vals[i-1]) {
				return corruptf("keyword values")
			}
			vals = append(vals, v)
		}
		ords := make([]uint32, sg.n)
		for d := 0; d < sg.n && r.ok; d++ {
			ords[d] = r.u32()
			if ords[d] > uint32(cnt) {
				return corruptf("keyword ordinal")
			}
		}
		sg.kwVals = append(sg.kwVals, vals)
		sg.kwOrds = append(sg.kwOrds, ords)
	}
	if !r.done() {
		return corruptf("keywords section")
	}
	return nil
}

func (sg *Segment) parseNumerics(b []byte) error {
	r := newRd(b)
	nb := bitmapBytes(sg.n)
	for range sg.schema.Numeric {
		bm := bitmap(slices.Clone(r.bytes(nb)))
		if !r.ok {
			return corruptf("numerics section")
		}
		if sg.n%8 != 0 && bm[nb-1]>>(sg.n%8) != 0 {
			return corruptf("numeric presence padding")
		}
		vals := make([]int64, sg.n)
		for d := 0; d < sg.n; d++ {
			vals[d] = int64(r.u64())
			if !bm.get(uint32(d)) && vals[d] != 0 {
				return corruptf("absent numeric value not zero")
			}
		}
		sg.numHas = append(sg.numHas, bm)
		sg.numVals = append(sg.numVals, vals)
	}
	if !r.done() {
		return corruptf("numerics section")
	}
	return nil
}

func (sg *Segment) parseTermDict(b []byte) error {
	r := newRd(b)
	tc := int(r.u32())
	bc := int(r.u32())
	if !r.ok || bc != (tc+dictBlock-1)/dictBlock || bc > len(b)/4 {
		return corruptf("term dictionary header")
	}
	sg.termCount = tc
	sg.blockOffs = make([]uint32, bc)
	for i := range sg.blockOffs {
		sg.blockOffs[i] = r.u32()
	}
	if !r.ok {
		return corruptf("term dictionary block index")
	}
	sg.blocks = b[r.off:]
	// Decode every entry: keys strictly increasing, block boundaries exact,
	// postings and positions contiguous and valid.
	var key, prev []byte
	postOff, posOff := 0, 0
	br := newRd(sg.blocks)
	for i := 0; i < tc; i++ {
		if i%dictBlock == 0 && uint32(br.off) != sg.blockOffs[i/dictBlock] {
			return corruptf("block %d offset", i/dictBlock)
		}
		shared := int(br.uvarint())
		suffix := int(br.uvarint())
		if !br.ok || shared > len(prev) || (i%dictBlock == 0 && shared != 0) || shared+suffix > maxKeyBytes || shared+suffix < 2 {
			return corruptf("term %d key", i)
		}
		key = append(append(key[:0], prev[:shared]...), br.bytes(suffix)...)
		df := int(br.uvarint())
		po := int(br.uvarint())
		pp := int(br.uvarint())
		if !br.ok || (i > 0 && bytes.Compare(key, prev) <= 0) || int(key[0]) >= len(sg.schema.Text) {
			return corruptf("term %d order", i)
		}
		if df < 1 || df > sg.n || po != postOff || pp != posOff {
			return corruptf("term %d postings reference", i)
		}
		var err error
		postOff, posOff, err = sg.validatePostings(int(key[0]), df, po, pp)
		if err != nil {
			return err
		}
		prev = append(prev[:0], key...)
	}
	if !br.done() || postOff != len(sg.postings) || posOff != len(sg.positions) {
		return corruptf("term dictionary trailing data")
	}
	return nil
}

// validatePostings decodes one term's postings and positions completely
// and returns the offsets just past them.
func (sg *Segment) validatePostings(field, df, po, pp int) (int, int, error) {
	nskip := (df - 1) / postingsBlock
	sk := newRd(sg.postings)
	sk.off = po
	if !sk.need(nskip * termSkipSize) {
		return 0, 0, corruptf("skip table out of range")
	}
	base := po + nskip*termSkipSize
	off := base
	pos := pp
	prev := uint32(0)
	norms := sg.norms[field]
	for j := 0; j < df; j++ {
		if j > 0 && j%postingsBlock == 0 {
			last, np, npos := sk.u32(), int(sk.u32()), int(sk.u32())
			if last != prev || np != off-base || npos != pos-pp {
				return 0, 0, corruptf("skip entry %d", j/postingsBlock-1)
			}
		}
		d, o, ok := uvarint32(sg.postings, off)
		if !ok || (j > 0 && d == 0) {
			return 0, 0, corruptf("doc delta")
		}
		doc := uint64(prev) + uint64(d)
		if doc >= uint64(sg.n) {
			return 0, 0, corruptf("doc %d out of range", doc)
		}
		tf, o2, ok := uvarint32(sg.postings, o)
		if !ok || tf == 0 || tf > norms[doc] {
			return 0, 0, corruptf("term frequency")
		}
		off = o2
		prev = uint32(doc)
		p := uint64(0)
		for k := uint32(0); k < tf; k++ {
			v, o3, ok := uvarint32(sg.positions, pos)
			if !ok || (k > 0 && v == 0) {
				return 0, 0, corruptf("position")
			}
			if k == 0 {
				p = uint64(v)
			} else {
				p += uint64(v)
			}
			if p >= uint64(norms[doc]) {
				return 0, 0, corruptf("position beyond field length")
			}
			pos = o3
		}
	}
	return off, pos, nil
}

func (sg *Segment) parseTrigrams(b []byte) error {
	r := newRd(b)
	cnt := int(r.u32())
	if !r.ok || cnt > (len(b)-4)/triEntrySize {
		return corruptf("trigram count")
	}
	sg.triCount = cnt
	sg.triEnts = b[4 : 4+cnt*triEntrySize]
	sg.triData = b[4+cnt*triEntrySize:]
	off := 0
	var prevTri uint32
	for i := 0; i < cnt; i++ {
		e := sg.triEnts[i*triEntrySize:]
		tri := binary.LittleEndian.Uint32(e)
		df := int(binary.LittleEndian.Uint32(e[4:]))
		o := int(binary.LittleEndian.Uint32(e[8:]))
		if tri >= 1<<24 || (i > 0 && tri <= prevTri) || df < 1 || df > sg.n || o != off {
			return corruptf("trigram entry %d", i)
		}
		prevTri = tri
		nskip := (df - 1) / postingsBlock
		sk := newRd(sg.triData)
		sk.off = o
		if !sk.need(nskip * triSkipSize) {
			return corruptf("trigram skip table")
		}
		base := o + nskip*triSkipSize
		p := base
		prev := uint32(0)
		for j := 0; j < df; j++ {
			if j > 0 && j%postingsBlock == 0 {
				last, np := sk.u32(), int(sk.u32())
				if last != prev || np != p-base {
					return corruptf("trigram skip entry")
				}
			}
			d, o2, ok := uvarint32(sg.triData, p)
			if !ok || (j > 0 && d == 0) || uint64(prev)+uint64(d) >= uint64(sg.n) {
				return corruptf("trigram postings")
			}
			prev += d
			p = o2
		}
		off = p
	}
	if off != len(sg.triData) {
		return corruptf("trigram trailing data")
	}
	return nil
}

func (sg *Segment) parseStored(b []byte) error {
	r := newRd(b)
	bc := int(r.u32())
	if !r.ok || bc > (len(b)-4)/storedEntrySize {
		return corruptf("stored block count")
	}
	sg.stored = make([]storedBlock, bc)
	dataLen := uint64(len(b) - 4 - bc*storedEntrySize)
	first, off := uint64(0), uint64(0)
	for i := range sg.stored {
		e := storedBlock{r.u32(), r.u32(), r.u32(), r.u32(), r.u32()}
		if uint64(e.first) != first || e.count == 0 || e.count > storedBlockDocs || uint64(e.off) != off ||
			e.clen == 0 || off+uint64(e.clen) > dataLen || e.rlen > maxStoredRaw {
			return corruptf("stored block %d", i)
		}
		sg.stored[i] = e
		first += uint64(e.count)
		off += uint64(e.clen)
	}
	if !r.ok || first != uint64(sg.n) || off != dataLen {
		return corruptf("stored blocks do not cover the segment")
	}
	sg.storedData = b[r.off:]
	nf := len(sg.schema.Text) + len(sg.schema.Stored)
	for i := range sg.stored {
		raw, err := sg.blockRaw(i)
		if err != nil {
			return err
		}
		br := newRd(raw)
		for d := uint32(0); d < sg.stored[i].count; d++ {
			cnt := int(br.uvarint())
			last := -1
			for k := 0; k < cnt && br.ok; k++ {
				f := int(br.uvarint())
				l := int(br.uvarint())
				if f <= last || f >= nf || l == 0 {
					return corruptf("stored record")
				}
				br.bytes(l)
				last = f
			}
		}
		if !br.done() {
			return corruptf("stored block %d records", i)
		}
	}
	return nil
}

func (sg *Segment) blockRaw(i int) ([]byte, error) {
	e := sg.stored[i]
	comp := sg.storedData[e.off : e.off+e.clen]
	raw, err := sg.codec.dec.DecodeAll(comp, make([]byte, 0, e.rlen))
	if err != nil || len(raw) != int(e.rlen) {
		return nil, corruptf("stored block %d does not decompress", i)
	}
	return raw, nil
}

// lookupTerm finds the dictionary entry of (field, term).
func (sg *Segment) lookupTerm(field int, term string, scratch []byte) (df, postOff, posOff int, ok bool) {
	if sg.termCount == 0 {
		return 0, 0, 0, false
	}
	key := append(append(scratch[:0], byte(field)), term...)
	// last block whose first key <= key
	lo, hi := 0, len(sg.blockOffs)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if bytes.Compare(sg.firstKey(mid), key) <= 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return 0, 0, 0, false
	}
	blk := lo - 1
	r := newRd(sg.blocks)
	r.off = int(sg.blockOffs[blk])
	var cur []byte
	cnt := min(dictBlock, sg.termCount-blk*dictBlock)
	for i := 0; i < cnt; i++ {
		shared := int(r.uvarint())
		suffix := int(r.uvarint())
		if !r.ok || shared > len(cur) {
			return 0, 0, 0, false
		}
		cur = append(cur[:shared], r.bytes(suffix)...)
		d, po, pp := int(r.uvarint()), int(r.uvarint()), int(r.uvarint())
		if !r.ok {
			return 0, 0, 0, false
		}
		switch c := bytes.Compare(cur, key); {
		case c == 0:
			return d, po, pp, true
		case c > 0:
			return 0, 0, 0, false
		}
	}
	return 0, 0, 0, false
}

func (sg *Segment) firstKey(blk int) []byte {
	r := newRd(sg.blocks)
	r.off = int(sg.blockOffs[blk])
	r.uvarint()
	l := int(r.uvarint())
	return r.bytes(l)
}

// termIter returns an iterator over the postings of (field, term), or
// nil if the term is absent.
func (sg *Segment) termIter(field int, term string, scratch []byte) *postIter {
	df, po, pp, ok := sg.lookupTerm(field, term, scratch)
	if !ok {
		return nil
	}
	it := &postIter{}
	it.reset(sg.postings, po, df, true, sg.positions, pp)
	return it
}

// trigramIter returns an iterator over the docs containing trigram t, or
// nil.
func (sg *Segment) trigramIter(t uint32) *postIter {
	lo, hi := 0, sg.triCount
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if binary.LittleEndian.Uint32(sg.triEnts[mid*triEntrySize:]) < t {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == sg.triCount {
		return nil
	}
	e := sg.triEnts[lo*triEntrySize:]
	if binary.LittleEndian.Uint32(e) != t {
		return nil
	}
	it := &postIter{}
	it.reset(sg.triData, int(binary.LittleEndian.Uint32(e[8:])), int(binary.LittleEndian.Uint32(e[4:])), false, nil, 0)
	return it
}

// findID returns the local doc of id.
func (sg *Segment) findID(id uint64) (uint32, bool) {
	i, ok := slices.BinarySearch(sg.ids, id)
	return uint32(i), ok
}

// storedCache keeps the last decompressed stored block.
type storedCache struct {
	sg    *Segment
	block int
	raw   []byte
}

// storedFields returns the stored field values of doc: text fields then
// stored-only fields (schema order), nil when absent. The slices alias the
// cache's decompressed block and are valid until the cache moves.
func (sg *Segment) storedFields(doc uint32, cache *storedCache, dst [][]byte) ([][]byte, error) {
	blk, _ := slices.BinarySearchFunc(sg.stored, doc, func(e storedBlock, d uint32) int {
		switch {
		case e.first+e.count <= d:
			return -1
		case e.first > d:
			return 1
		}
		return 0
	})
	if blk >= len(sg.stored) {
		return nil, corruptf("doc %d has no stored block", doc)
	}
	if cache.sg != sg || cache.block != blk || cache.raw == nil {
		raw, err := sg.blockRaw(blk)
		if err != nil {
			return nil, err
		}
		*cache = storedCache{sg: sg, block: blk, raw: raw}
	}
	nf := len(sg.schema.Text) + len(sg.schema.Stored)
	out := slices.Grow(dst[:0], nf)[:nf]
	clear(out)
	r := newRd(cache.raw)
	for d := sg.stored[blk].first; d <= doc && r.ok; d++ {
		cnt := int(r.uvarint())
		for k := 0; k < cnt && r.ok; k++ {
			f := int(r.uvarint())
			l := int(r.uvarint())
			v := r.bytes(l)
			if d == doc && f < nf {
				out[f] = v
			}
		}
	}
	if !r.ok {
		return nil, corruptf("stored record of doc %d", doc)
	}
	return out, nil
}

// docFieldsAt reconstructs the normalized document at local doc.
func (sg *Segment) docFieldsAt(doc uint32, cache *storedCache) (docFields, error) {
	if cache == nil {
		cache = &storedCache{}
	}
	raw, err := sg.storedFields(doc, cache, nil)
	if err != nil {
		return docFields{}, err
	}
	vals := make([]string, len(raw))
	for i, v := range raw {
		vals[i] = string(v)
	}
	s := sg.schema
	nt := len(s.Text)
	f := docFields{
		id: sg.ids[doc], gen: sg.gens[doc],
		text: vals[:nt], stored: vals[nt:],
		keyword: make([]string, len(s.Keyword)),
		numeric: make([]int64, len(s.Numeric)),
		hasNum:  make([]bool, len(s.Numeric)),
	}
	for k := range s.Keyword {
		if o := sg.kwOrds[k][doc]; o > 0 {
			f.keyword[k] = sg.kwVals[k][o-1]
		}
	}
	for k := range s.Numeric {
		if sg.numHas[k].get(doc) {
			f.hasNum[k] = true
			f.numeric[k] = sg.numVals[k][doc]
		}
	}
	return f, nil
}

// Documents decodes every document of the segment in id order with its
// generation (used by tests, merges and tools).
func (sg *Segment) Documents() ([]IndexedDoc, error) {
	var cache storedCache
	out := make([]IndexedDoc, 0, sg.n)
	for d := 0; d < sg.n; d++ {
		f, err := sg.docFieldsAt(uint32(d), &cache)
		if err != nil {
			return nil, err
		}
		out = append(out, IndexedDoc{Doc: f.document(sg.schema), Gen: f.gen})
	}
	return out, nil
}
