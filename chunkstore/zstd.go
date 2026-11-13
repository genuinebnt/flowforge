package chunkstore

import (
	"github.com/klauspost/compress/zstd"
)

// Pinned zstd settings (FORMAT.md §2). The format does not depend on the
// compressed bytes: chunk ids hash uncompressed data and integrity is
// re-checked after decompression, so any RFC 8878 encoder may write a
// record. These settings make the Go writer reproducible.
const (
	// ZstdLevel is the nominal zstd level; klauspost maps it to
	// zstd.SpeedDefault ("roughly equivalent to the default Zstandard
	// mode (level 3)"; zstd.EncoderLevelFromZstd(3) == SpeedDefault).
	ZstdLevel = 3
	// ZstdWindowLog is log2 of the encoder window (8 MiB, the
	// SpeedDefault default pinned explicitly). Frames are written single
	// segment, so the window descriptor is omitted and decoders size
	// their buffer from the frame content size, which equals raw_len.
	ZstdWindowLog = 23
	// MaxRecordRaw bounds raw_len of any record (data chunk or manifest).
	MaxRecordRaw = 64 << 20
)

type codec struct {
	enc *zstd.Encoder
	dec *zstd.Decoder
}

func encoderOptions(level zstd.EncoderLevel) []zstd.EOption {
	return []zstd.EOption{
		zstd.WithEncoderLevel(level),
		zstd.WithWindowSize(1 << ZstdWindowLog),
		zstd.WithEncoderCRC(true),      // frame checksum on
		zstd.WithEncoderConcurrency(1), // one encoder; EncodeAll output is a pure function of (options, src)
		zstd.WithSingleSegment(true),
		zstd.WithZeroFrames(true),
		zstd.WithLowerEncoderMem(false),
	}
}

func newCodec() (*codec, error) {
	return newCodecLevel(zstd.EncoderLevelFromZstd(ZstdLevel))
}

func newCodecLevel(level zstd.EncoderLevel) (*codec, error) {
	enc, err := zstd.NewWriter(nil, encoderOptions(level)...)
	if err != nil {
		return nil, wrapErr(CodeInvalidConfig, "zstd", err)
	}
	// DecodeAll on a pooled decoder starts no goroutines; concurrency only
	// sizes the pool.
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(4),
		zstd.WithDecoderMaxMemory(MaxRecordRaw),
		zstd.WithDecoderMaxWindow(MaxRecordRaw),
		zstd.WithDecodeAllCapLimit(true),
	)
	if err != nil {
		_ = enc.Close()
		return nil, wrapErr(CodeInvalidConfig, "zstd", err)
	}
	return &codec{enc: enc, dec: dec}, nil
}

func (c *codec) compress(src []byte) []byte {
	return c.enc.EncodeAll(src, make([]byte, 0, len(src)/2+64))
}

// decompress decodes one record frame into exactly rawLen bytes.
func (c *codec) decompress(id ChunkID, frame []byte, rawLen uint32) ([]byte, error) {
	if rawLen == 0 || rawLen > MaxRecordRaw {
		return nil, errf(CodeRecordMalformed, id.String(), "raw_len %d out of range", rawLen)
	}
	out, err := c.dec.DecodeAll(frame, make([]byte, 0, rawLen))
	if err != nil {
		return nil, wrapErr(CodeChunkDecompress, id.String(), err)
	}
	if len(out) != int(rawLen) {
		return nil, errf(CodeChunkLengthMismatch, id.String(), "decompressed %d bytes, raw_len %d", len(out), rawLen)
	}
	return out, nil
}

// openRecord verifies a record read from a pack against its index entry
// and returns the uncompressed chunk: header lengths, CRC32C of the frame,
// decompression to exactly raw_len, then SHA-256 against the id.
func (c *codec) openRecord(e IndexEntry, rec []byte) ([]byte, error) {
	obj := e.ID.String()
	if len(rec) != RecordHeaderSize+int(e.CompLen) {
		return nil, errf(CodeRecordMalformed, obj, "record is %d bytes, want %d", len(rec), RecordHeaderSize+int(e.CompLen))
	}
	h := parseRecordHeader(rec)
	if h.RawLen != e.RawLen || h.CompLen != e.CompLen {
		return nil, errf(CodeRecordMalformed, obj, "record lengths %d/%d disagree with index %d/%d", h.RawLen, h.CompLen, e.RawLen, e.CompLen)
	}
	frame := rec[RecordHeaderSize:]
	if CRC32C(frame) != h.CRC {
		return nil, errf(CodeRecordChecksum, obj, "frame CRC32C mismatch")
	}
	raw, err := c.decompress(e.ID, frame, e.RawLen)
	if err != nil {
		return nil, err
	}
	if SumChunk(raw) != e.ID {
		return nil, errf(CodeChunkHashMismatch, obj, "SHA-256 of decompressed chunk differs from its id")
	}
	return raw, nil
}

func (c *codec) close() {
	_ = c.enc.Close()
	c.dec.Close()
}
