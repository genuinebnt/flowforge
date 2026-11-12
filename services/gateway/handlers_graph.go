package gateway

import (
	"context"
	"sync"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/genuinebnt/flowforge/core"
	graphv1 "github.com/genuinebnt/flowforge/gen/flowforge/graph/v1"
	similarityv1 "github.com/genuinebnt/flowforge/gen/flowforge/similarity/v1"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
)

// programParam returns the program from the path ({pid}) or, for the
// /graph/... forms, the program_id query parameter.
func (c *call) programParam(required bool) (string, error) {
	if v := c.r.PathValue("pid"); v != "" {
		return c.pathID("pid", core.PrefixProgram)
	}
	return bodyID("program_id", c.r.URL.Query().Get("program_id"), core.PrefixProgram, required)
}

func (s *Server) listEntities(c *call) error {
	pid, err := c.programParam(true)
	if err != nil {
		return err
	}
	size, tok, err := c.page()
	if err != nil {
		return err
	}
	seen, err := c.queryTime("seen_after")
	if err != nil {
		return err
	}
	q := c.r.URL.Query()
	types := append(c.queryList("asset_type"), c.queryList("asset_types")...)
	res, err := s.cfg.Backends.Graph.FindEntities(c.ctx, &pb.FindEntitiesRequest{ProgramId: pid, AssetTypes: types,
		ScopeStatus: q.Get("scope_status"), State: q.Get("state"), SeenAfter: pbTime(seen), KeyPrefix: q.Get("key_prefix"),
		Page: &pb.PageRequest{PageSize: size, PageToken: tok}})
	if err != nil {
		return err
	}
	return c.ok(listOf(entities(res.GetEntities()), res.GetPage().GetNextPageToken()))
}

func (s *Server) getEntity(c *call) error {
	id, err := c.pathID("id", core.PrefixEntity)
	if err != nil {
		return err
	}
	tagsToo, err := c.queryBool("include_tags")
	if err != nil {
		return err
	}
	e, err := s.cfg.Backends.Graph.GetEntity(c.ctx, &pb.GetEntityRequest{EntityId: id, IncludeTags: tagsToo})
	if err != nil {
		return err
	}
	return c.ok(entityJSON(e))
}

func (s *Server) neighbors(c *call) error {
	id, err := c.pathID("id", core.PrefixEntity)
	if err != nil {
		return err
	}
	dir, err := parseDirection(c.r.URL.Query().Get("direction"))
	if err != nil {
		return err
	}
	depth, err := c.queryInt("depth", 1, 1, 5)
	if err != nil {
		return err
	}
	maxNodes, err := c.queryInt("max_nodes", 500, 1, 10000)
	if err != nil {
		return err
	}
	since, err := c.queryTime("since")
	if err != nil {
		return err
	}
	g, err := s.cfg.Backends.Graph.Neighbors(c.ctx, &pb.NeighborsRequest{EntityId: id, Labels: c.queryList("labels"), Direction: dir,
		Depth: int32(depth), Since: pbTime(since), MaxNodes: int32(maxNodes)})
	if err != nil {
		return err
	}
	return c.ok(subgraphJSON(g))
}

func (s *Server) search(c *call) error {
	pid, err := c.programParam(true)
	if err != nil {
		return err
	}
	q := c.r.URL.Query().Get("q")
	if q == "" || len(q) > 256 {
		return errValidation("q: required, at most 256 bytes")
	}
	limit, err := c.queryInt("limit", 20, 1, 100)
	if err != nil {
		return err
	}
	fuzzy, err := c.queryBool("fuzzy")
	if err != nil {
		return err
	}
	res, err := s.cfg.Backends.Graph.Search(c.ctx, &pb.SearchRequest{ProgramId: pid, Query: q, AssetTypes: c.queryList("asset_types"),
		Limit: int32(limit), Fuzzy: fuzzy})
	if err != nil {
		return err
	}
	hits := []map[string]any{}
	for _, h := range res.GetHits() {
		hits = append(hits, map[string]any{"score": h.GetScore(), "entity": entityJSON(h.GetEntity()), "matched_trigrams": strs(h.GetMatchedTrigrams())})
	}
	return c.ok(map[string]any{"hits": hits})
}

