package chunkstore

import (
	"crypto/sha256"
	"encoding/binary"
)

// Packfile "FFPK" layout (FORMAT.md §3).
const (
	PackMagic        = "FFPK"
	PackVersion      = 1
	PackHeaderSize   = 16 // magic[4] version u16 flags u16 created_unix_ns i64
	PackFooterSize   = 8  // record_count u32 crc32c u32
	RecordHeaderSize = 12 // raw_len u32 comp_len u32 crc32c(frame) u32
)

// RecordHeader is the fixed prefix of a pack record.
type RecordHeader struct {
	RawLen  uint32
	CompLen uint32
	CRC     uint32 // CRC32C of the zstd frame
}

func parseRecordHeader(b []byte) RecordHeader {
	return RecordHeader{
		RawLen:  binary.LittleEndian.Uint32(b[0:]),
		CompLen: binary.LittleEndian.Uint32(b[4:]),
		CRC:     binary.LittleEndian.Uint32(b[8:]),
	}
}

// PackWriter builds a packfile in memory. Records are appended in call
// order; Finish adds the footer.
type PackWriter struct {
	buf   []byte
	count uint32
}

// NewPackWriter starts a pack whose header carries createdUnixNs.
func NewPackWriter(createdUnixNs int64) *PackWriter {
	b := make([]byte, PackHeaderSize, 1<<16)
	copy(b, PackMagic)
	binary.LittleEndian.PutUint16(b[4:], PackVersion)
	binary.LittleEndian.PutUint16(b[6:], 0)
	binary.LittleEndian.PutUint64(b[8:], uint64(createdUnixNs))
	return &PackWriter{buf: b}
}

// Append adds a record holding frame (a zstd frame of rawLen bytes) and
// returns the record's offset.
func (w *PackWriter) Append(rawLen uint32, frame []byte) uint64 {
	off := uint64(len(w.buf))
	var h [RecordHeaderSize]byte
	binary.LittleEndian.PutUint32(h[0:], rawLen)
	binary.LittleEndian.PutUint32(h[4:], uint32(len(frame)))
	binary.LittleEndian.PutUint32(h[8:], CRC32C(frame))
	w.buf = append(w.buf, h[:]...)
	w.buf = append(w.buf, frame...)
	w.count++
	return off
}

// Size returns the bytes written so far, without the footer.
func (w *PackWriter) Size() int { return len(w.buf) }

// Count returns the number of records.
func (w *PackWriter) Count() int { return int(w.count) }

// record returns the bytes of the record at off (header and frame).
func (w *PackWriter) record(off uint64, compLen uint32) []byte {
	return w.buf[off : off+RecordHeaderSize+uint64(compLen)]
}

// Finish appends the footer and returns the pack bytes, its id and its
// footer CRC. The writer must not be used afterwards.
func (w *PackWriter) Finish() (data []byte, id PackID, crc uint32) {
	w.buf = binary.LittleEndian.AppendUint32(w.buf, w.count)
	crc = CRC32C(w.buf)
	w.buf = binary.LittleEndian.AppendUint32(w.buf, crc)
	return w.buf, sha256.Sum256(w.buf), crc
}

// PackRecord locates one record inside a decoded pack.
type PackRecord struct {
	Offset uint64
	RecordHeader
}

// Pack is a decoded packfile.
type Pack struct {
	Version       uint16
	Flags         uint16
	CreatedUnixNs int64
	Records       []PackRecord
	CRC           uint32 // footer CRC32C
	Size          uint64
}

// Frame returns the zstd frame of r within data (the bytes Pack was
// decoded from).
func (r PackRecord) Frame(data []byte) []byte {
	start := r.Offset + RecordHeaderSize
	return data[start : start+uint64(r.CompLen)]
}

// DecodePack validates data as a packfile: size, magic, footer CRC32C,
// version and flags, then the record walk (every record fits, raw_len in
// [1, MaxRecordRaw], each frame's CRC32C, record count equals the footer).
// Frames are not decompressed.
func DecodePack(data []byte) (*Pack, error) {
	const obj = "pack"
	if len(data) < PackHeaderSize+PackFooterSize {
		return nil, errf(CodePackTruncated, obj, "%d bytes", len(data))
	}
	if string(data[:4]) != PackMagic {
		return nil, errf(CodePackBadMagic, obj, "magic %q", data[:4])
	}
	end := len(data) - 4
	crc := binary.LittleEndian.Uint32(data[end:])
	if CRC32C(data[:end]) != crc {
		return nil, errf(CodePackChecksum, obj, "footer CRC32C mismatch")
	}
	p := &Pack{
		Version:       binary.LittleEndian.Uint16(data[4:]),
		Flags:         binary.LittleEndian.Uint16(data[6:]),
		CreatedUnixNs: int64(binary.LittleEndian.Uint64(data[8:])),
		CRC:           crc,
		Size:          uint64(len(data)),
	}
	if p.Version != PackVersion || p.Flags != 0 {
		return nil, errf(CodePackUnsupported, obj, "version %d flags %#x", p.Version, p.Flags)
	}
	count := binary.LittleEndian.Uint32(data[end-4:])
	body := uint64(end - 4)
	off := uint64(PackHeaderSize)
	for off < body {
		if body-off < RecordHeaderSize {
			return nil, errf(CodePackMalformed, obj, "partial record header at %d", off)
		}
		h := parseRecordHeader(data[off:])
		if h.RawLen == 0 || h.RawLen > MaxRecordRaw || h.CompLen == 0 {
			return nil, errf(CodePackMalformed, obj, "record at %d: raw_len %d comp_len %d", off, h.RawLen, h.CompLen)
		}
		if uint64(h.CompLen) > body-off-RecordHeaderSize {
			return nil, errf(CodePackMalformed, obj, "record at %d overruns the pack", off)
		}
		r := PackRecord{Offset: off, RecordHeader: h}
		if CRC32C(r.Frame(data)) != h.CRC {
			return nil, errf(CodeRecordChecksum, obj, "record at %d", off)
		}
		p.Records = append(p.Records, r)
		off += RecordHeaderSize + uint64(h.CompLen)
	}
	if uint64(len(p.Records)) != uint64(count) {
		return nil, errf(CodePackMalformed, obj, "footer count %d, found %d records", count, len(p.Records))
	}
	return p, nil
}
