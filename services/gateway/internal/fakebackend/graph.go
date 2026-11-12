package fakebackend

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/genuinebnt/flowforge/core"
	graphv1 "github.com/genuinebnt/flowforge/gen/flowforge/graph/v1"
	simv1 "github.com/genuinebnt/flowforge/gen/flowforge/similarity/v1"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
)

// seedGraph gives every new program a small graph: api.acme.com ->
// 203.0.113.10, jenkins.acme.com, and three change events. b.mu is held.
func (b *Backend) seedGraph(tenant, program string) {
	mk := func(typ, key string, content map[string]any) *pb.Entity {
		st, _ := structpb.NewStruct(content)
		e := &pb.Entity{Id: b.id(core.PrefixEntity), ProgramId: program, AssetType: typ, Key: key, Content: st, ScopeStatus: "in_scope",
			Confidence: 0.93, State: "active", CreatedAt: b.now(), LastSeen: b.now()}
		b.entities[e.Id] = &entity{tenant: tenant, e: e}
		return e
	}
	api := mk("FQDN", "fqdn:api.acme.com", map[string]any{"name": "api.acme.com"})
	ip := mk("IPAddress", "ip:203.0.113.10", map[string]any{"address": "203.0.113.10", "ip_version": "IPv4"})
	jen := mk("FQDN", "fqdn:jenkins.acme.com", map[string]any{"name": "jenkins.acme.com"})
	edge := &pb.Edge{Id: b.id(core.PrefixEdge), RelationType: "BasicDNSRelation", Label: "dns_record", FromEntityId: api.Id, ToEntityId: ip.Id,
		Confidence: 0.97, State: "active", CreatedAt: b.now(), LastSeen: b.now()}
	b.edgesByP[program] = append(b.edgesByP[program], edge)
	for _, e := range []*pb.Entity{api, ip, jen} {
		b.addChange(program, &pb.ChangeEvent{Type: "entity_added", EntityId: e.Id, Entity: e})
	}
}

// addChange appends a change event; b.mu is held.
func (b *Backend) addChange(program string, c *pb.ChangeEvent) {
	b.seq++
	c.Id, c.Seq, c.ProgramId, c.At = b.id(core.PrefixChange), b.seq, program, b.now()
	b.changes[program] = append(b.changes[program], c)
	b.cond.Broadcast()
}

// EmitChange adds a change event to a program (tests drive watch streams).
func (b *Backend) EmitChange(program, typ string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.addChange(program, &pb.ChangeEvent{Type: typ})
	return b.seq
}

type graphSrv struct {
	pb.UnimplementedGraphServiceServer
	b *Backend
}

func (g graphSrv) entityOf(ctx context.Context, id string) (*pb.Entity, error) {
	e, ok := g.b.entities[id]
	if !ok || e.tenant != from(ctx).tenant {
		return nil, notFound()
	}
	return e.e, nil
}

func (g graphSrv) GetEntity(ctx context.Context, r *pb.GetEntityRequest) (*pb.Entity, error) {
	g.b.mu.Lock()
	defer g.b.mu.Unlock()
	return g.entityOf(ctx, r.GetEntityId())
}

func (g graphSrv) programEntities(ctx context.Context, program string) ([]*pb.Entity, error) {
	if _, err := g.b.programOf(ctx, program); err != nil {
		return nil, err
	}
	var out []*pb.Entity
	for _, e := range g.b.entities {
		if e.e.ProgramId == program {
			out = append(out, e.e)
		}
	}
	sortBy(out, func(e *pb.Entity) string { return e.Id })
	return out, nil
}

func (g graphSrv) FindEntities(ctx context.Context, r *pb.FindEntitiesRequest) (*pb.FindEntitiesResponse, error) {
	g.b.mu.Lock()
	defer g.b.mu.Unlock()
	all, err := g.programEntities(ctx, r.GetProgramId())
	if err != nil {
		return nil, err
	}
	var sel []*pb.Entity
	for _, e := range all {
		if (len(r.GetAssetTypes()) == 0 || contains(r.GetAssetTypes(), e.AssetType)) && strings.HasPrefix(e.Key, r.GetKeyPrefix()) {
			sel = append(sel, e)
		}
	}
	items, pg, err := page(sel, r.GetPage())
	if err != nil {
		return nil, err
	}
	return &pb.FindEntitiesResponse{Entities: items, Page: pg}, nil
}

