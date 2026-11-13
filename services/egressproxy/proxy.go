// Package egressproxy is the scope-checked egress proxy (spec 3.2, 4.4,
// 4.5; V2_DESIGN §9): the tool runner's only way out. It serves an HTTP
// CONNECT and forward proxy, a SOCKS5 listener (CONNECT only) and a DNS
// forwarder (UDP and TCP).
//
// Every connection names the allow decision it relies on (the
// X-Flowforge-Decision-Id header, or a ticket issued for it). The proxy
// checks that the decision belongs to the caller's tenant, is recent and an
// allow, covers exactly the requested (host, port) and that no kill switch
// is engaged, then connects only to the decision's pinned addresses (never
// re-resolving, so DNS rebinding cannot redirect the connection). Private,
// loopback, link-local and metadata addresses are refused unless the scope
// explicitly allows them with a cidr rule. Plain-HTTP redirects are
// re-checked hop by hop; a redirect to an out-of-scope host is refused.
//
// A URL decision covers its URL's host and port. A host-level decision
// (Host or IP) covers its host for TCP to the ports of the program's
// PortPolicy, narrowed to the ports the tool call declared (HeaderPorts or
// the ticket); anything else is refused (port_not_allowed).
//
// Passive decisions may also reach the intelligence-source allowlist
// (crt.sh, RDAP, Team Cymru): the source, not a program asset, is the
// target; each such connection is recorded as a decision with reason
// intel_source.
//
// Every request and tunnel is charged against the per-target limit
// (rl:h:<program>:<registrable domain or ip>, GCRA at the scope rate, plus
// a concurrency cap); denials answer 429 (SOCKS: "connection not allowed by
// ruleset"). Every refusal is logged and counted.
//
// The notifier's webhook and Slack deliveries take a second path
// (X-Flowforge-Egress-Purpose: notify, G-SVC-08): alert destinations are
// operator-configured, not program targets, so instead of a scope decision
// the destination host must be on NotifyAllowlist, the caller must be the
// notifier service, only CONNECT (https) is accepted, and the proxy resolves
// the host itself and connects only to addresses that pass the webhook SSRF
// checks of spec 15.4 (alerts.CheckResolved), pinned for the connection.
// The per-target limit does not apply to it: it is keyed by program, and
// the notifier throttles its own deliveries (spec 15.3).
package egressproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
)

// Decisions looks up recorded scope decisions (scopesvc.DecisionLog).
type Decisions interface {
	Decision(ctx context.Context, tenant core.UUID, id string) (scopesvc.DecisionRecord, error)
}

// Scope is what the proxy asks the scope service (scopesvc.Client).
type Scope interface {
	CheckTarget(ctx context.Context, t scopesvc.Target) (scope.Decision, error)
	KillSwitch(ctx context.Context, tenant, program core.UUID) (engine.KillSwitchState, error)
	Policy(ctx context.Context, tenant, program core.UUID, version int32) (*scope.Policy, error)
}

// Recorder appends decisions the proxy makes itself (intel_source) to the
// decision log (scopesvc.Repo implements it).
type Recorder interface {
	RecordDecisions(ctx context.Context, ds []scopesvc.DecisionRecord) error
}

