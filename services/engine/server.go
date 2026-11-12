// Package engine is the engine service (spec 3.1, 9, 11.4): EngineService
// over engine.Engine, wired with the compiler, the event store and repo
// (Postgres or in memory), the scope service client, the tool runner and
// graph clients, the limiter and the outbox; the worker pool; the
// scope.events consumer (scope changes and kill switches); and the Cloud
// Scheduler endpoint POST /internal/schedules/{id}:fire.
package engine

import (
	"context"

	"google.golang.org/grpc"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Server implements flowforge.v1.EngineService by calling engine.Engine
// with the verified caller.
type Server struct {
	pb.UnimplementedEngineServiceServer
	E *engine.Engine
}

func callerOf(ctx context.Context) (engine.Caller, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return engine.Caller{}, err
	}
	return id.EngineCaller(), nil
}

// CheckWorkflow implements EngineService.
func (s *Server) CheckWorkflow(ctx context.Context, req *pb.CheckWorkflowRequest) (*pb.CheckWorkflowResponse, error) {
	c, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	prg, err := convert.OptionalID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return nil, err
	}
	r, err := s.E.CheckWorkflow(ctx, c, engine.CheckWorkflowRequest{Source: req.GetSource(), ProgramID: prg})
	if err != nil {
		return nil, convert.WithDiagnosticDetails(err)
	}
	return &pb.CheckWorkflowResponse{Diagnostics: convert.DiagnosticsProto(r.Diagnostics), InferredEffects: r.Effects,
		InferredClass: convert.Class(r.Class), Ok: r.OK}, nil
}

// RegisterWorkflow implements EngineService.
func (s *Server) RegisterWorkflow(ctx context.Context, req *pb.RegisterWorkflowRequest) (*pb.WorkflowVersion, error) {
	c, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	prg, err := convert.OptionalID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return nil, err
	}
	v, err := s.E.RegisterWorkflow(ctx, c, engine.RegisterWorkflowRequest{Name: req.GetName(), Source: req.GetSource(), ProgramID: prg})
	if err != nil {
		return nil, convert.WithDiagnosticDetails(err)
	}
	return convert.WorkflowVersionProto(v), nil
}

// GetWorkflowVersion implements EngineService.
func (s *Server) GetWorkflowVersion(ctx context.Context, req *pb.GetWorkflowVersionRequest) (*pb.WorkflowVersion, error) {
	c, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	v, err := s.E.GetWorkflowVersion(ctx, c, req.GetName(), req.GetVersion())
	if err != nil {
		return nil, err
	}
	return convert.WorkflowVersionProto(v), nil
}

func page(p *pb.PageRequest) (engine.Page, error) {
	n, tok, err := convert.Page(p)
	return engine.Page{Size: n, Token: tok}, err
}

// ListWorkflows implements EngineService.
func (s *Server) ListWorkflows(ctx context.Context, req *pb.ListWorkflowsRequest) (*pb.ListWorkflowsResponse, error) {
	c, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	p, err := page(req.GetPage())
	if err != nil {
		return nil, err
	}
	vs, next, err := s.E.ListWorkflows(ctx, c, p)
	if err != nil {
		return nil, err
	}
	out := &pb.ListWorkflowsResponse{Page: convert.PageResp(next)}
	for _, v := range vs {
		out.Latest = append(out.Latest, convert.WorkflowVersionProto(v))
	}
	return out, nil
}

// SubmitRun implements EngineService. The idempotency key comes from the
// idempotency-key metadata (spec 11.4) and is required.
func (s *Server) SubmitRun(ctx context.Context, req *pb.SubmitRunRequest) (*pb.Run, error) {
	c, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	key := platform.IdempotencyKeyFrom(ctx)
	if key == "" {
		return nil, convert.Invalid("idempotency-key", "SubmitRun requires idempotency-key metadata")
	}
	prg, err := convert.OptionalID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return nil, err
	}
	in, err := convert.StructToJSON(req.GetInput())
	if err != nil {
		return nil, convert.Invalid("input", "%v", err)
	}
	r, err := s.E.SubmitRun(ctx, c, engine.SubmitRunRequest{WorkflowName: req.GetWorkflowName(), WorkflowVersion: req.GetWorkflowVersion(),
		ProgramID: prg, Input: in, Priority: int16(req.GetPriority()), Deadline: convert.Duration(req.GetDeadline()), IdempotencyKey: key})
	if err != nil {
		return nil, err
	}
	return convert.RunProto(r), nil
}

func runID(s string) (core.UUID, error) { return convert.ParseID(core.PrefixRun, "run_id", s) }

// GetRun implements EngineService.
func (s *Server) GetRun(ctx context.Context, req *pb.GetRunRequest) (*pb.Run, error) {
	c, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	id, err := runID(req.GetRunId())
	if err != nil {
		return nil, err
	}
	r, err := s.E.GetRun(ctx, c, id)
	if err != nil {
		return nil, err
	}
	return convert.RunProto(r), nil
}

// ListRuns implements EngineService.
func (s *Server) ListRuns(ctx context.Context, req *pb.ListRunsRequest) (*pb.ListRunsResponse, error) {
	c, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	p, err := page(req.GetPage())
	if err != nil {
		return nil, err
	}
	prg, err := convert.OptionalID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return nil, err
	}
	rs, next, err := s.E.ListRuns(ctx, c, engine.RunFilter{ProgramID: prg, Status: convert.RunStatus(req.GetStatus()),
		WorkflowName: req.GetWorkflowName(), CreatedAfter: convert.Time(req.GetCreatedAfter())}, p)
	if err != nil {
		return nil, err
	}
	out := &pb.ListRunsResponse{Page: convert.PageResp(next)}
	for _, r := range rs {
		out.Runs = append(out.Runs, convert.RunProto(r))
	}
	return out, nil
}

