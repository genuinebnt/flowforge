package toolrunner

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dnsmsg"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/egressproxy"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/socks5"
)

// Network is what adapters use to reach anything: the egress proxy in
// production (*Egress), recorded fixtures in tests. Every method takes the
// allow decision the connection relies on.
type Network interface {
	// HTTPClient returns a client for one program target (redirects are
	// re-checked with the scope service, spec 4.5).
	HTTPClient(tc *ToolCtx, d scope.Decision, skip func(target, reason string)) *http.Client
	// IntelClient returns a client for intelligence sources under the
	// passive decision d (redirects stay under d; the proxy keeps them on
	// the allowlist).
	IntelClient(tc *ToolCtx, d scope.Decision) *http.Client
	// DialTCP opens a TCP connection to host:port, declaring the ports the
	// tool call may use.
	DialTCP(ctx context.Context, tc *ToolCtx, d scope.Decision, host string, port int, declared []int) (net.Conn, error)
	// LookupDNS resolves (name, t) through the proxy's DNS forwarder.
	LookupDNS(ctx context.Context, tc *ToolCtx, d scope.Decision, name string, t dnsmsg.Type) (egressproxy.DNSAnswer, error)
}

// Egress is the tool runner's only route to targets (spec 3.2, 4.4): the
// scope-checked egress proxy. Every request names the allow decision it
// relies on; the proxy connects only to that decision's pinned addresses.
type Egress struct {
	Proxy  *url.URL
	Tokens platform.TokenSource
	// Service is the identity the runner presents to the proxy.
	Service string
	// IDTokens adds a Google ID token (X-Serverless-Authorization) when
	// the proxy is an https:// Cloud Run URL (spec 17.3; G-SVC-13).
	IDTokens platform.IDTokenSource
	// TLS overrides the TLS configuration towards an https:// proxy
	// (tests); nil means the system roots.
	TLS *tls.Config
	// SOCKS makes DialTCP use the proxy's SOCKS5 listener instead of
	// CONNECT.
	SOCKS bool

	mu      sync.Mutex
	tickets map[string]cachedTicket
}

type cachedTicket struct {
	t     egressproxy.TicketResponse
	until time.Time
}

var _ Network = (*Egress)(nil)

// NewEgress parses the proxy address (host:port, http://host:port or
// https://host[:port], the last spoken to over TLS).
func NewEgress(addr string, tokens platform.TokenSource) (*Egress, error) {
	if addr == "" {
		return nil, nil
	}
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		u, err = url.Parse("http://" + addr)
	}
	if err != nil {
		return nil, fmt.Errorf("egress proxy address %q: %w", addr, err)
	}
	return &Egress{Proxy: u, Tokens: tokens, Service: "tool-runner"}, nil
}

type decisionHolder struct {
	mu sync.Mutex
	id string
}

func (h *decisionHolder) get() string { h.mu.Lock(); defer h.mu.Unlock(); return h.id }
func (h *decisionHolder) set(id string) {
	h.mu.Lock()
	h.id = id
	h.mu.Unlock()
}

func (e *Egress) headers(ctx context.Context, tc *ToolCtx, decision string) (http.Header, error) {
	h := http.Header{}
	if e.Tokens != nil {
		tok, err := e.Tokens.Token(ctx, platform.Identity{Tenant: tc.Tenant, Actor: platform.ServiceActor(e.Service), Role: auth.RoleAnalyst})
		if err != nil {
			return nil, err
		}
		h.Set(platform.HeaderProxyAuthorization, "Bearer "+tok)
	}
	if e.Proxy.Scheme == "https" && e.IDTokens != nil {
		tok, err := e.IDTokens.IDToken(ctx, "https://"+e.Proxy.Hostname())
		if err != nil {
			return nil, err
		}
		h.Set(platform.MDServerlessAuthorization, "Bearer "+tok)
	}
	h.Set(platform.HeaderDecisionID, decision)
	h.Set(platform.HeaderRunID, core.PublicID(core.PrefixRun, tc.Run))
	h.Set(platform.HeaderEffectPath, tc.Path)
	return h, nil
}

func (e *Egress) transport(tc *ToolCtx, holder *decisionHolder) http.RoundTripper {
	tr := &http.Transport{
		Proxy:             http.ProxyURL(e.Proxy),
		DisableKeepAlives: true,
		GetProxyConnectHeader: func(ctx context.Context, _ *url.URL, _ string) (http.Header, error) {
			return e.headers(ctx, tc, holder.get())
		},
		TLSClientConfig:       e.TLS, // used for the proxy hop when it is https://
		ResponseHeaderTimeout: 30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
	}
	return roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme == "http" {
			h, err := e.headers(r.Context(), tc, holder.get())
			if err != nil {
				return nil, err
			}
			r = r.Clone(r.Context())
			for k, v := range h {
				r.Header[k] = v
			}
		}
		return tr.RoundTrip(r)
	})
}

