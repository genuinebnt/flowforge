package egressproxy_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dnsmsg"
	"github.com/genuinebnt/flowforge/engine"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
	"github.com/genuinebnt/flowforge/services/toolrunner"
)

// labCert is a self-signed certificate for the lab names and 127.0.0.1.
func labCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "app.test"},
		DNSNames: []string{"app.test", "www.app.test", "evil.test"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

type tlsTarget struct {
	port int
	hits atomic.Int64
}

// tlsListen serves h over TLS (HTTP/1.1) on 127.0.0.1 with cert.
func tlsListen(t *testing.T, cert tls.Certificate, h http.HandlerFunc) *tlsTarget {
	t.Helper()
	tt := &tlsTarget{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tt.hits.Add(1); h(w, r) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	_, ps, _ := net.SplitHostPort(srv.Listener.Addr().String())
	tt.port, _ = strconv.Atoi(ps)
	return tt
}

func decisions(e *v2env) []scopesvc.DecisionRecord {
	return e.st.ScopeRepo.(*scopesvc.MemRepo).Decisions(tenant)
}

// TestRegress_G_SVC_07_HTTPSRedirectHops: inside a TLS tunnel the proxy
// cannot see a redirect, so the runner checks every hop itself and the
// proxy checks every hop's tunnel. The runner's client re-checks each
// Location with the scope service (an active, resolving check whose
// decision is recorded and pins the hop's addresses) and opens the next
// hop's tunnel under that decision; a denied hop is reported and never
// contacted. Independently, the proxy admits a tunnel only for the
// (host, port) its decision covers, so a client that followed a hop under
// the previous hop's decision is refused (host_mismatch). httpprobe, the
// other HTTP tool on HTTPClient, behaves the same; collect and crawl
// follow hop by hop with their own checks (TestContentTools_CollectRedirects).
func TestRegress_G_SVC_07_HTTPSRedirectHops(t *testing.T) {
	cert, pool := labCert(t)
	evil := tlsListen(t, cert, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "evil") })
	www := tlsListen(t, cert, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "www") })
	wwwURL := "https://www.app.test:" + strconv.Itoa(www.port) + "/"
	evilURL := "https://evil.test:" + strconv.Itoa(evil.port) + "/"
	app := tlsListen(t, cert, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hop":
			http.Redirect(w, r, wwwURL, http.StatusFound)
		case "/", "/out":
			http.Redirect(w, r, evilURL, http.StatusFound)
		default:
			_, _ = io.WriteString(w, "app")
		}
	})
	e := newV2(t, nil, nil, nil)
	e.eg.TargetTLS = &tls.Config{RootCAs: pool}
	appURL := "https://app.test:" + strconv.Itoa(app.port)
	d := e.decide(t, appURL+"/hop", scope.TargetURL, core.ClassActive, true)
	var skipped []string
	client := e.eg.HTTPClient(e.tc, d, func(target, reason string) { skipped = append(skipped, reason+" "+target) })

	// An in-scope hop to another host: followed under its own decision.
	if body := get(t, client, appURL+"/hop", http.StatusOK); body != "www" || www.hits.Load() != 1 || len(skipped) != 0 {
		t.Fatalf("in-scope hop: %q, %d hits, skipped %v", body, www.hits.Load(), skipped)
	}
	var hop *scopesvc.DecisionRecord
	for _, rec := range decisions(e) {
		if rec.Decision.Target == wwwURL && rec.Class == core.ClassActive && rec.Decision.Allowed() {
			hop = &rec
		}
	}
	if hop == nil || hop.Decision.ID == d.ID || len(hop.Decision.PinnedIPs) != 1 || hop.Decision.PinnedIPs[0] != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("the hop's decision was not recorded and pinned: %+v", hop)
	}
	// An out-of-scope hop: the 302 is the answer, the hop is reported and
	// its denial recorded, and evil.test is never contacted.
	resp, err := client.Get(appURL + "/out")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || len(skipped) != 1 || !strings.HasPrefix(skipped[0], "redirect_denied:") || evil.hits.Load() != 0 {
		t.Fatalf("out-of-scope hop: %d, skipped %v, evil hits %d", resp.StatusCode, skipped, evil.hits.Load())
	}
	denied := false
	for _, rec := range decisions(e) {
		denied = denied || (rec.Decision.Target == evilURL && !rec.Decision.Allowed())
	}
	if !denied {
		t.Fatal("the denied hop was not recorded")
	}
	// The proxy path: a client that follows the hop under the first hop's
	// decision is refused at the tunnel. IntelClient (ctlog, rdap, asn)
	// keeps its decision across hops by design, so this is also the check
	// that confines intelligence-source redirects to the allowlist.
	before := www.hits.Load()
	if _, err := e.eg.IntelClient(e.tc, d).Get(appURL + "/hop"); refusedReason(err) != "host_mismatch" || www.hits.Load() != before {
		t.Fatalf("hop under a stale decision: %v (www hits %d -> %d)", err, before, www.hits.Load())
	}
	// httpprobe: its probe of / redirects out of scope; skipped, not
	// followed.
	em := &collect{}
	probe := toolrunner.HTTPProbe{Ports: []toolrunner.ProbePort{{Scheme: "https", Port: app.port}}}
	if _, err := probe.Run(e.ctx, json.RawMessage(`"app.test"`), e.tc, em); err != nil {
		t.Fatal(err)
	}
	if evil.hits.Load() != 0 || len(em.skipped) != 1 || !strings.Contains(em.skipped[0], "redirect_denied") {
		t.Fatalf("httpprobe: skipped %v, evil hits %d", em.skipped, evil.hits.Load())
	}
}

