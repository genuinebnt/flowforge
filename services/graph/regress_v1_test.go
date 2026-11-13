package graph_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/graphstore/storetest"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
)

func TestRegress_G_CORE_03_PendingReferences_Memory(t *testing.T) { pendingRefs(t, "") }
func TestRegress_G_CORE_03_PendingReferences_Postgres(t *testing.T) {
	pendingRefs(t, servicetest.FreshDB(t))
}

// pendingRefs: through GraphService, an observation whose edge names an
// entity id that is not ingested yet succeeds (nothing applied), and the
// ingest that creates the entity applies it; ff_graph_pending_observations
// counts both events.
func pendingRefs(t *testing.T, dsn string) {
	st, a, _ := servicetest.Stack(t, devstack.Options{DBDSN: dsn})
	prog, err := st.Scope.CreateProgram(a, &pb.CreateProgramRequest{Name: "g", InitialScope: servicetest.ScopeDoc("app.test")})
	if err != nil {
		t.Fatal(err)
	}
	at := timestamppb.New(time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC))
	fqdnID := storetest.EntityIDIn(core.PublicID(core.PrefixTenant, servicetest.TenantA), prog.GetId(), oam.FQDN{Name: "x.app.test"})
	first, err := st.Graph.IngestObservations(a, &pb.IngestRequest{ProgramId: prog.GetId(), ToolName: "test", Observation: &pb.Observation{
		ObservationId: "refers", ObservedAt: at, Confidence: 0.9,
		Entities: []*pb.OamEntity{{Ref: "u", AssetType: "URL", Content: content(map[string]any{"url": "https://x.app.test/"})}},
		Edges: []*pb.OamEdge{{From: "u", To: fqdnID, Relation: &pb.OamRelation{RelationType: "SimpleRelation", Label: "domain",
			Content: content(map[string]any{})}}}}})
	if err != nil || first.GetEntitiesCreated() != 0 || len(first.GetChanges()) != 0 {
		t.Fatalf("pending ingest: %v %v", first, err)
	}
	second, err := st.Graph.IngestObservations(a, &pb.IngestRequest{ProgramId: prog.GetId(), ToolName: "test", Observation: &pb.Observation{
		ObservationId: "creates", ObservedAt: at, Confidence: 0.9,
		Entities: []*pb.OamEntity{{Ref: "n", AssetType: "FQDN", Content: content(map[string]any{"name": "x.app.test"})}}}})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(second.GetChanges()); n != 3 { // the FQDN, then the released URL and edge
		t.Fatalf("changes of the releasing ingest: %d, want 3: %v", n, second.GetChanges())
	}
	reg := st.Metrics.Registry
	if reg.Value("ff_graph_pending_observations_total", "buffered") != 1 || reg.Value("ff_graph_pending_observations_total", "released") != 1 {
		t.Fatalf("pending metric: buffered %v released %v", reg.Value("ff_graph_pending_observations_total", "buffered"),
			reg.Value("ff_graph_pending_observations_total", "released"))
	}
}
