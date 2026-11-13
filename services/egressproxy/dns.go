package egressproxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dnsmsg"
	"github.com/genuinebnt/flowforge/scope"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
)

// DNSAnswer is an upstream resolution: the response code and the answer
// records (a CNAME chain first, as a recursive resolver returns it).
type DNSAnswer struct {
	RCode   dnsmsg.RCode
	Answers []dnsmsg.RR
}

// Upstream resolves forwarded queries. It is the forwarder's only route to
// the DNS; tests inject RecordsUpstream.
type Upstream interface {
	// Lookup resolves (name, t) recursively; an error means no answer
	// (the forwarder replies SERVFAIL).
	Lookup(ctx context.Context, name string, t dnsmsg.Type) (DNSAnswer, error)
}

// maxCNAMEHops bounds the chains the forwarder follows and filters.
const maxCNAMEHops = 8

// maxAnswerRecords caps the records an upstream answer may contribute.
const maxAnswerRecords = 64

// ServeDNSUDP answers DNS over UDP on pc until ctx ends.
func (p *Proxy) ServeDNSUDP(ctx context.Context, pc net.PacketConn) error {
	p.init()
	go func() {
		<-ctx.Done()
		pc.Close()
	}()
	buf := make([]byte, dnsmsg.MaxMessageLen)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if resp := p.answerDNS(qctx, q, true); resp != nil {
				_, _ = pc.WriteTo(resp, from)
			}
		}()
	}
}

// ServeDNSTCP answers DNS over TCP (RFC 1035 §4.2.2 framing, RFC 7766) on l
// until ctx ends.
func (p *Proxy) ServeDNSTCP(ctx context.Context, l net.Listener) error {
	p.init()
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			defer c.Close()
			for {
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				q, err := dnsmsg.ReadTCP(c, dnsmsg.MaxMessageLen)
				if err != nil {
					return
				}
				qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				resp := p.answerDNS(qctx, q, false)
				cancel()
				if resp == nil || dnsmsg.WriteTCP(c, resp) != nil {
					return
				}
			}
		}()
	}
}

