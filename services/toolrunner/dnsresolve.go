package toolrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dnsmsg"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
)

// DNSResolve is the passive dnsresolve tool (spec 6.4 "DNS resolution",
// V2_DESIGN §8, operator decision D3: DNS resolution is passive). Through
// the egress proxy's DNS forwarder it queries A, AAAA, MX, NS and TXT and
// emits IPAddress and FQDN entities with dns_record edges (BasicDNSRelation
// for A, AAAA, CNAME and NS; PrefDNSRelation for MX) and TXT data as
// DNSRecordProperty tags. Every hop of a CNAME chain is scope-checked
// (spec 4.5); a denied hop is skipped and its addresses are not used.
// Without a forwarder it falls back to the runner's resolver (A and AAAA).
type DNSResolve struct{}

// Spec implements Adapter.
func (DNSResolve) Spec() Spec {
	return Spec{Name: "dnsresolve", Version: "2.0.0", Description: "Resolve A, AAAA, CNAME, MX, NS and TXT records",
		InputType: "Fqdn", OutputType: "List<IpAddress>", Observes: []string{oam.TypeFQDN, oam.TypeIPAddress},
		Class: core.ClassPassive, RateRPS: 50, RateBurst: 100, Concurrency: 16, Timeout: 30 * time.Second, Idempotent: true}
}

// Run implements Adapter.
func (r DNSResolve) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	name, err := decodeHost(input)
	if err != nil {
		return nil, err
	}
	t, err := scope.Normalize(name, scope.TargetHost)
	if err != nil || !t.IsName() {
		return nil, Errorf("invalid_input", false, "input is not a DNS name")
	}
	name = t.Name()
	d, ok, err := tc.Allowed(ctx, out, name, scope.TargetHost, core.ClassPassive)
	if !ok {
		if err != nil {
			return nil, err
		}
		return json.RawMessage("[]"), nil
	}
	if nw := tc.Network(); nw != nil {
		res, err := r.viaForwarder(ctx, tc, nw, d, name, out)
		if !errors.Is(err, ErrNoDNS) {
			return res, err
		}
	}
	return r.viaResolver(ctx, tc, name, out)
}

func (DNSResolve) viaResolver(ctx context.Context, tc *ToolCtx, name string, out Emitter) (json.RawMessage, error) {
	if tc.Resolver == nil {
		return nil, Errorf("no_resolver", false, "the runner has no resolver")
	}
	addrs, err := tc.Resolver.LookupAddrs(ctx, name)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var de *net.DNSError
		if errors.As(err, &de) && de.IsTemporary {
			return nil, Errorf("unavailable", true, "resolution failed: "+err.Error())
		}
		_ = out.Progress(1, 1, "no addresses")
		return json.RawMessage("[]"), nil
	}
	obs := oam.Observation{ObservationID: tc.ObservationID("dnsresolve", name), ObservedAt: tc.Now(), Confidence: 0.95,
		Entities: []oam.ObservedEntity{{Ref: "n", Asset: oam.FQDN{Name: name}}}}
	ips := make([]string, 0, len(addrs))
	for i, a := range addrs {
		a = a.Unmap()
		ver, rr := "IPv4", 1
		if a.Is6() {
			ver, rr = "IPv6", 28
		}
		ref := "ip" + strconv.Itoa(i)
		obs.Entities = append(obs.Entities, oam.ObservedEntity{Ref: ref, Asset: oam.IPAddress{Address: a.String(), IPVersion: ver}})
		obs.Edges = append(obs.Edges, oam.ObservedEdge{From: "n", To: ref,
			Relation: oam.BasicDNSRelation{Label: oam.LabelDNSRecord, Header: oam.DNSHeader{RRType: rr, Class: 1}}})
		ips = append(ips, a.String())
	}
	if err := out.Observation(obs); err != nil {
		return nil, err
	}
	_ = out.Progress(1, 1, fmt.Sprintf("%d addresses", len(ips)))
	return json.Marshal(ips)
}

// obsBuilder accumulates entities with stable refs.
type obsBuilder struct {
	obs  oam.Observation
	refs map[string]string
}

func (b *obsBuilder) entity(key string, a oam.Asset) string {
	if r, ok := b.refs[key]; ok {
		return r
	}
	r := "e" + strconv.Itoa(len(b.refs))
	b.refs[key] = r
	b.obs.Entities = append(b.obs.Entities, oam.ObservedEntity{Ref: r, Asset: a})
	return r
}

func (b *obsBuilder) fqdn(n string) (string, bool) {
	t, err := scope.Normalize(strings.TrimSuffix(n, "."), scope.TargetHost)
	if err != nil || !t.IsName() {
		return "", false
	}
	return b.entity("fqdn:"+t.Name(), oam.FQDN{Name: t.Name()}), true
}

func ttl(v uint32) int { return int(min(v, 1<<31-1)) }

var dnsQueryTypes = []dnsmsg.Type{dnsmsg.TypeA, dnsmsg.TypeAAAA, dnsmsg.TypeMX, dnsmsg.TypeNS, dnsmsg.TypeTXT}

