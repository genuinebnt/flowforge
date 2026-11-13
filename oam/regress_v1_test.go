package oam_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/oam"
)

// TestRegress_G_CORE_03_PendingReferences pins the buffer of observations
// that name entity ids not ingested yet: they wait instead of failing,
// cascade (an observation released by one entity may create the entity
// another waits for), are deduplicated while waiting, are dropped if
// invalid once their references exist, are bounded per tenant and expire
// by observed_at; MaxPending < 0 restores the old rejection.
func TestRegress_G_CORE_03_PendingReferences(t *testing.T) {
	fqdn, ip := oam.FQDN{Name: "a.acme.com"}, oam.IPAddress{Address: "203.0.113.7"}
	fqdnID, ipID := entityIDOf("ten", "prg", fqdn), entityIDOf("ten", "prg", ip)
	refTo := func(obsID string, a oam.Asset, to string) oam.Observation {
		o := obs(obsID, "x", t0, 0.5, ent("e", a))
		o.Edges = []oam.ObservedEdge{{From: "e", To: to, Relation: oam.SimpleRelation{Label: "related_to"}}}
		return o
	}

	t.Run("cascade", func(t *testing.T) {
		g := oam.NewGraph(oam.Config{})
		// A (creates a URL) waits for the FQDN, which B creates; B waits
		// for the IP, which C creates.
		a := refTo("A", oam.URL{URL: "https://a.acme.com/"}, fqdnID)
		b := refTo("B", fqdn, ipID)
		c := obs("C", "x", t0, 0.5, ent("i", ip))
		for _, o := range []oam.Observation{a, b} {
			if r := mustApply(t, g, o); !r.Pending {
				t.Fatalf("%s: %+v, want pending", o.ObservationID, r)
			}
		}
		if r := mustApply(t, g, a); !r.Pending || r.Duplicate {
			t.Fatalf("re-sent pending observation: %+v", r)
		}
		if n := g.PendingCount("ten"); n != 2 {
			t.Fatalf("pending %d, want 2", n)
		}
		r := mustApply(t, g, c)
		if !slices.Equal(r.Released, []string{"B", "A"}) {
			t.Fatalf("released %v, want [B A]", r.Released)
		}
		if len(g.Entities()) != 3 || len(g.Edges()) != 2 || g.PendingCount("ten") != 0 {
			t.Fatalf("after release: %d entities, %d edges, %d pending", len(g.Entities()), len(g.Edges()), g.PendingCount("ten"))
		}
		if r := mustApply(t, g, a); !r.Duplicate {
			t.Fatalf("released observation re-sent: %+v, want duplicate", r)
		}
	})

	t.Run("invalid once resolved is dropped", func(t *testing.T) {
		g := oam.NewGraph(oam.Config{})
		bad := obs("bad", "x", t0, 0.5, ent("s", oam.Service{UniqueID: "s1"}))
		bad.Edges = []oam.ObservedEdge{{From: "s", To: fqdnID, Relation: oam.SimpleRelation{Label: "certificate"}}}
		mustApply(t, g, bad)
		r := mustApply(t, g, obs("f", "x", t0, 0.5, ent("f", fqdn)))
		if len(r.Released) != 0 || len(g.Entities()) != 1 || g.PendingCount("ten") != 0 {
			t.Fatalf("invalid buffered observation: released %v, %d entities", r.Released, len(g.Entities()))
		}
	})

	t.Run("bound and expiry", func(t *testing.T) {
		g := oam.NewGraph(oam.Config{MaxPending: 1})
		mustApply(t, g, refTo("p1", oam.URL{URL: "https://x.acme.com/"}, fqdnID))
		if _, err := g.Apply(refTo("p2", oam.URL{URL: "https://y.acme.com/"}, fqdnID)); !errors.Is(err, oam.ErrPendingLimit) {
			t.Fatalf("over the bound: %v, want ErrPendingLimit", err)
		}
		if got := g.ExpirePending("ten", t0); len(got) != 0 {
			t.Fatalf("expired %v before its time", got)
		}
		if got := g.ExpirePending("ten", t0.Add(time.Second)); !slices.Equal(got, []string{"p1"}) {
			t.Fatalf("expired %v, want [p1]", got)
		}
		mustApply(t, g, refTo("p2", oam.URL{URL: "https://y.acme.com/"}, fqdnID))
	})

	t.Run("buffering disabled", func(t *testing.T) {
		g := oam.NewGraph(oam.Config{MaxPending: -1})
		if _, err := g.Apply(refTo("p", oam.URL{URL: "https://x.acme.com/"}, fqdnID)); !errors.Is(err, oam.ErrUnknownEntity) {
			t.Fatalf("MaxPending -1: %v, want ErrUnknownEntity", err)
		}
	})
}
