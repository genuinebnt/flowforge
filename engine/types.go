package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
)

// Caller identifies who calls an Engine method. The gateway authenticates
// and authorizes (permissions of spec 13.6); the engine enforces tenant
// isolation on every query, the approval role match and the self-approval
// rule (spec 9.7).
type Caller struct {
	// Tenant scopes every read and write. Another tenant's resource is
	// reported as NOT_FOUND (spec 11.8).
	Tenant core.UUID
	// Actor is "user:<id>", "apikey:<id>" or "schedule:<id>".
	Actor string
	// Roles are the roles the actor holds in the tenant (built-in roles of
	// spec 13.6 and custom approval roles such as "finance_ap").
	Roles []string
}

// HasRole reports whether the caller holds role.
func (c Caller) HasRole(role string) bool { return slices.Contains(c.Roles, role) }

// IsUser reports whether the actor is an interactive user (the only kind
// of requester the self-approval rule applies to).
func (c Caller) IsUser() bool { return strings.HasPrefix(c.Actor, "user:") }

// RunStatus is the status of a run (spec 9.2). It is the eventstore's
// type, so the scheduler state and the projection share one vocabulary.
type RunStatus = eventstore.RunStatus

// Run statuses (spec 9.2).
const (
	StatusPending      = eventstore.StatusPending
	StatusRunning      = eventstore.StatusRunning
	StatusWaiting      = eventstore.StatusWaiting
	StatusPaused       = eventstore.StatusPaused
	StatusCancelling   = eventstore.StatusCancelling
	StatusCompensating = eventstore.StatusCompensating
	StatusSucceeded    = eventstore.StatusSucceeded
	StatusFailed       = eventstore.StatusFailed
	StatusCancelled    = eventstore.StatusCancelled
)

// Terminal reports whether s is succeeded, failed or cancelled.
func Terminal(s RunStatus) bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusCancelled
}

// Compensation outcomes carried by failed and cancelled runs (spec 9.2).
const (
	CompensationNone      = "none"
	CompensationCompleted = "completed"
	CompensationFailed    = "failed"
)

// Workflow version statuses (spec 10.4).
const (
	VersionPublished  = "published"
	VersionDeprecated = "deprecated"
)

// SourcePos is a position in a source file (spec 7.7).
type SourcePos struct {
	Line   int `json:"line"`
	Col    int `json:"col"`
	Offset int `json:"offset"`
}

// SourceSpan is a labelled source range (spec 7.7).
type SourceSpan struct {
	File  string    `json:"file"`
	Start SourcePos `json:"start"`
	End   SourcePos `json:"end"`
	Label string    `json:"label"`
}

// Diagnostic is a compiler message in the JSON shape of spec 7.7.
type Diagnostic struct {
	Code      string       `json:"code"`
	Severity  string       `json:"severity"` // error, warning or hint
	Message   string       `json:"message"`
	Primary   SourceSpan   `json:"primary"`
	Secondary []SourceSpan `json:"secondary"`
	Help      string       `json:"help,omitempty"`
}

// IsError reports whether d is an error diagnostic.
func (d Diagnostic) IsError() bool { return d.Severity == "error" }

// HasErrors reports whether any diagnostic is an error.
func HasErrors(ds []Diagnostic) bool { return slices.ContainsFunc(ds, Diagnostic.IsError) }

// DiagAgentEffectsDisabled is the code of the registration diagnostic for
// workflows that use agent_call or execute_plan (docs/DECISIONS.md D1).
const DiagAgentEffectsDisabled = "E0490"

// WorkflowAttributes are the workflow-level annotations the engine
// enforces at run time (spec 7.5). They travel in the module's ANNOTS
// section (bytecode.Annotations); a compiler may also report them next to
// the module, and the engine stores the merged result with the version.
type WorkflowAttributes struct {
	// Timeout is #[timeout(d)] on the workflow: the run deadline.
	Timeout time.Duration `json:"timeout,omitempty"`
	// MaxEffects is #[effects(max = N)] (0: the default of 10,000).
	MaxEffects int `json:"max_effects,omitempty"`
	// OnUnknownRetry is #[on_unknown(retry)]: an effect whose outcome is
	// unknown after a crash is re-executed instead of waiting for an
	// operator, even when its tool is not idempotent.
	OnUnknownRetry bool `json:"on_unknown_retry,omitempty"`
}

// WorkflowVersion is an immutable published workflow version (spec 9.1,
// 10.4, proto WorkflowVersion).
type WorkflowVersion struct {
	ID            core.UUID
	WorkflowID    core.UUID
	Tenant        core.UUID
	Name          string
	Version       int32
	Status        string // published or deprecated
	Source        string
	SourceSHA256  [32]byte
	Module        []byte // encoded FFBC module
	ModuleSHA256  [32]byte
	Effects       []string // inferred effect names (spec 7.5 plus scope, approval, alert)
	InferredClass core.ActivityClass
	Diagnostics   []Diagnostic // warnings and hints of a successful compile
	Attributes    WorkflowAttributes
	// ProgramID is the program the version was policy-checked against at
	// registration (zero: none).
	ProgramID core.UUID
	CreatedBy string
	CreatedAt time.Time
}

