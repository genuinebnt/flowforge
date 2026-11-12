package engine

import (
	"encoding/json"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/scope"
)

// Event types of the run log (spec 9.3).
const (
	EvRunSubmitted         = "run_submitted"
	EvRunStarted           = "run_started"
	EvEffectScheduled      = "effect_scheduled"
	EvScopeDecision        = "scope_decision"
	EvEffectStarted        = "effect_started"
	EvEffectCompleted      = "effect_completed"
	EvEffectFailed         = "effect_failed"
	EvEffectRetryScheduled = "effect_retry_scheduled"
	EvEffectOutcomeUnknown = "effect_outcome_unknown"
	EvObservationIngested  = "observation_ingested"
	EvApprovalRequested    = "approval_requested"
	EvApprovalDecided      = "approval_decided"
	EvApprovalExpired      = "approval_expired"
	EvTimerStarted         = "timer_started"
	EvTimerFired           = "timer_fired"
	EvAgentCallRecorded    = "agent_call_recorded"
	EvPlanProposed         = "plan_proposed"
	EvPlanAccepted         = "plan_accepted"
	EvPlanRejected         = "plan_rejected"
	EvChildRunStarted      = "child_run_started"
	EvChildRunFinished     = "child_run_finished"
	EvBudgetExceeded       = "budget_exceeded"
	EvScopeChanged         = "scope_changed"
	EvRunPaused            = "run_paused"
	EvRunResumed           = "run_resumed"
	EvCompensationStarted  = "compensation_started"
	EvCompensationStep     = "compensation_step"
	EvCompensationFinished = "compensation_finished"
	EvSnapshotTaken        = "snapshot_taken"
	EvAlertEmitted         = "alert_emitted"
	EvLog                  = "log"
	EvRunCancelRequested   = "run_cancel_requested"
	EvRunCancelled         = "run_cancelled"
	EvRunSucceeded         = "run_succeeded"
	EvRunFailed            = "run_failed"
)

// Event actors (spec 9.3).
const (
	ActorEngine = "engine"
	ActorVM     = "vm"
)

// ToolActor returns the actor of events reported by a tool.
func ToolActor(tool string) string { return "tool:" + tool }

// RunSubmittedPayload is the payload of run_submitted. Besides the key
// fields of spec 9.3 it carries everything needed to rebuild the runs
// projection from the log.
type RunSubmittedPayload struct {
	WorkflowVersionID string          `json:"workflow_version_id"`
	WorkflowName      string          `json:"workflow_name"`
	WorkflowVersion   int32           `json:"workflow_version"`
	ProgramID         string          `json:"program_id,omitempty"`
	ScopeVersion      int32           `json:"scope_version"`
	Input             json.RawMessage `json:"input"`
	IdempotencyKey    string          `json:"idempotency_key"`
	RequestHash       string          `json:"request_hash"`
	SubmittedBy       string          `json:"submitted_by"`
	Priority          int16           `json:"priority"`
	DeadlineAt        string          `json:"deadline_at,omitempty"`
	ParentRunID       string          `json:"parent_run_id,omitempty"`
}

// RunStartedPayload is the payload of run_started.
type RunStartedPayload struct {
	ModuleSHA256 string `json:"module_sha256"`
	FuelLimit    int64  `json:"fuel_limit"`
}

