// Package bustest is the contract every bus.Bus adapter must meet (spec
// 11.7, 23.7; G-SVC-01): at-least-once delivery to every subscription,
// redelivery after a handler error, dead-lettering after the configured
// attempts, ordering per ordering key, and exactly-once effects through an
// inbox. It is imported only from tests (bus.Mem, bus.PgBus and
// pubsubbus.Bus over pstest run it).
package bustest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/bus"
)

// Options describe the adapter under test.
type Options struct {
	// New returns a fresh, empty bus for one subtest; cleanup is the
	// test's (t.Cleanup).
	New func(t *testing.T) bus.Bus
	// MaxAttempts is the number of deliveries before a message is
	// dead-lettered.
	MaxAttempts int
	// Timeout bounds every wait (default 20 s).
	Timeout time.Duration
}

// Run runs the contract as subtests.
func Run(t *testing.T, o Options) {
	if o.Timeout <= 0 {
		o.Timeout = 20 * time.Second
	}
	t.Run("AtLeastOnceEverySubscription", func(t *testing.T) { atLeastOnce(t, o) })
	t.Run("RedeliveryAfterError", func(t *testing.T) { redelivery(t, o) })
	t.Run("DeadLetterAfterMaxAttempts", func(t *testing.T) { deadLetter(t, o) })
	t.Run("OrderingPerKey", func(t *testing.T) { ordering(t, o) })
	t.Run("InboxDedupe", func(t *testing.T) { inboxDedupe(t, o) })
}

// EventID returns the n-th test event id (a UUIDv7-shaped string, as
// PgInbox needs a UUID).
func EventID(n int) string { return fmt.Sprintf("0190a1b2-0000-7000-8000-%012x", n) }

// RunEnvelope is a run.events envelope whose ordering key is run.
func RunEnvelope(n int, run string, seq int64) *pb.EventEnvelope {
	return &pb.EventEnvelope{EventId: EventID(n), Type: "run.run_progress", SchemaVersion: 1,
		TenantId: "ten_test", Payload: &pb.EventEnvelope_RunEvent{RunEvent: &pb.RunEventPayload{RunId: run, Seq: seq}}}
}

