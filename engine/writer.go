package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
)

// pending is an event about to be appended.
type pending struct {
	typ     string
	actor   string
	payload any
}

func ev(typ, actor string, payload any) pending {
	return pending{typ: typ, actor: actor, payload: payload}
}

// append writes events atomically at the head of the log with the lease's
// fence, then folds them into the run state and publishes them. A fenced
// append means the lease was lost: the runner stops immediately and never
// writes again (spec 9.4).
func (r *runner) append(evs ...pending) error {
	if len(evs) == 0 {
		return nil
	}
	if r.fenced.Load() {
		return ErrLeaseLost
	}
	at := r.now().Truncate(time.Microsecond)
	out := make([]eventstore.Event, len(evs))
	st := r.log.status
	for i, p := range evs {
		b, err := json.Marshal(p.payload)
		if err != nil {
			return fmt.Errorf("engine: encode %s: %w", p.typ, err)
		}
		next, err := nextStatus(st, p.typ)
		if err != nil {
			return fmt.Errorf("engine: refusing to append: %w", err)
		}
		st = next
		out[i] = eventstore.Event{RunID: r.run, Seq: r.log.head + uint64(i) + 1, Type: p.typ, At: at, Actor: p.actor, Payload: b}
	}
	lease := r.currentLease()
	if _, err := r.e.d.Store.Append(r.ctx, r.run, r.log.head, lease.Fence, out); err != nil {
		if errors.Is(err, eventstore.ErrFenced) {
			r.fenced.Store(true)
			r.stop()
			return fmt.Errorf("%w: %v", ErrLeaseLost, err)
		}
		return err
	}
	for _, e := range out {
		if err := r.log.apply(e); err != nil {
			return fmt.Errorf("engine: appended event does not fold: %w", err)
		}
		if p := effectPath(e); p != "" {
			r.touched[p] = true
		}
	}
	r.publish(out)
	return nil
}

// effectPath returns the effect path an event refers to, if any.
func effectPath(e eventstore.Event) string {
	switch e.Type {
	case EvEffectScheduled, EvEffectStarted, EvEffectCompleted, EvEffectFailed,
		EvEffectRetryScheduled, EvEffectOutcomeUnknown:
		var p struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(e.Payload, &p) == nil {
			return p.Path
		}
	}
	return ""
}

// publishBacklog republishes events appended by an earlier owner that
// crashed before handing them to the publisher.
func (r *runner) publishBacklog(evs []eventstore.Event) {
	if r.rec.PublishedSeq >= uint64(len(evs)) {
		return
	}
	r.publish(evs[r.rec.PublishedSeq:])
}

// publish hands events to the outbox: every event to run.events and
// approval lifecycle events to approval.events. Envelope ids derive from
// (run, seq), so republishing is harmless. A failure leaves the watermark
// where it is; the next advance republishes from there.
func (r *runner) publish(evs []eventstore.Event) {
	if r.e.d.Publisher == nil || len(evs) == 0 {
		if !r.pubFailed && len(evs) > 0 {
			r.rec.PublishedSeq = evs[len(evs)-1].Seq
		}
		return
	}
	if r.pubFailed {
		return
	}
	envs := make([]Envelope, 0, len(evs))
	for _, e := range evs {
		payload, _ := json.Marshal(map[string]any{
			"run_id": r.run.String(), "seq": e.Seq, "type": e.Type, "actor": e.Actor, "payload": e.Payload,
		})
		envs = append(envs, Envelope{
			EventID: derivedID("run-event", r.run.String(), fmt.Sprint(e.Seq)), Topic: TopicRunEvents,
			Type: "run." + e.Type, OrderingKey: r.run.String(), Tenant: r.rec.Tenant, Program: r.rec.ProgramID,
			OccurredAt: e.At, Payload: payload,
		})
		if env, ok := r.approvalEnvelope(e); ok {
			envs = append(envs, env)
		}
	}
	if err := r.e.d.Publisher.Publish(r.ctx, envs); err != nil {
		r.pubFailed = true
		return
	}
	r.rec.PublishedSeq = evs[len(evs)-1].Seq
}

func (r *runner) approvalEnvelope(e eventstore.Event) (Envelope, bool) {
	var id, status string
	switch e.Type {
	case EvApprovalRequested:
		var p ApprovalRequestedPayload
		_ = json.Unmarshal(e.Payload, &p)
		id, status = p.ApprovalID, ApprovalPending
	case EvApprovalDecided:
		var p ApprovalDecidedPayload
		_ = json.Unmarshal(e.Payload, &p)
		id, status = p.ApprovalID, p.Decision
	case EvApprovalExpired:
		var p ApprovalExpiredPayload
		_ = json.Unmarshal(e.Payload, &p)
		id, status = p.ApprovalID, ApprovalExpired
	default:
		return Envelope{}, false
	}
	a := r.log.approvals[id]
	if a == nil {
		return Envelope{}, false
	}
	payload, _ := json.Marshal(map[string]any{
		"approval_id": id, "run_id": r.run.String(), "role": a.role, "status": status,
		"reason": a.reason, "expires_at": core.FormatTime(a.expiresAt),
	})
	return Envelope{
		EventID: derivedID("approval-event", r.run.String(), fmt.Sprint(e.Seq)), Topic: TopicApprovalEvents,
		Type: "approval." + status, OrderingKey: id, Tenant: r.rec.Tenant, Program: r.rec.ProgramID,
		OccurredAt: e.At, Payload: payload,
	}, true
}