// EffectScheduledPayload is the payload of effect_scheduled. Args is a
// JSON rendering of the canonical arguments for humans (proofs shown as
// their targets, closures by name); ArgsHash is hex SHA-256 of the
// canonical CBOR encoding (vm.ArgsHash), which replay compares.
type EffectScheduledPayload struct {
	Path           string          `json:"path"`
	Kind           string          `json:"kind"`
	ArgsHash       string          `json:"args_hash"`
	Args           json.RawMessage `json:"args"`
	Tool           string          `json:"tool,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

// ScopeDecisionPayload is the payload of scope_decision. Purpose is
// scope_check (the effect's own decision), recheck (the re-check right
// before a tool call) or revalidate (after a scope version change).
type ScopeDecisionPayload struct {
	Path     string         `json:"path"`
	Purpose  string         `json:"purpose"`
	Decision scope.Decision `json:"decision"`
}

// Scope decision purposes.
const (
	PurposeScopeCheck = "scope_check"
	PurposeRecheck    = "recheck"
	PurposeRevalidate = "revalidate"
)

// Resolution records an operator's ResolveUnknownEffect decision.
type Resolution struct {
	Action string `json:"action"`
	Actor  string `json:"actor"`
}

// EffectStartedPayload is the payload of effect_started.
type EffectStartedPayload struct {
	Path       string      `json:"path"`
	Attempt    int         `json:"attempt"`
	Executor   string      `json:"executor"`
	Resolution *Resolution `json:"resolution,omitempty"`
}

// EffectCompletedPayload is the payload of effect_completed. Result is the
// JSON rendering of the result; Value is the exact result (base64 of the
// canonical CBOR encoding, vm.EncodeValue) that replay feeds to the VM.
type EffectCompletedPayload struct {
	Path       string          `json:"path"`
	Result     json.RawMessage `json:"result"`
	Value      string          `json:"value"`
	DurationMs int64           `json:"duration_ms"`
	Resolution *Resolution     `json:"resolution,omitempty"`
}

// EffectError is the error of effect_failed.
type EffectError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// EffectFailedPayload is the payload of effect_failed. Final marks the
// terminal failure of the effect (no retry follows); FailCode is then the
// run failure code the VM receives (spec 8.7).
type EffectFailedPayload struct {
	Path       string      `json:"path"`
	Attempt    int         `json:"attempt"`
	Error      EffectError `json:"error"`
	Final      bool        `json:"final"`
	FailCode   string      `json:"fail_code,omitempty"`
	Resolution *Resolution `json:"resolution,omitempty"`
}

// EffectRetryScheduledPayload is the payload of effect_retry_scheduled.
type EffectRetryScheduledPayload struct {
	Path    string `json:"path"`
	Attempt int    `json:"attempt"`
	RetryAt string `json:"retry_at"`
}

// EffectOutcomeUnknownPayload is the payload of effect_outcome_unknown.
type EffectOutcomeUnknownPayload struct {
	Path    string `json:"path"`
	Attempt int    `json:"attempt"`
}

// ObservationIngestedPayload is the payload of observation_ingested.
type ObservationIngestedPayload struct {
	Path          string `json:"path"`
	ObservationID string `json:"observation_id"`
	EntityCount   int    `json:"entity_count"`
	EdgeCount     int    `json:"edge_count"`
}

// ApprovalRequestedPayload is the payload of approval_requested.
type ApprovalRequestedPayload struct {
	Path       string          `json:"path"`
	ApprovalID string          `json:"approval_id"`
	Role       string          `json:"role"`
	Reason     string          `json:"reason"`
	Proposed   json.RawMessage `json:"proposed"`
	ExpiresAt  string          `json:"expires_at"`
	Round      int             `json:"round,omitempty"`
}

// ApprovalDecidedPayload is the payload of approval_decided.
type ApprovalDecidedPayload struct {
	ApprovalID string `json:"approval_id"`
	Decision   string `json:"decision"` // approved or denied
	Actor      string `json:"actor"`
	Comment    string `json:"comment"`
}

// ApprovalExpiredPayload is the payload of approval_expired.
type ApprovalExpiredPayload struct {
	ApprovalID string `json:"approval_id"`
}

// TimerPayload is the payload of timer_started and timer_fired.
type TimerPayload struct {
	Path    string `json:"path"`
	TimerID string `json:"timer_id"`
	FireAt  string `json:"fire_at"`
}

// BudgetExceededPayload is the payload of budget_exceeded.
type BudgetExceededPayload struct {
	Resource string `json:"resource"`
	Limit    int64  `json:"limit"`
	Used     int64  `json:"used"`
}

// ScopeChangedPayload is the payload of scope_changed. Action is continue
// (the run re-validated against the new version and is now pinned to it)
// or pause.
type ScopeChangedPayload struct {
	OldVersion int32  `json:"old_version"`
	NewVersion int32  `json:"new_version"`
	Action     string `json:"action"`
}

// Scope change actions.
const (
	ScopeActionContinue = "continue"
	ScopeActionPause    = "pause"
)

// ReasonPayload is the payload of run_paused and run_resumed.
type ReasonPayload struct {
	Reason string `json:"reason"`
}

// CompensationStartedPayload is the payload of compensation_started. Cause
// is failure or cancel; Code and Message are the failure the run ends with.
// PathPrefix is the root effect sequence number under which compensation
// k numbers its effects PathPrefix/k/i.
type CompensationStartedPayload struct {
	Count      int    `json:"count"`
	Cause      string `json:"cause"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
	PathPrefix uint32 `json:"path_prefix"`
}

// Compensation causes.
const (
	CauseFailure = "failure"
	CauseCancel  = "cancel"
)

// CompensationStepPayload is the payload of compensation_step.
type CompensationStepPayload struct {
	Index  int          `json:"index"`
	Name   string       `json:"name"`
	Result string       `json:"result"` // ok or failed
	Error  *EffectError `json:"error,omitempty"`
}

// CompensationFinishedPayload is the payload of compensation_finished.
type CompensationFinishedPayload struct {
	Outcome string `json:"outcome"` // completed or failed
}

// SnapshotTakenPayload is the payload of snapshot_taken.
type SnapshotTakenPayload struct {
	AtSeq      uint64 `json:"at_seq"`
	SnapshotID string `json:"snapshot_id"`
}

// AlertEmittedPayload is the payload of alert_emitted.
type AlertEmittedPayload struct {
	Path     string `json:"path"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// LogPayload is the payload of log.
type LogPayload struct {
	Path      string `json:"path"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	Truncated bool   `json:"truncated,omitempty"`
}

// CancelPayload is the payload of run_cancel_requested and run_cancelled.
type CancelPayload struct {
	Actor  string `json:"actor"`
	Reason string `json:"reason"`
}

// RunSucceededPayload is the payload of run_succeeded.
type RunSucceededPayload struct {
	Output json.RawMessage `json:"output"`
}

// RunFailedPayload is the payload of run_failed.
type RunFailedPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return core.FormatTime(t)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := core.ParseTime(s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// msTime truncates t to the millisecond precision of the JSON wire form,
// so a time computed live equals the one parsed back from the log.
func msTime(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }
