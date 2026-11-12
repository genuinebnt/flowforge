// Package bus is the event bus of the internal services (spec 3.2, 11.7,
// 23.7): the Bus port, an in-process implementation with at-least-once
// delivery, redelivery on handler error and dead-lettering after N
// attempts, the outbox relay that publishes each service's outbox table,
// and the processed_events inbox consumers use for idempotency.
//
// A Google Pub/Sub adapter is out of scope (no SDK in this module); it
// would implement Bus with one subscription per (topic, consumer), the
// envelope's ordering key and traceparent as attributes, and the
// subscription's dead-letter policy. See services/README.md.
package bus

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Handler consumes one envelope. A non-nil error asks for redelivery.
// Handlers must be idempotent by event_id (Inbox).
type Handler func(ctx context.Context, env *pb.EventEnvelope) error

// Bus is the publish/subscribe port. Delivery is at least once and in
// publication order per subscription.
type Bus interface {
	// Publish delivers env to every subscription of topic.
	Publish(ctx context.Context, topic string, env *pb.EventEnvelope) error
	// Subscribe registers consumer on topic and delivers to h until ctx is
	// done. A consumer name identifies one subscription: messages published
	// after Subscribe returns are delivered to it.
	Subscribe(ctx context.Context, topic, consumer string, h Handler) error
}

// DeadLetterSuffix names the dead-letter topic of a topic.
const DeadLetterSuffix = ".dead-letter"

// DeadLetter is a message that exhausted its delivery attempts.
type DeadLetter struct {
	Topic, Consumer string
	Envelope        *pb.EventEnvelope
	Reason          string
}

// Mem is the in-process Bus. Each subscription has its own queue and
// goroutine; a failing handler is retried with exponential backoff
// (Backoff, doubling, capped at one second) up to MaxAttempts, then the
// message goes to <topic>.dead-letter.
type Mem struct {
	MaxAttempts int
	Backoff     time.Duration
	Logger      *slog.Logger
	Metrics     *platform.Metrics

	mu   sync.Mutex
	subs map[string][]*subscription
	dead []DeadLetter
}

// NewMem returns an in-process bus (5 attempts, 10 ms initial backoff).
func NewMem(log *slog.Logger, m *platform.Metrics) *Mem {
	if log == nil {
		log = platform.DiscardLogger()
	}
	if m == nil {
		m = platform.NewMetrics(nil)
	}
	return &Mem{MaxAttempts: 5, Backoff: 10 * time.Millisecond, Logger: log, Metrics: m, subs: map[string][]*subscription{}}
}

type subscription struct {
	topic, consumer string
	h               Handler
	mu              sync.Mutex
	queue           []*pb.EventEnvelope
	signal          chan struct{}
}

// Publish implements Bus.
func (b *Mem) Publish(ctx context.Context, topic string, env *pb.EventEnvelope) error {
	b.mu.Lock()
	subs := append([]*subscription(nil), b.subs[topic]...)
	b.mu.Unlock()
	for _, s := range subs {
		s.mu.Lock()
		s.queue = append(s.queue, proto.Clone(env).(*pb.EventEnvelope))
		s.mu.Unlock()
		select {
		case s.signal <- struct{}{}:
		default:
		}
	}
	return nil
}

// Subscribe implements Bus.
func (b *Mem) Subscribe(ctx context.Context, topic, consumer string, h Handler) error {
	s := &subscription{topic: topic, consumer: consumer, h: h, signal: make(chan struct{}, 1)}
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], s)
	b.mu.Unlock()
	go b.loop(ctx, s)
	return nil
}

// DeadLetters returns the messages dead-lettered so far.
func (b *Mem) DeadLetters() []DeadLetter {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]DeadLetter(nil), b.dead...)
}

func (b *Mem) loop(ctx context.Context, s *subscription) {
	defer func() {
		b.mu.Lock()
		subs := b.subs[s.topic]
		for i, x := range subs {
			if x == s {
				b.subs[s.topic] = append(subs[:i:i], subs[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
	}()
	for {
		s.mu.Lock()
		var env *pb.EventEnvelope
		if len(s.queue) > 0 {
			env, s.queue = s.queue[0], s.queue[1:]
		}
		s.mu.Unlock()
		if env == nil {
			select {
			case <-ctx.Done():
				return
			case <-s.signal:
			}
			continue
		}
		b.deliver(ctx, s, env)
	}
}

func (b *Mem) deliver(ctx context.Context, s *subscription, env *pb.EventEnvelope) {
	hctx := ctx
	if t, ok := platform.ParseTraceparent(env.GetTraceparent()); ok {
		hctx = platform.WithTrace(ctx, t.Child())
	}
	delay := b.Backoff
	var err error
	for attempt := 1; attempt <= max(b.MaxAttempts, 1); attempt++ {
		if err = safeCall(hctx, s.h, env); err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		b.Logger.WarnContext(hctx, "bus.redeliver", "topic", s.topic, "consumer", s.consumer, "event_id", env.GetEventId(), "attempt", attempt, "error", err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, time.Second)
	}
	b.mu.Lock()
	b.dead = append(b.dead, DeadLetter{Topic: s.topic, Consumer: s.consumer, Envelope: env, Reason: err.Error()})
	b.mu.Unlock()
	b.Metrics.BusDeadLetters.Inc(s.topic)
	b.Logger.ErrorContext(hctx, "bus.dead_letter", "topic", s.topic, "consumer", s.consumer, "event_id", env.GetEventId(), "error", err.Error())
	if s.topic[len(s.topic)-min(len(s.topic), len(DeadLetterSuffix)):] != DeadLetterSuffix {
		_ = b.Publish(ctx, s.topic+DeadLetterSuffix, env)
	}
}

func safeCall(ctx context.Context, h Handler, env *pb.EventEnvelope) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errPanic
		}
	}()
	return h(ctx, env)
}

type busError string

func (e busError) Error() string { return string(e) }

const errPanic = busError("bus: handler panicked")
