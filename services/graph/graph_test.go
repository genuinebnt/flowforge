package graph_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/genuinebnt/flowforge/core"
	graphv1 "github.com/genuinebnt/flowforge/gen/flowforge/graph/v1"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
)

func content(m map[string]any) *structpb.Struct { s, _ := structpb.NewStruct(m); return s }

func observation(id, name, ip string) *pb.Observation {
	return &pb.Observation{ObservationId: id, ObservedAt: timestamppb.New(time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)), Confidence: 0.9,
		Entities: []*pb.OamEntity{
			{Ref: "n", AssetType: "FQDN", Content: content(map[string]any{"name": name})},
			{Ref: "i", AssetType: "IPAddress", Content: content(map[string]any{"address": ip, "ip_version": "IPv4"})},
		},
		Edges: []*pb.OamEdge{{From: "n", To: "i", Relation: &pb.OamRelation{RelationType: "BasicDNSRelation", Label: "dns_record",
			Content: content(map[string]any{"header": map[string]any{"rr_type": 1, "class": 1, "ttl": 300}})}}},
	}
}

func TestGraphService_Memory(t *testing.T)   { runGraph(t, "") }
func TestGraphService_Postgres(t *testing.T) { runGraph(t, servicetest.FreshDB(t)) }

func runGraph(t *testing.T, dsn string) {
	st, a, b := servicetest.Stack(t, devstack.Options{DBDSN: dsn})
	prog, err := st.Scope.CreateProgram(a, &pb.CreateProgramRequest{Name: "g", InitialScope: servicetest.ScopeDoc("app.test")})
	if err != nil {
		t.Fatal(err)
	}
	ingest := func(id, name, ip string) *pb.IngestResponse {
		t.Helper()
		r, err := st.Graph.IngestObservations(a, &pb.IngestRequest{ProgramId: prog.GetId(), ToolName: "test", Observation: observation(id, name, ip)})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := ingest("obs_1", "app.test", "203.0.113.10")
	if r.GetEntitiesCreated() != 2 || r.GetEdgesCreated() != 1 || len(r.GetChanges()) == 0 {
		t.Fatalf("ingest %v", r)
	}
	if again := ingest("obs_1", "app.test", "203.0.113.10"); again.GetEntitiesCreated() != 0 || len(again.GetChanges()) != 0 {
		t.Fatalf("re-ingest is not idempotent: %v", again)
	}
	ingest("obs_2", "other.test", "203.0.113.11")
	fqdn := r.GetRefToEntityId()["n"]
	e, err := st.Graph.GetEntity(a, &pb.GetEntityRequest{EntityId: fqdn})
	if err != nil || e.GetScopeStatus() != "in_scope" {
		t.Fatalf("entity %v %v (scope status from the scope service)", e, err)
	}
	if o, _ := st.Graph.FindEntities(a, &pb.FindEntitiesRequest{ProgramId: prog.GetId(), KeyPrefix: "fqdn:other"}); len(o.GetEntities()) != 1 || o.GetEntities()[0].GetScopeStatus() != "out_of_scope" {
		t.Fatalf("out-of-scope entity %v", o)
	}
	n, err := st.Graph.Neighbors(a, &pb.NeighborsRequest{EntityId: fqdn, Direction: pb.Direction_DIRECTION_OUT, Depth: 1})
	if err != nil || len(n.GetEntities()) != 2 || len(n.GetEdges()) != 1 {
		t.Fatalf("neighbors %v %v", n, err)
	}
	if s, err := st.Graph.Search(a, &pb.SearchRequest{ProgramId: prog.GetId(), Query: "app.test"}); err != nil || len(s.GetHits()) == 0 {
		t.Fatalf("search %v %v", s, err)
	}
	tag, err := st.Graph.AddTag(a, &pb.AddTagRequest{TargetKind: "entity", TargetId: fqdn, PropertyType: "SimpleProperty", Name: "owner", Content: content(map[string]any{"value": "team-a"})})
	if err != nil || tag.GetName() != "owner" {
		t.Fatalf("tag %v %v", tag, err)
	}

	// Tenant isolation.
	_, err = st.Graph.GetEntity(b, &pb.GetEntityRequest{EntityId: fqdn})
	servicetest.IsKind(t, err, core.KindNotFound, "")
	_, err = st.Graph.Neighbors(b, &pb.NeighborsRequest{EntityId: fqdn, Direction: pb.Direction_DIRECTION_OUT})
	servicetest.IsKind(t, err, core.KindNotFound, "")
	_, err = st.Graph.AddTag(b, &pb.AddTagRequest{TargetKind: "entity", TargetId: fqdn, PropertyType: "SimpleProperty", Name: "x", Content: content(map[string]any{"value": "y"})})
	servicetest.IsKind(t, err, core.KindNotFound, "")
	if f, err := st.Graph.FindEntities(b, &pb.FindEntitiesRequest{ProgramId: prog.GetId()}); err != nil || len(f.GetEntities()) != 0 {
		t.Fatalf("B finds %v %v", f, err)
	}
	if s, err := st.Graph.Search(b, &pb.SearchRequest{ProgramId: prog.GetId(), Query: "app"}); err != nil || len(s.GetHits()) != 0 {
		t.Fatalf("B searches %v %v", s, err)
	}
	if d, err := st.Graph.Diff(b, &pb.DiffRequest{ProgramId: prog.GetId(), From: &pb.DiffRequest_Since{Since: timestamppb.New(time.Unix(0, 0))}}); err != nil || len(d.GetChanges()) != 0 {
		t.Fatalf("B diffs %v %v", d, err)
	}

	// WatchChanges is resumable by after_seq.
	watch := func(ctx context.Context, after int64, n int) []*pb.ChangeEvent {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		s, err := st.Graph.WatchChanges(ctx, &pb.WatchChangesRequest{ProgramId: prog.GetId(), AfterSeq: after})
		if err != nil {
			t.Fatal(err)
		}
		var out []*pb.ChangeEvent
		for len(out) < n {
			ev, err := s.Recv()
			if err != nil {
				t.Fatalf("watch: %v after %d", err, len(out))
			}
			out = append(out, ev)
		}
		return out
	}
	first := watch(a, 0, 3)
	next := watch(a, first[1].GetSeq(), 1)
	if next[0].GetSeq() != first[2].GetSeq() {
		t.Fatalf("resume: %d, want %d", next[0].GetSeq(), first[2].GetSeq())
	}

	// Analytics over the CSR snapshot; tenant and watermark cross-checks.
	res, err := st.Analytics.RunAnalysis(a, &graphv1.RunAnalysisRequest{TenantId: core.PublicID(core.PrefixTenant, servicetest.TenantA),
		ProgramId: prog.GetId(), Algorithm: &graphv1.RunAnalysisRequest_Components{Components: &graphv1.Components{}}})
	if err != nil || res.GetNodeCount() != 4 || len(res.GetGroups().GetGroups()) != 2 {
		t.Fatalf("components %v %v", res, err)
	}
	bfs, err := st.Analytics.RunAnalysis(a, &graphv1.RunAnalysisRequest{ProgramId: prog.GetId(),
		Algorithm: &graphv1.RunAnalysisRequest_Bfs{Bfs: &graphv1.Bfs{StartEntityId: fqdn, MaxDepth: 2, Direction: graphv1.Direction_DIRECTION_OUT}}})
	if err != nil || len(bfs.GetLevels().GetLevels()) != 2 {
		t.Fatalf("bfs %v %v", bfs, err)
	}
	path, err := st.Analytics.RunAnalysis(a, &graphv1.RunAnalysisRequest{ProgramId: prog.GetId(),
		Algorithm: &graphv1.RunAnalysisRequest_Shortest{Shortest: &graphv1.ShortestPath{FromEntityId: fqdn, ToEntityId: r.GetRefToEntityId()["i"]}}})
	if err != nil || len(path.GetPath().GetEntityIds()) != 2 || len(path.GetPath().GetEdgeIds()) != 1 || path.GetPath().GetEdgeIds()[0] == "" {
		t.Fatalf("path %v %v", path, err)
	}
	_, err = st.Analytics.RunAnalysis(a, &graphv1.RunAnalysisRequest{ProgramId: prog.GetId(), MinWatermarkSeq: 1 << 40,
		Algorithm: &graphv1.RunAnalysisRequest_Scc{Scc: &graphv1.Scc{}}})
	servicetest.IsKind(t, err, core.KindFailedPrecondition, "")
	_, err = st.Analytics.RunAnalysis(a, &graphv1.RunAnalysisRequest{TenantId: core.PublicID(core.PrefixTenant, servicetest.TenantB), ProgramId: prog.GetId(),
		Algorithm: &graphv1.RunAnalysisRequest_Scc{Scc: &graphv1.Scc{}}})
	servicetest.IsKind(t, err, core.KindNotFound, "")
	if empty, err := st.Analytics.RunAnalysis(b, &graphv1.RunAnalysisRequest{ProgramId: prog.GetId(),
		Algorithm: &graphv1.RunAnalysisRequest_Scc{Scc: &graphv1.Scc{}}}); err != nil || empty.GetNodeCount() != 0 {
		t.Fatalf("B's snapshot of A's program: %v %v", empty, err)
	}
	if info, err := st.Analytics.GetSnapshotInfo(a, &graphv1.GetSnapshotInfoRequest{ProgramId: prog.GetId()}); err != nil || info.GetNodeCount() != 4 || info.GetSha256() == "" {
		t.Fatalf("snapshot info %v %v", info, err)
	}

	// graph.changes reach the bus (inline in memory, through the
	// change_events relay in Postgres).
	got := make(chan string, 16)
	_ = st.Bus.Subscribe(a, "graph.changes", "test", func(_ context.Context, env *pb.EventEnvelope) error {
		got <- env.GetType()
		return nil
	})
	ingest("obs_3", "new.app.test", "203.0.113.12")
	select {
	case ty := <-got:
		if ty != "graph.entity_added" {
			t.Fatalf("type %s", ty)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no graph.changes event")
	}
}
