package placement_test

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/placement"
	"github.com/genuinebnt/flowforge/testkit"
)

// algorithms returns fresh instances of every Placement implementation.
func algorithms() []struct {
	name string
	p    placement.Placement
} {
	return []struct {
		name string
		p    placement.Placement
	}{
		{"rendezvous", placement.Rendezvous{}},
		{"consistent", placement.NewConsistentHash(placement.ConsistentConfig{})},
	}
}

func workers(n int) []placement.WorkerID {
	out := make([]placement.WorkerID, n)
	for i := range out {
		out[i] = placement.WorkerID(fmt.Sprintf("worker-%02d", i+1))
	}
	return out
}

func keys(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = fmt.Appendf(nil, "run-%04d", i)
	}
	return out
}

func TestPlc001_Deterministic(t *testing.T) {
	live := workers(7)
	for i, alg := range algorithms() {
		peer := algorithms()[i].p // a second instance stands in for another node
		t.Run(alg.name, func(t *testing.T) {
			reversed := slices.Clone(live)
			slices.Reverse(reversed)
			for _, k := range keys(200) {
				want, ok := alg.p.Owner(k, live)
				if !ok {
					t.Fatalf("Owner(%q) = none with %d live workers", k, len(live))
				}
				wantRank := alg.p.Ranking(k, live)
				for range 3 {
					if got, _ := alg.p.Owner(k, live); got != want {
						t.Fatalf("Owner(%q) changed between calls: %q then %q", k, want, got)
					}
				}
				if got, _ := peer.Owner(k, reversed); got != want {
					t.Fatalf("Owner(%q) on another node with reordered membership = %q, want %q", k, got, want)
				}
				if got := peer.Ranking(k, reversed); !slices.Equal(got, wantRank) {
					t.Fatalf("Ranking(%q) differs across nodes: %v vs %v", k, got, wantRank)
				}
				if wantRank[0] != want {
					t.Fatalf("Ranking(%q)[0] = %q, want owner %q", k, wantRank[0], want)
				}
			}
		})
	}
}

// shares returns each worker's fraction of keys.
func shares(p placement.Placement, ks [][]byte, live []placement.WorkerID) (map[string]placement.WorkerID, map[placement.WorkerID]float64) {
	owners := make(map[string]placement.WorkerID, len(ks))
	counts := make(map[placement.WorkerID]float64, len(live))
	for _, k := range ks {
		w, _ := p.Owner(k, live)
		owners[string(k)] = w
		counts[w]++
	}
	for w := range counts {
		counts[w] /= float64(len(ks))
	}
	return owners, counts
}

func TestPlc002_MinimalMovement(t *testing.T) {
	// "Within a few percent of uniform": every worker's share is within five
	// percentage points of 1/n.
	const tolerance = 0.05
	ks := keys(1000)
	before := workers(5)
	removed := before[2]
	after := slices.Delete(slices.Clone(before), 2, 3)

	for _, alg := range algorithms() {
		t.Run(alg.name, func(t *testing.T) {
			ownersBefore, shareBefore := shares(alg.p, ks, before)
			ownersAfter, shareAfter := shares(alg.p, ks, after)
			moved := 0
			for k, was := range ownersBefore {
				now := ownersAfter[k]
				switch {
				case was == removed:
					moved++
					if now == removed {
						t.Fatalf("key %q still owned by removed worker", k)
					}
				case now != was:
					t.Fatalf("key %q moved from live worker %q to %q", k, was, now)
				}
			}
			t.Logf("%d of %d keys moved (expected about %d)", moved, len(ks), len(ks)/len(before))
			if moved == 0 {
				t.Fatal("removed worker owned no keys")
			}
			checkUniform(t, "before", shareBefore, before, tolerance)
			checkUniform(t, "after", shareAfter, after, tolerance)
		})
	}
}

func checkUniform(t *testing.T, label string, share map[placement.WorkerID]float64, live []placement.WorkerID, tol float64) {
	t.Helper()
	ideal := 1 / float64(len(live))
	for _, w := range live {
		if d := math.Abs(share[w] - ideal); d > tol {
			t.Errorf("%s: worker %q has share %.3f, ideal %.3f (off by %.3f > %.3f)", label, w, share[w], ideal, d, tol)
		}
	}
}

func TestPlc003_EmptyMembership(t *testing.T) {
	for _, alg := range algorithms() {
		t.Run(alg.name, func(t *testing.T) {
			for _, live := range [][]placement.WorkerID{nil, {}} {
				if w, ok := alg.p.Owner([]byte("k"), live); ok || w != "" {
					t.Fatalf("Owner with empty membership = (%q, %v), want none", w, ok)
				}
				if r := alg.p.Ranking([]byte("k"), live); r == nil || len(r) != 0 {
					t.Fatalf("Ranking with empty membership = %#v, want empty slice", r)
				}
				if _, ok := placement.ClaimDelay(alg.p, []byte("k"), live, "w1", time.Second); ok {
					t.Fatal("ClaimDelay with empty membership reported a delay")
				}
			}
		})
	}
	ring := placement.NewRing(nil, placement.ConsistentConfig{})
	if _, ok := ring.Owner([]byte("k")); ok {
		t.Fatal("empty Ring has an owner")
	}
	if len(ring.Ranking([]byte("k"))) != 0 {
		t.Fatal("empty Ring has a ranking")
	}
}

