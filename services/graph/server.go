// Package graph is the graph service (spec 3.1, 5, 11.6, 21.6):
// GraphService over a graphstore.Store (Postgres or in memory), the
// GraphAnalytics service over CSR snapshots built by Store.BuildSnapshot,
// and the publication of graph.changes (the change_events outbox relay in
// Postgres, direct publication in memory).
package graph

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Options configure a Server.
type Options struct {
	Store graphstore.Store
	// Bus, when set together with PublishInline, receives every change of a
	// write as graph.changes right after the write (memory stores have no
	// outbox). With Postgres the change_events relay publishes instead.
	Bus           bus.Bus
	PublishInline bool
	Logger        *slog.Logger
	Metrics       *platform.Metrics
	Clock         core.Clock
}

// Server implements flowforge.v1.GraphService.
type Server struct {
	pb.UnimplementedGraphServiceServer
	o Options
}

// NewServer returns a graph service.
func NewServer(o Options) *Server {
	if o.Logger == nil {
		o.Logger = platform.DiscardLogger()
	}
	if o.Metrics == nil {
		o.Metrics = platform.NewMetrics(nil)
	}
	if o.Clock == nil {
		o.Clock = &core.SystemClock{}
	}
	return &Server{o: o}
}

func caller(ctx context.Context) (graphstore.Caller, platform.Identity, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return graphstore.Caller{}, id, err
	}
	return graphstore.Caller{TenantID: id.TenantPublicID()}, id, nil
}

func (s *Server) publish(ctx context.Context, changes []graphstore.ChangeEvent) {
	if !s.o.PublishInline || s.o.Bus == nil {
		return
	}
	tp := ""
	if t, ok := platform.TraceFrom(ctx); ok {
		tp = t.String()
	}
	for _, c := range changes {
		if err := s.o.Bus.Publish(ctx, convert.TopicGraphChanges, convert.ChangeEnvelope(c, tp)); err != nil {
			s.o.Logger.ErrorContext(ctx, "graph.publish_failed", "change_id", c.ID, "error", err.Error())
		}
	}
}

// IngestObservations implements GraphService (idempotent per observation id).
func (s *Server) IngestObservations(ctx context.Context, req *pb.IngestRequest) (*pb.IngestResponse, error) {
	_, id, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	obs, err := convert.ObservationDomain(req.GetObservation(), req.GetProgramId(), req.GetRunId(), req.GetToolName(), req.GetToolVersion())
	if err != nil {
		return nil, err
	}
	res, err := s.o.Store.IngestObservation(ctx, graphstore.IngestRequest{TenantID: id.TenantPublicID(), ProgramID: req.GetProgramId(),
		RunID: req.GetRunId(), EffectPath: req.GetEffectPath(), ToolName: req.GetToolName(), ToolVersion: req.GetToolVersion(), Observation: obs})
	if errors.Is(err, oam.ErrPendingLimit) {
		s.o.Metrics.GraphPending.Inc("rejected")
	}
	if err != nil {
		return nil, err
	}
	s.o.Metrics.GraphIngest.Since(start)
	if res.Pending {
		s.o.Metrics.GraphPending.Inc("buffered")
	}
	s.o.Metrics.GraphPending.Add(float64(len(res.Released)), "released")
	s.o.Metrics.GraphPending.Add(float64(res.Expired), "expired")
	s.publish(ctx, res.Changes)
	out := &pb.IngestResponse{EntitiesCreated: int32(res.EntitiesCreated), EntitiesUpdated: int32(res.EntitiesUpdated),
		EdgesCreated: int32(res.EdgesCreated), EdgesUpdated: int32(res.EdgesUpdated), RefToEntityId: res.RefToEntityID}
	for _, c := range res.Changes {
		out.Changes = append(out.Changes, convert.ChangeProto(c))
	}
	return out, nil
}

