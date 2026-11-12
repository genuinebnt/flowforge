package graphstore

import (
	"cmp"
	"container/heap"
	"context"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/csr"
	"github.com/genuinebnt/flowforge/oam"
)

// Reader is the port the traversal algorithms run on: one program of one
// tenant, loaded a frontier at a time. Mem and the Postgres store
// implement it; the algorithms (and therefore the results) are shared.
type Reader interface {
	// Entities returns the program's entities among ids, in any state;
	// unknown ids are absent from the map. Tags are not needed.
	Entities(ctx context.Context, ids []string) (map[string]Entity, error)
	// Edges returns the program's edges, in any state, leaving ids (out)
	// and/or entering ids (in), restricted to labels when non-empty. Each
	// edge appears once.
	Edges(ctx context.Context, ids []string, out, in bool, labels []string) ([]Edge, error)
}

// Traversals skip stale entities and edges: staleness hides records
// (spec 23.5) without deleting them.
func liveEntity(e Entity, since time.Time) bool {
	return e.State == oam.StateActive && (since.IsZero() || !e.LastSeen.Before(since))
}

func liveEdge(e Edge, since time.Time) bool {
	return e.State == oam.StateActive && (since.IsZero() || !e.LastSeen.Before(since))
}

// NormalizeLabels lower-cases, trims and de-duplicates relation labels.
func NormalizeLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	slices.Sort(out)
	return out
}

// ValidateNeighbors applies the NeighborsRequest defaults and bounds.
func ValidateNeighbors(req NeighborsRequest) (NeighborsRequest, error) {
	if req.Direction < DirectionOut || req.Direction > DirectionBoth {
		return req, InvalidArgument("direction", "direction must be out, in or both")
	}
	switch {
	case req.Depth == 0:
		req.Depth = 1
	case req.Depth < 0 || req.Depth > MaxDepth:
		return req, InvalidArgument("depth", "depth %d not in 1..%d", req.Depth, MaxDepth)
	}
	switch {
	case req.MaxNodes == 0:
		req.MaxNodes = DefaultMaxNodes
	case req.MaxNodes < 0 || req.MaxNodes > MaxMaxNodes:
		return req, InvalidArgument("max_nodes", "max_nodes %d not in 1..%d", req.MaxNodes, MaxMaxNodes)
	}
	req.Labels = NormalizeLabels(req.Labels)
	if !req.Since.IsZero() {
		req.Since = StoreTime(req.Since)
	}
	return req, nil
}

// TraverseNeighbors runs the breadth-first neighbourhood query of spec 5.9
// from start (which is always returned first, whatever its state). req
// must have passed ValidateNeighbors. Each entity and edge is returned at
// most once; an edge is included when it was followed from a node above
// the depth limit and both its endpoints are in the result. Entities are
// ordered by (distance, id), edges by id. When MaxNodes cuts the result,
// Truncated is set.
func TraverseNeighbors(ctx context.Context, r Reader, start Entity, req NeighborsRequest) (Subgraph, error) {
	res, _, err := bfs(ctx, r, start, req)
	return res, err
}