// answerDNS is the forwarder policy. A query is answered only when it
// carries a valid ticket (EDNS option EDNSTicketOption) whose decision is
// still an allow, no kill switch is engaged, and the name passes a passive
// scope check (recorded by the scope service) or is an allowlisted
// intelligence source for a passive decision (recorded as intel_source).
// Everything else is REFUSED. Records of a CNAME chain are kept only up to
// the first hop that fails the same check. The answer is capped at the UDP
// payload size (TC set when cut) or MaxTCPResponse.
func (p *Proxy) answerDNS(ctx context.Context, q []byte, udp bool) []byte {
	m, _, err := dnsmsg.Decode(q, dnsmsg.Strict)
	if err != nil {
		h, ok := dnsmsg.DecodeHeader(q)
		if !ok || h.Response {
			return nil
		}
		return p.reply(dnsmsg.Message{Header: h}, dnsmsg.RCodeFormErr, nil, udp, 512)
	}
	if m.Header.Response {
		return nil
	}
	limit := 512
	edns, hasEDNS := m.EDNS()
	if hasEDNS && udp {
		limit = min(max(int(edns.UDPSize), 512), p.MaxUDPResponse)
	}
	if len(m.Questions) != 1 {
		return p.reply(m, dnsmsg.RCodeFormErr, nil, udp, limit)
	}
	if m.Header.Opcode != dnsmsg.OpcodeQuery {
		return p.reply(m, dnsmsg.RCodeNotImp, nil, udp, limit)
	}
	qn := m.Questions[0]
	deny := func(rq *request, rf *refusal) []byte {
		p.logRefusal(ctx, rq, qn.Name, rf)
		return p.reply(m, dnsmsg.RCodeRefused, nil, udp, limit)
	}
	tk, _ := edns.Option(EDNSTicketOption)
	sess := p.tickets.get(string(tk), p.Now())
	if sess == nil {
		return deny(nil, refuse("unauthenticated", "DNS query without a valid ticket"))
	}
	rq := sess.request()
	rq.via = "dns"
	if rf := p.loadDecision(ctx, rq); rf != nil {
		return deny(rq, rf)
	}
	if rf := p.killSwitch(ctx, rq); rf != nil {
		return deny(rq, rf)
	}
	if qn.Class != dnsmsg.ClassINET {
		return deny(rq, refuse("invalid_target", "only class IN is served"))
	}
	if qn.Type == dnsmsg.TypeANY || qn.Type == dnsmsg.TypeOPT {
		return p.reply(m, dnsmsg.RCodeNotImp, nil, udp, limit)
	}
	allowed := map[string]bool{}
	check := func(name string) bool {
		k := strings.ToLower(name)
		if v, ok := allowed[k]; ok {
			return v
		}
		ok, rf := p.dnsAllowed(ctx, rq, name)
		if rf != nil {
			p.logRefusal(ctx, rq, name, rf)
		}
		allowed[k] = ok
		return ok
	}
	if !check(qn.Name) {
		return p.reply(m, dnsmsg.RCodeRefused, nil, udp, limit)
	}
	if p.Upstream == nil {
		return p.reply(m, dnsmsg.RCodeServFail, nil, udp, limit)
	}
	ans, err := p.Upstream.Lookup(ctx, qn.Name, qn.Type)
	if err != nil {
		p.Logger.WarnContext(ctx, "egress.dns_upstream_failed", "name", qn.Name, "error", err.Error())
		return p.reply(m, dnsmsg.RCodeServFail, nil, udp, limit)
	}
	p.Logger.InfoContext(ctx, "egress.dns", "name", qn.Name, "type", int(qn.Type), "rcode", int(ans.RCode), "decision_id", rq.decision)
	return p.reply(m, ans.RCode, filterChain(qn.Name, ans.Answers, check), udp, limit)
}

// filterChain keeps the records owned by name and by the targets of its
// CNAME chain, stopping at the first hop that check refuses (that hop's
// CNAME record, which belongs to an allowed owner, is kept).
func filterChain(name string, rrs []dnsmsg.RR, check func(string) bool) []dnsmsg.RR {
	if len(rrs) > maxAnswerRecords {
		rrs = rrs[:maxAnswerRecords]
	}
	owners := map[string]bool{strings.ToLower(name): true}
	cur := name
	for range maxCNAMEHops {
		next := ""
		for _, r := range rrs {
			if r.Type == dnsmsg.TypeCNAME && r.Data.Kind == dnsmsg.KindName && dnsmsg.EqualNames(r.Name, cur) {
				next = r.Data.Target
				break
			}
		}
		if next == "" || owners[strings.ToLower(next)] || !check(next) {
			break
		}
		owners[strings.ToLower(next)] = true
		cur = next
	}
	var out []dnsmsg.RR
	for _, r := range rrs {
		if owners[strings.ToLower(r.Name)] {
			out = append(out, r)
		}
	}
	return out
}

// dnsAllowed decides one name: an intelligence source for a passive
// decision, or a passive scope check of the name (reverse names are checked
// as the IP they denote).
func (p *Proxy) dnsAllowed(ctx context.Context, rq *request, name string) (bool, *refusal) {
	target, kind := strings.TrimSuffix(name, "."), scope.TargetHost
	if a, ok := dnsmsg.ParseReverseName(name); ok {
		target, kind = a.String(), scope.TargetIP
	}
	t, err := scope.Normalize(target, kind)
	if err != nil {
		return false, refuse("invalid_target", "name does not normalize")
	}
	if src := p.intelFor(t.String()); src != nil && kind == scope.TargetHost {
		if rq.rec.Class != core.ClassPassive {
			return false, refuse("intel_requires_passive", "intelligence sources are reachable only by passive tools")
		}
		if rf := p.recordIntel(ctx, rq, t.String(), nil); rf != nil {
			return false, rf
		}
		return true, nil
	}
	d, err := p.Scope.CheckTarget(ctx, scopesvc.Target{Tenant: rq.id.Tenant, Program: rq.rec.Program, Target: t.String(), Kind: kind,
		Class: core.ClassPassive, Run: rq.rec.RunID, Path: rq.rec.EffectPath})
	if err != nil {
		return false, refuse("scope_check_failed", "passive scope check failed")
	}
	if !d.Allowed() {
		return false, refuse("dns_denied", d.Reason)
	}
	return true, nil
}

