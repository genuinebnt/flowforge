package chunkstore

import (
	"encoding/binary"
)

// Manifest "FFMF" layout (FORMAT.md §5).
const (
	ManifestMagic      = "FFMF"
	ManifestVersion    = 1
	ManifestHeaderSize = 20 // magic[4] version u16 flags u16 total_len u64 count u32
	// MaxManifestChunks is the largest chunk count whose encoding fits a
	// record (raw_len <= MaxRecordRaw).
	MaxManifestChunks = (MaxRecordRaw - ManifestHeaderSize - 4) / 32
)

// Manifest is the recipe of a blob: its total length and its chunk ids
// in order.
type Manifest struct {
	TotalLen uint64
	Chunks   []ChunkID
}

// Encode serializes m. The bytes, and so the ManifestID, are a pure
// function of (TotalLen, Chunks).
func (m *Manifest) Encode() []byte {
	b := make([]byte, 0, ManifestHeaderSize+32*len(m.Chunks)+4)
	b = append(b, ManifestMagic...)
	b = binary.LittleEndian.AppendUint16(b, ManifestVersion)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint64(b, m.TotalLen)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(m.Chunks)))
	for _, id := range m.Chunks {
		b = append(b, id[:]...)
	}
	return binary.LittleEndian.AppendUint32(b, CRC32C(b))
}

// ID returns the manifest's content id.
func (m *Manifest) ID() ManifestID { return ManifestID(SumChunk(m.Encode())) }

// DecodeManifest validates and decodes an encoded manifest: size, magic,
// CRC32C, version and flags, exact length for the count, and
// count <= total_len (every chunk is at least one byte) with
// count == 0 exactly when total_len == 0.
func DecodeManifest(data []byte) (*Manifest, error) {
	const obj = "manifest"
	if len(data) < ManifestHeaderSize+4 {
		return nil, errf(CodeManifestTruncated, obj, "%d bytes", len(data))
	}
	if string(data[:4]) != ManifestMagic {
		return nil, errf(CodeManifestBadMagic, obj, "magic %q", data[:4])
	}
	end := len(data) - 4
	if CRC32C(data[:end]) != binary.LittleEndian.Uint32(data[end:]) {
		return nil, errf(CodeManifestChecksum, obj, "CRC32C mismatch")
	}
	version := binary.LittleEndian.Uint16(data[4:])
	flags := binary.LittleEndian.Uint16(data[6:])
	if version != ManifestVersion || flags != 0 {
		return nil, errf(CodeManifestUnsupported, obj, "version %d flags %#x", version, flags)
	}
	total := binary.LittleEndian.Uint64(data[8:])
	n := uint64(binary.LittleEndian.Uint32(data[16:]))
	if n > MaxManifestChunks || uint64(len(data)) != ManifestHeaderSize+32*n+4 {
		return nil, errf(CodeManifestMalformed, obj, "length %d does not match %d chunks", len(data), n)
	}
	if n > total || (n == 0) != (total == 0) || total > n*MaxRecordRaw {
		return nil, errf(CodeManifestMalformed, obj, "%d chunks cannot hold %d bytes", n, total)
	}
	m := &Manifest{TotalLen: total, Chunks: make([]ChunkID, n)}
	for i := range m.Chunks {
		copy(m.Chunks[i][:], data[ManifestHeaderSize+32*i:])
	}
	return m, nil
}
