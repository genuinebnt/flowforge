package limiter

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestLim008_HierarchyNoPartialCharge: a tool call acquires its tenant and
// target keys (among others) atomically. It is denied if either denies,
// and then neither is charged.
func TestLim008_HierarchyNoPartialCharge(t *testing.T) {
	rules := []Rule{
		all(Params{Rate: 1000, Burst: 100}),
		{Pattern: "rl:t:*", Params: Params{Rate: 1, Burst: 2}}, // tenant: 2 per second
		{Pattern: "rl:h:*", Params: Params{Rate: 1, Burst: 3}}, // target: 3 per second
	}
	tenantA := ToolCall{Class: "active", TenantID: "ten_a", ProgramID: "prg_1", Target: "acme.com", Tool: "httpx"}
	tenantB := ToolCall{Class: "active", TenantID: "ten_b", ProgramID: "prg_1", Target: "acme.com", Tool: "httpx"}
	for _, v := range allVariants() {
		t.Run(v.name, func(t *testing.T) {
			l := mustNew(t, v, rules...)
			call := func(tc ToolCall) Result {
				t.Helper()
				reqs, err := tc.Requests(1)
				if err != nil {
					t.Fatal(err)
				}
				res, err := l.AcquireAll(reqs, 0)
				if err != nil {
					t.Fatal(err)
				}
				return res
			}
			// Tenant A spends its tenant budget (2); the shared target has 1 left.
			for i := range 2 {
				if res := call(tenantA); !res.Allowed {
					t.Fatalf("call %d denied: %+v", i, res)
				}
			}
			// Tenant denies: nothing is charged, so the target's last token
			// is still there for tenant B.
			if res := call(tenantA); res.Allowed || res.Limit != 2 || res.RetryAfter <= 0 {
				t.Fatalf("tenant limit did not deny: %+v", res)
			}
			if res := call(tenantB); !res.Allowed || res.Remaining != 0 {
				t.Fatalf("target token was charged by the denied call: %+v", res)
			}
			// Target denies now: tenant B's own budget stays untouched.
			if res := call(tenantB); res.Allowed || res.Limit != 3 {
				t.Fatalf("target limit did not deny: %+v", res)
			}
			solo, err := l.Acquire(TenantKey("ten_b", "active"), 1, 0)
			if err != nil || !solo.Allowed || solo.Remaining != 0 {
				t.Fatalf("tenant b was charged by the denied call: %+v %v", solo, err)
			}
		})
	}
}

// TestLimAcquireAll_RetryIsMaxOfDenials checks the 14.1 rule directly.
func TestLimAcquireAll_RetryIsMaxOfDenials(t *testing.T) {
	rules := []Rule{
		{Pattern: "fast", Params: Params{Rate: 10, Burst: 1}}, // retry 100 ms
		{Pattern: "slow", Params: Params{Rate: 1, Burst: 1}},  // retry 1 s
		{Pattern: "free", Params: Params{Rate: 1, Burst: 5}},
	}
	for _, v := range gcraVariants() {
		l := mustNew(t, v, rules...)
		l.Acquire("fast", 1, 0)
		l.Acquire("slow", 1, 0)
		res, err := l.AcquireAll([]Request{{"fast", 1}, {"free", 1}, {"slow", 1}}, 0)
		if err != nil || res.Allowed || res.RetryAfter != time.Second {
			t.Fatalf("%s: %+v %v, want denial with retry 1s", v.name, res, err)
		}
		if res, _ := l.Acquire("free", 5, 0); !res.Allowed {
			t.Fatalf("%s: free key was charged", v.name)
		}
	}
}