// fakeStream is a ToolRunnerService.Execute stream that keeps the
// messages.
type fakeStream struct {
	grpc.ServerStream
	ctx  context.Context
	msgs []*pb.ExecuteResponse
}

func (f *fakeStream) Context() context.Context { return f.ctx }
func (f *fakeStream) Send(m *pb.ExecuteResponse) error {
	f.msgs = append(f.msgs, m)
	return nil
}

// runTool executes one call on srv as the engine of tenant.
func runTool(t *testing.T, e *v2env, srv *toolrunner.Server, tool string, input any) []*pb.ExecuteResponse {
	t.Helper()
	in, err := structpb.NewValue(input)
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeStream{ctx: platform.WithIdentity(e.ctx, platform.Identity{Tenant: tenant, Actor: "service:engine", Role: auth.RoleAnalyst})}
	if err := srv.Execute(&pb.ExecuteRequest{Tool: tool, Input: in, ProgramId: e.prog, EffectPath: "0", IdempotencyKey: "k"}, fs); err != nil {
		t.Fatal(err)
	}
	return fs.msgs
}

// TestRegress_G_SVC_16_RawProtocolBytes: through the real proxy, raw_ref
// names a transcript in the chunk store holding the protocol bytes the
// tools received: the DNS response messages from the forwarder (wire
// format), the TLS server's handshake records, and the HTTP response as
// read after TLS, with its Set-Cookie value and a credential in the body
// redacted.
func TestRegress_G_SVC_16_RawProtocolBytes(t *testing.T) {
	const awsKey = "AKIAIOSFODNN7EXAMPLE"
	cert, pool := labCert(t)
	web := tlsListen(t, cert, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "session=supersecretsession1; Path=/; Secure")
		_, _ = io.WriteString(w, "<title>lab</title> config "+awsKey)
	})
	e := newV2(t, nil, []int{web.port}, nil)
	e.eg.TargetTLS = &tls.Config{RootCAs: pool}
	raw := toolrunner.NewChunkRaw(chunkstore.NewMemBackend(), chunkstore.Config{})
	srv := toolrunner.NewServer(toolrunner.Options{Adapters: []toolrunner.Adapter{toolrunner.DNSResolve{}, toolrunner.TLSCert{},
		toolrunner.HTTPProbe{Ports: []toolrunner.ProbePort{{Scheme: "https", Port: web.port}}}},
		Scope: scopesvc.NewClient(e.st.Scope, "tool-runner"), Egress: e.eg, Raw: raw})
	transcript := func(tool string, input any) toolrunner.RawTranscript {
		t.Helper()
		ref := ""
		for _, m := range runTool(t, e, srv, tool, input) {
			if m.GetError() != nil {
				t.Fatalf("%s: %v", tool, m.GetError())
			}
			if o := m.GetObservation(); o != nil {
				ref = o.GetRawRef()
			}
		}
		if !strings.HasPrefix(ref, "chunk://r") {
			t.Fatalf("%s: raw_ref %q", tool, ref)
		}
		b, err := raw.GetRaw(e.ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		tr, err := toolrunner.DecodeRawTranscript(b)
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}

	tr := transcript("dnsresolve", "app.test")
	if len(tr.Records) != 5 || tr.Truncated {
		t.Fatalf("dnsresolve: %d records", len(tr.Records))
	}
	m, _, err := dnsmsg.Decode(tr.Records[0].Data, dnsmsg.Strict)
	if tr.Records[0].Kind != toolrunner.RawDNSMessage || err != nil || !m.Header.Response || len(m.Answers) != 1 ||
		m.Answers[0].Data.IP != netip.MustParseAddr("127.0.0.1") || tr.Records[0].Redacted {
		t.Fatalf("first DNS record: %+v %v", m, err)
	}

	tr = transcript("tlscert", []any{"app.test", float64(web.port)})
	rec := tr.Records[0]
	// A TLS record: content type 22 (handshake), version 3.x, then a
	// ServerHello (handshake type 2), RFC 8446 §5.1, §4.
	if len(tr.Records) != 1 || rec.Kind != toolrunner.RawTLSHandshake || len(rec.Data) < 6 || rec.Data[0] != 22 || rec.Data[1] != 3 || rec.Data[5] != 2 {
		t.Fatalf("tlscert record: %+v", tr.Records)
	}

	tr = transcript("httpprobe", "app.test")
	rec = tr.Records[0]
	body := string(rec.Data)
	if rec.Kind != toolrunner.RawHTTPResponse || !rec.Redacted || !strings.HasPrefix(body, "HTTP/1.1 200 OK\r\n") ||
		!strings.Contains(body, "<title>lab</title>") || strings.Contains(body, "supersecretsession1") || strings.Contains(body, awsKey) ||
		!strings.Contains(body, "Set-Cookie: session=[REDACTED:cookie_value:") {
		t.Fatalf("http record: %q", body)
	}
	if !bytes.Contains(rec.Data, []byte("\r\n\r\n")) {
		t.Fatal("http record lost its framing")
	}
}

