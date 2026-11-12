package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/pgrepo"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Compiler adapts compiler.EngineAdapter to the engine's Compiler port by
// converting the request and result field by field (the diagnostic JSON
// shape of spec 7.7 is identical on both sides).
type Compiler struct {
	A compiler.EngineAdapter
}

var _ engine.Compiler = Compiler{}

func adapterRequest(req engine.CompileRequest) compiler.AdapterRequest {
	out := compiler.AdapterRequest{Name: req.Name, Source: req.Source}
	if req.Policy != nil {
		out.Policy = &compiler.AdapterPolicy{MaxClass: req.Policy.MaxClass}
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, compiler.AdapterTool{Name: t.Name, InputType: t.InputType, OutputType: t.OutputType,
			EffectClass: t.EffectClass, Idempotent: t.Idempotent, SpecSHA256: t.SpecSHA256})
	}
	return out
}

func checkResult(r compiler.AdapterCheckResult) (engine.CheckResult, error) {
	out := engine.CheckResult{Effects: r.Effects, Class: r.Class, OK: r.OK}
	b, err := json.Marshal(r.Diagnostics)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(b, &out.Diagnostics); err != nil {
		return out, err
	}
	return out, nil
}

// Check implements engine.Compiler.
func (c Compiler) Check(ctx context.Context, req engine.CompileRequest) (engine.CheckResult, error) {
	r, err := c.A.Check(ctx, adapterRequest(req))
	if err != nil {
		return engine.CheckResult{}, err
	}
	return checkResult(r)
}

// Compile implements engine.Compiler.
func (c Compiler) Compile(ctx context.Context, req engine.CompileRequest) (engine.CompileResult, error) {
	r, err := c.A.Compile(ctx, adapterRequest(req))
	if err != nil {
		return engine.CompileResult{}, err
	}
	cr, err := checkResult(r.AdapterCheckResult)
	if err != nil {
		return engine.CompileResult{}, err
	}
	return engine.CompileResult{CheckResult: cr, Module: r.Module, Attributes: engine.WorkflowAttributes{
		Timeout: time.Duration(r.Attributes.TimeoutNanos), MaxEffects: r.Attributes.MaxEffects, OnUnknownRetry: r.Attributes.OnUnknownRetry,
	}}, nil
}

// Executor is the engine's ToolExecutor over ToolRunnerService.Execute:
// observations are collected from the stream, Done carries the output,
// ToolError becomes *engine.ToolError and anything else is a retryable
// transport failure. Cancelling ctx cancels the stream, which cancels the
// tool.
type Executor struct {
	C      pb.ToolRunnerServiceClient
	Logger *slog.Logger
}

var _ engine.ToolExecutor = (*Executor)(nil)

// Execute implements engine.ToolExecutor.
func (x *Executor) Execute(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
	in, err := convert.ValueFromJSON(call.Input)
	if err != nil {
		return engine.ToolResult{}, &engine.ToolError{Code: "invalid_input", Message: err.Error()}
	}
	ctx = platform.AsService(ctx, call.Tenant, "engine")
	ctx = platform.WithLogFields(ctx, convert.ID(core.PrefixRun, call.RunID), call.Path)
	if call.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, call.Timeout)
		defer cancel()
	}
	stream, err := x.C.Execute(ctx, &pb.ExecuteRequest{Tool: call.Tool, Version: call.Version, Input: in,
		RunId: convert.ID(core.PrefixRun, call.RunID), EffectPath: call.Path, IdempotencyKey: call.IdempotencyKey,
		ProgramId: convert.ID(core.PrefixProgram, call.ProgramID), ScopeVersion: call.ScopeVersion,
		ScopeDecisionIds: call.DecisionIDs, Timeout: convert.Dur(call.Timeout)})
	if err != nil {
		return engine.ToolResult{}, err
	}
	var res engine.ToolResult
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return engine.ToolResult{}, errors.New("tool runner closed the stream without Done")
		}
		if err != nil {
			return engine.ToolResult{}, err
		}
		switch ev := m.GetEvent().(type) {
		case *pb.ExecuteResponse_Observation:
			o, err := convert.ObservationToJSON(ev.Observation)
			if err != nil {
				return engine.ToolResult{}, &engine.ToolError{Code: "invalid_output", Message: err.Error()}
			}
			res.Observations = append(res.Observations, o)
		case *pb.ExecuteResponse_Skipped:
			if x.Logger != nil {
				x.Logger.InfoContext(ctx, "engine.tool_skipped", "tool", call.Tool, "target", ev.Skipped.GetTarget(), "reason", ev.Skipped.GetReason())
			}
		case *pb.ExecuteResponse_Error:
			e := ev.Error
			return engine.ToolResult{}, &engine.ToolError{Code: e.GetCode(), Message: e.GetMessage(), Retryable: e.GetRetryable()}
		case *pb.ExecuteResponse_Done:
			out, err := convert.ValueToJSON(ev.Done.GetOutput())
			if err != nil {
				return engine.ToolResult{}, &engine.ToolError{Code: "invalid_output", Message: err.Error()}
			}
			res.Output = out
			return res, nil
		}
	}
}