func TestLimKeys(t *testing.T) {
	cases := []struct{ got, want string }{
		{GlobalKey("active"), "rl:g:active"},
		{TenantKey("ten_1", "active"), "rl:t:ten_1:active"},
		{ProgramKey("prg_1"), "rl:p:prg_1"},
		{HostKey("prg_1", "Acme.COM"), "rl:h:prg_1:acme.com"},
		{HostKey("prg_1", "2001:db8::1"), "rl:h:prg_1:2001%3Adb8%3A%3A1"},
		{ToolKey("ten_1", "nuclei"), "rl:tool:ten_1:nuclei"},
		{AuthIPKey("203.0.113.7", "login"), "rl:auth:ip:203.0.113.7:login"},
		{APIKey("ten_1", "write"), "rl:api:ten_1:write"},
		{TenantKey("a:b", "c"), "rl:t:a%3Ab:c"},
		{TenantKey("a", "b:c"), "rl:t:a:b%3Ac"},
		{TenantKey("100%", "x"), "rl:t:100%25:x"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	// sha256("user@example.com")
	const want = "rl:auth:acct:b4c9a289323b21a01c3e940f150eb9b8c542587f1abfd8f0e1cc1ffc5e475514"
	for _, e := range []string{"user@example.com", "  User@Example.COM "} {
		if got := AuthAccountKey(e); got != want {
			t.Errorf("AuthAccountKey(%q) = %q", e, got)
		}
	}
	reqs, err := ToolCall{Class: "active", TenantID: "t", ProgramID: "p", Target: "h.com", Tool: "x"}.Requests(2)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range reqs {
		if r.Cost != 2 {
			t.Fatalf("cost %d", r.Cost)
		}
		keys = append(keys, r.Key)
	}
	if got := strings.Join(keys, " "); got != "rl:g:active rl:t:t:active rl:p:p rl:h:p:h.com rl:tool:t:x" {
		t.Fatalf("keys %q", got)
	}
	reqs, _ = ToolCall{Class: "passive", TenantID: "t", ProgramID: "p", Tool: "x"}.Requests(1)
	if len(reqs) != 4 {
		t.Fatalf("no-target tool call: %v", reqs)
	}
	if _, err := (ToolCall{Class: "active", TenantID: "t", Tool: "x"}).Requests(1); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("missing program: %v", err)
	}
}

func TestLimRules(t *testing.T) {
	r, err := NewRules(
		Rule{"*", Params{Rate: 1, Burst: 1}},
		Rule{"rl:t:*", Params{Rate: 1, Burst: 2}},
		Rule{"rl:t:big:*", Params{Rate: 1, Burst: 3}},
		Rule{"rl:t:big:exact", Params{Rate: 1, Burst: 4}},
	)
	if err != nil {
		t.Fatal(err)
	}
	for key, burst := range map[string]int{"other": 1, "rl:t:x": 2, "rl:t:big:y": 3, "rl:t:big:exact": 4, "rl:t:big:exactly": 3} {
		p, err := r.Lookup(key)
		if err != nil || p.Burst != burst {
			t.Errorf("Lookup(%q) = %+v %v, want burst %d", key, p, err, burst)
		}
	}
	// Reconfiguring a pattern replaces it.
	if err := r.Configure("rl:t:*", Params{Rate: 1, Burst: 9}); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.Lookup("rl:t:x"); p.Burst != 9 {
		t.Fatalf("reconfigured burst %d", p.Burst)
	}
	for _, bad := range []string{"", "a*b", "**", "*x"} {
		if err := r.Configure(bad, Params{Rate: 1, Burst: 1}); !errors.Is(err, ErrInvalidPattern) {
			t.Errorf("pattern %q: %v", bad, err)
		}
	}
	if err := r.Configure("x", Params{}); !errors.Is(err, ErrInvalidParams) {
		t.Errorf("bad params: %v", err)
	}
	var zero Rules
	if _, err := zero.Lookup("x"); !errors.Is(err, ErrNoRule) {
		t.Errorf("zero Rules: %v", err)
	}
	if err := zero.Configure("x", Params{Rate: 1, Burst: 1}); err != nil {
		t.Errorf("zero Rules Configure: %v", err)
	}
	if p, err := zero.Lookup("x"); err != nil || p.Burst != 1 {
		t.Errorf("zero Rules after Configure: %+v %v", p, err)
	}
	// Configure on a live limiter takes effect for existing keys.
	g, _ := NewGCRA(all(Params{Rate: 1, Burst: 1}))
	g.Acquire("k", 1, 0)
	if res, _ := g.Acquire("k", 1, 0); res.Allowed {
		t.Fatal("burst 1 allowed twice")
	}
	g.Configure("k", Params{Rate: 1, Burst: 3})
	if res, _ := g.Acquire("k", 2, 0); !res.Allowed {
		t.Fatalf("raised burst not applied: %+v", res)
	}
}
