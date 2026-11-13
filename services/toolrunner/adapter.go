// Package toolrunner is the tool runner service (spec 3.1, 6, 11.5): a
// registry of tool adapters, ListTools, and Execute, which streams
// observations, progress, structured errors, skipped targets and a final
// Done. Before an adapter touches a concrete target it re-checks scope with
// the scope service (spec 4.4, tool-runner layer); a denied target is
// reported as Skipped and never contacted. Active adapters reach targets
// only through the egress proxy (Egress), carrying the pinned decision.
//
// Adapters that work without the internet (spec 20, M7): dnsresolve
// (passive; through the proxy's DNS forwarder), httpprobe (active; HTTP GET through the
// proxy), tcpconnect (active; CONNECT through the proxy) and mock. The v2
// tools add portscan, tlscert, ctlog, rdap, asn and the content tools of
// V2_DESIGN §8 (ContentAdapters: collect, crawl, wellknown, extract,
// unpack_sourcemap, diff_latest, search_changes), which store what they
// fetch with the content service.
package toolrunner

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
)

// Spec describes a tool (spec 6.2). The proto ToolSpec is derived from it
// (convert.ToolSpecProto), retry policy and idempotency class included.
type Spec struct {
	Name, Version, Description string
	InputType, OutputType      string // DSL type texts
	Observes                   []string
	Class                      core.ActivityClass
	RateRPS                    float64
	RateBurst, Concurrency     int
	Timeout                    time.Duration
	Idempotent                 bool
	// Idempotency is the engine.Idempotency* class ("" derives it from
	// Idempotent).
	Idempotency string
	// Retry is the tool's retry policy (spec 6.2 "retry"); the zero value
	// means the default passed to EngineSpec.
	Retry engine.RetryPolicy
}

// Retry policies of the built-in tools (spec 6.2, 9.5). Every code they
// list is one the runner reports with retryable set: "unavailable" (a
// transport or upstream failure), "deadline_exceeded" (the call's or the
// tool's own timeout) and, for the content tools, "content_unavailable".
// A rate-limit denial at the egress proxy is not in any list: the runner
// reports it as rate_limited with a retry delay, which the engine waits
// out without counting an attempt.
var (
	// RetryTransient is the policy of target-facing tools: 3 attempts,
	// 1 s doubling to 30 s, full jitter.
	RetryTransient = engine.RetryPolicy{MaxAttempts: 3, RetryOn: []string{"unavailable", "deadline_exceeded"},
		Backoff: engine.Backoff{InitialMs: 1000, MaxMs: 30000, Multiplier: 2, Jitter: "full"}}
	// RetryIntel is the policy of tools that query public intelligence
	// sources (crt.sh, RDAP, Team Cymru), which are slow and shed load: 4
	// attempts, 2 s doubling to 60 s.
	RetryIntel = engine.RetryPolicy{MaxAttempts: 4, RetryOn: []string{"unavailable", "deadline_exceeded"},
		Backoff: engine.Backoff{InitialMs: 2000, MaxMs: 60000, Multiplier: 2, Jitter: "full"}}
	// RetryContent is RetryTransient plus content_unavailable (the content
	// service was unreachable).
	RetryContent = engine.RetryPolicy{MaxAttempts: 3, RetryOn: []string{"unavailable", "deadline_exceeded", "content_unavailable"},
		Backoff: engine.Backoff{InitialMs: 1000, MaxMs: 30000, Multiplier: 2, Jitter: "full"}}
	// RetryNever is one attempt (tools whose failures are deterministic).
	RetryNever = engine.RetryPolicy{MaxAttempts: 1}
)