func (g graphSrv) Neighbors(ctx context.Context, r *pb.NeighborsRequest) (*pb.Subgraph, error) {
	g.b.mu.Lock()
	defer g.b.mu.Unlock()
	e, err := g.entityOf(ctx, r.GetEntityId())
	if err != nil {
		return nil, err
	}
	out := &pb.Subgraph{Entities: []*pb.Entity{e}}
	for _, ed := range g.b.edgesByP[e.ProgramId] {
		if len(r.GetLabels()) > 0 && !contains(r.GetLabels(), ed.Label) {
			continue
		}
		var other string
		switch {
		case ed.FromEntityId == e.Id && r.GetDirection() != pb.Direction_DIRECTION_IN:
			other = ed.ToEntityId
		case ed.ToEntityId == e.Id && r.GetDirection() != pb.Direction_DIRECTION_OUT:
			other = ed.FromEntityId
		default:
			continue
		}
		out.Edges = append(out.Edges, ed)
		out.Entities = append(out.Entities, g.b.entities[other].e)
	}
	return out, nil
}

func (g graphSrv) FindPath(ctx context.Context, r *pb.FindPathRequest) (*pb.PathResponse, error) {
	g.b.mu.Lock()
	defer g.b.mu.Unlock()
	from, err := g.entityOf(ctx, r.GetFromEntityId())
	if err != nil {
		return nil, err
	}
	if _, err := g.entityOf(ctx, r.GetToEntityId()); err != nil {
		return nil, err
	}
	for _, ed := range g.b.edgesByP[from.ProgramId] {
		if ed.FromEntityId == r.GetFromEntityId() && ed.ToEntityId == r.GetToEntityId() {
			return &pb.PathResponse{EntityIds: []string{ed.FromEntityId, ed.ToEntityId}, Edges: []*pb.Edge{ed}, TotalCost: 1, CombinedConfidence: ed.Confidence}, nil
		}
	}
	return &pb.PathResponse{}, nil
}

func (g graphSrv) Diff(ctx context.Context, r *pb.DiffRequest) (*pb.DiffResponse, error) {
	g.b.mu.Lock()
	defer g.b.mu.Unlock()
	if _, err := g.b.programOf(ctx, r.GetProgramId()); err != nil {
		return nil, err
	}
	var sel []*pb.ChangeEvent
	for _, c := range g.b.changes[r.GetProgramId()] {
		if s := r.GetSince(); s == nil || !c.At.AsTime().Before(s.AsTime()) {
			sel = append(sel, c)
		}
	}
	items, pg, err := page(sel, r.GetPage())
	if err != nil {
		return nil, err
	}
	return &pb.DiffResponse{Changes: items, Page: pg}, nil
}

func (g graphSrv) Search(ctx context.Context, r *pb.SearchRequest) (*pb.SearchResponse, error) {
	g.b.mu.Lock()
	defer g.b.mu.Unlock()
	all, err := g.programEntities(ctx, r.GetProgramId())
	if err != nil {
		return nil, err
	}
	out := &pb.SearchResponse{}
	for _, e := range all {
		if strings.Contains(e.Key, strings.ToLower(r.GetQuery())) && len(out.Hits) < int(r.GetLimit()) {
			out.Hits = append(out.Hits, &pb.SearchHit{Entity: e, Score: 0.78})
		}
	}
	return out, nil
}