// reply encodes the answer to m within limit octets.
func (p *Proxy) reply(m dnsmsg.Message, rc dnsmsg.RCode, answers []dnsmsg.RR, udp bool, limit int) []byte {
	r := dnsmsg.Message{Header: dnsmsg.Header{ID: m.Header.ID, Response: true, Opcode: m.Header.Opcode,
		// The header holds only the low 4 bits of an RCODE; the forwarder
		// does not relay extended RCODEs in OPT.
		RecursionDesired: m.Header.RecursionDesired, RecursionAvailable: true, CheckingDisabled: m.Header.CheckingDisabled, RCode: rc & 0xF},
		Questions: m.Questions, Answers: answers}
	if _, ok := m.EDNS(); ok {
		r.SetEDNS(dnsmsg.EDNS{UDPSize: uint16(p.MaxUDPResponse)})
	}
	if !udp {
		limit = p.MaxTCPResponse
	}
	b, _, err := dnsmsg.EncodeTruncated(r, limit)
	if err != nil {
		r.Questions, r.Answers, r.Additional = nil, nil, nil
		r.Header.RCode = dnsmsg.RCodeServFail
		b, _ = dnsmsg.Encode(r)
	}
	return b
}

// RecordsUpstream answers from a fixed record set, following CNAMEs like a
// recursive resolver (tests and the dev stack's injected names). A name
// with no record of any type is NXDOMAIN.
type RecordsUpstream struct {
	mu  sync.Mutex
	RRs []dnsmsg.RR
}

// Set replaces the records owned by name of type t.
func (u *RecordsUpstream) Set(name string, t dnsmsg.Type, rrs ...dnsmsg.RR) {
	u.mu.Lock()
	defer u.mu.Unlock()
	kept := u.RRs[:0:0]
	for _, r := range u.RRs {
		if !(dnsmsg.EqualNames(r.Name, name) && r.Type == t) {
			kept = append(kept, r)
		}
	}
	u.RRs = append(kept, rrs...)
}

// Lookup implements Upstream.
func (u *RecordsUpstream) Lookup(_ context.Context, name string, t dnsmsg.Type) (DNSAnswer, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []dnsmsg.RR
	cur, known := name, false
	for range maxCNAMEHops + 1 {
		var cname *dnsmsg.RR
		for i, r := range u.RRs {
			if !dnsmsg.EqualNames(r.Name, cur) {
				continue
			}
			known = true
			if r.Type == t {
				out = append(out, r)
			} else if r.Type == dnsmsg.TypeCNAME && t != dnsmsg.TypeCNAME && cname == nil {
				cname = &u.RRs[i]
			}
		}
		if cname == nil || len(out) > 0 {
			break
		}
		out = append(out, *cname)
		cur = cname.Data.Target
	}
	if !known {
		return DNSAnswer{RCode: dnsmsg.RCodeNXDomain}, nil
	}
	return DNSAnswer{Answers: out}, nil
}

// ResolverUpstream answers A and AAAA queries with a scope.Resolver (the
// dev stack's injected resolver) and other types from Records.
type ResolverUpstream struct {
	Resolver scope.Resolver
	Records  *RecordsUpstream
	// TTL of synthesized records (default 60 s).
	TTL uint32
}

