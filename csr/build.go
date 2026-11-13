package csr

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Build constructs a snapshot (spec 21.2).
//
// nodes holds the entity ids; nodeTypes is either empty (every type 0) or
// parallel to nodes. Edge Src and Dst index nodes, Label indexes labels.
// Build renumbers nodes in ascending entity-id order, so the same nodes,
// edges and label dictionary (the dictionary order is part of the content:
// it fixes the label indices) in any node or edge order yield a
// byte-identical snapshot (csr_009). Rows are sorted by (target, label,
// confidence); the transpose lists each node's in-edges by (source,
// forward edge index). Confidence -0 is stored as +0. Errors wrap
// ErrInvalidInput.
//
// PORT: slices.SortFunc is not stable, which is harmless here because
// records that compare equal are identical. Keep that property (or use a
// stable sort) so csr_009's cross-language SHA-256 comparison holds.
//
// The work is O(V log V) for the id sort (O(V) when nodes is already
// sorted) plus O(V + E) for the counting sort and the transpose, plus
// sorting each row. The input slices are not retained or modified.
func Build(meta Meta, nodes []core.UUID, nodeTypes []uint8, edges []Edge, labels []string) (*Graph, error) {
	if meta.Flags&^knownFlags != 0 {
		return nil, fmt.Errorf("%w: unknown flags %#x", ErrInvalidInput, meta.Flags)
	}
	if err := validateLabels(labels); err != nil {
		return nil, err
	}
	v := len(nodes)
	if uint64(v) > math.MaxUint32 {
		return nil, fmt.Errorf("%w: %d nodes exceed u32 indices", ErrInvalidInput, v)
	}
	if uint64(len(edges)) > math.MaxUint32 {
		return nil, fmt.Errorf("%w: %d edges exceed u32 edge indices", ErrInvalidInput, len(edges))
	}
	if len(nodeTypes) != 0 && len(nodeTypes) != v {
		return nil, fmt.Errorf("%w: %d node types for %d nodes", ErrInvalidInput, len(nodeTypes), v)
	}
	quantise := meta.Flags&FlagConfU8 != 0
	for i, e := range edges {
		if int64(e.Src) >= int64(v) || int64(e.Dst) >= int64(v) {
			return nil, fmt.Errorf("%w: edge %d endpoint out of range", ErrInvalidInput, i)
		}
		if int(e.Label) >= len(labels) {
			return nil, fmt.Errorf("%w: edge %d label %d not in dictionary of %d", ErrInvalidInput, i, e.Label, len(labels))
		}
		if !validConf(e.Conf) {
			return nil, fmt.Errorf("%w: edge %d confidence %v not in [0,1]", ErrInvalidInput, i, e.Conf)
		}
	}

	// Dense id assignment in entity-id order.
	order := make([]uint32, v)
	for i := range order {
		order[i] = uint32(i)
	}
	cmpID := func(a, b uint32) int { return bytes.Compare(nodes[a][:], nodes[b][:]) }
	if !slices.IsSortedFunc(order, cmpID) {
		slices.SortFunc(order, cmpID)
	}
	remap := make([]uint32, v)
	g := &Graph{
		meta:     normaliseMeta(meta),
		labels:   slices.Clone(labels),
		nodeID:   make([]core.UUID, v),
		nodeType: make([]uint8, v),
	}
	for newIdx, old := range order {
		if newIdx > 0 && nodes[old] == g.nodeID[newIdx-1] {
			return nil, fmt.Errorf("%w: duplicate node id %s", ErrInvalidInput, nodes[old])
		}
		remap[old] = uint32(newIdx)
		g.nodeID[newIdx] = nodes[old]
		if len(nodeTypes) != 0 {
			g.nodeType[newIdx] = nodeTypes[old]
		}
	}

	// Counting sort by source: degree, prefix sum, scatter.
	g.offsets = make([]uint64, v+1)
	for _, e := range edges {
		g.offsets[remap[e.Src]+1]++
	}
	for i := 1; i <= v; i++ {
		g.offsets[i] += g.offsets[i-1]
	}
	type rec struct {
		dst   uint32
		label uint8
		conf  float32
	}
	recs := make([]rec, len(edges))
	cursor := slices.Clone(g.offsets[:v])
	for _, e := range edges {
		s := remap[e.Src]
		c := e.Conf
		if c == 0 {
			// Fold -0 into +0: they compare equal in the row sort below
			// but encode to different bits, which would make the file
			// depend on the input order (TestRegress_CSRNegativeZeroConf).
			c = 0
		}
		if quantise {
			c = quantiseConf(c)
		}
		recs[cursor[s]] = rec{dst: remap[e.Dst], label: e.Label, conf: c}
		cursor[s]++
	}
	byKey := func(a, b rec) int {
		if c := cmp.Compare(a.dst, b.dst); c != 0 {
			return c
		}
		if c := cmp.Compare(a.label, b.label); c != 0 {
			return c
		}
		return cmp.Compare(a.conf, b.conf)
	}
	for s := range v {
		row := recs[g.offsets[s]:g.offsets[s+1]]
		if len(row) > 1 {
			slices.SortFunc(row, byKey)
		}
	}
	g.targets = make([]uint32, len(edges))
	g.edgeLabel = make([]uint8, len(edges))
	g.edgeConf = make([]float32, len(edges))
	for i, r := range recs {
		g.targets[i], g.edgeLabel[i], g.edgeConf[i] = r.dst, r.label, r.conf
	}
	g.inOffsets, g.inSources, g.inEdge = transpose(g.offsets, g.targets)
	return g, nil
}

