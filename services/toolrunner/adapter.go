// Package toolrunner is the tool runner service (spec 3.1, 6, 11.5): a
// registry of tool adapters, ListTools, and Execute, which streams
// observations, progress, structured errors, skipped targets and a final
// Done. Before an adapter touches a concrete target it re-checks scope with
// the scope service (spec 4.4, tool-runner layer); a denied target is
// reported as Skipped and never contacted. Active adapters reach targets
// only through the egress proxy (Egress), carrying the pinned decision.
//
// Adapters that work without the internet (spec 20, M7): dnsresolve
// (passive; injectable resolver), httpprobe (active; HTTP GET through the
// proxy), tcpconnect (active; CONNECT through the proxy) and mock.
package toolrunner

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
)

// Spec describes a tool (spec 6.2). The proto ToolSpec is derived from it
// (convert.ToolSpecProto) with the shared default retry policy.
type Spec struct {
	Name, Version, Description string
	InputType, OutputType      string // DSL type texts
	Observes                   []string
	Class                      core.ActivityClass
	RateRPS                    float64
	RateBurst, Concurrency     int
	Timeout                    time.Duration
	Idempotent                 bool
}

// EngineSpec converts s to the engine's tool spec (hash included).
func (s Spec) EngineSpec(retry engine.RetryPolicy) engine.ToolSpec {
	es := engine.ToolSpec{Name: s.Name, Version: s.Version, Description: s.Description, InputType: s.InputType,
		OutputType: s.OutputType, Observes: s.Observes, Idempotent: s.Idempotent, Retry: retry,
		Rate:      engine.Rate{RequestsPerSecond: s.RateRPS, Burst: s.RateBurst, Concurrency: s.Concurrency},
		TimeoutMs: s.Timeout.Milliseconds()}
	es.EffectClass = "read" // tools that touch no target (spec 7.5)
	if s.Class != core.ClassNone {
		es.EffectClass = s.Class.String()
	}
	es.SpecSHA256 = es.ComputeSHA256()
	return es
}

// Adapter is one tool (spec 6.3). Run must stream results as they are
// produced (at most oam.MaxBatchEntities entities per observation), report
// progress, return structured errors (*ToolError, with a retryable flag),
// open network connections only through tc.Egress and stop within 2 s of
// ctx being cancelled. It returns the tool's JSON output.
type Adapter interface {
	Spec() Spec
	Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error)
}

// Emitter receives an execution's stream.
type Emitter interface {
	Observation(o oam.Observation) error
	Progress(done, total int64, msg string) error
	Skipped(target, reason string) error
}

// ToolError is a structured tool failure (proto ToolError).
type ToolError struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

// Errorf returns a *ToolError.
func Errorf(code string, retryable bool, msg string) *ToolError {
	return &ToolError{Code: code, Message: msg, Retryable: retryable}
}

// ToolCtx is what an adapter may use: the call's identity and scope, the
// re-check function, the egress proxy and the resolver for passive lookups.
type ToolCtx struct {
	Tenant, Program, Run core.UUID
	Path                 string
	ScopeVersion         int32
	DecisionIDs          []string
	IdempotencyKey       string
	Clock                core.Clock
	// Check re-checks one concrete target with the scope service (resolving
	// and pinning addresses for active classes). Errors are denials.
	Check func(ctx context.Context, target string, kind scope.TargetKind, class core.ActivityClass) (scope.Decision, error)
	// Egress is the only way out for active tools; nil when the runner has
	// no proxy configured (active tools then fail with no_egress).
	Egress *Egress
	// Resolver serves passive name resolution (dnsresolve).
	Resolver scope.Resolver
}

// Allowed re-checks target and, when it is denied (or the check fails),
// reports it as Skipped and returns false. The adapter must not contact a
// target for which Allowed returned false.
func (tc *ToolCtx) Allowed(ctx context.Context, out Emitter, target string, kind scope.TargetKind, class core.ActivityClass) (scope.Decision, bool, error) {
	d, err := tc.Check(ctx, target, kind, class)
	if err != nil {
		if ctx.Err() != nil {
			return d, false, ctx.Err()
		}
		return d, false, out.Skipped(target, "scope_check_failed")
	}
	if !d.Allowed() {
		return d, false, out.Skipped(target, d.Reason)
	}
	return d, true, nil
}

// ObservationID derives a deterministic observation id for the n-th
// observation of an execution, so retries of the same effect re-emit the
// same ids and graph ingest stays idempotent.
func (tc *ToolCtx) ObservationID(parts ...string) string {
	return core.PublicID(core.PrefixObservation, engine.DerivedID(append([]string{"toolrunner", tc.IdempotencyKey, tc.Run.String(), tc.Path}, parts...)...))
}

// Now returns the clock reading.
func (tc *ToolCtx) Now() time.Time { return tc.Clock.Now().UTC() }

// ErrNoEgress is returned by active tools when no egress proxy is configured.
var ErrNoEgress = &ToolError{Code: "no_egress", Message: "the tool runner has no egress proxy configured"}

func decodeString(input json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(input, &s); err != nil {
		var obj struct {
			Name string `json:"name"`
		}
		if err2 := json.Unmarshal(input, &obj); err2 != nil || obj.Name == "" {
			return "", Errorf("invalid_input", false, "input must be a JSON string")
		}
		return obj.Name, nil
	}
	return s, nil
}

func isToolError(err error) (*ToolError, bool) {
	var te *ToolError
	ok := errors.As(err, &te)
	return te, ok
}
