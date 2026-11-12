package toolrunner

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
)

// CancelGrace is how long Execute waits for an adapter after cancellation
// (spec 6.3: adapters stop within 2 s).
const CancelGrace = 2 * time.Second

// Checker is the per-target scope check (scope service client).
type Checker interface {
	CheckTarget(ctx context.Context, t scopesvc.Target) (scope.Decision, error)
}

// Options configure a Server.
type Options struct {
	Adapters []Adapter
	Scope    Checker
	Egress   *Egress
	Resolver scope.Resolver
	Clock    core.Clock
	Logger   *slog.Logger
}

// Server implements flowforge.v1.ToolRunnerService.
type Server struct {
	pb.UnimplementedToolRunnerServiceServer
	o     Options
	tools map[string]Adapter
}

// NewServer returns a tool runner with the given adapters.
func NewServer(o Options) *Server {
	if o.Clock == nil {
		o.Clock = &core.SystemClock{}
	}
	if o.Logger == nil {
		o.Logger = platform.DiscardLogger()
	}
	s := &Server{o: o, tools: map[string]Adapter{}}
	for _, a := range o.Adapters {
		s.tools[a.Spec().Name] = a
	}
	return s
}

// DefaultAdapters are the built-in tools.
func DefaultAdapters(probe HTTPProbe) []Adapter {
	return []Adapter{DNSResolve{}, probe, TCPConnect{}, Mock{}}
}

// ListTools implements ToolRunnerService.
func (s *Server) ListTools(ctx context.Context, _ *pb.ListToolsRequest) (*pb.ListToolsResponse, error) {
	if _, err := platform.MustIdentity(ctx); err != nil {
		return nil, err
	}
	out := &pb.ListToolsResponse{}
	for _, a := range s.tools {
		out.Tools = append(out.Tools, convert.ToolSpecProto(a.Spec().EngineSpec(convert.DefaultToolRetry)))
	}
	sort.Slice(out.Tools, func(i, j int) bool { return out.Tools[i].Name < out.Tools[j].Name })
	return out, nil
}

type streamEmitter struct {
	mu     sync.Mutex
	stream grpc.ServerStreamingServer[pb.ExecuteResponse]
	closed bool
	count  int64
	log    *slog.Logger
	ctx    context.Context
}

func (e *streamEmitter) send(r *pb.ExecuteResponse) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return context.Canceled
	}
	return e.stream.Send(r)
}

func (e *streamEmitter) Observation(o oam.Observation) error {
	if len(o.Entities) > oam.MaxBatchEntities {
		return Errorf("batch_too_large", false, "an observation may carry at most 500 entities")
	}
	if err := o.Validate(); err != nil {
		return Errorf("invalid_observation", false, err.Error())
	}
	p, err := convert.ObservationProto(o)
	if err != nil {
		return Errorf("invalid_observation", false, err.Error())
	}
	if err := e.send(&pb.ExecuteResponse{Event: &pb.ExecuteResponse_Observation{Observation: p}}); err != nil {
		return err
	}
	e.mu.Lock()
	e.count++
	e.mu.Unlock()
	return nil
}

func (e *streamEmitter) Progress(done, total int64, msg string) error {
	return e.send(&pb.ExecuteResponse{Event: &pb.ExecuteResponse_Progress{Progress: &pb.Progress{ItemsDone: done, ItemsTotal: total, Message: msg}}})
}

func (e *streamEmitter) Skipped(target, reason string) error {
	e.log.InfoContext(e.ctx, "toolrunner.skipped", "target", target, "reason", reason)
	return e.send(&pb.ExecuteResponse{Event: &pb.ExecuteResponse_Skipped{Skipped: &pb.Skipped{Target: target, Reason: reason}}})
}

