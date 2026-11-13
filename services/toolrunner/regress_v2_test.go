package toolrunner_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dnsmsg"
	"github.com/genuinebnt/flowforge/engine"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/egressproxy"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	"github.com/genuinebnt/flowforge/services/toolrunner"
)

// dnsNet is a Network with a DNS forwarder only: A answers for the names
// in answers (FQDNs with the trailing dot), counted per name. noDNS makes
// it a proxy without a forwarder.
type dnsNet struct {
	answers map[string]string
	noDNS   bool
	mu      sync.Mutex
	queries map[string]int
}

func (n *dnsNet) count(name string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.queries[name]
}

func (n *dnsNet) HTTPClient(*toolrunner.ToolCtx, scope.Decision, func(string, string)) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("dnsNet: no HTTP") })}
}

func (n *dnsNet) IntelClient(tc *toolrunner.ToolCtx, d scope.Decision) *http.Client {
	return n.HTTPClient(tc, d, nil)
}

func (n *dnsNet) DialTCP(context.Context, *toolrunner.ToolCtx, scope.Decision, string, int, []int) (net.Conn, error) {
	return nil, errors.New("dnsNet: no TCP")
}

func (n *dnsNet) LookupDNS(_ context.Context, _ *toolrunner.ToolCtx, _ scope.Decision, name string, t dnsmsg.Type) (egressproxy.DNSAnswer, error) {
	if n.noDNS {
		return egressproxy.DNSAnswer{}, toolrunner.ErrNoDNS
	}
	n.mu.Lock()
	if n.queries == nil {
		n.queries = map[string]int{}
	}
	n.queries[name]++
	n.mu.Unlock()
	ip, ok := n.answers[name]
	if !ok || t != dnsmsg.TypeA {
		return egressproxy.DNSAnswer{}, nil
	}
	return egressproxy.DNSAnswer{Answers: []dnsmsg.RR{{Name: name, Type: dnsmsg.TypeA, Class: dnsmsg.ClassINET, TTL: 60,
		Data: dnsmsg.RData{Kind: dnsmsg.KindA, IP: netip.MustParseAddr(ip)}}}}, nil
}

