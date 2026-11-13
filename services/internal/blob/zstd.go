package blob

import (
	"encoding/binary"
	"errors"

	"github.com/cespare/xxhash/v2"
)

// zstd frames (RFC 8878) for the run archives of spec 10.7 ("zstd JSON
// Lines"). The module has no zstd encoder, so ZstdStored writes valid zstd
// frames made of Raw (stored) blocks: any zstd decoder (`zstd -d`,
// klauspost/compress, libzstd) reads them, the format and file names are
// final, and only the compression ratio is missing. Swapping in a real
// encoder later changes no reader.
//
// PORT: a Rust port can use the zstd crate with level 3 directly; the
// archive contract is "a zstd frame of JSON Lines with a content checksum".

const (
	zstdMagic    = 0xFD2FB528
	zstdMaxBlock = 128 << 10 // RFC 8878 3.1.1.2.3: Block_Maximum_Size cap
)

// ZstdStored returns data as one zstd frame of Raw blocks with
// Frame_Content_Size and a content checksum (the low 32 bits of XXH64,
// RFC 8878 3.1.1).
func ZstdStored(data []byte) []byte {
	out := make([]byte, 0, len(data)+len(data)/zstdMaxBlock*3+24)
	out = binary.LittleEndian.AppendUint32(out, zstdMagic)
	// Frame_Header_Descriptor: FCS_Field_Size=3 (8 bytes), Single_Segment=1
	// (no Window_Descriptor; the window is the content size),
	// Content_Checksum=1, no dictionary.
	out = append(out, 0b1110_0100)
	out = binary.LittleEndian.AppendUint64(out, uint64(len(data)))
	rest := data
	for {
		n := min(len(rest), zstdMaxBlock)
		last := n == len(rest)
		// Block_Header (3.1.1.2): Last_Block bit 0, Block_Type 0 (Raw) in
		// bits 1-2, Block_Size in bits 3-23.
		h := uint32(n) << 3
		if last {
			h |= 1
		}
		out = append(out, byte(h), byte(h>>8), byte(h>>16))
		out = append(out, rest[:n]...)
		rest = rest[n:]
		if last {
			break
		}
	}
	return binary.LittleEndian.AppendUint32(out, uint32(xxhash.Sum64(data)))
}

// ErrZstd reports a frame ZstdDecodeStored cannot read.
var ErrZstd = errors.New("blob: not a stored zstd frame")

// ZstdDecodeStored reads one frame written by ZstdStored (Raw and RLE
// blocks only; compressed blocks need a real decoder) and verifies its
// checksum. It exists for tests and for restoring an archive without a
// zstd tool.
func ZstdDecodeStored(b []byte) ([]byte, error) {
	if len(b) < 5 || binary.LittleEndian.Uint32(b) != zstdMagic {
		return nil, ErrZstd
	}
	fhd := b[4]
	b = b[5:]
	if fhd&0b0010_0000 == 0 || fhd&0b11 != 0 {
		return nil, ErrZstd // needs a Window_Descriptor or a dictionary
	}
	var fcsLen int
	switch fhd >> 6 {
	case 0:
		fcsLen = 1
	case 1:
		fcsLen = 2
	case 2:
		fcsLen = 4
	case 3:
		fcsLen = 8
	}
	if len(b) < fcsLen {
		return nil, ErrZstd
	}
	b = b[fcsLen:]
	var out []byte
	for {
		if len(b) < 3 {
			return nil, ErrZstd
		}
		h := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
		b = b[3:]
		size := int(h >> 3)
		switch (h >> 1) & 3 {
		case 0: // Raw
			if len(b) < size {
				return nil, ErrZstd
			}
			out = append(out, b[:size]...)
			b = b[size:]
		case 1: // RLE: one byte repeated Block_Size times
			if len(b) < 1 {
				return nil, ErrZstd
			}
			for range size {
				out = append(out, b[0])
			}
			b = b[1:]
		default:
			return nil, ErrZstd
		}
		if h&1 == 1 {
			break
		}
	}
	if fhd&0b100 != 0 {
		if len(b) < 4 || binary.LittleEndian.Uint32(b) != uint32(xxhash.Sum64(out)) {
			return nil, ErrZstd
		}
	}
	return out, nil
}
