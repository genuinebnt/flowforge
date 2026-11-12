package oam_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
)

// benchObservations builds n observations over n/5 entities (spec 25.8
// b_oam_01 scaled down) of which 30% are re-deliveries of earlier ones.
// Each observation reports a host name, its address and the A record
// between them.
func benchObservations(n int) []oam.Observation {
	r := core.NewSplitMix64(1)
	hosts := max(n/10, 1) // two entities (name and IP) per host: n/5 entities
	tools := []string{"subfinder", "dnsx", "httpx"}
	out := make([]oam.Observation, 0, n)
	for i := 0; len(out) < n; i++ {
		if len(out) > 0 && core.IntN(r, 10) < 3 {
			out = append(out, out[core.IntN(r, len(out))])
			continue
		}
		h := core.IntN(r, hosts)
		out = append(out, oam.Observation{
			ObservationID: fmt.Sprintf("obs-%d", i), TenantID: "ten", ProgramID: "prg", RunID: "run",
			Tool:       oam.Tool{Name: tools[core.IntN(r, len(tools))]},
			ObservedAt: t0.Add(time.Duration(i) * time.Millisecond), Confidence: 0.8,
			Entities: []oam.ObservedEntity{
				{Ref: "h", Asset: oam.FQDN{Name: fmt.Sprintf("host%d.acme.com", h)}},
				{Ref: "a", Asset: oam.IPAddress{Address: fmt.Sprintf("10.%d.%d.%d", h>>16&255, h>>8&255, h&255)}},
			},
			Edges: []oam.ObservedEdge{{From: "h", To: "a", Relation: oam.BasicDNSRelation{
				Label: "dns_record", Header: oam.DNSHeader{RRType: 1, Class: 1, TTL: 300}}}},
		})
	}
	return out
}

// BenchmarkOAM01_Merge measures merge throughput (spec 25.8 b_oam_01).
func BenchmarkOAM01_Merge(b *testing.B) {
	for _, n := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("obs=%d", n), func(b *testing.B) {
			list := benchObservations(n)
			b.ReportAllocs()
			iters := 0
			for b.Loop() {
				g := oam.NewGraph(oam.Config{})
				for _, o := range list {
					if _, err := g.Apply(o); err != nil {
						b.Fatal(err)
					}
				}
				iters++
			}
			b.ReportMetric(float64(n*iters)/b.Elapsed().Seconds(), "obs/s")
		})
	}
}