// edgeProbe is a test tool that makes its scope decision, lets the test
// change the world (before), then sends n requests to its URL under that
// decision through the proxy. With retry set, a request refused 429 is
// taken over (ToolCtx.RetryingEdgeLimit), retry runs, and the request is
// sent once more.
type edgeProbe struct {
	url    string
	n      int
	before func()
	retry  func()
}

func (p *edgeProbe) Spec() toolrunner.Spec {
	return toolrunner.Spec{Name: "edgeprobe", Version: "1", InputType: "String", OutputType: "Int", Class: core.ClassActive, Timeout: time.Minute}
}

func (p *edgeProbe) Run(ctx context.Context, _ json.RawMessage, tc *toolrunner.ToolCtx, out toolrunner.Emitter) (json.RawMessage, error) {
	d, ok, err := tc.Allowed(ctx, out, p.url, scope.TargetURL, core.ClassActive)
	if !ok {
		return nil, err
	}
	if p.before != nil {
		p.before()
	}
	okN := 0
	for range p.n {
		for try := 0; try < 2; try++ {
			resp, err := tc.Network().HTTPClient(tc, d, func(string, string) {}).Get(p.url)
			if err != nil {
				break
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				okN++
			}
			if resp.StatusCode != http.StatusTooManyRequests || p.retry == nil {
				break
			}
			tc.RetryingEdgeLimit()
			p.retry()
		}
	}
	return json.Marshal(okN)
}

