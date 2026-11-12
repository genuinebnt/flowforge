package csr

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"slices"
	"testing"
)

// reseal recomputes every in-bounds section crc32c and the sha256 footer
// of b, so mutated inputs get past the checksums and exercise the
// structural validation behind them.
func reseal(b []byte) []byte {
	b = slices.Clone(b)
	if uint64(len(b)) < headerSize(0)+footerSize {
		return b
	}
	footer := uint64(len(b)) - footerSize
	n := uint64(binary.LittleEndian.Uint32(b[headerFixed:]))
	if n <= maxSections && headerSize(int(n)) <= footer {
		tab := crc32.MakeTable(crc32.Castagnoli)
		for i := range n {
			p := b[headerFixed+4+i*tableEntrySize:]
			off, l := binary.LittleEndian.Uint64(p[2:]), binary.LittleEndian.Uint64(p[10:])
			if l <= footer && off <= footer-l {
				binary.LittleEndian.PutUint32(p[18:], crc32.Checksum(b[off:off+l], tab))
			}
		}
	}
	sum := sha256.Sum256(b[:footer])
	copy(b[footer:], sum[:])
	return b
}

// checkDecode asserts that data either fails with one of the package's
// decode errors or yields a graph every algorithm runs on.
func checkDecode(t *testing.T, data []byte) {
	g, err := Decode(data)
	if err != nil {
		if !errors.Is(err, ErrFormat) && !errors.Is(err, ErrChecksum) && !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unclassified decode error: %v", err)
		}
		return
	}
	exerciseAll(t, g)
	again, err := g.Encode()
	if err != nil {
		t.Fatal(err)
	}
	g2, err := Decode(again)
	if err != nil {
		t.Fatalf("re-encoded graph rejected: %v", err)
	}
	assertSameGraph(t, g, g2)
}

func FuzzCSROpen(f *testing.F) {
	seeds := []struct {
		n      int
		edges  []Edge
		labels []string
		flags  uint16
	}{
		{n: 5, labels: []string{"dns_record", "port"}, edges: []Edge{{0, 1, 0, 0.9}, {0, 2, 1, 0.5}, {1, 3, 0, 0.8}, {2, 3, 1, 1}, {3, 4, 0, 0.7}, {4, 1, 1, 0.6}}},
		{n: 0},
		{n: 2, labels: []string{"x"}, edges: []Edge{{0, 0, 0, 0.25}, {0, 1, 0, 1}, {0, 1, 0, 1}}, flags: FlagConfU8},
	}
	for _, s := range seeds {
		g, err := Build(Meta{WatermarkSeq: 7, Flags: s.flags}, nodeUUIDs(s.n), nil, s.edges, s.labels)
		if err != nil {
			f.Fatal(err)
		}
		b, err := g.Encode()
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
		f.Add(b[:len(b)/2])
	}
	f.Add([]byte("FFCS"))
	f.Fuzz(func(t *testing.T, data []byte) {
		checkDecode(t, data)
		checkDecode(t, reseal(data))
	})
}

// TestCSRDecodeRejects pins specific structural failures behind valid
// checksums.
func TestCSRDecodeRejects(t *testing.T) {
	c := specCase(t)
	g := c.graph(t)
	good, err := g.Encode()
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	entry := func(b []byte, kind uint16) []byte {
		n := int(le.Uint32(b[headerFixed:]))
		for i := range n {
			p := b[headerFixed+4+i*tableEntrySize:]
			if le.Uint16(p) == kind {
				return p
			}
		}
		t.Fatalf("no section %d", kind)
		return nil
	}
	secData := func(b []byte, kind uint16) []byte {
		p := entry(b, kind)
		off, l := le.Uint64(p[2:]), le.Uint64(p[10:])
		return b[off : off+l]
	}
	cases := map[string]struct {
		mutate func(b []byte)
		want   error
	}{
		"bad magic":       {func(b []byte) { b[0] = 'X' }, ErrFormat},
		"version":         {func(b []byte) { le.PutUint16(b[4:], 2) }, ErrUnsupported},
		"unknown flag":    {func(b []byte) { le.PutUint16(b[6:], 1<<9) }, ErrUnsupported},
		"V mismatch":      {func(b []byte) { le.PutUint64(b[8:], 6) }, ErrFormat},
		"huge V":          {func(b []byte) { le.PutUint64(b[8:], 1<<40) }, ErrFormat},
		"section count":   {func(b []byte) { le.PutUint32(b[headerFixed:], 9) }, ErrFormat},
		"unknown kind":    {func(b []byte) { le.PutUint16(entry(b, secInEdge), 11) }, ErrFormat},
		"duplicate kind":  {func(b []byte) { le.PutUint16(entry(b, secInEdge), secInSources) }, ErrFormat},
		"misaligned":      {func(b []byte) { p := entry(b, secTargets); le.PutUint64(p[2:], le.Uint64(p[2:])+4) }, ErrFormat},
		"out of file":     {func(b []byte) { le.PutUint64(entry(b, secTargets)[2:], 1<<62) }, ErrFormat},
		"overlap":         {func(b []byte) { copy(entry(b, secTargets)[2:10], entry(b, secOffsets)[2:10]) }, ErrFormat},
		"target >= V":     {func(b []byte) { le.PutUint32(secData(b, secTargets), 5) }, ErrFormat},
		"unsorted row":    {func(b []byte) { d := secData(b, secTargets); le.PutUint32(d, 2); le.PutUint32(d[4:], 1) }, ErrFormat},
		"offsets":         {func(b []byte) { le.PutUint64(secData(b, secOffsets)[8:], 9) }, ErrFormat},
		"label index":     {func(b []byte) { secData(b, secEdgeLabel)[0] = 7 }, ErrFormat},
		"NaN conf":        {func(b []byte) { le.PutUint32(secData(b, secEdgeConf), 0x7fc00000) }, ErrFormat},
		"conf > 1":        {func(b []byte) { le.PutUint32(secData(b, secEdgeConf), 0x40000000) }, ErrFormat},
		"node order":      {func(b []byte) { secData(b, secNodeID)[15] = 0xff }, ErrFormat},
		"transpose":       {func(b []byte) { le.PutUint32(secData(b, secInEdge), 1) }, ErrFormat},
		"in_sources":      {func(b []byte) { le.PutUint32(secData(b, secInSources), 3) }, ErrFormat},
		"dict count":      {func(b []byte) { le.PutUint16(secData(b, secLabelDict), 300) }, ErrFormat},
		"dict truncation": {func(b []byte) { le.PutUint16(secData(b, secLabelDict)[2:], 200) }, ErrFormat},
		"dict bad UTF-8":  {func(b []byte) { secData(b, secLabelDict)[4] = 0xff }, ErrFormat},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := slices.Clone(good)
			tc.mutate(b)
			_, err := Decode(reseal(b))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	for _, labels := range [][]string{{"a", "a"}, {"x", "\xff"}} {
		if _, err := decodeLabelDict(encodeLabelDict(labels)); !errors.Is(err, ErrFormat) {
			t.Errorf("label dictionary %q accepted: %v", labels, err)
		}
	}
	if _, err := Decode(reseal(good)); err != nil {
		t.Fatalf("reseal broke a valid file: %v", err)
	}
}
