package devstack_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	graphv1 "github.com/genuinebnt/flowforge/gen/flowforge/graph/v1"
	simv1 "github.com/genuinebnt/flowforge/gen/flowforge/similarity/v1"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
)

// TestInternalAuth: every service rejects calls without a valid internal
// token; health checks need none.
func TestInternalAuth(t *testing.T) {
	st, _, _ := servicetest.Stack(t, devstack.Options{})
	raw := func(name string) *grpc.ClientConn {
		c, err := grpc.NewClient(st.Addrs[name], grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	calls := map[string]func(context.Context) error{
		"scope": func(ctx context.Context) error {
			_, err := pb.NewScopeServiceClient(raw(devstack.ScopeName)).ListPrograms(ctx, &pb.ListProgramsRequest{})
			return err
		},
		"engine": func(ctx context.Context) error {
			_, err := pb.NewEngineServiceClient(raw(devstack.EngineName)).ListRuns(ctx, &pb.ListRunsRequest{})
			return err
		},
		"tool-runner": func(ctx context.Context) error {
			_, err := pb.NewToolRunnerServiceClient(raw(devstack.ToolRunnerName)).ListTools(ctx, &pb.ListToolsRequest{})
			return err
		},
		"graph": func(ctx context.Context) error {
			_, err := pb.NewGraphServiceClient(raw(devstack.GraphName)).FindEntities(ctx, &pb.FindEntitiesRequest{})
			return err
		},
		"analytics": func(ctx context.Context) error {
			_, err := graphv1.NewGraphAnalyticsClient(raw(devstack.GraphName)).GetSnapshotInfo(ctx, &graphv1.GetSnapshotInfoRequest{})
			return err
		},
		"notifier": func(ctx context.Context) error {
			_, err := pb.NewNotifierServiceClient(raw(devstack.NotifierName)).ListRules(ctx, &pb.ListRulesRequest{})
			return err
		},
		"similarity": func(ctx context.Context) error {
			_, err := simv1.NewSimilarityClient(raw(devstack.SimilarityName)).GetIndexInfo(ctx, &simv1.GetIndexInfoRequest{})
			return err
		},
	}
	for name, call := range calls {
		for _, md := range []metadata.MD{{}, metadata.Pairs("authorization", "Bearer not.a.token"), metadata.Pairs("x-tenant-id", "ten_01JD8K0B000000000000000000")} {
			err := call(metadata.NewOutgoingContext(context.Background(), md))
			if status.Code(err) != codes.Unauthenticated {
				t.Errorf("%s with %v: %v", name, md, err)
			}
		}
	}
	h, err := healthpb.NewHealthClient(raw(devstack.EngineName)).Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil || h.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health %v %v", h, err)
	}
}
