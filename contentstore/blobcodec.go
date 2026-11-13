package contentstore

import (
	"encoding/binary"
	"hash/crc32"

	"github.com/klauspost/compress/zstd"
)

// BlobCodec selects how a snapshot's blobs are encoded before they reach
// the chunk store (a pluggable decision, docs/ALGORITHMS.md
// "contentstore: blob codec"; benchmark BenchmarkCStoreAlgo/codec/<name>).
type BlobCodec uint8

const (
	// CodecDelta (default): each blob is one zstd frame ("FFZB") whose
	// raw dictionary is the same blob of the snapshot's delta base (its
	// parent, or its lineage predecessor) and, for the derived blobs, the
	// snapshot's own body or normalized text. Versions with small edits
	// cost little more than the edit, and small artifacts compress as a
	// whole instead of per chunk. Chains are bounded by keyframes every
	// Config.KeyframeInterval versions.
	CodecDelta BlobCodec = iota
	// CodecChunk: blobs are stored as they are; the chunk store's FastCDC
	// dedupe and per-chunk zstd (level 3) do all the work. No chains; best
	// when versions share long unchanged runs (large bundles).
	CodecChunk
)

// String returns "delta" or "chunk".
func (c BlobCodec) String() string {
	if c == CodecChunk {
		return "chunk"
	}
	return "delta"
}

// ParseBlobCodec is the inverse of String.
func ParseBlobCodec(s string) (BlobCodec, error) {
	switch s {
	case "", "delta":
		return CodecDelta, nil
	case "chunk":
		return CodecChunk, nil
	}
	return 0, invalid("unknown blob codec %q (delta, chunk)", s)
}

// FFZB blob format (little-endian):
//
//	offset size field
//	0      4    magic "FFZB"
//	4      1    version (1)
//	5      1    flags: bit 0 = a raw dictionary was used
//	6      2    reserved, zero
//	8      4    raw length
//	12     4    CRC32C (Castagnoli) of the raw bytes
//	16     ...  one zstd frame (RFC 8878), no frame checksum, dictionary
//	            id 1 when flag bit 0 is set
//
// Encoder settings are pinned so output is reproducible: one encoder
// goroutine, single-segment frames, level SpeedBestCompression (zstd 19)
// for raw blobs up to 1 MiB and SpeedBetterCompression (zstd 7) above,
// window blobWindow(len(dictionary) + len(raw)) (dictionary references
// span the base version).
//
// PORT: the dictionary is raw content (zstd "prefix" / raw dictionary,
// ZSTD_dct_rawContent in the C library); the Rust zstd crate's
// with_dictionary takes it as raw content because it lacks the dictionary
// magic.
const (
	blobMagic       = "FFZB"
	blobVersion     = 1
	blobHeaderSize  = 16
	blobDictFlag    = 1
	blobBestLimit   = 1 << 20
	blobWindowLog   = 27
	maxBlobRaw      = 256 << 20
	blobRawDictID   = 1
	blobDictMaxSize = 128 << 20
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// encodeBlob encodes raw with the delta codec under dict.
func encodeBlob(raw, dict []byte) ([]byte, error) {
	if len(raw) > maxBlobRaw {
		return nil, invalid("blob of %d bytes exceeds %d", len(raw), maxBlobRaw)
	}
	if len(dict) > blobDictMaxSize {
		dict = dict[len(dict)-blobDictMaxSize:]
	}
	level := zstd.SpeedBestCompression
	if len(raw) > blobBestLimit {
		level = zstd.SpeedBetterCompression
	}
	opts := []zstd.EOption{zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(false),
		zstd.WithSingleSegment(true), zstd.WithZeroFrames(true), zstd.WithWindowSize(blobWindow(len(dict) + len(raw)))}
	var flags byte
	if len(dict) > 0 {
		opts = append(opts, zstd.WithEncoderDictRaw(blobRawDictID, dict))
		flags |= blobDictFlag
	}
	enc, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		return nil, internal("zstd encoder", err)
	}
	defer enc.Close()
	out := make([]byte, blobHeaderSize, blobHeaderSize+len(raw)/3+64)
	copy(out, blobMagic)
	out[4], out[5] = blobVersion, flags
	binary.LittleEndian.PutUint32(out[8:], uint32(len(raw)))
	binary.LittleEndian.PutUint32(out[12:], crc32.Checksum(raw, castagnoli))
	return enc.EncodeAll(raw, out), nil
}

// blobWindow is the encoder window for n bytes of dictionary plus input:
// the next power of two at or above n, within [2^17, 2^27], so references
// reach the whole base version without allocating the maximum window for
// small blobs.
func blobWindow(n int) int {
	w := 1 << 17
	for w < n && w < 1<<blobWindowLog {
		w <<= 1
	}
	return w
}

// isBlob reports a delta-codec blob.
func isBlob(b []byte) bool { return len(b) >= blobHeaderSize && string(b[:4]) == blobMagic }

// decodeBlob reverses encodeBlob; dict must be the one used to encode.
func decodeBlob(b, dict []byte) ([]byte, error) {
	if !isBlob(b) || b[4] != blobVersion || b[6] != 0 || b[7] != 0 {
		return nil, integrity("blob header")
	}
	n := binary.LittleEndian.Uint32(b[8:])
	if n > maxBlobRaw {
		return nil, integrity("blob length %d", n)
	}
	if len(dict) > blobDictMaxSize {
		dict = dict[len(dict)-blobDictMaxSize:]
	}
	opts := []zstd.DOption{zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(maxBlobRaw + blobDictMaxSize),
		zstd.WithDecoderMaxWindow(1 << blobWindowLog), zstd.WithDecodeAllCapLimit(true)}
	if b[5]&blobDictFlag != 0 {
		if len(dict) == 0 {
			return nil, integrity("blob needs its dictionary")
		}
		opts = append(opts, zstd.WithDecoderDictRaw(blobRawDictID, dict))
	}
	dec, err := zstd.NewReader(nil, opts...)
	if err != nil {
		return nil, internal("zstd decoder", err)
	}
	defer dec.Close()
	raw, err := dec.DecodeAll(b[blobHeaderSize:], make([]byte, 0, n))
	if err != nil {
		return nil, integrity("blob frame: %v", err)
	}
	if uint32(len(raw)) != n || crc32.Checksum(raw, castagnoli) != binary.LittleEndian.Uint32(b[12:]) {
		return nil, integrity("blob checksum")
	}
	return raw, nil
}
