package chunkstore

import (
	"bytes"
	"encoding/binary"
	"slices"
)

// Pack index "FFPI" layout (FORMAT.md §4).
const (
	IndexMagic      = "FFPI"
	IndexVersion    = 1
	IndexHeaderSize = 56 // magic[4] version u16 flags u16 count u32 pack_crc u32 pack_size u64 pack_id[32]
	indexFanoutSize = 256 * 4
	indexEntrySize  = 32 + 8 + 4 + 4
	// MaxIndexEntries bounds the entry count so sizes never overflow.
	MaxIndexEntries = 1 << 26
)

// IndexEntry locates one chunk in a pack.
type IndexEntry struct {
	ID      ChunkID
	Offset  uint64 // of the record header within the pack
	CompLen uint32 // zstd frame length
	RawLen  uint32 // uncompressed length
}

// RecordSize is the record's size in the pack (header plus frame).
func (e IndexEntry) RecordSize() uint64 { return RecordHeaderSize + uint64(e.CompLen) }

// Index is a decoded pack index: entries sorted by id with a git-style
// cumulative fan-out table on the first id byte.
type Index struct {
	PackID   PackID
	PackSize uint64
	PackCRC  uint32 // the pack's footer CRC32C
	Entries  []IndexEntry
	fanout   [256]uint32
}

// NewIndex sorts a copy of entries by id and builds the fan-out table. It
// fails on duplicate ids.
func NewIndex(packID PackID, packSize uint64, packCRC uint32, entries []IndexEntry) (*Index, error) {
	es := slices.Clone(entries)
	slices.SortFunc(es, func(a, b IndexEntry) int { return a.ID.Compare(b.ID) })
	for i := 1; i < len(es); i++ {
		if es[i].ID == es[i-1].ID {
			return nil, errf(CodeIndexMalformed, packID.String(), "duplicate chunk %s", es[i].ID)
		}
	}
	ix := &Index{PackID: packID, PackSize: packSize, PackCRC: packCRC, Entries: es}
	ix.buildFanout()
	return ix, nil
}

func (ix *Index) buildFanout() {
	ix.fanout = [256]uint32{}
	for _, e := range ix.Entries {
		ix.fanout[e.ID[0]]++
	}
	var sum uint32
	for b := range ix.fanout {
		sum += ix.fanout[b]
		ix.fanout[b] = sum
	}
}

