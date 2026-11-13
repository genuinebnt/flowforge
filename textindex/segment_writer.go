package textindex

import (
	"fmt"
	"slices"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Segment format constants (FORMAT.md).
const (
	segMagic      = "FFTX"
	segEndMagic   = "XTFF"
	segVersion    = 1
	segHeaderSize = 8
	segFooterSize = 16
	dirEntrySize  = 24

	secMeta      = 1
	secIDs       = 2
	secNorms     = 3
	secKeywords  = 4
	secNumerics  = 5
	secTermDict  = 6
	secPostings  = 7
	secPositions = 8
	secTrigrams  = 9
	secStored    = 10
	numSections  = 10

	postingsBlock    = 128 // postings per skip block
	dictBlock        = 64  // terms per dictionary block
	storedBlockDocs  = 16  // a stored block holds at most 16 documents
	storedBlockBytes = 16 << 10
	termSkipSize     = 12
	triSkipSize      = 8
	triEntrySize     = 12
	storedEntrySize  = 20
	maxKeyBytes      = 1 + 4*MaxTokenBytes // field id + folded term; folding may change a rune's UTF-8 length, so 4 bytes per span byte bound it
	maxStoredRaw     = 64 << 20
	maxDocBytes      = 16 << 20
)

// codec holds the pinned zstd encoder (level 3 = SpeedDefault, no frame
// checksum, one encoder goroutine) and a decoder bounded to maxStoredRaw.
// EncodeAll and DecodeAll are safe for concurrent use.
type codec struct {
	enc *zstd.Encoder
	dec *zstd.Decoder
}

func newCodec() (*codec, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderCRC(false),
		zstd.WithZeroFrames(true))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(2),
		zstd.WithDecoderMaxMemory(maxStoredRaw),
		zstd.WithDecoderMaxWindow(8<<20))
	if err != nil {
		return nil, err
	}
	return &codec{enc: enc, dec: dec}, nil
}

func (c *codec) close() {
	_ = c.enc.Close()
	c.dec.Close()
}

// IndexedDoc is a document with its generation (the sequence number of the
// operation that wrote it).
type IndexedDoc struct {
	Doc Document
	Gen uint64
}

// EncodeSegment builds an FFTX segment from docs (any order; ids must be
// distinct) with the posting codec pc (nil = VarintCodec). The bytes
// depend only on the schema, the codec and the normalized documents with
// their generations.
func EncodeSegment(s Schema, docs []IndexedDoc, pc PostingCodec) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	pc, err := checkCodec(pc)
	if err != nil {
		return nil, err
	}
	c, err := newCodec()
	if err != nil {
		return nil, err
	}
	defer c.close()
	fs := make([]docFields, len(docs))
	for i, d := range docs {
		f, err := normalize(s, d.Doc)
		if err != nil {
			return nil, err
		}
		f.gen = d.Gen
		fs[i] = f
	}
	return encodeSegment(s, fs, c, pc)
}

// termBuf collects one (field, term) key's postings while a segment is
// built; lastDoc is the last local doc appended (-1 none).
type termBuf struct {
	key     string
	docs    []uint32
	tfs     []uint32
	pos     []uint32
	lastDoc int64
}

