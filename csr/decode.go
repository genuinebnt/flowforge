package csr

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"slices"
	"time"
	"unicode/utf8"
)

// OpenOptions tunes snapshot loading.
type OpenOptions struct {
	// SkipDigest skips the whole-file sha256 comparison. The per-section
	// crc32c checks and every structural check still run, so a corrupt
	// file is still rejected; only corruption confined to the header and
	// padding can go unnoticed. Use it when the digest was already
	// verified (for example against graph.snapshots.sha256 after download).
	SkipDigest bool
}

// Decode parses and fully validates a snapshot file held in memory.
//
// When the host is little endian and b is suitably aligned, the returned
// Graph aliases b instead of copying the arrays: b must not be modified
// while the Graph is in use.
func Decode(b []byte) (*Graph, error) { return decode(b, OpenOptions{}) }

// DecodeWith is Decode with options.
func DecodeWith(b []byte, opts OpenOptions) (*Graph, error) { return decode(b, opts) }

type tableEntry struct {
	kind           uint16
	offset, length uint64
	crc            uint32
}

func decode(b []byte, opts OpenOptions) (*Graph, error) {
	size := uint64(len(b))
	if size < headerSize(0)+footerSize {
		return nil, fmt.Errorf("%w: %d bytes is shorter than a snapshot header", ErrFormat, size)
	}
	footerOff := size - footerSize
	if !opts.SkipDigest {
		sum := sha256.Sum256(b[:footerOff])
		if !bytes.Equal(sum[:], b[footerOff:]) {
			return nil, fmt.Errorf("%w: sha256 footer", ErrChecksum)
		}
	}
	le := binary.LittleEndian
	if string(b[:4]) != magic {
		return nil, fmt.Errorf("%w: bad magic %q", ErrFormat, b[:4])
	}
	if ver := le.Uint16(b[4:]); ver != FormatVersion {
		return nil, fmt.Errorf("%w: version %d", ErrUnsupported, ver)
	}
	var meta Meta
	meta.Flags = le.Uint16(b[6:])
	if meta.Flags&^knownFlags != 0 {
		return nil, fmt.Errorf("%w: flags %#x", ErrUnsupported, meta.Flags)
	}
	nv, ne := le.Uint64(b[8:]), le.Uint64(b[16:])
	if nv > math.MaxUint32 || ne > math.MaxUint32 {
		return nil, fmt.Errorf("%w: V=%d E=%d exceed u32 indices", ErrFormat, nv, ne)
	}
	copy(meta.TenantID[:], b[24:40])
	copy(meta.ProgramID[:], b[40:56])
	meta.WatermarkSeq = le.Uint64(b[56:])
	meta.CreatedAt = time.UnixMilli(int64(le.Uint64(b[64:]))).UTC()

	n := le.Uint32(b[headerFixed:])
	if n > maxSections {
		return nil, fmt.Errorf("%w: %d sections", ErrFormat, n)
	}
	hdrEnd := headerSize(int(n))
	if hdrEnd > footerOff {
		return nil, fmt.Errorf("%w: section table truncated", ErrFormat)
	}
	confWidth := uint64(4)
	if meta.Flags&FlagConfU8 != 0 {
		confWidth = 1
	}
	want := [numSections + 1]uint64{
		secOffsets: 8 * (nv + 1), secTargets: 4 * ne, secEdgeLabel: ne,
		secEdgeConf: confWidth * ne, secNodeType: nv, secNodeID: 16 * nv,
		secInOffsets: 8 * (nv + 1), secInSources: 4 * ne, secInEdge: 4 * ne,
	}
	var byKind [numSections + 1]*tableEntry
	entries := make([]tableEntry, n)
	for i := range entries {
		p := b[headerFixed+4+uint64(i)*tableEntrySize:]
		e := &entries[i]
		e.kind, e.offset, e.length, e.crc = le.Uint16(p), le.Uint64(p[2:]), le.Uint64(p[10:]), le.Uint32(p[18:])
		if e.kind < 1 || e.kind > numSections {
			return nil, fmt.Errorf("%w: unknown section kind %d", ErrFormat, e.kind)
		}
		if byKind[e.kind] != nil {
			return nil, fmt.Errorf("%w: duplicate section kind %d", ErrFormat, e.kind)
		}
		byKind[e.kind] = e
		if e.offset%sectionAlign != 0 || e.offset < hdrEnd || e.length > footerOff || e.offset > footerOff-e.length {
			return nil, fmt.Errorf("%w: section %d at [%d,+%d) outside the data area or misaligned", ErrFormat, e.kind, e.offset, e.length)
		}
		if e.kind != secLabelDict && e.length != want[e.kind] {
			return nil, fmt.Errorf("%w: section %d is %d bytes, want %d", ErrFormat, e.kind, e.length, want[e.kind])
		}
	}
	for k := 1; k <= numSections; k++ {
		if byKind[k] == nil {
			return nil, fmt.Errorf("%w: missing section %d", ErrFormat, k)
		}
	}
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, func(x, y tableEntry) int { return cmp.Compare(x.offset, y.offset) })
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1].offset+sorted[i-1].length > sorted[i].offset {
			return nil, fmt.Errorf("%w: sections %d and %d overlap", ErrFormat, sorted[i-1].kind, sorted[i].kind)
		}
	}
	tab := castagnoli()
	sec := func(kind uint16) []byte {
		e := byKind[kind]
		return b[e.offset : e.offset+e.length : e.offset+e.length]
	}
	for k := uint16(1); k <= numSections; k++ {
		if crc32.Checksum(sec(k), tab) != byKind[k].crc {
			return nil, fmt.Errorf("%w: crc32c of section %d", ErrChecksum, k)
		}
	}

	labels, err := decodeLabelDict(sec(secLabelDict))
	if err != nil {
		return nil, err
	}
	g := &Graph{
		meta:      meta,
		labels:    labels,
		offsets:   viewU64(sec(secOffsets)),
		targets:   viewU32(sec(secTargets)),
		edgeLabel: sec(secEdgeLabel),
		nodeType:  sec(secNodeType),
		nodeID:    viewUUIDs(sec(secNodeID)),
		inOffsets: viewU64(sec(secInOffsets)),
		inSources: viewU32(sec(secInSources)),
		inEdge:    viewU32(sec(secInEdge)),
	}
	if confWidth == 1 {
		q := sec(secEdgeConf)
		g.edgeConf = make([]float32, len(q))
		for i, x := range q {
			g.edgeConf[i] = dequantise(x)
		}
	} else {
		g.edgeConf = viewF32(sec(secEdgeConf))
	}
	if err := g.validate(); err != nil {
		return nil, err
	}
	return g, nil
}

