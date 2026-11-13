package chunkstore

import (
	"encoding/binary"
	"hash/crc32"

	"github.com/klauspost/compress/zstd"
)

// Delta frames ("FFZB"): one zstd frame compressed against a raw
// dictionary (typically the previous version of the same blob), so a new
// version costs about its edit. Callers store frames as ordinary blobs
// (they are incompressible, so Put keeps them raw); contentstore uses them
// for its delta codec. Format (little-endian):
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
// goroutine, single-segment frames, level SpeedBetterCompression,
// window deltaWindow(len(dictionary) + len(raw)) (dictionary references
// span the base version).
//
// PORT: the dictionary is raw content (zstd "prefix" / raw dictionary,
// ZSTD_dct_rawContent in the C library); the Rust zstd crate's
// with_dictionary takes it as raw content because it lacks the dictionary
// magic.
const (
	deltaMagic     = "FFZB"
	deltaVersion   = 1
	deltaDictFlag  = 1
	deltaWindowLog = 27
	deltaRawDictID = 1
)

// Delta frame bounds.
const (
	// DeltaHeaderSize is the fixed FFZB header length.
	DeltaHeaderSize = 16
	// MaxDeltaRaw bounds the raw bytes of one frame.
	MaxDeltaRaw = 256 << 20
	// MaxDeltaDict bounds the dictionary; a longer one is cut to its last
	// MaxDeltaDict bytes (the same cut on encode and decode).
	MaxDeltaDict = 128 << 20
)

// EncodeDelta encodes raw as one FFZB frame whose zstd raw dictionary is
// dict (nil: none). The output is a pure function of (raw, dict).
func EncodeDelta(raw, dict []byte) ([]byte, error) {
	if len(raw) > MaxDeltaRaw {
		return nil, errf(CodeTooLarge, "delta", "%d bytes exceed %d", len(raw), MaxDeltaRaw)
	}
	if len(dict) > MaxDeltaDict {
		dict = dict[len(dict)-MaxDeltaDict:]
	}
	// SpeedBetterCompression (zstd level 7 in klauspost's mapping): within
	// 2 % of SpeedBestCompression on the delta benchmarks at a quarter of
	// the time and memory (contentstore BenchmarkCStoreAlgo).
	level := zstd.SpeedBetterCompression
	opts := []zstd.EOption{zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(false),
		zstd.WithSingleSegment(true), zstd.WithZeroFrames(true), zstd.WithWindowSize(deltaWindow(len(dict) + len(raw)))}
	var flags byte
	if len(dict) > 0 {
		opts = append(opts, zstd.WithEncoderDictRaw(deltaRawDictID, dict))
		flags |= deltaDictFlag
	}
	enc, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		return nil, wrapErr(CodeInvalidConfig, "delta", err)
	}
	defer enc.Close()
	out := make([]byte, DeltaHeaderSize, DeltaHeaderSize+len(raw)/3+64)
	copy(out, deltaMagic)
	out[4], out[5] = deltaVersion, flags
	binary.LittleEndian.PutUint32(out[8:], uint32(len(raw)))
	binary.LittleEndian.PutUint32(out[12:], crc32.Checksum(raw, castagnoli))
	return enc.EncodeAll(raw, out), nil
}

// deltaWindow is the encoder window for n bytes of dictionary plus input:
// the next power of two at or above n, within [2^17, 2^27], so references
// reach the whole base version without allocating the maximum window for
// small blobs.
func deltaWindow(n int) int {
	w := 1 << 17
	for w < n && w < 1<<deltaWindowLog {
		w <<= 1
	}
	return w
}

// IsDelta reports bytes that start like an FFZB frame.
func IsDelta(b []byte) bool { return len(b) >= DeltaHeaderSize && string(b[:4]) == deltaMagic }

// DecodeDelta reverses EncodeDelta; dict must be the one it was encoded
// with. Errors: CodeRecordMalformed (header), CodeChunkDecompress (the
// frame, or a missing dictionary), CodeChunkLengthMismatch and
// CodeRecordChecksum (the raw length and CRC32C).
func DecodeDelta(b, dict []byte) ([]byte, error) {
	if !IsDelta(b) || b[4] != deltaVersion || b[6] != 0 || b[7] != 0 {
		return nil, errf(CodeRecordMalformed, "delta", "bad header")
	}
	n := binary.LittleEndian.Uint32(b[8:])
	if n > MaxDeltaRaw {
		return nil, errf(CodeRecordMalformed, "delta", "raw length %d", n)
	}
	if len(dict) > MaxDeltaDict {
		dict = dict[len(dict)-MaxDeltaDict:]
	}
	opts := []zstd.DOption{zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(MaxDeltaRaw + MaxDeltaDict),
		zstd.WithDecoderMaxWindow(1 << deltaWindowLog), zstd.WithDecodeAllCapLimit(true)}
	if b[5]&deltaDictFlag != 0 {
		if len(dict) == 0 {
			return nil, errf(CodeChunkDecompress, "delta", "frame needs its dictionary")
		}
		opts = append(opts, zstd.WithDecoderDictRaw(deltaRawDictID, dict))
	}
	dec, err := zstd.NewReader(nil, opts...)
	if err != nil {
		return nil, wrapErr(CodeInvalidConfig, "delta", err)
	}
	defer dec.Close()
	raw, err := dec.DecodeAll(b[DeltaHeaderSize:], make([]byte, 0, n))
	if err != nil {
		return nil, wrapErr(CodeChunkDecompress, "delta", err)
	}
	if uint32(len(raw)) != n {
		return nil, errf(CodeChunkLengthMismatch, "delta", "%d bytes, header says %d", len(raw), n)
	}
	if crc32.Checksum(raw, castagnoli) != binary.LittleEndian.Uint32(b[12:]) {
		return nil, errf(CodeRecordChecksum, "delta", "raw CRC32C mismatch")
	}
	return raw, nil
}