// Proxy is the egress proxy. ServeHTTP is the HTTP proxy; ServeSOCKS,
// ServeDNSUDP and ServeDNSTCP serve the other listeners.
type Proxy struct {
	Decisions Decisions
	Scope     Scope
	Verifier  platform.TokenVerifier
	Logger    *slog.Logger
	Metrics   *platform.Metrics
	// MaxDecisionAge bounds how old a pinned decision may be (default 1 h).
	MaxDecisionAge time.Duration
	// DialTimeout bounds each connection attempt (default 10 s).
	DialTimeout time.Duration
	// Now is the wall clock (default Clock.Now).
	Now func() time.Time
	// Clock drives the per-target limiter (monotonic readings) and decision
	// ids (default core.SystemClock). Tests inject a fake clock.
	Clock core.Clock
	// NotifyAllowlist are the destination hosts of the notify path: an
	// exact host name ("hooks.slack.com") or a suffix entry starting with a
	// dot (".example.com" matches every name below example.com, not
	// example.com itself). Empty refuses every notify request (fail
	// closed).
	NotifyAllowlist []string
	// Resolver resolves notify destinations (the decision path never
	// resolves: it uses the decision's pinned addresses).
	Resolver scope.Resolver
	// CheckNotifyAddrs vets the resolved notify addresses; nil means
	// alerts.CheckResolved (public unicast only). Tests replace it to reach
	// a loopback listener.
	CheckNotifyAddrs func([]netip.Addr) error

	// Ports is the TCP port policy for host-level decisions (nil:
	// DefaultPorts, 80 and 443).
	Ports *PortPolicy
	// Intel is the intelligence-source allowlist (nil: none).
	Intel []IntelSource
	// IntelResolver resolves intel sources without pinned Addrs.
	IntelResolver scope.Resolver
	// Recorder records intel_source decisions (nil: logged only).
	Recorder Recorder
	// Limiter is the per-target limiter (default an in-process GCRA).
	Limiter limiter.Limiter

	// TicketTTL bounds ticket lifetimes (default 60 s).
	TicketTTL time.Duration
	// SOCKSAddr and DNSAddr are advertised in tickets.
	SOCKSAddr, DNSAddr string
	// Upstream answers forwarded DNS queries (nil: every query SERVFAIL).
	Upstream Upstream
	// MaxUDPResponse caps UDP DNS answers (default 1232 octets, the DNS
	// Flag Day 2020 size that avoids IP fragmentation);
	// MaxTCPResponse caps TCP answers (default 16 KiB).
	MaxUDPResponse, MaxTCPResponse int

	once    sync.Once
	tickets tickets
	limits  hostLimits
}

func (p *Proxy) init() {
	p.once.Do(func() {
		if p.Logger == nil {
			p.Logger = platform.DiscardLogger()
		}
		if p.Metrics == nil {
			p.Metrics = platform.NewMetrics(nil)
		}
		if p.MaxDecisionAge <= 0 {
			p.MaxDecisionAge = time.Hour
		}
		if p.DialTimeout <= 0 {
			p.DialTimeout = 10 * time.Second
		}
		if p.Clock == nil {
			p.Clock = &core.SystemClock{}
		}
		if p.Now == nil {
			p.Now = p.Clock.Now
		}
		if p.Limiter == nil {
			g, _ := limiter.NewGCRA()
			p.Limiter = g
		}
		if p.TicketTTL <= 0 {
			p.TicketTTL = time.Minute
		}
		if p.MaxUDPResponse <= 0 {
			p.MaxUDPResponse = 1232
		}
		if p.MaxTCPResponse <= 0 {
			p.MaxTCPResponse = 16 << 10
		}
	})
}

type refusal struct {
	reason, detail string
	status         int
	retryAfter     time.Duration
}

func (r *refusal) Error() string { return r.reason + ": " + r.detail }

func refuse(reason, detail string) *refusal {
	return &refusal{reason: reason, detail: detail, status: http.StatusForbidden}
}

type request struct {
	id       platform.Identity
	rec      scopesvc.DecisionRecord
	scheme   string // "" for tunnels
	host     string // normalized name or IP text
	port     int
	runID    string
	path     string
	decision string
	ports    []int        // declared ports (nil: none declared)
	intel    *IntelSource // set when the target is an allowlisted source
	via      string       // "http", "connect", "socks5"
	// notify marks the notifier path; pinned are the addresses it checked.
	notify bool
	pinned []netip.Addr
}

// logRefusal logs and counts a refusal.
func (p *Proxy) logRefusal(ctx context.Context, rq *request, target string, rf *refusal) {
	p.Metrics.EgressRefused.Inc(rf.reason)
	attrs := []any{"reason", rf.reason, "detail", rf.detail, "target", target}
	if rq != nil {
		ctx = platform.WithLogFields(platform.WithIdentity(ctx, rq.id), rq.runID, rq.path)
		attrs = append(attrs, "decision_id", rq.decision, "via", rq.via)
	}
	p.Logger.WarnContext(ctx, "egress.refused", attrs...)
}