// Lookup implements Upstream.
func (u ResolverUpstream) Lookup(ctx context.Context, name string, t dnsmsg.Type) (DNSAnswer, error) {
	if u.Records != nil {
		if a, err := u.Records.Lookup(ctx, name, t); err == nil && len(a.Answers) > 0 {
			return a, nil
		}
	}
	if t != dnsmsg.TypeA && t != dnsmsg.TypeAAAA {
		return DNSAnswer{}, nil
	}
	ttl := u.TTL
	if ttl == 0 {
		ttl = 60
	}
	addrs, err := u.Resolver.LookupAddrs(ctx, strings.TrimSuffix(name, "."))
	if err != nil {
		var de *net.DNSError
		if errors.As(err, &de) && de.IsNotFound {
			return DNSAnswer{RCode: dnsmsg.RCodeNXDomain}, nil
		}
		return DNSAnswer{}, err
	}
	var out []dnsmsg.RR
	for _, a := range addrs {
		a = a.Unmap()
		switch {
		case t == dnsmsg.TypeA && a.Is4():
			out = append(out, dnsmsg.RR{Name: name, Type: t, Class: dnsmsg.ClassINET, TTL: ttl, Data: dnsmsg.RData{Kind: dnsmsg.KindA, IP: a}})
		case t == dnsmsg.TypeAAAA && a.Is6():
			out = append(out, dnsmsg.RR{Name: name, Type: t, Class: dnsmsg.ClassINET, TTL: ttl, Data: dnsmsg.RData{Kind: dnsmsg.KindAAAA, IP: a}})
		}
	}
	return DNSAnswer{Answers: out}, nil
}

// ServerUpstream forwards to a recursive resolver at Addr (host:port) over
// UDP, retrying over TCP when the answer is truncated.
type ServerUpstream struct {
	Addr    string
	Timeout time.Duration // per attempt, default 3 s
}

// Lookup implements Upstream.
func (u ServerUpstream) Lookup(ctx context.Context, name string, t dnsmsg.Type) (DNSAnswer, error) {
	to := u.Timeout
	if to <= 0 {
		to = 3 * time.Second
	}
	id := uint16(core.SystemRng{}.Uint64())
	q := dnsmsg.NewQuery(id, name, t)
	// 1232 octets: the DNS Flag Day 2020 payload size, which avoids IP
	// fragmentation on common paths.
	q.SetEDNS(dnsmsg.EDNS{UDPSize: 1232})
	b, err := dnsmsg.Encode(q)
	if err != nil {
		return DNSAnswer{}, err
	}
	resp, err := exchange(ctx, "udp", u.Addr, b, to)
	if err == nil {
		var m dnsmsg.Message
		if m, _, err = dnsmsg.Decode(resp, dnsmsg.Lenient); err == nil && m.Header.Truncated {
			resp, err = exchange(ctx, "tcp", u.Addr, b, to)
		}
	}
	if err != nil {
		return DNSAnswer{}, err
	}
	m, _, err := dnsmsg.Decode(resp, dnsmsg.Lenient)
	if err != nil {
		return DNSAnswer{}, err
	}
	if m.Header.ID != id || !m.Header.Response {
		return DNSAnswer{}, errors.New("egress: mismatched DNS response")
	}
	return DNSAnswer{RCode: m.Header.RCode, Answers: m.Answers}, nil
}

func exchange(ctx context.Context, network, addr string, q []byte, to time.Duration) ([]byte, error) {
	d := net.Dialer{Timeout: to}
	c, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(to))
	if network == "tcp" {
		if err := dnsmsg.WriteTCP(c, q); err != nil {
			return nil, err
		}
		return dnsmsg.ReadTCP(c, dnsmsg.MaxMessageLen)
	}
	if _, err := c.Write(q); err != nil {
		return nil, err
	}
	buf := make([]byte, dnsmsg.MaxMessageLen)
	n, err := c.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// NetUpstream answers with the system resolver (net.Resolver): A, AAAA,
