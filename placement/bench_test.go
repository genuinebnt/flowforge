package placement_test

import (
	"fmt"
	"testing"

	"github.com/genuinebnt/flowforge/placement"
)

// BenchmarkPlc01_Owner is b_plc_01: owner lookup at 10, 100 and 1,000
// workers. "consistent" goes through the Placement port (ring cached,
// membership compared on every call); "ring" is the bare binary search.
func BenchmarkPlc01_Owner(b *testing.B) {
	ks := keys(1024)
	for _, n := range []int{10, 100, 1000} {
		live := workers(n)
		ring := placement.NewRing(live, placement.ConsistentConfig{})
		ch := placement.NewConsistentHash(placement.ConsistentConfig{})
		cases := []struct {
			name  string
			owner func(k []byte) (placement.WorkerID, bool)
		}{
			{"rendezvous", func(k []byte) (placement.WorkerID, bool) { return placement.Rendezvous{}.Owner(k, live) }},
			{"consistent", func(k []byte) (placement.WorkerID, bool) { return ch.Owner(k, live) }},
			{"ring", ring.Owner},
		}
		for _, c := range cases {
			b.Run(fmt.Sprintf("%s/workers=%d", c.name, n), func(b *testing.B) {
				c.owner(ks[0]) // warm the ConsistentHash ring cache
				b.ReportAllocs()
				i := 0
				for b.Loop() {
					if _, ok := c.owner(ks[i&1023]); !ok {
						b.Fatal("no owner")
					}
					i++
				}
			})
		}
	}
}

// BenchmarkPlcAlgo compares the selectable placement schemes (one decision
// point: run ownership, spec 23.3) through the Placement port at 10, 100
// and 1,000 workers; Owner and Ranking (the claim order of ClaimDelay):
//
//	go test -run '^$' -bench PlcAlgo ./placement
func BenchmarkPlcAlgo(b *testing.B) {
	ks := keys(1024)
	for _, s := range placement.Schemes() {
		for _, n := range []int{10, 100, 1000} {
			live := workers(n)
			p, err := placement.New(s, placement.ConsistentConfig{})
			if err != nil {
				b.Fatal(err)
			}
			b.Run(fmt.Sprintf("%s/workers=%d/owner", s, n), func(b *testing.B) {
				p.Owner(ks[0], live) // warm the ring cache
				b.ReportAllocs()
				i := 0
				for b.Loop() {
					if _, ok := p.Owner(ks[i&1023], live); !ok {
						b.Fatal("no owner")
					}
					i++
				}
			})
			b.Run(fmt.Sprintf("%s/workers=%d/ranking", s, n), func(b *testing.B) {
				b.ReportAllocs()
				i := 0
				for b.Loop() {
					if len(p.Ranking(ks[i&1023], live)) != n {
						b.Fatal("short ranking")
					}
					i++
				}
			})
		}
	}
}
