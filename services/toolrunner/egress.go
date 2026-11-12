package toolrunner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Egress is the tool runner's only route to targets (spec 3.2, 4.4): the
// scope-checked egress proxy. Every request names the allow decision it
// relies on; the proxy connects only to that decision's pinned addresses.
type Egress struct {
	Proxy  *url.URL
	Tokens platform.TokenSource
	// Service is the identity the runner presents to the proxy.
	Service string
}

// NewEgress parses the proxy address (host:port or http://host:port).
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
		tok, err := e.Tokens.Token(ctx, platform.Identity{Tenant: tc.Tenant, Actor: platform.ServiceActor(e.Service)})
		if err != nil {
			return nil, err
		}
		h.Set(platform.HeaderProxyAuthorization, "Bearer "+tok)
	}
	h.Set(platform.HeaderDecisionID, decision)
	h.Set(platform.HeaderRunID, core.PublicID(core.PrefixRun, tc.Run))
	h.Set(platform.HeaderEffectPath, tc.Path)
	return h, nil
}

// HTTPClient returns a client for one target whose allow decision is d.
// Every request goes through the proxy (plain HTTP as a forward proxy,
// HTTPS through CONNECT) with d's id; connections are never reused across
// hops. Redirects are followed only after a fresh scope check of the
// Location (class active), whose decision the next hop then carries; a
// denied hop is reported through skip and not followed (spec 4.5).
func (e *Egress) HTTPClient(tc *ToolCtx, d scope.Decision, skip func(target, reason string)) *http.Client {
	holder := &decisionHolder{id: d.ID}
	tr := &http.Transport{
		Proxy:             http.ProxyURL(e.Proxy),
		DisableKeepAlives: true,
		GetProxyConnectHeader: func(ctx context.Context, _ *url.URL, _ string) (http.Header, error) {
			return e.headers(ctx, tc, holder.get())
		},
		ResponseHeaderTimeout: 30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
	}
	rt := roundTripper(func(r *http.Request) (*http.Response, error) {
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
	return &http.Client{
		Transport: rt,
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

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ErrRefused reports a connection the proxy refused.
var ErrRefused = errors.New("egress proxy refused the connection")

// Dial opens a TCP connection to hostport through the proxy (CONNECT) under
// decision d.
func (e *Egress) Dial(ctx context.Context, tc *ToolCtx, d scope.Decision, hostport string) (net.Conn, error) {
	var dl net.Dialer
	conn, err := dl.DialContext(ctx, "tcp", e.Proxy.Host)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	h, err := e.headers(ctx, tc, d.ID)
	if err != nil {
		conn.Close()
		return nil, err
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
		return nil, fmt.Errorf("%w: %s (%s)", ErrRefused, resp.Status, resp.Header.Get(platform.HeaderRefused))
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }
