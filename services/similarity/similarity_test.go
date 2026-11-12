package similarity_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	simv1 "github.com/genuinebnt/flowforge/gen/flowforge/similarity/v1"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
)

func fqdn(id, name string) *pb.Observation {
	c, _ := structpb.NewStruct(map[string]any{"name": name})
	return &pb.Observation{ObservationId: id, ObservedAt: timestamppb.Now(), Confidence: 0.9,
		Entities: []*pb.OamEntity{{Ref: "a", AssetType: "FQDN", Content: c}}}
}

func TestSimilarity(t *testing.T) {
	st, a, b := servicetest.Stack(t, devstack.Options{})
	prog, err := st.Scope.CreateProgram(a, &pb.CreateProgramRequest{Name: "s", InitialScope: servicetest.ScopeDoc("jenkins.app.test")})
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range []string{"jenkins.app.test", "jenkins.other.test", "mail.zzz.example"} {
		if _, err := st.Graph.IngestObservations(a, &pb.IngestRequest{ProgramId: prog.GetId(), ToolName: "t", Observation: fqdn("obs_"+string(rune('a'+i)), n)}); err != nil {
			t.Fatal(err)
		}
	}
	sc := &simv1.Scope{TenantId: core.PublicID(core.PrefixTenant, servicetest.TenantA), ProgramId: prog.GetId()}
	servicetest.Eventually(t, 10*time.Second, "index from graph.changes", func() bool {
		info, err := st.Similarity.GetIndexInfo(a, &simv1.GetIndexInfoRequest{Scope: sc})
		return err == nil && info.GetVectors() == 3 && info.GetWatermarkSeq() > 0
	})
	res, err := st.Similarity.SearchByText(a, &simv1.SearchByTextRequest{Scope: sc, Text: "jenkins", Params: &simv1.SearchParams{K: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetHits()) != 1 || !res.GetHits()[0].GetInScope() {
		t.Fatalf("only the in-scope host may come back: %v", res.GetHits())
	}
	all, err := st.Similarity.SearchByText(a, &simv1.SearchByTextRequest{Scope: sc, Text: "jenkins", Params: &simv1.SearchParams{K: 5, IncludeOutOfScope: true}})
	if err != nil || len(all.GetHits()) != 3 {
		t.Fatalf("include_out_of_scope: %v %v", all, err)
	}
	var marked int
	for _, h := range all.GetHits() {
		if !h.GetInScope() {
			marked++
		}
	}
	if marked != 2 {
		t.Fatalf("out-of-scope hits not marked: %v", all.GetHits())
	}
	viewer := st.As(a, servicetest.TenantA, servicetest.UserA, auth.RoleViewer)
	_, err = st.Similarity.SearchByText(viewer, &simv1.SearchByTextRequest{Scope: sc, Text: "jenkins", Params: &simv1.SearchParams{IncludeOutOfScope: true}})
	servicetest.IsKind(t, err, core.KindPermissionDenied, core.ReasonForbiddenRole)
	if h, err := st.Similarity.HybridSearch(a, &simv1.HybridSearchRequest{Scope: sc, Text: "jenkins.app", Params: &simv1.SearchParams{K: 3}}); err != nil || len(h.GetHits()) != 1 {
		t.Fatalf("hybrid %v %v", h, err)
	}
	byEnt, err := st.Similarity.SearchByEntity(a, &simv1.SearchByEntityRequest{Scope: sc, EntityId: res.GetHits()[0].GetEntityId(), Params: &simv1.SearchParams{K: 5, IncludeOutOfScope: true}})
	if err != nil || len(byEnt.GetHits()) != 2 {
		t.Fatalf("by entity (itself excluded): %v %v", byEnt, err)
	}

	// Explicit upserts are ordered by source seq and deduplicated by hash.
	ent := res.GetHits()[0].GetEntityId()
	vec := make([]float32, 256)
	vec[1] = 1
	up, err := st.Similarity.Upsert(a, &simv1.UpsertRequest{Scope: sc, EntityId: ent, Vector: vec, ContentHash: "h1", SourceSeq: 1 << 30})
	if err != nil || !up.GetApplied() {
		t.Fatalf("upsert %v %v", up, err)
	}
	if up, _ := st.Similarity.Upsert(a, &simv1.UpsertRequest{Scope: sc, EntityId: ent, Vector: vec, ContentHash: "h1", SourceSeq: 1 << 31}); up.GetApplied() {
		t.Fatal("unchanged hash applied")
	}
	if up, _ := st.Similarity.Upsert(a, &simv1.UpsertRequest{Scope: sc, EntityId: ent, Vector: vec, ContentHash: "h2", SourceSeq: 5}); up.GetApplied() {
		t.Fatal("stale seq applied")
	}
	if v, err := st.Similarity.SearchByVector(a, &simv1.SearchByVectorRequest{Scope: sc, Vector: vec, Params: &simv1.SearchParams{K: 1}}); err != nil || v.GetHits()[0].GetEntityId() != ent {
		t.Fatalf("by vector %v %v", v, err)
	}
	_, err = st.Similarity.SearchByVector(a, &simv1.SearchByVectorRequest{Scope: sc, Vector: vec, Params: &simv1.SearchParams{K: 500}})
	servicetest.IsKind(t, err, core.KindInvalidArgument, "")

	// Tenant isolation and the token cross-check.
	_, err = st.Similarity.SearchByText(b, &simv1.SearchByTextRequest{Scope: &simv1.Scope{ProgramId: prog.GetId()}, Text: "jenkins"})
	servicetest.IsKind(t, err, core.KindNotFound, "")
	_, err = st.Similarity.SearchByText(b, &simv1.SearchByTextRequest{Scope: sc, Text: "jenkins"})
	servicetest.IsKind(t, err, core.KindNotFound, "")
	if _, err := st.Similarity.Delete(a, &simv1.DeleteRequest{Scope: sc, EntityId: ent, SourceSeq: 1 << 32}); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.Similarity.SearchByVector(a, &simv1.SearchByVectorRequest{Scope: sc, Vector: vec, Params: &simv1.SearchParams{K: 3, IncludeOutOfScope: true}}); len(v.GetHits()) != 2 {
		t.Fatalf("after delete %v", v.GetHits())
	}
}
