package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/scope"
)

// ---- Compiler ----

// ProgramPolicy is what the compiler's policy check needs to know about
// the program a workflow is checked against (spec 7.5, rule 7).
type ProgramPolicy struct {
	ProgramID    core.UUID
	ScopeVersion int32
	MaxClass     core.ActivityClass
	Prohibited   []string
}

// CompileRequest is the input of Compiler.Check and Compiler.Compile.
type CompileRequest struct {
	// Name is the workflow name; the compiler uses "<name>.ff" as the file
	// name in diagnostics.
	Name   string
	Source string
	// Policy is the program's policy, or nil when no program is given (the
	// compiler then checks internal consistency only).
	Policy *ProgramPolicy
	// Tools are the registered tools (latest enabled version of each). The
	// compiler generates the tool declarations from them (spec 7.1) and
	// lists them in the module's TOOLS table with their SpecSHA256.
	Tools []ToolSpec
}

// CheckResult is the outcome of the front end (proto CheckWorkflowResponse).
type CheckResult struct {
	Diagnostics []Diagnostic
	// Effects are the inferred effect names of the workflow body: the DSL
	// effects of spec 7.5 (read, passive, active, intrusive, write,
	// financial_write, llm, time) and, when present, the engine effects
	// scope, approval, alert and plan.
	Effects []string
	Class   core.ActivityClass
	// OK is true when there is no error diagnostic.
	OK bool
}

// CompileResult is a CheckResult plus, when OK, the encoded module.
type CompileResult struct {
	CheckResult
	// Module is the encoded FFBC module (spec 8.3); nil unless OK.
	Module []byte
	// Attributes are the workflow annotations the engine enforces.
	Attributes WorkflowAttributes
}

// Compiler is the port to the DSL front end and code generator (spec 7,
// 8.5). Diagnostics describe problems with the source; the error result
// is only for failures of the compiler itself. The engine runs the
// bytecode verifier on every module, so a compiler bug cannot load an
// unverified module.
type Compiler interface {
	Check(ctx context.Context, req CompileRequest) (CheckResult, error)
	Compile(ctx context.Context, req CompileRequest) (CompileResult, error)
}

// ---- Repo ----

// Repo is the engine's relational state (spec 10.4) other than the event
// log, which lives in eventstore.Store and is authoritative. Every method
// that takes a tenant filters by it; the zero tenant is the system scope
// used by workers. Missing (or another tenant's) rows are ErrNotFound.
type Repo interface {
	// CreateWorkflowVersion creates the workflow (tenant, name) if needed
	// and stores v as its next version: v.Version is assigned (1, 2, ...)
	// and v.WorkflowID set. It returns the stored version.
	CreateWorkflowVersion(ctx context.Context, v WorkflowVersion) (WorkflowVersion, error)
	// GetWorkflowVersion returns version n of the tenant's workflow, or the
	// latest published version for n == 0.
	GetWorkflowVersion(ctx context.Context, tenant core.UUID, name string, n int32) (WorkflowVersion, error)
	// GetWorkflowVersionByID returns a version by id.
	GetWorkflowVersionByID(ctx context.Context, tenant, id core.UUID) (WorkflowVersion, error)
	// ListWorkflows returns the latest version of each of the tenant's
	// workflows ordered by name, after the name in token.
	ListWorkflows(ctx context.Context, tenant core.UUID, afterName string, limit int) ([]WorkflowVersion, error)
	// SetWorkflowVersionStatus sets a version's status.
	SetWorkflowVersionStatus(ctx context.Context, tenant core.UUID, name string, n int32, status string) (WorkflowVersion, error)

	// InsertRun stores the projection of a new run. With a store and repo
	// sharing one database (pgrepo) the row already exists and only the
	// fields the store does not write are set.
	InsertRun(ctx context.Context, r RunRecord) error
	// GetRun returns a run's projection.
	GetRun(ctx context.Context, tenant, id core.UUID) (RunRecord, error)
	// FindRunByIdempotencyKey returns the run submitted with key.
	FindRunByIdempotencyKey(ctx context.Context, tenant core.UUID, key string) (RunRecord, error)
	// ListRuns returns runs newest first (created_at, then id, descending)
	// that sort after the cursor (zero cursor: from the start).
	ListRuns(ctx context.Context, tenant core.UUID, f RunFilter, after RunCursor, limit int) ([]RunRecord, error)
	// ListActiveRuns returns non-terminal runs of a tenant (zero: every
	// tenant) and program (zero: any).
	ListActiveRuns(ctx context.Context, tenant, program core.UUID) ([]RunRecord, error)
	// UpdateRun replaces the projected fields of a run (status,
	// compensation outcome, output, failure, scope version, timestamps,
	// last and published seq).
	UpdateRun(ctx context.Context, r RunRecord) error

	// AddRunRequest queues a request for a run's worker.
	AddRunRequest(ctx context.Context, req RunRequest) (RunRequest, error)
	// PendingRunRequests returns the run's unapplied requests in order.
	PendingRunRequests(ctx context.Context, run core.UUID) ([]RunRequest, error)
	// MarkRunRequestsApplied marks requests as applied.
	MarkRunRequestsApplied(ctx context.Context, run core.UUID, ids []int64) error

	// UpsertApproval inserts a pending approval unless one exists for
	// (run, effect path), and returns the stored row either way.
	UpsertApproval(ctx context.Context, a Approval) (Approval, error)
	// GetApproval returns an approval.
	GetApproval(ctx context.Context, tenant, id core.UUID) (Approval, error)
	// ListApprovals returns approvals ordered by creation, after the id.
	ListApprovals(ctx context.Context, tenant core.UUID, f ApprovalFilter, after core.UUID, limit int) ([]Approval, error)
	// DecideApproval moves a pending approval to status (approved, denied,
	// expired or cancelled) with the deciding actor and comment. Only a
	// pending approval changes; changed reports whether this call did it.
	// The current row is returned in every case.
	DecideApproval(ctx context.Context, tenant, id core.UUID, status, actor, comment string, at time.Time) (a Approval, changed bool, err error)

	// UpsertTimer inserts a timer unless one with its id exists and
	// returns the stored row.
	UpsertTimer(ctx context.Context, t Timer) (Timer, error)
	// FireTimer marks a timer fired.
	FireTimer(ctx context.Context, id core.UUID, at time.Time) error

	// UpsertEffect stores the projection of an effect.
	UpsertEffect(ctx context.Context, e EffectRecord) error
	// ListEffects returns a run's effect projections in path order.
	ListEffects(ctx context.Context, run core.UUID) ([]EffectRecord, error)
}

