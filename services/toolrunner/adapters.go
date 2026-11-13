package toolrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// ProbePort is one scheme and port httpprobe tries.
type ProbePort struct {
	Scheme string
	Port   int
}

// HTTPProbe is the active httpprobe tool (spec 6.4 "HTTP probing"): an HTTP
// GET through the egress proxy per configured scheme and port, emitting URL,
// Service and Product entities with domain, ip_address, port and
// product_used edges and the page title as a SimpleProperty. Its output is
// the first responding Service (the prelude record), so workflows need no
// type declaration; URL, status, title and products are in the graph.
type HTTPProbe struct {
	// Ports defaults to https:443 and http:80.
	Ports []ProbePort
}

// Spec implements Adapter.
func (HTTPProbe) Spec() Spec {
	return Spec{Name: "httpprobe", Version: "1.0.0", Description: "HTTP probing and fingerprinting through the egress proxy",
		InputType: "InScope<Host>", OutputType: "Option<Service>",
		Observes: []string{oam.TypeURL, oam.TypeService, oam.TypeProduct, oam.TypeFQDN, oam.TypeIPAddress},
		Class:    core.ClassActive, RateRPS: 20, RateBurst: 40, Concurrency: 8, Timeout: 5 * time.Minute, Idempotent: true,
		Idempotency: engine.IdempotencyIdempotent, Retry: RetryTransient}
}

var titleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

type httpService struct {
	URL    string
	Status int
	Title  *string
	Tech   []product
	svc    oam.Service
}

// output is the prelude Service record (spec 7.1 prelude).
func (h *httpService) output() map[string]any {
	return map[string]any{"unique_id": h.svc.UniqueID, "service_type": h.svc.ServiceType, "output": h.svc.Output, "output_length": h.svc.OutputLength}
}

type product struct {
	Name    string  `json:"name"`
	Version *string `json:"version"`
	Vendor  *string `json:"vendor"`
}

// Run implements Adapter.
func (p HTTPProbe) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	host, err := decodeHost(input)
	if err != nil {
		return nil, err
	}
	if tc.Network() == nil {
		return nil, ErrNoEgress
	}
	ports := p.Ports
	if len(ports) == 0 {
		ports = []ProbePort{{"https", 443}, {"http", 80}}
	}
	var first *httpService
	for i, pp := range ports {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		_ = out.Progress(int64(i), int64(len(ports)), "probing")
		u := fmt.Sprintf("%s://%s/", pp.Scheme, net.JoinHostPort(host, strconv.Itoa(pp.Port)))
		d, ok, err := tc.Allowed(ctx, out, u, scope.TargetURL, core.ClassActive)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		svc, err := p.probe(ctx, tc, out, d, u, host, pp)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, ErrRefused) {
				_ = out.Skipped(u, "egress_refused")
			}
			continue
		}
		if first == nil {
			first = svc
		}
	}
	_ = out.Progress(int64(len(ports)), int64(len(ports)), "done")
	if first == nil {
		return json.RawMessage("null"), nil
	}
	return json.Marshal(first.output())
}