// GetEntity implements GraphService.
func (s *Server) GetEntity(ctx context.Context, req *pb.GetEntityRequest) (*pb.Entity, error) {
	c, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.o.Store.GetEntity(ctx, c, graphstore.GetEntityRequest{EntityID: req.GetEntityId(), IncludeTags: req.GetIncludeTags()})
	if err != nil {
		return nil, err
	}
	return convert.EntityProto(e), nil
}

// FindEntities implements GraphService.
func (s *Server) FindEntities(ctx context.Context, req *pb.FindEntitiesRequest) (*pb.FindEntitiesResponse, error) {
	c, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	res, err := s.o.Store.FindEntities(ctx, c, graphstore.FindEntitiesRequest{ProgramID: req.GetProgramId(), AssetTypes: req.GetAssetTypes(),
		ScopeStatus: oamStatus(req.GetScopeStatus()), State: oamState(req.GetState()), SeenAfter: convert.Time(req.GetSeenAfter()),
		KeyPrefix: req.GetKeyPrefix(), Page: page(req.GetPage())})
	if err != nil {
		return nil, err
	}
	out := &pb.FindEntitiesResponse{Page: convert.PageResp(res.NextPageToken)}
	for _, e := range res.Entities {
		out.Entities = append(out.Entities, convert.EntityProto(e))
	}
	return out, nil
}

func page(p *pb.PageRequest) graphstore.PageRequest {
	return graphstore.PageRequest{PageSize: int(p.GetPageSize()), PageToken: p.GetPageToken()}
}

func subgraphProto(g graphstore.Subgraph) *pb.Subgraph {
	out := &pb.Subgraph{Truncated: g.Truncated}
	for _, e := range g.Entities {
		out.Entities = append(out.Entities, convert.EntityProto(e))
	}
	for _, e := range g.Edges {
		out.Edges = append(out.Edges, convert.EdgeProto(e))
	}
	return out
}

// Neighbors implements GraphService.
func (s *Server) Neighbors(ctx context.Context, req *pb.NeighborsRequest) (*pb.Subgraph, error) {
	c, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	g, err := s.o.Store.Neighbors(ctx, c, graphstore.NeighborsRequest{EntityID: req.GetEntityId(), Labels: req.GetLabels(),
		Direction: graphstore.Direction(req.GetDirection()), Depth: int(req.GetDepth()), Since: convert.Time(req.GetSince()), MaxNodes: int(req.GetMaxNodes())})
	if err != nil {
		return nil, err
	}
	return subgraphProto(g), nil
}

// FindPath implements GraphService.
func (s *Server) FindPath(ctx context.Context, req *pb.FindPathRequest) (*pb.PathResponse, error) {
	c, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.o.Store.FindPath(ctx, c, graphstore.FindPathRequest{FromEntityID: req.GetFromEntityId(), ToEntityID: req.GetToEntityId(),
		MaxCost: req.GetMaxCost(), Labels: req.GetLabels()})
	if err != nil {
		return nil, err
	}
	out := &pb.PathResponse{EntityIds: p.EntityIDs, TotalCost: p.TotalCost, CombinedConfidence: p.CombinedConfidence}
	for _, e := range p.Edges {
		out.Edges = append(out.Edges, convert.EdgeProto(e))
	}
	return out, nil
}

// Diff implements GraphService.
func (s *Server) Diff(ctx context.Context, req *pb.DiffRequest) (*pb.DiffResponse, error) {
	c, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	res, err := s.o.Store.Diff(ctx, c, graphstore.DiffRequest{ProgramID: req.GetProgramId(), Since: convert.Time(req.GetSince()),
		RunIDA: req.GetRunIdA(), RunIDB: req.GetRunIdB(), Page: page(req.GetPage())})
	if err != nil {
		return nil, err
	}
	out := &pb.DiffResponse{Page: convert.PageResp(res.NextPageToken)}
	for _, ch := range res.Changes {
		out.Changes = append(out.Changes, convert.ChangeProto(ch))
	}
	return out, nil
}