// HTTPClient returns a client for one target whose allow decision is d.
// Every request goes through the proxy (plain HTTP as a forward proxy,
// HTTPS through CONNECT) with d's id; connections are never reused across
// hops. Redirects are followed only after a fresh scope check of the
// Location (class active), whose decision the next hop then carries; a
// denied hop is reported through skip and not followed (spec 4.5).
func (e *Egress) HTTPClient(tc *ToolCtx, d scope.Decision, skip func(target, reason string)) *http.Client {
	holder := &decisionHolder{id: d.ID}
	return &http.Client{
		Transport: e.transport(tc, holder),
		Timeout:   60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			target := req.URL.String()
			nd, err := tc.Check(req.Context(), target, scope.TargetURL, core.ClassActive)
			if err != nil || !nd.Allowed() {
				reason := "redirect_denied"
				if err == nil {
					reason += ":" + nd.Reason
				}
				skip(target, reason)
				return http.ErrUseLastResponse
			}
			holder.set(nd.ID)
			return nil
		},
	}
}

// IntelClient implements Network: every hop carries d; the proxy refuses
// hops that leave the intelligence-source allowlist.
func (e *Egress) IntelClient(tc *ToolCtx, d scope.Decision) *http.Client {
	return IntelHTTPClient(e.transport(tc, &decisionHolder{id: d.ID}))
}

// IntelHTTPClient wraps rt with the redirect policy of intelligence-source
// requests: at most 5 hops, http(s) only.
func IntelHTTPClient(rt http.RoundTripper) *http.Client {
	return &http.Client{Transport: rt, Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || (req.URL.Scheme != "https" && req.URL.Scheme != "http") {
				return http.ErrUseLastResponse
			}
			return nil
		}}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ErrRefused reports a connection the proxy refused.
var ErrRefused = errors.New("egress proxy refused the connection")

// ErrNoDNS reports that the proxy serves no DNS forwarder.
var ErrNoDNS = errors.New("egress proxy has no DNS forwarder")

// RefusedError is a refusal with the proxy's reason; RetryAfter is set for
// rate-limit refusals. errors.Is(err, ErrRefused) holds.
type RefusedError struct {
	Reason     string
	Status     string
	RetryAfter time.Duration
}

func (e *RefusedError) Error() string {
	return ErrRefused.Error() + ": " + e.Status + " (" + e.Reason + ")"
}

// Unwrap returns ErrRefused.
func (e *RefusedError) Unwrap() error { return ErrRefused }

// Dial opens a TCP connection to hostport through the proxy (CONNECT) under
// decision d, declaring no ports.
func (e *Egress) Dial(ctx context.Context, tc *ToolCtx, d scope.Decision, hostport string) (net.Conn, error) {
	return e.connect(ctx, tc, d, hostport, nil)
}

// DialTCP implements Network: CONNECT (or SOCKS5 when e.SOCKS) to host:port
// declaring the tool call's ports.
func (e *Egress) DialTCP(ctx context.Context, tc *ToolCtx, d scope.Decision, host string, port int, declared []int) (net.Conn, error) {
	if e.SOCKS {
		return e.DialSOCKS(ctx, tc, d, host, port, declared)
	}
	return e.connect(ctx, tc, d, net.JoinHostPort(host, strconv.Itoa(port)), declared)
}

func (e *Egress) connect(ctx context.Context, tc *ToolCtx, d scope.Decision, hostport string, declared []int) (net.Conn, error) {
	var dl net.Dialer
	addr := e.Proxy.Host
	if e.Proxy.Port() == "" {
		port := "80"
		if e.Proxy.Scheme == "https" {
			port = "443" // Cloud Run serves TLS on 443 only
		}
		addr = net.JoinHostPort(e.Proxy.Hostname(), port)
	}
	conn, err := dl.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if e.Proxy.Scheme == "https" {
		conf := &tls.Config{MinVersion: tls.VersionTLS12}
		if e.TLS != nil {
			conf = e.TLS.Clone()
		}
		if conf.ServerName == "" {
			conf.ServerName = e.Proxy.Hostname()
		}
		tlsConn := tls.Client(conn, conf)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tlsConn
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	h, err := e.headers(ctx, tc, d.ID)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if declared != nil {
		h.Set(egressproxy.HeaderPorts, egressproxy.FormatPorts(declared))
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: hostport}, Host: hostport, Header: h}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, refusedFrom(resp)
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

func refusedFrom(resp *http.Response) *RefusedError {
	re := &RefusedError{Reason: resp.Header.Get(platform.HeaderRefused), Status: resp.Status}
	if ms, err := strconv.ParseInt(resp.Header.Get(egressproxy.HeaderRetryAfterMs), 10, 64); err == nil && ms > 0 {
		re.RetryAfter = time.Duration(ms) * time.Millisecond
	}
	return re
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// Ticket returns a ticket for decision d and the declared ports (cached
// until shortly before it expires).
func (e *Egress) Ticket(ctx context.Context, tc *ToolCtx, d scope.Decision, declared []int) (egressproxy.TicketResponse, error) {
	key := d.ID + "|" + egressproxy.FormatPorts(declared) + "|" + tc.Tenant.String()
	now := time.Now()
	e.mu.Lock()
	if c, ok := e.tickets[key]; ok && now.Before(c.until) {
		e.mu.Unlock()
		return c.t, nil
	}
	e.mu.Unlock()
	h, err := e.headers(ctx, tc, d.ID)
	if err != nil {
		return egressproxy.TicketResponse{}, err
	}
	if declared != nil {
		h.Set(egressproxy.HeaderPorts, egressproxy.FormatPorts(declared))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+egressproxy.ControlHost+egressproxy.TicketPath, nil)
	if err != nil {
		return egressproxy.TicketResponse{}, err
	}
	req.Header = h
	tr := &http.Transport{Proxy: http.ProxyURL(e.Proxy), DisableKeepAlives: true, TLSClientConfig: e.TLS}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return egressproxy.TicketResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return egressproxy.TicketResponse{}, refusedFrom(resp)
	}
	var t egressproxy.TicketResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&t); err != nil || t.Ticket == "" {
		return egressproxy.TicketResponse{}, fmt.Errorf("egress: bad ticket response: %v", err)
	}
	life := time.Duration(t.ExpiresInMs)*time.Millisecond - 5*time.Second
	e.mu.Lock()
	if e.tickets == nil || len(e.tickets) > 4096 {
		e.tickets = map[string]cachedTicket{}
	}
	if life > 0 {
		e.tickets[key] = cachedTicket{t: t, until: now.Add(life)}
	}
	e.mu.Unlock()
	return t, nil
}

