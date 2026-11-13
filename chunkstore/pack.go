package chunkstore

import (
	"crypto/sha256"
	"encoding/binary"
	"hash/crc32"
)

// Packfile "FFPK" layout (FORMAT.md §3).
const (
	PackMagic        = "FFPK"
	PackVersion      = 1
	PackHeaderSize   = 16 // magic[4] version u16 flags u16 created_unix_ns i64
	PackFooterSize   = 8  // record_count u32 crc32c u32
	RecordHeaderSize = 16 // raw_len u32 stored_len u32 codec u8 reserved[3] crc32c u32
)

// Record codecs (the codec byte of a record header).
const (
	CodecRaw  uint8 = 0 // payload is the chunk itself; stored_len == raw_len
	CodecZstd uint8 = 1 // payload is one zstd frame (RFC 8878)
)

// RecordHeader is the fixed prefix of a pack record. CompLen is the
// payload length (stored_len in FORMAT.md; comp_len in the index).
type RecordHeader struct {
	RawLen  uint32
	CompLen uint32
	Codec   uint8
	// CRC is CRC32C over the first 12 header bytes followed by the
	// payload, so it also protects the lengths and the codec byte.
	CRC uint32
}

func parseRecordHeader(b []byte) (RecordHeader, bool) {
	h := RecordHeader{
		RawLen:  binary.LittleEndian.Uint32(b[0:]),
		CompLen: binary.LittleEndian.Uint32(b[4:]),
		Codec:   b[8],
		CRC:     binary.LittleEndian.Uint32(b[12:]),
	}
	reservedZero := b[9] == 0 && b[10] == 0 && b[11] == 0
	return h, reservedZero
}

// recordCRC computes the record checksum of rec (header and payload).
func recordCRC(rec []byte) uint32 {
	c := crc32.Update(0, castagnoli, rec[:12])
	return crc32.Update(c, castagnoli, rec[RecordHeaderSize:])
}

// validRecordShape checks the header fields that do not need the payload.
func validRecordShape(h RecordHeader) bool {
	switch {
	case h.RawLen == 0 || h.RawLen > MaxRecordRaw || h.CompLen == 0:
		return false
	case h.Codec == CodecRaw:
		return h.CompLen == h.RawLen
	case h.Codec == CodecZstd:
		return h.CompLen <= MaxRecordRaw+1<<16
	}
	return false
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

// Append adds a record whose payload, encoded with codec, decodes to
// rawLen bytes, and returns the record's offset.
func (w *PackWriter) Append(rawLen uint32, codec uint8, payload []byte) uint64 {
	off := uint64(len(w.buf))
	var h [RecordHeaderSize]byte
	binary.LittleEndian.PutUint32(h[0:], rawLen)
	binary.LittleEndian.PutUint32(h[4:], uint32(len(payload)))
	h[8] = codec
	w.buf = append(w.buf, h[:]...)
	w.buf = append(w.buf, payload...)
	rec := w.buf[off:]
	binary.LittleEndian.PutUint32(rec[12:], recordCRC(rec))
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
// footer CRC. The writer is unchanged, so a failed write can be retried
// and more records appended.
func (w *PackWriter) Finish() (data []byte, id PackID, crc uint32) {
	data = make([]byte, len(w.buf), len(w.buf)+PackFooterSize)
	copy(data, w.buf)
	data = binary.LittleEndian.AppendUint32(data, w.count)
	crc = CRC32C(data)
	data = binary.LittleEndian.AppendUint32(data, crc)
	return data, sha256.Sum256(data), crc
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

// Payload returns the payload of r within data (the bytes Pack was
// decoded from).
func (r PackRecord) Payload(data []byte) []byte {
	start := r.Offset + RecordHeaderSize
	return data[start : start+uint64(r.CompLen)]
}

// DecodePack validates data as a packfile: size, magic, footer CRC32C,
// version and flags, then the record walk (every record fits, a known
// codec with zero reserved bytes, raw_len in [1, MaxRecordRaw], raw
// records with stored_len == raw_len, each record's CRC32C, record count
// equal to the footer). Payloads are not decompressed.
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
		h, reservedZero := parseRecordHeader(data[off:])
		if !reservedZero || !validRecordShape(h) {
			return nil, errf(CodePackMalformed, obj, "record at %d: raw_len %d stored_len %d codec %d", off, h.RawLen, h.CompLen, h.Codec)
		}
		if uint64(h.CompLen) > body-off-RecordHeaderSize {
			return nil, errf(CodePackMalformed, obj, "record at %d overruns the pack", off)
		}
		r := PackRecord{Offset: off, RecordHeader: h}
		if recordCRC(data[off:off+RecordHeaderSize+uint64(h.CompLen)]) != h.CRC {
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
