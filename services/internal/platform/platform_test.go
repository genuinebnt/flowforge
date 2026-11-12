package platform_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

func TestErrorMappingRoundTrip(t *testing.T) {
	cases := []struct {
		err    error
		code   codes.Code
		reason string
	}{
		{core.Errorf(core.KindNotFound, core.ReasonNotFound, "run not found"), codes.NotFound, core.ReasonNotFound},
		{core.Errorf(core.KindAborted, core.ReasonVersionConflict, "conflict"), codes.Aborted, core.ReasonVersionConflict},
		{&core.Error{Kind: core.KindResourceExhausted, Reason: core.ReasonRateLimited, RetryAfter: 3 * time.Second}, codes.ResourceExhausted, core.ReasonRateLimited},
		{core.Errorf(core.KindPermissionDenied, core.ReasonKillSwitchActive, "kill"), codes.PermissionDenied, core.ReasonKillSwitchActive},
		{context.DeadlineExceeded, codes.DeadlineExceeded, core.ReasonDeadlineExceeded},
		{errors.New("secret detail"), codes.Internal, core.ReasonInternal},
	}
	for _, c := range cases {
		st := platform.ToStatus(c.err, "test", "req_1")
		if got := status.Code(st); got != c.code {
			t.Fatalf("%v: code %v, want %v", c.err, got, c.code)
		}
		s, _ := status.FromError(st)
		var info *errdetails.ErrorInfo
		var retry *errdetails.RetryInfo
		for _, d := range s.Details() {
			switch v := d.(type) {
			case *errdetails.ErrorInfo:
				info = v
			case *errdetails.RetryInfo:
				retry = v
			}
		}
		if info == nil || info.GetReason() != c.reason || info.GetMetadata()["request_id"] != "req_1" || info.GetDomain() != "test" {
			t.Fatalf("%v: error info %v", c.err, info)
		}
		if c.code == codes.ResourceExhausted && (retry == nil || retry.GetRetryDelay().AsDuration() != 3*time.Second) {
			t.Fatalf("RESOURCE_EXHAUSTED must carry RetryInfo: %v", retry)
		}
		if c.code == codes.Internal && strings.Contains(s.Message(), "secret") {
			t.Fatalf("INTERNAL leaks the cause: %q", s.Message())
		}
		back := platform.FromStatus(st)
		var ce *core.Error
		if !errors.As(back, &ce) || ce.Reason != c.reason {
			t.Fatalf("reverse mapping of %v: %v", c.err, back)
		}
		if c.code == codes.ResourceExhausted && ce.RetryAfter != 3*time.Second {
			t.Fatalf("retry after %v", ce.RetryAfter)
		}
	}
	// Diagnostics travel as details.
	e := core.Errorf(core.KindInvalidArgument, core.ReasonDSLDiagnostics, "bad")
	e.Details = []any{&pb.Diagnostic{Code: "E0301", Message: "unchecked"}}
	back := platform.FromStatus(platform.ToStatus(e, "test", ""))
	var ce *core.Error
	if !errors.As(back, &ce) || len(ce.Details) != 1 || ce.Details[0].(*pb.Diagnostic).GetCode() != "E0301" {
		t.Fatalf("diagnostic details: %+v", back)
	}
}