// TestRegress_G_ENG_09_EdgeOutcome: the egress proxy is the enforcement
// point of the per-target limit and of kill switches. When it refuses a
// request with rate_limited the tool runner reports the execution as
// rate_limited with the proxy's retry delay (which the engine waits out
// without counting an attempt), and when a kill switch was engaged after
// the runner's re-check (a race) it reports kill_switch_active, which the
// engine turns into a kill_switch_race cancellation.
func TestRegress_G_ENG_09_EdgeOutcome(t *testing.T) {
	web := listen(t, "127.0.0.1:0", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	u := "http://app.test:" + strconv.Itoa(web.port) + "/"
	t.Run("rate_limited", func(t *testing.T) {
		e := newV2(t, &pb.RateLimit{RequestsPerSecond: 1, Burst: 1, Concurrency: 4}, nil, nil)
		srv := toolrunner.NewServer(toolrunner.Options{Adapters: []toolrunner.Adapter{&edgeProbe{url: u, n: 2}},
			Scope: scopesvc.NewClient(e.st.Scope, "tool-runner"), Egress: e.eg})
		msgs := runTool(t, e, srv, "edgeprobe", "x")
		te := msgs[len(msgs)-1].GetError()
		if te.GetCode() != engine.CodeRateLimited || !te.GetRetryable() || te.GetRetryAfter().AsDuration() <= 0 || te.GetRetryAfter().AsDuration() > time.Second {
			t.Fatalf("rate-limited execution: %v", msgs[len(msgs)-1])
		}
	})
	t.Run("taken_over_by_the_tool", func(t *testing.T) {
		e := newV2(t, &pb.RateLimit{RequestsPerSecond: 1, Burst: 1, Concurrency: 4}, nil, nil)
		srv := toolrunner.NewServer(toolrunner.Options{Adapters: []toolrunner.Adapter{&edgeProbe{url: u, n: 2,
			retry: func() { e.clock.Advance(time.Second) }}},
			Scope: scopesvc.NewClient(e.st.Scope, "tool-runner"), Egress: e.eg})
		msgs := runTool(t, e, srv, "edgeprobe", "x")
		if d := msgs[len(msgs)-1].GetDone(); d == nil || d.GetOutput().GetNumberValue() != 2 {
			t.Fatalf("a refusal the tool retried was reported: %v", msgs[len(msgs)-1])
		}
	})
	t.Run("kill_switch_race", func(t *testing.T) {
		e := newV2(t, nil, nil, nil)
		before := web.hits.Load()
		engage := func() {
			if _, err := e.st.Scope.SetKillSwitch(e.ctx, &pb.SetKillSwitchRequest{Level: "program", TargetId: e.prog, Enabled: true, Reason: "race"}); err != nil {
				t.Fatal(err)
			}
		}
		srv := toolrunner.NewServer(toolrunner.Options{Adapters: []toolrunner.Adapter{&edgeProbe{url: u, n: 1, before: engage}},
			Scope: scopesvc.NewClient(e.st.Scope, "tool-runner"), Egress: e.eg})
		msgs := runTool(t, e, srv, "edgeprobe", "x")
		if te := msgs[len(msgs)-1].GetError(); te.GetCode() != engine.CodeKillSwitch || te.GetRetryable() || web.hits.Load() != before {
			t.Fatalf("kill switch race: %v (target hits %d -> %d)", msgs[len(msgs)-1], before, web.hits.Load())
		}
	})
}