// randomLive draws 1..maxN worker ids (possibly with duplicates).
func randomLive(rng core.Rng, maxN int) []placement.WorkerID {
	n := 1 + core.IntN(rng, maxN)
	out := make([]placement.WorkerID, n)
	for i := range out {
		out[i] = placement.WorkerID(fmt.Sprintf("w%d", core.IntN(rng, 3*maxN)))
	}
	return out
}

func randomKey(rng core.Rng) []byte {
	k := make([]byte, core.IntN(rng, 24))
	for i := range k {
		k[i] = byte(rng.Uint64())
	}
	return k
}

func TestPlcProp_OwnerIsLive(t *testing.T) {
	rng := testkit.Rng(t)
	for _, alg := range algorithms() {
		t.Run(alg.name, func(t *testing.T) {
			for range testkit.Cases() {
				live := randomLive(rng, 16)
				key := randomKey(rng)
				w, ok := alg.p.Owner(key, live)
				if !ok || !slices.Contains(live, w) {
					t.Fatalf("Owner(%x, %v) = (%q, %v): not a live worker", key, live, w, ok)
				}
				r := alg.p.Ranking(key, live)
				want := slices.Clone(live)
				slices.Sort(want)
				want = slices.Compact(want)
				got := slices.Clone(r)
				slices.Sort(got)
				if !slices.Equal(got, want) {
					t.Fatalf("Ranking(%x, %v) = %v: not a permutation of the distinct live set", key, live, r)
				}
				if r[0] != w {
					t.Fatalf("Ranking(%x)[0] = %q, Owner = %q", key, r[0], w)
				}
			}
		})
	}
}

func TestPlcProp_AddOnlyMovesToNew(t *testing.T) {
	rng := testkit.Rng(t)
	for i, alg := range algorithms() {
		// A separate instance per membership keeps each ConsistentHash
		// ring cache warm, as on two nodes with different views.
		grownAlg := algorithms()[i].p
		t.Run(alg.name, func(t *testing.T) {
			for range testkit.Cases() {
				live := randomLive(rng, 12)
				added := placement.WorkerID(fmt.Sprintf("new-%d", rng.Uint64()))
				grown := append(slices.Clone(live), added)
				for range 10 {
					key := randomKey(rng)
					was, _ := alg.p.Owner(key, live)
					now, _ := grownAlg.Owner(key, grown)
					if now != was && now != added {
						t.Fatalf("adding %q moved key %x from %q to %q", added, key, was, now)
					}
				}
			}
		})
	}
}

func TestRank_ClaimDelay(t *testing.T) {
	live := workers(4)
	key := []byte("run-0001")
	for _, alg := range algorithms() {
		t.Run(alg.name, func(t *testing.T) {
			ranking := alg.p.Ranking(key, live)
			for i, w := range ranking {
				d, ok := placement.ClaimDelay(alg.p, key, live, w, 2*time.Second)
				if !ok || d != time.Duration(i)*2*time.Second {
					t.Fatalf("ClaimDelay(%q) = (%v, %v), want (%v, true)", w, d, ok, time.Duration(i)*2*time.Second)
				}
			}
			if _, ok := placement.Rank(alg.p, key, live, "stranger"); ok {
				t.Fatal("Rank of a non-member reported ok")
			}
		})
	}
}

func TestConsistentWeights(t *testing.T) {
	live := workers(3)
	heavy := live[0]
	ch := placement.NewConsistentHash(placement.ConsistentConfig{
		Weight: func(w placement.WorkerID) int {
			if w == heavy {
				return 2
			}
			return 1
		},
	})
	_, share := shares(ch, keys(20000), live)
	if share[heavy] < 0.45 || share[heavy] > 0.55 {
		t.Fatalf("weight-2 worker share = %.3f, want about 0.50", share[heavy])
	}
}

func TestStaticMembership(t *testing.T) {
	m := placement.StaticMembership{"b", "a", "b"}
	if got := m.Live(); !slices.Equal(got, []placement.WorkerID{"a", "b"}) {
		t.Fatalf("Live() = %v", got)
	}
}

func TestHeartbeatMembership(t *testing.T) {
	clk := testkit.NewFakeClock(time.Unix(0, 0))
	m := placement.NewHeartbeatMembership(clk, 0)
	m.Heartbeat("w1")
	clk.Advance(placement.HeartbeatInterval)
	m.Heartbeat("w2")

	steps := []struct {
		advance time.Duration
		want    []placement.WorkerID
	}{
		{0, []placement.WorkerID{"w1", "w2"}},
		{10*time.Second - time.Millisecond, []placement.WorkerID{"w1", "w2"}}, // w1 at 14.999s
		{time.Millisecond, []placement.WorkerID{"w2"}},                        // w1 at exactly 15s: dead
		{5 * time.Second, []placement.WorkerID{}},                             // w2 at 15s
	}
	for i, s := range steps {
		clk.Advance(s.advance)
		if got := m.Live(); !slices.Equal(got, s.want) {
			t.Fatalf("step %d: Live() = %v, want %v", i, got, s.want)
		}
	}
	m.Heartbeat("w1")
	m.Heartbeat("w3")
	m.Leave("w3")
	if got := m.Live(); !slices.Equal(got, []placement.WorkerID{"w1"}) {
		t.Fatalf("after re-heartbeat and leave: Live() = %v", got)
	}
}