func (p *Proxy) refused(ctx context.Context, w http.ResponseWriter, rq *request, target string, rf *refusal) {
	p.logRefusal(ctx, rq, target, rf)
	w.Header().Set(platform.HeaderRefused, rf.reason)
	if rf.status == http.StatusTooManyRequests {
		setRetry(w.Header(), rf.retryAfter)
	}
	http.Error(w, "egress refused: "+rf.reason, rf.status)
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.init()
	ctx := r.Context()
	if r.Method != http.MethodConnect && r.URL.IsAbs() && strings.EqualFold(r.URL.Hostname(), ControlHost) {
		p.serveTicket(w, r)
		return
	}
	target := r.Host
	if r.Method != http.MethodConnect {
		target = r.URL.String()
	}
	rq, rf := p.identify(ctx, r.Header)
	if rf == nil {
		rf = parseHTTPTarget(r, rq)
	}
	if rf == nil {
		if r.Header.Get(platform.HeaderEgressPurpose) == platform.PurposeNotify {
			rf = p.validateNotify(ctx, r, rq)
		} else {
			rf = p.authorize(ctx, rq)
		}
	}
	if rf != nil {
		p.refused(ctx, w, rq, target, rf)
		return
	}
	conn, release, rf := p.connect(ctx, rq)
	if rf != nil {
		p.refused(ctx, w, rq, target, rf)
		return
	}
	defer release()
	if r.Method == http.MethodConnect {
		p.tunnel(w, conn)
		return
	}
	p.forward(ctx, w, r, rq, conn)
}

// identify authenticates the token and reads the request's decision, run,
// effect path and declared ports.
func (p *Proxy) identify(ctx context.Context, h http.Header) (*request, *refusal) {
	p.init()
	if p.Verifier == nil {
		return nil, refuse("no_verifier", "proxy has no token verifier")
	}
	id, err := platform.IdentityFromBearer(ctx, p.Verifier, h.Get(platform.HeaderProxyAuthorization))
	if err != nil {
		rf := refuse("unauthenticated", err.Error())
		rf.status = http.StatusProxyAuthRequired
		return nil, rf
	}
	rq := &request{id: id, decision: h.Get(platform.HeaderDecisionID), runID: h.Get(platform.HeaderRunID), path: h.Get(platform.HeaderEffectPath)}
	ports, ok := ParsePorts(h.Get(HeaderPorts))
	if !ok {
		return rq, refuse("invalid_target", "malformed "+HeaderPorts)
	}
	rq.ports = ports
	return rq, nil
}

func parseHTTPTarget(r *http.Request, rq *request) *refusal {
	switch {
	case r.Method == http.MethodConnect:
		rq.via = "connect"
		h, ps, err := net.SplitHostPort(r.Host)
		if err != nil {
			return refuse("invalid_target", "CONNECT authority must be host:port")
		}
		rq.host, rq.scheme = h, ""
		if rq.port, err = strconv.Atoi(ps); err != nil || rq.port < 1 || rq.port > 65535 {
			return refuse("invalid_target", "bad port")
		}
	case r.URL.IsAbs():
		rq.via = "http"
		if r.URL.User != nil {
			return refuse("invalid_target", "userinfo in URL")
		}
		if r.URL.Scheme != "http" {
			return refuse("invalid_target", "only http is forwarded; use CONNECT for https")
		}
		rq.scheme, rq.host = r.URL.Scheme, r.URL.Hostname()
		rq.port = 80
		if ps := r.URL.Port(); ps != "" {
			var err error
			if rq.port, err = strconv.Atoi(ps); err != nil || rq.port < 1 || rq.port > 65535 {
				return refuse("invalid_target", "bad port")
			}
		}
		if r.Host != "" && !strings.EqualFold(stripPort(r.Host), rq.host) {
			return refuse("invalid_target", "Host header differs from the request URL")
		}
	default:
		return refuse("invalid_target", "not a proxy request")
	}
	return nil
}