// TargetFacing reports whether runs of the version act on targets and
// therefore need a program with a scope document.
func (v WorkflowVersion) TargetFacing() bool {
	return v.InferredClass > core.ClassNone || slices.Contains(v.Effects, "scope")
}

// RunRecord is the queryable projection of a run (engine.runs, spec 10.4).
// The event log is authoritative; the engine repairs the record from it.
type RunRecord struct {
	ID                  core.UUID
	Tenant              core.UUID
	ProgramID           core.UUID // zero: none
	WorkflowVersionID   core.UUID
	WorkflowName        string
	WorkflowVersion     int32
	ParentRunID         core.UUID // zero: none
	ScopeVersion        int32
	Status              RunStatus
	CompensationOutcome string
	Input               json.RawMessage
	Output              json.RawMessage // nil until succeeded
	FailureCode         string
	FailureMessage      string
	CreatedBy           string
	IdempotencyKey      string
	RequestHash         []byte
	Priority            int16
	DeadlineAt          time.Time // zero: none
	CreatedAt           time.Time
	StartedAt           time.Time
	FinishedAt          time.Time
	LastSeq             uint64
	// PublishedSeq is the highest seq whose events were handed to the
	// Publisher (the run.events outbox watermark).
	PublishedSeq uint64
}

// WaitingOn describes what a waiting run waits for (REST GetRun,
// spec 12.6).
type WaitingOn struct {
	Kind       string    `json:"kind"` // approval, timer, retry, operator, rate_limit
	Path       string    `json:"path,omitempty"`
	ApprovalID string    `json:"approval_id,omitempty"`
	Until      time.Time `json:"until,omitzero"`
}

// Run is the API view of a run (proto Run plus waiting_on).
type Run struct {
	RunRecord
	WaitingOn *WaitingOn
}

// RunFilter selects runs for ListRuns (proto ListRunsRequest).
type RunFilter struct {
	ProgramID    core.UUID // zero: any
	Status       RunStatus // "": any
	WorkflowName string    // "": any
	CreatedAfter time.Time // zero: any
}

// Page is a page request: Size items (default 50, at most 500) after the
// opaque Token of a previous response.
type Page struct {
	Size  int
	Token string
}

// Approval statuses (spec 10.4).
const (
	ApprovalPending   = "pending"
	ApprovalApproved  = "approved"
	ApprovalDenied    = "denied"
	ApprovalExpired   = "expired"
	ApprovalCancelled = "cancelled"
)

// Approval is an approval request addressed to a role (spec 9.7, 10.4,
// proto Approval).
type Approval struct {
	ID           core.UUID
	Tenant       core.UUID
	RunID        core.UUID
	EffectPath   string // the approval effect's path; escalations append "#<round>"
	Role         string
	Reason       string
	Proposed     json.RawMessage
	Status       string
	ExpiresAt    time.Time
	EscalateRole string // "": no escalation
	DecidedBy    string // actor, "" while pending
	DecidedAt    time.Time
	Comment      string
	CreatedAt    time.Time
}

// ApprovalFilter selects approvals for ListApprovals.
type ApprovalFilter struct {
	Status string    // "": any
	Role   string    // "": any
	RunID  core.UUID // zero: any
}

// Timer is a sleep timer (engine.timers).
type Timer struct {
	ID     core.UUID
	RunID  core.UUID
	Path   string
	FireAt time.Time
	Fired  time.Time // zero until fired
}

// Effect states of the effects projection (engine.effects).
const (
	EffectStateScheduled = "scheduled"
	EffectStateStarted   = "started"
	EffectStateCompleted = "completed"
	EffectStateFailed    = "failed"
	EffectStateUnknown   = "unknown"
)

// EffectRecord is the projection of one effect (engine.effects).
type EffectRecord struct {
	RunID          core.UUID
	Path           string
	Seq            uint64 // seq of the effect_scheduled event
	Kind           string
	ArgsHash       []byte
	State          string
	Attempt        int
	IdempotencyKey string
	ScheduledAt    time.Time
	StartedAt      time.Time
	CompletedAt    time.Time
	NextRetryAt    time.Time
	Result         json.RawMessage
	Error          json.RawMessage
}

// Run request kinds: operator and system actions a worker applies to a
// run at its next effect boundary (the API never appends to a run's log;
// only the lease holder does).
const (
	RequestCancel       = "cancel"
	RequestResolve      = "resolve_unknown"
	RequestScopeChanged = "scope_changed"
)

// Resolutions of ResolveUnknownEffect (proto ResolveUnknownEffectRequest).
const (
	ResolveRetry           = "retry"
	ResolveAssumeCompleted = "assume_completed"
	ResolveAssumeFailed    = "assume_failed"
)

// RunRequest is a queued request for a run: cancellation, the resolution
// of an unknown effect outcome, or a scope version change.
type RunRequest struct {
	ID         int64 // assigned by the Repo
	RunID      core.UUID
	Kind       string
	Actor      string
	Reason     string
	Path       string          // resolve_unknown
	Resolution string          // resolve_unknown
	Result     json.RawMessage // resolve_unknown with assume_completed
	NewVersion int32           // scope_changed
	CreatedAt  time.Time
}
