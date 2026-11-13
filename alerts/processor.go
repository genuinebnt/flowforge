package alerts

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// DefaultConsumer is the consumer name recorded in processed_events.
const DefaultConsumer = "notifier"

// Processor errors.
var (
	ErrNilEvent       = errors.New("alerts: nil event")
	ErrMissingEventID = errors.New("alerts: event has no event_id")
	ErrMissingClock   = errors.New("alerts: processor needs a clock")
	ErrMissingStore   = errors.New("alerts: processor needs a processed-events store")
	ErrMissingRules   = errors.New("alerts: processor needs a rule set")
)

// ProcessedStore is the consumer-idempotency port (processed_events,
// spec 10.6 and 15.1). In production the mark is written in the same
// transaction that stores the alert events.
type ProcessedStore interface {
	// MarkProcessed records (consumer, eventID) and reports whether it was
	// recorded now (true) or had been recorded before (false).
	MarkProcessed(ctx context.Context, consumer, eventID string, at time.Time) (bool, error)
}

// MemProcessedStore is the in-memory ProcessedStore fake.
type MemProcessedStore struct {
	mu   sync.Mutex
	seen map[[2]string]time.Time
}

// NewMemProcessedStore returns an empty store.
func NewMemProcessedStore() *MemProcessedStore {
	return &MemProcessedStore{seen: map[[2]string]time.Time{}}
}

// MarkProcessed implements ProcessedStore.
func (s *MemProcessedStore) MarkProcessed(ctx context.Context, consumer, eventID string, at time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := [2]string{consumer, eventID}
	if _, ok := s.seen[k]; ok {
		return false, nil
	}
	s.seen[k] = at
	return true, nil
}

