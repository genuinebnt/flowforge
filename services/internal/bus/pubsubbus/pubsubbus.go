// Package pubsubbus is the Google Pub/Sub adapter of the bus.Bus port
// (spec 11.7, 17: "ordering enabled, one subscription per consumer,
// dead-letter topics, retry policy exponential, ack deadline 60 s"),
// selected by FF_BUS=pubsub with FF_PUBSUB_PROJECT and FF_TOPIC_PREFIX
// (G-SVC-01).
//
// Names follow infra/modules/pubsub-topic: topic <prefix><topic>, its
// dead-letter topic <prefix><topic>.dead-letter and one subscription per
// consumer <prefix><topic>.<consumer>. In GCP, Terraform owns them (with the
// retry and dead-letter policies); with Options.CreateMissing the adapter
// creates any that are missing, which is what the emulator and pstest need.
//
// Each message carries the envelope as proto bytes, the spec 11.7 ordering
// key (bus.OrderingKey) and attributes (bus.Attributes: tenant_id, type,
// event_id, traceparent, plus role and status on approval.events). A
// handler error nacks the message; Pub/Sub redelivers it with the
// subscription's backoff and moves it to the dead-letter topic after
// max_delivery_attempts. Dead-lettering is Pub/Sub's job here, unlike
// bus.Mem and bus.PgBus, which republish themselves.
package pubsubbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Options configure the adapter.
type Options struct {
	// Prefix is FF_TOPIC_PREFIX, prepended to every topic and subscription.
	Prefix string
	// CreateMissing creates missing topics and subscriptions (emulator,
	// pstest, a fresh lab project); in prod Terraform has created them.
	CreateMissing bool
	// MaxAttempts is the dead-letter policy of subscriptions this adapter
	// creates (default 5, Pub/Sub's minimum and the Terraform default).
	MaxAttempts int
	// MinBackoff and MaxBackoff are the retry policy of created
	// subscriptions (defaults 10 s and 600 s, as in Terraform).
	MinBackoff, MaxBackoff time.Duration
	// AckDeadline of created subscriptions (default 60 s, spec 17).
	AckDeadline time.Duration
	Logger      *slog.Logger
	Metrics     *platform.Metrics
}

// Bus publishes to and receives from Pub/Sub. It is safe for concurrent use.
type Bus struct {
	c *pubsub.Client
	o Options

	mu         sync.Mutex
	publishers map[string]*pubsub.Publisher
	ensured    map[string]bool
}

// New returns a bus on client. Close stops its publishers; the client
// stays the caller's.
func New(c *pubsub.Client, o Options) *Bus {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 5
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = 10 * time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 600 * time.Second
	}
	if o.AckDeadline <= 0 {
		o.AckDeadline = 60 * time.Second
	}
	if o.Logger == nil {
		o.Logger = platform.DiscardLogger()
	}
	if o.Metrics == nil {
		o.Metrics = platform.NewMetrics(nil)
	}
	return &Bus{c: c, o: o, publishers: map[string]*pubsub.Publisher{}, ensured: map[string]bool{}}
}

// TopicName is the fully qualified name of a spec 11.7 topic.
func (b *Bus) TopicName(topic string) string {
	return "projects/" + b.c.Project() + "/topics/" + b.o.Prefix + topic
}

// SubscriptionName is the fully qualified name of consumer's subscription.
func (b *Bus) SubscriptionName(topic, consumer string) string {
	return "projects/" + b.c.Project() + "/subscriptions/" + b.o.Prefix + topic + "." + consumer
}

func (b *Bus) publisher(ctx context.Context, topic string) (*pubsub.Publisher, error) {
	b.mu.Lock()
	p := b.publishers[topic]
	b.mu.Unlock()
	if p != nil {
		return p, nil
	}
	if err := b.ensureTopic(ctx, topic); err != nil {
		return nil, err
	}
	p = b.c.Publisher(b.TopicName(topic))
	p.EnableMessageOrdering = true
	// Publish is synchronous (the relay marks a row published only after
	// it returns), so bundling would only add its delay threshold.
	p.PublishSettings.DelayThreshold = time.Millisecond
	b.mu.Lock()
	defer b.mu.Unlock()
	if cur := b.publishers[topic]; cur != nil {
		p.Stop()
		return cur, nil
	}
	b.publishers[topic] = p
	return p, nil
}

func (b *Bus) ensureTopic(ctx context.Context, topic string) error {
	if !b.o.CreateMissing {
		return nil
	}
	name := b.TopicName(topic)
	b.mu.Lock()
	done := b.ensured[name]
	b.mu.Unlock()
	if done {
		return nil
	}
	_, err := b.c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: name})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return fmt.Errorf("pubsub: create topic %s: %w", name, err)
	}
	b.mu.Lock()
	b.ensured[name] = true
	b.mu.Unlock()
	return nil
}