// normalizeHost replaces rq.host by its normalized form (scope 4.5), so
// matching and allowlists compare canonical names.
func normalizeHost(rq *request) *refusal {
	t, err := scope.Normalize(rq.host, scope.TargetHost)
	if err != nil {
		return refuse("invalid_target", "host does not normalize")
	}
	rq.host = t.String()
	return nil
}

// loadDecision loads the pinned decision for the caller's tenant and checks
// that it is an allow, recent, and of the request's run.
func (p *Proxy) loadDecision(ctx context.Context, rq *request) *refusal {
	if rq.decision == "" {
		return refuse("decision_missing", "no "+platform.HeaderDecisionID)
	}
	rec, err := p.Decisions.Decision(ctx, rq.id.Tenant, rq.decision)
	if err != nil {
		return refuse("decision_unknown", "decision not found for this tenant")
	}
	rq.rec = rec
	d := rec.Decision
	if !d.Allowed() {
		return refuse("decision_not_allow", d.Reason)
	}
	if age := p.Now().Sub(d.DecidedAt); age > p.MaxDecisionAge || age < -time.Minute {
		return refuse("decision_expired", "decision is too old")
	}
	if rq.runID != "" && !rec.RunID.IsZero() && rq.runID != core.PublicID(core.PrefixRun, rec.RunID) {
		return refuse("decision_run_mismatch", "decision belongs to another run")
	}
	return nil
}

func (p *Proxy) killSwitch(ctx context.Context, rq *request) *refusal {
	ks, err := p.Scope.KillSwitch(ctx, rq.id.Tenant, rq.rec.Program)
	if err != nil {
		return refuse("kill_switch_unknown", "kill switch state unavailable")
	}
	if ks.Engaged {
		return refuse("kill_switch", ks.Level)
	}
	return nil
}

// authorize checks that rq's pinned decision covers (host, port), or that
// the target is an intelligence source reachable by the decision's class.
func (p *Proxy) authorize(ctx context.Context, rq *request) *refusal {
	if rf := normalizeHost(rq); rf != nil {
		return rf
	}
	if rf := p.loadDecision(ctx, rq); rf != nil {
		return rf
	}
	hostLevel, rf := matchTarget(rq.rec.Decision.Target, rq)
	switch {
	case rf != nil:
		src := p.intelFor(rq.host)
		if src == nil {
			return rf
		}
		if rq.rec.Class != core.ClassPassive {
			return refuse("intel_requires_passive", "intelligence sources are reachable only by passive tools")
		}
		if !src.port(rq.port) || (rq.ports != nil && !slices.Contains(rq.ports, rq.port)) {
			return refuse("port_not_allowed", "port not allowed for this intelligence source")
		}
		rq.intel = src
	case hostLevel:
		if !p.Ports.Allowed(rq.rec.Program.String(), rq.port) {
			return refuse("port_not_allowed", "port "+strconv.Itoa(rq.port)+" is not in the program's port policy")
		}
		if rq.ports != nil && !slices.Contains(rq.ports, rq.port) {
			return refuse("port_not_allowed", "port "+strconv.Itoa(rq.port)+" was not declared by the tool call")
		}
	}
	return p.killSwitch(ctx, rq)
}

func (p *Proxy) intelFor(host string) *IntelSource {
	for i := range p.Intel {
		if p.Intel[i].matches(host) {
			return &p.Intel[i]
		}
	}
	return nil
}

