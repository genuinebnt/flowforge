package graph

import (
	"context"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/csr"
	graphv1 "github.com/genuinebnt/flowforge/gen/flowforge/graph/v1"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// SnapshotMaxAge is how long a built snapshot is reused when the request
// does not need a newer watermark.
const SnapshotMaxAge = 30 * time.Second

// Analytics implements flowforge.graph.v1.GraphAnalytics over CSR snapshots
// (spec 21.6). Tenant and program come from the token; the request's
// tenant_id is cross-checked against it.
type Analytics struct {
	graphv1.UnimplementedGraphAnalyticsServer
	Store graphstore.Store
	Clock core.Clock
	// Blobs, when set, receives a snapshot file (spec 21.4,
	// snapshots/{tenant}/{program}/{watermark}.ffcs) and a graph.snapshots
	// catalogue row each time a snapshot at a new watermark is built, so
	// GetSnapshotInfo reports its gcs_uri (G-OPS-14).
	Blobs graphstore.BlobStore
	// OnWriteError observes a failed snapshot write (default: ignored;
	// the in-memory snapshot still serves the request).
	OnWriteError func(ctx context.Context, program string, err error)

	mu      sync.Mutex
	snaps   map[[2]string]snapEntry
	written map[[2]string]uint64
}

type snapEntry struct {
	g         *csr.Graph
	watermark uint64
	builtAt   time.Time
}

// NewAnalytics returns the analytics service.
func NewAnalytics(store graphstore.Store, clock core.Clock) *Analytics {
	if clock == nil {
		clock = &core.SystemClock{}
	}
	return &Analytics{Store: store, Clock: clock, snaps: map[[2]string]snapEntry{}, written: map[[2]string]uint64{}}
}

func (a *Analytics) scope(ctx context.Context, tenantID, programID string) (graphstore.Caller, string, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return graphstore.Caller{}, "", err
	}
	if tenantID != "" {
		t, err := core.ParsePublicID(core.PrefixTenant, tenantID)
		if err != nil || t != id.Tenant {
			// Never reveal another tenant's existence (spec 11.8).
			return graphstore.Caller{}, "", core.Errorf(core.KindNotFound, core.ReasonNotFound, "program not found")
		}
	}
	p, err := convert.ParseID(core.PrefixProgram, "program_id", programID)
	if err != nil {
		return graphstore.Caller{}, "", err
	}
	return graphstore.Caller{TenantID: id.TenantPublicID()}, core.PublicID(core.PrefixProgram, p), nil
}

func (a *Analytics) snapshot(ctx context.Context, c graphstore.Caller, program string, min uint64) (snapEntry, error) {
	k := [2]string{c.TenantID, program}
	a.mu.Lock()
	e, ok := a.snaps[k]
	a.mu.Unlock()
	if ok && e.watermark >= min && a.Clock.Now().Sub(e.builtAt) < SnapshotMaxAge {
		return e, nil
	}
	g, wm, err := a.Store.BuildSnapshot(ctx, c, program)
	if err != nil {
		return snapEntry{}, err
	}
	e = snapEntry{g: g, watermark: wm, builtAt: a.Clock.Now()}
	a.mu.Lock()
	a.snaps[k] = e
	_, seen := a.written[k]
	fresh := a.Blobs != nil && (!seen || wm > a.written[k])
	if fresh {
		a.written[k] = wm
	}
	a.mu.Unlock()
	if fresh {
		// The Store port records catalogue rows only through
		// WriteSnapshot, which builds its own copy (one extra build per new
		// watermark); a row at an existing watermark is kept, not doubled.
		if _, err := a.Store.WriteSnapshot(ctx, c, program, a.Blobs); err != nil {
			a.mu.Lock()
			delete(a.written, k)
			a.mu.Unlock()
			if a.OnWriteError != nil {
				a.OnWriteError(ctx, program, err)
			}
		}
	}
	if wm < min {
		return e, core.Errorf(core.KindFailedPrecondition, core.ReasonValidationFailed,
			"snapshot watermark %d is older than min_watermark_seq %d", wm, min)
	}
	return e, nil
}

