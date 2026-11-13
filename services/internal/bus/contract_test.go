package bus_test

import (
	"context"
	"sync"
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

// TestRegress_G_SVC_25_PgOutboxRelaysKeepKeyOrder: two relays (two engine
// replicas) claimed one outbox with FOR UPDATE SKIP LOCKED, so while the
// first was publishing a batch the second took the next rows and could
// publish an event of the same run before an earlier one. A second relay
// now claims nothing until the first commits.
func TestRegress_G_SVC_25_PgOutboxRelaysKeepKeyOrder(t *testing.T) {
	ctx := context.Background()
	pool := pgPool(t)
	for i := 1; i <= 2; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO engine.outbox (event_id, topic, ordering_key, payload) VALUES ($1, 'run.events', 'run_1', '{}')`,
			bustest.EventID(i)); err != nil {
			t.Fatal(err)
		}
	}
	ob := bus.PgOutbox{DB: pool, Table: "engine.outbox"}
	var mu sync.Mutex
	var order []int64
	publish := func(_ context.Context, rows []bus.OutboxRow) error {
		mu.Lock()
		defer mu.Unlock()
		for _, r := range rows {
			order = append(order, r.ID)
		}
		return nil
	}
	inA, release := make(chan struct{}), make(chan struct{})
	doneA := make(chan error, 1)
	go func() {
		_, err := ob.Claim(ctx, 1, func(ctx context.Context, rows []bus.OutboxRow) error {
			close(inA)
			<-release
			return publish(ctx, rows)
		})
		doneA <- err
	}()
	<-inA
	n, err := ob.Claim(ctx, 1, publish)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-doneA; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a second relay claimed %d rows while the first was publishing", n)
	}
	if n, err := ob.Claim(ctx, 10, publish); err != nil || n != 1 {
		t.Fatalf("after the first relay: claimed %d (%v)", n, err)
	}
	if len(order) != 2 || order[0] >= order[1] {
		t.Fatalf("publication order %v", order)
	}
}