// EngineSpec converts s to the engine's tool spec (hash included);
// defaultRetry applies when s declares no policy.
func (s Spec) EngineSpec(defaultRetry engine.RetryPolicy) engine.ToolSpec {
	retry := s.Retry
	if retry.MaxAttempts == 0 {
		retry = defaultRetry
	}
	es := engine.ToolSpec{Name: s.Name, Version: s.Version, Description: s.Description, InputType: s.InputType,
		OutputType: s.OutputType, Observes: s.Observes, Idempotent: s.Idempotent, Idempotency: s.Idempotency, Retry: retry,
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

// ToolError is a structured tool failure (proto ToolError). RetryAfter is
// set with code rate_limited when the network edge denied a request.
type ToolError struct {
	Code       string
	Message    string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

// Errorf returns a *ToolError.
func Errorf(code string, retryable bool, msg string) *ToolError {
	return &ToolError{Code: code, Message: msg, Retryable: retryable}
}

// ToolCtx is what an adapter may use: the call's identity and scope, the
// re-check function and the egress proxy.
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
	// CheckPinned is Check with resolution and pinning whatever the class
	// (passive tools that open a connection, such as tlscert). Nil: Check.
	CheckPinned func(ctx context.Context, target string, kind scope.TargetKind, class core.ActivityClass) (scope.Decision, error)
	// Egress is the only way out for active tools; nil when the runner has
	// no proxy configured (active tools then fail with no_egress).
	Egress *Egress
	// Net, when set, replaces Egress as the route out (tests use recorded
	// fixtures).
	Net Network
	// Raw receives the raw protocol bytes the egress paths read (nil:
	// none are recorded; G-SVC-16).
	Raw *RawCapture

	edgeMu    sync.Mutex
	edgeKill  bool
	edgeWait  time.Duration
	edgeLimit int // rate-limit refusals no adapter took over
}

// Proxy refusal reasons that describe the network edge rather than the
// target (services/egressproxy).
const (
	refusedKillSwitch  = "kill_switch"
	refusedRateLimited = "rate_limited"
	refusedConcurrency = "concurrency_limited"
)

// concurrencyWait is the retry delay of a concurrency_limited refusal,
// which carries none: about one request time.
const concurrencyWait = time.Second

// noteRefusal remembers proxy refusals that concern the whole execution:
// a kill switch, and per-target rate or concurrency limits (with the
// longest retry delay). Execute reports them in place of the tool's
// result (edgeOutcome).
func (tc *ToolCtx) noteRefusal(re *RefusedError) {
	if tc == nil || re == nil {
		return
	}
	tc.edgeMu.Lock()
	defer tc.edgeMu.Unlock()
	switch re.Reason {
	case refusedKillSwitch:
		tc.edgeKill = true
	case refusedRateLimited:
		tc.edgeWait = max(tc.edgeWait, re.RetryAfter, time.Millisecond)
		tc.edgeLimit++
	case refusedConcurrency:
		tc.edgeWait = max(tc.edgeWait, re.RetryAfter, concurrencyWait)
		tc.edgeLimit++
	}
}

// RetryingEdgeLimit is called by an adapter that retries, itself, a
// request the proxy refused with rate_limited or concurrency_limited
// (portscan paces and retries): it takes that refusal over, so Execute
// does not report the execution as rate-limited for it. A refusal the
// adapter gives up on is not taken over.
func (tc *ToolCtx) RetryingEdgeLimit() {
	tc.edgeMu.Lock()
	defer tc.edgeMu.Unlock()
	tc.edgeLimit = max(tc.edgeLimit-1, 0)
}

// edgeOutcome is the execution's result as the network edge decided it:
// a kill-switch refusal (engine.CodeKillSwitch: the switch was engaged
// after the engine's re-check, a race the engine reports), else a
// rate-limit wait (engine.CodeRateLimited with the delay, which the engine
// waits out without counting an attempt), else nil. Either overrides what
// the adapter returned, since its result lacks what the edge refused.
func (tc *ToolCtx) edgeOutcome() *ToolError {
	tc.edgeMu.Lock()
	defer tc.edgeMu.Unlock()
	switch {
	case tc.edgeKill:
		return &ToolError{Code: engine.CodeKillSwitch, Message: "the egress proxy refused a request: a kill switch is engaged"}
	case tc.edgeLimit > 0:
		return &ToolError{Code: engine.CodeRateLimited, Message: "the egress proxy's per-target limit refused a request",
			Retryable: true, RetryAfter: tc.edgeWait}
	}
	return nil
}

// Network returns the route out: Net, else Egress, else nil.
func (tc *ToolCtx) Network() Network {
	if tc.Net != nil {
		return tc.Net
	}
	if tc.Egress != nil {
		return tc.Egress
	}
	return nil
}

// AllowedPinned is Allowed with a resolving, pinning check (CheckPinned).
func (tc *ToolCtx) AllowedPinned(ctx context.Context, out Emitter, target string, kind scope.TargetKind, class core.ActivityClass) (scope.Decision, bool, error) {
	check := tc.CheckPinned
	if check == nil {
		check = tc.Check
	}
	return allowedWith(ctx, check, out, target, kind, class)
}

// Allowed re-checks target and, when it is denied (or the check fails),
// reports it as Skipped and returns false. The adapter must not contact a
// target for which Allowed returned false.
func (tc *ToolCtx) Allowed(ctx context.Context, out Emitter, target string, kind scope.TargetKind, class core.ActivityClass) (scope.Decision, bool, error) {
	return allowedWith(ctx, tc.Check, out, target, kind, class)
}

func allowedWith(ctx context.Context, check func(context.Context, string, scope.TargetKind, core.ActivityClass) (scope.Decision, error),
	out Emitter, target string, kind scope.TargetKind, class core.ActivityClass) (scope.Decision, bool, error) {
	d, err := check(ctx, target, kind, class)
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

// decodeHost accepts a JSON string, {"name": s} or the DSL Host enum
// {"variant": "Name"|"Ip", "values": [s]}.
func decodeHost(input json.RawMessage) (string, error) {
	var v struct {
		Variant string   `json:"variant"`
		Values  []string `json:"values"`
	}
	if err := json.Unmarshal(input, &v); err == nil && v.Variant != "" && len(v.Values) == 1 {
		return v.Values[0], nil
	}
	return decodeString(input)
}

// decodeArgs splits a tuple input [a, b] into exactly n raw values.
func decodeArgs(input json.RawMessage, n int) ([]json.RawMessage, error) {
	var args []json.RawMessage
	if err := json.Unmarshal(input, &args); err != nil || len(args) != n {
		return nil, Errorf("invalid_input", false, "input must be a tuple of "+string(rune('0'+n))+" values")
	}
	return args, nil
}

func isToolError(err error) (*ToolError, bool) {
	var te *ToolError
	ok := errors.As(err, &te)
	return te, ok
}