// transpose builds the incoming CSR with the same counting sort, keyed on
// the target. Scanning forward edges in index order makes each in-row
// ascending by source with ties in forward-edge order.
func transpose(offsets []uint64, targets []uint32) (inOff []uint64, inSrc, inEdge []uint32) {
	v := len(offsets) - 1
	inOff = make([]uint64, v+1)
	for _, t := range targets {
		inOff[t+1]++
	}
	for i := 1; i <= v; i++ {
		inOff[i] += inOff[i-1]
	}
	cursor := slices.Clone(inOff[:v])
	inSrc = make([]uint32, len(targets))
	inEdge = make([]uint32, len(targets))
	for s := range v {
		for i := offsets[s]; i < offsets[s+1]; i++ {
			t := targets[i]
			inSrc[cursor[t]] = uint32(s)
			inEdge[cursor[t]] = uint32(i)
			cursor[t]++
		}
	}
	return inOff, inSrc, inEdge
}

func validConf(c float32) bool { return c >= 0 && c <= 1 } // false for NaN

// quantiseConf rounds c to the nearest multiple of 1/255, the value a
// FlagConfU8 snapshot stores.
//
// PORT: math.Round rounds halves away from zero (f64::round does the
// same); the product is taken in float64 after widening the float32.
func quantiseConf(c float32) float32 { return dequantise(quantByte(c)) }

func quantByte(c float32) byte { return byte(math.Round(float64(c) * 255)) }

func dequantise(q byte) float32 { return float32(q) / 255 }

func normaliseMeta(m Meta) Meta {
	m.CreatedAt = time.UnixMilli(m.CreatedAt.UnixMilli()).UTC()
	return m
}

// Builder accumulates nodes and edges by entity id and label name and
// then calls Build. It is a convenience for callers that read entities and
// edges from the system of record; it is not safe for concurrent use.
type Builder struct {
	ids    []core.UUID
	types  []uint8
	index  map[core.UUID]uint32
	labels []string
	label  map[string]uint8
	edges  []Edge
	err    error // sticky error from NewBuilder, reported by Build
}

// NewBuilder returns an empty Builder. labels seeds the label dictionary;
// AddEdge appends unseen labels in first-use order, so the snapshot bytes
// depend on edge insertion order unless every label is seeded here.
func NewBuilder(labels ...string) *Builder {
	b := &Builder{index: map[core.UUID]uint32{}, label: map[string]uint8{}}
	for _, l := range labels {
		if _, err := b.labelIndex(l); err != nil {
			b.err = err
			break
		}
	}
	return b
}

// AddNode registers an entity. Adding an id again keeps the first type.
func (b *Builder) AddNode(id core.UUID, nodeType uint8) {
	if _, ok := b.index[id]; ok {
		return
	}
	b.index[id] = uint32(len(b.ids))
	b.ids = append(b.ids, id)
	b.types = append(b.types, nodeType)
}

// AddEdge adds a directed edge between two registered entities.
func (b *Builder) AddEdge(from, to core.UUID, label string, conf float32) error {
	s, ok := b.index[from]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownNode, from)
	}
	d, ok := b.index[to]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownNode, to)
	}
	if !validConf(conf) {
		return fmt.Errorf("%w: confidence %v not in [0,1]", ErrInvalidInput, conf)
	}
	l, err := b.labelIndex(label)
	if err != nil {
		return err
	}
	b.edges = append(b.edges, Edge{Src: s, Dst: d, Label: l, Conf: conf})
	return nil
}

func (b *Builder) labelIndex(name string) (uint8, error) {
	if i, ok := b.label[name]; ok {
		return i, nil
	}
	if len(b.labels) >= MaxLabels {
		return 0, fmt.Errorf("%w: label dictionary full", ErrInvalidInput)
	}
	i := uint8(len(b.labels))
	b.labels = append(b.labels, name)
	b.label[name] = i
	return i, nil
}

// Build builds the snapshot from everything added so far.
func (b *Builder) Build(meta Meta) (*Graph, error) {
	if b.err != nil {
		return nil, b.err
	}
	return Build(meta, b.ids, b.types, b.edges, b.labels)
}