func node(g *csr.Graph, field, entityID string) (uint32, error) {
	u, err := convert.ParseID(core.PrefixEntity, field, entityID)
	if err != nil {
		return 0, err
	}
	v, ok := g.Index(u)
	if !ok {
		return 0, core.Errorf(core.KindNotFound, core.ReasonNotFound, "entity %s is not in the snapshot", field)
	}
	return v, nil
}

func ids(g *csr.Graph, vs []uint32) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		if u, err := g.NodeID(v); err == nil {
			out = append(out, core.PublicID(core.PrefixEntity, u))
		}
	}
	return out
}

func labels(g *csr.Graph, names []string) csr.LabelSet {
	if len(names) == 0 {
		return csr.AnyLabel()
	}
	return g.LabelSetOf(graphstore.NormalizeLabels(names)...)
}

func groups(g *csr.Graph, p csr.Partition) *graphv1.GroupResult {
	out := &graphv1.GroupResult{}
	for i := 0; i < p.Count(); i++ {
		out.Groups = append(out.Groups, &graphv1.Group{Id: uint64(i), EntityIds: ids(g, p.Group(i))})
	}
	return out
}

// RunAnalysis implements GraphAnalytics.
func (a *Analytics) RunAnalysis(ctx context.Context, req *graphv1.RunAnalysisRequest) (*graphv1.RunAnalysisResponse, error) {
	c, program, err := a.scope(ctx, req.GetTenantId(), req.GetProgramId())
	if err != nil {
		return nil, err
	}
	e, err := a.snapshot(ctx, c, program, req.GetMinWatermarkSeq())
	if err != nil {
		return nil, err
	}
	g := e.g
	out := &graphv1.RunAnalysisResponse{WatermarkSeq: e.watermark, NodeCount: uint64(g.NumNodes()), EdgeCount: uint64(g.NumEdges())}
	switch alg := req.GetAlgorithm().(type) {
	case *graphv1.RunAnalysisRequest_Bfs:
		start, err := node(g, "bfs.start_entity_id", alg.Bfs.GetStartEntityId())
		if err != nil {
			return nil, err
		}
		dir := csr.Direction(alg.Bfs.GetDirection())
		if dir == csr.DirUnspecified {
			dir = csr.DirOut
		}
		depth := int(alg.Bfs.GetMaxDepth())
		if depth == 0 {
			depth = -1
		}
		r, err := csr.BFS(g, start, csr.BFSOptions{MaxDepth: depth, Direction: dir, Labels: labels(g, alg.Bfs.GetLabels())})
		if err != nil {
			return nil, convert.Invalid("bfs", "%v", err)
		}
		lr := &graphv1.LevelResult{}
		for d, lv := range r.Levels {
			lr.Levels = append(lr.Levels, &graphv1.Level{Depth: uint32(d), EntityIds: ids(g, lv)})
		}
		out.Result = &graphv1.RunAnalysisResponse_Levels{Levels: lr}
	case *graphv1.RunAnalysisRequest_Shortest:
		from, err := node(g, "shortest.from_entity_id", alg.Shortest.GetFromEntityId())
		if err != nil {
			return nil, err
		}
		to, err := node(g, "shortest.to_entity_id", alg.Shortest.GetToEntityId())
		if err != nil {
			return nil, err
		}
		p, ok, err := csr.ShortestPath(g, from, to, csr.PathOptions{Labels: labels(g, alg.Shortest.GetLabels())})
		if err != nil {
			return nil, convert.Invalid("shortest", "%v", err)
		}
		pr := &graphv1.PathResult{}
		if ok {
			pr.EntityIds, pr.Confidence = ids(g, p.Nodes), p.Confidence
			pr.EdgeIds = a.edgeIDs(ctx, c, pr.EntityIds, alg.Shortest.GetLabels())
		}
		out.Result = &graphv1.RunAnalysisResponse_Path{Path: pr}
	case *graphv1.RunAnalysisRequest_Pagerank:
		r, err := csr.PageRank(g, csr.PageRankOptions{Damping: alg.Pagerank.GetDamping(), Tolerance: alg.Pagerank.GetTolerance(),
			MaxIterations: int(alg.Pagerank.GetMaxIterations())})
		if err != nil {
			return nil, convert.Invalid("pagerank", "%v", err)
		}
		n := int(alg.Pagerank.GetTopN())
		if n <= 0 || n > 1000 {
			n = 20
		}
		order := make([]uint32, len(r.Scores))
		for i := range order {
			order[i] = uint32(i)
		}
		sort.SliceStable(order, func(i, j int) bool { return r.Scores[order[i]] > r.Scores[order[j]] })
		sr := &graphv1.ScoreResult{Iterations: uint32(r.Iterations), Converged: r.Converged}
		for _, v := range order[:min(n, len(order))] {
			sr.TopN = append(sr.TopN, &graphv1.Score{EntityId: ids(g, []uint32{v})[0], Score: r.Scores[v]})
		}
		out.Result = &graphv1.RunAnalysisResponse_Scores{Scores: sr}
	case *graphv1.RunAnalysisRequest_Components:
		if alg.Components.GetStrong() {
			out.Result = &graphv1.RunAnalysisResponse_Groups{Groups: groups(g, csr.StronglyConnectedComponents(g))}
		} else {
			out.Result = &graphv1.RunAnalysisResponse_Groups{Groups: groups(g, csr.WeaklyConnectedComponents(g))}
		}
	case *graphv1.RunAnalysisRequest_Scc:
		out.Result = &graphv1.RunAnalysisResponse_Groups{Groups: groups(g, csr.StronglyConnectedComponents(g))}
	case *graphv1.RunAnalysisRequest_Kcore:
		out.Result = &graphv1.RunAnalysisResponse_Groups{Groups: &graphv1.GroupResult{Groups: []*graphv1.Group{
			{Id: uint64(alg.Kcore.GetK()), EntityIds: ids(g, csr.KCore(g, alg.Kcore.GetK()))}}}}
	default:
		return nil, convert.Invalid("algorithm", "an algorithm is required")
	}
	return out, nil
}