func decodeLabelDict(b []byte) ([]string, error) {
	if len(b) < 2 {
		return nil, fmt.Errorf("%w: label dictionary truncated", ErrFormat)
	}
	n := int(binary.LittleEndian.Uint16(b))
	if n > MaxLabels {
		return nil, fmt.Errorf("%w: %d labels, at most %d", ErrFormat, n, MaxLabels)
	}
	b = b[2:]
	labels := make([]string, 0, n)
	seen := make(map[string]struct{}, n)
	for range n {
		if len(b) < 2 {
			return nil, fmt.Errorf("%w: label dictionary truncated", ErrFormat)
		}
		l := int(binary.LittleEndian.Uint16(b))
		if len(b)-2 < l {
			return nil, fmt.Errorf("%w: label dictionary truncated", ErrFormat)
		}
		s := string(b[2 : 2+l])
		b = b[2+l:]
		if !utf8.ValidString(s) {
			return nil, fmt.Errorf("%w: label is not UTF-8", ErrFormat)
		}
		if _, dup := seen[s]; dup {
			return nil, fmt.Errorf("%w: duplicate label %q", ErrFormat, s)
		}
		seen[s] = struct{}{}
		labels = append(labels, s)
	}
	if len(b) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes in label dictionary", ErrFormat, len(b))
	}
	return labels, nil
}

// validate checks every invariant the algorithms rely on, so no later
// index can go out of bounds: offsets start at 0, never decrease and end
// at E; targets are below V and ascending within a row; labels index the
// dictionary; confidences lie in [0,1]; node ids are strictly ascending;
// and the transpose is exactly the one Build derives from the forward
// arrays.
func (g *Graph) validate() error {
	nv, ne := len(g.nodeID), len(g.targets)
	if err := checkOffsets("offsets", g.offsets, nv, ne); err != nil {
		return err
	}
	if err := checkOffsets("in_offsets", g.inOffsets, nv, ne); err != nil {
		return err
	}
	for v := range nv {
		row := g.targets[g.offsets[v]:g.offsets[v+1]]
		for i, t := range row {
			if int64(t) >= int64(nv) {
				return fmt.Errorf("%w: target %d of node %d out of range", ErrFormat, t, v)
			}
			if i > 0 && row[i-1] > t {
				return fmt.Errorf("%w: row %d not sorted", ErrFormat, v)
			}
		}
	}
	for i, l := range g.edgeLabel {
		if int(l) >= len(g.labels) {
			return fmt.Errorf("%w: edge %d label %d not in dictionary", ErrFormat, i, l)
		}
	}
	for i, c := range g.edgeConf {
		if !validConf(c) {
			return fmt.Errorf("%w: edge %d confidence %v", ErrFormat, i, c)
		}
	}
	for i := 1; i < nv; i++ {
		if bytes.Compare(g.nodeID[i-1][:], g.nodeID[i][:]) >= 0 {
			return fmt.Errorf("%w: node ids not strictly ascending at %d", ErrFormat, i)
		}
	}
	cursor := slices.Clone(g.inOffsets[:nv])
	for s := range nv {
		for i := g.offsets[s]; i < g.offsets[s+1]; i++ {
			t := g.targets[i]
			c := cursor[t]
			if c >= g.inOffsets[t+1] || g.inSources[c] != uint32(s) || uint64(g.inEdge[c]) != i {
				return fmt.Errorf("%w: transpose does not match forward edge %d", ErrFormat, i)
			}
			cursor[t]++
		}
	}
	return nil
}

func checkOffsets(name string, off []uint64, nv, ne int) error {
	if len(off) != nv+1 || off[0] != 0 || off[nv] != uint64(ne) {
		return fmt.Errorf("%w: %s must run from 0 to E", ErrFormat, name)
	}
	for i := 1; i <= nv; i++ {
		if off[i] < off[i-1] {
			return fmt.Errorf("%w: %s decrease at %d", ErrFormat, name, i)
		}
	}
	return nil
}