func bfs(ctx context.Context, r Reader, start Entity, req NeighborsRequest) (Subgraph, map[string]int, error) {
	out := req.Direction == DirectionOut || req.Direction == DirectionBoth
	in := req.Direction == DirectionIn || req.Direction == DirectionBoth
	depth := map[string]int{start.ID: 0}
	res := Subgraph{Entities: []Entity{start}}
	seenEdge := map[string]bool{}
	frontier := []string{start.ID}
	for d := 1; d <= req.Depth && len(frontier) > 0; d++ {
		if err := ctx.Err(); err != nil {
			return Subgraph{}, nil, Classify(err)
		}
		edges, err := r.Edges(ctx, frontier, out, in, req.Labels)
		if err != nil {
			return Subgraph{}, nil, Internal("load edges", err)
		}
		slices.SortFunc(edges, func(a, b Edge) int { return cmp.Compare(a.ID, b.ID) })
		inFrontier := make(map[string]bool, len(frontier))
		for _, id := range frontier {
			inFrontier[id] = true
		}
		others := func(e Edge) []string {
			var o []string
			if out && inFrontier[e.FromEntityID] {
				o = append(o, e.ToEntityID)
			}
			if in && inFrontier[e.ToEntityID] {
				o = append(o, e.FromEntityID)
			}
			return o
		}
		var want []string
		for _, e := range edges {
			if !liveEdge(e, req.Since) || seenEdge[e.ID] {
				continue
			}
			for _, o := range others(e) {
				if _, ok := depth[o]; !ok && !slices.Contains(want, o) {
					want = append(want, o)
				}
			}
		}
		loaded, err := r.Entities(ctx, want)
		if err != nil {
			return Subgraph{}, nil, Internal("load entities", err)
		}
		levelStart := len(res.Entities)
		var next []string
		for _, e := range edges {
			if !liveEdge(e, req.Since) || seenEdge[e.ID] {
				continue
			}
			include := false
			for _, o := range others(e) {
				if _, ok := depth[o]; ok {
					include = true
					continue
				}
				ent, ok := loaded[o]
				if !ok || !liveEntity(ent, req.Since) {
					continue
				}
				if len(res.Entities) >= req.MaxNodes {
					res.Truncated = true
					continue
				}
				depth[o] = d
				res.Entities = append(res.Entities, ent)
				next = append(next, o)
				include = true
			}
			if include {
				seenEdge[e.ID] = true
				res.Edges = append(res.Edges, e)
			}
		}
		slices.SortFunc(res.Entities[levelStart:], func(a, b Entity) int { return cmp.Compare(a.ID, b.ID) })
		slices.Sort(next)
		frontier = next
	}
	slices.SortFunc(res.Edges, func(a, b Edge) int { return cmp.Compare(a.ID, b.ID) })
	return res, depth, nil
}

// ValidateBlast applies the BlastRadiusRequest defaults and bounds.
func ValidateBlast(req BlastRadiusRequest) (BlastRadiusRequest, error) {
	switch {
	case req.Depth == 0:
		req.Depth = DefaultBlastDepth
	case req.Depth < 0 || req.Depth > MaxDepth:
		return req, InvalidArgument("depth", "depth %d not in 1..%d", req.Depth, MaxDepth)
	}
	return req, nil
}

