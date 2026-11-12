package csr

import (
	"cmp"
	"fmt"
	"slices"
	"sync"

	"github.com/genuinebnt/flowforge/core"
)

// ChangeKind is the kind of a graph change event the overlay understands.
type ChangeKind uint8

// Change kinds (a subset of the spec 5.7 change events: the ones that
// alter the snapshot's topology).
const (
	ChangeNodeAdded ChangeKind = iota + 1
	ChangeEdgeAdded
	ChangeEdgeRemoved
)

// Change is one graph.change_events row as the overlay sees it.
type Change struct {
	Seq  uint64
	Kind ChangeKind
	// Node and NodeType describe ChangeNodeAdded.
	Node     core.UUID
	NodeType uint8
	// From, To, Label and Conf describe edge changes. ChangeEdgeRemoved
	// removes every edge From -> To with Label (Conf is ignored).
	From, To core.UUID
	Label    string
	Conf     float32
}

type edgeKey struct {
	src, dst uint32
	label    uint8
}

// Overlay is the in-memory delta over a snapshot for change events with
// seq above its watermark (spec 21.4). Point queries consult snapshot plus
// overlay; whole-graph analytics use the snapshot alone.
//
// Node indices below the snapshot's V are the snapshot's; nodes added by
// the overlay follow in insertion order. Labels unknown to the snapshot
// are appended to a copy of its dictionary.
//
// The effective graph is the multiset of snapshot edges whose
// (source, target, label) was not removed, plus the overlay's added edges
// that were not removed later. An Overlay is safe for concurrent use.
type Overlay struct {
	mu       sync.RWMutex
	base     *Graph
	labels   []string
	labelIdx map[string]uint8
	newIDs   []core.UUID
	newTypes []uint8
	newIdx   map[core.UUID]uint32
	addedOut map[uint32][]Edge
	addedIn  map[uint32][]Edge
	removed  map[edgeKey]struct{}
	log      []Change
	lastSeq  uint64
}

// NewOverlay returns an empty overlay over base.
func NewOverlay(base *Graph) *Overlay {
	o := &Overlay{
		base:     base,
		labels:   slices.Clone(base.labels),
		labelIdx: make(map[string]uint8, len(base.labels)),
		newIdx:   map[core.UUID]uint32{},
		addedOut: map[uint32][]Edge{},
		addedIn:  map[uint32][]Edge{},
		removed:  map[edgeKey]struct{}{},
		lastSeq:  base.meta.WatermarkSeq,
	}
	for i, l := range base.labels {
		o.labelIdx[l] = uint8(i)
	}
	return o
}

// Base returns the snapshot under the overlay.
func (o *Overlay) Base() *Graph { return o.base }

// Watermark returns the seq of the last change applied (the snapshot's
// watermark when none was).
func (o *Overlay) Watermark() uint64 {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.lastSeq
}

// Size returns the number of change events applied, the measure the
// compaction policy compares with the snapshot's edge count.
func (o *Overlay) Size() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.log)
}

// Changes returns a copy of the applied change events in seq order.
func (o *Overlay) Changes() []Change {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return slices.Clone(o.log)
}