// validateNotify checks a notifier delivery (see the package comment).
func (p *Proxy) validateNotify(ctx context.Context, r *http.Request, rq *request) *refusal {
	rq.notify = true
	if rf := normalizeHost(rq); rf != nil {
		return rf
	}
	if rq.id.Actor != platform.ServiceActor("notifier") {
		return refuse("notify_not_notifier", "the notify path is for the notifier service")
	}
	if r.Method != http.MethodConnect {
		return refuse("notify_https_only", "notify deliveries are https (CONNECT)")
	}
	if !notifyAllowed(p.NotifyAllowlist, rq.host) {
		return refuse("notify_not_allowlisted", "destination host is not on the notify allowlist")
	}
	if a, err := netip.ParseAddr(rq.host); err == nil {
		rq.pinned = []netip.Addr{a}
	} else {
		if p.Resolver == nil {
			return refuse("notify_no_resolver", "proxy cannot resolve notify destinations")
		}
		addrs, err := p.Resolver.LookupAddrs(ctx, rq.host)
		if err != nil {
			return refuse("notify_resolve_failed", err.Error())
		}
		rq.pinned = addrs
	}
	// Every address must pass: a name with one internal address could be
	// steered there by whichever answer the dialer picks.
	check := p.CheckNotifyAddrs
	if check == nil {
		check = alerts.CheckResolved
	}
	if err := check(rq.pinned); err != nil {
		return refuse("forbidden_address", err.Error())
	}
	return nil
}

// notifyAllowed matches host against the allowlist entries (exact names,
// or ".suffix" entries for the names below a domain).
func notifyAllowed(list []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, e := range list {
		e = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(e, ".")))
		switch {
		case e == "":
		case strings.HasPrefix(e, "."):
			if strings.HasSuffix(host, e) && len(host) > len(e) {
				return true
			}
		case host == e:
			return true
		}
	}
	return false
}

