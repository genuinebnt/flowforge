package chunkstore

import (
	"encoding/binary"
)

// Manifest "FFMF" layout (FORMAT.md §5). Version 2 adds the chunker
// descriptor; version 1 (no descriptor) is still read.
const (
	ManifestMagic = "FFMF"
	// ManifestVersion is the version writers produce for a manifest
	// that records its chunker.
	ManifestVersion = 2
	// ManifestVersionLegacy is the version-1 layout, written only for a
	// Manifest whose Chunking is unset.
	ManifestVersionLegacy = 1
	// ManifestHeaderSize is the version-2 header: magic[4] version u16
	// flags u16 total_len u64 count u32 algo u8 reserved[3] min u32 avg u32
	// max u32.
	ManifestHeaderSize = 36
	// ManifestLegacyHeaderSize is the version-1 header (no descriptor).
	ManifestLegacyHeaderSize = 20
	// MaxManifestChunks is the largest chunk count whose version-2
	// encoding fits a record (raw_len <= MaxRecordRaw).
	MaxManifestChunks = (MaxRecordRaw - ManifestHeaderSize - 4) / 32
	// maxLegacyManifestChunks is the version-1 bound.
	maxLegacyManifestChunks = (MaxRecordRaw - ManifestLegacyHeaderSize - 4) / 32
)

// Manifest is the recipe of a blob: its total length, its chunk ids in
// order, and the chunker that cut them.
type Manifest struct {
	TotalLen uint64
	Chunks   []ChunkID
	// Chunking is the canonical configuration of the chunker that produced
	// Chunks (Cutter.Config). A zero Algo means "not recorded": the
	// manifest encodes as version 1, as manifests written before version 2
	// did (those were all FastCDC, parameters unrecorded). Store.Put always
	// records it.
	Chunking ChunkerConfig
}

// Encode serializes m: version 2 when m.Chunking.Algo is set (the other
// Chunking fields must then be its canonical form), else version 1. The
// bytes, and so the ManifestID, are a pure function of (TotalLen, Chunks,
// Chunking): the same content cut by two chunkers has two manifests.
func (m *Manifest) Encode() []byte {
	if m.Chunking.Algo == 0 {
		b := make([]byte, 0, ManifestLegacyHeaderSize+32*len(m.Chunks)+4)
		b = append(b, ManifestMagic...)
		b = binary.LittleEndian.AppendUint16(b, ManifestVersionLegacy)
		b = binary.LittleEndian.AppendUint16(b, 0)
		b = binary.LittleEndian.AppendUint64(b, m.TotalLen)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(m.Chunks)))
		for _, id := range m.Chunks {
			b = append(b, id[:]...)
		}
		return binary.LittleEndian.AppendUint32(b, CRC32C(b))
	}
	c := m.Chunking
	b := make([]byte, 0, ManifestHeaderSize+32*len(m.Chunks)+4)
	b = append(b, ManifestMagic...)
	b = binary.LittleEndian.AppendUint16(b, ManifestVersion)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint64(b, m.TotalLen)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(m.Chunks)))
	b = append(b, byte(c.Algo), 0, 0, 0)
	b = binary.LittleEndian.AppendUint32(b, uint32(c.MinSize))
	b = binary.LittleEndian.AppendUint32(b, uint32(c.AvgSize))
	b = binary.LittleEndian.AppendUint32(b, uint32(c.MaxSize))
	for _, id := range m.Chunks {
		b = append(b, id[:]...)
	}
	return binary.LittleEndian.AppendUint32(b, CRC32C(b))
}

// ID returns the manifest's content id.
func (m *Manifest) ID() ManifestID { return ManifestID(SumChunk(m.Encode())) }

// DecodeManifest validates and decodes an encoded manifest of either
// version: size, magic, CRC32C, version and flags; for version 2 a known
// algorithm (else MANIFEST_UNSUPPORTED), zero reserved bytes and
// canonical, valid parameters; exact length for the count; and
// count <= total_len (every chunk is at least one byte) with count == 0
// exactly when total_len == 0.
func DecodeManifest(data []byte) (*Manifest, error) {
	const obj = "manifest"
	if len(data) < ManifestLegacyHeaderSize+4 {
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
	if (version != ManifestVersion && version != ManifestVersionLegacy) || flags != 0 {
		return nil, errf(CodeManifestUnsupported, obj, "version %d flags %#x", version, flags)
	}
	total := binary.LittleEndian.Uint64(data[8:])
	n := uint64(binary.LittleEndian.Uint32(data[16:]))
	header, maxN := uint64(ManifestLegacyHeaderSize), uint64(maxLegacyManifestChunks)
	var cfg ChunkerConfig
	if version == ManifestVersion {
		header, maxN = ManifestHeaderSize, MaxManifestChunks
		if len(data) < ManifestHeaderSize+4 {
			return nil, errf(CodeManifestTruncated, obj, "%d bytes for a version 2 manifest", len(data))
		}
		algo := ChunkAlgo(data[20])
		if algo < ChunkFastCDC || algo > ChunkWhole {
			return nil, errf(CodeManifestUnsupported, obj, "chunker algorithm %d", data[20])
		}
		if data[21] != 0 || data[22] != 0 || data[23] != 0 {
			return nil, errf(CodeManifestMalformed, obj, "reserved bytes are not zero")
		}
		cfg = ChunkerConfig{Algo: algo, MinSize: int(binary.LittleEndian.Uint32(data[24:])),
			AvgSize: int(binary.LittleEndian.Uint32(data[28:])), MaxSize: int(binary.LittleEndian.Uint32(data[32:]))}
		// A recorded configuration must be one Cutter.Config can return,
		// so the manifest re-encodes to its bytes and names a chunker
		// that can re-cut the blob.
		if canon, err := cfg.canonical(); err != nil || canon != cfg {
			return nil, errf(CodeManifestMalformed, obj, "chunker %s %d/%d/%d is not a valid canonical configuration",
				algo, cfg.MinSize, cfg.AvgSize, cfg.MaxSize)
		}
	}
	if n > maxN || uint64(len(data)) != header+32*n+4 {
		return nil, errf(CodeManifestMalformed, obj, "length %d does not match %d chunks", len(data), n)
	}
	if n > total || (n == 0) != (total == 0) || total > n*MaxRecordRaw {
		return nil, errf(CodeManifestMalformed, obj, "%d chunks cannot hold %d bytes", n, total)
	}
	m := &Manifest{TotalLen: total, Chunks: make([]ChunkID, n), Chunking: cfg}
	for i := range m.Chunks {
		copy(m.Chunks[i][:], data[header+32*uint64(i):])
	}
	return m, nil
}
