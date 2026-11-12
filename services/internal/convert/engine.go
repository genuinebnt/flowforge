package convert

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/eventstore"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
)

// DiagnosticProto converts an engine (spec 7.7) diagnostic.
func DiagnosticProto(d engine.Diagnostic) *pb.Diagnostic {
	out := &pb.Diagnostic{Code: d.Code, Message: d.Message, Help: d.Help, Primary: spanProto(d.Primary)}
	switch d.Severity {
	case "error":
		out.Severity = pb.Diagnostic_SEVERITY_ERROR
	case "warning":
		out.Severity = pb.Diagnostic_SEVERITY_WARNING
	case "hint":
		out.Severity = pb.Diagnostic_SEVERITY_HINT
	}
	for _, s := range d.Secondary {
		out.Secondary = append(out.Secondary, spanProto(s))
	}
	return out
}

func spanProto(s engine.SourceSpan) *pb.SourceSpan {
	return &pb.SourceSpan{File: s.File, Label: s.Label,
		Start: &pb.SourcePos{Line: int32(s.Start.Line), Col: int32(s.Start.Col), Offset: int32(s.Start.Offset)},
		End:   &pb.SourcePos{Line: int32(s.End.Line), Col: int32(s.End.Col), Offset: int32(s.End.Offset)}}
}

// DiagnosticsProto converts a list of diagnostics.
func DiagnosticsProto(ds []engine.Diagnostic) []*pb.Diagnostic {
	out := make([]*pb.Diagnostic, 0, len(ds))
	for _, d := range ds {
		out = append(out, DiagnosticProto(d))
	}
	return out
}

// DiagnosticsFromJSON converts any diagnostics in the spec 7.7 JSON shape
// (dsl, alerts and engine diagnostics share it).
func DiagnosticsFromJSON(v any) []*pb.Diagnostic {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var ds []engine.Diagnostic
	if json.Unmarshal(b, &ds) != nil {
		return nil
	}
	return DiagnosticsProto(ds)
}

// WithDiagnosticDetails attaches the diagnostics of a DSL_DIAGNOSTICS error
// as proto details so they reach the client (spec 11.8).
func WithDiagnosticDetails(err error) error {
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Reason != core.ReasonDSLDiagnostics {
		return err
	}
	out := *ce
	out.Details = nil
	for _, d := range ce.Details {
		switch v := d.(type) {
		case engine.Diagnostic:
			out.Details = append(out.Details, DiagnosticProto(v))
		case []engine.Diagnostic:
			for _, x := range v {
				out.Details = append(out.Details, DiagnosticProto(x))
			}
		default:
			for _, x := range DiagnosticsFromJSON([]any{v}) {
				out.Details = append(out.Details, x)
			}
		}
	}
	return &out
}

// WorkflowVersionProto converts a workflow version.
func WorkflowVersionProto(v engine.WorkflowVersion) *pb.WorkflowVersion {
	return &pb.WorkflowVersion{
		Id: ID(core.PrefixWorkflowVersion, v.ID), Name: v.Name, Version: v.Version, Status: v.Status,
		ModuleSha256: hex.EncodeToString(v.ModuleSHA256[:]), Effects: v.Effects, InferredClass: Class(v.InferredClass),
		Diagnostics: DiagnosticsProto(v.Diagnostics), Source: v.Source, CreatedAt: TS(v.CreatedAt), CreatedBy: v.CreatedBy,
	}
}

// RunStatusProto converts a run status.
func RunStatusProto(s engine.RunStatus) pb.RunStatus {
	switch s {
	case eventstore.StatusPending:
		return pb.RunStatus_RUN_STATUS_PENDING
	case eventstore.StatusRunning:
		return pb.RunStatus_RUN_STATUS_RUNNING
	case eventstore.StatusWaiting:
		return pb.RunStatus_RUN_STATUS_WAITING
	case eventstore.StatusPaused:
		return pb.RunStatus_RUN_STATUS_PAUSED
	case eventstore.StatusCancelling:
		return pb.RunStatus_RUN_STATUS_CANCELLING
	case eventstore.StatusCompensating:
		return pb.RunStatus_RUN_STATUS_COMPENSATING
	case eventstore.StatusSucceeded:
		return pb.RunStatus_RUN_STATUS_SUCCEEDED
	case eventstore.StatusFailed:
		return pb.RunStatus_RUN_STATUS_FAILED
	case eventstore.StatusCancelled:
		return pb.RunStatus_RUN_STATUS_CANCELLED
	}
	return pb.RunStatus_RUN_STATUS_UNSPECIFIED
}

// RunStatus converts a proto run status ("" for UNSPECIFIED).
func RunStatus(s pb.RunStatus) engine.RunStatus {
	for _, x := range []engine.RunStatus{eventstore.StatusPending, eventstore.StatusRunning, eventstore.StatusWaiting,
		eventstore.StatusPaused, eventstore.StatusCancelling, eventstore.StatusCompensating, eventstore.StatusSucceeded,
		eventstore.StatusFailed, eventstore.StatusCancelled} {
		if RunStatusProto(x) == s {
			return x
		}
	}
	return ""
}