// runnerClient serves srv over bufconn and returns a client with an
// analyst identity of tenant A.
func runnerClient(t *testing.T, o toolrunner.Options) (pb.ToolRunnerServiceClient, context.Context) {
	t.Helper()
	clock := &core.SystemClock{}
	keys, _ := platform.NewDevKeys("t")
	v, _ := keys.Verifier(clock)
	tokens, _ := keys.Tokens(clock)
	if o.Scope == nil {
		o.Scope = checker{}
	}
	g := platform.NewGRPCServer(platform.ServerOptions{Service: "tool-runner", Verifier: v})
	pb.RegisterToolRunnerServiceServer(g, toolrunner.NewServer(o))
	lis := bufconn.Listen(1 << 20)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	conn, err := platform.Dial("passthrough:///buf", platform.ClientOptions{Tokens: tokens},
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	ctx := platform.WithIdentity(context.Background(), platform.Identity{Tenant: servicetest.TenantA, Actor: "service:engine", Role: auth.RoleAnalyst})
	return pb.NewToolRunnerServiceClient(conn), ctx
}

var regressProgram = core.PublicID(core.PrefixProgram, core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000f1"))

// execute runs one tool call and returns its stream.
func execute(t *testing.T, c pb.ToolRunnerServiceClient, ctx context.Context, tool string, input any) []*pb.ExecuteResponse {
	t.Helper()
	in, err := structpb.NewValue(input)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Execute(ctx, &pb.ExecuteRequest{Tool: tool, Input: in, ProgramId: regressProgram, EffectPath: "0", IdempotencyKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	var out []*pb.ExecuteResponse
	for {
		m, err := s.Recv()
		if err != nil {
			return out
		}
		out = append(out, m)
	}
}

// TestRegress_G_SVC_06_DNSOnlyThroughProxy: dnsresolve resolves only
// through the egress proxy's DNS forwarder. Without a proxy it fails with
// no_egress and with a proxy that has no forwarder with no_dns_forwarder
// (the runner has no resolver to fall back to: Options and ToolCtx carry
// none). The end-to-end half, through a real forwarder that answers only
// in-scope names and records its decisions, is
// TestRegress_G_SVC_06_ForwarderEndToEnd in services/egressproxy.
func TestRegress_G_SVC_06_DNSOnlyThroughProxy(t *testing.T) {
	for _, tc := range []struct {
		name string
		net  toolrunner.Network
		code string
	}{
		{"no proxy", nil, "no_egress"},
		{"proxy without forwarder", &dnsNet{noDNS: true}, "no_dns_forwarder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ctx := runnerClient(t, toolrunner.Options{Adapters: []toolrunner.Adapter{toolrunner.DNSResolve{}}, Net: tc.net})
			got := execute(t, c, ctx, "dnsresolve", "app.test")
			if e := got[len(got)-1].GetError(); e.GetCode() != tc.code || e.GetRetryable() {
				t.Fatalf("dnsresolve: %v, want a permanent %s", got, tc.code)
			}
			for _, m := range got {
				if m.GetObservation() != nil {
					t.Fatalf("an observation without a forwarder: %v", m)
				}
			}
		})
	}
	// With a forwarder the answer comes from it alone.
	nw := &dnsNet{answers: map[string]string{"app.test.": "192.0.2.7"}}
	c, ctx := runnerClient(t, toolrunner.Options{Adapters: []toolrunner.Adapter{toolrunner.DNSResolve{}}, Net: nw})
	got := execute(t, c, ctx, "dnsresolve", "app.test")
	if out := got[len(got)-1].GetDone().GetOutput().GetListValue().GetValues(); len(out) != 1 || out[0].GetStringValue() != "192.0.2.7" || nw.count("app.test.") == 0 {
		t.Fatalf("dnsresolve through the forwarder: %v", got)
	}
}

// TestRegress_G_SVC_09_ToolSpecRetryFields: every built-in tool declares a
// retry policy and an idempotency class; they travel in the proto
// ToolSpec, the engine's conversion reproduces the runner's spec hash, the
// policy validates, and every retry_on code is one the runner reports as
// retryable. A spec without the retry field (an older runner) gets
// DefaultToolRetry on both sides.
func TestRegress_G_SVC_09_ToolSpecRetryFields(t *testing.T) {
	cts, err := toolrunner.ContentAdapters(toolrunner.ContentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ads := append(toolrunner.DefaultAdapters(toolrunner.HTTPProbe{}), cts...)
	retryable := []string{"unavailable", "deadline_exceeded", "content_unavailable"}
	c, ctx := runnerClient(t, toolrunner.Options{Adapters: ads})
	resp, err := c.ListTools(ctx, &pb.ListToolsRequest{})
	if err != nil || len(resp.GetTools()) != len(ads) {
		t.Fatalf("ListTools: %d tools, %v", len(resp.GetTools()), err)
	}
	for _, p := range resp.GetTools() {
		if p.GetRetry() == nil || p.GetRetry().GetMaxAttempts() < 1 || p.GetIdempotency() == pb.IdempotencyClass_IDEMPOTENCY_CLASS_UNSPECIFIED {
			t.Errorf("%s: retry %v idempotency %v", p.GetName(), p.GetRetry(), p.GetIdempotency())
			continue
		}
		s := convert.ToolSpec(p)
		if got := s.SpecSHA256; p.GetSpecSha256() != hexOf(got[:]) {
			t.Errorf("%s: engine hash %x, runner %s", p.GetName(), got, p.GetSpecSha256())
		}
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", p.GetName(), err)
		}
		for _, code := range s.Retry.RetryOn {
			if !slices.Contains(retryable, code) {
				t.Errorf("%s retries on %q, which no tool reports as retryable", p.GetName(), code)
			}
		}
		if s.ReexecSafe() != p.GetIdempotent() {
			t.Errorf("%s: re-execution %v with idempotent %v", p.GetName(), s.ReexecSafe(), p.GetIdempotent())
		}
	}
	// A runner without the retry field: both sides use DefaultToolRetry.
	old := convert.ToolSpecProto(toolrunner.Mock{}.Spec().EngineSpec(convert.DefaultToolRetry))
	old.Retry, old.Idempotency = nil, pb.IdempotencyClass_IDEMPOTENCY_CLASS_UNSPECIFIED
	legacy := toolrunner.Mock{}.Spec()
	legacy.Retry, legacy.Idempotency = engine.RetryPolicy{}, ""
	want := legacy.EngineSpec(convert.DefaultToolRetry)
	if got := convert.ToolSpec(old); got.SpecSHA256 != want.SpecSHA256 {
		t.Fatalf("legacy spec hash %x, want %x", got.SpecSHA256, want.SpecSHA256)
	}
	// A class this build does not know is rejected, never guessed.
	bad := convert.ToolSpecProto(toolrunner.Mock{}.Spec().EngineSpec(convert.DefaultToolRetry))
	bad.Idempotency = pb.IdempotencyClass(42)
	if err := convert.ToolSpec(bad).Validate(); !errors.Is(err, engine.ErrInvalidToolSpec) {
		t.Fatalf("unknown idempotency class: %v", err)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

// TestRegress_G_SVC_16_RawTranscriptFormat pins the FFRW v1 bytes of a
// two-record transcript, the round trip, and the decoder's rejections.
func TestRegress_G_SVC_16_RawTranscriptFormat(t *testing.T) {
	tr := toolrunner.RawTranscript{Records: []toolrunner.RawRecord{
		{Kind: toolrunner.RawDNSMessage, Label: "d", Data: []byte{0xab}},
		{Kind: toolrunner.RawHTTPResponse, Label: "h", Data: []byte("HTTP"), Truncated: true, Redacted: true},
	}}
	b := toolrunner.EncodeRawTranscript(tr)
	want := []byte("FFRW\x01\x00\x00\x02" + "\x02\x00\x00\x01d\x00\x00\x00\x01\xab" + "\x01\x03\x00\x01h\x00\x00\x00\x04HTTP")
	if !bytes.Equal(b, want) {
		t.Fatalf("encoding\n got %q\nwant %q", b, want)
	}
	got, err := toolrunner.DecodeRawTranscript(b)
	if err != nil || len(got.Records) != 2 || !got.Records[1].Truncated || !got.Records[1].Redacted || string(got.Records[1].Data) != "HTTP" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, bad := range [][]byte{nil, []byte("FFRX\x01\x00\x00\x00"), b[:len(b)-1], append(bytes.Clone(b), 0),
		[]byte("FFRW\x02\x00\x00\x00"), []byte("FFRW\x01\x00\x00\x01\x09\x00\x00\x00\x00\x00\x00\x00")} {
		if _, err := toolrunner.DecodeRawTranscript(bad); !errors.Is(err, toolrunner.ErrRawFormat) {
			t.Errorf("%q decoded: %v", bad, err)
		}
	}
}

// TestRegress_G_SVC_16_RedactHTTPResponse: Set-Cookie values, credential
// headers and credential-shaped strings in headers and the body are
// replaced; everything else, the status line and the framing are kept.
func TestRegress_G_SVC_16_RedactHTTPResponse(t *testing.T) {
	const key = "AKIAIOSFODNN7EXAMPLE"
	in := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nSet-Cookie: sid=s3cr3tvalue42; Path=/; HttpOnly\r\n" +
		"X-Api-Key: abc123\r\nX-Debug: key=" + key + "\r\n\r\naws " + key + " end"
	out, changed := toolrunner.RedactHTTPResponse([]byte(in))
	s := string(out)
	if !changed || strings.Contains(s, "s3cr3tvalue42") || strings.Contains(s, key) || strings.Contains(s, "abc123") {
		t.Fatalf("secrets left: %q", s)
	}
	for _, keep := range []string{"HTTP/1.1 200 OK\r\n", "Content-Type: text/plain\r\n", "Set-Cookie: sid=[REDACTED:cookie_value:", "; Path=/; HttpOnly\r\n",
		"X-Api-Key: [REDACTED:credential_header:", "\r\n\r\naws [REDACTED:", " end"} {
		if !strings.Contains(s, keep) {
			t.Errorf("missing %q in %q", keep, s)
		}
	}
	plain := "HTTP/1.1 404 Not Found\r\nContent-Length: 2\r\n\r\nno"
	if out, changed := toolrunner.RedactHTTPResponse([]byte(plain)); changed || string(out) != plain {
		t.Fatalf("a response without secrets changed: %q", out)
	}
}

// TestRegress_G_SVC_16_RawCaptureBounded: the capture limit cuts a record
// and flags the transcript; later records keep their place but no bytes.
func TestRegress_G_SVC_16_RawCaptureBounded(t *testing.T) {
	c := toolrunner.NewRawCapture(10)
	c.Add(toolrunner.RawDNSMessage, "a", []byte("0123456"))
	c.Add(toolrunner.RawDNSMessage, "b", []byte("789abcdef"))
	c.Add(toolrunner.RawDNSMessage, "c", []byte("x"))
	tr, err := toolrunner.DecodeRawTranscript(c.Transcript())
	if err != nil {
		t.Fatal(err)
	}
	if !tr.Truncated || len(tr.Records) != 3 || string(tr.Records[0].Data) != "0123456" || tr.Records[0].Truncated ||
		string(tr.Records[1].Data) != "789" || !tr.Records[1].Truncated || len(tr.Records[2].Data) != 0 || !tr.Records[2].Truncated {
		t.Fatalf("bounded capture: %+v", tr)
	}
	if (*toolrunner.RawCapture)(nil).Transcript() != nil || toolrunner.NewRawCapture(0).Transcript() != nil {
		t.Fatal("an empty capture has a transcript")
	}
}

// TestRegress_G_SVC_16_ChunkRawStore: transcripts are stored in the chunk
// store, one namespace per tenant and day; equal bytes give equal refs;
// another tenant's identical transcript lives in its own namespace.
func TestRegress_G_SVC_16_ChunkRawStore(t *testing.T) {
	ctx := context.Background()
	b := chunkstore.NewMemBackend()
	s := toolrunner.NewChunkRaw(b, chunkstore.Config{})
	day := time.Date(2026, 11, 13, 23, 59, 0, 0, time.UTC)
	data := bytes.Repeat([]byte("raw protocol bytes "), 1000)
	r1, err := s.PutRaw(ctx, servicetest.TenantA, day, data)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := s.PutRaw(ctx, servicetest.TenantA, day, data)
	r3, _ := s.PutRaw(ctx, servicetest.TenantB, day, data)
	if r1 != r2 || !strings.HasPrefix(r1, "chunk://r20261113-"+servicetest.TenantA.String()+"/") || strings.Split(r3, "/")[2] == strings.Split(r1, "/")[2] {
		t.Fatalf("refs %s %s %s", r1, r2, r3)
	}
	got, err := s.GetRaw(ctx, r1)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back: %d bytes, %v", len(got), err)
	}
	if _, err := s.GetRaw(ctx, "mem://raw/x"); !errors.Is(err, toolrunner.ErrRawRef) {
		t.Fatalf("foreign ref: %v", err)
	}
	// The stored form is compressed: the repeated text takes far less than
	// its length in the backend.
	names, _ := b.List(ctx, "")
	size := 0
	for _, n := range names {
		o, _ := b.Get(ctx, n)
		size += len(o)
	}
	if size*4 > 2*len(data) {
		t.Fatalf("stored %d bytes for two copies of %d", size, len(data))
	}
	_ = s.Close(ctx)
}
