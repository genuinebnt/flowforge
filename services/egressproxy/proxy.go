// Package egressproxy is the scope-checked egress proxy (spec 3.2, 4.4,
// 4.5): an HTTP CONNECT and forward proxy that is the tool runner's only
// way out. Every request names the allow decision it relies on
// (X-Flowforge-Decision-Id); the proxy checks that the decision belongs to
// the caller's tenant, allows exactly the requested host (and, for URL
// decisions, scheme and port), is recent, and that no kill switch is
// engaged, then connects only to the decision's pinned addresses (never
// re-resolving, so DNS rebinding cannot redirect the connection). Private,
// loopback, link-local and metadata addresses are refused unless the scope
// explicitly allows them with a cidr rule. Plain-HTTP redirects are
// re-checked hop by hop; a redirect to an out-of-scope host is refused.
// Every refusal is logged and counted.
//
// The notifier's webhook and Slack deliveries take a second path
// (X-Flowforge-Egress-Purpose: notify, G-SVC-08): alert destinations are
// operator-configured, not program targets, so instead of a scope decision
// the destination host must be on NotifyAllowlist, the caller must be the
// notifier service, only CONNECT (https) is accepted, and the proxy resolves
// the host itself and connects only to addresses that pass the webhook SSRF
// checks of spec 15.4 (alerts.CheckResolved), pinned for the connection.
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
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

// Proxy is the egress proxy handler.
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
	// Now is the clock (default time.Now).
	Now func() time.Time
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

	once sync.Once
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
		if p.Now == nil {
			p.Now = time.Now
		}
	})
}

type refusal struct {
	reason, detail string
	status         int
}

func (r *refusal) Error() string { return r.reason + ": " + r.detail }

func refuse(reason, detail string) *refusal {
	return &refusal{reason: reason, detail: detail, status: http.StatusForbidden}
}

type request struct {
	id       platform.Identity
	rec      scopesvc.DecisionRecord
	scheme   string
	host     string // normalized name or IP text
	port     int
	runID    string
	path     string
	decision string
	// notify marks the notifier path; pinned are the addresses it checked.
	notify bool
	pinned []netip.Addr
}

func (p *Proxy) refused(ctx context.Context, w http.ResponseWriter, rq *request, target string, rf *refusal) {
	p.Metrics.EgressRefused.Inc(rf.reason)
	attrs := []any{"reason", rf.reason, "detail", rf.detail, "target", target}
	if rq != nil {
		ctx = platform.WithLogFields(platform.WithIdentity(ctx, rq.id), rq.runID, rq.path)
		attrs = append(attrs, "decision_id", rq.decision)
	}
	p.Logger.WarnContext(ctx, "egress.refused", attrs...)
	w.Header().Set(platform.HeaderRefused, rf.reason)
	http.Error(w, "egress refused: "+rf.reason, rf.status)
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.init()
	ctx := r.Context()
	target := r.Host
	if r.Method != http.MethodConnect {
		target = r.URL.String()
	}
	rq, rf := p.validate(ctx, r)
	if rf != nil {
		p.refused(ctx, w, rq, target, rf)
		return
	}
	conn, rf := p.dial(ctx, rq)
	if rf != nil {
		p.refused(ctx, w, rq, target, rf)
		return
	}
	if r.Method == http.MethodConnect {
		p.tunnel(w, conn)
		return
	}
	p.forward(ctx, w, r, rq, conn)
}

func (p *Proxy) validate(ctx context.Context, r *http.Request) (*request, *refusal) {
	if p.Verifier == nil {
		return nil, refuse("no_verifier", "proxy has no token verifier")
	}
	id, err := platform.IdentityFromBearer(ctx, p.Verifier, r.Header.Get(platform.HeaderProxyAuthorization))
	if err != nil {
		rf := refuse("unauthenticated", err.Error())
		rf.status = http.StatusProxyAuthRequired
		return nil, rf
	}
	rq := &request{id: id, decision: r.Header.Get(platform.HeaderDecisionID), runID: r.Header.Get(platform.HeaderRunID), path: r.Header.Get(platform.HeaderEffectPath)}
	// Where does the request want to go?
	switch {
	case r.Method == http.MethodConnect:
		h, ps, err := net.SplitHostPort(r.Host)
		if err != nil {
			return rq, refuse("invalid_target", "CONNECT authority must be host:port")
		}
		rq.host, rq.scheme = h, ""
		if rq.port, err = strconv.Atoi(ps); err != nil || rq.port < 1 || rq.port > 65535 {
			return rq, refuse("invalid_target", "bad port")
		}
	case r.URL.IsAbs():
		if r.URL.User != nil {
			return rq, refuse("invalid_target", "userinfo in URL")
		}
		if r.URL.Scheme != "http" {
			return rq, refuse("invalid_target", "only http is forwarded; use CONNECT for https")
		}
		rq.scheme, rq.host = r.URL.Scheme, r.URL.Hostname()
		rq.port = 80
		if ps := r.URL.Port(); ps != "" {
			var err error
			if rq.port, err = strconv.Atoi(ps); err != nil || rq.port < 1 || rq.port > 65535 {
				return rq, refuse("invalid_target", "bad port")
			}
		}
		if r.Host != "" && !strings.EqualFold(stripPort(r.Host), rq.host) {
			return rq, refuse("invalid_target", "Host header differs from the request URL")
		}
	default:
		return rq, refuse("invalid_target", "not a proxy request")
	}
	t, err := scope.Normalize(rq.host, scope.TargetHost)
	if err != nil {
		return rq, refuse("invalid_target", "host does not normalize")
	}
	rq.host = t.String()
	if r.Header.Get(platform.HeaderEgressPurpose) == platform.PurposeNotify {
		return rq, p.validateNotify(ctx, r, rq)
	}
	// The pinned decision.
	if rq.decision == "" {
		return rq, refuse("decision_missing", "no "+platform.HeaderDecisionID)
	}
	rec, err := p.Decisions.Decision(ctx, id.Tenant, rq.decision)
	if err != nil {
		return rq, refuse("decision_unknown", "decision not found for this tenant")
	}
	rq.rec = rec
	d := rec.Decision
	if !d.Allowed() {
		return rq, refuse("decision_not_allow", d.Reason)
	}
	if age := p.Now().Sub(d.DecidedAt); age > p.MaxDecisionAge || age < -time.Minute {
		return rq, refuse("decision_expired", "decision is too old")
	}
	if rq.runID != "" && !rec.RunID.IsZero() && rq.runID != core.PublicID(core.PrefixRun, rec.RunID) {
		return rq, refuse("decision_run_mismatch", "decision belongs to another run")
	}
	if rf := matchTarget(d.Target, rq); rf != nil {
		return rq, rf
	}
	ks, err := p.Scope.KillSwitch(ctx, id.Tenant, rec.Program)
	if err != nil {
		return rq, refuse("kill_switch_unknown", "kill switch state unavailable")
	}
	if ks.Engaged {
		return rq, refuse("kill_switch", ks.Level)
	}
	return rq, nil
}

