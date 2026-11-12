package scope

import (
	"context"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// fuzzDocument covers every rule kind and effect, with overlapping names,
// prefixes and URL paths.
func fuzzDocument() Document {
	rule := func(id string, e RuleEffect, k RuleKind, v string) Rule {
		return Rule{ID: id, Effect: e, Kind: k, Value: v}
	}
	return Document{Version: 7, Rules: []Rule{
		rule("apex", EffectAllow, RuleDomain, "acme.com"),
		rule("sub", EffectAllow, RuleWildcardDomain, "*.acme.com"),
		rule("pay", EffectDeny, RuleDomain, "pay.acme.com"),
		rule("corp", EffectDeny, RuleWildcardDomain, "*.corp.acme.com"),
		rule("idn", EffectAllow, RuleDomain, "xn--bcher-kva.example"),
		rule("net", EffectAllow, RuleCIDR, "10.0.0.0/8"),
		rule("lab", EffectDeny, RuleCIDR, "10.9.0.0/16"),
		rule("v6", EffectAllow, RuleCIDR, "2001:db8::/32"),
		rule("v6deny", EffectDeny, RuleCIDR, "2001:db8:1::/48"),
		rule("api", EffectAllow, RuleURLPrefix, "https://api.example.org/v2/"),
		rule("admin", EffectDeny, RuleURLPrefix, "https://api.example.org/v2/admin"),
		rule("ipurl", EffectDeny, RuleURLPrefix, "http://10.1.2.3:8080/x"),
		rule("asn", EffectAllow, RuleASN, "AS64500"),
	}, Activity: Activity{MaxClass: core.ClassIntrusive}}
}

// FuzzScopeNormalize is fuzz_scope_normalize (spec 25.5): for arbitrary
// target strings, normalization is idempotent and the indexed decision
// equals the slow reference matcher's.
func FuzzScopeNormalize(f *testing.F) {
	seeds := []string{
		" WWW.Acme.COM. ", "a..b.com", "https://acme.com@evil.com/", "https://acme.com.evil.com/",
		"https://Acme.COM:443/a/b/../c#frag", "2130706433", "0177.0.0.1", "0x7f.1", "::ffff:10.0.0.1",
		"10.9.0.0/15", "2001:db8::/31", "https://api.example.org/v2/admin/../x", "http://10.1.2.3:8080/x/y",
		"аpple.com", "bücher.example", "fe80::1%eth0", "[::1]", "http://[::ffff:10.0.0.1]/",
	}
	for i, s := range seeds {
		f.Add(s, uint8(i%5), uint8(i%4))
	}
	ctx := context.Background()
	doc := fuzzDocument()
	indexed, err := Compile(doc, WithResolver(hashResolver{}))
	if err != nil {
		f.Fatal(err)
	}
	linear, err := Compile(doc, WithResolver(hashResolver{}), withLinearMatcher())
	if err != nil {
		f.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, target string, kind, class uint8) {
		k := TargetKind(kind % 6)
		if first, err := Normalize(target, k); err == nil {
			second, err := Normalize(first.String(), k)
			if err != nil || second.String() != first.String() {
				t.Fatalf("not idempotent: %q -> %q -> %q (%v)", target, first, second, err)
			}
		}
		req := Request{Target: target, Kind: k, Class: core.ActivityClass(class % 5), Now: now}
		got, want := indexed.Check(ctx, req), linear.Check(ctx, req)
		if !got.Equal(want) {
			t.Fatalf("indexed %+v, reference %+v for %+v", got, want, req)
		}
		checkDecisionInvariants(t, got)
		if gs, ws := indexed.Status(target, k), linear.Status(target, k); gs != ws {
			t.Fatalf("status: indexed %s, reference %s for %q", gs, ws, target)
		}
	})
}
