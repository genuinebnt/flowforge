package textindex

import (
	"encoding/binary"
	"hash/crc32"
	"slices"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func crc32c(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// appendUvarint appends v as unsigned LEB128 (minimal encoding).
func appendUvarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

// uvarint32 decodes a minimal LEB128 u32 at b[off:]. It returns ok=false
// on truncation, overflow (more than 5 bytes or a value above 2^32-1) or a
// non-minimal encoding. It never reads outside b.
func uvarint32(b []byte, off int) (v uint32, next int, ok bool) {
	var x uint64
	for n := 0; n < 5; n++ {
		if off+n >= len(b) || off+n < 0 {
			return 0, off, false
		}
		c := b[off+n]
		x |= uint64(c&0x7F) << (7 * n)
		if c < 0x80 {
			if n > 0 && c == 0 {
				return 0, off, false // non-minimal
			}
			if x > 0xFFFFFFFF {
				return 0, off, false
			}
			return uint32(x), off + n + 1, true
		}
	}
	return 0, off, false
}

// skipUvarints skips n varints starting at off, returning the new offset;
// ok=false if b ends first.
func skipUvarints(b []byte, off, n int) (int, bool) {
	for ; n > 0; n-- {
		for {
			if off >= len(b) {
				return off, false
			}
			c := b[off]
			off++
			if c < 0x80 {
				break
			}
		}
	}
	return off, true
}

func sortUnique32(t []uint32) []uint32 {
	slices.Sort(t)
	return slices.Compact(t)
}

// rd is a bounds-checked little-endian reader. After any failed read ok is
// false and every further read returns zero values.
type rd struct {
	b   []byte
	off int
	ok  bool
}

func newRd(b []byte) *rd { return &rd{b: b, ok: true} }

func (r *rd) need(n int) bool {
	if !r.ok || n < 0 || r.off > len(r.b)-n {
		r.ok = false
		return false
	}
	return true
}

func (r *rd) u8() uint8 {
	if !r.need(1) {
		return 0
	}
	v := r.b[r.off]
	r.off++
	return v
}

func (r *rd) u16() uint16 {
	if !r.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(r.b[r.off:])
	r.off += 2
	return v
}

func (r *rd) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b[r.off:])
	r.off += 4
	return v
}

func (r *rd) u64() uint64 {
	if !r.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(r.b[r.off:])
	r.off += 8
	return v
}

func (r *rd) bytes(n int) []byte {
	if !r.need(n) {
		return nil
	}
	v := r.b[r.off : r.off+n]
	r.off += n
	return v
}

func (r *rd) uvarint() uint32 {
	if !r.ok {
		return 0
	}
	v, next, ok := uvarint32(r.b, r.off)
	if !ok {
		r.ok = false
		return 0
	}
	r.off = next
	return v
}

func (r *rd) done() bool { return r.ok && r.off == len(r.b) }

func le16(dst []byte, v uint16) []byte { return binary.LittleEndian.AppendUint16(dst, v) }
func le32(dst []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(dst, v) }
func le64(dst []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(dst, v) }

// bitmap is a plain little-endian bitmap: bit i is byte i/8, bit i%8
// (LSB first).
type bitmap []byte

func newBitmap(n int) bitmap         { return make(bitmap, (n+7)/8) }
func (b bitmap) get(i uint32) bool   { return int(i/8) < len(b) && b[i/8]&(1<<(i%8)) != 0 }
func (b bitmap) set(i uint32)        { b[i/8] |= 1 << (i % 8) }
func (b bitmap) clone() bitmap       { return slices.Clone(b) }
func bitmapBytes(n int) int          { return (n + 7) / 8 }
func popcount(b bitmap) (n int)      { for _, c := range b { n += popcnt8(c) }; return n }
func popcnt8(c byte) int             { n := 0; for ; c != 0; c &= c - 1 { n++ }; return n }