// CNAME, MX, NS, TXT and PTR. TTLs are not available and are set to TTL.
type NetUpstream struct {
	R   *net.Resolver
	TTL uint32
}

// Lookup implements Upstream.
func (u NetUpstream) Lookup(ctx context.Context, name string, t dnsmsg.Type) (DNSAnswer, error) {
	r := u.R
	if r == nil {
		r = net.DefaultResolver
	}
	ttl := u.TTL
	if ttl == 0 {
		ttl = 60
	}
	rr := func(d dnsmsg.RData) dnsmsg.RR {
		return dnsmsg.RR{Name: name, Type: t, Class: dnsmsg.ClassINET, TTL: ttl, Data: d}
	}
	abs := func(s string) string {
		if !strings.HasSuffix(s, ".") {
			return s + "."
		}
		return s
	}
	var out []dnsmsg.RR
	var err error
	host := strings.TrimSuffix(name, ".")
	switch t {
	case dnsmsg.TypeA, dnsmsg.TypeAAAA:
		var ips []netip.Addr
		network := "ip4"
		if t == dnsmsg.TypeAAAA {
			network = "ip6"
		}
		if ips, err = r.LookupNetIP(ctx, network, host); err == nil {
			for _, a := range ips {
				k := dnsmsg.KindA
				if t == dnsmsg.TypeAAAA {
					k = dnsmsg.KindAAAA
				}
				out = append(out, rr(dnsmsg.RData{Kind: k, IP: a.Unmap()}))
			}
		}
	case dnsmsg.TypeCNAME:
		var c string
		if c, err = r.LookupCNAME(ctx, host); err == nil && !dnsmsg.EqualNames(c, name) {
			out = append(out, rr(dnsmsg.RData{Kind: dnsmsg.KindName, Target: abs(c)}))
		}
	case dnsmsg.TypeMX:
		var mxs []*net.MX
		if mxs, err = r.LookupMX(ctx, host); err == nil {
			for _, mx := range mxs {
				out = append(out, rr(dnsmsg.RData{Kind: dnsmsg.KindMX, Preference: mx.Pref, Target: abs(mx.Host)}))
			}
		}
	case dnsmsg.TypeNS:
		var nss []*net.NS
		if nss, err = r.LookupNS(ctx, host); err == nil {
			for _, ns := range nss {
				out = append(out, rr(dnsmsg.RData{Kind: dnsmsg.KindName, Target: abs(ns.Host)}))
			}
		}
	case dnsmsg.TypeTXT:
		var txts []string
		if txts, err = r.LookupTXT(ctx, host); err == nil {
			for _, s := range txts {
				// net.Resolver joins a record's character-strings; split
				// again at the 255-octet limit of one (RFC 1035 §3.3).
				var parts []string
				for len(s) > 255 {
					parts, s = append(parts, s[:255]), s[255:]
				}
				out = append(out, rr(dnsmsg.RData{Kind: dnsmsg.KindTXT, Strings: append(parts, s)}))
			}
		}
	case dnsmsg.TypePTR:
		if a, ok := dnsmsg.ParseReverseName(name); ok {
			var names []string
			if names, err = r.LookupAddr(ctx, a.String()); err == nil {
				for _, n := range names {
					out = append(out, rr(dnsmsg.RData{Kind: dnsmsg.KindName, Target: abs(n)}))
				}
			}
		}
	default:
		return DNSAnswer{RCode: dnsmsg.RCodeNotImp}, nil
	}
	if err != nil {
		var de *net.DNSError
		if errors.As(err, &de) && de.IsNotFound {
			return DNSAnswer{RCode: dnsmsg.RCodeNXDomain}, nil
		}
		return DNSAnswer{}, err
	}
	return DNSAnswer{Answers: out}, nil
}
