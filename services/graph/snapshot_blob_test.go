package graph_test

import (
	"strings"
	"testing"

	graphv1 "github.com/genuinebnt/flowforge/gen/flowforge/graph/v1"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
)

func TestRegress_GOPS14_SnapshotFilesToBlobStore_Memory(t *testing.T) { snapshotBlobs(t, "") }
func TestRegress_GOPS14_SnapshotFilesToBlobStore_Postgres(t *testing.T) {
	snapshotBlobs(t, servicetest.FreshDB(t))
}

// snapshotBlobs: with a blob store, the snapshot analytics builds is also
// written as a spec 21.4 file and catalogued, so GetSnapshotInfo reports
// its URI and the file decodes from the store.
func snapshotBlobs(t *testing.T, dsn string) {
	blobs := &graphstore.MemBlobs{}
	st, a, _ := servicetest.Stack(t, devstack.Options{DBDSN: dsn, Blobs: blobs})
	prog, err := st.Scope.CreateProgram(a, &pb.CreateProgramRequest{Name: "g", InitialScope: servicetest.ScopeDoc("app.test")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Graph.IngestObservations(a, &pb.IngestRequest{ProgramId: prog.GetId(), ToolName: "test",
		Observation: observation("obs_1", "app.test", "203.0.113.10")}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Analytics.RunAnalysis(a, &graphv1.RunAnalysisRequest{ProgramId: prog.GetId(),
		Algorithm: &graphv1.RunAnalysisRequest_Components{Components: &graphv1.Components{}}}); err != nil {
		t.Fatal(err)
	}
	info, err := st.Analytics.GetSnapshotInfo(a, &graphv1.GetSnapshotInfoRequest{ProgramId: prog.GetId()})
	if err != nil || !strings.HasPrefix(info.GetGcsUri(), "mem://snapshots/") || info.GetNodeCount() != 2 {
		t.Fatalf("snapshot info %v %v", info, err)
	}
	if data, err := blobs.Get(a, info.GetGcsUri()); err != nil || len(data) == 0 {
		t.Fatalf("snapshot file: %d bytes, %v", len(data), err)
	}
}
