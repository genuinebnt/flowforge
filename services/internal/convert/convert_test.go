package convert_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/convert"
)

func TestObservationRoundTrip(t *testing.T) {
	o := oam.Observation{ObservationID: "obs_1", ObservedAt: time.Date(2026, 11, 12, 14, 2, 12, 311e6, time.UTC), Confidence: 0.9,
		Tool:     oam.Tool{Name: "httpprobe"},
		Entities: []oam.ObservedEntity{{Ref: "a", Asset: oam.FQDN{Name: "api.acme.com"}}, {Ref: "b", Asset: oam.IPAddress{Address: "203.0.113.10", IPVersion: "IPv4"}}},
		Edges:    []oam.ObservedEdge{{From: "a", To: "b", Relation: oam.BasicDNSRelation{Label: "dns_record", Header: oam.DNSHeader{RRType: 1, Class: 1, TTL: 300}}}},
		Tags:     []oam.ObservedTag{{Target: "a", Property: oam.SimpleProperty{Name: "title", Value: "x"}}},
	}
	p, err := convert.ObservationProto(o)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := convert.ObservationToJSON(p)
	if err != nil {
		t.Fatal(err)
	}
	// The engine adds envelope fields; the parser ignores them.
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["tenant_id"], m["effect_path"] = "t", "0/1"
	raw, _ = json.Marshal(m)
	back, err := convert.ObservationFromJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	d, err := convert.ObservationDomain(back, "prg_x", "", "httpprobe", "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Entities) != 2 || len(d.Edges) != 1 || len(d.Tags) != 1 || !d.ObservedAt.Equal(o.ObservedAt) || d.ProgramID != "prg_x" {
		t.Fatalf("round trip %+v", d)
	}
}

func TestScopeDocAndDecision(t *testing.T) {
	doc := scope.Document{Rules: []scope.Rule{{ID: "a", Effect: scope.EffectAllow, Kind: scope.RuleWildcardDomain, Value: "*.acme.com"}},
		Activity: scope.Activity{MaxClass: core.ClassActive, Windows: []scope.Window{}}}
	back, err := convert.ScopeDoc(convert.ScopeDocProto(doc))
	if err != nil || back.Rules[0] != doc.Rules[0] || back.Activity.MaxClass != core.ClassActive {
		t.Fatalf("%+v %v", back, err)
	}
	p := convert.DecisionProto(scope.Decision{ID: "dec_1", Result: scope.ResultAllow, Reason: "allow"})
	p.PinnedIps = []string{"not-an-ip"}
	if convert.Decision(p).Allowed() {
		t.Fatal("an unparsable pinned address must turn the allow into a deny")
	}
}

func TestToolSpecHashAgrees(t *testing.T) {
	s := engine.ToolSpec{Name: "x", Version: "1", InputType: "Fqdn", OutputType: "List<IpAddress>", EffectClass: "passive", Retry: convert.DefaultToolRetry, TimeoutMs: 30000}
	p := convert.ToolSpecProto(s)
	back := convert.ToolSpec(p)
	if back.SpecSHA256 != s.ComputeSHA256() {
		t.Fatal("the engine and the runner compute different spec hashes")
	}
}
