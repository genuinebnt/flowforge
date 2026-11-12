package bus_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/convert"
)

func wait(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500 && !cond(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("timed out")
	}
}

func TestMemBus_RedeliveryAndDeadLetter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := bus.NewMem(nil, nil)
	b.Backoff = time.Millisecond
	var flaky, poison, dead atomic.Int32
	_ = b.Subscribe(ctx, "t", "flaky", func(context.Context, *pb.EventEnvelope) error {
		if flaky.Add(1) < 3 {
			return errors.New("transient")
		}
		return nil
	})
	_ = b.Subscribe(ctx, "t", "poison", func(context.Context, *pb.EventEnvelope) error { poison.Add(1); return errors.New("always") })
	_ = b.Subscribe(ctx, "t"+bus.DeadLetterSuffix, "ops", func(context.Context, *pb.EventEnvelope) error { dead.Add(1); return nil })
	_ = b.Publish(ctx, "t", &pb.EventEnvelope{EventId: "e1"})
	wait(t, func() bool { return flaky.Load() == 3 && dead.Load() == 1 })
	if poison.Load() != int32(b.MaxAttempts) || len(b.DeadLetters()) != 1 || b.DeadLetters()[0].Consumer != "poison" {
		t.Fatalf("poison attempts %d, dead letters %v", poison.Load(), b.DeadLetters())
	}
}

func TestOutboxRelayAndInbox(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := bus.NewMem(nil, nil)
	out := bus.NewMemOutbox()
	inbox := bus.NewMemInbox()
	var applied atomic.Int32
	_ = b.Subscribe(ctx, convert.TopicScopeEvents, "c", func(ctx context.Context, env *pb.EventEnvelope) error {
		if ok, _ := inbox.MarkProcessed(ctx, "c", env.GetEventId()); ok {
			applied.Add(1)
		}
		return nil
	})
	row := bus.OutboxRow{EventID: "0190a1b2-0000-7000-8000-000000000001", Topic: convert.TopicScopeEvents,
		Payload: []byte(`{"event_id":"0190a1b2-0000-7000-8000-000000000001","type":"scope.version_published","schema_version":1,"tenant_id":"0190a1b2-0000-7000-8000-00000000a001","occurred_at":"2026-11-12T14:00:00.000Z","payload":{"version":2}}`)}
	out.Append(row, row) // the same event id twice is stored once
	r := &bus.Relay{Source: out, Bus: b}
	if n, err := r.RunOnce(ctx); n != 1 || err != nil {
		t.Fatalf("relay %d %v", n, err)
	}
	_ = b.Publish(ctx, convert.TopicScopeEvents, &pb.EventEnvelope{EventId: row.EventID}) // a redelivery
	wait(t, func() bool { return applied.Load() == 1 })
	time.Sleep(20 * time.Millisecond)
	if applied.Load() != 1 || out.Pending() != 0 {
		t.Fatalf("applied %d, pending %d", applied.Load(), out.Pending())
	}
}