// Lookup finds id by fan-out then binary search.
func (ix *Index) Lookup(id ChunkID) (IndexEntry, bool) {
	lo := uint32(0)
	if id[0] > 0 {
		lo = ix.fanout[id[0]-1]
	}
	hi := ix.fanout[id[0]]
	for lo < hi {
		mid := lo + (hi-lo)/2
		switch c := bytes.Compare(ix.Entries[mid].ID[:], id[:]); {
		case c == 0:
			return ix.Entries[mid], true
		case c < 0:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return IndexEntry{}, false
}

// Encode serializes the index. The bytes are a pure function of the
// pack fields and the entry set.
func (ix *Index) Encode() []byte {
	n := len(ix.Entries)
	size := IndexHeaderSize + indexFanoutSize + n*indexEntrySize + 4
	b := make([]byte, 0, size)
	b = append(b, IndexMagic...)
	b = binary.LittleEndian.AppendUint16(b, IndexVersion)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint32(b, uint32(n))
	b = binary.LittleEndian.AppendUint32(b, ix.PackCRC)
	b = binary.LittleEndian.AppendUint64(b, ix.PackSize)
	b = append(b, ix.PackID[:]...)
	for _, f := range ix.fanout {
		b = binary.LittleEndian.AppendUint32(b, f)
	}
	for _, e := range ix.Entries {
		b = append(b, e.ID[:]...)
	}
	for _, e := range ix.Entries {
		b = binary.LittleEndian.AppendUint64(b, e.Offset)
	}
	for _, e := range ix.Entries {
		b = binary.LittleEndian.AppendUint32(b, e.CompLen)
	}
	for _, e := range ix.Entries {
		b = binary.LittleEndian.AppendUint32(b, e.RawLen)
	}
	return binary.LittleEndian.AppendUint32(b, CRC32C(b))
}

// DecodeIndex validates and decodes an index: size, magic, CRC32C,
// version and flags, exact length for the count, fan-out consistent with
// the ids, ids strictly ascending, records within the pack body,
// non-overlapping, and raw_len in [1, MaxRecordRaw].
func DecodeIndex(data []byte) (*Index, error) {
	const obj = "index"
	if len(data) < IndexHeaderSize+indexFanoutSize+4 {
		return nil, errf(CodeIndexTruncated, obj, "%d bytes", len(data))
	}
	if string(data[:4]) != IndexMagic {
		return nil, errf(CodeIndexBadMagic, obj, "magic %q", data[:4])
	}
	end := len(data) - 4
	if CRC32C(data[:end]) != binary.LittleEndian.Uint32(data[end:]) {
		return nil, errf(CodeIndexChecksum, obj, "CRC32C mismatch")
	}
	version := binary.LittleEndian.Uint16(data[4:])
	flags := binary.LittleEndian.Uint16(data[6:])
	if version != IndexVersion || flags != 0 {
		return nil, errf(CodeIndexUnsupported, obj, "version %d flags %#x", version, flags)
	}
	n := uint64(binary.LittleEndian.Uint32(data[8:]))
	if n > MaxIndexEntries || uint64(len(data)) != IndexHeaderSize+indexFanoutSize+n*indexEntrySize+4 {
		return nil, errf(CodeIndexMalformed, obj, "length %d does not match %d entries", len(data), n)
	}
	ix := &Index{
		PackCRC:  binary.LittleEndian.Uint32(data[12:]),
		PackSize: binary.LittleEndian.Uint64(data[16:]),
		Entries:  make([]IndexEntry, n),
	}
	copy(ix.PackID[:], data[24:56])
	fo := data[IndexHeaderSize:]
	for b := range ix.fanout {
		ix.fanout[b] = binary.LittleEndian.Uint32(fo[b*4:])
	}
	ids := data[IndexHeaderSize+indexFanoutSize:]
	offs := ids[n*32:]
	comps := offs[n*8:]
	raws := comps[n*4:]
	for i := range ix.Entries {
		e := &ix.Entries[i]
		copy(e.ID[:], ids[i*32:])
		e.Offset = binary.LittleEndian.Uint64(offs[i*8:])
		e.CompLen = binary.LittleEndian.Uint32(comps[i*4:])
		e.RawLen = binary.LittleEndian.Uint32(raws[i*4:])
	}
	if err := ix.validate(); err != nil {
		return nil, err
	}
	return ix, nil
}

func (ix *Index) validate() error {
	const obj = "index"
	var want [256]uint32
	for i, e := range ix.Entries {
		if i > 0 && bytes.Compare(ix.Entries[i-1].ID[:], e.ID[:]) >= 0 {
			return errf(CodeIndexMalformed, obj, "ids not strictly ascending at %d", i)
		}
		want[e.ID[0]]++
	}
	var sum uint32
	for b := range want {
		sum += want[b]
		if ix.fanout[b] != sum {
			return errf(CodeIndexMalformed, obj, "fan-out[%d] = %d, want %d", b, ix.fanout[b], sum)
		}
	}
	if ix.PackSize < PackHeaderSize+PackFooterSize {
		return errf(CodeIndexMalformed, obj, "pack_size %d too small", ix.PackSize)
	}
	body := ix.PackSize - PackFooterSize
	spans := make([]IndexEntry, len(ix.Entries))
	copy(spans, ix.Entries)
	slices.SortFunc(spans, func(a, b IndexEntry) int {
		switch {
		case a.Offset < b.Offset:
			return -1
		case a.Offset > b.Offset:
			return 1
		}
		return 0
	})
	next := uint64(PackHeaderSize)
	for _, e := range spans {
		if e.RawLen == 0 || e.RawLen > MaxRecordRaw || e.CompLen == 0 {
			return errf(CodeIndexMalformed, obj, "entry %s: raw_len %d comp_len %d", e.ID, e.RawLen, e.CompLen)
		}
		if e.Offset < next || e.Offset > body || e.RecordSize() > body-e.Offset {
			return errf(CodeIndexMalformed, obj, "entry %s: record [%d,+%d) overlaps or leaves the pack body", e.ID, e.Offset, e.RecordSize())
		}
		next = e.Offset + e.RecordSize()
	}
	return nil
}