// TraverseBlastRadius returns the active entities reachable from start
// within depth hops over csr.BlastRadiusLabels, following edges forward,
// ranked by distance and then id; start itself is omitted (spec 5.9).
func TraverseBlastRadius(ctx context.Context, r Reader, start Entity, req BlastRadiusRequest) ([]Reach, error) {
	sub, depth, err := bfs(ctx, r, start, NeighborsRequest{
		Labels: csr.BlastRadiusLabels(), Direction: DirectionOut, Depth: req.Depth, MaxNodes: math.MaxInt,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Reach, 0, len(sub.Entities)-1)
	for _, e := range sub.Entities[1:] {
		out = append(out, Reach{Entity: e, Distance: depth[e.ID]})
	}
	return out, nil
}

// ValidatePath checks a FindPathRequest's bounds and normalizes labels.
func ValidatePath(req FindPathRequest) (FindPathRequest, error) {
	if math.IsNaN(req.MaxCost) || math.IsInf(req.MaxCost, 0) || req.MaxCost < 0 {
		return req, InvalidArgument("max_cost", "max_cost must be a non-negative number")
	}
	req.Labels = NormalizeLabels(req.Labels)
	return req, nil
}

// EdgeCost is the Dijkstra weight of an edge, -ln(confidence).
func EdgeCost(confidence float64) float64 { return -math.Log(confidence) }

// TraversePath finds the most likely directed path from one entity to
// another (spec 5.9): Dijkstra with edge cost -ln(confidence) over active
// edges (confidence 0 is never used) between active entities, optionally
// restricted to labels; paths costlier than MaxCost (when positive) are
// discarded. Ties are broken by (cost, entity id) settle order and edge id.
// No path yields an empty Path. More than MaxPathNodes settled entities is
// RESOURCE_EXHAUSTED.
func TraversePath(ctx context.Context, r Reader, from, to Entity, req FindPathRequest) (Path, error) {
	if from.ID == to.ID {
		return Path{EntityIDs: []string{from.ID}, CombinedConfidence: 1}, nil
	}
	if from.State != oam.StateActive || to.State != oam.StateActive {
		return Path{}, nil
	}
	dist := map[string]float64{from.ID: 0}
	prev := map[string]Edge{}
	live := map[string]bool{from.ID: true, to.ID: true}
	settled := map[string]bool{}
	pq := &pathHeap{{cost: 0, id: from.ID}}
	for pq.Len() > 0 {
		it := heap.Pop(pq).(pathItem)
		if settled[it.id] {
			continue
		}
		settled[it.id] = true
		if it.id == to.ID {
			break
		}
		if len(settled) > MaxPathNodes {
			return Path{}, core.Errorf(core.KindResourceExhausted, ReasonTraversalLimit, "path search settled more than %d entities", MaxPathNodes)
		}
		if err := ctx.Err(); err != nil {
			return Path{}, Classify(err)
		}
		edges, err := r.Edges(ctx, []string{it.id}, true, false, req.Labels)
		if err != nil {
			return Path{}, Internal("load edges", err)
		}
		slices.SortFunc(edges, func(a, b Edge) int { return cmp.Compare(a.ID, b.ID) })
		var want []string
		for _, e := range edges {
			if _, known := live[e.ToEntityID]; !known && !slices.Contains(want, e.ToEntityID) {
				want = append(want, e.ToEntityID)
			}
		}
		if len(want) > 0 {
			ents, err := r.Entities(ctx, want)
			if err != nil {
				return Path{}, Internal("load entities", err)
			}
			for _, id := range want {
				e, ok := ents[id]
				live[id] = ok && e.State == oam.StateActive
			}
		}
		for _, e := range edges {
			v := e.ToEntityID
			if !liveEdge(e, time.Time{}) || e.Confidence <= 0 || !live[v] || settled[v] {
				continue
			}
			nc := it.cost + EdgeCost(e.Confidence)
			if req.MaxCost > 0 && nc > req.MaxCost {
				continue
			}
			if old, ok := dist[v]; ok && nc >= old {
				continue
			}
			dist[v], prev[v] = nc, e
			heap.Push(pq, pathItem{cost: nc, id: v})
		}
	}
	if !settled[to.ID] {
		return Path{}, nil
	}
	var edges []Edge
	for v := to.ID; v != from.ID; {
		e := prev[v]
		edges = append(edges, e)
		v = e.FromEntityID
	}
	slices.Reverse(edges)
	p := Path{EntityIDs: []string{from.ID}, Edges: edges, CombinedConfidence: 1}
	for _, e := range edges {
		p.EntityIDs = append(p.EntityIDs, e.ToEntityID)
		p.TotalCost += EdgeCost(e.Confidence)
		p.CombinedConfidence *= e.Confidence
	}
	return p, nil
}

type pathItem struct {
	cost float64
	id   string
}

type pathHeap []pathItem

func (h pathHeap) Len() int { return len(h) }
func (h pathHeap) Less(i, j int) bool {
	return h[i].cost < h[j].cost || (h[i].cost == h[j].cost && h[i].id < h[j].id)
}
func (h pathHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *pathHeap) Push(x any)   { *h = append(*h, x.(pathItem)) }
func (h *pathHeap) Pop() any {
	old := *h
	it := old[len(old)-1]
	*h = old[:len(old)-1]
	return it
}