// Search implements GraphService.
func (s *Server) Search(ctx context.Context, req *pb.SearchRequest) (*pb.SearchResponse, error) {
	c, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	res, err := s.o.Store.Search(ctx, c, graphstore.SearchRequest{ProgramID: req.GetProgramId(), Query: req.GetQuery(),
		AssetTypes: req.GetAssetTypes(), Limit: int(req.GetLimit()), Fuzzy: req.GetFuzzy()})
	if err != nil {
		return nil, err
	}
	out := &pb.SearchResponse{}
	for _, h := range res.Hits {
		out.Hits = append(out.Hits, &pb.SearchHit{Entity: convert.EntityProto(h.Entity), Score: h.Score, MatchedTrigrams: h.MatchedTrigrams})
	}
	return out, nil
}

// WatchChanges implements GraphService: every change after after_seq, in
// order, then new ones as they commit (resumable).
func (s *Server) WatchChanges(req *pb.WatchChangesRequest, stream grpc.ServerStreamingServer[pb.ChangeEvent]) error {
	ctx := stream.Context()
	c, _, err := caller(ctx)
	if err != nil {
		return err
	}
	err = s.o.Store.WatchChanges(ctx, c, graphstore.WatchRequest{ProgramID: req.GetProgramId(), AfterSeq: req.GetAfterSeq(),
		Types: convert.ChangeTypes(req.GetTypes())}, func(ev graphstore.ChangeEvent) error { return stream.Send(convert.ChangeProto(ev)) })
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// AddTag implements GraphService.
func (s *Server) AddTag(ctx context.Context, req *pb.AddTagRequest) (*pb.Tag, error) {
	c, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.o.Store.AddTag(ctx, c, graphstore.AddTagRequest{TargetKind: graphstore.TargetKind(req.GetTargetKind()), TargetID: req.GetTargetId(),
		PropertyType: req.GetPropertyType(), Name: req.GetName(), Content: convert.StructToMap(req.GetContent())})
	if err != nil {
		return nil, err
	}
	return convert.TagProto(t), nil
}

// SetStale implements GraphService.
func (s *Server) SetStale(ctx context.Context, req *pb.SetStaleRequest) (*pb.SetStaleResponse, error) {
	c, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	res, err := s.o.Store.SetStale(ctx, c, graphstore.SetStaleRequest{ProgramID: req.GetProgramId(), RunID: req.GetRunId(),
		ToolName: req.GetToolName(), AssetTypes: req.GetAssetTypes(), StaleAfterRuns: int(req.GetStaleAfterRuns())})
	if err != nil {
		return nil, err
	}
	s.publish(ctx, res.Changes)
	return &pb.SetStaleResponse{EntitiesMarked: int32(res.EntitiesMarked), EdgesMarked: int32(res.EdgesMarked)}, nil
}

// PolicySource returns current scope policies for passive scope status at
// ingest (scope.Client.Policy with version 0).
type PolicySource interface {
	Policy(ctx context.Context, tenant, program core.UUID, version int32) (*scope.Policy, error)
}

// ScopeFunc adapts a policy source to graphstore.PolicyScope with a short
// cache (policies change only when a new scope version is published).
func ScopeFunc(src PolicySource, ttl time.Duration) func(tenantID, programID string) *scope.Policy {
	type entry struct {
		p  *scope.Policy
		at time.Time
	}
	var mu sync.Mutex
	cache := map[[2]string]entry{}
	return func(tenantID, programID string) *scope.Policy {
		k := [2]string{tenantID, programID}
		mu.Lock()
		e, ok := cache[k]
		mu.Unlock()
		if ok && time.Since(e.at) < ttl {
			return e.p
		}
		t, err1 := core.ParsePublicID(core.PrefixTenant, tenantID)
		p, err2 := core.ParsePublicID(core.PrefixProgram, programID)
		if err1 != nil || err2 != nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pol, err := src.Policy(ctx, t, p, 0)
		if err != nil {
			pol = nil
		}
		mu.Lock()
		cache[k] = entry{pol, time.Now()}
		mu.Unlock()
		return pol
	}
}

func oamStatus(s string) oam.ScopeStatus { return oam.ScopeStatus(s) }
func oamState(s string) oam.State        { return oam.State(s) }
