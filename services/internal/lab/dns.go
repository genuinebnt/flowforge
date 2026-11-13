package lab

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dnsmsg"
)

// The lab's DNS server is authoritative for the zones of its hosts (each
// host's last two labels): an A question for a host answers its network's
// address (InAddr or OutAddr); other types of a host answer no records;
// any other name in a lab zone is NXDOMAIN; names outside the zones are
// REFUSED (it is not a resolver). RFC 1035 wire format through dnsmsg,
// over UDP and TCP on one port.

func (l *Lab) startDNS() error {
	tl, err := net.Listen("tcp", net.JoinHostPort(InAddr.String(), "0"))
	if err != nil {
		return err
	}
	pc, err := net.ListenPacket("udp", tl.Addr().String())
	if err != nil {
		_ = tl.Close()
		return err
	}
	l.dnsTCP, l.dnsUDP, l.dnsAddr = tl, pc, tl.Addr().String()
	l.wg.Add(2)
	go func() {
		defer l.wg.Done()
		buf := make([]byte, dnsmsg.MaxMessageLen)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if resp := l.answer(buf[:n], 512); resp != nil {
				_, _ = pc.WriteTo(resp, from)
			}
		}
	}()
	go func() {
		defer l.wg.Done()
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				q, err := dnsmsg.ReadTCP(c, dnsmsg.MaxMessageLen)
				if err != nil {
					return
				}
				if resp := l.answer(q, dnsmsg.MaxMessageLen); resp != nil {
					_ = dnsmsg.WriteTCP(c, resp)
				}
			}()
		}
	}()
	return nil
}

// answer builds the response to one query message (nil: drop it).
func (l *Lab) answer(b []byte, max int) []byte {
	q, _, err := dnsmsg.Decode(b, dnsmsg.Lenient)
	if err != nil || q.Header.Response || len(q.Questions) != 1 {
		return nil
	}
	qq := q.Questions[0]
	name := strings.TrimSuffix(strings.ToLower(qq.Name), ".")
	l.mu.Lock()
	l.queries = append(l.queries, Query{Name: name, Type: qq.Type})
	l.mu.Unlock()
	resp := dnsmsg.Message{Header: dnsmsg.Header{ID: q.Header.ID, Response: true, Authoritative: true,
		RecursionDesired: q.Header.RecursionDesired}, Questions: q.Questions}
	scopeName, known := l.scopes[name]
	switch {
	case known && qq.Type == dnsmsg.TypeA:
		addr := InAddr
		if scopeName == "out" {
			addr = OutAddr
		}
		resp.Answers = []dnsmsg.RR{{Name: qq.Name, Type: dnsmsg.TypeA, Class: dnsmsg.ClassINET, TTL: 60,
			Data: dnsmsg.RData{Kind: dnsmsg.KindA, IP: addr}}}
	case known:
		// NODATA: the name exists, the type has no records.
	case l.inZone(name):
		resp.Header.RCode = dnsmsg.RCodeNXDomain
	default:
		resp.Header.Authoritative = false
		resp.Header.RCode = dnsmsg.RCodeRefused
	}
	out, _, err := dnsmsg.EncodeTruncated(resp, max)
	if err != nil {
		return nil
	}
	return out
}

// inZone reports a name inside one of the lab zones.
func (l *Lab) inZone(name string) bool {
	for h := range l.scopes {
		labels := strings.Split(h, ".")
		zone := strings.Join(labels[max(0, len(labels)-2):], ".")
		if name == zone || strings.HasSuffix(name, "."+zone) {
			return true
		}
	}
	return false
}

// exchange sends one question over UDP and returns the answer records.
func exchange(ctx context.Context, addr, name string, t dnsmsg.Type) ([]dnsmsg.RR, dnsmsg.RCode, error) {
	id := uint16(core.SystemRng{}.Uint64())
	b, err := dnsmsg.Encode(dnsmsg.NewQuery(id, name, t))
	if err != nil {
		return nil, 0, err
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	c, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return nil, 0, err
	}
	defer c.Close()
	dl := time.Now().Add(5 * time.Second)
	if cd, ok := ctx.Deadline(); ok && cd.Before(dl) {
		dl = cd
	}
	_ = c.SetDeadline(dl)
	if _, err := c.Write(b); err != nil {
		return nil, 0, err
	}
	buf := make([]byte, dnsmsg.MaxMessageLen)
	n, err := c.Read(buf)
	if err != nil {
		return nil, 0, err
	}
	m, _, err := dnsmsg.Decode(buf[:n], dnsmsg.Lenient)
	if err != nil {
		return nil, 0, err
	}
	if m.Header.ID != id || !m.Header.Response {
		return nil, 0, errors.New("lab: mismatched DNS response")
	}
	return m.Answers, m.Header.RCode, nil
}