// edgeIDs resolves the graph edge ids along a path (the snapshot keeps no
// edge ids): for each hop, the most confident active edge between the two
// entities, restricted to labels.
func (a *Analytics) edgeIDs(ctx context.Context, c graphstore.Caller, path []string, labels []string) []string {
	var out []string
	for i := 0; i+1 < len(path); i++ {
		sub, err := a.Store.Neighbors(ctx, c, graphstore.NeighborsRequest{EntityID: path[i], Labels: labels, Direction: graphstore.DirectionOut, Depth: 1})
		if err != nil {
			return nil
		}
		best, conf := "", -1.0
		for _, e := range sub.Edges {
			if e.FromEntityID == path[i] && e.ToEntityID == path[i+1] && e.Confidence > conf {
				best, conf = e.ID, e.Confidence
			}
		}
		out = append(out, best)
	}
	return out
}

// GetSnapshotInfo implements GraphAnalytics: the catalogue's latest entry,
// or the in-memory snapshot (no gcs_uri) when none was written.
func (a *Analytics) GetSnapshotInfo(ctx context.Context, req *graphv1.GetSnapshotInfoRequest) (*graphv1.SnapshotInfo, error) {
	c, program, err := a.scope(ctx, req.GetTenantId(), req.GetProgramId())
	if err != nil {
		return nil, err
	}
	info, err := a.Store.LatestSnapshot(ctx, c, program)
	if err == nil {
		return &graphv1.SnapshotInfo{WatermarkSeq: info.WatermarkSeq, NodeCount: uint64(info.NodeCount), EdgeCount: uint64(info.EdgeCount),
			GcsUri: info.URI, BuiltAt: convert.TS(info.BuiltAt), Sha256: hex.EncodeToString(info.SHA256[:])}, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	e, err := a.snapshot(ctx, c, program, 0)
	if err != nil {
		return nil, err
	}
	d, _ := e.g.Digest()
	return &graphv1.SnapshotInfo{WatermarkSeq: e.watermark, NodeCount: uint64(e.g.NumNodes()), EdgeCount: uint64(e.g.NumEdges()),
		BuiltAt: convert.TS(e.builtAt), Sha256: hex.EncodeToString(d[:])}, nil
}