// CancelRun implements EngineService.
func (s *Server) CancelRun(ctx context.Context, req *pb.CancelRunRequest) (*pb.Run, error) {
	c, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	id, err := runID(req.GetRunId())
	if err != nil {
		return nil, err
	}
	r, err := s.E.CancelRun(ctx, c, id, req.GetReason())
	if err != nil {
		return nil, err
	}
	return convert.RunProto(r), nil
}

// ListRunEvents implements EngineService.
func (s *Server) ListRunEvents(ctx context.Context, req *pb.ListRunEventsRequest) (*pb.ListRunEventsResponse, error) {
	c, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	id, err := runID(req.GetRunId())
	if err != nil {
		return nil, err
	}
	p, err := page(req.GetPage())
	if err != nil {
		return nil, err
	}
	if req.GetAfterSeq() < 0 {
		return nil, convert.Invalid("after_seq", "after_seq must not be negative")
	}
	evs, next, err := s.E.ListRunEvents(ctx, c, id, uint64(req.GetAfterSeq()), req.GetTypes(), p)
	if err != nil {
		return nil, err
	}
	out := &pb.ListRunEventsResponse{Page: convert.PageResp(next)}
	for _, e := range evs {
		out.Events = append(out.Events, convert.RunEventProto(e))
	}
	return out, nil
}

// WatchRun implements EngineService: the run's events after after_seq in
// order, then new ones until the run's stream ends or the call is
// cancelled. A client resumes with the last seq it saw.
func (s *Server) WatchRun(req *pb.WatchRunRequest, stream grpc.ServerStreamingServer[pb.RunEvent]) error {
	ctx := stream.Context()
	c, err := callerOf(ctx)
	if err != nil {
		return err
	}
	id, err := runID(req.GetRunId())
	if err != nil {
		return err
	}
	if req.GetAfterSeq() < 0 {
		return convert.Invalid("after_seq", "after_seq must not be negative")
	}
	seq, err := s.E.WatchRun(ctx, c, id, uint64(req.GetAfterSeq()))
	if err != nil {
		return err
	}
	for ev, err := range seq {
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := stream.Send(convert.RunEventProto(ev)); err != nil {
			return err
		}
	}
	return nil
}

// ListApprovals implements EngineService.
func (s *Server) ListApprovals(ctx context.Context, req *pb.ListApprovalsRequest) (*pb.ListApprovalsResponse, error) {
	c, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	p, err := page(req.GetPage())
	if err != nil {
		return nil, err
	}
	run, err := convert.OptionalID(core.PrefixRun, "run_id", req.GetRunId())
	if err != nil {
		return nil, err
	}
	as, next, err := s.E.ListApprovals(ctx, c, engine.ApprovalFilter{Status: req.GetStatus(), Role: req.GetRole(), RunID: run}, p)
	if err != nil {
		return nil, err
	}
	out := &pb.ListApprovalsResponse{Page: convert.PageResp(next)}
	for _, a := range as {
		out.Approvals = append(out.Approvals, convert.ApprovalProto(a))
	}
	return out, nil
}

// GetApproval implements EngineService.
func (s *Server) GetApproval(ctx context.Context, req *pb.GetApprovalRequest) (*pb.Approval, error) {
	c, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	id, err := convert.ParseID(core.PrefixApproval, "approval_id", req.GetApprovalId())
	if err != nil {
		return nil, err
	}
	a, err := s.E.GetApproval(ctx, c, id)
	if err != nil {
		return nil, err
	}
	return convert.ApprovalProto(a), nil
}

// DecideApproval implements EngineService.
func (s *Server) DecideApproval(ctx context.Context, req *pb.DecideApprovalRequest) (*pb.Approval, error) {
	c, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	id, err := convert.ParseID(core.PrefixApproval, "approval_id", req.GetApprovalId())
	if err != nil {
		return nil, err
	}
	var d string
	switch req.GetDecision() {
	case pb.ApprovalDecision_APPROVAL_DECISION_APPROVE:
		d = engine.DecisionApprove
	case pb.ApprovalDecision_APPROVAL_DECISION_DENY:
		d = engine.DecisionDeny
	default:
		return nil, convert.Invalid("decision", "decision must be APPROVE or DENY")
	}
	a, err := s.E.DecideApproval(ctx, c, id, d, req.GetComment())
	if err != nil {
		return nil, err
	}
	return convert.ApprovalProto(a), nil
}

// ResolveUnknownEffect implements EngineService.
func (s *Server) ResolveUnknownEffect(ctx context.Context, req *pb.ResolveUnknownEffectRequest) (*pb.Run, error) {
	c, err := callerOf(ctx)
	if err != nil {
		return nil, err
	}
	id, err := runID(req.GetRunId())
	if err != nil {
		return nil, err
	}
	var result []byte
	if req.GetResult() != nil {
		if result, err = convert.ValueToJSON(req.GetResult()); err != nil {
			return nil, convert.Invalid("result", "%v", err)
		}
	}
	r, err := s.E.ResolveUnknownEffect(ctx, c, id, req.GetEffectPath(), req.GetResolution(), result)
	if err != nil {
		return nil, err
	}
	return convert.RunProto(r), nil
}
