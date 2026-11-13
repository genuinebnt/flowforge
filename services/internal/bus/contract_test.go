package bus_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/bus/bustest"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
)

func TestBusContract_Mem(t *testing.T) {
	bustest.Run(t, bustest.Options{MaxAttempts: 5, New: func(t *testing.T) bus.Bus {
		b := bus.NewMem(nil, nil)
		b.Backoff = time.Millisecond
		return b
	}})
}

// pgPool returns a pool on a fresh, migrated database (skipped without
// FF_TEST_PG_DSN).
func pgPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := servicetest.FreshDB(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrations.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func newPgBus(t testing.TB, pool *pgxpool.Pool) *bus.PgBus {
	b := &bus.PgBus{Pool: pool, MaxAttempts: 5, Backoff: 5 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, Poll: 50 * time.Millisecond}
	return b
}

func TestBusContract_Postgres(t *testing.T) {
	bustest.Run(t, bustest.Options{MaxAttempts: 5, New: func(t *testing.T) bus.Bus { return newPgBus(t, pgPool(t)) }})
}

// TestRegress_GSVC01_PgBusDurableAcrossProcesses: a subscription outlives
// its consumer. Messages published while no process consumes are delivered
// when a new bus instance (another process) subscribes; a delivery whose
// consumer died mid-handler (lease not released) is redelivered after the
// lease expires; acked rows are removed by Cleanup.
func TestRegress_GSVC01_PgBusDurableAcrossProcesses(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	first := newPgBus(t, pool)
	first.Lease = 300 * time.Millisecond
	c1, stop1 := context.WithCancel(ctx)
	got1 := make(chan string, 10)
	block := make(chan struct{})
	if err := first.Subscribe(c1, "run.events", "engine", func(ctx context.Context, env *pb.EventEnvelope) error {
		got1 <- env.GetEventId()
		select { // simulate a crash: never return before the process dies
		case <-block:
		case <-ctx.Done():
		}
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.Publish(ctx, "run.events", bustest.RunEnvelope(1, "run_1", 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got1:
	case <-time.After(10 * time.Second):
		t.Fatal("first consumer got nothing")
	}
	stop1() // the process dies holding the lease
	close(block)

	// Published while nobody consumes.
	if err := first.Publish(ctx, "run.events", bustest.RunEnvelope(2, "run_1", 2)); err != nil {
		t.Fatal(err)
	}

	second := newPgBus(t, pool)
	got2 := make(chan string, 10)
	c2, stop2 := context.WithCancel(ctx)
	defer stop2()
	if err := second.Subscribe(c2, "run.events", "engine", func(_ context.Context, env *pb.EventEnvelope) error {
		got2 <- env.GetEventId()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var order []string
	for len(order) < 2 {
		select {
		case id := <-got2:
			order = append(order, id)
		case <-time.After(10 * time.Second):
			t.Fatalf("second consumer got %v", order)
		}
	}
	if order[0] != bustest.EventID(1) || order[1] != bustest.EventID(2) {
		t.Fatalf("redelivery order %v (the leased message must come first: same ordering key)", order)
	}
	stop2()
	time.Sleep(50 * time.Millisecond)
	if _, err := pool.Exec(ctx, `UPDATE bus.deliveries SET done_at = now() - interval '2 days' WHERE status = 'acked'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE bus.messages SET published_at = now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	n, err := second.Cleanup(ctx, bus.Retention{})
	if err != nil || n != 4 {
		t.Fatalf("cleanup removed %d rows (%v), want 2 deliveries + 2 messages", n, err)
	}
}