func TestTraceparent(t *testing.T) {
	tp, ok := platform.ParseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if !ok || tp.String() != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Fatalf("parse: %v %v", tp, ok)
	}
	c := tp.Child()
	if c.TraceID != tp.TraceID || c.SpanID == tp.SpanID {
		t.Fatal("child must keep the trace and get a new span")
	}
	for _, bad := range []string{"", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "00-xyz-00f067aa0ba902b7-01"} {
		if _, ok := platform.ParseTraceparent(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestMetricsText(t *testing.T) {
	m := platform.NewMetrics(nil)
	m.ScopeDecisions.Inc("deny", "kill_switch")
	m.ScopeDecisions.Inc("deny", "kill_switch")
	m.GraphIngest.Observe(0.02)
	var b bytes.Buffer
	if err := m.Registry.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{`ff_scope_decisions_total{decision="deny",reason="kill_switch"} 2`, `ff_graph_ingest_seconds_bucket{le="0.025"} 1`,
		`ff_graph_ingest_seconds_count 1`, "# TYPE ff_runs_total counter", "# TYPE ff_approvals_pending gauge"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestLoggerFields(t *testing.T) {
	var b bytes.Buffer
	log := platform.NewLogger(&b, "scope", "dev", 0)
	ctx := platform.WithIdentity(context.Background(), platform.Identity{Tenant: core.MustParseUUID("0190a1b2-0000-7000-8000-00000000a001")})
	ctx = platform.WithRequestID(platform.WithTrace(ctx, platform.NewTrace()), "req_x")
	log.InfoContext(platform.WithLogFields(ctx, "run_1", "0/1"), "hello")
	var m map[string]any
	if err := json.Unmarshal(b.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ts", "level", "service", "env", "trace_id", "span_id", "request_id", "tenant_id", "run_id", "effect_path", "msg"} {
		if _, ok := m[k]; !ok {
			t.Errorf("log line lacks %s: %s", k, b.String())
		}
	}
}

// echo is a minimal ScopeService whose GetProgram reports the identity.
type echo struct {
	pb.UnimplementedScopeServiceServer
}

func (echo) GetProgram(ctx context.Context, _ *pb.GetProgramRequest) (*pb.Program, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	dl, _ := ctx.Deadline()
	return &pb.Program{Id: id.TenantPublicID(), Name: id.Actor, Platform: string(id.Role), Status: time.Until(dl).Round(time.Second).String()}, nil
}

func (echo) Check(context.Context, *pb.CheckRequest) (*pb.Decision, error) { panic("boom") }

func TestInterceptors(t *testing.T) {
	clock := &core.SystemClock{}
	keys, err := platform.NewDevKeys("test")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := platform.NewDevKeys("test")
	v, _ := keys.Verifier(clock)
	g := platform.NewGRPCServer(platform.ServerOptions{Service: "scope", Verifier: v})
	pb.RegisterScopeServiceServer(g, echo{})
	lis := bufconn.Listen(1 << 20)
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()
	dialer := grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() })
	tokens, _ := keys.Tokens(clock)
	conn, err := platform.Dial("passthrough:///buf", platform.ClientOptions{Tokens: tokens}, dialer)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := pb.NewScopeServiceClient(conn)
	tenant := core.MustParseUUID("0190a1b2-0000-7000-8000-00000000a001")
	ctx := platform.WithIdentity(context.Background(), platform.Identity{Tenant: tenant, Actor: "user:0190a1b2-0000-7000-8000-000000000001", Role: auth.RoleAnalyst})
	p, err := c.GetProgram(ctx, &pb.GetProgramRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if p.GetId() != core.PublicID(core.PrefixTenant, tenant) || p.GetName() != "user:0190a1b2-0000-7000-8000-000000000001" || p.GetPlatform() != "analyst" {
		t.Fatalf("identity %+v", p)
	}
	if p.GetStatus() != "5s" {
		t.Fatalf("default unary deadline %s", p.GetStatus())
	}
	// A panic becomes INTERNAL.
	if _, err := c.Check(ctx, &pb.CheckRequest{}); core.KindOf(err) != core.KindInternal {
		t.Fatalf("panic: %v", err)
	}
	// Raw calls: no token, a token from an untrusted key, a mismatched x-tenant-id.
	raw, _ := grpc.NewClient("passthrough:///buf", dialer, grpc.WithTransportCredentials(insecure.NewCredentials()))
	defer raw.Close()
	rc := pb.NewScopeServiceClient(raw)
	expect := func(name string, md metadata.MD, reason string) {
		t.Helper()
		_, err := rc.GetProgram(metadata.NewOutgoingContext(context.Background(), md), &pb.GetProgramRequest{})
		ce := platform.FromStatus(err).(*core.Error)
		if status.Code(err) != codes.Unauthenticated || ce.Reason != reason {
			t.Errorf("%s: %v (%s)", name, err, ce.Reason)
		}
	}
	expect("missing", metadata.MD{}, core.ReasonTokenMissing)
	otherTokens, _ := other.Tokens(clock)
	bad, _ := otherTokens.Token(ctx, platform.Identity{Tenant: tenant, Actor: "user:x", Role: auth.RoleAdmin})
	expect("untrusted key", metadata.Pairs("authorization", "Bearer "+bad), core.ReasonTokenInvalid)
	expect("garbage", metadata.Pairs("authorization", "Bearer abc.def.ghi"), core.ReasonTokenInvalid)
	good, _ := tokens.Token(ctx, platform.Identity{Tenant: tenant, Actor: "user:x", Role: auth.RoleAdmin})
	expect("tenant mismatch", metadata.Pairs("authorization", "Bearer "+good, "x-tenant-id", core.PublicID(core.PrefixTenant, core.MustParseUUID("0190a1b2-0000-7000-8000-00000000b002"))), core.ReasonTokenInvalid)
	expect("actor mismatch", metadata.Pairs("authorization", "Bearer "+good, "x-actor", "user:y"), core.ReasonTokenInvalid)
	// A client call without an identity never leaves the process.
	if _, err := c.GetProgram(context.Background(), &pb.GetProgramRequest{}); core.KindOf(err) != core.KindUnauthenticated {
		t.Fatalf("no identity: %v", err)
	}
}