// Apply records one change event. Events at or below the snapshot's
// watermark are already reflected and are skipped (false, nil). Events
// must otherwise arrive in increasing seq order (ErrSeqOrder). An edge
// event naming an unknown entity fails with ErrUnknownNode and is not
// recorded.
func (o *Overlay) Apply(c Change) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if c.Seq <= o.base.meta.WatermarkSeq {
		return false, nil
	}
	if c.Seq <= o.lastSeq {
		return false, fmt.Errorf("%w: seq %d after %d", ErrSeqOrder, c.Seq, o.lastSeq)
	}
	switch c.Kind {
	case ChangeNodeAdded:
		if _, ok := o.index(c.Node); !ok {
			if int64(o.numNodes()) >= 1<<32-1 {
				return false, fmt.Errorf("%w: node index space full", ErrInvalidInput)
			}
			o.newIdx[c.Node] = uint32(o.numNodes())
			o.newIDs = append(o.newIDs, c.Node)
			o.newTypes = append(o.newTypes, c.NodeType)
		}
	case ChangeEdgeAdded, ChangeEdgeRemoved:
		s, ok := o.index(c.From)
		if !ok {
			return false, fmt.Errorf("%w: %s", ErrUnknownNode, c.From)
		}
		d, ok := o.index(c.To)
		if !ok {
			return false, fmt.Errorf("%w: %s", ErrUnknownNode, c.To)
		}
		if c.Kind == ChangeEdgeAdded {
			if !validConf(c.Conf) {
				return false, fmt.Errorf("%w: confidence %v not in [0,1]", ErrInvalidInput, c.Conf)
			}
			l, ok := o.labelIdx[c.Label]
			if !ok {
				if len(o.labels) >= MaxLabels {
					return false, fmt.Errorf("%w: label dictionary full", ErrInvalidInput)
				}
				l = uint8(len(o.labels))
				o.labels = append(o.labels, c.Label)
				o.labelIdx[c.Label] = l
			}
			conf := c.Conf
			if o.base.meta.Flags&FlagConfU8 != 0 {
				conf = quantiseConf(conf)
			}
			e := Edge{Src: s, Dst: d, Label: l, Conf: conf}
			o.addedOut[s] = append(o.addedOut[s], e)
			o.addedIn[d] = append(o.addedIn[d], e)
		} else if l, ok := o.labelIdx[c.Label]; ok {
			k := edgeKey{s, d, l}
			o.removed[k] = struct{}{}
			drop := func(e Edge) bool { return e.Src == s && e.Dst == d && e.Label == l }
			o.addedOut[s] = slices.DeleteFunc(o.addedOut[s], drop)
			o.addedIn[d] = slices.DeleteFunc(o.addedIn[d], drop)
		}
	default:
		return false, fmt.Errorf("%w: change kind %d", ErrInvalidInput, c.Kind)
	}
	o.lastSeq = c.Seq
	o.log = append(o.log, c)
	return true, nil
}

// Rebase returns a new overlay over g holding the events of o that g does
// not reflect yet (seq above g's watermark). It is the replay step after a
// compaction swap.
func (o *Overlay) Rebase(g *Graph) (*Overlay, error) {
	n := NewOverlay(g)
	for _, c := range o.Changes() {
		if _, err := n.Apply(c); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (o *Overlay) numNodes() int { return len(o.base.nodeID) + len(o.newIDs) }

// NumNodes returns the node count of snapshot plus overlay.
func (o *Overlay) NumNodes() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.numNodes()
}

func (o *Overlay) index(id core.UUID) (uint32, bool) {
	if v, ok := o.base.Index(id); ok {
		return v, true
	}
	v, ok := o.newIdx[id]
	return v, ok
}

// Index returns the overlay-space index of an entity.
func (o *Overlay) Index(id core.UUID) (uint32, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.index(id)
}

// NodeID returns the entity id of overlay-space node v.
func (o *Overlay) NodeID(v uint32) (core.UUID, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if int64(v) >= int64(o.numNodes()) {
		return core.Nil, ErrNodeRange
	}
	if o.base.has(v) {
		return o.base.nodeID[v], nil
	}
	return o.newIDs[int(v)-len(o.base.nodeID)], nil
}

// Labels returns a copy of the overlay's label dictionary (the snapshot's
// followed by labels first seen in change events).
func (o *Overlay) Labels() []string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return slices.Clone(o.labels)
}

// LabelSetOf is Graph.LabelSetOf over the overlay's dictionary.
func (o *Overlay) LabelSetOf(names ...string) LabelSet {
	o.mu.RLock()
	defer o.mu.RUnlock()
	s := LabelSet{restricted: true}
	for _, n := range names {
		if i, ok := o.labelIdx[n]; ok {
			s.add(i)
		}
	}
	return s
}

func edgeOrder(byDst bool) func(a, b Edge) int {
	return func(a, b Edge) int {
		x, y := a.Src, b.Src
		if byDst {
			x, y = a.Dst, b.Dst
		}
		if c := cmp.Compare(x, y); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Label, b.Label); c != 0 {
			return c
		}
		return cmp.Compare(a.Conf, b.Conf)
	}
}

