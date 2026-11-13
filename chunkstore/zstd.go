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
	// DefaultRecompressLevel is the zstd level GC uses when it rewrites a
	// pack (klauspost: SpeedBestCompression).
	DefaultRecompressLevel = 19
)

// codec holds the pinned zstd encoder and a decoder bounded to
// MaxRecordRaw; both are safe for concurrent use (EncodeAll, DecodeAll).
type codec struct {
	enc *zstd.Encoder
	dec *zstd.Decoder
}

// encoderOptions are the pinned writer settings of FORMAT.md §2.
//
// PORT: klauspost's frames differ from the C library's for the same level;
// only frame validity is shared across languages, not the bytes.
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
	return newCodecLevel(ZstdLevel)
}

// newCodecLevel builds a codec whose encoder runs at the zstd level
// (mapped by zstd.EncoderLevelFromZstd: <3 fastest, 3-5 default, 6-9
// better, >=10 best).
func newCodecLevel(level int) (*codec, error) {
	enc, err := zstd.NewWriter(nil, encoderOptions(zstd.EncoderLevelFromZstd(level))...)
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

// keepCompressed reports whether a payload of compLen bytes saves at
// least 1/8 of rawLen: 8*compLen <= 7*rawLen (integer arithmetic).
func keepCompressed(compLen, rawLen int) bool { return 8*uint64(compLen) <= 7*uint64(rawLen) }

// encode returns the record codec and payload for raw: a zstd frame when
// it saves at least 1/8, the raw bytes otherwise.
func (c *codec) encode(raw []byte) (uint8, []byte) {
	frame := c.enc.EncodeAll(raw, make([]byte, 0, len(raw)/2+64))
	if keepCompressed(len(frame), len(raw)) {
		return CodecZstd, frame
	}
	return CodecRaw, raw
}

// decompress decodes one zstd payload into exactly rawLen bytes.
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
// and returns the uncompressed chunk: header lengths against the index,
// known codec and zero reserved bytes, the record CRC32C, decoding to
// exactly raw_len, then SHA-256 against the id.
func (c *codec) openRecord(e IndexEntry, rec []byte) ([]byte, error) {
	obj := e.ID.String()
	if len(rec) != RecordHeaderSize+int(e.CompLen) {
		return nil, errf(CodeRecordMalformed, obj, "record is %d bytes, want %d", len(rec), RecordHeaderSize+int(e.CompLen))
	}
	h, reservedZero := parseRecordHeader(rec)
	if h.RawLen != e.RawLen || h.CompLen != e.CompLen {
		return nil, errf(CodeRecordMalformed, obj, "record lengths %d/%d disagree with index %d/%d", h.RawLen, h.CompLen, e.RawLen, e.CompLen)
	}
	if recordCRC(rec) != h.CRC {
		return nil, errf(CodeRecordChecksum, obj, "record CRC32C mismatch")
	}
	if !reservedZero || !validRecordShape(h) {
		return nil, errf(CodeRecordMalformed, obj, "codec %d raw_len %d stored_len %d", h.Codec, h.RawLen, h.CompLen)
	}
	payload := rec[RecordHeaderSize:]
	var raw []byte
	if h.Codec == CodecRaw {
		raw = payload
	} else {
		var err error
		if raw, err = c.decompress(e.ID, payload, e.RawLen); err != nil {
			return nil, err
		}
	}
	if SumChunk(raw) != e.ID {
		return nil, errf(CodeChunkHashMismatch, obj, "SHA-256 of decoded chunk differs from its id")
	}
	return raw, nil
}

func (c *codec) close() {
	_ = c.enc.Close()
	c.dec.Close()
}
