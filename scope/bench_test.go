package scope

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// benchDocument builds n distinct rules over a wide value space: mixed
// domains, wildcards, IPv4 and IPv6 prefixes and URL prefixes, about one
// in five denies (spec 25.8, b_scope_01).
func benchDocument(r core.Rng, n int) Document {
	d := Document{Version: 1, Activity: Activity{MaxClass: core.ClassIntrusive}}
	for i := range n {
		rule := Rule{ID: fmt.Sprintf("r%d", i), Effect: EffectAllow}
		if core.IntN(r, 5) == 0 {
			rule.Effect = EffectDeny
		}
		zone := fmt.Sprintf("d%d.example", core.IntN(r, n+1))
		switch core.IntN(r, 10) {
		case 0, 1:
			rule.Kind, rule.Value = RuleDomain, fmt.Sprintf("h%d.%s", core.IntN(r, 50), zone)
		case 2, 3, 4:
			rule.Kind, rule.Value = RuleWildcardDomain, "*."+zone
		case 5, 6:
			a := netip.AddrFrom4([4]byte{byte(r.Uint64()), byte(r.Uint64()), byte(r.Uint64()), 0})
			rule.Kind, rule.Value = RuleCIDR, fmt.Sprintf("%s/%d", a, 12+core.IntN(r, 21))
		case 7:
			var b [16]byte
			b[0], b[1], b[2], b[3], b[4], b[5] = 0x20, 0x01, 0x0d, 0xb8, byte(r.Uint64()), byte(r.Uint64())
			rule.Kind, rule.Value = RuleCIDR, fmt.Sprintf("%s/%d", netip.AddrFrom16(b), 32+core.IntN(r, 33))
		default:
			rule.Kind, rule.Value = RuleURLPrefix, fmt.Sprintf("https://api.%s/v%d/", zone, core.IntN(r, 3))
		}
		d.Rules = append(d.Rules, rule)
	}
	return d
}

// benchTargets returns targets that hit rules about half of the time.
func benchTargets(r core.Rng, doc Document, n int) []Request {
	out := make([]Request, n)
	for i := range out {
		req := Request{Class: core.ClassPassive, Now: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
		rule := doc.Rules[core.IntN(r, len(doc.Rules))]
		hit := core.IntN(r, 2) == 0
		switch rule.Kind {
		case RuleDomain:
			req.Target, req.Kind = rule.Value, TargetHost
		case RuleWildcardDomain:
			req.Target, req.Kind = fmt.Sprintf("x%d.%s", i, rule.Value[2:]), TargetHost
		case RuleCIDR:
			p := netip.MustParsePrefix(rule.Value)
			req.Target, req.Kind = p.Addr().Next().String(), TargetIP
		case RuleURLPrefix:
			req.Target, req.Kind = rule.Value+"items/1?x=2", TargetURL
		}
		if !hit {
			switch core.IntN(r, 3) {
			case 0:
				req.Target, req.Kind = fmt.Sprintf("www.miss%d.example", i), TargetHost
			case 1:
				req.Target, req.Kind = netip.AddrFrom4([4]byte{byte(r.Uint64()), byte(r.Uint64()), byte(r.Uint64()), byte(r.Uint64())}).String(), TargetIP
			default:
				req.Target, req.Kind = fmt.Sprintf("https://miss%d.example/v1/a", i), TargetURL
			}
		}
		out[i] = req
	}
	return out
}

// BenchmarkScope01_Decide is b_scope_01: normalize and decide, indexed
// matcher against the naive linear scan, at 10, 1,000 and 10,000 rules.
func BenchmarkScope01_Decide(b *testing.B) {
	ctx := context.Background()
	for _, n := range []int{10, 1000, 10000} {
		r := core.NewSplitMix64(uint64(n))
		doc := benchDocument(r, n)
		reqs := benchTargets(r, doc, 4096)
		for _, m := range []struct {
			name string
			opts []Option
		}{{"indexed", nil}, {"naive", []Option{WithMatcher(MatcherLinear)}}} {
			p, err := Compile(doc, m.opts...)
			if err != nil {
				b.Fatal(err)
			}
			b.Run(fmt.Sprintf("%s/rules=%d", m.name, n), func(b *testing.B) {
				b.ReportAllocs()
				allowed, i := 0, 0
				for b.Loop() {
					if p.Check(ctx, reqs[i%len(reqs)]).Allowed() {
						allowed++
					}
					i++
				}
				b.ReportMetric(float64(allowed)/float64(max(i, 1)), "allow-ratio")
			})
		}
	}
}

// BenchmarkScopeAlgo compares the selectable Matchers (one decision point:
// first-matching-rule lookup, spec 4.3) on normalize-and-decide at 10,
// 1,000 and 10,000 rules (b_scope_01's workload):
//
//	go test -run '^$' -bench ScopeAlgo ./scope
func BenchmarkScopeAlgo(b *testing.B) {
	ctx := context.Background()
	for _, m := range Matchers() {
		for _, n := range []int{10, 1000, 10000} {
			r := core.NewSplitMix64(uint64(n))
			doc := benchDocument(r, n)
			reqs := benchTargets(r, doc, 4096)
			p, err := Compile(doc, WithMatcher(m))
			if err != nil {
				b.Fatal(err)
			}
			b.Run(fmt.Sprintf("%s/rules=%d", m, n), func(b *testing.B) {
				b.ReportAllocs()
				i := 0
				for b.Loop() {
					p.Check(ctx, reqs[i%len(reqs)])
					i++
				}
			})
		}
	}
}
