package textindex

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// CommitFile is the name of the commit point in a Directory.
const CommitFile = "COMMIT"

const (
	commitMagic   = "FFTC"
	tombMagic     = "FFTD"
	commitVersion = 1
	tombVersion   = 1
)

// commitSeg is one segment entry of the FFTC commit point.
type commitSeg struct {
	name     string
	docCount uint32
	segCRC   uint32
	delGen   uint64 // 0: no tombstone file
	delCount uint32
}

// commitPoint is the decoded FFTC file (FORMAT.md): commit generation,
// largest document generation, next segment number and the live segments
// in commit order.
type commitPoint struct {
	gen     uint64
	maxGen  uint64
	nextSeg uint64
	segs    []commitSeg
}

func encodeCommit(c *commitPoint) []byte {
	b := append([]byte(commitMagic), 0, 0, 0, 0)
	binary.LittleEndian.PutUint16(b[4:], commitVersion)
	b = le64(b, c.gen)
	b = le64(b, c.maxGen)
	b = le64(b, c.nextSeg)
	b = le32(b, uint32(len(c.segs)))
	for _, s := range c.segs {
		b = le16(b, uint16(len(s.name)))
		b = append(b, s.name...)
		b = le32(b, s.docCount)
		b = le32(b, s.segCRC)
		b = le64(b, s.delGen)
		b = le32(b, s.delCount)
	}
	return le32(b, crc32c(b))
}

func decodeCommit(b []byte) (*commitPoint, error) {
	if len(b) < 36 || string(b[:4]) != commitMagic {
		return nil, corruptf("bad commit point")
	}
	if crc32c(b[:len(b)-4]) != binary.LittleEndian.Uint32(b[len(b)-4:]) {
		return nil, fmt.Errorf("%w: commit point", ErrChecksum)
	}
	r := newRd(b[:len(b)-4])
	r.off = 4
	if r.u16() != commitVersion || r.u16() != 0 {
		return nil, corruptf("commit point version")
	}
	c := &commitPoint{gen: r.u64(), maxGen: r.u64(), nextSeg: r.u64()}
	n := int(r.u32())
	if n > len(b)/22 {
		return nil, corruptf("commit segment count")
	}
	for i := 0; i < n && r.ok; i++ {
		var s commitSeg
		s.name = string(r.bytes(int(r.u16())))
		s.docCount, s.segCRC, s.delGen, s.delCount = r.u32(), r.u32(), r.u64(), r.u32()
		c.segs = append(c.segs, s)
	}
	if !r.done() {
		return nil, corruptf("commit point")
	}
	return c, nil
}

func encodeTombstones(docCount int, segCRC uint32, delGen uint64, del bitmap) []byte {
	b := append([]byte(tombMagic), 0, 0, 0, 0)
	binary.LittleEndian.PutUint16(b[4:], tombVersion)
	b = le32(b, uint32(docCount))
	b = le32(b, segCRC)
	b = le64(b, delGen)
	b = le32(b, uint32(popcount(del)))
	b = append(b, del...)
	return le32(b, crc32c(b))
}

func decodeTombstones(b []byte, docCount int, segCRC uint32, delGen uint64) (bitmap, error) {
	if len(b) < 32 || string(b[:4]) != tombMagic {
		return nil, corruptf("bad tombstone file")
	}
	if crc32c(b[:len(b)-4]) != binary.LittleEndian.Uint32(b[len(b)-4:]) {
		return nil, fmt.Errorf("%w: tombstone file", ErrChecksum)
	}
	r := newRd(b[:len(b)-4])
	r.off = 4
	if r.u16() != tombVersion || r.u16() != 0 {
		return nil, corruptf("tombstone version")
	}
	n, crc, gen, cnt := r.u32(), r.u32(), r.u64(), r.u32()
	if int(n) != docCount || crc != segCRC || gen != delGen {
		return nil, corruptf("tombstone file does not belong to its segment")
	}
	bm := bitmap(append([]byte(nil), r.bytes(bitmapBytes(docCount))...))
	if !r.done() || popcount(bm) != int(cnt) || (docCount%8 != 0 && bm[len(bm)-1]>>(docCount%8) != 0) {
		return nil, corruptf("tombstone bitmap")
	}
	return bm, nil
}

func segFileName(n uint64) string { return fmt.Sprintf("seg_%016x.fftx", n) }

func tombFileName(seg string, delGen uint64) string {
	return fmt.Sprintf("%s_%016x.fftd", strings.TrimSuffix(seg, ".fftx"), delGen)
}

// parseSegFileName returns the number of a segment file name.
func parseSegFileName(name string) (uint64, bool) {
	hex, ok := strings.CutPrefix(name, "seg_")
	if !ok {
		return 0, false
	}
	hex, ok = strings.CutSuffix(hex, ".fftx")
	if !ok || len(hex) != 16 {
		return 0, false
	}
	n, err := strconv.ParseUint(hex, 16, 64)
	return n, err == nil
}
