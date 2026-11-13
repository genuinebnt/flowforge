package notifier_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/egressproxy"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	"github.com/genuinebnt/flowforge/services/notifier"
)

// TestRegress_G_SVC_08_WebhooksThroughEgressProxy: with an egress proxy
// configured, a webhook delivery is a CONNECT tunnel through the proxy's
// notify path, which resolves and pins the destination itself; a
// destination off the operator allowlist, a caller that is not the
// notifier, and (with the default checks) an internal address are refused
// by the proxy, so nothing reaches them.
func TestRegress_G_SVC_08_WebhooksThroughEgressProxy(t *testing.T) {
	st, _, _ := servicetest.Stack(t, devstack.Options{})
	var hits atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
	}))
	t.Cleanup(target.Close)
	_, port, _ := net.SplitHostPort(target.Listener.Addr().String())
	pool := x509.NewCertPool()
	pool.AddCert(target.Certificate())

	// The httptest certificate names example.com; the proxy's resolver
	// sends it to the loopback listener, which the test admits explicitly.
	resolver := servicetest.NewResolver(map[string]string{"example.com": "127.0.0.1", "other.example.org": "127.0.0.1"})
	var connects atomic.Int64
	newProxy := func(check func([]netip.Addr) error) *url.URL {
		p := &egressproxy.Proxy{Verifier: st.Verifier, Resolver: resolver, NotifyAllowlist: []string{"example.com"},
			CheckNotifyAddrs: check, Metrics: st.Metrics}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				connects.Add(1)
			}
			p.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		return u
	}
	loopbackOK := func([]netip.Addr) error { return nil }
	proxied := newProxy(loopbackOK)
	send := func(proxy *url.URL, hook string) error {
		s := &notifier.Senders{Secrets: notifier.NewMapSecrets(map[string]string{"test://hook": "s3cret"}),
			Egress: &notifier.NotifyEgress{Proxy: proxy, Tokens: st.Tokens, TLS: &tls.Config{RootCAs: pool}}}
		ch := notifier.Channel{ID: core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000c1"), Tenant: servicetest.TenantA,
			Kind: notifier.KindWebhook, Name: "hook", Enabled: true, Config: map[string]any{"url": hook, "secret_ref": "test://hook"}}
		_, err := s.Send(context.Background(), ch, "", notifier.Message{DeliveryID: "d1", Body: []byte(`{}`), At: time.Now()})
		return err
	}

	if err := send(proxied, "https://example.com:"+port+"/hook"); err != nil {
		t.Fatalf("allowlisted delivery through the proxy: %v", err)
	}
	if hits.Load() != 1 || connects.Load() != 1 {
		t.Fatalf("target hits %d, proxy CONNECTs %d; want 1 and 1", hits.Load(), connects.Load())
	}
	if err := send(proxied, "https://other.example.org:"+port+"/hook"); err == nil {
		t.Fatal("delivery to a host off the allowlist succeeded")
	}
	if err := send(newProxy(nil), "https://example.com:"+port+"/hook"); err == nil {
		t.Fatal("delivery to a loopback address passed the proxy's SSRF checks")
	}
	if hits.Load() != 1 {
		t.Fatalf("refused deliveries reached the target (%d hits)", hits.Load())
	}
	for _, reason := range []string{"notify_not_allowlisted", "forbidden_address"} {
		if st.Metrics.Registry.Value("ff_egress_refused_total", reason) == 0 {
			t.Errorf("no ff_egress_refused_total{reason=%q}", reason)
		}
	}

	// Another service's token cannot use the notify path.
	req, _ := http.NewRequest(http.MethodConnect, proxied.String(), nil)
	req.Host = "example.com:" + port
	req.URL = &url.URL{Scheme: "http", Host: proxied.Host}
	tok, err := st.Tokens.Token(context.Background(), servicetestIdentity("tool-runner"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Proxy-Authorization", "Bearer "+tok)
	req.Header.Set("X-Flowforge-Egress-Purpose", "notify")
	conn, err := net.Dial("tcp", proxied.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "403") || !strings.Contains(string(buf[:n]), "notify_not_notifier") {
		t.Fatalf("tool-runner on the notify path: %q", buf[:n])
	}
}
