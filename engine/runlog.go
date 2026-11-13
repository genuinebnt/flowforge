package engine

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/scope"
)

// runLog is the state of a run derived from its event log: everything the
// worker needs to replay the VM and to decide what to do at the live
// frontier. It is built by applying events in seq order; the live path
// applies each event right after appending it, so replay and live
// execution see exactly the same state.
type runLog struct {
	head   uint64
	status RunStatus
	lastAt time.Time

	submitted   RunSubmittedPayload
	submittedAt time.Time
	started     bool
	startedAt   time.Time
	moduleSHA   string

	effects map[string]*effectState
	order   []string // effect paths in scheduling order

	decisions map[string]scope.Decision // by decision id
	// allowed maps (normalized target, scope version) to an allow decision.
	allowed map[targetVersion]string

	approvals map[string]*approvalState // by approval id

	cancel *CancelPayload
	paused bool

	scopeVersion int32
	scopeChanges map[int32]string // new version -> action

	comp *compState

	terminal   bool
	output     json.RawMessage
	failCode   string
	failMsg    string
	finishedAt time.Time

	lastSnapshotSeq      uint64
	lastSnapshotAt       time.Time
	effectsSinceSnapshot int
}

type targetVersion struct {
	target  string
	version int32
}

type effectState struct {
	path         string
	kind         bytecode.EffectKind
	argsHash     string
	tool         string
	scheduledSeq uint64
	scheduledAt  time.Time

	attempt   int // highest started attempt
	inFlight  bool
	startedAt time.Time
	failures  int
	// uncounted is the number of effect_failed events marked Uncounted
	// (edge rate-limit waits): attempt - uncounted is the number of
	// attempts the retry policy has used.
	uncounted int

	retryPending bool
	retryAt      time.Time

	unknown bool

	done     bool
	value    []byte // canonical CBOR of the result
	result   json.RawMessage
	failed   bool
	failCode bytecode.FailCode
	failErr  EffectError
	doneAt   time.Time

	decisions []string // decision ids recorded under this path

	approvals []string // approval ids, one per round
	timerID   string
	fireAt    time.Time
	timerDone bool
}

type approvalState struct {
	id        string
	path      string
	role      string
	reason    string
	expiresAt time.Time
	round     int
	decided   bool
	decision  string // approved or denied
	actor     string
	comment   string
	expired   bool
}

type compState struct {
	started  CompensationStartedPayload
	steps    []CompensationStepPayload
	finished bool
	outcome  string
}

func newRunLog() *runLog {
	return &runLog{
		effects:      map[string]*effectState{},
		decisions:    map[string]scope.Decision{},
		allowed:      map[targetVersion]string{},
		approvals:    map[string]*approvalState{},
		scopeChanges: map[int32]string{},
	}
}