// encodeSegment writes docs (normalized). It sorts docs by id in place:
// local doc ids follow external ids, which makes the bytes independent of
// insertion order and lets a merge of any split reproduce them
// (TIDX-INV-08).
func encodeSegment(s Schema, docs []docFields, c *codec, pc PostingCodec) ([]byte, error) {
	slices.SortFunc(docs, func(a, b docFields) int {
		switch {
		case a.id < b.id:
			return -1
		case a.id > b.id:
			return 1
		}
		return 0
	})
	for i := 1; i < len(docs); i++ {
		if docs[i].id == docs[i-1].id {
			return nil, fmt.Errorf("%w: document id %d appears twice in one segment", ErrInvalidDocument, docs[i].id)
		}
	}
	n := len(docs)
	nt := len(s.Text)

	// PORT: Go map iteration order is random. The maps only index into
	// slices (terms, triKeys), which are sorted before writing, so map
	// order never reaches the output bytes.
	termIdx := map[string]int32{}
	var terms []*termBuf
	triIdx := map[uint32]int32{}
	var triDocs [][]uint32
	var triKeys []uint32
	norms := make([][]uint32, nt)
	for f := range norms {
		norms[f] = make([]uint32, n)
	}
	sumLen := make([]uint64, nt)
	var an analyzer
	var key []byte
	var tris []uint32
	var maxGen uint64

	for d := range docs {
		doc := &docs[d]
		maxGen = max(maxGen, doc.gen)
		size := 0
		for _, v := range doc.text {
			size += len(v)
		}
		for _, v := range doc.stored {
			size += len(v)
		}
		if size > maxDocBytes {
			return nil, fmt.Errorf("%w: document %d is larger than %d bytes", ErrInvalidDocument, doc.id, maxDocBytes)
		}
		tris = tris[:0]
		for f, text := range doc.text {
			if text == "" {
				continue
			}
			tb := []byte(text)
			npos := an.run(tb, func(term []byte, pos, _, _ int, _ TokenKind) {
				key = append(append(key[:0], byte(f)), term...)
				ti, ok := termIdx[string(key)]
				if !ok {
					ti = int32(len(terms))
					k := string(key)
					termIdx[k] = ti
					terms = append(terms, &termBuf{key: k, lastDoc: -1})
				}
				t := terms[ti]
				if t.lastDoc != int64(d) {
					t.lastDoc = int64(d)
					t.docs = append(t.docs, uint32(d))
					t.tfs = append(t.tfs, 0)
				}
				t.tfs[len(t.tfs)-1]++
				t.pos = append(t.pos, uint32(pos))
			})
			norms[f][d] = uint32(npos)
			sumLen[f] += uint64(npos)
			tris = appendTrigrams(tris, tb)
		}
		for _, t := range tris {
			ti, ok := triIdx[t]
			if !ok {
				ti = int32(len(triDocs))
				triIdx[t] = ti
				triDocs = append(triDocs, nil)
				triKeys = append(triKeys, t)
			}
			// Each doc is appended once per trigram (docs arrive in order).
			if dl := triDocs[ti]; len(dl) == 0 || dl[len(dl)-1] != uint32(d) {
				triDocs[ti] = append(dl, uint32(d))
			}
		}
	}

	var sec [numSections + 1][]byte

	// META
	m := le32(nil, uint32(n))
	m = le64(m, maxGen)
	m = append(m, byte(nt))
	for f, name := range s.Text {
		m = append(m, byte(len(name)))
		m = append(m, name...)
		m = le64(m, sumLen[f])
	}
	for _, l := range [][]string{s.Keyword, s.Numeric, s.Stored} {
		m = append(m, byte(len(l)))
		for _, name := range l {
			m = append(m, byte(len(name)))
			m = append(m, name...)
		}
	}
	sec[secMeta] = m

	// IDS
	ids := make([]byte, 0, 16*n)
	for _, d := range docs {
		ids = le64(ids, d.id)
	}
	for _, d := range docs {
		ids = le64(ids, d.gen)
	}
	sec[secIDs] = ids

	// NORMS
	var nb []byte
	for f := range nt {
		for _, v := range norms[f] {
			nb = le32(nb, v)
		}
	}
	sec[secNorms] = nb

	// KEYWORDS
	var kb []byte
	for k := range s.Keyword {
		var vals []string
		for _, d := range docs {
			if v := d.keyword[k]; v != "" {
				vals = append(vals, v)
			}
		}
		slices.Sort(vals)
		vals = slices.Compact(vals)
		kb = le32(kb, uint32(len(vals)))
		for _, v := range vals {
			kb = appendUvarint(kb, uint64(len(v)))
			kb = append(kb, v...)
		}
		for _, d := range docs {
			ord := uint32(0)
			if v := d.keyword[k]; v != "" {
				i, _ := slices.BinarySearch(vals, v)
				ord = uint32(i) + 1
			}
			kb = le32(kb, ord)
		}
	}
	sec[secKeywords] = kb

	// NUMERICS
	var numb []byte
	for k := range s.Numeric {
		bm := newBitmap(n)
		for i, d := range docs {
			if d.hasNum[k] {
				bm.set(uint32(i))
			}
		}
		numb = append(numb, bm...)
		for _, d := range docs {
			v := int64(0)
			if d.hasNum[k] {
				v = d.numeric[k]
			}
			numb = le64(numb, uint64(v))
		}
	}
	sec[secNumerics] = numb

	// TERMDICT, POSTINGS, POSITIONS
	slices.SortFunc(terms, func(a, b *termBuf) int { return strings.Compare(a.key, b.key) })
	var postings, positions, blocks []byte
	var blockOffs []uint32
	prevKey := ""
	var tmp, skips []byte
	var deltas []uint32
	for i, t := range terms {
		postOff, posOff := len(postings), len(positions)
		// Blocks of 128 postings. Block k > 0 gets a skip entry (last doc of
		// block k-1, offsets of block k); the entries form one table written
		// ahead of all blocks (FORMAT.md §1.6), so they are collected in
		// skips while the blocks go to tmp. Deltas continue across blocks:
		// prev starts at the last doc of the previous block, not at 0.
		tmp, skips = tmp[:0], skips[:0]
		pi := 0
		for b := 0; b < len(t.docs); b += postingsBlock {
			end := min(b+postingsBlock, len(t.docs))
			prev := uint32(0)
			if b > 0 {
				prev = t.docs[b-1]
				skips = le32(skips, prev)
				skips = le32(skips, uint32(len(tmp)))
				skips = le32(skips, uint32(len(positions)-posOff))
			}
			deltas = deltas[:0]
			for j := b; j < end; j++ {
				deltas = append(deltas, t.docs[j]-prev)
				prev = t.docs[j]
				tf := int(t.tfs[j])
				lastPos := uint32(0)
				for k := 0; k < tf; k++ {
					p := t.pos[pi+k]
					if k == 0 {
						positions = appendUvarint(positions, uint64(p))
					} else {
						positions = appendUvarint(positions, uint64(p-lastPos))
					}
					lastPos = p
				}
				pi += tf
			}
			tmp = pc.AppendBlock(tmp, deltas, t.tfs[b:end])
		}
		postings = append(postings, skips...)
		postings = append(postings, tmp...)

		if i%dictBlock == 0 {
			blockOffs = append(blockOffs, uint32(len(blocks)))
			prevKey = ""
		}
		shared := commonPrefix(prevKey, t.key)
		blocks = appendUvarint(blocks, uint64(shared))
		blocks = appendUvarint(blocks, uint64(len(t.key)-shared))
		blocks = append(blocks, t.key[shared:]...)
		blocks = appendUvarint(blocks, uint64(len(t.docs)))
		blocks = appendUvarint(blocks, uint64(postOff))
		blocks = appendUvarint(blocks, uint64(posOff))
		prevKey = t.key
	}
	td := le32(nil, uint32(len(terms)))
	td = le32(td, uint32(len(blockOffs)))
	for _, o := range blockOffs {
		td = le32(td, o)
	}
	sec[secTermDict] = append(td, blocks...)
	sec[secPostings] = postings
	sec[secPositions] = positions

	// TRIGRAMS
	order := make([]int, len(triKeys))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int {
		switch {
		case triKeys[a] < triKeys[b]:
			return -1
		case triKeys[a] > triKeys[b]:
			return 1
		}
		return 0
	})
	tri := le32(nil, uint32(len(order)))
	var triData []byte
	for _, oi := range order {
		dl := triDocs[oi]
		tri = le32(tri, triKeys[oi])
		tri = le32(tri, uint32(len(dl)))
		tri = le32(tri, uint32(len(triData)))
		tmp, skips = tmp[:0], skips[:0]
		for b := 0; b < len(dl); b += postingsBlock {
			end := min(b+postingsBlock, len(dl))
			prev := uint32(0)
			if b > 0 {
				prev = dl[b-1]
				skips = le32(skips, prev)
				skips = le32(skips, uint32(len(tmp)))
			}
			deltas = deltas[:0]
			for _, doc := range dl[b:end] {
				deltas = append(deltas, doc-prev)
				prev = doc
			}
			tmp = pc.AppendBlock(tmp, deltas, nil)
		}
		triData = append(triData, skips...)
		triData = append(triData, tmp...)
	}
	sec[secTrigrams] = append(tri, triData...)

	// STORED
	var entries, data, raw []byte
	nblocks := 0
	first := 0
	flush := func(end int) {
		if end == first {
			return
		}
		comp := c.enc.EncodeAll(raw, nil)
		entries = le32(entries, uint32(first))
		entries = le32(entries, uint32(end-first))
		entries = le32(entries, uint32(len(data)))
		entries = le32(entries, uint32(len(comp)))
		entries = le32(entries, uint32(len(raw)))
		data = append(data, comp...)
		raw = raw[:0]
		first = end
		nblocks++
	}
	for d := range docs {
		doc := &docs[d]
		cnt := 0
		for _, v := range doc.text {
			if v != "" {
				cnt++
			}
		}
		for _, v := range doc.stored {
			if v != "" {
				cnt++
			}
		}
		raw = appendUvarint(raw, uint64(cnt))
		for f, v := range doc.text {
			if v != "" {
				raw = appendUvarint(raw, uint64(f))
				raw = appendUvarint(raw, uint64(len(v)))
				raw = append(raw, v...)
			}
		}
		for f, v := range doc.stored {
			if v != "" {
				raw = appendUvarint(raw, uint64(nt+f))
				raw = appendUvarint(raw, uint64(len(v)))
				raw = append(raw, v...)
			}
		}
		if d+1-first >= storedBlockDocs || len(raw) >= storedBlockBytes {
			flush(d + 1)
		}
	}
	flush(n)
	st := le32(nil, uint32(nblocks))
	st = append(st, entries...)
	sec[secStored] = append(st, data...)

	// Assemble.
	size := segHeaderSize + 4 + numSections*dirEntrySize + segFooterSize
	for _, b := range sec {
		size += len(b)
	}
	out := make([]byte, 0, size)
	out = append(out, segMagic...)
	out = le16(out, segVersion)
	out = le16(out, pc.ID())
	type ext struct{ off, n int }
	var exts [numSections + 1]ext
	for id := 1; id <= numSections; id++ {
		if uint64(len(sec[id])) > 0xFFFFFFFF {
			return nil, fmt.Errorf("%w: section %d exceeds 4 GiB", ErrInvalidDocument, id)
		}
		exts[id] = ext{len(out), len(sec[id])}
		out = append(out, sec[id]...)
	}
	dirOff := len(out)
	out = le32(out, numSections)
	for id := 1; id <= numSections; id++ {
		out = le32(out, uint32(id))
		out = le64(out, uint64(exts[id].off))
		out = le64(out, uint64(exts[id].n))
		out = le32(out, crc32c(sec[id]))
	}
	out = le64(out, uint64(dirOff))
	out = le32(out, crc32c(out))
	out = append(out, segEndMagic...)
	return out, nil
}

func commonPrefix(a, b string) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