// Execute implements ToolRunnerService. Cancelling the call cancels the
// tool; the adapter gets CancelGrace to stop.
func (s *Server) Execute(req *pb.ExecuteRequest, stream grpc.ServerStreamingServer[pb.ExecuteResponse]) error {
	ctx := stream.Context()
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return err
	}
	a, ok := s.tools[req.GetTool()]
	if !ok {
		return core.Errorf(core.KindNotFound, core.ReasonNotFound, "tool %q is not registered", req.GetTool())
	}
	spec := a.Spec()
	if v := req.GetVersion(); v != "" && v != spec.Version {
		return core.Errorf(core.KindNotFound, core.ReasonNotFound, "tool %s version %s is not registered", spec.Name, v)
	}
	program, err := convert.OptionalID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return err
	}
	run, err := convert.OptionalID(core.PrefixRun, "run_id", req.GetRunId())
	if err != nil {
		return err
	}
	input, err := convert.ValueToJSON(req.GetInput())
	if err != nil {
		return convert.Invalid("input", "input: %v", err)
	}
	timeout := convert.Duration(req.GetTimeout())
	if timeout <= 0 || (spec.Timeout > 0 && spec.Timeout < timeout) {
		timeout = spec.Timeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ctx = platform.WithLogFields(ctx, req.GetRunId(), req.GetEffectPath())
	tc := &ToolCtx{Tenant: id.Tenant, Program: program, Run: run, Path: req.GetEffectPath(), ScopeVersion: req.GetScopeVersion(),
		DecisionIDs: req.GetScopeDecisionIds(), IdempotencyKey: req.GetIdempotencyKey(), Clock: s.o.Clock,
		Egress: s.o.Egress, Resolver: s.o.Resolver}
	tc.Check = func(ctx context.Context, target string, kind scope.TargetKind, class core.ActivityClass) (scope.Decision, error) {
		if s.o.Scope == nil || program.IsZero() {
			return scope.Decision{Result: scope.ResultDeny, Reason: "no_program"}, nil
		}
		return s.o.Scope.CheckTarget(ctx, scopesvc.Target{Tenant: id.Tenant, Program: program, Target: target, Kind: kind,
			Class: class, Run: run, Path: req.GetEffectPath(), Resolve: class > core.ClassPassive})
	}
	em := &streamEmitter{stream: stream, log: s.o.Logger, ctx: ctx}
	start := time.Now()
	type result struct {
		out json.RawMessage
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: Errorf("tool_panic", false, "the tool failed unexpectedly")}
			}
		}()
		out, err := a.Run(ctx, input, tc, em)
		done <- result{out, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		select {
		case res = <-done:
		case <-time.After(CancelGrace):
			s.o.Logger.WarnContext(ctx, "toolrunner.cancel_overrun", "tool", spec.Name)
			res = result{err: ctx.Err()}
		}
	}
	em.mu.Lock()
	em.closed = true
	em.mu.Unlock()
	if res.err != nil {
		if te, ok := isToolError(res.err); ok {
			return stream.Send(&pb.ExecuteResponse{Event: &pb.ExecuteResponse_Error{Error: &pb.ToolError{Code: te.Code, Message: te.Message, Retryable: te.Retryable}}})
		}
		if errors.Is(res.err, context.DeadlineExceeded) && stream.Context().Err() == nil {
			return stream.Send(&pb.ExecuteResponse{Event: &pb.ExecuteResponse_Error{Error: &pb.ToolError{Code: "timeout", Message: "tool timed out", Retryable: true}}})
		}
		return res.err
	}
	outV, err := convert.ValueFromJSON(res.out)
	if err != nil {
		return stream.Send(&pb.ExecuteResponse{Event: &pb.ExecuteResponse_Error{Error: &pb.ToolError{Code: "invalid_output", Message: err.Error()}}})
	}
	return stream.Send(&pb.ExecuteResponse{Event: &pb.ExecuteResponse_Done{Done: &pb.Done{Output: outV, Observations: em.count, Elapsed: convert.Dur(time.Since(start))}}})
}
