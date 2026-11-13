package oam

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// CoveringRun reports that a tool completed a run that covered the same
// input as its earlier runs (spec 5.7; SetStaleRequest in spec 11.6).
type CoveringRun struct {
	TenantID  string
	ProgramID string
	RunID     string
	Tool      string
	// AssetTypes limits which entity types the run covers; empty means all.
	// Edges reported by the tool are always covered.
	AssetTypes []string
	// StaleAfterRuns overrides the graph default when positive.
	StaleAfterRuns int
	// CompletedAt timestamps the resulting change events.
	CompletedAt time.Time
}

// StaleResult reports the effect of CompleteRun (SetStaleResponse).
type StaleResult struct {
	Duplicate      bool
	EntitiesMarked int
	EdgesMarked    int
	Changes        []ChangeEvent
}

// CompleteRun applies a covering run. For every entity and edge the tool
// has reported before, the run either reset the tool's miss counter (the
// record was reported in this run) or extends it by one. A record becomes
// stale once every tool that has covered it has missed it for at least
// StaleAfterRuns consecutive covering runs; sources that never ran a
// covering run do not keep it alive. A later sighting reactivates it.
// Nothing is deleted. Completing the same run twice has no effect.
func (g *Graph) CompleteRun(r CoveringRun) (StaleResult, error) {
	if strings.TrimSpace(r.Tool) == "" || strings.TrimSpace(r.RunID) == "" {
		return StaleResult{}, fmt.Errorf("%w: tool and run id are required", ErrInvalidRun)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rk := strings.Join([]string{r.TenantID, r.ProgramID, r.Tool, r.RunID}, "\x00")
	if _, ok := g.runs[rk]; ok {
		return StaleResult{Duplicate: true}, nil
	}
	g.runs[rk] = struct{}{}
	n := r.StaleAfterRuns
	if n <= 0 {
		n = g.staleAfter
	}
	em := &emitter{g: g, tenant: r.TenantID, prog: r.ProgramID, origin: "run:" + r.Tool + ":" + r.RunID, run: r.RunID, at: r.CompletedAt.UTC()}
	var res StaleResult
	for _, id := range slices.Sorted(maps.Keys(g.entities)) {
		e := g.entities[id]
		if e.tenant != r.TenantID || e.program != r.ProgramID {
			continue
		}
		if len(r.AssetTypes) > 0 && !slices.Contains(r.AssetTypes, e.assetType) {
			continue
		}
		if e.cover(r.Tool, r.RunID, n) {
			res.EntitiesMarked++
			em.emit(ChangeEntityStale, id, "", nil)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(g.edges)) {
		e := g.edges[id]
		if e.tenant != r.TenantID || e.program != r.ProgramID {
			continue
		}
		if e.cover(r.Tool, r.RunID, n) {
			res.EdgesMarked++
			em.emit(ChangeEdgeRemoved, "", id, nil)
		}
	}
	res.Changes = em.out
	return res, nil
}

// cover applies one covering run of tool to r and reports whether r's
// state changed (it can only become stale). The classic support becomes
// stale once every tool that covered r has missed it n times; r stays
// active while it has authoritative support.
func (r *record) cover(tool, run string, n int) bool {
	lr, ok := r.lastRun[tool]
	if !ok {
		return false
	}
	if lr.run == run {
		r.missed[tool] = 0
	} else {
		r.missed[tool]++
	}
	if !r.classicStale {
		stale := true
		for _, m := range r.missed {
			if m < n {
				stale = false
				break
			}
		}
		r.classicStale = stale
	}
	return r.settle()
}
