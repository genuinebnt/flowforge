package oam_test

import (
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/oam"
)

var t0 = time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)

// obs builds a one-observation envelope for tests.
func obs(id, tool string, at time.Time, conf float64, entities ...oam.ObservedEntity) oam.Observation {
	return oam.Observation{
		ObservationID: id, TenantID: "ten", ProgramID: "prg", RunID: "run-" + id,
		Tool: oam.Tool{Name: tool}, ObservedAt: at, Confidence: conf, Entities: entities,
	}
}

func ent(ref string, a oam.Asset) oam.ObservedEntity { return oam.ObservedEntity{Ref: ref, Asset: a} }

func mustApply(t testing.TB, g *oam.Graph, o oam.Observation) oam.Result {
	t.Helper()
	r, err := g.Apply(o)
	if err != nil {
		t.Fatalf("apply %s: %v", o.ObservationID, err)
	}
	return r
}

func mustLookup(t testing.TB, g *oam.Graph, a oam.Asset) oam.Entity {
	t.Helper()
	e, ok, err := g.Lookup("ten", "prg", a)
	if err != nil || !ok {
		t.Fatalf("lookup %v: ok=%v err=%v", a, ok, err)
	}
	return e
}
