package vecindex

import (
	"encoding/binary"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"os"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/genuinebnt/flowforge/core"
)

// Segment file format "FFDA" (spec 22.5), all integers little-endian:
//
//	sector 0        header (layout below), CRC32C of bytes [0, 4092) in [4092, 4096)
//	sector 1..      node section: node records, never straddling a sector
//	(aligned)       codebook section: f32[K][dim] in sub-space order
//	(aligned)       code section: u8[V][M]
//	(aligned)       id section: { uuid [16]byte | source_seq u64 }[V]
//	end - 8         trailer: "FFDT" | CRC32C(body || header sector)
//
// where body is every byte between the header sector and the trailer. A
// node record is { vector f32[dim] | degree u32 | neighbours u32[R] }. When
// a record fits a sector, floor(4096 / record) records share each sector
// and node i lives at sector 1 + i / perSector, slot i mod perSector; a
// larger record (for example dim 960 with R 64) starts on its own sector
// and occupies ceil(record / 4096) sectors. Every section carries its own
// CRC32C in the header and the trailer covers the whole file, so any
// flipped byte fails OpenSegment with ErrChecksum (vec_007).
const (
	// SectorSize is the I/O unit of the segment layout.
	SectorSize = 4096

	segmentMagic   = "FFDA"
	trailerMagic   = "FFDT"
	segmentVersion = 1
	trailerSize    = 8
	idEntrySize    = 24
	headerCRCOff   = SectorSize - 4

	// Header field offsets.
	hMagic      = 0
	hVersion    = 4
	hDim        = 8
	hMetric     = 12
	hR          = 16
	hCount      = 24
	hMedoid     = 32
	hPQM        = 36
	hPQK        = 40
	hRecordSize = 44
	hEpoch      = 48
	hMaxSeq     = 56
	hSections   = 64 // 4 x { off u64 | len u64 | crc u32 | pad u32 }
	hFileSize   = 160

	sectionNodes    = 0
	sectionCodebook = 1
	sectionCodes    = 2
	sectionIDs      = 3

	streamChunk = 256 * SectorSize
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// section is the extent and checksum of one header-declared section.
type section struct {
	off, len uint64
	crc      uint32
}

// segmentHeader is the decoded header sector.
type segmentHeader struct {
	dim, r         uint32
	metric         Metric
	count          uint64
	medoid         uint32
	pqM, pqK       uint32
	recordSize     uint32
	tombstoneEpoch uint64
	maxSeq         uint64
	sections       [4]section
	fileSize       uint64
}

// layout is the byte layout implied by (dim, R, V, M, K).
type layout struct {
	recordSize       uint64
	perSector        uint64 // records per sector, 0 when a record spans sectors
	sectorsPerRecord uint64
	sections         [4]section // crc unset
	trailerOff       uint64
	fileSize         uint64
}

func alignUp(x uint64) uint64 { return (x + SectorSize - 1) / SectorSize * SectorSize }

// computeLayout derives the layout. Inputs are bounded by the caller
// (dim <= 2^16, R <= 1024, V < 2^32, M <= dim, K <= 256), so no product
// overflows uint64.
func computeLayout(dim, r, count, m, k uint64) layout {
	var l layout
	l.recordSize = 4*dim + 4 + 4*r
	var nodeSectors uint64
	if l.recordSize <= SectorSize {
		l.perSector = SectorSize / l.recordSize
		nodeSectors = (count + l.perSector - 1) / l.perSector
	} else {
		l.sectorsPerRecord = (l.recordSize + SectorSize - 1) / SectorSize
		nodeSectors = count * l.sectorsPerRecord
	}
	l.sections[sectionNodes] = section{off: SectorSize, len: nodeSectors * SectorSize}
	cb := alignUp(SectorSize + nodeSectors*SectorSize)
	l.sections[sectionCodebook] = section{off: cb, len: k * dim * 4}
	codes := alignUp(cb + k*dim*4)
	l.sections[sectionCodes] = section{off: codes, len: count * m}
	ids := alignUp(codes + count*m)
	l.sections[sectionIDs] = section{off: ids, len: count * idEntrySize}
	l.trailerOff = ids + count*idEntrySize
	l.fileSize = l.trailerOff + trailerSize
	return l
}

// recordOffset is the file offset of node i's record.
func (l *layout) recordOffset(i uint64) uint64 {
	base := l.sections[sectionNodes].off
	if l.perSector > 0 {
		return base + (i/l.perSector)*SectorSize + (i%l.perSector)*l.recordSize
	}
	return base + i*l.sectorsPerRecord*SectorSize
}

func (h *segmentHeader) encode() []byte {
	b := make([]byte, SectorSize)
	copy(b[hMagic:], segmentMagic)
	le := binary.LittleEndian
	le.PutUint32(b[hVersion:], segmentVersion)
	le.PutUint32(b[hDim:], h.dim)
	le.PutUint32(b[hMetric:], uint32(h.metric))
	le.PutUint32(b[hR:], h.r)
	le.PutUint64(b[hCount:], h.count)
	le.PutUint32(b[hMedoid:], h.medoid)
	le.PutUint32(b[hPQM:], h.pqM)
	le.PutUint32(b[hPQK:], h.pqK)
	le.PutUint32(b[hRecordSize:], h.recordSize)
	le.PutUint64(b[hEpoch:], h.tombstoneEpoch)
	le.PutUint64(b[hMaxSeq:], h.maxSeq)
	for i, s := range h.sections {
		o := hSections + i*24
		le.PutUint64(b[o:], s.off)
		le.PutUint64(b[o+8:], s.len)
		le.PutUint32(b[o+16:], s.crc)
	}
	le.PutUint64(b[hFileSize:], h.fileSize)
	le.PutUint32(b[headerCRCOff:], crc32.Checksum(b[:headerCRCOff], castagnoli))
	return b
}

// decodeHeader checks the header checksum first, so a flipped header byte
// is reported as ErrChecksum, then parses and validates every field.
func decodeHeader(b []byte, size int64) (segmentHeader, layout, error) {
	var h segmentHeader
	le := binary.LittleEndian
	if crc32.Checksum(b[:headerCRCOff], castagnoli) != le.Uint32(b[headerCRCOff:]) {
		return h, layout{}, fmt.Errorf("%w: header", ErrChecksum)
	}
	if string(b[hMagic:hMagic+4]) != segmentMagic {
		return h, layout{}, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	if v := le.Uint32(b[hVersion:]); v != segmentVersion {
		return h, layout{}, fmt.Errorf("%w: unsupported version %d", ErrCorrupt, v)
	}
	h.dim = le.Uint32(b[hDim:])
	h.metric = Metric(le.Uint32(b[hMetric:]))
	if m := le.Uint32(b[hMetric:]); m > math.MaxUint8 {
		h.metric = MetricUnspecified
	}
	h.r = le.Uint32(b[hR:])
	h.count = le.Uint64(b[hCount:])
	h.medoid = le.Uint32(b[hMedoid:])
	h.pqM = le.Uint32(b[hPQM:])
	h.pqK = le.Uint32(b[hPQK:])
	h.recordSize = le.Uint32(b[hRecordSize:])
	h.tombstoneEpoch = le.Uint64(b[hEpoch:])
	h.maxSeq = le.Uint64(b[hMaxSeq:])
	for i := range h.sections {
		o := hSections + i*24
		h.sections[i] = section{off: le.Uint64(b[o:]), len: le.Uint64(b[o+8:]), crc: le.Uint32(b[o+16:])}
	}
	h.fileSize = le.Uint64(b[hFileSize:])

	switch {
	case h.dim < 1 || h.dim > MaxDimension:
		return h, layout{}, fmt.Errorf("%w: dimension %d", ErrCorrupt, h.dim)
	case !h.metric.Valid():
		return h, layout{}, fmt.Errorf("%w: metric %d", ErrCorrupt, h.metric)
	case h.r < 1 || h.r > MaxR:
		return h, layout{}, fmt.Errorf("%w: R=%d", ErrCorrupt, h.r)
	case h.count < 1 || h.count > math.MaxUint32:
		return h, layout{}, fmt.Errorf("%w: vector count %d", ErrCorrupt, h.count)
	case uint64(h.medoid) >= h.count:
		return h, layout{}, fmt.Errorf("%w: medoid %d >= %d", ErrCorrupt, h.medoid, h.count)
	case h.pqM < 1 || h.pqM > h.dim:
		return h, layout{}, fmt.Errorf("%w: pq m=%d", ErrCorrupt, h.pqM)
	case h.pqK < 1 || h.pqK > PQCentroids:
		return h, layout{}, fmt.Errorf("%w: pq k=%d", ErrCorrupt, h.pqK)
	}
	l := computeLayout(uint64(h.dim), uint64(h.r), h.count, uint64(h.pqM), uint64(h.pqK))
	if uint64(h.recordSize) != l.recordSize {
		return h, layout{}, fmt.Errorf("%w: record size %d, want %d", ErrCorrupt, h.recordSize, l.recordSize)
	}
	for i, s := range h.sections {
		if s.off != l.sections[i].off || s.len != l.sections[i].len {
			return h, layout{}, fmt.Errorf("%w: section %d extent", ErrCorrupt, i)
		}
	}
	if h.fileSize != l.fileSize || size < 0 || uint64(size) != l.fileSize {
		return h, layout{}, fmt.Errorf("%w: file size %d, header says %d, layout needs %d", ErrCorrupt, size, h.fileSize, l.fileSize)
	}
	return h, l, nil
}

// SegmentEntry is one vector to write into a segment.
type SegmentEntry struct {
	ID     core.UUID
	Seq    uint64 // source seq of the vector
	Vector []float32
}

// SegmentConfig configures segment construction.
type SegmentConfig struct {
	Metric Metric
	Graph  VamanaParams
	// PQ configures the quantiser; PQ.M zero means max(1, dim/8)
	// (spec 22.5: m = 96 for dim 768).
	PQ PQConfig
	// TombstoneEpoch is recorded in the header: the index generation the
	// segment was built at.
	TombstoneEpoch uint64
}

// WriteSegment builds a Vamana graph and a product quantiser over entries
// and writes the segment file to w, returning its size. Every random
// choice comes from rng. Under Cosine the vectors are normalised first.
func WriteSegment(w io.WriterAt, entries []SegmentEntry, cfg SegmentConfig, rng core.Rng) (int64, error) {
	if len(entries) == 0 {
		return 0, fmt.Errorf("vecindex: write segment: %w", ErrEmpty)
	}
	if !cfg.Metric.Valid() {
		return 0, fmt.Errorf("%w: %v", ErrInvalidMetric, cfg.Metric)
	}
	dim := len(entries[0].Vector)
	ids := make([]core.UUID, len(entries))
	seqs := make([]uint64, len(entries))
	flat := make([]float32, 0, len(entries)*dim)
	for i, e := range entries {
		pv, err := prepareVector(cfg.Metric, dim, e.Vector)
		if err != nil {
			return 0, fmt.Errorf("vecindex: write segment: entry %d: %w", i, err)
		}
		ids[i], seqs[i] = e.ID, e.Seq
		flat = append(flat, pv...)
	}
	return buildSegment(w, ids, seqs, flat, dim, cfg, rng)
}

// buildSegment builds and writes a segment over prepared row-major vectors.
func buildSegment(w io.WriterAt, ids []core.UUID, seqs []uint64, flat []float32, dim int, cfg SegmentConfig, rng core.Rng) (int64, error) {
	if dim < 1 || dim > MaxDimension {
		return 0, fmt.Errorf("%w: dimension %d", ErrInvalidConfig, dim)
	}
	g, err := buildGraphFlat(flat, dim, cfg.Metric, cfg.Graph, rng)
	if err != nil {
		return 0, fmt.Errorf("vecindex: write segment: %w", err)
	}
	pqCfg := cfg.PQ
	if pqCfg.M == 0 {
		pqCfg.M = max(1, dim/8)
	}
	pq, err := trainPQFlat(flat, len(ids), dim, pqCfg, rng)
	if err != nil {
		return 0, fmt.Errorf("vecindex: write segment: %w", err)
	}
	return writeSegmentFile(w, g, pq, ids, seqs, cfg.TombstoneEpoch)
}

// bodyWriter streams the body at increasing offsets, maintaining the
// running whole-file checksum and the current section's checksum.
type bodyWriter struct {
	w    io.WriterAt
	off  uint64 // next file offset
	buf  []byte
	base uint64 // file offset of buf[0]
	file hash.Hash32
	sec  hash.Hash32
	err  error
}

func (bw *bodyWriter) write(p []byte) {
	if bw.err != nil {
		return
	}
	bw.file.Write(p)
	if bw.sec != nil {
		bw.sec.Write(p)
	}
	bw.buf = append(bw.buf, p...)
	bw.off += uint64(len(p))
	if len(bw.buf) >= streamChunk {
		bw.flush()
	}
}

var zeroSector [SectorSize]byte

// padTo writes zeros up to file offset to.
func (bw *bodyWriter) padTo(to uint64) {
	for bw.off < to && bw.err == nil {
		n := min(to-bw.off, SectorSize)
		bw.write(zeroSector[:n])
	}
}

func (bw *bodyWriter) flush() {
	if bw.err != nil || len(bw.buf) == 0 {
		return
	}
	if _, err := bw.w.WriteAt(bw.buf, int64(bw.base)); err != nil {
		bw.err = fmt.Errorf("vecindex: write segment: %w", err)
	}
	bw.base += uint64(len(bw.buf))
	bw.buf = bw.buf[:0]
}

// beginSection starts checksumming a section at its declared offset.
func (bw *bodyWriter) beginSection(s section) {
	bw.padTo(s.off)
	bw.sec = crc32.New(castagnoli)
}

// endSection pads to the section end and returns its checksum.
func (bw *bodyWriter) endSection(s section) uint32 {
	bw.padTo(s.off + s.len)
	c := bw.sec.Sum32()
	bw.sec = nil
	return c
}

// writeSegmentFile serialises a built graph and quantiser.
func writeSegmentFile(w io.WriterAt, g *Graph, pq *PQ, ids []core.UUID, seqs []uint64, epoch uint64) (int64, error) {
	n := g.len()
	h := segmentHeader{
		dim: uint32(g.dim), r: uint32(g.r), metric: g.metric, count: uint64(n),
		medoid: g.medoid, pqM: uint32(pq.m), pqK: uint32(pq.k), tombstoneEpoch: epoch,
	}
	for _, s := range seqs {
		h.maxSeq = max(h.maxSeq, s)
	}
	l := computeLayout(uint64(g.dim), uint64(g.r), uint64(n), uint64(pq.m), uint64(pq.k))
	h.recordSize = uint32(l.recordSize)
	h.fileSize = l.fileSize
	bw := &bodyWriter{w: w, off: SectorSize, base: SectorSize, file: crc32.New(castagnoli), buf: make([]byte, 0, streamChunk+SectorSize)}
	le := binary.LittleEndian

	// Node records.
	rec := make([]byte, l.recordSize)
	bw.beginSection(l.sections[sectionNodes])
	for i := range n {
		clear(rec)
		for j, x := range g.vec(uint32(i)) {
			le.PutUint32(rec[4*j:], math.Float32bits(x))
		}
		o := 4 * g.dim
		adj := g.adj[i]
		le.PutUint32(rec[o:], uint32(len(adj)))
		for j, nb := range adj {
			le.PutUint32(rec[o+4+4*j:], nb)
		}
		bw.padTo(l.recordOffset(uint64(i)))
		bw.write(rec)
	}
	h.sections[sectionNodes] = l.sections[sectionNodes]
	h.sections[sectionNodes].crc = bw.endSection(l.sections[sectionNodes])

	// Codebooks.
	bw.beginSection(l.sections[sectionCodebook])
	fb := make([]byte, 4)
	for _, x := range pq.codebooks {
		le.PutUint32(fb, math.Float32bits(x))
		bw.write(fb)
	}
	h.sections[sectionCodebook] = l.sections[sectionCodebook]
	h.sections[sectionCodebook].crc = bw.endSection(l.sections[sectionCodebook])

	// PQ codes.
	bw.beginSection(l.sections[sectionCodes])
	code := make([]byte, pq.m)
	for i := range n {
		pq.encodeInto(g.vec(uint32(i)), code)
		bw.write(code)
	}
	h.sections[sectionCodes] = l.sections[sectionCodes]
	h.sections[sectionCodes].crc = bw.endSection(l.sections[sectionCodes])

	// Id table.
	bw.beginSection(l.sections[sectionIDs])
	ent := make([]byte, idEntrySize)
	for i := range n {
		copy(ent, ids[i][:])
		le.PutUint64(ent[16:], seqs[i])
		bw.write(ent)
	}
	h.sections[sectionIDs] = l.sections[sectionIDs]
	h.sections[sectionIDs].crc = bw.endSection(l.sections[sectionIDs])
	bw.flush()
	if bw.err != nil {
		return 0, bw.err
	}

	hdr := h.encode()
	bw.file.Write(hdr)
	var tr [trailerSize]byte
	copy(tr[:], trailerMagic)
	le.PutUint32(tr[4:], bw.file.Sum32())
	if _, err := w.WriteAt(tr[:], int64(l.trailerOff)); err != nil {
		return 0, fmt.Errorf("vecindex: write segment trailer: %w", err)
	}
	if _, err := w.WriteAt(hdr, 0); err != nil {
		return 0, fmt.Errorf("vecindex: write segment header: %w", err)
	}
	return int64(l.fileSize), nil
}

// Segment is an open, immutable segment file (spec 22.5). PQ codes, the
// codebooks and the id table live in RAM; full vectors and adjacency lists
// stay in the file and are read per search through an io.ReaderAt. It is
// safe for concurrent use.
type Segment struct {
	r      io.ReaderAt
	closer io.Closer
	hdr    segmentHeader
	lay    layout
	pq     *PQ
	codes  []byte
	ids    []core.UUID
	seqs   []uint64
	pool   sync.Pool // *beamScratch
	closed atomic.Bool
}

// OpenSegmentFile opens and fully verifies the segment at path. Close
// releases the file.
func OpenSegmentFile(path string) (*Segment, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("vecindex: open segment: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("vecindex: open segment: %w", err)
	}
	s, err := OpenSegment(f, st.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	s.closer = f
	return s, nil
}

// OpenSegment opens the segment of the given size read through r. It reads
// the whole file once, verifying the header checksum, every section
// checksum and the whole-file checksum, and validates every node record, so
// a damaged file fails here (ErrChecksum or ErrCorrupt) instead of
// producing wrong answers later. Allocation is bounded by size.
func OpenSegment(r io.ReaderAt, size int64) (*Segment, error) {
	if size < SectorSize+trailerSize {
		return nil, fmt.Errorf("%w: %d bytes is smaller than a header and trailer", ErrCorrupt, size)
	}
	hdrBytes := make([]byte, SectorSize)
	if _, err := r.ReadAt(hdrBytes, 0); err != nil {
		return nil, fmt.Errorf("vecindex: read segment header: %w", err)
	}
	h, l, err := decodeHeader(hdrBytes, size)
	if err != nil {
		return nil, err
	}
	s := &Segment{r: r, hdr: h, lay: l}
	if err := s.load(hdrBytes); err != nil {
		return nil, err
	}
	return s, nil
}

// load streams the body, checks every checksum, keeps the in-RAM sections
// and validates the node records.
func (s *Segment) load(hdrBytes []byte) error {
	h, l := &s.hdr, &s.lay
	file := crc32.New(castagnoli)
	var structural error // first structural problem, reported only if checksums pass
	pos := uint64(SectorSize)
	chunk := make([]byte, min(streamChunk, l.fileSize))
	// stream hashes [pos, end) into file (and sec), calling visit per chunk.
	stream := func(end uint64, sec hash.Hash32, visit func(off uint64, b []byte)) error {
		for pos < end {
			n := min(end-pos, uint64(len(chunk)))
			b := chunk[:n]
			if _, err := s.r.ReadAt(b, int64(pos)); err != nil {
				return fmt.Errorf("vecindex: read segment: %w", err)
			}
			file.Write(b)
			if sec != nil {
				sec.Write(b)
			}
			if visit != nil {
				visit(pos, b)
			}
			pos += n
		}
		return nil
	}
	var mem [4][]byte
	for i, sec := range h.sections {
		if err := stream(sec.off, nil, nil); err != nil { // alignment padding
			return err
		}
		sh := crc32.New(castagnoli)
		var visit func(uint64, []byte)
		if i == sectionNodes {
			visit = func(off uint64, b []byte) {
				if structural == nil {
					structural = s.checkNodes(off, b)
				}
			}
		} else {
			mem[i] = make([]byte, 0, sec.len)
			visit = func(_ uint64, b []byte) { mem[i] = append(mem[i], b...) }
		}
		if err := stream(sec.off+sec.len, sh, visit); err != nil {
			return err
		}
		if sh.Sum32() != sec.crc {
			return fmt.Errorf("%w: section %d", ErrChecksum, i)
		}
	}
	var tr [trailerSize]byte
	if _, err := s.r.ReadAt(tr[:], int64(l.trailerOff)); err != nil {
		return fmt.Errorf("vecindex: read segment trailer: %w", err)
	}
	file.Write(hdrBytes)
	if string(tr[:4]) != trailerMagic || binary.LittleEndian.Uint32(tr[4:]) != file.Sum32() {
		return fmt.Errorf("%w: whole file", ErrChecksum)
	}
	if structural != nil {
		return structural
	}
	return s.decodeSections(mem[sectionCodebook], mem[sectionCodes], mem[sectionIDs])
}

// checkNodes validates the records that lie entirely inside chunk b, which
// starts at file offset off. Chunks are sector multiples and records never
// straddle sectors except when one record spans several sectors, so a
// record is checked in the chunk that holds its degree field.
func (s *Segment) checkNodes(off uint64, b []byte) error {
	l := &s.lay
	degOff := 4 * uint64(s.hdr.dim)
	end := off + uint64(len(b))
	// First node whose record may start in this chunk.
	var first uint64
	rel := off - l.sections[sectionNodes].off
	if l.perSector > 0 {
		first = rel / SectorSize * l.perSector
	} else {
		first = rel / (l.sectorsPerRecord * SectorSize)
	}
	for i := first; i < s.hdr.count; i++ {
		ro := l.recordOffset(i)
		if ro+degOff+4 > end {
			break
		}
		if ro+degOff < off {
			continue
		}
		p := b[ro+degOff-off:]
		deg := binary.LittleEndian.Uint32(p)
		if deg > s.hdr.r {
			return fmt.Errorf("%w: node %d degree %d > R=%d", ErrCorrupt, i, deg, s.hdr.r)
		}
		for j := range uint64(deg) {
			if ro+degOff+4+4*j+4 > end {
				break // the rest of a multi-sector record is in the next chunk; search re-validates
			}
			if nb := binary.LittleEndian.Uint32(p[4+4*j:]); uint64(nb) >= s.hdr.count {
				return fmt.Errorf("%w: node %d neighbour %d out of range", ErrCorrupt, i, nb)
			}
		}
	}
	return nil
}

// decodeSections turns the verified in-RAM sections into typed data.
func (s *Segment) decodeSections(cb, codes, idb []byte) error {
	h := &s.hdr
	dim, m, k := int(h.dim), int(h.pqM), int(h.pqK)
	pq := &PQ{dim: dim, m: m, k: k, offs: subspaceOffsets(dim, m), codebooks: make([]float32, k*dim)}
	for i := range pq.codebooks {
		x := math.Float32frombits(binary.LittleEndian.Uint32(cb[4*i:]))
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return fmt.Errorf("%w: non-finite codebook entry", ErrCorrupt)
		}
		pq.codebooks[i] = x
	}
	for _, c := range codes {
		if int(c) >= k {
			return fmt.Errorf("%w: pq code %d >= k=%d", ErrCorrupt, c, k)
		}
	}
	n := int(h.count)
	s.ids = make([]core.UUID, n)
	s.seqs = make([]uint64, n)
	for i := range n {
		e := idb[i*idEntrySize:]
		copy(s.ids[i][:], e[:16])
		s.seqs[i] = binary.LittleEndian.Uint64(e[16:])
	}
	s.pq, s.codes = pq, codes
	return nil
}

// Close releases the underlying file when the segment was opened with
// OpenSegmentFile. Searches after Close fail with ErrClosed.
func (s *Segment) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}

// Len returns the number of vectors in the segment.
func (s *Segment) Len() int { return int(s.hdr.count) }

// Dim returns the vector dimension.
func (s *Segment) Dim() int { return int(s.hdr.dim) }

// Metric returns the segment's metric.
func (s *Segment) Metric() Metric { return s.hdr.metric }

// R returns the maximum out-degree.
func (s *Segment) R() int { return int(s.hdr.r) }

// Medoid returns the entry node.
func (s *Segment) Medoid() uint32 { return s.hdr.medoid }

// TombstoneEpoch returns the index generation recorded at build time.
func (s *Segment) TombstoneEpoch() uint64 { return s.hdr.tombstoneEpoch }

// MaxSeq returns the newest source seq among the segment's vectors.
func (s *Segment) MaxSeq() uint64 { return s.hdr.maxSeq }

// PQ returns the segment's quantiser.
func (s *Segment) PQ() *PQ { return s.pq }

// Entry returns the id and source seq of node i.
func (s *Segment) Entry(i uint32) (core.UUID, uint64, bool) {
	if int(i) >= len(s.ids) {
		return core.Nil, 0, false
	}
	return s.ids[i], s.seqs[i], true
}

// DiskBytes returns the file size.
func (s *Segment) DiskBytes() uint64 { return s.lay.fileSize }

// RAMBytes returns the resident footprint: PQ codes, codebooks and the id
// table (the full vectors stay on disk).
func (s *Segment) RAMBytes() uint64 {
	return uint64(len(s.codes)) + s.pq.bytes() + uint64(len(s.ids))*idEntrySize
}

// node is one decoded record.
type node struct {
	vec  []float32
	nbrs []uint32
}

// readNode reads and decodes node i into buf, vec and nbrs (scratch with
// capacities recordSize, dim and R).
func (s *Segment) readNode(i uint32, buf []byte, vec []float32, nbrs []uint32) (node, error) {
	if s.closed.Load() {
		return node{}, ErrClosed
	}
	buf = buf[:s.lay.recordSize]
	if _, err := s.r.ReadAt(buf, int64(s.lay.recordOffset(uint64(i)))); err != nil {
		return node{}, fmt.Errorf("vecindex: read node %d: %w", i, err)
	}
	le := binary.LittleEndian
	dim := int(s.hdr.dim)
	vec = vec[:dim]
	for j := range vec {
		vec[j] = math.Float32frombits(le.Uint32(buf[4*j:]))
	}
	o := 4 * dim
	deg := le.Uint32(buf[o:])
	if deg > s.hdr.r {
		return node{}, fmt.Errorf("%w: node %d degree %d", ErrCorrupt, i, deg)
	}
	nbrs = nbrs[:deg]
	for j := range nbrs {
		nb := le.Uint32(buf[o+4+4*j:])
		if uint64(nb) >= s.hdr.count {
			return node{}, fmt.Errorf("%w: node %d neighbour %d", ErrCorrupt, i, nb)
		}
		nbrs[j] = nb
	}
	return node{vec: vec, nbrs: nbrs}, nil
}

// Vector returns a copy of node i's full-precision vector, read from disk.
func (s *Segment) Vector(i uint32) ([]float32, error) {
	if uint64(i) >= s.hdr.count {
		return nil, fmt.Errorf("%w: node %d of %d", ErrInvalidParams, i, s.hdr.count)
	}
	nd, err := s.readNode(i, make([]byte, s.lay.recordSize), make([]float32, s.hdr.dim), make([]uint32, s.hdr.r))
	if err != nil {
		return nil, err
	}
	return nd.vec, nil
}

// Neighbors returns node i's adjacency list, read from disk.
func (s *Segment) Neighbors(i uint32) ([]uint32, error) {
	if uint64(i) >= s.hdr.count {
		return nil, fmt.Errorf("%w: node %d of %d", ErrInvalidParams, i, s.hdr.count)
	}
	nd, err := s.readNode(i, make([]byte, s.lay.recordSize), make([]float32, s.hdr.dim), make([]uint32, s.hdr.r))
	if err != nil {
		return nil, err
	}
	return nd.nbrs, nil
}

// forEach streams every node's vector in node order (used by merge).
func (s *Segment) forEach(fn func(i uint32, vec []float32) error) error {
	buf := make([]byte, s.lay.recordSize)
	vec := make([]float32, s.hdr.dim)
	nbrs := make([]uint32, s.hdr.r)
	for i := range uint32(s.hdr.count) {
		nd, err := s.readNode(i, buf, vec, nbrs)
		if err != nil {
			return err
		}
		if err := fn(i, nd.vec); err != nil {
			return err
		}
	}
	return nil
}

// SegmentHit is one segment search result.
type SegmentHit struct {
	ID       core.UUID
	Node     uint32
	Distance float32
}

// SearchStats reports the work of one beam search.
type SearchStats struct {
	// NodeReads is the number of node records read from the file (the
	// "SSD reads per query" of b_vec_03).
	NodeReads int
	// Iterations is the number of beam rounds (I/O round trips).
	Iterations int
}

// beamScratch is per-search state, pooled per segment.
type beamScratch struct {
	seen  *searchScratch
	table DistanceTable
	buf   []byte
	vec   []float32
	nbrs  []uint32
	beam  []uint32
	res   []nodeHit
}

// nodeHit is an exactly ranked node of a beam search.
type nodeHit struct {
	hit  Hit
	node uint32
}

func (s *Segment) scratch() *beamScratch {
	if v, ok := s.pool.Get().(*beamScratch); ok {
		return v
	}
	return &beamScratch{
		seen:  newSearchScratch(int(s.hdr.count)),
		table: DistanceTable{lut: make([]float32, s.pq.m*s.pq.k)},
		buf:   make([]byte, s.lay.recordSize),
		vec:   make([]float32, s.hdr.dim),
		nbrs:  make([]uint32, s.hdr.r),
	}
}

// Search runs the beam search of spec 22.5 and returns up to k nodes by
// exact distance. L is the search list size (>= k) and W the beam width.
// Under Cosine q is normalised first.
func (s *Segment) Search(q []float32, k, L, W int) ([]SegmentHit, SearchStats, error) {
	pq, err := prepareVector(s.hdr.metric, int(s.hdr.dim), q)
	if err != nil {
		return nil, SearchStats{}, err
	}
	if k < 1 || L < k || W < 1 {
		return nil, SearchStats{}, fmt.Errorf("%w: k=%d L=%d W=%d", ErrInvalidParams, k, L, W)
	}
	return s.search(pq, k, L, W)
}

// search is Search over a prepared query.
func (s *Segment) search(q []float32, k, L, W int) ([]SegmentHit, SearchStats, error) {
	var st SearchStats
	if s.closed.Load() {
		return nil, st, ErrClosed
	}
	sc := s.scratch()
	defer s.pool.Put(sc)
	s.pq.fillTable(q, s.hdr.metric, &sc.table)
	m := s.pq.m
	approx := func(i uint32) float32 {
		o := int(i) * m
		return sc.table.Distance(s.codes[o : o+m])
	}
	seen := sc.seen
	seen.begin()
	seen.mark(s.hdr.medoid)
	seen.list = append(seen.list, cand{id: s.hdr.medoid, dist: approx(s.hdr.medoid)})
	sc.res = sc.res[:0]
	for {
		// B = up to W closest unexpanded entries of the list.
		sc.beam = sc.beam[:0]
		for i := range seen.list {
			if len(sc.beam) == W {
				break
			}
			if !seen.list[i].expanded {
				seen.list[i].expanded = true
				sc.beam = append(sc.beam, seen.list[i].id)
			}
		}
		if len(sc.beam) == 0 {
			break
		}
		st.Iterations++
		for _, p := range sc.beam {
			nd, err := s.readNode(p, sc.buf, sc.vec, sc.nbrs)
			if err != nil {
				return nil, st, err
			}
			st.NodeReads++
			// Exact distance: the full vector arrived with the node sector.
			d := sanitize(Distance(s.hdr.metric, q, nd.vec))
			sc.res = append(sc.res, nodeHit{hit: Hit{ID: s.ids[p], Distance: d}, node: p})
			for _, n := range nd.nbrs {
				if seen.mark(n) {
					seen.list, _ = insertBounded(seen.list, cand{id: n, dist: approx(n)}, L)
				}
			}
		}
	}
	slices.SortFunc(sc.res, func(a, b nodeHit) int { return compareHit(a.hit, b.hit) })
	out := make([]SegmentHit, 0, min(k, len(sc.res)))
	for _, h := range sc.res[:min(k, len(sc.res))] {
		out = append(out, SegmentHit{ID: h.hit.ID, Node: h.node, Distance: h.hit.Distance})
	}
	return out, st, nil
}