func waitFor(t *testing.T, o Options, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(o.Timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type recorder struct {
	mu   sync.Mutex
	seen map[string][]string // consumer -> event ids, in handling order
}

func (r *recorder) add(c, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = map[string][]string{}
	}
	r.seen[c] = append(r.seen[c], id)
}

func (r *recorder) distinct(c string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := map[string]bool{}
	for _, id := range r.seen[c] {
		m[id] = true
	}
	return len(m)
}

func atLeastOnce(t *testing.T, o Options) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := o.New(t)
	var rec recorder
	for _, c := range []string{"alpha", "beta"} {
		if err := b.Subscribe(ctx, "run.events", c, func(_ context.Context, env *pb.EventEnvelope) error {
			rec.add(c, env.GetEventId())
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	const n = 20
	for i := range n {
		if err := b.Publish(ctx, "run.events", RunEnvelope(i+1, fmt.Sprintf("run_%d", i%4), int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, o, "every message on both subscriptions", func() bool { return rec.distinct("alpha") == n && rec.distinct("beta") == n })
}

func redelivery(t *testing.T, o Options) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := o.New(t)
	var mu sync.Mutex
	calls := 0
	done := false
	if err := b.Subscribe(ctx, "scope.events", "flaky", func(_ context.Context, env *pb.EventEnvelope) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		done = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(ctx, "scope.events", &pb.EventEnvelope{EventId: EventID(1), Type: "scope.version_published", TenantId: "ten_test", ProgramId: "prg_x"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, o, "the third attempt", func() bool { mu.Lock(); defer mu.Unlock(); return done })
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("handler called %d times, want 3", calls)
	}
}

func deadLetter(t *testing.T, o Options) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := o.New(t)
	var mu sync.Mutex
	attempts := 0
	var dead []string
	if err := b.Subscribe(ctx, "graph.changes", "poison", func(context.Context, *pb.EventEnvelope) error {
		mu.Lock()
		attempts++
		mu.Unlock()
		return errors.New("always fails")
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Subscribe(ctx, "graph.changes"+bus.DeadLetterSuffix, "ops", func(_ context.Context, env *pb.EventEnvelope) error {
		mu.Lock()
		dead = append(dead, env.GetEventId())
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(ctx, "graph.changes", &pb.EventEnvelope{EventId: EventID(7), Type: "graph.entity_added", TenantId: "ten_test", ProgramId: "prg_x"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, o, "the dead letter", func() bool { mu.Lock(); defer mu.Unlock(); return len(dead) > 0 })
	time.Sleep(100 * time.Millisecond) // no further attempts after dead-lettering
	mu.Lock()
	defer mu.Unlock()
	// The dead-letter topic is itself at least once: pstest re-publishes a
	// dead-lettered message on every delivery cycle (its ack of the
	// original uses a stale ack id), so count distinct event ids.
	if distinct := slices.Compact(slices.Sorted(slices.Values(dead))); attempts != o.MaxAttempts || len(distinct) != 1 || distinct[0] != EventID(7) {
		t.Fatalf("attempts %d (want %d), dead letters %v", attempts, o.MaxAttempts, dead)
	}
}

// ordering publishes 3 keys x 8 messages interleaved; the handler fails
// the third message of key A once. Each key must be handled in
// publication order, and nothing after A's third message may be handled
// before that message succeeds.
func ordering(t *testing.T, o Options) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := o.New(t)
	const perKey = 8
	keys := []string{"run_a", "run_b", "run_c"}
	var mu sync.Mutex
	log := map[string][]int64{}   // key -> seqs in invocation order
	okLog := map[string][]int64{} // key -> seqs acked
	failed := false
	if err := b.Subscribe(ctx, "run.events", "ordered", func(_ context.Context, env *pb.EventEnvelope) error {
		re := env.GetRunEvent()
		mu.Lock()
		defer mu.Unlock()
		log[re.GetRunId()] = append(log[re.GetRunId()], re.GetSeq())
		if re.GetRunId() == "run_a" && re.GetSeq() == 2 && !failed {
			failed = true
			return errors.New("fail once")
		}
		okLog[re.GetRunId()] = append(okLog[re.GetRunId()], re.GetSeq())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	n := 0
	for s := range perKey {
		for _, k := range keys {
			n++
			if err := b.Publish(ctx, "run.events", RunEnvelope(n, k, int64(s))); err != nil {
				t.Fatal(err)
			}
		}
	}
	waitFor(t, o, "every key", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, k := range keys {
			if len(okLog[k]) < perKey {
				return false
			}
		}
		return true
	})
	mu.Lock()
	defer mu.Unlock()
	for _, k := range keys {
		acked := slices.Compact(slices.Clone(okLog[k]))
		if !slices.IsSorted(acked) || len(acked) != perKey {
			t.Errorf("%s acked out of order: %v", k, okLog[k])
		}
		// Invocations never go past a message that has not succeeded:
		// after a failure the same seq comes back first.
		inv := log[k]
		for i := 1; i < len(inv); i++ {
			if inv[i] > inv[i-1]+1 {
				t.Errorf("%s skipped ahead: %v", k, inv)
				break
			}
		}
	}
	if !slices.Contains(log["run_a"], 2) || countOf(log["run_a"], 2) < 2 {
		t.Errorf("run_a seq 2 was not redelivered: %v", log["run_a"])
	}
}

func countOf(s []int64, v int64) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}

// inboxDedupe publishes the same event twice (a relay crash between
// publish and mark); the inbox makes the effect happen once.
func inboxDedupe(t *testing.T, o Options) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := o.New(t)
	inbox := bus.NewMemInbox()
	var mu sync.Mutex
	calls, applied := 0, 0
	if err := b.Subscribe(ctx, "auth.events", "notifier", func(ctx context.Context, env *pb.EventEnvelope) error {
		fresh, err := inbox.MarkProcessed(ctx, "notifier", env.GetEventId())
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		calls++
		if fresh {
			applied++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	env := &pb.EventEnvelope{EventId: EventID(42), Type: "auth.login_succeeded", TenantId: "ten_test"}
	for range 2 {
		if err := b.Publish(ctx, "auth.events", env); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, o, "both deliveries", func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 2 })
	mu.Lock()
	defer mu.Unlock()
	if applied != 1 {
		t.Fatalf("applied %d times, want 1", applied)
	}
}