func stripPort(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

// matchTarget checks that the decision's normalized target covers the
// requested host (and scheme and port for URL decisions). Permission
// attaches to the host name, never to an IP alone (spec 4.5). hostLevel
// reports a Host or IP decision, whose port the caller checks against the
// port policy.
func matchTarget(decided string, rq *request) (hostLevel bool, rf *refusal) {
	t, err := scope.Normalize(decided, scope.TargetUnspecified)
	if err != nil {
		return false, refuse("decision_invalid", "decision target does not normalize")
	}
	switch t.Kind {
	case scope.TargetURL:
		u, err := url.Parse(t.String())
		if err != nil {
			return false, refuse("decision_invalid", "decision URL")
		}
		port := 80
		if u.Scheme == "https" {
			port = 443
		}
		if ps := u.Port(); ps != "" {
			port, _ = strconv.Atoi(ps)
		}
		if !strings.EqualFold(u.Hostname(), rq.host) || port != rq.port {
			return false, refuse("host_mismatch", "request target differs from the decision")
		}
		if rq.scheme != "" && rq.scheme != u.Scheme {
			return false, refuse("host_mismatch", "scheme differs from the decision")
		}
		if rq.scheme == "" && u.Scheme != "https" {
			return false, refuse("host_mismatch", "tunnel under an http decision")
		}
		return false, nil
	case scope.TargetHost, scope.TargetIP:
		if t.String() != rq.host {
			return false, refuse("host_mismatch", "request host differs from the decision")
		}
		return true, nil
	}
	return false, refuse("decision_invalid", "decision target kind cannot be connected to")
}

// Forbidden reports addresses the proxy never connects to unless the scope
// explicitly allows them: unspecified, loopback, private (RFC 1918, ULA),
// link-local (including 169.254.169.254, the metadata server), multicast,
// CGNAT and 0.0.0.0/8.
func Forbidden(a netip.Addr) bool {
	a = a.Unmap()
	return !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(a) || netip.MustParsePrefix("0.0.0.0/8").Contains(a) ||
		a.Zone() != ""
}

// connect admits rq against the per-target limit and dials. The release
// function must be called when the request or tunnel ends. A notify
// request has no program to key a limit on and dials its checked
// addresses.
func (p *Proxy) connect(ctx context.Context, rq *request) (net.Conn, func(), *refusal) {
	if rq.notify {
		conn, rf := p.dialPinned(ctx, rq.pinned, rq.port)
		if rf != nil {
			return nil, nil, rf
		}
		return conn, func() {}, nil
	}
	release, rf := p.admit(ctx, rq)
	if rf != nil {
		return nil, nil, rf
	}
	var conn net.Conn
	if rq.intel != nil {
		conn, rf = p.dialIntel(ctx, rq)
	} else {
		conn, rf = p.dial(ctx, rq)
	}
	if rf != nil {
		release()
		return nil, nil, rf
	}
	return conn, release, nil
}

func (p *Proxy) dial(ctx context.Context, rq *request) (net.Conn, *refusal) {
	ips := rq.rec.Decision.PinnedIPs
	if len(ips) == 0 {
		return nil, refuse("no_pinned_ips", "the decision pins no addresses")
	}
	var pol *scope.Policy
	var lastErr error
	allowed := 0
	for _, ip := range ips {
		if Forbidden(ip) {
			if pol == nil {
				var err error
				if pol, err = p.Scope.Policy(ctx, rq.id.Tenant, rq.rec.Program, rq.rec.ScopeVersion); err != nil {
					return nil, refuse("policy_unavailable", "cannot verify a restricted address")
				}
			}
			if pol.Status(ip.String(), scope.TargetIP) != scope.StatusInScope {
				continue
			}
		}
		allowed++
		conn, err := p.dialAddr(ctx, ip, rq.port)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if allowed == 0 {
		return nil, refuse("forbidden_address", "every pinned address is internal and not explicitly in scope")
	}
	rf := refuse("connect_failed", lastErr.Error())
	rf.status = http.StatusBadGateway
	return nil, rf
}

func (p *Proxy) dialAddr(ctx context.Context, ip netip.Addr, port int) (net.Conn, error) {
	d := net.Dialer{Timeout: p.DialTimeout}
	return d.DialContext(ctx, "tcp", netip.AddrPortFrom(ip.Unmap(), uint16(port)).String())
}

// dialIntel connects to an intelligence source: its configured addresses,
// or the resolved ones (internal addresses refused), recorded as a
// decision with reason intel_source.
func (p *Proxy) dialIntel(ctx context.Context, rq *request) (net.Conn, *refusal) {
	addrs := rq.intel.Addrs
	if len(addrs) == 0 {
		if p.IntelResolver == nil {
			return nil, refuse("intel_unresolvable", "no resolver for intelligence sources")
		}
		res, err := p.IntelResolver.LookupAddrs(ctx, rq.host)
		if err != nil {
			rf := refuse("connect_failed", "resolving the intelligence source failed")
			rf.status = http.StatusBadGateway
			return nil, rf
		}
		for _, a := range res {
			if !Forbidden(a) {
				addrs = append(addrs, a.Unmap())
			}
		}
		if len(addrs) == 0 {
			return nil, refuse("forbidden_address", "the intelligence source resolves only to internal addresses")
		}
	}
	if rf := p.recordIntel(ctx, rq, rq.host, addrs); rf != nil {
		return nil, rf
	}
	return p.dialPinned(ctx, addrs, rq.port)
}

// dialPinned connects to the first reachable address of addrs (already
// checked by the caller).
func (p *Proxy) dialPinned(ctx context.Context, addrs []netip.Addr, port int) (net.Conn, *refusal) {
	var lastErr error
	for _, ip := range addrs {
		conn, err := p.dialAddr(ctx, ip, port)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	rf := refuse("connect_failed", fmt.Sprint(lastErr)) // lastErr is nil when addrs is empty
	rf.status = http.StatusBadGateway
	return nil, rf
}

// recordIntel records and logs an intel_source decision for host.
func (p *Proxy) recordIntel(ctx context.Context, rq *request, host string, addrs []netip.Addr) *refusal {
	uid := core.NewUUIDv7(p.Clock, core.SystemRng{})
	d := scope.Decision{ID: core.PublicID(core.PrefixDecision, uid), Result: scope.ResultAllow, Reason: ReasonIntelSource,
		Target: host, PinnedIPs: slices.Clone(addrs), DecidedAt: p.Now().UTC()}
	if d.PinnedIPs == nil {
		d.PinnedIPs = []netip.Addr{}
	}
	rec := scopesvc.DecisionRecord{ID: uid, Tenant: rq.id.Tenant, Program: rq.rec.Program, ScopeVersion: rq.rec.ScopeVersion,
		RunID: rq.rec.RunID, EffectPath: rq.rec.EffectPath, Class: core.ClassPassive, Decision: d}
	if p.Recorder != nil {
		if err := p.Recorder.RecordDecisions(ctx, []scopesvc.DecisionRecord{rec}); err != nil {
			return refuse("decision_log_unavailable", "cannot record the intel_source decision")
		}
	}
	p.Metrics.ScopeDecisions.Inc("allow", ReasonIntelSource)
	lctx := platform.WithLogFields(platform.WithIdentity(ctx, rq.id), rq.runID, rq.path)
	p.Logger.InfoContext(lctx, "egress.intel_source", "target", host, "decision_id", d.ID, "via_decision", rq.decision, "via", rq.via)
	return nil
}

func (p *Proxy) tunnel(w http.ResponseWriter, upstream net.Conn) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	var pre []byte
	if n := buf.Reader.Buffered(); n > 0 {
		pre = make([]byte, n)
		n, _ = buf.Read(pre)
		pre = pre[:n]
	}
	relay(client, upstream, pre)
}

// relay copies both ways until either side is done, then closes both.
func relay(client, upstream net.Conn, pre []byte) {
	go func() {
		if len(pre) > 0 {
			_, _ = upstream.Write(pre)
		}
		_, _ = io.Copy(upstream, client)
		closeWrite(upstream)
	}()
	_, _ = io.Copy(client, upstream)
	client.Close()
	upstream.Close()
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
		return
	}
	c.Close()
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *Proxy) forward(ctx context.Context, w http.ResponseWriter, r *http.Request, rq *request, conn net.Conn) {
	used := false
	tr := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if used {
				return nil, errors.New("egress: one connection per request")
			}
			used = true
			return conn, nil
		},
		DisableKeepAlives: true, ResponseHeaderTimeout: 30 * time.Second,
	}
	out := r.Clone(ctx)
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	for k := range out.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-flowforge-") {
			out.Header.Del(k)
		}
	}
	resp, err := tr.RoundTrip(out)
	if err != nil {
		rf := refuse("upstream_failed", err.Error())
		rf.status = http.StatusBadGateway
		p.refused(ctx, w, rq, r.URL.String(), rf)
		return
	}
	defer resp.Body.Close()
	if loc, err := resp.Location(); err == nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		next, rf := p.checkRedirect(ctx, rq, loc)
		if rf != nil {
			p.refused(ctx, w, rq, loc.String(), rf)
			return
		}
		w.Header().Set(platform.HeaderRedirectDecision, next)
	}
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// checkRedirect re-runs the scope check on a redirect hop (spec 4.5). A hop
// to another origin needs its own allow decision; its id is returned in
// X-Flowforge-Redirect-Decision so the client can follow through the proxy.
// A hop of an intelligence-source request must stay on the allowlist and
// keeps the original (passive) decision.
func (p *Proxy) checkRedirect(ctx context.Context, rq *request, loc *url.URL) (string, *refusal) {
	if loc.User != nil {
		return "", refuse("redirect_denied", "userinfo in Location")
	}
	if rq.intel != nil {
		t, err := scope.Normalize(loc.Hostname(), scope.TargetHost)
		if err != nil || p.intelFor(t.String()) == nil {
			return "", refuse("redirect_denied", "redirect leaves the intelligence-source allowlist")
		}
		return rq.decision, nil
	}
	d, err := p.Scope.CheckTarget(ctx, scopesvc.Target{Tenant: rq.id.Tenant, Program: rq.rec.Program, Target: loc.String(),
		Kind: scope.TargetURL, Class: max(rq.rec.Class, core.ClassActive), Run: rq.rec.RunID, Path: rq.rec.EffectPath, Resolve: true})
	if err != nil {
		return "", refuse("redirect_denied", "scope check failed")
	}
	if !d.Allowed() {
		return "", refuse("redirect_denied", d.Reason)
	}
	return d.ID, nil
}