func (b *Bus) ensureSubscription(ctx context.Context, topic, consumer string) error {
	if !b.o.CreateMissing {
		return nil
	}
	if err := b.ensureTopic(ctx, topic); err != nil {
		return err
	}
	sub := &pubsubpb.Subscription{
		Name: b.SubscriptionName(topic, consumer), Topic: b.TopicName(topic),
		AckDeadlineSeconds: int32(b.o.AckDeadline / time.Second), EnableMessageOrdering: true,
		RetryPolicy: &pubsubpb.RetryPolicy{MinimumBackoff: durationpb.New(b.o.MinBackoff), MaximumBackoff: durationpb.New(b.o.MaxBackoff)},
	}
	if !strings.HasSuffix(topic, bus.DeadLetterSuffix) {
		if err := b.ensureTopic(ctx, topic+bus.DeadLetterSuffix); err != nil {
			return err
		}
		sub.DeadLetterPolicy = &pubsubpb.DeadLetterPolicy{DeadLetterTopic: b.TopicName(topic + bus.DeadLetterSuffix),
			MaxDeliveryAttempts: int32(b.o.MaxAttempts)}
	}
	_, err := b.c.SubscriptionAdminClient.CreateSubscription(ctx, sub)
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return fmt.Errorf("pubsub: create subscription %s: %w", sub.Name, err)
	}
	return nil
}

// Publish implements bus.Bus and waits for the server's acknowledgement.
// A failed publish on an ordering key pauses that key in the client
// library; it is resumed so the caller's retry can go through.
func (b *Bus) Publish(ctx context.Context, topic string, env *pb.EventEnvelope) error {
	data, err := proto.Marshal(env)
	if err != nil {
		return err
	}
	p, err := b.publisher(ctx, topic)
	if err != nil {
		return err
	}
	key := bus.OrderingKey(topic, env)
	_, err = p.Publish(ctx, &pubsub.Message{Data: data, Attributes: bus.Attributes(topic, env), OrderingKey: key}).Get(ctx)
	if err != nil && key != "" {
		p.ResumePublish(key)
	}
	return err
}

// Subscribe implements bus.Bus. It returns once the subscription exists (or,
// without CreateMissing, once it is known to exist) and receives in the
// background until ctx is done.
func (b *Bus) Subscribe(ctx context.Context, topic, consumer string, h bus.Handler) error {
	if err := b.ensureSubscription(ctx, topic, consumer); err != nil {
		return err
	}
	name := b.SubscriptionName(topic, consumer)
	if !b.o.CreateMissing {
		if _, err := b.c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name}); err != nil {
			return fmt.Errorf("pubsub: subscription %s: %w", name, err)
		}
	}
	s := b.c.Subscriber(name)
	go func() {
		backoff := time.Second
		for ctx.Err() == nil {
			err := s.Receive(ctx, func(ctx context.Context, m *pubsub.Message) { b.handle(ctx, topic, consumer, h, m) })
			if ctx.Err() != nil {
				return
			}
			b.o.Logger.WarnContext(ctx, "bus.receive_failed", "subscription", name, "error", fmt.Sprint(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
		}
	}()
	return nil
}

func (b *Bus) handle(ctx context.Context, topic, consumer string, h bus.Handler, m *pubsub.Message) {
	env := &pb.EventEnvelope{}
	if err := proto.Unmarshal(m.Data, env); err != nil {
		// Never decodable: nack until the dead-letter policy moves it.
		b.o.Logger.ErrorContext(ctx, "bus.undecodable", "topic", topic, "consumer", consumer, "error", err.Error())
		m.Nack()
		return
	}
	hctx := ctx
	if t, ok := platform.ParseTraceparent(env.GetTraceparent()); ok {
		hctx = platform.WithTrace(ctx, t.Child())
	}
	if err := call(hctx, h, env); err != nil {
		attempt := 0
		if m.DeliveryAttempt != nil {
			attempt = *m.DeliveryAttempt
		}
		b.o.Logger.WarnContext(hctx, "bus.redeliver", "topic", topic, "consumer", consumer, "event_id", env.GetEventId(),
			"attempt", attempt, "error", err.Error())
		// Pub/Sub moves the message itself; the counter assumes the
		// subscription's policy is MaxAttempts (true for subscriptions this
		// adapter creates; keep Terraform's max_delivery_attempts equal).
		if attempt >= b.o.MaxAttempts {
			b.o.Metrics.BusDeadLetters.Inc(topic)
		}
		m.Nack()
		return
	}
	m.Ack()
}

func call(ctx context.Context, h bus.Handler, env *pb.EventEnvelope) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New("bus: handler panicked")
		}
	}()
	return h(ctx, env)
}

// Close flushes and stops the publishers.
func (b *Bus) Close() {
	b.mu.Lock()
	ps := b.publishers
	b.publishers = map[string]*pubsub.Publisher{}
	b.mu.Unlock()
	for _, p := range ps {
		p.Stop()
	}
}

var _ bus.Bus = (*Bus)(nil)
