package toolrunner_test

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	"github.com/genuinebnt/flowforge/services/toolrunner"
)

// TestRegress_GOPS14_RawOutputToBlobStore: with a blob store each
// observation is stored as the tool reported it and its raw_ref names the
// object (spec 10.5 graph.observations.raw_ref); the same effect run twice
// writes the same object.
func TestRegress_GOPS14_RawOutputToBlobStore(t *testing.T) {
	clock := &core.SystemClock{}
	keys, _ := platform.NewDevKeys("t")
	v, _ := keys.Verifier(clock)
	tokens, _ := keys.Tokens(clock)
	blobs := &graphstore.MemBlobs{}
	srv := toolrunner.NewServer(toolrunner.Options{Adapters: []toolrunner.Adapter{toolrunner.DNSResolve{}}, Scope: checker{},
		Resolver: servicetest.NewResolver(map[string]string{"app.test": "127.0.0.1"}), Raw: blobs})
	g := platform.NewGRPCServer(platform.ServerOptions{Service: "tool-runner", Verifier: v})
	pb.RegisterToolRunnerServiceServer(g, srv)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()
	conn, err := platform.Dial("passthrough:///buf", platform.ClientOptions{Tokens: tokens},
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := pb.NewToolRunnerServiceClient(conn)
	ctx := platform.WithIdentity(context.Background(), platform.Identity{Tenant: servicetest.TenantA, Actor: "service:engine", Role: auth.RoleAnalyst})
	prog := core.PublicID(core.PrefixProgram, core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000f1"))
	run := core.PublicID(core.PrefixRun, core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000f2"))
	refs := map[string]bool{}
	for range 2 {
		in, _ := structpb.NewValue("app.test")
		s, err := c.Execute(ctx, &pb.ExecuteRequest{Tool: "dnsresolve", Input: in, ProgramId: prog, RunId: run, EffectPath: "3", IdempotencyKey: "k"})
		if err != nil {
			t.Fatal(err)
		}
		for {
			m, err := s.Recv()
			if err != nil {
				break
			}
			if o := m.GetObservation(); o != nil {
				refs[o.GetRawRef()] = true
			}
		}
	}
	if len(refs) != 1 {
		t.Fatalf("raw refs %v, want one object for the repeated effect", refs)
	}
	for ref := range refs {
		if !strings.HasPrefix(ref, "mem://raw/"+core.PublicID(core.PrefixTenant, servicetest.TenantA)+"/"+prog+"/"+run+"/") {
			t.Fatalf("raw_ref %q", ref)
		}
		data, err := blobs.Get(ctx, ref)
		var obs struct {
			Entities []json.RawMessage `json:"entities"`
		}
		if err != nil || json.Unmarshal(data, &obs) != nil || len(obs.Entities) != 2 {
			t.Fatalf("raw object %s: %v", data, err)
		}
	}
}
