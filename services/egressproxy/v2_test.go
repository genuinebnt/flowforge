package egressproxy_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dnsmsg"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/egressproxy"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
	"github.com/genuinebnt/flowforge/services/toolrunner"
	"github.com/genuinebnt/flowforge/socks5"
	"github.com/genuinebnt/flowforge/testkit"
	"google.golang.org/protobuf/types/known/structpb"
)

// tcpTarget is a TCP listener that answers "hello <n>" and counts accepts.
type tcpTarget struct {
	l    net.Listener
	hits atomic.Int64
	port int
	addr string
}

func tcpListen(t *testing.T, addr string) *tcpTarget {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	tt := &tcpTarget{l: l, addr: l.Addr().String()}
	_, ps, _ := net.SplitHostPort(l.Addr().String())
	tt.port, _ = strconv.Atoi(ps)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			n := tt.hits.Add(1)
			go func() {
				defer c.Close()
				_, _ = io.WriteString(c, "hello "+l.Addr().String()+" "+strconv.FormatInt(n, 10)+"\n")
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()
	t.Cleanup(func() { l.Close() })
	return tt
}

type v2env struct {
	st       *devstack.Stack
	ctx      context.Context
	program  core.UUID
	prog     string
	resolver *servicetest.Resolver
	records  *egressproxy.RecordsUpstream
	clock    *testkit.FakeClock
	eg       *toolrunner.Egress
	tc       *toolrunner.ToolCtx
}

// newV2 starts a stack whose program allows app.test (and *.app.test) and
// 127.0.0.1, with the given rate and allowed ports, a fake limiter clock,
// a records-backed DNS upstream and the intel sources given.
func newV2(t *testing.T, rate *pb.RateLimit, ports []int, intel []egressproxy.IntelSource) *v2env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	e := &v2env{resolver: servicetest.NewResolver(map[string]string{"app.test": "127.0.0.1", "www.app.test": "127.0.0.1",
		"evil.test": "127.0.0.1", "crt.test": "127.0.0.1"}),
		records: &egressproxy.RecordsUpstream{}, clock: testkit.NewFakeClock(time.Now())}
	st, err := devstack.Start(ctx, devstack.Options{Resolver: e.resolver, EgressPorts: ports, IntelSources: intel,
		DNSUpstream: egressproxy.ResolverUpstream{Resolver: e.resolver, Records: e.records}, EgressClock: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	e.ctx = st.As(ctx, tenant, "user:0190a1b2-0000-7000-8000-000000000001", auth.RoleAdmin)
	prog, err := st.Scope.CreateProgram(e.ctx, &pb.CreateProgramRequest{Name: "egress-v2", InitialScope: &pb.ScopeDocument{
		Rules: []*pb.ScopeRule{
			{Id: "app", Effect: pb.RuleEffect_RULE_EFFECT_ALLOW, Kind: pb.RuleKind_RULE_KIND_DOMAIN, Value: "app.test"},
			{Id: "app-sub", Effect: pb.RuleEffect_RULE_EFFECT_ALLOW, Kind: pb.RuleKind_RULE_KIND_WILDCARD_DOMAIN, Value: "*.app.test"},
			{Id: "lo", Effect: pb.RuleEffect_RULE_EFFECT_ALLOW, Kind: pb.RuleKind_RULE_KIND_CIDR, Value: "127.0.0.1/32"},
		},
		Activity: &pb.ActivityPolicy{MaxClass: pb.ActivityClass_ACTIVITY_CLASS_ACTIVE, Rate: rate},
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.prog = prog.GetId()
	e.program, _ = core.ParsePublicID(core.PrefixProgram, prog.GetId())
	sc := scopesvc.NewClient(st.Scope, "tool-runner")
	e.tc = &toolrunner.ToolCtx{Tenant: tenant, Program: e.program, Path: "0", Clock: &core.SystemClock{}}
	e.tc.Check = func(ctx context.Context, tgt string, kind scope.TargetKind, class core.ActivityClass) (scope.Decision, error) {
		return sc.CheckTarget(ctx, scopesvc.Target{Tenant: tenant, Program: e.program, Target: tgt, Kind: kind, Class: class, Resolve: class > core.ClassPassive})
	}
	e.tc.CheckPinned = func(ctx context.Context, tgt string, kind scope.TargetKind, class core.ActivityClass) (scope.Decision, error) {
		return sc.CheckTarget(ctx, scopesvc.Target{Tenant: tenant, Program: e.program, Target: tgt, Kind: kind, Class: class, Resolve: true})
	}
	if e.eg, err = toolrunner.NewEgress("http://"+st.Addrs[devstack.EgressName], st.Tokens); err != nil {
		t.Fatal(err)
	}
	e.tc.Egress = e.eg
	return e
}

func (e *v2env) decide(t *testing.T, target string, kind scope.TargetKind, class core.ActivityClass, pinned bool) scope.Decision {
	t.Helper()
	check := e.tc.Check
	if pinned {
		check = e.tc.CheckPinned
	}
	d, err := check(e.ctx, target, kind, class)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func refusedReason(err error) string {
	var re *toolrunner.RefusedError
	if errors.As(err, &re) {
		return re.Reason
	}
	return ""
}

func readGreeting(t *testing.T, c net.Conn) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	s, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	return s
}

// TestEgressV2_TCPPorts: a host-level decision covers (host, port) only for
// ports in the program's port policy and, when declared, in the tool
// call's declaration; CONNECT to an allowed port reaches the pinned
// address, any other port is refused before anything is dialled.
func TestEgressV2_TCPPorts(t *testing.T) {
	allowed := tcpListen(t, "127.0.0.1:0")
	other := tcpListen(t, "127.0.0.1:0")
	e := newV2(t, nil, []int{allowed.port}, nil)
	d := e.decide(t, "app.test", scope.TargetHost, core.ClassActive, true)
	if !d.Allowed() || len(d.PinnedIPs) != 1 {
		t.Fatalf("decision %+v", d)
	}
	c, err := e.eg.DialTCP(e.ctx, e.tc, d, "app.test", allowed.port, []int{allowed.port})
	if err != nil {
		t.Fatalf("CONNECT to an allowed port: %v", err)
	}
	if g := readGreeting(t, c); g == "" {
		t.Fatal("no data")
	}
	c.Close()
	// A port outside the policy.
	if _, err := e.eg.DialTCP(e.ctx, e.tc, d, "app.test", other.port, []int{other.port}); refusedReason(err) != "port_not_allowed" {
		t.Fatalf("CONNECT to a non-allowed port: %v", err)
	}
	// An allowed port the tool call did not declare.
	if _, err := e.eg.DialTCP(e.ctx, e.tc, d, "app.test", allowed.port, []int{22}); refusedReason(err) != "port_not_allowed" {
		t.Fatalf("undeclared port: %v", err)
	}
	// A URL decision covers only its own port, whatever the policy.
	ud := e.decide(t, "https://app.test:"+strconv.Itoa(other.port)+"/", scope.TargetURL, core.ClassActive, true)
	c, err = e.eg.Dial(e.ctx, e.tc, ud, "app.test:"+strconv.Itoa(other.port))
	if err != nil {
		t.Fatalf("URL decision on its port: %v", err)
	}
	c.Close()
	if _, err := e.eg.Dial(e.ctx, e.tc, ud, "app.test:"+strconv.Itoa(allowed.port)); refusedReason(err) != "host_mismatch" {
		t.Fatalf("URL decision on another port: %v", err)
	}
	if other.hits.Load() != 1 || allowed.hits.Load() != 1 {
		t.Fatalf("hits: allowed %d other %d", allowed.hits.Load(), other.hits.Load())
	}
	if e.st.Metrics.Registry.Value("ff_egress_refused_total", "port_not_allowed") < 2 {
		t.Fatal("port refusals not counted")
	}
}

// TestEgressV2_SOCKS5: SOCKS5 CONNECT with the decision id and a ticket
// reaches the pinned address; another host, an undeclared port, a bad
// ticket and BIND are refused; DNS rebinding after the decision still only
// reaches the pinned address.
func TestEgressV2_SOCKS5(t *testing.T) {
	target := tcpListen(t, "127.0.0.1:0")
	rebound := tcpListen(t, "127.0.0.2:"+strconv.Itoa(target.port))
	e := newV2(t, nil, []int{target.port}, nil)
	e.eg.SOCKS = true
	d := e.decide(t, "app.test", scope.TargetHost, core.ClassActive, true)
	c, err := e.eg.DialTCP(e.ctx, e.tc, d, "app.test", target.port, []int{target.port})
	if err != nil {
		t.Fatalf("SOCKS5 CONNECT: %v", err)
	}
	readGreeting(t, c)
	c.Close()
	// Rebinding: app.test now resolves to 127.0.0.2; the decision's pin wins.
	e.resolver.Set("app.test", "127.0.0.2")
	c, err = e.eg.DialTCP(e.ctx, e.tc, d, "app.test", target.port, []int{target.port})
	if err != nil {
		t.Fatalf("SOCKS5 after rebinding: %v", err)
	}
	readGreeting(t, c)
	c.Close()
	if rebound.hits.Load() != 0 || target.hits.Load() != 2 {
		t.Fatalf("rebinding reached %d (target %d)", rebound.hits.Load(), target.hits.Load())
	}
	e.resolver.Set("app.test", "127.0.0.1")
	// Another host under app.test's decision, and an IP literal.
	for _, h := range []string{"evil.test", "127.0.0.1"} {
		if _, err := e.eg.DialTCP(e.ctx, e.tc, d, h, target.port, []int{target.port}); refusedReason(err) == "" {
			t.Fatalf("SOCKS5 to %s: %v", h, err)
		}
	}
	// An undeclared port: the ticket binds the declaration.
	if _, err := e.eg.DialTCP(e.ctx, e.tc, d, "app.test", target.port, []int{1}); refusedReason(err) == "" {
		t.Fatalf("undeclared port: %v", err)
	}
	tk, err := e.eg.Ticket(e.ctx, e.tc, d, []int{target.port})
	if err != nil || tk.SOCKS5 != e.st.Addrs[devstack.SOCKSName] || tk.DNS != e.st.Addrs[devstack.DNSName] {
		t.Fatalf("ticket %+v %v", tk, err)
	}
	raw := func() net.Conn {
		c, err := net.Dial("tcp", tk.SOCKS5)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		return c
	}
	// A forged ticket and a ticket under another decision id fail auth.
	for _, up := range [][2]string{{tk.DecisionID, "tk_forged"}, {"dec_01JD8KBB00000000000000000", tk.Ticket}} {
		c := raw()
		if _, err := socks5.ClientConnect(c, up[0], up[1], socks5.AddrFrom("app.test", uint16(target.port))); socks5.CodeOf(err) != socks5.CodeAuthFailed {
			t.Fatalf("auth %v: %v", up, err)
		}
		c.Close()
	}
	// BIND is parsed and refused with reply 7.
	c = raw()
	g, _ := socks5.AppendGreeting(nil, socks5.Greeting{Methods: []socks5.Method{socks5.MethodUserPass}})
	u, _ := socks5.AppendUserPass(nil, socks5.UserPass{Username: tk.DecisionID, Password: tk.Ticket})
	b, _ := socks5.AppendRequest(nil, socks5.Request{Command: socks5.CmdBind, Addr: socks5.AddrFrom("app.test", uint16(target.port))})
	_, _ = c.Write(append(append(g, u...), b...))
	_, _ = socks5.ReadMethodSelection(c)
	_, _ = socks5.ReadAuthReply(c)
	if rep, err := socks5.ReadReply(c); err != nil || rep.Code != socks5.CommandNotSupported {
		t.Fatalf("BIND: %+v %v", rep, err)
	}
	c.Close()
	if target.hits.Load() != 2 {
		t.Fatalf("refused requests reached the target: %d", target.hits.Load())
	}
}

// TestEgressV2_DNSForwarder: real UDP and TCP queries (dnsmsg) with a
// ticket are answered for in-scope names, REFUSED for others and without a
// ticket; CNAME chains are cut at the first out-of-scope hop.
func TestEgressV2_DNSForwarder(t *testing.T) {
	e := newV2(t, nil, nil, []egressproxy.IntelSource{{Host: "asn.cymru.test", Subdomains: true}})
	e.records.Set("www.app.test.", dnsmsg.TypeCNAME, dnsmsg.RR{Name: "www.app.test.", Type: dnsmsg.TypeCNAME, Class: 1, TTL: 30,
		Data: dnsmsg.RData{Kind: dnsmsg.KindName, Target: "edge.app.test."}})
	e.records.Set("edge.app.test.", dnsmsg.TypeA, dnsmsg.RR{Name: "edge.app.test.", Type: dnsmsg.TypeA, Class: 1, TTL: 30,
		Data: dnsmsg.RData{Kind: dnsmsg.KindA, IP: netip.MustParseAddr("192.0.2.10")}})
	e.records.Set("shop.app.test.", dnsmsg.TypeCNAME, dnsmsg.RR{Name: "shop.app.test.", Type: dnsmsg.TypeCNAME, Class: 1, TTL: 30,
		Data: dnsmsg.RData{Kind: dnsmsg.KindName, Target: "shops.evil.test."}})
	e.records.Set("shops.evil.test.", dnsmsg.TypeA, dnsmsg.RR{Name: "shops.evil.test.", Type: dnsmsg.TypeA, Class: 1, TTL: 30,
		Data: dnsmsg.RData{Kind: dnsmsg.KindA, IP: netip.MustParseAddr("198.51.100.66")}})
	e.records.Set("1.2.0.192.origin.asn.cymru.test.", dnsmsg.TypeTXT, dnsmsg.RR{Name: "1.2.0.192.origin.asn.cymru.test.", Type: dnsmsg.TypeTXT,
		Class: 1, TTL: 30, Data: dnsmsg.RData{Kind: dnsmsg.KindTXT, Strings: []string{"64500 | 192.0.2.0/24 | ZZ | test | 2026-01-01"}}})
	d := e.decide(t, "app.test", scope.TargetHost, core.ClassPassive, false)
	tk, err := e.eg.Ticket(e.ctx, e.tc, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	dnsAddr := e.st.Addrs[devstack.DNSName]
	q := func(name string, typ dnsmsg.Type) (egressproxy.DNSAnswer, error) {
		return toolrunner.QueryDNS(e.ctx, dnsAddr, tk.Ticket, name, typ)
	}
	ans, err := q("app.test.", dnsmsg.TypeA)
	if err != nil || len(ans.Answers) != 1 || ans.Answers[0].Data.IP.String() != "127.0.0.1" {
		t.Fatalf("in-scope A: %+v %v", ans, err)
	}
	ans, err = q("www.app.test.", dnsmsg.TypeA)
	if err != nil || len(ans.Answers) != 2 || ans.Answers[1].Data.IP.String() != "192.0.2.10" {
		t.Fatalf("in-scope CNAME chain: %+v %v", ans, err)
	}
	ans, err = q("shop.app.test.", dnsmsg.TypeA)
	if err != nil || len(ans.Answers) != 1 || ans.Answers[0].Type != dnsmsg.TypeCNAME {
		t.Fatalf("chain leaving the scope must stop at the CNAME: %+v %v", ans, err)
	}
	if _, err := q("evil.test.", dnsmsg.TypeA); refusedReason(err) != "dns_refused" {
		t.Fatalf("out-of-scope name: %v", err)
	}
	// Reverse names are checked as the IP they denote.
	if _, err := q(dnsmsg.ReverseName(netip.MustParseAddr("127.0.0.1")), dnsmsg.TypePTR); err != nil {
		t.Fatalf("in-scope PTR: %v", err)
	}
	if _, err := q(dnsmsg.ReverseName(netip.MustParseAddr("198.51.100.66")), dnsmsg.TypePTR); refusedReason(err) != "dns_refused" {
		t.Fatalf("out-of-scope PTR: %v", err)
	}
	// Intel zone: allowed for this passive decision.
	ans, err = q("1.2.0.192.origin.asn.cymru.test.", dnsmsg.TypeTXT)
	if err != nil || len(ans.Answers) != 1 {
		t.Fatalf("intel TXT: %+v %v", ans, err)
	}
	// ... but not for an active decision.
	ad := e.decide(t, "app.test", scope.TargetHost, core.ClassActive, true)
	atk, err := e.eg.Ticket(e.ctx, e.tc, ad, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := toolrunner.QueryDNS(e.ctx, dnsAddr, atk.Ticket, "1.2.0.192.origin.asn.cymru.test.", dnsmsg.TypeTXT); refusedReason(err) != "dns_refused" {
		t.Fatalf("intel name under an active decision: %v", err)
	}
	// No ticket, or a forged one: REFUSED (a raw UDP query built with dnsmsg).
	for _, ticket := range []string{"", "tk_forged"} {
		m := dnsmsg.NewQuery(77, "app.test.", dnsmsg.TypeA)
		if ticket != "" {
			m.SetEDNS(dnsmsg.EDNS{UDPSize: 1232, Options: []dnsmsg.Option{{Code: egressproxy.EDNSTicketOption, Data: []byte(ticket)}}})
		}
		b, _ := dnsmsg.Encode(m)
		c, err := net.Dial("udp", dnsAddr)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Write(b)
		buf := make([]byte, 2048)
		n, err := c.Read(buf)
		c.Close()
		if err != nil {
			t.Fatal(err)
		}
		r, _, err := dnsmsg.Decode(buf[:n], dnsmsg.Strict)
		if err != nil || r.Header.ID != 77 || r.Header.RCode != dnsmsg.RCodeRefused || len(r.Answers) != 0 {
			t.Fatalf("ticket %q: %+v %v", ticket, r.Header, err)
		}
	}
	// TCP framing.
	c, err := net.Dial("tcp", dnsAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	m := dnsmsg.NewQuery(78, "app.test.", dnsmsg.TypeA)
	m.SetEDNS(dnsmsg.EDNS{UDPSize: 1232, Options: []dnsmsg.Option{{Code: egressproxy.EDNSTicketOption, Data: []byte(tk.Ticket)}}})
	b, _ := dnsmsg.Encode(m)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := dnsmsg.WriteTCP(c, b); err != nil {
		t.Fatal(err)
	}
	rb, err := dnsmsg.ReadTCP(c, dnsmsg.MaxMessageLen)
	if err != nil {
		t.Fatal(err)
	}
	if r, _, err := dnsmsg.Decode(rb, dnsmsg.Strict); err != nil || r.Header.RCode != 0 || len(r.Answers) != 1 {
		t.Fatalf("TCP answer: %+v %v", r, err)
	}
	// The passive checks were recorded as decisions.
	recorded := 0
	for _, rec := range e.st.ScopeRepo.(*scopesvc.MemRepo).Decisions(tenant) {
		if rec.Class == core.ClassPassive && rec.Decision.Target == "shops.evil.test" && !rec.Decision.Allowed() {
			recorded++
		}
	}
	if recorded == 0 {
		t.Fatal("the denied CNAME hop was not recorded as a decision")
	}
	// dnsresolve end to end through the forwarder.
	em := &collect{}
	out, err := toolrunner.DNSResolve{}.Run(e.ctx, json.RawMessage(`"www.app.test"`), e.tc, em)
	if err != nil || string(out) != `["192.0.2.10"]` || em.obs != 1 {
		t.Fatalf("dnsresolve: %s %v %d", out, err, em.obs)
	}
}

// TestEgressV2_RateLimit: a tool with internal parallelism is held to the
// program rate at the proxy: with the limiter clock frozen exactly burst
// connections pass, the rest are refused 429 rate_limited; after one second
// rate more pass. The concurrency cap holds open tunnels per target.
func TestEgressV2_RateLimit(t *testing.T) {
	var targets []*tcpTarget
	var ports []int
	for range 12 {
		tt := tcpListen(t, "127.0.0.1:0")
		targets = append(targets, tt)
		ports = append(ports, tt.port)
	}
	e := newV2(t, &pb.RateLimit{RequestsPerSecond: 2, Burst: 3, Concurrency: 50}, ports, nil)
	scan := toolrunner.PortScan{Pace: time.Nanosecond, Parallel: 12, Retries: -1}
	in, _ := json.Marshal([]any{"app.test", ports})
	em := &collect{}
	out, err := scan.Run(e.ctx, in, e.tc, em)
	if err != nil {
		t.Fatal(err)
	}
	var open []int
	_ = json.Unmarshal(out, &open)
	hits := func() int64 {
		var n int64
		for _, tt := range targets {
			n += tt.hits.Load()
		}
		return n
	}
	if len(open) != 3 || hits() != 3 {
		t.Fatalf("frozen clock: %d open, %d connections, want burst 3 (%s)", len(open), hits(), out)
	}
	limited := e.st.Metrics.Registry.Value("ff_egress_refused_total", "rate_limited")
	if limited != 9 {
		t.Fatalf("rate_limited refusals %v, want 9", limited)
	}
	e.clock.Advance(time.Second)
	out, err = scan.Run(e.ctx, in, e.tc, &collect{})
	_ = json.Unmarshal(out, &open)
	if err != nil || len(open) != 2 || hits() != 5 {
		t.Fatalf("after 1 s at 2 rps: %s %v, %d connections", out, err, hits())
	}
	// Concurrency: two tunnels open, the third is refused until one closes.
	e = newV2(t, &pb.RateLimit{RequestsPerSecond: 100, Burst: 100, Concurrency: 2}, ports, nil)
	d := e.decide(t, "app.test", scope.TargetHost, core.ClassActive, true)
	var held []net.Conn
	for range 2 {
		c, err := e.eg.DialTCP(e.ctx, e.tc, d, "app.test", ports[0], ports)
		if err != nil {
			t.Fatal(err)
		}
		readGreeting(t, c)
		held = append(held, c)
	}
	if _, err := e.eg.DialTCP(e.ctx, e.tc, d, "app.test", ports[1], ports); refusedReason(err) != "concurrency_limited" {
		t.Fatalf("third tunnel: %v", err)
	}
	held[0].Close()
	servicetest.Eventually(t, 5*time.Second, "slot released", func() bool {
		c, err := e.eg.DialTCP(e.ctx, e.tc, d, "app.test", ports[1], ports)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	held[1].Close()
	// Plain HTTP requests are charged too: a 429 with Retry-After.
	web := listen(t, "127.0.0.1:0", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	e2 := newV2(t, &pb.RateLimit{RequestsPerSecond: 1, Burst: 1, Concurrency: 4}, []int{web.port}, nil)
	ud := e2.decide(t, "http://app.test:"+strconv.Itoa(web.port)+"/", scope.TargetURL, core.ClassActive, true)
	cl := rawProxyClient(t, e2.st, ud.ID)
	if r := do(t, cl, "http://app.test:"+strconv.Itoa(web.port)+"/"); r.StatusCode != http.StatusOK {
		t.Fatalf("first request %d", r.StatusCode)
	}
	r := do(t, cl, "http://app.test:"+strconv.Itoa(web.port)+"/")
	if r.StatusCode != http.StatusTooManyRequests || r.Header.Get(platform.HeaderRefused) != "rate_limited" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("second request: %d %q %q", r.StatusCode, r.Header.Get(platform.HeaderRefused), r.Header.Get("Retry-After"))
	}
}

// TestEgressV2_IntelSources: allowlisted intelligence sources are reachable
// only under passive decisions, each connection is recorded as an
// intel_source decision, and ctlog works end to end through the proxy.
func TestEgressV2_IntelSources(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	crt := listen(t, "127.0.0.1:0", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RawQuery)
		mu.Unlock()
		if r.URL.Query().Get("output") == "json" {
			_, _ = io.WriteString(w, `[{"id":7,"name_value":"www.app.test\n*.dev.app.test","common_name":"www.app.test"}]`)
			return
		}
		http.NotFound(w, r)
	})
	e := newV2(t, nil, nil, []egressproxy.IntelSource{{Host: "crt.test", Ports: []int{crt.port}, Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}})
	crtURL := "http://crt.test:" + strconv.Itoa(crt.port) + "/?q=x&output=json"
	passive := e.decide(t, "app.test", scope.TargetHost, core.ClassPassive, false)
	if r := do(t, rawProxyClient(t, e.st, passive.ID), crtURL); r.StatusCode != http.StatusOK {
		t.Fatalf("passive decision to an intel source: %d %q", r.StatusCode, r.Header.Get(platform.HeaderRefused))
	}
	active := e.decide(t, "app.test", scope.TargetHost, core.ClassActive, true)
	if r := do(t, rawProxyClient(t, e.st, active.ID), crtURL); r.StatusCode != http.StatusForbidden || r.Header.Get(platform.HeaderRefused) != "intel_requires_passive" {
		t.Fatalf("active decision to an intel source: %d %q", r.StatusCode, r.Header.Get(platform.HeaderRefused))
	}
	// A non-allowlisted host stays a host mismatch for the passive decision.
	if r := do(t, rawProxyClient(t, e.st, passive.ID), "http://evil.test:"+strconv.Itoa(crt.port)+"/"); r.Header.Get(platform.HeaderRefused) != "host_mismatch" {
		t.Fatalf("passive decision to a non-intel host: %q", r.Header.Get(platform.HeaderRefused))
	}
	// ctlog through the real proxy.
	em := &collect{}
	out, err := (&toolrunner.CTLog{BaseURL: "http://crt.test:" + strconv.Itoa(crt.port)}).Run(e.ctx, json.RawMessage(`"app.test"`), e.tc, em)
	if err != nil || string(out) != `["dev.app.test","www.app.test"]` {
		t.Fatalf("ctlog: %s %v %+v", out, err, em.skipped)
	}
	intel := 0
	for _, rec := range e.st.ScopeRepo.(*scopesvc.MemRepo).Decisions(tenant) {
		if rec.Decision.Reason == egressproxy.ReasonIntelSource {
			intel++
			if rec.Decision.Target != "crt.test" || rec.Class != core.ClassPassive || rec.Program != e.program || !rec.Decision.Allowed() {
				t.Fatalf("intel decision %+v", rec)
			}
		}
	}
	if intel < 3 {
		t.Fatalf("intel_source decisions recorded: %d", intel)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) < 3 {
		t.Fatalf("crt.test requests %v", paths)
	}
}

// collect is a minimal Emitter.
type collect struct {
	mu      sync.Mutex
	obs     int
	skipped []string
}

func (c *collect) Observation(o oam.Observation) error {
	o.Tool = oam.Tool{Name: "test"}
	if err := o.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	c.obs++
	c.mu.Unlock()
	return nil
}
func (c *collect) Progress(int64, int64, string) error { return nil }
func (c *collect) Skipped(t, r string) error {
	c.mu.Lock()
	c.skipped = append(c.skipped, t+" "+r)
	c.mu.Unlock()
	return nil
}

// TestEgressV2_TLSCert: tlscert (passive, pinned decision) reads the chain
// through a CONNECT tunnel and aborts the handshake: the server never
// receives a request.
func TestEgressV2_TLSCert(t *testing.T) {
	var reqs atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reqs.Add(1) }))
	srv.StartTLS()
	t.Cleanup(srv.Close)
	_, ps, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(ps)
	e := newV2(t, nil, []int{port}, nil)
	in, _ := json.Marshal([]any{"app.test", port})
	// D3: tlscert's passive check asks for resolution, so the decision pins
	// addresses and the proxy accepts the tunnel.
	d := e.decide(t, "app.test", scope.TargetHost, core.ClassPassive, true)
	if !d.Allowed() || len(d.PinnedIPs) != 1 {
		t.Fatalf("passive pinned decision %+v", d)
	}
	if nd := e.decide(t, "app.test", scope.TargetHost, core.ClassPassive, false); len(nd.PinnedIPs) != 0 {
		t.Fatalf("passive check without resolution pinned %v", nd.PinnedIPs)
	}
	em := &collect{}
	out, err := toolrunner.TLSCert{}.Run(e.ctx, in, e.tc, em)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Issuer string `json:"issuer_common_name"`
		Serial string `json:"serial_number"`
	}
	if err := json.Unmarshal(out, &rec); err != nil || rec.Serial == "" || em.obs != 1 {
		t.Fatalf("tlscert: %s %v %d %v", out, err, em.obs, em.skipped)
	}
	if reqs.Load() != 0 {
		t.Fatal("tlscert sent an HTTP request")
	}
	// The same through the tool runner service (its own pinned re-check).
	inV, _ := structpb.NewValue([]any{"app.test", float64(port)})
	stream, err := e.st.ToolRunner.Execute(e.ctx, &pb.ExecuteRequest{Tool: "tlscert", Input: inV, ProgramId: e.prog, EffectPath: "0", IdempotencyKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	var done *pb.Done
	var obs int
	for {
		m, err := stream.Recv()
		if err != nil {
			break
		}
		if m.GetObservation() != nil {
			obs++
		}
		if m.GetError() != nil {
			t.Fatalf("tool error %v", m.GetError())
		}
		if m.GetDone() != nil {
			done = m.GetDone()
		}
	}
	if done == nil || obs != 1 || done.GetOutput().GetStructValue().GetFields()["serial_number"].GetStringValue() != rec.Serial {
		t.Fatalf("Execute tlscert: %v (%d observations)", done, obs)
	}
	if reqs.Load() != 0 {
		t.Fatal("tlscert sent an HTTP request")
	}
	// Out of scope: nothing dialled.
	out, err = toolrunner.TLSCert{}.Run(e.ctx, json.RawMessage(`["evil.test", `+ps+`]`), e.tc, em)
	if err != nil || string(out) != "null" || len(em.skipped) != 1 {
		t.Fatalf("denied: %s %v %v", out, err, em.skipped)
	}
}
