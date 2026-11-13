package toolrunner_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dnsmsg"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/egressproxy"
	"github.com/genuinebnt/flowforge/services/toolrunner"
	"github.com/genuinebnt/flowforge/testkit"
)

var update = flag.Bool("update", false, "record the expect members of conformance/tools fixtures")

// fixture is one recorded tool run (conformance/tools/<tool>/*.json).
type fixture struct {
	Description string          `json:"description"`
	Input       json.RawMessage `json:"input"`
	Scope       struct {
		Allow []string            `json:"allow"`
		Pins  map[string][]string `json:"pins"`
	} `json:"scope"`
	HTTP []struct {
		URL      string            `json:"url"`
		Status   int               `json:"status"`
		Headers  map[string]string `json:"headers"`
		Body     json.RawMessage   `json:"body"`
		BodyText *string           `json:"body_text"`
	} `json:"http"`
	DNS []struct {
		Name    string  `json:"name"`
		Type    uint16  `json:"type"`
		RCode   uint8   `json:"rcode"`
		Refused bool    `json:"refused"`
		Answers []rrFix `json:"answers"`
	} `json:"dns"`
	TCP []struct {
		Host    string `json:"host"`
		Port    int    `json:"port"`
		Open    bool   `json:"open"`
		Refused string `json:"refused"`
		TLS     *struct {
			CertPEM string `json:"cert_pem"`
			KeyPEM  string `json:"key_pem"`
		} `json:"tls"`
	} `json:"tcp"`
	Expect *expectation `json:"expect,omitempty"`
}

type rrFix struct {
	Name       string   `json:"name"`
	Type       uint16   `json:"type"`
	TTL        uint32   `json:"ttl"`
	IP         string   `json:"ip,omitempty"`
	Target     string   `json:"target,omitempty"`
	Preference uint16   `json:"preference,omitempty"`
	TXT        []string `json:"txt,omitempty"`
}

func (r rrFix) rr() dnsmsg.RR {
	out := dnsmsg.RR{Name: r.Name, Type: dnsmsg.Type(r.Type), Class: dnsmsg.ClassINET, TTL: r.TTL}
	switch dnsmsg.KindFor(out.Type) {
	case dnsmsg.KindA, dnsmsg.KindAAAA:
		out.Data = dnsmsg.RData{Kind: dnsmsg.KindFor(out.Type), IP: netip.MustParseAddr(r.IP)}
	case dnsmsg.KindName:
		out.Data = dnsmsg.RData{Kind: dnsmsg.KindName, Target: r.Target}
	case dnsmsg.KindMX:
		out.Data = dnsmsg.RData{Kind: dnsmsg.KindMX, Preference: r.Preference, Target: r.Target}
	case dnsmsg.KindTXT:
		out.Data = dnsmsg.RData{Kind: dnsmsg.KindTXT, Strings: r.TXT}
	}
	return out
}

type skip struct {
	Target string `json:"target"`
	Reason string `json:"reason"`
}

type expectation struct {
	Output       json.RawMessage   `json:"output,omitempty"`
	Error        string            `json:"error,omitempty"`
	Retryable    bool              `json:"retryable,omitempty"`
	Skipped      []skip            `json:"skipped"`
	Requests     []string          `json:"requests"`
	Dials        []string          `json:"dials"`
	DNSQueries   []string          `json:"dns_queries"`
	Observations []json.RawMessage `json:"observations"`
}

// fixtureNet serves a fixture's recorded responses and records every
// request, dial and query (toolrunner.Network).
type fixtureNet struct {
	t   *testing.T
	f   *fixture
	mu  sync.Mutex
	req []string
	dl  []string
	dq  []string
}

func (n *fixtureNet) rt() http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		u := r.URL.String()
		n.mu.Lock()
		n.req = append(n.req, u)
		n.mu.Unlock()
		for _, h := range n.f.HTTP {
			if h.URL != u {
				continue
			}
			var body []byte
			if h.BodyText != nil {
				body = []byte(*h.BodyText)
			} else {
				body = h.Body
			}
			resp := &http.Response{StatusCode: h.Status, Status: strconv.Itoa(h.Status) + " " + http.StatusText(h.Status), Header: http.Header{},
				Body: io.NopCloser(bytes.NewReader(body)), Request: r, ProtoMajor: 1, ProtoMinor: 1}
			for k, v := range h.Headers {
				resp.Header.Set(k, v)
			}
			return resp, nil
		}
		return nil, errors.New("fixture: unrecorded request " + u)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (n *fixtureNet) HTTPClient(*toolrunner.ToolCtx, scope.Decision, func(string, string)) *http.Client {
	return &http.Client{Transport: n.rt()}
}

func (n *fixtureNet) IntelClient(_ *toolrunner.ToolCtx, d scope.Decision) *http.Client {
	if !d.Allowed() {
		n.t.Errorf("intel request under a non-allow decision")
	}
	return toolrunner.IntelHTTPClient(n.rt())
}

