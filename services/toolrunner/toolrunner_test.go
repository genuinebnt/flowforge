package toolrunner_test

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
	"github.com/genuinebnt/flowforge/services/toolrunner"
)

type checker struct{}

func (checker) CheckTarget(_ context.Context, t scopesvc.Target) (scope.Decision, error) {
	if t.Target == "app.test" {
		return scope.Decision{ID: "dec_x", Result: scope.ResultAllow, Reason: "allow", Target: t.Target}, nil
	}
	return scope.Decision{Result: scope.ResultDeny, Reason: "no_matching_allow"}, nil
}

type slow struct{ stopped atomic.Int64 }

func (*slow) Spec() toolrunner.Spec {
	return toolrunner.Spec{Name: "slow", Version: "1", InputType: "String", OutputType: "String", Timeout: time.Minute}
}

func (s *slow) Run(ctx context.Context, _ json.RawMessage, _ *toolrunner.ToolCtx, out toolrunner.Emitter) (json.RawMessage, error) {
	_ = out.Progress(0, 1, "started")
	<-ctx.Done()
	s.stopped.Store(time.Now().UnixNano())
	return nil, ctx.Err()
}

func TestToolRunner(t *testing.T) {
	clock := &core.SystemClock{}
	keys, _ := platform.NewDevKeys("t")
	v, _ := keys.Verifier(clock)
	tokens, _ := keys.Tokens(clock)
	res := &dnsNet{answers: map[string]string{"app.test.": "127.0.0.1", "evil.test.": "127.0.0.1"}}
	sl := &slow{}
	srv := toolrunner.NewServer(toolrunner.Options{Adapters: []toolrunner.Adapter{toolrunner.DNSResolve{}, toolrunner.Mock{}, sl, toolrunner.HTTPProbe{}},
		Scope: checker{}, Net: res})
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

	tools, err := c.ListTools(ctx, &pb.ListToolsRequest{})
	if err != nil || len(tools.GetTools()) != 4 {
		t.Fatalf("tools %v %v", tools, err)
	}
	prog := core.PublicID(core.PrefixProgram, core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000f1"))
	exec := func(tool string, input any) []*pb.ExecuteResponse {
		t.Helper()
		in, _ := structpb.NewValue(input)
		s, err := c.Execute(ctx, &pb.ExecuteRequest{Tool: tool, Input: in, ProgramId: prog, EffectPath: "0", IdempotencyKey: "k"})
		if err != nil {
			t.Fatal(err)
		}
		var out []*pb.ExecuteResponse
		for {
			m, err := s.Recv()
			if err != nil {
				return out
			}
			out = append(out, m)
		}
	}
	got := exec("dnsresolve", "app.test")
	last := got[len(got)-1].GetDone()
	if last == nil || last.GetObservations() != 1 || last.GetOutput().GetListValue().GetValues()[0].GetStringValue() != "127.0.0.1" {
		t.Fatalf("dnsresolve app.test: %v", got)
	}
	var obs *pb.Observation
	for _, m := range got {
		if o := m.GetObservation(); o != nil {
			obs = o
		}
	}
	if obs == nil || len(obs.GetEntities()) != 2 || obs.GetEdges()[0].GetRelation().GetRelationType() != "BasicDNSRelation" {
		t.Fatalf("observation %v", got)
	}
	// A denied target is skipped and never resolved.
	got = exec("dnsresolve", "evil.test")
	if got[0].GetSkipped().GetTarget() != "evil.test" || len(got[len(got)-1].GetDone().GetOutput().GetListValue().GetValues()) != 0 || res.count("evil.test.") != 0 {
		t.Fatalf("dnsresolve evil.test: %v (queries %d)", got, res.count("evil.test."))
	}
	// Unknown tools are NOT_FOUND.
	s, _ := c.Execute(ctx, &pb.ExecuteRequest{Tool: "nmap"})
	_, err = s.Recv()
	servicetest.IsKind(t, err, core.KindNotFound, "")

	// Cancelling the call cancels the tool within the 2 s budget.
	cctx, cancel := context.WithCancel(ctx)
	in, _ := structpb.NewValue("x")
	s, err = c.Execute(cctx, &pb.ExecuteRequest{Tool: "slow", Input: in})
	if err != nil {
		t.Fatal(err)
	}
	if m, err := s.Recv(); err != nil || m.GetProgress() == nil {
		t.Fatalf("progress %v %v", m, err)
	}
	at := time.Now()
	cancel()
	servicetest.Eventually(t, 3*time.Second, "tool stop", func() bool { return sl.stopped.Load() != 0 })
	if d := time.Unix(0, sl.stopped.Load()).Sub(at); d > toolrunner.CancelGrace {
		t.Fatalf("tool stopped after %v", d)
	}
}
