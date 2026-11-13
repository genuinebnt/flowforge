package wire_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/wire"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// TestEgressConfigFromEnv: the standalone variables map to the proxy's
// port policy, intel allowlist, listeners, upstream, notify allowlist and
// Redis CA; bad values fail.
func TestEgressConfigFromEnv(t *testing.T) {
	o, err := wire.EgressConfigFromEnv(env(map[string]string{
		"FF_EGRESS_PORTS": "80, 443,8000-8099", "FF_EGRESS_INTEL": "default,crt.test:8443=127.0.0.1,*.cymru.test:43",
		"FF_EGRESS_SOCKS_LISTEN": "127.0.0.1:0", "FF_EGRESS_DNS_LISTEN": "127.0.0.1:0", "FF_EGRESS_DNS_UPSTREAM": "192.0.2.53:53",
		"FF_NOTIFY_ALLOWLIST": "hooks.slack.com, ,.example.com", "FF_REDIS_CA": "pem",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !o.Ports.Allowed("p", 8050) || o.Ports.Allowed("p", 22) || !o.Ports.Allowed("p", 443) {
		t.Fatalf("ports %+v", o.Ports)
	}
	n := len(o.Intel)
	last, prev := o.Intel[n-1], o.Intel[n-2]
	if n < 3 || prev.Host != "crt.test" || prev.Ports[0] != 8443 || prev.Addrs[0].String() != "127.0.0.1" || !last.Subdomains || last.Host != "cymru.test" {
		t.Fatalf("intel %+v", o.Intel)
	}
	if len(o.NotifyAllowlist) != 2 || o.NotifyAllowlist[0] != "hooks.slack.com" || o.NotifyAllowlist[1] != ".example.com" || o.RedisCA != "pem" {
		t.Fatalf("notify allowlist %q, redis CA %q", o.NotifyAllowlist, o.RedisCA)
	}
	if o.Upstream == nil || o.SOCKSListen == "" || o.DNSListen == "" {
		t.Fatalf("%+v", o)
	}
	if o, _ := wire.EgressConfigFromEnv(env(nil)); o.Ports != nil || o.Intel != nil || o.SOCKSListen != "" {
		t.Fatalf("defaults %+v", o)
	}
	for k, v := range map[string]string{"FF_EGRESS_PORTS": "0", "FF_EGRESS_INTEL": "x:99999"} {
		if _, err := wire.EgressConfigFromEnv(env(map[string]string{k: v})); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
	if _, err := wire.ParsePortRanges("9-1"); err == nil {
		t.Error("reversed range accepted")
	}
}

type noDecisions struct{}

func (noDecisions) Decision(context.Context, core.UUID, string) (scopesvc.DecisionRecord, error) {
	return scopesvc.DecisionRecord{}, errors.New("none")
}

// TestEgressProxyStandaloneListeners: with listen addresses the built
// proxy opens the SOCKS5 and DNS listeners, advertises them and serves
// them from the component's run loops.
func TestEgressProxyStandaloneListeners(t *testing.T) {
	keys, _ := platform.NewDevKeys("t")
	v, _ := keys.Verifier(&core.SystemClock{})
	e := &wire.Env{Service: "egress-proxy", Logger: platform.DiscardLogger(), Metrics: platform.NewMetrics(nil), Clock: &core.SystemClock{}, Verifier: v}
	c, p, err := wire.EgressProxy(e, wire.EgressOptions{ScopeAddr: "127.0.0.1:1", Decisions: noDecisions{},
		SOCKSListen: "0.0.0.0:0", DNSListen: "127.0.0.1:0", DNSAdvertise: "dns.sidecar:53"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !strings.HasPrefix(p.SOCKSAddr, "127.0.0.1:") || p.DNSAddr != "dns.sidecar:53" || len(c.Run) != 3 {
		t.Fatalf("socks %q dns %q runs %d", p.SOCKSAddr, p.DNSAddr, len(c.Run))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run[0](ctx) }()
	conn, err := net.Dial("tcp", p.SOCKSAddr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte{5, 1, 0}) // no-auth only: refused with 0xFF
	b := make([]byte, 2)
	if _, err := conn.Read(b); err != nil || b[1] != 0xFF {
		t.Fatalf("SOCKS5 reply %x %v", b, err)
	}
	conn.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