func (p HTTPProbe) probe(ctx context.Context, tc *ToolCtx, out Emitter, d scope.Decision, u, host string, pp ProbePort) (*httpService, error) {
	client := tc.Network().HTTPClient(tc, d, func(target, reason string) { _ = out.Skipped(target, reason) })
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "flowforge-httpprobe/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if r := resp.Header.Get(platform.HeaderRefused); r != "" {
		_ = out.Skipped(u, "egress_refused:"+r)
		return nil, fmt.Errorf("%w: %s", ErrRefused, r)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	final := resp.Request.URL.String()
	svc := &httpService{URL: final, Status: resp.StatusCode, Tech: []product{}}
	if m := titleRE.FindSubmatch(body); m != nil {
		t := strings.TrimSpace(html.UnescapeString(string(m[1])))
		if t != "" {
			svc.Title = &t
		}
	}
	fq := oam.FQDN{Name: host}
	fqKey, err := fq.Key()
	if err != nil {
		return nil, err
	}
	svc.svc = oam.Service{UniqueID: oam.ServiceUniqueID(fqKey, pp.Port, "tcp", "http"), ServiceType: "http",
		Output: resp.Proto + " " + resp.Status, OutputLength: len(body), Attributes: headerAttrs(resp.Header)}
	obs := oam.Observation{ObservationID: tc.ObservationID("httpprobe", u), ObservedAt: tc.Now(), Confidence: 0.9,
		Entities: []oam.ObservedEntity{
			{Ref: "u", Asset: oam.URL{URL: final}},
			{Ref: "h", Asset: fq},
			{Ref: "s", Asset: svc.svc},
		},
		Edges: []oam.ObservedEdge{
			{From: "u", To: "h", Relation: oam.SimpleRelation{Label: oam.LabelDomain}},
			{From: "u", To: "s", Relation: oam.PortRelation{Label: oam.LabelPort, PortNumber: pp.Port, Protocol: "tcp"}},
		},
	}
	if len(d.PinnedIPs) > 0 {
		a := d.PinnedIPs[0]
		ver := "IPv4"
		if a.Is6() {
			ver = "IPv6"
		}
		obs.Entities = append(obs.Entities, oam.ObservedEntity{Ref: "ip", Asset: oam.IPAddress{Address: a.String(), IPVersion: ver}})
		obs.Edges = append(obs.Edges, oam.ObservedEdge{From: "u", To: "ip", Relation: oam.SimpleRelation{Label: oam.LabelIPAddress}})
	}
	if name, version := parseServer(resp.Header.Get("Server")); name != "" {
		obs.Entities = append(obs.Entities, oam.ObservedEntity{Ref: "p", Asset: oam.Opaque{Type: oam.TypeProduct, Content: map[string]any{"name": name}}})
		obs.Edges = append(obs.Edges, oam.ObservedEdge{From: "s", To: "p", Relation: oam.SimpleRelation{Label: oam.LabelProductUsed}})
		pr := product{Name: name}
		if version != "" {
			pr.Version = &version
		}
		svc.Tech = append(svc.Tech, pr)
	}
	if svc.Title != nil {
		obs.Tags = append(obs.Tags, oam.ObservedTag{Target: "u", Property: oam.SimpleProperty{Name: "title", Value: *svc.Title}})
	}
	if err := out.Observation(obs); err != nil {
		return nil, err
	}
	return svc, nil
}

func headerAttrs(h http.Header) map[string][]string {
	out := map[string][]string{}
	for _, k := range []string{"Server", "X-Powered-By", "Content-Type"} {
		if v := h.Values(k); len(v) > 0 {
			out[strings.ToLower(k)] = v
		}
	}
	return out
}

// parseServer splits "nginx/1.25.3 (Ubuntu)" into name and version.
func parseServer(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	first, _, _ := strings.Cut(s, " ")
	name, version, _ := strings.Cut(first, "/")
	return strings.ToLower(name), version
}

func hostAsset(host string) (oam.Asset, string, error) {
	var a oam.Asset = oam.FQDN{Name: host}
	if ip, err := netip.ParseAddr(host); err == nil {
		ver := "IPv4"
		if ip.Is6() {
			ver = "IPv6"
		}
		a = oam.IPAddress{Address: ip.String(), IPVersion: ver}
	}
	k, err := a.Key()
	return a, k, err
}

// Mock is a deterministic test tool: passive, no network; it echoes its
// input and, for a string input, emits one FQDN entity "<input>" (no scope
// re-check: it touches no target).
type Mock struct {
	// Fail, when set, makes every run fail with this error.
	Fail *ToolError
}

// Spec implements Adapter.
func (Mock) Spec() Spec {
	return Spec{Name: "mock", Version: "1.0.0", Description: "Deterministic test tool", InputType: "String",
		OutputType: "String", Observes: []string{oam.TypeFQDN}, Class: core.ClassNone, Timeout: 10 * time.Second, Idempotent: true,
		Idempotency: engine.IdempotencyIdempotent, Retry: RetryNever}
}

// Run implements Adapter.
func (m Mock) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	if m.Fail != nil {
		return nil, m.Fail
	}
	if s, err := decodeString(input); err == nil {
		if t, err := scope.Normalize(s, scope.TargetHost); err == nil && t.IsName() {
			obs := oam.Observation{ObservationID: tc.ObservationID("mock", s), ObservedAt: tc.Now(), Confidence: 0.5,
				Entities: []oam.ObservedEntity{{Ref: "a", Asset: oam.FQDN{Name: t.Name()}}}}
			if err := out.Observation(obs); err != nil {
				return nil, err
			}
		}
	}
	_ = out.Progress(1, 1, "")
	return input, nil
}