func (o *Overlay) outEdges(v uint32) []Edge {
	g := o.base
	var out []Edge
	if g.has(v) {
		for i := g.offsets[v]; i < g.offsets[v+1]; i++ {
			e := Edge{Src: v, Dst: g.targets[i], Label: g.edgeLabel[i], Conf: g.edgeConf[i]}
			if _, gone := o.removed[edgeKey{e.Src, e.Dst, e.Label}]; !gone {
				out = append(out, e)
			}
		}
	}
	out = append(out, o.addedOut[v]...)
	slices.SortFunc(out, edgeOrder(true))
	return out
}

func (o *Overlay) inEdges(v uint32) []Edge {
	g := o.base
	var out []Edge
	if g.has(v) {
		for j := g.inOffsets[v]; j < g.inOffsets[v+1]; j++ {
			i := g.inEdge[j]
			e := Edge{Src: g.inSources[j], Dst: v, Label: g.edgeLabel[i], Conf: g.edgeConf[i]}
			if _, gone := o.removed[edgeKey{e.Src, e.Dst, e.Label}]; !gone {
				out = append(out, e)
			}
		}
	}
	out = append(out, o.addedIn[v]...)
	slices.SortFunc(out, edgeOrder(false))
	return out
}

// OutEdges returns the effective out-edges of v sorted by
// (target, label, confidence), the order a rebuilt snapshot would have.
func (o *Overlay) OutEdges(v uint32) ([]Edge, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if int64(v) >= int64(o.numNodes()) {
		return nil, ErrNodeRange
	}
	return o.outEdges(v), nil
}

// InEdges returns the effective in-edges of v sorted by
// (source, label, confidence).
func (o *Overlay) InEdges(v uint32) ([]Edge, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if int64(v) >= int64(o.numNodes()) {
		return nil, ErrNodeRange
	}
	return o.inEdges(v), nil
}

// BFS is the bounded breadth-first search of the package-level BFS over
// snapshot plus overlay. It returns the levels only (overlay edges have no
// forward edge index). Intended for point queries with small depth.
func (o *Overlay) BFS(start uint32, opts BFSOptions) ([][]uint32, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if int64(start) >= int64(o.numNodes()) {
		return nil, fmt.Errorf("%w: start %d", ErrNodeRange, start)
	}
	if !opts.Direction.valid() {
		return nil, fmt.Errorf("%w: direction %s", ErrInvalidInput, opts.Direction)
	}
	seen := map[uint32]struct{}{start: {}}
	levels := [][]uint32{{start}}
	for depth := 0; opts.MaxDepth < 0 || depth < opts.MaxDepth; depth++ {
		var next []uint32
		visit := func(w uint32) {
			if _, ok := seen[w]; !ok {
				seen[w] = struct{}{}
				next = append(next, w)
			}
		}
		for _, v := range levels[depth] {
			if opts.Direction != DirIn {
				for _, e := range o.outEdges(v) {
					if opts.Labels.Has(e.Label) {
						visit(e.Dst)
					}
				}
			}
			if opts.Direction != DirOut {
				for _, e := range o.inEdges(v) {
					if opts.Labels.Has(e.Label) {
						visit(e.Src)
					}
				}
			}
		}
		if len(next) == 0 {
			break
		}
		slices.Sort(next)
		levels = append(levels, next)
	}
	return levels, nil
}

// Materialize builds a new snapshot of snapshot plus overlay. meta is used
// as given; callers normally set WatermarkSeq to Watermark().
func (o *Overlay) Materialize(meta Meta) (*Graph, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	g := o.base
	ids := append(slices.Clone(g.nodeID), o.newIDs...)
	types := append(slices.Clone(g.nodeType), o.newTypes...)
	var edges []Edge
	for v := range uint32(o.numNodes()) {
		edges = append(edges, o.outEdges(v)...)
	}
	return Build(meta, ids, types, edges, o.labels)
}