// RunProto converts a run.
func RunProto(r engine.Run) *pb.Run {
	out := &pb.Run{
		Id: ID(core.PrefixRun, r.ID), ProgramId: ID(core.PrefixProgram, r.ProgramID), WorkflowName: r.WorkflowName,
		WorkflowVersion: r.WorkflowVersion, ScopeVersion: r.ScopeVersion, Status: RunStatusProto(r.Status),
		CompensationOutcome: r.CompensationOutcome, FailureCode: r.FailureCode, FailureMessage: r.FailureMessage,
		CreatedBy: r.CreatedBy, CreatedAt: TS(r.CreatedAt), StartedAt: TS(r.StartedAt), FinishedAt: TS(r.FinishedAt),
		LastSeq: int64(r.LastSeq), ParentRunId: ID(core.PrefixRun, r.ParentRunID),
	}
	out.Input = MustStruct(r.Input)
	if len(r.Output) > 0 {
		out.Output, _ = ValueFromJSON(r.Output)
	}
	return out
}

// RunEventProto converts a run log event.
func RunEventProto(e eventstore.Event) *pb.RunEvent {
	return &pb.RunEvent{RunId: ID(core.PrefixRun, e.RunID), Seq: int64(e.Seq), Type: e.Type, At: TS(e.At),
		Actor: e.Actor, Payload: MustStruct(e.Payload)}
}

// ApprovalProto converts an approval.
func ApprovalProto(a engine.Approval) *pb.Approval {
	return &pb.Approval{
		Id: ID(core.PrefixApproval, a.ID), RunId: ID(core.PrefixRun, a.RunID), EffectPath: a.EffectPath, Role: a.Role,
		Reason: a.Reason, Proposed: MustStruct(a.Proposed), Status: a.Status, ExpiresAt: TS(a.ExpiresAt),
		DecidedBy: a.DecidedBy, DecidedAt: TS(a.DecidedAt), Comment: a.Comment, CreatedAt: TS(a.CreatedAt),
	}
}

// DefaultToolRetry is the retry policy attached to every tool the engine
// learns from ToolRunnerService.ListTools: the proto ToolSpec has no retry
// field, so both sides use this constant and compute the same spec hash.
var DefaultToolRetry = engine.RetryPolicy{
	MaxAttempts: 3, RetryOn: []string{"unavailable", "deadline_exceeded"},
	Backoff: engine.Backoff{InitialMs: 1000, MaxMs: 30000, Multiplier: 2, Jitter: "full"},
}

// ToolSpecProto converts an engine tool spec. spec_sha256 is the spec's
// canonical hash (engine.ToolSpec.ComputeSHA256).
func ToolSpecProto(s engine.ToolSpec) *pb.ToolSpec {
	sha := s.ComputeSHA256()
	cls := pb.ActivityClass_ACTIVITY_CLASS_UNSPECIFIED
	if c, err := core.ParseActivityClass(s.EffectClass); err == nil {
		cls = Class(c)
	}
	return &pb.ToolSpec{
		Name: s.Name, Version: s.Version, Description: s.Description, InputType: s.InputType, OutputType: s.OutputType,
		Observes: s.Observes, EffectClass: cls, RateRps: s.Rate.RequestsPerSecond, RateBurst: int32(s.Rate.Burst),
		Concurrency: int32(s.Rate.Concurrency), Timeout: Dur(time.Duration(s.TimeoutMs) * time.Millisecond),
		Idempotent: s.Idempotent, SpecSha256: hex.EncodeToString(sha[:]),
	}
}

// ToolSpec converts a proto tool spec, attaching DefaultToolRetry and
// computing SpecSHA256.
func ToolSpec(p *pb.ToolSpec) engine.ToolSpec {
	s := engine.ToolSpec{
		Name: p.GetName(), Version: p.GetVersion(), Description: p.GetDescription(), InputType: p.GetInputType(),
		OutputType: p.GetOutputType(), Observes: p.GetObserves(), Idempotent: p.GetIdempotent(), Retry: DefaultToolRetry,
		Rate:      engine.Rate{RequestsPerSecond: p.GetRateRps(), Burst: int(p.GetRateBurst()), Concurrency: int(p.GetConcurrency())},
		TimeoutMs: Duration(p.GetTimeout()).Milliseconds(),
	}
	// The proto carries only target-facing classes; UNSPECIFIED is a tool
	// that touches no target (effect "read", spec 7.5).
	s.EffectClass = "read"
	if c := ClassFrom(p.GetEffectClass()); c != core.ClassNone {
		s.EffectClass = c.String()
	}
	s.SpecSHA256 = s.ComputeSHA256()
	return s
}