func (n *fixtureNet) DialTCP(ctx context.Context, _ *toolrunner.ToolCtx, d scope.Decision, host string, port int, declared []int) (net.Conn, error) {
	hp := net.JoinHostPort(host, strconv.Itoa(port))
	n.mu.Lock()
	n.dl = append(n.dl, hp)
	n.mu.Unlock()
	if !d.Allowed() || len(d.PinnedIPs) == 0 {
		n.t.Errorf("dial %s without a pinned allow decision", hp)
	}
	if !slices.Contains(declared, port) {
		n.t.Errorf("dial %s: port not declared (%v)", hp, declared)
	}
	for _, c := range n.f.TCP {
		if c.Host != host || c.Port != port {
			continue
		}
		if c.Refused != "" {
			return nil, &toolrunner.RefusedError{Reason: c.Refused, Status: "fixture"}
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			if c.TLS == nil {
				_, _ = io.Copy(io.Discard, server)
				return
			}
			cert, err := tls.X509KeyPair([]byte(c.TLS.CertPEM), []byte(c.TLS.KeyPEM))
			if err != nil {
				n.t.Errorf("fixture key pair: %v", err)
				return
			}
			ts := tls.Server(server, &tls.Config{Certificates: []tls.Certificate{cert}, GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
				if h.ServerName != host {
					n.t.Errorf("SNI %q, want %q", h.ServerName, host)
				}
				return nil, nil
			}})
			_ = ts.HandshakeContext(ctx)
			if b, _ := io.ReadAll(ts); len(b) > 0 {
				n.t.Errorf("tlscert sent application data: %q", b)
			}
		}()
		return client, nil
	}
	return nil, &toolrunner.RefusedError{Reason: "connect_failed", Status: "fixture"}
}

func (n *fixtureNet) LookupDNS(_ context.Context, _ *toolrunner.ToolCtx, d scope.Decision, name string, t dnsmsg.Type) (egressproxy.DNSAnswer, error) {
	n.mu.Lock()
	n.dq = append(n.dq, name+" "+strconv.Itoa(int(t)))
	n.mu.Unlock()
	if !d.Allowed() {
		n.t.Errorf("DNS query under a non-allow decision")
	}
	for _, q := range n.f.DNS {
		if q.Name == name && dnsmsg.Type(q.Type) == t {
			if q.Refused {
				return egressproxy.DNSAnswer{}, &toolrunner.RefusedError{Reason: "dns_refused", Status: "REFUSED"}
			}
			a := egressproxy.DNSAnswer{RCode: dnsmsg.RCode(q.RCode)}
			for _, r := range q.Answers {
				a.Answers = append(a.Answers, r.rr())
			}
			return a, nil
		}
	}
	return egressproxy.DNSAnswer{RCode: dnsmsg.RCodeNXDomain}, nil
}

// fixtureScope allows the listed targets ("*.x" = below x), pinning the
// listed addresses.
func fixtureScope(f *fixture) func(context.Context, string, scope.TargetKind, core.ActivityClass) (scope.Decision, error) {
	var mu sync.Mutex
	n := 0
	return func(_ context.Context, target string, kind scope.TargetKind, _ core.ActivityClass) (scope.Decision, error) {
		t, err := scope.Normalize(target, kind)
		if err != nil {
			return scope.Decision{Result: scope.ResultDeny, Reason: "invalid_target"}, nil
		}
		s := t.String()
		for _, a := range f.Scope.Allow {
			if s == a || (strings.HasPrefix(a, "*.") && strings.HasSuffix(s, a[1:])) {
				mu.Lock()
				n++
				id := "dec_fixture_" + strconv.Itoa(n)
				mu.Unlock()
				d := scope.Decision{ID: id, Result: scope.ResultAllow, Reason: "allow", Target: s, PinnedIPs: []netip.Addr{}}
				for _, ip := range f.Scope.Pins[s] {
					d.PinnedIPs = append(d.PinnedIPs, netip.MustParseAddr(ip))
				}
				return d, nil
			}
		}
		return scope.Decision{Result: scope.ResultDeny, Reason: "no_matching_allow", PinnedIPs: []netip.Addr{}}, nil
	}
}

// recorder is an Emitter that validates observations like the server.
type recorder struct {
	t    *testing.T
	tool oam.Tool
	mu   sync.Mutex
	obs  []json.RawMessage
	skip []skip
}

func (r *recorder) Observation(o oam.Observation) error {
	if len(o.Entities) > oam.MaxBatchEntities {
		r.t.Errorf("observation with %d entities", len(o.Entities))
	}
	o.Tool = r.tool
	if err := o.Validate(); err != nil {
		r.t.Errorf("invalid observation: %v", err)
	}
	b, err := oam.EncodeObservation(o)
	if err != nil {
		r.t.Fatal(err)
	}
	r.mu.Lock()
	r.obs = append(r.obs, canonical(r.t, b))
	r.mu.Unlock()
	return nil
}

