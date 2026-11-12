package csr

import (
	"bytes"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Header flags (spec 21.1, 21.4).
const (
	// FlagConfU8 marks a snapshot whose edge_conf section stores
	// confidences quantised to a byte (value q means q/255). Without it the
	// section stores little-endian float32.
	FlagConfU8 uint16 = 1 << 0
	// FlagStaleExcluded records that stale and ageing entities (spec 5.7)
	// were excluded when the snapshot was built.
	FlagStaleExcluded uint16 = 1 << 1

	knownFlags = FlagConfU8 | FlagStaleExcluded
)

// MaxLabels is the size of the label dictionary address space: edge labels
// are stored as one byte.
const MaxLabels = 256

// Meta is the snapshot header information that is not derived from the
// graph content.
type Meta struct {
	TenantID  core.UUID
	ProgramID core.UUID
	// WatermarkSeq is the graph.change_events seq the snapshot reflects.
	WatermarkSeq uint64
	// CreatedAt is stored with millisecond precision; Build truncates it.
	CreatedAt time.Time
	// Flags is a combination of the Flag constants.
	Flags uint16
}

// Edge is one directed edge given to Build. Src and Dst index the node
// slice passed alongside; Label indexes the label dictionary.
type Edge struct {
	Src, Dst uint32
	Label    uint8
	// Conf is the edge confidence in [0, 1].
	Conf float32
}

// Graph is an immutable CSR snapshot of one (tenant, program) graph with
// its transpose (spec 21.1). Nodes are dense indices 0..V-1 ordered by
// entity id; forward edges are numbered 0..E-1 in row order.
//
// A Graph is safe for concurrent use by any number of readers. A Graph
// returned by Open may be backed by a read-only memory mapping: slices
// returned by its accessors alias that mapping and must not be used after
// Close.
type Graph struct {
	meta   Meta
	labels []string

	offsets   []uint64 // V+1
	targets   []uint32 // E
	edgeLabel []uint8  // E
	edgeConf  []float32
	nodeType  []uint8     // V
	nodeID    []core.UUID // V, strictly ascending

	inOffsets []uint64 // V+1
	inSources []uint32 // E
	inEdge    []uint32 // E, forward edge index

	undirOnce sync.Once
	undir     *undirected

	closeOnce sync.Once
	release   func() error
	closeErr  error
}

// Meta returns the snapshot header information.
func (g *Graph) Meta() Meta { return g.meta }

// NumNodes returns V.
func (g *Graph) NumNodes() int { return len(g.nodeID) }

// NumEdges returns E.
func (g *Graph) NumEdges() int { return len(g.targets) }

// Labels returns a copy of the label dictionary.
func (g *Graph) Labels() []string { return slices.Clone(g.labels) }

// LabelIndex returns the dictionary index of name.
func (g *Graph) LabelIndex(name string) (uint8, bool) {
	for i, l := range g.labels {
		if l == name {
			return uint8(i), true
		}
	}
	return 0, false
}

// LabelSetOf returns a restricted LabelSet holding the named labels that
// exist in the dictionary. Unknown names are ignored, so a set built only
// from unknown names matches no edge at all.
func (g *Graph) LabelSetOf(names ...string) LabelSet {
	s := LabelSet{restricted: true}
	for _, n := range names {
		if i, ok := g.LabelIndex(n); ok {
			s.add(i)
		}
	}
	return s
}

// NodeID returns the entity id of node v.
func (g *Graph) NodeID(v uint32) (core.UUID, error) {
	if int64(v) >= int64(len(g.nodeID)) {
		return core.Nil, ErrNodeRange
	}
	return g.nodeID[v], nil
}

// NodeType returns the asset type code of node v.
func (g *Graph) NodeType(v uint32) (uint8, error) {
	if int64(v) >= int64(len(g.nodeType)) {
		return 0, ErrNodeRange
	}
	return g.nodeType[v], nil
}

// Index returns the dense index of entity id by binary search over the
// sorted node id table.
func (g *Graph) Index(id core.UUID) (uint32, bool) {
	i, ok := slices.BinarySearchFunc(g.nodeID, id, func(a, b core.UUID) int {
		return bytes.Compare(a[:], b[:])
	})
	return uint32(i), ok
}

func (g *Graph) has(v uint32) bool { return int64(v) < int64(len(g.nodeID)) }

// OutRange returns the half-open range of forward edge indices leaving v.
func (g *Graph) OutRange(v uint32) (lo, hi int, err error) {
	if !g.has(v) {
		return 0, 0, ErrNodeRange
	}
	return int(g.offsets[v]), int(g.offsets[v+1]), nil
}

// OutNeighbors returns the targets of v's out-edges, ascending, with one
// entry per edge. The slice aliases the snapshot and must not be modified.
func (g *Graph) OutNeighbors(v uint32) ([]uint32, error) {
	if !g.has(v) {
		return nil, ErrNodeRange
	}
	return g.targets[g.offsets[v]:g.offsets[v+1]:g.offsets[v+1]], nil
}

// InNeighbors returns the sources of v's in-edges, ascending, with one
// entry per edge. The slice aliases the snapshot and must not be modified.
func (g *Graph) InNeighbors(v uint32) ([]uint32, error) {
	if !g.has(v) {
		return nil, ErrNodeRange
	}
	return g.inSources[g.inOffsets[v]:g.inOffsets[v+1]:g.inOffsets[v+1]], nil
}

// InEdges returns the forward edge indices of v's in-edges, parallel to
// InNeighbors. The slice aliases the snapshot and must not be modified.
func (g *Graph) InEdges(v uint32) ([]uint32, error) {
	if !g.has(v) {
		return nil, ErrNodeRange
	}
	return g.inEdge[g.inOffsets[v]:g.inOffsets[v+1]:g.inOffsets[v+1]], nil
}

// EdgeAt returns forward edge i. Finding its source is a binary search
// over the offsets.
func (g *Graph) EdgeAt(i int) (Edge, error) {
	if i < 0 || i >= len(g.targets) {
		return Edge{}, ErrInvalidInput
	}
	return Edge{Src: g.edgeSource(uint64(i)), Dst: g.targets[i], Label: g.edgeLabel[i], Conf: g.edgeConf[i]}, nil
}

// edgeSource returns the row holding edge i (i < E).
func (g *Graph) edgeSource(i uint64) uint32 {
	v := sort.Search(len(g.nodeID), func(v int) bool { return g.offsets[v+1] > i })
	return uint32(v)
}

// Offsets returns a copy of the forward offsets array (length V+1).
func (g *Graph) Offsets() []uint64 { return slices.Clone(g.offsets) }

// Targets returns a copy of the forward targets array.
func (g *Graph) Targets() []uint32 { return slices.Clone(g.targets) }

// EdgeLabels returns a copy of the per-edge label indices.
func (g *Graph) EdgeLabels() []uint8 { return slices.Clone(g.edgeLabel) }

// EdgeConfs returns a copy of the per-edge confidences.
func (g *Graph) EdgeConfs() []float32 { return slices.Clone(g.edgeConf) }

// NodeTypes returns a copy of the per-node type codes.
func (g *Graph) NodeTypes() []uint8 { return slices.Clone(g.nodeType) }

// NodeIDs returns a copy of the node id table, ascending.
func (g *Graph) NodeIDs() []core.UUID { return slices.Clone(g.nodeID) }

// InOffsets returns a copy of the transposed offsets array.
func (g *Graph) InOffsets() []uint64 { return slices.Clone(g.inOffsets) }

// InSources returns a copy of the transposed sources array.
func (g *Graph) InSources() []uint32 { return slices.Clone(g.inSources) }

// InEdgeIndex returns a copy of the in_edge array: for each transposed
// slot, the index of the forward edge it mirrors.
func (g *Graph) InEdgeIndex() []uint32 { return slices.Clone(g.inEdge) }

// Edges returns every forward edge in index order.
func (g *Graph) Edges() []Edge {
	out := make([]Edge, 0, len(g.targets))
	for v := range len(g.nodeID) {
		for i := g.offsets[v]; i < g.offsets[v+1]; i++ {
			out = append(out, Edge{Src: uint32(v), Dst: g.targets[i], Label: g.edgeLabel[i], Conf: g.edgeConf[i]})
		}
	}
	return out
}

// Close releases the memory mapping behind a Graph returned by Open. It is
// a no-op for graphs from Build or Decode and is idempotent. After Close
// the Graph and every slice obtained from it must no longer be used.
func (g *Graph) Close() error {
	g.closeOnce.Do(func() {
		if g.release != nil {
			g.closeErr = g.release()
		}
	})
	return g.closeErr
}