func (g graphSrv) WatchChanges(r *pb.WatchChangesRequest, st grpc.ServerStreamingServer[pb.ChangeEvent]) error {
	b := g.b
	ctx := st.Context()
	stop := context.AfterFunc(ctx, func() { b.mu.Lock(); b.cond.Broadcast(); b.mu.Unlock() })
	defer stop()
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.programOf(ctx, r.GetProgramId()); err != nil {
		return err
	}
	next := r.GetAfterSeq()
	for {
		for _, c := range b.changes[r.GetProgramId()] {
			if c.Seq > next && (len(r.GetTypes()) == 0 || contains(r.GetTypes(), c.Type)) {
				b.mu.Unlock()
				err := st.Send(c)
				b.mu.Lock()
				if err != nil {
					return err
				}
				next = c.Seq
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.cond.Wait()
	}
}

// ---- analytics and similarity -----------------------------------------

type analyticsSrv struct {
	graphv1.UnimplementedGraphAnalyticsServer
	b *Backend
}

func (a analyticsSrv) RunAnalysis(ctx context.Context, r *graphv1.RunAnalysisRequest) (*graphv1.RunAnalysisResponse, error) {
	if r.GetTenantId() != from(ctx).tenant {
		return nil, fail(codes.PermissionDenied, core.ReasonForbiddenRole, "tenant_id disagrees with the token")
	}
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	ents, err := graphSrv{b: a.b}.programEntities(ctx, r.GetProgramId())
	if err != nil {
		return nil, err
	}
	res := &graphv1.RunAnalysisResponse{WatermarkSeq: uint64(a.b.seq), NodeCount: uint64(len(ents)), EdgeCount: uint64(len(a.b.edgesByP[r.GetProgramId()]))}
	switch r.GetAlgorithm().(type) {
	case *graphv1.RunAnalysisRequest_Pagerank:
		var top []*graphv1.Score
		for i, e := range ents {
			top = append(top, &graphv1.Score{EntityId: e.Id, Score: 1 / float64(i+2)})
		}
		res.Result = &graphv1.RunAnalysisResponse_Scores{Scores: &graphv1.ScoreResult{TopN: top, Iterations: 12, Converged: true}}
	case *graphv1.RunAnalysisRequest_Bfs:
		res.Result = &graphv1.RunAnalysisResponse_Levels{Levels: &graphv1.LevelResult{Levels: []*graphv1.Level{{Depth: 0, EntityIds: []string{r.GetBfs().GetStartEntityId()}}}}}
	default:
		var ids []string
		for _, e := range ents {
			ids = append(ids, e.Id)
		}
		res.Result = &graphv1.RunAnalysisResponse_Groups{Groups: &graphv1.GroupResult{Groups: []*graphv1.Group{{Id: 1, EntityIds: ids}}}}
	}
	return res, nil
}

func (a analyticsSrv) GetSnapshotInfo(ctx context.Context, r *graphv1.GetSnapshotInfoRequest) (*graphv1.SnapshotInfo, error) {
	if r.GetTenantId() != from(ctx).tenant {
		return nil, fail(codes.PermissionDenied, core.ReasonForbiddenRole, "tenant_id disagrees with the token")
	}
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	if _, err := a.b.programOf(ctx, r.GetProgramId()); err != nil {
		return nil, err
	}
	return &graphv1.SnapshotInfo{WatermarkSeq: uint64(a.b.seq), NodeCount: 3, EdgeCount: 1, BuiltAt: a.b.now(), Sha256: strings.Repeat("c", 64)}, nil
}

type simSrv struct {
	simv1.UnimplementedSimilarityServer
	b *Backend
}

func (s simSrv) search(ctx context.Context, sc *simv1.Scope, p *simv1.SearchParams, exclude string) (*simv1.SearchResponse, error) {
	if sc.GetTenantId() != from(ctx).tenant {
		return nil, fail(codes.PermissionDenied, core.ReasonForbiddenRole, "scope.tenant_id disagrees with the token")
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	ents, err := graphSrv{b: s.b}.programEntities(ctx, sc.GetProgramId())
	if err != nil {
		return nil, err
	}
	out := &simv1.SearchResponse{IndexWatermarkSeq: uint64(s.b.seq), SegmentsSearched: 1}
	for i, e := range ents {
		if e.Id != exclude && len(out.Hits) < int(p.GetK()) {
			out.Hits = append(out.Hits, &simv1.Hit{EntityId: e.Id, Distance: float32(i) / 10, Score: 1 / float32(i+30), InScope: true})
		}
	}
	return out, nil
}

func (s simSrv) SearchByEntity(ctx context.Context, r *simv1.SearchByEntityRequest) (*simv1.SearchResponse, error) {
	return s.search(ctx, r.GetScope(), r.GetParams(), r.GetEntityId())
}

func (s simSrv) SearchByText(ctx context.Context, r *simv1.SearchByTextRequest) (*simv1.SearchResponse, error) {
	return s.search(ctx, r.GetScope(), r.GetParams(), "")
}

func (s simSrv) HybridSearch(ctx context.Context, r *simv1.HybridSearchRequest) (*simv1.SearchResponse, error) {
	return s.search(ctx, r.GetScope(), r.GetParams(), "")
}

func (s simSrv) GetIndexInfo(ctx context.Context, r *simv1.GetIndexInfoRequest) (*simv1.IndexInfo, error) {
	if r.GetScope().GetTenantId() != from(ctx).tenant {
		return nil, fail(codes.PermissionDenied, core.ReasonForbiddenRole, "scope.tenant_id disagrees with the token")
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if _, err := s.b.programOf(ctx, r.GetScope().GetProgramId()); err != nil {
		return nil, err
	}
	return &simv1.IndexInfo{ModelId: "text-embedding-005", ModelVersion: "1", Dimension: 768, Metric: simv1.Metric_METRIC_COSINE, Vectors: 3,
		Segments: 1, WatermarkSeq: uint64(s.b.seq)}, nil
}

// ---- notifier ----------------------------------------------------------

type notifierSrv struct {
	pb.UnimplementedNotifierServiceServer
	b *Backend
}

func (n notifierSrv) PutRule(ctx context.Context, r *pb.PutRuleRequest) (*pb.AlertRule, error) {
	if !strings.HasPrefix(strings.TrimSpace(r.GetSource()), "alert ") {
		st := fail(codes.InvalidArgument, core.ReasonDSLDiagnostics, "alert rule does not compile")
		return nil, st
	}
	b := n.b
	b.mu.Lock()
	defer b.mu.Unlock()
	t := from(ctx).tenant
	if r.GetId() != "" {
		cur, ok := b.rules[r.GetId()]
		if !ok || cur.tenant != t {
			return nil, notFound()
		}
		cur.r = &pb.AlertRule{Id: cur.r.Id, ProgramId: r.GetProgramId(), Name: r.GetName(), Source: r.GetSource(), Enabled: r.GetEnabled(), Version: cur.r.Version + 1}
		return cur.r, nil
	}
	ar := &pb.AlertRule{Id: b.id(core.PrefixAlertRule), ProgramId: r.GetProgramId(), Name: r.GetName(), Source: r.GetSource(), Enabled: r.GetEnabled(), Version: 1}
	b.rules[ar.Id] = &rule{tenant: t, r: ar}
	return ar, nil
}

func (n notifierSrv) GetRule(ctx context.Context, r *pb.GetRuleRequest) (*pb.AlertRule, error) {
	n.b.mu.Lock()
	defer n.b.mu.Unlock()
	cur, ok := n.b.rules[r.GetId()]
	if !ok || cur.tenant != from(ctx).tenant {
		return nil, notFound()
	}
	return cur.r, nil
}

func (n notifierSrv) ListRules(ctx context.Context, r *pb.ListRulesRequest) (*pb.ListRulesResponse, error) {
	n.b.mu.Lock()
	defer n.b.mu.Unlock()
	var all []*pb.AlertRule
	for _, x := range n.b.rules {
		if x.tenant == from(ctx).tenant {
			all = append(all, x.r)
		}
	}
	sortBy(all, func(x *pb.AlertRule) string { return x.Id })
	items, pg, err := page(all, r.GetPage())
	if err != nil {
		return nil, err
	}
	return &pb.ListRulesResponse{Rules: items, Page: pg}, nil
}

func (n notifierSrv) DeleteRule(ctx context.Context, r *pb.DeleteRuleRequest) (*pb.DeleteRuleResponse, error) {
	n.b.mu.Lock()
	defer n.b.mu.Unlock()
	cur, ok := n.b.rules[r.GetId()]
	if !ok || cur.tenant != from(ctx).tenant {
		return nil, notFound()
	}
	delete(n.b.rules, r.GetId())
	return &pb.DeleteRuleResponse{}, nil
}

func (n notifierSrv) TestRule(_ context.Context, r *pb.TestRuleRequest) (*pb.TestRuleResponse, error) {
	if !strings.HasPrefix(strings.TrimSpace(r.GetSource()), "alert ") {
		return &pb.TestRuleResponse{Diagnostics: []*pb.Diagnostic{{Code: "E0101", Severity: pb.Diagnostic_SEVERITY_ERROR, Message: "expected 'alert'"}}}, nil
	}
	matched := r.GetChange() != nil && strings.Contains(r.GetSource(), r.GetChange().GetType())
	var payload *structpb.Struct
	if matched {
		payload, _ = structpb.NewStruct(map[string]any{"text": "matched " + r.GetChange().GetType()})
	}
	return &pb.TestRuleResponse{Matched: matched, RenderedPayload: payload}, nil
}

func (n notifierSrv) PutChannel(ctx context.Context, r *pb.PutChannelRequest) (*pb.Channel, error) {
	b := n.b
	b.mu.Lock()
	defer b.mu.Unlock()
	t := from(ctx).tenant
	id := r.GetId()
	if id != "" {
		cur, ok := b.channels[id]
		if !ok || cur.tenant != t {
			return nil, notFound()
		}
	} else {
		id = b.id(core.PrefixChannel)
	}
	c := &pb.Channel{Id: id, Kind: r.GetKind(), Name: r.GetName(), Config: r.GetConfig(), Enabled: r.GetEnabled()}
	b.channels[id] = &channel{tenant: t, c: c}
	return c, nil
}

func (n notifierSrv) ListChannels(ctx context.Context, _ *pb.ListChannelsRequest) (*pb.ListChannelsResponse, error) {
	n.b.mu.Lock()
	defer n.b.mu.Unlock()
	var all []*pb.Channel
	for _, c := range n.b.channels {
		if c.tenant == from(ctx).tenant {
			all = append(all, c.c)
		}
	}
	sortBy(all, func(c *pb.Channel) string { return c.Id })
	return &pb.ListChannelsResponse{Channels: all}, nil
}

func (n notifierSrv) ListAlertEvents(ctx context.Context, r *pb.ListAlertEventsRequest) (*pb.ListAlertEventsResponse, error) {
	_, pg, err := page([]*pb.AlertEvent{}, r.GetPage())
	if err != nil {
		return nil, err
	}
	return &pb.ListAlertEventsResponse{Page: pg}, nil
}
