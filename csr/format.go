package csr

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"hash/crc32"
	"io"
	"math"
)

// File layout constants for the "FFCS" snapshot format (spec 21.1).
//
//	header:  magic "FFCS" | version u16 | flags u16 | V u64 | E u64 |
//	         tenant_id 16B | program_id 16B | watermark_seq u64 |
//	         created_at i64 (Unix milliseconds)
//	table:   n u32, then n * {kind u16, offset u64, length u64, crc32c u32}
//	sections (each starting at a multiple of 64, zero padded)
//	footer:  sha256 of every byte before it (the footer itself is 64-byte
//	         aligned, so it is always the last 32 bytes of the file)
//
// All integers are little endian. Offsets in the table are absolute file
// offsets; lengths exclude padding.
//
// PORT: this is a cross-language contract: csr_009 compares the SHA-256
// of Go- and Rust-built files (docs/PORTING_RUST.md 4.4). created_at is
// time.UnixMilli (floor to milliseconds), float32 confidences are their
// IEEE-754 bits, and CRC32C is crc32.Castagnoli (the crc32c crate).
const (
	// FormatVersion is the only snapshot version this package reads and
	// writes.
	FormatVersion = 1

	magic          = "FFCS"
	headerFixed    = 72 // bytes before the section count
	tableEntrySize = 22 // kind u16 + offset u64 + length u64 + crc32c u32
	sectionAlign   = 64
	footerSize     = sha256.Size
	maxSections    = 64
)

// Section kinds.
const (
	secOffsets   uint16 = 1
	secTargets   uint16 = 2
	secEdgeLabel uint16 = 3
	secEdgeConf  uint16 = 4
	secNodeType  uint16 = 5
	secNodeID    uint16 = 6
	secLabelDict uint16 = 7
	secInOffsets uint16 = 8
	secInSources uint16 = 9
	secInEdge    uint16 = 10
	numSections         = 10
)

func castagnoli() *crc32.Table { return crc32.MakeTable(crc32.Castagnoli) }

func alignUp(n uint64) uint64 { return (n + sectionAlign - 1) &^ (sectionAlign - 1) }

func headerSize(n int) uint64 { return headerFixed + 4 + uint64(n)*tableEntrySize }

// section is one array of the file together with a chunked serializer, so
// the encoder never materialises the whole file in memory.
type section struct {
	kind   uint16
	length uint64
	emit   func(scratch []byte, fn func([]byte) error) error
	offset uint64
	crc    uint32
}

const chunkBytes = 64 << 10

func u64Section(kind uint16, xs []uint64) section {
	return section{kind: kind, length: 8 * uint64(len(xs)), emit: func(buf []byte, fn func([]byte) error) error {
		xs := xs // each pass consumes its own copy of the slice header
		for len(xs) > 0 {
			n := min(len(xs), len(buf)/8)
			for i, x := range xs[:n] {
				binary.LittleEndian.PutUint64(buf[8*i:], x)
			}
			if err := fn(buf[:8*n]); err != nil {
				return err
			}
			xs = xs[n:]
		}
		return nil
	}}
}

func u32Section(kind uint16, xs []uint32) section {
	return section{kind: kind, length: 4 * uint64(len(xs)), emit: func(buf []byte, fn func([]byte) error) error {
		xs := xs // each pass consumes its own copy of the slice header
		for len(xs) > 0 {
			n := min(len(xs), len(buf)/4)
			for i, x := range xs[:n] {
				binary.LittleEndian.PutUint32(buf[4*i:], x)
			}
			if err := fn(buf[:4*n]); err != nil {
				return err
			}
			xs = xs[n:]
		}
		return nil
	}}
}

func confSection(xs []float32, asByte bool) section {
	if asByte {
		return section{kind: secEdgeConf, length: uint64(len(xs)), emit: func(buf []byte, fn func([]byte) error) error {
			xs := xs // each pass consumes its own copy of the slice header
			for len(xs) > 0 {
				n := min(len(xs), len(buf))
				for i, x := range xs[:n] {
					buf[i] = quantByte(x)
				}
				if err := fn(buf[:n]); err != nil {
					return err
				}
				xs = xs[n:]
			}
			return nil
		}}
	}
	return section{kind: secEdgeConf, length: 4 * uint64(len(xs)), emit: func(buf []byte, fn func([]byte) error) error {
		xs := xs // each pass consumes its own copy of the slice header
		for len(xs) > 0 {
			n := min(len(xs), len(buf)/4)
			for i, x := range xs[:n] {
				binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(x))
			}
			if err := fn(buf[:4*n]); err != nil {
				return err
			}
			xs = xs[n:]
		}
		return nil
	}}
}

func rawSection(kind uint16, b []byte) section {
	return section{kind: kind, length: uint64(len(b)), emit: func(_ []byte, fn func([]byte) error) error {
		if len(b) == 0 {
			return nil
		}
		return fn(b)
	}}
}

// encodeLabelDict renders the label dictionary section: count u16, then
// for each label a u16 byte length and the UTF-8 bytes.
func encodeLabelDict(labels []string) []byte {
	n := 2
	for _, l := range labels {
		n += 2 + len(l)
	}
	b := make([]byte, 0, n)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(labels)))
	for _, l := range labels {
		b = binary.LittleEndian.AppendUint16(b, uint16(len(l)))
		b = append(b, l...)
	}
	return b
}

