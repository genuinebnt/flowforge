package csr

import (
	"fmt"
	"slices"
)

// BlastRadiusLabels returns the ownership and hosting relation labels that
// blast_radius follows (spec 5.9), plus the v2 content labels a page
// depends on (V2 design §3.4): the scripts and styles it loads and the
// documents it embeds.
func BlastRadiusLabels() []string {
	return []string{"dns_record", "port", "certificate", "contains", "owns", "loads_script", "loads_style", "embeds"}
}

// Reach is a node found by a traversal together with its hop distance.
type Reach struct {
	Node     uint32
	Distance int
}

// BlastRadius returns the entities reachable from start within depth hops
// over the blast-radius labels, following edges forward ("from has label
// to"), ranked by distance and then node index; start itself is omitted.
// Labels absent from the snapshot's dictionary are simply never matched.
// depth must be in [0, MaxPointDepth].
func BlastRadius(g *Graph, start uint32, depth int) ([]Reach, error) {
	if depth < 0 || depth > MaxPointDepth {
		return nil, fmt.Errorf("%w: depth %d not in [0,%d]", ErrInvalidInput, depth, MaxPointDepth)
	}
	r, err := BFS(g, start, BFSOptions{MaxDepth: depth, Direction: DirOut, Labels: g.LabelSetOf(BlastRadiusLabels()...)})
	if err != nil {
		return nil, err
	}
	out := make([]Reach, 0, r.Reached()-1)
	for d, level := range r.Levels[1:] {
		for _, v := range level {
			out = append(out, Reach{Node: v, Distance: d + 1})
		}
	}
	return slices.Clip(out), nil
}
