package blob

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// zstd frames (RFC 8878) for the run archives of spec 10.7 ("zstd JSON
// Lines"). The encoder settings are pinned (default level, frame content
// checksum on, single goroutine) so the same input always gives the same
// frame; readers accept any valid zstd frame, including ones written by
// libzstd or by the earlier stored-block writer.
//
// PORT: the Rust port can use the zstd crate (level 3, checksum on). Frames
// from different encoders differ byte for byte but decode to the same JSON
// Lines; the archive contract is the decoded content plus the frame
// checksum, not the compressed bytes.

var (
	encOnce sync.Once
	enc     *zstd.Encoder
	encErr  error
)

func encoder() (*zstd.Encoder, error) {
	encOnce.Do(func() {
		enc, encErr = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedDefault),
			zstd.WithEncoderCRC(true),
			zstd.WithEncoderConcurrency(1))
	})
	return enc, encErr
}

// ZstdEncode compresses data into one zstd frame with a content checksum.
func ZstdEncode(data []byte) ([]byte, error) {
	e, err := encoder()
	if err != nil {
		return nil, fmt.Errorf("blob: zstd encoder: %w", err)
	}
	return e.EncodeAll(data, nil), nil
}

// ZstdDecode decompresses a zstd frame, verifying its checksum, and caps
// the output at maxSize bytes so a hostile archive cannot exhaust memory.
func ZstdDecode(b []byte, maxSize int64) ([]byte, error) {
	// The decoder's own memory cap must be positive; the exact output cap is
	// enforced by the LimitReader below.
	d, err := zstd.NewReader(bytes.NewReader(b), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(max(maxSize, 1<<20))))
	if err != nil {
		return nil, fmt.Errorf("blob: zstd reader: %w", err)
	}
	defer d.Close()
	out, err := io.ReadAll(io.LimitReader(d, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("blob: zstd decode: %w", err)
	}
	if int64(len(out)) > maxSize {
		return nil, fmt.Errorf("blob: zstd decode: output exceeds %d bytes", maxSize)
	}
	return out, nil
}