func (g *Graph) sections() []section {
	ids := make([]byte, 16*len(g.nodeID))
	for i, id := range g.nodeID {
		copy(ids[16*i:], id[:])
	}
	return []section{
		u64Section(secOffsets, g.offsets),
		u32Section(secTargets, g.targets),
		rawSection(secEdgeLabel, g.edgeLabel),
		confSection(g.edgeConf, g.meta.Flags&FlagConfU8 != 0),
		rawSection(secNodeType, g.nodeType),
		rawSection(secNodeID, ids),
		rawSection(secLabelDict, encodeLabelDict(g.labels)),
		u64Section(secInOffsets, g.inOffsets),
		u32Section(secInSources, g.inSources),
		u32Section(secInEdge, g.inEdge),
	}
}

// layout assigns offsets and checksums and returns the footer offset.
func layout(secs []section, scratch []byte) uint64 {
	tab := castagnoli()
	cur := alignUp(headerSize(len(secs)))
	for i := range secs {
		s := &secs[i]
		s.offset = cur
		var crc uint32
		_ = s.emit(scratch, func(p []byte) error { crc = crc32.Update(crc, tab, p); return nil })
		s.crc = crc
		cur = alignUp(cur + s.length)
	}
	return cur
}

// EncodedSize returns the size in bytes of the snapshot file.
func (g *Graph) EncodedSize() int64 {
	secs := g.sections()
	cur := alignUp(headerSize(len(secs)))
	for _, s := range secs {
		cur = alignUp(cur + s.length)
	}
	return int64(cur + footerSize)
}

// WriteTo writes the snapshot file to w. The output depends only on the
// graph content and Meta, never on how the graph was built.
func (g *Graph) WriteTo(w io.Writer) (int64, error) {
	scratch := make([]byte, chunkBytes)
	secs := g.sections()
	footerOff := layout(secs, scratch)

	hw := &hashingWriter{w: w, h: sha256.New()}
	hdr := make([]byte, 0, alignUp(headerSize(len(secs))))
	hdr = append(hdr, magic...)
	hdr = binary.LittleEndian.AppendUint16(hdr, FormatVersion)
	hdr = binary.LittleEndian.AppendUint16(hdr, g.meta.Flags)
	hdr = binary.LittleEndian.AppendUint64(hdr, uint64(len(g.nodeID)))
	hdr = binary.LittleEndian.AppendUint64(hdr, uint64(len(g.targets)))
	hdr = append(hdr, g.meta.TenantID[:]...)
	hdr = append(hdr, g.meta.ProgramID[:]...)
	hdr = binary.LittleEndian.AppendUint64(hdr, g.meta.WatermarkSeq)
	hdr = binary.LittleEndian.AppendUint64(hdr, uint64(g.meta.CreatedAt.UnixMilli()))
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(len(secs)))
	for _, s := range secs {
		hdr = binary.LittleEndian.AppendUint16(hdr, s.kind)
		hdr = binary.LittleEndian.AppendUint64(hdr, s.offset)
		hdr = binary.LittleEndian.AppendUint64(hdr, s.length)
		hdr = binary.LittleEndian.AppendUint32(hdr, s.crc)
	}
	if err := hw.write(hdr); err != nil {
		return hw.n, err
	}
	for _, s := range secs {
		if err := hw.pad(s.offset); err != nil {
			return hw.n, err
		}
		if err := s.emit(scratch, hw.write); err != nil {
			return hw.n, err
		}
	}
	if err := hw.pad(footerOff); err != nil {
		return hw.n, err
	}
	sum := hw.h.Sum(nil)
	n, err := w.Write(sum)
	hw.n += int64(n)
	return hw.n, err
}

// Encode returns the snapshot file as a byte slice.
func (g *Graph) Encode() ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(int(g.EncodedSize()))
	if _, err := g.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Digest returns the sha256 footer of the snapshot file (spec 21.1). It
// identifies the whole file: the graph content and Meta (tenant, program,
// watermark, created_at, flags), so equal graphs with different metadata
// have different digests.
func (g *Graph) Digest() ([sha256.Size]byte, error) {
	var d [sha256.Size]byte
	var tail tailWriter
	if _, err := g.WriteTo(&tail); err != nil {
		return d, err
	}
	copy(d[:], tail.last[:])
	return d, nil
}

type hashingWriter struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (hw *hashingWriter) write(p []byte) error {
	hw.h.Write(p)
	n, err := hw.w.Write(p)
	hw.n += int64(n)
	return err
}

func (hw *hashingWriter) pad(to uint64) error {
	var zero [sectionAlign]byte
	for uint64(hw.n) < to {
		k := min(to-uint64(hw.n), sectionAlign)
		if err := hw.write(zero[:k]); err != nil {
			return err
		}
	}
	return nil
}

// tailWriter discards output but remembers its last 32 bytes.
type tailWriter struct {
	last [sha256.Size]byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	if len(p) >= len(t.last) {
		copy(t.last[:], p[len(p)-len(t.last):])
	} else {
		copy(t.last[:], t.last[len(p):])
		copy(t.last[len(t.last)-len(p):], p)
	}
	return len(p), nil
}
