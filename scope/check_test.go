package scope

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

func TestDecisionJSONShape(t *testing.T) {
	// The scope:check response of spec 12.5, plus decided_at.
	d := Decision{ID: "dec_01JD8K4P", Result: ResultDeny, Reason: ReasonExplicitDeny, RuleID: "r5", ScopeVersion: 2,
		Target: "https://admin.corp.acme.com/login", DecidedAt: time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)}
	got, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	want := `{ "result": "deny", "reason": "explicit_deny", "rule_id": "r5", "scope_version": 2,
	  "normalized_target": "https://admin.corp.acme.com/login", "pinned_ips": [], "decision_id": "dec_01JD8K4P",
	  "decided_at": "2026-11-12T14:00:00.000Z" }`
	if canon(t, got) != canon(t, []byte(want)) {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	allow := Decision{Result: ResultAllow, Reason: ReasonAllow, RuleID: "a", PinnedIPs: []netip.Addr{netip.MustParseAddr("2001:db8::1")}}
	got, _ = json.Marshal(allow)
	if want := `{"normalized_target":"","pinned_ips":["2001:db8::1"],"reason":"allow","result":"allow","rule_id":"a","scope_version":0}`; canon(t, got) != want {
		t.Fatalf("got %s", got)
	}
	var back Decision
	if err := json.Unmarshal([]byte(`{"result":"deny","reason":"x","decided_at":"yesterday"}`), &back); err == nil {
		t.Fatal("bad decided_at accepted")
	}
	if (Decision{}).Allowed() {
		t.Fatal("zero decision allows")
	}
}

func canon(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	c, err := core.CanonicalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(c)
}

func resolverTestPolicy(t *testing.T, r Resolver) *Policy {
	t.Helper()
	p, err := Compile(Document{Rules: []Rule{{ID: "a", Effect: EffectAllow, Kind: RuleWildcardDomain, Value: "*.acme.com"}},
		Activity: Activity{MaxClass: core.ClassActive}}, WithResolver(r))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type funcResolver func(ctx context.Context, host string) ([]netip.Addr, error)

func (f funcResolver) LookupAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	return f(ctx, host)
}

func TestResolutionFailsClosed(t *testing.T) {
	answers := StaticResolver{Answers: map[string][]netip.Addr{"www.acme.com": {netip.MustParseAddr("203.0.113.1")}}}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		r    Resolver
		ctx  context.Context
	}{
		{"nil resolver", nil, context.Background()},
		{"cancelled context", answers, cancelled},
		{"zoned answer", funcResolver(func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("fe80::1%eth0")}, nil
		}), context.Background()},
		{"invalid answer", funcResolver(func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{{}}, nil
		}), context.Background()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var opts []Option
			if c.r != nil {
				opts = append(opts, WithResolver(c.r))
			}
			p, err := Compile(Document{Rules: []Rule{{ID: "a", Effect: EffectAllow, Kind: RuleWildcardDomain, Value: "*.acme.com"}},
				Activity: Activity{MaxClass: core.ClassActive}}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			d := p.Check(c.ctx, Request{Target: "www.acme.com", Class: core.ClassActive})
			if d.Allowed() || d.Reason != ReasonResolutionFailed {
				t.Fatalf("got %+v, want deny(resolution_failed)", d)
			}
			// Passive checks never resolve, so they are unaffected.
			if d := p.Check(c.ctx, Request{Target: "www.acme.com", Class: core.ClassPassive}); !d.Allowed() {
				t.Fatalf("passive: %+v", d)
			}
		})
	}
}

func TestStaticResolver(t *testing.T) {
	boom := errors.New("boom")
	r := StaticResolver{Answers: map[string][]netip.Addr{"a.example": {netip.MustParseAddr("192.0.2.1")}},
		Errors: map[string]error{"b.example": boom}}
	ctx := context.Background()
	got, err := r.LookupAddrs(ctx, "a.example")
	if err != nil || len(got) != 1 {
		t.Fatalf("a.example: %v %v", got, err)
	}
	got[0] = netip.Addr{}
	if again, _ := r.LookupAddrs(ctx, "a.example"); !again[0].IsValid() {
		t.Fatal("answers are shared with the caller")
	}
	if _, err := r.LookupAddrs(ctx, "b.example"); !errors.Is(err, boom) {
		t.Fatalf("b.example: %v", err)
	}
	if _, err := r.LookupAddrs(ctx, "c.example"); !errors.Is(err, ErrNoSuchHost) {
		t.Fatalf("c.example: %v", err)
	}
	if _, err := resolveOnce(ctx, r, "c.example"); !errors.Is(err, ErrResolution) || !errors.Is(err, ErrNoSuchHost) {
		t.Fatalf("resolveOnce: %v", err)
	}
}

func TestCheckResolvesOnce(t *testing.T) {
	res := &countingResolver{Resolver: StaticResolver{Answers: map[string][]netip.Addr{
		"www.acme.com": {netip.MustParseAddr("203.0.113.1")}}}}
	p := resolverTestPolicy(t, res)
	ctx := context.Background()
	p.Check(ctx, Request{Target: "www.acme.com", Class: core.ClassActive})
	p.Check(ctx, Request{Target: "https://www.acme.com/x", Kind: TargetURL, Class: core.ClassActive})
	p.Check(ctx, Request{Target: "www.acme.com", Class: core.ClassPassive})
	p.Check(ctx, Request{Target: "www.acme.com", Class: core.ClassActive, SkipResolution: true})
	if got := res.calls["www.acme.com"]; got != 2 {
		t.Fatalf("lookups = %d, want 2 (one per active, resolving check)", got)
	}
}

func TestStatus(t *testing.T) {
	p, err := Compile(Document{Rules: []Rule{
		{ID: "a", Effect: EffectAllow, Kind: RuleWildcardDomain, Value: "*.acme.com"},
		{ID: "d", Effect: EffectDeny, Kind: RuleDomain, Value: "pay.acme.com"},
		{ID: "n", Effect: EffectAllow, Kind: RuleASN, Value: "AS64500"},
	}, Activity: Activity{MaxClass: core.ClassNone}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		target string
		kind   TargetKind
		want   Status
	}{
		{"www.acme.com", TargetHost, StatusInScope}, // max_class does not affect status
		{"https://WWW.acme.com/x", TargetURL, StatusInScope},
		{"pay.acme.com", TargetHost, StatusOutOfScope},
		{"acme.com", TargetHost, StatusOutOfScope},
		{"203.0.113.1", TargetIP, StatusOutOfScope}, // asn rules never match
		{"www..acme.com", TargetHost, StatusUnknown},
		{"not a url", TargetURL, StatusUnknown},
		{"www.acme.com", TargetKind(99), StatusUnknown},
	}
	for _, c := range cases {
		if got := p.Status(c.target, c.kind); got != c.want {
			t.Errorf("Status(%q, %s) = %s, want %s", c.target, c.kind, got, c.want)
		}
		if got := p.Allows(c.target, c.kind); got != (c.want == StatusInScope) {
			t.Errorf("Allows(%q) = %v", c.target, got)
		}
	}
}

func TestChainEmptyHops(t *testing.T) {
	p := resolverTestPolicy(t, nil)
	res := p.CheckChain(context.Background(), Request{Target: "www.acme.com", Class: core.ClassPassive}, nil)
	if !res.Allowed || len(res.Decisions) != 1 {
		t.Fatalf("%+v", res)
	}
	if (ChainResult{}).Final().Allowed() {
		t.Fatal("empty chain result allows")
	}
}