func (DNSResolve) viaForwarder(ctx context.Context, tc *ToolCtx, nw Network, d scope.Decision, name string, out Emitter) (json.RawMessage, error) {
	b := &obsBuilder{refs: map[string]string{}, obs: oam.Observation{ObservationID: tc.ObservationID("dnsresolve", name),
		ObservedAt: tc.Now(), Confidence: 0.95}}
	root, _ := b.fqdn(name)
	ips := []string{}
	seenIP := map[string]bool{}
	// Every CNAME hop target is re-checked once (spec 4.5).
	hopOK := map[string]bool{name: true}
	allowedHop := func(n string) (bool, error) {
		if v, ok := hopOK[n]; ok {
			return v, nil
		}
		_, ok, err := tc.Allowed(ctx, out, n, scope.TargetHost, core.ClassPassive)
		hopOK[n] = ok
		return ok, err
	}
	answered := 0
	for i, qt := range dnsQueryTypes {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		_ = out.Progress(int64(i), int64(len(dnsQueryTypes)), "querying")
		ans, err := nw.LookupDNS(ctx, tc, d, name+".", qt)
		if err != nil {
			if errors.Is(err, ErrNoDNS) && i == 0 {
				return nil, ErrNoDNS
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var re *RefusedError
			if errors.As(err, &re) {
				_ = out.Skipped(name, "egress_refused:"+re.Reason)
				return json.Marshal(ips)
			}
			continue
		}
		answered++
		// Follow the CNAME chain from name, checking each hop.
		owner := name
		ownerRef := root
		for range 8 {
			var next *dnsmsg.RR
			for j := range ans.Answers {
				rr := ans.Answers[j]
				if rr.Type == dnsmsg.TypeCNAME && dnsmsg.EqualNames(rr.Name, owner+".") {
					next = &ans.Answers[j]
					break
				}
			}
			if next == nil {
				break
			}
			target := strings.ToLower(strings.TrimSuffix(next.Data.Target, "."))
			tref, ok := b.fqdn(target)
			if !ok {
				break
			}
			b.edge(ownerRef, tref, oam.BasicDNSRelation{Label: oam.LabelDNSRecord, Header: oam.DNSHeader{RRType: 5, Class: 1, TTL: ttl(next.TTL)}})
			ok, err := allowedHop(target)
			if err != nil {
				return nil, err
			}
			if !ok {
				owner = ""
				break
			}
			owner, ownerRef = target, tref
		}
		if owner == "" {
			continue // the chain left the scope: nothing below it is used
		}
		for _, rr := range ans.Answers {
			if !dnsmsg.EqualNames(rr.Name, owner+".") || rr.Type != qt {
				continue
			}
			switch rr.Data.Kind {
			case dnsmsg.KindA, dnsmsg.KindAAAA:
				a := rr.Data.IP.Unmap()
				ver := "IPv4"
				if a.Is6() {
					ver = "IPv6"
				}
				ref := b.entity("ip:"+a.String(), oam.IPAddress{Address: a.String(), IPVersion: ver})
				b.edge(ownerRef, ref, oam.BasicDNSRelation{Label: oam.LabelDNSRecord, Header: oam.DNSHeader{RRType: int(rr.Type), Class: 1, TTL: ttl(rr.TTL)}})
				if !seenIP[a.String()] {
					seenIP[a.String()] = true
					ips = append(ips, a.String())
				}
			case dnsmsg.KindMX:
				if ref, ok := b.fqdn(rr.Data.Target); ok {
					b.edge(ownerRef, ref, oam.PrefDNSRelation{Label: oam.LabelDNSRecord, Header: oam.DNSHeader{RRType: 15, Class: 1, TTL: ttl(rr.TTL)},
						Preference: int(rr.Data.Preference)})
				}
			case dnsmsg.KindName:
				if ref, ok := b.fqdn(rr.Data.Target); ok {
					b.edge(ownerRef, ref, oam.BasicDNSRelation{Label: oam.LabelDNSRecord, Header: oam.DNSHeader{RRType: int(rr.Type), Class: 1, TTL: ttl(rr.TTL)}})
				}
			case dnsmsg.KindTXT:
				b.obs.Tags = append(b.obs.Tags, oam.ObservedTag{Target: ownerRef, Property: oam.DNSRecordProperty{Name: "dns_record",
					Header: oam.DNSHeader{RRType: 16, Class: 1, TTL: ttl(rr.TTL)}, Data: strings.Join(rr.Data.Strings, "")}})
			}
		}
	}
	if answered == 0 {
		return nil, Errorf("unavailable", true, "the DNS forwarder did not answer")
	}
	if err := out.Observation(b.obs); err != nil {
		return nil, err
	}
	_ = out.Progress(int64(len(dnsQueryTypes)), int64(len(dnsQueryTypes)), fmt.Sprintf("%d addresses", len(ips)))
	return json.Marshal(ips)
}

func (b *obsBuilder) edge(from, to string, r oam.Relation) {
	for _, e := range b.obs.Edges {
		if e.From == from && e.To == to && e.Relation == r {
			return
		}
	}
	b.obs.Edges = append(b.obs.Edges, oam.ObservedEdge{From: from, To: to, Relation: r})
}
