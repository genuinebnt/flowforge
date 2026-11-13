package textindex

import (
	"encoding/binary"
	"fmt"
)

// PostingCodec encodes one block of at most 128 postings: doc-id deltas
// and, for term postings, term frequencies (tfs is nil for trigram
// postings). The codec of a segment is recorded in its header; only the
// built-in codecs below can be read back (README "Choosing an algorithm").
type PostingCodec interface {
	// ID is the codec id stored in the FFTX header.
	ID() uint16
	// Name is a short stable name ("plain", "varint", "groupvarint").
	Name() string
	// AppendBlock appends the encoding of deltas (and tfs when non-nil,
	// same length) to dst.
	AppendBlock(dst []byte, deltas, tfs []uint32) []byte
	// DecodeBlock decodes len(deltas) postings from src into deltas (and
	// tfs when non-nil) and returns the bytes consumed; ok=false when src
	// is too short or malformed. It never reads outside src.
	DecodeBlock(src []byte, deltas, tfs []uint32) (n int, ok bool)
}

// Built-in codec ids.
const (
	CodecPlain       = 1 // fixed-width little-endian u32 deltas, then u32 tfs
	CodecVarint      = 2 // interleaved LEB128 (delta, tf) pairs; the default
	CodecGroupVarint = 3 // group-varint deltas, then group-varint tfs
)

// PlainCodec stores every value as a little-endian u32: the baseline.
type PlainCodec struct{}

// VarintCodec stores (delta, tf) pairs as minimal unsigned LEB128,
// interleaved. It is the default.
type VarintCodec struct{}

// GroupVarintCodec stores the deltas, then the tfs, in groups of four
// values led by a control byte holding four 2-bit (byte length - 1)
// fields, lane 0 in the low bits (Dean, WSDM 2009). A final partial group
// has control bits only for its present values.
type GroupVarintCodec struct{}

// Codecs lists the built-in posting codecs in id order.
func Codecs() []PostingCodec { return []PostingCodec{PlainCodec{}, VarintCodec{}, GroupVarintCodec{}} }

func codecByID(id uint16) (PostingCodec, bool) {
	for _, c := range Codecs() {
		if c.ID() == id {
			return c, true
		}
	}
	return nil, false
}

// ID implements PostingCodec.
func (PlainCodec) ID() uint16 { return CodecPlain }

// Name implements PostingCodec.
func (PlainCodec) Name() string { return "plain" }

// AppendBlock implements PostingCodec.
func (PlainCodec) AppendBlock(dst []byte, deltas, tfs []uint32) []byte {
	for _, v := range deltas {
		dst = le32(dst, v)
	}
	for _, v := range tfs {
		dst = le32(dst, v)
	}
	return dst
}

// DecodeBlock implements PostingCodec.
func (PlainCodec) DecodeBlock(src []byte, deltas, tfs []uint32) (int, bool) {
	n := len(deltas) * 4
	if tfs != nil {
		n += len(tfs) * 4
	}
	if len(src) < n {
		return 0, false
	}
	for i := range deltas {
		deltas[i] = binary.LittleEndian.Uint32(src[4*i:])
	}
	off := 4 * len(deltas)
	for i := range tfs {
		tfs[i] = binary.LittleEndian.Uint32(src[off+4*i:])
	}
	return n, true
}

// ID implements PostingCodec.
func (VarintCodec) ID() uint16 { return CodecVarint }

// Name implements PostingCodec.
func (VarintCodec) Name() string { return "varint" }

// AppendBlock implements PostingCodec.
func (VarintCodec) AppendBlock(dst []byte, deltas, tfs []uint32) []byte {
	for i, v := range deltas {
		dst = appendUvarint(dst, uint64(v))
		if tfs != nil {
			dst = appendUvarint(dst, uint64(tfs[i]))
		}
	}
	return dst
}

// DecodeBlock implements PostingCodec.
func (VarintCodec) DecodeBlock(src []byte, deltas, tfs []uint32) (int, bool) {
	off := 0
	var ok bool
	for i := range deltas {
		if deltas[i], off, ok = uvarint32(src, off); !ok {
			return 0, false
		}
		if tfs != nil {
			if tfs[i], off, ok = uvarint32(src, off); !ok {
				return 0, false
			}
		}
	}
	return off, true
}

// ID implements PostingCodec.
func (GroupVarintCodec) ID() uint16 { return CodecGroupVarint }

// Name implements PostingCodec.
func (GroupVarintCodec) Name() string { return "groupvarint" }

func byteLen(v uint32) int {
	switch {
	case v < 1<<8:
		return 1
	case v < 1<<16:
		return 2
	case v < 1<<24:
		return 3
	}
	return 4
}

func appendGroupVarint(dst []byte, vs []uint32) []byte {
	for g := 0; g < len(vs); g += 4 {
		grp := vs[g:min(g+4, len(vs))]
		ctl := byte(0)
		for k, v := range grp {
			ctl |= byte(byteLen(v)-1) << (2 * k)
		}
		dst = append(dst, ctl)
		for _, v := range grp {
			for b := 0; b < byteLen(v); b++ {
				dst = append(dst, byte(v>>(8*b)))
			}
		}
	}
	return dst
}

func decodeGroupVarint(src []byte, off int, vs []uint32) (int, bool) {
	for g := 0; g < len(vs); g += 4 {
		if off >= len(src) {
			return 0, false
		}
		ctl := src[off]
		off++
		end := min(g+4, len(vs))
		if end-g < 4 && ctl>>(2*(end-g)) != 0 {
			return 0, false // control bits for absent lanes must be zero
		}
		for k := g; k < end; k++ {
			l := int(ctl>>(2*(k-g))&3) + 1
			if off+l > len(src) {
				return 0, false
			}
			var v uint32
			for b := 0; b < l; b++ {
				v |= uint32(src[off+b]) << (8 * b)
			}
			vs[k] = v
			off += l
		}
	}
	return off, true
}

// AppendBlock implements PostingCodec.
func (GroupVarintCodec) AppendBlock(dst []byte, deltas, tfs []uint32) []byte {
	dst = appendGroupVarint(dst, deltas)
	if tfs != nil {
		dst = appendGroupVarint(dst, tfs)
	}
	return dst
}

// DecodeBlock implements PostingCodec.
func (GroupVarintCodec) DecodeBlock(src []byte, deltas, tfs []uint32) (int, bool) {
	off, ok := decodeGroupVarint(src, 0, deltas)
	if !ok {
		return 0, false
	}
	if tfs != nil {
		if off, ok = decodeGroupVarint(src, off, tfs); !ok {
			return 0, false
		}
	}
	return off, true
}

func checkCodec(c PostingCodec) (PostingCodec, error) {
	if c == nil {
		return VarintCodec{}, nil
	}
	if _, ok := codecByID(c.ID()); !ok {
		return nil, fmt.Errorf("%w: posting codec %q (id %d) is not a built-in codec", ErrInvalidConfig, c.Name(), c.ID())
	}
	return c, nil
}
