package flowforgev1_test

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	graphv1 "github.com/genuinebnt/flowforge/gen/flowforge/graph/v1"
	similarityv1 "github.com/genuinebnt/flowforge/gen/flowforge/similarity/v1"
	flowforgev1 "github.com/genuinebnt/flowforge/gen/flowforge/v1"
)

// The imports above register every generated file; these references keep the
// dependency explicit.
var (
	_ = flowforgev1.File_flowforge_v1_common_proto
	_ = graphv1.File_flowforge_graph_v1_analytics_proto
	_ = similarityv1.File_flowforge_similarity_v1_similarity_proto
)

// flowforgeFiles returns every registered file in a flowforge.* package.
func flowforgeFiles(t *testing.T) []protoreflect.FileDescriptor {
	t.Helper()
	var files []protoreflect.FileDescriptor
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if strings.HasPrefix(string(fd.Package()), "flowforge.") {
			files = append(files, fd)
		}
		return true
	})
	if len(files) != 9 {
		t.Fatalf("found %d flowforge proto files, want 9", len(files))
	}
	return files
}

// Spec 11.1: every enum has a zero value named *_UNSPECIFIED, and (proto
// convention adopted for 21.6/22.9) every value carries the enum's prefix.
func TestContract_EnumZeroValueUnspecified(t *testing.T) {
	var check func(enums protoreflect.EnumDescriptors)
	var walk func(msgs protoreflect.MessageDescriptors)
	count := 0
	check = func(enums protoreflect.EnumDescriptors) {
		for i := range enums.Len() {
			ed := enums.Get(i)
			count++
			zero := ed.Values().ByNumber(0)
			if zero == nil {
				t.Errorf("%s: no zero value", ed.FullName())
				continue
			}
			if !strings.HasSuffix(string(zero.Name()), "_UNSPECIFIED") {
				t.Errorf("%s: zero value %s does not end in _UNSPECIFIED", ed.FullName(), zero.Name())
			}
			prefix := strings.TrimSuffix(string(zero.Name()), "UNSPECIFIED")
			for j := range ed.Values().Len() {
				if v := ed.Values().Get(j); !strings.HasPrefix(string(v.Name()), prefix) {
					t.Errorf("%s: value %s lacks prefix %s", ed.FullName(), v.Name(), prefix)
				}
			}
		}
	}
	walk = func(msgs protoreflect.MessageDescriptors) {
		for i := range msgs.Len() {
			md := msgs.Get(i)
			check(md.Enums())
			walk(md.Messages())
		}
	}
	for _, fd := range flowforgeFiles(t) {
		check(fd.Enums())
		walk(fd.Messages())
	}
	if count == 0 {
		t.Fatal("no enums found")
	}
}

type rpcKind int

const (
	unary rpcKind = iota
	serverStream
)

// TestContract_ServiceRPCs pins the RPC names and streaming shapes listed in
// spec 11.3-11.7, 21.6 and 22.9.
func TestContract_ServiceRPCs(t *testing.T) {
	want := map[protoreflect.FullName]map[string]rpcKind{
		"flowforge.v1.ScopeService": {
			"Check": unary, "CheckBatch": unary, "CreateProgram": unary, "GetProgram": unary,
			"ListPrograms": unary, "PutScope": unary, "GetScope": unary, "ListScopeVersions": unary,
			"SetKillSwitch": unary, "GetKillSwitches": unary,
		},
		"flowforge.v1.EngineService": {
			"CheckWorkflow": unary, "RegisterWorkflow": unary, "GetWorkflowVersion": unary, "ListWorkflows": unary,
			"SubmitRun": unary, "GetRun": unary, "ListRuns": unary, "CancelRun": unary, "ListRunEvents": unary,
			"WatchRun":      serverStream,
			"ListApprovals": unary, "GetApproval": unary, "DecideApproval": unary, "ResolveUnknownEffect": unary,
		},
		"flowforge.v1.ToolRunnerService": {
			"ListTools": unary, "Execute": serverStream,
		},
		"flowforge.v1.GraphService": {
			"IngestObservations": unary, "GetEntity": unary, "FindEntities": unary, "Neighbors": unary,
			"FindPath": unary, "Diff": unary, "Search": unary, "WatchChanges": serverStream,
			"AddTag": unary, "SetStale": unary,
		},
		"flowforge.v1.NotifierService": {
			"PutRule": unary, "GetRule": unary, "ListRules": unary, "DeleteRule": unary, "TestRule": unary,
			"PutChannel": unary, "ListChannels": unary, "ListAlertEvents": unary,
		},
		"flowforge.graph.v1.GraphAnalytics": {
			"RunAnalysis": unary, "GetSnapshotInfo": unary,
		},
		"flowforge.similarity.v1.Similarity": {
			"Upsert": unary, "Delete": unary, "SearchByVector": unary, "SearchByEntity": unary,
			"SearchByText": unary, "HybridSearch": unary, "GetIndexInfo": unary,
		},
	}

	got := map[protoreflect.FullName]protoreflect.ServiceDescriptor{}
	for _, fd := range flowforgeFiles(t) {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			got[sd.FullName()] = sd
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected service %s", name)
		}
	}
	for name, rpcs := range want {
		sd, ok := got[name]
		if !ok {
			t.Errorf("missing service %s", name)
			continue
		}
		var have []string
		for i := range sd.Methods().Len() {
			md := sd.Methods().Get(i)
			have = append(have, string(md.Name()))
			kind, ok := rpcs[string(md.Name())]
			if !ok {
				t.Errorf("%s: unexpected rpc %s", name, md.Name())
				continue
			}
			if md.IsStreamingClient() {
				t.Errorf("%s.%s: client streaming, want none", name, md.Name())
			}
			if wantStream := kind == serverStream; md.IsStreamingServer() != wantStream {
				t.Errorf("%s.%s: server streaming = %v, want %v", name, md.Name(), md.IsStreamingServer(), wantStream)
			}
		}
		for rpc := range rpcs {
			if !slices.Contains(have, rpc) {
				t.Errorf("%s: missing rpc %s", name, rpc)
			}
		}
	}
}
