package vecindex

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

// reseal recomputes every checksum a segment file declares, so the fuzzer
// can reach the structural validation behind the checksums: random bytes
// almost never carry valid CRCs.
func reseal(b []byte) []byte {
	if len(b) < SectorSize+trailerSize {
		return b
	}
	c := slices.Clone(b)
	le := binary.LittleEndian
	body := uint64(len(c) - trailerSize)
	for i := range 4 {
		o := hSections + i*24
		off, n := le.Uint64(c[o:]), le.Uint64(c[o+8:])
		if off >= SectorSize && off <= body && n <= body-off {
			le.PutUint32(c[o+16:], crc32.Checksum(c[off:off+n], castagnoli))
		}
	}
	le.PutUint32(c[headerCRCOff:], crc32.Checksum(c[:headerCRCOff], castagnoli))
	h := crc32.New(castagnoli)
	h.Write(c[SectorSize:body])
	h.Write(c[:SectorSize])
	copy(c[body:], trailerMagic)
	le.PutUint32(c[body+4:], h.Sum32())
	return c
}

func fuzzSegmentSeed(f *testing.F) []byte {
	r := core.NewSplitMix64(1)
	entries := make([]SegmentEntry, 12)
	for i := range entries {
		entries[i] = SegmentEntry{ID: idOf(i), Seq: uint64(i), Vector: randomVector(r, 6)}
	}
	var m memFile
	if _, err := WriteSegment(&m, entries, SegmentConfig{Metric: L2, Graph: VamanaParams{R: 4, LBuild: 8}, PQ: PQConfig{M: 2}}, r); err != nil {
		f.Fatal(err)
	}
	return m.b
}

// fuzz_segment_open: arbitrary bytes as a segment file give a clean error,
// never a panic or an out-of-bounds read; files that do open (including
// fuzzed files whose checksums were recomputed) answer searches without
// panicking.
func FuzzSegmentOpen(f *testing.F) {
	valid := fuzzSegmentSeed(f)
	f.Add(valid)
	f.Add(valid[:SectorSize])
	f.Add(valid[:len(valid)-1])
	f.Add([]byte{})
	f.Add([]byte("FFDA"))
	hdr := slices.Clone(valid)
	binary.LittleEndian.PutUint64(hdr[hCount:], 1<<40)
	f.Add(reseal(hdr))
	deg := slices.Clone(valid)
	binary.LittleEndian.PutUint32(deg[SectorSize+4*6:], 999)
	f.Add(reseal(deg))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, b := range [][]byte{data, reseal(data)} {
			s, err := OpenSegment(bytesReaderAt(b), int64(len(b)))
			if err != nil {
				if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrChecksum) {
					t.Fatalf("unexpected error class: %v", err)
				}
				continue
			}
			q := make([]float32, s.Dim())
			for i := range q {
				q[i] = float32(i%7) - 3
			}
			if s.Metric() == Cosine {
				q[0] = 1
			}
			if _, _, err := s.Search(q, 1, 8, 2); err != nil && !errors.Is(err, ErrCorrupt) {
				t.Fatalf("search on an opened segment: %v", err)
			}
			for i := range min(s.Len(), 4) {
				if _, err := s.Vector(uint32(i)); err != nil && !errors.Is(err, ErrCorrupt) {
					t.Fatal(err)
				}
			}
		}
	})
}