func (r *recorder) Progress(int64, int64, string) error { return nil }

func (r *recorder) Skipped(target, reason string) error {
	r.mu.Lock()
	r.skip = append(r.skip, skip{target, reason})
	r.mu.Unlock()
	return nil
}

func canonical(t testing.TB, b []byte) json.RawMessage {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	c, err := core.CanonicalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var fixtureTenant = core.MustParseUUID("0190a1b2-0000-7000-8000-00000000f1f1")

// fixtureAdapters builds a fresh adapter per run (no cache across files).
func fixtureAdapters() map[string]toolrunner.Adapter {
	return map[string]toolrunner.Adapter{
		"ctlog": &toolrunner.CTLog{}, "rdap": &toolrunner.RDAP{}, "asn": &toolrunner.ASN{}, "dnsresolve": toolrunner.DNSResolve{},
		"portscan": toolrunner.PortScan{Pace: time.Millisecond}, "tlscert": toolrunner.TLSCert{},
	}
}

// TestToolFixtures runs every adapter against its recorded fixtures and
// asserts the output, error, skipped targets, contacted URLs, dials, DNS
// queries and emitted observations exactly. Denial fixtures prove that a
// denied target is skipped and nothing is contacted.
func TestToolFixtures(t *testing.T) {
	root := testkit.ConformanceDir(t, "tools")
	files, _ := filepath.Glob(filepath.Join(root, "*", "*.json"))
	if len(files) < 15 {
		t.Fatalf("only %d tool fixtures", len(files))
	}
	for _, path := range files {
		tool := filepath.Base(filepath.Dir(path))
		t.Run(tool+"/"+strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var f fixture
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			a := fixtureAdapters()[tool]
			if a == nil {
				t.Fatalf("no adapter %s", tool)
			}
			nw := &fixtureNet{t: t, f: &f}
			spec := a.Spec()
			tc := &toolrunner.ToolCtx{Tenant: fixtureTenant, Program: core.MustParseUUID("0190a1b2-0000-7000-8000-00000000f1f2"),
				Run: core.MustParseUUID("0190a1b2-0000-7000-8000-00000000f1f3"), Path: "0.1", IdempotencyKey: "fixture",
				Clock: testkit.NewFakeClock(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)), Net: nw}
			tc.Check = fixtureScope(&f)
			tc.CheckPinned = tc.Check
			rec := &recorder{t: t, tool: oam.Tool{Name: spec.Name, Version: spec.Version}}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			outJSON, err := a.Run(ctx, f.Input, tc, rec)
			got := expectation{Skipped: rec.skip, Requests: nw.req, Dials: nw.dl, DNSQueries: nw.dq, Observations: rec.obs}
			if err != nil {
				var te *toolrunner.ToolError
				if !errors.As(err, &te) {
					t.Fatalf("untyped error %v", err)
				}
				got.Error, got.Retryable = te.Code, te.Retryable
			} else {
				got.Output = canonical(t, outJSON)
			}
			sort.Strings(got.Dials)
			for _, s := range []*[]string{&got.Requests, &got.Dials, &got.DNSQueries} {
				if *s == nil {
					*s = []string{}
				}
			}
			if got.Skipped == nil {
				got.Skipped = []skip{}
			}
			if got.Observations == nil {
				got.Observations = []json.RawMessage{}
			}
			if *update {
				f.Expect = &got
				var m map[string]json.RawMessage
				_ = json.Unmarshal(raw, &m)
				e, _ := json.Marshal(got)
				m["expect"] = e
				var buf bytes.Buffer
				enc := json.NewEncoder(&buf)
				enc.SetIndent("", " ")
				enc.SetEscapeHTML(false)
				keys := []string{"description", "input", "scope", "http", "dns", "tcp", "expect"}
				buf.WriteString("{\n")
				first := true
				for _, k := range keys {
					v, ok := m[k]
					if !ok {
						continue
					}
					if !first {
						buf.WriteString(",\n")
					}
					first = false
					var pretty bytes.Buffer
					_ = json.Indent(&pretty, v, " ", " ")
					kb, _ := json.Marshal(k)
					buf.WriteString(" " + string(kb) + ": ")
					buf.Write(pretty.Bytes())
				}
				buf.WriteString("\n}\n")
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			if f.Expect == nil {
				t.Fatal("fixture has no expect member (run with -update and review)")
			}
			want := *f.Expect
			gb, _ := json.Marshal(normalizeExpect(t, got))
			wb, _ := json.Marshal(normalizeExpect(t, want))
			if !bytes.Equal(gb, wb) {
				t.Fatalf("run differs from the recording:\n got %s\nwant %s", gb, wb)
			}
		})
	}
}

func normalizeExpect(t *testing.T, e expectation) expectation {
	for i, o := range e.Observations {
		e.Observations[i] = canonical(t, o)
	}
	if len(e.Output) > 0 {
		e.Output = canonical(t, e.Output)
	}
	return e
}