// Len returns the number of recorded (consumer, event) pairs.
func (s *MemProcessedStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// AlertStatus is the status of a stored alert event (spec 10.6). The
// processor creates pending and suppressed events; delivery moves pending
// ones to sent or failed.
type AlertStatus string

// Alert statuses.
const (
	StatusPending    AlertStatus = "pending"
	StatusSent       AlertStatus = "sent"
	StatusFailed     AlertStatus = "failed"
	StatusSuppressed AlertStatus = "suppressed"
)

// AlertRecord is an alert_events row to store.
type AlertRecord struct {
	RuleID         string
	DedupeKey      string
	TriggerEventID string
	Status         AlertStatus
	Severity       Severity
	Payload        Payload
	CreatedAt      time.Time
	Digested       bool // delivered as part of a digest, not on its own
}

// Notification is something to deliver: one alert or one digest, to each
// of Actions, not before DeliverAt (quiet hours).
type Notification struct {
	RuleID    string
	Severity  Severity
	Actions   []Action
	DedupeKey string   // empty for digests
	Payload   *Payload // set for a single alert
	Digest    *Digest  // set for a digest
	DeliverAt time.Time
	Delayed   bool // DeliverAt was moved by quiet hours
}

// Outcome is the result of processing one event.
type Outcome struct {
	Duplicate     bool // event_id seen before: nothing was produced (inv_08)
	Alerts        []AlertRecord
	Notifications []Notification
	Errors        []RuleError
	// State lists the throttle windows and digests this event changed,
	// to persist with the alert events (G-LIM-06).
	State StateChange
}

// ProcessorConfig configures a Processor.
type ProcessorConfig struct {
	Consumer string // default DefaultConsumer
	Clock    core.Clock
	Store    ProcessedStore
}

// Processor applies the notifier semantics of spec 15.3 on top of a
// RuleSet: consumer idempotency, dedupe keys, throttling, digests and
// quiet hours. Time comes only from the injected clock. It is safe for
// concurrent use.
type Processor struct {
	rules    *RuleSet
	clock    core.Clock
	store    ProcessedStore
	consumer string

	mu       sync.Mutex
	throttle *Throttle
	digests  *Digester
	quiet    map[string]*QuietHours // rule id -> quiet hours of open digests
}

// NewProcessor validates cfg and returns a processor.
func NewProcessor(rules *RuleSet, cfg ProcessorConfig) (*Processor, error) {
	switch {
	case rules == nil:
		return nil, ErrMissingRules
	case cfg.Clock == nil:
		return nil, ErrMissingClock
	case cfg.Store == nil:
		return nil, ErrMissingStore
	}
	if cfg.Consumer == "" {
		cfg.Consumer = DefaultConsumer
	}
	return &Processor{
		rules: rules, clock: cfg.Clock, store: cfg.Store, consumer: cfg.Consumer,
		throttle: NewThrottle(), digests: NewDigester(), quiet: map[string]*QuietHours{},
	}, nil
}

// Process handles one event. A redelivered event (same event_id) yields
// Outcome{Duplicate: true} and no alerts.
func (p *Processor) Process(ctx context.Context, ev *Event) (Outcome, error) {
	if ev == nil {
		return Outcome{}, ErrNilEvent
	}
	if ev.ID == "" {
		return Outcome{}, ErrMissingEventID
	}
	now := p.clock.Now()
	fresh, err := p.store.MarkProcessed(ctx, p.consumer, ev.ID, now)
	if err != nil {
		return Outcome{}, fmt.Errorf("alerts: mark event %s processed: %w", ev.ID, err)
	}
	if !fresh {
		return Outcome{Duplicate: true}, nil
	}
	eval := p.rules.Evaluate(ev)
	out := Outcome{Errors: eval.Errors}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range eval.Matches {
		throttle, digest, quiet := m.rule.Throttle, m.rule.Digest, m.rule.QuietHours
		rec := AlertRecord{
			RuleID: m.RuleID, DedupeKey: m.DedupeKey, TriggerEventID: ev.ID,
			Status: StatusPending, Severity: m.Severity, Payload: m.Payload, CreatedAt: now,
		}
		dec := p.throttle.Admit(m.DedupeKey, throttle, now)
		if w, ok := p.throttle.window(m.DedupeKey); ok && throttle > 0 {
			out.State.Throttle = append(out.State.Throttle, w)
		}
		if !dec.Fire {
			rec.Status = StatusSuppressed
			out.Alerts = append(out.Alerts, rec)
			continue
		}
		rec.Payload.SuppressedCount = dec.SuppressedBefore
		if digest > 0 {
			rec.Digested = true
			m.Payload = rec.Payload
			p.digests.Add(m, digest, now)
			p.quiet[m.RuleID] = quiet
			dg := *p.digests.open[m.RuleID]
			dg.Items = append([]Payload(nil), dg.Items...)
			out.State.Digests = append(out.State.Digests, dg)
			out.Alerts = append(out.Alerts, rec)
			continue
		}
		payload := rec.Payload
		at := quiet.DeliverAt(m.Severity, now)
		out.Alerts = append(out.Alerts, rec)
		out.Notifications = append(out.Notifications, Notification{
			RuleID: m.RuleID, Severity: m.Severity, Actions: m.Actions, DedupeKey: m.DedupeKey,
			Payload: &payload, DeliverAt: at, Delayed: at.After(now),
		})
	}
	return out, nil
}

// Flush closes the digest windows that have ended by the clock's current
// time and returns one notification per digest.
func (p *Processor) Flush() []Notification {
	now := p.clock.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Notification
	for _, dg := range p.digests.Due(now) {
		d := dg
		quiet := p.quiet[d.RuleID]
		delete(p.quiet, d.RuleID)
		at := quiet.DeliverAt(d.Severity, now)
		out = append(out, Notification{
			RuleID: d.RuleID, Severity: d.Severity, Actions: d.Actions,
			Digest: &d, DeliverAt: at, Delayed: at.After(now),
		})
	}
	return out
}

// NextFlush reports when the earliest open digest window ends.
func (p *Processor) NextFlush() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.digests.NextDue()
}

// PruneThrottle drops throttle windows that have ended; call it
// periodically to bound memory.
func (p *Processor) PruneThrottle() int {
	now := p.clock.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.throttle.Prune(now)
}