// RunCursor is a ListRuns position: the last returned run's creation time
// and id.
type RunCursor struct {
	CreatedAt time.Time
	ID        core.UUID
}

// ---- Scope ----

// ScopeCheck is one scope decision request (proto CheckRequest).
type ScopeCheck struct {
	Tenant       core.UUID
	Program      core.UUID
	ScopeVersion int32 // the run's pinned version
	Target       string
	Kind         scope.TargetKind
	Class        core.ActivityClass
	RunID        core.UUID
	Path         string
	Now          time.Time
	// DecisionID is a deterministic id the engine proposes for the
	// decision; a scope service that assigns its own ids may ignore it.
	DecisionID string
}

// KillSwitchState reports whether a kill switch applies to a run.
type KillSwitchState struct {
	Engaged bool
	Level   string // global, tenant or program
	Reason  string
}

// Scope is the port to the scope service (spec 4, 11.3). Every error is
// treated as deny (spec 2.4 "no silent success").
type Scope interface {
	// CurrentVersion returns the scope version of a program of the tenant
	// that is in effect now: the highest version whose effective_from has
	// passed (spec 4.1), never a pending future-dated one. Runs are pinned
	// to it at submission. ErrNotFound if the program does not exist or no
	// scope document is in effect.
	CurrentVersion(ctx context.Context, tenant, program core.UUID) (int32, error)
	// Policy returns the compiled scope document of a version.
	Policy(ctx context.Context, tenant, program core.UUID, version int32) (*scope.Policy, error)
	// Check decides one target (spec 4.3) and records the decision.
	Check(ctx context.Context, req ScopeCheck) (scope.Decision, error)
	// KillSwitch reports whether a global, tenant or program kill switch
	// applies (program may be zero).
	KillSwitch(ctx context.Context, tenant, program core.UUID) (KillSwitchState, error)
}

// ---- Tools ----

// Backoff is a tool's retry backoff (spec 6.2, 9.5).
type Backoff struct {
	InitialMs  int64   `json:"initial_ms"`
	MaxMs      int64   `json:"max_ms"`
	Multiplier float64 `json:"multiplier"`
	Jitter     string  `json:"jitter,omitempty"` // "full" (the only mode)
}

// RetryPolicy is a tool's retry policy (spec 6.2). MaxAttempts 0 means a
// single attempt.
type RetryPolicy struct {
	MaxAttempts int      `json:"max_attempts"`
	RetryOn     []string `json:"retry_on,omitempty"`
	Backoff     Backoff  `json:"backoff"`
}

// Rate is a tool's rate limit (spec 6.2).
type Rate struct {
	RequestsPerSecond float64 `json:"requests_per_second"`
	Burst             int     `json:"burst"`
	Concurrency       int     `json:"concurrency"`
}

// ToolSpec is a registered tool (spec 6.2, proto ToolSpec, engine.tools).
type ToolSpec struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// InputType and OutputType are DSL type texts ("InScope<Host>",
	// "Option<HttpService>"). A call with one input sends that value as
	// the JSON input; several inputs are sent as a JSON array.
	InputType  string   `json:"input_type"`
	OutputType string   `json:"output_type"`
	Observes   []string `json:"observes,omitempty"`
	// EffectClass is the tool's declared effect (spec 7.5): passive,
	// active or intrusive for target-facing tools, otherwise read, write,
	// financial_write or llm.
	EffectClass string      `json:"effect_class"`
	Idempotent  bool        `json:"idempotent"`
	Retry       RetryPolicy `json:"retry"`
	Rate        Rate        `json:"rate"`
	// TimeoutMs bounds one execution (0: no limit beyond the run's).
	TimeoutMs int64 `json:"timeout_ms"`
	// SpecSHA256 is the hash a module's TOOLS table pins.
	SpecSHA256 [32]byte `json:"-"`
}