// validateNotify checks a notifier delivery (see the package comment).
func (p *Proxy) validateNotify(ctx context.Context, r *http.Request, rq *request) *refusal {
	rq.notify = true
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
// attaches to the host name, never to an IP alone (spec 4.5).
func matchTarget(decided string, rq *request) *refusal {
	t, err := scope.Normalize(decided, scope.TargetUnspecified)
	if err != nil {
		return refuse("decision_invalid", "decision target does not normalize")
	}
	switch t.Kind {
	case scope.TargetURL:
		u, err := url.Parse(t.String())
		if err != nil {
			return refuse("decision_invalid", "decision URL")
		}
		port := 80
		if u.Scheme == "https" {
			port = 443
		}
		if ps := u.Port(); ps != "" {
			port, _ = strconv.Atoi(ps)
		}
		if !strings.EqualFold(u.Hostname(), rq.host) || port != rq.port {
			return refuse("host_mismatch", "request target differs from the decision")
		}
		if rq.scheme != "" && rq.scheme != u.Scheme {
			return refuse("host_mismatch", "scheme differs from the decision")
		}
		if rq.scheme == "" && u.Scheme != "https" {
			return refuse("host_mismatch", "CONNECT under an http decision")
		}
	case scope.TargetHost, scope.TargetIP:
		if t.String() != rq.host {
			return refuse("host_mismatch", "request host differs from the decision")
		}
	default:
		return refuse("decision_invalid", "decision target kind cannot be connected to")
	}
	return nil
}

// Forbidden reports addresses the proxy never connects to unless the scope
// explicitly allows them: unspecified, loopback, private (RFC 1918, ULA),
// link-local (including 169.254.169.254, the metadata server), multicast,
// CGNAT, 0.0.0.0/8, and invalid or zoned addresses. IPv4-mapped IPv6 is
// unmapped first. It is narrower than alerts.IsForbiddenAddr (no
// documentation or reserved ranges) because scope rules, not this list,
// decide which public targets are reachable.
func Forbidden(a netip.Addr) bool {
	a = a.Unmap()
	return !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(a) || netip.MustParsePrefix("0.0.0.0/8").Contains(a) ||
		a.Zone() != ""
}

func (p *Proxy) dial(ctx context.Context, rq *request) (net.Conn, *refusal) {
	if rq.notify {
		return p.dialPinned(ctx, rq.pinned, rq.port)
	}
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
		d := net.Dialer{Timeout: p.DialTimeout}
		conn, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(ip.Unmap(), uint16(rq.port)).String())
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

// dialPinned connects to the first reachable address of addrs (already
// checked by the caller).
func (p *Proxy) dialPinned(ctx context.Context, addrs []netip.Addr, port int) (net.Conn, *refusal) {
	var lastErr error
	for _, ip := range addrs {
		d := net.Dialer{Timeout: p.DialTimeout}
		conn, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(ip.Unmap(), uint16(port)).String())
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	rf := refuse("connect_failed", fmt.Sprint(lastErr))
	rf.status = http.StatusBadGateway
	return nil, rf
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
	go func() {
		if buf.Reader.Buffered() > 0 {
			b := make([]byte, buf.Reader.Buffered())
			n, _ := buf.Read(b)
			_, _ = upstream.Write(b[:n])
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
func (p *Proxy) checkRedirect(ctx context.Context, rq *request, loc *url.URL) (string, *refusal) {
	if loc.User != nil {
		return "", refuse("redirect_denied", "userinfo in Location")
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