func (s *Server) paths(c *call) error {
	var in struct {
		FromEntityID string   `json:"from_entity_id"`
		ToEntityID   string   `json:"to_entity_id"`
		MaxCost      float64  `json:"max_cost"`
		Labels       []string `json:"labels"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if _, err := bodyID("from_entity_id", in.FromEntityID, core.PrefixEntity, true); err != nil {
		return err
	}
	if _, err := bodyID("to_entity_id", in.ToEntityID, core.PrefixEntity, true); err != nil {
		return err
	}
	if in.MaxCost < 0 {
		return errValidation("max_cost: must not be negative")
	}
	p, err := s.cfg.Backends.Graph.FindPath(c.ctx, &pb.FindPathRequest{FromEntityId: in.FromEntityID, ToEntityId: in.ToEntityID, MaxCost: in.MaxCost, Labels: in.Labels})
	if err != nil {
		return err
	}
	return c.ok(pathJSON(p))
}

func (s *Server) diff(c *call) error {
	pid, err := c.programParam(true)
	if err != nil {
		return err
	}
	size, tok, err := c.page()
	if err != nil {
		return err
	}
	since, err := c.queryTime("since")
	if err != nil {
		return err
	}
	q := c.r.URL.Query()
	a, err := bodyID("run_id_a", q.Get("run_id_a"), core.PrefixRun, false)
	if err != nil {
		return err
	}
	b, err := bodyID("run_id_b", q.Get("run_id_b"), core.PrefixRun, false)
	if err != nil {
		return err
	}
	req := &pb.DiffRequest{ProgramId: pid, RunIdB: b, Page: &pb.PageRequest{PageSize: size, PageToken: tok}}
	switch {
	case !since.IsZero() && a != "":
		return errValidation("since and run_id_a are mutually exclusive")
	case !since.IsZero():
		req.From = &pb.DiffRequest_Since{Since: timestamppb.New(since)}
	case a != "":
		req.From = &pb.DiffRequest_RunIdA{RunIdA: a}
	default:
		return errValidation("since or run_id_a is required")
	}
	res, err := s.cfg.Backends.Graph.Diff(c.ctx, req)
	if err != nil {
		return err
	}
	items := []map[string]any{}
	for _, ch := range res.GetChanges() {
		items = append(items, changeJSON(ch))
	}
	return c.ok(listOf(items, res.GetPage().GetNextPageToken()))
}

// ---- analytics (21.6) --------------------------------------------------

func (s *Server) analysis(c *call) error {
	var in analysisIn
	if err := c.decode(&in); err != nil {
		return err
	}
	pid := c.r.PathValue("pid")
	if pid != "" {
		var err error
		if pid, err = c.pathID("pid", core.PrefixProgram); err != nil {
			return err
		}
		if in.ProgramID != "" && in.ProgramID != pid {
			return errValidation("program_id: disagrees with the path")
		}
	} else if _, err := bodyID("program_id", in.ProgramID, core.PrefixProgram, true); err != nil {
		return err
	} else {
		pid = in.ProgramID
	}
	req, err := in.proto(c.p.tenant(), pid)
	if err != nil {
		return err
	}
	res, err := s.cfg.Backends.Analytics.RunAnalysis(c.ctx, req)
	if err != nil {
		return err
	}
	return c.ok(analysisJSON(res))
}

func (s *Server) snapshots(c *call) error {
	pid, err := c.programParam(true)
	if err != nil {
		return err
	}
	if _, _, err := c.page(); err != nil {
		return err
	}
	info, err := s.cfg.Backends.Analytics.GetSnapshotInfo(c.ctx, &graphv1.GetSnapshotInfoRequest{TenantId: c.p.tenant(), ProgramId: pid})
	if err != nil {
		return err
	}
	return c.ok(listOf([]map[string]any{snapshotJSON(pid, info)}, ""))
}

// ---- similarity (22.9) -------------------------------------------------

func (s *Server) similarEntity(c *call) error {
	id, err := c.pathID("id", core.PrefixEntity)
	if err != nil {
		return err
	}
	pid, err := c.programParam(false)
	if err != nil {
		return err
	}
	if pid == "" {
		e, err := s.cfg.Backends.Graph.GetEntity(c.ctx, &pb.GetEntityRequest{EntityId: id})
		if err != nil {
			return err
		}
		pid = e.GetProgramId()
	}
	k, err := c.queryInt("k", 10, 1, 100)
	if err != nil {
		return err
	}
	oos, err := c.queryBool("include_out_of_scope")
	if err != nil {
		return err
	}
	res, err := s.cfg.Backends.Similarity.SearchByEntity(c.ctx, &similarityv1.SearchByEntityRequest{
		Scope: &similarityv1.Scope{TenantId: c.p.tenant(), ProgramId: pid}, EntityId: id, ModelId: c.r.URL.Query().Get("model"),
		Params: &similarityv1.SearchParams{K: uint32(k), IncludeOutOfScope: oos},
	})
	if err != nil {
		return err
	}
	return c.ok(s.similarJSON(c.ctx, res))
}

func (s *Server) similarText(c *call) error {
	var in struct {
		ProgramID         string `json:"program_id"`
		Text              string `json:"text"`
		K                 uint32 `json:"k"`
		Mode              string `json:"mode"`
		Model             string `json:"model"`
		IncludeOutOfScope bool   `json:"include_out_of_scope"`
		RRFK              uint32 `json:"rrf_k"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	pid := c.r.PathValue("pid")
	if pid != "" {
		var err error
		if pid, err = c.pathID("pid", core.PrefixProgram); err != nil {
			return err
		}
	} else if _, err := bodyID("program_id", in.ProgramID, core.PrefixProgram, true); err != nil {
		return err
	} else {
		pid = in.ProgramID
	}
	if in.Text == "" || len(in.Text) > 8192 {
		return errValidation("text: required, at most 8192 bytes")
	}
	if in.K == 0 {
		in.K = 10
	}
	if in.K > 100 {
		return errValidation("k: must be from 1 to 100")
	}
	scope := &similarityv1.Scope{TenantId: c.p.tenant(), ProgramId: pid}
	params := &similarityv1.SearchParams{K: in.K, IncludeOutOfScope: in.IncludeOutOfScope}
	var res *similarityv1.SearchResponse
	var err error
	switch in.Mode {
	case "", "vector":
		res, err = s.cfg.Backends.Similarity.SearchByText(c.ctx, &similarityv1.SearchByTextRequest{Scope: scope, Text: in.Text, ModelId: in.Model, Params: params})
	case "hybrid":
		res, err = s.cfg.Backends.Similarity.HybridSearch(c.ctx, &similarityv1.HybridSearchRequest{Scope: scope, Text: in.Text, ModelId: in.Model, Params: params, RrfK: in.RRFK})
	default:
		return errValidation("mode: must be vector or hybrid")
	}
	if err != nil {
		return err
	}
	return c.ok(s.similarJSON(c.ctx, res))
}

// similarJSON renders hits with each entity's asset type (spec 22.9 shows
// "type"; the similarity contract does not carry it, so it is looked up in
// the graph, best effort, eight at a time).
func (s *Server) similarJSON(ctx context.Context, res *similarityv1.SearchResponse) map[string]any {
	hits := make([]map[string]any, len(res.GetHits()))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, h := range res.GetHits() {
		hits[i] = map[string]any{"entity_id": h.GetEntityId(), "type": nil, "score": h.GetScore(), "distance": h.GetDistance(), "in_scope": h.GetInScope()}
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if e, err := s.cfg.Backends.Graph.GetEntity(ctx, &pb.GetEntityRequest{EntityId: id}); err == nil {
				hits[i]["type"] = e.GetAssetType()
			}
		}(i, h.GetEntityId())
	}
	wg.Wait()
	return map[string]any{"index_watermark_seq": res.GetIndexWatermarkSeq(), "segments_searched": res.GetSegmentsSearched(), "hits": hits}
}

func (s *Server) similarityIndex(c *call) error {
	pid, err := c.pathID("pid", core.PrefixProgram)
	if err != nil {
		return err
	}
	info, err := s.cfg.Backends.Similarity.GetIndexInfo(c.ctx, &similarityv1.GetIndexInfoRequest{
		Scope: &similarityv1.Scope{TenantId: c.p.tenant(), ProgramId: pid}, ModelId: c.r.URL.Query().Get("model"),
	})
	if err != nil {
		return err
	}
	return c.ok(indexInfoJSON(info))
}