// DialSOCKS opens a TCP connection to host:port through the proxy's SOCKS5
// listener (username = decision id, password = ticket).
func (e *Egress) DialSOCKS(ctx context.Context, tc *ToolCtx, d scope.Decision, host string, port int, declared []int) (net.Conn, error) {
	t, err := e.Ticket(ctx, tc, d, declared)
	if err != nil {
		return nil, err
	}
	if t.SOCKS5 == "" {
		return nil, errors.New("egress proxy has no SOCKS5 listener")
	}
	var dl net.Dialer
	conn, err := dl.DialContext(ctx, "tcp", t.SOCKS5)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if _, err := socks5.ClientConnect(conn, t.DecisionID, t.Ticket, socks5.AddrFrom(host, uint16(port))); err != nil {
		conn.Close()
		var se *socks5.Error
		if errors.As(err, &se) && se.Code == socks5.CodeRequestFailed {
			return nil, &RefusedError{Reason: "socks5_" + se.Reply.String(), Status: "SOCKS5 reply " + strconv.Itoa(int(se.Reply))}
		}
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// LookupDNS implements Network: a query to the proxy's DNS forwarder
// carrying the ticket of d in an EDNS option (UDP, retried over TCP when
// truncated). REFUSED is a *RefusedError.
func (e *Egress) LookupDNS(ctx context.Context, tc *ToolCtx, d scope.Decision, name string, t dnsmsg.Type) (egressproxy.DNSAnswer, error) {
	tk, err := e.Ticket(ctx, tc, d, nil)
	if err != nil {
		return egressproxy.DNSAnswer{}, err
	}
	if tk.DNS == "" {
		return egressproxy.DNSAnswer{}, ErrNoDNS
	}
	return QueryDNS(ctx, tk.DNS, tk.Ticket, name, t)
}

// QueryDNS sends one query for (name, t) with ticket to the forwarder at
// addr.
func QueryDNS(ctx context.Context, addr, ticket, name string, t dnsmsg.Type) (egressproxy.DNSAnswer, error) {
	id := uint16(core.SystemRng{}.Uint64())
	q := dnsmsg.NewQuery(id, name, t)
	q.SetEDNS(dnsmsg.EDNS{UDPSize: 1232, Options: []dnsmsg.Option{{Code: egressproxy.EDNSTicketOption, Data: []byte(ticket)}}})
	b, err := dnsmsg.Encode(q)
	if err != nil {
		return egressproxy.DNSAnswer{}, err
	}
	resp, err := dnsExchange(ctx, "udp", addr, b)
	var m dnsmsg.Message
	if err == nil {
		if m, _, err = dnsmsg.Decode(resp, dnsmsg.Lenient); err == nil && m.Header.Truncated {
			if resp, err = dnsExchange(ctx, "tcp", addr, b); err == nil {
				m, _, err = dnsmsg.Decode(resp, dnsmsg.Lenient)
			}
		}
	}
	if err != nil {
		return egressproxy.DNSAnswer{}, err
	}
	if m.Header.ID != id || !m.Header.Response {
		return egressproxy.DNSAnswer{}, errors.New("egress: mismatched DNS response")
	}
	if m.Header.RCode == dnsmsg.RCodeRefused {
		return egressproxy.DNSAnswer{}, &RefusedError{Reason: "dns_refused", Status: "REFUSED"}
	}
	return egressproxy.DNSAnswer{RCode: m.Header.RCode, Answers: m.Answers}, nil
}

func dnsExchange(ctx context.Context, network, addr string, q []byte) ([]byte, error) {
	d := net.Dialer{Timeout: 3 * time.Second}
	c, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	dl := time.Now().Add(5 * time.Second)
	if cd, ok := ctx.Deadline(); ok && cd.Before(dl) {
		dl = cd
	}
	_ = c.SetDeadline(dl)
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