// foldLog builds the state of a run from its complete event log.
func foldLog(events []eventstore.Event) (*runLog, error) {
	l := newRunLog()
	for _, ev := range events {
		if err := l.apply(ev); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func decode[T any](ev eventstore.Event) (T, error) {
	var p T
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return p, fmt.Errorf("engine: event %d (%s): %w", ev.Seq, ev.Type, err)
	}
	return p, nil
}

func (l *runLog) effect(path string) *effectState { return l.effects[path] }

func (l *runLog) mustEffect(ev eventstore.Event, path string) (*effectState, error) {
	e := l.effects[path]
	if e == nil {
		return nil, fmt.Errorf("engine: event %d (%s) for unscheduled effect %q", ev.Seq, ev.Type, path)
	}
	return e, nil
}

// apply folds one event. It checks the status transition and the
// per-effect lifecycle, so a corrupt log is reported instead of silently
// replayed.
func (l *runLog) apply(ev eventstore.Event) error {
	if ev.Seq != l.head+1 {
		return fmt.Errorf("engine: event seq %d after %d", ev.Seq, l.head)
	}
	to, err := nextStatus(l.status, ev.Type)
	if err != nil {
		return fmt.Errorf("event %d: %w", ev.Seq, err)
	}
	if err := l.applyPayload(ev); err != nil {
		return err
	}
	l.status = to
	l.head = ev.Seq
	l.lastAt = ev.At
	return nil
}

func (l *runLog) applyPayload(ev eventstore.Event) error {
	switch ev.Type {
	case EvRunSubmitted:
		p, err := decode[RunSubmittedPayload](ev)
		if err != nil {
			return err
		}
		l.submitted, l.submittedAt = p, ev.At
		l.scopeVersion = p.ScopeVersion
	case EvRunStarted:
		p, err := decode[RunStartedPayload](ev)
		if err != nil {
			return err
		}
		l.started, l.startedAt, l.moduleSHA = true, ev.At, p.ModuleSHA256
	case EvEffectScheduled:
		p, err := decode[EffectScheduledPayload](ev)
		if err != nil {
			return err
		}
		if _, dup := l.effects[p.Path]; dup {
			return fmt.Errorf("engine: event %d schedules effect %q twice", ev.Seq, p.Path)
		}
		kind, ok := bytecode.ParseEffectKind(p.Kind)
		if !ok {
			return fmt.Errorf("engine: event %d: unknown effect kind %q", ev.Seq, p.Kind)
		}
		l.effects[p.Path] = &effectState{
			path: p.Path, kind: kind, argsHash: p.ArgsHash, tool: p.Tool,
			scheduledSeq: ev.Seq, scheduledAt: ev.At,
		}
		l.order = append(l.order, p.Path)
		l.effectsSinceSnapshot++
	case EvScopeDecision:
		p, err := decode[ScopeDecisionPayload](ev)
		if err != nil {
			return err
		}
		d := p.Decision
		l.decisions[d.ID] = d
		if d.Allowed() {
			l.allowed[targetVersion{d.Target, int32(d.ScopeVersion)}] = d.ID
		}
		if e := l.effects[p.Path]; e != nil {
			e.decisions = append(e.decisions, d.ID)
		}
	case EvEffectStarted:
		p, err := decode[EffectStartedPayload](ev)
		if err != nil {
			return err
		}
		e, err := l.mustEffect(ev, p.Path)
		if err != nil {
			return err
		}
		if e.done {
			return fmt.Errorf("engine: event %d starts finished effect %q", ev.Seq, p.Path)
		}
		e.attempt, e.inFlight, e.startedAt = p.Attempt, true, ev.At
		e.retryPending, e.unknown = false, false
	case EvEffectCompleted:
		p, err := decode[EffectCompletedPayload](ev)
		if err != nil {
			return err
		}
		e, err := l.mustEffect(ev, p.Path)
		if err != nil {
			return err
		}
		if e.done {
			return fmt.Errorf("engine: event %d records a second result for effect %q", ev.Seq, p.Path)
		}
		val, err := base64.StdEncoding.DecodeString(p.Value)
		if err != nil {
			return fmt.Errorf("engine: event %d: bad value: %w", ev.Seq, err)
		}
		e.done, e.value, e.result, e.doneAt = true, val, p.Result, ev.At
		e.inFlight, e.retryPending, e.unknown = false, false, false
	case EvEffectFailed:
		p, err := decode[EffectFailedPayload](ev)
		if err != nil {
			return err
		}
		e, err := l.mustEffect(ev, p.Path)
		if err != nil {
			return err
		}
		if e.done {
			return fmt.Errorf("engine: event %d records a second result for effect %q", ev.Seq, p.Path)
		}
		e.inFlight, e.unknown = false, false
		e.failures++
		if p.Uncounted {
			e.uncounted++
		}
		e.failErr = p.Error
		if p.Final {
			code, ok := bytecode.ParseFailCode(p.FailCode)
			if !ok {
				code = bytecode.FailEffectFailed
			}
			e.done, e.failed, e.failCode, e.doneAt = true, true, code, ev.At
			e.retryPending = false
		}
	case EvEffectRetryScheduled:
		p, err := decode[EffectRetryScheduledPayload](ev)
		if err != nil {
			return err
		}
		e, err := l.mustEffect(ev, p.Path)
		if err != nil {
			return err
		}
		e.retryPending, e.retryAt = true, parseTime(p.RetryAt)
	case EvEffectOutcomeUnknown:
		p, err := decode[EffectOutcomeUnknownPayload](ev)
		if err != nil {
			return err
		}
		e, err := l.mustEffect(ev, p.Path)
		if err != nil {
			return err
		}
		e.unknown, e.inFlight = true, false
	case EvApprovalRequested:
		p, err := decode[ApprovalRequestedPayload](ev)
		if err != nil {
			return err
		}
		e, err := l.mustEffect(ev, p.Path)
		if err != nil {
			return err
		}
		l.approvals[p.ApprovalID] = &approvalState{
			id: p.ApprovalID, path: p.Path, role: p.Role, reason: p.Reason,
			expiresAt: parseTime(p.ExpiresAt), round: p.Round,
		}
		e.approvals = append(e.approvals, p.ApprovalID)
	case EvApprovalDecided:
		p, err := decode[ApprovalDecidedPayload](ev)
		if err != nil {
			return err
		}
		a := l.approvals[p.ApprovalID]
		if a == nil || a.decided || a.expired {
			return fmt.Errorf("engine: event %d decides unknown or closed approval %s", ev.Seq, p.ApprovalID)
		}
		a.decided, a.decision, a.actor, a.comment = true, p.Decision, p.Actor, p.Comment
	case EvApprovalExpired:
		p, err := decode[ApprovalExpiredPayload](ev)
		if err != nil {
			return err
		}
		a := l.approvals[p.ApprovalID]
		if a == nil || a.decided || a.expired {
			return fmt.Errorf("engine: event %d expires unknown or closed approval %s", ev.Seq, p.ApprovalID)
		}
		a.expired = true
	case EvTimerStarted:
		p, err := decode[TimerPayload](ev)
		if err != nil {
			return err
		}
		e, err := l.mustEffect(ev, p.Path)
		if err != nil {
			return err
		}
		e.timerID, e.fireAt = p.TimerID, parseTime(p.FireAt)
	case EvTimerFired:
		p, err := decode[TimerPayload](ev)
		if err != nil {
			return err
		}
		e, err := l.mustEffect(ev, p.Path)
		if err != nil {
			return err
		}
		e.timerDone = true
	case EvScopeChanged:
		p, err := decode[ScopeChangedPayload](ev)
		if err != nil {
			return err
		}
		l.scopeChanges[p.NewVersion] = p.Action
		if p.Action == ScopeActionContinue {
			l.scopeVersion = p.NewVersion
		}
	case EvRunPaused:
		l.paused = true
	case EvRunResumed:
		l.paused = false
	case EvRunCancelRequested:
		p, err := decode[CancelPayload](ev)
		if err != nil {
			return err
		}
		l.cancel = &p
	case EvCompensationStarted:
		p, err := decode[CompensationStartedPayload](ev)
		if err != nil {
			return err
		}
		if l.comp != nil {
			return fmt.Errorf("engine: event %d starts compensation twice", ev.Seq)
		}
		l.comp = &compState{started: p}
	case EvCompensationStep:
		p, err := decode[CompensationStepPayload](ev)
		if err != nil {
			return err
		}
		if l.comp == nil || p.Index != len(l.comp.steps) {
			return fmt.Errorf("engine: event %d: compensation step %d out of order", ev.Seq, p.Index)
		}
		l.comp.steps = append(l.comp.steps, p)
	case EvCompensationFinished:
		p, err := decode[CompensationFinishedPayload](ev)
		if err != nil {
			return err
		}
		if l.comp == nil {
			return fmt.Errorf("engine: event %d finishes compensation that never started", ev.Seq)
		}
		l.comp.finished, l.comp.outcome = true, p.Outcome
	case EvSnapshotTaken:
		l.lastSnapshotSeq, l.lastSnapshotAt, l.effectsSinceSnapshot = ev.Seq, ev.At, 0
	case EvRunSucceeded:
		p, err := decode[RunSucceededPayload](ev)
		if err != nil {
			return err
		}
		l.terminal, l.output, l.finishedAt = true, p.Output, ev.At
	case EvRunFailed:
		p, err := decode[RunFailedPayload](ev)
		if err != nil {
			return err
		}
		l.terminal, l.failCode, l.failMsg, l.finishedAt = true, p.Code, p.Message, ev.At
	case EvRunCancelled:
		l.terminal, l.finishedAt = true, ev.At
		l.failCode, l.failMsg = bytecode.FailCancelled.String(), "cancelled"
		if l.cancel != nil && l.cancel.Reason != "" {
			l.failMsg = l.cancel.Reason
		}
	}
	return nil
}

// compensationOutcome is none, completed or failed (spec 9.2).
func (l *runLog) compensationOutcome() string {
	if l.comp == nil {
		return CompensationNone
	}
	if l.comp.finished {
		return l.comp.outcome
	}
	for _, s := range l.comp.steps {
		if s.Result != "ok" {
			return CompensationFailed
		}
	}
	return CompensationNone
}

// openApproval returns the approval of an effect that is still open.
func (l *runLog) openApproval(e *effectState) *approvalState {
	if len(e.approvals) == 0 {
		return nil
	}
	return l.approvals[e.approvals[len(e.approvals)-1]]
}

// waitingOn derives what a run waits for from its open effects.
func (l *runLog) waitingOn() *WaitingOn {
	if l.terminal {
		return nil
	}
	for _, path := range l.order {
		e := l.effects[path]
		if e.done {
			continue
		}
		switch {
		case e.unknown:
			return &WaitingOn{Kind: "operator", Path: path}
		case e.retryPending:
			return &WaitingOn{Kind: "retry", Path: path, Until: e.retryAt}
		case e.timerID != "" && !e.timerDone:
			return &WaitingOn{Kind: "timer", Path: path, Until: e.fireAt}
		}
		if a := l.openApproval(e); a != nil && !a.decided && !a.expired {
			return &WaitingOn{Kind: "approval", Path: path, ApprovalID: a.id, Until: a.expiresAt}
		}
	}
	return nil
}

// project writes the log-derived fields into a run record.
func (l *runLog) project(r *RunRecord) {
	r.Status = l.status
	r.CompensationOutcome = l.compensationOutcome()
	r.ScopeVersion = l.scopeVersion
	r.LastSeq = l.head
	if l.started {
		r.StartedAt = l.startedAt
	}
	if l.terminal {
		r.FinishedAt = l.finishedAt
		r.Output = l.output
		if l.status != StatusSucceeded {
			r.FailureCode, r.FailureMessage = l.failCode, l.failMsg
		}
	}
}

// recordFromSubmitted rebuilds a run record from run_submitted (repair of
// a missing projection).
func recordFromSubmitted(run, tenant core.UUID, p RunSubmittedPayload, at time.Time) RunRecord {
	r := RunRecord{
		ID: run, Tenant: tenant, WorkflowName: p.WorkflowName, WorkflowVersion: p.WorkflowVersion,
		ScopeVersion: p.ScopeVersion, Status: StatusPending, CompensationOutcome: CompensationNone,
		Input: p.Input, CreatedBy: p.SubmittedBy, IdempotencyKey: p.IdempotencyKey,
		Priority: p.Priority, DeadlineAt: parseTime(p.DeadlineAt), CreatedAt: at,
	}
	r.WorkflowVersionID, _ = core.ParseUUID(p.WorkflowVersionID)
	r.ProgramID, _ = core.ParseUUID(p.ProgramID)
	r.ParentRunID, _ = core.ParseUUID(p.ParentRunID)
	if h, err := hexDecode(p.RequestHash); err == nil {
		r.RequestHash = h
	}
	return r
}