// ComputeSHA256 returns the canonical hash of the spec (SHA-256 of its
// canonical JSON without the hash itself).
func (s ToolSpec) ComputeSHA256() [32]byte {
	b, err := core.CanonicalJSON(s)
	if err != nil {
		return [32]byte{}
	}
	return sha256.Sum256(b)
}

// ToolRegistry is the port to the tool registry (engine.tools). The
// bytecode verifier's ToolResolver is built from it.
type ToolRegistry interface {
	// ListTools returns the latest enabled version of every tool.
	ListTools(ctx context.Context) ([]ToolSpec, error)
	// ResolveTool returns the version of the named tool whose spec hash is
	// sha (the version a module pins); ErrNotFound if there is none.
	ResolveTool(ctx context.Context, name string, sha [32]byte) (ToolSpec, error)
}

// ToolCall is one execution request (proto ExecuteRequest).
type ToolCall struct {
	Tool    string
	Version string
	// Input is the JSON input; ScopeProof arguments are unwrapped to their
	// targets.
	Input json.RawMessage
	RunID core.UUID
	Path  string
	// IdempotencyKey is hex(sha256(run_id || "/" || path)) (spec 9.5); it
	// is the same for every attempt and after crashes.
	IdempotencyKey string
	Tenant         core.UUID
	ProgramID      core.UUID
	ScopeVersion   int32
	// DecisionIDs are the scope decisions that authorize the inputs.
	DecisionIDs []string
	Timeout     time.Duration
	Attempt     int
}

// ToolResult is a successful execution (proto Done plus the streamed
// observations).
type ToolResult struct {
	Output json.RawMessage
	// Observations are observation envelopes (spec 6.1), opaque to the
	// engine apart from the fields it fills in.
	Observations []json.RawMessage
}

// ToolError is a structured tool failure (proto ToolError).
type ToolError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

// ToolExecutor is the port to the tool runner (spec 6.3, 11.5). Errors
// are *ToolError when the tool reports one; any other error is treated as
// a retryable transport failure (unavailable). Implementations must stop
// promptly when ctx is cancelled.
type ToolExecutor interface {
	Execute(ctx context.Context, call ToolCall) (ToolResult, error)
}

// ---- Graph ingest ----

// IngestRequest forwards a tool's observations to the graph.
type IngestRequest struct {
	Tenant       core.UUID
	Program      core.UUID
	RunID        core.UUID
	Path         string
	Tool         string
	ToolVersion  string
	Observations []json.RawMessage
}

// IngestResult describes one ingested observation.
type IngestResult struct {
	ObservationID string
	EntityCount   int
	EdgeCount     int
}

// Ingester is the port to GraphService.IngestObservations (spec 3.3, 6.1).
// Ingesting the same observation id twice must have no further effect.
type Ingester interface {
	Ingest(ctx context.Context, req IngestRequest) ([]IngestResult, error)
}

// ---- Limiter ----

// Limiter is the rate-limit port (spec 14). limiter.Limiter implements it.
type Limiter interface {
	AcquireAll(reqs []limiter.Request, now time.Duration) (limiter.Result, error)
}

// ---- Publisher ----

// Outbox topics (spec 23.7).
const (
	TopicRunEvents      = "run.events"
	TopicApprovalEvents = "approval.events"
)

// Envelope is an outbox message (proto EventEnvelope).
type Envelope struct {
	EventID     core.UUID // deterministic: consumers de-duplicate on it
	Topic       string
	Type        string // run.<event type> or approval.<status>
	OrderingKey string
	Tenant      core.UUID
	Program     core.UUID
	OccurredAt  time.Time
	Payload     json.RawMessage
}

// Publisher writes outbox messages. Publishing the same EventID twice must
// be harmless (the relay and consumers de-duplicate).
type Publisher interface {
	Publish(ctx context.Context, envs []Envelope) error
}

// ---- Optional ports ----

// Observer receives measurements the engine domain cannot export itself
// (metrics belong to services, spec 18.1). Calls are synchronous on the
// worker; implementations must not block.
type Observer interface {
	// RunStarted reports a run that just appended run_started, with the
	// time since its run_submitted event (both event times, so a
	// replayed or re-claimed run reports the same value).
	RunStarted(tenant core.UUID, submitToStart time.Duration)
}

// Embedder computes text embeddings (effect embed, spec 22.9).
type Embedder interface {
	Embed(ctx context.Context, tenant core.UUID, text string) ([]float64, error)
}

// SimilarityIndex answers similar(entity, k) (effect similarity_query).
type SimilarityIndex interface {
	Similar(ctx context.Context, tenant core.UUID, entity string, k int) ([]string, error)
}