// SyncTools feeds the engine's tool registry from ToolRunnerService.ListTools
// (spec 6.2): every listed spec is registered with DefaultToolRetry and its
// canonical hash, which must equal the runner's spec_sha256.
func SyncTools(ctx context.Context, c pb.ToolRunnerServiceClient, register func(context.Context, engine.ToolSpec) error, log *slog.Logger) (int, error) {
	resp, err := c.ListTools(platform.WithIdentity(ctx, platform.Identity{Actor: platform.ServiceActor("engine"), Role: auth.RoleAnalyst}), &pb.ListToolsRequest{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range resp.GetTools() {
		s := convert.ToolSpec(t)
		if got := fmt.Sprintf("%x", s.SpecSHA256); t.GetSpecSha256() != "" && got != t.GetSpecSha256() {
			log.WarnContext(ctx, "engine.tool_hash_mismatch", "tool", t.GetName(), "runner", t.GetSpecSha256(), "engine", got)
			continue
		}
		if err := register(ctx, s); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// PgToolStore adapts pgrepo.Tools to ToolStore.
type PgToolStore struct{ *pgrepo.Tools }

// Register implements ToolStore.
func (p PgToolStore) Register(ctx context.Context, s engine.ToolSpec) error {
	_, err := p.Tools.Register(ctx, s)
	return err
}

// Ingester is the engine's Ingester over GraphService.IngestObservations
// (one call per observation; the graph is idempotent per observation id).
type Ingester struct {
	C pb.GraphServiceClient
}

var _ engine.Ingester = (*Ingester)(nil)

// Ingest implements engine.Ingester.
func (g *Ingester) Ingest(ctx context.Context, req engine.IngestRequest) ([]engine.IngestResult, error) {
	if req.Program.IsZero() {
		return nil, nil // the graph is per program: runs without one keep no assets
	}
	ctx = platform.AsService(ctx, req.Tenant, "engine")
	out := make([]engine.IngestResult, 0, len(req.Observations))
	for _, raw := range req.Observations {
		o, err := convert.ObservationFromJSON(raw)
		if err != nil {
			return nil, err
		}
		resp, err := g.C.IngestObservations(ctx, &pb.IngestRequest{ProgramId: convert.ID(core.PrefixProgram, req.Program),
			RunId: convert.ID(core.PrefixRun, req.RunID), EffectPath: req.Path, ToolName: req.Tool, ToolVersion: req.ToolVersion, Observation: o})
		if err != nil {
			return nil, err
		}
		out = append(out, engine.IngestResult{ObservationID: o.GetObservationId(),
			EntityCount: int(resp.GetEntitiesCreated() + resp.GetEntitiesUpdated()), EdgeCount: int(resp.GetEdgesCreated() + resp.GetEdgesUpdated())})
	}
	return out, nil
}

// MemPublisher is the engine Publisher of the dev stack: envelopes go to an
// in-memory outbox that a bus.Relay publishes.
type MemPublisher struct{ Outbox *bus.MemOutbox }

// Publish implements engine.Publisher.
func (p MemPublisher) Publish(_ context.Context, envs []engine.Envelope) error {
	for _, e := range envs {
		p.Outbox.Append(bus.OutboxRow{EventID: e.EventID.String(), Topic: e.Topic, OrderingKey: e.OrderingKey,
			Payload: convert.OutboxFromEngine(e), CreatedAt: e.OccurredAt})
	}
	return nil
}

// MeteredPublisher derives the run and effect metrics of spec 18.1 from
// the events the engine publishes, then delegates.
type MeteredPublisher struct {
	Next    engine.Publisher
	Metrics *platform.Metrics
}

// Publish implements engine.Publisher.
func (p MeteredPublisher) Publish(ctx context.Context, envs []engine.Envelope) error {
	if err := p.Next.Publish(ctx, envs); err != nil {
		return err
	}
	for _, e := range envs {
		switch e.Type {
		case "run." + engine.EvRunSucceeded, "run." + engine.EvRunCancelled:
			p.Metrics.RunsTotal.Inc(strings.TrimPrefix(strings.TrimPrefix(e.Type, "run.run_"), "run."))
		case "run." + engine.EvRunFailed:
			p.Metrics.RunsTotal.Inc("failed")
			if strings.Contains(string(e.Payload), "nondeterminism_detected") {
				p.Metrics.ReplayNondeterminism.Inc()
			}
		case "run." + engine.EvEffectCompleted:
			p.Metrics.EffectsTotal.Inc("any", "completed")
		case "run." + engine.EvEffectFailed:
			p.Metrics.EffectsTotal.Inc("any", "failed")
		case "approval.pending":
			p.Metrics.ApprovalsPending.Add(1)
		case "approval.approved", "approval.denied", "approval.expired", "approval.cancelled":
			p.Metrics.ApprovalsPending.Add(-1)
		}
	}
	return nil
}

// DefaultLimitRules are the spec 14.3 key ceilings the engine applies
// (fallback "*" so no key is ever unconfigured, which would fail closed).
func DefaultLimitRules() []limiter.Rule {
	return []limiter.Rule{
		{Pattern: "rl:g:*", Params: limiter.Params{Rate: 1000, Burst: 2000}},
		{Pattern: "rl:t:*", Params: limiter.Params{Rate: 100, Burst: 200}},
		{Pattern: "rl:p:*", Params: limiter.Params{Rate: 50, Burst: 100}},
		{Pattern: "rl:h:*", Params: limiter.Params{Rate: 10, Burst: 20}},
		{Pattern: "rl:tool:*", Params: limiter.Params{Rate: 50, Burst: 100}},
		{Pattern: "*", Params: limiter.Params{Rate: 100, Burst: 200}},
	}
}

// ContextLimiter adapts a limiter that does I/O (Redis) to the engine's
// Limiter port with a per-call timeout; errors fail closed in the engine.
type ContextLimiter struct {
	L       limiter.ContextLimiter
	Timeout time.Duration
}

// AcquireAll implements engine.Limiter.
func (c ContextLimiter) AcquireAll(reqs []limiter.Request, now time.Duration) (limiter.Result, error) {
	t := c.Timeout
	if t <= 0 {
		t = 200 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()
	return c.L.AcquireAll(ctx, reqs, now)
}

// MeteredLimiter counts denials (ff_limiter_denied_total{level}).
type MeteredLimiter struct {
	Next    engine.Limiter
	Metrics *platform.Metrics
}

// AcquireAll implements engine.Limiter.
func (m MeteredLimiter) AcquireAll(reqs []limiter.Request, now time.Duration) (limiter.Result, error) {
	r, err := m.Next.AcquireAll(reqs, now)
	if err == nil && !r.Allowed {
		m.Metrics.LimiterDenied.Inc("tool_call")
	}
	return r, err
}
