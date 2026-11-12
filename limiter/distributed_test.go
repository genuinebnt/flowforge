package limiter

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// leftover sums the unspent tokens of a node's held leases.
func leftover(b *LeasedBudget) int {
	n := 0
	for _, l := range b.leases {
		n += l.left
	}
	return n
}

// available drains what the store can still grant for key at now.
func available(t *testing.T, s *MemStore, key string, burst int, now time.Duration) int {
	t.Helper()
	l, err := s.Lease(context.Background(), LeaseRequest{Key: key, Holder: "probe", Min: 1, Want: burst, TTL: time.Second, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return l.Granted
}

// TestLim011_LeasedBudget: nodes lease chunks of a global budget, spend
// locally, let leases expire, return remainders and crash. Total spent
// across holders never exceeds the global budget, and every token is
// accounted for exactly once: spent + forfeited (crashed holders, leases
// pruned before their return) + still available == budget.
func TestLim011_LeasedBudget(t *testing.T) {
	ctx := context.Background()
	const budget = 50
	// A rate of one token per ~11.6 days makes the bucket a fixed budget
	// over the simulated minute.
	fixed := Params{Rate: 1e-6, Burst: budget}
	r := testkit.Rng(t)
	for c := range testkit.Cases() / 10 {
		store, err := NewMemStore(all(fixed))
		if err != nil {
			t.Fatal(err)
		}
		newNode := func(i int) *LeasedBudget {
			b, err := NewLeasedBudget(LeaseConfig{Store: store, Holder: fmt.Sprint("node", i), Chunk: 1 + core.IntN(r, 8), Margin: ms(50)})
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		nodes := []*LeasedBudget{newNode(0), newNode(1), newNode(2)}
		spent, forfeited := 0, 0
		now := time.Duration(0)
		for range 300 {
			now += ms(int64(core.IntN(r, 400)))
			i := core.IntN(r, len(nodes))
			if core.IntN(r, 40) == 0 { // crash: held leases are never returned
				forfeited += leftover(nodes[i]) + nodes[i].Forfeited()
				nodes[i] = newNode(i)
				continue
			}
			cost := 1 + core.IntN(r, 3)
			res, err := nodes[i].Acquire(ctx, "k", cost, now)
			if err != nil {
				t.Fatal(err)
			}
			if res.Allowed {
				spent += cost
			}
			if spent > budget {
				t.Fatalf("case %d: spent %d > budget %d", c, spent, budget)
			}
		}
		for _, n := range nodes {
			n.Close(ctx, now)
			if leftover(n) != 0 {
				t.Fatal("Close kept a lease")
			}
			forfeited += n.Forfeited() // leases pruned after a long idle period
		}
		if got := available(t, store, "k", budget, now); spent+forfeited+got != budget {
			t.Fatalf("case %d: spent %d + forfeited %d + available %d != budget %d", c, spent, forfeited, got, budget)
		}
	}

	t.Run("refilling budget", func(t *testing.T) {
		// With a refilling central bucket the bound is burst + rate * elapsed.
		p := Params{Rate: 5, Burst: 20}
		store, _ := NewMemStore(all(p))
		var nodes []*LeasedBudget
		for i := range 4 {
			n, _ := NewLeasedBudget(LeaseConfig{Store: store, Holder: fmt.Sprint(i), Chunk: 3})
			nodes = append(nodes, n)
		}
		spent, now := 0, time.Duration(0)
		for range 5000 {
			now += ms(int64(core.IntN(r, 20)))
			if res, err := nodes[core.IntN(r, 4)].Acquire(ctx, "k", 1, now); err != nil {
				t.Fatal(err)
			} else if res.Allowed {
				spent++
			}
			if bound := 20 + int(5*now/time.Second); spent > bound {
				t.Fatalf("spent %d > %d at %v", spent, bound, now)
			}
		}
		if spent < int(5*now/time.Second)/2 {
			t.Fatalf("spent only %d over %v", spent, now)
		}
	})

	t.Run("expiry", func(t *testing.T) {
		store, _ := NewMemStore(all(fixed))
		node, _ := NewLeasedBudget(LeaseConfig{Store: store, Holder: "a", Chunk: 10})
		if res, _ := node.Acquire(ctx, "k", 1, 0); !res.Allowed || res.Remaining != 9 {
			t.Fatalf("first acquire: %+v", res)
		}
		old := node.leases["k"].ID
		// At expiry the node stops spending the old lease: it returns the 9
		// unspent tokens and leases afresh.
		if res, _ := node.Acquire(ctx, "k", 1, DefaultLeaseTTL); !res.Allowed || node.leases["k"].ID == old {
			t.Fatalf("expired lease reused: %+v", res)
		}
		if err := store.Return(ctx, old, 9, DefaultLeaseTTL); !errors.Is(err, ErrUnknownLease) {
			t.Fatalf("second return of an expired lease: %v", err)
		}
		node.Close(ctx, DefaultLeaseTTL)
		if got := available(t, store, "k", budget, DefaultLeaseTTL); got != budget-2 {
			t.Fatalf("available %d, want %d", got, budget-2)
		}
	})

	t.Run("pruned lease is forfeited", func(t *testing.T) {
		store, _ := NewMemStore(all(fixed))
		l, _ := store.Lease(ctx, LeaseRequest{Key: "k", Min: 1, Want: 5, TTL: time.Second, Now: 0})
		store.Lease(ctx, LeaseRequest{Key: "k", Min: 1, Want: 1, TTL: time.Second, Now: 3 * time.Second}) // prunes l
		if err := store.Return(ctx, l.ID, 5, 3*time.Second); !errors.Is(err, ErrUnknownLease) {
			t.Fatalf("return after prune: %v", err)
		}
		if store.Open() != 1 {
			t.Fatalf("open leases %d", store.Open())
		}
	})

	t.Run("denial charges nothing", func(t *testing.T) {
		store, _ := NewMemStore(Rule{"a", Params{Rate: 1e-6, Burst: 10}}, Rule{"b", Params{Rate: 1e-6, Burst: 1}})
		node, _ := NewLeasedBudget(LeaseConfig{Store: store, Chunk: 1})
		if res, _ := node.AcquireAll(ctx, []Request{{"a", 1}, {"b", 1}}, 0); !res.Allowed {
			t.Fatalf("first: %+v", res)
		}
		res, err := node.AcquireAll(ctx, []Request{{"a", 1}, {"b", 1}}, 0)
		if err != nil || res.Allowed || res.RetryAfter <= 0 {
			t.Fatalf("b exhausted: %+v %v", res, err)
		}
		node.Close(ctx, 0)
		if got := available(t, store, "a", 10, 0); got != 9 {
			t.Fatalf("a available %d, want 9", got)
		}
	})

	t.Run("errors", func(t *testing.T) {
		store, _ := NewMemStore(all(Params{Rate: 1, Burst: 3}))
		node, _ := NewLeasedBudget(LeaseConfig{Store: store, Chunk: 2})
		if res, err := node.Acquire(ctx, "k", 4, 0); !errors.Is(err, ErrCostExceedsCapacity) || res.RetryAfter != Never {
			t.Fatalf("cost > burst: %+v %v", res, err)
		}
		if _, err := node.Acquire(ctx, "k", 0, 0); !errors.Is(err, ErrInvalidCost) {
			t.Fatalf("zero cost: %v", err)
		}
		if _, err := node.AcquireAll(ctx, nil, 0); !errors.Is(err, ErrEmptyRequest) {
			t.Fatalf("empty: %v", err)
		}
		for _, cfg := range []LeaseConfig{{Chunk: 1}, {Store: store}, {Store: store, Chunk: 1, TTL: time.Second, Margin: time.Second}} {
			if _, err := NewLeasedBudget(cfg); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("config %+v: %v", cfg, err)
			}
		}
		if err := store.Return(ctx, "nope", 0, 0); !errors.Is(err, ErrUnknownLease) {
			t.Fatalf("unknown lease: %v", err)
		}
	})
}

// downStore is a Store that is always unreachable.
type downStore struct{}

func (downStore) Lease(context.Context, LeaseRequest) (Lease, error) {
	return Lease{}, errors.New("connection refused")
}
func (downStore) Return(context.Context, string, int, time.Duration) error {
	return errors.New("connection refused")
}

// TestLimFailover: on a store outage active work is denied (fail closed)
// and passive work falls back to the conservative local limiter (23.8).
func TestLimFailover(t *testing.T) {
	ctx := context.Background()
	remote, _ := NewLeasedBudget(LeaseConfig{Store: downStore{}, Chunk: 1})
	local, _ := NewGCRA(all(Params{Rate: 1, Burst: 1}))
	f := Failover{Primary: remote, Local: local}
	if res, err := f.Acquire(ctx, "k", 1, WorkActive, 0); !errors.Is(err, ErrUnavailable) || res.Allowed {
		t.Fatalf("active during outage: %+v %v", res, err)
	}
	if res, err := f.Acquire(ctx, "k", 1, WorkPassive, 0); err != nil || !res.Allowed {
		t.Fatalf("passive during outage: %+v %v", res, err)
	}
	if res, _ := f.Acquire(ctx, "k", 1, WorkPassive, 0); res.Allowed {
		t.Fatal("local fallback is not limiting")
	}
	if _, err := (Failover{Primary: remote}).Acquire(ctx, "k", 1, WorkPassive, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("passive without local: %v", err)
	}
	// A healthy primary decides on its own, bad requests are not outages.
	store, _ := NewMemStore(all(Params{Rate: 1, Burst: 2}))
	up, _ := NewLeasedBudget(LeaseConfig{Store: store, Chunk: 1})
	f = Failover{Primary: up, Local: local}
	if res, err := f.Acquire(ctx, "k", 2, WorkActive, 0); err != nil || !res.Allowed {
		t.Fatalf("healthy: %+v %v", res, err)
	}
	if _, err := f.Acquire(ctx, "k", 3, WorkPassive, 0); !errors.Is(err, ErrCostExceedsCapacity) {
		t.Fatalf("bad request: %v", err)
	}
}

func TestLimCRDT(t *testing.T) {
	p := Params{Rate: 10, Burst: 10, Window: time.Second}
	a, _ := NewCRDTLimiter("a", all(p))
	b, _ := NewCRDTLimiter("b", all(p))
	// Partitioned, each node admits the full limit: the documented overshoot.
	for i := range 10 {
		ra, _ := a.Acquire("k", 1, ms(int64(i)))
		rb, _ := b.Acquire("k", 1, ms(int64(i)))
		if !ra.Allowed || !rb.Allowed {
			t.Fatalf("partitioned acquire %d denied", i)
		}
	}
	// After gossip both see 20 and deny.
	sa, sb := a.State("k"), b.State("k")
	a.Merge("k", sb...)
	b.Merge("k", sa...)
	for _, n := range []*CRDTLimiter{a, b} {
		if res, _ := n.Acquire("k", 1, ms(20)); res.Allowed {
			t.Fatal("merged node admitted beyond the limit")
		}
		if got := n.State("k")[1].Value(); got != 20 {
			t.Fatalf("merged total %d", got)
		}
	}
	// Merging again changes nothing (idempotent); a stale window is ignored.
	a.Merge("k", sb...)
	a.Merge("k", GCounter{Window: -5, Counts: map[string]uint64{"z": 100}})
	if got := a.State("k")[1].Value(); got != 20 {
		t.Fatalf("re-merge changed total to %d", got)
	}
	// Two windows later the counts are gone.
	if res, _ := a.Acquire("k", 10, ms(2500)); !res.Allowed {
		t.Fatalf("new window: %+v", res)
	}
	if _, err := NewCRDTLimiter("", all(p)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("empty node id accepted")
	}
	// A node that has never seen a key adopts a gossiped counter.
	c, _ := NewCRDTLimiter("c", all(p))
	c.Merge("k", a.State("k")...)
	if got := c.State("k")[1].Value(); got != 10 {
		t.Fatalf("adopted total %d", got)
	}
}

func TestLimProp_GCounterMerge(t *testing.T) {
	r := testkit.Rng(t)
	gen := func() GCounter {
		g := GCounter{Window: int64(core.IntN(r, 3))}
		for range core.IntN(r, 4) {
			g.Add(fmt.Sprint("n", core.IntN(r, 4)), uint64(core.IntN(r, 10)))
		}
		return g
	}
	merge := func(xs ...GCounter) GCounter {
		var out GCounter
		for _, x := range xs {
			out.Merge(x.Clone())
		}
		return out
	}
	same := func(x, y GCounter) bool {
		if x.Window != y.Window || len(x.Counts) != len(y.Counts) {
			return false
		}
		for k, v := range x.Counts {
			if y.Counts[k] != v {
				return false
			}
		}
		return true
	}
	for range testkit.Cases() {
		x, y, z := gen(), gen(), gen()
		if !same(merge(x, y), merge(y, x)) {
			t.Fatalf("not commutative: %+v %+v", x, y)
		}
		if !same(merge(merge(x, y), z), merge(x, merge(y, z))) {
			t.Fatalf("not associative: %+v %+v %+v", x, y, z)
		}
		if !same(merge(x, x), merge(x)) {
			t.Fatalf("not idempotent: %+v", x)
		}
	}
}
