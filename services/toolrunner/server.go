package toolrunner

import (
	"context"
	"crypto/sha256"
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
	// Net, when set, replaces Egress as every execution's route out
	// (ToolCtx.Net; tests use recorded fixtures).
	Net    Network
	Clock  core.Clock
	Logger *slog.Logger
	// Raw, when set, keeps the raw protocol bytes of each execution (HTTP
	// responses, DNS messages, TLS handshake records, as the egress paths
	// read them; HTTP redacted) as an FFRW transcript; its ref becomes the
	// raw_ref of the execution's observations (spec 10.5
	// graph.observations.raw_ref; G-OPS-14, G-SVC-16). An observation of
	// an execution that captured nothing has no raw_ref.
	Raw RawStore
	// RawLimit bounds the bytes captured per execution (default
	// DefaultRawLimit).
	RawLimit int
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

// DefaultAdapters are the built-in tools: dnsresolve, httpprobe, portscan
// (and its alias tcpconnect), tlscert, ctlog, rdap, asn and mock.
func DefaultAdapters(probe HTTPProbe) []Adapter {
	return []Adapter{DNSResolve{}, probe, PortScan{}, TCPConnect{}, TLSCert{}, &CTLog{}, &RDAP{}, &ASN{}, Mock{}}
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
	tool   oam.Tool
	raw    RawStore
	cap    *RawCapture
	tenant core.UUID
	clock  core.Clock
	// lastRaw and lastRef are the transcript last stored and its ref, so
	// observations emitted with nothing captured in between share it.
	lastRaw [32]byte
	lastRef string
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
	if o.Tool.Name == "" {
		o.Tool = e.tool
	}
	if err := o.Validate(); err != nil {
		return Errorf("invalid_observation", false, err.Error())
	}
	if e.raw != nil && o.RawRef == "" {
		o.RawRef = e.rawRef(o.ObservationID)
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

// rawRef stores the transcript captured so far and returns its ref ("" when
// nothing was captured or the store failed). Raw bytes are evidence, not
// the result: a failed write is logged and the observation goes out
// without a ref. Identical transcripts (no new bytes since the last
// observation) reuse the last ref.
func (e *streamEmitter) rawRef(obsID string) string {
	data := e.cap.Transcript()
	if data == nil {
		return ""
	}
	sum := sha256.Sum256(data)
	e.mu.Lock()
	if e.lastRef != "" && sum == e.lastRaw {
		ref := e.lastRef
		e.mu.Unlock()
		return ref
	}
	e.mu.Unlock()
	ref, err := e.raw.PutRaw(e.ctx, e.tenant, e.clock.Now(), data)
	if err != nil {
		e.log.WarnContext(e.ctx, "toolrunner.raw_write_failed", "observation_id", obsID, "error", err.Error())
		return ""
	}
	e.mu.Lock()
	e.lastRaw, e.lastRef = sum, ref
	e.mu.Unlock()
	return ref
}

func (e *streamEmitter) Progress(done, total int64, msg string) error {
	return e.send(&pb.ExecuteResponse{Event: &pb.ExecuteResponse_Progress{Progress: &pb.Progress{ItemsDone: done, ItemsTotal: total, Message: msg}}})
}

func (e *streamEmitter) Skipped(target, reason string) error {
	e.log.InfoContext(e.ctx, "toolrunner.skipped", "target", target, "reason", reason)
	return e.send(&pb.ExecuteResponse{Event: &pb.ExecuteResponse_Skipped{Skipped: &pb.Skipped{Target: target, Reason: reason}}})
}

func toolErrorProto(te *ToolError) *pb.ToolError {
	p := &pb.ToolError{Code: te.Code, Message: te.Message, Retryable: te.Retryable}
	if te.RetryAfter > 0 {
		p.RetryAfter = convert.Dur(te.RetryAfter)
	}
	return p
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
		Egress: s.o.Egress, Net: s.o.Net}
	if s.o.Raw != nil {
		tc.Raw = NewRawCapture(s.o.RawLimit)
	}
	check := func(resolve bool) func(ctx context.Context, target string, kind scope.TargetKind, class core.ActivityClass) (scope.Decision, error) {
		return func(ctx context.Context, target string, kind scope.TargetKind, class core.ActivityClass) (scope.Decision, error) {
			if s.o.Scope == nil || program.IsZero() {
				return scope.Decision{Result: scope.ResultDeny, Reason: "no_program"}, nil
			}
			return s.o.Scope.CheckTarget(ctx, scopesvc.Target{Tenant: id.Tenant, Program: program, Target: target, Kind: kind,
				Class: class, Run: run, Path: req.GetEffectPath(), Resolve: resolve || class > core.ClassPassive})
		}
	}
	tc.Check, tc.CheckPinned = check(false), check(true)
	em := &streamEmitter{stream: stream, log: s.o.Logger, ctx: ctx, tool: oam.Tool{Name: spec.Name, Version: spec.Version}, raw: s.o.Raw,
		cap: tc.Raw, tenant: id.Tenant, clock: s.o.Clock}
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
	if te := tc.edgeOutcome(); te != nil && stream.Context().Err() == nil {
		res.err = te
	}
	if res.err != nil {
		if te, ok := isToolError(res.err); ok {
			return stream.Send(&pb.ExecuteResponse{Event: &pb.ExecuteResponse_Error{Error: toolErrorProto(te)}})
		}
		if errors.Is(res.err, context.DeadlineExceeded) && stream.Context().Err() == nil {
			// The tool's own timeout: spec 9.5 "deadline exceeded" (the code
			// the retry policies list).
			return stream.Send(&pb.ExecuteResponse{Event: &pb.ExecuteResponse_Error{Error: &pb.ToolError{Code: "deadline_exceeded", Message: "tool timed out", Retryable: true}}})
		}
		return res.err
	}
	outV, err := convert.ValueFromJSON(res.out)
	if err != nil {
		return stream.Send(&pb.ExecuteResponse{Event: &pb.ExecuteResponse_Error{Error: &pb.ToolError{Code: "invalid_output", Message: err.Error()}}})
	}
	return stream.Send(&pb.ExecuteResponse{Event: &pb.ExecuteResponse_Done{Done: &pb.Done{Output: outV, Observations: em.count, Elapsed: convert.Dur(time.Since(start))}}})
}
