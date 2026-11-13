package egressproxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/genuinebnt/flowforge/socks5"
)

// handshakeTimeout bounds a SOCKS5 handshake.
const handshakeTimeout = 10 * time.Second

// ServeSOCKS serves SOCKS5 (RFC 1928, CONNECT only) on l until ctx ends.
// Username/password authentication (RFC 1929) carries the decision id and
// a ticket from TicketPath; the request then goes through the same checks
// as a CONNECT: the pinned decision must cover (host, port), the per-target
// limit must admit it and only pinned addresses are dialled. Refusals get
// reply 2 (connection not allowed by ruleset); BIND and UDP ASSOCIATE get
// reply 7.
func (p *Proxy) ServeSOCKS(ctx context.Context, l net.Listener) error {
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
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go p.serveSOCKSConn(ctx, c)
	}
}

func (p *Proxy) serveSOCKSConn(ctx context.Context, c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	var sess *session
	req, up, err := socks5.ServerHandshake(c, func(u socks5.UserPass) bool {
		s := p.tickets.get(u.Password, p.Now())
		if s == nil || s.decision != u.Username {
			return false
		}
		sess = s
		return true
	})
	if err != nil {
		reason := "socks_protocol"
		switch socks5.CodeOf(err) {
		case socks5.CodeAuthFailed, socks5.CodeNoAcceptableMethod:
			reason = "unauthenticated"
		case socks5.CodeCommandRefused:
			reason = "command_not_supported"
		}
		var rq *request
		if sess != nil {
			rq = sess.request()
			rq.via = "socks5"
		}
		_ = up
		p.logRefusal(ctx, rq, req.Addr.String(), refuse(reason, err.Error()))
		c.Close()
		return
	}
	rq := sess.request()
	rq.via, rq.host, rq.port = "socks5", req.Addr.Host(), int(req.Addr.Port)
	target := req.Addr.String()
	fail := func(rf *refusal, code socks5.ReplyCode) {
		p.logRefusal(ctx, rq, target, rf)
		_ = socks5.WriteReply(c, socks5.Reply{Code: code})
		c.Close()
	}
	if rq.port == 0 {
		fail(refuse("invalid_target", "port 0"), socks5.NotAllowed)
		return
	}
	if rf := p.authorize(ctx, rq); rf != nil {
		fail(rf, socks5.NotAllowed)
		return
	}
	upstream, release, rf := p.connect(ctx, rq)
	if rf != nil {
		code := socks5.NotAllowed
		if rf.reason == "connect_failed" {
			code = socks5.ConnectionRefused
		}
		fail(rf, code)
		return
	}
	defer release()
	bound := socks5.Addr{Type: socks5.AddrIPv4, IP: netip.IPv4Unspecified()}
	if ap, err := netip.ParseAddrPort(upstream.LocalAddr().String()); err == nil {
		bound = socks5.AddrFrom(ap.Addr().Unmap().String(), ap.Port())
	}
	if err := socks5.WriteReply(c, socks5.Reply{Code: socks5.Succeeded, Addr: bound}); err != nil {
		upstream.Close()
		c.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})
	relay(c, upstream, nil)
}
